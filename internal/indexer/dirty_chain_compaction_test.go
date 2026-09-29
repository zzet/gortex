package indexer

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// The working-tree chain's lifetime through the coordinator: background
// compaction and what cancels it, exhaustion at the hard depth, a restart
// that inherits a routed chain (with and without manifests), a clean reset,
// an edit lease that withdraws the routed top, and a refresh admitted against
// a caller's sample.

// chainEditIsland writes the k-th distinct body of island.go over a working
// tree that also carries two steady edits (helper.go, caller.go): every call is
// a new working-tree state whose delta over the previous one is that one file,
// while the dirty set stays larger than the delta.
func chainEditIsland(t *testing.T, f *coordinatorFixture, k int) {
	t.Helper()
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
	builderWriteFile(t, f.worktree, "caller.go", chainCallerEdit)
	builderWriteFile(t, f.worktree, "island.go",
		fmt.Sprintf("package fixture\n\nfunc Island() {\n\t// working-tree edit %d\n}\n", k))
}

// reopenStore closes the fixture's store and opens the same file again with a
// fresh lease manager: what a daemon restart leaves a coordinator to find.
func (f *coordinatorFixture) reopenStore(t *testing.T) {
	t.Helper()
	if err := f.store.Close(); err != nil {
		t.Fatalf("close the store for a restart: %v", err)
	}
	store := builderOpenStoreAt(t, f.storePath)
	t.Cleanup(func() { _ = store.Close() })
	f.store, f.catalog, f.leases = store, store.Catalog(), graphview.NewLeaseManager()
	f.registerRosterOwner(t)
}

// runningChainFixture is a coordinator whose loop runs with chaining on,
// behind an hour-long quiet window and no poll, so the only cycles are the
// ones refresh tickets demand; the lifecycle routes tickets to it.
func runningChainFixture(t *testing.T, cfg CheckoutCoordinatorConfig) (*coordinatorFixture, *CheckoutCoordinator, *CheckoutLifecycle, *ViewBuildGate) {
	t.Helper()
	f := newCoordinatorFixture(t)
	gate := NewViewBuildGate()
	gate.Open()
	cfg.Gate, cfg.Debounce = gate, time.Hour
	c := f.coordinator(t, cfg)
	l := &CheckoutLifecycle{
		catalog:      f.catalog,
		store:        f.store,
		coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c},
	}
	return f, c, l, gate
}

// chainTicketEdit applies edit k and waits for the demand-woken cycle that
// publishes it.
func chainTicketEdit(t *testing.T, f *coordinatorFixture, l *CheckoutLifecycle, k int) MutationResult {
	t.Helper()
	chainEditIsland(t, f, k)
	ticket, err := l.RequestCheckoutRefresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatalf("edit %d: request refresh: %v", k, err)
	}
	result := awaitCheckoutRefresh(t, ticket)
	if result.Err != nil || !result.Reindexed || result.AppliedGeneration == 0 {
		t.Fatalf("edit %d: ticket = %+v", k, result)
	}
	return result
}

// observeCompactions routes every finished compaction to the returned channel.
func observeCompactions(c *CheckoutCoordinator) <-chan DirtyChainCompaction {
	reports := make(chan DirtyChainCompaction, 16)
	c.compaction.mu.Lock()
	c.compaction.done = func(r DirtyChainCompaction) { reports <- r }
	c.compaction.mu.Unlock()
	return reports
}

func awaitCompaction(t *testing.T, reports <-chan DirtyChainCompaction) DirtyChainCompaction {
	t.Helper()
	select {
	case r := <-reports:
		return r
	case <-time.After(60 * time.Second):
		t.Fatal("no background compaction finished")
		return DirtyChainCompaction{}
	}
}

// requireFullManifest asserts a generation carries a complete, full input
// manifest — what a compaction and a direct build write.
func requireFullManifest(t *testing.T, f *coordinatorFixture, generationID int64) {
	t.Helper()
	meta, _, found, err := f.store.AtGeneration(generationID).InputManifest(context.Background())
	if err != nil || !found || !meta.IsFull {
		t.Fatalf("generation %d manifest: found=%v full=%v err=%v", generationID, found, meta.IsFull, err)
	}
}

