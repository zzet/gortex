package store_sqlite

import (
	"context"
	"errors"
	"os"
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

// Scaled: the fold's mark while editing is 4 MiB (production 256 MiB), the
// reclaim threshold 4 MiB and the pressure mark 16 MiB (production 256 MiB and
// 1 GiB), the reclaim polls every 25 ms (production 5 s). The log sits at 8
// MiB, between the marks, and the lane never goes idle. A refused step asks
// for the reclaim, the reclaim resets the log inside the busy lane, and the
// fold finishes.
func TestARefusedFoldGetsTheReclaimInsideABusyLane(t *testing.T) {
	prevMark := chainFoldWALMarkEditing
	chainFoldWALMarkEditing = 4 << 20
	t.Cleanup(func() { chainFoldWALMarkEditing = prevMark })
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

// The same with the daemon's defaults: no environment override, the
// production marks (256 MiB for the fold while editing, 256 MiB reclaim
// threshold, 1 GiB pressure mark), budget and 5 s poll. Only the lane and the
// log's size are the test's: a log of ~300 MiB under a lane held throughout.
func TestARefusedFoldGetsTheReclaimWithTheDaemonDefaults(t *testing.T) {
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
	require.Greater(t, size, int64(256<<20), "precondition: over the fold's default mark")
	require.Less(t, size, int64(1<<30), "precondition: under the default pressure mark")
	done, refusals := foldAgainstABusyLane(t, s, lane, 60*time.Second)
	t.Logf("fold done=%t refusals=%d pressure_resets=%d", done, refusals, s.WALCopyStats().PressureResets)
	require.True(t, done, "the fold never got under its mark with the daemon's defaults")
	require.Positive(t, refusals)
}

// The burst test with the daemon's defaults (threshold 256 MiB, pressure mark
// 1 GiB, 5 s poll, 512 MiB/min copy budget): a log over 1 GiB under a lane
// busy 70% of the time (0.7 s held, 0.3 s free) with ~7 MiB/s of writes (the
// 440 MB/min of measured edit bursts). The log is reset inside the busy lane and
// its size in frames stays under the mark plus one attempt. It writes more
// than 1 GiB, so it runs only with GORTEX_STORE_DEFAULTS_BURST=1: once per
// window, not in the ordinary suite.
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
