package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

// The ref-view fixture: one repository whose main branch is the base corpus,
// plus branches nobody has ever checked out.
//
// Everything under it is real. The repository is a real git repository, the
// corpus is a real index of its main branch, the catalog rows are written
// through the catalog's own API, and the builds are the production builder.
// What the tests drive is the manager's own decisions, so nothing between the
// selector and the adopted generation is stubbed.
//
// The concurrency tests are deterministic without a clock and therefore
// without synctest: what has to interleave is a build against another
// selection, and the manager's build barrier parks the first exactly where the
// second has to overtake it. A synctest bubble would virtualise time that no
// assertion here depends on, around real git subprocesses and a real SQLite
// writer that it cannot virtualise at all.
//
// The few tests that DO depend on a window narrow the manager's own windows
// instead, for the same reason: the pass under them is real, and last_progress
// is unix seconds, so a virtual clock would have to advance a stamp the
// database writes for itself.

type refViewFixture struct {
	t testing.TB

	store   *store_sqlite.Store
	catalog *store_sqlite.Catalog

	// repo is the repository the corpus was indexed from. Its working tree
	// stays on main for the whole of every test: a ref view exists precisely
	// to serve state nobody has checked out.
	repo string

	familyID   string
	graphID    string
	checkoutID string
	// treeA is the committed tree main holds, and the tree the corpus holds.
	treeA string
}

func newRefViewFixture(t testing.TB) *refViewFixture {
	t.Helper()
	builderIsolateGit(t)

	repo := builderTempDir(t, "refview")
	builderGit(t, repo, "init", "--initial-branch=main")
	builderWriteTree(t, repo, builderTreeA())
	builderGit(t, repo, "add", "-A")
	builderGit(t, repo, "commit", "-m", "A")
	treeA := builderGit(t, repo, "rev-parse", "HEAD^{tree}")

	store := builderOpenStore(t, "refview")
	builderIndex(t, store, repo)

	f := &refViewFixture{
		t:          t,
		store:      store,
		catalog:    store.Catalog(),
		repo:       repo,
		familyID:   "family-refview",
		graphID:    GraphIDFor(builderRepoPrefix),
		checkoutID: "checkout-refview",
		treeA:      treeA,
	}
	f.writeCatalogIdentity()
	return f
}

// writeCatalogIdentity records what the reconciler would have recorded: the
// family, the checkout the corpus came from, and its dedicated graph.
func (f *refViewFixture) writeCatalogIdentity() {
	f.t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()

	err := f.catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          f.familyID,
		CommonDirIdentity: filepath.Join(f.repo, ".git"),
		State:             reconcile.FamilyStateReady,
		CreatedAt:         now,
		LastSeen:          now,
	})
	if err != nil {
		f.t.Fatalf("upsert family: %v", err)
	}

	err = f.catalog.AllocateCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:     f.checkoutID,
		Incarnation:    "incarnation-refview",
		FamilyID:       f.familyID,
		RootPath:       f.repo,
		GitDir:         filepath.Join(f.repo, ".git"),
		AdminName:      "@main",
		State:          store_sqlite.CheckoutStateReady,
		DesiredMode:    store_sqlite.CheckoutModeDedicated,
		EffectiveMode:  store_sqlite.CheckoutModeDedicated,
		HeadRef:        "refs/heads/main",
		HeadTree:       f.treeA,
		LastAccessible: now,
		LastSeen:       now,
	})
	if err != nil {
		f.t.Fatalf("allocate the checkout: %v", err)
	}

	dedicated := store_sqlite.DedicatedGraph{
		GraphID:         f.graphID,
		OwnerCheckoutID: f.checkoutID,
		RepoPrefix:      builderRepoPrefix,
		FamilyID:        f.familyID,
		IsPrimaryBase:   true,
		State:           reconcile.GraphStateReady,
	}
	if err := f.catalog.UpsertDedicatedGraph(ctx, dedicated); err != nil {
		f.t.Fatalf("bind the dedicated graph: %v", err)
	}

	pipeline := DedicatedBasePipelineFor(config.Default().Index)
	generationID, payload, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:         dedicatedBaseGenerationKind,
		GraphID:           f.graphID,
		LayerID:           f.graphID + ":base",
		CheckoutID:        f.checkoutID,
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
	builderIndex(f.t, payload, f.repo)
	if err := f.store.PublishPayloadGeneration(ctx, generationID, now+1); err != nil {
		f.t.Fatalf("publish the primary generation: %v", err)
	}
	dedicated.ActiveGenerationID = generationID
	if err := f.catalog.UpsertDedicatedGraph(ctx, dedicated); err != nil {
		f.t.Fatalf("activate the primary generation: %v", err)
	}
}

// commitTree commits a tree on a scratch branch and leaves the working tree
// back on main, so the only trace of the commit is in the object store. Its
// commit and tree ids are what a ref is then pointed at.
func (f *refViewFixture) commitTree(tree map[string]string, message string) (commit, treeOID string) {
	f.t.Helper()
	builderGit(f.t, f.repo, "switch", "--force-create", "scratch", "main")
	builderWriteTree(f.t, f.repo, tree)
	builderGit(f.t, f.repo, "add", "-A")
	builderGit(f.t, f.repo, "commit", "-m", message)
	commit = builderGit(f.t, f.repo, "rev-parse", "HEAD^{commit}")
	treeOID = builderGit(f.t, f.repo, "rev-parse", "HEAD^{tree}")
	builderGit(f.t, f.repo, "switch", "--force", "main")
	return commit, treeOID
}

// recommit builds a new commit carrying the SAME tree as its parent — an
// amend, a rebase, an empty commit. It writes a commit object directly, so
// nothing is checked out and no ref moves.
func (f *refViewFixture) recommit(parent string) string {
	f.t.Helper()
	tree := builderGit(f.t, f.repo, "rev-parse", parent+"^{tree}")
	return builderGit(f.t, f.repo, "commit-tree", tree, "-p", parent, "-m", "same tree, new commit")
}

func (f *refViewFixture) setRef(ref, oid string) {
	f.t.Helper()
	builderGit(f.t, f.repo, "update-ref", ref, oid)
}

func (f *refViewFixture) manager(t testing.TB, barrier func()) *RefViewManager {
	t.Helper()
	return f.managerTuned(t, barrier, nil)
}

