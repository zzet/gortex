package store_sqlite

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// foldAgainstABusyLane begins a fold over a small chain on s and steps it
// while the lane stays busy (an edit burst that never pauses), with the log
// held between the fold's mark and the pressure mark by churn writes. It
// reports whether the fold finished within limit, and its refusals.
// The limit is a stall limit, not a deadline: it restarts whenever the fold
// commits a step or the reclaim resets the log, so a slow host only makes the
// test longer.
func foldAgainstABusyLane(t *testing.T, s *Store, lane *fakeBuildLane, limit time.Duration) (done bool, refusals int) {
	t.Helper()
	return stepFoldAgainstABusyLane(t, s, lane, beginTestFold(t, s), limit)
}

// beginTestFold begins a fold over a small chain. Built before a test grows
// the log to where it wants it: the chain's own writes commit, and a commit
// may let the writer's auto-checkpoint restart a log nothing pins.
func beginTestFold(t *testing.T, s *Store) *ChainFold {
	t.Helper()
	ctx := context.Background()
	chain := foldChain(t, s, 200)
	to := reservedGeneration(t, s, "folded")
	fold, err := s.BeginChainFold(ctx, ChainFoldRequest{Chain: chain, To: to, Owner: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = fold.Release(ctx) })
	return fold
}

// stepFoldAgainstABusyLane steps fold with the lane held; see
// foldAgainstABusyLane.
func stepFoldAgainstABusyLane(t *testing.T, s *Store, lane *fakeBuildLane, fold *ChainFold, limit time.Duration) (done bool, refusals int) {
	t.Helper()
	ctx := context.Background()
	lane.held.Store(true)
	defer lane.held.Store(false)
	deadline := time.Now().Add(limit)
	lastResets := s.WALReclaimStats().Resets
	for time.Now().Before(deadline) {
		if r := s.WALReclaimStats().Resets; r != lastResets {
			lastResets, deadline = r, time.Now().Add(limit)
		}
		finished, err := fold.Step(ctx)
		switch {
		case errors.Is(err, ErrChainFoldWALMark):
			refusals++
			time.Sleep(20 * time.Millisecond)
			continue
		case errors.Is(err, ErrChainFoldYielded):
			time.Sleep(time.Millisecond)
			continue
		}
		require.NoError(t, err)
		deadline = time.Now().Add(limit) // a committed step is progress
		if finished {
			return true, refusals
		}
	}
	return false, refusals
}

// Scaled: the fold's mark is 4 MiB (production 1 GiB), the reclaim threshold
// 4 MiB and the pressure mark 16 MiB (production 256 MiB and 1 GiB), the
// reclaim polls every 25 ms (production 5 s). The log sits at 8 MiB, over the
// fold's mark, and the lane never goes idle. A refused step asks for the
// reclaim, the reclaim resets the log inside the busy lane, and the fold
// finishes.
func TestARefusedFoldGetsTheReclaimInsideABusyLane(t *testing.T) {
	prevMark := chainFoldWALMark
	chainFoldWALMark = 4 << 20
	t.Cleanup(func() { chainFoldWALMark = prevMark })
	s, path, lane := pressureStore(t, 0)
	fold := beginTestFold(t, s)
	lane.held.Store(true) // an edit burst from here on: nothing reclaims the log in a gap
	// The log as the fold measures it (frames, not the file's size: a log
	// restarted after a complete copy keeps its file). An attempt the loop
	// began before the lane was held may still reset it meanwhile, so grow
	// until it is between the marks.
	logBytes := func() int64 {
		m := s.WALWriteMark()
		return int64(m.MxFrame) * (int64(m.PageSize) + walFrameHeaderBytes)
	}
	for k := 0; logBytes() <= 8<<20 && k < 64; k++ {
		require.NoError(t, churnWriteOnce(s, k))
	}
	_ = path
	size := logBytes()
	require.Greater(t, size, int64(4<<20), "precondition: over the fold's mark")
	require.Less(t, size, int64(16<<20), "precondition: under the pressure mark")
	before := s.WALCopyStats()
	done, refusals := stepFoldAgainstABusyLane(t, s, lane, fold, 20*time.Second)
	after := s.WALCopyStats()
	t.Logf("fold done=%t refusals=%d pressure_runs=%d pressure_resets=%d", done, refusals, after.PressureRuns-before.PressureRuns, after.PressureResets-before.PressureResets)
	require.True(t, done, "the fold never got under its mark inside the busy lane")
	require.Positive(t, refusals, "precondition: the fold was refused at first")
	require.Positive(t, after.PressureResets-before.PressureResets)
}

