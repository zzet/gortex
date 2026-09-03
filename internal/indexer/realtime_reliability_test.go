package indexer

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/thirdparty/fswatcher"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/search"
)

// toggleExtractor is a parser.Extractor whose Extract result is flipped
// at runtime. In good mode it emits a file node plus one function node;
// in fail mode it returns (nil, err) — the exact shape that drives
// indexFile's `result == nil` branch (a transient parse failure /
// quarantine), which tree-sitter's error-tolerant grammars never
// produce for real Go source. The custom ".fk" extension keeps it off
// every other extractor's turf.
type toggleExtractor struct {
	mu    sync.Mutex
	fail  bool
	funcs []string
}

func (e *toggleExtractor) Language() string     { return "faketoggle" }
func (e *toggleExtractor) Extensions() []string { return []string{".fk"} }

func (e *toggleExtractor) setFail(f bool) {
	e.mu.Lock()
	e.fail = f
	e.mu.Unlock()
}

func (e *toggleExtractor) setFuncs(names ...string) {
	e.mu.Lock()
	e.funcs = names
	e.mu.Unlock()
}

func (e *toggleExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	e.mu.Lock()
	fail := e.fail
	funcs := append([]string(nil), e.funcs...)
	e.mu.Unlock()
	if fail {
		return nil, errors.New("toggleExtractor: forced parse failure")
	}
	nodes := []*graph.Node{{
		ID:       filePath,
		Kind:     graph.KindFile,
		Name:     filepath.Base(filePath),
		FilePath: filePath,
		Language: "faketoggle",
	}}
	for _, fn := range funcs {
		nodes = append(nodes, &graph.Node{
			ID:       filePath + "::" + fn,
			Kind:     graph.KindFunction,
			Name:     fn,
			FilePath: filePath,
			Language: "faketoggle",
		})
	}
	return &parser.ExtractionResult{Nodes: nodes}, nil
}

func newToggleIndexer(t *testing.T) (*Indexer, *toggleExtractor, *store_sqlite.Store) {
	t.Helper()
	ext := &toggleExtractor{}
	reg := parser.NewRegistry()
	reg.Register(ext)
	// A real sqlite store, so idx.search is the production
	// SymbolSearcherBackend over the store's native symbol FTS: the
	// "search entry survived / was evicted" assertions below then pin the
	// corpus the daemon actually serves. The store rides out so a test can
	// gate on a non-empty corpus before trusting any of them.
	store := newFTSStore(t)
	idx := New(store, reg, config.IndexConfig{Workers: 1}, zap.NewNop())
	return idx, ext, store
}

// tier0Dodge is appended to every searchHasID query. The sqlite backend
// short-circuits an identifier-shaped query on an exact FindNodesByName hit
// and returns before the ranked FTS tier runs (store_fts.go tier 0), so
// asking for "Alpha" would be answered straight out of the graph — these
// assertions would then restate the GetNode check on the line above them and
// prove nothing about the search corpus. A second whitespace-separated token
// makes the query non-identifier-shaped. It matches no document and the MATCH
// expression is a prefix-OR, so the real term still decides the answer.
const tier0Dodge = " zznosuchtokenzz"

func searchHasID(idx *Indexer, query, id string) bool {
	for _, r := range idx.search.Search(query+tier0Dodge, 50) {
		if r.ID == id {
			return true
		}
	}
	return false
}

