package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// A global external the type checker writes (ext::go:fmt::Errorf: no
// repository, no workspace, no project) is called from two repositories in
// two workspaces. find_usages of it answers under each scope with that
// scope's callers only: the external is admitted, the other workspace's
// callers are not.
func TestFindUsagesOfAGlobalExternalStaysInScope(t *testing.T) {
	srv, repoA, repoB := newIsolationServer(t)
	alphaID := symbolIDByName(t, srv, "AlphaThing")
	betaID := symbolIDByName(t, srv, "BetaThing")
	const ext = "ext::go:fmt::Errorf"
	srv.graph.AddNode(&graph.Node{ID: ext, Kind: graph.KindFunction, Name: "Errorf",
		FilePath: "external::go:fmt", Language: "go",
		Meta: map[string]any{"external": true, "module_path": "fmt", "module_role": "stdlib"}})
	srv.graph.AddEdge(&graph.Edge{From: alphaID, To: ext, Kind: graph.EdgeCalls})
	srv.graph.AddEdge(&graph.Edge{From: betaID, To: ext, Kind: graph.EdgeCalls})

	usages := func(ctx context.Context, args map[string]any) string {
		t.Helper()
		args["id"] = ext
		res, err := srv.handleFindUsages(ctx, makeReq("find_usages", args))
		require.NoError(t, err)
		return toolResultText(res)
	}
	for _, tc := range []struct {
		name       string
		ctx        context.Context
		args       map[string]any
		want, deny string
	}{
		{"alpha's workspace", sessionCtx("s-alpha", repoA), map[string]any{}, alphaID, betaID},
		{"beta's workspace", sessionCtx("s-beta", repoB), map[string]any{}, betaID, alphaID},
		{"alpha's repository, alpha's session", sessionCtx("s-alpha-repo", repoA), map[string]any{"repo": "repo-a"}, alphaID, betaID},
		{"repo-a's scope, no session", context.Background(), map[string]any{"repo": "repo-a"}, alphaID, betaID},
		{"repo-b's scope, no session", context.Background(), map[string]any{"repo": "repo-b"}, betaID, alphaID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := usages(tc.ctx, tc.args)
			require.Contains(t, text, tc.want, "the scope's own caller is missing: %s", text)
			require.NotContains(t, text, tc.deny, "another scope's caller leaked: %s", text)
		})
	}
}
