package mcp

import (
	"context"
	"path/filepath"
	"testing"

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

func setupMultiRepoStatsServer(t *testing.T) *Server {
	t.Helper()
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
	singleton := indexer.New(g, reg, config.IndexConfig{}, zap.NewNop())
	return NewServer(eng, g, singleton, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})
}

// In multi-repo mode the per_repo dump carries only node/edge totals per repo
// — no by_kind / by_language histogram and no edges⋈nodes join — so the stats
// payload reads from the persisted counters instead of scanning.
func TestGraphStatsPerRepoTotalsOnlyNoHistogram(t *testing.T) {
	srv := setupMultiRepoStatsServer(t)
	payload := srv.buildGraphStatsPayload(context.Background())

	perRepo, ok := payload["per_repo"].(map[string]any)
	require.True(t, ok, "multi-repo payload must carry per_repo")
	require.Contains(t, perRepo, "repo-a")
	require.Contains(t, perRepo, "repo-b")

	for name, raw := range perRepo {
		if name == "_truncated" {
			continue
		}
		entry, ok := raw.(map[string]any)
		require.True(t, ok, "per_repo[%s] must be an object", name)
		require.Contains(t, entry, "total_nodes", "per_repo[%s] must carry totals", name)
		require.Contains(t, entry, "total_edges", "per_repo[%s] must carry totals", name)
		require.NotContains(t, entry, "by_kind", "per_repo[%s] must omit the node histogram", name)
		require.NotContains(t, entry, "by_language", "per_repo[%s] must omit the language histogram", name)
	}
}

// The whole-graph total_nodes equals the sum of the per_repo totals: both are
// sourced from the same per-repo counters, so the dump is internally
// consistent.
func TestGraphStatsTotalNodesEqualsPerRepoSum(t *testing.T) {
	srv := setupMultiRepoStatsServer(t)
	payload := srv.buildGraphStatsPayload(context.Background())

	total, ok := payload["total_nodes"].(int)
	require.True(t, ok, "total_nodes must be an int")
	require.Positive(t, total)

	perRepo := payload["per_repo"].(map[string]any)
	sum := 0
	for name, raw := range perRepo {
		if name == "_truncated" {
			continue
		}
		entry := raw.(map[string]any)
		n, ok := entry["total_nodes"].(int)
		require.True(t, ok, "per_repo[%s].total_nodes must be an int", name)
		sum += n
	}
	assert.Equal(t, total, sum, "total_nodes must equal the sum of per_repo totals")
}
