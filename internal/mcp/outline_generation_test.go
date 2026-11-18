package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestOutlineGenerationCommunitiesProductionWrapperParity(t *testing.T) {
	store, _ := buildAnalysisCacheTestGraph(t, 80)
	defer store.Close()
	store.AddNode(&graph.Node{ID: "foreign", Kind: graph.KindFunction, Name: "Foreign", RepoPrefix: "foreign", FilePath: "foreign.go"})
	s, metrics := populateAnalysisForTest(store)
	require.NoError(t, metrics.cacheSaveErr)
	s.session, s.sessions = newSessionState(), newSessionMap()
	installContractCoreKindTestRuntime(t, s)
	for _, scope := range []graph.NodeScope{
		{WorkspaceID: "repo", RepoAllow: map[string]bool{"repo": true}},
		{WorkspaceID: "foreign", RepoAllow: map[string]bool{"foreign": true}},
		{WorkspaceID: "missing", RepoAllow: map[string]bool{"missing": true}},
	} {
		ctx := outlineBoundContext(s, scope.WorkspaceID, scope.WorkspaceID, scope.RepoAllow)
		want := outlineJSON(t, s, ctx, legacyOutlineReader{Reader: store})
		counted := &outlineCountingReader{Store: store}
		got := outlineJSON(t, s, ctx, counted)
		require.JSONEq(t, string(want), string(got))
		require.Positive(t, counted.communityQueries, "the production contract-core wrapper must reach the generation query")
		require.Zero(t, counted.allNodes)
		require.Zero(t, counted.allNodesLight)
		require.Zero(t, counted.allEdges)
	}
	ctx := context.Background()
	_, supported, err := s.outlineGenerationCommunities(ctx, legacyOutlineReader{Reader: store}, graph.NodeScope{})
	require.NoError(t, err)
	require.False(t, supported, "selected readers without this node-visibility capability retain fallback")
	_, supported, err = s.outlineGenerationCommunities(ctx, store.AtGeneration(5), graph.NodeScope{})
	require.NoError(t, err)
	require.False(t, supported, "a base partition cannot be queried through a foreign payload generation")
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	_, _, err = s.outlineGenerationCommunities(ctx, store, graph.NodeScope{})
	require.ErrorIs(t, err, context.Canceled)
	store.AddNode(&graph.Node{ID: "mutation"})
	_, supported, err = s.outlineGenerationCommunities(context.Background(), store, graph.NodeScope{})
	require.NoError(t, err)
	require.False(t, supported, "stale receipt must never expose old generation rows")
}
