package telemetry

import (
	"testing"
	"time"
)

func fixedNow(day string) func() time.Time {
	t, _ := time.Parse("2006-01-02", day)
	return func() time.Time { return t }
}

func TestRecorderRecordsWhenEnabled(t *testing.T) {
	store := NewStore(t.TempDir())
	r := NewRecorder(Consent{Enabled: true}, store)
	r.now = fixedNow("2026-06-18")

	r.Record("mcp_tool_call", "search_symbols")
	r.Record("mcp_tool_call", "search_symbols")
	r.Record("cli_command", "review")
	r.Flush()

	got, err := store.Load("2026-06-18")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Counts["mcp_tool_call:search_symbols"] != 2 {
		t.Errorf("tool counter = %d, want 2", got.Counts["mcp_tool_call:search_symbols"])
	}
	if got.Counts["cli_command:review"] != 1 {
		t.Errorf("cli counter = %d, want 1", got.Counts["cli_command:review"])
	}
}

func TestRecorderDisabledRecordsNothing(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	r := NewRecorder(Consent{Enabled: false}, store)
	r.now = fixedNow("2026-06-18")
	if r.Enabled() {
		t.Fatal("disabled recorder reports Enabled()")
	}

	r.Record("mcp_tool_call", "search_symbols")
	r.Flush()

	// Nothing recorded → no day files written at all (no telemetry dir churn).
	days, err := store.Days()
	if err != nil {
		t.Fatalf("Days: %v", err)
	}
	if len(days) != 0 {
		t.Errorf("disabled recorder wrote days %v", days)
	}
}

func TestRecorderNilSafe(t *testing.T) {
	var r *Recorder
	// None of these may panic.
	r.Record("mcp_tool_call", "x")
	r.Flush()
	if r.Enabled() {
		t.Error("nil recorder reports Enabled()")
	}
}

func TestRecorderDropsDisallowedKey(t *testing.T) {
	store := NewStore(t.TempDir())
	r := NewRecorder(Consent{Enabled: true}, store)
	r.now = fixedNow("2026-06-18")

	r.Record("file_path", "/Users/x/secret.go") // not allow-listed
	r.Flush()

	if days, _ := store.Days(); len(days) != 0 {
		t.Errorf("disallowed key produced a rollup file: %v", days)
	}
}

func TestRecorderThresholdAutoFlush(t *testing.T) {
	store := NewStore(t.TempDir())
	r := NewRecorder(Consent{Enabled: true}, store)
	r.now = fixedNow("2026-06-18")

	for range recorderFlushThreshold {
		r.Record("index", "1k-10k")
	}
	// The threshold was reached, so a flush already happened without an
	// explicit Flush() call.
	got, _ := store.Load("2026-06-18")
	if got.Counts["index:1k-10k"] != recorderFlushThreshold {
		t.Errorf("auto-flush counter = %d, want %d", got.Counts["index:1k-10k"], recorderFlushThreshold)
	}
}

func TestRecorderDayRollover(t *testing.T) {
	store := NewStore(t.TempDir())
	r := NewRecorder(Consent{Enabled: true}, store)

	day := "2026-06-18"
	r.now = func() time.Time { tm, _ := time.Parse("2006-01-02", day); return tm }
	r.Record("cli_command", "a")

	day = "2026-06-19" // clock advances past midnight
	r.Record("cli_command", "b")
	r.Flush()

	d18, _ := store.Load("2026-06-18")
	d19, _ := store.Load("2026-06-19")
	if d18.Counts["cli_command:a"] != 1 {
		t.Errorf("day-18 counter = %d, want 1 (prior day flushed on rollover)", d18.Counts["cli_command:a"])
	}
	if d19.Counts["cli_command:b"] != 1 {
		t.Errorf("day-19 counter = %d, want 1", d19.Counts["cli_command:b"])
	}
}

func TestRecorderLiveConsentStopsRecording(t *testing.T) {
	store := NewStore(t.TempDir())
	enabled := true
	r := NewRecorderFunc(func() bool { return enabled }, store)
	r.now = fixedNow("2026-06-18")

	r.Record("cli_command", "a")
	r.Flush()
	if got, _ := store.Load("2026-06-18"); got.Counts["cli_command:a"] != 1 {
		t.Fatalf("enabled recorder did not record: %v", got.Counts)
	}

	// Flip consent off live: further records drop and a flush is a no-op, so a
	// running daemon stops recording the moment the user disables telemetry.
	enabled = false
	if r.Enabled() {
		t.Error("Enabled() should follow live consent (off)")
	}
	r.Record("cli_command", "b")
	r.Flush()
	if got, _ := store.Load("2026-06-18"); got.Counts["cli_command:b"] != 0 {
		t.Errorf("recorder kept recording after consent flipped off: %v", got.Counts)
	}
}

func TestCachedConsentResolver(t *testing.T) {
	t.Setenv("GORTEX_TELEMETRY", "")
	t.Setenv("DO_NOT_TRACK", "")
	dir := t.TempDir()
	if err := SaveConsent(dir, true, "test", nil); err != nil {
		t.Fatal(err)
	}
	// A stepped clock, not wall time: the TTL boundary is the thing under test,
	// so the test must not lose a race between SaveConsent's disk write and a
	// real deadline on a loaded machine.
	const ttl = 20 * time.Millisecond
	base, _ := time.Parse("2006-01-02", "2026-06-18")
	clock := base
	resolve := cachedConsentResolver(dir, ttl, func() time.Time { return clock })
	if !resolve() {
		t.Fatal("resolver should reflect persisted enabled=true")
	}
	// Flip persisted to off; within the TTL the cached (true) value persists.
	if err := SaveConsent(dir, false, "test", nil); err != nil {
		t.Fatal(err)
	}
	clock = base.Add(ttl - 1)
	if !resolve() {
		t.Error("within TTL the cached value should persist")
	}
	// Once the TTL has elapsed it re-reads → false.
	clock = base.Add(ttl)
	if resolve() {
		t.Error("after TTL the resolver should re-read and return false")
	}
}

func TestCachedConsentResolverUsesRealClock(t *testing.T) {
	t.Setenv("GORTEX_TELEMETRY", "")
	t.Setenv("DO_NOT_TRACK", "")
	dir := t.TempDir()
	if err := SaveConsent(dir, true, "test", nil); err != nil {
		t.Fatal(err)
	}
	// The exported constructor is the one production wires up; a long TTL keeps
	// this to the wiring (dir + real clock) with no timing assumption.
	if !CachedConsentResolver(dir, time.Hour)() {
		t.Error("exported resolver should reflect persisted enabled=true")
	}
}
