package serverstack

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestMemoryBackendMtimesSnapshotRoundTrip pins the warm-boot contract of
// issue davalillo/gortex#26: the memory backend's file-mtime sidecar must
// survive a snapshot dump + reopen, because priorMtimesFromStore seeds the
// warm-restart reconcile from it. Without this round trip every warm boot
// re-reconciles the whole corpus.
func TestMemoryBackendMtimesSnapshotRoundTrip(t *testing.T) {
	snapPath := filepath.Join(t.TempDir(), "store.sqlite")

	// Session 1: graph nodes + mtime rows across two repos, then close
	// (snapshot dump).
	s1 := graph.NewMemoryStore()
	s1.AddNode(&graph.Node{ID: "repoA/a.go::f", Kind: graph.KindFunction, Name: "f", FilePath: "a.go"})
	s1.AddNode(&graph.Node{ID: "repoB/b.go::g", Kind: graph.KindFunction, Name: "g", FilePath: "b.go"})
	if err := s1.BulkSetFileMtimes("repoA", map[string]int64{"a.go": 111, "sub/c.go": 222}); err != nil {
		t.Fatalf("BulkSetFileMtimes repoA: %v", err)
	}
	if err := s1.BulkSetFileMtimes("repoB", map[string]int64{"b.go": 333}); err != nil {
		t.Fatalf("BulkSetFileMtimes repoB: %v", err)
	}
	if err := snapshotMemoryStore(s1, snapPath); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// Session 2: reopen from the snapshot; the sidecar must come back.
	s2, close2, err := openMemoryBackend(snapPath, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer close2()
	mem, ok := s2.(*graph.MemoryStore)
	if !ok {
		t.Fatalf("backend is %T, want *graph.MemoryStore", s2)
	}
	if _, hasReader := s2.(graph.FileMtimeReader); !hasReader {
		t.Fatalf("reopened backend does not implement FileMtimeReader")
	}
	wantA := map[string]int64{"a.go": 111, "sub/c.go": 222}
	if got := mem.LoadFileMtimes("repoA"); !equalMtimes(got, wantA) {
		t.Fatalf("repoA mtimes after reopen: want %v, got %v", wantA, got)
	}
	wantB := map[string]int64{"b.go": 333}
	if got := mem.LoadFileMtimes("repoB"); !equalMtimes(got, wantB) {
		t.Fatalf("repoB mtimes after reopen: want %v, got %v", wantB, got)
	}

	// A session-2 sidecar update must survive the next dump/reopen cycle too.
	if err := mem.ReplaceFileMtimes("repoA", map[string]int64{"a.go": 444}); err != nil {
		t.Fatalf("ReplaceFileMtimes: %v", err)
	}
	if err := snapshotMemoryStore(mem, snapPath); err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	s3, close3, err := openMemoryBackend(snapPath, nil)
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	defer close3()
	mem3 := s3.(*graph.MemoryStore)
	wantA2 := map[string]int64{"a.go": 444}
	if got := mem3.LoadFileMtimes("repoA"); !equalMtimes(got, wantA2) {
		t.Fatalf("repoA mtimes after second reopen: want %v (pruned), got %v", wantA2, got)
	}
	if got := mem3.LoadFileMtimes("repoB"); !equalMtimes(got, wantB) {
		t.Fatalf("repoB mtimes after second reopen: want %v, got %v", wantB, got)
	}
}

func equalMtimes(got, want map[string]int64) bool {
	if got == nil && len(want) == 0 {
		return true
	}
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}
