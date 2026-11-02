package store_sqlite

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This is a small real-WAL causal control, not a physical-write benchmark.
// Known standalone cold bulk cannot take reset authority without an override.
// It must not start PASSIVE copying merely to discover that same refusal later.
func TestStandaloneBulkReclaimDefersBeforePassive(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, db := finalBackfillFixture(t)
	growWAL(t, s, 2) // Seed before pinning the writer; growWAL uses writerDB directly.
	s.BeginBulkLoad()
	s.writeMu.Lock()
	standalone := s.bulkConn != nil && s.generationBulkLoad == 0 && !s.coordinatedBulkLoad
	s.writeMu.Unlock()
	require.True(t, standalone, "fixture must engage actual standalone cold bulk, not generation lease")
	defer func() { require.NoError(t, s.FlushBulk()) }()
	path := s.dbPath + "-wal"
	cfg := walReclaimConfig{thresholdBytes: 1, ceilingBytes: 2 << 30, truncateBudget: walReclaimTruncateBudget}
	require.Positive(t, walFileSize(path))
	require.Less(t, walFileSize(path), cfg.ceilingBytes)
	var passive atomic.Int64
	old := walCheckpointCallObserver
	t.Cleanup(func() { walCheckpointCallObserver = old })
	walCheckpointCallObserver = func(mode string, at time.Time, elapsed time.Duration) {
		if old != nil {
			old(mode, at, elapsed)
		}
		if mode == "PASSIVE" {
			passive.Add(1)
		}
	}
	before := s.WALCopyStats().WrittenBytes
	res := s.reclaimWALAttempt(cfg, db, path)
	calls, copied := passive.Load(), s.WALCopyStats().WrittenBytes-before
	walCheckpointCallObserver = old
	require.Equal(t, walReclaimSkipped, res.outcome)
	require.Equal(t, "bulk_writer", res.reason)
	require.False(t, res.leaseOverride)
	// Foreground transactions still use the real pinned connection after refusal.
	progressCtx, cancelProgress := context.WithTimeout(t.Context(), time.Second)
	defer cancelProgress()
	require.NoError(t, s.writeMu.LockContext(progressCtx), "foreground writer must regain actual admission")
	tx, err := s.beginWriteContext(progressCtx)
	if err == nil {
		_, err = tx.ExecContext(progressCtx, `UPDATE wal_churn SET payload='standalone-progress' WHERE id=1`)
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
	}
	s.writeMu.Unlock()
	require.NoError(t, err)
	var payload string
	require.NoError(t, s.db.QueryRowContext(progressCtx, `SELECT payload FROM wal_churn WHERE id=1`).Scan(&payload))
	require.Equal(t, "standalone-progress", payload)
	t.Logf("actual PASSIVE=%d VFS_written_bytes=%d outcome=%v reason=%s WAL=%d; no whole-window physical attribution", calls, copied, res.outcome, res.reason, walFileSize(path))
	require.Zero(t, calls, "known standalone refusal must precede actual PASSIVE copy")
	require.Zero(t, copied, "known standalone refusal must not pay paced copy work")
}
