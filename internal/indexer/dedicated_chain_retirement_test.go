package indexer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// dedicatedChainFixture seeds one dedicated graph and the base generations
// under it, so a test can describe a chain the way adoption leaves one: a live
// head the graph's active pointer names, the ancestry under it, and whatever
// earlier chains were replaced.
type dedicatedChainFixture struct {
	t       testing.TB
	store   *store_sqlite.Store
	catalog *store_sqlite.Catalog
	request store_sqlite.PayloadGenerationRequest
	seq     int
}

const (
	dedicatedChainFamilyID   = "family-dedicated-chain"
	dedicatedChainCheckoutID = "checkout-dedicated-chain"
	dedicatedChainGraphID    = "graph-dedicated-chain"
	dedicatedChainRepoPrefix = "repo-dedicated-chain"
)

func newDedicatedChainFixture(t *testing.T) *dedicatedChainFixture {
	t.Helper()
	inner := newSparseBuildFlightFixture(t)
	ctx := context.Background()
	catalog := inner.store.Catalog()
	now := time.Now().Unix()
	if err := catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          dedicatedChainFamilyID,
		CommonDirIdentity: "common-dedicated-chain",
		State:             "ready",
		CreatedAt:         now,
		LastSeen:          now,
	}); err != nil {
		t.Fatalf("upsert family: %v", err)
	}
	if err := catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:    dedicatedChainCheckoutID,
		Incarnation:   "incarnation-dedicated-chain",
		FamilyID:      dedicatedChainFamilyID,
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated,
		LastSeen:      now,
	}); err != nil {
		t.Fatalf("upsert owner checkout: %v", err)
	}
	f := &dedicatedChainFixture{t: t, store: inner.store, catalog: catalog}
	f.upsertGraph(0)
	f.request = payloadRequestForBuild(inner.request)
	f.request.GraphID = dedicatedChainGraphID
	f.request.CheckoutID = dedicatedChainCheckoutID
	f.request.OwnerKind = checkoutLayerOwnerKind
	f.request.GenerationKind = DedicatedBaseGenerationKind
	return f
}

func (f *dedicatedChainFixture) upsertGraph(active int64) {
	f.t.Helper()
	if err := f.catalog.UpsertDedicatedGraph(context.Background(), store_sqlite.DedicatedGraph{
		GraphID:            dedicatedChainGraphID,
		OwnerCheckoutID:    dedicatedChainCheckoutID,
		RepoPrefix:         dedicatedChainRepoPrefix,
		FamilyID:           dedicatedChainFamilyID,
		IsPrimaryBase:      true,
		ActiveGenerationID: active,
		State:              "ready",
	}); err != nil {
		f.t.Fatalf("upsert dedicated graph (active %d): %v", active, err)
	}
}

// base seeds one dedicated base generation over parent.
func (f *dedicatedChainFixture) base(state store_sqlite.ViewGenerationState, parent int64) int64 {
	f.t.Helper()
	f.seq++
	request := f.request
	request.LayerID = fmt.Sprintf("dedicated-base-%02d", f.seq)
	request.BaseGenerationID = parent
	return seedLifecycleListingGeneration(f.t, f.store, request, state)
}

// dependentLayer seeds a checkout's commit layer over a base, which is the
// reference that keeps a replaced base alive until the dependent recomposes.
func (f *dedicatedChainFixture) dependentLayer(checkoutID string, parent int64) int64 {
	f.t.Helper()
	if err := f.catalog.UpsertCheckout(context.Background(), store_sqlite.Checkout{
		CheckoutID:    checkoutID,
		Incarnation:   "incarnation-" + checkoutID,
		FamilyID:      dedicatedChainFamilyID,
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeAutomatic,
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
		LastSeen:      time.Now().Unix(),
	}); err != nil {
		f.t.Fatalf("upsert dependent checkout %s: %v", checkoutID, err)
	}
	f.seq++
	request := f.request
	request.GenerationKind = CommitLayerGenerationKind
	request.CheckoutID = checkoutID
	request.LayerID = fmt.Sprintf("dependent-layer-%02d", f.seq)
	request.BaseGenerationID = parent
	return seedLifecycleListingGeneration(f.t, f.store, request, store_sqlite.ViewGenerationReady)
}

