package store_sqlite

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// slowCopy makes the reclaim's copy take seconds on a test-sized log: the
// budget applies while editing, so a budget of a few MiB/s with a small bucket
// stretches a pass over the ~6 MiB of distinct pages the churn table dirties.
func slowCopy(t *testing.T, s *Store, bytesPerMinute int64) {
	t.Helper()
	setCopyBudget(t, bytesPerMinute, 256<<10, 0)
	s.walCopy.sawBusy(time.Now()) // an edit just happened: the budget applies
}

func interruptInsteadOfPause(t *testing.T) {
	t.Helper()
	walCopyInterruptInsteadOfPause = true
	t.Cleanup(func() { walCopyInterruptInsteadOfPause = false })
}

// runAttemptAcrossACycle runs one reclaim attempt and holds the lane for hold,
// starting after delay (while the attempt's first pass copies).
func runAttemptAcrossACycle(t *testing.T, s *Store, path string, lane *fakeBuildLane, delay, hold time.Duration) walReclaimResult {
	t.Helper()
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ckpt.Close() })
	cfg := walReclaimConfig{thresholdBytes: 1 << 20, drainDeadline: 250 * time.Millisecond, truncateBudget: walReclaimTruncateBudget, readerWait: time.Second}
	done := make(chan walReclaimResult, 1)
	go func() { done <- s.reclaimWALOnce(cfg, ckpt, path+"-wal") }()
	time.Sleep(delay)
	select {
	case res := <-done:
		t.Fatalf("precondition: the attempt finished before the cycle: %s", res.stampSuffix())
	default:
	}
	lane.held.Store(true)
	time.Sleep(hold)
	lane.held.Store(false)
	select {
	case res := <-done:
		return res
	case <-time.After(60 * time.Second):
		t.Fatal("the attempt did not finish after the cycle")
	}
	return walReclaimResult{}
}

// A pass that an edit interrupts in the middle keeps its progress: its page
// writes wait while the lane is held, no page is written inside the cycle,
// and when the cycle ends the pass completes and moves nBackfill.
func TestReclaimCopyPausesForAnEditAndKeepsItsProgress(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0") // drive attempts directly
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	if walCopyMethods.Load() == 0 {
		t.Skip("the copy pause is not installed in this process")
	}
	seedWALChurnTable(t, s)
	growWAL(t, s, 16)
	lane := &fakeBuildLane{}
	lane.install(s)
	slowCopy(t, s, 120<<20)
	before := s.WALCopyStats()

	res := runAttemptAcrossACycle(t, s, path, lane, 700*time.Millisecond, time.Second)
	after := s.WALCopyStats()
	t.Logf("attempt: outcome=%s reason=%q%s", res.outcome, res.reason, res.stampSuffix())
	require.NotNil(t, res.copy)
	require.Positive(t, res.copy.pauses, "the pass did not pause for the edit")
	require.GreaterOrEqual(t, res.copy.pauseNs, 800*time.Millisecond)
	require.False(t, res.passDiscarded, "a pass was cut short")
	require.Equal(t, before.DiscardedPasses, after.DiscardedPasses)
	require.Equal(t, before.PacedPassesCutShort, after.PacedPassesCutShort)
	require.Equal(t, before.WrittenWhileBusyBytes, after.WrittenWhileBusyBytes, "pages were copied inside the edit")
	require.True(t, res.framesKnown)
	require.True(t, res.progressed, "the paused pass did not move the backfill")
	if res.outcome != walReclaimReset {
		require.GreaterOrEqual(t, res.framesAfter.NBackfill, res.framesBefore.MxFrame, "backfilled_after must cover the log the pass started on")
	}
}

