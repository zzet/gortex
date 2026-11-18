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

func TestWALReclaimFinalCauseSurvivesDisappearingDemand(t *testing.T) {
	for _, kind := range []string{"intent", "parked", "both", "shutdown"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
			s, path := openWALReclaimStore(t)
			defer func() { require.NoError(t, s.Close()) }()
			seedWALChurnTable(t, s)
			growWAL(t, s, 4)
			ckpt, err := openWALReclaimCheckpointDB(path)
			require.NoError(t, err)
			defer ckpt.Close()
			before := s.WALReclaimStats()
			oldHook := walResetCheckpointConnObserver
			defer func() { walResetCheckpointConnObserver = oldHook }()
			entered := make(chan context.Context, 1)
			beforeReturn := make(chan func(), 1)
			var resetCalls atomic.Int32
			walResetCheckpointConnObserver = func(ctx context.Context, _ *sql.Conn) {
				if resetCalls.Add(1) != 1 {
					// Record unexpected reentry without parking on a second
					// test token. The completion oracle below must reject it.
					return
				}
				entered <- ctx
				<-ctx.Done()
				// Remove real demand BEFORE the attempt consumes the cancelled
				// query result/updates counters. Attribution must use its cause.
				cleanup := <-beforeReturn
				cleanup()
			}
			done := make(chan walReclaimResult, 1)
			joined := false
			defer func() {
				if joined {
					return
				}
				s.backgroundCheckpoint.mu.Lock()
				attempt := s.backgroundCheckpoint.active
				s.backgroundCheckpoint.mu.Unlock()
				if attempt != nil {
					attempt.cancel(context.Canceled)
				}
				select {
				case beforeReturn <- func() {}:
				default:
				}
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("owned reclaim did not join")
				}
			}()
			go func() {
				// readerWait=0 selects the supported terminal closed-gate
				// path. An ordinary open-gate round may legitimately retry
				// after the demand disappears, even within one attempt.
				done <- s.reclaimWALOnce(walReclaimConfig{thresholdBytes: 1 << 30,
					drainDeadline: time.Second, truncateBudget: walReclaimTruncateBudget}, ckpt, path+"-wal")
			}()
			var resetCtx context.Context
			select {
			case resetCtx = <-entered:
			case result := <-done:
				joined = true
				t.Fatalf("real reset boundary was not reached: %+v", result)
			case <-time.After(5 * time.Second):
				t.Fatal("real reset did not start")
			}
			var cleanups []func()
			if kind == "parked" || kind == "both" {
				writerCtx, cancel := context.WithCancel(t.Context())
				writerDone := make(chan error, 1)
				go func() {
					err := s.writeMu.LockContext(writerCtx)
					if err == nil {
						s.writeMu.Unlock()
					}
					writerDone <- err
				}()
				var once sync.Once
				stopWriter := func() { once.Do(func() { cancel(); <-writerDone }) }
				defer stopWriter()
				cleanups = append(cleanups, stopWriter)
				require.Eventually(t, func() bool { return s.writeMu.waiting() > 0 }, time.Second, time.Millisecond,
					"an actual foreground writer must park behind the held reclaim gate")
			}
			if kind == "intent" || kind == "both" {
				release := s.AnnounceWrite()
				defer release()
				cleanups = append(cleanups, release)
				if kind == "both" {
					require.Same(t, walParkedDemand, s.writeDemandCause(), "observed parked demand takes precedence when both are positive")
				}
			}
			if kind == "shutdown" {
				attempt := resetCtx.Value(walReclaimAttemptKey{}).(*backgroundCheckpointAttempt)
				attempt.cancel(context.Canceled)
			}
			beforeReturn <- func() {
				for _, cleanup := range cleanups {
					cleanup()
				}
			}
			var result walReclaimResult
			select {
			case result = <-done:
				joined = true
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled reclaim did not finish")
			}
			require.EqualValues(t, 1, resetCalls.Load(), "unexpected reset reentry is not a terminal cancelled attempt")
			require.NotEqual(t, walReclaimReset, result.outcome)
			require.Zero(t, s.writeIntents.Load())
			require.Zero(t, s.writeMu.waiting())
			after := s.WALReclaimStats()
			parked := after.CancelledWriterParked - before.CancelledWriterParked
			intent := after.CancelledWriteIntent - before.CancelledWriteIntent
			other := after.CancelledOther - before.CancelledOther
			require.Equal(t, int64(1), parked+intent+other, "one final attempt, not polling/child/cleanup counts")
			switch kind {
			case "parked", "both":
				require.ErrorIs(t, context.Cause(resetCtx), errWALReclaimWriterWaiting)
				require.Equal(t, int64(1), parked)
			case "intent":
				require.ErrorIs(t, context.Cause(resetCtx), errWALReclaimWriterWaiting)
				require.Equal(t, int64(1), intent)
			case "shutdown":
				require.Equal(t, int64(1), other)
			}
		})
	}
}

