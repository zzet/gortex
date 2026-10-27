package graph

import "testing"

// These tests pin PR zzet/gortex#901 review R1: every write capability the
// memory backend exposes must be JOURNALED. The indexer reaches the live
// store through capability assertions (FileBatchEvicter, EdgeKindEvicter,
// PathlessNodeBatchEvicter) and, before this refactor, the embedded *Graph
// answered those assertions with unjournaled writes: a crash between an
// eviction and the next snapshot replayed the evicted nodes and edges back,
// and the write never took applyMu.
//
// Each test asserts three things through the capability interface:
//  1. the capability reaches the store at all,
//  2. the eviction grows the journal,
//  3. replaying that journal into a fresh store keeps the eviction.

func capabilityTestNode(id, filePath string) *Node {
	return &Node{
		ID:       id,
		Kind:     KindFunction,
		Name:     id,
		FilePath: filePath,
		Language: "go",
	}
}

// TestMemoryStoreJournalsFileBatchEviction covers the incremental reindex
// path: the indexer evicts a file batch through graph.FileBatchEvicter
// (internal/indexer/incremental_batch.go), so that write must be journaled.
func TestMemoryStoreJournalsFileBatchEviction(t *testing.T) {
	store, path := journalTestStore(t)
	store.AddBatch([]*Node{
		capabilityTestNode("gone1", "a.go"),
		capabilityTestNode("gone2", "b.go"),
		capabilityTestNode("keep", "keep.go"),
	}, []*Edge{{From: "keep", To: "gone1", Kind: EdgeCalls, FilePath: "keep.go"}})

	evicter, ok := any(store).(FileBatchEvicter)
	if !ok {
		t.Fatalf("MemoryStore must expose FileBatchEvicter: only journaled writes may reach the live store (PR #901 review R1)")
	}
	before := store.JournalBytes()
	nodesRemoved, edgesRemoved := evicter.EvictFiles([]string{"a.go", "b.go"})
	if nodesRemoved != 2 || edgesRemoved != 1 {
		t.Fatalf("EvictFiles removed %d nodes/%d edges, want 2/1", nodesRemoved, edgesRemoved)
	}
	if grown := store.JournalBytes(); grown <= before {
		t.Fatalf("file batch eviction was not journaled: bytes %d -> %d", before, grown)
	}
	if err := store.CloseJournal(); err != nil {
		t.Fatalf("CloseJournal: %v", err)
	}

	replayed := NewMemoryStore()
	if _, err := replayed.ReplayJournal(path); err != nil {
		t.Fatalf("ReplayJournal: %v", err)
	}
	if got := replayed.GetNode("gone1"); got != nil {
		t.Fatalf("replay resurrected evicted node gone1: %+v", got)
	}
	if got := replayed.GetNode("gone2"); got != nil {
		t.Fatalf("replay resurrected evicted node gone2: %+v", got)
	}
	if replayed.GetNode("keep") == nil {
		t.Fatal("surviving node keep lost in replay")
	}
	if got := replayed.EdgeCount(); got != 0 {
		t.Fatalf("edges after replay: want 0 (keep->gone1 evicted), got %d", got)
	}
}

// TestMemoryStoreJournalsEdgeKindEviction covers the set-oriented edge
// eviction the resolver uses through graph.EdgeKindEvicter.
func TestMemoryStoreJournalsEdgeKindEviction(t *testing.T) {
	store, path := journalTestStore(t)
	store.AddBatch([]*Node{
		capabilityTestNode("a", "a.go"),
		capabilityTestNode("b", "b.go"),
		capabilityTestNode("c", "c.go"),
	}, []*Edge{
		{From: "a", To: "b", Kind: EdgeCalls, FilePath: "a.go"},
		{From: "a", To: "c", Kind: EdgeCalls, FilePath: "a.go"},
	})

	evicter, ok := any(store).(EdgeKindEvicter)
	if !ok {
		t.Fatalf("MemoryStore must expose EdgeKindEvicter: only journaled writes may reach the live store (PR #901 review R1)")
	}
	before := store.JournalBytes()
	if removed := evicter.EvictEdgesByKinds([]EdgeKind{EdgeCalls}); removed != 2 {
		t.Fatalf("EvictEdgesByKinds removed %d edges, want 2", removed)
	}
	if grown := store.JournalBytes(); grown <= before {
		t.Fatalf("edge-kind eviction was not journaled: bytes %d -> %d", before, grown)
	}
	if err := store.CloseJournal(); err != nil {
		t.Fatalf("CloseJournal: %v", err)
	}

	replayed := NewMemoryStore()
	if _, err := replayed.ReplayJournal(path); err != nil {
		t.Fatalf("ReplayJournal: %v", err)
	}
	if got := replayed.EdgeCount(); got != 0 {
		t.Fatalf("edges after replay: want 0, got %d", got)
	}
	for _, id := range []string{"a", "b", "c"} {
		if replayed.GetNode(id) == nil {
			t.Fatalf("node %q lost in replay", id)
		}
	}
}

// TestMemoryStoreJournalsPathlessNodeEviction covers the resolver-stub
// eviction (pruneUnanchoredPathless / pruneRedundantPathless) that reaches
// the live store through graph.PathlessNodeBatchEvicter.
func TestMemoryStoreJournalsPathlessNodeEviction(t *testing.T) {
	store, path := journalTestStore(t)
	store.AddBatch([]*Node{
		capabilityTestNode("external::dep", ""),
		capabilityTestNode("keep", "keep.go"),
	}, []*Edge{{From: "keep", To: "external::dep", Kind: EdgeCalls, FilePath: "keep.go"}})

	evicter, ok := any(store).(PathlessNodeBatchEvicter)
	if !ok {
		t.Fatalf("MemoryStore must expose PathlessNodeBatchEvicter: only journaled writes may reach the live store (PR #901 review R1)")
	}
	before := store.JournalBytes()
	nodesRemoved, edgesRemoved := evicter.EvictPathlessNodesByIDs([]string{"external::dep"})
	if nodesRemoved != 1 || edgesRemoved != 1 {
		t.Fatalf("EvictPathlessNodesByIDs removed %d nodes/%d edges, want 1/1", nodesRemoved, edgesRemoved)
	}
	if grown := store.JournalBytes(); grown <= before {
		t.Fatalf("pathless eviction was not journaled: bytes %d -> %d", before, grown)
	}
	if err := store.CloseJournal(); err != nil {
		t.Fatalf("CloseJournal: %v", err)
	}

	replayed := NewMemoryStore()
	if _, err := replayed.ReplayJournal(path); err != nil {
		t.Fatalf("ReplayJournal: %v", err)
	}
	if got := replayed.GetNode("external::dep"); got != nil {
		t.Fatalf("replay resurrected evicted pathless node: %+v", got)
	}
	if replayed.GetNode("keep") == nil {
		t.Fatal("surviving node keep lost in replay")
	}
	if got := replayed.EdgeCount(); got != 0 {
		t.Fatalf("edges after replay: want 0, got %d", got)
	}
}
