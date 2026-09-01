package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/search"
)

// The coordinator fixture: one family, one primary checkout whose content is
// the base corpus, and one linked worktree the daemon serves automatically.
//
// Everything under it is real. The repository is a real git family with a real
// linked worktree, the corpus is a real index of the primary, the catalog rows
// are written through the catalog's own API, and the builds are the production
// builders. What the tests drive is the coordinator's own decisions, so
// nothing between the signal and the routed generation is stubbed.

const coordinatorAdminName = "worktree"

type coordinatorFixture struct {
	t testing.TB

	store   *store_sqlite.Store
	catalog *store_sqlite.Catalog
	leases  *graphview.LeaseManager

	// primary is the checkout whose working tree the corpus was indexed from.
	primary string
	// worktree is the automatic checkout the coordinator serves.
	worktree string

	familyID   string
	graphID    string
	primaryID  string
	checkoutID string
	// treeA is the committed tree both checkouts start at, and the tree the
	// corpus holds.
	treeA string
}

// newCoordinatorFixture builds the family, indexes the primary and writes the
// catalog identity a coordinator needs to exist.
func newCoordinatorFixture(t testing.TB) *coordinatorFixture {
	t.Helper()
	builderIsolateGit(t)

	family := builderTempDir(t, "family")
	primary := filepath.Join(family, "primary")
	if err := os.MkdirAll(primary, 0o755); err != nil {
		t.Fatalf("mkdir primary: %v", err)
	}
	builderGit(t, primary, "init", "--initial-branch=main")
	builderWriteTree(t, primary, builderTreeA())
	builderGit(t, primary, "add", "-A")
	builderGit(t, primary, "commit", "-m", "A")
	treeA := builderGit(t, primary, "rev-parse", "HEAD^{tree}")

	// The worktree is a real linked one: it shares the object store, which is
	// what lets a commit layer read a tree that was never checked out here.
	worktree := filepath.Join(family, coordinatorAdminName)
	builderGit(t, primary, "worktree", "add", "-b", "feature", worktree)

	store := builderOpenStore(t, "base")
	builderIndex(t, store, primary)

	f := &coordinatorFixture{
		t:          t,
		store:      store,
		catalog:    store.Catalog(),
		leases:     graphview.NewLeaseManager(),
		primary:    primary,
		worktree:   worktree,
		familyID:   "family-coordinator",
		graphID:    GraphIDFor(builderRepoPrefix),
		primaryID:  "checkout-primary",
		checkoutID: "checkout-worktree",
		treeA:      treeA,
	}
	f.writeCatalogIdentity()
	return f
}

// writeCatalogIdentity records what the reconciler would have recorded: the
// family, the two checkouts, and the primary's dedicated graph binding.
func (f *coordinatorFixture) writeCatalogIdentity() {
	f.t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()

	err := f.catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          f.familyID,
		CommonDirIdentity: filepath.Join(f.primary, ".git"),
		State:             reconcile.FamilyStateReady,
		CreatedAt:         now,
		LastSeen:          now,
	})
	if err != nil {
		f.t.Fatalf("upsert family: %v", err)
	}

	primary := store_sqlite.Checkout{
		CheckoutID:     f.primaryID,
		Incarnation:    "incarnation-primary",
		FamilyID:       f.familyID,
		RootPath:       f.primary,
		GitDir:         filepath.Join(f.primary, ".git"),
		AdminName:      "@main",
		State:          store_sqlite.CheckoutStateReady,
		DesiredMode:    store_sqlite.CheckoutModeDedicated,
		EffectiveMode:  store_sqlite.CheckoutModeDedicated,
		HeadRef:        "refs/heads/main",
		HeadTree:       f.treeA,
		LastAccessible: now,
		LastSeen:       now,
	}
	if err := f.catalog.AllocateCheckout(ctx, primary); err != nil {
		f.t.Fatalf("allocate the primary checkout: %v", err)
	}

	automatic := primary
	automatic.CheckoutID = f.checkoutID
	automatic.Incarnation = "incarnation-worktree"
	automatic.RootPath = f.worktree
	automatic.GitDir = filepath.Join(f.primary, ".git", "worktrees", coordinatorAdminName)
	automatic.AdminName = coordinatorAdminName
	automatic.DesiredMode = store_sqlite.CheckoutModeAutomatic
	automatic.EffectiveMode = store_sqlite.CheckoutModeAutomatic
	automatic.HeadRef = "refs/heads/feature"
	if err := f.catalog.AllocateCheckout(ctx, automatic); err != nil {
		f.t.Fatalf("allocate the automatic checkout: %v", err)
	}

	dedicated := store_sqlite.DedicatedGraph{
		GraphID:         f.graphID,
		OwnerCheckoutID: f.primaryID,
		RepoPrefix:      builderRepoPrefix,
		FamilyID:        f.familyID,
		IsPrimaryBase:   true,
		State:           reconcile.GraphStateReady,
	}
	if err := f.catalog.UpsertDedicatedGraph(ctx, dedicated); err != nil {
		f.t.Fatalf("bind the primary dedicated graph: %v", err)
	}

	pipeline := DedicatedBasePipelineFor(config.Default().Index)
	generationID, payload, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:         dedicatedBaseGenerationKind,
		GraphID:           f.graphID,
		LayerID:           f.graphID + ":base",
		CheckoutID:        f.primaryID,
		GenerationKind:    dedicatedBaseGenerationKind,
		TreeOID:           f.treeA,
		ConfigHash:        pipeline.ConfigHash,
		ExtractorVersions: pipeline.ExtractorVersions,
		ResolverVersion:   pipeline.ResolverVersion,
		CreatedAt:         now,
	})
	if err != nil {
		f.t.Fatalf("begin the primary generation: %v", err)
	}
	builderIndex(f.t, payload, f.primary)
	if err := f.store.PublishPayloadGeneration(ctx, generationID, now+1); err != nil {
		f.t.Fatalf("publish the primary generation: %v", err)
	}
	dedicated.ActiveGenerationID = generationID
	if err := f.catalog.UpsertDedicatedGraph(ctx, dedicated); err != nil {
		f.t.Fatalf("activate the primary generation: %v", err)
	}
}

// coordinator builds one over the fixture, with the self-signal off: the tests
// decide when a cycle runs, either by signalling inside a synctest bubble or by
// calling the cycle directly.
func (f *coordinatorFixture) coordinator(t testing.TB, cfg CheckoutCoordinatorConfig) *CheckoutCoordinator {
	t.Helper()
	cfg.CheckoutID = f.checkoutID
	cfg.CheckoutRoot = f.worktree
	cfg.FamilyID = f.familyID
	cfg.GraphID = f.graphID
	cfg.RepoPrefix = builderRepoPrefix
	cfg.WorkspaceID = builderRepoPrefix
	cfg.ProjectID = builderRepoPrefix
	cfg.Store = f.store
	if cfg.Builder == nil {
		cfg.Builder = builderNewBuilder(f.store)
	}
	cfg.Leases = f.leases
	cfg.Config = config.Default().Index
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = -1
	}
	coordinator, err := NewCheckoutCoordinator(cfg)
	if err != nil {
		t.Fatalf("NewCheckoutCoordinator: %v", err)
	}
	// The cleanup is registered on the test that created the coordinator, not
	// on the fixture's: inside a synctest bubble the loop's channels may only
	// be touched from within the bubble, and the bubble's own T is the only
	// one whose cleanup runs there.
	t.Cleanup(func() { _ = coordinator.Close() })
	return coordinator
}

// inertCoordinator returns a coordinator whose loop has already been stopped,
// so the only cycles that run are the ones the test calls.
//
// Everything else about it is a running coordinator: the reuse cache, the
// retirement backlog and the self-signal a rescheduled cycle raises all live on
// the struct rather than in the loop. Stopping the loop is what lets a test
// assert on that self-signal — a running loop would consume it and arm a window
// of its own, which is a second cycle the test did not ask for.
func (f *coordinatorFixture) inertCoordinator(t testing.TB, cfg CheckoutCoordinatorConfig) *CheckoutCoordinator {
	t.Helper()
	coordinator := f.coordinator(t, cfg)
	if err := coordinator.Close(); err != nil {
		t.Fatalf("stop the coordinator loop: %v", err)
	}
	return coordinator
}

// commitTreeB commits the fixture's second tree in the worktree, leaving the
// primary — and therefore the corpus — where it was.
func (f *coordinatorFixture) commitTreeB() string {
	f.t.Helper()
	builderWriteTree(f.t, f.worktree, builderTreeB())
	builderGit(f.t, f.worktree, "add", "-A")
	builderGit(f.t, f.worktree, "commit", "-m", "B")
	return builderGit(f.t, f.worktree, "rev-parse", "HEAD^{tree}")
}

// siblingCheckout allocates a second automatic identity in the family and
// returns its checkout id. It has no coordinator and no working copy: what the
// tests need from it is a second row that can hold a route.
func (f *coordinatorFixture) siblingCheckout(adminName string) string {
	f.t.Helper()
	checkout := store_sqlite.Checkout{
		CheckoutID:     "checkout-" + adminName,
		Incarnation:    "incarnation-" + adminName,
		FamilyID:       f.familyID,
		RootPath:       filepath.Join(filepath.Dir(f.worktree), adminName),
		GitDir:         filepath.Join(f.primary, ".git", "worktrees", adminName),
		AdminName:      adminName,
		State:          store_sqlite.CheckoutStateReady,
		DesiredMode:    store_sqlite.CheckoutModeAutomatic,
		EffectiveMode:  store_sqlite.CheckoutModeAutomatic,
		HeadRef:        "refs/heads/" + adminName,
		HeadTree:       f.treeA,
		LastAccessible: time.Now().Unix(),
		LastSeen:       time.Now().Unix(),
	}
	if err := f.catalog.AllocateCheckout(context.Background(), checkout); err != nil {
		f.t.Fatalf("allocate the sibling checkout: %v", err)
	}
	return checkout.CheckoutID
}