// managerTuned builds a manager whose build timings the caller may narrow.
// The production windows are minutes wide, which is exactly what a test that
// drives them cannot wait for.
func (f *refViewFixture) managerTuned(t testing.TB, barrier func(), tune func(*RefViewManagerConfig)) *RefViewManager {
	t.Helper()
	cfg := RefViewManagerConfig{
		Store:        f.store,
		Builder:      builderNewBuilder(f.store),
		Config:       config.Default().Index,
		Logger:       zap.NewNop(),
		buildBarrier: barrier,
	}
	if tune != nil {
		tune(&cfg)
	}
	manager, err := NewRefViewManager(cfg)
	if err != nil {
		t.Fatalf("NewRefViewManager: %v", err)
	}
	return manager
}

// viewID is the catalog id a request for one ref lands on.
func (f *refViewFixture) viewID(ref string) string {
	req := f.request(ref)
	req.EnrichmentProfile = defaultEnrichmentProfile
	return refViewID(req)
}

// awaitBuildState waits for a view's single attempt to reach one state. A
// build detached from its request finishes on its own goroutine, so the only
// thing a test can wait on is the row it closes.
func (f *refViewFixture) awaitBuildState(refViewID string, want store_sqlite.ViewGenerationState) {
	f.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		rows := f.builds(refViewID)
		if len(rows) == 1 && rows[0].State == want {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("build rows = %+v, want one attempt in state %q", rows, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitBuildProgress waits for a view's single attempt to report progress at
// or past one stamp.
func (f *refViewFixture) awaitBuildProgress(refViewID string, want int64) {
	f.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		rows := f.builds(refViewID)
		if len(rows) == 1 && rows[0].LastProgress >= want {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("build rows = %+v, want one attempt reporting progress at or past %d", rows, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *refViewFixture) request(ref string) RefViewRequest {
	return RefViewRequest{
		GraphID:       f.graphID,
		SelectorKind:  gitstate.ViewSelectorGitRef,
		SelectorValue: ref,
		RepoDir:       f.repo,
		RepoPrefix:    builderRepoPrefix,
		WorkspaceID:   builderRepoPrefix,
		ProjectID:     builderRepoPrefix,
	}
}

func (f *refViewFixture) view(refViewID string) store_sqlite.RefView {
	f.t.Helper()
	view, found, err := f.catalog.GetRefView(context.Background(), refViewID)
	if err != nil || !found {
		f.t.Fatalf("read ref view %s: found=%v err=%v", refViewID, found, err)
	}
	return view
}

func (f *refViewFixture) builds(refViewID string) []store_sqlite.RefViewBuild {
	f.t.Helper()
	rows, err := f.catalog.ListRefViewBuilds(context.Background(), refViewID)
	if err != nil {
		f.t.Fatalf("list ref view builds: %v", err)
	}
	return rows
}

// generations enumerates every generation a ref view build produced, in any
// state.
func (f *refViewFixture) generations() []store_sqlite.ViewGeneration {
	f.t.Helper()
	rows, err := f.catalog.ListViewGenerations(context.Background(),
		store_sqlite.ViewGenerationFilter{OwnerKind: refViewOwnerKind})
	if err != nil {
		f.t.Fatalf("list view generations: %v", err)
	}
	return rows
}

func (f *refViewFixture) generation(id int64) store_sqlite.ViewGeneration {
	f.t.Helper()
	row, found, err := f.catalog.GetViewGeneration(context.Background(), id)
	if err != nil || !found {
		f.t.Fatalf("read generation %d: found=%v err=%v", id, found, err)
	}
	return row
}

// --- the ordinary path --------------------------------------------------

// TestRefViewBuildsOnceAndReusesThePayload is the base claim: selecting a
// branch nobody has checked out builds a generation for its tree, and
// selecting it again while nothing moved builds nothing.
func TestRefViewBuildsOnceAndReusesThePayload(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, treeB := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)
	f.setRef("refs/heads/canceled-alias", commitB)

	var builds atomic.Int64
	manager := f.manager(t, func() { builds.Add(1) })
	ctx := context.Background()

	first, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("first selection: %v", err)
	}
	if first.State != store_sqlite.RefViewReady || !first.Built || first.GenerationID == 0 {
		t.Fatalf("first selection = %+v, want a ready view built by this call", first)
	}
	if first.Resolved.CommitOID != commitB || first.Resolved.TreeOID != treeB {
		t.Fatalf("first selection resolved %+v, want commit %s tree %s", first.Resolved, commitB, treeB)
	}
	if row := f.generation(first.GenerationID); row.TreeOID != treeB || row.State != store_sqlite.ViewGenerationReady {
		t.Fatalf("generation = %+v, want a ready generation at tree %s", row, treeB)
	}

	view := f.view(first.RefViewID)
	if view.ActiveGenerationID != first.GenerationID || view.ActiveCommit != commitB || view.ActiveTree != treeB {
		t.Fatalf("ref view = %+v, want it serving generation %d at %s", view, first.GenerationID, commitB)
	}
	if view.State != store_sqlite.RefViewReady || !view.ExactView {
		t.Fatalf("ref view state = %q exact=%v", view.State, view.ExactView)
	}

	second, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("second selection: %v", err)
	}
	if second.Built || second.GenerationID != first.GenerationID || second.State != store_sqlite.RefViewReady {
		t.Fatalf("second selection = %+v, want the first generation served without a build", second)
	}
	if n := builds.Load(); n != 1 {
		t.Fatalf("%d build passes ran for two selections of an unmoved branch", n)
	}
}

// TestRefViewSelectionIsWhatNoticesMovement pins the cost model: a ref that
// moves while nobody is asking costs nothing, and the rebuild happens on the
// next selection rather than on the movement.
func TestRefViewSelectionIsWhatNoticesMovement(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)

	var builds atomic.Int64
	manager := f.manager(t, func() { builds.Add(1) })
	ctx := context.Background()

	first, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("first selection: %v", err)
	}

	// The branch moves to a different tree. Nothing selects it, and nothing
	// watches it, so nothing may happen.
	moved := builderTreeB()
	moved["late.go"] = "package fixture\n\nfunc Late() {\n}\n"
	commitC, treeC := f.commitTree(moved, "C")
	f.setRef("refs/heads/feature", commitC)

	if n := builds.Load(); n != 1 {
		t.Fatalf("%d build passes ran, want only the first selection's", n)
	}
	idle := f.view(first.RefViewID)
	if idle.ActiveGenerationID != first.GenerationID || idle.ActiveCommit != commitB {
		t.Fatalf("ref view moved without a selection: %+v", idle)
	}

	second, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("second selection: %v", err)
	}
	if !second.Built || second.GenerationID == first.GenerationID {
		t.Fatalf("second selection = %+v, want a rebuild off generation %d", second, first.GenerationID)
	}
	if n := builds.Load(); n != 2 {
		t.Fatalf("%d build passes ran, want one per noticed movement", n)
	}
	view := f.view(first.RefViewID)
	if view.ActiveGenerationID != second.GenerationID || view.ActiveCommit != commitC || view.ActiveTree != treeC {
		t.Fatalf("ref view = %+v, want it serving generation %d at %s", view, second.GenerationID, commitC)
	}
}