// TestIndexFile_ParseFailureKeepsPriorNodes is the central proof of the
// parse-then-swap fix: re-indexing a file whose new bytes are
// unparseable must NOT zero the file's prior nodes / edges / search
// entries. Stale-but-present beats empty. A clean re-index then swaps
// them as normal.
//
// Against the pre-fix evict-first code this test FAILS: indexFile
// evicted the graph + search entries before parsing and returned early
// on result == nil, leaving the file at zero nodes.
func TestIndexFile_ParseFailureKeepsPriorNodes(t *testing.T) {
	idx, ext, store := newToggleIndexer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "main.fk")
	idx.SetRootPath(dir)

	// First index, good mode — one function lands in graph + search.
	ext.setFail(false)
	ext.setFuncs("Alpha")
	writeFile(t, path, "alpha body")
	require.NoError(t, idx.IndexFile(path))

	funcID := "main.fk::Alpha"
	require.NotNil(t, idx.graph.GetNode(funcID), "Alpha must be indexed before the bad edit")
	requireSymbolFTS(t, store)
	require.True(t, searchHasID(idx, "Alpha", funcID), "Alpha must be in the search index before the bad edit")
	nodesBefore := len(idx.graph.GetFileNodes("main.fk"))
	require.Equal(t, 2, nodesBefore, "file node + Alpha")

	// Save a transiently unparseable edit. extractFile returns
	// (nil, err); indexFile must NOT evict.
	ext.setFail(true)
	writeFile(t, path, "this no longer parses")
	require.Error(t, idx.IndexFile(path),
		"a failed parse should surface the extractor error")

	// The prior state survives, untouched.
	assert.Equal(t, nodesBefore, len(idx.graph.GetFileNodes("main.fk")),
		"a failed re-index must leave the file's prior nodes intact, not zero them")
	assert.NotNil(t, idx.graph.GetNode(funcID),
		"Alpha must still exist after the failed re-index")
	assert.True(t, searchHasID(idx, "Alpha", funcID),
		"Alpha must still be in the search index after the failed re-index")

	// A subsequent valid re-index swaps cleanly: Alpha gone, Beta in.
	ext.setFail(false)
	ext.setFuncs("Beta")
	writeFile(t, path, "beta body")
	require.NoError(t, idx.IndexFile(path))

	assert.Nil(t, idx.graph.GetNode(funcID),
		"a successful re-index must evict the old Alpha node")
	assert.False(t, searchHasID(idx, "Alpha", funcID),
		"a successful re-index must remove Alpha's stale search entry")
	betaID := "main.fk::Beta"
	assert.NotNil(t, idx.graph.GetNode(betaID), "Beta must be indexed by the clean swap")
	assert.True(t, searchHasID(idx, "Beta", betaID), "Beta must be in the search index after the clean swap")
}

// TestPatchGraphModify_ParseFailureKeepsPriorNodes proves the LIVE
// watcher path is parse-safe, not just indexFile in isolation. The
// editor-save path goes Watcher event -> patchGraph(ChangeModified),
// which used to call EvictFile BEFORE IndexFile — so a transiently
// unparseable save dropped the file's nodes even with indexFile itself
// fixed. With the pre-evict removed, a failed modify through patchGraph
// must leave the file's prior nodes / search entries intact, and a clean
// modify must still swap. Against the pre-fix patchGraph this FAILS.
func TestPatchGraphModify_ParseFailureKeepsPriorNodes(t *testing.T) {
	idx, ext, store := newToggleIndexer(t)
	dir := t.TempDir()
	idx.SetRootPath(dir)
	path := filepath.Join(dir, "main.fk")

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)

	// Initial index through the live create patch.
	ext.setFail(false)
	ext.setFuncs("Alpha")
	writeFile(t, path, "alpha body")
	require.NoError(t, w.patchGraph(path, ChangeCreated))

	funcID := "main.fk::Alpha"
	require.NotNil(t, idx.graph.GetNode(funcID), "Alpha must be indexed via the create patch")
	requireSymbolFTS(t, store)
	require.True(t, searchHasID(idx, "Alpha", funcID))
	nodesBefore := len(idx.graph.GetFileNodes("main.fk"))
	require.Equal(t, 2, nodesBefore, "file node + Alpha")

	// A transiently-unparseable save arrives as a Modify on the live path.
	// patchGraph must surface the parse failure to its caller — while the
	// assertions below pin that the graph kept the prior nodes.
	ext.setFail(true)
	writeFile(t, path, "this no longer parses")
	require.Error(t, w.patchGraph(path, ChangeModified))

	assert.Equal(t, nodesBefore, len(idx.graph.GetFileNodes("main.fk")),
		"a failed modify through the live watcher path must not zero the file's nodes")
	assert.NotNil(t, idx.graph.GetNode(funcID), "Alpha must survive the failed live modify")
	assert.True(t, searchHasID(idx, "Alpha", funcID), "Alpha's search entry must survive the failed live modify")

	// A clean modify swaps cleanly.
	ext.setFail(false)
	ext.setFuncs("Beta")
	writeFile(t, path, "beta body")
	_ = w.patchGraph(path, ChangeModified)
	assert.Nil(t, idx.graph.GetNode(funcID), "a clean live modify evicts Alpha")
	assert.NotNil(t, idx.graph.GetNode("main.fk::Beta"), "a clean live modify indexes Beta")
}