func TestDirtyChainCompactsInBackground(t *testing.T) {
	f, c, l, _ := runningChainFixture(t, CheckoutCoordinatorConfig{Retain: 1})
	reports := observeCompactions(c)
	recordsBefore := DefaultPublicationPhases().Snapshot(f.checkoutID)
	var chain []int64
	for k := 1; k <= dirtyChainCompactionDepth; k++ {
		chain = append(chain, int64(chainTicketEdit(t, f, l, k).AppliedGeneration))
	}
	for i := 1; i < len(chain); i++ {
		if row, _ := f.generation(chain[i]); row.BaseGenerationID != chain[i-1] {
			t.Fatalf("edit %d stands on %d, want the previous publication %d", i+1, row.BaseGenerationID, chain[i-1])
		}
	}
	report := awaitCompaction(t, reports)
	if report.Outcome != dirtyChainCompactionFlipped || report.Canceled || report.GenerationID == 0 ||
		report.ChainDepthBefore != dirtyChainCompactionDepth || report.Top != chain[len(chain)-1] {
		t.Fatalf("compaction = %+v, want a flip of the depth-%d chain topped by %d", report, dirtyChainCompactionDepth, chain[len(chain)-1])
	}
	if report.Duration <= 0 || report.BuildDuration <= 0 {
		t.Fatalf("compaction cost unrecorded: duration=%v fold=%v", report.Duration, report.BuildDuration)
	}
	t.Logf("compaction: generation %d folded by copy, %v (fold %v)",
		report.GenerationID, report.Duration, report.BuildDuration)
	route := f.route()
	if route.DirtyGenerationID != report.GenerationID {
		t.Fatalf("the route names %d, want the compacted %d", route.DirtyGenerationID, report.GenerationID)
	}
	compacted, _ := f.generation(report.GenerationID)
	if compacted.BaseGenerationID != route.CommitGenerationID {
		t.Fatalf("the compacted generation stands on %d, want the commit generation %d", compacted.BaseGenerationID, route.CommitGenerationID)
	}
	top, _ := f.generation(chain[len(chain)-1])
	if compacted.LowerViewFingerprint != top.LowerViewFingerprint {
		t.Fatalf("the compaction describes %s, the chain it replaced %s", compacted.LowerViewFingerprint, top.LowerViewFingerprint)
	}
	requireFullManifest(t, f, report.GenerationID)
	chainAssertFlat(t, f, "compacted")
	// The compaction is on the publication record beside the edits, so an
	// edit's timing can show maintenance ran inside its window.
	var compactionRecord *PublicationPhaseSnapshot
	for _, r := range newRecordKeys(f.checkoutID, recordsBefore) {
		if r.Source == PublicationSourceBackgroundCompaction {
			compactionRecord = &r
		}
	}
	if compactionRecord == nil || !compactionRecord.Terminal || compactionRecord.DirtyGenerationID != report.GenerationID {
		t.Fatalf("the compaction's publication record = %+v, want a terminal record on %d", compactionRecord, report.GenerationID)
	}
	for _, phase := range []PublicationPhase{PublicationAdmitted, PublicationPublished, PublicationRouteFlipped, PublicationTicketCompleted} {
		if !slices.Contains(phaseNames(*compactionRecord), phase) {
			t.Errorf("the compaction record misses %s: %v", phase, phaseNames(*compactionRecord))
		}
	}

	owed := c.retirementBacklog()
	for _, id := range chain {
		if !slices.Contains(owed, id) {
			t.Fatalf("chain member %d is not owed once the route moved to the compaction (backlog %v)", id, owed)
		}
	}
	c.SweepRetirements(t.Context())
	for _, id := range chain {
		if _, found := f.generation(id); found {
			t.Fatalf("chain member %d survived the sweep after the compaction flip", id)
		}
	}

	// The next edit stands on the compacted generation, at depth 2.
	next := int64(chainTicketEdit(t, f, l, dirtyChainCompactionDepth+1).AppliedGeneration)
	if row, _ := f.generation(next); row.BaseGenerationID != report.GenerationID {
		t.Fatalf("the edit after the compaction stands on %d, want the compacted %d", row.BaseGenerationID, report.GenerationID)
	}
	chainAssertFlat(t, f, "after-compaction")
	if stats := c.DirtyChainCompactionStats(); stats.Scheduled != 1 || stats.Flipped != 1 {
		t.Fatalf("compaction stats = %+v", stats)
	}
}