func (f *coordinatorFixture) route() store_sqlite.CheckoutRoute {
	f.t.Helper()
	route, found, err := f.catalog.GetCheckoutRoute(context.Background(), f.checkoutID)
	if err != nil || !found {
		f.t.Fatalf("read route: found=%v err=%v", found, err)
	}
	return route
}

func (f *coordinatorFixture) generation(id int64) (store_sqlite.ViewGeneration, bool) {
	f.t.Helper()
	row, found, err := f.catalog.GetViewGeneration(context.Background(), id)
	if err != nil {
		f.t.Fatalf("read generation %d: %v", id, err)
	}
	return row, found
}

// generations enumerates every generation the checkout coordinator owns. The
// primary's immutable dedicated generation is fixture infrastructure, not an
// attempt or routed layer produced by the coordinator, so it is excluded from
// assertions about coordinator output. Generation ids are a dense autoincrement
// sequence and a retired one leaves a hole, so the walk covers a fixed range.
func (f *coordinatorFixture) generations() []store_sqlite.ViewGeneration {
	f.t.Helper()
	var out []store_sqlite.ViewGeneration
	for id := int64(1); id <= 64; id++ {
		row, found := f.generation(id)
		if !found || (row.GenerationKind == dedicatedBaseGenerationKind && row.CheckoutID == f.primaryID) {
			continue
		}
		out = append(out, row)
	}
	return out
}

// reconcileOnce runs one cycle directly. The loop is not involved: what these
// tests assert about is the decision the cycle makes, and driving it by hand
// keeps every assertion free of a clock.
func coordinatorReconcile(t *testing.T, c *CheckoutCoordinator) CheckoutCycle {
	t.Helper()
	out := c.reconcile(context.Background())
	if out.Err != nil {
		t.Fatalf("reconcile: %v", out.Err)
	}
	return out
}

// --- the debounce -------------------------------------------------------

