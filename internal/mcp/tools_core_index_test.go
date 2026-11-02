package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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

// setupMiniRepo creates a temp directory with a single Go file and returns
// the absolute path. Shared by tests that need a small real repo to index.
func setupMiniRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc Hello() {}\n"),
		0o644,
	))
	return dir
}

// TestHandleIndexRepository_MultiRepoRoutesThroughMultiIndexer verifies that
// in multi-repo mode the index_repository tool delegates to multiIndexer
// (preserving RepoPrefix on all nodes), rather than polluting the graph via
// the prefix-less singleton indexer.
func TestHandleIndexRepository_MultiRepoRoutesThroughMultiIndexer(t *testing.T) {
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
	registerMCPFixtureRepos(t, mi, cm)
	require.True(t, mi.IsMultiRepo())

	// Invariant before: every node has a non-empty RepoPrefix, and both repos
	// are represented in the graph's per-repo index.
	for _, n := range g.AllNodes() {
		require.NotEmpty(t, n.RepoPrefix, "multi-repo nodes must carry RepoPrefix")
	}
	statsBefore := g.RepoStats()
	require.Contains(t, statsBefore, "repo-a")
	require.Contains(t, statsBefore, "repo-b")

	eng := query.NewEngine(g)
	// Pass a non-nil singleton indexer to ensure the handler's multi-repo
	// branch runs even when s.indexer is also present.
	singleton := indexer.New(g, reg, config.IndexConfig{}, zap.NewNop())
	srv := NewServer(eng, g, singleton, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})

	// Re-index repo-a by path. Should route through multiIndexer and preserve
	// prefixes on all resulting nodes.
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": repoA}
	result, err := srv.handleIndexRepository(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError,
		"expected success; got error result: %+v", result.Content)

	// Invariant after: no node should have dropped its RepoPrefix; both repos
	// still present in per-repo stats.
	for _, n := range g.AllNodes() {
		assert.NotEmpty(t, n.RepoPrefix,
			"node %s lost RepoPrefix after re-index", n.ID)
	}
	statsAfter := g.RepoStats()
	assert.Contains(t, statsAfter, "repo-a",
		"repo-a missing from per_repo stats after re-index")
	assert.Contains(t, statsAfter, "repo-b",
		"repo-b should remain in per_repo stats after re-index")

	// Also accept the repo prefix directly.
	req2 := mcplib.CallToolRequest{}
	req2.Params.Arguments = map[string]any{"path": "repo-b"}
	result2, err := srv.handleIndexRepository(context.Background(), req2)
	require.NoError(t, err)
	assert.False(t, result2.IsError)
}

// TestHandleIndexRepository_ReconcilesFromPersistedConfig exercises T0.3:
// the tuck audit saw repos listed by get_active_project but rejected by
// index_repository as "not a tracked repository" because the persisted
// config got ahead of the in-memory MultiIndexer state (warmup error,
// daemon restart, etc.). The user had to re-track every repo manually.
// After the reconcile fix, handleIndexRepository consults the persisted
// config when the in-memory lookup misses and auto-tracks the entry —
// the operation succeeds without a prior explicit track_repository call.
func TestHandleIndexRepository_ReconcilesFromPersistedConfig(t *testing.T) {
	repoA := setupMiniRepo(t, "repo-a")
	repoB := setupMiniRepo(t, "repo-b")

	// Persist BOTH repos to the global config file — the state a user
	// sees via get_active_project.
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

	preg := parser.NewRegistry()
	preg.Register(languages.NewGoExtractor())

	g := graph.New()
	// Intentionally DO NOT call IndexAll / TrackRepoCtx — simulates the
	// warmup-drift state: config persisted, but MultiIndexer empty.
	mi := indexer.NewMultiIndexer(g, preg, search.NewNull(), cm, zap.NewNop())
	require.Empty(t, mi.AllMetadata(),
		"precondition: MultiIndexer must start empty to reproduce drift")

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": repoA}
	result, err := srv.handleIndexRepository(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError,
		"handleIndexRepository must reconcile persisted-config drift instead of rejecting; got %+v",
		result.Content)

	// Post-condition: repo-a is now in the live MultiIndexer too.
	assert.NotNil(t, mi.GetMetadata("repo-a"),
		"reconcile must populate mi.repos so future queries see the repo")
}

// TestHandleIndexRepository_MultiRepoRejectsUntrackedPath verifies that
// index_repository fails cleanly when called with a path that isn't tracked
// in multi-repo mode (instead of silently polluting the graph with
// unprefixed nodes).
func TestHandleIndexRepository_MultiRepoRejectsUntrackedPath(t *testing.T) {
	repoA := setupMiniRepo(t, "repo-a")
	repoB := setupMiniRepo(t, "repo-b")
	untracked := setupMiniRepo(t, "stranger")

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
	registerMCPFixtureRepos(t, mi, cm)

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": untracked}
	result, err := srv.handleIndexRepository(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError, "expected error result for untracked path")

	// Graph should still be clean: no untracked nodes landed in it.
	for _, n := range g.AllNodes() {
		assert.NotEmpty(t, n.RepoPrefix,
			"no node should be created for an untracked path")
	}
}
