package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"

	"golang.org/x/sync/semaphore"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// checkoutMutationReaderLimit gives mutations on one checkout shared access
// while reserving an exclusive acquisition for the rare topology operations
// that can invalidate their root, graph, route, or coordinator identity.
const checkoutMutationReaderLimit int64 = 1 << 20

type checkoutMutationGate struct {
	sem *semaphore.Weighted

	// refs is protected by checkoutMutationFences.mu. A reference is acquired
	// while resolving the registry entry, before waiting on sem, and remains
	// held until the corresponding semaphore acquisition is released. That
	// closes the lookup-before-acquire race which otherwise lets retirement
	// publish a replacement gate beside a waiter on the old semaphore.
	refs uint64

	// retiring prevents new lookups from joining this gate. Waiters sleep on
	// retirementDone and re-resolve the registry entry after either successful
	// reclamation or reactivation caused by cancellation/guard loss.
	retiring       bool
	retirementDone chan struct{}
	drained        chan struct{}
}

type checkoutMutationFences struct {
	mu        sync.Mutex
	families  map[string]*checkoutMutationGate
	checkouts map[string]*checkoutMutationGate
	graphs    map[string]*checkoutMutationGate
}

func newCheckoutMutationFences() *checkoutMutationFences {
	return &checkoutMutationFences{
		families:  map[string]*checkoutMutationGate{},
		checkouts: map[string]*checkoutMutationGate{},
		graphs:    map[string]*checkoutMutationGate{},
	}
}

func (l *CheckoutLifecycle) ensureMutationFences() *checkoutMutationFences {
	if l == nil {
		return nil
	}
	l.mutationFencesOnce.Do(func() {
		if l.mutationFences == nil {
			l.mutationFences = newCheckoutMutationFences()
		}
	})
	return l.mutationFences
}

type checkoutMutationGateLease struct {
	fences *checkoutMutationFences
	gate   *checkoutMutationGate
	once   sync.Once
}

func newCheckoutMutationGate() *checkoutMutationGate {
	return &checkoutMutationGate{
		sem: semaphore.NewWeighted(checkoutMutationReaderLimit),
	}
}

// leaseMany resolves a complete, already ordered key set under one registry
// lock. If any member is retiring, it takes no references and waits before
// retrying the whole set. This prevents a multi-key acquisition from pinning
// one gate while waiting for another gate's retirement to finish.
func (f *checkoutMutationFences) leaseMany(
	ctx context.Context,
	registry map[string]*checkoutMutationGate,
	ids []string,
) ([]*checkoutMutationGateLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		f.mu.Lock()
		var retirementDone <-chan struct{}
		for _, id := range ids {
			gate := registry[id]
			if gate != nil && gate.retiring {
				retirementDone = gate.retirementDone
				break
			}
		}
		if retirementDone == nil {
			leases := make([]*checkoutMutationGateLease, 0, len(ids))
			for _, id := range ids {
				gate := registry[id]
				if gate == nil {
					gate = newCheckoutMutationGate()
					registry[id] = gate
				}
				gate.refs++
				leases = append(leases, &checkoutMutationGateLease{
					fences: f,
					gate:   gate,
				})
			}
			f.mu.Unlock()
			return leases, nil
		}
		f.mu.Unlock()

		select {
		case <-retirementDone:
			// The entry was either deleted or reactivated. Resolve it again;
			// keeping the old pointer would split exclusion across two gates.
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (f *checkoutMutationFences) lease(
	ctx context.Context,
	registry map[string]*checkoutMutationGate,
	id string,
) (*checkoutMutationGateLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		f.mu.Lock()
		gate := registry[id]
		if gate == nil {
			gate = newCheckoutMutationGate()
			registry[id] = gate
		}
		if !gate.retiring {
			gate.refs++
			f.mu.Unlock()
			return &checkoutMutationGateLease{fences: f, gate: gate}, nil
		}
		done := gate.retirementDone
		f.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (l *checkoutMutationGateLease) finish(releaseWeight int64) {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if releaseWeight > 0 {
			l.gate.sem.Release(releaseWeight)
		}
		f := l.fences
		f.mu.Lock()
		if l.gate.refs == 0 {
			f.mu.Unlock()
			panic("indexer: checkout mutation gate lease released without a reference")
		}
		l.gate.refs--
		if l.gate.refs == 0 && l.gate.retiring && l.gate.drained != nil {
			close(l.gate.drained)
			l.gate.drained = nil
		}
		f.mu.Unlock()
	})
}

func (l *checkoutMutationGateLease) releaseReference() {
	l.finish(0)
}

func (l *checkoutMutationGateLease) releaseSemaphore(weight int64) {
	l.finish(weight)
}

// checkoutMutationFenceRetirementGuard revalidates the authoritative logical
// deletion after every old lookup and semaphore holder has drained. Returning
// false leaves the original gate live; returning an error does the same.
type checkoutMutationFenceRetirementGuard func(context.Context) (bool, error)

func (f *checkoutMutationFences) reactivateRetiringGate(
	registry map[string]*checkoutMutationGate,
	id string,
	gate *checkoutMutationGate,
) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if registry[id] != gate || !gate.retiring {
		return
	}
	done := gate.retirementDone
	gate.retiring = false
	gate.retirementDone = nil
	gate.drained = nil
	close(done)
}

