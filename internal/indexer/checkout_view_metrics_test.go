package indexer

import (
	"context"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/viewmetrics"
)

// What the coordinator and the lifecycle report about themselves.
//
// The counters are the aggregate half of the view lifecycle's observability:
// the log lines beside them carry the checkout and generation ids, and these
// carry the shape. Both halves have to be pinned — a counter that stops being
// emitted is invisible until someone needs it, which is the worst possible
// moment to find out.

// counterDelta reports how much one series moved between two snapshots. Every
// assertion here is a delta rather than an absolute so it cannot be perturbed
// by whatever else in the package touched the process registry first.
func counterDelta(before, after viewmetrics.Snapshot, key string) int64 {
	return after.Counters[key] - before.Counters[key]
}

func cycleKey(outcome string) string {
	return viewmetrics.CoordinatorCycleTotal + "{outcome=" + outcome + "}"
}

// TestCoordinatorCycleOutcomesAreCounted drives the two cycles every checkout
// goes through — the first, which builds both slots, and the next one, which
// finds nothing to do — and pins the outcomes each reports.
func TestCoordinatorCycleOutcomesAreCounted(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})

	before := viewmetrics.Read()
	c.cycle(context.Background())
	afterFirst := viewmetrics.Read()

	if got := counterDelta(before, afterFirst, cycleKey(viewmetrics.OutcomeBuiltCommit)); got != 1 {
		t.Fatalf("first cycle built_commit = %d, want 1", got)
	}
	if got := counterDelta(before, afterFirst, cycleKey(viewmetrics.OutcomeBuiltDirty)); got != 1 {
		t.Fatalf("first cycle built_dirty = %d, want 1", got)
	}
	if got := counterDelta(before, afterFirst, cycleKey(viewmetrics.OutcomeSkipped)); got != 0 {
		t.Fatalf("a cycle that built both slots also counted as skipped (%d)", got)
	}

	c.cycle(context.Background())
	afterSecond := viewmetrics.Read()
	if got := counterDelta(afterFirst, afterSecond, cycleKey(viewmetrics.OutcomeSkipped)); got != 1 {
		t.Fatalf("second cycle skipped = %d, want 1", got)
	}
	if got := counterDelta(afterFirst, afterSecond, cycleKey(viewmetrics.OutcomeBuiltCommit)); got != 0 {
		t.Fatalf("a settled checkout rebuilt its commit slot (%d)", got)
	}
}

// TestCoordinatorCommitReuseIsCountedAsAdoption pins the branch-switch cache's
// own outcome: a cycle that re-routes a generation an earlier cycle built is
// an adoption, not a build, and the two must not be one number.
func TestCoordinatorCommitReuseIsCountedAsAdoption(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})

	commitA := builderGit(t, f.worktree, "rev-parse", "HEAD")
	coordinatorReconcile(t, c)
	f.commitTreeB()
	coordinatorReconcile(t, c)
	// Detached, the way the reuse test arrives back at A's tree: the cache is
	// keyed by the tree, not by the ref that reached it.
	builderGit(t, f.worktree, "checkout", "--detach", commitA)

	before := viewmetrics.Read()
	c.cycle(context.Background())
	after := viewmetrics.Read()

	if got := counterDelta(before, after, cycleKey(viewmetrics.OutcomeAdoptedCommit)); got != 1 {
		t.Fatalf("switching back adopted_commit = %d, want 1", got)
	}
	if got := counterDelta(before, after, cycleKey(viewmetrics.OutcomeBuiltCommit)); got != 0 {
		t.Fatalf("switching back rebuilt the commit layer (%d)", got)
	}
}