func TestWALReclaimDemandConsumersRetainOneAttemptCause(t *testing.T) {
	attempt := &backgroundCheckpointAttempt{}
	ctx := context.WithValue(t.Context(), walReclaimAttemptKey{}, attempt)
	noteWALReclaimDemand(ctx, walParkedDemand)
	noteWALReclaimDemand(ctx, walParkedDemand)
	noteWALReclaimDemand(ctx, walIntentDemand)
	require.Equal(t, uint32(walReclaimWriterParked), attempt.reclaimInterruption.Load())
	var stats WALReclaimStats
	stats.noteInterruption(walReclaimResult{attempt: attempt})
	require.Equal(t, int64(1), stats.CancelledWriterParked)
	require.Zero(t, stats.CancelledWriteIntent+stats.CancelledOther)
}

func TestWALReclaimHelperCleanupAndResetAreNotExtraAttempts(t *testing.T) {
	s, _ := openWALReclaimStore(t)
	defer s.Close()
	attempt, err := s.beginCheckpointAttempt(checkpointIgnoresCycle, true)
	require.NoError(t, err)
	ctx, stop := s.yieldToWriters(attempt.ctx)
	stop() // unconditional helper cleanup: not an operation cancellation
	require.ErrorIs(t, context.Cause(ctx), context.Canceled)
	require.Zero(t, attempt.reclaimInterruption.Load())
	noteWALReclaimCancellation(attempt, context.DeadlineExceeded)
	require.Zero(t, attempt.reclaimInterruption.Load(), "a child budget is not a caller/shutdown cancellation")
	s.finishBackgroundCheckpointAttempt(attempt)
	require.Zero(t, attempt.reclaimInterruption.Load(), "normal attempt cleanup must not increment causes")
	var stats WALReclaimStats
	attempt.reclaimInterruption.Store(uint32(walReclaimWriteIntent))
	stats.noteInterruption(walReclaimResult{attempt: attempt, outcome: walReclaimReset})
	require.Zero(t, stats.CancelledWriteIntent, "a successful final reset is not a cancelled attempt")
	var cause *walReclaimDemandCause
	require.True(t, errors.As(walParkedDemand, &cause))
	require.ErrorIs(t, walParkedDemand, errWALReclaimWriterWaiting)
}

func TestObservedWALPeakIsSharedPerStoreAndSurvivesReset(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, _ := openWALReclaimStore(t)
	defer s.Close()
	other, _ := openWALReclaimStore(t)
	defer other.Close()
	otherBefore := other.WALReclaimStats()
	seedWALChurnTable(t, s)
	growWAL(t, s, 4)
	_, wal := s.DBStats()
	require.Positive(t, wal)
	before := s.WALReclaimStats()
	require.True(t, before.WALPeakObserved)
	require.GreaterOrEqual(t, before.ObservedPeakWALBytes, wal)
	require.Equal(t, before.ObservedPeakWALBytes, s.AtGeneration(7).WALReclaimStats().ObservedPeakWALBytes)
	func() {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		_, err := s.resetWALForReclaim(t.Context())
		require.NoError(t, err)
	}()
	_, afterWAL := s.DBStats()
	require.Less(t, afterWAL, wal)
	require.Equal(t, before.ObservedPeakWALBytes, s.WALReclaimStats().ObservedPeakWALBytes,
		"TRUNCATE/shrink/status reads cannot reset high-water evidence")
	require.Equal(t, otherBefore.ObservedPeakWALBytes, other.WALReclaimStats().ObservedPeakWALBytes)
	require.Equal(t, otherBefore.WALPeakObserved, other.WALReclaimStats().WALPeakObserved)
	var empty Store
	require.False(t, empty.WALReclaimStats().WALPeakObserved)
}

func TestWALReclaimSuccessfulFinalResetDoesNotCountCleanup(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, path := openWALReclaimStore(t)
	defer s.Close()
	seedWALChurnTable(t, s)
	growWAL(t, s, 4)
	ckpt, err := openWALReclaimCheckpointDB(path)
	require.NoError(t, err)
	defer ckpt.Close()
	res := s.reclaimWALOnce(walReclaimConfig{thresholdBytes: 1 << 30, drainDeadline: time.Second, truncateBudget: walReclaimTruncateBudget, readerWait: time.Second}, ckpt, path+"-wal")
	require.Equal(t, walReclaimReset, res.outcome)
	stats := s.WALReclaimStats()
	require.Zero(t, stats.CancelledWriterParked+stats.CancelledWriteIntent+stats.CancelledOther, "normal operation/yield-context cleanup is not a cancelled attempt")
	memory, err := Open(":memory:")
	require.NoError(t, err)
	defer memory.Close()
	memory.DBStats()
	memory.noteObservedWALBytes(64)
	require.False(t, memory.WALReclaimStats().WALPeakObserved, "in-memory stores have no physical WAL observation")
}
