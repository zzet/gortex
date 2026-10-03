package store_sqlite

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Edit cycles take the build lane for a few hundred milliseconds with gaps of
// a few hundred between them, while admitted writes take the log over its threshold
// but stay under the pressure mark. The reclaim loop's checkpoint work may run only in the
// gaps: no checkpoint starts inside a cycle, and one running when a cycle
// starts stops active work within the lane poll (20 ms) plus scheduling slack.
// Positively observed same-call VFS sync entered before the edit is reported
// separately from active work; unknown or ambiguous spans retain the wall bound.
// Raw overlap and late returns remain visible alongside the active-work share.
func TestWALReclaimUsesNoTimeInsideEditCycles(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "8")
	// Ceiling 64 MiB: the pressure mark (4 x the threshold) is 32 MiB, above
	// the log in this test, so every attempt waits for the gaps.
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB", "64")
	prevFloor := walReclaimCeilingFloor
	walReclaimCeilingFloor = 0
	t.Cleanup(func() { walReclaimCeilingFloor = prevFloor })
	setWALReclaimCadence(t, 25*time.Millisecond, 25*time.Millisecond, 200*time.Millisecond)

	type span = reclaimCheckpointSpan
	var mu sync.Mutex
	var calls []span
	var cycles []span
	walCheckpointCallObserver = func(_ string, start time.Time, took time.Duration) {
		mu.Lock()
		calls = append(calls, span{start, start.Add(took)})
		mu.Unlock()
	}
	t.Cleanup(func() { walCheckpointCallObserver = nil })
	// A paced pass that meets an edit waits in its page writes instead of
	// being interrupted (wal_copy_pause.go): its span may cover the edit, but
	// it writes nothing there. Those passes are judged by the pages they
	// wrote inside the edits, not by their span.
	var pausedPasses []span
	var checkpointSyncs []reclaimCheckpointSyncInterval
	walCopyPassObserver = func(start, end time.Time, pauses int) {
		if pauses > 0 {
			mu.Lock()
			pausedPasses = append(pausedPasses, span{start, end})
			mu.Unlock()
		}
	}
	t.Cleanup(func() { walCopyPassObserver = nil })

	diag := newWindowsWALDiagnostic(t)
	diag.install()
	defer diag.finish()
	s, path := openWALReclaimStore(t)
	diag.store.Store(s)
	// Register Close before lease acquisition so any failed setup still joins
	// active SQL. A successful lease cancels and joins the previous attempt,
	// then prevents another background attempt during observer/policy setup.
	var phaseProbeInstalled bool
	defer func() {
		if phaseProbeInstalled {
			reclaimCheckpointSyncProbeState.Store(nil)
		}
	}()
	defer func() { _ = s.Close() }()
	lease, leaseErr := s.acquireGenerationBulkCheckpointLease()
	require.NoError(t, leaseErr)
	defer s.releaseGenerationBulkCheckpointLease(lease)
	s.backgroundCheckpoint.mu.Lock()
	activeAtInstallation := s.backgroundCheckpoint.active
	s.backgroundCheckpoint.mu.Unlock()
	require.Nil(t, activeAtInstallation, "setup lease must join the prior background attempt")
	reclaimCheckpointSyncProbeState.Store(&reclaimCheckpointSyncProbe{store: s, observe: func(p reclaimCheckpointSyncInterval) {
		mu.Lock()
		checkpointSyncs = append(checkpointSyncs, p)
		mu.Unlock()
	}})
	phaseProbeInstalled = true
	seedWALChurnTable(t, s)
	growWAL(t, s, 12) // start over the threshold, below the pressure mark
	lane := &fakeBuildLane{}
	lane.install(s)
	require.True(t, s.releaseGenerationBulkCheckpointLease(lease))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the edit cycles
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(250 * time.Millisecond): // gap
			}
			start := time.Now()
			lane.held.Store(true)
			diag.record("edit-start", -1, nil)
			time.Sleep(400 * time.Millisecond)
			lane.held.Store(false)
			diag.record("edit-end", -1, nil)
			mu.Lock()
			cycles = append(cycles, span{start, time.Now()})
			mu.Unlock()
		}
	}()
	// Qualify the ordinary nonpressure policy: stop admitting new fixture
	// writes at 24 MiB until the store itself resets. This preserves headroom
	// below the 32 MiB pressure exception; it is not a sustained-throughput
	// workload (the separate pressure and bulk cases exercise that policy).
	// No checkpoint of the test's own supplies progress.
	initialResets := s.WALReclaimStats().Resets
	var committedWrites []span
	var writesAfterReset, admissionPauses int
	var admissionWait time.Duration
	var maxWAL int64
	var writeErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		for k := 0; ; k++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			// Read the reset counter first: a reset racing the size sample
			// must release this wait rather than require another reset.
			resets := s.WALReclaimStats().Resets
			size := walFileSize(path + "-wal")
			maxWAL = max(maxWAL, size)
			if size >= 24<<20 {
				admissionPauses++
				started := time.Now()
				for s.WALReclaimStats().Resets <= resets {
					maxWAL = max(maxWAL, walFileSize(path+"-wal"))
					select {
					case <-stop:
						admissionWait += time.Since(started)
						return
					case <-time.After(20 * time.Millisecond):
					}
				}
				admissionWait += time.Since(started)
			}
			wasReset := s.WALReclaimStats().Resets > initialResets
			started := time.Now()
			if err := churnWriteOnce(s, k); err != nil {
				mu.Lock()
				writeErr = err
				mu.Unlock()
				return
			}
			committedWrites = append(committedWrites, span{started, time.Now()})
			if wasReset {
				writesAfterReset++
			}
			if w := walFileSize(path + "-wal"); w > maxWAL {
				maxWAL = w
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(30 * time.Millisecond):
			}
			var n int
			_ = s.db.QueryRow(`SELECT count(*) FROM wal_churn WHERE id < 100`).Scan(&n)
		}
	}()
	time.Sleep(8 * time.Second)
	close(stop)
	wg.Wait()
	// Freeze progress before cleanup. Join the owned checkpoint loop before
	// matching calls: an unfinished concurrent call must not be invisible to
	// the ambiguity check, and shutdown must not supply observation resets.
	stats := s.WALReclaimStats()
	s.stopCheckpointLoop()
	s.backgroundCheckpoint.mu.Lock()
	activeAtClassification := s.backgroundCheckpoint.active
	s.backgroundCheckpoint.mu.Unlock()
	require.Nil(t, activeAtClassification, "classification requires all owned checkpoint calls joined")

	mu.Lock()
	defer mu.Unlock()
	require.NoError(t, writeErr)
	// Scheduling slack past the 20 ms lane poll; the race detector slows the
	// checkpoint's interrupt checks several-fold, so its bound is wider.
	slack := 60 * time.Millisecond
	if raceDetectorOn {
		slack = 250 * time.Millisecond
	}
	paused := func(k span) bool {
		for _, p := range pausedPasses {
			if !k.start.Before(p.start) && !k.end.After(p.end.Add(time.Millisecond)) {
				return true
			}
		}
		return false
	}
	// A same-TLS/Store span must belong wholly to exactly this call and overlap
	// no other call. Ambiguous, post-edit and unknown spans waive nothing.
	knownSync := func(callIndex int, editStart, end time.Time) time.Duration {
		return knownPreEditCheckpointSync(s, calls, callIndex, checkpointSyncs, editStart, end)
	}
	var inside, rawInside, allRawInside, cycleTime time.Duration
	startedInside, lateStops, rawLateStops := 0, 0, 0
	for _, c := range cycles {
		cycleTime += c.end.Sub(c.start)
		for callIndex, k := range calls {
			allLo, allHi := maxTime(c.start, k.start), minTime(c.end, k.end)
			if allHi.After(allLo) {
				allRawInside += allHi.Sub(allLo)
			}
			if paused(k) {
				continue
			}
			lo, hi := maxTime(c.start, k.start), minTime(c.end, k.end)
			if hi.After(lo) {
				rawInside += hi.Sub(lo)
				inside += max(time.Duration(0), hi.Sub(lo)-knownSync(callIndex, c.start, hi))
			}
			if k.start.After(c.start.Add(slack)) && k.start.Before(c.end) {
				startedInside++
			}
			if k.start.Before(c.start) && k.end.After(c.start.Add(walCheckpointCycleYieldPoll+slack)) {
				rawLateStops++
				activeStop := k.end.Sub(c.start) - knownSync(callIndex, c.start, k.end)
				if activeStop > walCheckpointCycleYieldPoll+slack {
					lateStops++
				}
			}
		}
	}
	writesInsideCycles := 0
	for _, w := range committedWrites {
		for _, c := range cycles {
			if !w.start.Before(c.start) && !w.end.After(c.end) {
				writesInsideCycles++
				break
			}
		}
	}
	t.Logf("nonpressure producer: commits=%d entirely_inside_cycles=%d commits_after_reset=%d actual_observation_resets=%d admission_pauses=%d admission_wait=%s high_water=24MiB", len(committedWrites), writesInsideCycles, writesAfterReset, stats.Resets-initialResets, admissionPauses, admissionWait)
	require.GreaterOrEqual(t, len(committedWrites), 12, "the nonpressure fixture must commit meaningful real writes")
	require.Positive(t, writesInsideCycles, "the fixture must include actual committed writes during edit cycles")
	require.Positive(t, stats.Resets-initialResets, "setup or shutdown must not supply the observation's reset progress")
	require.Positive(t, writesAfterReset, "real writes must resume after an authoritative reset")
	var worstForeground time.Duration
	for _, w := range committedWrites {
		worstForeground = max(worstForeground, w.end.Sub(w.start))
	}
	t.Logf("all_raw_checkpoint_overlap=%s original_nonpaused_raw_overlap=%s raw_late_stops=%d observed_sync_intervals=%d foreground_gate_SQL_max=%s", allRawInside, rawInside, rawLateStops, len(checkpointSyncs), worstForeground)
	require.LessOrEqual(t, worstForeground, 500*time.Millisecond, "actual foreground SQL exceeded existing mutation latency contract")
	share := float64(inside) / float64(max(cycleTime, 1))
	t.Logf("cycles=%d checkpoint_calls=%d time_inside_cycles=%s of %s (%.2f%%) started_inside=%d late_stops=%d resets=%d wal_max=%.1fMiB",
		len(cycles), len(calls), inside.Round(time.Millisecond), cycleTime.Round(time.Millisecond), 100*share,
		startedInside, lateStops, stats.Resets, float64(maxWAL)/(1<<20))
	require.GreaterOrEqual(t, len(cycles), 5)
	require.Greater(t, maxWAL, int64(8<<20), "precondition: the log passed its threshold during the cycles")
	require.Less(t, maxWAL, int64(32<<20), "precondition: the log stayed under the pressure mark")
	require.Positive(t, stats.Resets, "the reclaim must still reset the log in the gaps")
	require.Zero(t, s.WALCopyStats().WrittenWhileBusyBytes, "a paused pass wrote pages inside an edit cycle")
	require.Zero(t, startedInside, "a checkpoint started inside an edit cycle")
	require.Zero(t, lateStops, "a checkpoint ran on past the start of an edit cycle")
	maxShare := 0.05
	if raceDetectorOn {
		maxShare = 0.10
	}
	require.Less(t, share, maxShare, "the reclaim exceeded active-work share outside positively observed sync inside edit cycles")
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// A mutation that never takes the build lane (an undo flips the route without
// a build) still stands background work down from its announcement to its
// release; a leaked announcement stops doing so after the bound.
func TestAnnouncedMutationStandsBackgroundWorkDown(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	growWAL(t, s, 8)
	lane := &fakeBuildLane{} // installed, never held
	lane.install(s)
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ckpt.Close()
	cfg := walReclaimConfig{thresholdBytes: 1 << 20, drainDeadline: defaultWALReclaimDrainDeadline, truncateBudget: walReclaimTruncateBudget, readerWait: defaultWALReclaimReaderWait}

	release := s.AnnounceWrite()
	res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	if res.outcome != walReclaimSkipped || res.reason != "build_lane_busy" {
		t.Fatalf("reclaim during an announced undo: outcome=%s reason=%q, want skipped build_lane_busy", res.outcome, res.reason)
	}
	release()
	if res := s.reclaimWALOnce(cfg, ckpt, path+"-wal"); res.outcome != walReclaimReset {
		t.Fatalf("reclaim after the release: outcome=%s reason=%q, want reset", res.outcome, res.reason)
	}
}

