package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

func TestPrivateRunSocketOverrideKeepsCleanupInsideOwnedDirectory(t *testing.T) {
	base := shortPrivateSocketBase(t)
	t.Setenv("GORTEX_LOAD_SOCKET_BASE", base)
	before, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(base, "caller-owned-marker")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	p, err := newPrivateRun(ctx, t.TempDir(), "/unused/binary", []Corpus{gitFixture(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(p.SocketDir); _ = p.removeState() })
	if filepath.Dir(p.SocketDir) != base || filepath.Dir(p.Socket) != p.SocketDir || len(p.Socket) >= 104 {
		t.Fatalf("socket escaped owned short root: %+v", p)
	}
	if err := os.RemoveAll(p.SocketDir); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(base)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("cleanup replaced caller root: %v", err)
	}
	value, err := os.ReadFile(marker)
	if err != nil || string(value) != "keep" {
		t.Fatalf("cleanup touched caller-owned sibling: %q %v", value, err)
	}
}

func TestSearchAcceptanceOuterUsesActualOutputFlag(t *testing.T) {
	for _, editors := range []int{1, 4, 8} {
		args := searchAcceptanceArgs(editors, 1000)
		args = append(args, "--search-readback-acceptance", "--edit-mean=1s", "--edit-ack=local", "--out=/owned/report.json")
		parsed, err := parseRun(args)
		if err != nil || parsed.Output != "/owned/report.json" || parsed.EditCount != 1000 || parsed.Editors != editors || !parsed.SearchReadbackAcceptance {
			t.Fatalf("actual per-cell argv refused or altered: %+v %v", parsed, err)
		}
		if _, err := parseRun(append(args, "--output=/owned/invalid.json")); err == nil {
			t.Fatal("obsolete unregistered --output flag unexpectedly accepted")
		}
	}
}
