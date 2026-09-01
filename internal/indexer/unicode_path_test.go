package indexer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/thirdparty/fswatcher"
	"go.uber.org/zap"
	"golang.org/x/text/unicode/norm"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/search"
)

// Non-ASCII filename fixtures. The accented-Latin name "café.go" is the
// one with a genuine NFC/NFD split, so wherever a decomposed form is
// needed the test derives it in code (decompose) rather than typing
// it, guaranteeing the two byte forms differ. CJK
// ideographs and Cyrillic letters have no canonical decomposition —
// they are stable in every normal form — so they are written directly
// (as \u escapes, keeping this source file pure-ASCII).
const (
	cjkFile     = "日本語.go"       // 日本語.go
	cyrFile     = "кириллица.go" // кириллица.go
	accentedDir = "résumé"       // résumé/ — accented directory
	accentFile  = "café.go"      // café.go
)

// decompose returns the NFD (canonically decomposed) form of s — the
// byte form macOS APFS / HFS+ hands back for a filename.
func decompose(s string) string { return norm.NFD.String(s) }

// goSrc returns a tiny compilable Go file declaring one uniquely-named
// function, so a test can assert the symbol made it into the graph.
func goSrc(funcName string) string {
	return "package main\n\nfunc " + funcName + "() {}\n"
}

// fileKindNodes returns only the file-kind nodes the graph holds for
// the given key — used to detect a duplicate file-node leaking after a
// re-index.
func fileKindNodes(g graph.Store, key string) []*graph.Node {
	var out []*graph.Node
	for _, n := range g.GetFileNodes(key) {
		if n.Kind == graph.KindFile {
			out = append(out, n)
		}
	}
	return out
}

// fakeModify / fakeDelete build minimal fswatcher events for driving
// handleEvent directly, mirroring fakeCreate in storm_test.go.
func fakeModify(path string) fswatcher.WatchEvent {
	return fswatcher.WatchEvent{
		Path:  path,
		Types: []fswatcher.EventType{fswatcher.EventMod},
		Time:  time.Now(),
	}
}

func fakeDelete(path string) fswatcher.WatchEvent {
	return fswatcher.WatchEvent{
		Path:  path,
		Types: []fswatcher.EventType{fswatcher.EventRemove},
		Time:  time.Now(),
	}
}

// TestDiscovery_IndexesNonASCIIFilenames proves the bulk index walk
// discovers and indexes files whose names use CJK, Cyrillic, and
// accented-Latin characters, including a file inside an accented
// directory. Each file's symbol must land in the graph.
func TestDiscovery_IndexesNonASCIIFilenames(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, cjkFile), goSrc("CJKFunc"))
	writeFile(t, filepath.Join(dir, cyrFile), goSrc("CyrFunc"))
	writeFile(t, filepath.Join(dir, accentFile), goSrc("AccentFunc"))

	subDir := filepath.Join(dir, accentedDir)
	require.NoError(t, os.MkdirAll(subDir, 0o755))
	writeFile(t, filepath.Join(subDir, "nested.go"), goSrc("NestedFunc"))

	g := graph.New()
	idx := newTestIndexer(g)
	result, err := idx.Index(dir)
	require.NoError(t, err)

	assert.Equal(t, 4, result.FileCount, "all four non-ASCII-path files must be discovered")
	for _, fn := range []string{"CJKFunc", "CyrFunc", "AccentFunc", "NestedFunc"} {
		assert.NotEmptyf(t, g.FindNodesByName(fn), "%s must be indexed", fn)
	}
}

// TestDiscovery_NonASCIIFileNodesKeyedCanonically checks that a file
// with a non-ASCII name is reachable through GetFileNodes under the
// canonical (NFC, slash) key — i.e. the key relKey produces. This is
// the contract every other subsystem depends on: if the graph keys a
// non-ASCII file under a non-canonical form, the watcher and the
// incremental reconcile cannot find it.
func TestDiscovery_NonASCIIFileNodesKeyedCanonically(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, cjkFile), goSrc("CJKFunc"))

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	canonicalKey := idx.RelKey(filepath.Join(dir, cjkFile))
	require.Equal(t, pathkey.Normalize(cjkFile), canonicalKey,
		"relKey must yield the NFC slash-form of the filename")

	nodes := g.GetFileNodes(canonicalKey)
	assert.NotEmpty(t, nodes, "the CJK file's nodes must be reachable under the canonical key")
}

