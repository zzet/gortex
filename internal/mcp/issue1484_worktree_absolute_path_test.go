package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// readReq builds a read_file request with the given arguments.
func readReq(args map[string]any) mcplib.CallToolRequest {
	var req mcplib.CallToolRequest
	req.Params.Name = "read_file"
	req.Params.Arguments = args
	return req
}

// TestIssue1484_WorktreeAbsolutePathInferedView reproduces and verifies the
// fix for #1484: an absolute path that falls inside a *routed* linked worktree
// now reads back verbatim even with no view selector, instead of being refused
// as "outside every indexed repository root".
//
// The fix keeps the confinement guard intact: a path that is genuinely outside
// every tracked repo — and every routed linked checkout — is still refused, and
// the explicit-worktree-view path continues to work.
func TestIssue1484_WorktreeAbsolutePathInferedView(t *testing.T) {
	stack := newViewStack(t)
	require.NotEqual(t, stack.worktreeRoot, stack.repoRoot, "worktree must be outside the primary root")

	// A real file living in the sibling worktree.
	target := filepath.Join(stack.worktreeRoot, "Sources", "X.swift")
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	const want = `content":"package Probe`
	require.NoError(t, os.WriteFile(target, []byte("package Probe\n\nlet probe = 1\n"), 0o644))
	absTarget := target // canonical absolute path (worktree is not a symlink)

	// All calls originate from the primary checkout's cwd, as in the issue.
	cwd := stack.repoRoot

	// 1. No view selector: the routed worktree root is absent from the guard's
	//    primary root set, but the absolute path still resolves to exactly one
	//    routed linked checkout, so the guard admits it and the file reads back.
	res, err := stack.callWithView(t, cwd, "read_file", map[string]any{"path": absTarget},
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			return stack.srv.handleReadFile(ctx, readReq(map[string]any{"path": absTarget}))
		})
	require.NoError(t, err)
	t.Logf("no-view result (IsError=%v): %s", res.IsError, readText(t, res))
	require.False(t, res.IsError, "absolute path into a routed worktree must read without a view: %s", readText(t, res))
	require.Contains(t, readText(t, res), want)

	// 2. Same path, plus a worktree view selector: still reads back exactly,
	//    preserving the issue's explicit-workaround behavior.
	res, err = stack.callWithView(t, cwd, "read_file", map[string]any{
		"path": absTarget,
		"view": map[string]any{"kind": "worktree", "checkout_id": viewTestWorktree},
	}, func(ctx context.Context) (*mcplib.CallToolResult, error) {
		req := readReq(map[string]any{
			"path": absTarget,
			"view": map[string]any{"kind": "worktree", "checkout_id": viewTestWorktree},
		})
		return stack.srv.handleReadFile(ctx, req)
	})
	require.NoError(t, err)
	require.False(t, res.IsError, "with a view the file must read: %s", readText(t, res))
	require.Contains(t, readText(t, res), want)

	// 3. Relative path + view also reads back exactly.
	res, err = stack.callWithView(t, cwd, "read_file", map[string]any{
		"path": "Sources/X.swift",
		"view": map[string]any{"kind": "worktree", "checkout_id": viewTestWorktree},
	}, func(ctx context.Context) (*mcplib.CallToolResult, error) {
		req := readReq(map[string]any{
			"path": "Sources/X.swift",
			"view": map[string]any{"kind": "worktree", "checkout_id": viewTestWorktree},
		})
		return stack.srv.handleReadFile(ctx, req)
	})
	require.NoError(t, err)
	require.False(t, res.IsError, "relative path + view must read: %s", readText(t, res))
	require.Contains(t, readText(t, res), want)

	// 4. An absolute path genuinely outside every tracked repo and every routed
	//    linked checkout is still refused, keeping the guard's confinement.
	outside := filepath.Join(filepath.Dir(stack.worktreeRoot), "everywhere", "elsewhere.go")
	require.NoError(t, os.MkdirAll(filepath.Dir(outside), 0o755))
	require.NoError(t, os.WriteFile(outside, []byte("package outsider\n"), 0o644))
	res, err = stack.callWithView(t, cwd, "read_file", map[string]any{"path": outside},
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			return stack.srv.handleReadFile(ctx, readReq(map[string]any{"path": outside}))
		})
	require.NoError(t, err)
	t.Logf("outside result (IsError=%v): %s", res.IsError, readText(t, res))
	require.True(t, res.IsError, "absolute path outside all repos must stay refused: %s", readText(t, res))
	require.Contains(t, readText(t, res), "outside every indexed repository")
}