// TestPollGitHead_DiffFailureRetriesRange proves the lastSHA-advance
// fix: when the git diff for the moved range errors, pollGitHead must
// leave lastSHA at the old SHA so the next cycle retries the same
// (un-reconciled) range. Advancing it on failure would permanently skip
// that span.
//
// We force the diff failure by seeding lastSHA with a bogus SHA — `git
// diff <bogus>..HEAD` errors with "unknown revision". The fix then
// requires lastSHA to stay bogus across the failing poll, and the range
// to reconcile once lastSHA is a real prior commit.
func TestPollGitHead_DiffFailureRetriesRange(t *testing.T) {
	if !haveGit(t) {
		t.Skip("git binary not available in PATH")
	}
	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "-q", "-b", "main")
	runGit(t, repoDir, "config", "user.email", "test@example.com")
	runGit(t, repoDir, "config", "user.name", "Test")
	runGit(t, repoDir, "config", "commit.gpgsign", "false")

	writeFile(t, filepath.Join(repoDir, "a.go"), "package main\nfunc Alpha() {}\n")
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-q", "-m", "main: Alpha")
	firstSHA, err := pollerHeadSHA(repoDir)
	require.NoError(t, err)

	writeFile(t, filepath.Join(repoDir, "b.go"), "package main\nfunc Beta() {}\n")
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-q", "-m", "main: Beta")

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(repoDir)
	_, err = idx.IndexCtx(testCtx(), repoDir)
	require.NoError(t, err)
	require.NotEmpty(t, g.GetFileNodes("a.go"))
	require.NotEmpty(t, g.GetFileNodes("b.go"))

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)
	p := newPoller(w, idx, zap.NewNop())

	// Seed lastSHA with a SHA git can't resolve — the diff for this
	// cycle's range will error.
	const bogus = "0000000000000000000000000000000000000000"
	p.mu.Lock()
	p.lastSHA = bogus
	p.mu.Unlock()

	// Failing cycle: diff errors, so lastSHA must NOT advance.
	require.False(t, p.pollGitHead(), "a failed diff reports no reconcile")
	p.mu.Lock()
	stuck := p.lastSHA
	p.mu.Unlock()
	require.Equal(t, bogus, stuck,
		"a failed git diff must leave lastSHA at the old SHA so the range is retried, not skipped")

	// Now point lastSHA at a real prior commit; the retry reconciles
	// the same HEAD range that the failure left un-reconciled.
	p.mu.Lock()
	p.lastSHA = firstSHA
	p.mu.Unlock()
	require.True(t, p.pollGitHead(), "the retry must reconcile the previously-failed range")
	head, err := pollerHeadSHA(repoDir)
	require.NoError(t, err)
	p.mu.Lock()
	settled := p.lastSHA
	p.mu.Unlock()
	assert.Equal(t, head, settled, "a successful diff advances lastSHA to HEAD")
}

func haveGit(t *testing.T) bool {
	t.Helper()
	_, err := exec.LookPath("git")
	return err == nil
}

// TestWatcher_OverflowEventTriggersReconcile proves the kernel-overflow
// gap is closed: a pathless EventOverflow on the Events channel triggers
// a coalesced full-tree reconcile (the signal the Linux inotify backend
// raises when its queue overflows and events are lost). The reconcileFn
// seam stands in for full-tree reconciliation so the assertion is
// deterministic and platform-independent.
func TestWatcher_OverflowEventTriggersReconcile(t *testing.T) {
	idx, _, _ := newToggleIndexer(t)
	idx.SetRootPath(t.TempDir())
	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)

	var calls int32
	done := make(chan struct{}, 1)
	w.reconcileMu.Lock()
	w.reconcileFn = func() {
		atomic.AddInt32(&calls, 1)
		select {
		case done <- struct{}{}:
		default:
		}
	}
	w.reconcileMu.Unlock()

	w.handleEvent(fswatcher.WatchEvent{Types: []fswatcher.EventType{fswatcher.EventOverflow}})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("overflow event did not trigger a reconcile")
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "exactly one reconcile from one overflow")
}

