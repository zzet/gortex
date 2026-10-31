package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type workspaceDeadCodeSpy struct {
	*store_sqlite.Store
	foreign *bool
}

func (r *workspaceDeadCodeSpy) BindReadContext(ctx context.Context) graph.Reader {
	return &workspaceDeadCodeSpy{Store: r.WithReadContext(ctx), foreign: r.foreign}
}
func (r *workspaceDeadCodeSpy) DeadCodeCandidatesInReposFiltered(k []graph.NodeKind, in map[graph.NodeKind][]graph.EdgeKind, repos []string, filter graph.OutlineEdgeFilter) ([]*graph.Node, error) {
	nodes, err := r.Store.DeadCodeCandidatesInReposFiltered(k, in, repos, filter)
	for _, n := range nodes {
		if n.WorkspaceID == "foreign" {
			*r.foreign = true
		}
	}
	return nodes, err
}
func TestDeadCodeWorkspaceFiltersBeforeCandidateAdjacency(t *testing.T) {
	s, store := outlineFixture(t)
	store.AddBatch([]*graph.Node{
		{ID: "own-private", Name: "privateOwn", Kind: graph.KindFunction, RepoPrefix: "stale-owner", WorkspaceID: "ws", FilePath: "own.go", Language: "go"},
		{ID: "foreign-private", Name: "privateForeign", Kind: graph.KindMethod, RepoPrefix: "foreign", WorkspaceID: "foreign", FilePath: "foreign.go", Language: "go"},
	}, nil)
	foreign := false
	reader := newContractCoreEdges(&workspaceDeadCodeSpy{Store: store, foreign: &foreign}, t.Context(), nil)
	ctx := outlineBoundContext(s, "workspace-only", "ws", nil)
	ctx = withRequestView(ctx, &requestView{reader: reader})
	result, err := s.handleFindDeadCode(ctx, makeReq("find_dead_code", map[string]any{"format": "json"}))
	require.NoError(t, err)
	require.Contains(t, toolResultText(result), "own-private", "registry-independent workspace metadata remains authoritative")
	require.NotContains(t, toolResultText(result), "foreign-private")
	require.False(t, foreign, "foreign candidates must be excluded before the method adjacency pass, not only from final output")
}

func (r *workspaceDeadCodeSpy) DeadCodeCandidatesInScopeFiltered(k []graph.NodeKind, in map[graph.NodeKind][]graph.EdgeKind, repos []string, scope graph.NodeScope, filter graph.OutlineEdgeFilter, exclude graph.DeadCodeCandidateExclusions) ([]*graph.Node, error) {
	nodes, err := r.Store.DeadCodeCandidatesInScopeFiltered(k, in, repos, scope, filter, exclude)
	for _, n := range nodes {
		if n.WorkspaceID == "foreign" {
			*r.foreign = true
		}
	}
	return nodes, err
}
