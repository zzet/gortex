package indexer

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// The twin of TestKindParitySemantic with the settings the daemon starts
// with: the product's handle-rooted compiler load (sibling bodies stripped,
// type-check state retained), the default enrichment admission floor (no
// GORTEX_ENRICH_MIN_NODES), and the coordinator's default compaction quiet
// period. TestKindParitySemantic overrides all three (semanticChainFixture);
// this test overrides none. The same triangulation and residual rules apply.
func TestKindParitySemanticProductDefaults(t *testing.T) {
	f, c, mgr := semanticChainFixtureProduct(t, kindParityTree())
	coordinatorReconcile(t, c)
	primary := newKindParityPrimaryWith(t, f.worktree, mgr)
	for _, edit := range kindParityEdits() {
		applyKindParityEdit(t, f.worktree, edit)
		out := coordinatorReconcile(t, c)
		primary.save(edit.paths())
		view := chainMaterialize(t, f)
		kindParityCheckIndexed(t, fmt.Sprintf("semantic defaults depth %d/%s", out.DirtyChainDepth, edit.name), f.worktree, view.Reader, nil,
			func() graph.Reader { return primary.store }, builderIndexSemantic(mgr))
		stamped := 0
		for _, n := range view.Reader.AllNodes() {
			if n != nil && n.Meta["semantic_source"] != nil {
				stamped++
			}
		}
		// Under the default admission floor a fixture this small may not
		// admit the go/types pass at all; the log says which case ran.
		t.Logf("semantic defaults depth %d/%s: %d nodes carry go/types stamps", out.DirtyChainDepth, edit.name, stamped)
		view.Close()
	}
}

// semanticChainFixtureProduct is semanticChainFixture without its three
// overrides: the product's compiler load, the default admission floor and the
// default compaction quiet period.
func semanticChainFixtureProduct(t *testing.T, tree map[string]string) (*coordinatorFixture, *CheckoutCoordinator, *semantic.Manager) {
	t.Helper()
	mgr := goTypesManager(t)
	f := newCoordinatorFixtureIndexedBy(t, tree, builderIndexSemantic(mgr))
	builder := builderNewBuilder(f.store)
	builder.Semantic = mgr
	builder.wholeModuleCompilerLoad = false
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{Builder: builder})
	return f, c, mgr
}
