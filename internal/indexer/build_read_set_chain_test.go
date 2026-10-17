package indexer

import (
	"context"
	"testing"
	"time"
)

func coordinatorReadSetCapability(t *testing.T, c *CheckoutCoordinator) bool {
	t.Helper()
	sample, err := c.sampler.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	proof, err := c.sampler.ConfirmReadSet(context.Background(), sample, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !proof.Confirmed && proof.Reason != "the checkout's filesystem gives no change stamps" {
		t.Fatalf("fixture cannot establish read-set capability: %+v", proof)
	}
	return proof.Confirmed
}

// TestChainedDirtyBuildConfirmsByReadSet pins the prepublish fence on the
// daemon's sparse path: a coordinator cycle whose working-tree build chains on
// the previous one hands its plan's read set to PrePublish and is confirmed by
// it — no full re-sample — for the direct chain root and for the chained child
// alike. Filesystems without trusted change stamps instead take exactly one
// full confirmation sample and must publish the same graph.
func TestChainedDirtyBuildConfirmsByReadSet(t *testing.T) {
	f := newCoordinatorFixture(t)
	builder := builderNewBuilder(f.store)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{Builder: builder})
	c.compaction.quiet = -1
	coordinatorReconcile(t, c)
	confirmsReadSet := coordinatorReadSetCapability(t, c)

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
		wantConfirmed, wantFallback := confirmedBefore+1, fallbackBefore
		if !confirmsReadSet {
			wantConfirmed, wantFallback = confirmedBefore, fallbackBefore+1
		}
		if confirmed != wantConfirmed || fallback != wantFallback {
			t.Errorf("step %d (parent %d): read-set confirmations %d -> %d (want %d), fallbacks %d -> %d (want %d)",
				step, out.DirtyParentGenerationID, confirmedBefore, confirmed, wantConfirmed, fallbackBefore, fallback, wantFallback)
		}
		chainAssertFlat(t, f, "read-set chain confirmation")
	}
}
