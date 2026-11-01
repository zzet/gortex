package store_sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWALReclaimUrgentResetYieldsToEditBurst(t *testing.T) {
	s, db := finalBackfillFixture(t)
	release := s.AnnounceWrite()
	defer release()
	before := s.WALReclaimStats().WriterHold.Count()
	res := &walReclaimResult{urgent: true}
	done, err := s.reclaimWALResetHold(t.Context(), walReclaimConfig{}, db, res, true, false)
	require.ErrorIs(t, err, errWALReclaimWriterWaiting)
	require.False(t, done)
	require.Equal(t, before, s.WALReclaimStats().WriterHold.Count(), "urgent reclaim acquired the writer despite an announced edit")
	release()
	done, err = s.reclaimWALResetHold(t.Context(), walReclaimConfig{}, db, &walReclaimResult{urgent: true}, true, false)
	require.NoError(t, err)
	require.True(t, done, "the same owned WAL must reset after writer demand ends")
	require.True(t, walLogIsReset(s.dbPath, s.dbPath+"-wal"))
}

func TestWALReclaimUrgentRefusesQueuedWriter(t *testing.T) {
	s, db := finalBackfillFixture(t)
	s.writeMu.Lock()
	writerDone := make(chan struct{})
	go func() { s.writeMu.Lock(); s.writeMu.Unlock(); close(writerDone) }()
	require.Eventually(t, func() bool { return s.writeMu.waiting() > 0 }, time.Second, time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := s.reclaimWALResetHoldOnce(ctx, walReclaimConfig{}, db, &walReclaimResult{urgent: true}, true, false, walReclaimResetHold)
	s.writeMu.Unlock()
	<-writerDone
	require.ErrorIs(t, err, errWALReclaimWriterWaiting, "urgent reclaim must refuse queued edits before writer admission")
}

func TestWALReclaimPressureDrainDefersDuringBuild(t *testing.T) {
	s, db := finalBackfillFixture(t)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)
	defer lane.held.Store(false)
	res := &walReclaimResult{urgent: true, pressure: true}
	err := s.reclaimWALPressureResetAfterReaderDrain(t.Context(), db, res, 100*time.Millisecond)
	require.Error(t, err, "pressure reclaim must defer before closing the read gate")
	require.False(t, res.pauseClosed, "pressure reclaim closed the read gate during a build")
	require.Zero(t, res.writerHolds)
}

func TestWALReclaimInteractiveReadBlocksAllCompletionClasses(t *testing.T) {
	for _, class := range []string{"ordinary", "bulk", "hard_cap"} {
		t.Run(class, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			release := s.AnnounceInteractiveRead()
			defer release()
			res := &walReclaimResult{urgent: true, bulkCompletion: class == "bulk", lastResort: class == "hard_cap"}
			before := s.WALReclaimStats().WriterHold.Count()
			_, err := s.reclaimWALResetHoldOnce(t.Context(), walReclaimConfig{}, db, res, true, false, walReclaimResetHold)
			require.ErrorIs(t, err, errWALReclaimInteractiveRead)
			require.Equal(t, before, s.WALReclaimStats().WriterHold.Count())
			_, _, err = s.readGate.quiesceReclaim(t.Context(), time.Now().Add(time.Second), s, res)
			require.ErrorIs(t, err, errWALReclaimInteractiveRead)
			require.False(t, s.readGate.closed.Load())
			release()
			release()
			require.Zero(t, s.walReclaim.interactive.active.Load())
			done, err := s.reclaimWALResetHold(t.Context(), walReclaimConfig{}, db, &walReclaimResult{urgent: true}, true, false)
			require.NoError(t, err)
			require.True(t, done)
		})
	}
}

func TestWALReclaimInteractiveReadWinsQueuedAdmission(t *testing.T) {
	s, _ := finalBackfillFixture(t)
	s.writeMu.Lock()
	res := &walReclaimResult{urgent: true, lastResort: true}
	done := make(chan error, 1)
	go func() { done <- s.lockWALReclaimWriter(t.Context(), res) }()
	require.Eventually(t, func() bool { return s.writeMu.reclaimWaiters.Load() > 0 }, time.Second, time.Millisecond)
	require.Nil(t, s.writeDemandCause(), "a reclaim must not preempt itself as a queued foreground writer")
	release := s.AnnounceInteractiveRead()
	defer release()
	before := s.WALReclaimStats().WriterHold.Count()
	s.writeMu.Unlock()
	require.ErrorIs(t, <-done, errWALReclaimInteractiveRead)
	require.Equal(t, before, s.WALReclaimStats().WriterHold.Count(), "a marker arriving after the early check must prevent reclaim admission")
}