// TestCoordinatorCoalescesASignalBurst pins the quiet window: a burst of
// signals is one state change, and the coordinator has to see it as one.
//
// The whole test runs on virtual time. Nothing sleeps for real, and the bubble
// refuses to end while the coordinator's goroutine is still alive, so the
// no-leak half of the claim is checked by the bubble rather than asserted.
func TestCoordinatorCoalescesASignalBurst(t *testing.T) {
	f := newCoordinatorFixture(t)

	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var cycles []CheckoutCycle
		c := f.coordinator(t, CheckoutCoordinatorConfig{
			Debounce: 300 * time.Millisecond,
			cycleDone: func(cycle CheckoutCycle) {
				mu.Lock()
				cycles = append(cycles, cycle)
				mu.Unlock()
			},
		})

		for i := 0; i < 5; i++ {
			c.Signal("burst")
			time.Sleep(50 * time.Millisecond)
		}
		synctest.Wait()
		mu.Lock()
		early := len(cycles)
		mu.Unlock()
		if early != 0 {
			t.Fatalf("%d cycles ran while signals were still arriving — the window did not coalesce", early)
		}

		time.Sleep(400 * time.Millisecond)
		synctest.Wait()

		mu.Lock()
		defer mu.Unlock()
		if len(cycles) != 1 {
			t.Fatalf("a burst of 5 signals ran %d cycles, want 1", len(cycles))
		}
		cycle := cycles[0]
		if cycle.Err != nil {
			t.Fatalf("the cycle failed: %v", cycle.Err)
		}
		if !cycle.CommitBuilt || !cycle.DirtyBuilt {
			t.Fatalf("the first cycle built commit=%v dirty=%v, want both", cycle.CommitBuilt, cycle.DirtyBuilt)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestCoordinatorSignalDuringACycleSchedulesTheNextOne pins that a signal
// arriving while a cycle is IN FLIGHT is neither lost nor acted on twice.
//
// The claim only means anything if the signal really lands mid-build, so the
// dirty barrier holds the first cycle inside its build while the second claim
// arrives. Two things have to be true while it is held: no cycle has finished,
// and no second build has started — one loop, one cycle at a time. Once the
// build is released, the claim that arrived during it is worth exactly one
// more cycle, and no more.
func TestCoordinatorSignalDuringACycleSchedulesTheNextOne(t *testing.T) {
	f := newCoordinatorFixture(t)

	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		cycles, builds := 0, 0
		entered := make(chan struct{})
		release := make(chan struct{})
		c := f.coordinator(t, CheckoutCoordinatorConfig{
			Debounce: 300 * time.Millisecond,
			dirtyBarrier: func() {
				mu.Lock()
				builds++
				first := builds == 1
				mu.Unlock()
				if first {
					close(entered)
					<-release
				}
			},
			cycleDone: func(CheckoutCycle) {
				mu.Lock()
				cycles++
				mu.Unlock()
			},
		})

		c.Signal("first")
		<-entered
		c.Signal("second")
		synctest.Wait()

		mu.Lock()
		heldCycles, heldBuilds := cycles, builds
		mu.Unlock()
		if heldCycles != 0 {
			t.Fatalf("%d cycles finished while the first one was still building", heldCycles)
		}
		if heldBuilds != 1 {
			t.Fatalf("%d builds were running at once, want the one the barrier holds", heldBuilds)
		}

		close(release)
		time.Sleep(400 * time.Millisecond)
		synctest.Wait()
		mu.Lock()
		got := cycles
		mu.Unlock()
		if got != 2 {
			t.Fatalf("ran %d cycles, want the one that was running and the one the signal claimed", got)
		}

		// Nothing claimed a third window, so nothing runs one.
		time.Sleep(400 * time.Millisecond)
		synctest.Wait()
		mu.Lock()
		defer mu.Unlock()
		if cycles != 2 {
			t.Fatalf("a coalesced signal ran %d cycles in total", cycles)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// --- the branch-switch cache --------------------------------------------

// TestCoordinatorReusesACommitLayerOnTheWayBack is the cache assertion.
//
// A -> B -> A. The switch back must route the generation the first cycle built
// for A, without invoking the builder at all: the identity of a commit layer
// is the tree it targets over the base it sits on, and neither has changed.
func TestCoordinatorReusesACommitLayerOnTheWayBack(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	commitA := builderGit(t, f.worktree, "rev-parse", "HEAD")

	first := coordinatorReconcile(t, c)
	if !first.CommitBuilt || first.CommitGenerationID == 0 {
		t.Fatalf("the first cycle did not build a commit layer: %+v", first)
	}
	generationA := first.CommitGenerationID

	f.commitTreeB()
	second := coordinatorReconcile(t, c)
	if !second.CommitBuilt {
		t.Fatal("the switch to B reused a generation — B's tree had never been built")
	}
	if second.CommitGenerationID == generationA {
		t.Fatalf("B routed A's generation %d", generationA)
	}
	if !second.DirtyBuilt {
		t.Fatal("the dirty slot must be rebuilt over a new commit layer even when the working tree did not move")
	}

	// Back to A's tree. Detached on purpose: the identity keys on the tree, so
	// arriving at the same content by another route is still a cache hit.
	builderGit(t, f.worktree, "checkout", "--detach", commitA)
	third := coordinatorReconcile(t, c)
	if third.CommitBuilt {
		t.Fatal("the switch back to A re-indexed A's tree — the retained generation was not reused")
	}
	if !third.CommitReused {
		t.Fatalf("the switch back did not report a reuse: %+v", third)
	}
	if third.CommitGenerationID != generationA {
		t.Fatalf("the switch back routed generation %d, want the retained %d",
			third.CommitGenerationID, generationA)
	}
	if got := f.route().CommitGenerationID; got != generationA {
		t.Fatalf("the route names generation %d, want %d", got, generationA)
	}

	// One generation per tree built, and no second one for A.
	commits := 0
	for _, row := range f.generations() {
		if row.GenerationKind == CommitLayerGenerationKind {
			commits++
		}
	}
	if commits != 2 {
		t.Fatalf("%d commit generations exist for two trees visited three times", commits)
	}
}

func TestCoordinatorPublishesCheckoutHeadBeforeTheSwitchedRoute(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)

	treeB := f.commitTreeB()
	cycle := coordinatorReconcile(t, c)
	if cycle.CommitGenerationID == 0 || cycle.DirtyGenerationID == 0 {
		t.Fatalf("switched checkout did not publish both route slots: %+v", cycle)
	}

	sample, err := gitstate.SampleDirty(context.Background(), f.worktree)
	if err != nil {
		t.Fatalf("SampleDirty: %v", err)
	}
	checkout, found, err := f.catalog.GetCheckout(context.Background(), f.checkoutID)
	if err != nil || !found {
		t.Fatalf("GetCheckout: found=%v err=%v", found, err)
	}
	if checkout.HeadRef != sample.HeadRef || checkout.HeadCommit != sample.HeadCommit ||
		checkout.HeadTree != sample.HeadTree || checkout.HeadTree != treeB {
		t.Fatalf("catalog HEAD = %q/%q/%q, sampled %q/%q/%q treeB=%q",
			checkout.HeadRef, checkout.HeadCommit, checkout.HeadTree,
			sample.HeadRef, sample.HeadCommit, sample.HeadTree, treeB)
	}
	route := f.route()
	if !graphview.RouteReady(route) {
		t.Fatalf("switched route is not ready: %+v", route)
	}
	commit, found := f.generation(route.CommitGenerationID)
	if !found || commit.TreeOID != checkout.HeadTree {
		t.Fatalf("routed commit generation tree = %q, checkout HEAD tree = %q",
			commit.TreeOID, checkout.HeadTree)
	}
}

func TestCoordinatorPersistsSameCommitRefSwitchWithoutMovingRoute(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	before := f.route()

	builderGit(t, f.worktree, "checkout", "-b", "same-commit-alias")
	cycle, settled := c.settledWithoutBuild(context.Background())
	if !settled {
		t.Fatal("same-commit ref switch did not settle in the read-only preflight")
	}
	if cycle.CommitBuilt || cycle.DirtyBuilt || cycle.CommitReused {
		t.Fatalf("same-commit ref switch did physical work: %+v", cycle)
	}
	after := f.route()
	if after.RouteEpoch != before.RouteEpoch ||
		after.CommitGenerationID != before.CommitGenerationID ||
		after.DirtyGenerationID != before.DirtyGenerationID {
		t.Fatalf("same-commit ref switch moved route: before=%+v after=%+v", before, after)
	}
	checkout, found, err := f.catalog.GetCheckout(context.Background(), f.checkoutID)
	if err != nil || !found {
		t.Fatalf("GetCheckout: found=%v err=%v", found, err)
	}
	if checkout.HeadRef != "refs/heads/same-commit-alias" {
		t.Fatalf("catalog HEAD ref = %q, want same-commit alias", checkout.HeadRef)
	}
}

func BenchmarkCoordinatorStableHeadObservation(b *testing.B) {
	f := newCoordinatorFixture(b)
	c := f.inertCoordinator(b, CheckoutCoordinatorConfig{})
	ctx := context.Background()
	cycle := c.reconcile(ctx)
	if cycle.Err != nil {
		b.Fatalf("initial reconcile: %v", cycle.Err)
	}
	sample, err := c.sampler.Sample(ctx)
	if err != nil {
		b.Fatal(err)
	}
	sample, err = c.canonicalDirtySnapshot(ctx, sample)
	if err != nil {
		b.Fatal(err)
	}

	writes := 0
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		changed, err := c.updateCheckoutHead(ctx, sample)
		if err != nil {
			b.Fatal(err)
		}
		if changed {
			writes++
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(writes)/float64(b.N), "head-writes/op")
}

// TestCoordinatorReusesCommitAcrossIsolatedDirtyStates combines the cache and
// composition contracts across a real A -> B -> A transition. Each checkout
// state has a distinct tracked edit and non-ignored untracked file; every
// routed stack must equal a flat index of disk, and returning to A must reuse
// A's original commit generation without leaking either dirty state.
func TestCoordinatorReusesCommitAcrossIsolatedDirtyStates(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	commitA := builderGit(t, f.worktree, "rev-parse", "HEAD")

	assertState := func(label string, cycle CheckoutCycle, present, absent []string) {
		t.Helper()
		if cycle.CommitGenerationID == 0 || cycle.DirtyGenerationID == 0 {
			t.Fatalf("%s did not route both layers: %+v", label, cycle)
		}
		reader := coordinatorComposedReader(t, f, cycle.CommitGenerationID, cycle.DirtyGenerationID)
		flat := builderOpenStore(t, "flat-"+label)
		builderIndex(t, flat, f.worktree)
		builderAssertReadersAgree(t, reader, flat)
		builderAssertMasksValidate(t, f.store, cycle.CommitGenerationID)
		builderAssertMasksValidate(t, f.store, cycle.DirtyGenerationID)

		ids := builderNodeIDs(reader)
		for _, id := range present {
			if !slices.Contains(ids, id) {
				t.Errorf("%s view lost %s; ids=%v", label, id, ids)
			}
		}
		for _, id := range absent {
			if slices.Contains(ids, id) {
				t.Errorf("%s view leaked %s; ids=%v", label, id, ids)
			}
		}
	}
	graphID := func(file, symbol string) string {
		return builderRepoPrefix + "/" + file + "::" + symbol
	}

	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc AWorkingEdit() {}\n")
	builderWriteFile(t, f.worktree, "a_untracked.go", "package fixture\n\nfunc AUntracked() {}\n")
	if got := builderGit(t, f.worktree, "ls-files", "--others", "--exclude-standard"); got != "a_untracked.go" {
		t.Fatalf("A non-ignored untracked files = %q, want a_untracked.go", got)
	}
	first := coordinatorReconcile(t, c)
	if !first.CommitBuilt || !first.DirtyBuilt {
		t.Fatalf("A did not build its initial pair: %+v", first)
	}
	generationA, dirtyA := first.CommitGenerationID, first.DirtyGenerationID
	assertState("a-first", first,
		[]string{graphID("helper.go", "AWorkingEdit"), graphID("a_untracked.go", "AUntracked")},
		[]string{graphID("helper.go", "BWorkingEdit"), graphID("b_untracked.go", "BUntracked")})

	builderGit(t, f.worktree, "checkout", "--", ".")
	if err := os.Remove(filepath.Join(f.worktree, "a_untracked.go")); err != nil {
		t.Fatalf("remove A's untracked file: %v", err)
	}
	f.commitTreeB()
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc BWorkingEdit() {}\n")
	builderWriteFile(t, f.worktree, "b_untracked.go", "package fixture\n\nfunc BUntracked() {}\n")
	if got := builderGit(t, f.worktree, "ls-files", "--others", "--exclude-standard"); got != "b_untracked.go" {
		t.Fatalf("B non-ignored untracked files = %q, want b_untracked.go", got)
	}
	second := coordinatorReconcile(t, c)
	if !second.CommitBuilt || second.CommitGenerationID == generationA {
		t.Fatalf("B did not build a distinct commit generation: %+v", second)
	}
	if !second.DirtyBuilt || second.DirtyGenerationID == dirtyA {
		t.Fatalf("B did not build a distinct dirty generation: %+v", second)
	}
	assertState("b", second,
		[]string{graphID("helper.go", "BWorkingEdit"), graphID("b_untracked.go", "BUntracked")},
		[]string{graphID("helper.go", "AWorkingEdit"), graphID("a_untracked.go", "AUntracked")})

	builderGit(t, f.worktree, "checkout", "--", ".")
	if err := os.Remove(filepath.Join(f.worktree, "b_untracked.go")); err != nil {
		t.Fatalf("remove B's untracked file: %v", err)
	}
	builderGit(t, f.worktree, "checkout", "--detach", commitA)
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc AReturnEdit() {}\n")
	builderWriteFile(t, f.worktree, "a_return_untracked.go", "package fixture\n\nfunc AReturnUntracked() {}\n")
	if got := builderGit(t, f.worktree, "ls-files", "--others", "--exclude-standard"); got != "a_return_untracked.go" {
		t.Fatalf("returned-A non-ignored untracked files = %q, want a_return_untracked.go", got)
	}
	third := coordinatorReconcile(t, c)
	if third.CommitBuilt || !third.CommitReused || third.CommitGenerationID != generationA {
		t.Fatalf("return to A did not reuse commit generation %d: %+v", generationA, third)
	}
	if !third.DirtyBuilt || third.DirtyGenerationID == dirtyA || third.DirtyGenerationID == second.DirtyGenerationID {
		t.Fatalf("returned A did not build an isolated dirty generation: %+v", third)
	}
	assertState("a-return", third,
		[]string{graphID("helper.go", "AReturnEdit"), graphID("a_return_untracked.go", "AReturnUntracked")},
		[]string{graphID("helper.go", "BWorkingEdit"), graphID("b_untracked.go", "BUntracked")})

	commits := 0
	for _, row := range f.generations() {
		if row.GenerationKind == CommitLayerGenerationKind {
			commits++
		}
	}
	if commits != 2 {
		t.Fatalf("%d commit generations exist for two trees visited three times", commits)
	}
}

func BenchmarkCoordinatorStableAdoptedCommitDirtyReconcile(b *testing.B) {
	f := newCoordinatorFixture(b)
	ctx := context.Background()

	const siblingName = "benchmark-sibling"
	siblingRoot := filepath.Join(filepath.Dir(f.worktree), siblingName)
	builderGit(b, f.primary, "worktree", "add", "-b", siblingName, siblingRoot)
	siblingID := f.siblingCheckout(siblingName)

	checkoutID, worktree := f.checkoutID, f.worktree
	f.checkoutID, f.worktree = siblingID, siblingRoot
	sibling := f.inertCoordinator(b, CheckoutCoordinatorConfig{})
	siblingCycle := sibling.reconcile(ctx)
	if siblingCycle.Err != nil {
		b.Fatalf("build sibling commit layer: %v", siblingCycle.Err)
	}
	siblingRoute := f.route()
	if siblingRoute.CommitGenerationID <= 0 {
		b.Fatal("sibling routed no commit generation")
	}

	f.checkoutID, f.worktree = checkoutID, worktree
	dirtyPath := filepath.Join(f.worktree, "benchmark_dirty.go")
	if err := os.WriteFile(dirtyPath, []byte("package fixture\n\nfunc BenchmarkDirty() {}\n"), 0o644); err != nil {
		b.Fatalf("write dirty benchmark file: %v", err)
	}
	coordinator := f.inertCoordinator(b, CheckoutCoordinatorConfig{})
	adopted := coordinator.reconcile(ctx)
	if adopted.Err != nil {
		b.Fatalf("adopt sibling commit layer: %v", adopted.Err)
	}
	initialRoute := f.route()
	if initialRoute.CommitGenerationID != siblingRoute.CommitGenerationID || initialRoute.DirtyGenerationID <= 0 {
		b.Fatalf("route did not adopt commit %d with a dirty layer: %+v", siblingRoute.CommitGenerationID, initialRoute)
	}

	physicalBuilds := 0
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		cycle := coordinator.reconcile(ctx)
		if cycle.Err != nil {
			b.Fatalf("stable reconcile: %v", cycle.Err)
		}
		if cycle.CommitBuilt {
			physicalBuilds++
		}
		if cycle.DirtyBuilt {
			physicalBuilds++
		}
	}
	b.StopTimer()

	finalRoute := f.route()
	if finalRoute.CommitGenerationID != initialRoute.CommitGenerationID ||
		finalRoute.DirtyGenerationID != initialRoute.DirtyGenerationID {
		b.Fatalf("stable reconcile moved generations: before=%+v after=%+v", initialRoute, finalRoute)
	}
	b.ReportMetric(float64(physicalBuilds)/float64(b.N), "physical-builds/op")
	b.ReportMetric(float64(finalRoute.RouteEpoch-initialRoute.RouteEpoch)/float64(b.N), "route-epoch-advances/op")
}

// TestCoordinatorLeavesASettledRouteAlone pins the cheapest outcome: a cycle
// on a checkout nobody has touched builds nothing and flips nothing.
func TestCoordinatorLeavesASettledRouteAlone(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})

	first := coordinatorReconcile(t, c)
	epoch := f.route().RouteEpoch

	// Keep the build gate closed. A settled poll must finish from the read-only
	// preflight instead of joining the derived-build queue.
	c.gate = NewViewBuildGate()
	var second CheckoutCycle
	c.cycleDone = func(out CheckoutCycle) { second = out }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c.cycle(ctx)
	if second.Err != nil {
		t.Fatalf("settled preflight waited for build admission: %v", second.Err)
	}
	if second.CommitBuilt || second.DirtyBuilt || second.CommitReused {
		t.Fatalf("a cycle on an unchanged checkout did work: %+v", second)
	}
	if second.CommitGenerationID != first.CommitGenerationID ||
		second.DirtyGenerationID != first.DirtyGenerationID {
		t.Fatalf("the route moved without a build: %+v then %+v", first, second)
	}
	if got := f.route().RouteEpoch; got != epoch {
		t.Fatalf("route epoch moved from %d to %d without a flip", epoch, got)
	}
}

