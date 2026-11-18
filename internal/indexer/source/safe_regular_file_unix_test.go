//go:build unix

package source

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestReadRegularFileFilesystemNonDirectoryAncestorIsNotAbsent(t *testing.T) {
	root := t.TempDir()
	regularWrite(t, root, "nested", "not a directory")
	src := regularFilesystem(t, root)
	data, _, err := ReadRegularFile(t.Context(), src, "nested/go.mod", 64)
	// Legacy root-error mapping may simplify ENOTDIR to ErrNotInSource;
	// the safety contract is that it must never become proven absence.
	if data != nil || err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("non-directory ancestor = %q, %v", data, err)
	}
}

func TestReadRegularFileFilesystemConfinesSwappedParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	regularWrite(t, root, "pkg/go.mod", "module inside\n")
	regularWrite(t, outside, "go.mod", "module outside\n")
	src := regularFilesystem(t, root)
	data, _, err := src.readRegularFileWithOpen(t.Context(), "pkg/go.mod", 64, func(openRoot *os.Root, name string) (*os.File, error) {
		if err := os.Rename(filepath.Join(root, "pkg"), filepath.Join(root, "old-pkg")); err != nil {
			return nil, err
		}
		if err := os.Symlink(outside, filepath.Join(root, "pkg")); err != nil {
			return nil, err
		}
		return openRootRegularFile(openRoot, name)
	})
	if data != nil || !errors.Is(err, ErrOutsideRoot) || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("swapped outside parent = %q, %v", data, err)
	}
}

func TestReadRegularFileFilesystemSymlinksAndFIFO(t *testing.T) {
	root := t.TempDir()
	regularWrite(t, root, "pkg/go.mod", "module example.org/pkg\n")
	src := regularFilesystem(t, root)
	if err := os.Symlink("pkg/go.mod", filepath.Join(root, "link.mod")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pkg", filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "fifo.mod"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("fifo.mod", filepath.Join(root, "fifo-link.mod")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"link.mod", "fifo.mod", "fifo-link.mod"} {
		data, _, err := ReadRegularFile(t.Context(), src, name, 64)
		if data != nil || !errors.Is(err, ErrNotRegularFile) || errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("read(%s)=%q,%v", name, data, err)
		}
	}
	data, _, err := ReadRegularFile(t.Context(), src, "linked/go.mod", 64)
	if err != nil || string(data) != "module example.org/pkg\n" {
		t.Fatalf("confined ancestor=%q,%v", data, err)
	}
}

func TestReadRegularFileFilesystemRevalidatesSwappedFIFO(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "go.mod")
	regularWrite(t, root, "go.mod", "module initial\n")
	src := regularFilesystem(t, root)
	type outcome struct {
		data []byte
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		data, _, err := src.readRegularFileWithOpen(t.Context(), "go.mod", 64, func(root *os.Root, name string) (*os.File, error) {
			if err := os.Remove(filePath); err != nil {
				return nil, err
			}
			if err := unix.Mkfifo(filePath, 0o600); err != nil {
				return nil, err
			}
			return openRootRegularFile(root, name)
		})
		done <- outcome{data, err}
	}()
	select {
	case got := <-done:
		if got.data != nil || !errors.Is(got.err, ErrNotRegularFile) {
			t.Fatalf("swapped FIFO=%q,%v", got.data, got.err)
		}
	case <-time.After(2 * time.Second):
		// Unblock a regressed opener and join it before failing the test.
		fd, err := unix.Open(filePath, unix.O_RDWR|unix.O_NONBLOCK, 0)
		if err != nil {
			t.Fatalf("unblock FIFO regression: %v", err)
		}
		defer unix.Close(fd)
		select {
		case <-done:
			t.Fatal("safe read blocked opening a replaced FIFO")
		case <-time.After(5 * time.Second):
			t.Fatal("safe read did not unwind after FIFO release")
		}
	}
}
func TestReadRegularFileFilesystemRevalidatesSwappedSymlinkAndSize(t *testing.T) {
	for _, kind := range []string{"symlink", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			filePath := filepath.Join(root, "go.mod")
			regularWrite(t, root, "go.mod", "module initial\n")
			regularWrite(t, root, "target.mod", "module target\n")
			src := regularFilesystem(t, root)
			want := ErrNotRegularFile
			if kind == "oversize" {
				want = ErrRegularFileTooLarge
			}
			data, _, err := src.readRegularFileWithOpen(t.Context(), "go.mod", 64, func(root *os.Root, name string) (*os.File, error) {
				if err := os.Remove(filePath); err != nil {
					return nil, err
				}
				if kind == "symlink" {
					if err := os.Symlink("target.mod", filePath); err != nil {
						return nil, err
					}
				} else if err := os.WriteFile(filePath, make([]byte, 128), 0o644); err != nil {
					return nil, err
				}
				return openRootRegularFile(root, name)
			})
			if data != nil || !errors.Is(err, want) || errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("swapped %s=%q,%v; want %v", kind, data, err, want)
			}
		})
	}
}
