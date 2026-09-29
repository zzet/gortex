package indexer

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// The coordinator with working-tree chaining on: which parent a cycle builds
// over, what a reader pinned before a flip keeps seeing, what a torn or
// superseded build publishes, how a chain is retired once the route leaves
// it, and the foreground-cost rules the cycle keeps (a refresh ticket skips
// the quiet window, one cycle shares one working-copy sample, no cycle
// deletes payload).

const chainIslandTwo = "package fixture\n\nfunc Island() {\n}\n\nfunc IslandTwo() {\n}\n"

func chainCoordinator(t *testing.T, cfg CheckoutCoordinatorConfig) (*coordinatorFixture, *CheckoutCoordinator) {
	t.Helper()
	f := newCoordinatorFixture(t)
	return f, f.inertCoordinator(t, cfg)
}

func chainMaterialize(t *testing.T, f *coordinatorFixture) *graphview.RepoView {
	t.Helper()
	materializer := &graphview.Materializer{Store: f.store, Catalog: f.catalog, Leases: f.leases}
	view, err := materializer.MaterializeCheckout(context.Background(), f.checkoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	return view
}

// chainAssertFlat requires the served checkout view to equal a clean whole
// index of the worktree as it is now.
func chainAssertFlat(t *testing.T, f *coordinatorFixture, label string) {
	t.Helper()
	view := chainMaterialize(t, f)
	defer view.Close()
	flat := builderOpenStore(t, "flat-"+label)
	builderIndex(t, flat, f.worktree)
	builderAssertReadersAgree(t, view.Reader, flat)
}

func TestDirtyChildPinnedReaderKeepsSnapshot(t *testing.T) {
	f, c := chainCoordinator(t, CheckoutCoordinatorConfig{})
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
	first := coordinatorReconcile(t, c)
	if !first.DirtyBuilt || first.DirtyChainDepth != 1 || first.DirtyChainReason != dirtyChainFallbackNoParent {
		t.Fatalf("the first working-tree build = %+v, want direct with reason %s", first, dirtyChainFallbackNoParent)
	}
	pinned := chainMaterialize(t, f)
	defer pinned.Close()
	before := builderOpenStore(t, "flat-before")
	builderIndex(t, before, f.worktree)

	builderWriteFile(t, f.worktree, "island.go", chainIslandTwo)
	second := coordinatorReconcile(t, c)
	if !second.DirtyBuilt || second.DirtyParentGenerationID != first.DirtyGenerationID || second.DirtyChainDepth != 2 {
		t.Fatalf("the second build = %+v, want a child of %d at depth 2", second, first.DirtyGenerationID)
	}
	row, _ := f.generation(second.DirtyGenerationID)
	if row.BaseGenerationID != first.DirtyGenerationID {
		t.Fatalf("the child's physical parent is %d, want %d", row.BaseGenerationID, first.DirtyGenerationID)
	}
	if got := f.route(); got.CommitGenerationID != first.CommitGenerationID || got.DirtyGenerationID != second.DirtyGenerationID {
		t.Fatalf("the route is %+v, want commit %d and the child %d", got, first.CommitGenerationID, second.DirtyGenerationID)
	}

	// The reader materialized before the flip keeps the state it pinned.
	if got := pinned.Reader.FindNodesByName("IslandTwo"); len(got) > 0 {
		t.Fatalf("the pinned reader sees IslandTwo, published after it was pinned")
	}
	builderAssertReadersAgree(t, pinned.Reader, before)

	// A reader materialized now sees exactly the new state.
	chainAssertFlat(t, f, "after")
	fresh := chainMaterialize(t, f)
	defer fresh.Close()
	if got := fresh.Reader.FindNodesByName("IslandTwo"); len(got) != 1 {
		t.Fatalf("a new reader sees %d IslandTwo nodes, want 1", len(got))
	}
}

func TestDirtyChildTornBuildPublishesNothing(t *testing.T) {
	f, c := chainCoordinator(t, CheckoutCoordinatorConfig{})
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
	first := coordinatorReconcile(t, c)

	var saves atomic.Int32
	c.dirtyBarrier = func() {
		n := saves.Add(1)
		builderWriteFile(t, f.worktree, "island.go",
			fmt.Sprintf("package fixture\n\nfunc Island() {\n\t// save %d under the build\n}\n", n))
	}
	builderWriteFile(t, f.worktree, "caller.go", chainCallerEdit)
	out := c.reconcile(context.Background())
	if out.Err != nil || !out.Rescheduled || out.DirtyBuilt {
		t.Fatalf("a checkout moving under every attempt = %+v, want a reschedule and no publication", out)
	}
	if got := f.route().DirtyGenerationID; got != first.DirtyGenerationID {
		t.Fatalf("the route names %d after two torn builds, want the parent %d still", got, first.DirtyGenerationID)
	}
	torn := 0
	for _, row := range f.generations() {
		if row.BaseGenerationID != first.DirtyGenerationID {
			continue
		}
		torn++
		if row.State == store_sqlite.ViewGenerationReady {
			t.Errorf("torn child %d over %d was published", row.GenerationID, first.DirtyGenerationID)
		}
	}
	if torn != 2 {
		t.Fatalf("%d children were attempted over the parent, want 2", torn)
	}
	if owed := c.retirementBacklog(); len(owed) < 2 {
		t.Fatalf("the torn attempts are not owed a retirement: backlog %v", owed)
	}
}

func TestDirtyChildConcurrentEditSupersedes(t *testing.T) {
	f, c := chainCoordinator(t, CheckoutCoordinatorConfig{})
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
	first := coordinatorReconcile(t, c)

	var once atomic.Bool
	c.dirtyBarrier = func() {
		if once.CompareAndSwap(false, true) {
			builderWriteFile(t, f.worktree, "sneaked.go", "package fixture\n\nfunc Sneaked() {\n}\n")
		}
	}
	builderWriteFile(t, f.worktree, "caller.go", chainCallerEdit)
	out := coordinatorReconcile(t, c)
	if !out.DirtyBuilt || out.Rescheduled {
		t.Fatalf("the retry did not publish: %+v", out)
	}
	if out.DirtyParentGenerationID != first.DirtyGenerationID {
		t.Fatalf("the retry stood on %d, want the routed parent %d", out.DirtyParentGenerationID, first.DirtyGenerationID)
	}
	sample, err := c.sampler.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	row, _ := f.generation(out.DirtyGenerationID)
	if row.LowerViewFingerprint != sample.Fingerprint {
		t.Fatalf("the published child describes %s, the checkout is at %s", row.LowerViewFingerprint, sample.Fingerprint)
	}
	for _, g := range f.generations() {
		if g.BaseGenerationID == first.DirtyGenerationID && g.GenerationID != out.DirtyGenerationID &&
			g.State == store_sqlite.ViewGenerationReady {
			t.Errorf("the superseded attempt %d is Ready", g.GenerationID)
		}
	}
	chainAssertFlat(t, f, "concurrent")
}

func TestDirtyCycleNeverRetiresPayloadInline(t *testing.T) {
	f, c := chainCoordinator(t, CheckoutCoordinatorConfig{Retain: 1})
	var inline atomic.Int32
	c.retireCalled = func(int64) { inline.Add(1) }

	steps := []func(){
		func() { builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit) },
		func() { builderWriteFile(t, f.worktree, "caller.go", chainCallerEdit) },
		func() { builderWriteFile(t, f.worktree, "island.go", chainIslandEdit) },
		func() { builderGit(t, f.worktree, "checkout", "--", ".") },
		func() { builderWriteFile(t, f.worktree, "island.go", chainIslandTwo) },
	}
	for i, step := range steps {
		step()
		out := coordinatorReconcile(t, c)
		if !out.DirtyBuilt && !out.DirtyReused {
			t.Fatalf("step %d moved nothing: %+v", i, out)
		}
	}
	if n := inline.Load(); n != 0 {
		t.Fatalf("the cycles retired %d generations inline, want every retirement owed to the sweep", n)
	}
	owed := c.retirementBacklog()
	if len(owed) == 0 {
		t.Fatal("five route moves with a one-deep cache owe no retirement")
	}
	if retired := c.SweepRetirements(context.Background()); retired == 0 {
		t.Fatalf("the sweep collected nothing of the owed %v", owed)
	}
}

