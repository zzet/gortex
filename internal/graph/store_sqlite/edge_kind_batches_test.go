package store_sqlite

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func openEdgeKindBatchTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open SQLite store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close SQLite store: %v", err)
		}
	})
	return store
}

func TestScanEdgesByKindsBatchedBoundsRewritesAndFreezesHighWater(t *testing.T) {
	store := openEdgeKindBatchTestStore(t)
	// Pool-wide InUse can otherwise count unrelated maintenance readers.
	stopStoreBackgroundReaders(store)
	store.db.SetMaxOpenConns(1)
	store.db.SetMaxIdleConns(1)

	const selectedCount = 7
	edges := make([]*graph.Edge, 0, selectedCount+2)
	for i := 0; i < selectedCount; i++ {
		kind := graph.EdgeReads
		if i%2 != 0 {
			kind = graph.EdgeReferences
		}
		edges = append(edges, &graph.Edge{
			From: fmt.Sprintf("from-%02d", i), To: fmt.Sprintf("unresolved::name-%02d", i),
			Kind: kind, FilePath: "repo/file.go", Line: i + 1,
			Confidence: 0.75, ConfidenceLabel: "high", Origin: "parser", Tier: "syntax",
			CrossRepo: true,
			Meta: map[string]any{
				"custom": "value", "resolve_terminal": true,
				"resolve_terminal_reason": "fixture", "semantic_source": "test",
			},
		})
	}
	edges = append(edges,
		&graph.Edge{From: "ignored-1", To: "target", Kind: graph.EdgeCalls, FilePath: "repo/file.go", Line: 100},
		&graph.Edge{From: "ignored-2", To: "target", Kind: graph.EdgeImports, FilePath: "repo/file.go", Line: 101},
	)
	store.AddBatch(nil, edges)

	seen := make(map[string]int, selectedCount)
	var batchSizes []int
	store.ScanEdgesByKindsBatched(
		[]graph.EdgeKind{graph.EdgeReads, "", graph.EdgeReferences, graph.EdgeReads},
		3,
		func(page []*graph.Edge) bool {
			if inUse := store.db.Stats().InUse; inUse != 0 {
				t.Fatalf("yield retained %d read connection(s)", inUse)
			}
			batchSizes = append(batchSizes, len(page))
			rewrites := make([]graph.EdgeReindex, 0, len(page))
			for _, edge := range page {
				seen[edge.From]++
				if edge.Confidence != 0.75 || edge.ConfidenceLabel != "high" || edge.Origin != "parser" || edge.Tier != "syntax" || !edge.CrossRepo {
					t.Fatalf("edge payload was not preserved: %#v", edge)
				}
				if edge.Meta["custom"] != "value" || edge.Meta["resolve_terminal_reason"] != "fixture" || edge.Meta["semantic_source"] != "test" {
					t.Fatalf("edge metadata was not preserved: %#v", edge.Meta)
				}
				oldTo := edge.To
				edge.To = "resolved::" + edge.From
				rewrites = append(rewrites, graph.EdgeReindex{Edge: edge, OldTo: oldTo})
			}
			store.ReindexEdges(rewrites)
			return true
		},
	)

	if len(seen) != selectedCount {
		t.Fatalf("visited %d selected edges, want %d: %v", len(seen), selectedCount, seen)
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("edge %q visited %d times", id, count)
		}
	}
	for _, size := range batchSizes {
		if size < 1 || size > 3 {
			t.Fatalf("batch size = %d, want 1..3", size)
		}
	}
}

func TestScanEdgesByKindsBatchedEmptyAndEarlyStop(t *testing.T) {
	store := openEdgeKindBatchTestStore(t)
	store.AddBatch(nil, []*graph.Edge{
		{From: "from-1", To: "to-1", Kind: graph.EdgeReads, FilePath: "repo/file.go", Line: 1},
		{From: "from-2", To: "to-2", Kind: graph.EdgeReads, FilePath: "repo/file.go", Line: 2},
	})

	store.ScanEdgesByKindsBatched(nil, 2, func([]*graph.Edge) bool {
		t.Fatal("empty kinds invoked callback")
		return false
	})
	calls := 0
	store.ScanEdgesByKindsBatched([]graph.EdgeKind{graph.EdgeReads}, 1, func(page []*graph.Edge) bool {
		calls++
		if len(page) != 1 {
			t.Fatalf("page size = %d, want 1", len(page))
		}
		return false
	})
	if calls != 1 {
		t.Fatalf("callback calls = %d, want 1", calls)
	}
}
