package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

// ErrCheckoutMutationStale means the view used to prepare an edit is no longer
// the current writable checkout. No disk write has been admitted by Prepare.
var ErrCheckoutMutationStale = errors.New("indexer: checkout mutation view is stale")

// ErrCheckoutMutationPending means a disk mutation has not yet reached a fresh
// routed generation. It does not mean the disk mutation was rolled back.
var ErrCheckoutMutationPending = errors.New("indexer: checkout mutation refresh is pending")

// ErrCheckoutMutationBusy reports work that ran out of the caller's budget or
// found a queue full, with the stage named. At admission, before source is
// written, that is this checkout's cycle lock or the lock-free snapshot
// pre-check that runs while the lock is held; in the synchronous Refresh,
// after the disk commit, it is the shared build lane. Retry once the active
// work releases what it holds; no primary fallback writes. A Refresh that
// gave up leaves its withdrawn route for Close to reschedule.
var ErrCheckoutMutationBusy = errors.New("indexer: checkout mutation lane is busy; retry")

// CheckoutMutation owns one checkout's route lock for a source edit; it takes
// the shared build lane only inside Refresh, for the length of that build.
// Callers must Close it on every exit, including dry runs. It never writes
// source itself and must not be used to update the primary corpus.
type CheckoutMutation struct {
	mu              sync.Mutex
	coordinator     *CheckoutCoordinator
	checkout        store_sqlite.Checkout
	rootInfo        os.FileInfo
	route           store_sqlite.CheckoutRoute
	prepared        bool
	fresh           bool
	closed          bool
	refreshQueued   bool
	refreshReserved bool
	snapshotPinned  bool
	headRef         string
	headCommit      string
	headTree        string
	// receipt is this lease's authority over the routed dirty generation it
	// withdraws and republishes. Exactly one output generation per mutation.
	receipt    *OutputMutationReceipt
	receiptErr error
}

// lifecycleOutputAuthority resolves the process authority a checkout mutation
// admits through. It is the MultiIndexer's — the same one every generation-zero
// lane resolves to — so a checkout source edit and a legacy corpus mutation are
// ordered by one authority rather than two.
func lifecycleOutputAuthority(l *CheckoutLifecycle) *OutputGenerationAuthority {
	if l == nil || l.mi == nil {
		return defaultOutputGenerationAuthority()
	}
	return l.mi.outputGenerationAuthority()
}

