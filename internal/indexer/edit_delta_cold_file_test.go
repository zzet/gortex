package indexer

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graphview"
)

// An edit of a file no pre-warm guessed (none ran here) costs indexed reads
// scoped to the file: over a keyed stack, its first delta reads no layer
// wholesale and loads the file's rows into the stack's cache, and the next
// delta over the same stack is served the file's rows from that cache.
func TestAColdFilesDeltasReadOnlyTheFileAndKeepItPerStack(t *testing.T) {
	fx := newDirtyChainFixture(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()
	row, found := fx.f.generation(fx.d1)
	if !found {
		t.Fatal("no parent generation")
	}
	materializer := graphview.Materializer{Store: fx.f.store, Catalog: fx.f.catalog, Leases: fx.f.leases, Logger: zap.NewNop()}
	view, err := materializer.MaterializeRefView(ctx, row.GraphID, fx.d1)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	base := commitLayerBase{Reader: view.Reader, stack: []int64{9400001, 9400002}}
	run := func(content string) *EditDeltaReport {
		t.Helper()
		builderWriteFile(t, fx.f.worktree, "island.go", content)
		recordLastEditDelta(nil)
		if _, _, err := fx.c.builder.BuildDirtyLayer(ctx, DirtyLayerRequest{
			Identity: fx.c.dirtyIdentity(fx.f.graphID, fx.d1), Base: base, CheckoutRoot: fx.f.worktree,
			RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
		}); err != nil {
			t.Fatal(err)
		}
		report := LastEditDeltaReport()
		if report == nil {
			t.Fatal("the working tree was not built by a per-file delta")
		}
		return report
	}
	first := run(chainIslandEdit)
	second := run(strings.Replace(chainIslandEdit, "third", "fourth", 1))
	for i, r := range []*EditDeltaReport{first, second} {
		if r.WholeLayerLoads != 0 {
			t.Fatalf("delta %d read %d layers wholesale (%d rows)", i+1, r.WholeLayerLoads, r.WholeLayerRows)
		}
	}
	if first.StackPathNodeMisses == 0 {
		t.Fatalf("the first delta loaded no file rows into the stack's cache (hits %d)", first.StackPathNodeHits)
	}
	if second.StackPathNodeMisses != 0 || second.StackPathNodeHits == 0 {
		t.Fatalf("the second delta's file rows: hits %d misses %d; want every read served from the stack's cache",
			second.StackPathNodeHits, second.StackPathNodeMisses)
	}
}
