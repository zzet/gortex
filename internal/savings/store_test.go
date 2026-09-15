package savings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/persistence"
)

// testLedgerPath returns a fresh sidecar DB path. Each test gets its own
// file so the process-shared sidecar handle cache can't leak state
// between tests.
func testLedgerPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "sidecar.sqlite")
}

// mustSnapshot unwraps Snapshot for tests that expect a healthy ledger.
func mustSnapshot(t *testing.T, s *Store) File {
	t.Helper()
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return snap
}

// closeOnCleanup releases the sidecar handle when the test ends, so the
// process-wide handle cache doesn't accumulate open DBs (and TempDir
// cleanup works on platforms that refuse to delete open files).
func closeOnCleanup(t *testing.T, s *Store) *Store {
	t.Helper()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAddObservation_PerLanguageBucket(t *testing.T) {
	path := testLedgerPath(t)

	s, err := Open(path)
	if err == nil {
		closeOnCleanup(t, s)
	}
	if err != nil {
		t.Fatal(err)
	}

	s.AddObservation(Observation{Repo: "/repo-a", Language: "go", Tool: "get_symbol_source", Returned: 100, Saved: 200})
	s.AddObservation(Observation{Repo: "/repo-a", Language: "go", Tool: "get_symbol_source", Returned: 50, Saved: 80})
	s.AddObservation(Observation{Repo: "/repo-b", Language: "typescript", Tool: "batch_symbols", Returned: 30, Saved: 70})
	// Empty language is allowed (e.g. record() called with a nil node);
	// it should accumulate in the totals but not in any per-language bucket.
	s.AddObservation(Observation{Repo: "/repo-c", Tool: "smart_context", Returned: 10, Saved: 20})
	// Observations are buffered per Store, so committing the window is what
	// makes them visible to a second handle on the same file — exactly what
	// another process reading the ledger sees.
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	reopened, err := Open(path)
	if err == nil {
		closeOnCleanup(t, reopened)
	}
	if err != nil {
		t.Fatal(err)
	}
	snap := mustSnapshot(t, reopened)

	if got, want := snap.Totals.CallsCounted, int64(4); got != want {
		t.Errorf("CallsCounted = %d, want %d", got, want)
	}
	if len(snap.PerLanguage) != 2 {
		t.Errorf("PerLanguage size = %d, want 2 (empty-language observation must not create a bucket)", len(snap.PerLanguage))
	}
	if g := snap.PerLanguage["go"]; g == nil || g.CallsCounted != 2 || g.TokensSaved != 280 {
		t.Errorf("go bucket = %+v, want calls=2 saved=280 (200+80)", g)
	}
	if ts := snap.PerLanguage["typescript"]; ts == nil || ts.CallsCounted != 1 || ts.TokensSaved != 70 {
		t.Errorf("typescript bucket = %+v, want calls=1 saved=70", ts)
	}
	if len(snap.PerRepo) != 3 {
		t.Errorf("PerRepo size = %d, want 3", len(snap.PerRepo))
	}
}

// The headline property of the sidecar-backed ledger as its readers see it:
// reading through the store is exact with no flush step in the caller. The
// ledger file exists from the first observation, and the totals and events a
// reader gets include everything recorded — buffered or already committed —
// because every read path flushes first. (Durability against a SIGKILL is
// bounded by the flush window instead; see the package doc.)
func TestAddObservation_DurableImmediately(t *testing.T) {
	path := testLedgerPath(t)

	s, err := Open(path)
	if err == nil {
		closeOnCleanup(t, s)
	}
	if err != nil {
		t.Fatal(err)
	}
	s.AddObservation(Observation{Repo: "/r", Language: "go", Tool: "read_file", Returned: 10, Saved: 90})

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("ledger DB must exist immediately after the first observation: %v", err)
	}
	snap := mustSnapshot(t, s)
	if snap.Totals.CallsCounted != 1 || snap.Totals.TokensSaved != 90 {
		t.Errorf("snapshot = %+v, want calls=1 saved=90 with no flush", snap.Totals)
	}
	evs, err := s.EventsSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Tool != "read_file" {
		t.Errorf("events = %+v, want one read_file event", evs)
	}
}

func TestConcurrentWriters_SameLedger(t *testing.T) {
	path := testLedgerPath(t)

	const perStore = 200
	stores := make([]*Store, 4)
	for i := range stores {
		s, err := Open(path)
		if err == nil {
			closeOnCleanup(t, s)
		}
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = s
	}

	var wg sync.WaitGroup
	for i, s := range stores {
		wg.Add(1)
		go func(s *Store, repo string) {
			defer wg.Done()
			for j := 0; j < perStore; j++ {
				s.AddObservation(Observation{Repo: repo, Tool: "test", Returned: 1, Saved: 10})
			}
		}(s, "/repo-"+string(rune('a'+i)))
	}
	wg.Wait()
	// Each Store buffers its own window, so a cross-store read is exact only
	// after every writer has flushed — the same boundary a second process
	// reading the ledger sees. Flushing here is the assertion that no
	// observation is lost across writers, not a workaround: without it the
	// test would be measuring one store's buffer, not the shared ledger.
	for _, s := range stores {
		if err := s.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
	}

	snap := mustSnapshot(t, stores[0])
	wantCalls := int64(len(stores) * perStore)
	if got := snap.Totals.CallsCounted; got != wantCalls {
		t.Errorf("CallsCounted = %d, want %d (observation lost across writers)", got, wantCalls)
	}
	if got, want := snap.Totals.TokensSaved, wantCalls*10; got != want {
		t.Errorf("TokensSaved = %d, want %d", got, want)
	}
	evs, err := stores[0].EventsSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got := int64(len(evs)); got != wantCalls {
		t.Errorf("events = %d, want %d", got, wantCalls)
	}
}