// BeginCheckoutMutation admits a source edit against the exact checkout route
// the caller materialized. It changes neither disk nor catalog: a dry run may
// simply close the lease.
//
// Admission takes only this checkout's cycle lock, never the daemon's one
// physical build lane. A lease builds nothing on its own: Prepare withdraws a
// route and EnqueueRefresh hands publication to the coordinator loop, so the
// lane would only make every edit wait for whichever checkout happens to be
// building. The one lease operation that does build, Refresh, queues for the
// lane itself. Lock then lane is the order every builder uses (the
// coordinator's cycle and RehomeTo included), so nothing that owns the lane
// ever waits for this lock.
//
// Admission waits until the caller deadline or coordinator shutdown, without a
// separate admission timeout. Dry runs take the same lease to validate the exact
// route and disk snapshot; they may therefore also wait behind this checkout's
// own work in progress (an explicit reindex of a tree the edit still matches,
// say), but not behind another checkout's. A tree the coordinator is rebuilding
// for is refused as stale before any wait, and a checkout being rehomed has no
// coordinator registered, so that is refused at once as well.
func (l *CheckoutLifecycle) BeginCheckoutMutation(ctx context.Context, checkoutID, expectedRoot string, expectedRouteEpoch int64) (*CheckoutMutation, error) {
	if l == nil || l.catalog == nil || checkoutID == "" || expectedRoot == "" || expectedRouteEpoch <= 0 {
		return nil, fmt.Errorf("%w: exact checkout identity and route epoch are required", ErrCheckoutMutationStale)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	checkout, found, err := l.catalog.GetCheckout(ctx, checkoutID)
	if err != nil {
		return nil, err
	}
	if !found || !sameMutationRoot(checkout.RootPath, expectedRoot) {
		return nil, fmt.Errorf("%w: checkout root changed", ErrCheckoutMutationStale)
	}
	rootInfo, err := checkoutRootFileInfo(checkout.RootPath)
	if err != nil || !rootInfo.IsDir() {
		return nil, fmt.Errorf("%w: checkout root is unavailable", ErrCheckoutMutationStale)
	}
	l.coordMu.Lock()
	c := l.coordinators[checkoutID]
	closing := l.coordinatorClosing
	l.coordMu.Unlock()
	if c == nil || closing || !sameMutationRoot(c.root, expectedRoot) {
		return nil, fmt.Errorf("%w: checkout coordinator is not available; retry after activation", ErrCheckoutMutationStale)
	}
	if !c.admitSourceMutation() {
		return nil, fmt.Errorf("%w: checkout coordinator is closing", ErrCheckoutMutationStale)
	}
	admitted := true
	defer func() {
		if admitted {
			c.releaseSourceMutation()
		}
	}()

	waitCtx, cancel := checkoutMutationContext(ctx, c.lifetimeContext())
	defer cancel()
	// A free lock is taken at once and everything is validated under it, as
	// before. A held lock means this checkout's coordinator is queued for the
	// lane or building, and while that rebuild is pending the route still
	// serves the last coherent generation: reads look exact and an edit gets
	// this far, but the lock is held for the whole wait and the edit would be
	// refused as stale the moment it got the lock anyway. So before waiting,
	// admission checks lock-free whether the tree still matches the routed
	// view and refuses a stale one now. The check under the lock below stays
	// the authority.
	if !c.cycleMu.TryLock() {
		if err := c.refuseStaleAdmission(waitCtx, expectedRouteEpoch); err != nil {
			return nil, err
		}
		if err := acquireCycleLock(waitCtx, c); err != nil {
			return nil, checkoutMutationAdmissionError(waitCtx, "checkout cycle lock", err)
		}
	}
	cycleOwned := true
	defer func() {
		if cycleOwned {
			c.cycleMu.Unlock()
		}
	}()
	m := &CheckoutMutation{coordinator: c, checkout: checkout, rootInfo: rootInfo}
	if err := m.validateCheckout(waitCtx); err != nil {
		return nil, err
	}
	route, found, err := c.catalog.GetCheckoutRoute(waitCtx, checkoutID)
	if err == nil && (!found || route.State != store_sqlite.RouteActive || route.RouteEpoch != expectedRouteEpoch || route.CommitGenerationID <= 0 || route.DirtyGenerationID <= 0) {
		err = fmt.Errorf("%w: checkout route changed; read the current exact view and retry", ErrCheckoutMutationStale)
	}
	if err != nil {
		return nil, err
	}
	m.route = route
	if err := m.validateSnapshot(waitCtx); err != nil {
		return nil, err
	}
	// The lease names exactly one output generation: this checkout's routed
	// DIRTY generation, the one Prepare withdraws and Refresh republishes. The
	// receipt is opened last, under the cycle lock, so its issue order is the
	// order edits are admitted for this checkout; Close settles it.
	receipt, err := lifecycleOutputAuthority(l).Begin(waitCtx, OutputEntryCheckoutSourceMutation, OutputMutationTarget{
		Kind:        OutputGenerationCheckout,
		OwnerKey:    "checkout:" + checkout.CheckoutID,
		CheckoutID:  checkout.CheckoutID,
		Incarnation: checkout.Incarnation,
		Generation:  route.DirtyGenerationID,
	})
	if err != nil {
		return nil, err
	}
	m.receipt = receipt
	admitted, cycleOwned = false, false // Close now owns every acquired resource.
	return m, nil
}

// Receipt reports this lease's output-generation receipt, nil once Close has
// settled it.
func (m *CheckoutMutation) Receipt() *OutputMutationReceipt {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.receipt
}

// ReceiptError reports why a fulfilled edit could not fulfil its generation —
// ErrOutputMutationReceiptSuperseded when a newer mutation for this checkout
// took the authority over. Nil when the lease settled cleanly.
func (m *CheckoutMutation) ReceiptError() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.receiptErr
}

