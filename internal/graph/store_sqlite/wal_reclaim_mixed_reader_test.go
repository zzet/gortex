package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A complete-copy reset must distinguish real WAL pins from database-only readers: old WAL pin ends after the first reset return,
// while a later database-only (slot0) reader remains alive beyond the cap.
func TestCompleteWALResetRetriesAfterPinEndsWithLateReaderStillAlive(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, path := openWALReclaimStore(t)
	t.Cleanup(func() { _ = s.Close() })
	s.stopCheckpointLoop()
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
	copied, err := checkpointWALOnceOn(t.Context(), ckpt, "PASSIVE")
	require.NoError(t, err)
	require.False(t, copied.incomplete())
	late, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = late.Rollback() })
	var lateBefore int
	require.NoError(t, late.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&lateBefore))
	require.Equal(t, before, lateBefore)
	// Positively establish that the real retained pin blocks SQLite itself:
	// complete backfill and busy=1, not a gate epoch inference.
	s.writeMu.Lock()
	probeCtx, cancelProbe := context.WithTimeout(t.Context(), 500*time.Millisecond)
	busy, busyErr := s.resetWALForReclaim(probeCtx)
	cancelProbe()
	s.writeMu.Unlock()
	require.ErrorIs(t, busyErr, errSQLiteCheckpointIncomplete)
	require.Equal(t, 1, busy.Busy)
	require.Equal(t, busy.WALFrames, busy.CheckpointedFrames)
	require.Positive(t, busy.WALFrames)
	firstReturned, releaseObserved := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	priorObserver := walCheckpointCallObserver
	var firstOnce, releaseOnce sync.Once
	var calls atomic.Int64
	var firstReturnAfter atomic.Int64
	type result struct {
		err     error
		res     walReclaimResult
		elapsed time.Duration
	}
	finished := make(chan result, 1)
	joined := make(chan struct{})
	started := time.Now()
	walCheckpointCallObserver = func(mode string, start time.Time, took time.Duration) {
		if mode == "TRUNCATE" && calls.Add(1) == 1 {
			firstReturnAfter.Store(time.Since(started).Nanoseconds())
			firstOnce.Do(func() { close(firstReturned) })
			// The completed first attempt is the barrier. Release the actual pin
			// BEFORE the epoch waiter starts: this also tests lost-wakeup handling.
			select {
			case <-releaseObserved:
			case <-ctx.Done():
			}
		}
		if priorObserver != nil {
			priorObserver(mode, start, took)
		}
	}
	t.Cleanup(func() {
		cancel()
		releaseOnce.Do(func() { close(releaseObserved) })
		_ = pin.Rollback()
		_ = late.Rollback()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("owned reclaim did not join")
		}
		walCheckpointCallObserver = priorObserver
		_ = ckpt.Close()
	})
	go func() {
		defer close(joined)
		res := walReclaimResult{lastResort: true, urgent: true, bytesBefore: walFileSize(path + "-wal")}
		begin := time.Now()
		err := s.reclaimWALInLane(ctx, walReclaimConfig{thresholdBytes: 1 << 20, readerWait: 30 * time.Second}, ckpt, &res)
		finished <- result{err, res, time.Since(begin)}
	}()
	select {
	case <-firstReturned:
	case <-ctx.Done():
		t.Fatal("no first actual TRUNCATE return")
	}
	require.Less(t, time.Duration(firstReturnAfter.Load()), walReclaimMaxWriterHold, "the real pin must end within the original emergency cap")
	require.NoError(t, pin.Rollback())
	releaseOnce.Do(func() { close(releaseObserved) })
	var got result
	select {
	case got = <-finished:
	case <-ctx.Done():
		t.Fatal("reclaim did not finish")
	}
	// The late transaction is still alive, and must retain its correct view.
	var lateAfter int
	require.NoError(t, late.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&lateAfter))
	require.Equal(t, lateBefore, lateAfter)
	// Positive authority control after any product refusal: with the real pin
	// gone, SQLite resets successfully while that same late transaction lives.
	s.writeMu.Lock()
	authorityCtx, cancelAuthority := context.WithTimeout(t.Context(), 500*time.Millisecond)
	authority, authorityErr := s.resetWALForReclaim(authorityCtx)
	cancelAuthority()
	s.writeMu.Unlock()
	t.Logf("mixed readers: positive_initial_busy=%+v first_product_reset_return=%s elapsed=%s product_error=%v reason=%s writer_hold=%s later_authority=%+v/%v late_reader_alive=true calls=%d", busy, time.Duration(firstReturnAfter.Load()), got.elapsed, got.err, got.res.reason, got.res.writerHold, authority, authorityErr, calls.Load())
	require.NoError(t, authorityErr)
	require.False(t, errors.Is(got.err, context.Canceled))
	require.NoError(t, got.err, "a harmless late reader must not consume the reset's cap after the real WAL pin ends")
	require.True(t, got.res.openGate)
	require.LessOrEqual(t, got.res.writerHold, walReclaimMaxWriterHold+100*time.Millisecond)
	require.Zero(t, walFileSize(path+"-wal"))
	require.Zero(t, s.WALReclaimStats().PauseCount)
}

