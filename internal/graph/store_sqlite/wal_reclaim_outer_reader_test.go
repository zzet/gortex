package store_sqlite

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The outer writer-free attempt: the actual writer-free outer attempt cannot wait on
// a harmless database-only reader after its actual WAL pin retires.
func TestOuterWALAttemptRetriesWithHarmlessReaderAlive(t *testing.T) {
	runWALOuterMixedReader(t, false)
}

func TestInLaneOpenGateRetriesWithHarmlessReaderAlive(t *testing.T) {
	runWALOuterMixedReader(t, true)
}

func runWALOuterMixedReader(t *testing.T, inLane bool) {
	t.Helper()
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, path := openWALReclaimStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedWALChurnTable(t, s)
	growWAL(t, s, 8)
	pin, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = pin.Rollback() })
	var before int
	require.NoError(t, pin.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&before))
	ckpt, err := openWALReclaimCheckpointDB(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ckpt.Close() })
	full, err := checkpointWALOnceOn(t.Context(), ckpt, "PASSIVE")
	require.NoError(t, err)
	require.Positive(t, full.WALFrames)
	require.Equal(t, full.WALFrames, full.CheckpointedFrames)
	late, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = late.Rollback() })
	var lateBefore int
	require.NoError(t, late.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&lateBefore))
	require.Equal(t, before, lateBefore)
	s.writeMu.Lock()
	probe, cancelProbe := context.WithTimeout(t.Context(), 500*time.Millisecond)
	positive, positiveErr := s.resetWALForReclaim(probe)
	cancelProbe()
	s.writeMu.Unlock()
	require.ErrorIs(t, positiveErr, errSQLiteCheckpointIncomplete)
	require.Equal(t, 1, positive.Busy)
	require.Positive(t, positive.WALFrames)
	require.Equal(t, positive.WALFrames, positive.CheckpointedFrames)
	controlCtx, cancelControl := context.WithCancel(t.Context())
	first, releaseBarrier := make(chan struct{}), make(chan struct{})
	prior := walCheckpointCallObserver
	priorRound := walReclaimRoundObserver
	var partialWaits, retainedReaders int
	walReclaimRoundObserver = func(stage string, epoch uint64, older int, err error) {
		if stage == "partial_wait_end" && err == nil {
			partialWaits++
			retainedReaders = older
		}
		if priorRound != nil {
			priorRound(stage, epoch, older, err)
		}
	}
	var calls atomic.Int64
	var once sync.Once
	joined := make(chan struct{})
	done := make(chan walReclaimResult, 1)
	walCheckpointCallObserver = func(mode string, start time.Time, took time.Duration) {
		if mode == "TRUNCATE" && calls.Add(1) == 1 {
			close(first)
			select {
			case <-releaseBarrier:
			case <-controlCtx.Done():
			}
		}
		if prior != nil {
			prior(mode, start, took)
		}
	}
	t.Cleanup(func() {
		cancelControl()
		once.Do(func() { close(releaseBarrier) })
		_ = pin.Rollback()
		_ = late.Rollback()
		s.backgroundCheckpoint.mu.Lock()
		if a := s.backgroundCheckpoint.active; a != nil {
			a.cancel(context.Canceled)
		}
		s.backgroundCheckpoint.mu.Unlock()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("outer attempt failed to join")
		}
		walCheckpointCallObserver = prior
		walReclaimRoundObserver = priorRound
	})
	go func() {
		defer close(joined)
		cfg := walReclaimConfig{thresholdBytes: 1 << 30, readerWait: time.Hour, truncateBudget: walReclaimTruncateBudget, drainDeadline: 250 * time.Millisecond}
		var res walReclaimResult
		if inLane {
			ctx, cancel := context.WithTimeout(controlCtx, walReclaimLaneBudget)
			defer cancel()
			res = walReclaimResult{bytesBefore: walFileSize(path + "-wal")}
			if err := s.reclaimWALInLane(ctx, cfg, ckpt, &res); err != nil {
				res.outcome = walReclaimDeferred
			} else {
				res.outcome = walReclaimReset
			}
		} else {
			res = s.reclaimWALOnce(cfg, ckpt, path+"-wal")
		}
		done <- res
	}()
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("no actual first outer TRUNCATE return")
	}
	require.NoError(t, pin.Rollback())
	once.Do(func() { close(releaseBarrier) })
	var got walReclaimResult
	select {
	case got = <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("outer attempt never returned")
	}
	var after int
	require.NoError(t, late.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&after))
	require.Equal(t, lateBefore, after)
	s.writeMu.Lock()
	authorityCtx, cancelAuthority := context.WithTimeout(t.Context(), 500*time.Millisecond)
	authority, authorityErr := s.resetWALForReclaim(authorityCtx)
	cancelAuthority()
	s.writeMu.Unlock()
	require.NoError(t, authorityErr)
	require.Equal(t, 0, authority.Busy)
	require.Zero(t, s.readGate.waits.Load())
	require.False(t, s.readGate.closed.Load())
	require.Nil(t, got.resetReaders, "completed reset must not retain a stale cohort witness")
	require.Equal(t, walReclaimReset, got.outcome, "the actual outer path must ask SQLite again after a real pin retires rather than wait for a harmless retained reader")
	require.Positive(t, partialWaits, "the completed-copy cohort must use partial readmission")
	require.Positive(t, retainedReaders, "partial readmission must retain the harmless reader")
	require.GreaterOrEqual(t, calls.Load(), int64(2), "retiring the real pin must retry SQLite while the harmless reader remains alive")
}