// TestRelKey_NFCvsNFDConverge is the heart of the cross-platform fix:
// the same file path handed to the indexer in decomposed (macOS) and
// precomposed (git / Linux) Unicode forms must reduce to one graph
// key. Without the NFC fold in relKey the two forms diverge and a
// single file splits across two keys.
func TestRelKey_NFCvsNFDConverge(t *testing.T) {
	dir := t.TempDir()
	idx := newTestIndexer(graph.New())
	idx.SetRootPath(dir)

	nfc := pathkey.Normalize(accentFile) // precomposed
	nfd := decompose(accentFile)         // "e" + combining acute
	require.NotEqual(t, nfc, nfd, "fixture invalid: NFC and NFD forms identical")

	keyNFC := idx.RelKey(filepath.Join(dir, nfc))
	keyNFD := idx.RelKey(filepath.Join(dir, nfd))
	assert.Equal(t, keyNFC, keyNFD,
		"relKey must fold NFC and NFD spellings of one filename to the same key")
}

// TestIncrementalReindex_NonASCIIFileNoDuplicate is the regression
// test for the duplicate-node bug. A file with a non-ASCII name is
// indexed, then modified and re-indexed incrementally. The graph must
// hold exactly one file-node for it afterwards — if the incremental
// path keyed the file under a different Unicode form than the bulk
// walk, a stale duplicate would survive alongside the fresh node.
func TestIncrementalReindex_NonASCIIFileNoDuplicate(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, cjkFile)
	writeFile(t, target, goSrc("Before"))

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	key := idx.RelKey(target)
	fileNodesBefore := fileKindNodes(g, key)
	require.Len(t, fileNodesBefore, 1, "exactly one file-node after the initial index")
	require.NotEmpty(t, g.FindNodesByName("Before"))

	// Modify and re-index incrementally.
	bumpMtime(t, target, goSrc("After"))
	_, err = idx.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)

	fileNodesAfter := fileKindNodes(g, key)
	assert.Len(t, fileNodesAfter, 1,
		"incremental re-index of a non-ASCII file must not leave a duplicate file-node")
	assert.NotEmpty(t, g.FindNodesByName("After"), "the new symbol must be indexed")
	assert.Empty(t, g.FindNodesByName("Before"), "the old symbol must be evicted")
}

// TestIncrementalReindex_NonASCIIFileNotSpuriouslyDeleted proves an
// unchanged non-ASCII file survives an incremental reconcile. The
// deletion-detection pass intersects the freshly-walked disk set
// against the mtime map; if the two were keyed in different Unicode
// forms, the file would be misclassified as deleted and purged.
func TestIncrementalReindex_NonASCIIFileNotSpuriouslyDeleted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, cjkFile), goSrc("Survivor"))
	writeFile(t, filepath.Join(dir, "ascii.go"), goSrc("AsciiSurvivor"))

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("Survivor"))

	// Re-index with nothing changed on disk.
	_, err = idx.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)

	assert.NotEmpty(t, g.FindNodesByName("Survivor"),
		"an untouched non-ASCII file must not be evicted by deletion detection")
	assert.NotEmpty(t, g.FindNodesByName("AsciiSurvivor"))
}

// TestEvictFile_NonASCIIFormMismatch is the direct regression test for
// the git-watcher path. The git watcher derives paths from `git diff`
// output (NFC) while the bulk filesystem walk on macOS stores NFD. An
// EvictFile call made with one Unicode form must still evict a file
// the graph indexed under the other — otherwise a branch-switch
// change to a non-ASCII file silently leaves a stale subtree.
func TestEvictFile_NonASCIIFormMismatch(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, accentFile)
	writeFile(t, target, goSrc("DoomedFunc"))

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("DoomedFunc"))

	// Evict using an explicitly decomposed (NFD) spelling of the path.
	// The graph keyed the file under the canonical NFC form; without
	// the fold in EvictFile this call would no-op and leak the nodes.
	nfdPath := filepath.Join(dir, decompose(accentFile))
	require.NotEqual(t, filepath.Join(dir, pathkey.Normalize(accentFile)), nfdPath,
		"fixture invalid: NFC and NFD path spellings identical")

	nodesRemoved, _ := idx.EvictFile(nfdPath)
	assert.Positive(t, nodesRemoved,
		"EvictFile must remove nodes even when handed a different Unicode form than the walk used")
	assert.Empty(t, g.FindNodesByName("DoomedFunc"),
		"the file's symbol must be gone after the cross-form evict")
}

// TestWatcher_NonASCIIFileModify drives the watcher's event path with
// a non-ASCII-named file and asserts the modify is routed to the right
// graph node. handleEvent is exercised directly (no real fsnotify
// backend) for determinism, mirroring the storm-mode unit tests.
func TestWatcher_NonASCIIFileModify(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, cyrFile)
	writeFile(t, target, goSrc("OriginalCyr"))

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.SetRootPath(dir)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("OriginalCyr"))

	w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)

	// Modify on disk, then deliver a synthetic modify event.
	writeFile(t, target, goSrc("ModifiedCyr"))
	w.handleEvent(fakeModify(target))

	ev := waitForEvent(t, w, 2*time.Second)
	assert.Equal(t, ChangeModified, ev.Kind)
	assert.NotEmpty(t, g.FindNodesByName("ModifiedCyr"),
		"a modify of a non-ASCII-named file must reach the graph")
	assert.Empty(t, g.FindNodesByName("OriginalCyr"))
}