// TestWatcher_OverflowReconcileCoalesces proves a burst of overflow
// signals collapses into at most one reconcile in flight — the loop is
// never blocked and the tree isn't re-walked per dropped event.
func TestWatcher_OverflowReconcileCoalesces(t *testing.T) {
	idx, _, _ := newToggleIndexer(t)
	idx.SetRootPath(t.TempDir())
	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)

	var calls int32
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	w.reconcileMu.Lock()
	w.reconcileFn = func() {
		atomic.AddInt32(&calls, 1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release // hold the reconcile "in flight"
	}
	w.reconcileMu.Unlock()

	// First signal starts the (blocked) reconcile.
	w.triggerOverflowReconcile("queue-overflow")
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first reconcile never started")
	}

	// A burst while one is in flight must be coalesced away.
	for i := 0; i < 25; i++ {
		w.triggerOverflowReconcile("queue-overflow")
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls),
		"overflow signals during an in-flight reconcile must coalesce to one")

	close(release)
	// Once the in-flight reconcile drains, a fresh signal runs again.
	require.Eventually(t, func() bool {
		w.reconcileMu.Lock()
		pending := w.reconcilePending
		w.reconcileMu.Unlock()
		return !pending
	}, 2*time.Second, 5*time.Millisecond, "reconcilePending must clear after the reconcile finishes")
}

// TestWatcher_OverflowReconcileIndexesMissedFile is the end-to-end proof
// that the real IncrementalReindexPaths reconcile recovers a file
// whose create/modify event was lost. We index a tree, drop a brand-new
// file on disk (simulating a missed inotify create), then drive an
// overflow through the real reconcile and assert the new file is now in
// the graph.
func TestWatcher_OverflowReconcileIndexesMissedFile(t *testing.T) {
	dir := t.TempDir()
	ext := &toggleExtractor{}
	reg := parser.NewRegistry()
	reg.Register(ext)
	g := graph.New()
	idx := New(g, reg, config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(dir)

	ext.setFail(false)
	ext.setFuncs("Seed")
	writeFile(t, filepath.Join(dir, "seed.fk"), "seed body")
	_, err := idx.IndexCtx(testCtx(), dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.GetFileNodes("seed.fk"))

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)

	// A new file appears on disk but its create event was "lost".
	ext.setFuncs("Recovered")
	writeFile(t, filepath.Join(dir, "missed.fk"), "recovered body")
	require.Empty(t, g.GetFileNodes("missed.fk"), "missed file must be absent before the reconcile")

	// Drive the real full-tree reconciliation through the overflow path, with
	// a thin wrapper only to know when it finishes.
	done := make(chan struct{}, 1)
	w.reconcileMu.Lock()
	w.reconcileFn = func() {
		_, rerr := idx.IncrementalReindexPaths(dir, nil)
		require.NoError(t, rerr)
		done <- struct{}{}
	}
	w.reconcileMu.Unlock()

	w.handleEvent(fswatcher.WatchEvent{Types: []fswatcher.EventType{fswatcher.EventOverflow}})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("overflow reconcile never ran")
	}

	assert.NotEmpty(t, g.GetFileNodes("missed.fk"),
		"the overflow-driven reconcile must index the previously-missed file")
}

// TestWatcher_NewSubdirScanIndexesPreWatchFile proves the new-subdir
// race is closed: a file written into a freshly-created directory before
// its watch attaches (so its own create event is never delivered) is
// still indexed, because the directory's create event triggers a scoped
// subtree scan. We drive the real path — handleEvent -> enqueueDirScan
// -> runDirScan -> IncrementalReindexPaths, no seam — and assert the
// pre-watch file lands in the graph.
func TestWatcher_NewSubdirScanIndexesPreWatchFile(t *testing.T) {
	dir := t.TempDir()
	ext := &toggleExtractor{}
	reg := parser.NewRegistry()
	reg.Register(ext)
	g := graph.New()
	idx := New(g, reg, config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(dir)

	ext.setFail(false)
	ext.setFuncs("Seed")
	writeFile(t, filepath.Join(dir, "seed.fk"), "seed body")
	_, err := idx.IndexCtx(testCtx(), dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.GetFileNodes("seed.fk"))

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)

	// A new subdirectory appears with a file already inside it; the
	// file's own create event was lost (it landed before the watch on
	// the new directory attached), so only the directory create arrives.
	subdir := filepath.Join(dir, "pkg")
	require.NoError(t, os.MkdirAll(subdir, 0o755))
	ext.setFuncs("Buried")
	writeFile(t, filepath.Join(subdir, "buried.fk"), "buried body")
	// The graph keys file nodes under OS-native separators (see
	// graphRelKey), so build the expected key with filepath.Join instead
	// of a hard-coded "pkg/buried.fk" — the slash form only matches on
	// POSIX and would spuriously fail this test on Windows.
	buriedKey := filepath.Join("pkg", "buried.fk")
	require.Empty(t, g.GetFileNodes(buriedKey),
		"the pre-watch file must be absent before the directory scan")

	w.handleEvent(fswatcher.WatchEvent{
		Path:  subdir,
		Types: []fswatcher.EventType{fswatcher.EventCreate},
	})

	require.Eventually(t, func() bool {
		return len(g.GetFileNodes(buriedKey)) > 0
	}, 5*time.Second, 10*time.Millisecond,
		"the new-directory create must trigger a scoped scan that indexes the pre-watch file")
}