// --- coalescing ---------------------------------------------------------

// TestRefViewCoalescesConcurrentSelections is the claim the partial unique
// index exists for: two selections of one view produce one build row and one
// generation, and the one that lost is handed the winner's build token rather
// than a bare failure.
//
// The interleaving is exact rather than raced: the winner is parked in its
// build barrier — after its pass, before it may publish — and the second
// selection runs while it is parked, which is the only window in which a
// second claim is possible at all.
func TestRefViewCoalescesConcurrentSelections(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)

	// Only the first pass parks. A second one would mean coalescing failed,
	// and it has to be free to finish so the assertions below can say so
	// instead of the test deadlocking on its own barrier.
	var builds atomic.Int64
	parked := make(chan struct{})
	release := make(chan struct{})
	manager := f.manager(t, func() {
		if builds.Add(1) == 1 {
			close(parked)
			<-release
		}
	})
	ctx := context.Background()

	var (
		wg     sync.WaitGroup
		winner RefViewResult
		winErr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		winner, winErr = manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	}()

	<-parked
	loser, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("second selection: %v", err)
	}
	close(release)
	wg.Wait()

	if winErr != nil {
		t.Fatalf("first selection: %v", winErr)
	}
	if winner.State != store_sqlite.RefViewReady || !winner.Built {
		t.Fatalf("first selection = %+v, want a ready view it built itself", winner)
	}
	if loser.State != store_sqlite.RefViewBuilding || loser.Built {
		t.Fatalf("second selection = %+v, want a building answer with no build of its own", loser)
	}
	if n := builds.Load(); n != 1 {
		t.Fatalf("%d build passes ran for two concurrent selections of one view", n)
	}

	rows := f.builds(winner.RefViewID)
	if len(rows) != 1 {
		t.Fatalf("%d build rows, want the two selections to have shared one: %+v", len(rows), rows)
	}
	if loser.BuildToken != rows[0].BuildToken {
		t.Fatalf("second selection got token %q, want the in-flight build's %q", loser.BuildToken, rows[0].BuildToken)
	}
	if rows[0].State != store_sqlite.ViewGenerationReady || rows[0].GenerationID != winner.GenerationID {
		t.Fatalf("build row = %+v, want it finished on generation %d", rows[0], winner.GenerationID)
	}
	if generations := f.generations(); len(generations) != 1 {
		t.Fatalf("%d ref-view generations, want one: %+v", len(generations), generations)
	}
}

