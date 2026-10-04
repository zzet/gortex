package indexer

import (
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// TestTornBuildNeverBecomesAChainParent pins the parent rule for a torn build: a
// working-tree build the tree moved under at the prepublish fence is left
// failed — not servable, not a chain hop, not reusable — so the next cycle
// never stands on it.
func TestTornBuildNeverBecomesAChainParent(t *testing.T) {
	t.Setenv(prepublishFullResampleEnv, "1")
	f := newCoordinatorFixture(t)
	builder := builderNewBuilder(f.store)

	var (
		armed bool
		once  sync.Once
		torn  int64
	)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{
		Builder: builder,
		dirtyBarrier: func() {
			if !armed {
				return
			}
			once.Do(func() {
				for _, row := range f.generations() {
					if row.State == store_sqlite.ViewGenerationBuilding {
						torn = row.GenerationID
					}
				}
				builderWriteFile(t, f.worktree, "sneaked.go", "package fixture\n\nfunc Sneaked() {}\n")
			})
		},
	})
	c.compaction.quiet = -1
	coordinatorReconcile(t, c)

	builderWriteFile(t, f.worktree, "other.go", "package fixture\n\nfunc Other() int { return 1 }\n")
	builderWriteFile(t, f.worktree, "extra.go", "package fixture\n\nfunc Extra() int { return 1 }\n")
	root := coordinatorReconcile(t, c)
	if !root.DirtyBuilt || root.DirtyGenerationID == 0 {
		t.Fatalf("the chain root did not publish: %+v", root)
	}

	armed = true
	builderWriteFile(t, f.worktree, "extra.go", "package fixture\n\nfunc Extra() int { return 2 }\n")
	c.reconcile(t.Context()) // the tear: this cycle's build is thrown away
	if torn == 0 {
		t.Fatal("no build was in flight when the tree moved")
	}
	row, found := f.generation(torn)
	if !found {
		t.Fatalf("torn generation %d vanished before the check", torn)
	}
	if row.State != store_sqlite.ViewGenerationFailed {
		t.Fatalf("the torn build is %q, want failed", row.State)
	}
	if servableGeneration(row.State) {
		t.Fatalf("a torn build (%s) is servable", row.State)
	}

	builderWriteFile(t, f.worktree, "extra.go", "package fixture\n\nfunc Extra() int { return 3 }\n")
	next := coordinatorReconcile(t, c)
	if !next.DirtyBuilt {
		t.Fatalf("the cycle after the tear built nothing: %+v", next)
	}
	if next.DirtyParentGenerationID == torn || next.DirtyGenerationID == torn {
		t.Fatalf("the cycle after the tear stands on the torn build %d: %+v", torn, next)
	}
	for id := next.DirtyGenerationID; id != 0; {
		hop, ok := f.generation(id)
		if !ok {
			break
		}
		if id == torn {
			t.Fatalf("the torn build %d is in the served chain", torn)
		}
		if hop.BaseGenerationID == id {
			break
		}
		id = hop.BaseGenerationID
	}
}
