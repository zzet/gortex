package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A save that changes only a literal is structurally inert to the per-save
// engine, but the working tree's file is not the one the layer below
// describes: its content hash, size and text differ. Every delta therefore
// claims each changed path and rewrites its files row, on the first edit and
// on every repeat edit of the same file.
func TestEditDeltaClaimsEveryChangedPathOnRepeatEdits(t *testing.T) {
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	c.compaction.quiet = -1
	coordinatorReconcile(t, c)
	full := filepath.Join(f.worktree, "c", "c.go")
	original, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		// Only the literal changes: the same declarations on the
		// same lines.
		edited := strings.Replace(string(original), "return 3", fmt.Sprintf("return %d", 3+i), 1)
		if err := os.WriteFile(full, []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
		out := coordinatorReconcile(t, c)
		if !out.DirtyBuilt {
			t.Fatalf("edit %d built no working-tree layer", i)
		}
		db := parityOpenRaw(t, f.store)
		gen := fmt.Sprint(out.DirtyGenerationID)
		masks := parityColumn(t, db, `SELECT file_path||':'||ownership_mode FROM generation_file_masks WHERE view_gen = ? ORDER BY file_path`, gen)
		served := parityColumn(t, db, `SELECT content_hash||':'||size FROM files WHERE view_gen = ? AND file_path = 'repo/c/c.go'`, gen)
		clean := builderOpenStore(t, fmt.Sprintf("repeat-clean-%d", i))
		builderIndex(t, clean, f.worktree)
		want := parityColumn(t, parityOpenRaw(t, clean), `SELECT content_hash||':'||size FROM files WHERE view_gen = 0 AND file_path = 'repo/c/c.go'`)
		claimed := false
		for _, m := range masks {
			if strings.HasPrefix(m, "repo/c/c.go:") {
				claimed = true
			}
		}
		if !claimed {
			t.Errorf("edit %d: generation %d does not claim repo/c/c.go (masks %v)", i, out.DirtyGenerationID, masks)
		}
		if fmt.Sprint(served) != fmt.Sprint(want) || len(want) != 1 {
			t.Errorf("edit %d: generation %d serves files row %v for repo/c/c.go, a whole index %v", i, out.DirtyGenerationID, served, want)
		}
	}
}

// A working tree whose files are all the state below is an empty change set;
// the delta for it is empty. The per-save engine is not run for it: handed no
// path, its scoped discovery would take the whole root as the scope and
// re-derive the repository into the generation.
func TestEditDeltaEmptyChangeSetIsAnEmptyDelta(t *testing.T) {
	repo := builderTempDir(t, "repo")
	builderWriteTree(t, repo, sharedRowsTree())
	builderGit(t, repo, "init", "-q")
	builderGit(t, repo, "add", "-A")
	builderGit(t, repo, "commit", "-q", "-m", "base")
	store := builderOpenStore(t, "empty-change-set")
	builderIndex(t, store, repo)
	builder := editDeltaBuilder(t, builderNewBuilder(store))
	chains := newDirtyChainBuilder(t, builder, store, repo, false)
	recordLastEditDelta(nil)
	id, report, _ := chains.build()
	delta := LastEditDeltaReport()
	if delta == nil {
		t.Skipf("a clean working tree built generation %d without the delta path (%+v)", id, report)
	}
	if len(delta.Paths) != 0 || delta.CoveredPaths != 0 || delta.PayloadNodes != 0 || delta.PayloadEdges != 0 {
		t.Fatalf("a clean working tree built a non-empty delta (generation %d): %+v", id, *delta)
	}
}