func TestDirtyChainCompactionCanceledByForegroundEdit(t *testing.T) {
	f, c, l, _ := runningChainFixture(t, CheckoutCoordinatorConfig{})
	reports := observeCompactions(c)

	// Hold the first compaction on the lane, before its fold, until the test
	// is ready, while a foreground edit arrives.
	proceed, inBuild, releaseBuild := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var heldOnce atomic.Bool
	c.compaction.mu.Lock()
	c.compaction.barrier = func(ctx context.Context) {
		if heldOnce.CompareAndSwap(false, true) {
			<-proceed
			close(inBuild)
			select {
			case <-releaseBuild:
			case <-ctx.Done():
			}
		}
	}
	c.compaction.mu.Unlock()

	var chain []int64
	for k := 1; k <= dirtyChainCompactionDepth; k++ {
		chain = append(chain, int64(chainTicketEdit(t, f, l, k).AppliedGeneration))
	}
	top := chain[len(chain)-1]
	close(proceed)
	select {
	case <-inBuild:
	case <-time.After(30 * time.Second):
		t.Fatal("the compaction never took the lane")
	}

	// A foreground edit while the compaction holds the lane.
	started := time.Now()
	chainEditIsland(t, f, dirtyChainCompactionDepth+1)
	ticket, err := l.RequestCheckoutRefresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	// The edit's cycle cancels the compaction before it queues for the lane,
	// so the held attempt gives the lane back at once.
	defer close(releaseBuild)

	result := awaitCheckoutRefresh(t, ticket)
	if result.Err != nil || !result.Reindexed {
		t.Fatalf("the foreground edit = %+v", result)
	}
	t.Logf("the foreground edit published generation %d in %v while a compaction held the lane", result.AppliedGeneration, time.Since(started))
	first := awaitCompaction(t, reports)
	if !first.Canceled || first.Outcome != dirtyChainCompactionCanceled || first.GenerationID != 0 {
		t.Fatalf("the interrupted compaction = %+v, want canceled with nothing published", first)
	}
	child, _ := f.generation(int64(result.AppliedGeneration))
	if child.BaseGenerationID != top {
		t.Fatalf("the foreground edit stands on %d, want the chain top %d (depth %d)", child.BaseGenerationID, top, dirtyChainCompactionDepth+1)
	}
	// Nothing the canceled compaction began was published: no Ready
	// generation over the commit generation describes the state it compacted.
	topRow, _ := f.generation(top)
	for _, row := range f.generations() {
		if row.GenerationKind == DirtyLayerGenerationKind && row.State == store_sqlite.ViewGenerationReady &&
			row.BaseGenerationID == f.route().CommitGenerationID && row.LowerViewFingerprint == topRow.LowerViewFingerprint {
			t.Fatalf("the canceled compaction published generation %d", row.GenerationID)
		}
	}

	// The foreground edit reached the soft depth again, so a second
	// compaction runs, undisturbed, and flips.
	second := awaitCompaction(t, reports)
	if second.Outcome != dirtyChainCompactionFlipped || second.Top != child.GenerationID {
		t.Fatalf("the second compaction = %+v, want a flip of %d", second, child.GenerationID)
	}
	chainAssertFlat(t, f, "after-cancel")
	if stats := c.DirtyChainCompactionStats(); stats.Canceled != 1 || stats.Flipped != 1 {
		t.Fatalf("compaction stats = %+v, want one canceled and one flipped", stats)
	}
}