// TestCoordinatorRebuildsOnlyTheDirtySlot pins the slot split: an edit that
// never reaches a commit changes what the working tree holds and nothing about
// what the checkout's HEAD names.
func TestCoordinatorRebuildsOnlyTheDirtySlot(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})

	first := coordinatorReconcile(t, c)
	builderWriteFile(t, f.worktree, "helper.go", `package fixture

func Helper() {
	// reworked in the working tree
}
`)
	second := coordinatorReconcile(t, c)
	if second.CommitBuilt || second.CommitReused {
		t.Fatalf("an uncommitted edit touched the commit slot: %+v", second)
	}
	if second.CommitGenerationID != first.CommitGenerationID {
		t.Fatalf("the commit slot moved from %d to %d over an uncommitted edit",
			first.CommitGenerationID, second.CommitGenerationID)
	}
	if !second.DirtyBuilt || second.DirtyGenerationID == first.DirtyGenerationID {
		t.Fatalf("the dirty slot did not rebuild: %+v", second)
	}

	row, found := f.generation(second.DirtyGenerationID)
	if !found {
		t.Fatalf("generation %d is not in the catalog", second.DirtyGenerationID)
	}
	if row.BaseGenerationID != second.CommitGenerationID {
		t.Fatalf("the dirty generation sits on %d, want the routed commit generation %d",
			row.BaseGenerationID, second.CommitGenerationID)
	}
	if row.LowerViewFingerprint == "" {
		t.Fatal("the dirty generation records no working-tree fingerprint to compare against")
	}
}

