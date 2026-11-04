package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/pathkey"
)

// The old physical spelling remains the oracle. Sharing the already checked
// root prefix must not change aliases, a missing suffix, Unicode or leaf policy.
func TestPendingTextMutationKeyMatchesPhysicalOracle(t *testing.T) {
	for _, mode := range []string{"regular", "missing_leaf", "missing_parent", "parent_alias", "leaf_alias", "root_alias", "unicode"} {
		t.Run(mode, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "repo")
			require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg"), 0o700))
			relative := "pkg/source.go"
			require.NoError(t, os.WriteFile(filepath.Join(root, relative), []byte("package sample\n"), 0o600))
			switch mode {
			case "missing_leaf":
				relative = "pkg/missing.go"
			case "missing_parent":
				relative = "missing/parent/source.go"
			case "parent_alias":
				requirePendingTextTestSymlink(t, filepath.Join(root, "pkg"), filepath.Join(root, "alias"))
				relative = "alias/source.go"
			case "leaf_alias":
				requirePendingTextTestSymlink(t, filepath.Join(root, relative), filepath.Join(root, "alias.go"))
				relative = "alias.go"
			case "root_alias":
				alias := filepath.Join(base, "alias-root")
				requirePendingTextTestSymlink(t, root, alias)
				root = alias
			case "unicode":
				relative = "pkg/caf\u00e9.go"
				require.NoError(t, os.WriteFile(filepath.Join(root, relative), []byte("package sample\n"), 0o600))
			}
			reader, err := openPendingTextRootForScan(root)
			require.NoError(t, err)
			t.Cleanup(func() { _ = reader.close(context.Background()) })
			key, err := reader.mutationKey(t.Context(), relative)
			closeErr := reader.close(t.Context())
			require.NoError(t, err)
			require.NoError(t, closeErr)
			require.Equal(t, pathkey.MutationKey(filepath.Join(root, relative)), key)
			_, err = reader.handle.Stat(".")
			require.Error(t, err)
		})
	}
}

func TestPendingTextMutationKeyRetainsPrefixAndFinalFences(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg", "nested"), 0o700))
	resolved := 0
	reader, err := openPendingTextRootResolved(root, func(path string) (string, error) {
		resolved++
		return filepath.EvalSymlinks(path)
	})
	require.NoError(t, err)
	reader.grouped = true
	t.Cleanup(func() { _ = reader.close(context.Background()) })
	for i := range 4 {
		relative := fmt.Sprintf("pkg/nested/source%d.go", i)
		require.NoError(t, os.WriteFile(filepath.Join(root, relative), []byte("package sample\n"), 0o600))
		key, err := reader.mutationKey(t.Context(), relative)
		require.NoError(t, err)
		require.Equal(t, pathkey.MutationKey(filepath.Join(root, relative)), key)
		require.Len(t, reader.directories, 2, "all borrowed parent identities must remain bound until close")
		require.Equal(t, 1, resolved)
	}
	require.NoError(t, reader.close(t.Context()))
	require.Equal(t, 2, resolved, "the final named-root spelling fence is mandatory")
}

func TestPendingTextMutationKeyRejectsLostAuthorityAndCancellation(t *testing.T) {
	for _, mode := range []string{"parent_rebound", "root_rebound", "cancelled", "not_local"} {
		t.Run(mode, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "repo")
			require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(root, "pkg/source.go"), []byte("package sample\n"), 0o600))
			reader, err := openPendingTextRootForScan(root)
			require.NoError(t, err)
			t.Cleanup(func() { _ = reader.close(context.Background()) })
			_, err = reader.mutationKey(t.Context(), "pkg/source.go")
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			relative := "pkg/source.go"
			var rename pendingTextTestRename
			switch mode {
			case "parent_rebound", "root_rebound":
				moved := filepath.Join(root, "pkg")
				if mode == "root_rebound" {
					moved = root
				}
				if !rename.try(t, moved, moved+".old") {
					require.NoError(t, reader.close(t.Context()))
					rename.afterClose(t)
					return
				}
				require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg"), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(root, "pkg/source.go"), []byte("package sample\n"), 0o600))
			case "cancelled":
				cancel()
			case "not_local":
				relative = "../source.go"
			}
			_, keyErr := reader.mutationKey(ctx, relative)
			closeErr := reader.close(ctx)
			if mode == "root_rebound" {
				require.Error(t, closeErr)
			} else {
				require.Error(t, keyErr)
			}
			if mode == "cancelled" {
				require.ErrorIs(t, keyErr, context.Canceled)
			}
			_, err = reader.handle.Stat(".")
			require.Error(t, err)
		})
	}
}

// A real acknowledged entry cannot transfer its mutation namespace merely
// because a new root alias reaches the same physical bytes and file inode.
func TestPendingTextSelectionRejectsChangedMutationNamespace(t *testing.T) {
	s, _, ctx, path, entry := snapshotFacadeFixture(t)
	files, err := s.pendingTextFiles(ctx, ResolvedScope{}, nil)
	require.NoError(t, err)
	require.Len(t, files, 1)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	root := filepath.Dir(path)
	var rename pendingTextTestRename
	if !rename.try(t, root, root+".old") {
		return
	}
	t.Cleanup(func() { _ = os.Remove(root); _ = os.RemoveAll(root + ".old") })
	requirePendingTextTestSymlink(t, root+".old", root)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.NotEqual(t, entry.key, pathkey.MutationKey(path), "the actual alias changes writer identity despite equal bytes")
	files, err = s.pendingTextFiles(ctx, ResolvedScope{}, nil)
	require.ErrorContains(t, err, "pending text physical identity changed")
	require.Nil(t, files)
}