// TestCoordinatorTornBuildIsCountedAsRescheduledNotCASLost separates the two
// conditions that both leave a cycle rescheduled. A working tree that would
// not settle under two builds is not a lost compare-and-set, and reading one
// number for both would hide whichever is rarer — which is the one that
// matters, because they call for opposite responses.
func TestCoordinatorTornBuildIsCountedAsRescheduledNotCASLost(t *testing.T) {
	f := newCoordinatorFixture(t)

	moving := false
	edits := 0
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{
		dirtyBarrier: func() {
			if !moving {
				return
			}
			edits++
			builderWriteFile(t, f.worktree, "churn.go",
				"package fixture\n\n// edit "+string(rune('a'+edits))+"\n")
		},
	})
	coordinatorReconcile(t, c)

	moving = true
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc Helper() {}\n")

	before := viewmetrics.Read()
	c.cycle(context.Background())
	after := viewmetrics.Read()

	if got := counterDelta(before, after, cycleKey(viewmetrics.OutcomeRescheduled)); got != 1 {
		t.Fatalf("rescheduled = %d, want 1", got)
	}
	if got := counterDelta(before, after, cycleKey(viewmetrics.OutcomeCASLost)); got != 0 {
		t.Fatalf("a torn working tree was counted as a lost route flip (%d)", got)
	}
	if got := counterDelta(before, after, cycleKey(viewmetrics.OutcomeBuiltDirty)); got != 0 {
		t.Fatalf("a cycle that routed nothing reported a dirty build (%d)", got)
	}
}

// TestCommittedUnderTheCycleIsCountedAsHeadMoved separates the third condition
// that leaves a cycle rescheduled. A checkout that committed while its commit
// layer was being built is not a torn working tree and not a lost route flip:
// nothing raced this coordinator and nothing was left half written — the cycle
// simply built for a head the checkout has left, and the answer is another
// cycle. Reading one number for all three would hide the one that says commit
// builds are slower than the checkout moves.
func TestCommittedUnderTheCycleIsCountedAsHeadMoved(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()

	base, err := c.primaryBase(ctx)
	if err != nil {
		t.Fatalf("primaryBase: %v", err)
	}
	route, err := c.ensureRoute(ctx, base)
	if err != nil {
		t.Fatalf("ensureRoute: %v", err)
	}
	treeA := builderGit(t, f.worktree, "rev-parse", "HEAD^{tree}")
	var out CheckoutCycle
	commitGeneration, err := c.reconcileCommitSlot(ctx, base, treeA, &route, &out)
	if err != nil {
		t.Fatalf("reconcileCommitSlot: %v", err)
	}
	f.commitTreeB()

	before := viewmetrics.Read()
	if err := c.reconcileDirtySlot(ctx, commitGeneration, treeA, &route, &out); err != nil {
		t.Fatalf("reconcileDirtySlot: %v", err)
	}
	after := viewmetrics.Read()

	if got := counterDelta(before, after, cycleKey(viewmetrics.OutcomeHeadMoved)); got != 1 {
		t.Fatalf("head_moved = %d, want 1", got)
	}
	if got := counterDelta(before, after, cycleKey(viewmetrics.OutcomeRescheduled)); got != 0 {
		t.Fatalf("a checkout that committed was counted as a torn working tree (%d)", got)
	}
	if got := counterDelta(before, after, cycleKey(viewmetrics.OutcomeCASLost)); got != 0 {
		t.Fatalf("a checkout that committed was counted as a lost route flip (%d)", got)
	}
	if got := counterDelta(before, after, cycleKey(viewmetrics.OutcomeBuiltDirty)); got != 0 {
		t.Fatalf("a cycle that routed nothing reported a dirty build (%d)", got)
	}
}

func refViewSelectionKey(outcome string) string {
	return viewmetrics.RefViewSelectionTotal + "{outcome=" + outcome + "}"
}

// TestRefViewSelectionOutcomesAreCounted separates the two answers a caller
// cannot tell apart from the outside: a view that was already current, and one
// this selection had to build. Both come back ready, and only the counter says
// which — which is the difference between a warm cache and a rebuild loop.
func TestRefViewSelectionOutcomesAreCounted(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)
	manager := f.manager(t, nil)
	ctx := context.Background()

	before := viewmetrics.Read()
	if _, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature")); err != nil {
		t.Fatalf("first selection: %v", err)
	}
	built := viewmetrics.Read()
	if got := counterDelta(before, built, refViewSelectionKey(viewmetrics.RefViewAdopted)); got != 1 {
		t.Fatalf("adopted selections = %d, want 1", got)
	}
	if got := counterDelta(before, built, refViewSelectionKey(viewmetrics.RefViewReady)); got != 0 {
		t.Fatalf("a selection that built was also counted as already ready (%d)", got)
	}

	if _, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature")); err != nil {
		t.Fatalf("second selection: %v", err)
	}
	reused := viewmetrics.Read()
	if got := counterDelta(built, reused, refViewSelectionKey(viewmetrics.RefViewReady)); got != 1 {
		t.Fatalf("ready selections = %d, want 1", got)
	}
	if got := counterDelta(built, reused, refViewSelectionKey(viewmetrics.RefViewAdopted)); got != 0 {
		t.Fatalf("an unmoved ref rebuilt its payload (%d)", got)
	}
}

