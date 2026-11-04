package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPendingTextScanRootSharesSiblingDirectoryDescriptors(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "pkg", "deep")
	require.NoError(t, os.MkdirAll(parent, 0o700))
	raw := []byte("package fixture\nfunc Current() int { return 7 }\n")
	for i := range 32 {
		require.NoError(t, os.WriteFile(filepath.Join(parent, fmt.Sprintf("source-%02d.go", i)), raw, 0o600))
	}
	reader, err := openPendingTextRootForScan(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.close(context.Background()) })
	var opened []*os.Root
	reader.openRoot = func(parent *os.Root, name string) (*os.Root, error) {
		child, err := parent.OpenRoot(name)
		if err == nil {
			opened = append(opened, child)
		}
		return child, err
	}
	for i := range 32 {
		current, evidence, readErr := reader.read(t.Context(), fmt.Sprintf("pkg/deep/source-%02d.go", i), nil, 1<<20, make([]byte, 32<<10))
		require.NoError(t, readErr)
		require.Equal(t, raw, current)
		require.Equal(t, len(raw), evidence.byteCount)
	}
	require.NoError(t, reader.close(t.Context()))
	for _, handle := range opened {
		_, statErr := handle.Stat(".")
		require.Error(t, statErr, "every ancestor descriptor closes with its result group")
	}
	t.Logf("actual_ancestor_opens=%d siblings=32 distinct_ancestor_dirs=2", len(opened))
	require.LessOrEqual(t, len(opened), 2, "share ancestor descriptors among sorted sibling files")
}

// The old uncached path may correctly follow the new directory. This control
// forbids only successful bytes from a cached old directory after rebinding;
// current named bytes or an explicit whole-group refusal are both safe.
func TestPendingTextScanRootCannotServeCachedDirectoryAfterRebind(t *testing.T) {
	for _, mode := range []string{"replacement", "inserted_alias"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, "pkg")
			require.NoError(t, os.Mkdir(parent, 0o700))
			old := []byte("package fixture\nfunc Before() int { return 1 }\n")
			next := []byte("package fixture\nfunc Current() int { return 8 }\n")
			for _, name := range []string{"a.go", "b.go"} {
				require.NoError(t, os.WriteFile(filepath.Join(parent, name), old, 0o600))
			}
			reader, err := openPendingTextRootForScan(root)
			require.NoError(t, err)
			t.Cleanup(func() { _ = reader.close(context.Background()) })
			first, _, err := reader.read(t.Context(), "pkg/a.go", nil, 1<<20, nil)
			require.NoError(t, err)
			require.Equal(t, old, first)
			staleDescriptor, err := os.OpenRoot(parent)
			require.NoError(t, err)
			defer staleDescriptor.Close()
			var rename pendingTextTestRename
			if !rename.try(t, parent, parent+".old") {
				current, _, readErr := reader.read(t.Context(), "pkg/b.go", nil, 1<<20, nil)
				require.NoError(t, readErr)
				require.Equal(t, old, current, "denied rename leaves the current named bytes unchanged")
				require.NoError(t, reader.close(t.Context()))
				require.NoError(t, staleDescriptor.Close())
				rename.afterClose(t)
				_, _, readErr = reader.read(t.Context(), "pkg/b.go", nil, 1<<20, nil)
				require.Error(t, readErr, "closed old directory cannot supply a later successful group")
				return
			}
			target := parent
			if mode == "inserted_alias" {
				target = filepath.Join(root, "new-target")
			}
			require.NoError(t, os.Mkdir(target, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(target, "b.go"), next, 0o600))
			if mode == "inserted_alias" {
				requirePendingTextTestSymlink(t, target, parent)
			}
			staleFile, err := staleDescriptor.Open("b.go")
			require.NoError(t, err)
			staleBytes, err := io.ReadAll(staleFile)
			require.NoError(t, err)
			require.NoError(t, staleFile.Close())
			require.Equal(t, old, staleBytes, "actual captured directory retains old bytes after rename")
			named, err := os.ReadFile(filepath.Join(parent, "b.go"))
			require.NoError(t, err)
			require.Equal(t, next, named)
			current, _, readErr := reader.read(t.Context(), "pkg/b.go", nil, 1<<20, nil)
			closeErr := reader.close(t.Context())
			if readErr == nil && closeErr == nil {
				require.True(t, bytes.Equal(next, current), "a successful group must not return old cached-directory bytes")
			}
		})
	}
}

