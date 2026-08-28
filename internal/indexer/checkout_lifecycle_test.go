package indexer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/search"
)

// lifecycleGrace is the pair of windows the lifecycle tests run against.
// They are wall-clock durations only in name: the fixture drives a manual
// clock, so nothing here ever waits.
var lifecycleGrace = reconcile.Config{
	AvailabilityGrace: 30 * time.Second,
	RemovalGrace:      30 * time.Second,
}

// manualClock is the lifecycle's and the reconciler's only source of time.
// Every deadline the catalog stores is measured from it, so a test can put a
// grace window in the past without sleeping through it.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func newManualClock() *manualClock {
	return &manualClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// fakeWatcher records the attach / detach calls the lifecycle makes, which
// is what "the watcher followed the tracked set" means from outside.
type fakeWatcher struct {
	mu       sync.Mutex
	attached map[string]bool
	calls    []string
}

func newFakeWatcher() *fakeWatcher {
	return &fakeWatcher{attached: map[string]bool{}}
}

func (w *fakeWatcher) AddRepo(prefix string, _ config.WatchConfig) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.attached[prefix] = true
	w.calls = append(w.calls, "add:"+prefix)
	return nil
}

func (w *fakeWatcher) RemoveRepo(prefix string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.attached, prefix)
	w.calls = append(w.calls, "remove:"+prefix)
	return nil
}

func (w *fakeWatcher) isAttached(prefix string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.attached[prefix]
}

// countingNotifier stands in for the MCP server's session fan-out.
type countingNotifier struct {
	mu          sync.Mutex
	invalidated int
	analysed    int
}

func (n *countingNotifier) InvalidateSessionScopes() {
	n.mu.Lock()
	n.invalidated++
	n.mu.Unlock()
}

func (n *countingNotifier) RunAnalysis() {
	n.mu.Lock()
	n.analysed++
	n.mu.Unlock()
}

func (n *countingNotifier) counts() (int, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.invalidated, n.analysed
}

// lifecycleFixture is one test's store, indexer, config and lifecycle. The
// store is a real sqlite one: the guarded allocations, the cascades and the
// partial-unique primary index are most of what these tests exercise.
type lifecycleFixture struct {
	t       *testing.T
	dir     string
	dbPath  string
	cfgPath string
	store   *store_sqlite.Store
	catalog *store_sqlite.Catalog
	cm      *config.ConfigManager
	mi      *MultiIndexer
	lc      *CheckoutLifecycle
	watcher *fakeWatcher
	notify  *countingNotifier
	clock   *manualClock
}

func newLifecycleFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	dir := t.TempDir()
	f := &lifecycleFixture{
		t:       t,
		dir:     dir,
		dbPath:  filepath.Join(dir, "store.sqlite"),
		cfgPath: filepath.Join(dir, "config.yaml"),
		watcher: newFakeWatcher(),
		notify:  &countingNotifier{},
		clock:   newManualClock(),
	}

	gc := &config.GlobalConfig{}
	gc.SetConfigPath(f.cfgPath)
	require.NoError(t, gc.Save())

	f.open()
	return f
}

