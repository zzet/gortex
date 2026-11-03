package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPendingTextRootRejectsDirectoryReplacementWithSameLeaf(t *testing.T) {
	for _, replaceRoot := range []bool{false, true} {
		name := "parent"
		if replaceRoot {
			name = "root"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "repo")
			parent := filepath.Join(root, "pkg")
			require.NoError(t, os.MkdirAll(parent, 0o700))
			path := filepath.Join(parent, "source.go")
			require.NoError(t, os.WriteFile(path, []byte("package fixture\nfunc Current() {}\n"), 0o600))
			reader, err := openPendingTextRoot(root)
			require.NoError(t, err)
			t.Cleanup(func() { _ = reader.close(context.Background()) })
			var rename pendingTextTestRename
			current, _, readErr := reader.read(context.Background(), "pkg/source.go", func() {
				moved := parent
				if replaceRoot {
					moved = root
				}
				if !rename.try(t, moved, moved+".old") {
					return
				}
				require.NoError(t, os.MkdirAll(parent, 0o700))
				old := filepath.Join(parent+".old", "source.go")
				if replaceRoot {
					old = filepath.Join(root+".old", "pkg", "source.go")
				}
				require.NoError(t, os.Link(old, path), "same file inode in a new parent must not preserve path authority")
			}, 1<<20, make([]byte, 32<<10))
			closeErr := reader.close(context.Background())
			if rename.blocked {
				require.NoError(t, readErr)
				require.NoError(t, closeErr)
				require.Equal(t, []byte("package fixture\nfunc Current() {}\n"), current)
				rename.afterClose(t)
				_, err = reader.handle.Stat(".")
				require.Error(t, err)
				return
			}
			require.Error(t, readErr, "unchanged leaf inode does not prove its parent/root identity")
			if replaceRoot {
				require.Error(t, closeErr, "final root fence must reject even an otherwise complete result")
			}
		})
	}
}

func TestPendingTextRootFinalFenceRejectsRebindingWithoutReads(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	require.NoError(t, os.Mkdir(root, 0o700))
	reader, err := openPendingTextRoot(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.close(context.Background()) })
	var rename pendingTextTestRename
	if !rename.try(t, root, root+".old") {
		require.NoError(t, reader.close(context.Background()))
		rename.afterClose(t)
		_, err = reader.handle.Stat(".")
		require.Error(t, err)
		return
	}
	require.NoError(t, os.Mkdir(root, 0o700))
	require.Error(t, reader.close(context.Background()), "empty selection must not bypass final root identity")
}

func TestPendingTextRootAmortizesOnlyPrefixResolution(t *testing.T) {
	root := t.TempDir()
	calls := 0
	reader, err := openPendingTextRootResolved(root, func(path string) (string, error) {
		calls++
		return filepath.EvalSymlinks(path)
	})
	require.NoError(t, err)
	scratch := make([]byte, 32<<10)
	for i := range 4 {
		relative := fmt.Sprintf("source-%d.go", i)
		content := []byte(fmt.Sprintf("package fixture\n// useful source %d\nfunc Source%d() {}\n", i, i))
		require.NoError(t, os.WriteFile(filepath.Join(root, relative), content, 0o600))
		raw, evidence, err := reader.read(context.Background(), relative, nil, 1<<20, scratch)
		require.NoError(t, err)
		require.Equal(t, content, raw)
		require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(content)), evidence.contentSHA256)
		require.Equal(t, len(content), evidence.byteCount)
		require.Equal(t, 1, calls, "per-file reads must not resolve the absolute prefix again")
	}
	require.NoError(t, reader.close(context.Background()))
	require.Equal(t, 2, calls, "one final root spelling revalidation is mandatory")
	_, err = reader.handle.Stat(".")
	require.Error(t, err, "root descriptor must be closed")
}

