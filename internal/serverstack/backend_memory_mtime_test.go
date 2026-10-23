package serverstack

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

// TestFailedDumpKeepsJournal pins the review-escalation fix (issue
// davalillo/gortex#25): the journal reset runs only after a successful
// snapshot dump. The journal is the only durable record of mutations the
// failed snapshot never captured — resetting it on a failed dump would
// silently drop them on the next boot.
func TestFailedDumpKeepsJournal(t *testing.T) {
	dir := t.TempDir()
	snapPath := filepath.Join(dir, "store.sqlite")

	// Session 1: clean baseline (snapshot published, journal reset).
	s1, close1, err := openMemoryBackend(snapPath, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s1.AddNode(&graph.Node{ID: "repoA/a.go::f", Kind: graph.KindFunction, Name: "f", FilePath: "a.go"})
	close1()

	// Session 2: journal a mutation the snapshot does not contain, then
	// force the dump to fail — the tmp target is a non-empty directory, so
	// neither its removal nor its open can succeed.
	s2, close2, err := openMemoryBackend(snapPath, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s2.AddNode(&graph.Node{ID: "repoB/b.go::g", Kind: graph.KindFunction, Name: "g", FilePath: "b.go"})
	tmpDir := snapPath + ".snapshot.tmp"
	if err := os.MkdirAll(filepath.Join(tmpDir, "blocker"), 0o755); err != nil {
		t.Fatalf("block tmp: %v", err)
	}
	close2() // dump fails; the journal must survive untouched

	// Session 3: the journaled mutation must still replay.
	s3, close3, err := openMemoryBackend(snapPath, nil)
	if err != nil {
		t.Fatalf("third open: %v", err)
	}
	defer close3()
	if got := s3.GetNode("repoB/b.go::g"); got == nil || got.Name != "g" {
		t.Fatalf("failed dump lost journaled mutations: %+v", got)
	}
}

// TestEpochSidecarRoundTrip pins the generation flow (issue
// davalillo/gortex#23): the sidecar starts absent (epoch 0), every successful
// dump+reset bumps it, and the reopened store pairs with it.
func TestEpochSidecarRoundTrip(t *testing.T) {
	snapPath := filepath.Join(t.TempDir(), "store.sqlite")
	epochPath := memoryEpochPath(snapPath)

	s1, close1, err := openMemoryBackend(snapPath, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mem1 := s1.(*graph.MemoryStore)
	if mem1.JournalEpoch() != 0 {
		t.Fatalf("initial epoch: want 0, got %d", mem1.JournalEpoch())
	}
	close1()
	if got := readJournalEpoch(epochPath); got != 1 {
		t.Fatalf("sidecar after first close: want 1, got %d", got)
	}

	s2, close2, err := openMemoryBackend(snapPath, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	mem2 := s2.(*graph.MemoryStore)
	if mem2.JournalEpoch() != 1 {
		t.Fatalf("reopened epoch: want 1, got %d", mem2.JournalEpoch())
	}
	// The journal was reset at close, so its header must pair with the
	// loaded snapshot generation — no stale-skip on reopen.
	if _, _, _, skipped, lagged := mem2.ReplayAnomalies(); skipped || lagged {
		t.Fatalf("unexpected replay anomaly: skipped=%v lagged=%v", skipped, lagged)
	}
	close2()
	if got := readJournalEpoch(epochPath); got != 2 {
		t.Fatalf("sidecar after second close: want 2, got %d", got)
	}
}

// TestPeriodicSnapshotJournalSizeTrigger: a journal crossing the byte
// threshold takes a snapshot off the hot path and rotates the journal.
func TestPeriodicSnapshotJournalSizeTrigger(t *testing.T) {
	t.Setenv("GORTEX_SNAPSHOT_JOURNAL_BYTES", "1")
	t.Setenv("GORTEX_SNAPSHOT_TICK", "20ms")
	snapPath := filepath.Join(t.TempDir(), "store.sqlite")
	epochPath := memoryEpochPath(snapPath)

	s, closeFn, err := openMemoryBackend(snapPath, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mem := s.(*graph.MemoryStore)
	mem.AddNode(&graph.Node{ID: "repoA/a.go::f", Kind: graph.KindFunction, Name: "f", FilePath: "a.go"})

	deadline := time.Now().Add(3 * time.Second)
	for {
		if got := readJournalEpoch(epochPath); got == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("periodic trigger never fired; journal bytes=%d", mem.JournalBytes())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if mem.JournalEpoch() != 1 {
		t.Fatalf("epoch after periodic dump: want 1, got %d", mem.JournalEpoch())
	}
	if mem.JournalBytes() == 0 {
		// The header line post-reset is fine; the point is the rotation.
		t.Log("journal fully rotated (header-only)")
	}
	// The snapshot published by the trigger must contain the mutation.
	if _, err := os.Stat(snapPath); err != nil {
		t.Fatalf("snapshot missing: %v", err)
	}
	closeFn()
}