// open builds the store, indexer and lifecycle over the fixture's paths. It
// is separate from construction so a test can simulate a daemon restart by
// dropping the whole stack and building a fresh one over the same files.
func (f *lifecycleFixture) open() {
	f.t.Helper()
	store, err := store_sqlite.Open(f.dbPath)
	require.NoError(f.t, err)
	f.store = store
	f.catalog = store.Catalog()

	cm, err := config.NewConfigManager(f.cfgPath)
	require.NoError(f.t, err)
	f.cm = cm

	f.mi = NewMultiIndexer(store, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	lc, err := NewCheckoutLifecycle(CheckoutLifecycleConfig{
		MultiIndexer:  f.mi,
		ConfigManager: cm,
		Graph:         store,
		Logger:        zap.NewNop(),
		Reconcile:     lifecycleGrace,
		Clock:         f.clock.Now,
	})
	require.NoError(f.t, err)
	lc.SetWatcherSource(func() RepoWatcher { return f.watcher })
	lc.SetNotifier(f.notify)
	f.lc = lc
}

// restart tears the stack down and builds a new one over the same store and
// config file — a daemon stop and start, with everything durable intact.
func (f *lifecycleFixture) restart() {
	f.t.Helper()
	require.NoError(f.t, f.lc.Close())
	require.NoError(f.t, f.mi.Close(context.Background()))
	require.NoError(f.t, f.store.Close())
	f.open()
}

// close tears the stack down in the order the daemon does. The coordinators go
// first: each one may be part way through a build against this store, and
// closing the store under a live build is the one teardown order that turns a
// background write into a failure.
func (f *lifecycleFixture) close() {
	_ = f.lc.Close()
	_ = f.mi.Close(context.Background())
	_ = f.store.Close()
}

func TestViewBuildGateWaitUntilOpenBlocksUntilOpen(t *testing.T) {
	gate := NewViewBuildGate()
	result := make(chan error, 1)
	go func() {
		result <- gate.WaitUntilOpen(context.Background())
	}()

	select {
	case err := <-result:
		t.Fatalf("WaitUntilOpen returned before Open: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	gate.Open()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("WaitUntilOpen after Open: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitUntilOpen did not return after Open")
	}
}

func TestViewBuildGateWaitUntilOpenReady(t *testing.T) {
	gate := NewViewBuildGate()
	gate.Open()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := gate.WaitUntilOpen(ctx); err != nil {
		t.Fatalf("WaitUntilOpen on an open gate: %v", err)
	}
}

func TestViewBuildGateWaitUntilOpenHonorsCancellation(t *testing.T) {
	gate := NewViewBuildGate()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := gate.WaitUntilOpen(ctx); err != context.Canceled {
		t.Fatalf("WaitUntilOpen cancellation = %v, want %v", err, context.Canceled)
	}
}

func TestViewBuildGateWaitUntilOpenDoesNotConsumeActiveSlot(t *testing.T) {
	gate := NewViewBuildGate()
	gate.Open()

	release, err := gate.Acquire(context.Background(), ViewBuildBackground)
	if err != nil {
		t.Fatalf("Acquire active slot: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := gate.WaitUntilOpen(ctx); err != nil {
		t.Fatalf("WaitUntilOpen while the active slot is occupied: %v", err)
	}
}

func BenchmarkViewBuildGateWaitUntilOpenReady(b *testing.B) {
	gate := NewViewBuildGate()
	gate.Open()
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := gate.WaitUntilOpen(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func TestCheckoutLifecycleCloseToleratesUninitializedTransitionOwner(t *testing.T) {
	lifecycle := &CheckoutLifecycle{}
	require.NoError(t, lifecycle.Close())
	require.NoError(t, lifecycle.Close(), "Close remains idempotent")
}

// volumeEvidenceUsable reports whether this platform's path evidence carries
// the volume identity that separates "this root was deleted" from "the volume
// it lived on is not mounted right now".
//
// It samples a directory that certainly exists and reads gitstate's
// per-platform path-identity seam, so the answer comes from that seam rather
// than from a hardcoded list of operating systems: a platform that grows an
// implementation flips this to true on its own. The two conditions mirror the
// classifier's own precondition for confirming a prunable worktree, and a
// divergence between them cannot pass quietly — it lands the caller on the
// wrong arm, where the evidence assertion fails.
//
// Where it is false, deleting a directory proves nothing and the janitor
// correctly refuses to act on it; git's administrative record is the only
// removal evidence such a platform has.
func volumeEvidenceUsable(t *testing.T, dir string) bool {
	t.Helper()
	sample := gitstate.SamplePathEvidence(dir)
	if !sample.RootExists {
		t.Fatalf("volume evidence probe of %q found no such directory: %+v", dir, sample)
	}
	return sample.VolumeToken != "" && sample.VolumeKind != gitstate.VolumeKindUnsupported
}

// gitRepo creates a git repository with one Go file and returns its root.
func (f *lifecycleFixture) gitRepo(name string) string {
	f.t.Helper()
	root := filepath.Join(f.dir, name)
	require.NoError(f.t, os.MkdirAll(root, 0o755))
	runGit(f.t, root, "init", "-q", "-b", "main")
	runGit(f.t, root, "config", "user.email", "test@example.com")
	runGit(f.t, root, "config", "user.name", "Test")
	runGit(f.t, root, "config", "commit.gpgsign", "false")
	writeFile(f.t, filepath.Join(root, name+".go"), "package a\n\nfunc A() {}\n")
	runGit(f.t, root, "add", ".")
	runGit(f.t, root, "commit", "-q", "-m", "init")
	return root
}

// worktreeOf adds a linked worktree of an existing repository.
func (f *lifecycleFixture) worktreeOf(main, name string) string {
	f.t.Helper()
	root := filepath.Join(f.dir, name)
	runGit(f.t, main, "worktree", "add", "-q", "-b", name, root)
	writeFile(f.t, filepath.Join(root, name+".go"), "package a\n\nfunc B() {}\n")
	return root
}

// familyOf reads the family a tracked prefix belongs to, through the same
// dedicated-graph binding the sweep uses.
func (f *lifecycleFixture) familyOf(prefix string) store_sqlite.DedicatedGraph {
	f.t.Helper()
	row, ok, err := f.catalog.GetDedicatedGraph(context.Background(), GraphIDFor(prefix))
	require.NoError(f.t, err)
	require.Truef(f.t, ok, "prefix %s has no dedicated-graph binding", prefix)
	return row
}

func (f *lifecycleFixture) checkoutOf(prefix string) store_sqlite.Checkout {
	f.t.Helper()
	binding := f.familyOf(prefix)
	row, ok, err := f.catalog.GetCheckout(context.Background(), binding.OwnerCheckoutID)
	require.NoError(f.t, err)
	require.Truef(f.t, ok, "prefix %s has no checkout row", prefix)
	return row
}

// configPaths reads the tracked-repository list back off disk, which is the
// only thing that survives a restart.
func (f *lifecycleFixture) configPaths() []string {
	f.t.Helper()
	saved, err := config.LoadGlobal(f.cfgPath)
	require.NoError(f.t, err)
	out := make([]string, 0, len(saved.Repos))
	for _, entry := range saved.Repos {
		out = append(out, entry.Path)
	}
	return out
}

// assertRegistered states everything one explicit registration must leave
// behind, whichever surface asked for it.
func (f *lifecycleFixture) assertRegistered(prefix, root string, source store_sqlite.IntentSourceKind) {
	f.t.Helper()
	ctx := context.Background()

	binding := f.familyOf(prefix)
	assert.Equal(f.t, prefix, binding.RepoPrefix, "the graph row binds the repo prefix")
	assert.Equal(f.t, reconcile.GraphStateReady, binding.State)

	family, ok, err := f.catalog.GetRepositoryFamily(ctx, binding.FamilyID)
	require.NoError(f.t, err)
	require.True(f.t, ok, "the family row exists")
	assert.Equal(f.t, FamilyIDFor(family.CommonDirIdentity), family.FamilyID,
		"the family id is derived from the common dir, so it is reproducible")

	checkout := f.checkoutOf(prefix)
	assert.Equal(f.t, store_sqlite.CheckoutStateReady, checkout.State)
	assert.Equal(f.t, store_sqlite.CheckoutModeDedicated, checkout.DesiredMode)
	assert.Equal(f.t, store_sqlite.CheckoutModeDedicated, checkout.EffectiveMode)
	assert.NotEmpty(f.t, checkout.Incarnation)
	assert.NotEmpty(f.t, checkout.AdminName)

	intents, err := f.catalog.ListTrackingIntents(ctx, checkout.CheckoutID)
	require.NoError(f.t, err)
	require.Len(f.t, intents, 1, "one intent, from the surface that asked")
	assert.Equal(f.t, source, intents[0].SourceKind)
	assert.True(f.t, intents[0].Active)

	_, present, err := f.catalog.GetCheckoutPathEvidence(ctx, checkout.CheckoutID)
	require.NoError(f.t, err)
	assert.True(f.t, present, "the removal test needs a stored sample of the root")

	assert.False(f.t, f.watcher.isAttached(prefix), "a route-owned dedicated repo bypasses the legacy watcher")
	assert.Contains(f.t, f.configPaths(), root, "the tracked set is persisted")
	assert.NotNil(f.t, f.mi.GetMetadata(prefix), "the repo is in the corpus")
}

// TestCheckoutLifecycleTrackSurfaceParity drives both explicit track surfaces
// and asserts they leave identical state behind. The surfaces differ in one
// thing only — who is recorded as having asked.
func TestCheckoutLifecycleTrackSurfaceParity(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	cliRoot := f.gitRepo("via-cli")
	mcpRoot := f.gitRepo("via-mcp")

	cli, err := f.lc.Register(ctx, config.RepoEntry{Path: cliRoot}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, cli.CatalogErr)
	require.False(t, cli.AlreadyTracked)

	mcp, err := f.lc.Register(ctx, config.RepoEntry{Path: mcpRoot}, TrackSourceMCP)
	require.NoError(t, err)
	require.NoError(t, mcp.CatalogErr)
	require.False(t, mcp.AlreadyTracked)

	f.assertRegistered(cli.Prefix, cliRoot, TrackSourceCLI)
	f.assertRegistered(mcp.Prefix, mcpRoot, TrackSourceMCP)

	// Separate repositories are separate families with their own primaries.
	assert.NotEqual(t, cli.FamilyID, mcp.FamilyID)
	assert.True(t, f.familyOf(cli.Prefix).IsPrimaryBase)
	assert.True(t, f.familyOf(mcp.Prefix).IsPrimaryBase)

	invalidated, analysed := f.notify.counts()
	assert.Equal(t, 2, invalidated, "each track invalidates the session scopes")
	assert.Equal(t, 2, analysed, "each track reruns the analysis")

	// Re-registering is idempotent: the identity is reused, not minted again.
	again, err := f.lc.Register(ctx, config.RepoEntry{Path: cliRoot}, TrackSourceCLI)
	require.NoError(t, err)
	assert.True(t, again.AlreadyTracked)
	assert.Equal(t, cli.CheckoutID, again.CheckoutID)
	assert.Equal(t, cli.Incarnation, again.Incarnation)
}

// TestCheckoutLifecycleUntrackSurfaceParity untracks through both surfaces
// and asserts the same teardown ran: no checkout rows, no watcher, no config
// entry — and a later track mints a genuinely new identity rather than
// resurrecting the old one.
func TestCheckoutLifecycleUntrackSurfaceParity(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		source store_sqlite.IntentSourceKind
	}{
		{name: "cli", source: TrackSourceCLI},
		{name: "mcp", source: TrackSourceMCP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := f.gitRepo("untrack-" + tc.name)
			tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: root}, tc.source)
			require.NoError(t, err)
			require.NoError(t, tracked.CatalogErr)
			familyID := tracked.FamilyID
			require.NotEmpty(t, familyID)

			result, err := f.lc.Untrack(ctx, root)
			require.NoError(t, err)
			assert.Equal(t, tracked.Prefix, result.Prefix)
			assert.Equal(t, []string{string(tc.source)}, result.Revoked)

			checkouts, err := f.catalog.ListCheckouts(ctx, familyID)
			require.NoError(t, err)
			assert.Empty(t, checkouts, "the forget saga removes the checkout row")
			_, ok, err := f.catalog.GetDedicatedGraph(ctx, GraphIDFor(tracked.Prefix))
			require.NoError(t, err)
			assert.False(t, ok, "the graph binding goes with it")

			assert.False(t, f.watcher.isAttached(tracked.Prefix), "the watcher is detached")
			assert.Nil(t, f.mi.GetMetadata(tracked.Prefix), "the repo leaves the corpus")
			assert.NotContains(t, f.configPaths(), root, "the config no longer lists it")

			// Tracking the same path again is a new checkout, not the old one.
			retracked, err := f.lc.Register(ctx, config.RepoEntry{Path: root}, tc.source)
			require.NoError(t, err)
			require.NoError(t, retracked.CatalogErr)
			assert.Equal(t, familyID, retracked.FamilyID, "the family survives its checkouts")
			assert.NotEqual(t, tracked.CheckoutID, retracked.CheckoutID)
			assert.NotEqual(t, tracked.Incarnation, retracked.Incarnation)
			assert.False(t, f.watcher.isAttached(retracked.Prefix),
				"retracking recreates a route-owned dedicated corpus without a legacy watcher")
		})
	}
}

// TestCheckoutLifecycleReloadDiff covers both halves of a configuration
// reload: an added entry is registered exactly as an explicit track would
// be, a removed entry that something else can serve is retired, and a
// removed entry that nothing else can serve records a pending transition
// instead of deleting a corpus nobody asked to lose.
func TestCheckoutLifecycleReloadDiff(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	main := f.gitRepo("reload-main")
	worktree := f.worktreeOf(main, "reload-wt")

	gc := f.cm.Global()
	require.NoError(t, gc.AddRepo(config.RepoEntry{Path: main, Name: "reload-main"}))
	require.NoError(t, gc.AddRepo(config.RepoEntry{Path: worktree, Name: "reload-wt"}))
	require.NoError(t, gc.Save())

	added, err := f.lc.ApplyReload(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, added.Added)
	assert.Equal(t, 0, added.Removed)
	f.assertRegistered("reload-main", main, TrackSourceConfig)
	f.assertRegistered("reload-wt", worktree, TrackSourceConfig)
	require.True(t, f.familyOf("reload-main").IsPrimaryBase,
		"the first checkout registered holds the family's primary base")
	require.False(t, f.familyOf("reload-wt").IsPrimaryBase)

	// The worktree is a live non-primary checkout of a family that has a
	// ready primary, so dropping explicit intent demotes it to an automatic
	// overlay instead of making the live checkout disappear.
	require.NoError(t, gc.RemoveRepo(worktree))
	require.NoError(t, gc.Save())
	removed, err := f.lc.ApplyReload(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, removed.Removed)
	assert.Equal(t, 0, removed.Pending)
	assert.NotNil(t, f.mi.GetMetadata("reload-wt"),
		"revoking explicit intent leaves the live checkout as an automatic overlay")
	assert.False(t, f.watcher.isAttached("reload-wt"))

	// The main checkout owns the family's primary base: nothing is left to
	// serve it from, so its removal is recorded and NOT applied.
	mainCheckout := f.checkoutOf("reload-main")
	require.NoError(t, gc.RemoveRepo(main))
	require.NoError(t, gc.Save())
	pending, err := f.lc.ApplyReload(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, pending.Removed)
	assert.Equal(t, 1, pending.Pending)
	assert.NotNil(t, f.mi.GetMetadata("reload-main"),
		"a config edit must not silently delete the only servable checkout")

	transition, ok, err := f.catalog.GetIntentTransition(ctx, mainCheckout.CheckoutID)
	require.NoError(t, err)
	require.True(t, ok, "the removal is recorded as a pending transition")
	assert.Equal(t, store_sqlite.IntentTransitionPending, transition.State)
	assert.Equal(t, store_sqlite.CheckoutModeAutomatic, transition.RequestedMode)

	// A second reload over the same state does not stack a second
	// transition, and still does not delete.
	repeat, err := f.lc.ApplyReload(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, repeat.Pending)
	assert.NotNil(t, f.mi.GetMetadata("reload-main"))
}

// TestCheckoutLifecycleSweepRemovesVanishedWorktree is the janitor path: a
// linked worktree is deleted on disk, the sweep holds it through its removal
// grace, cleans it up once the grace expires, persists the removal, and does
// not resurrect it after a restart.
func TestCheckoutLifecycleSweepRemovesVanishedWorktree(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	main := f.gitRepo("sweep-main")
	worktree := f.worktreeOf(main, "sweep-wt")

	mainTracked, err := f.lc.Register(ctx, config.RepoEntry{Path: main, Name: "sweep-main"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, mainTracked.CatalogErr)
	wtTracked, err := f.lc.Register(ctx, config.RepoEntry{Path: worktree, Name: "sweep-wt"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, wtTracked.CatalogErr)
	worktreeView := f.materialize(wtTracked.CheckoutID)
	require.NotEmpty(t, contentIdentities(worktreeView.Reader, wtTracked.Prefix),
		"the worktree's routed view is indexed before it vanishes")
	worktreeView.Close()

	deletedRows, err := f.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{
		GraphID: wtTracked.GraphID,
	})
	require.NoError(t, err)
	require.NotEmpty(t, deletedRows, "the dedicated worktree owns durable generations before removal")
	deletedGenerationIDs := make([]int64, 0, len(deletedRows))
	deletedGenerationSet := make(map[int64]struct{}, len(deletedRows))
	for _, row := range deletedRows {
		deletedGenerationSet[row.GenerationID] = struct{}{}
	}
	var (
		populatedDeletedGeneration int64
		hasDerivedChild            bool
	)
	for _, row := range deletedRows {
		deletedGenerationIDs = append(deletedGenerationIDs, row.GenerationID)
		_, hasOwnedBase := deletedGenerationSet[row.BaseGenerationID]
		hasDerivedChild = hasDerivedChild || hasOwnedBase
		if populatedDeletedGeneration == 0 && len(contentIdentities(f.store.AtGeneration(row.GenerationID), wtTracked.Prefix)) > 0 {
			populatedDeletedGeneration = row.GenerationID
		}
	}
	require.True(t, hasDerivedChild,
		"the deleted graph includes a child generation that must retire before its base")
	require.NotZero(t, populatedDeletedGeneration, "the retirement assertion starts with real generation payload")

	primaryRowsBefore, err := f.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{
		GraphID: mainTracked.GraphID,
	})
	require.NoError(t, err)
	require.NotEmpty(t, primaryRowsBefore, "the healthy primary sibling owns its own generations")

	// Nothing has vanished yet: a sweep changes nothing.
	report, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Families, "both checkouts share one family")
	assert.Equal(t, 0, report.Removed)

	// Which evidence class puts this checkout on the removal path is a
	// platform property, probed through the path-identity seam.
	wantEvidence := reconcile.EvidencePrunableConfirmed
	require.NoError(t, os.RemoveAll(worktree))
	if !volumeEvidenceUsable(t, main) {
		// A deleted root and an unmounted volume are the same observation
		// here, so the deletion alone is not evidence and the janitor holds.
		// Pruning gives git's administrative omission instead, which is the
		// other way onto the same removal path — everything below is the one
		// janitor, reached by the evidence this platform actually has.
		runGit(t, main, "worktree", "prune")
		wantEvidence = reconcile.EvidenceAuthoritativeOmission
	}

	// The first sweep after the removal starts the removal clock. The
	// checkout keeps its identity and its nodes until the clock expires.
	graced, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, graced.Removed, "a removal waits out its grace")
	held := f.checkoutOf("sweep-wt")
	assert.Equal(t, store_sqlite.CheckoutStateRemovalGrace, held.State,
		"queries must leave the vanished checkout's route during grace")
	assert.NotZero(t, held.RemovalDetectedAt, "the removal clock is running")
	assert.Equal(t, string(wantEvidence), held.RemovalEvidence)
	assert.Zero(t, held.UnavailableSince, "evidenced removal is not an outage")
	assert.NotNil(t, f.mi.GetMetadata("sweep-wt"))

	// Still inside the grace: a sweep must not act.
	f.clock.advance(lifecycleGrace.RemovalGrace - time.Second)
	stillHeld, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, stillHeld.Removed)
	assert.NotNil(t, f.mi.GetMetadata("sweep-wt"))

	// Past the deadline: the forget saga runs and drives every hook.
	f.clock.advance(2 * time.Second)
	swept, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, swept.Removed)
	assert.Nil(t, f.mi.GetMetadata("sweep-wt"), "the vanished worktree leaves the corpus")
	_, graphStillPresent, graphErr := f.catalog.GetDedicatedGraph(ctx, wtTracked.GraphID)
	require.NoError(t, graphErr)
	assert.False(t, graphStillPresent, "its logical graph is forgotten")
	deletedRowsAfter, err := f.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{
		GraphID: wtTracked.GraphID,
	})
	require.NoError(t, err)
	assert.Empty(t, deletedRowsAfter, "a deleted graph leaves no generation catalog rows")
	for _, generationID := range deletedGenerationIDs {
		_, found, lookupErr := f.catalog.GetViewGeneration(ctx, generationID)
		require.NoError(t, lookupErr)
		assert.False(t, found, "generation %d survives its owning graph", generationID)
		assert.Empty(t, contentIdentities(f.store.AtGeneration(generationID), wtTracked.Prefix),
			"generation %d leaves no payload", generationID)
	}
	assert.Empty(t, contentIdentities(f.store.AtGeneration(populatedDeletedGeneration), wtTracked.Prefix),
		"the known-populated generation payload is physically purged")

	primaryRowsAfter, err := f.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{
		GraphID: mainTracked.GraphID,
	})
	require.NoError(t, err)
	require.Len(t, primaryRowsAfter, len(primaryRowsBefore), "the healthy primary sibling is untouched")
	for i := range primaryRowsBefore {
		assert.Equal(t, primaryRowsBefore[i].GenerationID, primaryRowsAfter[i].GenerationID)
	}
	assert.False(t, f.watcher.isAttached("sweep-wt"), "its watcher is detached")
	assert.NotContains(t, f.configPaths(), worktree, "the removal is persisted")

	// The live main checkout is untouched throughout.
	assert.NotNil(t, f.mi.GetMetadata("sweep-main"))
	assert.Contains(t, f.configPaths(), main)

	// A restart re-reads the same store. Nothing comes back.
	f.restart()
	require.NoError(t, f.lc.Seed(ctx))
	assert.Nil(t, f.mi.GetMetadata("sweep-wt"), "a restart does not resurrect it")
	checkouts, err := f.catalog.ListCheckouts(ctx, f.familyOf("sweep-main").FamilyID)
	require.NoError(t, err)
	require.Len(t, checkouts, 1, "only the surviving checkout is left")
	assert.NotEqual(t, "sweep-wt", checkouts[0].RootPath)
}

