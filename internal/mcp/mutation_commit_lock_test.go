package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The commit boundary must prove ownership of this path, rather than relying
// on every current and future caller to remember a separate lock convention.
func TestCommitFileMutationRefusesUnheldSourcePath(t *testing.T) {
	srv, dir := setupTestServer(t)
	path := filepath.Join(dir, "main.go")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	_, err = srv.commitFileMutation(t.Context(), "edit_file", "unheld", "unheld", "main.go", path, []byte("package main\n"), 0o644)
	require.Error(t, err, "an unheld source path must refuse before disk mutation")
	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, before, after, "a refused unheld commit must preserve exact bytes")
}

func TestMutationCommitRequiresLiveCapturedPathAdmission(t *testing.T) {
	srv, dir := setupTestServer(t)
	path := filepath.Join(dir, "main.go")
	ctx, release, err := acquireMutationPath(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(release)
	const accepted = "package main\nfunc Accepted() {}\n"
	_, err = srv.commitFileMutation(ctx, "edit_file", "held", "held", "main.go", path, []byte(accepted), 0o644)
	require.NoError(t, err)
	current, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, accepted, string(current))

	// A context saved by a returned handler is not a permanent capability.
	release()
	_, err = srv.commitFileMutation(ctx, "edit_file", "released", "released", "main.go", path, []byte("package main\n"), 0o644)
	require.ErrorIs(t, err, errMutationNotApplied)
	current, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, accepted, string(current))

	otherCtx, otherRelease, err := acquireMutationPath(t.Context(), filepath.Join(dir, "other.go"))
	require.NoError(t, err)
	defer otherRelease()
	_, err = srv.commitFileMutation(otherCtx, "edit_file", "wrong-path", "wrong-path", "main.go", path, []byte("package main\n"), 0o644)
	require.ErrorIs(t, err, errMutationNotApplied)
}

func TestMutationCommitInheritedAdmissionHonorsParentLifetime(t *testing.T) {
	srv, dir := setupTestServer(t)
	path := filepath.Join(dir, "main.go")
	parent, release, err := acquireMutationPath(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(release)
	// A nested helper can reuse its caller's set without reacquiring the same
	// non-reentrant token. Releasing the child never releases the parent.
	child, childRelease, err := acquireMutationPaths(parent, []string{path, path})
	require.NoError(t, err)
	childRelease()
	_, err = srv.commitFileMutation(child, "edit_file", "parent-held", "parent-held", "main.go", path, []byte("package main\nfunc Nested() {}\n"), 0o644)
	require.NoError(t, err)
	release()
	_, err = srv.commitFileMutation(child, "edit_file", "parent-returned", "parent-returned", "main.go", path, []byte("package main\n"), 0o644)
	require.ErrorIs(t, err, errMutationNotApplied)
}

func TestMutationCommitRefusesRetargetedParentAlias(t *testing.T) {
	srv, dir := setupTestServer(t)
	originalDir, otherDir, aliasDir := filepath.Join(dir, "original"), filepath.Join(dir, "other"), filepath.Join(dir, "alias")
	require.NoError(t, os.MkdirAll(originalDir, 0o755))
	require.NoError(t, os.MkdirAll(otherDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(originalDir, "file.go"), []byte("package main\nfunc Original() {}\n"), 0o644))
	other := filepath.Join(otherDir, "file.go")
	const untouched = "package main\nfunc Other() {}\n"
	require.NoError(t, os.WriteFile(other, []byte(untouched), 0o644))
	if err := os.Symlink(originalDir, aliasDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	aliasPath := filepath.Join(aliasDir, "file.go")
	ctx, release, err := acquireMutationPath(t.Context(), aliasPath)
	require.NoError(t, err)
	defer release()
	require.NoError(t, os.Remove(aliasDir))
	require.NoError(t, os.Symlink(otherDir, aliasDir))
	_, err = srv.commitFileMutation(ctx, "edit_file", "retargeted", "retargeted", "alias/file.go", aliasPath, []byte("package main\n"), 0o644)
	require.ErrorIs(t, err, errMutationNotApplied)
	info, err := os.Lstat(aliasDir)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink)
	current, err := os.ReadFile(other)
	require.NoError(t, err)
	require.Equal(t, untouched, string(current))
}

func TestMutationCommitCanceledAdmissionWritesNothing(t *testing.T) {
	srv, dir := setupTestServer(t)
	path := filepath.Join(dir, "main.go")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	parent, cancel := context.WithCancel(t.Context())
	ctx, release, err := acquireMutationPath(parent, path)
	require.NoError(t, err)
	defer release()
	cancel()
	_, err = srv.commitFileMutation(ctx, "edit_file", "canceled-held", "canceled-held", "main.go", path, []byte("package main\n"), 0o644)
	require.ErrorIs(t, err, context.Canceled)
	current, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, current)
}