// A fresh ledger reports nothing — including a zero FirstSeen. The
// flat-file store seeded FirstSeen=now at Open, which made the dashboard
// print "tracking since <the moment you ran the CLI>" on a machine that
// had never recorded anything.
func TestOpen_FreshLedger_EmptySnapshot(t *testing.T) {
	s, err := Open(testLedgerPath(t))
	if err == nil {
		closeOnCleanup(t, s)
	}
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	snap := mustSnapshot(t, s)
	if snap.Totals.CallsCounted != 0 {
		t.Errorf("new ledger has CallsCounted=%d, want 0", snap.Totals.CallsCounted)
	}
	if !snap.FirstSeen.IsZero() {
		t.Errorf("new ledger FirstSeen = %v, want zero time (nothing recorded yet)", snap.FirstSeen)
	}
	if snap.Version != schemaVersion {
		t.Errorf("new ledger version=%d, want %d", snap.Version, schemaVersion)
	}
}

func TestObservation_StampsFirstAndLastSeen(t *testing.T) {
	s, err := Open(testLedgerPath(t))
	if err == nil {
		closeOnCleanup(t, s)
	}
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC().Add(-time.Second)
	s.AddObservation(Observation{Tool: "test", Returned: 1, Saved: 1})
	after := time.Now().UTC().Add(time.Second)

	snap := mustSnapshot(t, s)
	if snap.FirstSeen.Before(before) || snap.FirstSeen.After(after) {
		t.Errorf("FirstSeen = %v, want within [%v, %v]", snap.FirstSeen, before, after)
	}
	if snap.LastUpdated.Before(before) || snap.LastUpdated.After(after) {
		t.Errorf("LastUpdated = %v, want within [%v, %v]", snap.LastUpdated, before, after)
	}
}

func TestAddObservation_ConcurrentSafe(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	const per = 250
	var wg sync.WaitGroup
	var expectedSaved atomic.Int64
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range per {
				s.AddObservation(Observation{Tool: "test", Returned: 10, Saved: 100})
				expectedSaved.Add(100)
			}
		}()
	}
	wg.Wait()

	snap := mustSnapshot(t, s)
	if got, want := snap.Totals.CallsCounted, int64(workers*per); got != want {
		t.Errorf("CallsCounted = %d, want %d", got, want)
	}
	if got, want := snap.Totals.TokensSaved, expectedSaved.Load(); got != want {
		t.Errorf("TokensSaved = %d, want %d", got, want)
	}
}

func TestOpen_EmptyPath_InMemoryOnly(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	s.AddObservation(Observation{Repo: "r", Tool: "test", Returned: 10, Saved: 100})
	if err := s.Flush(); err != nil {
		t.Errorf("Flush on in-memory store should no-op, got: %v", err)
	}
	snap := mustSnapshot(t, s)
	if snap.Totals.CallsCounted != 1 {
		t.Errorf("in-memory store should track, got CallsCounted=%d", snap.Totals.CallsCounted)
	}
	evs, err := s.EventsSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Errorf("in-memory store should keep events, got %d", len(evs))
	}
}

func TestReset_ClearsLedger(t *testing.T) {
	path := testLedgerPath(t)

	s, _ := Open(path)
	closeOnCleanup(t, s)
	s.AddObservation(Observation{Repo: "/r", Tool: "test", Returned: 50, Saved: 500})

	if err := s.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	snap := mustSnapshot(t, s)
	if snap.Totals.CallsCounted != 0 {
		t.Errorf("totals should be cleared after reset, got CallsCounted=%d", snap.Totals.CallsCounted)
	}
	if !snap.FirstSeen.IsZero() {
		t.Errorf("FirstSeen should be cleared after reset, got %v", snap.FirstSeen)
	}
	evs, err := s.EventsSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Errorf("events should be cleared after reset, got %d", len(evs))
	}
}

// Snapshot keeps the JSON shape graph_stats and `gortex savings --json`
// expose — the surface contract of cumulative_savings.
func TestSnapshot_JSONShape(t *testing.T) {
	s, _ := Open(testLedgerPath(t))
	closeOnCleanup(t, s)
	s.AddObservation(Observation{Repo: "/repo-a", Language: "go", Tool: "test", Returned: 10, Saved: 100})

	data, err := json.Marshal(mustSnapshot(t, s))
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "first_seen", "last_updated", "totals", "per_repo", "per_language"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("missing key %q in snapshot JSON", key)
		}
	}
}

func TestEventsSince_Filters(t *testing.T) {
	s, _ := Open(testLedgerPath(t))
	closeOnCleanup(t, s)
	s.AddObservation(Observation{Tool: "a", Returned: 1, Saved: 1})
	time.Sleep(5 * time.Millisecond)
	cutoff := time.Now().UTC()
	time.Sleep(5 * time.Millisecond)
	s.AddObservation(Observation{Tool: "b", Returned: 1, Saved: 1})

	evs, err := s.EventsSince(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Tool != "b" {
		t.Errorf("EventsSince(cutoff) = %+v, want only [b]", evs)
	}
	all, err := s.EventsSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("EventsSince(zero) = %d, want 2", len(all))
	}
}

