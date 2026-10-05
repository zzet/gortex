package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A cached-count response must not trigger the expensive audit or fall back to
// corpus counts, even when the persisted counters disagree with the rows.
type graphStatsCachedCountSpy struct {
	*contractCoreStatsStore
}

func (*graphStatsCachedCountSpy) ScanRepoMemoryEstimates(context.Context) (map[string]graph.RepoMemoryEstimate, error) {
	panic("stats provenance must not audit the corpus")
}

func (*graphStatsCachedCountSpy) RepoStats() map[string]graph.GraphStats {
	panic("cached counters must not fall back to a corpus scan")
}

func requireCachedRepoCountProvenance(t *testing.T, payload map[string]any) {
	t.Helper()
	require.Equal(t, map[string]any{
		"accuracy":   "cached_estimate",
		"source":     "index_snapshot",
		"freshness":  "unverified",
		"counted_at": nil,
	}, payload["per_repo_counts"])
}

func TestGraphStatsCachedCountProvenanceUsesSelectedGeneration(t *testing.T) {
	srv := setupMultiRepoStatsServer(t)
	base, err := store_sqlite.Open(filepath.Join(t.TempDir(), "counts.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = base.Close() })
	require.NoError(t, base.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "selected", NodeCount: 99, EdgeCount: 98,
	}))
	selectedStore := base.AtGeneration(4)
	selectedStore.AddBatch([]*graph.Node{
		{ID: "selected/a.go::A", Kind: graph.KindFunction, FilePath: "selected/a.go", RepoPrefix: "selected"},
		{ID: "selected/a.go::B", Kind: graph.KindFunction, FilePath: "selected/a.go", RepoPrefix: "selected"},
	}, nil)
	// IndexedAt is index provenance, not the time these counts were measured.
	require.NoError(t, selectedStore.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "selected", NodeCount: 7, EdgeCount: 3, IndexedAt: 1700000000,
	}))
	selected := &graphStatsCachedCountSpy{contractCoreStatsStore: &contractCoreStatsStore{Store: selectedStore}}
	ctx := withRequestView(t.Context(), &requestView{reader: selected})
	stats := &graph.GraphStats{TotalNodes: 2}
	for _, core := range []bool{false, true} {
		if core {
			installContractCoreKindTestRuntime(t, srv)
		}
		before := selected.counterReads
		payload := srv.buildGraphStatsPayloadFromStats(ctx, stats)
		require.Equal(t, map[string]any{
			"selected": map[string]any{"total_nodes": 7, "total_edges": 3},
		}, payload["per_repo"], "selected generation counts must not be replaced by canonical or physical-base counts")
		requireCachedRepoCountProvenance(t, payload)
		require.Equal(t, before+1, selected.counterReads, "metadata classification must not add a counter read")
		require.Zero(t, selected.repoScans)
		require.Zero(t, selected.checked)
		require.Zero(t, selected.legacy)
	}
	// Missing cached entries are still unverified, not proof of an empty repo.
	selected.Store = base.AtGeneration(5)
	before := selected.counterReads
	payload := srv.buildGraphStatsPayloadFromStats(ctx, &graph.GraphStats{})
	require.Empty(t, payload["per_repo"])
	requireCachedRepoCountProvenance(t, payload)
	require.Equal(t, before+1, selected.counterReads)
}

func TestGraphStatsMaintainedAndComposedCountsAreNotCachedEstimates(t *testing.T) {
	srv := setupMultiRepoStatsServer(t)
	payload := srv.buildGraphStatsPayload(t.Context())
	require.NotContains(t, payload, "per_repo_counts", "maintained in-memory counters must not be labeled stale index snapshots")
	maintained := newContractCoreEdges(srv.graph, t.Context(), nil)
	_, cached := perRepoTotalsWithCacheProvenance(maintained)
	require.False(t, cached, "core wrapping must preserve maintained counter classification")

	base, err := store_sqlite.Open(filepath.Join(t.TempDir(), "overlay.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = base.Close() })
	base.AddBatch([]*graph.Node{
		{ID: "selected/a.go::Old", Kind: graph.KindFunction, FilePath: "selected/a.go", RepoPrefix: "selected"},
		{ID: "selected/deleted.go::Gone", Kind: graph.KindFunction, FilePath: "selected/deleted.go", RepoPrefix: "selected"},
	}, nil)
	require.NoError(t, base.SetRepoIndexState(graph.RepoIndexState{RepoPrefix: "selected", NodeCount: 99, EdgeCount: 98}))
	layer := graph.NewOverlayLayer()
	layer.MarkFile("selected/a.go", false)
	layer.MarkFile("selected/deleted.go", true)
	layer.AddNode("selected/a.go", &graph.Node{ID: "selected/a.go::New", Kind: graph.KindType, FilePath: "selected/a.go", RepoPrefix: "selected"})
	selected := graph.NewOverlaidView(base, layer)
	ctx := withRequestView(t.Context(), &requestView{reader: selected})
	for _, core := range []bool{false, true} {
		if core {
			installContractCoreKindTestRuntime(t, srv)
		}
		payload := srv.buildGraphStatsPayloadFromStats(ctx, &graph.GraphStats{TotalNodes: 1})
		require.Equal(t, map[string]any{
			"selected": map[string]any{"total_nodes": 1, "total_edges": 0},
		}, payload["per_repo"])
		require.NotContains(t, payload, "per_repo_counts", "composed counts must not inherit underlying cached-count provenance")
	}
}

func TestGraphStatsTruncationDoesNotAdvertiseUnsupportedRepoArgument(t *testing.T) {
	payload := cappedRepoTotals(map[string]repoTotal{
		"large": {nodes: 7, edges: 3},
		"small": {nodes: 1},
	}, 1)
	require.Len(t, payload, 2)
	require.Equal(t, map[string]any{"total_nodes": 7, "total_edges": 3}, payload["large"])
	require.NotContains(t, payload, "small")
	marker := payload["_truncated"].(map[string]any)
	require.Equal(t, 1, marker["shown"])
	require.Equal(t, 2, marker["total_repos"])
	note := marker["note"].(string)
	require.NotContains(t, note, "graph_stats")
	require.NotContains(t, note, "repo=")
	require.Contains(t, note, "omitted entries are not evidence of an empty repository")
}