// retire marks an entry before waiting for pre-existing lookup leases. New
// acquisitions therefore wait without retaining the old gate and re-resolve
// after completion. Cancellation and failed logical guards reactivate that
// same entry so waiters do not remain stranded behind a half-retired gate.
// The caller must release every topology/mutation token containing id before
// invoking retire; retaining its own lease would make the drain self-blocking.
func (f *checkoutMutationFences) retire(
	ctx context.Context,
	registry map[string]*checkoutMutationGate,
	id string,
	guard checkoutMutationFenceRetirementGuard,
) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		f.mu.Lock()
		gate := registry[id]
		if gate == nil {
			f.mu.Unlock()
			return true, nil
		}
		if gate.retiring {
			done := gate.retirementDone
			f.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}

		gate.retiring = true
		gate.retirementDone = make(chan struct{})
		gate.drained = make(chan struct{})
		if gate.refs == 0 {
			close(gate.drained)
		}
		drained := gate.drained
		f.mu.Unlock()

		select {
		case <-drained:
			if err := ctx.Err(); err != nil {
				f.reactivateRetiringGate(registry, id, gate)
				return false, err
			}
		case <-ctx.Done():
			f.reactivateRetiringGate(registry, id, gate)
			return false, ctx.Err()
		}

		if guard != nil {
			stillDeleted, err := guard(ctx)
			if err != nil {
				f.reactivateRetiringGate(registry, id, gate)
				return false, err
			}
			if !stillDeleted {
				f.reactivateRetiringGate(registry, id, gate)
				return false, nil
			}
		}
		if err := ctx.Err(); err != nil {
			f.reactivateRetiringGate(registry, id, gate)
			return false, err
		}

		f.mu.Lock()
		if registry[id] != gate || !gate.retiring || gate.refs != 0 {
			f.mu.Unlock()
			f.reactivateRetiringGate(registry, id, gate)
			return false, nil
		}
		delete(registry, id)
		done := gate.retirementDone
		gate.retirementDone = nil
		gate.drained = nil
		f.mu.Unlock()
		close(done)
		return true, nil
	}
}

// retireCheckoutMutationFence is wired after authoritative checkout removal.
// The guard must confirm that the checkout has not been recreated while old
// mutation/topology holders were draining.
func (l *CheckoutLifecycle) retireCheckoutMutationFence(
	ctx context.Context,
	checkoutID string,
	guard checkoutMutationFenceRetirementGuard,
) (bool, error) {
	if l == nil || checkoutID == "" {
		return false, errors.New("indexer: checkout mutation fence identity is unavailable")
	}
	fences := l.ensureMutationFences()
	return fences.retire(ctx, fences.checkouts, checkoutID, guard)
}

// retireGraphMutationFence is wired after the last route and generation for a
// graph are gone. The supplied guard rechecks that authoritative condition.
func (l *CheckoutLifecycle) retireGraphMutationFence(
	ctx context.Context,
	graphID string,
	guard checkoutMutationFenceRetirementGuard,
) (bool, error) {
	if l == nil || graphID == "" {
		return false, errors.New("indexer: checkout graph mutation fence identity is unavailable")
	}
	fences := l.ensureMutationFences()
	return fences.retire(ctx, fences.graphs, graphID, guard)
}