// receiptStillCurrent is the PREVENTIVE half of the output-generation fence on
// the checkout lane: a lease whose routed dirty generation a newer mutation
// already took over is refused before it withdraws the route or republishes,
// so no byte of that generation moves under lost authority. Caller holds m.mu.
//
// It is called three times: before Prepare withdraws the route, before Refresh
// starts, and again inside Refresh AFTER the shared build gate is acquired and
// immediately before the build that republishes — the gate is an unbounded
// wait, so a check taken before it does not cover the publish.
//
// In today's production shape this can only refuse work in a shape the cycle
// lock does not already exclude — BeginCheckoutMutation holds c.cycleMu from
// admission through Close, so two live receipts for one "checkout:<id>" owner
// do not overlap. The check exists so the invariant survives the lease
// outliving that lock (asynchronous publication), rather than being an
// accidental property of the current locking.
//
// LIMITATION: the asynchronous route (Prepare -> disk write -> EnqueueRefresh
// -> Close) hands the republish to the coordinator loop, which runs after this
// lease is gone. Close abandons the receipt there, correctly — this lease
// fulfilled no generation — but the republish that follows carries no receipt
// of its own. Covering it means carrying the generation identity into
// enqueueCheckoutRefresh (internal/indexer/checkout_refresh.go) and the
// coordinator loop (checkout_coordinator.go), neither of which this item owns.
func (m *CheckoutMutation) receiptStillCurrent() error {
	if m.receipt == nil || !m.receipt.Superseded() {
		return nil
	}
	return fmt.Errorf("%w: checkout %q generation %d was taken over by a newer mutation",
		ErrOutputMutationReceiptSuperseded, m.checkout.CheckoutID, m.receipt.Target().Generation)
}

// Prepare withdraws the old dirty generation immediately before the disk
// commit. It is idempotent until Refresh or EnqueueRefresh; failures and dry
// runs must not call it. Already-pinned readers retain their immutable view.
func (m *CheckoutMutation) Prepare(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.fresh || m.refreshQueued {
		return fmt.Errorf("%w: mutation lease is no longer writable", ErrCheckoutMutationStale)
	}
	if m.prepared {
		return nil
	}
	if err := m.receiptStillCurrent(); err != nil {
		return err
	}
	ctx, cancel := checkoutMutationContext(ctx, m.coordinator.lifetimeContext())
	defer cancel()
	if err := m.validateCheckout(ctx); err != nil {
		return err
	}
	if err := m.validateSnapshot(ctx); err != nil {
		return err
	}
	if err := m.coordinator.reserveCheckoutRefresh(); err != nil {
		return err
	}
	m.refreshReserved = true
	if err := m.coordinator.clearDirtySlot(ctx, &m.route); err != nil {
		m.coordinator.releaseCheckoutRefreshReservation()
		m.refreshReserved = false
		return fmt.Errorf("%w: withdraw dirty route: %w", ErrCheckoutMutationStale, err)
	}
	m.prepared = true
	return nil
}

func checkoutMutationAdmissionError(ctx context.Context, stage string, err error) error {
	// Preserve the retry diagnosis and the deadline identity for a wait that
	// ran out. Explicit cancellation (including coordinator shutdown) remains
	// cancellation, rather than a retryable lane-busy error.
	if errors.Is(err, ErrViewBuildQueueFull) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: waiting for %s: %w", ErrCheckoutMutationBusy, stage, err)
	}
	return err
}

// Refresh publishes the edited checkout through its sparse coordinator, never
// through the primary's incremental indexer. Nil error promises an active route
// containing both resulting generations. A failure leaves a retry to Close.
//
// This is the one lease operation that builds, so it is the one that queues
// for the shared build lane. It queues while holding the cycle lock, the same
// lock-then-lane order the coordinator's own builds use. A wait that runs out
// reports the same busy identity and stage admission used to, and leaves the
// withdrawn route for Close to retry.
func (m *CheckoutMutation) Refresh(ctx context.Context) (CheckoutCycle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || !m.prepared || m.refreshQueued {
		return CheckoutCycle{}, fmt.Errorf("%w: no prepared checkout mutation", ErrCheckoutMutationStale)
	}
	// Preventive: a lease that lost its generation does not republish it.
	if err := m.receiptStillCurrent(); err != nil {
		return CheckoutCycle{}, err
	}
	ctx, cancel := checkoutMutationContext(ctx, m.coordinator.lifetimeContext())
	defer cancel()
	if err := m.validateCheckout(ctx); err != nil {
		return CheckoutCycle{}, err
	}
	releaseLane, err := m.coordinator.gate.Acquire(ctx, ViewBuildInteractive)
	if err != nil {
		return CheckoutCycle{}, checkoutMutationAdmissionError(ctx, "shared view-build gate", err)
	}
	defer releaseLane()
	// The receipt must cover the REPUBLISH, not merely the lease. Acquiring the
	// shared build gate blocks — that is the whole reason Refresh is the one
	// lease operation that queues — so the check at the top of this method was
	// taken before an unbounded wait. Re-check here, after the gate and
	// immediately before the build that publishes the new generations: a lease
	// whose generation a newer mutation took over during the wait must not
	// publish over that newer decision.
	if err := m.receiptStillCurrent(); err != nil {
		return CheckoutCycle{}, err
	}
	out := m.coordinator.reconcile(ctx)
	recordCoordinatorCycle(out)
	if out.Err != nil {
		return out, out.Err
	}
	if out.Rescheduled || out.Deferred || out.CommitGenerationID <= 0 || out.DirtyGenerationID <= 0 {
		return out, ErrCheckoutMutationPending
	}
	route, found, err := m.coordinator.catalog.GetCheckoutRoute(ctx, m.checkout.CheckoutID)
	if err != nil {
		return out, err
	}
	if !found || route.State != store_sqlite.RouteActive || route.CommitGenerationID != out.CommitGenerationID || route.DirtyGenerationID != out.DirtyGenerationID {
		return out, ErrCheckoutMutationPending
	}
	m.route, m.fresh = route, true
	return out, nil
}