// TestRefViewBuildOutlivesACanceledRequest pins what a build must never be
// owned by: the request that started it. A tool deadline is tens of seconds
// and a build over a large tree is not, so a build bound to the request ctx is
// killed exactly when it was most expensive — and the attempt it was holding
// dies with it, taking every selection that coalesced onto it.
//
// The request keeps only the wait. It answers with the build to poll, and the
// pass runs to publication on the daemon's own context for whoever asks next.
func TestRefViewBuildOutlivesACanceledRequest(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, treeB := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)
	f.setRef("refs/heads/canceled-alias", commitB)
	viewID := f.viewID("refs/heads/feature")

	parked, release := make(chan struct{}), make(chan struct{})
	var builds atomic.Int64
	manager := f.manager(t, func() {
		if builds.Add(1) == 1 {
			close(parked)
			<-release
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var (
		wg        sync.WaitGroup
		abandoned RefViewResult
		abandErr  error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		abandoned, abandErr = manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	}()

	<-parked
	cancel()
	wg.Wait()

	if abandErr != nil {
		t.Fatalf("a selection whose client gave up mid-build: %v", abandErr)
	}
	if abandoned.State != store_sqlite.RefViewBuilding || abandoned.BuildToken == "" {
		t.Fatalf("selection = %+v, want a building answer naming the build to poll", abandoned)
	}
	rows := f.builds(viewID)
	if len(rows) != 1 || rows[0].State != store_sqlite.ViewGenerationBuilding {
		t.Fatalf("build rows = %+v, want the claim still held by the detached pass", rows)
	}
	if rows[0].BuildToken != abandoned.BuildToken {
		t.Fatalf("selection named token %q, want the running build's %q",
			abandoned.BuildToken, rows[0].BuildToken)
	}

	// The cancellation took the client, not the pass: releasing it publishes.
	close(release)
	f.awaitBuildState(viewID, store_sqlite.ViewGenerationReady)

	next, err := manager.EnsureRefView(context.Background(), f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("selection after the canceled one: %v", err)
	}
	if next.State != store_sqlite.RefViewReady || next.Built {
		t.Fatalf("selection = %+v, want the detached build's view served without a second pass", next)
	}
	if next.Resolved.TreeOID != treeB {
		t.Fatalf("selection resolved %+v, want tree %s", next.Resolved, treeB)
	}
	if n := builds.Load(); n != 1 {
		t.Fatalf("%d build passes ran, want the one the canceled request started", n)
	}
	if view := f.view(viewID); view.ActiveGenerationID != next.GenerationID {
		t.Fatalf("ref view = %+v, want it serving generation %d", view, next.GenerationID)
	}
	alias, err := manager.EnsureRefView(context.Background(), f.request("refs/heads/canceled-alias"))
	if err != nil {
		t.Fatalf("alias selection after canceled request: %v", err)
	}
	if alias.Built || alias.GenerationID != next.GenerationID {
		t.Fatalf("alias = %+v, want cached winner %d without another build", alias, next.GenerationID)
	}
	if generations := f.generations(); len(generations) != 1 {
		t.Fatalf("%d generations, want the detached build's one: %+v", len(generations), generations)
	}
}

// TestRefViewSelectionAnswersBuildingPastItsGrace pins the request-side bound.
// A selection waits out a short grace for the build it claimed — long enough
// that a trivial view still answers synchronously — and then hands back the
// token instead of holding the request open for the whole pass.
func TestRefViewSelectionAnswersBuildingPastItsGrace(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)
	viewID := f.viewID("refs/heads/feature")

	parked, release := make(chan struct{}), make(chan struct{})
	var builds atomic.Int64
	manager := f.managerTuned(t, func() {
		if builds.Add(1) == 1 {
			close(parked)
			<-release
		}
	}, func(cfg *RefViewManagerConfig) { cfg.buildGrace = 20 * time.Millisecond })

	result, err := manager.EnsureRefView(context.Background(), f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("selection: %v", err)
	}
	if result.State != store_sqlite.RefViewBuilding || result.Built {
		t.Fatalf("selection = %+v, want a building answer from a pass it did not wait out", result)
	}
	if result.BuildToken == "" {
		t.Fatal("a building answer named no build to poll")
	}

	<-parked
	rows := f.builds(viewID)
	if len(rows) != 1 || rows[0].BuildToken != result.BuildToken {
		t.Fatalf("build rows = %+v, want the one attempt the answer named (%q)", rows, result.BuildToken)
	}

	close(release)
	f.awaitBuildState(viewID, store_sqlite.ViewGenerationReady)

	next, err := manager.EnsureRefView(context.Background(), f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("selection after the grace expired: %v", err)
	}
	if next.State != store_sqlite.RefViewReady || next.Built || next.GenerationID == 0 {
		t.Fatalf("selection = %+v, want the finished build's view served as it stands", next)
	}
	if n := builds.Load(); n != 1 {
		t.Fatalf("%d build passes ran, want the one the first selection started", n)
	}
}

// TestRefViewHeartbeatKeepsASlowBuildClaimed pins the difference between a
// slow build and a dead one. The liveness cutoff reads last_progress, and a
// claim stamped only when it was made looks abandoned to every selection that
// arrives a window later — so a build that merely takes a while is reclaimed
// while it is still running, and the duplicate races the original to publish.
//
// The windows are narrowed rather than virtualised: last_progress is unix
// seconds, and the pass under it is real git plumbing against a real SQLite
// writer, neither of which a synctest bubble can advance a clock through.
func TestRefViewHeartbeatKeepsASlowBuildClaimed(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)
	viewID := f.viewID("refs/heads/feature")

	parked, release := make(chan struct{}), make(chan struct{})
	var builds atomic.Int64
	manager := f.managerTuned(t, func() {
		if builds.Add(1) == 1 {
			close(parked)
			<-release
		}
	}, func(cfg *RefViewManagerConfig) {
		cfg.buildLiveness = 2 * time.Second
		cfg.buildHeartbeat = 100 * time.Millisecond
		cfg.buildGrace = time.Minute
	})

	ctx := context.Background()
	var (
		wg     sync.WaitGroup
		winner RefViewResult
		winErr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		winner, winErr = manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	}()
	<-parked

	claimed := f.builds(viewID)
	if len(claimed) != 1 {
		t.Fatalf("%d build rows while one pass is parked: %+v", len(claimed), claimed)
	}
	// Wait until the parked claim reports progress from well past the window
	// it was claimed in. Nothing else stamps it, so this is where a build that
	// only ever stamps its claim time stops.
	f.awaitBuildProgress(viewID, claimed[0].LastProgress+3)

	loser, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("selection over a live but slow build: %v", err)
	}
	if loser.State != store_sqlite.RefViewBuilding || loser.BuildToken != claimed[0].BuildToken {
		t.Fatalf("selection = %+v, want it coalesced onto the running build %q",
			loser, claimed[0].BuildToken)
	}
	if rows := f.builds(viewID); len(rows) != 1 {
		t.Fatalf("%d build rows, want the one claim nobody was allowed to reclaim: %+v", len(rows), rows)
	}

	close(release)
	wg.Wait()
	if winErr != nil {
		t.Fatalf("the parked selection: %v", winErr)
	}
	if winner.State != store_sqlite.RefViewReady || !winner.Built {
		t.Fatalf("the parked selection = %+v, want it to have published its own pass", winner)
	}
	if n := builds.Load(); n != 1 {
		t.Fatalf("%d build passes ran, want the one that was never reclaimed", n)
	}
	if generations := f.generations(); len(generations) != 1 {
		t.Fatalf("%d generations, want the one build's: %+v", len(generations), generations)
	}
}