// retireFamilyMutationFence is wired after authoritative Git-family removal.
// The supplied guard rechecks that the family has not been recreated.
func (l *CheckoutLifecycle) retireFamilyMutationFence(
	ctx context.Context,
	familyID string,
	guard checkoutMutationFenceRetirementGuard,
) (bool, error) {
	if l == nil || familyID == "" {
		return false, errors.New("indexer: checkout family mutation fence identity is unavailable")
	}
	fences := l.ensureMutationFences()
	return fences.retire(ctx, fences.families, familyID, guard)
}

type checkoutMutationTokenState uint8

const (
	checkoutMutationTokenHeld checkoutMutationTokenState = iota
	checkoutMutationTokenTransferred
	checkoutMutationTokenReleased
)

type checkoutMutationLease struct {
	checkoutGate *checkoutMutationGateLease
	graphGate    *checkoutMutationGateLease
	once         sync.Once
}

func (l *checkoutMutationLease) release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.graphGate != nil {
			l.graphGate.releaseSemaphore(1)
		}
		if l.checkoutGate != nil {
			l.checkoutGate.releaseSemaphore(1)
		}
	})
}

// CheckoutMutationToken is the exact checkout topology admitted before an MCP
// writer touches disk. It is single-use: request cleanup releases an unused
// token, while successful ticket admission transfers its lease to the
// coordinator waiter until publication reaches a terminal result.
type CheckoutMutationToken struct {
	mu sync.Mutex

	lifecycle *CheckoutLifecycle
	lease     *checkoutMutationLease
	state     checkoutMutationTokenState

	coordinator        *CheckoutCoordinator
	checkoutID         string
	root               string
	graphID            string
	observedRouteEpoch int64
}

func (t *CheckoutMutationToken) CheckoutID() string {
	if t == nil {
		return ""
	}
	return t.checkoutID
}

func (t *CheckoutMutationToken) Root() string {
	if t == nil {
		return ""
	}
	return t.root
}

func (t *CheckoutMutationToken) GraphID() string {
	if t == nil {
		return ""
	}
	return t.graphID
}

func (t *CheckoutMutationToken) ObservedRouteEpoch() int64 {
	if t == nil {
		return 0
	}
	return t.observedRouteEpoch
}

// Release gives back an unused token. Once EnqueueCheckoutMutation transfers
// it, request cleanup becomes a no-op and the publication waiter owns release.
func (t *CheckoutMutationToken) Release() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.state != checkoutMutationTokenHeld {
		t.mu.Unlock()
		return
	}
	t.state = checkoutMutationTokenReleased
	lease := t.lease
	t.mu.Unlock()
	lease.release()
}