// The leak guard: an announcement that is never released stops standing
// background work down after editIntentStandDownMax (2 min in production), and
// the WARN is logged once for the episode.
func TestLeakedAnnouncementStopsStandingDownAndWarnsOnce(t *testing.T) {
	if editIntentStandDownMax != 2*time.Minute {
		t.Fatalf("production bound = %s, want 2m", editIntentStandDownMax)
	}
	s, _ := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	lane := &fakeBuildLane{}
	lane.install(s)
	logs := captureReclaimLog(t)
	prev := editIntentStandDownMax
	editIntentStandDownMax = 50 * time.Millisecond
	t.Cleanup(func() { editIntentStandDownMax = prev })
	leaked := s.AnnounceWrite() // never released within the bound
	defer leaked()
	if !s.buildLaneBusy() {
		t.Fatal("a fresh announcement must stand background work down")
	}
	time.Sleep(100 * time.Millisecond)
	for i := 0; i < 3; i++ {
		if s.buildLaneBusy() {
			t.Fatal("a leaked announcement stood background work down past its bound")
		}
	}
	if n := strings.Count(logs.String(), "write announcements held for"); n != 1 {
		t.Fatalf("leak WARN logged %d times, want once per episode:\n%s", n, logs.String())
	}
}

// Continuous edits look like an announcement that is always active. The WAL
// hard cap (4x the ceiling) must still start the reclaim then, bounded by the
// writer-hold cap, or the log grows without end (22 GB and 57.6 GB were seen
// when background work could not run).
func TestHardCapStartsTheReclaimUnderAPermanentAnnouncement(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "4")
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB", "8")
	prevFloor, prevSpacing, prevLeak := walReclaimCeilingFloor, walReclaimHardCapSpacing, editIntentStandDownMax
	walReclaimCeilingFloor, walReclaimHardCapSpacing, editIntentStandDownMax = 0, 200*time.Millisecond, time.Hour
	t.Cleanup(func() {
		walReclaimCeilingFloor, walReclaimHardCapSpacing, editIntentStandDownMax = prevFloor, prevSpacing, prevLeak
	})
	setWALReclaimCadence(t, 50*time.Millisecond, 50*time.Millisecond, 400*time.Millisecond)
	logs := captureReclaimLog(t)
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	lane := &fakeBuildLane{} // installed, never held: only the announcement
	lane.install(s)
	release := s.AnnounceWrite()
	defer release()
	if !s.buildLaneBusy() {
		t.Fatal("precondition: the announcement stands background work down")
	}
	churn := startWALChurn(t, s, path, 2, 100*time.Millisecond, 200*time.Millisecond)
	churn.runUntil(t, 140, 120*time.Second)
	churn.halt(t)
	const hardCap = 4 * (8 << 20)
	st := s.WALReclaimStats()
	t.Logf("permanent announcement: wal_max=%.1fMiB hard_cap_runs=%d resets=%d refusals=%d writer_hold_max=%s",
		float64(churn.maxWAL.Load())/(1<<20), st.CycleCeilingRuns, st.Resets, st.CycleRefusals, st.WriterHoldMax)
	if st.CycleCeilingRuns == 0 || st.Resets == 0 {
		t.Fatalf("the hard cap never started the reclaim under a permanent announcement: %+v", st)
	}
	if churn.maxWAL.Load() > 2*hardCap {
		t.Fatalf("the WAL reached %.1f MiB, past twice its hard cap", float64(churn.maxWAL.Load())/(1<<20))
	}
	if st.WriterHoldMax > walReclaimMaxWriterHold+raceSlack(250*time.Millisecond) {
		t.Fatalf("a hard-cap run held the writer %s", st.WriterHoldMax)
	}
	if !strings.Contains(logs.String(), "reason=wal_hard_cap") {
		t.Fatal("the hard-cap run was not logged")
	}
}