// TestCheckoutLifecycleSweepRecoversDeletedGraphRetirement covers the crash
// window after the catalog graph row is deleted but before the process-local
// owed set can be drained. The surviving generation rows are the durable
// backlog; a live materialized view still protects them until its lease ends.
func TestCheckoutLifecycleSweepRecoversDeletedGraphRetirement(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	targetRoot := f.gitRepo("orphaned-graph-target")
	target, err := f.lc.Register(ctx, config.RepoEntry{
		Path: targetRoot,
		Name: "orphaned-graph-target",
	}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, target.CatalogErr)
	siblingRoot := f.gitRepo("orphaned-graph-sibling")
	sibling, err := f.lc.Register(ctx, config.RepoEntry{
		Path: siblingRoot,
		Name: "orphaned-graph-sibling",
	}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, sibling.CatalogErr)

	targetView := f.materialize(target.CheckoutID)
	viewClosed := false
	t.Cleanup(func() {
		if !viewClosed {
			targetView.Close()
		}
	})
	leasedGenerations := targetView.GenerationSources()
	require.NotEmpty(t, leasedGenerations)
	var populatedLeasedGeneration int64
	for _, source := range leasedGenerations {
		if len(contentIdentities(f.store.AtGeneration(source.Generation), target.Prefix)) > 0 {
			populatedLeasedGeneration = source.Generation
			break
		}
	}
	require.NotZero(t, populatedLeasedGeneration, "the live lease protects real payload")

	targetRows, err := f.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{
		GraphID: target.GraphID,
	})
	require.NoError(t, err)
	require.NotEmpty(t, targetRows)
	siblingRowsBefore, err := f.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{
		GraphID: sibling.GraphID,
	})
	require.NoError(t, err)
	require.NotEmpty(t, siblingRowsBefore)

	// This is the durable state a crash can leave after logical teardown has
	// withdrawn the route and deleted graph ownership, but before the
	// process-local owed set can drain. Clear that set to model the restart.
	f.lc.withdrawStaleRoute(ctx, target.CheckoutID)
	require.NoError(t, f.catalog.DeleteDedicatedGraph(ctx, target.GraphID))
	f.lc.coordMu.Lock()
	f.lc.owed = map[int64]struct{}{}
	f.lc.coordMu.Unlock()
	f.lc.sweepRetirements(ctx)
	for _, source := range leasedGenerations {
		_, found, lookupErr := f.catalog.GetViewGeneration(ctx, source.Generation)
		require.NoError(t, lookupErr)
		assert.True(t, found, "a live lease keeps generation %d", source.Generation)
	}
	assert.NotEmpty(t, contentIdentities(f.store.AtGeneration(populatedLeasedGeneration), target.Prefix),
		"lease refusal preserves payload")

	targetView.Close()
	viewClosed = true
	f.lc.sweepRetirements(ctx)
	targetRowsAfter, err := f.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{
		GraphID: target.GraphID,
	})
	require.NoError(t, err)
	assert.Empty(t, targetRowsAfter, "the durable orphan backlog drains after the lease")
	for _, row := range targetRows {
		_, found, lookupErr := f.catalog.GetViewGeneration(ctx, row.GenerationID)
		require.NoError(t, lookupErr)
		assert.False(t, found)
		assert.Empty(t, contentIdentities(f.store.AtGeneration(row.GenerationID), target.Prefix))
	}

	siblingRowsAfter, err := f.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{
		GraphID: sibling.GraphID,
	})
	require.NoError(t, err)
	require.Len(t, siblingRowsAfter, len(siblingRowsBefore),
		"an independent dedicated sibling remains healthy")
	for i := range siblingRowsBefore {
		assert.Equal(t, siblingRowsBefore[i].GenerationID, siblingRowsAfter[i].GenerationID)
	}
}

