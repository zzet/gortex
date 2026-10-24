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
// Every plan is also held to the closure walk's (the comparison walk is the
// test's own; the report says whether the delta walked).
func TestEditDeltaDependentsPlanParsesNothingWithoutAnIntroducedName(t *testing.T) {
	tree := map[string]string{
		"go.mod":   "module example.com/deps\n\ngo 1.22\n",
		"a/a.go":   "package a\n\n// Old is here.\nfunc Old() int {\n\treturn 1\n}\n",
		"b/b.go":   "package b\n\nimport \"example.com/deps/a\"\n\nfunc Use() int {\n\treturn a.Old() + a.Fresh()\n}\n",
		"a/use.go": "package a\n\nfunc Twice() int {\n\treturn helper() * 2\n}\n",
	}
	compared := editDeltaCompareDependentsWithTheWalk(t)
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
	compared.requireCompared(t)
}

// editDeltaCompareDependentsWithTheWalk holds, for the rest of t, every
// dependents plan a delta reads from the pass's rows to the closure walk's
// plan for the same change, and fails t on a difference or a failed walk. The
// walk runs only to be compared; the delta still plans and reports from the
// rows.
//
// The delta property corpus (TestEditDeltaDirtyEquivalenceProperty), the
// invalidation classes (TestEditDeltaInvalidationClasses) and the
// introduced-name edits above run under it, so the plan is held to the walk on
// every change they build, in the same pass as their own oracle.
func editDeltaCompareDependentsWithTheWalk(t *testing.T) *dependentsWalkComparison {
	t.Helper()
	c := &dependentsWalkComparison{}
	editDeltaDependentsCheck = func(rows, walked []string, err error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.checked++
		if err != nil {
			t.Errorf("closure walk failed: %v", err)
			return
		}
		if !slices.Equal(rows, walked) {
			c.differ++
			t.Errorf("dependents from rows %v, closure walk %v", rows, walked)
		}
	}
	t.Cleanup(func() {
		editDeltaDependentsCheck = nil
		c.mu.Lock()
		defer c.mu.Unlock()
		t.Logf("%d dependents plans compared with the closure walk, %d differ", c.checked, c.differ)
	})
	return c
}

// dependentsWalkComparison counts the plans editDeltaCompareDependentsWithTheWalk
// compared.
type dependentsWalkComparison struct {
	mu              sync.Mutex
	checked, differ int
}

// requireCompared fails t when no delta planned its dependents from rows, so
// the comparison was vacuous. A test calls it once all its cases ran: a -run
// filter may pick a single case whose change deletes a file, which the walk
// plans itself.
func (c *dependentsWalkComparison) requireCompared(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.checked == 0 {
		t.Error("no delta planned its dependents from rows")
	}
}