// TestCoordinatorCleanCheckoutReachesAReadyRoute pins the state an automatic
// worktree spends most of its life in: committed past the base corpus and with
// nothing uncommitted at all.
//
// Its working-tree layer describes no change, which is a whole answer and not
// an absence — a route naming a commit generation and no dirty one is not
// servable, so a checkout that stops there serves the base corpus forever. The
// cycle that flips the commit slot has to settle the dirty slot too, and it has
// to do it from the loop's own scheduling: an automatic checkout with a quiet
// working tree receives no filesystem event to nudge it with.
func TestCoordinatorCleanCheckoutReachesAReadyRoute(t *testing.T) {
	f := newCoordinatorFixture(t)
	// The commit layer has real work to do — the checkout's tree is not the
	// corpus's — while the working tree holds nothing the commit does not.
	f.commitTreeB()

	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var cycles []CheckoutCycle
		c := f.coordinator(t, CheckoutCoordinatorConfig{
			Debounce:     300 * time.Millisecond,
			PollInterval: 15 * time.Second,
			cycleDone: func(cycle CheckoutCycle) {
				mu.Lock()
				cycles = append(cycles, cycle)
				mu.Unlock()
			},
		})

		// The one signal the lifecycle raises when it installs a coordinator.
		// Nothing else touches this checkout: the working tree is quiet, so no
		// watcher event follows.
		c.Signal("checkout registered")
		time.Sleep(time.Second)
		synctest.Wait()

		mu.Lock()
		ran := slices.Clone(cycles)
		mu.Unlock()
		if len(ran) == 0 {
			t.Fatal("the registration signal ran no cycle")
		}
		for _, cycle := range ran {
			if cycle.Err != nil {
				t.Fatalf("a cycle failed: %v", cycle.Err)
			}
		}

		route := f.route()
		if route.State != store_sqlite.RouteActive {
			t.Fatalf("the route is %q, want an active one: %+v", route.State, route)
		}
		if route.CommitGenerationID == 0 || route.DirtyGenerationID == 0 {
			t.Fatalf("a clean checkout settled on a half-routed stack: %+v", route)
		}

		row, found := f.generation(route.DirtyGenerationID)
		if !found || !servableGeneration(row.State) {
			t.Fatalf("the routed working-tree generation %d is %q", route.DirtyGenerationID, row.State)
		}
		if row.GenerationKind != DirtyLayerGenerationKind {
			t.Fatalf("the dirty slot names a %q generation", row.GenerationKind)
		}
		if row.BaseGenerationID != route.CommitGenerationID {
			t.Fatalf("the working-tree layer sits on %d, want the routed commit generation %d",
				row.BaseGenerationID, route.CommitGenerationID)
		}
		if row.CoveredFiles != 0 {
			t.Fatalf("a clean working tree's layer covers %d files, want none", row.CoveredFiles)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// --- a checkout that moves under a build --------------------------------

// TestCoordinatorRebuildsOnceWhenTheWorkingTreeMoves pins the single retry: a
// build whose checkout moved under it is worth exactly one more attempt, and
// what gets published is the state the checkout ended in.
func TestCoordinatorRebuildsOnceWhenTheWorkingTreeMoves(t *testing.T) {
	f := newCoordinatorFixture(t)

	var once sync.Once
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{
		dirtyBarrier: func() {
			once.Do(func() {
				builderWriteFile(t, f.worktree, "sneaked.go", `package fixture

func Sneaked() {
}
`)
			})
		},
	})

	cycle := coordinatorReconcile(t, c)
	if !cycle.DirtyBuilt || cycle.DirtyGenerationID == 0 {
		t.Fatalf("the retry did not publish a working-tree layer: %+v", cycle)
	}
	if cycle.Rescheduled {
		t.Fatalf("one supersede rescheduled instead of retrying: %+v", cycle)
	}
	if got := f.route().DirtyGenerationID; got != cycle.DirtyGenerationID {
		t.Fatalf("the route names dirty generation %d, want the retried %d",
			got, cycle.DirtyGenerationID)
	}

	// The published generation must describe the state that includes the file
	// the barrier sneaked in, not the one the first attempt read.
	composed := coordinatorComposedReader(t, f, cycle.CommitGenerationID, cycle.DirtyGenerationID)
	if composed.GetNode(builderRepoPrefix+"/sneaked.go::Sneaked") == nil {
		t.Fatal("the published layer describes the state the first attempt read, not the one on disk")
	}

	// The torn attempt was thrown away AND collected. Nothing routes it, so
	// leaving its payload behind would cost one sparse generation per edit that
	// lands while a build is running — which is what an editor saving twice in
	// a second does.
	route := f.route()
	for _, row := range f.generations() {
		if row.State == store_sqlite.ViewGenerationBuilding {
			t.Fatalf("generation %d was left building", row.GenerationID)
		}
		if row.GenerationID != route.CommitGenerationID && row.GenerationID != route.DirtyGenerationID {
			t.Fatalf("generation %d (%s) outlived the attempt that produced it",
				row.GenerationID, row.State)
		}
	}
}

// TestCoordinatorKeepsThePreviousRouteWhenEveryBuildIsTorn pins the refusal: a
// checkout under a stream of edits keeps the last coherent view it had, and
// the coordinator asks for another window rather than spinning.
func TestCoordinatorKeepsThePreviousRouteWhenEveryBuildIsTorn(t *testing.T) {
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

	settled := coordinatorReconcile(t, c)
	if settled.DirtyGenerationID == 0 {
		t.Fatalf("the first cycle settled no working-tree layer: %+v", settled)
	}
	before := f.route()

	moving = true
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc Helper() {}\n")
	torn := coordinatorReconcile(t, c)

	if !torn.Rescheduled {
		t.Fatalf("two torn builds did not reschedule: %+v", torn)
	}
	if edits != 2 {
		t.Fatalf("the barrier fired %d times, want exactly two attempts", edits)
	}
	after := f.route()
	if after.DirtyGenerationID != before.DirtyGenerationID {
		t.Fatalf("the dirty route moved from %d to %d over two torn builds",
			before.DirtyGenerationID, after.DirtyGenerationID)
	}
	if after.RouteEpoch != before.RouteEpoch {
		t.Fatalf("the route epoch moved from %d to %d without a flip",
			before.RouteEpoch, after.RouteEpoch)
	}
	if len(c.signal) != 1 {
		t.Fatal("the coordinator did not signal itself for another window")
	}
	for _, row := range f.generations() {
		if row.State == store_sqlite.ViewGenerationBuilding {
			t.Fatalf("generation %d was left building", row.GenerationID)
		}
	}
}

// --- a route that moved under the coordinator ---------------------------

// TestCoordinatorRescheduleWhenTheRouteMovesUnderIt pins the compare-and-set:
// a second coordinator on the same checkout is a legal thing to exist, and the
// one that loses the flip must leave the winner's route alone.
func TestCoordinatorRescheduleWhenTheRouteMovesUnderIt(t *testing.T) {
	f := newCoordinatorFixture(t)

	loser := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	winner := f.inertCoordinator(t, CheckoutCoordinatorConfig{})

	// The loser reads the route, and only then does the winner move it. That
	// is the race, taken apart: both cycles are real, and the interleaving is
	// the one the epoch guard exists for.
	base, err := loser.primaryBase(context.Background())
	if err != nil {
		t.Fatalf("primaryBase: %v", err)
	}
	stale, err := loser.ensureRoute(context.Background(), base)
	if err != nil {
		t.Fatalf("ensureRoute: %v", err)
	}
	won := coordinatorReconcile(t, winner)
	if won.CommitGenerationID == 0 {
		t.Fatalf("the winner routed nothing: %+v", won)
	}

	head := builderGit(t, f.worktree, "rev-parse", "HEAD^{tree}")
	var out CheckoutCycle
	_, err = loser.reconcileCommitSlot(context.Background(), base, head, &stale, &out)
	if err == nil {
		t.Fatal("the loser flipped a route whose epoch had moved")
	}
	if !errors.Is(err, errRouteMoved) {
		t.Fatalf("the loser failed with %v, want a lost route flip", err)
	}
	// The loser may resolve the winner's canonical generation from the ready
	// cache before its guarded bind loses. Building is not required for the
	// compare-and-set invariant; preserving the winning route is.

	route := f.route()
	if route.CommitGenerationID != won.CommitGenerationID {
		t.Fatalf("the route names %d, want the winner's %d",
			route.CommitGenerationID, won.CommitGenerationID)
	}

	// What the loser built is whole, published and routed by nobody. Nothing
	// refuses its retirement — no route names it, no layer sits on it, no view
	// leases it — so the offer that follows the supersede collects it outright
	// and only the winner's generations are left.
	for _, row := range f.generations() {
		if row.State == store_sqlite.ViewGenerationBuilding {
			t.Fatalf("generation %d was left building", row.GenerationID)
		}
		if row.GenerationID != route.CommitGenerationID && row.GenerationID != route.DirtyGenerationID {
			t.Fatalf("the loser's generation %d (%s) was left in the database",
				row.GenerationID, row.State)
		}
	}
}

// --- the two slots as one view ------------------------------------------

// TestCoordinatorNeverRoutesAWorkingTreeLayerOverAnotherTree is the composed-
// view invariant: the two slots of a route are only ever readable as a pair
// that describes one state of the checkout.
//
// A branch switch moves the commit slot and leaves the working-tree layer of
// the branch just left behind. That layer over the new tree is not a stale
// view of the checkout — it is a view of a repository that has never existed.
// The barrier stops the cycle between the two flips, which is exactly the
// window a query lands in during the rebuild, and asks what would be served.
func TestCoordinatorNeverRoutesAWorkingTreeLayerOverAnotherTree(t *testing.T) {
	f := newCoordinatorFixture(t)
	materializer := &graphview.Materializer{Store: f.store, Catalog: f.catalog, Leases: f.leases}

	watching := false
	midBuild := store_sqlite.CheckoutRoute{}
	var served []int64
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{
		// The barrier runs on this test's own goroutine, between the commit
		// flip and the dirty one.
		dirtyBarrier: func() {
			if !watching {
				return
			}
			watching = false
			midBuild = f.route()
			view, err := materializer.MaterializeCheckout(context.Background(), f.checkoutID)
			if err != nil {
				// A materializer that refuses outright is the honest answer
				// too; what must not happen is a view being served.
				return
			}
			served = view.Generations()
			view.Close()
		},
	})

	first := coordinatorReconcile(t, c)
	if first.CommitGenerationID == 0 || first.DirtyGenerationID == 0 {
		t.Fatalf("the first cycle did not route both slots: %+v", first)
	}

	f.commitTreeB()
	watching = true
	second := coordinatorReconcile(t, c)
	if second.CommitGenerationID == first.CommitGenerationID {
		t.Fatalf("the commit slot did not move: %+v", second)
	}
	if midBuild.CheckoutID == "" {
		t.Fatal("the barrier never ran, so nothing about the window was observed")
	}

	// Mid-build the route must not claim to be servable, and it must not still
	// name the layer built over the tree the checkout has left.
	if graphview.RouteReady(midBuild) {
		t.Fatalf("the route reported itself ready mid-build: %+v", midBuild)
	}
	if midBuild.DirtyGenerationID != 0 {
		t.Fatalf("the route still named working-tree generation %d over commit generation %d",
			midBuild.DirtyGenerationID, midBuild.CommitGenerationID)
	}
	if slices.Contains(served, first.DirtyGenerationID) {
		t.Fatalf("a view served %v, which stacks the old branch's working tree on the new one", served)
	}

	// And once the rebuild lands, the pair describes one state again.
	settled := f.route()
	if !graphview.RouteReady(settled) {
		t.Fatalf("the route did not come back up: %+v", settled)
	}
	row, found := f.generation(settled.DirtyGenerationID)
	if !found || row.BaseGenerationID != settled.CommitGenerationID {
		t.Fatalf("the routed working-tree generation sits on %d, not on the routed commit generation %d",
			row.BaseGenerationID, settled.CommitGenerationID)
	}
	if _, found := f.generation(first.DirtyGenerationID); found {
		t.Fatalf("the withdrawn working-tree generation %d was left behind", first.DirtyGenerationID)
	}
}

// TestCoordinatorRefusesAWorkingTreeLayerOverAStaleHead is the same invariant
// read across the other seam a cycle has: the HEAD the cycle started from.
//
// A commit layer takes minutes on a real repository, and a checkout is free to
// move while one is being built for it. The working-tree layer is sampled after
// that build lands, so it describes the checkout's NEW head — and stacking it on
// a commit layer built for the head the cycle started from composes one tree's
// content with another tree's absence of uncommitted change, which is a state
// the checkout has never been in. Worse, the pair is coherent enough to publish
// and route, so the checkout goes ready serving it.
//
// The two halves of the cycle are driven by hand because that is the interleaving
// itself: the commit slot is settled against the head the cycle sampled, the
// checkout commits, and only then is the working-tree slot reconciled.
func TestCoordinatorRefusesAWorkingTreeLayerOverAStaleHead(t *testing.T) {
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
	if !out.CommitBuilt || commitGeneration == 0 {
		t.Fatalf("the commit slot was not settled: %+v", out)
	}

	// The checkout commits while the layer for A is being built. Its working
	// tree is clean again the moment it lands, so nothing about the sample the
	// dirty half takes says that the tree underneath it has moved.
	f.commitTreeB()

	if err := c.reconcileDirtySlot(ctx, base.generationID, commitGeneration, treeA, &route, &out); err != nil {
		t.Fatalf("reconcileDirtySlot: %v", err)
	}
	if !out.Rescheduled {
		t.Fatalf("the cycle settled the dirty slot against a head it did not build for: %+v", out)
	}
	if out.DirtyGenerationID != 0 {
		t.Fatalf("the cycle routed working-tree generation %d over a commit layer for another tree",
			out.DirtyGenerationID)
	}
	for _, row := range f.generations() {
		if row.GenerationKind == DirtyLayerGenerationKind {
			t.Fatalf("generation %d describes a working tree over a commit layer for another tree",
				row.GenerationID)
		}
	}

	stored := f.route()
	if stored.DirtyGenerationID != 0 {
		t.Fatalf("the route names working-tree generation %d: %+v", stored.DirtyGenerationID, stored)
	}
	if graphview.RouteReady(stored) {
		t.Fatalf("the route reported itself ready over a stale commit layer: %+v", stored)
	}
	if len(c.signal) != 1 {
		t.Fatal("the coordinator did not signal itself for another window")
	}

	// The next cycle is what settles it: it samples the head the checkout is
	// really at, rebuilds the commit slot for it, and only then lays the working
	// tree over it.
	settled := coordinatorReconcile(t, c)
	if !settled.CommitBuilt || !settled.DirtyBuilt {
		t.Fatalf("the follow-up cycle did not rebuild both slots: %+v", settled)
	}
	row, found := f.generation(settled.DirtyGenerationID)
	if !found || row.BaseGenerationID != settled.CommitGenerationID {
		t.Fatalf("the routed working-tree generation sits on %d, not on the routed commit generation %d",
			row.BaseGenerationID, settled.CommitGenerationID)
	}
	if !graphview.RouteReady(f.route()) {
		t.Fatalf("the checkout did not come up: %+v", f.route())
	}
}

