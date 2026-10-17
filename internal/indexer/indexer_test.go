package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func setupTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// main.go
	writeFile(t, filepath.Join(dir, "main.go"), `package main

import "fmt"

func main() {
	fmt.Println("hello")
	helper()
}

func helper() {}
`)

	// pkg/util.go
	pkgDir := filepath.Join(dir, "pkg")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))
	writeFile(t, filepath.Join(pkgDir, "util.go"), `package pkg

type Config struct {
	Port int
}

func NewConfig() *Config {
	return &Config{Port: 8080}
}
`)

	// vendor/ should be excluded.
	vendorDir := filepath.Join(dir, "vendor")
	require.NoError(t, os.MkdirAll(vendorDir, 0o755))
	writeFile(t, filepath.Join(vendorDir, "lib.go"), `package vendor

func Ignored() {}
`)

	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func newTestIndexer(g graph.Store) *Indexer {
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cfg := config.Default().Index
	cfg.Workers = 2
	return New(g, reg, cfg, zap.NewNop())
}

// newFTSStore opens a throwaway sqlite store — the only Store that carries a
// native symbol FTS, and the one production always runs on.
func newFTSStore(t *testing.T) *store_sqlite.Store {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "fts.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newFTSIndexer indexes dir into a real sqlite store and returns both. It is
// the fixture for every assertion about what indexing makes SEARCHABLE:
// production always runs on a store whose native FTS answers symbol search, so
// idx.Search() here is the SymbolSearcherBackend reading store.SearchSymbols
// rather than an in-process index the daemon never builds.
//
// prepare runs on the indexer before Index, for the SetEmbedder /
// SetEmbeddingChunkOptions wiring some fixtures need.
func newFTSIndexer(
	t *testing.T,
	dir string,
	reg *parser.Registry,
	cfg config.IndexConfig,
	prepare ...func(*Indexer),
) (*Indexer, *store_sqlite.Store) {
	t.Helper()
	store := newFTSStore(t)
	idx := New(store, reg, cfg, zap.NewNop())
	for _, p := range prepare {
		p(idx)
	}
	_, err := idx.Index(dir)
	require.NoError(t, err)
	requireSymbolFTS(t, store)
	return idx, store
}

// requireSymbolFTS fails unless the store's symbol corpus actually holds
// something. An empty corpus satisfies every "must NOT be searchable"
// assertion for the wrong reason and turns the whole fixture into a silent
// pass, so no search test may run without this gate.
func requireSymbolFTS(t *testing.T, store *store_sqlite.Store) {
	t.Helper()
	count, err := store.SymbolFTSCount()
	require.NoError(t, err)
	require.Greater(t, count, 0, "fixture left the symbol FTS empty — search assertions would be vacuous")
}

// newTestIndexerGoJava registers both the Go and Java extractors — used by
// the cross-language Temporal join tests.
func newTestIndexerGoJava(g graph.Store) *Indexer {
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	reg.Register(languages.NewJavaExtractor())
	cfg := config.Default().Index
	cfg.Workers = 2
	return New(g, reg, cfg, zap.NewNop())
}

func TestIndex_SingleGoFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), `package main

func Hello() {}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	result, err := idx.Index(dir)
	require.NoError(t, err)

	assert.Equal(t, 1, result.FileCount)
	assert.Greater(t, result.NodeCount, 0)
	assert.Greater(t, result.EdgeCount, 0)
}

// TestIndexCtx_FlagsFullReindexForEnrichForce locks the wiring the enrichment
// completion marker depends on: a whole-repo IndexCtx re-parse evicts and
// re-creates every node/edge (dropping the repo's LSP hover-enrichment edges),
// so it must flag fullReindexed. runDeferredEnrich threads that into
// RepoEnrichState.Force to re-run enrichment past the marker gate even at an
// unchanged clean HEAD — without it a full re-track durably drops the repo's
// enrichment edges.
func TestIndexCtx_FlagsFullReindexForEnrichForce(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc Hello() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	require.False(t, idx.fullReindexed.Load(), "a fresh indexer has re-parsed nothing")

	_, err := idx.Index(dir)
	require.NoError(t, err)
	assert.True(t, idx.fullReindexed.Load(),
		"a whole-repo re-parse must flag fullReindexed so deferred enrichment forces past the completion marker")
}

// TestIncrementalReindexPaths_LeavesFullReindexClear is the boundary: a scoped
// incremental re-parse touches only the changed files and leaves every other
// file's enrichment edges intact, so it must NOT force a whole-repo re-enrich
// (which would re-run the multi-minute hover pass on every scoped save). It
// must, however, flag reparsedThisRun — the re-parsed file's own hover edges
// WERE evicted, so the deferred pass still has to run past the completion
// marker, just scoped to that file rather than the whole repo.
func TestIncrementalReindexPaths_LeavesFullReindexClear(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main.go")
	writeFile(t, main, "package main\n\nfunc Hello() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	result, err := idx.IncrementalReindexPaths(dir, []string{main})
	require.NoError(t, err)
	require.Positive(t, result.StaleFileCount, "the fresh file must re-parse")
	assert.False(t, idx.fullReindexed.Load(),
		"a scoped incremental re-parse must leave fullReindexed clear")
	assert.True(t, idx.reparsedThisRun.Load(),
		"a scoped incremental re-parse still evicted the changed file's hover edges, so it must flag reparsedThisRun to force the deferred enrich past the completion marker")
}

// TestIncrementalReindexPaths_MtimeBumpFlagsReparse mirrors the durable-loss
// scenario: after a repo is indexed and its enrichment marker recorded at a
// clean HEAD, a git checkout-and-back / stash-pop / no-op save bumps a file's
// mtime without changing its content or moving HEAD. The explicitly file-scoped
// route re-parses the mtime-drifted file (dropping its hover edges), so it must
// flag reparsedThisRun — otherwise the current completion marker would suppress
// restoration of those file-scoped enrichment edges.
func TestIncrementalReindexPaths_MtimeBumpFlagsReparse(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main.go")
	const content = "package main\n\nfunc Hello() {}\n"
	writeFile(t, main, content)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.False(t, idx.reparsedThisRun.Load(),
		"reparsedThisRun is a scoped-path flag; a full Index uses fullReindexed instead")
	// An index written before whole indexes stamped the extraction
	// fingerprints: the scoped path cannot prove the file inert and re-parses
	// it (TestIncrementalReindexPaths_MtimeBumpAfterWholeIndexIsInert is the
	// stamped case).
	stripFileFingerprints(t, g)

	// Same bytes, later mtime — a touch / checkout-roundtrip, content unchanged.
	bumpMtime(t, main, content)
	result, err := idx.IncrementalReindexPaths(dir, []string{main})
	require.NoError(t, err)
	require.Positive(t, result.StaleFileCount,
		"an explicit mtime-stale file must enter the scoped reparse path")
	assert.True(t, idx.reparsedThisRun.Load(),
		"a scoped re-parse of an mtime-drifted file must force deferred enrichment")
}

// TestIncrementalReindexPaths_WholeRootMtimeBumpFlagsReparse proves the unified
// modern pipeline preserves the enrichment marker contract for nil-path scans:
// an mtime-stale file that is re-parsed must report that work just like an
// explicitly scoped call.
func TestIncrementalReindexPaths_WholeRootMtimeBumpFlagsReparse(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main.go")
	const content = "package main\n\nfunc Hello() {}\n"
	writeFile(t, main, content)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.False(t, idx.reparsedThisRun.Load())
	stripFileFingerprints(t, g)

	bumpMtime(t, main, content)
	result, err := idx.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)
	require.Positive(t, result.StaleFileCount)
	assert.True(t, idx.reparsedThisRun.Load(),
		"a whole-root re-parse must force deferred enrichment for the touched file")
}

// stripFileFingerprints removes the extraction fingerprints from every file
// node, the shape of an index written before whole indexes stamped them.
func stripFileFingerprints(t *testing.T, g *graph.Graph) {
	t.Helper()
	for _, n := range g.AllNodes() {
		if n == nil || n.Kind != graph.KindFile || n.Meta == nil {
			continue
		}
		copied := *n
		copied.Meta = make(map[string]any, len(n.Meta))
		for k, v := range n.Meta {
			if !isFingerprintMeta(k) {
				copied.Meta[k] = v
			}
		}
		g.AddNode(&copied)
	}
}

// TestIncrementalReindexPaths_MtimeBumpAfterWholeIndexIsInert: a whole index
// stamps the extraction fingerprints a per-save compares against, so an
// mtime-only change after it is recognised as inert — nothing is re-parsed,
// nothing enrichment-bearing is dropped, and the flag stays clear.
func TestIncrementalReindexPaths_MtimeBumpAfterWholeIndexIsInert(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main.go")
	const content = "package main\n\nfunc Hello() {}\n"
	writeFile(t, main, content)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	bumpMtime(t, main, content)
	_, err = idx.IncrementalReindexPaths(dir, []string{main})
	require.NoError(t, err)
	assert.False(t, idx.reparsedThisRun.Load(),
		"an mtime-only change after a whole index is inert: no re-parse, no dropped enrichment")
}

// TestIncrementalReindex_NoChangeLeavesReparseClear guards the other side: a
// zero-change reconcile re-parses nothing, so reparsedThisRun stays clear and an
// unchanged repo at a current marker is legitimately skipped.
func TestIncrementalReindex_NoChangeLeavesReparseClear(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc Hello() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	result, err := idx.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)
	require.Zero(t, result.StaleFileCount, "nothing changed on disk")
	assert.False(t, idx.reparsedThisRun.Load(),
		"a zero-change reconcile must not force re-enrichment")
}

func TestIndex_MultipleFiles(t *testing.T) {
	dir := setupTestDir(t)

	g := graph.New()
	idx := newTestIndexer(g)
	result, err := idx.Index(dir)
	require.NoError(t, err)

	assert.Equal(t, 2, result.FileCount) // main.go + pkg/util.go (vendor excluded)
	assert.Greater(t, result.NodeCount, 4)
}

func TestIndex_ExcludePatterns(t *testing.T) {
	dir := setupTestDir(t)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	// Vendor file should not be indexed.
	nodes := g.FindNodesByName("Ignored")
	assert.Empty(t, nodes)
}

func TestIndex_RipgrepIgnoreFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc Kept() {}\n")
	writeFile(t, filepath.Join(dir, "gen.go"), "package main\n\nfunc FromGen() {}\n")
	writeFile(t, filepath.Join(dir, ".ignore"), "gen.go\n")

	// A nested directory carries its own ripgrep ignore file; it must
	// scope to that subtree only.
	sub := filepath.Join(dir, "sub")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	writeFile(t, filepath.Join(sub, "keep.go"), "package sub\n\nfunc SubKept() {}\n")
	writeFile(t, filepath.Join(sub, "drop.go"), "package sub\n\nfunc SubDropped() {}\n")
	writeFile(t, filepath.Join(sub, ".rgignore"), "drop.go\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	assert.NotEmpty(t, g.FindNodesByName("Kept"), "main.go should be indexed")
	assert.NotEmpty(t, g.FindNodesByName("SubKept"), "sub/keep.go should be indexed")
	assert.Empty(t, g.FindNodesByName("FromGen"), ".ignore should exclude gen.go")
	assert.Empty(t, g.FindNodesByName("SubDropped"), "sub/.rgignore should exclude sub/drop.go")
}

func TestEdgeSanityViolated(t *testing.T) {
	// A populated index with no edges trips the check.
	broken := &IndexResult{FileCount: 5, NodeCount: 40, EdgeCount: 0}
	assert.True(t, broken.EdgeSanityViolated(),
		"files+nodes but zero edges should violate the edge-sanity check")

	// A healthy index does not.
	healthy := &IndexResult{FileCount: 5, NodeCount: 40, EdgeCount: 60}
	assert.False(t, healthy.EdgeSanityViolated(),
		"an index with edges must not trip the check")

	// An empty repo (no files) is not a violation — nothing to index.
	empty := &IndexResult{FileCount: 0, NodeCount: 0, EdgeCount: 0}
	assert.False(t, empty.EdgeSanityViolated(),
		"an empty repo must not trip the check")

	// nil is safe.
	var nilResult *IndexResult
	assert.False(t, nilResult.EdgeSanityViolated(), "nil result must not trip the check")
}

func TestGrepText(t *testing.T) {
	dir := setupTestDir(t)
	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	// A literal present in main.go.
	hits := idx.GrepText("func helper", 0)
	require.NotEmpty(t, hits, "func helper should be found")
	found := false
	for _, h := range hits {
		if h.Path == "main.go" {
			found = true
		}
	}
	assert.True(t, found, "func helper should be located in main.go")

	// A literal present nowhere.
	assert.Empty(t, idx.GrepText("nonexistent_zzz_literal", 0))

	// After an incremental reindex the warm cache is rebuilt, so a
	// freshly added literal is visible.
	bumpMtime(t, filepath.Join(dir, "main.go"),
		"package main\n\nfunc main() {}\n\nfunc BrandNew() {}\n")
	_, err = idx.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)
	assert.NotEmpty(t, idx.GrepText("func BrandNew", 0),
		"GrepText must reflect content added by an incremental reindex")
}

func TestIndex_EdgeSanityHolds(t *testing.T) {
	// A real index of even a tiny repo produces edges, so the
	// edge-sanity check passes — guards the invariant against false
	// positives on legitimate indexes.
	dir := setupTestDir(t)
	g := graph.New()
	idx := newTestIndexer(g)
	result, err := idx.Index(dir)
	require.NoError(t, err)
	assert.False(t, result.EdgeSanityViolated(),
		"a normal index must not trip the edge-sanity check (files=%d nodes=%d edges=%d)",
		result.FileCount, result.NodeCount, result.EdgeCount)

	// A graph built only through the indexer's sanctioned mutation
	// paths is provenance-consistent: every edge's out- and in-edge
	// adjacency views agree on its identity hash.
	require.NoError(t, g.VerifyEdgeIdentities(),
		"a normally-indexed graph must have internally consistent edge identities")
}

func TestIndex_UnsupportedFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "readme.txt"), "hello world")

	g := graph.New()
	idx := newTestIndexer(g)
	result, err := idx.Index(dir)
	require.NoError(t, err)

	assert.Equal(t, 0, result.FileCount)
}

func TestIndexFile_SingleFile(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "main.go")
	writeFile(t, filePath, `package main

