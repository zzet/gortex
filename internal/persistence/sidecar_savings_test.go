package persistence

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestSidecar(t *testing.T) (*SidecarStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sidecar.sqlite")
	sc, err := OpenSidecar(path)
	if err != nil {
		t.Fatal(err)
	}
	return sc, path
}

// The durability contract behind the savings ledger: an observation
// survives a full close + reopen of the database — no flush step exists
// to forget.
func TestSavings_DurableAcrossReopen(t *testing.T) {
	sc, path := openTestSidecar(t)

	ev := SavingsEvent{
		TS:        time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		SessionID: "sess-1",
		Tool:      "get_symbol_source",
		Repo:      "repo-a",
		Language:  "go",
		Returned:  23,
		Saved:     77,
	}
	if err := sc.AddSavingsObservation(ev); err != nil {
		t.Fatal(err)
	}
	if err := sc.Close(); err != nil {
		t.Fatal(err)
	}

	sc2, err := OpenSidecar(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sc2.Close()

	buckets, firstSeen, lastUpdated, err := sc2.SavingsTotals()
	if err != nil {
		t.Fatal(err)
	}
	top := buckets[""]
	if top.Calls != 1 || top.Saved != 77 || top.Returned != 23 {
		t.Errorf("top-line bucket = %+v, want calls=1 saved=77 returned=23", top)
	}
	if r := buckets["repo:repo-a"]; r.Calls != 1 {
		t.Errorf("repo bucket = %+v, want calls=1", r)
	}
	if l := buckets["lang:go"]; l.Calls != 1 {
		t.Errorf("lang bucket = %+v, want calls=1", l)
	}
	if !firstSeen.Equal(ev.TS) || !lastUpdated.Equal(ev.TS) {
		t.Errorf("meta stamps = (%v, %v), want both %v", firstSeen, lastUpdated, ev.TS)
	}

	evs, err := sc2.SavingsEventsSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].SessionID != "sess-1" || !evs[0].TS.Equal(ev.TS) {
		t.Errorf("reloaded events = %+v", evs)
	}
}

