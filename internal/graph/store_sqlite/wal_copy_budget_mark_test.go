package store_sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The same callback used by real paced page writes starts below the mark with
// an exhausted editing budget. Checked SQL crosses it while that callback waits;
// observe budget-wait exit independently of subsequent page IO or SQLite sync.
func TestCopyBudgetStopsWhenCurrentWALCrossesPressureMark(t *testing.T) {
	s, db := finalBackfillFixture(t)
	lane := &fakeBuildLane{}
	lane.install(s)
	setCopyBudget(t, 60<<20, 256<<10, 0)
	s.walCopy.sawBusy(time.Now())
	_, err := checkpointWALOnceOn(t.Context(), db, "TRUNCATE")
	require.NoError(t, err)
	require.NoError(t, churnWriteOnce(s, 2))
	mark := s.WALWriteMark()
	require.True(t, mark.Valid)
	markBytes := int64(mark.MxFrame) * (int64(mark.PageSize) + walFrameHeaderBytes)
	pressureMark := markBytes + (512 << 10)
	require.Less(t, walFileSize(s.dbPath+"-wal"), pressureMark)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pacer := &walCopyPacer{store: s, ctx: ctx, stop: s.stopCheckpoint, rate: 1 << 20, hardCapBytes: pressureMark, walPath: s.dbPath + "-wal"}
	reserved := s.walCopy.take(time.Now(), 3<<20, 1<<20)
	require.Greater(t, reserved, 2*time.Second)
	s.walCopy.mu.Lock()
	initialRefill := s.walCopy.refilled
	s.walCopy.mu.Unlock()
	done := make(chan error, 1)
	var joined bool
	defer func() {
		cancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("owned budget callback did not join")
			}
		}
	}()
	go func() { pacer.beforeWrite(4096); done <- nil }()
	until := time.Now().Add(time.Second)
	entered := false
	for !entered && time.Now().Before(until) {
		s.walCopy.mu.Lock()
		entered = s.walCopy.refilled.After(initialRefill)
		s.walCopy.mu.Unlock()
		if !entered {
			time.Sleep(time.Millisecond)
		}
	}
	require.True(t, entered, "the page callback must first enter its below-mark budget wait")
	require.NoError(t, churnWriteOnce(s, 0))
	crossed := s.WALWriteMark()
	require.True(t, crossed.Valid)
	require.Equal(t, mark.Salt, crossed.Salt)
	require.Greater(t, int64(crossed.MxFrame)*(int64(crossed.PageSize)+walFrameHeaderBytes), pressureMark)
	crossingAt := time.Now()
	select {
	case err = <-done:
		joined = true
	case <-time.After(time.Second):
		t.Fatal("the page callback retained its old budget after the current WAL crossed the mark")
	}
	t.Logf("entry_frames=%d crossed_frames=%d pressure_mark=%d reserved_old_budget=%s completion_after_crossing=%s actual_copy_budget_wait=%s actual_written=%d", mark.MxFrame, crossed.MxFrame, pressureMark, reserved, time.Since(crossingAt), pacer.budgetNs, pacer.written)
	require.NoError(t, err)
	require.True(t, pacer.overPacingMark, "the callback must exit because it observed pressure, not because the old wait elapsed")
	require.Positive(t, pacer.written)
	require.Less(t, pacer.budgetNs, reserved)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&count))
	require.Equal(t, 4000, count)
	require.False(t, s.writeMu.held())
	require.False(t, s.readGate.closed.Load())
}