// TestWatcher_NonASCIIFileDelete proves a delete event for a non-ASCII
// file evicts the right node — the watcher derives the relative key
// from the event path, which must fold to the graph's stored key.
func TestWatcher_NonASCIIFileDelete(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, cjkFile)
	writeFile(t, target, goSrc("DeleteMe"))

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.SetRootPath(dir)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("DeleteMe"))

	require.NoError(t, os.Remove(target))
	w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)
	w.handleEvent(fakeDelete(target))

	ev := waitForEvent(t, w, 2*time.Second)
	assert.Equal(t, ChangeDeleted, ev.Kind)
	assert.Empty(t, g.FindNodesByName("DeleteMe"),
		"a delete of a non-ASCII-named file must evict its node")
}

// TestWatcher_NonASCIIEventPathCanonicalised checks the watcher's path
// normalisation: an event path delivered in decomposed (NFD) form —
// the form macOS FSEvents produces — must be folded so a downstream
// modify lands on the NFC-keyed node rather than missing it.
func TestWatcher_NonASCIIEventPathCanonicalised(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, accentFile)
	writeFile(t, target, goSrc("EventOriginal"))

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.SetRootPath(dir)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("EventOriginal"))

	w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)

	// Deliver the event under an explicitly decomposed path. The
	// graph node was keyed NFC by the index walk; the watcher must
	// reconcile the form before dispatching.
	writeFile(t, target, goSrc("EventModified"))
	nfdEventPath := filepath.Join(dir, decompose(accentFile))
	require.NotEqual(t, target, nfdEventPath, "fixture invalid: NFC and NFD paths identical")
	w.handleEvent(fakeModify(nfdEventPath))

	ev := waitForEvent(t, w, 2*time.Second)
	assert.Equal(t, ChangeModified, ev.Kind)
	assert.NotEmpty(t, g.FindNodesByName("EventModified"),
		"an NFD-form event path must still route the change to the NFC-keyed node")
	assert.Empty(t, g.FindNodesByName("EventOriginal"))
}

// TestGitWatcher_NonASCIIFileBranchSwitch is the end-to-end proof for
// the git watcher: a real repo whose feature branch adds a CJK-named
// file must, on checkout, have that file's symbol indexed. git emits
// the path in NFC; the reconcile must key it consistently with the
// rest of the graph.
func TestGitWatcher_NonASCIIFileBranchSwitch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}

	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "-q", "-b", "main")
	runGit(t, repoDir, "config", "user.email", "test@example.com")
	runGit(t, repoDir, "config", "user.name", "Test")
	runGit(t, repoDir, "config", "commit.gpgsign", "false")

	// main: an ASCII file only.
	writeFile(t, filepath.Join(repoDir, "main.go"), goSrc("MainFunc"))
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-q", "-m", "main: MainFunc")

	// feature: adds a CJK-named file.
	runGit(t, repoDir, "checkout", "-q", "-b", "feature")
	writeFile(t, filepath.Join(repoDir, cjkFile), goSrc("FeatureFunc"))
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "commit", "-q", "-m", "feature: add CJK-named file")
	runGit(t, repoDir, "checkout", "-q", "main")

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(repoDir)
	_, err := idx.IndexCtx(testCtx(), repoDir)
	require.NoError(t, err)
	require.Empty(t, g.FindNodesByName("FeatureFunc"), "main branch lacks the CJK file")

	gw, err := NewGitWatcher(repoDir, idx, zap.NewNop())
	require.NoError(t, err)
	gw.debounce = 50 * time.Millisecond
	drained := make(chan int, 1)
	gw.drained = func(n int) {
		select {
		case drained <- n:
		default:
		}
	}
	require.NoError(t, gw.Start())
	t.Cleanup(func() { _ = gw.Stop() })

	runGit(t, repoDir, "checkout", "-q", "feature")

	select {
	case n := <-drained:
		assert.GreaterOrEqual(t, n, 1, "the reconcile must touch the CJK file")
	case <-time.After(5 * time.Second):
		t.Fatal("git watcher did not reconcile the branch switch within timeout")
	}

	assert.NotEmpty(t, g.FindNodesByName("FeatureFunc"),
		"after checkout, the CJK-named file's symbol must be indexed")
	canonicalKey := idx.RelKey(filepath.Join(repoDir, cjkFile))
	assert.NotEmpty(t, g.GetFileNodes(canonicalKey),
		"the CJK file's nodes must be reachable under the canonical key")
	require.False(t, strings.Contains(canonicalKey, "\\"), "graph key must use forward slashes")
}