// --- retirement ---------------------------------------------------------

// TestCoordinatorSweepCollectsATornAttempt pins the whole retirement path for
// the commonest throwaway there is: a build the working tree moved under.
//
// The attempt is a complete payload for a state that no longer exists, and
// nothing will ever route it. A reader holding it is the one reason not to
// collect it on the spot, and the reason stops applying when the reader
// closes — which is what the janitor's sweep is for.
func TestCoordinatorSweepCollectsATornAttempt(t *testing.T) {
	f := newCoordinatorFixture(t)
	ctx := context.Background()

	var (
		once  sync.Once
		torn  int64
		lease *graphview.Lease
	)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{
		dirtyBarrier: func() {
			once.Do(func() {
				for _, row := range f.generations() {
					if row.State == store_sqlite.ViewGenerationBuilding {
						torn = row.GenerationID
					}
				}
				lease = f.leases.Acquire(torn)
				builderWriteFile(t, f.worktree, "sneaked.go", "package fixture\n\nfunc Sneaked() {}\n")
			})
		},
	})

	cycle := coordinatorReconcile(t, c)
	if torn == 0 {
		t.Fatal("no generation was in flight when the barrier ran")
	}
	if torn == cycle.DirtyGenerationID {
		t.Fatalf("the routed generation %d is the one the tear threw away", torn)
	}

	row, found := f.generation(torn)
	if !found {
		t.Fatalf("generation %d was collected while a reader still held it", torn)
	}
	if row.State != store_sqlite.ViewGenerationSuperseded {
		t.Fatalf("the torn attempt is %q, want superseded", row.State)
	}
	if retired := c.SweepRetirements(ctx); retired != 0 {
		t.Fatalf("the sweep collected %d leased generations", retired)
	}

	lease.Release()
	if retired := c.SweepRetirements(ctx); retired != 1 {
		t.Fatalf("the sweep collected %d generations after the reader closed, want the torn attempt", retired)
	}
	if _, found := f.generation(torn); found {
		t.Fatalf("generation %d survived its retirement", torn)
	}
}

// TestCoordinatorLeavesAGenerationAnotherCheckoutRoutes pins the guard that
// makes one payload shareable: retirement asks the catalog whether anything
// still points at the generation, so a second checkout routed to the very same
// build keeps it alive after the first checkout has moved on.
func TestCoordinatorLeavesAGenerationAnotherCheckoutRoutes(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{Retain: 1})
	ctx := context.Background()

	first := coordinatorReconcile(t, c)
	sibling := f.siblingCheckout("sibling")
	err := f.catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
		CheckoutID:         sibling,
		GraphID:            f.graphID,
		CommitGenerationID: first.CommitGenerationID,
		State:              store_sqlite.RouteActive,
	})
	if err != nil {
		t.Fatalf("route the sibling checkout: %v", err)
	}

	// A cache of one drops the generation the moment the checkout commits, so
	// the coordinator offers it for retirement with the sibling still on it.
	f.commitTreeB()
	second := coordinatorReconcile(t, c)
	if second.CommitGenerationID == first.CommitGenerationID {
		t.Fatal("the commit slot did not move")
	}
	if retired := c.SweepRetirements(ctx); retired != 0 {
		t.Fatalf("the sweep collected %d generations another checkout is routed to", retired)
	}
	if _, found := f.generation(first.CommitGenerationID); !found {
		t.Fatalf("generation %d was collected out from under the sibling's route", first.CommitGenerationID)
	}

	// The sibling leaves. Nothing names the generation now, so the offer the
	// coordinator kept on its backlog finally succeeds.
	if err := f.catalog.DeleteCheckoutRoute(ctx, sibling); err != nil {
		t.Fatalf("withdraw the sibling's route: %v", err)
	}
	if retired := c.SweepRetirements(ctx); retired != 1 {
		t.Fatalf("the sweep collected %d generations once the sibling left, want 1", retired)
	}
	if _, found := f.generation(first.CommitGenerationID); found {
		t.Fatalf("generation %d survived the last route that named it", first.CommitGenerationID)
	}
}

// TestCoordinatorRetiresAReplacedGenerationOnceItIsUnleased pins the lease
// integration: the generation a route left is collectable, and a live view
// holding it is what stops the collection until the view closes.
func TestCoordinatorRetiresAReplacedGenerationOnceItIsUnleased(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})

	first := coordinatorReconcile(t, c)
	materializer := &graphview.Materializer{Store: f.store, Catalog: f.catalog, Leases: f.leases}
	view, err := materializer.MaterializeCheckout(context.Background(), f.checkoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	if !slices.Contains(view.Generations(), first.DirtyGenerationID) {
		t.Fatalf("the view leases %v, which does not include the routed dirty generation %d",
			view.Generations(), first.DirtyGenerationID)
	}

	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc Helper() {}\n")
	second := coordinatorReconcile(t, c)
	if second.DirtyGenerationID == first.DirtyGenerationID {
		t.Fatal("the dirty slot did not move, so nothing was replaced")
	}
	if _, found := f.generation(first.DirtyGenerationID); !found {
		t.Fatal("a generation under a live view was collected")
	}
	if retired := c.SweepRetirements(context.Background()); retired != 0 {
		t.Fatalf("the janitor collected %d leased generations", retired)
	}

	view.Close()
	if retired := c.SweepRetirements(context.Background()); retired != 1 {
		t.Fatalf("the janitor collected %d generations after the view closed, want 1", retired)
	}
	if _, found := f.generation(first.DirtyGenerationID); found {
		t.Fatalf("generation %d survived its retirement", first.DirtyGenerationID)
	}
	if _, found := f.generation(second.DirtyGenerationID); !found {
		t.Fatalf("the routed generation %d was collected", second.DirtyGenerationID)
	}
}

// TestCoordinatorRetiresACommitLayerTheCacheEvicts pins the other end of the
// reuse cache: a generation the cache cannot hold any more is retire-eligible,
// and the retire that a live layer above it refuses is retried by the janitor.
func TestCoordinatorRetiresACommitLayerTheCacheEvicts(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{Retain: 1})

	first := coordinatorReconcile(t, c)
	f.commitTreeB()
	second := coordinatorReconcile(t, c)
	if second.CommitGenerationID == first.CommitGenerationID {
		t.Fatal("the commit slot did not move")
	}

	// A cache of one has evicted A's generation. Its retire was refused while
	// A's working-tree layer still named it as a base; the janitor retries
	// after that layer has been collected in its turn.
	c.SweepRetirements(context.Background())
	if _, found := f.generation(first.CommitGenerationID); found {
		t.Fatalf("generation %d survived eviction and two retire attempts", first.CommitGenerationID)
	}
	if _, found := f.generation(first.DirtyGenerationID); found {
		t.Fatalf("the replaced dirty generation %d was never collected", first.DirtyGenerationID)
	}
	if _, found := f.generation(second.CommitGenerationID); !found {
		t.Fatalf("the routed commit generation %d was collected", second.CommitGenerationID)
	}
}

// --- closing ------------------------------------------------------------

// TestCoordinatorCloseCancelsAnInFlightBuild pins shutdown at the last safe
// publication boundary. The dirty payload has been filled but is held before
// publish; Close cancels its build context, waits for abandonment, and returns
// without routing or publishing that canceled generation.
func TestCoordinatorCloseCancelsAnInFlightBuild(t *testing.T) {
	f := newCoordinatorFixture(t)

	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{})
		var once sync.Once
		var c *CheckoutCoordinator
		c = f.coordinator(t, CheckoutCoordinatorConfig{
			Debounce: 300 * time.Millisecond,
			dirtyBarrier: func() {
				once.Do(func() {
					close(entered)
					<-c.lifetimeContext().Done()
				})
			},
		})

		c.Signal("edit")
		<-entered

		closed := make(chan error, 1)
		go func() { closed <- c.Close() }()
		synctest.Wait()
		if err := <-closed; err != nil {
			t.Fatalf("Close: %v", err)
		}
		if c.Running() {
			t.Fatal("coordinator is still running after Close")
		}

		route := f.route()
		if route.DirtyGenerationID != 0 {
			t.Fatalf("close routed the canceled dirty generation: %+v", route)
		}
		for _, row := range f.generations() {
			if row.State == store_sqlite.ViewGenerationBuilding {
				t.Fatalf("close left generation %d building", row.GenerationID)
			}
			if row.GenerationKind == DirtyLayerGenerationKind && servableGeneration(row.State) {
				t.Fatalf("canceled dirty generation %d was published as %q", row.GenerationID, row.State)
			}
		}
	})
}

