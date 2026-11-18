package indexer

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The kind parity with the go/types enrichment on: the base corpus, every
// working-tree build, the clean index and the primary per-save path run the
// go/types pass (the whole module load), through the checkout's own cycles.
// A row the view and the primary path share but a clean index lacks (or the
// reverse) is an engine residual, reported; a row only the view differs on
// fails.
func TestKindParitySemantic(t *testing.T) {
	f, c, mgr := semanticChainFixture(t, kindParityTree())
	coordinatorReconcile(t, c)
	primary := newKindParityPrimaryWith(t, f.worktree, mgr)
	for _, edit := range kindParityEdits() {
		applyKindParityEdit(t, f.worktree, edit)
		out := coordinatorReconcile(t, c)
		primary.save(edit.paths())
		view := chainMaterialize(t, f)
		kindParityCheckIndexed(t, fmt.Sprintf("semantic depth %d/%s", out.DirtyChainDepth, edit.name), f.worktree, view.Reader, nil,
			func() graph.Reader { return primary.store }, builderIndexSemantic(mgr))
		view.Close()
	}
}
