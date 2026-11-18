package store_sqlite

import (
	"fmt"
	"iter"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func scopedKindSummaryRows(seq iter.Seq[*graph.Node]) []*graph.Node {
	var out []*graph.Node
	for node := range seq {
		out = append(out, node)
	}
	return out
}

func scopedKindSummaryFields(n *graph.Node) *graph.Node {
	return &graph.Node{
		ID: n.ID, Kind: n.Kind, Name: n.Name, QualName: n.QualName,
		FilePath: n.FilePath, StartLine: n.StartLine, EndLine: n.EndLine,
		StartColumn: n.StartColumn, EndColumn: n.EndColumn,
		Language: n.Language, RepoPrefix: n.RepoPrefix,
		WorkspaceID: n.WorkspaceID, ProjectID: n.ProjectID,
	}
}

func TestScopedKindSummaryPreservesPagedScopeAndGeneration(t *testing.T) {
	s := openScopedProjectionTestStore(t)
	const perRepo = scopedProjectionPage/2 + 17
	for _, generation := range []int64{0, 31} {
		var nodes []*graph.Node
		for i := 0; i < perRepo; i++ {
			for ordinal, repo := range []string{"", "a", "b", "foreign"} {
				id := fmt.Sprintf("row-%04d-%d", i, ordinal)
				nodes = append(nodes, &graph.Node{
					ID: id, Kind: graph.KindFile, Name: fmt.Sprintf("generation-%d-%d.go", generation, i), QualName: id + "/qualified",
					FilePath: repo + fmt.Sprintf("/file-%d.go", i), StartLine: i + 1, EndLine: i + 2, StartColumn: 2, EndColumn: 4,
					Language: "go", RepoPrefix: repo, WorkspaceID: "workspace", ProjectID: "project",
					Meta: map[string]any{"signature": "must not transfer", "doc": strings.Repeat("large retrieval payload ", 64), "nested": map[string]any{"generation": generation, "repo": repo}},
				})
			}
		}
		nodes = append(nodes, &graph.Node{ID: "ordinary-function", Kind: graph.KindFunction, FilePath: "a/file-0.go", RepoPrefix: "a", Name: "Function"})
		require.NoError(t, s.AtGeneration(generation).AddBatchChecked(nodes, nil))
	}
	// A removal in the base must not remove the same identity in generation 31.
	removed, _ := s.EvictFile("a/file-0.go")
	require.Positive(t, removed)
	for _, generation := range []int64{0, 31} {
		t.Run(fmt.Sprint(generation), func(t *testing.T) {
			handle := s.AtGeneration(generation)
			repos := []string{"b", "", "a", "b", ""}
			full := scopedKindSummaryRows(handle.NodesInScopeSeq(repos, nil, graph.KindFile))
			require.Greater(t, len(full), scopedProjectionPage)
			want := make([]*graph.Node, len(full))
			for i, node := range full {
				want[i] = scopedKindSummaryFields(node)
				if i > 0 {
					require.Less(t, full[i-1].ID, node.ID)
				}
			}
			got := scopedKindSummaryRows(handle.NodesLightByKindsInScopeSeq(repos, nil, graph.KindFile, graph.KindFile))
			require.Equal(t, want, got, "all thirteen fields, global order, deduped scopes/kinds and generation must match full rows")
			for _, row := range got {
				require.Equal(t, graph.KindFile, row.Kind)
				require.NotEqual(t, "foreign", row.RepoPrefix)
				require.Nil(t, row.Meta)
			}
			files := []string{"b/file-1.go", "a/file-1.go", "b/file-1.go"}
			selected := scopedKindSummaryRows(handle.NodesInScopeSeq(repos, files, graph.KindFile))
			want = nil
			for _, node := range selected {
				want = append(want, scopedKindSummaryFields(node))
			}
			require.Equal(t, want, scopedKindSummaryRows(handle.NodesLightByKindsInScopeSeq(repos, files, graph.KindFile)))
		})
	}
}

func TestScopedKindSummaryRetainsKindAndYieldSemantics(t *testing.T) {
	s := openScopedProjectionTestStore(t)
	require.NoError(t, s.AddBatchChecked([]*graph.Node{
		{ID: "file", Kind: graph.KindFile, FilePath: "a/file.go", RepoPrefix: "a", Language: "go"},
		{ID: "function", Kind: graph.KindFunction, FilePath: "a/file.go", RepoPrefix: "a"},
	}, nil))
	require.Empty(t, scopedKindSummaryRows(s.NodesLightByKindsInScopeSeq([]string{"a"}, nil, graph.NodeKind(""))))
	require.Empty(t, scopedKindSummaryRows(s.NodesLightByKindsInScopeSeq(nil, nil, graph.KindFile)))
	rows := scopedKindSummaryRows(s.NodesLightByKindsInScopeSeq([]string{"a"}, nil, graph.KindFunction, graph.KindFile, graph.KindFunction))
	require.Len(t, rows, 2)
	require.Equal(t, "function", rows[0].ID)
	require.Equal(t, "file", rows[1].ID)
	calls := 0
	s.NodesLightByKindsInScopeSeq([]string{"a"}, nil, graph.KindFunction, graph.KindFile)(func(*graph.Node) bool {
		calls++
		return false
	})
	require.Equal(t, 1, calls, "consumer stop must not continue into another kind cursor")
}
