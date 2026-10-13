package graph

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func newJournalPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "journal.jsonl")
}

func testNode(id string) *Node {
	return &Node{
		ID: id, Kind: KindFunction, Name: "Foo",
		FilePath: "internal/x.go", Language: "go", RepoPrefix: "gortex",
		Meta: map[string]any{"semantic_source": "lsp", "n": 42},
	}
}

func testEdge(from, to string) *Edge {
	return &Edge{
		From: from, To: to, Kind: EdgeCalls,
		FilePath: "internal/x.go", Line: 1,
		Meta:       map[string]any{"semantic_source": "lsp"},
		Confidence: 0.9,
	}
}

// TestMemoryStoreJournalReplay covers the full write-surface round trip:
// every journalled mutation must reproduce identical state on a fresh store.
func TestMemoryStoreJournalReplay(t *testing.T) {
	path := newJournalPath(t)
	store := NewMemoryStore()
	if err := store.OpenJournal(path); err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}

	a, b, c := testNode("a"), testNode("b"), testNode("c")
	store.AddNode(a)
	store.AddNode(b)
	store.AddNode(c)
	store.AddEdge(testEdge("a", "b"))
	store.AddEdge(testEdge("a", "c"))
	store.SetEdgeProvenance(testEdge("a", "b"), "lsp")
	store.ReindexEdge(testEdge("a", "b"), "b-old")
	store.ReindexEdges([]EdgeReindex{{Edge: testEdge("a", "c"), OldTo: "c-old"}})
	store.SetEdgeProvenanceBatch([]EdgeProvenanceUpdate{{Edge: testEdge("a", "c"), NewOrigin: "ast"}})
	store.RemoveEdge("a", "c", EdgeCalls)
	store.EvictFile("internal/x.go") // evicts everything; re-add after
	store.AddNode(a)
	store.AddEdge(testEdge("a", "b"))

	if err := store.CloseJournal(); err != nil {
		t.Fatalf("CloseJournal: %v", err)
	}
	if store.JournalErr() != nil {
		t.Fatalf("journal degraded during recording: %v", store.JournalErr())
	}

	// Replay into a fresh store.
	replayed := NewMemoryStore()
	ops, err := replayed.ReplayJournal(path)
	if err != nil {
		t.Fatalf("ReplayJournal: %v", err)
	}
	if ops == 0 {
		t.Fatal("replay applied zero operations")
	}
	if replayed.GetNode("a") == nil {
		t.Fatal("node a missing after replay")
	}
	if got := replayed.GetNode("a").Meta["semantic_source"]; got != "lsp" {
		t.Errorf("node Meta lost in replay: got %v", got)
	}
	edges := replayed.GetOutEdges("a")
	if len(edges) != 1 || edges[0].To != "b" {
		t.Fatalf("adjacency after replay: want exactly a->b, got %+v", edges)
	}
	if edges[0].Confidence != 0.9 {
		t.Errorf("edge Confidence lost in replay: got %v", edges[0].Confidence)
	}
	if edges[0].Meta["semantic_source"] != "lsp" {
		t.Errorf("edge Meta lost in replay: %+v", edges[0].Meta)
	}
}

// TestMemoryStoreAddBatchChunkedReplay exercises the AddBatch chunking path:
// a batch larger than the chunk size must journal as multiple bounded records
// and replay to the same state.
func TestMemoryStoreAddBatchChunkedReplay(t *testing.T) {
	path := newJournalPath(t)
	store := NewMemoryStore()
	if err := store.OpenJournal(path); err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	const total = journalAddBatchChunk + 37
	nodes := make([]*Node, total)
	for i := range nodes {
		nodes[i] = testNode(string(rune('a'+i%26)) + "-n" + strconv.Itoa(i))
	}
	edges := make([]*Edge, 10)
	for i := range edges {
		edges[i] = testEdge(nodes[i].ID, nodes[i+1].ID)
	}
	store.AddBatch(nodes, edges)
	if err := store.CloseJournal(); err != nil {
		t.Fatalf("CloseJournal: %v", err)
	}

	replayed := NewMemoryStore()
	if _, err := replayed.ReplayJournal(path); err != nil {
		t.Fatalf("ReplayJournal: %v", err)
	}
	if replayed.NodeCount() != total {
		t.Errorf("nodes after replay: want %d, got %d", total, replayed.NodeCount())
	}
	if replayed.EdgeCount() != len(edges) {
		t.Errorf("edges after replay: want %d, got %d", len(edges), replayed.EdgeCount())
	}
}

// TestMemoryStoreTornTailReplay asserts the crash-tolerance contract: a torn
// final line stops replay at the last complete record without error.
func TestMemoryStoreTornTailReplay(t *testing.T) {
	path := newJournalPath(t)
	store := NewMemoryStore()
	if err := store.OpenJournal(path); err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	store.AddNode(testNode("a"))
	if err := store.CloseJournal(); err != nil {
		t.Fatalf("CloseJournal: %v", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open journal for tail corruption: %v", err)
	}
	if _, err := f.WriteString(`{"op":"add_node","node":{"id":"to`); err != nil { // torn write
		t.Fatalf("torn write: %v", err)
	}
	f.Close()

	replayed := NewMemoryStore()
	ops, err := replayed.ReplayJournal(path)
	if err != nil {
		t.Fatalf("ReplayJournal on torn tail: %v", err)
	}
	if ops != 1 {
		t.Errorf("replayed ops: want 1 (complete record), got %d", ops)
	}
	if replayed.GetNode("a") == nil {
		t.Fatal("complete record lost to torn tail")
	}
}

// TestMemoryStoreClosedJournalKeepsServing asserts the degrade policy: with
// no journal attached (or after CloseJournal), mutations still work purely
// in-memory.
func TestMemoryStoreClosedJournalKeepsServing(t *testing.T) {
	store := NewMemoryStore() // journal never opened
	n := testNode("a")
	store.AddNode(n)
	store.AddEdge(testEdge("a", "b"))
	if store.GetNode("a") == nil || store.EdgeCount() != 1 {
		t.Fatal("store must serve without a journal")
	}
	if err := store.OpenJournal(newJournalPath(t)); err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if err := store.CloseJournal(); err != nil {
		t.Fatalf("CloseJournal: %v", err)
	}
	store.AddNode(testNode("c"))
	if store.GetNode("c") == nil {
		t.Fatal("store must keep serving after CloseJournal")
	}
	if err := store.ResetJournal(); err != nil {
		t.Errorf("ResetJournal without an open journal must be a no-op, got %v", err)
	}
}

// TestMemoryStoreResetJournal asserts that a reset empties the journal so a
// post-snapshot reopen replays nothing.
func TestMemoryStoreResetJournal(t *testing.T) {
	path := newJournalPath(t)
	store := NewMemoryStore()
	if err := store.OpenJournal(path); err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	store.AddNode(testNode("a"))
	if store.JournalBytes() == 0 {
		t.Fatal("journal must have bytes after a mutation")
	}
	if err := store.ResetJournal(); err != nil {
		t.Fatalf("ResetJournal: %v", err)
	}
	if store.JournalBytes() != 0 {
		t.Errorf("journal bytes after reset: want 0, got %d", store.JournalBytes())
	}
	if err := store.CloseJournal(); err != nil {
		t.Fatalf("CloseJournal: %v", err)
	}
	replayed := NewMemoryStore()
	ops, err := replayed.ReplayJournal(path)
	if err != nil {
		t.Fatalf("ReplayJournal: %v", err)
	}
	if ops != 0 {
		t.Errorf("replayed ops after reset: want 0, got %d", ops)
	}
}