func TestDirtyChainCompactionYieldsTheLaneToAnInteractiveBuild(t *testing.T) {
	f, c, l, gate := runningChainFixture(t, CheckoutCoordinatorConfig{})
	reports := observeCompactions(c)
	entered := make(chan struct{})
	var once atomic.Bool
	c.compaction.mu.Lock()
	c.compaction.barrier = func(ctx context.Context) {
		if once.CompareAndSwap(false, true) {
			close(entered)
			<-ctx.Done()
		}
	}
	c.compaction.mu.Unlock()
	for k := 1; k <= dirtyChainCompactionDepth; k++ {
		chainTicketEdit(t, f, l, k)
	}
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the compaction never took the lane")
	}
	// Another checkout's interactive build (nothing this coordinator knows
	// about) queues on the shared lane.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	started := time.Now()
	release, err := gate.Acquire(ctx, ViewBuildInteractive)
	if err != nil {
		t.Fatalf("the interactive build never got the lane: %v", err)
	}
	waited := time.Since(started)
	release()
	report := awaitCompaction(t, reports)
	if !slices.Contains(report.YieldedTo, "interactive_build") || report.Attempts < 2 || report.Outcome != dirtyChainCompactionFlipped {
		t.Fatalf("the compaction = %+v, want it to yield to the interactive build and flip on a later attempt", report)
	}
	t.Logf("the interactive build waited %v for the lane a compaction held; the compaction yielded %v and flipped on attempt %d",
		waited, report.YieldedTo, report.Attempts)
}

func TestDirtyChainCompactionWaitsForAQuietCheckout(t *testing.T) {
	f, c, l, _ := runningChainFixture(t, CheckoutCoordinatorConfig{})
	reports := observeCompactions(c)
	c.compaction.mu.Lock()
	c.compaction.quiet = 300 * time.Millisecond
	c.compaction.mu.Unlock()
	for k := 1; k <= dirtyChainCompactionDepth; k++ {
		chainTicketEdit(t, f, l, k)
	}
	// A reader holds the served view the moment the edit that owes the
	// compaction is published.
	reader := chainMaterialize(t, f)
	held := time.Now()
	select {
	case r := <-reports:
		reader.Close()
		t.Fatalf("a compaction ran while a reader held the served view: %+v", r)
	case <-time.After(time.Second):
	}
	reader.Close()
	released := time.Now()
	report := awaitCompaction(t, reports)
	if report.Outcome != dirtyChainCompactionFlipped || report.Attempts != 1 {
		t.Fatalf("the compaction after the reader left = %+v, want one attempt that flipped", report)
	}
	if report.QuietWait < time.Second {
		t.Fatalf("the compaction waited %v for quiet, want at least the second the reader held the view", report.QuietWait)
	}
	t.Logf("reader held %v; the compaction started %v after it left and took %v",
		released.Sub(held), report.QuietWait-released.Sub(held), report.Duration)
}

func TestDirtyChainCompactionYieldsToAReader(t *testing.T) {
	// The daemon's shape: one P, so a compaction that kept running would take
	// the CPU a query needs.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	f, c, l, _ := runningChainFixture(t, CheckoutCoordinatorConfig{})
	reports := observeCompactions(c)
	entered := make(chan struct{})
	yielded := make(chan time.Time, 1)
	var once atomic.Bool
	c.compaction.mu.Lock()
	c.compaction.quiet = 20 * time.Millisecond
	c.compaction.barrier = func(ctx context.Context) {
		if once.CompareAndSwap(false, true) {
			close(entered)
			<-ctx.Done()
			yielded <- time.Now()
		}
	}
	c.compaction.mu.Unlock()
	for k := 1; k <= dirtyChainCompactionDepth; k++ {
		chainTicketEdit(t, f, l, k)
	}
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the compaction never started")
	}
	// A query arrives while the compaction holds the lane.
	arrived := time.Now()
	reader := chainMaterialize(t, f)
	var gaveWay time.Time
	select {
	case gaveWay = <-yielded:
	case <-time.After(5 * time.Second):
		reader.Close()
		t.Fatal("the compaction did not yield to the reader")
	}
	queryStarted := time.Now()
	nodes := reader.Reader.FindNodesByName("Island")
	query := time.Since(queryStarted)
	reader.Close()
	if len(nodes) != 1 {
		t.Fatalf("the reader found %d Island nodes, want 1", len(nodes))
	}
	if lag := gaveWay.Sub(arrived); lag > 500*time.Millisecond {
		t.Fatalf("the compaction yielded %v after the reader arrived", lag)
	}
	report := awaitCompaction(t, reports)
	if !slices.Contains(report.YieldedTo, "reader") || report.Outcome != dirtyChainCompactionFlipped {
		t.Fatalf("the compaction = %+v, want it to yield to the reader, then flip once the reader left", report)
	}
	t.Logf("the compaction yielded %v after the reader arrived; the query took %v; the compaction flipped on attempt %d",
		gaveWay.Sub(arrived), query, report.Attempts)
}