// settleReceipt fulfils or abandons this lease's receipt exactly once, and
// records a refused fulfilment on the lease. Caller holds m.mu.
//
// A lease that reached a fresh route fulfils its generation; anything else — a
// dry run, a failed callback, a withdrawn route left for retry, a publication
// handed to the coordinator loop — did not, and must not claim it. A fulfilment
// refused as superseded is reported and rescheduled, never swallowed.
func (m *CheckoutMutation) settleReceipt() {
	if m.receipt == nil {
		return
	}
	receipt := m.receipt
	m.receipt = nil
	if !m.fresh {
		receipt.Abandon()
		return
	}
	if err := receipt.Complete(); err != nil {
		m.receiptErr = err
		m.coordinator.Signal("checkout mutation receipt was superseded")
	}
}

// Close releases a lease once. Any prepared edit that did not reach a fresh
// route is rescheduled, including a callback error or a partially applied write.
func (m *CheckoutMutation) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	if m.refreshReserved {
		m.coordinator.releaseCheckoutRefreshReservation()
		m.refreshReserved = false
	}
	if m.prepared && !m.fresh {
		m.coordinator.Signal("source mutation needs a dirty generation refresh")
	}
	// The settle stays here rather than moving into Refresh. Close is the only
	// point that knows whether this lease fulfilled anything: the asynchronous
	// route hands publication to the coordinator loop and fulfils nothing, and
	// keeping the receipt live from admission through Close is what makes a
	// CONCURRENT newer edit for this checkout supersede this one instead of
	// interleaving with it. What Refresh gained is the fence itself — the
	// receipt is re-validated after the build gate and immediately before the
	// republish — so a superseded receipt cannot publish even though it settles
	// later.
	m.settleReceipt()
	m.coordinator.cycleMu.Unlock()
	m.coordinator.releaseSourceMutation()
}

func (m *CheckoutMutation) validateCheckout(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	checkout, found, err := m.coordinator.catalog.GetCheckout(ctx, m.checkout.CheckoutID)
	if err != nil {
		return err
	}
	if !found || checkout.Incarnation != m.checkout.Incarnation || checkout.State != store_sqlite.CheckoutStateReady || checkout.EffectiveMode != store_sqlite.CheckoutModeAutomatic || checkout.DesiredMode != store_sqlite.CheckoutModeAutomatic || checkout.ActiveIntentTransitionID != "" || checkout.UnavailableSince != 0 || checkout.RemovalDetectedAt != 0 || !sameMutationRoot(checkout.RootPath, m.checkout.RootPath) {
		return fmt.Errorf("%w: checkout identity or availability changed", ErrCheckoutMutationStale)
	}
	info, err := checkoutRootFileInfo(checkout.RootPath)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: checkout root is unavailable", ErrCheckoutMutationStale)
	}
	if !os.SameFile(m.rootInfo, info) {
		return fmt.Errorf("%w: checkout root was replaced", ErrCheckoutMutationStale)
	}
	return nil
}

func sameMutationRoot(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		a = resolved
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = resolved
	}
	return pathkey.EqualPaths(a, b)
}

// An unchanged route epoch does not imply unchanged disk: external editors and
// git can run before the watcher reconciles. Refuse their newer state instead
// of applying symbol offsets from the previously materialized generation.
func (m *CheckoutMutation) validateSnapshot(ctx context.Context) error {
	c := m.coordinator
	sample, err := c.sampler.Sample(ctx)
	if err != nil {
		return fmt.Errorf("%w: sample checkout: %w", ErrCheckoutMutationStale, err)
	}
	if err := c.checkRoutedSnapshot(ctx, m.route, sample); err != nil {
		return err
	}
	if m.snapshotPinned && (m.headRef != sample.HeadRef || m.headCommit != sample.HeadCommit || m.headTree != sample.HeadTree) {
		return fmt.Errorf("%w: checkout HEAD changed since source admission", ErrCheckoutMutationStale)
	}
	m.snapshotPinned = true
	m.headRef, m.headCommit, m.headTree = sample.HeadRef, sample.HeadCommit, sample.HeadTree
	return nil
}

