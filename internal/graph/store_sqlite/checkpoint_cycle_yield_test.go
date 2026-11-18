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

	require.NotNil(t, s.readGate)
	s.readGate.mu.Lock()
	initialEpoch := s.readGate.epoch
	s.readGate.mu.Unlock()
	done := make(chan walReclaimResult, 1)
	go func() { done <- s.reclaimWALOnce(cfg, ckpt, path+"-wal") }()
	reclaimReturned := false
	defer func() {
		if !reclaimReturned {
			// Release the fixture's pinned reader and join the real attempt before
			// its checkpoint connection and store are closed, even on setup failure.
			lane.held.Store(false)
			_ = pinned.Rollback()
			select {
			case <-done:
			case <-time.After(25 * time.Second):
				t.Error("reclaim did not return during fixture cleanup")
			}
		}
	}()
	// Wait for the real writer-free old-reader boundary. The initial PASSIVE
	// may take longer than a guessed sleep on a slow filesystem.
	ready := time.NewTicker(time.Millisecond)
	defer ready.Stop()
	warmup := time.NewTimer(20 * time.Second)
	defer warmup.Stop()
waitForReader:
	for {
		s.readGate.mu.Lock()
		waiting := s.readGate.epoch > initialEpoch && s.readGate.changed != nil && s.readGate.olderLocked(s.readGate.epoch) > 0
		s.readGate.mu.Unlock()
		if waiting {
			break waitForReader
		}
		select {
		case res := <-done:
			reclaimReturned = true
			t.Fatalf("reclaim finished before the cycle began: outcome=%s reason=%q", res.outcome, res.reason)
		case <-warmup.C:
			t.Fatal("reclaim did not reach its writer-free old-reader wait")
		case <-ready.C:
		}
	}
	began := time.Now()
	lane.held.Store(true)
	var res walReclaimResult
	select {
	case res = <-done:
		reclaimReturned = true
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

// The hard cap is an admission threshold, not a byte-rate bound on writes
// between reclaim attempts. Drive the held-lane decision and one real reset
// over a fixed WAL, then check spacing with explicit times rather than churn.
func TestWALReclaimRunsDespiteTheLaneOnlyOverTheHardCap(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0") // explicit attempts only
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)
	defer lane.held.Store(false)
	seedWALChurnTable(t, s)
	growWAL(t, s, 4)
	walPath := path + "-wal"
	size := walFileSize(walPath)
	require.Greater(t, size, int64(walReclaimHardCapFactor))
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer func() { _ = ckpt.Close() }()
	logs := captureReclaimLog(t)

	// Below both marks the held lane refuses before touching the checkpoint.
	cfg := resolveWALReclaimConfig()
	cfg.thresholdBytes, cfg.ceilingBytes = size+1, size+1
	require.Less(t, size, walPressureMark(cfg))
	require.Less(t, size, walReclaimHardCapFactor*cfg.ceilingBytes)
	before, found := readWALIndexSnapshot(path)
	require.True(t, found)
	refused := s.reclaimWALOnce(cfg, ckpt, walPath)
	require.Equal(t, walReclaimSkipped, refused.outcome)
	require.Equal(t, "build_lane_busy", refused.reason)
	require.Zero(t, s.WALReclaimStats().CycleCeilingRuns)
	require.Equal(t, int64(1), s.WALReclaimStats().CycleRefusals)
	after, found := readWALIndexSnapshot(path)
	require.True(t, found)
	require.Equal(t, before.MxFrame, after.MxFrame)
	require.Equal(t, before.NBackfill, after.NBackfill)
	require.Equal(t, size, walFileSize(walPath))

	// The exact same WAL is now above the configured hard-cap threshold.
	// There are no writer or reader churn goroutines to race this decision.
	cfg.ceilingBytes = size / walReclaimHardCapFactor
	cfg.thresholdBytes = cfg.ceilingBytes
	require.GreaterOrEqual(t, size, walReclaimHardCapFactor*cfg.ceilingBytes)
	admitted := s.reclaimWALOnce(cfg, ckpt, walPath)
	require.True(t, admitted.hardCap, "the hard-cap policy must override the held lane")
	require.True(t, admitted.began)
	require.Equal(t, walReclaimReset, admitted.outcome, "reason=%q", admitted.reason)
	require.Zero(t, walFileSize(walPath))
	stats := s.WALReclaimStats()
	require.Equal(t, int64(1), stats.CycleCeilingRuns)
	require.Equal(t, int64(1), stats.Resets)
	require.Contains(t, logs.String(), "reason=wal_hard_cap")
	require.NotContains(t, logs.String(), "reason=wal_ceiling wal_bytes")

	// Spacing limits only hard-cap overrides. Pressure remains a separate
	// admission class; this check deliberately does not forbid pressure work.
	last := s.walReclaim.cycle.lastHardCap.Load()
	require.Positive(t, last)
	at := time.Unix(0, last)
	require.False(t, s.hardCapDue(at))
	require.False(t, s.hardCapDue(at.Add(walReclaimHardCapSpacing-time.Nanosecond)))
	require.True(t, s.hardCapDue(at.Add(walReclaimHardCapSpacing)))
}