// activate points the graph at its live head, the way adoption does.
func (f *dedicatedChainFixture) activate(generationID int64) {
	f.t.Helper()
	f.upsertGraph(generationID)
}

// lifecycle builds the sweep with an explicit retention window. A negative
// window retains no replaced chain at all, which is what makes a one-sweep
// assertion about collection possible.
func (f *dedicatedChainFixture) lifecycle(window int) *CheckoutLifecycle {
	f.t.Helper()
	lifecycle := newGenerationRetirementLifecycle(f.store, time.Now())
	lifecycle.supersededChainRetention = window
	return lifecycle
}

// TestDedicatedChainRetirementRetiresAReplacedRootAfterTwoAdvances is the leak
// this item closes. Two advances that each allocated a new full root leave two
// replaced chains behind; with a one-chain window the older of them is the one
// nothing is left to read, and the sweep collects it.
//
// Before the change the sweep enumerated no dedicated base at all —
// readyLayerRetirementCandidates dropped the kind and the discarded scan
// deferred to the owner checkout's coordinator — so both replaced roots stayed
// in the database for the life of the installation.
func TestDedicatedChainRetirementRetiresAReplacedRootAfterTwoAdvances(t *testing.T) {
	f := newDedicatedChainFixture(t)
	ctx := context.Background()

	first := f.base(store_sqlite.ViewGenerationSuperseded, 0)
	second := f.base(store_sqlite.ViewGenerationSuperseded, 0)
	live := f.base(store_sqlite.ViewGenerationReady, 0)
	f.activate(live)

	lifecycle := f.lifecycle(1)
	if retired := lifecycle.sweepRetirements(ctx); retired != 1 {
		t.Fatalf("retired generations = %d, want only the oldest replaced root", retired)
	}
	requireGenerationRetired(t, f.store, first)
	requireCatalogGenerationPresent(t, f.store, second)
	requireCatalogGenerationPresent(t, f.store, live)
}

// TestDedicatedChainRetirementRetainsTheConfiguredWindow is the bound on the
// other side: a window wide enough to cover both replaced chains collects
// neither, so a revert onto either of them still re-adopts instead of
// rebuilding.
func TestDedicatedChainRetirementRetainsTheConfiguredWindow(t *testing.T) {
	f := newDedicatedChainFixture(t)
	ctx := context.Background()

	first := f.base(store_sqlite.ViewGenerationSuperseded, 0)
	second := f.base(store_sqlite.ViewGenerationSuperseded, 0)
	live := f.base(store_sqlite.ViewGenerationReady, 0)
	f.activate(live)

	lifecycle := f.lifecycle(2)
	if retired := lifecycle.sweepRetirements(ctx); retired != 0 {
		t.Fatalf("retired generations inside the retention window = %d, want 0", retired)
	}
	for _, generationID := range []int64{first, second, live} {
		requireCatalogGenerationPresent(t, f.store, generationID)
	}
}

// TestDedicatedChainRetirementNeverRetiresTheLiveAncestry pins the half the
// widened enumeration could get wrong. Every ancestor of the active pointer is
// ready and unreferenced by any route, so a kind filter widened without a chain
// walk would offer the whole live chain on every sweep.
func TestDedicatedChainRetirementNeverRetiresTheLiveAncestry(t *testing.T) {
	f := newDedicatedChainFixture(t)
	ctx := context.Background()

	root := f.base(store_sqlite.ViewGenerationReady, 0)
	middle := f.base(store_sqlite.ViewGenerationReady, root)
	head := f.base(store_sqlite.ViewGenerationReady, middle)
	f.activate(head)
	replaced := f.base(store_sqlite.ViewGenerationSuperseded, 0)

	lifecycle := f.lifecycle(-1)
	candidates := lifecycle.orphanedGenerations(ctx, nil, nil)
	requireOnlyRetirementCandidate(t, candidates, replaced)
	if retired := lifecycle.sweepRetirements(ctx); retired != 1 {
		t.Fatalf("retired generations = %d, want only the replaced chain", retired)
	}
	requireGenerationRetired(t, f.store, replaced)
	for _, generationID := range []int64{root, middle, head} {
		requireCatalogGenerationPresent(t, f.store, generationID)
	}
}

