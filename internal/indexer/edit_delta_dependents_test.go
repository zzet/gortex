package indexer

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

// The dependents of a working-tree delta are planned from the pass's own rows:
// a body edit introduces no name, so the plan walks (and parses) nothing and
// re-derives no file; an edit that adds a function a caller elsewhere already
// references plans that caller, still without extracting anything again.
func TestEditDeltaDependentsPlanParsesNothingWithoutAnIntroducedName(t *testing.T) {
	tree := map[string]string{
		"go.mod":   "module example.com/deps\n\ngo 1.22\n",
		"a/a.go":   "package a\n\n// Old is here.\nfunc Old() int {\n\treturn 1\n}\n",
		"b/b.go":   "package b\n\nimport \"example.com/deps/a\"\n\nfunc Use() int {\n\treturn a.Old() + a.Fresh()\n}\n",
		"a/use.go": "package a\n\nfunc Twice() int {\n\treturn helper() * 2\n}\n",
	}
	f := newCoordinatorFixtureWithTree(t, tree)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	c.compaction.quiet = -1
	coordinatorReconcile(t, c)
	full := filepath.Join(f.worktree, "a", "a.go")
	write := func(body string) *EditDeltaReport {
		t.Helper()
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if out := coordinatorReconcile(t, c); !out.DirtyBuilt {
			t.Fatalf("no dirty build: %+v", out)
		}
		delta := LastEditDeltaReport()
		if delta == nil || !slices.Equal(delta.Paths, []string{"a/a.go"}) {
			t.Fatalf("the edit was not built as a delta of a/a.go: %+v", delta)
		}
		return delta
	}

	body := write("package a\n\n// Old is still here.\nfunc Old() int {\n\treturn 2\n}\n")
	if body.DependentsWalked || len(body.Dependents) != 0 {
		t.Fatalf("a body edit walked the closure (walked=%v) or planned dependents %v", body.DependentsWalked, body.Dependents)
	}

	local := write("package a\n\n// Old is still here.\nfunc Old() int {\n\treturn 2\n}\n\nfunc helper() int {\n\treturn 4\n}\n")
	if local.DependentsWalked || !slices.Contains(local.Dependents, "a/use.go") || slices.Contains(local.Dependents, "b/b.go") {
		t.Fatalf("adding an unexported name a/use.go calls must plan a/use.go alone: walked=%v dependents=%v", local.DependentsWalked, local.Dependents)
	}

	added := write("package a\n\n// Old is still here.\nfunc Old() int {\n\treturn 2\n}\n\nfunc helper() int {\n\treturn 4\n}\n\n// Fresh is new.\nfunc Fresh() int {\n\treturn 3\n}\n")
	if added.DependentsWalked || !slices.Contains(added.Dependents, "b/b.go") {
		t.Fatalf("adding a name b/b.go references must plan b/b.go without the walk: walked=%v dependents=%v", added.DependentsWalked, added.Dependents)
	}
}

// The plan read from the pass's rows is the closure walk's plan, on every
// change the delta property corpus and the invalidation classes build.
func TestEditDeltaDependentsPlanMatchesTheWalk(t *testing.T) {
	var mu sync.Mutex
	var checked, differ int
	editDeltaDependentsCheck = func(rows, walked []string, err error) {
		mu.Lock()
		defer mu.Unlock()
		checked++
		if err != nil {
			t.Errorf("closure walk failed: %v", err)
			return
		}
		if !slices.Equal(rows, walked) {
			differ++
			t.Errorf("dependents from rows %v, closure walk %v", rows, walked)
		}
	}
	t.Cleanup(func() { editDeltaDependentsCheck = nil })
	t.Run("corpus", TestEditDeltaDirtyEquivalenceProperty)
	t.Run("classes", TestEditDeltaInvalidationClasses)
	t.Run("introduced", TestEditDeltaDependentsPlanParsesNothingWithoutAnIntroducedName)
	mu.Lock()
	defer mu.Unlock()
	if checked == 0 {
		t.Fatal("no delta planned its dependents from rows")
	}
	t.Logf("%d plans compared, %d differ", checked, differ)
}