func TestWALReclaimInteractiveReadWinsReadGateAdmission(t *testing.T) {
	s, _ := finalBackfillFixture(t)
	s.readGate.mu.Lock()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		reopen, _, err := s.readGate.quiesceReclaim(t.Context(), time.Now().Add(time.Second), s, &walReclaimResult{lastResort: true})
		if reopen != nil {
			reopen()
		}
		done <- err
	}()
	<-started
	release := s.AnnounceInteractiveRead()
	defer release()
	s.readGate.mu.Unlock()
	require.ErrorIs(t, <-done, errWALReclaimInteractiveRead)
	require.False(t, s.readGate.closed.Load())
}

func TestWALReclaimBurstExceptionsStillReset(t *testing.T) {
	for _, kind := range []string{"bulk", "hard_cap"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			lane := &fakeBuildLane{}
			lane.install(s)
			lane.held.Store(true)
			defer lane.held.Store(false)
			res := &walReclaimResult{urgent: true, bulkCompletion: kind == "bulk", lastResort: kind == "hard_cap"}
			cfg := walReclaimConfig{readerWait: time.Second, drainDeadline: time.Second, truncateBudget: walReclaimTruncateBudget}
			require.NoError(t, s.reclaimWALInLane(t.Context(), cfg, db, res))
			require.True(t, walLogIsReset(s.dbPath, s.dbPath+"-wal"))
			stats := s.WALReclaimStats()
			require.Positive(t, stats.EditBurstWriterHold.Count())
			require.Equal(t, stats.WriterHold.Count(), stats.EditBurstWriterHold.Count(), "one existing hold observation must feed both populations")
			if kind == "bulk" {
				require.Positive(t, stats.BulkExceptionHolds)
				require.Zero(t, stats.HardCapExceptionHolds)
			} else {
				require.Positive(t, stats.HardCapExceptionHolds)
				require.Zero(t, stats.BulkExceptionHolds)
			}
		})
	}
}

func TestWALReclaimBurstRefusesAdaptiveWriterCredit(t *testing.T) {
	s, db := finalBackfillFixture(t)
	frontier, ok := readWALReclaimFrontier(s.dbPath)
	require.True(t, ok)
	release := s.AnnounceWrite()
	defer release()
	res := &walReclaimResult{urgent: true, slowTail: &walReclaimSlowTail{salt: frontier.salt, covered: frontier.backfill, copyElapsed: time.Second}}
	require.Positive(t, res.takeAdaptiveWriterBudget(), "the WAL must otherwise qualify for adaptive completion")
	_, err := s.reclaimWALResetHoldOnce(t.Context(), walReclaimConfig{}, db, res, true, false, res.takeAdaptiveWriterBudget())
	require.ErrorIs(t, err, errWALReclaimWriterWaiting)
	require.False(t, res.adaptiveUsed)
	require.Zero(t, s.WALReclaimStats().WriterHold.Count())
}

func TestWALReclaimBusyLaneStillAllowsShortOpenGateReset(t *testing.T) {
	s, db := finalBackfillFixture(t)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)
	defer lane.held.Store(false)
	res := &walReclaimResult{urgent: true, pressure: true}
	require.NoError(t, s.reclaimWALPressureResetOnce(t.Context(), db, res, walReclaimResetHold, false))
	require.True(t, walLogIsReset(s.dbPath, s.dbPath+"-wal"))
	stats := s.WALReclaimStats()
	require.Positive(t, stats.EditBurstWriterHold.Count(), "the real short slice must populate burst observations")
	require.LessOrEqual(t, stats.EditBurstWriterHold.Max, walReclaimResetHold)
	require.Zero(t, stats.EditBurstGateClosures)
}

func TestWALReclaimBuildLaneRefusesAdaptiveCompletion(t *testing.T) {
	s, db := finalBackfillFixture(t)
	frontier, ok := readWALReclaimFrontier(s.dbPath)
	require.True(t, ok)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)
	defer lane.held.Store(false)
	res := &walReclaimResult{urgent: true, slowTail: &walReclaimSlowTail{salt: frontier.salt, covered: frontier.backfill, copyElapsed: time.Second}}
	budget := res.takeAdaptiveWriterBudget()
	require.Greater(t, budget, walReclaimResetHold)
	_, err := s.reclaimWALResetHoldOnce(t.Context(), walReclaimConfig{}, db, res, true, false, budget)
	require.ErrorIs(t, err, errWALReclaimEditBurst)
	require.Zero(t, s.WALReclaimStats().WriterHold.Count())
	require.False(t, res.adaptiveUsed)
}

