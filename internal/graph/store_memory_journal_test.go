package graph

import (
	"path/filepath"
	"testing"
)

func journalTestStore(t *testing.T) (*MemoryStore, string) {
	t.Helper()
	m := NewMemoryStore()
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	if err := m.OpenJournal(path); err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	t.Cleanup(func() { _ = m.CloseJournal() })
	return m, path
}

func journalTestNode(id, name string) *Node {
	return &Node{
		ID:       id,
		Kind:     KindFunction,
		Name:     name,
		FilePath: "x.go",
		Language: "go",
	}
}

func journalTestEdge(from, to string) *Edge {
	return &Edge{
		From:     from,
		To:       to,
		Kind:     EdgeCalls,
		Origin:   "parse",
		FilePath: "x.go",
		Line:     3,
	}
}

// TestMemoryStoreJournalSkipsInertMutations pins the #22 guard: identical
// re-adds, same-origin provenance updates and ghost removals are applied but
// not journaled; only effective changes reach the journal.
func TestMemoryStoreJournalSkipsInertMutations(t *testing.T) {
	m, _ := journalTestStore(t)

	n := journalTestNode("a", "Alpha")
	e := journalTestEdge("a", "b")
	m.AddNode(n)
	m.AddEdge(e)
	baseline := m.JournalBytes()
	if baseline == 0 {
		t.Fatalf("expected the first add to journal bytes")
	}

	// Identical re-adds (fresh but deep-equal payloads) — applied, unjournaled.
	m.AddNode(journalTestNode("a", "Alpha"))
	m.AddEdge(journalTestEdge("a", "b"))
	m.AddBatch([]*Node{journalTestNode("a", "Alpha")}, []*Edge{journalTestEdge("a", "b")})
	m.SetEdgeProvenance(journalTestEdge("a", "b"), "parse")
	if grown := m.JournalBytes(); grown != baseline {
		t.Fatalf("inert re-adds grew the journal: %d -> %d", baseline, grown)
	}

	// Ghost removal — applied (no-op), unjournaled.
	if m.RemoveEdge("ghost", "b", EdgeCalls) {
		t.Fatalf("RemoveEdge(ghost) reported removal")
	}
	if grown := m.JournalBytes(); grown != baseline {
		t.Fatalf("ghost removal grew the journal: %d -> %d", baseline, grown)
	}

	// Effective changes must still journal: Meta change, origin change.
	changed := journalTestNode("a", "Alpha")
	changed.Meta = map[string]any{"enriched": true}
	m.AddNode(changed)
	if grown := m.JournalBytes(); grown <= baseline {
		t.Fatalf("Meta change did not grow the journal (%d)", grown)
	}
	afterMeta := m.JournalBytes()

	m.SetEdgeProvenance(journalTestEdge("a", "b"), "resolver")
	if grown := m.JournalBytes(); grown <= afterMeta {
		t.Fatalf("origin change did not grow the journal (%d)", grown)
	}
	afterOrigin := m.JournalBytes()

	// A brand-new node/edge still journals.
	m.AddNode(journalTestNode("c", "Gamma"))
	if grown := m.JournalBytes(); grown <= afterOrigin {
		t.Fatalf("new node did not grow the journal (%d)", grown)
	}
}

// TestMemoryStoreJournalInertReplayConsistency pins the replay contract: a
// journal that skipped inert ops reproduces the writer's state exactly on a
// fresh store (an inert op is by definition already implied by the state the
// replay starts from).
func TestMemoryStoreJournalInertReplayConsistency(t *testing.T) {
	m, path := journalTestStore(t)

	n := journalTestNode("a", "Alpha")
	n.Meta = map[string]any{"k": "v"}
	e := journalTestEdge("a", "b")
	e.Confidence = 0.75
	m.AddNode(n)
	m.AddEdge(e)
	// A round of fully-inert traffic between the effective ops — the re-adds
	// must be deep-equal to the stored payloads (Confidence and Meta included,
	// both json:"-" and invisible to the journal projection itself).
	inertNode := journalTestNode("a", "Alpha")
	inertNode.Meta = map[string]any{"k": "v"}
	inertEdge := journalTestEdge("a", "b")
	inertEdge.Confidence = 0.75
	m.AddNode(inertNode)
	m.AddEdge(inertEdge)
	m.SetEdgeProvenance(journalTestEdge("a", "b"), "parse")
	// Then real changes.
	renamed := journalTestNode("a", "AlphaRenamed")
	renamed.Meta = map[string]any{"k": "v"}
	m.AddNode(renamed)
	m.AddEdge(journalTestEdge("a", "c"))
	if err := m.CloseJournal(); err != nil {
		t.Fatalf("CloseJournal: %v", err)
	}

	replay := NewMemoryStore()
	ops, err := replay.ReplayJournal(path)
	if err != nil {
		t.Fatalf("ReplayJournal: %v", err)
	}
	if ops == 0 {
		t.Fatalf("replay applied 0 ops")
	}

	if got := replay.GetNode("a"); got == nil || got.Name != "AlphaRenamed" {
		t.Fatalf("replayed node = %+v, want AlphaRenamed", got)
	}
	if got := replay.GetNode("a").Meta["k"]; got != "v" {
		t.Fatalf("replayed node Meta = %v, want k=v", got)
	}
	out := replay.GetOutEdges("a")
	if len(out) != 2 {
		t.Fatalf("replayed edges = %d, want 2 (a→b, a→c)", len(out))
	}
	for _, got := range out {
		if got.To == "b" && got.Origin != "parse" {
			t.Fatalf("replayed a→b Origin = %q, want parse", got.Origin)
		}
		if got.To == "b" && got.Confidence != 0.75 {
			t.Fatalf("replayed a→b Confidence = %v, want 0.75", got.Confidence)
		}
	}
}
