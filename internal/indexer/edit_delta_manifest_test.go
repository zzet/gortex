package indexer

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// TestEditDeltaManifestChangesMatchAWholeIndex drives a module manifest change
// through the coordinator and after every step compares the served view with a
// whole index of the working tree (the replay oracle's comparison, empty
// allow-list). A step that changes a manifest is built by the sparse closure
// builder — the per-file delta re-derives a go.mod differently from a whole
// index — and every other step by the delta path. The edit after the manifest
// change is a delta over the manifest's new state; there the view must equal a
// whole index except rows the primary per-save engine produces identically.
func TestEditDeltaManifestChangesMatchAWholeIndex(t *testing.T) {
	f := newCoordinatorFixtureWithTree(t, replayTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	h := &replayHarness{f: f, c: c, sampleLimit: 40, clean: func(t *testing.T, label string) (graph.Reader, func()) {
		store := builderOpenStore(t, label)
		builderIndex(t, store, f.worktree)
		return store, nil
	}}
	o := newReplayOriginals(f.worktree)
	write := func(name, rel, body string) replayStep {
		return replayStep{name: name, apply: func(t testing.TB, root string) {
			o.remember(t, rel)
			replayWrite(t, root, rel, body)
		}}
	}
	usesSource := func(calls string) string {
		return "package e\n\nimport \"example.org/dep/v2/lib\"\n\n// Use calls into the dependency.\nfunc Use() int {\n\treturn " + calls + "\n}\n"
	}
	strict := func(step replayStep, manifest bool) {
		t.Helper()
		recordLastEditDelta(nil)
		records := h.run(t, []replayStep{step})
		if len(records) != 1 {
			t.Fatalf("%s: ran %d steps", step.name, len(records))
		}
		byDelta := LastEditDeltaReport() != nil
		switch {
		case !records[0].DirtyBuilt:
		case manifest && byDelta:
			t.Errorf("%s: a manifest change was built by the delta path", step.name)
		case !manifest && !byDelta:
			t.Errorf("%s: the working-tree layer was not built by the delta path", step.name)
		}
	}
	// A file importing a module the manifest does not require yet: a whole
	// index classifies the import as external.
	strict(write("import an unrequired module", "e/uses.go", usesSource("lib.Value()")), false)
	// The manifest now requires it: the same import is a dependency.
	strict(write("require the module", "go.mod",
		"module "+replayModule+"\n\ngo 1.22\n\nrequire example.org/dep/v2 v2.3.0\n"), true)

	// An edit after the manifest change is a delta again.
	recordLastEditDelta(nil)
	write("use it twice", "e/uses.go", usesSource("lib.Value() + lib.Value()")).apply(t, f.worktree)
	if out := coordinatorReconcile(t, c); !out.DirtyBuilt || LastEditDeltaReport() == nil {
		t.Fatalf("the edit after the manifest change was not built by the delta path: %+v", out)
	}
	view := chainMaterialize(t, f)
	clean := builderOpenStore(t, "clean-use-twice")
	builderIndex(t, clean, f.worktree)
	primary := primaryPerSaveOf(t, f.worktree, []string{"go.mod", "e/uses.go"}, config.Default().Index)
	if bad := editDeltaTriangulate(view.Reader, primary, clean); len(bad) > 0 {
		t.Errorf("rows where the delta differs from both the whole index and the primary per-save path:\n  %s",
			strings.Join(bad, "\n  "))
	}
	view.Close()

	strict(replayUndoAll(o), false)
}