func TestPendingTextScanRootVersionAndClosureControls(t *testing.T) {
	for _, mode := range []string{"same_size_restored_mtime", "same_bytes_new_inode", "parent_replaced", "root_replaced", "inserted_leaf_alias", "canceled", "byte_bound", "cancel_during_open", "ancestor_open_error"} {
		t.Run(mode, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "repo")
			parent := filepath.Join(root, "pkg", "deep")
			require.NoError(t, os.MkdirAll(parent, 0o700))
			path := filepath.Join(parent, "source.go")
			raw := []byte(strings.Repeat("package fixture\n", 64))
			require.NoError(t, os.WriteFile(path, raw, 0o600))
			before, err := os.Stat(path)
			require.NoError(t, err)
			reader, err := openPendingTextRootForScan(root)
			require.NoError(t, err)
			t.Cleanup(func() { _ = reader.close(context.Background()) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var opened []*os.Root
			reader.openRoot = func(parent *os.Root, name string) (*os.Root, error) {
				child, openErr := parent.OpenRoot(name)
				if openErr == nil {
					opened = append(opened, child)
				}
				if mode == "cancel_during_open" {
					cancel()
				}
				if mode == "ancestor_open_error" && openErr == nil {
					return child, errors.New("controlled actual-open refusal")
				}
				return child, openErr
			}
			maxBytes := int64(1 << 20)
			var rename pendingTextTestRename
			if mode == "byte_bound" {
				maxBytes = 2
			}
			current, _, readErr := reader.read(ctx, "pkg/deep/source.go", func() {
				switch mode {
				case "same_size_restored_mtime":
					require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("x"), len(raw)), 0o600))
					require.NoError(t, os.Chtimes(path, before.ModTime(), before.ModTime()))
				case "same_bytes_new_inode":
					if !rename.try(t, path, path+".old") {
						return
					}
					require.NoError(t, os.WriteFile(path, raw, 0o600))
				case "parent_replaced":
					if !rename.try(t, parent, parent+".old") {
						return
					}
					require.NoError(t, os.Mkdir(parent, 0o700))
					require.NoError(t, os.Link(filepath.Join(parent+".old", "source.go"), path))
				case "root_replaced":
					if !rename.try(t, root, root+".old") {
						return
					}
					require.NoError(t, os.MkdirAll(parent, 0o700))
					require.NoError(t, os.Link(filepath.Join(root+".old", "pkg", "deep", "source.go"), path))
				case "inserted_leaf_alias":
					if !rename.try(t, path, path+".old") {
						return
					}
					requirePendingTextTestSymlink(t, path+".old", path)
				case "canceled":
					cancel()
				}
			}, maxBytes, make([]byte, 32<<10))
			closeErr := reader.close(ctx)
			if rename.blocked {
				require.NoError(t, readErr)
				require.NoError(t, closeErr)
				require.Equal(t, raw, current, "kernel-refused replacement preserves current bytes")
				rename.afterClose(t)
			} else {
				require.Error(t, errors.Join(readErr, closeErr), "no complete group after physical/version/path/budget/cancellation refusal")
			}
			for _, handle := range opened {
				_, statErr := handle.Stat(".")
				require.Error(t, statErr, "all actual ancestor handles must close on every failure path")
			}
			_, statErr := reader.handle.Stat(".")
			require.Error(t, statErr)
		})
	}
}

func TestPendingTextScanRootInitialAliasKeepsLegacyBehavior(t *testing.T) {
	for _, mode := range []string{"parent", "leaf", "root"} {
		t.Run(mode, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "repo")
			require.NoError(t, os.MkdirAll(filepath.Join(root, "actual"), 0o700))
			raw := []byte("package fixture\nfunc Current() int { return 8 }\n")
			target := filepath.Join(root, "actual", "source.go")
			require.NoError(t, os.WriteFile(target, raw, 0o600))
			relative := "actual/source.go"
			switch mode {
			case "parent":
				requirePendingTextTestSymlink(t, filepath.Join(root, "actual"), filepath.Join(root, "alias"))
				relative = "alias/source.go"
			case "leaf":
				requirePendingTextTestSymlink(t, target, filepath.Join(root, "alias.go"))
				relative = "alias.go"
			case "root":
				alias := filepath.Join(base, "alias-root")
				requirePendingTextTestSymlink(t, root, alias)
				root = alias
			}
			reader, err := openPendingTextRootForScan(root)
			require.NoError(t, err)
			t.Cleanup(func() { _ = reader.close(context.Background()) })
			current, _, err := reader.read(t.Context(), relative, nil, 1<<20, nil)
			require.NoError(t, err)
			require.Equal(t, raw, current)
			require.NoError(t, reader.close(t.Context()))
		})
	}
}

func TestPendingTextScanRootFinalFenceRejectsRootRebindingWithoutReads(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.Mkdir(root, 0o700))
	reader, err := openPendingTextRootForScan(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.close(context.Background()) })
	var rename pendingTextTestRename
	if !rename.try(t, root, root+".old") {
		require.NoError(t, reader.close(t.Context()))
		rename.afterClose(t)
		_, err = reader.handle.Stat(".")
		require.Error(t, err, "kernel-protected root still closes before it can be renamed")
		return
	}
	require.NoError(t, os.Mkdir(root, 0o700))
	require.Error(t, reader.close(t.Context()), "empty groups still require the named-root fence")
	_, err = reader.handle.Stat(".")
	require.Error(t, err)
}

func requirePendingTextTestSymlink(t *testing.T, target, alias string) {
	t.Helper()
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
}