// An interrupted pass (the old behaviour, or shutdown) is recognised: SQLite
// recorded the attempt but not the backfill, and the attempt is stamped and
// counted as discarded.
func TestInterruptedReclaimPassIsDetectedAndCounted(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	growWAL(t, s, 16)
	lane := &fakeBuildLane{}
	lane.install(s)
	slowCopy(t, s, 120<<20)
	interruptInsteadOfPause(t)
	before := s.WALCopyStats()

	res := runAttemptAcrossACycle(t, s, path, lane, 700*time.Millisecond, 500*time.Millisecond)
	t.Logf("attempt: outcome=%s reason=%q%s", res.outcome, res.reason, res.stampSuffix())
	require.Equal(t, walReclaimSkipped, res.outcome)
	require.Equal(t, "build_lane_busy", res.reason)
	require.True(t, res.framesKnown)
	require.Greater(t, res.framesAfter.NBackfillAttempted, res.framesAfter.NBackfill, "precondition: SQLite recorded an attempt it did not complete")
	require.True(t, res.passDiscarded)
	require.Equal(t, before.DiscardedPasses+1, s.WALCopyStats().DiscardedPasses)
	require.Equal(t, before.PacedPassesCutShort+1, s.WALCopyStats().PacedPassesCutShort)
	require.Contains(t, res.stampSuffix(), "pass_discarded=true")
}

// The start guard: while the checkout is being edited, a pass starts only
// with budget in the bucket; idle, always.
func TestReclaimCopyStartsOnlyWithBudgetWhileEditing(t *testing.T) {
	s := openPayloadStore(t)
	setCopyBudget(t, 60<<20, 4<<20, 2<<20)
	now := time.Now()
	require.True(t, s.copyStartAllowed(now), "idle: a pass always starts")
	s.walCopy.sawBusy(now)
	require.True(t, s.copyStartAllowed(now), "a full bucket")
	rate := float64(walCopyBudget()) / 60
	require.Zero(t, s.walCopy.take(now, 3<<20, rate))
	require.False(t, s.copyStartAllowed(now), "1 MiB left of a 2 MiB minimum")
	require.True(t, s.copyStartAllowed(now.Add(2*time.Second)), "refilled at 1 MiB/s")
}

// A pause ends at the pressure mark: under an edit that does not end, a paused
// pass resumes once the log reaches the mark (and so long before the hard
// cap), so the one copy that would bring the log down is not held.
func TestReclaimCopyPauseEndsAtThePressureMark(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	if walCopyMethods.Load() == 0 {
		t.Skip("the copy pause is not installed in this process")
	}
	seedWALChurnTable(t, s)
	growWAL(t, s, 16)
	lane := &fakeBuildLane{}
	lane.install(s)
	slowCopy(t, s, 120<<20)
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer func() { _ = ckpt.Close() }()
	// Threshold 8 MiB, ceiling 64 MiB: the mark is 32 MiB, above the log the
	// attempt starts on.
	cfg := walReclaimConfig{thresholdBytes: 8 << 20, ceilingBytes: 64 << 20, drainDeadline: 250 * time.Millisecond, truncateBudget: walReclaimTruncateBudget, readerWait: time.Second}
	require.Less(t, walFileSize(path+"-wal"), int64(32<<20), "precondition: under the mark at the start")
	before := s.WALCopyStats()
	done := make(chan walReclaimResult, 1)
	go func() { done <- s.reclaimWALOnce(cfg, ckpt, path+"-wal") }()
	time.Sleep(700 * time.Millisecond)
	lane.held.Store(true) // an edit that never ends
	defer lane.held.Store(false)
	// Writes keep coming: the log passes the hard cap.
	for k := 0; walFileSize(path+"-wal") < int64(36<<20); k++ {
		require.NoError(t, churnWriteOnce(s, k))
	}
	var res walReclaimResult
	select {
	case res = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the paused pass did not resume at the hard cap")
	}
	t.Logf("attempt: outcome=%s reason=%q%s", res.outcome, res.reason, res.stampSuffix())
	require.Equal(t, before.PauseCapOverruns+1, s.WALCopyStats().PauseCapOverruns)
	require.Less(t, res.copy.pauseNs, walCopyPauseMax)
	require.Zero(t, s.WALCopyStats().PacedPassesCutShort-before.PacedPassesCutShort, "the pass completed")
}

