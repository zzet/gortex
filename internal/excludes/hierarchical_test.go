package excludes

import (
	"os"
	"path/filepath"
	"testing"
)

func mkIgnore(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHierarchical_RootLevel(t *testing.T) {
	root := t.TempDir()
	mkIgnore(t, filepath.Join(root, ".gortexignore"), "*.gen.go\nbuild/\n")
	h := NewHierarchical(root, ".gortexignore")

	if !h.Match(filepath.Join(root, "foo.gen.go"), false) {
		t.Error("root .gortexignore should exclude foo.gen.go")
	}
	if h.Match(filepath.Join(root, "foo.go"), false) {
		t.Error("foo.go should not be excluded")
	}
	if !h.Match(filepath.Join(root, "build"), true) {
		t.Error("build/ directory should be excluded so the subtree is pruned")
	}
	if !h.Match(filepath.Join(root, "pkg", "sub", "foo.gen.go"), false) {
		t.Error("root pattern should reach a deeply nested foo.gen.go")
	}
}

func TestHierarchical_PerDirectory(t *testing.T) {
	root := t.TempDir()
	// An ignore file deep in the tree scopes only to its own subtree.
	mkIgnore(t, filepath.Join(root, "internal", ".gortexignore"), "secret.go\n")
	h := NewHierarchical(root, ".gortexignore")

	if !h.Match(filepath.Join(root, "internal", "secret.go"), false) {
		t.Error("internal/.gortexignore should exclude internal/secret.go")
	}
	if !h.Match(filepath.Join(root, "internal", "sub", "secret.go"), false) {
		t.Error("internal/.gortexignore should reach internal/sub/secret.go")
	}
	if h.Match(filepath.Join(root, "secret.go"), false) {
		t.Error("internal/.gortexignore must not affect a root-level secret.go")
	}
	if h.Match(filepath.Join(root, "cmd", "secret.go"), false) {
		t.Error("internal/.gortexignore must not affect a sibling directory")
	}
}

func TestHierarchical_Nested(t *testing.T) {
	root := t.TempDir()
	mkIgnore(t, filepath.Join(root, ".gortexignore"), "*.tmp\n")
	mkIgnore(t, filepath.Join(root, "pkg", ".gortexignore"), "local.go\nvendored/\n")
	h := NewHierarchical(root, ".gortexignore")

	// Both the root rule and the pkg rule apply under pkg/.
	if !h.Match(filepath.Join(root, "pkg", "x.tmp"), false) {
		t.Error("root rule should still apply inside pkg/")
	}
	if !h.Match(filepath.Join(root, "pkg", "local.go"), false) {
		t.Error("pkg/.gortexignore should exclude pkg/local.go")
	}
	if !h.Match(filepath.Join(root, "pkg", "vendored"), true) {
		t.Error("pkg/.gortexignore should prune the pkg/vendored/ subtree")
	}
	if h.Match(filepath.Join(root, "local.go"), false) {
		t.Error("pkg/.gortexignore must not reach the root")
	}
}

func TestHierarchical_Negation(t *testing.T) {
	root := t.TempDir()
	mkIgnore(t, filepath.Join(root, ".gortexignore"), "*.log\n!keep.log\n")
	h := NewHierarchical(root, ".gortexignore")

	if !h.Match(filepath.Join(root, "debug.log"), false) {
		t.Error("*.log should exclude debug.log")
	}
	if h.Match(filepath.Join(root, "keep.log"), false) {
		t.Error("!keep.log should re-include keep.log")
	}
}

func TestHierarchical_NoFiles(t *testing.T) {
	root := t.TempDir()
	mkIgnore(t, filepath.Join(root, "foo.go"), "package x\n")
	h := NewHierarchical(root, ".gortexignore")
	if h.Match(filepath.Join(root, "foo.go"), false) {
		t.Error("with no ignore files, nothing should be excluded")
	}
}

func TestHierarchical_OutsideRoot(t *testing.T) {
	root := t.TempDir()
	mkIgnore(t, filepath.Join(root, ".gortexignore"), "*\n")
	h := NewHierarchical(root, ".gortexignore")
	if h.Match(filepath.Join(filepath.Dir(root), "elsewhere.go"), false) {
		t.Error("a path outside the root must never be excluded")
	}
}

func TestHierarchical_NilAndEmpty(t *testing.T) {
	var h *Hierarchical
	if h.Match("/anything", false) {
		t.Error("nil Hierarchical should match nothing")
	}
	root := t.TempDir()
	mkIgnore(t, filepath.Join(root, ".gortexignore"), "*\n")
	empty := NewHierarchical(root) // no filenames configured
	if empty.Match(filepath.Join(root, "foo.go"), false) {
		t.Error("an empty filename list should match nothing")
	}
}

func TestHierarchical_MultipleFilenames(t *testing.T) {
	root := t.TempDir()
	mkIgnore(t, filepath.Join(root, ".gortexignore"), "a.go\n")
	mkIgnore(t, filepath.Join(root, ".ignore"), "b.go\n")
	h := NewHierarchical(root, ".gortexignore", ".ignore")

	if !h.Match(filepath.Join(root, "a.go"), false) {
		t.Error(".gortexignore pattern should apply")
	}
	if !h.Match(filepath.Join(root, "b.go"), false) {
		t.Error(".ignore pattern should apply")
	}
	if h.Match(filepath.Join(root, "c.go"), false) {
		t.Error("c.go should not be excluded")
	}
}

func TestHierarchicalForgetRereadsIgnoreFile(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	ignore := filepath.Join(sub, ".gortexignore")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	h := NewHierarchical(root, ".gortexignore")
	target := filepath.Join(sub, "gen", "x.go")
	if h.Match(target, false) {
		t.Fatal("no ignore file yet")
	}
	if err := os.WriteFile(ignore, []byte("gen/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if h.Match(target, false) {
		t.Fatal("the cached empty rules stay until Forget")
	}
	h.Forget(sub)
	if !h.Match(target, false) {
		t.Fatal("Forget must reread the folder's ignore file")
	}
}

// A folder moved away and replaced by another tree must not keep the old
// tree's rules at any depth.
func TestHierarchicalForgetDropsDescendants(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	mkIgnore(t, filepath.Join(sub, "nested", ".gortexignore"), "gen/\n")
	h := NewHierarchical(root, ".gortexignore")
	target := filepath.Join(sub, "nested", "gen", "x.go")
	if !h.Match(target, false) {
		t.Fatal("nested rule applies")
	}
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sub, "nested", "gen"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.Forget(sub)
	if h.Match(target, false) {
		t.Fatal("Forget(sub) must drop the cached rules of sub/nested")
	}
	// Forget on a path that is not cached, such as a removed file, leaves
	// the other entries alone.
	h.Forget(filepath.Join(root, "other.go"))
	h.mu.RLock()
	n := len(h.cache)
	h.mu.RUnlock()
	if n != 4 {
		t.Fatalf("cache entries = %d, want root, sub, sub/nested and sub/nested/gen", n)
	}
}

// A folder is cached only after its parent, which lets Forget skip the
// scan for a folder that is not cached.
func TestHierarchicalCachesFolderOnlyAfterParent(t *testing.T) {
	root := t.TempDir()
	h := NewHierarchical(root, ".gortexignore")
	sub := filepath.Join(root, "sub")
	h.dirMatcher(sub)
	h.mu.RLock()
	_, ok := h.cache[sub]
	h.mu.RUnlock()
	if ok {
		t.Fatal("sub cached before root")
	}
	h.dirMatcher(root)
	h.dirMatcher(sub)
	h.mu.RLock()
	_, ok = h.cache[sub]
	h.mu.RUnlock()
	if !ok {
		t.Fatal("sub not cached after root")
	}
}