// A pass that began in a gap between edits and was paused by the next edit
// holds the loop inside its attempt. A writer refused on the log's size then
// asks for the reclaim: the pass stops pausing at the request, its attempt
// ends, and the loop runs the pressure attempt that resets the log inside the
// busy lane. Without that the request waits for the pause cap (3 min).
// Scaled as the test above; the log sits between the fold's mark and the
// pressure mark, where only a request lets the reclaim through the lane.
func TestARequestEndsAPausedCopy(t *testing.T) {
	prevMark := chainFoldWALMark
	chainFoldWALMark = 4 << 20
	t.Cleanup(func() { chainFoldWALMark = prevMark })
	s, _, lane := pressureStore(t, 0)
	fold := beginTestFold(t, s)
	logBytes := func() int64 {
		m := s.WALWriteMark()
		return int64(m.MxFrame) * (int64(m.PageSize) + walFrameHeaderBytes)
	}
	// Catch a pass mid-copy: grow the log with the lane held (nothing
	// reclaims it), free the lane until the loop's next pass has begun, and
	// hold it again. A pass that finished first (the log reset) or had not
	// begun is tried again.
	caught := false
	for try := 0; try < 50 && !caught; try++ {
		lane.held.Store(true)
		for k := 0; logBytes() <= 12<<20 && k < 256; k++ {
			require.NoError(t, churnWriteOnce(s, k))
		}
		require.Less(t, logBytes(), int64(16<<20), "precondition: under the pressure mark")
		lane.held.Store(false)
		for wait := time.Now().Add(2 * time.Second); time.Now().Before(wait) && walCopyPacersActive.Load() == 0; {
			time.Sleep(20 * time.Microsecond)
		}
		lane.held.Store(true)
		time.Sleep(100 * time.Millisecond)
		caught = walCopyPacersActive.Load() > 0 && logBytes() > 4<<20
	}
	require.True(t, caught, "precondition: a pass paused by the lane")
	before := s.WALCopyStats()
	done, refusals := stepFoldAgainstABusyLane(t, s, lane, fold, 20*time.Second)
	after := s.WALCopyStats()
	t.Logf("fold done=%t refusals=%d request_resumed_passes=%d pressure_runs=%d pressure_resets=%d",
		done, refusals, after.RequestResumedPasses-before.RequestResumedPasses, after.PressureRuns-before.PressureRuns, after.PressureResets-before.PressureResets)
	require.True(t, done, "the fold waited on a paused copy")
	require.Positive(t, refusals, "precondition: the fold was refused at first")
	require.Positive(t, after.RequestResumedPasses-before.RequestResumedPasses, "the paused pass did not end at the request")
	require.Positive(t, after.PressureResets-before.PressureResets)
}

