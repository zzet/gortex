package graph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedJournal journals one real node through the store's own write path and
// closes the journal, leaving a valid one-op file the tests append to.
func seedJournal(t *testing.T, path string) *MemoryStore {
	t.Helper()
	m := NewMemoryStore()
	if err := m.OpenJournal(path); err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	m.AddNode(&Node{ID: "repoA/a.go::f", Kind: KindFunction, Name: "f", FilePath: "a.go"})
	if err := m.CloseJournal(); err != nil {
		t.Fatalf("CloseJournal: %v", err)
	}
	return m
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatalf("append %q: %v", l, err)
		}
	}
}

func TestReplayJournalCountsUnknownOps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.jsonl")
	seedJournal(t, path)
	appendLines(t, path, `{"op":"add_batch_nodes","nodes":[{}]}`, `{"op":"from_the_future"}`)

	m := NewMemoryStore()
	ops, err := m.ReplayJournal(path)
	if err != nil {
		t.Fatalf("ReplayJournal: %v", err)
	}
	if ops != 2 {
		t.Fatalf("applied ops: want 2 (seed + batch; unknown excluded), got %d", ops)
	}
	unknown, corruptMidFile, corruptLine := m.ReplayAnomalies()
	if unknown != 1 {
		t.Fatalf("unknown ops: want 1, got %d", unknown)
	}
	if corruptMidFile || corruptLine != 0 {
		t.Fatalf("unexpected corruption signal: midFile=%v line=%d", corruptMidFile, corruptLine)
	}
}

func TestReplayJournalMidFileCorruptionFlagged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.jsonl")
	seedJournal(t, path)
	// Corrupt line in the MIDDLE: a journal this process cannot have
	// written. Replay stops (derived-cache semantics) but must name the
	// shape, not silently pass it off as a torn tail.
	appendLines(t, path, "{not json at all", `{"op":"add_node"}`)

	m := NewMemoryStore()
	ops, err := m.ReplayJournal(path)
	if err != nil {
		t.Fatalf("ReplayJournal: %v", err)
	}
	if ops != 1 {
		t.Fatalf("applied ops: want 1, got %d", ops)
	}
	_, corruptMidFile, corruptLine := m.ReplayAnomalies()
	if !corruptMidFile {
		t.Fatalf("mid-file corruption not flagged")
	}
	if corruptLine != 2 {
		t.Fatalf("corrupt line: want 2, got %d", corruptLine)
	}
}

func TestReplayJournalTornTailNotFlaggedMidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.jsonl")
	seedJournal(t, path)
	// Corrupt LAST line: the ordinary crash-between-flushes shape.
	appendLines(t, path, `{"op":"add_nod`)

	m := NewMemoryStore()
	ops, err := m.ReplayJournal(path)
	if err != nil {
		t.Fatalf("ReplayJournal: %v", err)
	}
	if ops != 1 {
		t.Fatalf("applied ops: want 1, got %d", ops)
	}
	_, corruptMidFile, corruptLine := m.ReplayAnomalies()
	if corruptMidFile {
		t.Fatalf("torn tail flagged as mid-file corruption")
	}
	if corruptLine != 2 {
		t.Fatalf("corrupt line: want 2, got %d", corruptLine)
	}
}

func TestReplayJournalEmptyBatchMemberTolerated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.jsonl")
	seedJournal(t, path)
	// A structurally valid batch op whose member projects to a zero-value
	// node: replay must neither panic nor invent a mutation.
	appendLines(t, path, `{"op":"add_batch_nodes","nodes":[{}]}`)

	m := NewMemoryStore()
	ops, err := m.ReplayJournal(path)
	if err != nil {
		t.Fatalf("ReplayJournal: %v", err)
	}
	if ops != 2 {
		t.Fatalf("applied ops: want 2, got %d", ops)
	}
	if n := m.GetNode(""); n != nil {
		t.Fatalf("zero-value member materialised a node: %+v", n)
	}
	if unknown, _, _ := m.ReplayAnomalies(); unknown != 0 {
		t.Fatalf("empty batch member counted as unknown op: %d", unknown)
	}
}

// TestJournalDegradeToInMemory exercises the JournalErr contract: once the
// journal append fails, the store keeps serving in memory and surfaces the
// first failure instead of retrying or crashing.
func TestJournalDegradeToInMemory(t *testing.T) {
	dir := t.TempDir()
	m := NewMemoryStore()
	if err := m.OpenJournal(filepath.Join(dir, "journal.jsonl")); err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if err := m.journalFile.Close(); err != nil {
		t.Fatalf("pre-close: %v", err)
	}

	n := &Node{ID: "repoA/b.go::g", Kind: KindFunction, Name: "g", FilePath: "b.go"}
	m.AddNode(n) // record fails on the closed file; apply must still land

	if m.JournalErr() == nil {
		t.Fatalf("JournalErr not set after append failure")
	}
	if got := m.GetNode(n.ID); got == nil || got.Name != "g" {
		t.Fatalf("degraded store lost the applied node: %+v", got)
	}
	// CloseJournal may legitimately fail here (it flushes onto the already
	// closed file); the degrade contract only requires JournalErr surfaced
	// and the graph serving.
	_ = m.CloseJournal()
	if !strings.Contains(m.JournalErr().Error(), "memory journal") {
		t.Fatalf("unexpected JournalErr shape: %v", m.JournalErr())
	}
}
