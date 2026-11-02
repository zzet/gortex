package store_sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWarmupShrinkPermissionIsLiveAndDoesNotAuthorizeOtherCompletion(t *testing.T) {
	s, _ := finalBackfillFixture(t)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)
	defer lane.held.Store(false)
	s.warmupWALWaiters.Store(1)
	defer s.warmupWALWaiters.Store(0)
	require.NoError(t, s.reclaimCompletionCause(t.Context(), &walReclaimResult{warmupShrink: true}))
	require.ErrorIs(t, s.reclaimCompletionCause(t.Context(), &walReclaimResult{}), errWALReclaimEditBurst, "blocked warmup cannot authorize reset/adaptive/closed-gate work")
	release := s.AnnounceInteractiveRead()
	require.ErrorIs(t, s.reclaimCompletionCause(t.Context(), &walReclaimResult{warmupShrink: true}), errWALReclaimInteractiveRead)
	release()
	release = s.AnnounceWrite()
	require.ErrorIs(t, s.reclaimCompletionCause(t.Context(), &walReclaimResult{warmupShrink: true}), s.writeDemandCause())
	release()
	s.warmupWALWaiters.Store(0)
	require.ErrorIs(t, s.reclaimCompletionCause(t.Context(), &walReclaimResult{warmupShrink: true}), errWALReclaimEditBurst)
}

func TestWarmupShrinkWithdrawnWhileWriterAdmissionQueued(t *testing.T) {
	s, _ := finalBackfillFixture(t)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)
	defer lane.held.Store(false)
	s.warmupWALWaiters.Store(1)
	defer s.warmupWALWaiters.Store(0)
	s.writeMu.Lock()
	held := true
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	done := make(chan error, 1)
	joined := false
	defer func() {
		cancel()
		if held {
			s.writeMu.Unlock()
		}
		if !joined {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("owned shrink admission did not join")
			}
		}
	}()
	res := &walReclaimResult{warmupShrink: true}
	go func() {
		err := s.lockWALReclaimCompletion(ctx, res)
		if err == nil {
			s.writeMu.Unlock()
		}
		done <- err
	}()
	require.Eventually(t, func() bool { return s.writeMu.reclaimWaiters.Load() > 0 }, time.Second, time.Millisecond)
	s.warmupWALWaiters.Store(0)
	s.writeMu.Unlock()
	held = false
	err := <-done
	joined = true
	require.ErrorIs(t, err, errWALReclaimEditBurst, "entry-time warmup permission survived withdrawal")
	require.True(t, res.deferredEditBurst)
	require.True(t, s.writeMu.TryLock(), "refused completion retained the writer")
	s.writeMu.Unlock()
	require.EqualValues(t, 1, s.WALReclaimStats().EditBurstWriterHold.Count(), "an actual short admission check is still a burst hold")
}

func TestWarmupShrinkWithdrawalBeforeJournalRestoreRetainsPendingCleanup(t *testing.T) {
	s, db := finalBackfillFixture(t)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)
	defer lane.held.Store(false)
	s.warmupWALWaiters.Store(1)
	defer s.warmupWALWaiters.Store(0)
	old := walShrinkBeforeRestore
	defer func() { walShrinkBeforeRestore = old }()
	walShrinkBeforeRestore = func() { s.warmupWALWaiters.Store(0) }
	_, _, err := s.shrinkWAL(t.Context(), db)
	require.ErrorIs(t, err, errWALReclaimEditBurst)
	require.True(t, s.walShrinkPending.Load(), "warmup returning after its first slice lost journal-limit cleanup")
	lane.held.Store(false)
	require.NoError(t, s.restoreWriterJournalLimit(t.Context()))
	require.False(t, s.walShrinkPending.Load())
}
