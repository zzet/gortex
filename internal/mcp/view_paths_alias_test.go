package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graphview"
)

func TestRelativeWithinRootResolvesCanonicalAlias(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real")
	realRoot := filepath.Join(realParent, "repo")
	realDir := filepath.Join(realRoot, "internal")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(base, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	for _, target := range []string{
		filepath.Join(aliasParent, "repo", "internal", "existing.go"),
		filepath.Join(aliasParent, "repo", "internal", "new.go"),
	} {
		if filepath.Base(target) == "existing.go" {
			if err := os.WriteFile(filepath.Join(realDir, "existing.go"), []byte("package internal\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		rel, ok := relativeWithinRoot(realRoot, target)
		if !ok || filepath.ToSlash(rel) != "internal/"+filepath.Base(target) {
			t.Errorf("relativeWithinRoot(%q, %q) = %q, %v", realRoot, target, rel, ok)
		}
	}
}

func TestViewPathRootRootedResolvesCanonicalAlias(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real")
	baseRoot := filepath.Join(realParent, "base")
	viewRoot := filepath.Join(realParent, "view")
	if err := os.MkdirAll(filepath.Join(baseRoot, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(viewRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(base, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	abs := filepath.Join(aliasParent, "base", "internal", "new.go")
	got := (viewPathRoot{root: viewRoot}).rooted(abs, baseRoot)
	want := filepath.Join(viewRoot, "internal", "new.go")
	if got != want {
		t.Fatalf("rooted alias = %q, want %q", got, want)
	}
}

func TestResolveOverlayGraphPathForRequestResolvesCanonicalAlias(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real")
	viewRoot := filepath.Join(realParent, "worktree")
	if err := os.MkdirAll(filepath.Join(viewRoot, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(base, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	view := &requestView{
		viewRoot: viewRoot,
		materialized: &graphview.RepoView{ID: graphview.RepoViewID{
			RepoPrefix: "repo",
		}},
	}
	ctx := withRequestView(context.Background(), view)
	abs := filepath.Join(aliasParent, "worktree", "internal", "new.go")
	got := (&Server{}).resolveOverlayGraphPathForRequest(ctx, abs, abs)
	if got != "repo/internal/new.go" {
		t.Fatalf("overlay graph path = %q, want repo/internal/new.go", got)
	}
}

func BenchmarkRelativeWithinRootAlias(b *testing.B) {
	base := b.TempDir()
	realParent := filepath.Join(base, "real")
	realRoot := filepath.Join(realParent, "repo")
	if err := os.MkdirAll(filepath.Join(realRoot, "internal"), 0o755); err != nil {
		b.Fatal(err)
	}
	aliasParent := filepath.Join(base, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		b.Skipf("symlinks unavailable: %v", err)
	}
	target := filepath.Join(aliasParent, "repo", "internal", "new.go")

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if rel, ok := relativeWithinRoot(realRoot, target); !ok || filepath.ToSlash(rel) != "internal/new.go" {
			b.Fatal("aliased target did not resolve")
		}
	}
}
