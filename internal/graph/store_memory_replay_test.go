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
	unknown, corruptMidFile, corruptLine, skippedStale, epochLagged := m.ReplayAnomalies()
	if unknown != 1 {
		t.Fatalf("unknown ops: want 1, got %d", unknown)
	}
	if corruptMidFile || corruptLine != 0 || skippedStale || epochLagged {
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
	_, corruptMidFile, corruptLine, _, _ := m.ReplayAnomalies()
	if !corruptMidFile {
		t.Fatalf("mid-file corruption not flagged")
	}
	if corruptLine != 3 {
		t.Fatalf("corrupt line: want 3 (header + op + appended), got %d", corruptLine)
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
	_, corruptMidFile, corruptLine, _, _ := m.ReplayAnomalies()
	if corruptMidFile {
		t.Fatalf("torn tail flagged as mid-file corruption")
	}
	if corruptLine != 3 {
		t.Fatalf("corrupt line: want 3 (header + op + appended), got %d", corruptLine)
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
	if unknown, _, _, _, _ := m.ReplayAnomalies(); unknown != 0 {
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

func TestJournalEpochHeaderWrittenAndBumped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.jsonl")
	if NewMemoryStore().JournalEpoch() != 0 {
		t.Fatalf("fresh store epoch: want 0")
	}
	m := seedJournal(t, path) // open + one op + close
	if m.JournalEpoch() != 0 {
		t.Fatalf("epoch after open: want 0, got %d", m.JournalEpoch())
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasPrefix(string(first), `{"op":"journal_open","ts":`) ||
		!strings.Contains(string(first), `"epoch":0`) {
		t.Fatalf("first journal line is not the epoch-0 header: %q", first)
	}

	// A reset after a (simulated) snapshot bumps the generation and re-arms
	// the header.
	if err := m.OpenJournal(path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := m.ResetJournal(); err != nil {
		t.Fatalf("ResetJournal: %v", err)
	}
	if m.JournalEpoch() != 1 {
		t.Fatalf("epoch after reset: want 1, got %d", m.JournalEpoch())
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after reset: %v", err)
	}
	if !strings.Contains(string(second), `"epoch":1`) {
		t.Fatalf("reset did not write the epoch-1 header: %q", second)
	}
}

// TestReplaySkipsJournalOlderThanSnapshot pins the epoch pairing: a journal
// whose header generation is below the loaded snapshot's is fully contained
// in that snapshot (mutations are blocked for the whole dump), so replay
// skips it and reports why.
func TestReplaySkipsJournalOlderThanSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.jsonl")
	seedJournal(t, path) // header epoch=0, one add_node op

	m := NewMemoryStore(WithJournalEpoch(1)) // snapshot generation 1
	ops, err := m.ReplayJournal(path)
	if err != nil {
		t.Fatalf("ReplayJournal: %v", err)
	}
	if ops != 0 {
		t.Fatalf("stale journal replayed %d ops", ops)
	}
	if _, _, _, skipped, lagged := m.ReplayAnomalies(); !skipped || lagged {
		t.Fatalf("anomalies: want skipped=true lagged=false, got skipped=%v lagged=%v", skipped, lagged)
	}
	if n := m.GetNode("repoA/a.go::f"); n != nil {
		t.Fatalf("skipped journal still applied its op: %+v", n)
	}
}

// TestReplayFlagsJournalNewerThanSnapshot covers the lagged-sidecar case:
// the journal's generation is ahead of the epoch file, so replay runs (the
// safe direction) and flags the discrepancy.
func TestReplayFlagsJournalNewerThanSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.jsonl")
	// A journal written by a generation-1 store (header epoch=1), replayed
	// against a store whose loaded snapshot generation is 0 — the shape a
	// crashed epoch-sidecar write produces.
	m1 := NewMemoryStore(WithJournalEpoch(1))
	if err := m1.OpenJournal(path); err != nil {
		t.Fatalf("open gen1 journal: %v", err)
	}
	m1.AddNode(&Node{ID: "repoA/z.go::z", Kind: KindFunction, Name: "z", FilePath: "z.go"})
	if err := m1.CloseJournal(); err != nil {
		t.Fatalf("close: %v", err)
	}

	m := NewMemoryStore() // epoch 0
	ops, err := m.ReplayJournal(path)
	if err != nil {
		t.Fatalf("ReplayJournal: %v", err)
	}
	if ops != 1 {
		t.Fatalf("lagged-sidecar journal must replay: applied %d ops", ops)
	}
	if _, _, _, skipped, lagged := m.ReplayAnomalies(); skipped || !lagged {
		t.Fatalf("anomalies: want skipped=false lagged=true, got skipped=%v lagged=%v", skipped, lagged)
	}
	if n := m.GetNode("repoA/z.go::z"); n == nil {
		t.Fatalf("lagged-sidecar journal op not applied")
	}
}

// TestReplayLegacyHeaderlessJournalUnchanged: journals written before the
// epoch marker have no header — they replay exactly as before.
func TestReplayLegacyHeaderlessJournalUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.jsonl")
	seedJournal(t, path)
	// Strip the header line, leaving a bare op.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	if len(lines) != 2 {
		t.Fatalf("unexpected journal shape: %q", data)
	}
	if err := os.WriteFile(path, []byte(lines[1]+"\n"), 0o644); err != nil {
		t.Fatalf("write legacy journal: %v", err)
	}

	m := NewMemoryStore(WithJournalEpoch(7))
	ops, err := m.ReplayJournal(path)
	if err != nil {
		t.Fatalf("ReplayJournal: %v", err)
	}
	if ops != 1 {
		t.Fatalf("legacy journal ops: want 1, got %d", ops)
	}
	if _, _, _, skipped, lagged := m.ReplayAnomalies(); skipped || lagged {
		t.Fatalf("legacy journal flagged: skipped=%v lagged=%v", skipped, lagged)
	}
	if n := m.GetNode("repoA/a.go::f"); n == nil {
		t.Fatalf("legacy journal op not applied")
	}
}

// TestReplayMidFileHeaderBenign: an append-open after a crash leaves a second
// header mid-file; replay must treat it as a known no-op.
func TestReplayMidFileHeaderBenign(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.jsonl")
	seedJournal(t, path)
	appendLines(t, path, `{"op":"journal_open","ts":1,"epoch":0}`)

	m := NewMemoryStore()
	ops, err := m.ReplayJournal(path)
	if err != nil {
		t.Fatalf("ReplayJournal: %v", err)
	}
	if ops != 2 {
		t.Fatalf("ops: want 2 (seed op + benign header), got %d", ops)
	}
	if unknown, _, _, _, _ := m.ReplayAnomalies(); unknown != 0 {
		t.Fatalf("mid-file header counted as unknown op: %d", unknown)
	}
}