// TestDedicatedChainRetirementDrainsAReplacedChainInOneSweep covers the
// cascade. Adoption labels only the head it replaced; the ancestry under that
// head is still ready, and nothing else would ever name it again. Offering the
// whole replaced chain in one pass — newest first, which is the only order
// retirement accepts — is what keeps a re-rooted chain from draining one
// generation per sweep.
func TestDedicatedChainRetirementDrainsAReplacedChainInOneSweep(t *testing.T) {
	f := newDedicatedChainFixture(t)
	ctx := context.Background()

	replacedRoot := f.base(store_sqlite.ViewGenerationReady, 0)
	replacedDelta := f.base(store_sqlite.ViewGenerationSuperseded, replacedRoot)
	live := f.base(store_sqlite.ViewGenerationReady, 0)
	f.activate(live)

	lifecycle := f.lifecycle(-1)
	if retired := lifecycle.sweepRetirements(ctx); retired != 2 {
		t.Fatalf("retired generations = %d, want the whole replaced chain", retired)
	}
	requireGenerationRetired(t, f.store, replacedDelta)
	requireGenerationRetired(t, f.store, replacedRoot)
	requireCatalogGenerationPresent(t, f.store, live)
}

// TestDedicatedChainRetirementWaitsForALeaseOnASupersededBase keeps the label
// from being permission. A replaced base a reader is holding open survives its
// own sweep and is collected on the one after the lease drops.
func TestDedicatedChainRetirementWaitsForALeaseOnASupersededBase(t *testing.T) {
	f := newDedicatedChainFixture(t)
	ctx := context.Background()

	live := f.base(store_sqlite.ViewGenerationReady, 0)
	f.activate(live)
	replaced := f.base(store_sqlite.ViewGenerationSuperseded, 0)

	lifecycle := f.lifecycle(-1)
	lease := lifecycle.leases.Acquire(replaced)
	if retired := lifecycle.sweepRetirements(ctx); retired != 0 {
		t.Fatalf("retired generations under a live lease = %d, want 0", retired)
	}
	requireCatalogGenerationPresent(t, f.store, replaced)

	lease.Release()
	if retired := lifecycle.sweepRetirements(ctx); retired != 1 {
		t.Fatalf("retired generations after the lease drained = %d, want 1", retired)
	}
	requireGenerationRetired(t, f.store, replaced)
	requireCatalogGenerationPresent(t, f.store, live)
}

// TestDedicatedChainRetirementKeepsADependentsBaseUntilItRecomposes is the
// gate-5 clause: a dependent worktree built its layer over the base that was
// just replaced, and that layer still composes over it. The base is offered and
// refused for as long as the dependent names it, and collected in the sweep
// after the dependent's own layer goes.
func TestDedicatedChainRetirementKeepsADependentsBaseUntilItRecomposes(t *testing.T) {
	f := newDedicatedChainFixture(t)
	ctx := context.Background()
	const dependentCheckoutID = "checkout-dedicated-chain-dependent"

	live := f.base(store_sqlite.ViewGenerationReady, 0)
	f.activate(live)
	replaced := f.base(store_sqlite.ViewGenerationSuperseded, 0)
	dependent := f.dependentLayer(dependentCheckoutID, replaced)
	if err := f.catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
		CheckoutID:         dependentCheckoutID,
		GraphID:            dedicatedChainGraphID,
		CommitGenerationID: dependent,
		State:              store_sqlite.RouteActive,
	}); err != nil {
		t.Fatalf("upsert dependent route: %v", err)
	}

	lifecycle := f.lifecycle(-1)
	if retired := lifecycle.sweepRetirements(ctx); retired != 0 {
		t.Fatalf("retired generations under a routed dependent = %d, want 0", retired)
	}
	requireCatalogGenerationPresent(t, f.store, replaced)
	requireCatalogGenerationPresent(t, f.store, dependent)

	// The dependent recomposes: its old route is withdrawn, so the layer and
	// the base it named become collectable in that order.
	if err := f.catalog.DeleteCheckoutRoute(ctx, dependentCheckoutID); err != nil {
		t.Fatalf("delete dependent route: %v", err)
	}
	if retired := lifecycle.sweepRetirements(ctx); retired != 2 {
		t.Fatalf("retired generations after the dependent recomposed = %d, want 2", retired)
	}
	requireGenerationRetired(t, f.store, dependent)
	requireGenerationRetired(t, f.store, replaced)
	requireCatalogGenerationPresent(t, f.store, live)
}