// TestWatcher_DirEventScanGating proves the scan trigger is gated on a
// Create: a directory create enqueues a scoped scan, while a bare
// directory modify (an mtime bump with no Create) does not — entry
// changes inside an existing directory fire their own file events. Uses
// the scanFn seam.
func TestWatcher_DirEventScanGating(t *testing.T) {
	idx, _, _ := newToggleIndexer(t)
	dir := t.TempDir()
	idx.SetRootPath(dir)
	subdir := filepath.Join(dir, "sub")
	require.NoError(t, os.MkdirAll(subdir, 0o755))

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)

	scanned := make(chan map[string]struct{}, 4)
	w.reconcileMu.Lock()
	w.scanFn = func(dirs map[string]struct{}) { scanned <- dirs }
	w.reconcileMu.Unlock()

	// A bare modify on the directory must NOT enqueue a scan.
	w.handleEvent(fswatcher.WatchEvent{
		Path:  subdir,
		Types: []fswatcher.EventType{fswatcher.EventMod},
	})
	select {
	case <-scanned:
		t.Fatal("a directory modify without a Create must not trigger a scan")
	case <-time.After(150 * time.Millisecond):
	}

	// A create on the directory must enqueue a scoped scan of it.
	w.handleEvent(fswatcher.WatchEvent{
		Path:  subdir,
		Types: []fswatcher.EventType{fswatcher.EventCreate},
	})
	select {
	case dirs := <-scanned:
		_, ok := dirs[subdir]
		assert.True(t, ok, "the scan set must contain the newly-created directory")
	case <-time.After(2 * time.Second):
		t.Fatal("a directory create must trigger a scoped scan")
	}
}

// panicOnReadStore wraps a real Store but emits a typed storage panic from
// GetFileNodes once armed — the exact operational shape panicOnFatal exposes
// at legacy Store boundaries.
type panicOnReadStore struct {
	graph.Store
	armed      atomic.Bool
	panicValue any
}

func (s *panicOnReadStore) GetFileNodes(p string) []*graph.Node {
	if s.armed.Load() {
		panic(s.panicValue)
	}
	return s.Store.GetFileNodes(p)
}

// TestWatcher_PatchStoragePanicRecoveredNotCrash proves the watcher panic
// firewall: a typed storage error during a debounced patch is recovered
// and logged, not propagated out of the timer goroutine to crash the
// whole daemon. The fsnotify-driven goroutines don't route through the
// MCP wrapToolHandler firewall, so a closed/locked DB during a restart
// (panicOnFatal) used to take the process down — the exact shape of the
// observed crash. Arbitrary panics are covered separately and still escape.
func TestWatcher_PatchStoragePanicRecoveredNotCrash(t *testing.T) {
	ext := &toggleExtractor{}
	reg := parser.NewRegistry()
	reg.Register(ext)
	_, storageErr := indexCtxRawStorageError(t)
	store := &panicOnReadStore{Store: graph.New(), panicValue: storageErr}
	idx := New(store, reg, config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	dir := t.TempDir()
	idx.SetRootPath(dir)
	path := filepath.Join(dir, "main.fk")

	ext.setFail(false)
	ext.setFuncs("Alpha")
	writeFile(t, path, "alpha body")
	require.NoError(t, idx.IndexFile(path))

	core, logs := observer.New(zapcore.ErrorLevel)
	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.New(core))
	require.NoError(t, err)

	// Arm the store so the next read panics, then drive the debounced
	// patch path. The panic fires in the AfterFunc goroutine.
	store.armed.Store(true)
	w.handleEvent(fswatcher.WatchEvent{
		Path:  path,
		Types: []fswatcher.EventType{fswatcher.EventMod},
	})

	require.Eventually(t, func() bool {
		return logs.FilterMessageSnippet("storage failure in background re-index").Len() > 0
	}, 2*time.Second, 10*time.Millisecond,
		"a storage panic in the debounced patch must be recovered and logged, not crash the daemon")
}
