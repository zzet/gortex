package indexer

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// ErrCheckoutByteAnchorUnavailable leaves a byte edit on ordinary route
// admission. A retained/private generation cannot stand in for an accepted view.
var ErrCheckoutByteAnchorUnavailable = fmt.Errorf("%w: no authenticated accepted byte-mutation anchor", ErrCheckoutMutationStale)

type checkoutByteAnchor struct {
	head         gitstate.HEADState
	headEvidence gitstate.HeadEvidence
	checkout     store_sqlite.Checkout
	rootInfo     os.FileInfo
	route        store_sqlite.CheckoutRoute
	ids          []int64
	lease        *graphview.Lease
	basePin      *graphview.BasePin
}

// captureByteAnchor runs before withdrawing an actual accepted route. The
// provisional route pins and final route comparison close retirement races;
// no payload, mask or graph reader is materialized.
func (c *CheckoutCoordinator) captureByteAnchor(ctx context.Context, route store_sqlite.CheckoutRoute) error {
	if c.leases == nil || route.State != store_sqlite.RouteActive || route.CommitGenerationID <= 0 || route.DirtyGenerationID <= 0 {
		return ErrCheckoutByteAnchorUnavailable
	}
	provisional := c.leases.Acquire(route.CommitGenerationID, route.DirtyGenerationID)
	defer provisional.Release()
	checkout, found, err := c.catalog.GetCheckout(ctx, c.checkoutID)
	if err != nil {
		return err
	}
	if !found || checkout.State != store_sqlite.CheckoutStateReady || checkout.EffectiveMode != store_sqlite.CheckoutModeAutomatic || checkout.DesiredMode != store_sqlite.CheckoutModeAutomatic || checkout.ActiveIntentTransitionID != "" || checkout.UnavailableSince != 0 || checkout.RemovalDetectedAt != 0 || !sameMutationRoot(checkout.RootPath, c.root) {
		return ErrCheckoutByteAnchorUnavailable
	}
	info, err := checkoutRootFileInfo(c.root)
	if err != nil || !info.IsDir() {
		return ErrCheckoutByteAnchorUnavailable
	}
	layers, err := c.checkRouteLayers(ctx, route)
	if err != nil {
		return err
	}
	headEvidence := c.sampler.CaptureHeadEvidence()
	if !headEvidence.Usable() {
		return ErrCheckoutByteAnchorUnavailable
	}
	head, err := gitstate.SampleHEAD(ctx, c.root)
	if err != nil {
		return err
	}
	if !headEvidence.Unchanged() || !c.routedCommitLayerFor(layers.commit, layers.base, head.TreeOID) {
		return ErrCheckoutByteAnchorUnavailable
	}
	lease, moved, err := c.baseViewMaterializer().PinCheckoutRoute(ctx, c.checkoutID, route)
	if moved {
		// Only the exact route reread mismatch is recoverable at optional
		// admission. Keep the materializer's refusal metadata inspectable.
		return fmt.Errorf("%w: %w", ErrCheckoutMutationRouteMoved, err)
	}
	if err != nil {
		return err
	}
	ids := lease.IDs()
	var basePin *graphview.BasePin
	for _, id := range ids {
		if id == graphview.BaseCorpusGeneration {
			basePin = c.leases.AcquireBaseCorpus(c.repoPrefix)
			break
		}
	}
	c.byteAnchorMu.Lock()
	old := c.byteAnchor
	c.byteAnchor = &checkoutByteAnchor{checkout: checkout, rootInfo: info, route: route, ids: ids, lease: lease, basePin: basePin, head: head, headEvidence: headEvidence}
	c.byteAnchorMu.Unlock()
	if old != nil {
		old.lease.Release()
		old.basePin.Release()
	}
	return nil
}

func (c *CheckoutCoordinator) closeByteAnchor() {
	c.byteAnchorMu.Lock()
	old := c.byteAnchor
	c.byteAnchor = nil
	c.byteAnchorMu.Unlock()
	if old != nil {
		old.lease.Release()
		old.basePin.Release()
		c.mu.Lock()
		if c.backlog == nil {
			c.backlog = map[int64]struct{}{}
		}
		for _, id := range old.ids {
			if id > 0 {
				c.backlog[id] = struct{}{}
			}
		}
		c.mu.Unlock()
	}
}

// BeginCheckoutByteMutation transfers only source-byte authority. It joins the
// source admission, drains the old unpublished attempt under cycleMu, and binds
// a fresh WHOLE checkout sample. It never certifies the anchored graph as fresh.
func (l *CheckoutLifecycle) BeginCheckoutByteMutation(ctx context.Context, checkoutID, root, incarnation string) (*CheckoutMutation, error) {
	return l.beginCheckoutByteMutation(ctx, checkoutID, root, incarnation, false)
}

