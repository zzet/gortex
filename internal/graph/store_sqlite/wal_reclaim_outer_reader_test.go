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
	var calls atomic.Int64
	var once sync.Once
	joined := make(chan struct{})
	type answer struct {
		res     walReclaimResult
		elapsed time.Duration
	}
	done := make(chan answer, 1)
	started := time.Now()
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
	})
	go func() {
		defer close(joined)
		begin := time.Now()
		cfg := walReclaimConfig{thresholdBytes: 1 << 30, readerWait: 5 * time.Second, truncateBudget: walReclaimTruncateBudget, drainDeadline: 250 * time.Millisecond}
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
		done <- answer{res, time.Since(begin)}
	}()
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("no actual first outer TRUNCATE return")
	}
	require.Less(t, time.Since(started), time.Second, "release actual pin well within original outer observation wait")
	require.NoError(t, pin.Rollback())
	once.Do(func() { close(releaseBarrier) })
	var got answer
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
	t.Logf("actual outer mixed: initialBusy=%+v full=%+v elapsed=%s outcome=%s reason=%s openGateReport=%s writerHold=%s laterAuthority=%+v/%v harmlessReaderAlive=true actualTruncates=%d", positive, full, got.elapsed, got.res.outcome, got.res.reason, got.res.openGateReport, got.res.writerHold, authority, authorityErr, calls.Load())
	require.NoError(t, authorityErr)
	require.Equal(t, 0, authority.Busy)
	require.Zero(t, s.readGate.waits.Load())
	require.False(t, s.readGate.closed.Load())
	require.LessOrEqual(t, got.res.writerHold, walReclaimResetHold+raceSlack(10*time.Millisecond))
	require.Nil(t, got.res.resetReaders, "completed reset must not retain a stale cohort witness")
	require.Equal(t, walReclaimReset, got.res.outcome, "the actual outer path must ask SQLite again after a real pin retires rather than wait for a harmless retained reader")
	require.Less(t, got.elapsed, time.Second, "safe reset must not consume the full cohort wait")
}
