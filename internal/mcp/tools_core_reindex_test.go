package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// bumpFile rewrites a file and pushes its mtime forward so the
// mtime-keyed staleness check always classifies it as changed
// regardless of filesystem timestamp resolution.
func bumpFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(path, future, future))
}

// reindexResult is the structured payload handleReindexRepository
// returns. Only the fields the tests assert on are decoded.
type reindexResult struct {
	Scope          string   `json:"scope"`
	NodeCount      int      `json:"node_count"`
	EdgeCount      int      `json:"edge_count"`
	FileCount      int      `json:"file_count"`
	StaleFileCount int      `json:"stale_file_count"`
	DurationMs     int64    `json:"duration_ms"`
	ReindexedPaths []string `json:"reindexed_paths"`
}

func decodeReindexResult(t *testing.T, res *mcplib.CallToolResult) reindexResult {
	t.Helper()
	require.NotNil(t, res)
	require.False(t, res.IsError, "tool returned an error result: %s", resultText(res))
	var out reindexResult
	require.NoError(t, json.Unmarshal([]byte(resultText(res)), &out))
	return out
}

// TestHandleReindexRepository_WholeRepoSingleMode exercises the
// single-repo path of reindex_repository with no `paths` argument: a
// file changed since the initial index is re-parsed, and the result
// reports scope="repository" plus a sane node/edge/stale count.
// TestHandleReindexRepository_ResolvesFailedReceiptForTheReparsedPath covers
// the wiring itself: a scoped reindex of one file must clear a terminally
// failed freshness receipt for that file, because otherwise a failed ingest
// fail-closes change.detect for the whole repository until retention lapses
// and reindex — the recovery callers reach for first — silently does nothing.
func TestHandleReindexRepository_ResolvesFailedReceiptForTheReparsedPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(target,
		[]byte("package main\n\nfunc Main() {}\n"), 0o644))

	g := graph.New()
	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)

	// A failed ingest for that exact path, plus one for a file the pass will
	// not touch: only the re-parsed path may be resolved.
	failed := pendingFreshnessReceipt(srv, "receipt-failed", "", target, 4)
	completeFreshnessReceipt(failed, indexer.MutationResult{
		RequestedGeneration: 4,
		Err:                 errors.New("context deadline exceeded"),
	})
	untouched := pendingFreshnessReceipt(srv, "receipt-untouched", "",
		filepath.Join(dir, "other.go"), 5)
	completeFreshnessReceipt(untouched, indexer.MutationResult{
		RequestedGeneration: 5,
		Err:                 errors.New("unrelated failure"),
	})

	bumpFile(t, target, "package main\n\nfunc Main() {}\n\nfunc Extra() {}\n")

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"paths": []any{target}}
	res, err := srv.handleReindexRepository(context.Background(), req)
	require.NoError(t, err)
	out := decodeReindexResult(t, res)
	require.Equal(t, 1, out.StaleFileCount, "the pass must have re-parsed the file")

	if outcome := failed.outcome(false); outcome.Err != nil || !outcome.Reindexed {
		t.Fatalf("reindex did not resolve the failed receipt for the re-parsed path: %+v", outcome)
	}
	if outcome := untouched.outcome(false); outcome.Err == nil {
		t.Fatal("reindex resolved a receipt for a path it never re-parsed")
	}
}

func TestHandleReindexRepository_WholeRepoSingleMode(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "pkg"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc Main() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pkg", "util.go"),
		[]byte("package pkg\n\nfunc Util() {}\n"), 0o644))

	g := graph.New()
	reg := testRegistry()
	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, idx, nil, zap.NewNop(), nil)

	// Change one file, then reindex the whole repo (no path arg).
	bumpFile(t, filepath.Join(dir, "pkg", "util.go"),
		"package pkg\n\nfunc Util() {}\n\nfunc UtilExtra() {}\n")

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{}
	res, err := srv.handleReindexRepository(context.Background(), req)
	require.NoError(t, err)
	out := decodeReindexResult(t, res)

	assert.Equal(t, "repository", out.Scope)
	assert.Equal(t, 1, out.StaleFileCount, "exactly the changed file was stale")
	assert.Empty(t, out.ReindexedPaths, "whole-repo scope reports no path list")
	assert.Greater(t, out.NodeCount, 0)
	assert.GreaterOrEqual(t, out.EdgeCount, 0)
	assert.Equal(t, 2, out.FileCount, "both source files counted")
	assert.NotEmpty(t, g.FindNodesByName("UtilExtra"),
		"the changed symbol must be in the graph after reindex")
}

