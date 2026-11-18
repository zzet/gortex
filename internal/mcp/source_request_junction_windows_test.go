//go:build windows

package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A directory junction inside a sibling repo is followed like a symlink: one
// that stays in the sibling is a sibling read, one into the worktree family's
// primary keeps the source-lane refusal.
func TestWorktreeSessionSiblingReadThroughJunction(t *testing.T) {
	stack := newViewStackWithSibling(t)
	require.NoError(t, os.WriteFile(filepath.Join(stack.worktreeRoot, "keep.go"), []byte("package repo\n\nfunc WorktreeMarker() {}\n"), 0o644))
	inner := filepath.Join(stack.siblingRoot, "inner")
	require.NoError(t, os.Mkdir(inner, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(inner, "inner.go"), []byte("package inner\n\nfunc Inner() {}\n"), 0o644))
	mustJunction(t, inner, filepath.Join(stack.siblingRoot, "junction"))
	mustJunction(t, stack.repoRoot, filepath.Join(stack.siblingRoot, "primary"))

	for _, path := range []string{filepath.Join(stack.siblingRoot, "junction", "inner.go"), "sibling/junction/inner.go"} {
		res, err := stack.callHandler(t, stack.worktreeRoot, "read_file", map[string]any{"path": path}, stack.srv.handleReadFile)
		require.NoError(t, err, path)
		require.False(t, res.IsError, "%s: %s", path, viewResultText(t, res))
		require.Contains(t, viewResultText(t, res), "func Inner()", path)
	}

	for _, path := range []string{filepath.Join(stack.siblingRoot, "primary", "keep.go"), "sibling/primary/keep.go"} {
		res, err := stack.callHandler(t, stack.worktreeRoot, "read_file", map[string]any{"path": path}, stack.srv.handleReadFile)
		require.NoError(t, err, path)
		require.True(t, res.IsError, "%s: %s", path, viewResultText(t, res))
		require.Contains(t, viewResultText(t, res), "outside the selected checkout", path)
		require.NotContains(t, viewResultText(t, res), "func Keeper()", path)
	}
}