func TestChainRetiresNewestFirstAfterReaderReleases(t *testing.T) {
	f, c := chainCoordinator(t, CheckoutCoordinatorConfig{Retain: 1})
	ctx := context.Background()
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
	d1 := coordinatorReconcile(t, c)
	builderWriteFile(t, f.worktree, "caller.go", chainCallerEdit)
	d2 := coordinatorReconcile(t, c)
	builderWriteFile(t, f.worktree, "island.go", chainIslandEdit)
	d3 := coordinatorReconcile(t, c)
	if d2.DirtyParentGenerationID != d1.DirtyGenerationID || d3.DirtyParentGenerationID != d2.DirtyGenerationID {
		t.Fatalf("the chain did not build: d1=%+v d2=%+v d3=%+v", d1, d2, d3)
	}
	chain := []int64{d3.DirtyGenerationID, d2.DirtyGenerationID, d1.DirtyGenerationID}

	reader := chainMaterialize(t, f)
	// A fresh coordinator (a daemon restart) inherits the routed chain and
	// knows nothing of its parents: only the walk down the chain can owe them.
	c = f.inertCoordinator(t, CheckoutCoordinatorConfig{Retain: 1})
	// Back to the committed state: the route moves off the chain to a
	// generation built direct over the commit generation.
	builderGit(t, f.worktree, "checkout", "--", ".")
	clean := coordinatorReconcile(t, c)
	if !clean.DirtyBuilt || clean.DirtyParentGenerationID != 0 || clean.DirtyChainReason != dirtyChainFallbackCleanCheckout {
		t.Fatalf("the clean state = %+v, want a direct build with reason %s", clean, dirtyChainFallbackCleanCheckout)
	}
	owed := c.retirementBacklog()
	for _, id := range chain {
		if !slices.Contains(owed, id) {
			t.Fatalf("chain member %d is not owed a retirement once the route left the chain (backlog %v)", id, owed)
		}
	}
	c.SweepRetirements(ctx)
	for _, id := range chain {
		if _, found := f.generation(id); !found {
			t.Fatalf("chain member %d was retired while a reader still pinned the chain", id)
		}
	}

	reader.Close()
	if retired := c.SweepRetirements(ctx); retired < len(chain) {
		t.Fatalf("one sweep after the reader released collected %d, want the whole chain of %d", retired, len(chain))
	}
	for _, id := range chain {
		if _, found := f.generation(id); found {
			t.Fatalf("chain member %d survived the sweep after its reader released", id)
		}
	}
	chainAssertFlat(t, f, "clean")
}

