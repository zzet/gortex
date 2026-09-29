package store_sqlite

import (
	"context"
	"database/sql"
	"strings"
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

// The PASSIVE schedule defers a due attempt while the lane is held, without
// backoff; past the deferral bound with the WAL above its threshold it runs
// exactly one forced attempt and starts a new episode; below the threshold it
// keeps deferring; released, the attempt runs unforced.
func TestPassiveCheckpointDeferralIsBounded(t *testing.T) {
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
	for at := time.Duration(0); at < bound; at += time.Second {
		require.False(t, sched.attemptYielding(now.Add(at), walPath, view, checkpointAt(at)), "a lane deferral must not back off")
	}
	require.Empty(t, calls, "an attempt started while the lane was held")
	sched.attemptYielding(now.Add(bound), walPath, view, checkpointAt(bound))
	require.Equal(t, []call{{at: bound, forced: true}}, calls, "one forced PASSIVE past the bound")
	for at := bound + time.Second; at < 2*bound; at += time.Second {
		sched.attemptYielding(now.Add(at), walPath, view, checkpointAt(at))
	}
	require.Len(t, calls, 1, "the forced attempt starts a new deferral episode")
	sched.attemptYielding(now.Add(2*bound+time.Second), walPath, view, checkpointAt(2*bound+time.Second))
	require.Len(t, calls, 2)
	require.True(t, calls[1].forced)

	// Below the threshold the bound never forces: nothing to backfill for.
	quiet := newWALCheckpointSchedule(now, time.Second, 1<<40) // periodic due, WAL under threshold
	for at := 2 * time.Second; at < 5*bound; at += time.Second {
		quiet.attemptYielding(now.Add(at), walPath, view, checkpointAt(-at))
	}
	require.Len(t, calls, 2, "a forced attempt ran with the WAL under its threshold")

	// Released: the due attempt runs, unforced.
	lane.held.Store(false)
	sched.attemptYielding(now.Add(3*bound), walPath, view, checkpointAt(3*bound))
	require.Len(t, calls, 3)
	require.False(t, calls[2].forced)

	st := s.WALReclaimStats()
	require.Equal(t, int64(2), st.CycleForced)
	require.Positive(t, st.CycleDeferrals)
	out := logs.String()
	require.Contains(t, out, "wal checkpoint deferred mode=PASSIVE reason=build_lane_busy")
	require.Contains(t, out, "wal checkpoint forced mode=PASSIVE reason=cycle_deferral_bound")
	require.Equal(t, 3, strings.Count(out, "reason=build_lane_busy"), "one deferral line per episode (two forced-bounded episodes, one below the threshold)")

	// Without a predicate (or with the yield disabled) nothing defers.
	s.SetBuildLaneBusy(nil)
	lane.held.Store(true)
	plain := newWALCheckpointSchedule(now, time.Hour, 64)
	plain.attemptYielding(now, walPath, s.cycleLane(), checkpointAt(0))
	require.Len(t, calls, 4)
}

// Over the WAL ceiling the reclaim runs despite a held lane: under continuous
// writes, rotating readers and a lane that never goes idle, the log is reset
// and stays near the ceiling instead of growing without bound. Below the
// ceiling a held lane still refuses it (the "does not start" case above runs
// with the production floor, 1 GiB).
func TestWALReclaimRunsDespiteTheLaneOverTheWALCeiling(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "4")
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB", "24")
	prevFloor := walReclaimCeilingFloor
	walReclaimCeilingFloor = 0
	t.Cleanup(func() { walReclaimCeilingFloor = prevFloor })
	setWALReclaimCadence(t, 50*time.Millisecond, 50*time.Millisecond, 400*time.Millisecond)
	logs := captureReclaimLog(t)
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true) // a cycle that never ends

	churn := startWALChurn(t, s, path, 4, 150*time.Millisecond, 300*time.Millisecond)
	churn.runUntil(t, 110, 90*time.Second)
	churn.halt(t)

	const ceiling = 24 << 20
	stats := s.WALReclaimStats()
	t.Logf("held lane: writes=%d wal_max=%.1fMiB ceiling=%.0fMiB ceiling_runs=%d resets=%d refusals=%d yields=%d writer_hold_max=%s",
		churn.writes.Load(), float64(churn.maxWAL.Load())/(1<<20), float64(stats.CeilingBytes)/(1<<20),
		stats.CycleCeilingRuns, stats.Resets, stats.CycleRefusals, stats.CycleYields, stats.WriterHoldMax)
	require.Equal(t, int64(ceiling), stats.CeilingBytes)
	require.Positive(t, stats.CycleCeilingRuns, "no reclaim ran over the ceiling")
	require.Positive(t, stats.Resets, "the ceiling runs must reset the log")
	require.Positive(t, stats.CycleRefusals, "below the ceiling the held lane must still refuse the reclaim")
	require.LessOrEqual(t, churn.maxWAL.Load(), int64(2*ceiling), "the WAL grew past twice its ceiling under a held lane")
	require.LessOrEqual(t, stats.WriterHoldMax, walReclaimMaxWriterHold+250*time.Millisecond, "the writer-hold cap bounds a ceiling run")
	require.Contains(t, logs.String(), "reason=wal_ceiling")
}
