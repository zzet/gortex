package pathkey

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCanonicalHasPathPrefixResolvesRootAndCWDAliases(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	realRepo := filepath.Join(realRoot, "repo")
	realCWD := filepath.Join(realRepo, "nested")
	if err := os.MkdirAll(realCWD, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(base, "alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	aliasRepo := filepath.Join(aliasRoot, "repo")
	aliasCWD := filepath.Join(aliasRepo, "nested")

	for _, tc := range []struct {
		name string
		root string
		cwd  string
	}{
		{name: "canonical root canonical cwd", root: realRepo, cwd: realCWD},
		{name: "canonical root alias cwd", root: realRepo, cwd: aliasCWD},
		{name: "alias root canonical cwd", root: aliasRepo, cwd: realCWD},
		{name: "alias root alias cwd", root: aliasRepo, cwd: aliasCWD},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !CanonicalHasPathPrefix(tc.cwd, tc.root) {
				t.Fatalf("CanonicalHasPathPrefix(%q, %q) = false", tc.cwd, tc.root)
			}
		})
	}

	wantRepo := CanonicalExistingRoot(realRepo)
	if got := CanonicalExistingRoot(aliasRepo); got != wantRepo {
		t.Fatalf("CanonicalExistingRoot(%q) = %q, want %q", aliasRepo, got, wantRepo)
	}
}

func TestCanonicalExistingRootFallsBackForMissingRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing", "..", "offline")
	want, err := filepath.Abs(missing)
	if err != nil {
		t.Fatal(err)
	}
	want = NormalizeVolume(filepath.Clean(want))
	if got := CanonicalExistingRoot(missing); got != want {
		t.Fatalf("CanonicalExistingRoot(%q) = %q, want fallback %q", missing, got, want)
	}
}

func TestCanonicalHasPathPrefixRetainsOfflineChildOfAlias(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(base, "alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// EvalSymlinks succeeds for aliasRoot but fails for its absent child. A
	// canonicalize-first comparison therefore mixes realRoot with aliasRoot
	// and loses a checkout precisely when disappearance cleanup needs to find
	// it. Lexical containment remains authoritative for the same spelling.
	offlineChild := filepath.Join(aliasRoot, "removed-worktree")
	if _, err := os.Stat(offlineChild); !os.IsNotExist(err) {
		t.Fatalf("offline fixture unexpectedly exists: %v", err)
	}
	if !CanonicalHasPathPrefix(offlineChild, aliasRoot) {
		t.Fatalf("offline child %q escaped aliased root %q", offlineChild, aliasRoot)
	}
	if CanonicalHasPathPrefix(filepath.Join(base, "alias-sibling"), aliasRoot) {
		t.Fatal("lexical fast path ignored the path-component boundary")
	}
}

func TestCanonicalHasPathPrefixResolvesMissingAliasLeaf(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	realRepo := filepath.Join(realRoot, "repo")
	if err := os.MkdirAll(realRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(base, "alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	missing := filepath.Join(aliasRoot, "repo", "new", "buffer.go")
	if !CanonicalHasPathPrefix(missing, realRepo) {
		t.Fatalf("missing aliased leaf %q escaped canonical root %q", missing, realRepo)
	}
	want := CanonicalPath(filepath.Join(realRepo, "new", "buffer.go"))
	if got := CanonicalPath(missing); got != want {
		t.Fatalf("CanonicalPath(%q) = %q, want %q", missing, got, want)
	}
}

func TestCanonicalExistingRootDarwinTmpAlias(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS /tmp alias regression")
	}
	root, err := os.MkdirTemp("/tmp", "gortex-pathkey-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	want = NormalizeVolume(filepath.Clean(want))
	if got := CanonicalExistingRoot(root); got != want {
		t.Fatalf("CanonicalExistingRoot(%q) = %q, want %q", root, got, want)
	}
}

func BenchmarkCanonicalHasPathPrefixCanonical(b *testing.B) {
	base := b.TempDir()
	realRepo := filepath.Join(base, "real", "repo")
	realCWD := filepath.Join(realRepo, "nested")
	if err := os.MkdirAll(realCWD, 0o755); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if !CanonicalHasPathPrefix(realCWD, realRepo) {
			b.Fatal("canonical cwd unexpectedly escaped root")
		}
	}
}

func BenchmarkCanonicalHasPathPrefixAlias(b *testing.B) {
	base := b.TempDir()
	realRepo := filepath.Join(base, "real", "repo")
	if err := os.MkdirAll(filepath.Join(realRepo, "nested"), 0o755); err != nil {
		b.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(filepath.Join(base, "real"), alias); err != nil {
		b.Skipf("symlinks unavailable: %v", err)
	}
	aliasCWD := filepath.Join(alias, "repo", "nested")

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if !CanonicalHasPathPrefix(aliasCWD, realRepo) {
			b.Fatal("alias unexpectedly escaped root")
		}
	}
}

func BenchmarkCanonicalHasPathPrefixOfflineAliasChild(b *testing.B) {
	base := b.TempDir()
	realRoot := filepath.Join(base, "real")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		b.Fatal(err)
	}
	aliasRoot := filepath.Join(base, "alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		b.Skipf("symlinks unavailable: %v", err)
	}
	offlineChild := filepath.Join(aliasRoot, "removed-worktree")

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if !CanonicalHasPathPrefix(offlineChild, aliasRoot) {
			b.Fatal("offline alias child unexpectedly escaped root")
		}
	}
}

func BenchmarkCanonicalHasPathPrefixMissingAliasLeaf(b *testing.B) {
	base := b.TempDir()
	realRoot := filepath.Join(base, "real")
	realRepo := filepath.Join(realRoot, "repo")
	if err := os.MkdirAll(realRepo, 0o755); err != nil {
		b.Fatal(err)
	}
	aliasRoot := filepath.Join(base, "alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		b.Skipf("symlinks unavailable: %v", err)
	}
	missing := filepath.Join(aliasRoot, "repo", "new", "buffer.go")

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if !CanonicalHasPathPrefix(missing, realRepo) {
			b.Fatal("missing alias leaf unexpectedly escaped root")
		}
	}
}