// TestCheckoutLifecycleSweepReachesAConfiguredButUnindexedRepo covers the
// family the corpus cannot name: a configured repository whose root could not
// be reached at boot has catalog rows and no corpus metadata, so a sweep that
// enumerated only what is indexed would never reconcile it — and availability
// handling would never engage for the one checkout it exists for.
func TestCheckoutLifecycleSweepReachesAConfiguredButUnindexedRepo(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	root := f.gitRepo("unindexed-repo")
	gc := f.cm.Global()
	require.NoError(t, gc.AddRepo(config.RepoEntry{Path: root, Name: "unindexed-repo"}))
	require.NoError(t, gc.Save())

	// Seeding records the identity without indexing — the state a boot that
	// skipped an unreachable root leaves behind.
	require.NoError(t, f.lc.Seed(ctx))
	tracked := f.checkoutOf("unindexed-repo")
	require.Nil(t, f.mi.GetMetadata("unindexed-repo"), "nothing of it is in the corpus")

	require.NoError(t, os.RemoveAll(root))

	graced, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, graced.Families, "the configured family is reconciled")
	assert.Equal(t, 0, graced.Removed)
	assert.False(t, graced.Reports[0].InventoryUsable, "git could not be read")

	held := f.checkoutOf("unindexed-repo")
	assert.Equal(t, tracked.CheckoutID, held.CheckoutID, "the identity survives")
	assert.Equal(t, store_sqlite.CheckoutStateAvailabilityGrace, held.State,
		"an unreachable root enters availability handling, indexed or not")
	assert.Zero(t, held.RemovalDetectedAt, "an outage never starts the removal clock")
}

