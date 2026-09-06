package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Any symbol join would consult the stale selected/base graph and incorrectly
// imply that it describes the currently pending checkout. Other reader calls
// also fail because the embedded interface deliberately has no implementation.
type pendingDetectReader struct {
	graph.Reader
	reads int
}

func (r *pendingDetectReader) GetFileNodes(string) []*graph.Node {
	r.reads++
	panic("pending checkout detection must not read graph symbols")
}

func TestDetectChangesPendingCheckoutDoesNotReadGraph(t *testing.T) {
	stack := newViewStack(t)
	root := stack.worktreeRoot
	require.NoError(t, os.MkdirAll(root, 0o755))
	checkoutMutationGit(t, root, "init")
	checkoutMutationGit(t, root, "config", "user.name", "Gortex Test")
	checkoutMutationGit(t, root, "config", "user.email", "test@gortex.invalid")
	path := filepath.Join(root, "edit.go")
	require.NoError(t, os.WriteFile(path, []byte("package sample\n\nfunc Before() {}\n"), 0o644))
	checkoutMutationGit(t, root, "add", ".")
	checkoutMutationGit(t, root, "commit", "-m", "baseline")
	require.NoError(t, os.WriteFile(path, []byte("package sample\n\nfunc After() {}\n"), 0o644))
	checkoutMutationGit(t, root, "add", "edit.go")

	ctx := WithSessionCWD(WithSessionID(context.Background(), "detect-file-only"), stack.repoRoot)
	ctx = withCheckoutControl(ctx, &checkoutControlScope{
		Checkout: store_sqlite.Checkout{
			CheckoutID: viewTestWorktree,
			RootPath:   root,
		},
		RepoPrefix:     "repo",
		CheckoutScoped: true,
	})
	reader := &pendingDetectReader{}
	ctx = withRequestView(ctx, &requestView{reader: reader})
	var req mcp.CallToolRequest
	req.Params.Name = "detect_changes"
	req.Params.Arguments = map[string]any{"scope": "staged", "format": "json"}
	result, err := stack.srv.handleDetectChanges(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError)
	payload := lifecycleResultPayload(t, result)
	require.Equal(t, false, payload["complete"])
	require.Equal(t, "UNKNOWN", payload["risk"])
	require.Equal(t, "pending", payload["graph_status"])
	require.Equal(t, root, payload["repo_root"])
	require.Equal(t, []any{"edit.go"}, payload["changed_files"])
	require.Empty(t, payload["changed_symbols"])
	require.Zero(t, reader.reads)
}
