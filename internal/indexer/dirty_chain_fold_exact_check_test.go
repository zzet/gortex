package indexer

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// foldCorruption alters a copied fold (generation to) the way a wrong copy
// would, before the fold is checked.
type foldCorruption struct {
	name   string
	damage func(t *testing.T, h *store_sqlite.Store)
}

// foldCorruptions are the four wrong copies: a row missing, a row altered, a
// row added, a mask wrong. The chain fixture (chainBurstEdit over tree A)
// claims island.go, helper.go, caller.go and core.go; oldname.go stays below.
func foldCorruptions() []foldCorruption {
	return []foldCorruption{
		{"row missing", func(t *testing.T, h *store_sqlite.Store) {
			if !h.RemoveEdge("repo/island.go", "repo/island.go::Island", graph.EdgeDefines) {
				t.Fatal("the fold holds no defines row for Island to remove")
			}
		}},
		{"row altered", func(t *testing.T, h *store_sqlite.Store) {
			n := h.GetNode("repo/island.go::Island")
			if n == nil {
				t.Fatal("the fold holds no Island node to alter")
			}
			altered := *n
			altered.EndLine += 7
			h.AddNode(&altered)
		}},
		{"row added", func(t *testing.T, h *store_sqlite.Store) {
			h.AddEdge(&graph.Edge{From: "repo/island.go::Island", To: "repo/helper.go::Helper",
				Kind: graph.EdgeCalls, FilePath: "repo/island.go", Line: 4, Origin: "ast_resolved"})
		}},
		{"mask wrong", func(t *testing.T, h *store_sqlite.Store) {
			// Hides the out-edges of a source below the chain that no member
			// masks: oldname.go's file node and its defines row into Renamed.
			if err := h.SetEdgeSourceMasks([]store_sqlite.EdgeSourceMask{{SourceID: "repo/oldname.go", Mode: store_sqlite.OwnershipReplace}}); err != nil {
				t.Fatal(err)
			}
		}},
	}
}

// corruptEveryFold installs the copy seam: every copied fold is damaged, and
// the damaged generation's id is recorded.
func corruptEveryFold(t *testing.T, f *coordinatorFixture, c *CheckoutCoordinator, damage func(*testing.T, *store_sqlite.Store)) *[]int64 {
	t.Helper()
	var damaged []int64
	c.compaction.mu.Lock()
	c.compaction.copyHook = func(_ context.Context, to int64) error {
		h, err := f.store.AtManagedGeneration(to)
		if err != nil {
			return err
		}
		damage(t, h)
		damaged = append(damaged, to)
		return nil
	}
	c.compaction.mu.Unlock()
	return &damaged
}

// assertNeverServed fails when a damaged fold is routed, is the parent of the
// route's generation, or is servable.
func assertNeverServed(t *testing.T, f *coordinatorFixture, damaged []int64) {
	t.Helper()
	if len(damaged) == 0 {
		t.Fatal("no fold was copied: the case did not reach the fold")
	}
	route := f.route()
	for _, id := range damaged {
		if route.DirtyGenerationID == id {
			t.Fatalf("the damaged fold %d is routed", id)
		}
		row, _ := f.generation(route.DirtyGenerationID)
		for row.GenerationID != 0 && row.BaseGenerationID != route.CommitGenerationID {
			if row.BaseGenerationID == id {
				t.Fatalf("the routed chain stands on the damaged fold %d", id)
			}
			row, _ = f.generation(row.BaseGenerationID)
		}
		if got, _ := f.generation(id); servableGeneration(got.State) {
			t.Fatalf("the damaged fold %d is servable (%v)", id, got.State)
		}
	}
}

// No reader is served a fold that has not passed the exact check: at the
// cap, a copy damaged in each of four ways is refused before publication and
// the edit builds direct. Override, named: background compactions closed (no
// fold runs, so the edit meets the cap); the copy seam damages the fold. The
// four cases share one checkout: each drives the chain to the cap again from
// the direct build the case before it left, and asserts it got there.
func TestInlineFoldAtTheCapIsNeverServedUnchecked(t *testing.T) {
	f, c, l := mcpChainFixture(t, builderTreeA(), false)
	next, depth := 0, 0 // the next burst edit; the routed chain's depth
	for _, damage := range foldCorruptions() {
		t.Run(damage.name, func(t *testing.T) {
			for depth < maxChainWalkDepth {
				out := mcpEdit(t, l, f, func() { chainBurstEdit(t, f, next) })
				next++
				if out.DirtyChainDepth != depth+1 {
					t.Fatalf("the edit at depth %d built at depth %d (%s), want a child of the chain", depth, out.DirtyChainDepth, out.DirtyChainReason)
				}
				depth = out.DirtyChainDepth
			}
			damaged := corruptEveryFold(t, f, c, damage.damage)
			at := mcpEdit(t, l, f, func() { chainBurstEdit(t, f, next) })
			next++
			depth = at.DirtyChainDepth
			if at.DirtyParentGenerationID != 0 || at.DirtyChainDepth != 1 {
				t.Fatalf("the edit stands on %d at depth %d after a damaged fold, want a direct build (%s)", at.DirtyParentGenerationID, at.DirtyChainDepth, at.DirtyChainReason)
			}
			if err := c.waitDirtyChainCompactions(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertNeverServed(t, f, *damaged)
		})
	}
}

// The background fold is published and routed only after its check has
// passed: a copy damaged in each of four ways is refused, and the chain
// stays routed. Override, named: background compactions closed (the fold is
// run by hand); the copy seam damages the fold. The four cases fold the same
// chain one after another: a refused fold leaves the route on that chain,
// which each case asserts before and after its fold.
func TestSteppedFoldIsNeverServedUnchecked(t *testing.T) {
	f, c, l := mcpChainFixture(t, builderTreeA(), false)
	var trigger CheckoutCycle
	for i := 0; i < dirtyChainCompactionDepth; i++ {
		trigger = mcpEdit(t, l, f, func() { chainBurstEdit(t, f, i) })
	}
	for _, damage := range foldCorruptions() {
		t.Run(damage.name, func(t *testing.T) {
			if f.route().DirtyGenerationID != trigger.DirtyGenerationID {
				t.Fatalf("the route is on %d before the fold, want the chain topped by %d", f.route().DirtyGenerationID, trigger.DirtyGenerationID)
			}
			damaged := corruptEveryFold(t, f, c, damage.damage)
			report := c.compactDirtyChain(context.Background(), trigger)
			if report.Outcome == dirtyChainCompactionFlipped || report.Outcome == dirtyChainCompactionPreferred {
				t.Fatalf("the damaged fold landed: %s", report.Outcome)
			}
			if f.route().DirtyGenerationID != trigger.DirtyGenerationID {
				t.Fatalf("the route moved off the chain to %d", f.route().DirtyGenerationID)
			}
			assertNeverServed(t, f, *damaged)
		})
	}
}