// TestRefViewFailedSelectionIsCounted pins the outcome a selector that cannot
// be resolved produces. It is the one answer that leaves the view serving
// whatever it was serving, so without the counter a branch that stopped
// resolving looks exactly like one nobody is asking about.
func TestRefViewFailedSelectionIsCounted(t *testing.T) {
	f := newRefViewFixture(t)
	manager := f.manager(t, nil)

	before := viewmetrics.Read()
	if _, err := manager.EnsureRefView(context.Background(), f.request("refs/heads/never-existed")); err == nil {
		t.Fatal("an unresolvable selector was served")
	}
	after := viewmetrics.Read()

	if got := counterDelta(before, after, refViewSelectionKey(viewmetrics.RefViewFailed)); got != 1 {
		t.Fatalf("failed selections = %d, want 1", got)
	}
	if got := counterDelta(before, after, refViewSelectionKey(viewmetrics.RefViewAdopted)); got != 0 {
		t.Fatalf("a failed selection was also counted as adopted (%d)", got)
	}
}

// TestViewsHealthCountsTheCatalogsPopulation pins the status block's shape:
// counts keyed by lifecycle state, and no identity anywhere in it.
func TestViewsHealthCountsTheCatalogsPopulation(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)

	lifecycle := newSweepLifecycle(t, f.store)
	health, err := lifecycle.ViewsHealth(context.Background())
	if err != nil {
		t.Fatalf("ViewsHealth: %v", err)
	}

	if health.Families != 1 {
		t.Fatalf("families = %d, want 1", health.Families)
	}
	if got := health.Checkouts["checkout_ready"]; got != 2 {
		t.Fatalf("ready checkouts = %d, want 2 (primary + worktree): %v", got, health.Checkouts)
	}
	if got := health.Generations["ready"]; got != 3 {
		t.Fatalf("ready generations = %d, want 3 (dedicated base + commit + dirty): %v", got, health.Generations)
	}
	if health.Leases != 0 {
		t.Fatalf("leases = %d, want 0 with no view materialized", health.Leases)
	}
	if len(health.Counters) == 0 {
		t.Fatal("the census carries no counters, so the metric registry is not reaching the status block")
	}
	// Every key the census carries must name a series the catalog declares.
	// That is what bounds the block: a key minted anywhere else would be a
	// series the cardinality guard never checked the vocabulary of.
	declared := map[string]bool{}
	for _, name := range viewmetrics.SeriesNames() {
		declared[name] = true
	}
	for key := range health.Counters {
		if !declared[seriesNameOf(key)] {
			t.Errorf("counter key %q names no declared series", key)
		}
	}
}

// seriesNameOf strips the label set and the duration suffix from a flattened
// series key, leaving the series name the catalog declares.
func seriesNameOf(key string) string {
	if i := strings.IndexByte(key, '{'); i >= 0 {
		return key[:i]
	}
	if i := strings.IndexByte(key, '|'); i >= 0 {
		return key[:i]
	}
	return key
}

// TestViewsHealthCountsALiveLease pins the one level that answers "why is this
// generation still here": a materialized view holds a lease, and a leased
// generation cannot retire.
func TestViewsHealthCountsALiveLease(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)

	lifecycle := newSweepLifecycle(t, f.store)
	lease := lifecycle.ViewLeases().Acquire(f.route().CommitGenerationID)
	health, err := lifecycle.ViewsHealth(context.Background())
	if err != nil {
		t.Fatalf("ViewsHealth: %v", err)
	}
	if health.Leases != 1 {
		t.Fatalf("leases while one generation is pinned = %d, want 1", health.Leases)
	}
	lease.Release()

	health, err = lifecycle.ViewsHealth(context.Background())
	if err != nil {
		t.Fatalf("ViewsHealth after release: %v", err)
	}
	if health.Leases != 0 {
		t.Fatalf("leases after release = %d, want 0", health.Leases)
	}
}
