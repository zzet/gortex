//go:build windows

package source

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestFilesystemSafeRegularFileWindows(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := NewFilesystemSource(root)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	t.Run("regular_manifest", func(t *testing.T) {
		data, meta, err := src.ReadRegularFile(context.Background(), "go.mod", 64)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "module example.test/fixture\n" || !meta.Mode.IsRegular() {
			t.Fatalf("regular manifest: data=%q mode=%v", data, meta.Mode)
		}
	})
	t.Run("missing_manifest", func(t *testing.T) {
		_, _, err := src.ReadRegularFile(context.Background(), "missing/go.mod", 64)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing parent: got %v, want fs.ErrNotExist", err)
		}
		// This existing parent reaches the native leaf open and checks that
		// NtCreateFile's NTSTATUS is converted to a caller-visible absence.
		file, err := openRootRegularFile(src.root, "missing.mod")
		if file != nil {
			_ = file.Close()
			t.Fatal("missing leaf unexpectedly opened")
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing leaf: got %v, want fs.ErrNotExist", err)
		}
	})
	t.Run("nonregular_directory", func(t *testing.T) {
		_, _, err := src.ReadRegularFile(context.Background(), "directory", 64)
		if !errors.Is(err, ErrNotRegularFile) {
			t.Fatalf("directory: got %v, want ErrNotRegularFile", err)
		}
	})
	t.Run("cancel_and_size", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, err := src.ReadRegularFile(ctx, "go.mod", 64)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled read: got %v", err)
		}
		_, _, err = src.ReadRegularFile(context.Background(), "go.mod", 3)
		if !errors.Is(err, ErrRegularFileTooLarge) {
			t.Fatalf("oversize manifest: got %v", err)
		}
	})
	t.Run("final_reparse_point", func(t *testing.T) {
		if err := os.Symlink(filepath.Join(root, "go.mod"), filepath.Join(root, "alias.mod")); err != nil {
			t.Skipf("Windows symlink privilege unavailable: %v", err)
		}
		// Exercise the native opener directly: the public read's initial Lstat
		// already rejects a stable link before NtCreateFile reaches the leaf.
		file, err := openRootRegularFile(src.root, "alias.mod")
		if file != nil {
			_ = file.Close()
			t.Fatal("native opener followed a final reparse point")
		}
		if !errors.Is(err, ErrNotRegularFile) {
			t.Fatalf("final reparse point: got %v, want ErrNotRegularFile", err)
		}
	})
	t.Run("in_root_parent_link", func(t *testing.T) {
		realDir := filepath.Join(root, "real")
		if err := os.Mkdir(realDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(realDir, "go.mod"), []byte("module example.test/inner\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realDir, filepath.Join(root, "inside")); err != nil {
			t.Skipf("Windows directory symlink privilege unavailable: %v", err)
		}
		data, _, err := src.ReadRegularFile(context.Background(), "inside/go.mod", 64)
		if err != nil || string(data) != "module example.test/inner\n" {
			t.Fatalf("confined parent link: data=%q err=%v", data, err)
		}
	})
	t.Run("outside_root_parent_link", func(t *testing.T) {
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "go.mod"), []byte("module example.test/outside\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
			t.Skipf("Windows directory symlink privilege unavailable: %v", err)
		}
		_, _, err := src.ReadRegularFile(context.Background(), "escape/go.mod", 64)
		if !errors.Is(err, ErrOutsideRoot) {
			t.Fatalf("outside parent link: got %v, want ErrOutsideRoot", err)
		}
	})
}