// BeginCheckoutByteHandoff grants ordinary checked mutation authority when a
// ready idle route's cycle lock can be taken immediately. It keeps that lock
// through the mutation, without a later whole-view selection/reacquisition.
// Pending or busy checkouts retain the authenticated byte handoff.
func (l *CheckoutLifecycle) BeginCheckoutByteHandoff(ctx context.Context, checkoutID, root, incarnation string) (*CheckoutMutation, error) {
	return l.beginCheckoutByteMutation(ctx, checkoutID, root, incarnation, true)
}

func (l *CheckoutLifecycle) beginCheckoutByteMutation(ctx context.Context, checkoutID, root, incarnation string, ordinaryWhenIdle bool) (_ *CheckoutMutation, resultErr error) {
	if l == nil || l.catalog == nil || checkoutID == "" || root == "" || incarnation == "" {
		return nil, ErrCheckoutByteAnchorUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// A retired route cannot authorize a byte handoff, even with a retained
	// anchor: checkByteAuthority admits only active or pending routes. Decline
	// this optional admission before activation can consume the caller's whole
	// deadline; the ordinary route wait retains its bounded refusal margin.
	var witness checkoutMutationRefusalWitness
	defer func() { resultErr = l.captureMutationRefusalError(resultErr, witness) }()
	initialRoute, found, err := l.catalog.GetCheckoutRoute(ctx, checkoutID)
	if err != nil {
		return nil, err
	}
	if found && initialRoute.State == store_sqlite.RouteRetired {
		return nil, ErrCheckoutByteAnchorUnavailable
	}
	c, closing := l.awaitMutationCoordinator(ctx, checkoutID)
	if c == nil || closing || !sameMutationRoot(c.root, root) || !c.admitSourceMutation() {
		return nil, ErrCheckoutByteAnchorUnavailable
	}
	admitted := true
	defer func() {
		if admitted {
			c.releaseSourceMutation()
		}
	}()
	waitCtx, cancel := checkoutMutationContext(ctx, c.lifetimeContext())
	defer cancel()
	// A live active route may provide the anchor on first admission. A pending
	// route may only use the explicit anchor captured before its withdrawal.
	route, found, err := c.catalog.GetCheckoutRoute(waitCtx, checkoutID)
	if err != nil {
		return nil, err
	}
	if ordinaryWhenIdle && found && route.State == store_sqlite.RouteActive && route.CommitGenerationID > 0 && route.DirtyGenerationID > 0 {
		c.byteMu.Lock()
		// TryLock never waits while holding byteMu. A winning admission owns
		// cycleMu before a new attempt can register, and keeps it until Close.
		ready := c.byteAttempt == nil && c.byteDemand == 0 && c.cycleMu.TryLock()
		c.byteMu.Unlock()
		if ready {
			cycleOwned := true
			defer func() {
				if cycleOwned {
					c.cycleMu.Unlock()
				}
			}()
			checkout, present, err := c.catalog.GetCheckout(waitCtx, checkoutID)
			if err != nil {
				return nil, err
			}
			if !present || checkout.Incarnation != incarnation || !sameMutationRoot(checkout.RootPath, root) {
				return nil, fmt.Errorf("%w: checkout identity changed", ErrCheckoutMutationStale)
			}
			witness = l.mutationRefusalWitness(checkout, route.RouteEpoch)
			rootInfo, err := checkoutRootFileInfo(root)
			if err != nil || !rootInfo.IsDir() {
				return nil, fmt.Errorf("%w: checkout root is unavailable", ErrCheckoutMutationStale)
			}
			StampPublicationPhase(ctx, PublicationMutationLocked)
			m, err := l.admitCheckoutMutationLocked(waitCtx, c, checkout, rootInfo, route.RouteEpoch, &route)
			if err != nil {
				return nil, err
			}
			admitted, cycleOwned = false, false
			l.clearMutationRefusalOnAdmission(witness, m)
			return m, nil
		}
	}
	if found && route.State == store_sqlite.RouteActive && route.DirtyGenerationID > 0 {
		if err := c.captureByteAnchor(waitCtx, route); err != nil {
			return nil, err
		}
	}
	c.byteAnchorMu.Lock()
	anchor := c.byteAnchor
	var held *graphview.Lease
	var baseHeld *graphview.BasePin
	if anchor != nil && anchor.checkout.Incarnation == incarnation && sameMutationRoot(anchor.checkout.RootPath, root) {
		held = c.leases.Acquire(anchor.ids...)
		if anchor.basePin != nil {
			baseHeld = c.leases.AcquireBaseCorpus(c.repoPrefix)
		}
	}
	c.byteAnchorMu.Unlock()
	if held == nil {
		return nil, ErrCheckoutByteAnchorUnavailable
	}
	owned := true
	defer func() {
		if owned {
			held.Release()
			baseHeld.Release()
		}
	}()
	witness = l.mutationRefusalWitness(anchor.checkout, anchor.route.RouteEpoch)
	defer func() { resultErr = withMutationRefusalRoute(resultErr, anchor.route) }()
	precheck := &CheckoutMutation{coordinator: c, checkout: anchor.checkout, rootInfo: anchor.rootInfo}
	if err := precheck.validateCheckout(waitCtx); err != nil {
		return nil, err
	}
	if !anchor.headEvidence.Unchanged() {
		return nil, ErrCheckoutByteAnchorUnavailable
	}
	if _, err := c.checkRouteLayers(waitCtx, anchor.route); err != nil {
		return nil, err
	}
	releaseDemand, attemptDone := c.reserveByteMutation()
	demanded := true
	defer func() {
		if demanded {
			releaseDemand()
		}
	}()
	// Join even a preflight attempt that has not acquired cycleMu yet.
	if attemptDone != nil {
		select {
		case <-attemptDone:
		case <-waitCtx.Done():
			return nil, checkoutMutationAdmissionError(waitCtx, "byte mutation attempt drain", waitCtx.Err())
		}
	}
	if err := acquireCycleLock(waitCtx, c); err != nil {
		return nil, checkoutMutationAdmissionError(waitCtx, "byte mutation attempt drain", err)
	}
	locked := true
	defer func() {
		if locked {
			c.cycleMu.Unlock()
		}
	}()
	m := &CheckoutMutation{coordinator: c, checkout: anchor.checkout, rootInfo: anchor.rootInfo, route: anchor.route, byteAnchor: anchor}
	if err := m.validateCheckout(waitCtx); err != nil {
		return nil, err
	}
	sample, err := sampleMutationAdmission(gitstate.WithUrgentSample(waitCtx), c.sampler, time.Time{})
	if err != nil {
		return nil, &checkoutMutationSampleFailure{err: fmt.Errorf("%w: byte admission sample: %w", ErrCheckoutMutationStale, err)}
	}
	if err := m.checkByteAuthority(waitCtx, sample); err != nil {
		return nil, err
	}
	m.byteSnapshot = &sample
	m.snapshotPinned = true
	m.headRef, m.headCommit, m.headTree = sample.HeadRef, sample.HeadCommit, sample.HeadTree
	receipt, err := lifecycleOutputAuthority(l).Begin(waitCtx, OutputEntryCheckoutSourceMutation, OutputMutationTarget{Kind: OutputGenerationCheckout, OwnerKey: "checkout:" + checkoutID, CheckoutID: checkoutID, Incarnation: incarnation, Generation: anchor.route.DirtyGenerationID})
	if err != nil {
		return nil, err
	}
	m.receipt = receipt
	var once sync.Once
	m.byteRelease = func() { once.Do(func() { held.Release(); baseHeld.Release(); releaseDemand() }) }
	owned, admitted, demanded, locked = false, false, false, false
	StampPublicationPhase(ctx, PublicationMutationAdmitted)
	l.clearMutationRefusalOnAdmission(witness, m)
	return m, nil
}

func (m *CheckoutMutation) checkByteAuthority(ctx context.Context, sample gitstate.DirtySnapshot) error {
	a := m.byteAnchor
	c := m.coordinator
	if a == nil || sample.Fingerprint == "" {
		return ErrCheckoutByteAnchorUnavailable
	}
	layers, err := c.checkRouteLayers(ctx, a.route)
	if err != nil {
		return err
	}
	if !a.headEvidence.Unchanged() || sample.HeadTree != a.head.TreeOID || sample.HeadCommit != a.head.CommitOID || sample.HeadRef != a.head.Ref {
		return fmt.Errorf("%w: accepted anchor HEAD changed", ErrCheckoutMutationStale)
	}
	if !c.routedCommitLayerFor(layers.commit, layers.base, sample.HeadTree) {
		return fmt.Errorf("%w: byte admission HEAD changed", ErrCheckoutMutationStale)
	}
	route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil {
		return err
	}
	if !found || route.GraphID != a.route.GraphID || route.CommitGenerationID != a.route.CommitGenerationID || (route.DirtyGenerationID != 0 && route.DirtyGenerationID != a.route.DirtyGenerationID) || (route.State != store_sqlite.RouteActive && route.State != store_sqlite.RoutePending) {
		return ErrCheckoutMutationRouteMoved
	}
	if m.byteSnapshot != nil && route != m.route {
		return ErrCheckoutMutationRouteMoved
	}
	// Use the CURRENT route epoch for the later guarded withdrawal, never an old
	// epoch from the accepted anchor. A pending slot already has no dirty view.
	m.route = route
	if m.byteSnapshot != nil && (sample.Fingerprint != m.byteSnapshot.Fingerprint || sample.HeadRef != m.headRef || sample.HeadCommit != m.headCommit || sample.HeadTree != m.headTree) {
		return ErrCheckoutMutationDiskMoved
	}
	return nil
}