func Original() {}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	origNodes := g.FindNodesByName("Original")
	require.Len(t, origNodes, 1)

	// Modify and re-index single file.
	writeFile(t, filePath, `package main

func Replaced() {}
`)
	require.NoError(t, idx.IndexFile(filePath))

	assert.Empty(t, g.FindNodesByName("Original"))
	assert.Len(t, g.FindNodesByName("Replaced"), 1)
}

func TestMtimeTracking(t *testing.T) {
	dir := t.TempDir()
	goFile := filepath.Join(dir, "main.go")
	writeFile(t, goFile, `package main

func Hello() {}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	result, err := idx.Index(dir)
	require.NoError(t, err)
	assert.Equal(t, 1, result.FileCount)

	// FileMtimes should be populated with the indexed file.
	mtimes := idx.FileMtimes()
	assert.NotEmpty(t, mtimes, "fileMtimes should be populated after Index()")
	assert.Contains(t, mtimes, "main.go", "fileMtimes should contain the indexed file")
	assert.Greater(t, mtimes["main.go"], int64(0), "mtime should be a positive unix nano value")
}

func TestMtimeIsStale_FreshFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), `package main

func Hello() {}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	// A just-indexed file should not be stale.
	assert.False(t, idx.IsStale("main.go"), "file should not be stale immediately after indexing")
}

func TestMtimeIsStale_ModifiedFile(t *testing.T) {
	dir := t.TempDir()
	goFile := filepath.Join(dir, "main.go")
	writeFile(t, goFile, `package main

func Hello() {}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	// Modify the file after indexing — ensure mtime actually changes.
	// Some filesystems have coarse mtime resolution, so we sleep briefly.
	time.Sleep(50 * time.Millisecond)
	writeFile(t, goFile, `package main

func HelloModified() {}
`)

	assert.True(t, idx.IsStale("main.go"), "file should be stale after modification")
}

func TestMtimeIsStale_UnknownFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), `package main

func Hello() {}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	// A file not in the index should be treated as stale.
	assert.True(t, idx.IsStale("unknown.go"), "unknown file should be treated as stale")
}

func TestMtimeUpdatedAfterIndexFile(t *testing.T) {
	dir := t.TempDir()
	goFile := filepath.Join(dir, "main.go")
	writeFile(t, goFile, `package main

func Original() {}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	originalMtime := idx.FileMtimes()["main.go"]

	// Modify and re-index the single file.
	time.Sleep(50 * time.Millisecond)
	writeFile(t, goFile, `package main

func Replaced() {}
`)
	require.NoError(t, idx.IndexFile(goFile))

	updatedMtime := idx.FileMtimes()["main.go"]
	assert.Greater(t, updatedMtime, originalMtime, "mtime should be updated after IndexFile")
	assert.False(t, idx.IsStale("main.go"), "file should not be stale after re-indexing")
}

func TestEvictFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), `package main

func Foo() {}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("Foo"))

	n, e := idx.EvictFile(filepath.Join(dir, "main.go"))
	assert.Greater(t, n, 0)
	assert.Greater(t, e, 0)
	assert.Empty(t, g.FindNodesByName("Foo"))
}

func TestIndex_WithRepoPrefix(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), `package main

func Hello() {}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRepoPrefix("myrepo")

	result, err := idx.Index(dir)
	require.NoError(t, err)
	assert.Equal(t, 1, result.FileCount)
	assert.Greater(t, result.NodeCount, 0)

	// Verify node IDs are prefixed.
	nodes := g.FindNodesByName("Hello")
	require.Len(t, nodes, 1)
	assert.Equal(t, "myrepo/main.go::Hello", nodes[0].ID)
	assert.Equal(t, "myrepo/main.go", nodes[0].FilePath)
	assert.Equal(t, "myrepo", nodes[0].RepoPrefix)

	// Verify file node is also prefixed.
	fileNodes := g.GetFileNodes("myrepo/main.go")
	assert.NotEmpty(t, fileNodes)

	// Verify repo index is populated.
	repoNodes := g.GetRepoNodes("myrepo")
	assert.NotEmpty(t, repoNodes)
	for _, n := range repoNodes {
		assert.Equal(t, "myrepo", n.RepoPrefix)
	}
}

func TestIndex_WithoutRepoPrefix_BackwardCompatible(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), `package main

func Hello() {}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	// No SetRepoPrefix — single-repo mode.

	result, err := idx.Index(dir)
	require.NoError(t, err)
	assert.Equal(t, 1, result.FileCount)

	nodes := g.FindNodesByName("Hello")
	require.Len(t, nodes, 1)
	assert.Equal(t, "main.go::Hello", nodes[0].ID)
	assert.Equal(t, "main.go", nodes[0].FilePath)
	assert.Equal(t, "", nodes[0].RepoPrefix)
}

func TestIndexFile_WithRepoPrefix(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "main.go")
	writeFile(t, filePath, `package main

func Original() {}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRepoPrefix("myrepo")

	_, err := idx.Index(dir)
	require.NoError(t, err)

	origNodes := g.FindNodesByName("Original")
	require.Len(t, origNodes, 1)
	assert.Equal(t, "myrepo/main.go::Original", origNodes[0].ID)
	assert.Equal(t, "myrepo", origNodes[0].RepoPrefix)

	// Modify and re-index single file.
	writeFile(t, filePath, `package main

func Replaced() {}
`)
	require.NoError(t, idx.IndexFile(filePath))

	assert.Empty(t, g.FindNodesByName("Original"))
	replaced := g.FindNodesByName("Replaced")
	require.Len(t, replaced, 1)
	assert.Equal(t, "myrepo/main.go::Replaced", replaced[0].ID)
	assert.Equal(t, "myrepo", replaced[0].RepoPrefix)
}

func TestEvictFile_WithRepoPrefix(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), `package main

func Foo() {}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRepoPrefix("myrepo")

	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("Foo"))

	n, e := idx.EvictFile(filepath.Join(dir, "main.go"))
	assert.Greater(t, n, 0)
	assert.Greater(t, e, 0)
	assert.Empty(t, g.FindNodesByName("Foo"))
}

func TestRepoPrefix_EdgesArePrefixed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), `package main

func Hello() {}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRepoPrefix("myrepo")

	_, err := idx.Index(dir)
	require.NoError(t, err)

	// All edges should have prefixed From/To and FilePath.
	edges := g.AllEdges()
	for _, e := range edges {
		assert.True(t, strings.HasPrefix(e.From, "myrepo/"),
			"edge From should be prefixed: %s", e.From)
		assert.True(t, strings.HasPrefix(e.To, "myrepo/"),
			"edge To should be prefixed: %s", e.To)
		assert.True(t, strings.HasPrefix(e.FilePath, "myrepo/"),
			"edge FilePath should be prefixed: %s", e.FilePath)
	}
}

func TestRepoPrefix_SetterGetter(t *testing.T) {
	g := graph.New()
	idx := newTestIndexer(g)

	assert.Equal(t, "", idx.RepoPrefix())
	idx.SetRepoPrefix("myrepo")
	assert.Equal(t, "myrepo", idx.RepoPrefix())
}
