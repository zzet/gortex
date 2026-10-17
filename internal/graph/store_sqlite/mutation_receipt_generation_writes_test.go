package store_sqlite

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// A base-graph mutation window observes writes to the base graph. A working-
// tree generation's payload is written through a derived handle on the same
// store core while another repository's window is open; those writes are not
// the window's mutation and must neither void its receipt nor widen its
// frontier. A voided receipt with no derived frontier sends the watcher to a
// whole-repository resolve (once measured: a no-op reconcile of one tracked
// repository, overlapping an edit delta's publication, resolved 114,688
// pending edges).
func TestGenerationWritesDoNotTouchBaseMutationReceipts(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	store.AddBatch([]*graph.Node{
		{ID: "other/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "other/a.go", RepoPrefix: "other"},
	}, nil)
	id, handle, err := store.BeginPayloadGeneration(ctx, PayloadGenerationRequest{OwnerKind: "ref_view", GraphID: "g", LayerID: "l", GenerationKind: "commit", TreeOID: "t", CreatedAt: 10})
	if err != nil {
		t.Fatal(err)
	}
	_ = id
	token := store.BeginMutationReceipt()
	// The generation's writes: a node without an exact file (an external
	// row), edges, and an exact edge removal (editDeltaSettleEnrichment).
	handle.AddBatch([]*graph.Node{
		{ID: "repo/x.go::X", Kind: graph.KindFunction, Name: "X", FilePath: "repo/x.go", RepoPrefix: "repo"},
		{ID: "ext::go:os::Getenv", Kind: graph.KindFunction, Name: "Getenv"},
	}, []*graph.Edge{
		{From: "repo/x.go::X", To: "ext::go:os::Getenv", Kind: graph.EdgeCalls, FilePath: "repo/x.go", Line: 3},
		{From: "repo/x.go::X", To: "unresolved::Y", Kind: graph.EdgeCalls, FilePath: "repo/x.go", Line: 4},
	})
	if n := handle.RemoveEdgesExact([]*graph.Edge{{From: "repo/x.go::X", To: "unresolved::Y", Kind: graph.EdgeCalls, FilePath: "repo/x.go", Line: 4}}); n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	receipt := store.EndMutationReceipt(token)
	if !receipt.Complete {
		t.Fatalf("a generation's writes voided a base mutation receipt: %q", receipt.IncompleteReason)
	}
	if receipt.ResolutionRelevant || len(receipt.ResolutionFiles()) > 0 {
		t.Fatalf("a generation's writes widened a base mutation receipt: files %v", receipt.ResolutionFiles())
	}
}

// A window opened on a generation handle observes that generation's writes
// (an indexer building a generation through its own handle keeps its
// receipt), and still ignores the base's.
func TestGenerationMutationReceiptObservesItsOwnGeneration(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	_, handle, err := store.BeginPayloadGeneration(ctx, PayloadGenerationRequest{OwnerKind: "ref_view", GraphID: "g", LayerID: "l", GenerationKind: "commit", TreeOID: "t", CreatedAt: 10})
	if err != nil {
		t.Fatal(err)
	}
	token := handle.BeginMutationReceipt()
	store.AddBatch([]*graph.Node{{ID: "base/b.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "base/b.go", RepoPrefix: "base"}}, nil)
	handle.AddBatch([]*graph.Node{{ID: "repo/x.go::X", Kind: graph.KindFunction, Name: "X", FilePath: "repo/x.go", RepoPrefix: "repo"}},
		[]*graph.Edge{{From: "repo/x.go::X", To: "unresolved::Y", Kind: graph.EdgeCalls, FilePath: "repo/x.go", Line: 4}})
	receipt := handle.EndMutationReceipt(token)
	files := receipt.ResolutionFiles()
	if !receipt.Complete {
		t.Fatalf("the generation's own window: %q", receipt.IncompleteReason)
	}
	found := false
	for _, f := range files {
		if f == "base/b.go" {
			t.Fatalf("the base's write reached a generation's window: %v", files)
		}
		if f == "repo/x.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the generation's window missed its own write: files %v", files)
	}
}