// A yielding attempt that began before the daemon installed its lane predicate
// must observe a later busy predicate and finish. A fold refused on the WAL
// mark can then request a pressure reset inside the still-busy lane. Installing
// the predicate at a real PASSIVE completion pins the same active attempt;
// its cancellation and completion are observed directly, not inferred from
// additional checkpoints continuing inside the edit. Scaled as above.
func TestARequestRunsAfterALatePredicateCancelsAnAttempt(t *testing.T) {
	prevMark := chainFoldWALMark
	chainFoldWALMark = 4 << 20
	t.Cleanup(func() { chainFoldWALMark = prevMark })
	var armed atomic.Bool
	var store atomic.Pointer[Store]
	var laneRef atomic.Pointer[fakeBuildLane]
	type capture struct {
		attempt  *backgroundCheckpointAttempt
		pausable bool
	}
	captured := make(chan capture, 1)
	stopObserver := make(chan struct{})
	prevObserver := walCheckpointCallObserver
	walCheckpointCallObserver = func(mode string, _ time.Time, _ time.Duration) {
		if mode != "PASSIVE" || !armed.CompareAndSwap(true, false) {
			return
		}
		st, l := store.Load(), laneRef.Load()
		if st == nil || l == nil {
			captured <- capture{}
			return
		}
		st.backgroundCheckpoint.mu.Lock()
		attempt := st.backgroundCheckpoint.active
		st.backgroundCheckpoint.mu.Unlock()
		pausable := attempt != nil && attempt.copy != nil && attempt.copy.pausable.Load()
		l.install(st)
		if attempt != nil && !pausable {
			// Keep this real completion callback alive until the dynamic
			// watcher cancels the attempt, so normal attempt cleanup cannot
			// replace its cancellation cause before it is witnessed.
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			select {
			case <-attempt.ctx.Done():
			case <-timer.C:
			case <-stopObserver:
			}
		}
		captured <- capture{attempt: attempt, pausable: pausable}
	}
	t.Cleanup(func() { walCheckpointCallObserver = prevObserver })
	s, _, lane := pressureStore(t, 0)
	// Unblock the callback before pressureStore's joined Close cleanup on
	// every assertion failure. Hook restoration follows Store closure.
	t.Cleanup(func() { close(stopObserver) })
	store.Store(s)
	laneRef.Store(lane)
	fold := beginTestFold(t, s)
	// Join any older attempt before removing the predicate. The setup lease
	// prevents a pass from starting or resetting the seeded WAL until armed.
	lease, err := s.acquireGenerationBulkCheckpointLease()
	require.NoError(t, err)
	t.Cleanup(func() { s.releaseGenerationBulkCheckpointLease(lease) })
	s.SetBuildLaneBusy(nil)
	logBytes := func() int64 {
		m := s.WALWriteMark()
		return int64(m.MxFrame) * (int64(m.PageSize) + walFrameHeaderBytes)
	}
	lane.held.Store(true)
	for k := 0; logBytes() <= 8<<20 && k < 256; k++ {
		require.NoError(t, churnWriteOnce(s, k))
	}
	require.Greater(t, logBytes(), int64(4<<20), "precondition: over the fold's mark")
	require.Less(t, logBytes(), int64(16<<20), "precondition: under the pressure mark")
	armed.Store(true)
	require.True(t, s.releaseGenerationBulkCheckpointLease(lease))
	var seen capture
	select {
	case seen = <-captured:
	case <-time.After(10 * time.Second):
		t.Fatal("no real PASSIVE completed before late predicate installation")
	}
	attempt := seen.attempt
	require.False(t, seen.pausable, "precondition: the captured pass began before predicate installation")
	require.NotNil(t, attempt, "precondition: PASSIVE belonged to an active attempt")
	require.ErrorIs(t, context.Cause(attempt.ctx), errWALCheckpointYieldedToCycle,
		"the late predicate must cancel the active yielding attempt")
	select {
	case <-attempt.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled checkpoint attempt did not finish")
	}
	require.True(t, lane.held.Load(), "the lane stays busy through the request")
	require.Greater(t, logBytes(), int64(4<<20), "precondition: refused fold still needs reclaim")
	require.Less(t, logBytes(), int64(16<<20), "precondition: request, not pressure size, grants admission")
	before := s.WALCopyStats()
	done, refusals := stepFoldAgainstABusyLane(t, s, lane, fold, 20*time.Second)
	after := s.WALCopyStats()
	t.Logf("late_attempt_cancelled=true fold done=%t refusals=%d pressure_runs=%d pressure_resets=%d",
		done, refusals, after.PressureRuns-before.PressureRuns, after.PressureResets-before.PressureResets)
	require.True(t, done, "the fold waited after the late-predicate attempt ended")
	require.Positive(t, refusals, "precondition: the fold was refused at first")
	require.Positive(t, after.PressureResets-before.PressureResets)
}

// With the daemon's defaults (no environment override: the fold's mark 1 GiB,
// the reclaim threshold 256 MiB, the pressure mark 1 GiB, the budget and the
// 5 s poll), a fold steps through a busy lane with the log between the
// reclaim threshold and the fold's mark: ~300 MiB, which a burst's own writes
// reach. It is never refused and finishes. (Under the former rule it was
// refused over 256 MiB for as long as the edits went on.)
func TestAFoldStepsBetweenTheThresholdAndItsMarkWithTheDaemonDefaults(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a 300 MiB log")
	}
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true) // hold the lane while the log grows: nothing reclaims it
	growWAL(t, s, 300)
	size := walFileSize(path + "-wal")
	require.Greater(t, size, int64(256<<20), "precondition: over the reclaim threshold")
	require.Less(t, size, chainFoldWALMark, "precondition: under the fold's mark")
	done, refusals := foldAgainstABusyLane(t, s, lane, 60*time.Second)
	t.Logf("fold done=%t refusals=%d log=%.0fMiB", done, refusals, float64(size)/(1<<20))
	require.True(t, done, "the fold did not finish between the threshold and its mark")
	require.Zero(t, refusals, "a step was refused under the fold's mark")
}