// TestDedicatedChainRetirementDoesNotDeferToTheOwnersCoordinator is the second
// half of the enumeration defect. Both scans skip a checkout with a live
// coordinator because that coordinator owns everything built for it — but a
// dedicated base is not built for the checkout's route and is not held by its
// reuse cache. The publisher owns it and the graph's active pointer names it,
// so a base has to be decided even while its owner checkout is being served.
func TestDedicatedChainRetirementDoesNotDeferToTheOwnersCoordinator(t *testing.T) {
	f := newDedicatedChainFixture(t)
	ctx := context.Background()

	live := f.base(store_sqlite.ViewGenerationReady, 0)
	f.activate(live)
	replaced := f.base(store_sqlite.ViewGenerationSuperseded, 0)

	lifecycle := f.lifecycle(-1)
	served := map[string]struct{}{dedicatedChainCheckoutID: {}}
	candidates := lifecycle.orphanedGenerations(ctx, served, nil)
	requireOnlyRetirementCandidate(t, candidates, replaced)
}

// TestDedicatedChainRetirementLeavesAGraphItCannotReadAlone proves the sweep
// fails closed. A catalog read that cannot say what the live chain is, is not
// evidence that there is no live chain.
func TestDedicatedChainRetirementLeavesAGraphItCannotReadAlone(t *testing.T) {
	f := newDedicatedChainFixture(t)
	ctx := context.Background()

	live := f.base(store_sqlite.ViewGenerationReady, 0)
	f.activate(live)
	replaced := f.base(store_sqlite.ViewGenerationSuperseded, 0)

	lifecycle := f.lifecycle(-1)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got := lifecycle.dedicatedChainRetirementCandidates(cancelled, []store_sqlite.ViewGeneration{{
		GenerationID:   replaced,
		GraphID:        dedicatedChainGraphID,
		CheckoutID:     dedicatedChainCheckoutID,
		OwnerKind:      checkoutLayerOwnerKind,
		GenerationKind: DedicatedBaseGenerationKind,
		State:          store_sqlite.ViewGenerationSuperseded,
	}}); len(got) != 0 {
		t.Fatalf("candidates from an unreadable graph = %v, want none", got)
	}
	requireCatalogGenerationPresent(t, f.store, replaced)
	requireCatalogGenerationPresent(t, f.store, live)
}

// TestDedicatedChainRetirementRetainsNothingForADeletedGraph is the other end
// of the leak. The retention window exists so a revert can re-adopt a replaced
// chain instead of rebuilding it; once the dedicated_graphs row is gone there
// is no pointer to compose those bases into anything and nothing left to revert
// into, so the rationale is void and the window must not run.
//
// Nothing else would ever reach them: the deleted-graph scan filters to ready
// rows (ListViewGenerations States ViewGenerationReady, MissingGraph true) and a
// superseded base is exactly what this item's adoption write produces, so a
// replaced chain under a deleted graph is enumerated here or nowhere. Graph
// deletion is on this pass's own production path — untrack sweeps retirements
// straight after it.
func TestDedicatedChainRetirementRetainsNothingForADeletedGraph(t *testing.T) {
	f := newDedicatedChainFixture(t)
	ctx := context.Background()

	first := f.base(store_sqlite.ViewGenerationSuperseded, 0)
	second := f.base(store_sqlite.ViewGenerationSuperseded, 0)
	third := f.base(store_sqlite.ViewGenerationSuperseded, 0)
	live := f.base(store_sqlite.ViewGenerationReady, 0)
	f.activate(live)
	if err := f.catalog.DeleteDedicatedGraph(ctx, dedicatedChainGraphID); err != nil {
		t.Fatalf("delete dedicated graph: %v", err)
	}

	// The production default window, not a test-only one: the leak this pins
	// retained defaultSupersededDedicatedChainRetention chains on every sweep,
	// forever.
	lifecycle := f.lifecycle(0)
	if got := lifecycle.supersededChainRetentionWindow(); got != defaultSupersededDedicatedChainRetention {
		t.Fatalf("retention window = %d, want the production default %d", got, defaultSupersededDedicatedChainRetention)
	}
	if retired := lifecycle.sweepRetirements(ctx); retired != 4 {
		t.Fatalf("retired generations of a deleted graph = %d, want all 4", retired)
	}
	for _, generationID := range []int64{first, second, third, live} {
		requireGenerationRetired(t, f.store, generationID)
	}
	if retired := lifecycle.sweepRetirements(ctx); retired != 0 {
		t.Fatalf("second sweep retired %d, want an empty backlog", retired)
	}
}