func TestPendingTextRootRetainsPhysicalVersionAndCancellationProof(t *testing.T) {
	for _, mode := range []string{"same_size_restored_mtime", "same_bytes_new_inode", "parent_symlink_inserted", "leaf_symlink_inserted", "canceled", "bound", "missing"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, "pkg")
			require.NoError(t, os.Mkdir(parent, 0o700))
			path := filepath.Join(parent, "source.go")
			content := bytes.Repeat([]byte("package fixture\n"), 256)
			require.NoError(t, os.WriteFile(path, content, 0o600))
			before, err := os.Stat(path)
			require.NoError(t, err)
			reader, err := openPendingTextRoot(root)
			require.NoError(t, err)
			t.Cleanup(func() { _ = reader.close(context.Background()) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			budget := int64(1 << 20)
			if mode == "bound" {
				budget = 2
			}
			if mode == "missing" {
				require.NoError(t, os.Remove(path))
			}
			var rename pendingTextTestRename
			current, _, readErr := reader.read(ctx, "pkg/source.go", func() {
				switch mode {
				case "same_size_restored_mtime":
					require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("b"), len(content)), 0o600))
					require.NoError(t, os.Chtimes(path, before.ModTime(), before.ModTime()))
				case "same_bytes_new_inode":
					if !rename.try(t, path, path+".old") {
						return
					}
					require.NoError(t, os.WriteFile(path, content, 0o600))
				case "parent_symlink_inserted":
					if !rename.try(t, parent, parent+".old") {
						return
					}
					if err := os.Symlink(parent+".old", parent); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
				case "leaf_symlink_inserted":
					if !rename.try(t, path, path+".old") {
						return
					}
					if err := os.Symlink(path+".old", path); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
				case "canceled":
					cancel()
				}
			}, budget, make([]byte, 32<<10))
			closeErr := reader.close(ctx)
			if rename.blocked {
				require.NoError(t, readErr)
				require.NoError(t, closeErr)
				require.Equal(t, content, current)
				rename.afterClose(t)
				_, err = reader.handle.Stat(".")
				require.Error(t, err)
				return
			}
			require.Error(t, readErr)
			if mode == "canceled" {
				require.ErrorIs(t, readErr, context.Canceled)
				require.ErrorIs(t, closeErr, context.Canceled)
			} else {
				require.NoError(t, closeErr)
			}
			_, err = reader.handle.Stat(".")
			require.Error(t, err, "all refusal paths must close the request descriptor")
		})
	}
}

func TestPendingTextRootPreservesLegacyAliasReads(t *testing.T) {
	for _, mode := range []string{"parent", "leaf", "root"} {
		t.Run(mode, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "repo")
			require.NoError(t, os.MkdirAll(filepath.Join(root, "actual"), 0o700))
			content := []byte("package fixture\nfunc Current() {}\n")
			target := filepath.Join(root, "actual", "source.go")
			require.NoError(t, os.WriteFile(target, content, 0o600))
			relative := "actual/source.go"
			switch mode {
			case "parent":
				if err := os.Symlink(filepath.Join(root, "actual"), filepath.Join(root, "alias")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
				relative = "alias/source.go"
			case "leaf":
				if err := os.Symlink(target, filepath.Join(root, "alias.go")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
				relative = "alias.go"
			case "root":
				alias := filepath.Join(base, "alias")
				if err := os.Symlink(root, alias); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
				root = alias
			}
			reader, err := openPendingTextRoot(root)
			require.NoError(t, err)
			raw, evidence, err := reader.read(context.Background(), relative, nil, 1<<20, make([]byte, 32<<10))
			require.NoError(t, err)
			require.Equal(t, content, raw)
			resolved, err := filepath.EvalSymlinks(target)
			require.NoError(t, err)
			require.Equal(t, resolved, evidence.resolvedPath)
			require.NoError(t, reader.close(context.Background()))
		})
	}
}

