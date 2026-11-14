//go:build windows

package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// mustJunction creates a directory junction. Unlike a symlink it needs no
// privilege, so these tests run on every Windows host.
func mustJunction(t *testing.T, target, link string) {
	t.Helper()
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	require.NoError(t, err, "mklink /J %s %s: %s", link, target, out)
}

// TestJunctionGuardRefusesEscape is the junction form of
// TestSymlinkGuardRefusesEscape. filepath.EvalSymlinks fails on a path through
// a junction, and the guard's fallback for a missing file then kept the lexical
// in-repo spelling, so reads and writes beneath a junction out of the repo were
// admitted.
func TestJunctionGuardRefusesEscape(t *testing.T) {
	repoRoot := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("TOP-SECRET-VALUE"), 0o600))
	pkg := filepath.Join(repoRoot, "pkg")
	require.NoError(t, os.Mkdir(pkg, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pkg, "ok.go"), []byte("package x\n"), 0o644))
	mustJunction(t, outside, filepath.Join(repoRoot, "out"))
	mustJunction(t, pkg, filepath.Join(repoRoot, "in"))

	srv := newReadGuardServer(t, repoRoot)
	ctx := context.Background()

	for _, path := range []string{
		filepath.Join(repoRoot, "out", "secret.txt"),
		filepath.Join(repoRoot, "out", "new.txt"),
		filepath.Join(repoRoot, "out", "new-dir", "new.txt"),
		"out/secret.txt",
	} {
		_, _, err := srv.resolveFilePath(ctx, path)
		require.ErrorIs(t, err, errPathEscape, "%s must be refused", path)
	}

	res := readFileTool(t, srv, map[string]any{"path": filepath.Join(repoRoot, "out", "secret.txt")})
	require.True(t, res.IsError, "a read through a junction out of the repo must be refused")
	require.NotContains(t, readText(t, res), "TOP-SECRET-VALUE")

	for _, path := range []string{
		filepath.Join(repoRoot, "pkg", "ok.go"),
		filepath.Join(repoRoot, "in", "ok.go"),
		filepath.Join(repoRoot, "in", "new.go"),
	} {
		_, _, err := srv.resolveFilePath(ctx, path)
		require.NoError(t, err, "%s stays inside the repo", path)
	}
}

// TestJunctionGuardAllowsJunctionedRoot covers a repository reached through a
// junction: its own files resolve through that junction, so the roots they are
// compared with must resolve through it too.
func TestJunctionGuardAllowsJunctionedRoot(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("TOP-SECRET-VALUE"), 0o600))
	base := t.TempDir()
	realRoot := filepath.Join(base, "real", "repo")
	require.NoError(t, os.MkdirAll(realRoot, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(realRoot, "ok.go"), []byte("package x\n"), 0o644))
	mustJunction(t, outside, filepath.Join(realRoot, "out"))
	mustJunction(t, filepath.Join(base, "real"), filepath.Join(base, "alias"))
	mustJunction(t, realRoot, filepath.Join(base, "direct"))

	ctx := context.Background()
	for _, root := range []string{filepath.Join(base, "alias", "repo"), filepath.Join(base, "direct")} {
		srv := newReadGuardServer(t, root)

		for _, name := range []string{"ok.go", "new.go"} {
			_, _, err := srv.resolveFilePath(ctx, filepath.Join(root, name))
			require.NoError(t, err, "%s belongs to the repo at %s", name, root)
		}
		_, _, err := srv.resolveFilePath(ctx, filepath.Join(root, "out", "secret.txt"))
		require.ErrorIs(t, err, errPathEscape, "a junction out of the repo at %s must be refused", root)
	}
}

// TestBatchLifecycleDestinationRefusesJunction holds a junction to the same rule
// as a symlink: a move destination must not be redirected through either, even
// when the target is inside the repository.
func TestBatchLifecycleDestinationRefusesJunction(t *testing.T) {
	repoRoot := t.TempDir()
	realParent := filepath.Join(repoRoot, "real-parent")
	require.NoError(t, os.Mkdir(realParent, 0o755))
	linkedParent := filepath.Join(repoRoot, "linked-parent")
	mustJunction(t, realParent, linkedParent)

	srv := newReadGuardServer(t, repoRoot)
	ctx := context.Background()

	err := srv.guardBatchLifecycleDestination(ctx, filepath.Join(linkedParent, "destination.txt"))
	require.ErrorContains(t, err, "move destination contains symlink component")
	require.NoError(t, srv.guardBatchLifecycleDestination(ctx, filepath.Join(realParent, "destination.txt")))
}