func TestImportLegacy_FullFlatFiles(t *testing.T) {
	legacyDir := t.TempDir()
	jsonPath := filepath.Join(legacyDir, "savings.json")
	jsonlPath := filepath.Join(legacyDir, "savings.jsonl")

	firstSeen := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	lastUpdated := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	legacy := File{
		Version:     schemaVersion,
		FirstSeen:   firstSeen,
		LastUpdated: lastUpdated,
		Totals:      Totals{TokensSaved: 1000, TokensReturned: 100, CallsCounted: 10},
		PerRepo:     map[string]*Totals{"repo-a": {TokensSaved: 1000, TokensReturned: 100, CallsCounted: 10}},
		PerLanguage: map[string]*Totals{"go": {TokensSaved: 1000, TokensReturned: 100, CallsCounted: 10}},
	}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(jsonPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(Event{TS: lastUpdated, Repo: "repo-a", Language: "go", Tool: "get_symbol_source", Returned: 23, Saved: 77})
	if err := os.WriteFile(jsonlPath, append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(testLedgerPath(t))
	if err == nil {
		closeOnCleanup(t, s)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacy(jsonPath); err != nil {
		t.Fatalf("ImportLegacy: %v", err)
	}

	snap := mustSnapshot(t, s)
	if snap.Totals.CallsCounted != 10 || snap.Totals.TokensSaved != 1000 {
		t.Errorf("imported totals = %+v, want calls=10 saved=1000", snap.Totals)
	}
	if r := snap.PerRepo["repo-a"]; r == nil || r.CallsCounted != 10 {
		t.Errorf("imported repo bucket = %+v", r)
	}
	if !snap.FirstSeen.Equal(firstSeen) {
		t.Errorf("FirstSeen = %v, want %v (carried from legacy file)", snap.FirstSeen, firstSeen)
	}
	evs, _ := s.EventsSince(time.Time{})
	if len(evs) != 1 || evs[0].Tool != "get_symbol_source" || evs[0].Saved != 77 {
		t.Errorf("imported events = %+v", evs)
	}

	// Legacy files renamed aside, originals gone.
	if _, err := os.Stat(jsonPath); !os.IsNotExist(err) {
		t.Errorf("legacy savings.json should be renamed after import, stat err=%v", err)
	}
	if _, err := os.Stat(jsonPath + ".bak"); err != nil {
		t.Errorf("expected savings.json.bak, stat err=%v", err)
	}
	if _, err := os.Stat(jsonlPath + ".bak"); err != nil {
		t.Errorf("expected savings.jsonl.bak, stat err=%v", err)
	}

	// Idempotent: a second import (e.g. another entry point racing the
	// first) must not double-count.
	if err := s.ImportLegacy(jsonPath); err != nil {
		t.Fatalf("second ImportLegacy: %v", err)
	}
	if got := mustSnapshot(t, s).Totals.CallsCounted; got != 10 {
		t.Errorf("totals after second import = %d, want 10 (no double count)", got)
	}
}

// A jsonl without its cumulative file (the flat-file flush never ran
// before the process died — the common SIGKILL case) still imports:
// totals are rebuilt from the events.
func TestImportLegacy_EventsOnlyRebuildsTotals(t *testing.T) {
	legacyDir := t.TempDir()
	jsonPath := filepath.Join(legacyDir, "savings.json")
	jsonlPath := filepath.Join(legacyDir, "savings.jsonl")

	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	var lines []byte
	for i := 0; i < 3; i++ {
		line, _ := json.Marshal(Event{TS: ts.Add(time.Duration(i) * time.Minute), Repo: "r", Language: "go", Tool: "smart_context", Returned: 10, Saved: 30})
		lines = append(lines, line...)
		lines = append(lines, '\n')
	}
	if err := os.WriteFile(jsonlPath, lines, 0o644); err != nil {
		t.Fatal(err)
	}

	s, _ := Open(testLedgerPath(t))
	closeOnCleanup(t, s)
	if err := s.ImportLegacy(jsonPath); err != nil {
		t.Fatalf("ImportLegacy: %v", err)
	}
	snap := mustSnapshot(t, s)
	if snap.Totals.CallsCounted != 3 || snap.Totals.TokensSaved != 90 {
		t.Errorf("rebuilt totals = %+v, want calls=3 saved=90", snap.Totals)
	}
	if r := snap.PerRepo["r"]; r == nil || r.CallsCounted != 3 {
		t.Errorf("rebuilt repo bucket = %+v", r)
	}
	if !snap.FirstSeen.Equal(ts) {
		t.Errorf("FirstSeen = %v, want first event ts %v", snap.FirstSeen, ts)
	}
}

// With nothing to import the mark is still set, so legacy files that
// appear later (e.g. restored from a backup) are not silently merged
// into a ledger that has moved on.
func TestImportLegacy_NothingToImportMarksDone(t *testing.T) {
	legacyDir := t.TempDir()
	jsonPath := filepath.Join(legacyDir, "savings.json")

	s, _ := Open(testLedgerPath(t))
	closeOnCleanup(t, s)
	if err := s.ImportLegacy(jsonPath); err != nil {
		t.Fatalf("ImportLegacy on missing files: %v", err)
	}

	// A legacy file materializing afterwards is ignored.
	legacy := File{Version: schemaVersion, Totals: Totals{TokensSaved: 5000, CallsCounted: 50}}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(jsonPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacy(jsonPath); err != nil {
		t.Fatal(err)
	}
	if got := mustSnapshot(t, s).Totals.CallsCounted; got != 0 {
		t.Errorf("late-appearing legacy file must not import, got calls=%d", got)
	}
}

// TestDefaultPath_HonorsXDGCacheHome verifies the legacy flat-file path is
// routed through the XDG resolver: an absolute $XDG_CACHE_HOME relocates
// it to <XDG_CACHE_HOME>/gortex/savings.json, so the legacy import looks
// where the flat-file era actually wrote.
func TestDefaultPath_HonorsXDGCacheHome(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdg)

	want := filepath.Join(xdg, "gortex", "savings.json")
	if got := DefaultPath(); got != want {
		t.Fatalf("DefaultPath() with XDG_CACHE_HOME = %s, want %s", got, want)
	}

	// The sibling event-log path follows the same root.
	wantEvents := filepath.Join(xdg, "gortex", "savings.jsonl")
	if got := DefaultEventsPath(); got != wantEvents {
		t.Fatalf("DefaultEventsPath() with XDG_CACHE_HOME = %s, want %s", got, wantEvents)
	}
}

// TestDefaultDBPath_HonorsXDGDataHome verifies the ledger DB follows the
// data-dir resolver — the same sidecar.sqlite the notes/memories
// managers share.
func TestDefaultDBPath_HonorsXDGDataHome(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)

	want := filepath.Join(xdg, "gortex", "sidecar.sqlite")
	if got := DefaultDBPath(); got != want {
		t.Fatalf("DefaultDBPath() with XDG_DATA_HOME = %s, want %s", got, want)
	}
}

// A legacy file with JSON null bucket values must import cleanly — a
// nil *Totals dereference here would crash-loop every server start.
func TestImportLegacy_NullBucketValues(t *testing.T) {
	legacyDir := t.TempDir()
	jsonPath := filepath.Join(legacyDir, "savings.json")
	body := `{"version":1,"totals":{"tokens_saved":10,"tokens_returned":1,"calls_counted":1},"per_repo":{"x":null},"per_language":{"y":null}}`
	if err := os.WriteFile(jsonPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	s, _ := Open(testLedgerPath(t))
	closeOnCleanup(t, s)
	if err := s.ImportLegacy(jsonPath); err != nil {
		t.Fatalf("ImportLegacy with null buckets: %v", err)
	}
	snap := mustSnapshot(t, s)
	if snap.Totals.CallsCounted != 1 {
		t.Errorf("totals = %+v, want calls=1", snap.Totals)
	}
	if len(snap.PerRepo) != 0 || len(snap.PerLanguage) != 0 {
		t.Errorf("null buckets must be dropped, got repo=%v lang=%v", snap.PerRepo, snap.PerLanguage)
	}
	if _, err := os.Stat(jsonPath + ".bak"); err != nil {
		t.Errorf("legacy file should be renamed after import: %v", err)
	}
}

// The flat-file cumulative was flush-batched while the event log
// appended eagerly; the import floors totals at what the events
// reconstruct so "Last 7 days" can never exceed "All time".
func TestImportLegacy_FlushLaggedTotalsFlooredByEvents(t *testing.T) {
	legacyDir := t.TempDir()
	jsonPath := filepath.Join(legacyDir, "savings.json")
	jsonlPath := filepath.Join(legacyDir, "savings.jsonl")

	lagged := File{
		Version: schemaVersion,
		Totals:  Totals{TokensSaved: 10, TokensReturned: 1, CallsCounted: 1},
	}
	data, _ := json.Marshal(lagged)
	if err := os.WriteFile(jsonPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	var lines []byte
	for i := 0; i < 3; i++ {
		line, _ := json.Marshal(Event{TS: ts.Add(time.Duration(i) * time.Minute), Tool: "t", Returned: 1, Saved: 10})
		lines = append(append(lines, line...), '\n')
	}
	if err := os.WriteFile(jsonlPath, lines, 0o644); err != nil {
		t.Fatal(err)
	}

	s, _ := Open(testLedgerPath(t))
	closeOnCleanup(t, s)
	if err := s.ImportLegacy(jsonPath); err != nil {
		t.Fatal(err)
	}
	snap := mustSnapshot(t, s)
	if snap.Totals.CallsCounted != 3 || snap.Totals.TokensSaved != 30 {
		t.Errorf("totals = %+v, want floored at the events' calls=3 saved=30", snap.Totals)
	}
}

// A hard event-log read error aborts the import without marking or
// renaming, so the next open retries instead of permanently losing the
// unread tail.
func TestImportLegacy_UnreadableEventsAborts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows has no POSIX mode bits: a 0000 file is still readable, so the read cannot be made to fail")
	}
	if os.Getuid() == 0 {
		t.Skip("permission bits don't bind as root")
	}
	legacyDir := t.TempDir()
	jsonPath := filepath.Join(legacyDir, "savings.json")
	jsonlPath := filepath.Join(legacyDir, "savings.jsonl")
	if err := os.WriteFile(jsonlPath, []byte("{}\n"), 0o000); err != nil {
		t.Fatal(err)
	}

	s, _ := Open(testLedgerPath(t))
	closeOnCleanup(t, s)
	if err := s.ImportLegacy(jsonPath); err == nil {
		t.Fatal("unreadable event log must abort the import")
	}
	if _, err := os.Stat(jsonlPath); err != nil {
		t.Errorf("aborted import must not rename the event log: %v", err)
	}
	// A later open (permissions fixed) imports successfully.
	if err := os.Chmod(jsonlPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacy(jsonPath); err != nil {
		t.Fatalf("retry after fixing permissions: %v", err)
	}
}

// waitFor polls until cond holds or the deadline passes. Used for the flush
// timer, the one part of the ledger that is not caller-driven.
func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// The defect this file's coalescing exists to fix: a read-only tool call used
// to open, write and commit a durable sidecar transaction (~37 KB of WAL) to
// book its own accounting. N observations inside one flush window must cost
// ZERO transactions until the window closes, and then exactly one.
func TestAddObservation_CoalescesIntoOneTransaction(t *testing.T) {
	s, err := Open(testLedgerPath(t))
	if err != nil {
		t.Fatal(err)
	}
	closeOnCleanup(t, s)

	const n = 12
	before := s.sc.SavingsCommitCount()
	for range n {
		s.AddObservation(Observation{Repo: "/r", Language: "go", Tool: "search_symbols", Returned: 10, Saved: 100})
	}
	if got := s.sc.SavingsCommitCount() - before; got != 0 {
		t.Errorf("sidecar transactions for %d read-only observations = %d, want 0 before the flush window closes", n, got)
	}
	if got := s.Pending(); got != n {
		t.Errorf("Pending() = %d, want %d buffered", got, n)
	}

	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := s.sc.SavingsCommitCount() - before; got != 1 {
		t.Errorf("sidecar transactions after the flush = %d, want exactly 1", got)
	}
	if got := s.Pending(); got != 0 {
		t.Errorf("Pending() after flush = %d, want 0", got)
	}

	snap := mustSnapshot(t, s)
	if snap.Totals.CallsCounted != n || snap.Totals.TokensSaved != n*100 || snap.Totals.TokensReturned != n*10 {
		t.Errorf("totals after flush = %+v, want calls=%d saved=%d returned=%d",
			snap.Totals, n, n*100, n*10)
	}
	if got := snap.PerRepo["/r"]; got == nil || got.CallsCounted != n {
		t.Errorf("per-repo totals = %+v, want calls=%d", got, n)
	}
	if got := snap.PerLanguage["go"]; got == nil || got.CallsCounted != n {
		t.Errorf("per-language totals = %+v, want calls=%d", got, n)
	}
}

// Coalescing must not make a reader see stale numbers: every read path on the
// store flushes first, so `gortex savings`, graph_stats and the savings tools
// reading through this store stay exact however long the window is.
func TestReadsFlushBufferedObservations(t *testing.T) {
	for _, tc := range []struct {
		name string
		read func(t *testing.T, s *Store) int64
	}{
		{"Snapshot", func(t *testing.T, s *Store) int64 { return mustSnapshot(t, s).Totals.CallsCounted }},
		{"EventsSince", func(t *testing.T, s *Store) int64 {
			evs, err := s.EventsSince(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			return int64(len(evs))
		}},
		{"ToolTotals", func(t *testing.T, s *Store) int64 {
			rows, err := s.ToolTotals(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			var calls int64
			for _, r := range rows {
				calls += r.CallsCounted
			}
			return calls
		}},
		{"ModelTotals", func(t *testing.T, s *Store) int64 {
			rows, err := s.ModelTotals(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			var calls int64
			for _, r := range rows {
				calls += r.CallsCounted
			}
			return calls
		}},
		{"ClientTotals", func(t *testing.T, s *Store) int64 {
			rows, err := s.ClientTotals(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			var calls int64
			for _, r := range rows {
				calls += r.CallsCounted
			}
			return calls
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(testLedgerPath(t))
			if err != nil {
				t.Fatal(err)
			}
			closeOnCleanup(t, s)

			const n = 5
			for range n {
				s.AddObservation(Observation{
					Repo: "/r", Language: "go", Tool: "read_file",
					Model: "claude", Client: "claude-code", Returned: 10, Saved: 100,
				})
			}
			if s.Pending() != n {
				t.Fatalf("precondition: Pending() = %d, want %d buffered", s.Pending(), n)
			}
			if got := tc.read(t, s); got != n {
				t.Errorf("%s saw %d calls, want %d — a read must flush the buffer first", tc.name, got, n)
			}
			if got := s.Pending(); got != 0 {
				t.Errorf("Pending() after a read = %d, want 0", got)
			}
		})
	}
}

// The shutdown contract: the daemon's teardown chain calls Flush (through
// Server.FlushSavings) and then Close. Both must persist the window, or the
// last minute of accounting dies with the process.
func TestClose_PersistsBufferedObservations(t *testing.T) {
	path := testLedgerPath(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	const n = 7
	for range n {
		s.AddObservation(Observation{Repo: "/r", Tool: "read_file", Returned: 1, Saved: 10})
	}
	if s.Pending() != n {
		t.Fatalf("precondition: Pending() = %d, want %d", s.Pending(), n)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	closeOnCleanup(t, reopened)
	snap := mustSnapshot(t, reopened)
	if snap.Totals.CallsCounted != n || snap.Totals.TokensSaved != n*10 {
		t.Errorf("totals after close+reopen = %+v, want calls=%d saved=%d", snap.Totals, n, n*10)
	}
}

// The count bound. A busy daemon must not accumulate an unbounded buffer
// waiting for a timer: the buffer flushes itself once it is full.
func TestFlushMax_BoundsTheBuffer(t *testing.T) {
	s, err := Open(testLedgerPath(t))
	if err != nil {
		t.Fatal(err)
	}
	closeOnCleanup(t, s)
	s.mu.Lock()
	s.flushMax = 4
	s.mu.Unlock()

	before := s.sc.SavingsCommitCount()
	for range 10 {
		s.AddObservation(Observation{Tool: "search_symbols", Returned: 1, Saved: 10})
	}
	if got := s.sc.SavingsCommitCount() - before; got != 2 {
		t.Errorf("transactions for 10 observations at flushMax=4 = %d, want 2", got)
	}
	if got := s.Pending(); got != 2 {
		t.Errorf("Pending() = %d, want the 2 that did not fill a batch", got)
	}
	if got := mustSnapshot(t, s).Totals.CallsCounted; got != 10 {
		t.Errorf("CallsCounted = %d, want 10 (no observation lost across the max-size flushes)", got)
	}
}

// The time bound. Nothing may sit buffered indefinitely just because the
// caller went quiet: an idle daemon still commits its window.
func TestFlushTimer_CommitsWithoutAReader(t *testing.T) {
	s, err := Open(testLedgerPath(t))
	if err != nil {
		t.Fatal(err)
	}
	closeOnCleanup(t, s)
	s.mu.Lock()
	s.flushEvery = 20 * time.Millisecond
	s.mu.Unlock()

	before := s.sc.SavingsCommitCount()
	s.AddObservation(Observation{Tool: "search_symbols", Returned: 1, Saved: 10})
	if !waitFor(t, 5*time.Second, func() bool { return s.sc.SavingsCommitCount() > before }) {
		t.Fatal("the flush timer never committed the buffered observation")
	}
	if got := s.Pending(); got != 0 {
		t.Errorf("Pending() after the timer fired = %d, want 0", got)
	}
	if got := s.sc.SavingsCommitCount() - before; got != 1 {
		t.Errorf("transactions = %d, want exactly 1 (the timer must not re-arm on an empty buffer)", got)
	}
}

// The escape hatch: an operator who wants the old per-call durability can
// have it, and a typo in the variable must not silently reinstate it either
// way — an unparseable value keeps the default.
func TestFlushIntervalEnv(t *testing.T) {
	t.Run("zero restores a transaction per observation", func(t *testing.T) {
		t.Setenv(flushIntervalEnv, "0")
		s, err := Open(testLedgerPath(t))
		if err != nil {
			t.Fatal(err)
		}
		closeOnCleanup(t, s)
		before := s.sc.SavingsCommitCount()
		for range 3 {
			s.AddObservation(Observation{Tool: "read_file", Returned: 1, Saved: 10})
		}
		if got := s.sc.SavingsCommitCount() - before; got != 3 {
			t.Errorf("transactions with the buffer disabled = %d, want 3", got)
		}
		if got := s.Pending(); got != 0 {
			t.Errorf("Pending() = %d, want 0 with the buffer disabled", got)
		}
	})
	t.Run("garbage falls back to the default", func(t *testing.T) {
		t.Setenv(flushIntervalEnv, "not-a-duration")
		s, err := Open(testLedgerPath(t))
		if err != nil {
			t.Fatal(err)
		}
		closeOnCleanup(t, s)
		s.AddObservation(Observation{Tool: "read_file", Returned: 1, Saved: 10})
		if got := s.Pending(); got != 1 {
			t.Errorf("Pending() = %d, want 1 — an unparseable interval must keep buffering", got)
		}
	})
}

// A reset wipes the ledger; a batch buffered before it must not land after
// the DELETE and resurrect part of what the user asked to clear.
func TestReset_DiscardsBufferedObservations(t *testing.T) {
	s, err := Open(testLedgerPath(t))
	if err != nil {
		t.Fatal(err)
	}
	closeOnCleanup(t, s)

	s.AddObservation(Observation{Repo: "/r", Tool: "test", Returned: 50, Saved: 500})
	if s.Pending() != 1 {
		t.Fatalf("precondition: Pending() = %d, want 1", s.Pending())
	}
	if err := s.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got := s.Pending(); got != 0 {
		t.Errorf("Pending() after reset = %d, want 0", got)
	}
	snap := mustSnapshot(t, s)
	if snap.Totals.CallsCounted != 0 {
		t.Errorf("CallsCounted after reset = %d, want 0 (a buffered observation must not survive)", snap.Totals.CallsCounted)
	}
}

// --- exactness across handles on one ledger --------------------------------
//
// persistence.OpenSidecar caches one connection per absolute path, so two
// savings.Store values opened on the same ledger share a handle while owning
// separate buffers. A read through either handle must therefore drain BOTH,
// or the reader reports a ledger whose live window is sitting in the other
// store's memory — the `gortex gain` failure this item repairs.

// sharedLedgerCommits reports how many sidecar transactions the ledger at
// path has taken, through the same cached handle the stores use.
func sharedLedgerCommits(t *testing.T, path string) int64 {
	t.Helper()
	sc, err := persistence.OpenSidecar(path)
	if err != nil {
		t.Fatalf("open sidecar: %v", err)
	}
	return sc.SavingsCommitCount()
}

func TestReadsFlushEveryHandleOnTheSameLedger(t *testing.T) {
	const observations = 12
	path := testLedgerPath(t)
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	reader, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	for range observations {
		writer.AddObservation(Observation{Repo: "/r", Language: "go", Tool: "search_symbols", Model: "m", Client: "c", Saved: 10, Returned: 1})
	}
	if got := writer.Pending(); got != observations {
		t.Fatalf("precondition: want %d buffered on the writer, got %d", observations, got)
	}
	if got := reader.Pending(); got != 0 {
		t.Fatalf("precondition: the reader handle buffers nothing of its own, got %d", got)
	}

	cases := []struct {
		name string
		read func() int64
	}{
		{"Snapshot", func() int64 {
			return mustSnapshot(t, reader).Totals.CallsCounted
		}},
		{"EventsSince", func() int64 {
			evs, err := reader.EventsSince(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			return int64(len(evs))
		}},
		{"ToolTotals", func() int64 {
			rows, err := reader.ToolTotals(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			var n int64
			for _, r := range rows {
				n += r.CallsCounted
			}
			return n
		}},
		{"ModelTotals", func() int64 {
			rows, err := reader.ModelTotals(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			var n int64
			for _, r := range rows {
				n += r.CallsCounted
			}
			return n
		}},
		{"ClientTotals", func() int64 {
			rows, err := reader.ClientTotals(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			var n int64
			for _, r := range rows {
				n += r.CallsCounted
			}
			return n
		}},
	}
	// The FIRST read must drain the writer's buffer; the rest confirm each
	// read path is wired the same way (they re-read an already-drained
	// ledger, which is exactly what a second reader sees).
	for i, tc := range cases {
		if i == 0 {
			if got := reader.Pending(); got != 0 {
				t.Fatalf("precondition: reader buffer must be empty before %s, got %d", tc.name, got)
			}
		}
		if got := tc.read(); got != observations {
			t.Errorf("%s through a second handle must see all %d buffered observations, got %d",
				tc.name, observations, got)
		}
		if got := writer.Pending(); got != 0 {
			t.Errorf("%s must have drained the writer's buffer, %d still pending", tc.name, got)
		}
	}
	if got := mustSnapshot(t, writer).DroppedObservations; got != 0 {
		t.Errorf("nothing may be dropped, got %d", got)
	}
}

// Draining a peer must not cost a transaction per observation: the whole
// window is still one commit.
func TestCrossHandleFlushStaysOneTransaction(t *testing.T) {
	const observations = 12
	path := testLedgerPath(t)
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	reader, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	before := sharedLedgerCommits(t, path)
	for range observations {
		writer.AddObservation(Observation{Repo: "/r", Language: "go", Tool: "read_file", Saved: 5})
	}
	if got := sharedLedgerCommits(t, path) - before; got != 0 {
		t.Fatalf("buffered observations must open no transaction, got %d", got)
	}
	if got := mustSnapshot(t, reader).Totals.CallsCounted; got != observations {
		t.Fatalf("reader must see %d, got %d", observations, got)
	}
	if got := sharedLedgerCommits(t, path) - before; got != 1 {
		t.Errorf("the cross-handle flush must commit the window as ONE transaction, got %d", got)
	}
}

// Closing one handle takes the shared connection away from every other
// store on it, so the close must commit their windows first — otherwise the
// writer's next flush fails with "database is closed" and the window is
// dropped, not merely delayed.
func TestClose_FlushesEveryHandleBeforeReleasingTheLedger(t *testing.T) {
	path := testLedgerPath(t)
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	writer.AddObservation(Observation{Repo: "/r", Language: "go", Tool: "get_symbol_source", Saved: 33, Returned: 3})
	if writer.Pending() != 1 {
		t.Fatalf("precondition: the observation must still be buffered")
	}

	if err := reader.Close(); err != nil { // the one-shot CLI reader's defer
		t.Fatalf("close: %v", err)
	}
	if got := writer.Pending(); got != 0 {
		t.Errorf("closing a peer handle must drain this store's buffer, %d still pending", got)
	}
	if got := writer.dropped.Load(); got != 0 {
		t.Errorf("no observation may be dropped by a peer's close, got %d", got)
	}

	fresh, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fresh.Close() }()
	snap := mustSnapshot(t, fresh)
	if snap.Totals.CallsCounted != 1 || snap.Totals.TokensSaved != 33 {
		t.Errorf("the window must be on disk after the peer close, got %+v", snap.Totals)
	}
}

// A reset must not leave a peer's buffer to flush itself back over the
// wipe — the same reasoning the single-handle path already carried.
func TestReset_DiscardsPeerHandleBuffers(t *testing.T) {
	path := testLedgerPath(t)
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	resetter, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	writer.AddObservation(Observation{Repo: "/r", Language: "go", Tool: "read_file", Saved: 90})
	if writer.Pending() != 1 {
		t.Fatalf("precondition: buffered")
	}
	if err := resetter.Reset(); err != nil {
		t.Fatal(err)
	}
	if got := writer.Pending(); got != 0 {
		t.Errorf("a reset must discard peer buffers, %d still pending", got)
	}
	if got := mustSnapshot(t, resetter).Totals.CallsCounted; got != 0 {
		t.Errorf("nothing may survive the reset, got %d calls", got)
	}
}

// --- the one-shot flush bound ----------------------------------------------

func TestSetFlushBounds_TightensTheWindow(t *testing.T) {
	path := testLedgerPath(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if every, max := s.FlushBounds(); every != DefaultFlushInterval || max != DefaultFlushMax {
		t.Fatalf("precondition: want the daemon default, got %v/%d", every, max)
	}

	s.SetFlushBounds(OneshotFlushInterval, OneshotFlushMax)
	every, max := s.FlushBounds()
	if every != OneshotFlushInterval || max != OneshotFlushMax {
		t.Errorf("SetFlushBounds must apply both bounds, got %v/%d", every, max)
	}
	if OneshotFlushInterval >= DefaultFlushInterval || OneshotFlushMax >= DefaultFlushMax {
		t.Errorf("the one-shot bounds must be tighter than the daemon's: %v/%d vs %v/%d",
			OneshotFlushInterval, OneshotFlushMax, DefaultFlushInterval, DefaultFlushMax)
	}

	// A non-positive argument leaves that bound alone.
	s.SetFlushBounds(0, 0)
	if every, max = s.FlushBounds(); every != OneshotFlushInterval || max != OneshotFlushMax {
		t.Errorf("a zero argument must not clobber a bound, got %v/%d", every, max)
	}
}

// Tightening must take effect for observations that are ALREADY buffered,
// or a burst booked before the entry point narrows the window keeps the old
// exposure.
func TestSetFlushBounds_AppliesToAnAlreadyArmedBuffer(t *testing.T) {
	path := testLedgerPath(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for range 3 {
		s.AddObservation(Observation{Repo: "/r", Language: "go", Tool: "read_file", Saved: 1})
	}
	if s.Pending() != 3 {
		t.Fatalf("precondition: 3 buffered, got %d", s.Pending())
	}
	// A count bound the buffer already meets commits immediately.
	s.SetFlushBounds(0, 2)
	if got := s.Pending(); got != 0 {
		t.Errorf("a count bound already met must commit the buffer, %d still pending", got)
	}
	if got := mustSnapshot(t, s).Totals.CallsCounted; got != 3 {
		t.Errorf("want 3 committed, got %d", got)
	}

	// A shorter interval re-arms rather than waiting out the old one.
	s.AddObservation(Observation{Repo: "/r", Language: "go", Tool: "read_file", Saved: 1})
	s.SetFlushBounds(20*time.Millisecond, 0)
	deadline := time.Now().Add(2 * time.Second)
	for s.Pending() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := s.Pending(); got != 0 {
		t.Errorf("the re-armed timer must commit inside the new interval, %d still pending", got)
	}
}

// The constraint: an operator knob is honoured, never silently raised or
// dropped. An explicit GORTEX_SAVINGS_FLUSH_INTERVAL outranks the entry
// point's bound in both directions.
func TestSetFlushBounds_NeverOverridesTheOperatorKnob(t *testing.T) {
	t.Setenv(flushIntervalEnv, "250ms")
	s, err := Open(testLedgerPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	s.SetFlushBounds(OneshotFlushInterval, OneshotFlushMax)
	if every, _ := s.FlushBounds(); every != 250*time.Millisecond {
		t.Errorf("an operator-set interval must survive SetFlushBounds, got %v", every)
	}

	// A typo is NOT an operator setting: it falls back to the default and
	// stays overridable, or a typo would pin the window.
	t.Setenv(flushIntervalEnv, "not-a-duration")
	s2, err := Open(testLedgerPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	s2.SetFlushBounds(OneshotFlushInterval, OneshotFlushMax)
	if every, _ := s2.FlushBounds(); every != OneshotFlushInterval {
		t.Errorf("an unparseable value must not pin the window, got %v", every)
	}
}

// The cross-handle flush reaches into other stores' buffers, so it has to be
// safe while those stores are being written, read, opened and closed. Race
// detector fodder: concurrent writers on one handle, concurrent readers on a
// second, and handles opening and closing underneath both.
func TestCrossHandleFlushIsRaceFree(t *testing.T) {
	const writers, perWriter = 6, 40
	path := testLedgerPath(t)
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	reader, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWriter {
				writer.AddObservation(Observation{Repo: "/r", Language: "go", Tool: "read_file", Saved: 1})
			}
		}()
	}
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if _, err := reader.Snapshot(); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	// A third handle opening and closing under the other two is the
	// registry's own churn: gortex gain against a live process.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 5 {
			transient, err := Open(path)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := transient.EventsSince(time.Time{}); err != nil {
				t.Error(err)
				return
			}
			// NOTE: not Close() — closing drops the SHARED handle from the
			// persistence cache and would pull it out from under the live
			// writers. That hazard is the caller's, and is documented on
			// Store.Close; this goroutine exercises the registry, not it.
		}
	}()
	wg.Wait()

	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	snap := mustSnapshot(t, reader)
	if want := int64(writers * perWriter); snap.Totals.CallsCounted != want {
		t.Errorf("every observation must be accounted for: want %d, got %d", want, snap.Totals.CallsCounted)
	}
	if snap.DroppedObservations != 0 {
		t.Errorf("nothing may be dropped, got %d", snap.DroppedObservations)
	}
}
