package store_sqlite

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeBuildLane is the lane-holder predicate the daemon installs, driven by
// the test.
type fakeBuildLane struct{ held atomic.Bool }

func (l *fakeBuildLane) install(s *Store) { s.SetBuildLaneBusy(l.held.Load) }

// No background checkpoint attempt starts while a cycle holds the build lane:
// the reclaim is refused before its PASSIVE step (the log is untouched), a
// PASSIVE-loop attempt never runs its checkpoint; once the lane is released
// the next reclaim resets the log.
func TestBackgroundCheckpointDoesNotStartWhileTheBuildLaneIsHeld(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0") // drive attempts directly
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	growWAL(t, s, 8)

	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)

	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()
	cfg := walReclaimConfig{thresholdBytes: 64 << 20, drainDeadline: 250 * time.Millisecond, truncateBudget: walReclaimTruncateBudget, readerWait: 5 * time.Second}

	before, _ := readWALIndexSnapshot(path)
	sizeBefore := walFileSize(path + "-wal")
	res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	after, _ := readWALIndexSnapshot(path)
	require.Equal(t, walReclaimSkipped, res.outcome)
	require.Equal(t, "build_lane_busy", res.reason)
	require.Zero(t, res.writerHold)
	require.Equal(t, before.MxFrame, after.MxFrame, "no reset while the lane is held")
	require.Equal(t, before.NBackfill, after.NBackfill, "not even the PASSIVE step may run while the lane is held")
	require.Equal(t, sizeBefore, walFileSize(path+"-wal"))

	ran := false
	complete, retry := s.runBackgroundCheckpointAttempt(func(context.Context) (bool, bool) { ran = true; return true, false })
	require.False(t, ran, "a PASSIVE-loop attempt started while the lane was held")
	require.False(t, complete)
	require.True(t, retry)

	st := s.WALReclaimStats()
	require.Equal(t, int64(1), st.CycleRefusals)
	require.Zero(t, st.CycleYields)

	lane.held.Store(false)
	res = s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	require.Equal(t, walReclaimReset, res.outcome, "reason=%q", res.reason)
	require.Zero(t, walFileSize(path+"-wal"))
}

// An attempt in flight when a cycle takes the lane stops at its next step:
// the reclaim parked in its writer-free wait for an old reader returns within
// a few polls (skipped, build_lane_busy, writer never taken), and a
// PASSIVE-loop attempt's context is cancelled with the yield cause. The forced
// PASSIVE ignores the lane.
func TestBackgroundCheckpointInFlightYieldsWhenACycleTakesTheLane(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	growWAL(t, s, 8)

	lane := &fakeBuildLane{}
	lane.install(s)

	// An old reader pins the log, so the reclaim waits (without the writer)
	// for up to readerWait = 20 s.
	pinned, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = pinned.Rollback() }()
	var n int
	require.NoError(t, pinned.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&n))
	growWAL(t, s, 8)

	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()
	cfg := walReclaimConfig{thresholdBytes: 64 << 20, drainDeadline: 250 * time.Millisecond, truncateBudget: walReclaimTruncateBudget, readerWait: 20 * time.Second}

	done := make(chan walReclaimResult, 1)
	go func() { done <- s.reclaimWALOnce(cfg, ckpt, path+"-wal") }()
	// Let it reach the reader wait.
	time.Sleep(500 * time.Millisecond)
	select {
	case res := <-done:
		t.Fatalf("reclaim finished before the cycle began: outcome=%s reason=%q", res.outcome, res.reason)
	default:
	}
	began := time.Now()
	lane.held.Store(true)
	var res walReclaimResult
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an in-flight reclaim kept running after a cycle took the build lane")
	}
	stopped := time.Since(began)
	t.Logf("in-flight reclaim: stopped %s after the cycle began, outcome=%s reason=%q writer_hold=%s", stopped, res.outcome, res.reason, res.writerHold)
	require.Equal(t, walReclaimSkipped, res.outcome)
	require.Equal(t, "build_lane_busy", res.reason)
	require.Zero(t, res.writerHold, "the writer must not be taken")
	require.Less(t, stopped, time.Second)
	require.Equal(t, int64(1), s.WALReclaimStats().CycleYields)

	// A PASSIVE-loop attempt: cancelled with the yield cause once the lane
	// is taken mid-run; reported as retry, not complete.
	lane.held.Store(false)
	var cause error
	var passiveComplete, passiveRetry bool
	start := make(chan struct{})
	passiveDone := make(chan struct{})
	go func() {
		defer close(passiveDone)
		passiveComplete, passiveRetry = s.runBackgroundCheckpointAttempt(func(ctx context.Context) (bool, bool) {
			close(start)
			<-ctx.Done()
			cause = context.Cause(ctx)
			return true, false
		})
	}()
	<-start
	lane.held.Store(true)
	select {
	case <-passiveDone:
	case <-time.After(5 * time.Second):
		t.Fatal("an in-flight PASSIVE attempt kept running after a cycle took the build lane")
	}
	require.ErrorIs(t, cause, errWALCheckpointYieldedToCycle)
	require.False(t, passiveComplete, "a yielded attempt must not count as complete")
	require.True(t, passiveRetry)

	// The forced PASSIVE runs although the lane is held, and is not cut short.
	forcedRan := false
	complete, _ := s.runBackgroundCheckpointAttemptWith(checkpointIgnoresCycle, func(ctx context.Context) (bool, bool) {
		forcedRan = true
		select {
		case <-ctx.Done():
			return false, true
		case <-time.After(10 * walCheckpointCycleYieldPoll):
			return true, false
		}
	})
	require.True(t, forcedRan)
	require.True(t, complete, "the forced PASSIVE was cancelled by the lane")
}