// TestCommitLayerReaderUsesRecordedNonzeroBase pins the ancestry seam used by
// dirty builds. An unchanged node must come from the commit generation's
// catalog-recorded base, never from the shared generation-zero corpus.
func TestCommitLayerReaderUsesRecordedNonzeroBase(t *testing.T) {
	f := newCoordinatorFixture(t)
	ctx := context.Background()
	filePath := builderRepoPrefix + "/nonzero_base.go"
	nodeID := filePath + "::BaseOnly"
	nodeAt := func(line int) *graph.Node {
		return &graph.Node{
			ID:         nodeID,
			Kind:       graph.KindFunction,
			Name:       "BaseOnly",
			FilePath:   filePath,
			StartLine:  line,
			EndLine:    line + 1,
			Language:   "go",
			RepoPrefix: builderRepoPrefix,
		}
	}

	// Poison generation zero so the former hard-coded ancestry returns a
	// deterministic wrong answer.
	f.store.AddBatch([]*graph.Node{nodeAt(99)}, nil)
	baseGeneration, baseHandle, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:      checkoutLayerOwnerKind,
		GraphID:        f.graphID,
		LayerID:        "test-nonzero-base",
		CheckoutID:     f.primaryID,
		GenerationKind: "dedicated",
		TreeOID:        "tree-nonzero-base",
		CreatedAt:      1000,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(base): %v", err)
	}
	baseHandle.AddBatch([]*graph.Node{nodeAt(5)}, nil)
	if err := f.store.PublishPayloadGeneration(ctx, baseGeneration, 1001); err != nil {
		t.Fatalf("PublishPayloadGeneration(base): %v", err)
	}

	commitGeneration, _, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:        checkoutLayerOwnerKind,
		GraphID:          f.graphID,
		LayerID:          "test-commit-over-nonzero-base",
		CheckoutID:       f.checkoutID,
		GenerationKind:   CommitLayerGenerationKind,
		BaseGenerationID: baseGeneration,
		TreeOID:          "tree-commit-over-nonzero-base",
		CreatedAt:        1002,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(commit): %v", err)
	}
	if err := f.store.PublishPayloadGeneration(ctx, commitGeneration, 1003); err != nil {
		t.Fatalf("PublishPayloadGeneration(commit): %v", err)
	}

	coordinator := &CheckoutCoordinator{store: f.store, catalog: f.catalog}
	reader, err := coordinator.commitLayerReader(ctx, commitGeneration)
	if err != nil {
		t.Fatalf("commitLayerReader: %v", err)
	}
	if got := reader.GetNode(nodeID); got == nil || got.StartLine != 5 {
		t.Fatalf("unchanged node = %+v, want catalog-recorded base line 5", got)
	}
}

// --- the composed view --------------------------------------------------

// coordinatorComposedReader stacks a checkout's two routed generations exactly
// as the materializer does, without taking a lease.
func coordinatorComposedReader(
	t *testing.T, f *coordinatorFixture, commitGeneration, dirtyGeneration int64,
) graph.Reader {
	t.Helper()
	commit, err := graphview.NewGenerationLayer(f.store.AtGeneration(commitGeneration))
	if err != nil {
		t.Fatalf("open commit generation %d: %v", commitGeneration, err)
	}
	base := graph.NewOverlaidViewWithLayer(f.store.AtGeneration(0), commit)
	if dirtyGeneration == 0 {
		return base
	}
	dirty, err := graphview.NewGenerationLayer(f.store.AtGeneration(dirtyGeneration))
	if err != nil {
		t.Fatalf("open dirty generation %d: %v", dirtyGeneration, err)
	}
	return graph.NewOverlaidViewWithLayer(base, dirty)
}

// TestCheckoutViewComposesLikeAFlatIndexOfTheWorktree is the end-to-end
// acceptance: a family with a primary and one automatic worktree, brought up
// by the coordinator and served by the materializer, answers every read the
// way a plain index of that worktree's disk does.
//
// The worktree is on its own branch AND has uncommitted edits, so both layers
// are carrying content — and the corpus underneath is at neither state.
func TestCheckoutViewComposesLikeAFlatIndexOfTheWorktree(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})

	f.commitTreeB()
	builderWriteFile(t, f.worktree, "helper.go", `package fixture

func Helper() {
	// only in the working tree
}
`)
	builderWriteFile(t, f.worktree, "untracked.go", `package fixture

func Untracked() {
}
`)
	cycle := coordinatorReconcile(t, c)
	if cycle.CommitGenerationID == 0 || cycle.DirtyGenerationID == 0 {
		t.Fatalf("the coordinator did not bring both slots up: %+v", cycle)
	}

	materializer := &graphview.Materializer{Store: f.store, Catalog: f.catalog, Leases: f.leases}
	view, err := materializer.MaterializeCheckout(context.Background(), f.checkoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	defer view.Close()

	flat := builderOpenStore(t, "flat-b")
	builderIndex(t, flat, f.worktree)
	if corpus := builderNodeIDs(f.store.AtGeneration(0)); slices.Equal(corpus, builderNodeIDs(view.Reader)) {
		t.Fatal("the served view carries the corpus's identities verbatim — neither layer changed anything")
	}
	builderAssertReadersAgree(t, view.Reader, flat)
	builderAssertMasksValidate(t, f.store, cycle.CommitGenerationID)
	builderAssertMasksValidate(t, f.store, cycle.DirtyGenerationID)

	// A branch switch in the worktree moves what the same checkout serves.
	builderGit(t, f.worktree, "checkout", "--", ".")
	if err := os.Remove(filepath.Join(f.worktree, "untracked.go")); err != nil {
		t.Fatalf("remove untracked.go: %v", err)
	}
	builderGit(t, f.worktree, "checkout", "--detach", "HEAD~1")
	switched := coordinatorReconcile(t, c)
	if switched.CommitGenerationID == cycle.CommitGenerationID {
		t.Fatalf("the branch switch left the commit slot at %d", cycle.CommitGenerationID)
	}

	view.Close()
	after, err := materializer.MaterializeCheckout(context.Background(), f.checkoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout after the switch: %v", err)
	}
	defer after.Close()

	flatA := builderOpenStore(t, "flat-a")
	builderIndex(t, flatA, f.worktree)
	builderAssertReadersAgree(t, after.Reader, flatA)
}

// --- lifecycle integration ----------------------------------------------

// TestCheckoutLifecycleRunsACoordinatorPerAutomaticCheckout is the wiring: the
// janitor's reconciliation is what decides a coordinator exists, and the
// dispositions it already reports are the whole of the decision.
//
// The worktree here is deliberately never tracked. That is what makes it
// automatic: the reconciler observes it in a family that has a primary
// dedicated graph, mints an identity for it, and the lifecycle gives that
// identity a coordinator. The route-owned dedicated primary keeps its own
// coordinator as well, so its immutable base can be composed with live layers.
func TestCheckoutLifecycleRunsACoordinatorPerAutomaticCheckout(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	main := f.gitRepo("coord-main")
	worktree := f.worktreeOf(main, "coord-wt")

	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: main, Name: "coord-main"}, TrackSourceCLI)
	if err != nil || tracked.CatalogErr != nil {
		t.Fatalf("register the primary: %v / %v", err, tracked.CatalogErr)
	}

	report, err := f.lc.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.Coordinators != 2 {
		t.Fatalf("%d coordinators after observing one automatic worktree and its dedicated primary, want 2", report.Coordinators)
	}

	checkouts, err := f.catalog.ListCheckouts(ctx, tracked.FamilyID)
	if err != nil {
		t.Fatalf("list checkouts: %v", err)
	}
	// Matched by identity rather than by path: git spells a worktree root with
	// its symlinks resolved, and the fixture's temporary directory is reached
	// through one on some platforms.
	var automatic *store_sqlite.Checkout
	for i := range checkouts {
		if checkouts[i].CheckoutID != tracked.CheckoutID {
			automatic = &checkouts[i]
		}
	}
	if automatic == nil {
		t.Fatal("the observed worktree has no catalog identity")
	}
	if automatic.AdminName != filepath.Base(worktree) {
		t.Fatalf("the second identity is %q, not the observed worktree", automatic.AdminName)
	}
	if automatic.EffectiveMode != store_sqlite.CheckoutModeAutomatic {
		t.Fatalf("the observed worktree is %q, want automatic", automatic.EffectiveMode)
	}
	if !f.lc.SignalCheckout(automatic.CheckoutID, "test") {
		t.Fatal("the automatic checkout has no coordinator listening for signals")
	}
	if !f.lc.SignalCheckout(tracked.CheckoutID, "test") {
		t.Fatal("the route-owned primary has no coordinator listening for signals")
	}

	// A second sweep is idempotent: the coordinator that is already running is
	// the one that keeps running.
	again, err := f.lc.Sweep(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again.Coordinators != 2 {
		t.Fatalf("%d coordinators after a second sweep, want the same 2", again.Coordinators)
	}

	// The worktree leaves. The removal clock has to expire before the identity
	// goes, and the coordinator goes with it.
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the worktree: %v", err)
	}
	if _, err := f.lc.Sweep(ctx); err != nil {
		t.Fatalf("sweep after the removal: %v", err)
	}
	f.clock.advance(lifecycleGrace.RemovalGrace + time.Second)
	gone, err := f.lc.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep after the grace: %v", err)
	}
	if gone.Coordinators != 1 {
		t.Fatalf("%d coordinators after automatic checkout removal, want the dedicated primary only", gone.Coordinators)
	}
	if f.lc.SignalCheckout(automatic.CheckoutID, "test") {
		t.Fatal("a coordinator is still listening for a checkout that is gone")
	}
	if !f.lc.SignalCheckout(tracked.CheckoutID, "test") {
		t.Fatal("removing the automatic checkout also stopped the dedicated primary")
	}
}

