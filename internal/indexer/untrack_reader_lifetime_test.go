package indexer

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// sentinelNode is one generation-zero row belonging to a repository, the
// smallest thing a pinned reader can lose when a public untrack purges payload
// out from under it.
func sentinelNode(prefix, name string) *graph.Node {
	return &graph.Node{
		ID:         prefix + "/" + name + ".go::" + name,
		Name:       name,
		Kind:       graph.KindFunction,
		FilePath:   prefix + "/" + name + ".go",
		RepoPrefix: prefix,
	}
}

// TestPublicUntrackKeepsAPinnedReadersCorpusUntilItCloses is the design's
// confirmed defect (docs/incremental-indexing-write-amplification.md, the
// public-untrack reader-lifetime paragraph), reduced to the lifecycle: a
// request that pinned the repository's base corpus through the serving door
// must keep reading coherent rows across a concurrent public untrack, and the
// untrack must finish only after that reader closes.
func TestPublicUntrackKeepsAPinnedReadersCorpusUntilItCloses(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	ctx := context.Background()

	root := f.gitRepo("pinned-reader")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: root, Name: "pinned-reader"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	prefix := tracked.Prefix

	node := sentinelNode(prefix, "PinnedSentinel")
	f.store.AddBatch([]*graph.Node{node}, nil)
	require.NotNil(t, f.store.GetNode(node.ID), "the sentinel was never in the corpus")

	// The serving door every request takes (internal/mcp/view_request.go:892,
	// internal/server/handler.go:400): a generation-zero pin plus the owner
	// half that speaks for the repository whose rows it is about to read.
	pin := f.lc.ViewLeases().AcquireBaseCorpus(prefix)
	require.NotNil(t, pin)
	require.True(t, pin.OwnerPinned(), "the serving pin carries no repository owner")

	out, untrackErr := f.lc.Untrack(ctx, root)
	if untrackErr != nil {
		require.ErrorIs(t, untrackErr, ErrRepositoryCleanupPending)
	}
	require.True(t, out.Pending, "untrack completed while a pinned reader still held the repository")

	require.NotNil(t, f.store.GetNode(node.ID),
		"public untrack purged a pinned reader's corpus out from under it")

	pin.Release()
	finishRepositoryCleanup(t, f.lc, prefix)

	require.Nil(t, f.store.GetNode(node.ID), "finalized cleanup left the repository's payload behind")
	require.Nil(t, f.mi.GetMetadata(prefix), "finalized cleanup left the repository in the corpus")
}

