package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func shortPrivateSocketBase(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp(os.TempDir(), "ls-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	return root
}

func TestPrivateSocketBaseDefaultCompatibility(t *testing.T) {
	t.Setenv("GORTEX_LOAD_SOCKET_BASE", "")
	got, err := privateRunSocketBase()
	want := "/tmp"
	if runtime.GOOS == "windows" {
		want = os.TempDir()
	}
	if err != nil || got != want {
		t.Fatalf("default changed: got=%q err=%v want=%q", got, err, want)
	}
}

func TestPrivateSocketBaseHonorsExplicitShortDirectory(t *testing.T) {
	root := shortPrivateSocketBase(t)
	t.Setenv("GORTEX_LOAD_SOCKET_BASE", root)
	got, err := privateRunSocketBase()
	if err != nil || got != root {
		t.Fatalf("explicit owned socket base ignored: got=%q err=%v want=%q", got, err, root)
	}
}

func TestPrivateSocketBaseRefusesUnsafeOverride(t *testing.T) {
	root := shortPrivateSocketBase(t)
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	long := filepath.Join(root, strings.Repeat("x", 104))
	if err := os.Mkdir(long, 0700); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"relative-socket-root", filepath.Join(root, "missing"), file, long} {
		t.Run(filepath.Base(value), func(t *testing.T) {
			t.Setenv("GORTEX_LOAD_SOCKET_BASE", value)
			if got, err := privateRunSocketBase(); err == nil || got != "" {
				t.Fatalf("unsafe or overlong override accepted: %q -> %q (%v)", value, got, err)
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		alias := filepath.Join(root, "alias")
		if err := os.Symlink(root, alias); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Setenv("GORTEX_LOAD_SOCKET_BASE", alias)
		if got, err := privateRunSocketBase(); err == nil || got != "" {
			t.Fatalf("socket base followed alias: %q (%v)", got, err)
		}
	})
}
