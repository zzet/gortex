package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestDeadCodeScopedPagesPreserveGenerationAndGlobalIncoming(t *testing.T) {
	previous := deadCodePageSize
	deadCodePageSize = 5
	t.Cleanup(func() { deadCodePageSize = previous })
	s, _ := openTempStore(t)
	writeDeadCodeFixture(t, s, []int64{0, 4}, 90)
	for _, generation := range []int64{0, 4} {
		nodes := []*graph.Node{{ID: "repo/cross-generation", Kind: graph.KindFunction, RepoPrefix: "repo"}}
		for i := range 14 {
			nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("repo/file%d", i), Kind: graph.KindFile, RepoPrefix: "repo"})
		}
		require.NoError(t, s.AtGeneration(generation).AddBatchChecked(nodes, nil))
	}
	require.NoError(t, s.AtGeneration(4).AddBatchChecked([]*graph.Node{
		{ID: "outside/caller", Kind: graph.KindFunction, RepoPrefix: "outside"},
	}, []*graph.Edge{{From: "outside/caller", To: "repo/cross-generation", Kind: graph.EdgeCalls}}))
	kinds := []graph.NodeKind{graph.KindFunction, graph.KindMethod, graph.KindType}
	allowed := map[graph.NodeKind][]graph.EdgeKind{
		graph.KindFunction: {graph.EdgeCalls}, graph.KindMethod: {graph.EdgeCalls}, graph.KindType: {graph.EdgeReferences},
	}
	for _, generation := range []int64{0, 4} {
		h := s.AtGeneration(generation)
		var want []*graph.Node
		for _, node := range legacyDeadCodeCandidates(h, kinds, allowed) {
			if node.RepoPrefix == "repo" {
				want = append(want, node)
			}
		}
		got, err := h.DeadCodeCandidatesInReposContext(t.Context(), kinds, allowed, []string{"repo", "repo"})
		require.NoError(t, err)
		require.Equal(t, want, got, "generation=%d", generation)
		crossGenerationDead := slices.ContainsFunc(got, func(n *graph.Node) bool { return n.ID == "repo/cross-generation" })
		require.Equal(t, generation == 0, crossGenerationDead, "only same-generation incoming edges count, regardless of caller repo")
		require.Len(t, slices.Collect(h.NodesByKindInRepos(graph.KindFile, []string{"repo"})), 14)
	}
}

func TestDeadCodeScopedPagesSeekWithoutSorting(t *testing.T) {
	s, _ := openTempStore(t)
	writeDeadCodeFixture(t, s, []int64{0, 4}, 90)
	for name, q := range map[string]string{
		"candidate": deadCodeScopedVerdictPageSQL(0), "files": deadCodeScopedFilesPageSQL,
	} {
		plan := generationLookupPlan(t, s.db, q, int64(0), "repo", "frontier", "function", 5)
		require.Contains(t, plan, "nodes_by_repo (repo_prefix=? AND view_gen=? AND id>?)", "%s: %s", name, plan)
		require.NotContains(t, strings.ToUpper(plan), "TEMP B-TREE", "%s: %s", name, plan)
		require.NotContains(t, plan, "SCAN n", "%s: %s", name, plan)
	}
	plan := generationLookupPlan(t, s.db, deadCodeScopedVerdictPageSQL(1), "calls", int64(0), "repo", "frontier", "function", 5)
	require.Contains(t, plan, "nodes_by_repo (repo_prefix=? AND view_gen=? AND id>?)")
	require.NotContains(t, strings.ToUpper(plan), "TEMP B-TREE")
}

func TestDeadCodeScopedReadContextSurvivesBinding(t *testing.T) {
	s, _ := openTempStore(t)
	writeDeadCodeFixture(t, s, []int64{0}, 30)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	bound := graph.BindReadContext(s, ctx)
	reader, ok := bound.(graph.ScopedDeadCodeReader)
	require.True(t, ok)
	require.Empty(t, reader.DeadCodeCandidatesInRepos([]graph.NodeKind{graph.KindFunction}, nil, []string{"repo"}))
	_, err := s.DeadCodeCandidatesInReposContext(ctx, []graph.NodeKind{graph.KindFunction}, nil, []string{"repo"})
	require.True(t, errors.Is(err, context.Canceled))
	_, err = s.IfaceImplementsRowsInReposContext(ctx, []string{"repo"})
	require.True(t, errors.Is(err, context.Canceled))
	require.Empty(t, slices.Collect(reader.NodesByKindInRepos(graph.KindFile, []string{"repo"})))
}

func TestScopedAnalysisReadsSurviveBulkIndexWindow(t *testing.T) {
	s, _ := openTempStore(t)
	require.NoError(t, s.AddBatchChecked([]*graph.Node{
		{ID: "a/main", Name: "main", Kind: graph.KindFunction, RepoPrefix: "a"},
		{ID: "a/dead", Name: "dead", Kind: graph.KindFunction, RepoPrefix: "a"},
		{ID: "a/file", Kind: graph.KindFile, RepoPrefix: "a"},
		{ID: "a/type", Kind: graph.KindType, RepoPrefix: "a"},
		{ID: "b/caller", Kind: graph.KindFunction, RepoPrefix: "b"},
		{ID: "b/interface", Kind: graph.KindInterface, RepoPrefix: "b", Meta: map[string]any{"methods": []string{"Work"}}},
	}, []*graph.Edge{
		{From: "b/caller", To: "a/main", Kind: graph.EdgeCalls},
		{From: "a/type", To: "b/interface", Kind: graph.EdgeImplements},
	}))
	// A cold bulk load deliberately removes secondary indexes. The capability
	// remains exact there, with unhinted SQL until index readiness returns.
	for _, name := range []string{"nodes_by_repo", "nodes_by_name", "edges_by_from"} {
		_, err := s.writerDB.Exec("DROP INDEX " + name)
		require.NoError(t, err)
	}
	nodes, err := s.DeadCodeCandidatesInReposFiltered([]graph.NodeKind{graph.KindFunction}, map[graph.NodeKind][]graph.EdgeKind{graph.KindFunction: {graph.EdgeCalls}}, []string{"a"}, graph.OutlineEdgeFilter{})
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	require.Equal(t, "a/dead", nodes[0].ID)
	rows, err := s.IfaceImplementsRowsInReposFiltered([]string{"a"}, graph.OutlineEdgeFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "b/interface", rows[0].IfaceID)
	files := slices.Collect(s.NodesByKindInRepos(graph.KindFile, []string{"a"}))
	require.Len(t, files, 1)
	require.Equal(t, "a/file", files[0].ID)
	mains, err := s.OutlineNodesByName(graph.NodeScope{RepoAllow: map[string]bool{"a": true}}, "main")
	require.NoError(t, err)
	require.Len(t, mains, 1)
	require.Equal(t, "a/main", mains[0].ID)
}
