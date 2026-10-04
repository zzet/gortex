package store_sqlite

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Start an above-mark attempt in an idle gap, then hold the edit lane before
// its first actual VFS sync returns. This models the captured idle-started
// attempt without replacing native durability or increasing any writer cap.
func TestPressureAttemptStartedIdleCompletesWhenEditBegins(t *testing.T) {
	s, db := finalBackfillFixture(t)
	growWAL(t, s, 6)
	cfg := walReclaimConfig{thresholdBytes: 1 << 20, ceilingBytes: 16 << 20, readerWait: time.Second, truncateBudget: walReclaimTruncateBudget}
	path := s.dbPath + "-wal"
	seeded := walFileSize(path)
	require.GreaterOrEqual(t, seeded, walPressureMark(cfg))
	require.Less(t, seeded, walReclaimHardCapFactor*cfg.ceilingBytes)
	lane := &fakeBuildLane{}
	lane.install(s)
	require.False(t, s.buildLaneBusy())
	entered, release := stallReclaimCheckpointSync(t, db)
	var firstSyncKind atomic.Int32
	firstSyncKind.Store(-1)
	state := reclaimSyncStallState.Load()
	require.NotNil(t, state)
	// Publish callback before launching SQL; it is immutable thereafter.
	state.beforeSync = func(kind int) { firstSyncKind.CompareAndSwap(-1, int32(kind)) }
	var releaseOnce sync.Once
	unpark := func() { releaseOnce.Do(func() { close(release) }) }
	done := make(chan walReclaimResult, 1)
	joined := make(chan struct{})
	var owned *backgroundCheckpointAttempt
	t.Cleanup(func() {
		if owned != nil {
			owned.cancel(context.Canceled)
		}
		unpark()
		lane.held.Store(false)
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("owned pressure attempt failed to join")
		}
	})
	go func() {
		done <- s.reclaimWALAttempt(cfg, db, path)
		close(joined)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("attempt did not reach actual sync")
	}
	s.backgroundCheckpoint.mu.Lock()
	owned = s.backgroundCheckpoint.active
	s.backgroundCheckpoint.mu.Unlock()
	require.NotNil(t, owned)
	require.GreaterOrEqual(t, firstSyncKind.Load(), int32(0))
	// The first sync is inside the writer-free pass; no Go writer gate is held.
	writerCtx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	writeStart := time.Now()
	lockErr := s.writeMu.LockContext(writerCtx)
	var sqlErr error
	if lockErr == nil {
		_, sqlErr = s.writerDB.ExecContext(writerCtx, `UPDATE wal_churn SET payload='idle-pressure-foreground' WHERE id=1`)
		s.writeMu.Unlock()
	}
	writeElapsed := time.Since(writeStart)
	cancel()
	t.Logf("seeded=%d pressure_mark=%d sync_kind=%d foreground_gate_SQL=%s gate_error=%v SQL_error=%v", seeded, walPressureMark(cfg), firstSyncKind.Load(), writeElapsed, lockErr, sqlErr)
	require.NoError(t, lockErr)
	require.NoError(t, sqlErr)
	require.LessOrEqual(t, writeElapsed, 500*time.Millisecond)
	lane.held.Store(true)
	defer lane.held.Store(false)
	// Keep the edit active through completion. This park is an explicit VFS
	// boundary counterfactual, not a claim about actual Windows fsync time.
	time.Sleep(300 * time.Millisecond)
	unpark()
	var result walReclaimResult
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("above-mark attempt did not finish under the existing policy budget")
	}
	require.True(t, result.pressure, "idle start must not misclassify the above-mark attempt as ordinary")
	require.Equal(t, walReclaimReset, result.outcome, "%s", result.reason)
	require.True(t, walLogIsReset(s.dbPath, path), "SQLite must actually reset the WAL")
	require.Positive(t, s.WALCopyStats().PressureRuns)
	require.LessOrEqual(t, result.writerSpent, walReclaimMaxWriterHold+100*time.Millisecond)
	var payload string
	require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id=1`).Scan(&payload))
	require.Equal(t, "idle-pressure-foreground", payload)
	t.Logf("pressure=%v outcome=%v reason=%s actual_writer_hold=%s aggregate_writer_hold=%s", result.pressure, result.outcome, result.reason, result.writerHold, result.writerSpent)
}