func TestSavings_MetaStampsMinMax(t *testing.T) {
	sc, _ := openTestSidecar(t)
	defer sc.Close()

	t1 := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	if err := sc.AddSavingsObservation(SavingsEvent{TS: t1, Tool: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := sc.AddSavingsObservation(SavingsEvent{TS: t2, Tool: "b"}); err != nil {
		t.Fatal(err)
	}

	_, firstSeen, lastUpdated, err := sc.SavingsTotals()
	if err != nil {
		t.Fatal(err)
	}
	if !firstSeen.Equal(t1) {
		t.Errorf("first_seen = %v, want %v (first observation wins)", firstSeen, t1)
	}
	if !lastUpdated.Equal(t2) {
		t.Errorf("last_updated = %v, want %v (latest observation wins)", lastUpdated, t2)
	}
}

func TestSavings_ResetClearsButKeepsImportMark(t *testing.T) {
	sc, _ := openTestSidecar(t)
	defer sc.Close()

	if err := sc.ImportLegacySavings(
		map[string]SavingsTotalsRow{"": {Saved: 100, Returned: 10, Calls: 1}},
		time.Now().UTC(), time.Now().UTC(), nil,
	); err != nil {
		t.Fatal(err)
	}
	if !sc.SavingsLegacyImportDone() {
		t.Fatal("import mark must be set after ImportLegacySavings")
	}
	if err := sc.ResetSavings(); err != nil {
		t.Fatal(err)
	}
	buckets, firstSeen, _, err := sc.SavingsTotals()
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 0 || !firstSeen.IsZero() {
		t.Errorf("reset must clear totals + meta, got buckets=%v firstSeen=%v", buckets, firstSeen)
	}
	if !sc.SavingsLegacyImportDone() {
		t.Error("reset must NOT clear the legacy-import mark (renamed files would re-import)")
	}
}

func TestSavings_ImportIsIdempotent(t *testing.T) {
	sc, _ := openTestSidecar(t)
	defer sc.Close()

	rows := map[string]SavingsTotalsRow{"": {Saved: 100, Returned: 10, Calls: 1}}
	if err := sc.ImportLegacySavings(rows, time.Time{}, time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := sc.ImportLegacySavings(rows, time.Time{}, time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	buckets, _, _, err := sc.SavingsTotals()
	if err != nil {
		t.Fatal(err)
	}
	if got := buckets[""].Calls; got != 1 {
		t.Errorf("calls after double import = %d, want 1", got)
	}
}

// The cost that made accounting the dominant writer on an idle machine: one
// durable transaction per recorded tool call. A batch is ONE transaction
// however many observations it carries, and it must fold repeated buckets
// into a single upsert rather than one per event.
func TestAddSavingsObservations_OneTransactionPerBatch(t *testing.T) {
	sc, _ := openTestSidecar(t)
	defer sc.Close()

	base := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	const n = 12
	batch := make([]SavingsEvent, 0, n)
	for i := range n {
		batch = append(batch, SavingsEvent{
			TS:        base.Add(time.Duration(i) * time.Second),
			SessionID: "sess",
			Tool:      "search_symbols",
			Repo:      "repo-a",
			Language:  "go",
			Returned:  10,
			Saved:     100,
		})
	}
	before := sc.SavingsCommitCount()
	if err := sc.AddSavingsObservations(batch); err != nil {
		t.Fatal(err)
	}
	if got := sc.SavingsCommitCount() - before; got != 1 {
		t.Errorf("commits for a %d-observation batch = %d, want 1", n, got)
	}

	buckets, firstSeen, lastUpdated, err := sc.SavingsTotals()
	if err != nil {
		t.Fatal(err)
	}
	if top := buckets[""]; top.Calls != n || top.Saved != n*100 || top.Returned != n*10 {
		t.Errorf("top-line bucket = %+v, want calls=%d saved=%d returned=%d", top, n, n*100, n*10)
	}
	if r := buckets["repo:repo-a"]; r.Calls != n || r.Saved != n*100 {
		t.Errorf("repo bucket = %+v, want calls=%d saved=%d", r, n, n*100)
	}
	if l := buckets["lang:go"]; l.Calls != n || l.Saved != n*100 {
		t.Errorf("lang bucket = %+v, want calls=%d saved=%d", l, n, n*100)
	}
	if !firstSeen.Equal(base) {
		t.Errorf("first_seen = %v, want the batch's earliest ts %v", firstSeen, base)
	}
	if want := base.Add((n - 1) * time.Second); !lastUpdated.Equal(want) {
		t.Errorf("last_updated = %v, want the batch's latest ts %v", lastUpdated, want)
	}

	evs, err := sc.SavingsEventsSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != n {
		t.Errorf("event rows = %d, want %d (batching must not collapse the event log)", len(evs), n)
	}
}

// Physical evidence for the same claim, in the unit the diagnosis measured:
// sidecar WAL bytes. Twelve one-observation transactions cost ~12x what one
// twelve-observation transaction costs; the assertion is deliberately loose
// (half, not a twelfth) so page-layout differences between platforms cannot
// make it flake while a reverted batch still fails it.
func TestAddSavingsObservations_WALCostFarBelowPerCall(t *testing.T) {
	const n = 12
	base := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	event := func(i int) SavingsEvent {
		return SavingsEvent{
			TS: base.Add(time.Duration(i) * time.Second), SessionID: "sess",
			Tool: "search_symbols", Repo: "repo-a", Language: "go", Returned: 10, Saved: 100,
		}
	}
	walSize := func(t *testing.T, path string) int64 {
		t.Helper()
		fi, err := os.Stat(path + "-wal")
		if err != nil {
			return 0
		}
		return fi.Size()
	}

	scPer, perPath := openTestSidecar(t)
	defer scPer.Close()
	startPer := walSize(t, perPath)
	for i := range n {
		if err := scPer.AddSavingsObservation(event(i)); err != nil {
			t.Fatal(err)
		}
	}
	perCall := walSize(t, perPath) - startPer

	scBatch, batchPath := openTestSidecar(t)
	defer scBatch.Close()
	startBatch := walSize(t, batchPath)
	batch := make([]SavingsEvent, 0, n)
	for i := range n {
		batch = append(batch, event(i))
	}
	if err := scBatch.AddSavingsObservations(batch); err != nil {
		t.Fatal(err)
	}
	batched := walSize(t, batchPath) - startBatch

	t.Logf("sidecar WAL growth: %d observations one-by-one = %d B, as one batch = %d B", n, perCall, batched)
	if perCall <= 0 {
		t.Skip("no measurable WAL growth on this filesystem; the transaction-count assertion carries the contract")
	}
	if batched >= perCall/2 {
		t.Errorf("batched WAL growth = %d B, want well under half of the per-call %d B", batched, perCall)
	}
}

// A buffered batch can reach the database after another process has already
// recorded a NEWER observation. Overwriting the stamps would walk
// last_updated backwards (and a late first batch would overwrite an earlier
// first_seen), so both stamps combine with what is stored.
func TestAddSavingsObservations_StampsNeverMoveBackwards(t *testing.T) {
	sc, _ := openTestSidecar(t)
	defer sc.Close()

	newer := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	older := newer.Add(-2 * time.Hour)
	if err := sc.AddSavingsObservations([]SavingsEvent{{TS: newer, Tool: "b"}}); err != nil {
		t.Fatal(err)
	}
	if err := sc.AddSavingsObservations([]SavingsEvent{{TS: older, Tool: "a"}}); err != nil {
		t.Fatal(err)
	}

	_, firstSeen, lastUpdated, err := sc.SavingsTotals()
	if err != nil {
		t.Fatal(err)
	}
	if !firstSeen.Equal(older) {
		t.Errorf("first_seen = %v, want the earliest observed %v", firstSeen, older)
	}
	if !lastUpdated.Equal(newer) {
		t.Errorf("last_updated = %v, want the latest observed %v (a late batch must not rewind it)", lastUpdated, newer)
	}
}

// An empty flush must not open a transaction: the flush timer fires on a
// store whose buffer a reader already drained, and that must cost nothing.
func TestAddSavingsObservations_EmptyBatchCommitsNothing(t *testing.T) {
	sc, _ := openTestSidecar(t)
	defer sc.Close()

	before := sc.SavingsCommitCount()
	if err := sc.AddSavingsObservations(nil); err != nil {
		t.Fatal(err)
	}
	if err := sc.AddSavingsObservations([]SavingsEvent{}); err != nil {
		t.Fatal(err)
	}
	if got := sc.SavingsCommitCount() - before; got != 0 {
		t.Errorf("commits for empty batches = %d, want 0", got)
	}
}