func (t *CheckoutMutationToken) transfer(lifecycle *CheckoutLifecycle) (func(), error) {
	if t == nil || lifecycle == nil {
		return nil, errors.New("indexer: checkout mutation token is unavailable")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lifecycle != lifecycle {
		return nil, errors.New("indexer: checkout mutation token belongs to another lifecycle")
	}
	if t.state != checkoutMutationTokenHeld {
		return nil, errors.New("indexer: checkout mutation token was already consumed")
	}
	t.state = checkoutMutationTokenTransferred
	return t.lease.release, nil
}

// AcquireCheckoutMutation pins the selected checkout's root, graph, route and
// live coordinator before a source writer reaches disk.
func (l *CheckoutLifecycle) AcquireCheckoutMutation(
	ctx context.Context,
	checkoutID, expectedRoot string,
) (*CheckoutMutationToken, error) {
	if l == nil || l.catalog == nil {
		return nil, errors.New("indexer: checkout mutation lifecycle is unavailable")
	}
	fences := l.ensureMutationFences()
	if ctx == nil {
		ctx = context.Background()
	}
	if checkoutID == "" || expectedRoot == "" {
		return nil, errors.New("indexer: checkout mutation identity is incomplete")
	}
	checkoutGate, err := fences.lease(ctx, fences.checkouts, checkoutID)
	if err != nil {
		return nil, err
	}
	if err := checkoutGate.gate.sem.Acquire(ctx, 1); err != nil {
		checkoutGate.releaseReference()
		return nil, err
	}
	lease := &checkoutMutationLease{checkoutGate: checkoutGate}
	fail := func(err error) (*CheckoutMutationToken, error) {
		lease.release()
		return nil, err
	}

	l.coordMu.Lock()
	coordinator := l.coordinators[checkoutID]
	closing := l.coordinatorClosing
	l.coordMu.Unlock()
	cleanRoot := filepath.Clean(expectedRoot)
	if closing || coordinator == nil || !coordinator.Running() ||
		filepath.Clean(coordinator.root) != cleanRoot {
		return fail(fmt.Errorf("indexer: checkout %q has no live coordinator at %q", checkoutID, cleanRoot))
	}

	checkout, found, err := l.catalog.GetCheckout(ctx, checkoutID)
	if err != nil {
		return fail(fmt.Errorf("indexer: read checkout mutation identity: %w", err))
	}
	if !found || checkout.State != store_sqlite.CheckoutStateReady ||
		checkout.ActiveIntentTransitionID != "" || filepath.Clean(checkout.RootPath) != cleanRoot {
		return fail(fmt.Errorf("indexer: checkout %q is not ready at %q", checkoutID, cleanRoot))
	}
	route, routed, moving, err := l.catalog.GetCheckoutRouteSnapshot(ctx, checkoutID)
	if err != nil {
		return fail(fmt.Errorf("indexer: read checkout mutation route: %w", err))
	}
	if moving || !routed || !graphview.RouteReady(route) || route.GraphID != coordinator.graphID {
		return fail(fmt.Errorf("indexer: checkout %q has no stable ready mutation route", checkoutID))
	}
	graphGate, err := fences.lease(ctx, fences.graphs, route.GraphID)
	if err != nil {
		return fail(err)
	}
	if err := graphGate.gate.sem.Acquire(ctx, 1); err != nil {
		graphGate.releaseReference()
		return fail(err)
	}
	lease.graphGate = graphGate

	// A base refresh may have won the graph fence after the first route read.
	// Re-read under both leases; from this point neither checkout topology nor
	// graph-wide base publication can invalidate the selected publisher.
	checkout, found, err = l.catalog.GetCheckout(ctx, checkoutID)
	if err != nil {
		return fail(fmt.Errorf("indexer: re-read checkout mutation identity: %w", err))
	}
	route, routed, moving, err = l.catalog.GetCheckoutRouteSnapshot(ctx, checkoutID)
	if err != nil {
		return fail(fmt.Errorf("indexer: re-read checkout mutation route: %w", err))
	}
	if !found || checkout.State != store_sqlite.CheckoutStateReady ||
		checkout.ActiveIntentTransitionID != "" || filepath.Clean(checkout.RootPath) != cleanRoot ||
		moving || !routed || !graphview.RouteReady(route) || route.GraphID != coordinator.graphID {
		return fail(fmt.Errorf("indexer: checkout %q moved during mutation admission", checkoutID))
	}

	// Revalidate the process-local publisher after the catalog snapshot. Once
	// destructive writers use the matching topology fence, this is redundant;
	// retaining it also makes partial startup and shutdown fail closed.
	l.coordMu.Lock()
	current := l.coordinators[checkoutID]
	closing = l.coordinatorClosing
	l.coordMu.Unlock()
	if closing || current != coordinator || !coordinator.Running() ||
		filepath.Clean(coordinator.root) != cleanRoot {
		return fail(fmt.Errorf("indexer: checkout %q publisher moved during mutation admission", checkoutID))
	}

	return &CheckoutMutationToken{
		lifecycle:          l,
		lease:              lease,
		state:              checkoutMutationTokenHeld,
		coordinator:        coordinator,
		checkoutID:         checkoutID,
		root:               cleanRoot,
		graphID:            route.GraphID,
		observedRouteEpoch: route.RouteEpoch,
	}, nil
}

// CheckoutGraphTopologyToken drains mutations composed over one base graph.
// It is used only at graph-wide invalidation boundaries such as dedicated-base
// refresh; worktrees over other graphs remain independent.
type CheckoutGraphTopologyToken struct {
	graphGate *checkoutMutationGateLease
	once      sync.Once
}

func (t *CheckoutGraphTopologyToken) Release() {
	if t == nil {
		return
	}
	t.once.Do(func() {
		if t.graphGate != nil {
			t.graphGate.releaseSemaphore(checkoutMutationReaderLimit)
		}
	})
}

func (l *CheckoutLifecycle) AcquireCheckoutGraphTopology(
	ctx context.Context, graphID string,
) (*CheckoutGraphTopologyToken, error) {
	if l == nil || graphID == "" {
		return nil, errors.New("indexer: checkout graph topology identity is unavailable")
	}
	fences := l.ensureMutationFences()
	if ctx == nil {
		ctx = context.Background()
	}
	gate, err := fences.lease(ctx, fences.graphs, graphID)
	if err != nil {
		return nil, err
	}
	if err := gate.gate.sem.Acquire(ctx, checkoutMutationReaderLimit); err != nil {
		gate.releaseReference()
		return nil, err
	}
	return &CheckoutGraphTopologyToken{graphGate: gate}, nil
}

// CheckoutFamilyTopologyToken serializes membership and primary-designation
// changes inside the named Git families. It never participates in ordinary
// source mutation admission, so a slow topology change in one family cannot
// block edits or discovery in another.
type CheckoutFamilyTopologyToken struct {
	gates []*checkoutMutationGateLease
	once  sync.Once
}

func (t *CheckoutFamilyTopologyToken) Release() {
	if t == nil {
		return
	}
	t.once.Do(func() {
		for i := len(t.gates) - 1; i >= 0; i-- {
			t.gates[i].releaseSemaphore(checkoutMutationReaderLimit)
		}
	})
}

func (l *CheckoutLifecycle) AcquireCheckoutFamilyTopology(
	ctx context.Context, familyIDs ...string,
) (*CheckoutFamilyTopologyToken, error) {
	if l == nil {
		return nil, errors.New("indexer: checkout family topology lifecycle is unavailable")
	}
	fences := l.ensureMutationFences()
	if ctx == nil {
		ctx = context.Background()
	}
	unique := make([]string, 0, len(familyIDs))
	seen := make(map[string]struct{}, len(familyIDs))
	for _, familyID := range familyIDs {
		if familyID == "" {
			continue
		}
		if _, duplicate := seen[familyID]; duplicate {
			continue
		}
		seen[familyID] = struct{}{}
		unique = append(unique, familyID)
	}
	slices.Sort(unique)
	token := &CheckoutFamilyTopologyToken{}
	leases, err := fences.leaseMany(ctx, fences.families, unique)
	if err != nil {
		return nil, err
	}
	for i, gate := range leases {
		if err := gate.gate.sem.Acquire(ctx, checkoutMutationReaderLimit); err != nil {
			for _, unacquired := range leases[i:] {
				unacquired.releaseReference()
			}
			token.Release()
			return nil, err
		}
		token.gates = append(token.gates, gate)
	}
	return token, nil
}

// CheckoutTopologyToken excludes admitted disk mutations for the named
// checkouts. IDs are sorted before acquisition, so two multi-checkout topology
// operations cannot deadlock by presenting the same component in reverse.
type CheckoutTopologyToken struct {
	gates []*checkoutMutationGateLease
	once  sync.Once
}

func (t *CheckoutTopologyToken) Release() {
	if t == nil {
		return
	}
	t.once.Do(func() {
		for i := len(t.gates) - 1; i >= 0; i-- {
			t.gates[i].releaseSemaphore(checkoutMutationReaderLimit)
		}
	})
}

// AcquireCheckoutTopology drains only the affected checkouts. Acquisitions for
// disjoint worktrees are independent even when one has to wait for a long
// publication ticket.
func (l *CheckoutLifecycle) AcquireCheckoutTopology(
	ctx context.Context, checkoutIDs ...string,
) (*CheckoutTopologyToken, error) {
	if l == nil {
		return nil, errors.New("indexer: checkout topology lifecycle is unavailable")
	}
	fences := l.ensureMutationFences()
	if ctx == nil {
		ctx = context.Background()
	}
	unique := make([]string, 0, len(checkoutIDs))
	seen := make(map[string]struct{}, len(checkoutIDs))
	for _, checkoutID := range checkoutIDs {
		if checkoutID == "" {
			continue
		}
		if _, duplicate := seen[checkoutID]; duplicate {
			continue
		}
		seen[checkoutID] = struct{}{}
		unique = append(unique, checkoutID)
	}
	slices.Sort(unique)
	token := &CheckoutTopologyToken{}
	leases, err := fences.leaseMany(ctx, fences.checkouts, unique)
	if err != nil {
		return nil, err
	}
	for i, gate := range leases {
		if err := gate.gate.sem.Acquire(ctx, checkoutMutationReaderLimit); err != nil {
			for _, unacquired := range leases[i:] {
				unacquired.releaseReference()
			}
			token.Release()
			return nil, err
		}
		token.gates = append(token.gates, gate)
	}
	return token, nil
}

type checkoutTopologyHeldContextKey struct{}
type checkoutFamilyTopologyHeldContextKey struct{}

func (l *CheckoutLifecycle) reconcileCheckoutFamilyTopologyGuard(
	ctx context.Context, familyIDs ...string,
) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	held, _ := ctx.Value(checkoutFamilyTopologyHeldContextKey{}).(map[string]struct{})
	missing := make([]string, 0, len(familyIDs))
	seen := make(map[string]struct{}, len(familyIDs))
	for _, familyID := range familyIDs {
		if familyID == "" {
			continue
		}
		if _, alreadyHeld := held[familyID]; alreadyHeld {
			continue
		}
		if _, duplicate := seen[familyID]; duplicate {
			continue
		}
		seen[familyID] = struct{}{}
		missing = append(missing, familyID)
	}
	if len(missing) == 0 {
		return ctx, func() {}, nil
	}
	topology, err := l.AcquireCheckoutFamilyTopology(ctx, missing...)
	if err != nil {
		return ctx, nil, err
	}
	next := make(map[string]struct{}, len(held)+len(familyIDs))
	for familyID := range held {
		next[familyID] = struct{}{}
	}
	for _, familyID := range familyIDs {
		if familyID != "" {
			next[familyID] = struct{}{}
		}
	}
	guarded := context.WithValue(ctx, checkoutFamilyTopologyHeldContextKey{}, next)
	return guarded, topology.Release, nil
}