// checkRoutedSnapshot refuses a sample the routed generations do not describe:
// a HEAD the commit layer was not built for, or a working tree whose dirty
// fingerprint the dirty layer does not carry. Under the cycle lock it is the
// authority; refuseStaleAdmission also runs it lock-free so an edit does not
// queue behind the very rebuild that makes it stale.
func (c *CheckoutCoordinator) checkRoutedSnapshot(ctx context.Context, route store_sqlite.CheckoutRoute, sample gitstate.DirtySnapshot) error {
	base, err := c.primaryBase(ctx)
	if err != nil {
		return err
	}
	commit, found, err := c.catalog.GetViewGeneration(ctx, route.CommitGenerationID)
	if err != nil {
		return err
	}
	if !found || !servableGeneration(commit.State) || route.GraphID != base.graphID || generationRowKey(commit) != generationIdentityKey(c.commitIdentity(base, sample.HeadTree)) {
		return fmt.Errorf("%w: checkout HEAD or primary base changed", ErrCheckoutMutationStale)
	}
	dirty, found, err := c.catalog.GetViewGeneration(ctx, route.DirtyGenerationID)
	if err != nil {
		return err
	}
	if !found || !servableGeneration(dirty.State) || dirty.BaseGenerationID != route.CommitGenerationID || dirty.LowerViewFingerprint != sample.Fingerprint {
		return fmt.Errorf("%w: checkout disk changed; wait for a fresh view and retry", ErrCheckoutMutationStale)
	}
	return nil
}

// refuseStaleAdmission is the lock-free half of admission's snapshot check. It
// reads the route the caller materialized and the checkout's disk state
// without holding the cycle lock, so a stale tree is refused at once instead
// of after the coordinator's queued build. A route that is not active at the
// expected epoch is the same refusal the locked check would give.
func (c *CheckoutCoordinator) refuseStaleAdmission(ctx context.Context, expectedRouteEpoch int64) error {
	route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil {
		return err
	}
	if !found || route.State != store_sqlite.RouteActive || route.RouteEpoch != expectedRouteEpoch || route.CommitGenerationID <= 0 || route.DirtyGenerationID <= 0 {
		return fmt.Errorf("%w: checkout route changed; read the current exact view and retry", ErrCheckoutMutationStale)
	}
	sample, err := c.sampler.Sample(ctx)
	if err != nil {
		// A deadline that runs out inside the git call is the caller's
		// budget ending during admission, not a stale tree: report it the way
		// a wait for the lock reports it, stage included.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return checkoutMutationAdmissionError(ctx, "checkout snapshot pre-check", ctxErr)
		}
		return fmt.Errorf("%w: sample checkout: %w", ErrCheckoutMutationStale, err)
	}
	return c.checkRoutedSnapshot(ctx, route, sample)
}

func checkoutMutationContext(ctx, lifetime context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	merged, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(lifetime, cancel)
	if lifetime.Err() != nil {
		cancel()
	}
	return merged, func() { stop(); cancel() }
}

// acquireCycleLock takes the coordinator's cycle lock or returns the context's
// error. TryLock polling keeps the wait cancellable: a caller deadline, the
// coordinator's lifetime and a rehome's rebuild budget all end it, and no
// goroutine is left behind blocked on a Lock nobody can interrupt.
func acquireCycleLock(ctx context.Context, c *CheckoutCoordinator) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.cycleMu.TryLock() {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *CheckoutCoordinator) admitSourceMutation() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sourceMutationsClosing || c.lifetimeContext().Err() != nil {
		return false
	}
	if c.sourceMutations == 0 {
		c.sourceMutationsDrained = make(chan struct{})
	}
	c.sourceMutations++
	return true
}

func (c *CheckoutCoordinator) releaseSourceMutation() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sourceMutations--
	if c.sourceMutations == 0 {
		close(c.sourceMutationsDrained)
		c.sourceMutationsDrained = nil
	}
}

func (c *CheckoutCoordinator) waitSourceMutations(ctx context.Context) error {
	c.mu.Lock()
	drained := c.sourceMutationsDrained
	c.mu.Unlock()
	if drained == nil {
		return nil
	}
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