// TestHandleReindexRepository_PathScopedSingleMode verifies that the
// optional `paths` argument is genuinely honored: only files under the
// supplied path are re-indexed, a stale file outside it is left alone,
// and the result reports scope="paths" with the path list echoed back.
func TestHandleReindexRepository_PathScopedSingleMode(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "in"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "out"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "in", "a.go"),
		[]byte("package in\n\nfunc A() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "out", "b.go"),
		[]byte("package out\n\nfunc B() {}\n"), 0o644))

	g := graph.New()
	reg := testRegistry()
	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, idx, nil, zap.NewNop(), nil)

	// Make both files stale; scope the reindex to "in" only.
	bumpFile(t, filepath.Join(dir, "in", "a.go"),
		"package in\n\nfunc A() {}\n\nfunc AScopedTool() {}\n")
	bumpFile(t, filepath.Join(dir, "out", "b.go"),
		"package out\n\nfunc B() {}\n\nfunc BUnscopedTool() {}\n")

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"paths": []any{filepath.Join(dir, "in")},
	}
	res, err := srv.handleReindexRepository(context.Background(), req)
	require.NoError(t, err)
	out := decodeReindexResult(t, res)

	assert.Equal(t, "paths", out.Scope)
	assert.Equal(t, 1, out.StaleFileCount,
		"only the file inside the scoped path should be re-indexed")
	assert.Equal(t, []string{filepath.Join(dir, "in")}, out.ReindexedPaths,
		"the scoped path list must be echoed back in the result")
	assert.NotEmpty(t, g.FindNodesByName("AScopedTool"),
		"the scoped file's new symbol must be in the graph")
	assert.Empty(t, g.FindNodesByName("BUnscopedTool"),
		"a file outside the scoped path must NOT be re-indexed")
}

// TestHandleReindexRepository_RelativePathScoped checks that scoped
// paths supplied relative to the repository root are honored.
func TestHandleReindexRepository_RelativePathScoped(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "c.go"),
		[]byte("package sub\n\nfunc C() {}\n"), 0o644))

	g := graph.New()
	reg := testRegistry()
	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, idx, nil, zap.NewNop(), nil)

	bumpFile(t, filepath.Join(dir, "sub", "c.go"),
		"package sub\n\nfunc C() {}\n\nfunc CRelTool() {}\n")

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"path":  dir,
		"paths": []any{"sub"}, // repo-root-relative
	}
	res, err := srv.handleReindexRepository(context.Background(), req)
	require.NoError(t, err)
	out := decodeReindexResult(t, res)

	assert.Equal(t, "paths", out.Scope)
	assert.Equal(t, 1, out.StaleFileCount)
	assert.NotEmpty(t, g.FindNodesByName("CRelTool"))
}

// TestHandleReindexRepository_BlankPathsTreatedAsWholeRepo verifies
// that a `paths` list containing only blank strings does not silently
// degrade into an empty scoped pass — it falls back to whole-repo.
func TestHandleReindexRepository_BlankPathsTreatedAsWholeRepo(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc Main() {}\n"), 0o644))

	g := graph.New()
	reg := testRegistry()
	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, idx, nil, zap.NewNop(), nil)

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"paths": []any{"", "  "}}
	res, err := srv.handleReindexRepository(context.Background(), req)
	require.NoError(t, err)
	out := decodeReindexResult(t, res)

	assert.Equal(t, "repository", out.Scope,
		"a paths list of only blank entries must fall back to whole-repo scope")
}

// TestHandleReindexRepository_MultiRepoRoutesByPrefix verifies the
// multi-repo path: reindex_repository accepts a tracked repo prefix,
// routes through the per-repo indexer, and keeps RepoPrefix on nodes.
func TestHandleReindexRepository_MultiRepoRoutesByPrefix(t *testing.T) {
	repoA := setupMiniRepo(t, "repo-a")
	repoB := setupMiniRepo(t, "repo-b")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())

	g := graph.New()
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)
	require.True(t, mi.IsMultiRepo())

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})

	// Change a file in repo-a, then reindex repo-a by prefix.
	bumpFile(t, filepath.Join(repoA, "main.go"),
		"package main\n\nfunc Hello() {}\n\nfunc HelloAgain() {}\n")

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": "repo-a"}
	res, err := srv.handleReindexRepository(context.Background(), req)
	require.NoError(t, err)
	out := decodeReindexResult(t, res)

	assert.Equal(t, "repository", out.Scope)
	assert.Equal(t, 1, out.StaleFileCount, "exactly the changed file in repo-a was stale")

	// Every node still carries its RepoPrefix and both repos remain.
	for _, n := range g.AllNodes() {
		assert.NotEmpty(t, n.RepoPrefix, "node %s lost RepoPrefix after reindex", n.ID)
	}
	stats := g.RepoStats()
	assert.Contains(t, stats, "repo-a")
	assert.Contains(t, stats, "repo-b")
}