// TestRefViewReclaimedBuildDoesNotPublish pins what losing a claim costs the
// build that lost it. A pass whose slot was taken over is no longer the one
// the view is waiting on, and adopting behind its successor would publish a
// payload nobody asked for over one somebody did — so the finished generation
// is superseded and the answer is a retry.
func TestRefViewReclaimedBuildDoesNotPublish(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)
	viewID := f.viewID("refs/heads/feature")

	parked, release := make(chan struct{}), make(chan struct{})
	var builds atomic.Int64
	manager := f.managerTuned(t, func() {
		if builds.Add(1) == 1 {
			close(parked)
			<-release
		}
	}, func(cfg *RefViewManagerConfig) {
		// The reclaim below is the test's decision, so nothing may stamp the
		// row back to life underneath it.
		cfg.buildHeartbeat = time.Hour
		cfg.buildGrace = time.Minute
	})

	var (
		wg       sync.WaitGroup
		outdated RefViewResult
		outErr   error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		outdated, outErr = manager.EnsureRefView(context.Background(), f.request("refs/heads/feature"))
	}()
	<-parked

	// What a claimant that decided this pass was dead does to it: fail the row
	// and take the slot. The cutoff is in the future so the reclaim is the
	// test's rather than the wall clock's.
	claimed := f.builds(viewID)
	if len(claimed) != 1 {
		t.Fatalf("%d build rows while one pass is parked: %+v", len(claimed), claimed)
	}
	successor := claimed[0]
	successor.BuildID, successor.BuildToken = "build-successor", "token-successor"
	successor.CreatedAt = time.Now().Unix()
	successor.LastProgress = successor.CreatedAt
	_, err := f.catalog.ClaimRefViewBuild(context.Background(), successor, successor.CreatedAt+3600)
	if err != nil {
		t.Fatalf("reclaim the parked pass's slot: %v", err)
	}

	close(release)
	wg.Wait()
	if outErr != nil {
		t.Fatalf("the reclaimed selection: %v", outErr)
	}
	if outdated.State != store_sqlite.RefViewBuilding || !outdated.Built {
		t.Fatalf("the reclaimed selection = %+v, want a pass that ran and could not publish", outdated)
	}
	if outdated.GenerationID != 0 {
		t.Fatalf("the reclaimed selection named generation %d, want none", outdated.GenerationID)
	}

	if view := f.view(viewID); view.ActiveGenerationID != 0 || view.ActiveCommit != "" {
		t.Fatalf("a reclaimed pass published its generation: %+v", view)
	}
	generations := f.generations()
	if len(generations) != 1 || generations[0].State != store_sqlite.ViewGenerationReady {
		t.Fatalf("generations = %+v, want the reclaimed pass's shared candidate kept ready", generations)
	}
	byID := map[string]store_sqlite.RefViewBuild{}
	for _, row := range f.builds(viewID) {
		byID[row.BuildID] = row
	}
	if len(byID) != 2 {
		t.Fatalf("%d build rows, want the reclaimed pass and its successor: %+v", len(byID), byID)
	}
	if row := byID[claimed[0].BuildID]; row.State != store_sqlite.ViewGenerationFailed || row.GenerationID != 0 {
		t.Fatalf("the reclaimed pass = %+v, want it left failed and publishing nothing", row)
	}
	if row := byID["build-successor"]; row.State != store_sqlite.ViewGenerationBuilding {
		t.Fatalf("the successor = %+v, want its claim untouched", row)
	}
}

// TestRefViewReclaimsAnAbandonedClaim is the same wedge from the other side: a
// daemon killed mid-build leaves a claim no completion will ever close, and no
// janitor touches the build rows. The liveness window is what breaks it — a
// claim that stopped reporting progress is taken over rather than waited on.
func TestRefViewReclaimsAnAbandonedClaim(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, treeB := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)
	f.setRef("refs/heads/original-tree", commitB)

	manager := f.manager(t, nil)
	ctx := context.Background()
	req := f.request("refs/heads/feature")
	req.EnrichmentProfile = defaultEnrichmentProfile
	viewID := refViewID(req)

	// The rows a worker that died holding the claim leaves behind: the view it
	// created, and its own attempt, with no progress since.
	view, err := f.catalog.GetOrCreateRefView(ctx, store_sqlite.RefView{
		RefViewID:         viewID,
		GraphID:           req.GraphID,
		SelectorKind:      string(req.SelectorKind),
		SelectorValue:     req.SelectorValue,
		EnrichmentProfile: req.EnrichmentProfile,
		State:             store_sqlite.RefViewPending,
		ExactView:         true,
	})
	if err != nil {
		t.Fatalf("seed the ref view: %v", err)
	}
	base, err := manager.base(ctx, req.GraphID)
	if err != nil {
		t.Fatalf("read the base: %v", err)
	}
	stale := time.Now().Add(-2 * refViewBuildLiveness).Unix()
	err = f.catalog.UpsertRefViewBuild(ctx, store_sqlite.RefViewBuild{
		BuildID:            "build-abandoned",
		RefViewID:          viewID,
		DesiredRef:         req.SelectorValue,
		DesiredCommit:      commitB,
		DesiredTree:        treeB,
		BaseGenerationID:   base.generationID,
		EnrichmentProfile:  req.EnrichmentProfile,
		BuildFingerprint:   refViewBuildFingerprint(manager.identity(viewID, base, treeB), req.EnrichmentProfile),
		CapturedRouteEpoch: view.RouteEpoch,
		State:              store_sqlite.ViewGenerationBuilding,
		BuildToken:         "token-abandoned",
		CreatedAt:          stale,
		LastProgress:       stale,
	})
	if err != nil {
		t.Fatalf("seed the abandoned attempt: %v", err)
	}

	result, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("selection over an abandoned claim: %v", err)
	}
	if result.State != store_sqlite.RefViewReady || !result.Built {
		t.Fatalf("selection = %+v, want it to have built the view itself", result)
	}
	if result.BuildToken == "token-abandoned" {
		t.Fatal("the selection was handed the abandoned claim's token")
	}

	rows := f.builds(viewID)
	if len(rows) != 2 {
		t.Fatalf("%d build rows, want the abandoned attempt and its successor: %+v", len(rows), rows)
	}
	for _, row := range rows {
		switch row.BuildID {
		case "build-abandoned":
			if row.State != store_sqlite.ViewGenerationFailed || row.Error == "" {
				t.Errorf("the abandoned attempt = %+v, want it failed with a recorded cause", row)
			}
		default:
			if row.State != store_sqlite.ViewGenerationReady || row.GenerationID != result.GenerationID {
				t.Errorf("the successor = %+v, want it finished on generation %d", row, result.GenerationID)
			}
		}
	}
}

// --- publish-time revalidation ------------------------------------------