// The PASSIVE schedule defers a due attempt for as long as the lane is held,
// without backoff and without ever forcing one (its CPU would come out of the
// edit; a lane that never idles is bounded by the reclaim's hard cap instead);
// released, the attempt runs unforced.
func TestPassiveCheckpointNeverRunsWhileTheLaneIsHeld(t *testing.T) {
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	growWAL(t, s, 4)
	walPath := path + "-wal"
	require.Greater(t, walFileSize(walPath), int64(1<<10))

	logs := captureReclaimLog(t)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)
	const bound = 10 * time.Second
	view := cycleLane{store: s, maxDeferral: bound}

	type call struct {
		at     time.Duration
		forced bool
	}
	var calls []call
	now := time.Now()
	checkpointAt := func(at time.Duration) func(bool) (bool, bool) {
		return func(forced bool) (bool, bool) {
			calls = append(calls, call{at: at, forced: forced})
			return false, true // incomplete: stays due
		}
	}
	sched := newWALCheckpointSchedule(now, time.Hour, 64) // WAL above threshold: due every poll
	for at := time.Duration(0); at < 5*bound; at += time.Second {
		require.False(t, sched.attemptYielding(now.Add(at), walPath, view, checkpointAt(at)), "a lane deferral must not back off")
	}
	require.Empty(t, calls, "an attempt ran while the lane was held")

	// Released: the due attempt runs, unforced.
	lane.held.Store(false)
	sched.attemptYielding(now.Add(6*bound), walPath, view, checkpointAt(6*bound))
	require.Len(t, calls, 1)
	require.False(t, calls[0].forced)

	st := s.WALReclaimStats()
	require.Zero(t, st.CycleForced)
	require.Positive(t, st.CycleDeferrals)
	out := logs.String()
	require.Contains(t, out, "wal checkpoint deferred mode=PASSIVE reason=build_lane_busy")
	require.NotContains(t, out, "wal checkpoint forced")

	// Without a predicate (or with the yield disabled) nothing defers.
	s.SetBuildLaneBusy(nil)
	lane.held.Store(true)
	plain := newWALCheckpointSchedule(now, time.Hour, 64)
	plain.attemptYielding(now, walPath, s.cycleLane(), checkpointAt(0))
	require.Len(t, calls, 2)
}

// Under a lane that never goes idle the log passes its ceiling (the reclaim
// waits for gaps between edit cycles), and over the hard cap (4x the ceiling)
// one bounded attempt runs despite the lane, spaced and logged, so the log
// still cannot grow without bound.
func TestWALReclaimRunsDespiteTheLaneOnlyOverTheHardCap(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "4")
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB", "12")
	prevFloor, prevSpacing := walReclaimCeilingFloor, walReclaimHardCapSpacing
	walReclaimCeilingFloor, walReclaimHardCapSpacing = 0, time.Second
	t.Cleanup(func() { walReclaimCeilingFloor, walReclaimHardCapSpacing = prevFloor, prevSpacing })
	setWALReclaimCadence(t, 50*time.Millisecond, 50*time.Millisecond, 400*time.Millisecond)
	logs := captureReclaimLog(t)
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true) // a cycle that never ends

	churn := startWALChurn(t, s, path, 4, 150*time.Millisecond, 300*time.Millisecond)
	churn.runUntil(t, 160, 120*time.Second)
	churn.halt(t)

	const ceiling, hardCap = 12 << 20, 4 * (12 << 20)
	stats := s.WALReclaimStats()
	t.Logf("held lane: writes=%d wal_max=%.1fMiB ceiling=%.0fMiB hard_cap_runs=%d resets=%d refusals=%d writer_hold_max=%s",
		churn.writes.Load(), float64(churn.maxWAL.Load())/(1<<20), float64(stats.CeilingBytes)/(1<<20),
		stats.CycleCeilingRuns, stats.Resets, stats.CycleRefusals, stats.WriterHoldMax)
	require.Equal(t, int64(ceiling), stats.CeilingBytes)
	require.Greater(t, churn.maxWAL.Load(), int64(ceiling), "precondition: the log passed its ceiling under the held lane")
	require.Positive(t, stats.CycleCeilingRuns, "no reclaim ran over the hard cap")
	require.Positive(t, stats.Resets, "the hard-cap runs must reset the log")
	require.Positive(t, stats.CycleRefusals, "below the hard cap the held lane must refuse the reclaim")
	require.LessOrEqual(t, churn.maxWAL.Load(), int64(2*hardCap), "the WAL grew past twice its hard cap under a held lane")
	require.LessOrEqual(t, stats.WriterHoldMax, walReclaimMaxWriterHold+250*time.Millisecond, "the writer-hold cap bounds a hard-cap run")
	out := logs.String()
	require.Contains(t, out, "reason=wal_hard_cap")
	require.NotContains(t, out, "reason=wal_ceiling wal_bytes", "a ceiling run started during the edit cycle")
}