// newRunningRefreshFixture is a coordinator whose loop runs, behind a quiet
// window far longer than any test waits, with a lifecycle that routes refresh
// requests to it.
func newRunningRefreshFixture(t *testing.T) (*coordinatorFixture, *CheckoutCoordinator, *CheckoutLifecycle) {
	t.Helper()
	f := newCoordinatorFixture(t)
	gate := NewViewBuildGate()
	gate.Open()
	c := f.coordinator(t, CheckoutCoordinatorConfig{Gate: gate, Debounce: time.Hour})
	l := &CheckoutLifecycle{
		catalog:      f.catalog,
		store:        f.store,
		coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c},
	}
	return f, c, l
}

func TestRefreshTicketCycleSkipsTheQuietWindow(t *testing.T) {
	f, _, l := newRunningRefreshFixture(t)
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)

	record := DefaultPublicationPhases().Begin(f.checkoutID,
		fmt.Sprintf("test-refresh-%d", time.Now().UnixNano()), PublicationSourceFreshRequest, time.Now())
	ctx := WithPublicationRecord(t.Context(), record)
	started := time.Now()
	ticket, err := l.RequestCheckoutRefresh(ctx, f.checkoutID, f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	// The window is an hour: only a demand wake can complete this ticket.
	result := awaitCheckoutRefresh(t, ticket)
	if result.Err != nil || !result.Reindexed || result.AppliedGeneration == 0 {
		t.Fatalf("the ticket = %+v, want a publication", result)
	}
	t.Logf("ticket published generation %d in %v with an hour-long quiet window",
		result.AppliedGeneration, time.Since(started))
	if got := f.route().DirtyGenerationID; uint64(got) != result.AppliedGeneration {
		t.Fatalf("the route names %d, the ticket reported %d", got, result.AppliedGeneration)
	}

	// The record handed in through the context was bound before the wake, so
	// the demand-woken cycle's first phases reached it.
	snap := record.Snapshot()
	have := map[PublicationPhase]bool{}
	for _, p := range snap.Phases {
		have[p.Phase] = true
	}
	for _, phase := range []PublicationPhase{
		PublicationTicketEnqueued, PublicationCycleStarted, PublicationAdmitted,
		PublicationPlanned, PublicationExtracted, PublicationPayloadFlushed, PublicationPublished, PublicationRouteFlipped,
	} {
		if !have[phase] {
			t.Errorf("the record misses %s: %+v", phase, snap.Phases)
		}
	}
	if snap.Ticket != ticket.Ticket.Generation {
		t.Errorf("the record is bound to ticket %d, want %d", snap.Ticket, ticket.Ticket.Generation)
	}
}