// TestRefViewSupersedesABuildWhoseTreeMoved pins the half of the revalidation
// that refuses: a branch that moved to a different tree while the build ran
// describes a state the view has left, so the finished generation is
// superseded and the view's active pointer does not move.
func TestRefViewSupersedesABuildWhoseTreeMoved(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	moved := builderTreeB()
	moved["late.go"] = "package fixture\n\nfunc Late() {\n}\n"
	commitC, treeC := f.commitTree(moved, "C")
	f.setRef("refs/heads/feature", commitB)
	f.setRef("refs/heads/original-tree", commitB)

	var builds atomic.Int64
	manager := f.manager(t, func() {
		if builds.Add(1) == 1 {
			f.setRef("refs/heads/feature", commitC)
		}
	})
	ctx := context.Background()

	result, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("selection: %v", err)
	}
	if result.State != store_sqlite.RefViewBuilding || !result.Built {
		t.Fatalf("selection = %+v, want a build that ran and could not be adopted", result)
	}
	if result.GenerationID != 0 {
		t.Fatalf("selection named generation %d, want none — nothing was adopted", result.GenerationID)
	}
	if result.Resolved.TreeOID != treeC {
		t.Fatalf("selection resolved %+v, want the tree the branch moved to (%s)", result.Resolved, treeC)
	}

	view := f.view(result.RefViewID)
	if view.ActiveGenerationID != 0 || view.ActiveCommit != "" {
		t.Fatalf("ref view flipped to a superseded build: %+v", view)
	}
	if generations := f.generations(); len(generations) != 0 {
		t.Fatalf("generations = %+v, want the moved build's unclaimed candidate retired", generations)
	}
	rows := f.builds(view.RefViewID)
	if len(rows) != 1 || rows[0].State != store_sqlite.ViewGenerationSuperseded || rows[0].GenerationID != 0 {
		t.Fatalf("build rows = %+v, want the attempt recorded as superseded", rows)
	}
	rebuilt, err := manager.EnsureRefView(ctx, f.request("refs/heads/original-tree"))
	if err != nil {
		t.Fatalf("rebuild the moved build's original tree: %v", err)
	}
	if !rebuilt.Built || rebuilt.GenerationID == 0 || rebuilt.State != store_sqlite.RefViewReady {
		t.Fatalf("rebuild = %+v, want a fresh ready generation", rebuilt)
	}
	if builds.Load() != 2 {
		t.Fatalf("%d builds ran, want the moved pass and the later requested tree", builds.Load())
	}
}

func TestRefViewRetiresCandidateWhenPublishResolutionFails(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	const ref = "refs/heads/feature"
	f.setRef(ref, commitB)

	manager := f.manager(t, func() {
		builderGit(t, f.repo, "update-ref", "-d", ref)
	})
	result, err := manager.EnsureRefView(context.Background(), f.request(ref))
	if err == nil {
		t.Fatalf("selection = %+v, want publish-time resolution failure", result)
	}
	if result.State != store_sqlite.RefViewFailed || !result.Built {
		t.Fatalf("selection = %+v, want a failed result after a completed build", result)
	}
	if generations := f.generations(); len(generations) != 0 {
		t.Fatalf("generations = %+v, want the unclaimed candidate retired", generations)
	}
	view := f.view(result.RefViewID)
	if view.ActiveGenerationID != 0 {
		t.Fatalf("failed publish changed the active generation: %+v", view)
	}
	rows := f.builds(result.RefViewID)
	if len(rows) != 1 || rows[0].State != store_sqlite.ViewGenerationFailed || rows[0].GenerationID != 0 {
		t.Fatalf("build rows = %+v, want one failed attempt without a generation", rows)
	}
}

func TestRefViewRetiresCandidateWhenReadyClaimDoesNotComplete(t *testing.T) {
	claimErr := errors.New("forced second ready claim failure")
	for _, tc := range []struct {
		name   string
		second func() (store_sqlite.ReadyGenerationClaim, bool, error)
		want   error
	}{
		{
			name: "error",
			second: func() (store_sqlite.ReadyGenerationClaim, bool, error) {
				return store_sqlite.ReadyGenerationClaim{}, false, claimErr
			},
			want: claimErr,
		},
		{
			name: "not_found",
			second: func() (store_sqlite.ReadyGenerationClaim, bool, error) {
				return store_sqlite.ReadyGenerationClaim{}, false, nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRefViewFixture(t)
			commitB, _ := f.commitTree(builderTreeB(), "B")
			const ref = "refs/heads/feature"
			f.setRef(ref, commitB)

			manager := f.manager(t, nil)
			realClaim := manager.claimReadyGeneration
			var calls atomic.Int64
			manager.claimReadyGeneration = func(
				ctx context.Context,
				req store_sqlite.ClaimReadyGenerationRequest,
			) (store_sqlite.ReadyGenerationClaim, bool, error) {
				if calls.Add(1) == 2 {
					return tc.second()
				}
				return realClaim(ctx, req)
			}

			result, err := manager.EnsureRefView(context.Background(), f.request(ref))
			if err == nil {
				t.Fatalf("selection = %+v, want the injected second-claim failure", result)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("selection error = %v, want %v", err, tc.want)
			}
			if calls.Load() != 2 {
				t.Fatalf("ready claims = %d, want the miss and post-build claim", calls.Load())
			}
			if generations := f.generations(); len(generations) != 0 {
				t.Fatalf("generations = %+v, want the unclaimed candidate retired", generations)
			}
			view := f.view(f.viewID(ref))
			if view.ActiveGenerationID != 0 {
				t.Fatalf("failed claim changed the active generation: %+v", view)
			}
			rows := f.builds(view.RefViewID)
			if len(rows) != 1 || rows[0].State != store_sqlite.ViewGenerationFailed || rows[0].GenerationID != 0 {
				t.Fatalf("build rows = %+v, want one failed attempt without a generation", rows)
			}
		})
	}
}

func TestRefViewRetiresCandidateThatLosesCanonicalClaim(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, treeB := f.commitTree(builderTreeB(), "B")
	const ref = "refs/heads/feature"
	f.setRef(ref, commitB)

	ctx := context.Background()
	manager := f.manager(t, nil)
	base, err := manager.base(ctx, f.graphID)
	if err != nil {
		t.Fatalf("resolve primary base: %v", err)
	}
	identity := manager.identity("ref-view-cache-competitor", base, treeB)
	var (
		winnerID  int64
		winnerErr error
	)
	manager.cacheMissBarrier = func() {
		winnerID, _, winnerErr = manager.builder.BuildCommitLayer(ctx, CommitLayerRequest{
			Identity: identity, Base: f.store.AtGeneration(base.generationID), RepoDir: f.repo,
			BaseTreeOID: base.treeOID, TargetTreeOID: treeB, RootPath: f.repo,
			RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
		})
	}

	result, err := manager.EnsureRefView(ctx, f.request(ref))
	if winnerErr != nil {
		t.Fatalf("build canonical competitor: %v", winnerErr)
	}
	if err != nil {
		t.Fatalf("selection: %v", err)
	}
	if !result.Built || result.State != store_sqlite.RefViewReady || result.GenerationID != winnerID {
		t.Fatalf("selection = %+v, want canonical competitor %d", result, winnerID)
	}
	generations := f.generations()
	if len(generations) != 1 || generations[0].GenerationID != winnerID || generations[0].State != store_sqlite.ViewGenerationReady {
		t.Fatalf("generations = %+v, want only canonical winner %d", generations, winnerID)
	}
	if view := f.view(result.RefViewID); view.ActiveGenerationID != winnerID {
		t.Fatalf("active view = %+v, want canonical winner %d", view, winnerID)
	}
}

