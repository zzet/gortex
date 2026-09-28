package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/viewmetrics"
)

const (
	foldMarksEditingMark = 256 << 20
	foldMarksFill        = 320 << 20
)

// fillWAL appends about n bytes of log through a second connection, in one
// transaction: real frames the store's WAL write mark counts, as a burst of
// large edits or a retirement slice leaves them.
// A read transaction opened before the fill stays open until the test ends,
// as a long read does in the daemon: it keeps any checkpoint from resetting
// the log, which stays over the editing mark while the fold runs.
func fillWAL(t *testing.T, f *coordinatorFixture, n int) {
	t.Helper()
	db := parityOpenRaw(t, f.store)
	db.SetMaxOpenConns(2)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS zz_wal_fill (b BLOB)`); err != nil {
		t.Fatal(err)
	}
	// The reader's snapshot predates the fill: no checkpoint can back-fill
	// or reset past it while it is open.
	reader, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := reader.QueryRow(`SELECT count(*) FROM zz_wal_fill`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Rollback() })
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 1<<20)
	for i := 0; i < n>>20; i++ {
		blob[0] = byte(i)
		if _, err := tx.Exec(`INSERT INTO zz_wal_fill (b) VALUES (?)`, blob); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// installDaemonBuildLaneBusy gives the store the daemon's build-lane predicate
// (cmd/gortex installBuildLaneBusy): the lane counts as busy while an edit's
// mutation cycle holds it. The store's editing marks key on it; a fixture
// without it never counts as editing.
func installDaemonBuildLaneBusy(f *coordinatorFixture, c *CheckoutCoordinator) {
	f.store.SetBuildLaneBusy(func() bool {
		st := c.gate.Stats()
		if !st.Active || st.Holder == nil {
			return false
		}
		switch st.Holder.Kind {
		case "checkout_mutation":
			return true
		case "checkout_cycle":
			return st.Holder.Priority == viewmetrics.BuildPriorityInteractive
		}
		return false
	})
	t := f.t
	if tt, ok := t.(interface{ Cleanup(func()) }); ok {
		tt.Cleanup(func() { f.store.SetBuildLaneBusy(nil) })
	}
}

// With the daemon's defaults, a stepped fold steps and lands while the log is
// between the editing mark (256 MiB) and the fold's own mark (1 GiB) and edits
// arrive: the fold is work the edits depend on, and only the retirement sweep
// is held at the editing mark. Override, named: the fold backend is a probe
// that records the log's size at each committed step (the real store's fold
// underneath, its marks unchanged); a reader holds the filled log from being
// reset (fillWAL).
func TestSteppedFoldStepsBetweenTheEditingAndTheFoldMark(t *testing.T) {
	f, c, l := mcpChainFixtureDefaults(t, builderTreeA())
	installDaemonBuildLaneBusy(f, c)
	probe := &walMarkProbe{inner: storeChainFoldBackend{store: f.store}, store: f.store, mark: foldMarksEditingMark}
	c.compaction.mu.Lock()
	c.compaction.backend = probe
	c.compaction.mu.Unlock()
	fillWAL(t, f, foldMarksFill)
	if mark := f.store.WALWriteMark(); !mark.Valid || int64(mark.MxFrame)*(int64(mark.PageSize)+24) < foldMarksEditingMark {
		t.Fatalf("the fill left the log under the editing mark: %+v", mark)
	}
	// Edits keep arriving while the fold runs: its steps meet the log over
	// the editing mark with the lane busy.
	for i := 0; i < 12; i++ {
		mcpEdit(t, l, f, func() { chainBurstEdit(t, f, i) })
		time.Sleep(100 * time.Millisecond)
	}
	if err := c.waitDirtyChainCompactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := c.DirtyChainCompactionStats()
	t.Logf("folds %+v; steps %d, refused over a mark %d, committed over the editing mark %d, worst log at a committed step %d MiB",
		stats, probe.steps, probe.refused, probe.overCommitted, probe.worstCommitted>>20)
	if stats.Flipped == 0 {
		t.Fatalf("no fold landed: %+v", stats)
	}
	if probe.refused > 0 {
		t.Errorf("%d fold steps refused over a mark with the log at most %d MiB, under the fold's mark", probe.refused, foldMarksFill>>20+64)
	}
	if per := stats.Duration / time.Duration(max(stats.AttemptsStarted, 1)); per > 10*time.Second {
		t.Errorf("a fold took %v on average, over 10 s", per)
	}
	if probe.overCommitted == 0 {
		t.Fatalf("no fold step committed with the log over the editing mark: the fold is held at 256 MiB")
	}
	if probe.worstCommitted >= 1<<30 {
		t.Fatalf("a fold step committed with %d MiB of log, over the fold's mark", probe.worstCommitted>>20)
	}
}
