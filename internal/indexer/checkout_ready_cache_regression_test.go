package indexer

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// TestCoordinatorCachedCommitAdoptionStaysPending pins the publication
// boundary for a cached branch switch. Clearing the old branch's dirty layer
// cannot make the new commit layer active before its own dirty layer exists.
func TestCoordinatorCachedCommitAdoptionStaysPending(t *testing.T) {
	f := newCoordinatorFixture(t)
	coordinator := f.inertCoordinator(t, CheckoutCoordinatorConfig{})

	first := coordinatorReconcile(t, coordinator)
	if first.CommitGenerationID == 0 {
		t.Fatalf("the initial route has no commit generation: %+v", first)
	}
	f.commitTreeB()
	second := coordinatorReconcile(t, coordinator)
	if second.CommitGenerationID == first.CommitGenerationID {
		t.Fatalf("the second tree reused the first generation: %+v", second)
	}

	builderGit(t, f.worktree, "reset", "--hard", "HEAD^")
	base, err := coordinator.primaryBase(context.Background())
	if err != nil {
		t.Fatalf("primaryBase: %v", err)
	}
	route := f.route()
	if route.DirtyGenerationID == 0 {
		t.Fatalf("the settled second route has no dirty generation: %+v", route)
	}
	targetTree := builderGit(t, f.worktree, "rev-parse", "HEAD^{tree}")
	var cycle CheckoutCycle
	generationID, err := coordinator.reconcileCommitSlot(
		context.Background(), base, targetTree, &route, &cycle,
	)
	if err != nil {
		t.Fatalf("adopt cached commit: %v", err)
	}
	if generationID != first.CommitGenerationID || !cycle.CommitReused {
		t.Fatalf("the first commit was not reused: generation=%d cycle=%+v", generationID, cycle)
	}

	persisted := f.route()
	if persisted.State != store_sqlite.RoutePending || graphview.RouteReady(persisted) {
		t.Fatalf("cached commit adoption exposed an active route: %+v", persisted)
	}
	if persisted.CommitGenerationID != first.CommitGenerationID || persisted.DirtyGenerationID != 0 {
		t.Fatalf("cached commit adoption published an incoherent pair: %+v", persisted)
	}
}