func TestDirtyChainExhaustionBuildsTheStateDirect(t *testing.T) {
	// No loop, so no compaction runs: the chain grows to the hard bound.
	f, c := chainCoordinator(t, CheckoutCoordinatorConfig{})
	var last CheckoutCycle
	for k := 1; k <= maxDirtyChainDepth; k++ {
		chainEditIsland(t, f, k)
		last = coordinatorReconcile(t, c)
		if !last.DirtyBuilt || last.DirtyChainDepth != k {
			t.Fatalf("edit %d = %+v, want a build at depth %d", k, last, k)
		}
		if want := k >= dirtyChainCompactionDepth; last.CompactionScheduled != want {
			t.Fatalf("edit %d at depth %d scheduled a compaction = %t, want %t", k, k, last.CompactionScheduled, want)
		}
	}
	// The fold the soft depth owed never ran, so the edit that meets the
	// exhausted chain builds its state direct over the commit generation, and
	// the next edit chains over it again.
	chainEditIsland(t, f, maxDirtyChainDepth+1)
	out := coordinatorReconcile(t, c)
	if !out.DirtyBuilt || out.DirtyParentGenerationID != 0 || out.DirtyChainDepth != 1 ||
		out.DirtyChainReason != dirtyChainFallbackChainDepthExhausted {
		t.Fatalf("the edit past the bound = %+v, want a direct build (%s)", out, dirtyChainFallbackChainDepthExhausted)
	}
	chainAssertFlat(t, f, "exhausted")
	chainEditIsland(t, f, maxDirtyChainDepth+2)
	next := coordinatorReconcile(t, c)
	if next.DirtyParentGenerationID != out.DirtyGenerationID || next.DirtyChainDepth != 2 {
		t.Fatalf("the edit after the direct build = %+v, want a child of %d at depth 2", next, out.DirtyGenerationID)
	}
	chainAssertFlat(t, f, "after-exhaustion")
}

func TestDirtyChainRestartReusesRoutedChain(t *testing.T) {
	f, c := chainCoordinator(t, CheckoutCoordinatorConfig{})
	var chain []CheckoutCycle
	for k := 1; k <= 3; k++ {
		chainEditIsland(t, f, k)
		chain = append(chain, coordinatorReconcile(t, c))
	}
	top := chain[len(chain)-1]
	if top.DirtyChainDepth != 3 {
		t.Fatalf("the chain before the restart = %+v", top)
	}
	commitsBefore := len(f.generations())

	f.reopenStore(t)
	c = f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	chainEditIsland(t, f, 4)
	out := coordinatorReconcile(t, c)
	if out.CommitBuilt || out.CommitGenerationID != top.CommitGenerationID {
		t.Fatalf("the restarted coordinator rebuilt the commit slot: %+v", out)
	}
	if !out.DirtyBuilt || out.DirtyParentGenerationID != top.DirtyGenerationID || out.DirtyChainDepth != 4 || out.DirtyChainReason != "" {
		t.Fatalf("the first edit after the restart = %+v, want a child of the routed top %d at depth 4", out, top.DirtyGenerationID)
	}
	if w := out.DirtyWork; w == nil || w.ParserInputs > 2 {
		t.Fatalf("the edit after the restart re-derived more than the edit: %+v", w)
	}
	if got := len(f.generations()); got != commitsBefore+1 {
		t.Fatalf("the restart edit created %d generations, want exactly one", got-commitsBefore)
	}
	chainAssertFlat(t, f, "restart")
}

