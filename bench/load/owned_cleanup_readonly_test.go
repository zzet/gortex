package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPrivateCleanupRemovesReadOnlyOwnedModuleCache(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires POSIX directory permissions enforced for the owning user")
	}
	root := t.TempDir()
	module := filepath.Join(root, "home", "go", "pkg", "mod", "example.test", "module@v1.0.0")
	if err := os.MkdirAll(module, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(path, 0o700)
			}
			return err
		})
	})
	if err := os.WriteFile(filepath.Join(module, "LICENSE"), []byte("fixture"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(module, 0o500); err != nil {
		t.Fatal(err)
	}
	retained := filepath.Join(root, "daemon-1.stderr.log")
	if err := os.WriteFile(retained, []byte("retained evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &privateRun{Root: root}
	if err := p.removeState(); err != nil {
		t.Fatalf("cleanup must remove owned read-only module directories: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "home")); !os.IsNotExist(err) {
		t.Fatalf("owned module cache remained after cleanup: %v", err)
	}
	if data, err := os.ReadFile(retained); err != nil || string(data) != "retained evidence" {
		t.Fatalf("cleanup removed evidence: %q %v", data, err)
	}
}

func TestPrivateCleanupDoesNotFollowOwnedStateSymlinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	file := filepath.Join(outside, "retained")
	if err := os.WriteFile(file, []byte("outside"), 0o400); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "home")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := (&privateRun{Root: root}).removeState(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(file)
	if err != nil || after.Mode() != before.Mode() {
		t.Fatalf("cleanup changed an external symlink target: %v", err)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "outside" {
		t.Fatalf("cleanup touched external bytes: %q %v", data, err)
	}
}

func TestPrivateCleanupRejectsReplacedRunRoot(t *testing.T) {
	outside := t.TempDir()
	home := filepath.Join(outside, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "replaced")
	if err := os.Symlink(outside, root); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := (&privateRun{Root: root}).removeState(); err == nil {
		t.Fatal("cleanup accepted a symlink in place of the owned run root")
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("cleanup touched state outside the owned run: %v", err)
	}
}
