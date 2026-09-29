package indexer

import (
	"testing"
	"time"
)

// TestChainedDirtyBuildConfirmsByReadSet pins the prepublish fence on the
// daemon's sparse path: a coordinator cycle whose working-tree build chains on
// the previous one hands its plan's read set to PrePublish and is confirmed by
// it — no full re-sample — for the direct chain root and for the chained child
// alike.
func TestChainedDirtyBuildConfirmsByReadSet(t *testing.T) {
	f := newCoordinatorFixture(t)
	builder := builderNewBuilder(f.store)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{Builder: builder})
	coordinatorReconcile(t, c)

	for step, body := range []string{
		"package fixture\n\nfunc Extra() int { return 1 }\n",
		"package fixture\n\nfunc Extra() int { return 2 }\n",
	} {
		if step == 0 {
			// A second dirty file, so the child's delta is smaller than a
			// direct build and the cycle chains.
			builderWriteFile(t, f.worktree, "other.go", "package fixture\n\nfunc Other() int { return 1 }\n")
		}
		builderWriteFile(t, f.worktree, "extra.go", body)
		time.Sleep(2 * time.Second / 20) // past the change-stamp margin
		confirmedBefore, fallbackBefore := readSetCounts()
		out := coordinatorReconcile(t, c)
		if !out.DirtyBuilt {
			t.Fatalf("step %d built nothing: %+v", step, out)
		}
		if step == 1 && out.DirtyParentGenerationID == 0 {
			t.Fatalf("step 1 = %+v, want a chained build", out)
		}
		confirmed, fallback := readSetCounts()
		if confirmed != confirmedBefore+1 || fallback != fallbackBefore {
			t.Errorf("step %d (parent %d): read-set confirmations %d -> %d, fallbacks %d -> %d; want one confirmation and no full re-sample",
				step, out.DirtyParentGenerationID, confirmedBefore, confirmed, fallbackBefore, fallback)
		}
	}
}
