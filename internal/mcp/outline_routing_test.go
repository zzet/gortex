package mcp

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// This drives the actual router and tool middleware, rather than injecting a
// Store in the request context. Primary checkout sessions are the production
// scoped-query path; automatic sibling stacks retain their own exact fallback.
func TestOutlinePrimaryRoutingPreservesStableCacheAndScopedQueries(t *testing.T) {
	v := newViewStack(t)
	t.Cleanup(v.srv.DrainBackground)
	installContractCoreKindTestRuntime(t, v.srv)
	v.srv.RunAnalysis()
	var first outlineResponseKey
	for round := 0; round < 2; round++ {
		result, err := v.callWithView(t, v.repoRoot, "get_repo_outline", map[string]any{"format": "json"}, func(ctx context.Context) (*mcp.CallToolResult, error) {
			reader := v.srv.readerFor(ctx)
			core, ok := reader.(contractCoreWrapped)
			require.True(t, ok)
			require.Same(t, v.store, core.contractCore().Reader, "primary routing must keep the shared Store handle")
			require.Implements(t, (*graph.ScopedDeadCodeReader)(nil), reader)
			require.Implements(t, (*graph.FilteredScopedDeadCodeReader)(nil), core.contractCore().Reader)
			_, bound := v.srv.sessionScopeOptions(ctx)
			require.True(t, bound)
			opts, _ := v.srv.scopeOptionsWithRepoNarrow(ctx, repoAllowFromContext(ctx))
			key, cacheable := v.srv.outlineCacheKey(reader, graph.NodeScope{WorkspaceID: opts.WorkspaceID, ProjectID: opts.ProjectID, RepoAllow: opts.RepoAllow}, bound)
			require.True(t, cacheable)
			if round == 0 {
				first = key
			} else {
				require.Equal(t, first, key, "per-request core wrappers must not churn the identity")
			}
			return v.srv.handleGetRepoOutline(ctx, makeReq("get_repo_outline", map[string]any{"format": "json"}))
		})
		require.NoError(t, err)
		require.False(t, result.IsError)
		require.Len(t, v.srv.outlineResponses.keys, 1)
	}
	result, err := v.callWithView(t, v.worktreeRoot, "get_repo_outline", map[string]any{"format": "json"}, func(ctx context.Context) (*mcp.CallToolResult, error) {
		reader := v.srv.readerFor(ctx)
		core, ok := reader.(contractCoreWrapped)
		require.True(t, ok)
		require.NotSame(t, v.store, core.contractCore().Reader)
		opts, bound := v.srv.sessionScopeOptions(ctx)
		require.True(t, bound)
		_, cacheable := v.srv.outlineCacheKey(reader, graph.NodeScope{WorkspaceID: opts.WorkspaceID, ProjectID: opts.ProjectID, RepoAllow: opts.RepoAllow}, bound)
		require.False(t, cacheable, "a routed stack cannot inherit its physical store's visibility or revision proof")
		require.Implements(t, (*graph.ScopedDeadCodeReader)(nil), reader, "the mask must still expose its exact-reader fallback")
		return v.srv.handleGetRepoOutline(ctx, makeReq("get_repo_outline", map[string]any{"format": "json"}))
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
}
