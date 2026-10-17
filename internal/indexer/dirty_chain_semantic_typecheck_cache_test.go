package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestDirtyChainSemanticTypecheckCacheParity: with the per-checkout closure
// and type-check state (and sibling-body stripping) on, chained edits keep
// parity with a clean semantic index, binding rows included, and a repeated
// edit in a package whose closure the state already holds runs no `go list`.
func TestDirtyChainSemanticTypecheckCacheParity(t *testing.T) {
	f, c, mgr := semanticChainFixtureWith(t, semanticBindingTree(), true)
	coordinatorReconcile(t, c)

	for i := 0; i < 8; i++ {
		semanticWriteUnit(t, f.worktree, accumulatedDirtyIndependent, i, true)
	}
	root := coordinatorReconcile(t, c)
	if root.DirtyParentGenerationID != 0 {
		t.Fatalf("the chain root = %+v, want a direct build", root)
	}
	scopeAssertCounter(t, f, "cached-root", root)
	assertSemanticParity(t, f, mgr, "cached-root")

	hits := 0
	for k, edit := range []struct {
		layout accumulatedDirtyLayout
		unit   int
		files  int
		pkg    string
		hit    bool
	}{
		{accumulatedDirtyIndependent, 9, 1, builderRepoPrefix + "/ind/p009", false},
		{accumulatedDirtyIndependent, 9, 1, builderRepoPrefix + "/ind/p009", true},
		{accumulatedDirtySamePackage, 3, retentionUnits, builderRepoPrefix + "/same", false},
		{accumulatedDirtySamePackage, 4, retentionUnits, builderRepoPrefix + "/same", true},
		{accumulatedDirtyIndependent, 2, 1, builderRepoPrefix + "/ind/p002", true},
	} {
		semanticWriteUnitRevision(t, f.worktree, edit.layout, edit.unit, k)
		out := coordinatorReconcile(t, c)
		label := fmt.Sprintf("cached-chained-%d", k)
		if out.DirtyParentGenerationID == 0 {
			t.Fatalf("%s = %+v, want a chained build", label, out)
		}
		packages := scopeAssertCounter(t, f, label, out)
		if !slices.Equal(packages, []string{edit.pkg}) {
			t.Errorf("%s: packages(handle) = %v, want only %s", label, packages, edit.pkg)
		}
		cc := out.DirtyWork.CompilerContext
		if got := cc.Files; got != edit.files {
			t.Errorf("%s: %d compiled files loaded, want %d", label, got, edit.files)
		}
		if cc.Cache == nil {
			t.Fatalf("%s: no cache counters on the compiler context: %+v", label, cc)
		}
		t.Logf("%s: cache %+v", label, *cc.Cache)
		if cc.Cache.Bypass != "" {
			t.Errorf("%s: the pass bypassed the cached path (%s)", label, cc.Cache.Bypass)
		}
		if edit.hit {
			hits++
			if cc.Cache.ClosureHits != 1 || cc.Cache.ClosureMisses != 0 || cc.Cache.GoListMs != 0 {
				t.Errorf("%s: cache %+v, want a closure hit with no go list", label, *cc.Cache)
			}
		}
		if n := semanticGenerationBindingRows(t, f, out.DirtyGenerationID); n == 0 {
			t.Errorf("%s: the generation wrote no binding rows; the binding oracle is vacuous", label)
		}
		assertSemanticParity(t, f, mgr, label)
	}
	if hits == 0 {
		t.Fatal("no closure hit was exercised")
	}
}

// semanticWriteUnitRevision writes unit i with a body edit unique to
// revision k (a body-only change against every earlier revision).
func semanticWriteUnitRevision(t *testing.T, repoDir string, layout accumulatedDirtyLayout, i, k int) {
	t.Helper()
	src := strings.Replace(semanticBindingUnitSource(layout, i, true, false), "\tv += 1\n", fmt.Sprintf("\tv += %d\n", k+2), 1)
	full := filepath.Join(repoDir, filepath.FromSlash(accumulatedDirtyUnitPath(layout, i)))
	if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
		t.Fatalf("write unit %d revision %d: %v", i, k, err)
	}
}