func (l *CheckoutLifecycle) reconcileCheckoutTopologyGuard(
	ctx context.Context, checkoutIDs ...string,
) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	held, _ := ctx.Value(checkoutTopologyHeldContextKey{}).(map[string]struct{})
	missing := make([]string, 0, len(checkoutIDs))
	seen := make(map[string]struct{}, len(checkoutIDs))
	for _, checkoutID := range checkoutIDs {
		if checkoutID == "" {
			continue
		}
		if _, alreadyHeld := held[checkoutID]; alreadyHeld {
			continue
		}
		if _, duplicate := seen[checkoutID]; duplicate {
			continue
		}
		seen[checkoutID] = struct{}{}
		missing = append(missing, checkoutID)
	}
	if len(missing) == 0 {
		return ctx, func() {}, nil
	}
	topology, err := l.AcquireCheckoutTopology(ctx, missing...)
	if err != nil {
		return ctx, nil, err
	}
	next := make(map[string]struct{}, len(held)+len(checkoutIDs))
	for checkoutID := range held {
		next[checkoutID] = struct{}{}
	}
	for _, checkoutID := range checkoutIDs {
		if checkoutID != "" {
			next[checkoutID] = struct{}{}
		}
	}
	guarded := context.WithValue(ctx, checkoutTopologyHeldContextKey{}, next)
	return guarded, topology.Release, nil
}

func (l *CheckoutLifecycle) reconcileFamilyCheckoutTopologyGuard(
	ctx context.Context,
	familyIDs []string,
	checkoutIDs []string,
) (context.Context, func(), error) {
	familyCtx, releaseFamily, err := l.reconcileCheckoutFamilyTopologyGuard(ctx, familyIDs...)
	if err != nil {
		return ctx, nil, err
	}
	checkoutCtx, releaseCheckout, err := l.reconcileCheckoutTopologyGuard(
		familyCtx, checkoutIDs...,
	)
	if err != nil {
		releaseFamily()
		return ctx, nil, err
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			releaseCheckout()
			releaseFamily()
		})
	}
	return checkoutCtx, release, nil
}

func checkoutTopologyHeld(ctx context.Context, checkoutID string) bool {
	if ctx == nil || checkoutID == "" {
		return false
	}
	held, _ := ctx.Value(checkoutTopologyHeldContextKey{}).(map[string]struct{})
	_, ok := held[checkoutID]
	return ok
}