// lifecycleCoordinator reaches into the registry for one checkout's running
// coordinator, so a test can drive its cycles instead of waiting on its timer.
func lifecycleCoordinator(t *testing.T, l *CheckoutLifecycle, checkoutID string) *CheckoutCoordinator {
	t.Helper()
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	coordinator := l.coordinators[checkoutID]
	if coordinator == nil {
		t.Fatalf("checkout %s has no coordinator", checkoutID)
	}
	return coordinator
}

// TestCheckoutLifecycleCollectsAForgottenCheckoutsPayload closes the loop the
// teardown opens.
//
// Forgetting a checkout withdraws its route first of all, and the route row is
// the only thing in the catalog that names the generations built for it. The
// coordinator being dropped is therefore the last moment those ids exist
// anywhere, and the sweep that dropped it is what has to collect them — or
// every worktree the daemon ever served leaves its payload in the database
// permanently.
func TestCheckoutLifecycleCollectsAForgottenCheckoutsPayload(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	main := f.gitRepo("collect-main")
	worktree := f.worktreeOf(main, "collect-wt")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: main, Name: "collect-main"}, TrackSourceCLI)
	if err != nil || tracked.CatalogErr != nil {
		t.Fatalf("register the primary: %v / %v", err, tracked.CatalogErr)
	}

	checkouts, err := f.catalog.ListCheckouts(ctx, tracked.FamilyID)
	if err != nil {
		t.Fatalf("list checkouts: %v", err)
	}
	automatic := ""
	for _, checkout := range checkouts {
		if checkout.CheckoutID != tracked.CheckoutID {
			automatic = checkout.CheckoutID
		}
	}
	if automatic == "" {
		t.Fatal("the observed worktree has no catalog identity")
	}

	// The coordinator's loop is stopped and one cycle driven by hand, so the
	// route is settled without waiting on a timer. The lifecycle still holds
	// the coordinator, which is what the teardown below drops.
	coordinator := lifecycleCoordinator(t, f.lc, automatic)
	if err := coordinator.Close(); err != nil {
		t.Fatalf("stop the coordinator loop: %v", err)
	}
	coordinatorReconcile(t, coordinator)
	routed, found, err := f.catalog.GetCheckoutRoute(ctx, automatic)
	if err != nil || !found || !graphview.RouteReady(routed) {
		t.Fatalf("the automatic checkout is not routed: %+v (found=%v err=%v)", routed, found, err)
	}

	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove the worktree: %v", err)
	}
	// Where the platform cannot prove a deleted root is deleted, the deletion
	// alone never starts the removal clock, so nothing would ever be forgotten
	// and the payload this test is about would not be collectable. Pruning
	// supplies git's own omission, which is the removal evidence such a
	// platform has, and the collection below is unchanged.
	if !volumeEvidenceUsable(t, main) {
		runGit(t, main, "worktree", "prune")
	}
	grace, err := f.lc.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep after the removal: %v", err)
	}
	f.clock.advance(lifecycleGrace.RemovalGrace + time.Second)
	gone, err := f.lc.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep after the grace: %v", err)
	}
	primaryRoute, primaryFound, primaryErr := f.catalog.GetCheckoutRoute(ctx, tracked.CheckoutID)
	if primaryErr != nil || !primaryFound || primaryRoute.CommitGenerationID != routed.CommitGenerationID {
		t.Fatalf("the surviving primary does not route the shared commit generation: found=%v err=%v primary=%+v forgotten=%+v", primaryFound, primaryErr, primaryRoute, routed)
	}

	// Entering removal grace withdraws the overlay route immediately so reads
	// fall back to the base graph. Only the forgotten checkout's unique dirty
	// layer is collectable: its commit layer is canonical and still routed by
	// the surviving primary checkout.
	if retired := grace.Retired + gone.Retired; retired != 1 {
		t.Fatalf("the sweeps collected %d generations, want the forgotten checkout's unique dirty layer exactly once", retired)
	}
	if _, found, err := f.catalog.GetViewGeneration(ctx, routed.DirtyGenerationID); err != nil || found {
		t.Fatalf("dirty generation %d outlived the checkout it was built for (err=%v)", routed.DirtyGenerationID, err)
	}
	if _, found, err := f.catalog.GetViewGeneration(ctx, routed.CommitGenerationID); err != nil || !found {
		t.Fatalf("shared commit generation %d did not survive the primary route (err=%v)", routed.CommitGenerationID, err)
	}
}

// newSweepLifecycle builds a lifecycle over an existing store with nothing in
// memory: no coordinator registry, no owed generations, and no family to
// reconcile.
//
// It is what a process that died starts back up as. Whatever a sweep collects
// through it, it had to find in the catalog — the in-memory ledgers every other
// retirement path reads from went with the crash.
func newSweepLifecycle(t *testing.T, store *store_sqlite.Store) *CheckoutLifecycle {
	t.Helper()
	mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), nil, zap.NewNop())
	lc, err := NewCheckoutLifecycle(CheckoutLifecycleConfig{
		MultiIndexer: mi,
		Graph:        store,
		Logger:       zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("NewCheckoutLifecycle: %v", err)
	}
	t.Cleanup(func() {
		_ = lc.Close()
		_ = mi.Close(context.Background())
	})
	return lc
}

// TestSweepCollectsCrashOrphanedGenerations closes the hole the in-memory
// backlogs leave.
//
// Every other handle on a generation nobody should be reading is in memory: the
// coordinator's backlog, its reuse cache, and the lifecycle's owed set. A
// process that dies between superseding a generation and retiring it drops all
// three at once, and nothing in the catalog points at what it dropped — the
// payload would be unreachable for the life of the database. The sweep has to
// find those rows by reading the generations themselves.
//
// The routed pair is the control. It is exactly as unreferenced by any
// in-memory ledger as the orphans are, and the only thing that distinguishes it
// is the route — so a scan that collected it would be collecting the view the
// checkout is being served from.
func TestSweepCollectsCrashOrphanedGenerations(t *testing.T) {
	f := newCoordinatorFixture(t)
	ctx := context.Background()

	// One real cycle: the coordinator builds both layers and routes them.
	coordinator := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, coordinator)
	routed := f.route()
	if routed.CommitGenerationID == 0 || routed.DirtyGenerationID == 0 {
		t.Fatalf("the coordinator routed %+v, want both slots filled", routed)
	}

	// A discarded generation for the routed checkout that the route does not
	// name — what a supersede leaves behind when the retire never runs.
	superseded, err := f.catalog.CreateViewGeneration(ctx, store_sqlite.ViewGeneration{
		OwnerKind:      checkoutLayerOwnerKind,
		GraphID:        f.graphID,
		LayerID:        commitLayerID(f.checkoutID),
		CheckoutID:     f.checkoutID,
		GenerationKind: CommitLayerGenerationKind,
		TreeOID:        "tree-superseded",
		State:          store_sqlite.ViewGenerationSuperseded,
	})
	if err != nil {
		t.Fatalf("seed the superseded generation: %v", err)
	}

	// And a published commit layer for a checkout nothing routes any more —
	// what a coordinator's reuse cache was holding when it stopped.
	stranded := f.siblingCheckout("stranded")
	orphan, err := f.catalog.CreateViewGeneration(ctx, store_sqlite.ViewGeneration{
		OwnerKind:      checkoutLayerOwnerKind,
		GraphID:        f.graphID,
		LayerID:        commitLayerID(stranded),
		CheckoutID:     stranded,
		GenerationKind: CommitLayerGenerationKind,
		TreeOID:        f.treeA,
		State:          store_sqlite.ViewGenerationReady,
	})
	if err != nil {
		t.Fatalf("seed the stranded commit layer: %v", err)
	}

	report, err := newSweepLifecycle(t, f.store).Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.Retired != 2 {
		t.Fatalf("the sweep collected %d generations, want the two the crash orphaned", report.Retired)
	}
	for name, generationID := range map[string]int64{
		"superseded": superseded,
		"stranded":   orphan,
	} {
		if _, found, err := f.catalog.GetViewGeneration(ctx, generationID); err != nil || found {
			t.Fatalf("the %s generation %d survived the sweep (err=%v)", name, generationID, err)
		}
	}
	for name, generationID := range map[string]int64{
		"commit": routed.CommitGenerationID,
		"dirty":  routed.DirtyGenerationID,
	} {
		if _, found, err := f.catalog.GetViewGeneration(ctx, generationID); err != nil || !found {
			t.Fatalf("the routed %s generation %d was collected (err=%v)", name, generationID, err)
		}
	}
	if after := f.route(); after != routed {
		t.Fatalf("the route moved under the sweep: %+v, want %+v", after, routed)
	}
}
