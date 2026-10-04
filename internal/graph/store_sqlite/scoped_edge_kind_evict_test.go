package store_sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestEvictEdgesFromSourcesByKindsIsScopedSetOrientedAndCancellable(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{
		{ID: "changed", Kind: graph.KindMethod},
		{ID: "unchanged", Kind: graph.KindMethod},
		{ID: "field", Kind: graph.KindField},
		{ID: "process", Kind: graph.KindString},
	}, []*graph.Edge{
		{From: "changed", To: "field", Kind: graph.EdgeAccessesField, FilePath: "changed.go", Line: 1},
		{From: "changed", To: "process", Kind: graph.EdgeExecutesProcess, FilePath: "changed.go", Line: 2},
		{From: "unchanged", To: "field", Kind: graph.EdgeAccessesField, FilePath: "unchanged.go", Line: 3},
		{From: "changed", To: "field", Kind: graph.EdgeWrites, FilePath: "changed.go", Line: 4},
	})

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	removed, err := store.EvictEdgesFromSourcesByKinds(cancelled,
		[]string{"changed"}, []graph.EdgeKind{graph.EdgeAccessesField})
	if !errors.Is(err, context.Canceled) || removed != 0 {
		t.Fatalf("cancelled eviction = (%d, %v), want (0, context.Canceled)", removed, err)
	}
	if got := store.EdgeCount(); got != 4 {
		t.Fatalf("cancelled eviction changed edge count to %d, want 4", got)
	}

	removed, err = store.EvictEdgesFromSourcesByKinds(context.Background(),
		[]string{"changed", "changed"},
		[]graph.EdgeKind{graph.EdgeAccessesField, graph.EdgeExecutesProcess, graph.EdgeAccessesField})
	if err != nil || removed != 2 {
		t.Fatalf("scoped eviction = (%d, %v), want (2, nil)", removed, err)
	}
	if got := store.EdgeCount(); got != 2 {
		t.Fatalf("remaining edge count = %d, want 2", got)
	}
	if got := store.GetOutEdges("unchanged"); len(got) != 1 || got[0].Kind != graph.EdgeAccessesField {
		t.Fatalf("unchanged source was touched: %#v", got)
	}
	if got := store.GetOutEdges("changed"); len(got) != 1 || got[0].Kind != graph.EdgeWrites {
		t.Fatalf("base edge was touched: %#v", got)
	}

	// Mirrors the production DELETE in EvictEdgesFromSourcesByKinds,
	// including its handle-generation predicate: the generation-first
	// edges_by_from key is only seekable when view_gen is bound.
	for _, generation := range []int64{baseViewGeneration, 17} {
		plan := sqliteExplainPlan(t, store.db, `
DELETE FROM edges
WHERE from_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))
  AND kind IN (SELECT CAST(value AS TEXT) FROM json_each(?))
  AND view_gen = ?`, `["changed"]`, `["accesses_field"]`, generation)
		if !strings.Contains(plan, "edges_by_from (view_gen=? AND from_id=? AND kind=?)") {
			t.Fatalf("generation %d: scoped eviction must seek the (view_gen, from_id, kind) index; plan:\n%s", generation, plan)
		}
		if strings.Contains(plan, "SCAN edges") {
			t.Fatalf("generation %d: scoped eviction scanned the edge table; plan:\n%s", generation, plan)
		}
	}
}
