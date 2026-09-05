package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

// ErrCheckoutMutationStale means the view used to prepare an edit is no longer
// the current writable checkout. No disk write has been admitted by Prepare.
var ErrCheckoutMutationStale = errors.New("indexer: checkout mutation view is stale")

// ErrCheckoutMutationPending means a disk mutation has not yet reached a fresh
// routed generation. It does not mean the disk mutation was rolled back.
var ErrCheckoutMutationPending = errors.New("indexer: checkout mutation refresh is pending")

// CheckoutMutation owns one checkout's physical build lane and route lock for
// a source edit. Callers must Close it on every exit, including dry runs. It
// never writes source itself and must not be used to update the primary corpus.
type CheckoutMutation struct {
	mu          sync.Mutex
	coordinator *CheckoutCoordinator
	checkout    store_sqlite.Checkout
	rootInfo    os.FileInfo
	route       store_sqlite.CheckoutRoute
	release     func()
	prepared    bool
	fresh       bool
	closed      bool
}

// BeginCheckoutMutation admits a source edit against the exact checkout route
// the caller materialized. It changes neither disk nor catalog: a dry run may
// simply close the lease. Gate-before-cycleMu matches background reconciliation
// so an edit can never hold the route lock while waiting on a build that needs it.
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
	rootInfo, err := os.Stat(checkout.RootPath)
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
	releaseGate, err := c.gate.Acquire(waitCtx, ViewBuildInteractive)
	if err != nil {
		return nil, err
	}
	gateOwned := true
	defer func() {
		if gateOwned {
			releaseGate()
		}
	}()
	if err := lockCheckoutMutationCycle(waitCtx, c); err != nil {
		return nil, err
	}
	cycleOwned := true
	defer func() {
		if cycleOwned {
			c.cycleMu.Unlock()
		}
	}()
	m := &CheckoutMutation{coordinator: c, checkout: checkout, rootInfo: rootInfo, release: releaseGate}
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
	admitted, gateOwned, cycleOwned = false, false, false // Close now owns every acquired resource.
	return m, nil
}

// Prepare withdraws the old dirty generation immediately before the disk
// commit. It is idempotent until Refresh; validation/preview failures and dry
// runs must not call it. Already-pinned readers retain their immutable view.
func (m *CheckoutMutation) Prepare(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.fresh {
		return fmt.Errorf("%w: mutation lease is no longer writable", ErrCheckoutMutationStale)
	}
	if m.prepared {
		return nil
	}
	ctx, cancel := checkoutMutationContext(ctx, m.coordinator.lifetimeContext())
	defer cancel()
	if err := m.validateCheckout(ctx); err != nil {
		return err
	}
	if err := m.validateSnapshot(ctx); err != nil {
		return err
	}
	if err := m.coordinator.clearDirtySlot(ctx, &m.route); err != nil {
		return fmt.Errorf("%w: withdraw dirty route: %w", ErrCheckoutMutationStale, err)
	}
	m.prepared = true
	return nil
}

// Refresh publishes the edited checkout through its sparse coordinator, never
// through the primary's incremental indexer. Nil error promises an active route
// containing both resulting generations. A failure leaves a retry to Close.
func (m *CheckoutMutation) Refresh(ctx context.Context) (CheckoutCycle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || !m.prepared {
		return CheckoutCycle{}, fmt.Errorf("%w: no prepared checkout mutation", ErrCheckoutMutationStale)
	}
	ctx, cancel := checkoutMutationContext(ctx, m.coordinator.lifetimeContext())
	defer cancel()
	if err := m.validateCheckout(ctx); err != nil {
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
	if m.prepared && !m.fresh {
		m.coordinator.Signal("source mutation needs a dirty generation refresh")
	}
	m.coordinator.cycleMu.Unlock()
	m.release()
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
	info, err := os.Stat(checkout.RootPath)
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
	base, err := c.primaryBase(ctx)
	if err != nil {
		return err
	}
	commit, found, err := c.catalog.GetViewGeneration(ctx, m.route.CommitGenerationID)
	if err != nil {
		return err
	}
	if !found || !servableGeneration(commit.State) || m.route.GraphID != base.graphID || generationRowKey(commit) != generationIdentityKey(c.commitIdentity(base, sample.HeadTree)) {
		return fmt.Errorf("%w: checkout HEAD or primary base changed", ErrCheckoutMutationStale)
	}
	dirty, found, err := c.catalog.GetViewGeneration(ctx, m.route.DirtyGenerationID)
	if err != nil {
		return err
	}
	if !found || !servableGeneration(dirty.State) || dirty.BaseGenerationID != m.route.CommitGenerationID || dirty.LowerViewFingerprint != sample.Fingerprint {
		return fmt.Errorf("%w: checkout disk changed; wait for a fresh view and retry", ErrCheckoutMutationStale)
	}
	return nil
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

// TryLock polling avoids leaving an unbounded goroutine behind when a caller
// cancels while another checkout transition owns cycleMu.
func lockCheckoutMutationCycle(ctx context.Context, c *CheckoutCoordinator) error {
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
