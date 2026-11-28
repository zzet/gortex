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

// TestMemoryStoreJournalsExactEdgeRemoval pins the exact-identity capability:
// two sibling call sites share from/to/kind but differ by line, and only the
// requested identity may be removed. RemoveEdgesExact's documented coarse
// fallback (one RemoveEdge per from/to/kind) would drop a sibling here, so the
// memory backend must expose the set-oriented capability, journaled.
func TestMemoryStoreJournalsExactEdgeRemoval(t *testing.T) {
	store, path := journalTestStore(t)
	store.AddBatch([]*Node{
		capabilityTestNode("a", "a.go"),
		capabilityTestNode("b", "b.go"),
	}, []*Edge{
		{From: "a", To: "b", Kind: EdgeCalls, FilePath: "a.go", Line: 10},
		{From: "a", To: "b", Kind: EdgeCalls, FilePath: "a.go", Line: 20},
	})
	if got := store.EdgeCount(); got != 2 {
		t.Fatalf("seeded edges: want 2 siblings, got %d", got)
	}

	remover, ok := any(store).(ExactEdgeBatchRemover)
	if !ok {
		t.Fatalf("MemoryStore must expose ExactEdgeBatchRemover: the coarse RemoveEdge fallback would drop sibling call sites (PR #901 review R1)")
	}
	before := store.JournalBytes()
	removed := remover.RemoveEdgesExact([]*Edge{{From: "a", To: "b", Kind: EdgeCalls, FilePath: "a.go", Line: 10}})
	if removed != 1 {
		t.Fatalf("RemoveEdgesExact removed %d edges, want exactly 1", removed)
	}
	if grown := store.JournalBytes(); grown <= before {
		t.Fatalf("exact edge removal was not journaled: bytes %d -> %d", before, grown)
	}
	if err := store.CloseJournal(); err != nil {
		t.Fatalf("CloseJournal: %v", err)
	}

	replayed := NewMemoryStore()
	if _, err := replayed.ReplayJournal(path); err != nil {
		t.Fatalf("ReplayJournal: %v", err)
	}
	out := replayed.GetOutEdges("a")
	if len(out) != 1 {
		t.Fatalf("sibling edges after replay: want 1, got %d", len(out))
	}
	if out[0].Line != 20 {
		t.Fatalf("replay removed the wrong sibling: surviving line %d, want 20", out[0].Line)
	}
}

// TestMemoryStoreJournalsDerivedContractReplacement pins the composite
// frontier write: the edge removal, the added nodes/edges and the touched
// topic set must replay as one record, exactly as the live call applied them.
func TestMemoryStoreJournalsDerivedContractReplacement(t *testing.T) {
	store, path := journalTestStore(t)
	store.AddBatch([]*Node{
		capabilityTestNode("a", "a.go"),
		capabilityTestNode("b", "b.go"),
	}, []*Edge{
		{From: "a", To: "b", Kind: EdgeCalls, FilePath: "a.go", Line: 10},
		{From: "a", To: "b", Kind: EdgeCalls, FilePath: "a.go", Line: 20},
	})

	replacer, ok := any(store).(DerivedContractReplacer)
	if !ok {
		t.Fatalf("MemoryStore must expose DerivedContractReplacer: it is the resolver's live frontier path, not an optional enrichment (PR #901 review R1)")
	}
	topic := &Node{ID: "topic::t1", Kind: KindTopic, Name: "t1", FilePath: "", Language: "go"}
	before := store.JournalBytes()
	result, err := replacer.ReplaceDerivedContracts(DerivedContractReplacement{
		RemoveEdges: []*Edge{{From: "a", To: "b", Kind: EdgeCalls, FilePath: "a.go", Line: 10}},
		Nodes:       []*Node{topic},
		Edges:       []*Edge{{From: "a", To: "topic::t1", Kind: EdgeProducesTopic, FilePath: "a.go"}},
	})
	if err != nil {
		t.Fatalf("ReplaceDerivedContracts: %v", err)
	}
	if result.EdgesRemoved != 1 || result.NodesChanged != 1 || result.EdgesAdded != 1 {
		t.Fatalf("result = %+v, want 1 edge removed / 1 node changed / 1 edge added", result)
	}
	if grown := store.JournalBytes(); grown <= before {
		t.Fatalf("derived contract replacement was not journaled: bytes %d -> %d", before, grown)
	}
	if err := store.CloseJournal(); err != nil {
		t.Fatalf("CloseJournal: %v", err)
	}

	replayed := NewMemoryStore()
	if _, err := replayed.ReplayJournal(path); err != nil {
		t.Fatalf("ReplayJournal: %v", err)
	}
	if replayed.GetNode("topic::t1") == nil {
		t.Fatal("added topic node lost in replay")
	}
	if got := replayed.EdgeCount(); got != 2 {
		t.Fatalf("edges after replay: want 2 (line-20 calls + produces-topic), got %d", got)
	}
	var sawCalls, sawProduces bool
	for _, e := range replayed.GetOutEdges("a") {
		if e.Kind == EdgeCalls && e.Line == 20 {
			sawCalls = true
		}
		if e.Kind == EdgeProducesTopic && e.To == "topic::t1" {
			sawProduces = true
		}
	}
	if !sawCalls || !sawProduces {
		t.Fatalf("replayed frontier wrong: calls@20=%v produces_topic=%v", sawCalls, sawProduces)
	}
}