// The hard-cap attempt is exempt from the start guard: with the lane held, the
// budget spent and the log over the hard cap, it still runs and resets.
func TestHardCapAttemptIgnoresTheCopyStartGuard(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	growWAL(t, s, 16)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)
	slowCopy(t, s, 60<<20)
	walCopyStartMinTokens.Store(16 << 20)
	s.walCopy.take(time.Now(), 1<<30, float64(walCopyBudget())/60) // the bucket is empty
	require.False(t, s.copyStartAllowed(time.Now()), "precondition: the guard refuses an ordinary pass")
	prevSpacing := walReclaimHardCapSpacing
	walReclaimHardCapSpacing = 0
	t.Cleanup(func() { walReclaimHardCapSpacing = prevSpacing })
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer func() { _ = ckpt.Close() }()
	// A ceiling of 1 MiB: the hard cap (4 MiB) is under the log.
	cfg := walReclaimConfig{thresholdBytes: 1 << 20, ceilingBytes: 1 << 20, drainDeadline: 250 * time.Millisecond, truncateBudget: walReclaimTruncateBudget, readerWait: time.Second}
	require.Greater(t, walFileSize(path+"-wal"), int64(4<<20), "precondition: over the hard cap")
	res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	t.Logf("hard-cap attempt: outcome=%s reason=%q%s", res.outcome, res.reason, res.stampSuffix())
	require.NotEqual(t, "copy_budget", res.reason, "the start guard held back the hard-cap attempt")
	require.Equal(t, walReclaimReset, res.outcome, "reason=%q", res.reason)
}

// The hard-cap attempt is exempt from the pause and the budget: its passes do
// not go through the pacer at all, so a held lane and a 1 MiB/s budget do not
// slow it.
func TestHardCapAttemptIsNeitherPausedNorPaced(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	growWAL(t, s, 16)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)
	slowCopy(t, s, 60<<20) // 1 MiB/s while editing: ~6 s for this log if paced
	prevSpacing := walReclaimHardCapSpacing
	walReclaimHardCapSpacing = 0
	t.Cleanup(func() { walReclaimHardCapSpacing = prevSpacing })
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer func() { _ = ckpt.Close() }()
	cfg := walReclaimConfig{thresholdBytes: 1 << 20, ceilingBytes: 1 << 20, drainDeadline: 250 * time.Millisecond, truncateBudget: walReclaimTruncateBudget, readerWait: time.Second}
	before := s.WALCopyStats()
	res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	t.Logf("hard-cap attempt: outcome=%s reason=%q%s", res.outcome, res.reason, res.stampSuffix())
	after := s.WALCopyStats()
	require.Equal(t, walReclaimReset, res.outcome, "reason=%q", res.reason)
	require.Equal(t, before.Passes, after.Passes, "a hard-cap pass went through the pacer")
	require.Equal(t, before.BudgetWait, after.BudgetWait)
	require.Less(t, res.elapsed, 3*time.Second, "the hard-cap attempt was slowed")
}

// An attempt begun before any lane was installed has no watcher to end a
// pause, so it never pauses: a lane held from later on (a daemon wires its
// predicate after Open) must not stall it, and with it the reclaim loop.
func TestAttemptBegunWithoutALaneNeverPauses(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	growWAL(t, s, 16)
	slowCopy(t, s, 60<<20) // were it paced: ~6 s, and paused under the lane
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer func() { _ = ckpt.Close() }()
	cfg := walReclaimConfig{thresholdBytes: 1 << 20, drainDeadline: 250 * time.Millisecond, truncateBudget: walReclaimTruncateBudget, readerWait: time.Second}
	done := make(chan walReclaimResult, 1)
	go func() { done <- s.reclaimWALOnce(cfg, ckpt, path+"-wal") }()
	time.Sleep(200 * time.Millisecond)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true) // held for the rest of the test
	defer lane.held.Store(false)
	select {
	case res := <-done:
		t.Logf("attempt: outcome=%s reason=%q%s", res.outcome, res.reason, res.stampSuffix())
		require.Zero(t, res.copy.pauses, "an attempt with no lane watcher paused")
	case <-time.After(15 * time.Second):
		t.Fatal("an attempt begun without a lane stalled under a lane held later")
	}
}

// pressureStore opens a store whose pressure mark is 16 MiB (threshold 4 MiB,
// ceiling 64 MiB, hard cap 256 MiB) with the reclaim loop running.
func pressureStore(t *testing.T, copyBytesPerMinute int64) (*Store, string, *fakeBuildLane) {
	t.Helper()
	if copyBytesPerMinute > 0 {
		// Before the store opens: its reclaim loop reads these.
		setCopyBudget(t, copyBytesPerMinute, 256<<10, 0)
	}
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "4")
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB", "64")
	setWALReclaimCadence(t, 25*time.Millisecond, 25*time.Millisecond, 200*time.Millisecond)
	s, path := openWALReclaimStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedWALChurnTable(t, s)
	lane := &fakeBuildLane{}
	lane.install(s)
	return s, path, lane
}