// TestAPinnedUntrackDoesNotCoupleUnrelatedRepositories is the other half of the
// design's verdict on the same defect: the reader protection may not be bought
// by blocking generation zero globally, because generation zero is shared. A
// repository held open by its own reader must not hold any other repository's
// teardown.
func TestAPinnedUntrackDoesNotCoupleUnrelatedRepositories(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	ctx := context.Background()

	heldRoot := f.gitRepo("coupled-held")
	held, err := f.lc.Register(ctx, config.RepoEntry{Path: heldRoot, Name: "coupled-held"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, held.CatalogErr)
	otherRoot := f.gitRepo("coupled-other")
	other, err := f.lc.Register(ctx, config.RepoEntry{Path: otherRoot, Name: "coupled-other"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, other.CatalogErr)

	heldNode := sentinelNode(held.Prefix, "HeldSentinel")
	otherNode := sentinelNode(other.Prefix, "OtherSentinel")
	f.store.AddBatch([]*graph.Node{heldNode, otherNode}, nil)

	pin := f.lc.ViewLeases().AcquireBaseCorpus(held.Prefix)
	require.NotNil(t, pin)
	require.True(t, pin.OwnerPinned())

	out, untrackErr := f.lc.Untrack(ctx, heldRoot)
	if untrackErr != nil {
		require.ErrorIs(t, untrackErr, ErrRepositoryCleanupPending)
	}
	require.True(t, out.Pending)

	// The unrelated repository is untouched and still admits readers: its own
	// owner never entered the held repository's cleanup.
	require.False(t, f.lc.RepositoryAdmissionClosed(other.Prefix),
		"one repository's pending untrack closed an unrelated repository's admission")
	sibling := f.lc.ViewLeases().AcquireBaseCorpus(other.Prefix)
	require.True(t, sibling.OwnerPinned(), "an unrelated repository stopped admitting readers")
	sibling.Release()
	require.NotNil(t, f.store.GetNode(otherNode.ID))

	// And it can be torn down to completion while the held one's
	// generation-zero reader is still live.
	otherOut, otherErr := f.lc.Untrack(ctx, otherRoot)
	require.NoError(t, otherErr, "an unrelated untrack waited behind another repository's reader")
	require.False(t, otherOut.Pending, "an unrelated untrack was made pending by another repository's reader")
	require.Nil(t, f.store.GetNode(otherNode.ID), "the unrelated repository kept its payload")
	require.NotNil(t, f.store.GetNode(heldNode.ID), "the held repository lost its pinned corpus")

	pin.Release()
	finishRepositoryCleanup(t, f.lc, held.Prefix)
	require.Nil(t, f.store.GetNode(heldNode.ID))
}

// TestACoordinatorHoldsItsRepositoryOwnerUntilItsLoopEnds is the worker half of
// the same rule. A build loop reads the repository's payload and writes
// generations into it for as long as it runs, so the admission its constructor
// took has to last as long as the loop does — not as long as the constructor.
func TestACoordinatorHoldsItsRepositoryOwnerUntilItsLoopEnds(t *testing.T) {
	f := newFamilyFixture(t, "coordinator-owner")
	t.Cleanup(f.close)
	ctx := context.Background()

	// Start from a family running nothing, so the only admission under test is
	// the one the coordinator built below takes.
	f.lc.dropCoordinator(f.automatic.CheckoutID)
	require.Zero(t, f.lc.liveCoordinators(""))

	checkout, found, err := f.catalog.GetCheckout(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	coordinator, err := f.lc.buildCoordinator(ctx, f.primaryGraph, checkout)
	require.NoError(t, err)
	require.NotNil(t, coordinator)
	require.True(t, coordinator.Running())

	// The first step of every public untrack (repository_cleanup.go:141):
	// close the repository's admission and take the drain the cleanup waits on
	// before it retires generations and purges payload.
	_, drain, closed, err := f.lc.closeRepositoryAdmission(ctx, f.primaryGraph)
	require.NoError(t, err)
	require.True(t, closed)
	require.NotNil(t, drain)
	select {
	case <-drain.Done():
		t.Fatal("the repository drained while a build loop was still running over its payload")
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, coordinator.Close())
	select {
	case <-drain.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the repository never drained after its build loop ended")
	}
}

// TestAConstructorAdmittedIntoAClosingRepositoryClosesWhatItStarted covers the
// one ordering a coordinator-lifetime admission opens up: a constructor that
// was admitted before the close, and records its actor after the cleanup's
// first snapshot of the running actors, would otherwise hold a drain open that
// nothing is left to close.
func TestAConstructorAdmittedIntoAClosingRepositoryClosesWhatItStarted(t *testing.T) {
	f := newFamilyFixture(t, "constructor-race")
	t.Cleanup(f.close)
	ctx := context.Background()

	f.lc.dropCoordinator(f.automatic.CheckoutID)
	require.Zero(t, f.lc.liveCoordinators(""))

	// The seam runs inside buildCoordinator, after its admission and before the
	// coordinator exists — exactly the window a concurrent untrack closes in.
	var closedDrain *graphview.RepositoryDrain
	f.lc.configSnapshot = func(
		index config.IndexConfig, prefix, workspaceID, projectID string,
	) (config.IndexConfig, string, error) {
		if closedDrain == nil {
			_, drain, closed, err := f.lc.closeRepositoryAdmission(ctx, f.primaryGraph)
			require.NoError(f.t, err)
			require.True(f.t, closed)
			closedDrain = drain
		}
		return snapshotDedicatedBaseConfig(index, prefix, workspaceID, projectID)
	}

	checkout, found, err := f.catalog.GetCheckout(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	coordinator, err := f.lc.buildCoordinator(ctx, f.primaryGraph, checkout)
	require.Error(t, err, "a constructor kept a coordinator over a repository that stopped admitting")
	require.Nil(t, coordinator)
	require.NotNil(t, closedDrain, "the seam never ran; the window was not exercised")

	require.Zero(t, f.lc.liveCoordinators(""), "the refused constructor left its build loop running")
	select {
	case <-closedDrain.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the refused constructor kept the repository's admission pinned")
	}
}

// budgetYieldingPayload makes the first retirement attempt of every generation
// stop the way a real sweep stops when it spends its budget: the fence stays,
// the rows are a strict subset of what they were, and the next pass continues
// (store_sqlite/payload_generation_sweep.go:72-85).
type budgetYieldingPayload struct {
	graph.Store

	mu      sync.Mutex
	yielded map[int64]bool
	calls   int
}

func (p *budgetYieldingPayload) RetirePayloadGeneration(ctx context.Context, id int64, inUse func(int64) bool) error {
	p.mu.Lock()
	p.calls++
	first := !p.yielded[id]
	p.yielded[id] = true
	p.mu.Unlock()
	if first {
		return fmt.Errorf("%w: generation %d", store_sqlite.ErrPayloadSweepBudgetExhausted, id)
	}
	return p.Store.(*store_sqlite.Store).RetirePayloadGeneration(ctx, id, inUse)
}

func (p *budgetYieldingPayload) WaitPayloadBuildFlights(ctx context.Context, ids ...int64) error {
	return p.Store.(*store_sqlite.Store).WaitPayloadBuildFlights(ctx, ids...)
}

func (p *budgetYieldingPayload) PayloadBuildFlightActive(id int64) bool {
	return p.Store.(*store_sqlite.Store).PayloadBuildFlightActive(id)
}

func (p *budgetYieldingPayload) PurgeRepo(prefix string) error {
	return p.Store.(*store_sqlite.Store).PurgeRepo(prefix)
}

func (p *budgetYieldingPayload) yieldCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// TestASweepBudgetYieldLeavesTheUntrackPendingAndResumes states the rule the
// store's own budget documentation asks this file for: a sweep that stops on
// its budget is a yield, not a failure. Reported as an error it reaches
// reconcile/saga.go, which then skips DeleteDedicatedGraph and turns a slow
// untrack into a failed one; reported as pending it goes back on the cleanup
// runtime's retry and the sweep resumes.
func TestASweepBudgetYieldLeavesTheUntrackPendingAndResumes(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	ctx := context.Background()

	root := f.gitRepo("sweep-budget")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: root, Name: "sweep-budget"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	// One positive generation for the graph, so the untrack has something to
	// retire before it can purge payload.
	generation, handle, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:      checkoutLayerOwnerKind,
		GraphID:        tracked.GraphID,
		CheckoutID:     tracked.CheckoutID,
		GenerationKind: "commit",
		LayerID:        "sweep-budget-layer",
		TreeOID:        "tree-sweep-budget",
		ConfigHash:     "config-sweep-budget",
	})
	require.NoError(t, err)
	require.Positive(t, generation)
	handle.AddBatch([]*graph.Node{sentinelNode(tracked.Prefix, "BudgetSentinel")}, nil)
	require.NoError(t, f.store.PublishPayloadGeneration(ctx, generation, 1))

	payload := &budgetYieldingPayload{Store: f.store, yielded: map[int64]bool{}}
	f.mi.graph = payload

	out, untrackErr := f.lc.Untrack(ctx, root)
	require.NoError(t, untrackErr,
		"a sweep that yielded on its budget was reported as a hard cleanup failure")
	require.True(t, out.Pending, "a yielded sweep did not leave the untrack retryable")
	require.Positive(t, payload.yieldCount())

	// The yield is resumable: the same saga runs the sweep again and finishes.
	finishRepositoryCleanup(t, f.lc, tracked.Prefix)
	require.Nil(t, f.mi.GetMetadata(tracked.Prefix), "the resumed cleanup never completed")
	_, found, err := f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
	require.NoError(t, err)
	require.False(t, found, "the resumed cleanup left the dedicated graph behind")
}

// TestLifecycleCloseStopsAnOffRouteCoordinatorBeforeWaitingOnItsAdmission is
// the shutdown counterpart. A transition drives a whole rebuild with a
// coordinator before anything registers it, so at shutdown that loop is in
// started and in no registry — and it now holds its repository's owner
// admission until it ends. Close must therefore stop the started actors before
// it waits for the admissions to drain, or it waits on a build loop nothing
// has been asked to stop.
func TestLifecycleCloseStopsAnOffRouteCoordinatorBeforeWaitingOnItsAdmission(t *testing.T) {
	f := newFamilyFixture(t, "close-off-route")
	t.Cleanup(f.close)
	ctx := context.Background()

	f.lc.dropCoordinator(f.automatic.CheckoutID)
	checkout, found, err := f.catalog.GetCheckout(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	// Built and never installed: the off-route shape.
	coordinator, err := f.lc.buildCoordinator(ctx, f.primaryGraph, checkout)
	require.NoError(t, err)
	require.NotNil(t, coordinator)
	require.True(t, coordinator.Running())
	require.Empty(t, f.lc.coordinators, "the actor under test reached the registry")

	closed := make(chan error, 1)
	go func() { closed <- f.lc.Close() }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(60 * time.Second):
		t.Fatal("Close never returned: it waited for an admission held by a build loop nothing closed")
	}
	require.False(t, coordinator.Running(), "Close left an off-route build loop running")
}