func TestRefViewRepeatedMovesRetainNoCandidates(t *testing.T) {
	f := newRefViewFixture(t)
	const attempts = 100
	commits := f.movingRefHistory(attempts + 1)
	const ref = "refs/heads/feature"
	f.setRef(ref, commits[0])

	next := 1
	manager := f.manager(t, func() {
		f.setRef(ref, commits[next])
		next++
	})
	ctx := context.Background()
	for attempt := 0; attempt < attempts; attempt++ {
		result, err := manager.EnsureRefView(ctx, f.request(ref))
		if err != nil {
			t.Fatalf("retry %d: %v", attempt, err)
		}
		if result.State != store_sqlite.RefViewBuilding || !result.Built || result.GenerationID != 0 {
			t.Fatalf("retry %d = %+v, want a finished superseded build", attempt, result)
		}
	}
	if generations := f.generations(); len(generations) != 0 {
		t.Fatalf("generations after %d retries = %+v, want no unclaimed payload", attempts, generations)
	}
	rows := f.builds(f.viewID(ref))
	if len(rows) != attempts {
		t.Fatalf("build rows = %d, want %d completed retry records", len(rows), attempts)
	}
	for _, row := range rows {
		if row.State != store_sqlite.ViewGenerationSuperseded || row.GenerationID != 0 {
			t.Fatalf("retry build retained a generation: %+v", row)
		}
	}
}

// TestRefViewAdoptsANewCommitOnTheSameTree pins the half that adopts: a branch
// that moved to a different COMMIT carrying the same tree describes exactly
// the payload that was just built, so the generation is adopted and the new
// commit is stamped beside it.
//
// The build counter is the load-bearing assertion. A payload is a function of
// the tree, so noticing the commit must not cost a second pass — and the
// counter is what says it did not.
func TestRefViewAdoptsANewCommitOnTheSameTree(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, treeB := f.commitTree(builderTreeB(), "B")
	sameTree := f.recommit(commitB)
	f.setRef("refs/heads/feature", commitB)

	var builds atomic.Int64
	manager := f.manager(t, func() {
		if builds.Add(1) == 1 {
			f.setRef("refs/heads/feature", sameTree)
		}
	})
	ctx := context.Background()

	result, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("selection: %v", err)
	}
	if result.State != store_sqlite.RefViewReady || result.GenerationID == 0 {
		t.Fatalf("selection = %+v, want the built generation adopted", result)
	}
	if n := builds.Load(); n != 1 {
		t.Fatalf("%d build passes ran, want one — the tree never changed", n)
	}
	if result.Resolved.CommitOID != sameTree || result.Resolved.TreeOID != treeB {
		t.Fatalf("selection resolved %+v, want commit %s on tree %s", result.Resolved, sameTree, treeB)
	}

	view := f.view(result.RefViewID)
	if view.ActiveGenerationID != result.GenerationID {
		t.Fatalf("ref view = %+v, want it serving generation %d", view, result.GenerationID)
	}
	if view.ActiveCommit != sameTree || view.ActiveTree != treeB {
		t.Fatalf("ref view = %+v, want the commit the branch moved to (%s) on tree %s", view, sameTree, treeB)
	}
	if row := f.generation(result.GenerationID); row.State != store_sqlite.ViewGenerationReady {
		t.Fatalf("generation = %+v, want it ready", row)
	}

	// The next selection has nothing left to do: the payload matches the tree
	// and the metadata already names the commit.
	next, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("follow-up selection: %v", err)
	}
	if next.Built || next.GenerationID != result.GenerationID {
		t.Fatalf("follow-up selection = %+v, want the adopted generation served as it stands", next)
	}
}

// --- resolution failures ------------------------------------------------

// TestRefViewRecordsAnUnresolvableSelector pins the failure path: a selector
// that names nothing local fails the selection with the typed availability
// error and leaves the view's active pointer alone.
func TestRefViewRecordsAnUnresolvableSelector(t *testing.T) {
	f := newRefViewFixture(t)
	manager := f.manager(t, nil)
	ctx := context.Background()

	result, err := manager.EnsureRefView(ctx, f.request("refs/heads/never-created"))
	if err == nil {
		t.Fatal("selecting an absent branch succeeded")
	}
	if !errors.Is(err, gitstate.ErrRefNotAvailableLocally) {
		t.Fatalf("selection error = %v, want a local-availability failure", err)
	}
	if result.State != store_sqlite.RefViewFailed {
		t.Fatalf("selection = %+v, want a failed view", result)
	}

	view := f.view(result.RefViewID)
	if view.State != store_sqlite.RefViewFailed || view.LastError == "" {
		t.Fatalf("ref view = %+v, want the failure recorded", view)
	}
	if view.ActiveGenerationID != 0 {
		t.Fatalf("a failed selection flipped the active pointer: %+v", view)
	}
	if generations := f.generations(); len(generations) != 0 {
		t.Fatalf("a failed selection built %d generations: %+v", len(generations), generations)
	}
}