// TestCheckoutLifecycleSeedIsIdempotent runs the startup path twice and
// asserts the second pass mints nothing the first already minted.
//
// Seeding is a migration followed by the boot reconciliation. The migration
// half must leave an existing identity exactly as it found it — a re-keyed
// checkout or a second intent per restart is the bug this guards — while the
// reconciliation half is an observation and moves the clocks and the path
// sample it takes, exactly as the janitor's pass does an hour later.
func TestCheckoutLifecycleSeedIsIdempotent(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	root := f.gitRepo("seed-repo")
	gc := f.cm.Global()
	require.NoError(t, gc.AddRepo(config.RepoEntry{Path: root, Name: "seed-repo"}))
	require.NoError(t, gc.Save())

	require.NoError(t, f.lc.Seed(ctx))
	first := f.checkoutOf("seed-repo")
	firstEvidence, present, err := f.catalog.GetCheckoutPathEvidence(ctx, first.CheckoutID)
	require.NoError(t, err)
	require.True(t, present)
	assert.Nil(t, f.mi.GetMetadata("seed-repo"), "seeding records identity, it does not index")

	f.clock.advance(time.Hour)
	require.NoError(t, f.lc.Seed(ctx))
	second := f.checkoutOf("seed-repo")
	secondEvidence, _, err := f.catalog.GetCheckoutPathEvidence(ctx, second.CheckoutID)
	require.NoError(t, err)

	assert.Equal(t, first.CheckoutID, second.CheckoutID, "the identity is reused, not minted again")
	assert.Equal(t, first.Incarnation, second.Incarnation, "the row is not re-keyed")
	assert.Equal(t, first.AdminName, second.AdminName)
	assert.Equal(t, first.RootPath, second.RootPath)
	assert.Equal(t, first.State, second.State)
	assert.Equal(t, first.DesiredMode, second.DesiredMode)
	assert.Equal(t, first.EffectiveMode, second.EffectiveMode)
	assert.Zero(t, second.UnavailableSince, "a reachable root starts no availability clock")
	assert.Zero(t, second.RemovalDetectedAt, "a reachable root starts no removal clock")

	assert.Equal(t, firstEvidence.CheckoutID, secondEvidence.CheckoutID)
	assert.Equal(t, firstEvidence.RootPathIdentity, secondEvidence.RootPathIdentity,
		"the sample still describes the same root")

	intents, err := f.catalog.ListTrackingIntents(ctx, first.CheckoutID)
	require.NoError(t, err)
	assert.Len(t, intents, 1, "the intent is upserted on its source key, not duplicated")
}

