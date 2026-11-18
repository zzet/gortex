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
	// Use the actual pinned writer: writerDB cannot admit another connection
	// while standalone bulk owns it. A byte-changing commit creates backlog,
	// unlike a retained WAL allocation or a no-op UPDATE.
	seedCtx, cancelSeed := context.WithTimeout(t.Context(), time.Second)
	defer cancelSeed()
	require.NoError(t, s.writeMu.LockContext(seedCtx))
	txSeed, seedErr := s.beginWriteContext(seedCtx)
	if seedErr == nil {
		_, seedErr = txSeed.ExecContext(seedCtx, `UPDATE wal_churn SET payload=randomblob(1024) WHERE id=1`)
		if seedErr == nil {
			seedErr = txSeed.Commit()
		} else {
			_ = txSeed.Rollback()
		}
	}
	s.writeMu.Unlock()
	require.NoError(t, seedErr)
	frontierBefore, frontierOK := readWALIndexSnapshot(s.dbPath)
	require.True(t, frontierOK)
	require.Positive(t, frontierBefore.PendingFrames(), "fixture needs actual uncheckpointed frames, not allocation")
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
	var releases atomic.Int64
	oldRelease := s.writeMu.onRelease.Load()
	releaseProbe := func() {
		releases.Add(1)
		if oldRelease != nil {
			(*oldRelease)()
		}
	}
	s.writeMu.onRelease.Store(&releaseProbe)
	t.Cleanup(func() { s.writeMu.onRelease.Store(oldRelease) })
	holdsBefore := s.writeMu.holds.Load()
	beforeIO := vfsIOMark()
	before := s.WALCopyStats().WrittenBytes
	res := s.reclaimWALAttempt(cfg, db, path)
	calls, copied := passive.Load(), s.WALCopyStats().WrittenBytes-before
	walCheckpointCallObserver = old
	s.writeMu.onRelease.Store(oldRelease)
	require.EqualValues(t, 1, s.writeMu.holds.Load()-holdsBefore, "preflight is a real ordinary gate hold")
	require.EqualValues(t, 1, releases.Load(), "existing release callback must run")
	require.Zero(t, s.writeMu.reclaimSince.Load(), "preflight must not mint reclaim hold credit")
	ioDelta := vfsIOMark().Since(beforeIO)
	frontierAfter, frontierAfterOK := readWALIndexSnapshot(s.dbPath)
	require.True(t, frontierAfterOK)
	t.Logf("frontier before=%+v after=%+v checkpoint_main_write_bytes=%d paced_copy_bytes=%d", frontierBefore, frontierAfter, ioDelta.OtherMainWriteBytes, copied)
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
	t.Logf("actual PASSIVE=%d paced_written_bytes=%d outcome=%v reason=%s WAL=%d; no whole-window physical attribution", calls, copied, res.outcome, res.reason, walFileSize(path))
	require.Zero(t, ioDelta.OtherMainWriteBytes, "known standalone refusal must precede actual checkpoint page writes")
	require.Zero(t, calls, "known standalone refusal must precede actual PASSIVE copy")
	require.Zero(t, copied, "known standalone refusal must not pay paced copy work")
}

// The negative control keeps the same real SQL/VFS witness live without bulk.
func TestStandalonePreflightLeavesOrdinaryCopyEnabled(t *testing.T) {
	s, db := finalBackfillFixture(t)
	frontier, ok := readWALIndexSnapshot(s.dbPath)
	require.True(t, ok)
	require.Positive(t, frontier.PendingFrames())
	before := vfsIOMark()
	res := s.reclaimWALAttempt(walReclaimConfig{thresholdBytes: 1, ceilingBytes: 2 << 30, truncateBudget: walReclaimTruncateBudget}, db, s.dbPath+"-wal")
	require.NotEqual(t, "bulk_writer", res.reason)
	require.Positive(t, vfsIOMark().Since(before).OtherMainWriteBytes, "ordinary attempt must still perform real checkpoint page writes")
	require.True(t, s.writeMu.TryLock(), "ordinary attempt must release its gate")
	s.writeMu.Unlock()
}

// Unlock runs an existing callback while still holding the gate. Cancellation
// observed there must not be hidden by a previously computed bulk shortcut.
func TestStandalonePreflightPreservesCancellationDuringRelease(t *testing.T) {
	s, db := finalBackfillFixture(t)
	s.BeginBulkLoad()
	defer func() { require.NoError(t, s.FlushBulk()) }()
	oldRelease := s.writeMu.onRelease.Load()
	var cancelled atomic.Bool
	releaseProbe := func() {
		if cancelled.CompareAndSwap(false, true) {
			s.backgroundCheckpoint.mu.Lock()
			attempt := s.backgroundCheckpoint.active
			s.backgroundCheckpoint.mu.Unlock()
			if attempt != nil {
				attempt.cancel(context.Canceled)
			}
		}
		if oldRelease != nil {
			(*oldRelease)()
		}
	}
	s.writeMu.onRelease.Store(&releaseProbe)
	t.Cleanup(func() { s.writeMu.onRelease.Store(oldRelease) })
	res := s.reclaimWALAttempt(walReclaimConfig{thresholdBytes: 1, ceilingBytes: 2 << 30, truncateBudget: walReclaimTruncateBudget}, db, s.dbPath+"-wal")
	s.writeMu.onRelease.Store(oldRelease)
	require.True(t, cancelled.Load(), "must exercise real probe release")
	require.NotEqual(t, "bulk_writer", res.reason, "actual attempt cancellation must retain precedence")
	require.ErrorIs(t, context.Cause(res.attempt.ctx), context.Canceled)
}