// TestRefViewRebuildsActiveGenerationAfterSourceWithdrawal pins the recovery
// contract. Losing source.snapshot leaves the published structural graph and
// active pointer available as a labeled fallback, but it makes that generation
// non-current so the next selection can replace it with a source-complete one.
func TestRefViewRebuildsActiveGenerationAfterSourceWithdrawal(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)
	f.setRef("refs/heads/source-incomplete-alias", commitB)

	var builds atomic.Int64
	manager := f.manager(t, func() { builds.Add(1) })
	ctx := context.Background()

	first, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("build the first view: %v", err)
	}
	if !first.Built || first.State != store_sqlite.RefViewReady || first.GenerationID == 0 {
		t.Fatalf("first selection = %+v, want a newly built ready generation", first)
	}
	if err := f.catalog.WithdrawProducer(ctx, first.GenerationID, commitLayerSourceSnapshotCapability, "test: source object pruned"); err != nil {
		t.Fatalf("withdraw source.snapshot: %v", err)
	}

	viewBeforeRecovery := f.view(first.RefViewID)
	if viewBeforeRecovery.ActiveGenerationID != first.GenerationID {
		t.Fatalf("active view before recovery = %+v, want structural fallback generation %d", viewBeforeRecovery, first.GenerationID)
	}
	current, err := manager.activeIsCurrent(ctx, viewBeforeRecovery, viewBeforeRecovery.ActiveBuildFingerprint)
	if err != nil {
		t.Fatalf("check withdrawn active generation: %v", err)
	}
	if current {
		t.Fatal("source-incomplete active generation still reported current")
	}

	recovered, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("recover the active view after source withdrawal: %v", err)
	}
	if !recovered.Built || recovered.State != store_sqlite.RefViewReady || recovered.GenerationID == 0 || recovered.GenerationID == first.GenerationID {
		t.Fatalf("recovery selection = %+v, want a newly built source-complete generation after %d", recovered, first.GenerationID)
	}
	if view := f.view(first.RefViewID); view.ActiveGenerationID != recovered.GenerationID {
		t.Fatalf("active view after recovery = %+v, want generation %d", view, recovered.GenerationID)
	}
	if n := builds.Load(); n != 2 {
		t.Fatalf("%d build passes ran, want the initial build and one recovery", n)
	}

	reused, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("reuse the recovered active view: %v", err)
	}
	if reused.Built || reused.State != store_sqlite.RefViewReady || reused.GenerationID != recovered.GenerationID {
		t.Fatalf("selection after recovery = %+v, want generation %d without another build", reused, recovered.GenerationID)
	}
	if n := builds.Load(); n != 2 {
		t.Fatalf("%d build passes ran after recovery reuse, want two", n)
	}
}

func (f *refViewFixture) movingRefHistory(count int) []string {
	f.t.Helper()
	commits := make([]string, 0, count)
	for i := 0; i < count; i++ {
		tree := builderTreeB()
		tree["retry.go"] = "package fixture\n\nconst Retry = " + strconv.Itoa(i) + "\n"
		commit, _ := f.commitTree(tree, "retry-"+strconv.Itoa(i))
		commits = append(commits, commit)
	}
	return commits
}

// BenchmarkRefViewCandidateRetirementOnMovedRef measures the expensive retry
// boundary itself. retained/op makes the resource behavior visible beside
// latency and allocations: a moved selector must not leave its finished but
// unclaimed candidate behind.
func BenchmarkRefViewCandidateRetirementOnMovedRef(b *testing.B) {
	for _, attempts := range []int{1, 10, 100} {
		b.Run(strconv.Itoa(attempts), func(b *testing.B) {
			var retained int
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				f := newRefViewFixture(b)
				commits := f.movingRefHistory(attempts + 1)
				f.setRef("refs/heads/feature", commits[0])
				next := 1
				manager := f.manager(b, func() {
					f.setRef("refs/heads/feature", commits[next])
					next++
				})
				ctx := context.Background()
				b.StartTimer()
				for attempt := 0; attempt < attempts; attempt++ {
					result, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
					if err != nil {
						b.Fatalf("retry %d: %v", attempt, err)
					}
					if result.State != store_sqlite.RefViewBuilding || !result.Built {
						b.Fatalf("retry %d = %+v, want a finished superseded build", attempt, result)
					}
				}
				b.StopTimer()
				retained += len(f.generations())
			}
			b.ReportMetric(float64(retained)/float64(b.N*attempts), "retained/op")
		})
	}
}

func BenchmarkRefViewCurrentReadyGeneration(b *testing.B) {
	for _, tc := range []struct {
		name     string
		withdraw bool
	}{
		{name: "source-complete"},
		{name: "source-unavailable", withdraw: true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			f := newRefViewFixture(b)
			commitB, _ := f.commitTree(builderTreeB(), "B")
			f.setRef("refs/heads/feature", commitB)

			var builds atomic.Int64
			manager := f.manager(b, func() { builds.Add(1) })
			ctx := context.Background()
			first, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
			if err != nil {
				b.Fatalf("build the benchmark view: %v", err)
			}
			if tc.withdraw {
				if err := f.catalog.WithdrawProducer(ctx, first.GenerationID, commitLayerSourceSnapshotCapability, "benchmark: source object pruned"); err != nil {
					b.Fatalf("withdraw source.snapshot: %v", err)
				}
			}
			view := f.view(first.RefViewID)

			b.Run("activeIsCurrent", func(b *testing.B) {
				before := builds.Load()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					current, err := manager.activeIsCurrent(ctx, view, view.ActiveBuildFingerprint)
					if err != nil {
						b.Fatalf("activeIsCurrent: %v", err)
					}
					if current == tc.withdraw {
						b.Fatalf("active current = %t, want %t", current, !tc.withdraw)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(builds.Load()-before)/float64(b.N), "builds/op")
			})

			b.Run("EnsureRefView", func(b *testing.B) {
				before := builds.Load()
				previousGeneration := first.GenerationID
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					result, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
					if err != nil {
						b.Fatalf("EnsureRefView: %v", err)
					}
					if tc.withdraw {
						if !result.Built || result.GenerationID == 0 || result.GenerationID == previousGeneration {
							b.Fatalf("recovery selection = %+v, want a new generation after %d", result, previousGeneration)
						}
						previousGeneration = result.GenerationID
						if err := f.catalog.WithdrawProducer(ctx, result.GenerationID, commitLayerSourceSnapshotCapability, "benchmark: repeat source withdrawal"); err != nil {
							b.Fatalf("withdraw recovered source.snapshot: %v", err)
						}
					} else if result.Built || result.GenerationID != first.GenerationID {
						b.Fatalf("ready selection = %+v, want generation %d without a build", result, first.GenerationID)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(builds.Load()-before)/float64(b.N), "builds/op")
			})
		})
	}
}