// TestCheckoutLifecycleSeedReusesDedicatedGraphOwnedUnderPreviousPrefix covers
// a restart whose config still names the same checkout but derives a different
// repo prefix. The durable owner binding wins over the newly derived graph ID.
func TestCheckoutLifecycleSeedReusesDedicatedGraphOwnedUnderPreviousPrefix(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	root := f.gitRepo("seed-owner-replay")
	gc := f.cm.Global()
	require.NoError(t, gc.AddRepo(config.RepoEntry{Path: root, Name: "seed-original"}))
	require.NoError(t, gc.Save())
	require.NoError(t, f.lc.Seed(ctx))

	beforeCheckout := f.checkoutOf("seed-original")
	beforeGraph := f.familyOf("seed-original")

	require.NoError(t, gc.RemoveRepo(root))
	require.NoError(t, gc.AddRepo(config.RepoEntry{Path: root, Name: "seed-renamed"}))
	require.NoError(t, gc.Save())
	f.restart()
	require.NoError(t, f.lc.Seed(ctx))

	reused, found, err := f.catalog.GetDedicatedGraphByOwner(ctx, beforeCheckout.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, beforeGraph.GraphID, reused.GraphID)
	assert.Equal(t, beforeGraph.RepoPrefix, reused.RepoPrefix)
	assert.Equal(t, beforeGraph.FamilyID, reused.FamilyID)

	_, found, err = f.catalog.GetDedicatedGraph(ctx, GraphIDFor("seed-renamed"))
	require.NoError(t, err)
	assert.False(t, found, "restart must not mint a graph for the newly derived prefix")
	graphs, err := f.catalog.ListDedicatedGraphs(ctx, beforeGraph.FamilyID)
	require.NoError(t, err)
	assert.Len(t, graphs, 1, "the stable checkout owner keeps exactly one dedicated graph")

	afterCheckout := f.checkoutOf("seed-original")
	assert.Equal(t, beforeCheckout.CheckoutID, afterCheckout.CheckoutID)
	assert.Equal(t, beforeCheckout.Incarnation, afterCheckout.Incarnation)
}