func TestPendingTextRootWalkUsesSingleComponentOperations(t *testing.T) {
	const depth = 20
	root := t.TempDir()
	relative := filepath.Join(strings.Repeat("p"+string(filepath.Separator), depth), "source.go")
	path := filepath.Join(root, relative)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	content := []byte("package fixture\n// bounded physical path control\nfunc Current() {}\n")
	require.NoError(t, os.WriteFile(path, content, 0o600))
	reader, err := openPendingTextRoot(root)
	require.NoError(t, err)
	var multi []string
	metadataCalls := 0
	var opened []*os.Root
	reader.lstat = func(dir *os.Root, name string) (os.FileInfo, error) {
		metadataCalls++
		if filepath.Base(name) != name {
			multi = append(multi, name)
		}
		return dir.Lstat(name)
	}
	reader.openRoot = func(dir *os.Root, name string) (*os.Root, error) {
		if filepath.Base(name) != name {
			multi = append(multi, name)
		}
		next, err := dir.OpenRoot(name)
		if err == nil {
			opened = append(opened, next)
		}
		return next, err
	}
	raw, _, readErr := reader.read(context.Background(), relative, nil, 4096, make([]byte, 32<<10))
	closeErr := reader.close(context.Background())
	require.NoError(t, readErr)
	require.NoError(t, closeErr)
	require.Equal(t, content, raw)
	for _, dir := range opened {
		_, err := dir.Stat(".")
		require.Error(t, err, "every actual transient root must be closed")
	}
	require.Empty(t, multi, "a confined proof must not restart traversal through cumulative prefixes")
	require.Len(t, opened, 2*depth, "exactly one before/after confined open per parent")
	require.Equal(t, 2*(depth+1), metadataCalls, "every parent and leaf retains its before/after no-follow check")
}

func TestPendingTextRootWalkerClosesTransientRootsOnRefusal(t *testing.T) {
	for _, mode := range []string{"open_failure", "cancel", "inserted_escape", "non_regular"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			relative := filepath.Join("a", "b", "c", "d", "source.go")
			path := filepath.Join(root, relative)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte("package fixture\nfunc Current() {}\n"), 0o600))
			if mode == "non_regular" {
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Mkdir(path, 0o700))
			}
			reader, err := openPendingTextRoot(root)
			require.NoError(t, err)
			var opened []*os.Root
			calls, maxLive := 0, 0
			insertedEscape := false
			sentinel := errors.New("controlled actual walker-open failure")
			external := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(external, "c", "d"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(external, "c", "d", "source.go"), []byte("foreign bytes"), 0o600))
			if mode == "inserted_escape" {
				probe := filepath.Join(root, "symlink-probe")
				if err := os.Symlink(external, probe); err != nil {
					_ = reader.close(context.Background())
					t.Skipf("symlink unavailable: %v", err)
				}
				require.NoError(t, os.Remove(probe))
			}
			reader.openRoot = func(dir *os.Root, name string) (*os.Root, error) {
				calls++
				if mode == "open_failure" && calls == 3 {
					return nil, sentinel
				}
				if mode == "inserted_escape" && calls == 2 {
					target := filepath.Join(root, "a", "b")
					if err := os.Rename(target, target+".old"); err != nil {
						return nil, err
					}
					if err := os.Symlink(external, target); err != nil {
						return nil, err
					}
					insertedEscape = true
				}
				next, err := dir.OpenRoot(name)
				if err == nil {
					opened = append(opened, next)
					live := 0
					for _, owned := range opened {
						if _, err := owned.Stat("."); err == nil {
							live++
						}
					}
					maxLive = max(maxLive, live)
				}
				return next, err
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var after func()
			if mode == "cancel" {
				after = cancel
			}
			_, _, readErr := reader.read(ctx, relative, after, 4096, make([]byte, 32<<10))
			_, anchorErr := reader.handle.Stat(".")
			closeErr := reader.close(ctx)
			require.NoError(t, anchorErr, "walker failure must not close the group's anchor")
			require.Error(t, readErr)
			switch mode {
			case "open_failure":
				require.ErrorIs(t, readErr, sentinel)
				require.NoError(t, closeErr)
			case "cancel":
				require.ErrorIs(t, readErr, context.Canceled)
				require.ErrorIs(t, closeErr, context.Canceled)
			case "inserted_escape":
				require.True(t, insertedEscape, "the escaping parent was actually installed")
				require.NoError(t, closeErr)
			case "non_regular":
				require.NoError(t, closeErr)
			}
			for _, dir := range opened {
				_, err := dir.Stat(".")
				require.Error(t, err, "failure must close every actual transient handle")
			}
			require.LessOrEqual(t, maxLive, 2, "at most a current walker and its short transition coexist")
		})
	}
}