func TestCycleSharesOneWorkingCopySample(t *testing.T) {
	// The loop is parked behind an hour-long window (demand included), so
	// every cycle here is the one the test runs; the fixture has already
	// brought both slots up.
	f, c, _ := newCheckoutMutationFixture(t)
	ctx := context.Background()
	cycle := func() CheckoutCycle {
		t.Helper()
		var out CheckoutCycle
		c.cycleDone = func(cycle CheckoutCycle) { out = cycle }
		c.cycle(ctx)
		if out.Err != nil {
			t.Fatalf("cycle: %v", out.Err)
		}
		return out
	}
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
	before := c.sampler.SamplesTaken()
	if out := cycle(); !out.DirtyBuilt || out.CommitBuilt {
		t.Fatalf("an edit's cycle = %+v, want a working-tree build only", out)
	}
	// One sample decides the settle check, the reconcile, the slot and the
	// build's change set; the pre-publish fence confirms the build's read set
	// against it without another (confirmDirtyBuildInputs).
	if got := c.sampler.SamplesTaken() - before; got != 1 {
		t.Fatalf("a working-tree build cycle took %d samples, want 1 (shared; the fence is the read set)", got)
	}

	// A refresh ticket served by a settled cycle: the ticket's own capture
	// sample, the cycle's one sample, and no re-sample at completion.
	checkout, found, err := f.catalog.GetCheckout(ctx, f.checkoutID)
	if err != nil || !found {
		t.Fatalf("read checkout: found=%v err=%v", found, err)
	}
	rootInfo, err := checkoutRootFileInfo(checkout.RootPath)
	if err != nil {
		t.Fatal(err)
	}
	request, err := c.captureCheckoutRefresh(ctx, checkout, rootInfo, "")
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := c.enqueueCheckoutRefresh(request, false)
	if err != nil {
		t.Fatal(err)
	}
	before = c.sampler.SamplesTaken()
	out := cycle()
	if out.DirtyBuilt || out.CommitBuilt {
		t.Fatalf("a settled checkout's cycle built: %+v", out)
	}
	if result := awaitCheckoutRefresh(t, ticket); result.Err != nil || !result.Reindexed {
		t.Fatalf("the ticket = %+v", result)
	}
	// The ticket's capture sample began after it was requested, so the
	// cycle settles on it and completes the ticket with it: no sample of its
	// own.
	if got := c.sampler.SamplesTaken() - before; got != 0 {
		t.Fatalf("a settled ticket cycle took %d samples, want 0 (the settle check and completion reuse the ticket's capture sample)", got)
	}
}