// The reset inside a busy lane holds the writer for at most its own cap: a
// slow reset gives up at the cap, and an edit admitted meanwhile waits no
// longer than that.
func TestPressureResetHoldsTheWriterAtMostItsCap(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0") // drive the step directly
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	lane := &fakeBuildLane{}
	lane.install(s)
	growWAL(t, s, 24)
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer func() { _ = ckpt.Close() }()
	_, _ = checkpointWALOnceOn(context.Background(), ckpt, "PASSIVE") // the copy is complete
	lane.held.Store(true)
	defer lane.held.Store(false)
	walPressureResetHook = func(ctx context.Context) { // a reset that would take 300 ms
		select {
		case <-ctx.Done():
		case <-time.After(300 * time.Millisecond):
		}
	}
	t.Cleanup(func() { walPressureResetHook = nil })
	var wait time.Duration
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(10 * time.Millisecond)
		release := s.AnnounceWrite()
		defer release()
		started := time.Now()
		s.writeMu.Lock()
		wait = time.Since(started)
		s.writeMu.Unlock()
	}()
	var res walReclaimResult
	err = s.reclaimWALPressureReset(context.Background(), ckpt, &res)
	wg.Wait()
	t.Logf("slow pressure reset: err=%v reason=%q writer_hold=%s edit_wait=%s", err, res.reason, res.writerHold, wait)
	require.ErrorIs(t, err, errWALPressureHold)
	limit := walReclaimPressureHold + testTimingSlack(20*time.Millisecond)
	require.GreaterOrEqual(t, res.writerHold, walReclaimPressureHold-5*time.Millisecond, "precondition: the slow reset held the writer to the cap")
	require.Less(t, res.writerHold, limit)
	require.Greater(t, wait, time.Duration(0))
	require.Less(t, wait, limit, "the edit waited longer than the pressure hold")
	require.Equal(t, int64(1), s.WALCopyStats().PressureGiveUps)
	require.Zero(t, s.WALCopyStats().PressureLaneResets)
	_ = path
}

// Below the pressure mark a held lane still refuses the reclaim, as in the
// samples of a normal run.
func TestBelowThePressureMarkAHeldLaneRefusesTheReclaim(t *testing.T) {
	// Drive this branch directly. The autonomous poller must not race the
	// precondition, and the seed table/schema WAL is not part of the edit log.
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB", "64")
	s, path := openWALReclaimStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedWALChurnTable(t, s)
	require.NoError(t, s.CheckpointWAL())
	growWAL(t, s, 6)
	lane := &fakeBuildLane{}
	lane.install(s)
	// Open captured disabled reclaim; changing this env only constructs the
	// explicit attempt's production configuration, without starting a poller.
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "4")
	cfg := resolveWALReclaimConfig()
	size := walFileSize(path + "-wal")
	require.Greater(t, size, cfg.thresholdBytes, "precondition: above ordinary reclaim threshold")
	require.Less(t, size, int64(16<<20), "precondition: under the mark")
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer func() { _ = ckpt.Close() }()
	lane.held.Store(true)
	defer lane.held.Store(false)
	before := s.WALCopyStats()
	res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	require.Equal(t, walReclaimSkipped, res.outcome)
	require.Equal(t, "build_lane_busy", res.reason)
	require.Equal(t, before.PressureRuns, s.WALCopyStats().PressureRuns)
}

// setCopyBudget sets the copy budget for one test and restores it after.
func setCopyBudget(t *testing.T, perMinute, burst, startMin int64) {
	t.Helper()
	prevRate, prevBurst, prevMin := walCopyBudgetBytesPerMinute.Load(), walCopyBudgetBurst.Load(), walCopyStartMinTokens.Load()
	walCopyBudgetBytesPerMinute.Store(perMinute)
	walCopyBudgetBurst.Store(burst)
	walCopyStartMinTokens.Store(startMin)
	t.Cleanup(func() {
		walCopyBudgetBytesPerMinute.Store(prevRate)
		walCopyBudgetBurst.Store(prevBurst)
		walCopyStartMinTokens.Store(prevMin)
	})
}