// TestHandleReindexRepository_MultiRepoPathScoped verifies a scoped
// reindex in multi-repo mode honors the `paths` argument within the
// targeted repo.
func TestHandleReindexRepository_MultiRepoPathScoped(t *testing.T) {
	repoA := setupMiniRepo(t, "repo-a")

	// Add a second file inside a subdirectory of repo-a.
	require.NoError(t, os.MkdirAll(filepath.Join(repoA, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoA, "sub", "extra.go"),
		[]byte("package sub\n\nfunc Extra() {}\n"), 0o644))

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{{Path: repoA, Name: "repo-a"}},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())

	g := graph.New()
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})

	// Make both files stale; scope the reindex to the "sub" dir only.
	bumpFile(t, filepath.Join(repoA, "main.go"),
		"package main\n\nfunc Hello() {}\n\nfunc HelloScoped() {}\n")
	bumpFile(t, filepath.Join(repoA, "sub", "extra.go"),
		"package sub\n\nfunc Extra() {}\n\nfunc ExtraScoped() {}\n")

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"path":  "repo-a",
		"paths": []any{filepath.Join(repoA, "sub")},
	}
	res, err := srv.handleReindexRepository(context.Background(), req)
	require.NoError(t, err)
	out := decodeReindexResult(t, res)

	assert.Equal(t, "paths", out.Scope)
	assert.Equal(t, 1, out.StaleFileCount,
		"only the file inside the scoped subdirectory should be re-indexed")
	assert.NotEmpty(t, g.FindNodesByName("ExtraScoped"))
	assert.Empty(t, g.FindNodesByName("HelloScoped"),
		"a file outside the scoped path must NOT be re-indexed")
}

// TestHandleReindexRepository_MultiRepoRejectsUntrackedPath verifies
// reindex_repository fails cleanly when called with a path that is not
// a tracked repository in multi-repo mode.
func TestHandleReindexRepository_MultiRepoRejectsUntrackedPath(t *testing.T) {
	repoA := setupMiniRepo(t, "repo-a")
	untracked := setupMiniRepo(t, "stranger")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{{Path: repoA, Name: "repo-a"}},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())

	g := graph.New()
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": untracked}
	res, err := srv.handleReindexRepository(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.IsError, "expected an error result for an untracked path")
}

// TestReindexRepositoryTool_RegisteredAndDiscoverable verifies the
// reindex_repository tool is registered on the server and — because it
// is not a hot eager tool — discoverable via the tools_search lazy
// catalog. After promotion it lands on the live MCP server with the
// `paths` argument declared on its schema.
func TestReindexRepositoryTool_RegisteredAndDiscoverable(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc Main() {}\n"), 0o644))

	g := graph.New()
	reg := testRegistry()
	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, idx, nil, zap.NewNop(), nil)

	// reindex_repository is specialised, so it is NOT in the hot eager
	// set — it must therefore be reachable through tools_search.
	assert.False(t, hotEagerTools["reindex_repository"],
		"reindex_repository is specialised and should be lazily discovered, not eager")

	// Discoverable by exact name through the lazy catalog.
	hits := srv.lazy.Query("select:reindex_repository", 5)
	require.Len(t, hits, 1, "reindex_repository must be discoverable via tools_search")
	tool := hits[0].tool
	assert.Equal(t, "reindex_repository", tool.Name)

	// Also surfaced by a keyword search.
	kw := srv.lazy.Query("incremental reindex", 20)
	var foundByKeyword bool
	for _, h := range kw {
		if h.tool.Name == "reindex_repository" {
			foundByKeyword = true
			break
		}
	}
	assert.True(t, foundByKeyword,
		"reindex_repository must surface in a keyword tools_search")

	// The optional `paths` argument must be declared on the schema.
	_, hasPaths := tool.InputSchema.Properties["paths"]
	assert.True(t, hasPaths, "the reindex_repository schema must declare the `paths` argument")

	// Promotion lands it on the live MCP server, callable like any tool.
	srv.lazy.Promote("reindex_repository")
	assert.NotNil(t, srv.MCPServer().GetTool("reindex_repository"),
		"after tools_search promotion the tool must be on the live MCP server")
}