// TestDedicatedChainRetirementAlwaysReOffersARetiringBase pins the clause that
// pre-empts the chain walk. A base whose fence is already committed has had its
// decision taken on an earlier pass; what is left is to finish it. Retaining it
// because some pointer still reaches it would strand a half-finished retirement
// with no other enumeration path back to it, so a retiring row is re-offered
// even when the walk retained it.
//
// The state is constructed rather than produced, which is the point: a row in
// this shape is what an interrupted retirement plus a pointer that outlived it
// leaves behind, and the sweep has to drain it rather than protect it.
func TestDedicatedChainRetirementAlwaysReOffersARetiringBase(t *testing.T) {
	f := newDedicatedChainFixture(t)
	ctx := context.Background()

	root := f.base(store_sqlite.ViewGenerationReady, 0)
	interrupted := f.base(store_sqlite.ViewGenerationReady, root)
	head := f.base(store_sqlite.ViewGenerationReady, interrupted)
	f.activate(head)
	// The fence lands after the chain is built — nothing can be built over a
	// retiring generation, which is why this state only ever arises from a
	// retirement that started and did not finish.
	if err := f.catalog.SetViewGenerationState(ctx, interrupted,
		store_sqlite.ViewGenerationRetiring, store_sqlite.ViewGenerationReady); err != nil {
		t.Fatalf("fence the interrupted generation: %v", err)
	}

	lifecycle := f.lifecycle(-1)
	// head's chain walk reaches interrupted, so only the retiring clause can
	// put it back in front of retirement.
	requireOnlyRetirementCandidate(t, lifecycle.orphanedGenerations(ctx, nil, nil), interrupted)

	// Offered is still not collected: head names it as its base, so the
	// catalog's reference predicate refuses and every row survives.
	if retired := lifecycle.sweepRetirements(ctx); retired != 0 {
		t.Fatalf("retired generations under a live dependent = %d, want 0", retired)
	}
	for _, generationID := range []int64{root, interrupted, head} {
		requireCatalogGenerationPresent(t, f.store, generationID)
	}
}

// TestDedicatedChainRetirementCapsOneSweepsBacklog pins the per-sweep bound. A
// database carrying a long-leaked backlog — which is what every installation
// that ran before this item has — must drain over several passes rather than
// hand one sweep an unbounded batch of payload deletions.
func TestDedicatedChainRetirementCapsOneSweepsBacklog(t *testing.T) {
	f := newDedicatedChainFixture(t)
	ctx := context.Background()

	// Rows for a graph that no longer exists: nothing is retained, so the cap
	// is the only thing that can bound the result.
	rows := make([]store_sqlite.ViewGeneration, 0, maxDedicatedChainRetirementCandidates+64)
	for i := 0; i < cap(rows); i++ {
		rows = append(rows, store_sqlite.ViewGeneration{
			GenerationID:   int64(i + 1),
			GraphID:        "graph-that-was-deleted",
			CheckoutID:     dedicatedChainCheckoutID,
			OwnerKind:      checkoutLayerOwnerKind,
			GenerationKind: DedicatedBaseGenerationKind,
			State:          store_sqlite.ViewGenerationSuperseded,
		})
	}
	lifecycle := f.lifecycle(0)
	got := lifecycle.dedicatedChainRetirementCandidates(ctx, rows)
	if len(got) != maxDedicatedChainRetirementCandidates {
		t.Fatalf("candidates from a %d-row backlog = %d, want the sweep bound %d",
			len(rows), len(got), maxDedicatedChainRetirementCandidates)
	}
	// Newest first is the only order a chain can be collected in.
	for i := 1; i < len(got); i++ {
		if got[i-1].GenerationID <= got[i].GenerationID {
			t.Fatalf("candidate %d (%d) is not newer than %d (%d)",
				i-1, got[i-1].GenerationID, i, got[i].GenerationID)
		}
	}
}
