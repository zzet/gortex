package store_sqlite_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func openStatsStore(t *testing.T) *store_sqlite.Store {
	t.Helper()
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "stats.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func statsRepoNodes(repo string, n int) []*graph.Node {
	out := make([]*graph.Node, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &graph.Node{
			ID:         fmt.Sprintf("%s/a.go::F%d", repo, i),
			Kind:       graph.KindFunction,
			Name:       fmt.Sprintf("F%d", i),
			FilePath:   repo + "/a.go",
			RepoPrefix: repo,
		})
	}
	return out
}

// Index-state counters can predate derived writes. Global statistics must
// describe the physical corpus, including when those index counters drift.
func TestStatsTotalsIgnoreStaleIndexStateCounters(t *testing.T) {
	s := openStatsStore(t)
	s.AddBatch(statsRepoNodes("r1", 5), nil)
	s.AddBatch(statsRepoNodes("r2", 5), nil)

	require.NoError(t, s.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "r1", NodeCount: 7, EdgeCount: 2,
	}))
	require.NoError(t, s.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "r2", NodeCount: 11, EdgeCount: 4,
	}))

	st := s.Stats()
	require.Equal(t, 10, st.TotalNodes, "stale repo counters must not replace physical totals")
	require.Zero(t, st.TotalEdges, "stale repo counters must not invent edges")
}

// When the counters match the corpus, the counter sum equals what the exact
// scan would report — the fast path returns the same answer.
func TestStatsCounterSumEqualsExactScanWhenSeededToMatch(t *testing.T) {
	s := openStatsStore(t)
	s.AddBatch(statsRepoNodes("r1", 9), nil)
	require.NoError(t, s.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "r1", NodeCount: s.NodeCount(), EdgeCount: s.EdgeCount(),
	}))

	st := s.Stats()
	require.Equal(t, s.NodeCount(), st.TotalNodes)
	require.Equal(t, s.EdgeCount(), st.TotalEdges)
	require.Equal(t, 9, st.TotalNodes)
}

// With no repo_index_state rows for the view, Stats falls back to the exact
// node/edge scan rather than reporting a counter-absent zero.
func TestStatsFallsBackToExactScanWithoutCounters(t *testing.T) {
	s := openStatsStore(t)
	nodes := statsRepoNodes("r1", 6)
	edges := []*graph.Edge{
		{From: nodes[0].ID, To: nodes[1].ID, Kind: graph.EdgeCalls, FilePath: "r1/a.go", Line: 1},
		{From: nodes[1].ID, To: nodes[2].ID, Kind: graph.EdgeCalls, FilePath: "r1/a.go", Line: 2},
	}
	s.AddBatch(nodes, edges)

	st := s.Stats()
	require.Equal(t, s.NodeCount(), st.TotalNodes, "fallback must match the exact node scan")
	require.Equal(t, s.EdgeCount(), st.TotalEdges, "fallback must match the exact edge scan")
	require.Equal(t, 6, st.TotalNodes, "fallback must not report a counter-absent zero")
}

// OverlaidView composes over the physical base, even when the base has stale
// index-state metadata. No underlying generation can bypass overlay scope.
func TestOverlaidViewStatsComposesOverPhysicalBase(t *testing.T) {
	base := openStatsStore(t)
	base.AddBatch(statsRepoNodes("repo", 4), nil)
	require.NoError(t, base.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "repo", NodeCount: 100, EdgeCount: 50,
	}))
	require.Equal(t, 4, base.Stats().TotalNodes)
	require.Zero(t, base.Stats().TotalEdges)

	layer := graph.NewOverlayLayer()
	layer.MarkFile("repo/new.go", false)
	layer.AddNode("repo/new.go", &graph.Node{
		ID:         "repo/new.go::Added",
		Name:       "Added",
		Kind:       graph.KindFunction,
		FilePath:   "repo/new.go",
		RepoPrefix: "repo",
	})
	view := graph.NewOverlaidView(base, layer)

	require.Equal(t, base.NodeCount()+1, view.NodeCount(), "overlay adds exactly one node")
	nodeDelta := view.NodeCount() - base.NodeCount()
	edgeDelta := view.EdgeCount() - base.EdgeCount()

	got := view.Stats()
	require.Equal(t, base.Stats().TotalNodes+nodeDelta, got.TotalNodes,
		"overlay Stats must be physical base total plus the overlay node delta")
	require.Equal(t, base.Stats().TotalEdges+edgeDelta, got.TotalEdges)
	require.Equal(t, 5, got.TotalNodes,
		"composition is physical base (4) + 1, not stale metadata or the layer count")
}
