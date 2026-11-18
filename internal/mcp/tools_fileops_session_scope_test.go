package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
)

func TestResolveFilePath_SessionWorkspaceScope(t *testing.T) {
	srv, home, outsider := newIsolationServer(t)
	ctx := sessionCtx("file-path-alpha", home)

	t.Run("shared relative file resolves inside session workspace", func(t *testing.T) {
		abs, rel, err := srv.resolveFilePath(ctx, "main.go")
		require.NoError(t, err)
		require.Equal(t, filepath.Join(home, "main.go"), abs)
		require.Equal(t, "repo-a/main.go", rel)
	})

	t.Run("outsider-only relative file cannot be read", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(outsider, "outside.go"),
			[]byte("package main\n"), 0o644))
		req := mcplib.CallToolRequest{}
		req.Params.Arguments = map[string]any{"path": "outside.go"}
		result, err := srv.handleReadFile(ctx, req)
		require.NoError(t, err)
		require.True(t, result.IsError)
	})

	t.Run("unbound shared relative file stays ambiguous", func(t *testing.T) {
		_, _, err := srv.resolveFilePath(context.Background(), "main.go")
		require.ErrorIs(t, err, errPathUnresolved)
		require.Contains(t, err.Error(), "multiple tracked repos")
	})

	t.Run("new relative file anchors to session workspace", func(t *testing.T) {
		abs, rel, err := srv.resolveFilePath(ctx, "new.go")
		require.NoError(t, err)
		require.Equal(t, filepath.Join(home, "new.go"), abs)
		require.Equal(t, "repo-a/new.go", rel)
	})

	t.Run("untracked session cwd denies inferred anchors", func(t *testing.T) {
		untracked := sessionCtx("file-path-untracked", t.TempDir())
		allowed, bound := srv.sessionWorkspaceRepoSet(untracked)
		require.True(t, bound)
		require.Empty(t, allowed)
		_, _, err := srv.resolveFilePath(untracked, "main.go")
		require.ErrorIs(t, err, errPathUnresolved)
	})
}

func TestResolveFilePath_SharedWorkspaceStaysAmbiguous(t *testing.T) {
	f := newContainedFixture(t)
	ctx := sessionCtx("file-path-shared-workspace", f.repoA)
	allowed, bound := f.srv.sessionWorkspaceRepoSet(ctx)
	require.True(t, bound)
	require.Equal(t, map[string]bool{"repo-a": true, "repo-c": true}, allowed)
	_, _, err := f.srv.resolveFilePath(ctx, "main.go")
	require.ErrorIs(t, err, errPathUnresolved)
	require.Contains(t, err.Error(), "multiple tracked repos")
}

func TestResolveFilePath_ForeignCheckoutAmbiguityDoesNotBlockSession(t *testing.T) {
	outsider, _, srv := setupWorktreePair(t)
	home := wsRepo(t, "home", "home", "HomeThing")
	require.NoError(t, os.WriteFile(filepath.Join(home, "shared.go"),
		[]byte("package main\n\nfunc HomeShared() {}\n"), 0o644))
	_, err := srv.multiIndexer.TrackRepo(config.RepoEntry{Path: home, Name: "home"})
	require.NoError(t, err)
	require.NotEmpty(t, srv.multiIndexer.LinkedWorktreeRoots(outsider))

	ctx := sessionCtx("file-path-home-with-foreign-worktrees", home)
	abs, rel, err := srv.resolveFilePath(ctx, "shared.go")
	require.NoError(t, err)
	require.Equal(t, resolvePath(t, filepath.Join(home, "shared.go")), resolvePath(t, abs))
	require.Equal(t, "home/shared.go", rel)

	// The foreign family's ambiguity must still be enforced for a client
	// with no session workspace, proving the family guard remains active.
	_, _, err = srv.resolveFilePath(context.Background(), "shared.go")
	require.ErrorIs(t, err, errPathAmbiguousCheckout)
}