func TestWALReclaimBurstDeferralsRetainActualCounters(t *testing.T) {
	for _, kind := range []string{"edit", "interactive"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			var release func()
			if kind == "edit" {
				release = s.AnnounceWrite()
			} else {
				release = s.AnnounceInteractiveRead()
			}
			cfg := walReclaimConfig{thresholdBytes: 1, drainDeadline: time.Second, truncateBudget: walReclaimTruncateBudget}
			res := s.reclaimWALOnce(cfg, db, s.dbPath+"-wal")
			release()
			require.NotEqual(t, walReclaimReset, res.outcome)
			stats := s.WALReclaimStats()
			if kind == "edit" {
				require.EqualValues(t, 1, stats.DeferredEditBurst)
				require.Zero(t, stats.DeferredInteractiveRead)
			} else {
				require.EqualValues(t, 1, stats.DeferredInteractiveRead)
				require.Zero(t, stats.DeferredEditBurst)
			}
			require.Zero(t, stats.WriterHold.Count())
			require.Zero(t, stats.EditBurstGateClosures)
			require.Zero(t, stats.PauseCount)
		})
	}
}

func TestWALReclaimShrinkDefersWithoutLosingMandatoryCleanup(t *testing.T) {
	s, db := finalBackfillFixture(t)
	s.walShrinkPending.Store(true)
	release := s.AnnounceInteractiveRead()
	_, err := s.shrinkWALSliceOnce(t.Context(), db, walShrinkMinSlice)
	require.ErrorIs(t, err, errWALReclaimInteractiveRead)
	require.ErrorIs(t, s.restoreWriterJournalLimit(t.Context()), errWALReclaimInteractiveRead)
	require.True(t, s.walShrinkPending.Load(), "a refused restoration erased mandatory journal-limit cleanup")
	require.Zero(t, s.WALReclaimStats().WriterHold.Count())
	release()
	require.NoError(t, s.restoreWriterJournalLimit(t.Context()))
	require.False(t, s.walShrinkPending.Load(), "cleanup must complete after the interactive call ends")
	require.Positive(t, s.WALReclaimStats().WriterHold.Count())
	require.EqualValues(t, 2, s.WALReclaimStats().DeferredInteractiveRead)
}

func TestWALReclaimBurstArrivesBetweenLastShrinkSliceAndRestore(t *testing.T) {
	for _, kind := range []string{"interactive", "build"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			lane := &fakeBuildLane{}
			lane.install(s)
			var release func()
			oldHook := walShrinkBeforeRestore
			walShrinkBeforeRestore = func() {
				if kind == "interactive" {
					release = s.AnnounceInteractiveRead()
				} else {
					lane.held.Store(true)
					release = func() { lane.held.Store(false) }
				}
			}
			t.Cleanup(func() {
				walShrinkBeforeRestore = oldHook
				if release != nil {
					release()
				}
			})
			_, _, err := s.shrinkWAL(t.Context(), db)
			require.Error(t, err, "the last-slice gap must not admit restore during new interactive/edit demand")
			require.True(t, s.walShrinkPending.Load(), "refused restore lost its retryable cleanup intent")
			if kind == "interactive" {
				require.ErrorIs(t, err, errWALReclaimInteractiveRead)
			} else {
				require.ErrorIs(t, err, errWALReclaimEditBurst)
			}
			release()
			walShrinkBeforeRestore = nil
			_, _, err = s.shrinkWAL(t.Context(), db)
			require.NoError(t, err)
			require.False(t, s.walShrinkPending.Load())
		})
	}
}

func TestWALReclaimClosedGateDefersDuringBuild(t *testing.T) {
	s, db := finalBackfillFixture(t)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)
	defer lane.held.Store(false)
	res := &walReclaimResult{urgent: true}
	err := s.reclaimWALInLane(t.Context(), walReclaimConfig{drainDeadline: time.Second, truncateBudget: walReclaimTruncateBudget}, db, res)
	require.ErrorIs(t, err, errWALReclaimEditBurst)
	require.False(t, res.pauseClosed)
	require.Zero(t, s.WALReclaimStats().WriterHold.Count())
	require.Zero(t, s.WALReclaimStats().EditBurstGateClosures)
}
