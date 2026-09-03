package store_sqlite

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestCrossRepoFlagUpdateSeeksRequestedLogicalKeys(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	query, args := crossRepoFlagStatement(store.viewGen, []*graph.Edge{{
		From: "repoA/a.go::A", To: "repoB/b.go::B", Kind: graph.EdgeCalls,
		FilePath: "repoA/a.go", Line: 9,
	}})
	rows, err := store.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	require.NoError(t, err)
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	plan := strings.Join(details, "\n")
	require.Contains(t, plan, "sqlite_autoindex_edges_1",
		"requested logical keys must seek through edges' UNIQUE identity index")
	require.NotContains(t, plan, "SCAN edges",
		"one cross-repo batch must not scan the complete edge table")
}

func TestMarkEdgesCrossRepoIsSetOrientedDurableAndIdempotent(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	from := &graph.Node{ID: "repoA/a.go::A", Kind: graph.KindFunction, FilePath: "repoA/a.go", RepoPrefix: "repoA"}
	to := &graph.Node{ID: "repoB/b.go::B", Kind: graph.KindFunction, FilePath: "repoB/b.go", RepoPrefix: "repoB"}
	base := &graph.Edge{From: from.ID, To: to.ID, Kind: graph.EdgeCalls, FilePath: from.FilePath, Line: 9}
	store.AddBatch([]*graph.Node{from, to}, []*graph.Edge{base})

	rows := store.CrossRepoCandidates(graph.BaseKindsForCrossRepo())
	require.Len(t, rows, 1)
	projected := rows[0].Edge
	require.False(t, projected.CrossRepo)

	changed, statements, err := store.markEdgesCrossRepo([]*graph.Edge{projected, projected})
	require.NoError(t, err)
	require.Equal(t, 1, changed)
	require.Equal(t, 1, statements, "duplicate logical keys must share one set-oriented UPDATE")

	stored := store.GetEdgeCandidates([]graph.EdgeEndpoint{{From: from.ID, To: to.ID}}, nil).EndpointKind(from.ID, to.ID, graph.EdgeCalls)
	require.NotNil(t, stored)
	require.True(t, stored.CrossRepo)

	changed, statements, err = store.markEdgesCrossRepo([]*graph.Edge{projected})
	require.NoError(t, err)
	require.Zero(t, changed)
	require.Equal(t, 1, statements)
}

func TestMutationScopedCrossRepoCandidatesSeparatesEdgeSourcesAndIncidentDefinitions(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	nodes := []*graph.Node{
		{ID: "repoA/a.go::A", Kind: graph.KindFunction, FilePath: "repoA/a.go", RepoPrefix: "repoA"},
		{ID: "repoB/b.go::B", Kind: graph.KindFunction, FilePath: "repoB/b.go", RepoPrefix: "repoB"},
		{ID: "repoD/definition.go::D", Kind: graph.KindFunction, FilePath: "repoD/definition.go", RepoPrefix: "repoD"},
		{ID: "repoE/e.go::E", Kind: graph.KindFunction, FilePath: "repoE/e.go", RepoPrefix: "repoE"},
		{ID: "repoF/edge-source.go::F", Kind: graph.KindFunction, FilePath: "repoF/edge-source.go", RepoPrefix: "repoF"},
		{ID: "repoG/g.go::G", Kind: graph.KindFunction, FilePath: "repoG/g.go", RepoPrefix: "repoG"},
	}
	edges := []*graph.Edge{
		{From: nodes[0].ID, To: nodes[1].ID, Kind: graph.EdgeCalls, FilePath: "repoF/edge-source.go", Line: 1},
		{From: nodes[4].ID, To: nodes[5].ID, Kind: graph.EdgeCalls, FilePath: "elsewhere.go", Line: 2},
		{From: nodes[2].ID, To: nodes[3].ID, Kind: graph.EdgeCalls, FilePath: "elsewhere.go", Line: 3},
		{From: nodes[0].ID, To: nodes[1].ID, Kind: graph.EdgeCalls, FilePath: "repoD/definition.go", Line: 4},
	}
	store.AddBatch(nodes, edges)

	assertLines := func(rows []graph.CrossRepoCandidateRow, want ...int) {
		t.Helper()
		got := make(map[int]struct{}, len(rows))
		for _, row := range rows {
			if row.Edge != nil {
				got[row.Edge.Line] = struct{}{}
			}
		}
		require.Len(t, rows, len(want))
		for _, line := range want {
			require.Contains(t, got, line)
		}
	}
	baseKinds := graph.BaseKindsForCrossRepo()
	assertLines(store.CrossRepoCandidatesForMutation(baseKinds, []string{"repoF/edge-source.go"}, nil), 1)
	assertLines(store.CrossRepoCandidatesForMutation(baseKinds, nil, []string{"repoD/definition.go"}), 3)
	assertLines(store.CrossRepoCandidatesForMutation(baseKinds, []string{"repoF/edge-source.go"}, []string{"repoA/a.go"}), 1, 4)

	many := make([]string, 0, 601)
	for i := 0; i < 600; i++ {
		many = append(many, fmt.Sprintf("unrelated/%d.go", i))
	}
	many = append(many, "repoF/edge-source.go")
	assertLines(store.CrossRepoCandidatesForMutation(baseKinds, many, nil), 1)
	require.Empty(t, store.CrossRepoCandidatesForMutation(baseKinds, nil, nil))
}

func TestScopedCrossRepoCandidatesUseExactIncidentFrontiers(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	nodes := []*graph.Node{
		{ID: "repoA/a.go::A", Kind: graph.KindFunction, FilePath: "repoA/a.go", RepoPrefix: "repoA"},
		{ID: "repoB/b.go::B", Kind: graph.KindFunction, FilePath: "repoB/b.go", RepoPrefix: "repoB"},
		{ID: "repoC/c.go::C", Kind: graph.KindFunction, FilePath: "repoC/c.go", RepoPrefix: "repoC"},
		{ID: "repoD/d.go::D", Kind: graph.KindFunction, FilePath: "repoD/d.go", RepoPrefix: "repoD"},
	}
	edges := []*graph.Edge{
		{From: nodes[0].ID, To: nodes[1].ID, Kind: graph.EdgeCalls, FilePath: nodes[0].FilePath, Line: 1},
		{From: nodes[2].ID, To: nodes[3].ID, Kind: graph.EdgeCalls, FilePath: nodes[2].FilePath, Line: 2},
	}
	store.AddBatch(nodes, edges)

	byRepo := store.CrossRepoCandidatesForRepos(graph.BaseKindsForCrossRepo(), []string{"repoB"})
	require.Len(t, byRepo, 1)
	require.Equal(t, edges[0].From, byRepo[0].Edge.From, "target-only repo scope must retain inbound edges")

	// Hundreds of paths still bind as one JSON value rather than exceeding
	// SQLite's host-parameter limit.
	files := make([]string, 0, 601)
	for i := 0; i < 600; i++ {
		files = append(files, fmt.Sprintf("unrelated/%d.go", i))
	}
	files = append(files, nodes[1].FilePath)
	byFile := store.CrossRepoCandidatesForFiles(graph.BaseKindsForCrossRepo(), files)
	require.Len(t, byFile, 1)
	require.Equal(t, edges[0].From, byFile[0].Edge.From, "target-file scope must retain inbound edges")
}