// Over the fold's mark with the daemon's defaults: the fold's first refusal
// asks for the reclaim, and the time from that request to the reset is
// bounded, although edits keep writing (a writer adds ~7 MiB/s under a lane
// busy throughout). The loop wakes at the request (not at its next poll), the
// pressure step queues for the writer (up to walReclaimPressureWriterWait),
// and while the log is over the fold's mark the copy is not paced by the
// budget. It writes more than 1 GiB of log, so it runs only with
// GORTEX_STORE_DEFAULTS_FOLD_MARK=1.
func TestARefusedFoldGetsItsResetInBoundedTimeWithTheDaemonDefaults(t *testing.T) {
	if os.Getenv("GORTEX_STORE_DEFAULTS_FOLD_MARK") != "1" {
		t.Skip("set GORTEX_STORE_DEFAULTS_FOLD_MARK=1 (writes a >1 GiB log)")
	}
	s, _ := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)
	logBytes := func() int64 {
		m := s.WALWriteMark()
		return int64(m.MxFrame) * (int64(m.PageSize) + walFrameHeaderBytes)
	}
	for k := 0; logBytes() <= chainFoldWALMark+(64<<20); k++ {
		require.NoError(t, churnWriteOnce(s, k))
	}
	ctx := context.Background()
	chain := foldChain(t, s, 200)
	to := reservedGeneration(t, s, "folded")
	fold, err := s.BeginChainFold(ctx, ChainFoldRequest{Chain: chain, To: to, Owner: "test"})
	require.NoError(t, err)
	defer func() { _ = fold.Release(ctx) }()

	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() { // the edits' writes: ~1 MiB every 150 ms
		defer close(writerDone)
		for k := 0; ; k++ {
			select {
			case <-stop:
				return
			case <-time.After(150 * time.Millisecond):
			}
			_ = churnWriteOnce(s, k)
		}
	}()
	resets := s.WALReclaimStats().Resets
	var requested time.Time
	var reset time.Duration
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		_, err := fold.Step(ctx)
		if errors.Is(err, ErrChainFoldWALMark) {
			if requested.IsZero() {
				requested = time.Now()
			}
		} else if err != nil && !errors.Is(err, ErrChainFoldYielded) {
			require.NoError(t, err)
		}
		if !requested.IsZero() && s.WALReclaimStats().Resets > resets {
			reset = time.Since(requested)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(stop)
	<-writerDone
	cp := s.WALCopyStats()
	t.Logf("fold refused at log=%.0fMiB; reset %s after the request; pressure_runs=%d lane_resets=%d give_ups=%d",
		float64(chainFoldWALMark+(64<<20))/(1<<20), reset.Round(time.Millisecond), cp.PressureRuns, cp.PressureLaneResets, cp.PressureGiveUps)
	require.False(t, requested.IsZero(), "precondition: the fold was refused over its mark")
	require.NotZero(t, reset, "no reset within 5 minutes of the request")
	require.Less(t, reset, 60*time.Second, "the reset came later than the bound")
}

// The burst test with the daemon's defaults (threshold 256 MiB, pressure mark
// 1 GiB, 5 s poll, 512 MiB/min copy budget): a log over 1 GiB under a lane
// busy 70% of the time (0.7 s held, 0.3 s free) with ~7 MiB/s of writes (the
// 440 MB/min of measured edit bursts). The log is reset inside the busy lane and
// its size in frames stays under the mark plus one attempt. It writes more
// than 1 GiB, so it runs only with GORTEX_STORE_DEFAULTS_BURST=1: once per
// measurement run, not in the ordinary suite.
func TestReclaimResetsDuringABurstWithTheDaemonDefaults(t *testing.T) {
	if os.Getenv("GORTEX_STORE_DEFAULTS_BURST") != "1" {
		t.Skip("set GORTEX_STORE_DEFAULTS_BURST=1 (writes a >1 GiB log)")
	}
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)
	for k := 0; walFileSize(path+"-wal") < 1100<<20; k++ {
		require.NoError(t, churnWriteOnce(s, k))
	}
	logBytes := func() int64 {
		m := s.WALWriteMark()
		return int64(m.MxFrame) * (int64(m.PageSize) + walFrameHeaderBytes)
	}
	require.Greater(t, logBytes(), int64(1<<30), "precondition: over the default pressure mark")
	stop := make(chan struct{})
	done := make(chan struct{})
	var maxAfterReset int64
	resetsBefore := s.WALReclaimStats().Resets
	go func() {
		defer close(done)
		for k := 0; ; k++ {
			select {
			case <-stop:
				return
			case <-time.After(150 * time.Millisecond):
			}
			held := (k % 7) < 5 // ~70% of the time
			lane.held.Store(held)
			_ = churnWriteOnce(s, k)
			if s.WALReclaimStats().Resets > resetsBefore {
				if b := logBytes(); b > maxAfterReset {
					maxAfterReset = b
				}
			}
		}
	}()
	time.Sleep(90 * time.Second)
	close(stop)
	<-done
	lane.held.Store(false)
	cp := s.WALCopyStats()
	t.Logf("defaults burst: pressure_runs=%d pressure_resets=%d (inside the lane %d) give_ups=%d log_max_after_first_reset=%.0fMiB",
		cp.PressureRuns, cp.PressureResets, cp.PressureLaneResets, cp.PressureGiveUps, float64(maxAfterReset)/(1<<20))
	// One reset inside the burst brings the log under the mark; from there
	// the ordinary reclaim keeps it down in the gaps.
	require.Positive(t, cp.PressureRuns, "no attempt ran through the busy lane over the mark")
	require.Greater(t, s.WALReclaimStats().Resets, resetsBefore, "the log was not reset during the burst")
	require.Less(t, maxAfterReset, int64(1536<<20), "the log outgrew the mark plus one attempt")
}