func TestCheckoutLifecycleBindDedicatedGraphRejectsOwnerFamilyMismatch(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	root := f.gitRepo("seed-owner-family-guard")
	gc := f.cm.Global()
	require.NoError(t, gc.AddRepo(config.RepoEntry{Path: root, Name: "family-owner"}))
	require.NoError(t, gc.Save())
	require.NoError(t, f.lc.Seed(ctx))

	checkout := f.checkoutOf("family-owner")
	owned := f.familyOf("family-owner")
	graphID, err := f.lc.bindDedicatedGraph(ctx, "different-family", checkout.CheckoutID, "different-prefix")
	require.ErrorIs(t, err, store_sqlite.ErrCatalogStaleGuard)
	assert.Empty(t, graphID)

	standing, found, lookupErr := f.catalog.GetDedicatedGraphByOwner(ctx, checkout.CheckoutID)
	require.NoError(t, lookupErr)
	require.True(t, found)
	assert.Equal(t, owned.GraphID, standing.GraphID)
	_, found, lookupErr = f.catalog.GetDedicatedGraph(ctx, GraphIDFor("different-prefix"))
	require.NoError(t, lookupErr)
	assert.False(t, found, "a family mismatch must not insert a second graph")
	graphs, lookupErr := f.catalog.ListDedicatedGraphs(ctx, owned.FamilyID)
	require.NoError(t, lookupErr)
	assert.Len(t, graphs, 1)
}

// TestCheckoutLifecycleInaccessibleRootIsForgottenAfterGrace covers a checkout
// whose root and Git directory disappear together. It receives one
// availability grace and is then removed completely, including its persisted
// configuration, rather than surviving as an unavailable shadow graph.
func TestCheckoutLifecycleInaccessibleRootIsForgottenAfterGrace(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	root := f.gitRepo("offline-repo")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: root, Name: "offline-repo"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	require.NoError(t, os.RemoveAll(root))

	graced, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, graced.Removed)
	require.Len(t, graced.Reports, 1)
	assert.False(t, graced.Reports[0].InventoryUsable)

	held := f.checkoutOf("offline-repo")
	assert.Equal(t, tracked.CheckoutID, held.CheckoutID)
	assert.Equal(t, store_sqlite.CheckoutStateAvailabilityGrace, held.State)
	assert.Zero(t, held.RemovalDetectedAt, "inaccessibility uses only one grace clock")

	f.clock.advance(lifecycleGrace.AvailabilityGrace + time.Second)
	expired, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, expired.Removed)
	_, checkoutExists, err := f.catalog.GetCheckout(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	assert.False(t, checkoutExists, "the checkout catalog row is removed")
	assert.False(t, f.watcher.isAttached("offline-repo"), "its watcher is detached")
	assert.Nil(t, f.mi.GetMetadata("offline-repo"), "its corpus is evicted")
	assert.NotContains(t, f.configPaths(), root, "the removal is persisted")

	f.restart()
	require.NoError(t, f.lc.Seed(ctx))
	assert.Nil(t, f.mi.GetMetadata("offline-repo"), "a restart does not resurrect it")
}

// TestCheckoutLifecycleImplicitRegistrationWritesNoIntent covers the
// auto-index path: the checkout is recorded and served like any other, but
// nobody asked for it, so no tracking intent claims otherwise — and nothing
// is written to the configuration that a later boot could read back as one.
func TestCheckoutLifecycleImplicitRegistrationWritesNoIntent(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	root := f.gitRepo("implicit-repo")
	_, err := f.mi.TrackRepoCtx(ctx, config.RepoEntry{Path: root, Name: "implicit-repo"})
	require.NoError(t, err)
	require.NoError(t, f.lc.RecordImplicit(ctx, root))

	checkout := f.checkoutOf("implicit-repo")
	intents, err := f.catalog.ListTrackingIntents(ctx, checkout.CheckoutID)
	require.NoError(t, err)
	assert.Empty(t, intents, "an implicit observation is not an intent")
	assert.True(t, f.watcher.isAttached("implicit-repo"), "it is still watched")
	assert.NotContains(t, f.configPaths(), root,
		"persisting it would make the next boot seed a manual_config intent for it")

	// With no intent to revoke, forgetting it is unobstructed.
	result, err := f.lc.Untrack(ctx, root)
	require.NoError(t, err)
	assert.Empty(t, result.Revoked)
	assert.Nil(t, f.mi.GetMetadata("implicit-repo"))
}

// TestCheckoutLifecycleImplicitRegistrationSurvivesRestartWithoutIntent is the
// consequence the previous test's config assertion protects: a restart re-reads
// the tracked-repository list from disk and seeds an intent for every entry it
// finds, so an implicit observation that had been persisted would come back one
// boot later as explicit configuration.
func TestCheckoutLifecycleImplicitRegistrationSurvivesRestartWithoutIntent(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	root := f.gitRepo("implicit-restart")
	_, err := f.mi.TrackRepoCtx(ctx, config.RepoEntry{Path: root, Name: "implicit-restart"})
	require.NoError(t, err)
	require.NoError(t, f.lc.RecordImplicit(ctx, root))
	checkout := f.checkoutOf("implicit-restart")

	f.restart()
	require.NoError(t, f.lc.Seed(ctx))

	intents, err := f.catalog.ListTrackingIntents(ctx, checkout.CheckoutID)
	require.NoError(t, err)
	assert.Empty(t, intents, "a boot does not turn an observation into intent")
}

