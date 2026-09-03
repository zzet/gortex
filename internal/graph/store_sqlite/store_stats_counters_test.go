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

// Stats totals are summed from the persisted repo_index_state counters, not a
// scan of the nodes/edges tables. Seeding a counter that disagrees with the
// stored corpus is the only way to tell the two sources apart from outside:
// the counter sum (7+11=18 nodes, 2+4=6 edges) differs from the 10 stored
// nodes and 0 stored edges.
func TestStatsTotalsComeFromIndexStateCounters(t *testing.T) {
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
	require.Equal(t, 18, st.TotalNodes, "totals must be the counter sum, not a node recount")
	require.Equal(t, 6, st.TotalEdges, "totals must be the counter sum, not an edge recount")
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

// OverlaidView.Stats composes the counter-sourced base totals with the
// overlay's own node/edge delta — base + delta, not the layer's bare count and
// not a recount of the overlay generation. The base counter is seeded larger
// than the stored corpus so the composed total can only be right if the base
// half came from the counter.
func TestOverlaidViewStatsComposesOverCounterBase(t *testing.T) {
	base := openStatsStore(t)
	base.AddBatch(statsRepoNodes("repo", 4), nil)
	require.NoError(t, base.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "repo", NodeCount: 100, EdgeCount: 50,
	}))
	require.Equal(t, 100, base.Stats().TotalNodes)
	require.Equal(t, 50, base.Stats().TotalEdges)

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
		"overlay Stats must be base counter total plus the overlay node delta")
	require.Equal(t, base.Stats().TotalEdges+edgeDelta, got.TotalEdges)
	require.Equal(t, 101, got.TotalNodes,
		"composition is base counter (100) + 1, not the overlay's bare node count")
}