func TestOlderReaderDecreaseWaitKeepsItsCapturedCohort(t *testing.T) {
	t.Run("release_before_subscription", func(t *testing.T) {
		g := newSQLiteReadGate()
		first, err := g.enterEpoch(t.Context())
		require.NoError(t, err)
		second, err := g.enterEpoch(t.Context())
		require.NoError(t, err)
		defer g.leaveEpoch(second)
		epoch := g.advance()
		before := g.olderCount(epoch)
		g.leaveEpoch(first)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		older, err := g.waitOlderDecrease(ctx, epoch, before, time.Now().Add(time.Second))
		require.NoError(t, err)
		require.Equal(t, 1, older)
	})
	t.Run("newer_release_does_not_wake_reset", func(t *testing.T) {
		g := newSQLiteReadGate()
		old, err := g.enterEpoch(t.Context())
		require.NoError(t, err)
		defer g.leaveEpoch(old)
		epoch := g.advance()
		newer, err := g.enterEpoch(t.Context())
		require.NoError(t, err)
		var releaseNew sync.Once
		defer releaseNew.Do(func() { g.leaveEpoch(newer) })
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		type answer struct {
			older int
			err   error
		}
		done := make(chan answer, 1)
		joined := make(chan struct{})
		go func() {
			defer close(joined)
			n, err := g.waitOlderDecrease(ctx, epoch, 1, time.Now().Add(5*time.Second))
			done <- answer{n, err}
		}()
		t.Cleanup(func() {
			cancel()
			select {
			case <-joined:
			case <-time.After(5 * time.Second):
				t.Error("cohort waiter did not join")
			}
		})
		var subscribed chan struct{}
		require.Eventually(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); subscribed = g.changed; return subscribed != nil }, time.Second, time.Millisecond)
		releaseNew.Do(func() { g.leaveEpoch(newer) })
		require.Eventually(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.changed != nil && g.changed != subscribed }, time.Second, time.Millisecond, "the waiter must process and ignore the newer release")
		cancel()
		select {
		case got := <-done:
			require.Equal(t, 1, got.older)
			require.ErrorIs(t, got.err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("canceled cohort waiter did not return")
		}
	})
	t.Run("no_progress_deadline", func(t *testing.T) {
		g := newSQLiteReadGate()
		old, err := g.enterEpoch(t.Context())
		require.NoError(t, err)
		defer g.leaveEpoch(old)
		epoch := g.advance()
		older, err := g.waitOlderDecrease(t.Context(), epoch, 1, time.Now().Add(20*time.Millisecond))
		require.Equal(t, 1, older)
		require.ErrorIs(t, err, errWALReclaimReadersInFlight)
	})
	t.Run("cancellation_does_not_become_progress", func(t *testing.T) {
		g := newSQLiteReadGate()
		old, err := g.enterEpoch(t.Context())
		require.NoError(t, err)
		epoch := g.advance()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		g.leaveEpoch(old)
		older, err := g.waitOlderDecrease(ctx, epoch, 1, time.Now().Add(time.Second))
		require.Zero(t, older)
		require.ErrorIs(t, err, context.Canceled)
	})
}