// TestCheckoutLifecycleRegistrationBringsUpCoordinators pins the timing the
// janitor's tick is far too coarse for.
//
// Tracking a repository whose worktrees already exist has to give those
// worktrees a routed view within the registration, not within the hour the
// default reconcile interval puts between janitor passes. Registering one of
// them explicitly afterwards is the opposite transition: the working copy gets
// a corpus of its own and stops being served through a composed view.
func TestCheckoutLifecycleRegistrationBringsUpCoordinators(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	main := f.gitRepo("register-main")
	worktree := f.worktreeOf(main, "register-wt")

	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: main, Name: "register-main"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	checkouts, err := f.catalog.ListCheckouts(ctx, tracked.FamilyID)
	require.NoError(t, err)
	var automatic *store_sqlite.Checkout
	for i := range checkouts {
		if checkouts[i].CheckoutID != tracked.CheckoutID {
			automatic = &checkouts[i]
		}
	}
	require.NotNil(t, automatic, "the worktree that already existed got no identity")
	assert.Equal(t, store_sqlite.CheckoutModeAutomatic, automatic.EffectiveMode)
	assert.True(t, f.lc.SignalCheckout(automatic.CheckoutID, "test"),
		"the worktree has no coordinator until a sweep runs")

	retracked, err := f.lc.Register(ctx, config.RepoEntry{Path: worktree, Name: "register-wt"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, retracked.CatalogErr)
	assert.Equal(t, automatic.CheckoutID, retracked.CheckoutID,
		"an explicit track adopts the observed identity rather than minting a second one")
	assert.Equal(t, store_sqlite.CheckoutModeDedicated, f.checkoutOf(retracked.Prefix).EffectiveMode,
		"tracking a worktree explicitly gives it a corpus of its own")
	assert.True(t, f.lc.SignalCheckout(automatic.CheckoutID, "test"),
		"a dedicated checkout is served by its own graph-bound coordinator")
}

// coordinatorReported reads one checkout's coordinator flag off the
// administrative census — the answer 'gortex repos families' renders.
func (f *lifecycleFixture) coordinatorReported(checkoutID string) bool {
	f.t.Helper()
	overview, err := f.lc.FamiliesOverview(context.Background(), "")
	require.NoError(f.t, err)
	for _, family := range overview.Families {
		for _, checkout := range family.Checkouts {
			if checkout.CheckoutID == checkoutID {
				return checkout.CoordinatorLive
			}
		}
	}
	f.t.Fatalf("checkout %s is not in the census", checkoutID)
	return false
}

// TestCoordinatorLivenessFollowsTheLoopNotTheRegistry states what the
// administrative surfaces mean by a live coordinator: this process is running
// the checkout's build loop.
//
// The registry is not that. Every transition builds its replacement, drives a
// whole rebuild with it and registers it only once that rebuild has landed, so
// a census taken across a restart-sized build reads a daemon whose loops are
// running as one running none — the opposite of what the operator asking is
// looking for, and exactly when they are asking.
func TestCoordinatorLivenessFollowsTheLoopNotTheRegistry(t *testing.T) {
	f := newFamilyFixture(t, "liveness")
	defer f.close()
	ctx := context.Background()

	// The daemon's restart path: a fresh stack over the same store, the tracked
	// set re-registered the way warmup re-tracks it, and the seeding that
	// reconciles every family it touched — which brings both the dedicated
	// primary and the automatic checkout's coordinators back up.
	f.restart()
	_, err := f.mi.TrackRepoCtx(ctx, config.RepoEntry{Path: f.main, Name: f.mainPrefix})
	require.NoError(t, err)
	require.NoError(t, f.lc.Seed(ctx))
	owner := f.checkoutOf(f.mainPrefix)
	require.True(t, f.coordinatorReported(f.automatic.CheckoutID),
		"the restart's own reconciliation brought no automatic coordinator back")
	require.True(t, f.coordinatorReported(owner.CheckoutID),
		"the restart's own reconciliation brought no dedicated coordinator back")
	require.Equal(t, 2, f.lc.liveCoordinators(""))

	// The window every automatic transition opens: that checkout's registered
	// coordinator is dropped first. The independent dedicated loop remains.
	f.lc.dropCoordinator(f.automatic.CheckoutID)
	require.False(t, f.coordinatorReported(f.automatic.CheckoutID),
		"a dropped coordinator is still reported live")
	require.True(t, f.coordinatorReported(owner.CheckoutID))
	require.Equal(t, 1, f.lc.liveCoordinators(""))

	checkout, found, err := f.catalog.GetCheckout(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	coordinator, err := f.lc.buildCoordinator(ctx, f.primaryGraph, checkout)
	require.NoError(t, err)
	require.NotNil(t, coordinator)

	assert.True(t, f.coordinatorReported(f.automatic.CheckoutID),
		"a running build loop is reported as no coordinator at all")
	assert.True(t, f.coordinatorReported(owner.CheckoutID))
	assert.Equal(t, 2, f.lc.liveCoordinators(""),
		"the reconcile verb omitted one of two running build loops")
	assert.Equal(t, 2, f.lc.liveCoordinators(f.familyID))
	assert.Zero(t, f.lc.liveCoordinators("another-family"),
		"a family that runs nothing is counted a coordinator")

	require.NoError(t, coordinator.Close())
	assert.False(t, f.coordinatorReported(f.automatic.CheckoutID),
		"a stopped coordinator is reported live")
	assert.True(t, f.coordinatorReported(owner.CheckoutID))
	assert.Equal(t, 1, f.lc.liveCoordinators(""))
}

// TestCheckoutLifecycleAdminNameIdentity states the identity rule the whole
// lifecycle rests on: a checkout is keyed by (family, administrative name),
// so the main worktree and a linked one of the same repository are two
// identities in one family.
func TestCheckoutLifecycleAdminNameIdentity(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	main := f.gitRepo("ident-main")
	worktree := f.worktreeOf(main, "ident-wt")

	mainTracked, err := f.lc.Register(ctx, config.RepoEntry{Path: main, Name: "ident-main"}, TrackSourceCLI)
	require.NoError(t, err)
	wtTracked, err := f.lc.Register(ctx, config.RepoEntry{Path: worktree, Name: "ident-wt"}, TrackSourceMCP)
	require.NoError(t, err)

	assert.Equal(t, mainTracked.FamilyID, wtTracked.FamilyID, "one repository, one family")
	assert.NotEqual(t, mainTracked.CheckoutID, wtTracked.CheckoutID)
	assert.Equal(t, gitstate.MainAdminName, f.checkoutOf("ident-main").AdminName)
	assert.NotEqual(t, gitstate.MainAdminName, f.checkoutOf("ident-wt").AdminName)

	checkouts, err := f.catalog.ListCheckouts(ctx, mainTracked.FamilyID)
	require.NoError(t, err)
	names := make([]string, 0, len(checkouts))
	for _, checkout := range checkouts {
		names = append(names, checkout.AdminName)
	}
	slices.Sort(names)
	assert.Len(t, names, 2, "two working copies, two identities")
}
