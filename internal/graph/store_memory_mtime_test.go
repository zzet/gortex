package graph

import (
	"reflect"
	"testing"
)

func TestMemoryStoreFileMtimesBulkSetLoad(t *testing.T) {
	m := NewMemoryStore()
	if got := m.LoadFileMtimes("repoA"); got != nil {
		t.Fatalf("unknown prefix: want nil, got %v", got)
	}
	if err := m.BulkSetFileMtimes("repoA", map[string]int64{"a.go": 100, "b/c.go": 200}); err != nil {
		t.Fatalf("BulkSetFileMtimes: %v", err)
	}
	if err := m.BulkSetFileMtimes("repoA", nil); err != nil {
		t.Fatalf("BulkSetFileMtimes(nil): %v", err)
	}
	want := map[string]int64{"a.go": 100, "b/c.go": 200}
	if got := m.LoadFileMtimes("repoA"); !reflect.DeepEqual(got, want) {
		t.Fatalf("after bulk set: want %v, got %v", want, got)
	}
	// LoadFileMtimes must return an owned copy: mutating it must not leak
	// into the store's sidecar.
	got := m.LoadFileMtimes("repoA")
	got["a.go"] = 999
	if m.LoadFileMtimes("repoA")["a.go"] != 100 {
		t.Fatalf("LoadFileMtimes leaked the store's map: %+v", m.LoadFileMtimes("repoA"))
	}
	if got := m.LoadFileMtimes("repoB"); got != nil {
		t.Fatalf("untouched prefix: want nil, got %v", got)
	}
}

func TestMemoryStoreFileMtimesReplacePrunes(t *testing.T) {
	m := NewMemoryStore()
	if err := m.BulkSetFileMtimes("repoA", map[string]int64{"a.go": 100, "gone.go": 150}); err != nil {
		t.Fatalf("BulkSetFileMtimes: %v", err)
	}
	// Empty input is a no-op: it must never wipe a repo's mtimes (the
	// empty-census contract shared with store_sqlite).
	if err := m.ReplaceFileMtimes("repoA", nil); err != nil {
		t.Fatalf("ReplaceFileMtimes(nil): %v", err)
	}
	if len(m.LoadFileMtimes("repoA")) != 2 {
		t.Fatalf("empty replace wiped rows: %+v", m.LoadFileMtimes("repoA"))
	}
	if err := m.ReplaceFileMtimes("repoA", map[string]int64{"a.go": 300}); err != nil {
		t.Fatalf("ReplaceFileMtimes: %v", err)
	}
	want := map[string]int64{"a.go": 300}
	if got := m.LoadFileMtimes("repoA"); !reflect.DeepEqual(got, want) {
		t.Fatalf("after replace: want %v (pruned), got %v", want, got)
	}
}

func TestMemoryStoreFileMtimesDelete(t *testing.T) {
	m := NewMemoryStore()
	if err := m.BulkSetFileMtimes("repoA", map[string]int64{"a.go": 100, "b.go": 200}); err != nil {
		t.Fatalf("BulkSetFileMtimes: %v", err)
	}
	if err := m.DeleteFileMtimes("repoA", nil); err != nil {
		t.Fatalf("DeleteFileMtimes(nil): %v", err)
	}
	if err := m.DeleteFileMtimes("repoA", []string{"a.go"}); err != nil {
		t.Fatalf("DeleteFileMtimes: %v", err)
	}
	want := map[string]int64{"b.go": 200}
	if got := m.LoadFileMtimes("repoA"); !reflect.DeepEqual(got, want) {
		t.Fatalf("after delete: want %v, got %v", want, got)
	}
	// Deleting the last row empties the prefix entirely.
	if err := m.DeleteFileMtimes("repoA", []string{"b.go"}); err != nil {
		t.Fatalf("DeleteFileMtimes: %v", err)
	}
	if got := m.LoadFileMtimes("repoA"); got != nil {
		t.Fatalf("fully deleted prefix: want nil, got %v", got)
	}
	if got := m.FileMtimePrefixes(); len(got) != 0 {
		t.Fatalf("FileMtimePrefixes after full delete: want empty, got %v", got)
	}
}

func TestMemoryStoreFileMtimesEvictRepoClearsSidecar(t *testing.T) {
	m := NewMemoryStore()
	n := &Node{ID: "repoA/a.go::f", Kind: KindFunction, Name: "f", FilePath: "a.go"}
	m.AddNode(n)
	if err := m.BulkSetFileMtimes("repoA", map[string]int64{"a.go": 100}); err != nil {
		t.Fatalf("BulkSetFileMtimes: %v", err)
	}
	m.EvictRepo("repoA")
	if got := m.LoadFileMtimes("repoA"); got != nil {
		t.Fatalf("evicted repo keeps mtime rows: %v", got)
	}
	if got := m.FileMtimePrefixes(); len(got) != 0 {
		t.Fatalf("FileMtimePrefixes after evict: want empty, got %v", got)
	}
}

func TestMemoryStoreFileMtimesNotJournaled(t *testing.T) {
	dir := t.TempDir()
	m := NewMemoryStore()
	if err := m.OpenJournal(dir + "/journal.jsonl"); err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	before := m.JournalBytes()
	if err := m.BulkSetFileMtimes("repoA", map[string]int64{"a.go": 100}); err != nil {
		t.Fatalf("BulkSetFileMtimes: %v", err)
	}
	if err := m.ReplaceFileMtimes("repoA", map[string]int64{"a.go": 200}); err != nil {
		t.Fatalf("ReplaceFileMtimes: %v", err)
	}
	if err := m.DeleteFileMtimes("repoA", []string{"a.go"}); err != nil {
		t.Fatalf("DeleteFileMtimes: %v", err)
	}
	if after := m.JournalBytes(); after != before {
		t.Fatalf("mtime sidecar ops journaled: %d -> %d bytes", before, after)
	}
}