func TestDirtyChainRestartWithoutManifestFallsBack(t *testing.T) {
	f, c := chainCoordinator(t, CheckoutCoordinatorConfig{})
	chainEditIsland(t, f, 1)
	first := coordinatorReconcile(t, c)
	requireFullManifest(t, f, first.DirtyGenerationID)

	// A generation written before manifests existed: no meta row, no entries.
	db := parityOpenRaw(t, f.store)
	for _, table := range []string{"generation_input_manifest", "generation_input_manifest_meta"} {
		if _, err := db.Exec(`DELETE FROM `+table+` WHERE view_gen = ?`, first.DirtyGenerationID); err != nil {
			t.Fatalf("drop %s rows: %v", table, err)
		}
	}
	_ = db.Close()
	f.reopenStore(t)
	c = f.inertCoordinator(t, CheckoutCoordinatorConfig{})

	chainEditIsland(t, f, 2)
	out := coordinatorReconcile(t, c)
	if !out.DirtyBuilt || out.DirtyParentGenerationID != 0 || out.DirtyChainDepth != 1 ||
		out.DirtyChainReason != dirtyChainFallbackParentManifestMissing {
		t.Fatalf("an edit over a pre-manifest generation = %+v, want a direct build with reason %s",
			out, dirtyChainFallbackParentManifestMissing)
	}
	requireFullManifest(t, f, out.DirtyGenerationID)
	chainAssertFlat(t, f, "pre-manifest")
	chainEditIsland(t, f, 3)
	if next := coordinatorReconcile(t, c); next.DirtyParentGenerationID != out.DirtyGenerationID {
		t.Fatalf("the edit after the fallback = %+v, want a child of %d", next, out.DirtyGenerationID)
	}
}

func TestDirtyChainCleanResetRoutesDirect(t *testing.T) {
	f, c := chainCoordinator(t, CheckoutCoordinatorConfig{Retain: 2})
	ctx := context.Background()
	var chain []int64
	for k := 1; k <= 3; k++ {
		chainEditIsland(t, f, k)
		chain = append(chain, coordinatorReconcile(t, c).DirtyGenerationID)
	}
	builderGit(t, f.worktree, "checkout", "--", ".")
	clean := coordinatorReconcile(t, c)
	if !clean.DirtyBuilt || clean.DirtyParentGenerationID != 0 || clean.DirtyChainDepth != 1 ||
		clean.DirtyChainReason != dirtyChainFallbackCleanCheckout {
		t.Fatalf("the clean reset = %+v, want a direct build with reason %s", clean, dirtyChainFallbackCleanCheckout)
	}
	row, _ := f.generation(clean.DirtyGenerationID)
	if row.BaseGenerationID != clean.CommitGenerationID {
		t.Fatalf("the clean generation stands on %d, want the commit generation %d (never a child)", row.BaseGenerationID, clean.CommitGenerationID)
	}
	chainAssertFlat(t, f, "clean")

	// An edit after the reset (the clean parent carries nothing to reuse, so
	// it is built direct: delta_not_smaller over the clean parent); resetting
	// again re-routes the cached clean generation instead of building.
	chainEditIsland(t, f, 4)
	edited := coordinatorReconcile(t, c)
	if !edited.DirtyBuilt || edited.DirtyChainDepth != 1 || edited.DirtyChainReason != dirtyChainFallbackDeltaNotSmaller {
		t.Fatalf("the edit after the reset = %+v, want a direct build (%s)", edited, dirtyChainFallbackDeltaNotSmaller)
	}
	builderGit(t, f.worktree, "checkout", "--", ".")
	again := coordinatorReconcile(t, c)
	if !again.DirtyReused || again.DirtyBuilt || again.DirtyGenerationID != clean.DirtyGenerationID {
		t.Fatalf("the second reset = %+v, want the cached clean generation %d re-routed", again, clean.DirtyGenerationID)
	}
	chainAssertFlat(t, f, "clean-again")

	// The abandoned chain is owed and collected; what the route and the
	// cache still use is not.
	c.SweepRetirements(ctx)
	for _, id := range chain {
		if _, found := f.generation(id); found {
			t.Fatalf("chain member %d survived the sweep after two clean resets (backlog %v)", id, c.retirementBacklog())
		}
	}
	for _, id := range []int64{clean.DirtyGenerationID, edited.DirtyGenerationID} {
		if _, found := f.generation(id); !found {
			t.Fatalf("generation %d the route or the reuse cache holds was retired", id)
		}
	}
}

