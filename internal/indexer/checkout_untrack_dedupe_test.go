package indexer

import (
	"strconv"
	"testing"

	"github.com/zzet/gortex/internal/reconcile"
)

func previewClosureLayerIDs(dependents []reconcile.Dependent) []string {
	ids := make([]string, 0, len(dependents))
	for _, dependent := range dependents {
		if dependent.Kind == reconcile.DependentLayer {
			ids = append(ids, dependent.ID)
		}
	}
	return ids
}

func uniqueLayerIDs(generationIDs ...int64) []string {
	seen := make(map[string]struct{}, len(generationIDs))
	ids := make([]string, 0, len(generationIDs))
	for _, generationID := range generationIDs {
		id := layerID(generationID)
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

func TestUniqueUntrackDependentsPreservesFirstStableIdentity(t *testing.T) {
	dependents := []reconcile.Dependent{
		{Kind: reconcile.DependentLayer, ID: "2", Detail: "first layer detail"},
		{Kind: reconcile.DependentGraph, ID: "2", Detail: "same id, different kind"},
		{Kind: reconcile.DependentLayer, ID: "3", Detail: "next layer"},
		{Kind: reconcile.DependentLayer, ID: "2", Detail: "duplicate layer detail"},
	}

	got := uniqueUntrackDependents(dependents)
	if len(got) != 3 {
		t.Fatalf("dedupe returned %d dependents, want 3: %+v", len(got), got)
	}
	if got[0].Detail != "first layer detail" || got[1].Kind != reconcile.DependentGraph || got[2].ID != "3" {
		t.Fatalf("dedupe changed first occurrence or stable order: %+v", got)
	}
}

func BenchmarkUniqueUntrackDependents(b *testing.B) {
	const unique = 64
	input := make([]reconcile.Dependent, 0, unique*2)
	for i := 0; i < unique; i++ {
		id := strconv.Itoa(i)
		input = append(input,
			reconcile.Dependent{Kind: reconcile.DependentLayer, ID: id, Detail: "first"},
			reconcile.Dependent{Kind: reconcile.DependentLayer, ID: id, Detail: "duplicate"},
		)
	}

	b.ReportAllocs()
	for b.Loop() {
		work := append([]reconcile.Dependent(nil), input...)
		if got := uniqueUntrackDependents(work); len(got) != unique {
			b.Fatalf("dedupe returned %d dependents, want %d", len(got), unique)
		}
	}
}
