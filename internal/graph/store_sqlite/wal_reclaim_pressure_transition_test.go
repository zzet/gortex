package store_sqlite

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Append real foreground transactions during each actual writer-free WAL sync.
// The first pass remains below pressure; the primary round crosses the mark
// with a >256-frame tail before SQLite publishes backfill. A completed pass
// must hand the WAL to fresh pressure
// admission rather than repeat ordinary rounds until an edit withdraws it.
func TestOrdinaryWALAttemptHandsOverAfterPressureCrossing(t *testing.T) {
	previousRate := walHoldCopyRate.Swap(0)
	t.Cleanup(func() { walHoldCopyRate.Store(previousRate) })
	s, db := finalBackfillFixture(t)
	lane := &fakeBuildLane{}
	lane.install(s)
	path := s.dbPath + "-wal"
	cfg := walReclaimConfig{thresholdBytes: 3 << 20, ceilingBytes: 64 << 20, readerWait: defaultWALReclaimReaderWait, truncateBudget: walReclaimTruncateBudget}
	before, ok := readWALReclaimFrontier(s.dbPath)
	require.True(t, ok)
	require.Less(t, walFileSize(path), walPressureMark(cfg))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	var inject atomic.Bool
	inject.Store(true)
	type batch struct {
		count int
		done  chan error
	}
	requests := make(chan batch)
	producerDone := make(chan struct{})
	var commits, worstWrite atomic.Int64
	go func() {
		defer close(producerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case request := <-requests:
				var err error
				for range request.count {
					wctx, finish := context.WithTimeout(ctx, 500*time.Millisecond)
					started := time.Now()
					err = s.writeMu.LockContext(wctx)
					if err == nil {
						_, err = s.writerDB.ExecContext(wctx, `UPDATE wal_churn SET payload = ? WHERE id % 16 = 2`, fmt.Sprintf("%01024d", commits.Load()+1))
						s.writeMu.Unlock()
					}
					elapsed := int64(time.Since(started))
					worstWrite.Store(max(worstWrite.Load(), elapsed))
					finish()
					if err != nil {
						break
					}
					commits.Add(1)
				}
				request.done <- err
			}
		}
	}()
	stopCancelDone := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		defer close(stopCancelDone)
		s.backgroundCheckpoint.mu.Lock()
		active := s.backgroundCheckpoint.active
		s.backgroundCheckpoint.mu.Unlock()
		if active != nil {
			active.cancel(context.Cause(ctx))
		}
	})
	previous := walCheckpointResultObserver
	var passes int
	var firstPassBytes int64
	var crossing walReclaimFrontier
	var crossingKnown bool
	var producerError error
	walCheckpointResultObserver = func(mode string, start time.Time, took time.Duration, result walCheckpointResult, scanErr error) {
		if previous != nil {
			previous(mode, start, took, result, scanErr)
		}
		if mode != "PASSIVE" || scanErr != nil || result.incomplete() || !inject.Load() || ctx.Err() != nil || s.writeMu.held() {
			return
		}
		passes++
		if passes == 1 {
			firstPassBytes = walFileSize(path)
		}
		if walFileSize(path) >= walPressureMark(cfg) && !crossingKnown {
			crossing, crossingKnown = readWALReclaimFrontier(s.dbPath)
		}
	}
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			cancel()
			if !stopCancel() {
				<-stopCancelDone
			}
			<-producerDone
			walCheckpointResultObserver = previous
			lane.held.Store(false)
		})
	}
	t.Cleanup(cleanup)
	observeAdaptiveMultiFrameSync(t, db, func(file int) {
		if file != vfsFileWAL || !inject.Load() || ctx.Err() != nil || s.writeMu.held() {
			return
		}
		request := batch{count: 2, done: make(chan error, 1)}
		select {
		case requests <- request:
		case <-ctx.Done():
			return
		}
		select {
		case producerError = <-request.done:
		case <-ctx.Done():
			return
		}
		if producerError != nil {
			return
		}
		if walFileSize(path) >= walPressureMark(cfg) && !crossingKnown {
			crossing, crossingKnown = readWALReclaimFrontier(s.dbPath)
		}
		time.Sleep(100 * time.Millisecond)
	}, nil)
	started := time.Now()
	result := s.reclaimWALOnce(cfg, db, path)
	elapsed := time.Since(started)
	inject.Store(false)
	t.Logf("ordinary outcome=%s reason=%s passes=%d foreground_commits=%d elapsed=%s writer_hold=%s crossed=%v tail=%d", result.outcome.String(), result.reason, passes, commits.Load(), elapsed, result.writerSpent, crossingKnown, crossing.mx-crossing.backfill)
	require.NoError(t, producerError)
	require.True(t, crossingKnown, "the primary-round pressure crossing must be real")
	require.Equal(t, before.salt, crossing.salt, "the mark crossed within the original WAL")
	require.Greater(t, crossing.mx-crossing.backfill, walReclaimPressureSmallFrames)
	require.Less(t, firstPassBytes, walPressureMark(cfg), "initial pass must remain below pressure")
	require.GreaterOrEqual(t, passes, 2, "the initial pass alone must not cross the mark")
	require.Equal(t, walReclaimSkipped, result.outcome)
	require.Equal(t, "pressure_mark", result.reason, "ordinary rounds must yield to fresh pressure admission")
	require.Zero(t, result.writerSpent, "handoff must not grant/reset under an ordinary writer hold")
	require.True(t, ctx.Err() == nil, "handoff used the entire bounded observation window")
	// Fresh admission, with the edit held, must apply the existing pressure
	// policy and authoritative reset, not reinterpret the previous SQL result.
	lane.held.Store(true)
	fresh := s.reclaimWALOnce(cfg, db, path)
	require.True(t, fresh.pressure)
	require.Equal(t, walReclaimReset, fresh.outcome, "%s", fresh.reason)
	require.True(t, walLogIsReset(s.dbPath, path))
	require.LessOrEqual(t, fresh.writerSpent, walReclaimMaxWriterHold)
	require.Less(t, time.Duration(worstWrite.Load()), 500*time.Millisecond)
	var payload string
	require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id=2`).Scan(&payload))
	require.Equal(t, fmt.Sprintf("%01024d", commits.Load()), payload)
	require.False(t, s.readGate.closed.Load())
	t.Logf("fresh pressure reset=%v writer_hold=%s foreground_gate_SQL_max=%s", fresh.outcome == walReclaimReset, fresh.writerSpent, time.Duration(worstWrite.Load()))
	cleanup()
}

func TestPressureHandoffPreservesCopyAndAdmissionGuards(t *testing.T) {
	s, _ := finalBackfillFixture(t)
	growWAL(t, s, 16)
	cfg := walReclaimConfig{thresholdBytes: 3 << 20, ceilingBytes: 64 << 20}
	path := s.dbPath + "-wal"
	require.GreaterOrEqual(t, walFileSize(path), walPressureMark(cfg))
	for _, kind := range []string{"complete", "reader_limited", "SQL_error", "unknown_contention", "unknown_no_error", "cancelled", "pressure_off", "bulk_override", "hard_cap", "already_pressure", "below_mark"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := walCheckpointResult{WALFrames: 10, CheckpointedFrames: 10}
			res := walReclaimResult{}
			config := cfg
			var err error
			want := kind == "complete" || kind == "reader_limited"
			previousOff := walPressureOff
			defer func() { walPressureOff = previousOff }()
			switch kind {
			case "reader_limited":
				result.Busy, result.CheckpointedFrames = 1, 3
				err = errSQLiteCheckpointIncomplete
			case "SQL_error":
				err = fmt.Errorf("test disk I/O error")
			case "unknown_contention":
				result = walCheckpointResult{Busy: 1, WALFrames: -1, CheckpointedFrames: -1}
				err = errSQLiteCheckpointIncomplete
			case "unknown_no_error":
				result = walCheckpointResult{WALFrames: -1, CheckpointedFrames: -1}
			case "cancelled":
				cancel()
			case "pressure_off":
				walPressureOff = true
			case "bulk_override":
				res.leaseOverride = true
			case "hard_cap":
				res.hardCap = true
			case "already_pressure":
				res.pressure = true
			case "below_mark":
				config.thresholdBytes = 64 << 20
			}
			require.Equal(t, want, s.walAttemptHandsOverToPressure(ctx, config, path, &res, result, err))
			require.Zero(t, res.writerSpent)
			require.False(t, res.openGate, "handoff is never reset authority")
		})
	}
	// A handoff is a skip, which never adds retry backoff; the existing next
	// coalesced wake is eligible for fresh pressure/lease/lane admission.
	now := time.Now()
	schedule := newWALReclaimSchedule(time.Second, time.Minute)
	schedule.observeProgress(now, walReclaimSkipped, true)
	require.True(t, schedule.ready(now))
	require.Equal(t, time.Second, schedule.backoff)
}

// Qualify the scheduling boundary separately from the same-WAL crossing above:
// the returned first SQL call is held while real writes raise the current mark.
// A fresh pressure attempt must start without waiting for the default 5s poll.
func TestPressureHandoffWakesFreshLoopAdmission(t *testing.T) {
	s, _ := finalBackfillFixture(t)
	path := s.dbPath + "-wal"
	cfg := walReclaimConfig{thresholdBytes: 3 << 20, ceilingBytes: 64 << 20, readerWait: defaultWALReclaimReaderWait, truncateBudget: walReclaimTruncateBudget}
	require.Less(t, walFileSize(path), walPressureMark(cfg))
	wake := make(chan struct{}, 1)
	s.walReclaimWake.Store(&wake)
	done := make(chan struct{})
	crossed := make(chan error, 1)
	proceed := make(chan struct{})
	fresh := make(chan struct{}, 1)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(proceed) }) }
	previous := walCheckpointResultObserver
	first := true
	walCheckpointResultObserver = func(mode string, start time.Time, took time.Duration, result walCheckpointResult, scanErr error) {
		if previous != nil {
			previous(mode, start, took, result, scanErr)
		}
		if mode != "PASSIVE" || scanErr != nil || result.incomplete() {
			return
		}
		s.backgroundCheckpoint.mu.Lock()
		active := s.backgroundCheckpoint.active
		pressure := active != nil && active.copy.pressure
		s.backgroundCheckpoint.mu.Unlock()
		if pressure {
			select {
			case fresh <- struct{}{}:
			default:
			}
			return
		}
		if !first || active == nil {
			return
		}
		first = false
		var err error
		for i := range 16 {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			err = s.writeMu.LockContext(ctx)
			if err == nil {
				_, err = s.writerDB.ExecContext(ctx, `UPDATE wal_churn SET payload = ? WHERE id % 16 = 2`, fmt.Sprintf("%01024d", i+1))
				s.writeMu.Unlock()
			}
			cancel()
			if err != nil {
				break
			}
		}
		crossed <- err
		<-proceed
	}
	go s.runWALReclaimLoop(cfg, path, walReclaimPollInterval, done)
	t.Cleanup(func() {
		release()
		s.stopCheckpointLoop()
		s.backgroundCheckpoint.mu.Lock()
		active := s.backgroundCheckpoint.active
		s.backgroundCheckpoint.mu.Unlock()
		if active != nil {
			active.cancel(context.Canceled)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("owned reclaim loop did not join")
			<-done // retain hook ownership until the owned loop actually exits
		}
		walCheckpointResultObserver = previous
		s.walReclaimWake.Store(nil)
	})
	wake <- struct{}{}
	select {
	case err := <-crossed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("initial ordinary SQL did not return")
	}
	require.GreaterOrEqual(t, walFileSize(path), walPressureMark(cfg))
	started := time.Now()
	release()
	select {
	case <-fresh:
		t.Logf("fresh pressure admission after handoff=%s default_poll=%s", time.Since(started), walReclaimPollInterval)
	case <-time.After(2 * time.Second):
		t.Fatal("pressure handoff waited for the default poll instead of waking fresh admission")
	}
}