func TestDirtyChainCheckoutMutationStandsOnTheWithdrawnTop(t *testing.T) {
	f := newCoordinatorFixture(t)
	gate := NewViewBuildGate()
	gate.Open()
	c := f.coordinator(t, CheckoutCoordinatorConfig{Gate: gate, Debounce: time.Hour, debounceDemand: true})
	l := &CheckoutLifecycle{catalog: f.catalog, store: f.store, coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}}
	chainEditIsland(t, f, 1)
	previous := coordinatorReconcile(t, c)

	for k := 2; k <= 3; k++ {
		route := f.route()
		m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, route.RouteEpoch)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Prepare(t.Context()); err != nil {
			m.Close()
			t.Fatal(err)
		}
		if f.route().DirtyGenerationID != 0 {
			m.Close()
			t.Fatal("prepare did not withdraw the routed working-tree generation")
		}
		chainEditIsland(t, f, k)
		out, err := m.Refresh(t.Context())
		m.Close()
		if err != nil {
			t.Fatalf("edit %d: refresh: %v", k, err)
		}
		if !out.DirtyBuilt || !out.DirtyParentPreferred || out.DirtyParentGenerationID != previous.DirtyGenerationID ||
			out.DirtyChainDepth != k || out.DirtyChainReason != "" {
			t.Fatalf("edit %d through a lease = %+v, want a child of the withdrawn top %d at depth %d",
				k, out, previous.DirtyGenerationID, k)
		}
		if _, found := f.generation(previous.DirtyGenerationID); !found {
			t.Fatalf("the withdrawn top %d was collected under the edit", previous.DirtyGenerationID)
		}
		previous = out
		chainAssertFlat(t, f, fmt.Sprintf("lease-%d", k))
	}
	if owed := c.retirementBacklog(); slices.Contains(owed, previous.DirtyGenerationID) {
		t.Fatalf("the routed top %d is owed a retirement: %v", previous.DirtyGenerationID, owed)
	}
}

func TestRequestCheckoutRefreshFromSampleAdmitsTheGivenSample(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	ctx := t.Context()
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
	sample, err := c.sampler.Sample(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := c.sampler.SamplesTaken()
	ticket, err := l.RequestCheckoutRefreshFromSample(ctx, f.checkoutID, f.worktree, sample)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.sampler.SamplesTaken() - before; got != 0 {
		t.Fatalf("admission against a given sample took %d samples of its own, want 0", got)
	}
	c.cycle(ctx)
	result := awaitCheckoutRefresh(t, ticket)
	if result.Err != nil || !result.Reindexed || int64(result.AppliedGeneration) != f.route().DirtyGenerationID {
		t.Fatalf("the ticket = %+v, want the routed generation %d", result, f.route().DirtyGenerationID)
	}
	row, _ := f.generation(int64(result.AppliedGeneration))
	if row.LowerViewFingerprint != sample.Fingerprint {
		t.Fatalf("the published generation describes %s, the admitted sample %s", row.LowerViewFingerprint, sample.Fingerprint)
	}

	// A sample of a state the tree has already left is admitted as given,
	// and the ticket is superseded rather than completed by the newer state.
	stale := sample
	builderWriteFile(t, f.worktree, "island.go", chainIslandEdit)
	ticket, err = l.RequestCheckoutRefreshFromSample(ctx, f.checkoutID, f.worktree, stale)
	if err != nil {
		t.Fatal(err)
	}
	c.cycle(ctx)
	if result := awaitCheckoutRefresh(t, ticket); !errors.Is(result.Err, ErrCheckoutRefreshSuperseded) {
		t.Fatalf("a ticket admitted against a left state = %+v, want superseded", result)
	}
}
