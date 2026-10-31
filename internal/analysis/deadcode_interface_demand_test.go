package analysis

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type demandIfaceReader struct {
	*graph.Graph
	globalQueries int
	outgoingIDs   []string
}

func (r *demandIfaceReader) IfaceImplementsRowsInRepos(repos []string) []graph.IfaceImplementsRow {
	r.globalQueries++
	return r.Graph.IfaceImplementsRowsInRepos(repos)
}
func (r *demandIfaceReader) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	r.outgoingIDs = append(r.outgoingIDs, ids...)
	return r.Graph.GetOutEdgesByNodeIDs(ids)
}
func TestDeadCodeInterfaceEvidenceIsDemandedByMethodCandidates(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "a/method", Kind: graph.KindMethod, Name: "work", FilePath: "a/f.go", Language: "go", RepoPrefix: "a", WorkspaceID: "ws"},
		{ID: "a/type", Kind: graph.KindType, Name: "owner", FilePath: "a/f.go", Language: "go", RepoPrefix: "a", WorkspaceID: "ws"},
		{ID: "a/dead", Kind: graph.KindMethod, Name: "unusedMethod", FilePath: "a/f.go", Language: "go", RepoPrefix: "a", WorkspaceID: "ws"},
		{ID: "a/dangling", Kind: graph.KindMethod, Name: "throughMissing", FilePath: "a/f.go", Language: "go", RepoPrefix: "a", WorkspaceID: "ws"},
		{ID: "b/iface", Kind: graph.KindInterface, Name: "Foreign", RepoPrefix: "b", WorkspaceID: "other", Meta: map[string]any{"methods": []string{"work", "throughMissing"}}},
		{ID: "a/unrelated", Kind: graph.KindType, Name: "otherOwner", RepoPrefix: "a", WorkspaceID: "ws", Meta: map[string]any{"external": true}},
	}, []*graph.Edge{
		{From: "a/method", To: "a/type", Kind: graph.EdgeMemberOf},
		{From: "a/type", To: "b/iface", Kind: graph.EdgeImplements},
		{From: "a/dangling", To: "missing-owner", Kind: graph.EdgeMemberOf},
		{From: "missing-owner", To: "b/iface", Kind: graph.EdgeImplements},
		{From: "a/unrelated", To: "b/iface", Kind: graph.EdgeImplements},
	})
	for _, scoped := range []bool{false, true} {
		t.Run(map[bool]string{false: "unbound dangling owner", true: "exact scope"}[scoped], func(t *testing.T) {
			r := &demandIfaceReader{Graph: g}
			opts := FindDeadCodeOptions{}
			if scoped {
				scope := graph.NodeScope{WorkspaceID: "ws"}
				opts.NodeScope = &scope
			}
			result := FindDeadCode(r, nil, nil, opts)
			var ids []string
			for _, e := range result {
				ids = append(ids, e.ID)
			}
			require.Contains(t, ids, "a/dead")
			require.NotContains(t, ids, "a/method", "foreign interface evidence still satisfies an admitted receiver")
			if scoped {
				require.Contains(t, ids, "a/dangling")
			} else {
				require.NotContains(t, ids, "a/dangling", "unbound legacy dangling implementing identity remains valid")
			}
			require.Zero(t, r.globalQueries, "do not enumerate every implementing owner before candidate admission")
			require.NotContains(t, r.outgoingIDs, "a/unrelated", "unrelated implementing types need no adjacency read")
		})
	}
}

func TestDeadCodeWithoutMethodCandidatesDoesNotReadInterfaceEvidence(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{{ID: "a/function", Kind: graph.KindFunction, Name: "unused", FilePath: "a/f.go", Language: "go", RepoPrefix: "a"}}, nil)
	r := &demandIfaceReader{Graph: g}
	require.Len(t, FindDeadCode(r, nil, nil), 1)
	require.Zero(t, r.globalQueries)
	require.Empty(t, r.outgoingIDs)
}
