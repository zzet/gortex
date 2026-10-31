package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func pointPublicationFixture(t *testing.T) (string, *Indexer, *Watcher) {
	t.Helper()
	root, idx, w := inertTestWatcher(t, "edit.go", "package p\n\nfunc Before() {}\n")
	// Explicit queue admission exercises the real executor without starting an
	// fsnotify loop. Stop must still join every timer/patch and close its handle.
	w.degradedNoFsnotify = true
	t.Cleanup(func() {
		if !w.mutationAdmissionStopped() {
			require.NoError(t, w.Stop())
		}
	})
	path := filepath.Join(root, "edit.go")
	bumpMtime(t, path, "package p\n\nfunc After() {}\n")
	return path, idx, w
}

func newPointPublication(t *testing.T, name string) *PublicationPhaseRecord {
	t.Helper()
	// Keep room for the separately owned physical successor. Eviction refusal
	// has its own bounded-recorder control below.
	recorder := NewPublicationPhaseRecorder(2, 1)
	t.Cleanup(func() { runtime.KeepAlive(recorder) })
	return recorder.Begin("", name, PublicationSourceMCPEdit, time.Now())
}

func requirePointExecution(t *testing.T, record *PublicationPhaseRecord, want bool) {
	t.Helper()
	offsets := phaseOffsets(record.Snapshot())
	for _, phase := range []PublicationPhase{PublicationCycleStarted, PublicationPatchExecutionStarted, PublicationPatchExecutionFinished,
		PublicationPatchDerivedStarted, PublicationPatchDerivedFinished} {
		if want {
			require.Contains(t, offsets, phase)
		} else {
			require.NotContains(t, offsets, phase, "rejected/shared attempts must not publish receipt-owned spans")
		}
	}
	if want {
		require.NotNil(t, record.Snapshot().laneWait, "only this winning attempt owns its actual lane pair")
		require.NotNil(t, record.Snapshot().patchLaps, "real standalone/MultiWatcher paths must carry the same local scalar collector")
		require.LessOrEqual(t, offsets[PublicationPatchExecutionStarted], offsets[PublicationPatchDerivedStarted])
		require.LessOrEqual(t, offsets[PublicationPatchDerivedStarted], offsets[PublicationPatchDerivedFinished])
		require.LessOrEqual(t, offsets[PublicationPatchDerivedFinished], offsets[PublicationPatchExecutionFinished])
		timing := record.Snapshot().EditTimings()
		require.Equal(t, "measured", timing.States["derived"])
		require.NotNil(t, timing.Milliseconds["derived"])
	} else {
		require.Nil(t, record.Snapshot().laneWait, "rejected/shared attempts must not borrow the winner's lane wait")
		require.Nil(t, record.Snapshot().debounce, "rejected/shared attempts must not borrow the winner's quiet timer")
		require.Nil(t, record.Snapshot().attemptWindow, "rejected/shared attempts must not publish the winner's physical window")
		require.Nil(t, record.Snapshot().admissions, "rejected/shared attempts must not borrow admission measurements")
		require.Nil(t, record.Snapshot().patchLaps, "rejected attempts must not transfer any scalar evidence")
	}
}

func awaitPublicationSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("point execution did not reach its controlled boundary")
	}
}

func TestCanonicalPublicationObservesRealStandaloneExecution(t *testing.T) {
	path, idx, w := pointPublicationFixture(t)
	record := newPointPublication(t, "standalone")
	ticket, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
	require.NoError(t, err)
	result := awaitMutationRetryResult(t, ticket)
	require.NoError(t, result.Err)
	require.NotNil(t, nodeByName(t, idx, "edit.go", "After"), "a real parse/apply must have occurred")
	requirePointExecution(t, record, true)
	require.Equal(t, ticket.Generation, record.Snapshot().Ticket)
	require.Zero(t, record.Snapshot().DirtyGenerationID)
}

func TestCanonicalPublicationObservesMultiWatcherExecutor(t *testing.T) {
	mw, mi, root, _ := setupMultiWatcherTest(t)
	for _, w := range mw.watchers {
		w.degradedNoFsnotify = true
		t.Cleanup(func() {
			if !w.mutationAdmissionStopped() {
				require.NoError(t, w.Stop())
			}
		})
	}
	w := mw.watchers["repo-a"]
	require.NotNil(t, w.pointReindexRaw, "production MultiWatcher must install the daemon's MultiIndexer executor")
	path := filepath.Join(root, "main.go")
	bumpMtime(t, path, "package main\n\nfunc ChangedByDaemonPath() {}\n")
	record := newPointPublication(t, "multi")
	ticket, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
	require.NoError(t, err)
	require.NoError(t, awaitMutationRetryResult(t, ticket).Err)
	require.NotNil(t, nodeByName(t, mi.GetIndexer("repo-a"), "repo-a/main.go", "ChangedByDaemonPath"))
	requirePointExecution(t, record, true)
}

func TestCanonicalPublicationCommitsOnlyGuardedWinningAttempt(t *testing.T) {
	for _, nativeSuccessor := range []bool{false, true} {
		t.Run(fmt.Sprintf("native_successor=%t", nativeSuccessor), func(t *testing.T) {
			path, _, w := pointPublicationFixture(t)
			beforeComplete, release := make(chan struct{}), make(chan struct{})
			var releaseOnce atomic.Bool
			t.Cleanup(func() {
				if releaseOnce.CompareAndSwap(false, true) {
					close(release)
				}
			})
			w.mutationBeforeComplete = func(_ string, generation uint64) {
				if generation == 1 {
					close(beforeComplete)
					<-release
				}
			}
			older := newPointPublication(t, "older")
			first, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), older), path)
			require.NoError(t, err)
			awaitPublicationSignal(t, beforeComplete)
			requirePointExecution(t, older, false)
			bumpMtime(t, path, "package p\n\nfunc Newer() {}\n")
			newer := newPointPublication(t, "newer")
			var second *MutationTicket
			if nativeSuccessor {
				// The normal fsnotify path has no request context. A native
				// successor must resolve the older ticket without borrowing its
				// record for the newer physical work.
				second = w.scheduleFileMutation(path, ChangeModified)
				require.NotNil(t, second)
			} else {
				second, err = w.EnqueueFileMutation(WithPublicationRecord(t.Context(), newer), path)
				require.NoError(t, err)
			}
			if releaseOnce.CompareAndSwap(false, true) {
				close(release)
			}
			for _, ticket := range []*MutationTicket{first, second} {
				result := awaitMutationRetryResult(t, ticket)
				require.NoError(t, result.Err)
				require.Equal(t, ticket.Generation, result.RequestedGeneration)
				require.Equal(t, second.Generation, result.AppliedGeneration)
			}
			require.Greater(t, second.Generation, first.Generation)
			requirePointExecution(t, older, false)
			requirePointExecution(t, newer, !nativeSuccessor)
			require.Equal(t, first.Generation, older.Snapshot().Ticket)
			if nativeSuccessor {
				require.Eventually(t, func() bool {
					winner := older.CoalescedPublication()
					return winner != nil && winner.Timings.Window.PatchWallMS != nil
				}, 5*time.Second, time.Millisecond)
				winner := older.CoalescedPublication()
				require.Equal(t, first.Generation, winner.RequestedGeneration)
				require.Equal(t, second.Generation, winner.AppliedGeneration)
				require.NotEqual(t, older.Key(), winner.Publication.Key)
				require.Nil(t, winner.Timings.Milliseconds["debounce"])
				require.Equal(t, "not_established", winner.BackgroundCoverage)
			}
		})
	}
}

func TestCanonicalPublicationDiscardsRetryAttemptSpans(t *testing.T) {
	path, idx, w := pointPublicationFixture(t)
	failedAt := make(chan time.Time, 1)
	secondEntered, releaseSecond := make(chan struct{}), make(chan struct{})
	var released atomic.Bool
	t.Cleanup(func() {
		if released.CompareAndSwap(false, true) {
			close(releaseSecond)
		}
	})
	var calls atomic.Int32
	w.mutationRetryDelayFn = func(int) time.Duration { return time.Millisecond }
	w.pointReindexRaw = func(file string, stamps *PublicationStamps) (*IndexResult, error) {
		if calls.Add(1) == 1 {
			failedAt <- time.Now()
			return nil, fmt.Errorf("busy: %w", context.DeadlineExceeded)
		}
		close(secondEntered)
		<-releaseSecond
		return idx.incrementalPointWatcherPathObserved(idx.rootPath, file, stamps)
	}
	origin := time.Now()
	recorder := NewPublicationPhaseRecorder(1, 1)
	t.Cleanup(func() { runtime.KeepAlive(recorder) })
	record := recorder.Begin("", "retry", PublicationSourceMCPEdit, origin)
	ticket, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
	require.NoError(t, err)
	awaitPublicationSignal(t, secondEntered)
	firstFailed := <-failedAt
	requirePointExecution(t, record, false)
	if released.CompareAndSwap(false, true) {
		close(releaseSecond)
	}
	require.NoError(t, awaitMutationRetryResult(t, ticket).Err)
	requirePointExecution(t, record, true)
	require.EqualValues(t, 2, calls.Load())
	require.Nil(t, record.Snapshot().debounce, "winning retry backoff is not the initial debounce and cannot borrow the failed attempt's pair")
	require.Equal(t, "not_observed", record.Snapshot().EditTimings().States["debounce"])
	require.Same(t, recorder, record.owner.Value(), "late completion requires retained recorder membership")
	require.Eventually(t, func() bool { return record.Snapshot().attemptWindow != nil }, 2*time.Second, time.Millisecond)
	require.Equal(t, 1, record.Snapshot().attemptWindow.retry)
	require.Equal(t, ticket.Generation, record.Snapshot().EditTimings().Window.PatchAccounting.AttemptTicket)
	require.Equal(t, 1, record.Snapshot().EditTimings().Window.PatchAccounting.AttemptRetry)
	require.GreaterOrEqual(t, record.Snapshot().attemptWindow.startNS, firstFailed.Sub(origin).Nanoseconds(), "physical wall belongs only to the winning retry")
	require.GreaterOrEqual(t, phaseOffsets(record.Snapshot())[PublicationPatchExecutionStarted], firstFailed.Sub(origin).Nanoseconds())
	require.GreaterOrEqual(t, record.Snapshot().laneWait.startNS, firstFailed.Sub(origin).Nanoseconds(), "retry must retain only the winning attempt's wait")
}

func TestCanonicalPublicationDiscardsStoppedInFlightAttempt(t *testing.T) {
	path, idx, w := pointPublicationFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var released atomic.Bool
	t.Cleanup(func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	})
	w.pointReindexRaw = func(file string, stamps *PublicationStamps) (*IndexResult, error) {
		close(entered)
		<-release
		return idx.incrementalPointWatcherPathObserved(idx.rootPath, file, stamps)
	}
	record := newPointPublication(t, "stop")
	ticket, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
	require.NoError(t, err)
	awaitPublicationSignal(t, entered)
	stopped := make(chan error, 1)
	go func() { stopped <- w.Stop() }()
	require.ErrorIs(t, awaitMutationRetryResult(t, ticket).Err, errWatcherStopped)
	requirePointExecution(t, record, false)
	if released.CompareAndSwap(false, true) {
		close(release)
	}
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not join the released point attempt")
	}
	requirePointExecution(t, record, false)
}

func TestCanonicalPublicationDiscardsFailedExecutionSpans(t *testing.T) {
	path, _, w := pointPublicationFixture(t)
	failure := errors.New("point executor rejected the patch")
	var calls atomic.Int32
	w.pointReindexRaw = func(string, *PublicationStamps) (*IndexResult, error) {
		calls.Add(1)
		return nil, failure
	}
	record := newPointPublication(t, "failed")
	ticket, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
	require.NoError(t, err)
	require.ErrorIs(t, awaitMutationRetryResult(t, ticket).Err, failure)
	require.EqualValues(t, 1, calls.Load(), "permanent failures must not be retried")
	requirePointExecution(t, record, false)
}

func TestPointPublicationPreservesLaneContextAndNativeAbstention(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	w.config.DebounceMs = int(time.Hour / time.Millisecond)
	ticket := w.scheduleFileMutation("native.go", ChangeModified)
	require.Nil(t, w.beginPointPublication("native.go", ticket.Generation))
	require.True(t, w.completeMutationWaitersIfCurrent("native.go", ticket.Generation, nil))
	require.NoError(t, awaitMutationRetryResult(t, ticket).Err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	attempt := &pointPublicationAttempt{record: newPointPublication(t, "context"), stamps: &PublicationStamps{}}
	observed := attempt.withContext(ctx)
	wantDeadline, _ := ctx.Deadline()
	gotDeadline, ok := observed.Deadline()
	require.True(t, ok)
	require.Equal(t, wantDeadline, gotDeadline)
	cancel()
	require.ErrorIs(t, observed.Err(), context.Canceled)
	require.Nil(t, phaseRecordFrom(observed), "a collector context must not live-publish into a receipt")
}

func TestCanonicalPublicationBypassedDerivedTailRemainsUnobserved(t *testing.T) {
	path, idx, _ := pointPublicationFixture(t)
	idx.deferGlobalPasses.Store(true)
	stamps := &PublicationStamps{}
	record := newPointPublication(t, "bypassed")
	// This is the existing non-forced reconcile mode: even with a collector,
	// its explicit deferred tail must not be fabricated as measured zero.
	_, err := idx.incrementalWatcherPaths(idx.rootPath, []string{path}, incrementalPathMode{
		detectDeletions: true,
		publication:     stamps,
	})
	require.NoError(t, err)
	require.NotNil(t, nodeByName(t, idx, "edit.go", "After"))
	record.Absorb(stamps)
	offsets := phaseOffsets(record.Snapshot())
	require.NotContains(t, offsets, PublicationPatchDerivedStarted)
	require.NotContains(t, offsets, PublicationPatchDerivedFinished)
	timing := record.Snapshot().EditTimings()
	require.Nil(t, timing.Milliseconds["derived"])
	require.Equal(t, "not_observed", timing.States["derived"])
}

func TestCanonicalNativeWinnerServesMultipleObservedPredecessors(t *testing.T) {
	path, _, w := pointPublicationFixture(t)
	w.config.DebounceMs = int(time.Hour / time.Millisecond)
	recorder := NewPublicationPhaseRecorder(8, 1)
	t.Cleanup(func() { runtime.KeepAlive(recorder) })
	records := []*PublicationPhaseRecord{
		recorder.Begin("", "first", PublicationSourceMCPEdit, time.Now()),
		recorder.Begin("", "second", PublicationSourceMCPEdit, time.Now()),
	}
	var tickets []*MutationTicket
	for _, record := range records {
		ticket, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
		require.NoError(t, err)
		tickets = append(tickets, ticket)
	}
	native := w.scheduleFileMutation(path, ChangeModified)
	require.NotNil(t, native)
	w.mu.Lock()
	w.pending[path].Reset(time.Millisecond)
	w.mu.Unlock()
	for _, ticket := range append(tickets, native) {
		result := awaitMutationRetryResult(t, ticket)
		require.NoError(t, result.Err)
		require.Equal(t, native.Generation, result.AppliedGeneration)
	}
	for i, record := range records {
		requirePointExecution(t, record, false)
		require.Eventually(t, func() bool {
			winner := record.CoalescedPublication()
			return winner != nil && winner.Timings.Window.PatchWallMS != nil
		}, 5*time.Second, time.Millisecond)
		winner := record.CoalescedPublication()
		require.Equal(t, tickets[i].Generation, winner.RequestedGeneration)
		require.Equal(t, native.Generation, winner.AppliedGeneration)
		require.Nil(t, winner.Timings.Milliseconds["debounce"])
	}
	require.Equal(t, records[0].CoalescedPublication().Publication.Key, records[1].CoalescedPublication().Publication.Key)
	// Neither a retained edit nor its weak link can resurrect an evicted winner.
	for i := 0; i < 8; i++ {
		recorder.Begin("", fmt.Sprintf("replacement-%d", i), PublicationSourceMCPEdit, time.Now())
	}
	for _, record := range records {
		require.Nil(t, record.CoalescedPublication())
	}
}

func TestCoalescedAttemptRequiresRetainedOwnerAndOneShotLink(t *testing.T) {
	origin := time.Now()
	recorder := NewPublicationPhaseRecorder(4, 1)
	record := recorder.Begin("", "edit", PublicationSourceMCPEdit, origin)
	require.True(t, record.bindCanonicalTicket(1, origin))
	winner := record.beginCoalescedAttempt(2, 0, origin.Add(time.Millisecond))
	require.NotNil(t, winner)
	require.Nil(t, record.beginCoalescedAttempt(2, 0, origin.Add(2*time.Millisecond)), "wall-clock readings cannot rebind a callback identity")
	// Admission alone cannot manufacture a serving relationship.
	record.linkCoalescedWinner(winner, 1, 2)
	require.Nil(t, record.CoalescedPublication())
	stamps := winner.newCanonicalPatchStamps()
	winner.Absorb(stamps)
	record.linkCoalescedWinner(winner, 1, 2)
	linked := record.CoalescedPublication()
	require.NotNil(t, linked)
	require.Nil(t, linked.Timings.Window.PatchWallMS, "unfinished physical work remains unavailable")
	other := record.beginCoalescedAttempt(3, 0, origin.Add(2*time.Millisecond))
	require.NotNil(t, other)
	other.Absorb(other.newCanonicalPatchStamps())
	record.linkCoalescedWinner(other, 1, 3)
	require.Equal(t, uint64(2), record.CoalescedPublication().AppliedGeneration, "a served ticket cannot be rebound")
	require.Equal(t, uint64(2), linked.AppliedGeneration, "earlier returned values stay immutable")
	// A retained pointer to an evicted record cannot create another attempt.
	for i := 0; i < 4; i++ {
		recorder.Begin("", fmt.Sprintf("evict-%d", i), PublicationSourceMCPEdit, origin)
	}
	require.Nil(t, record.beginCoalescedAttempt(4, 0, origin.Add(3*time.Millisecond)))
	require.Nil(t, record.CoalescedPublication())
	absent := &PublicationPhaseRecord{}
	require.Nil(t, absent.beginCoalescedAttempt(1, 0, origin))
	require.Nil(t, absent.CoalescedPublication())
	runtime.KeepAlive(recorder)
}

func TestCanonicalObservationIndexFollowsOutstandingWaiters(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	w.config.DebounceMs = int(time.Hour / time.Millisecond)
	for i := 0; i < 100; i++ {
		w.scheduleFileMutation(fmt.Sprintf("native-%d.go", i), ChangeModified)
	}
	require.Nil(t, w.observedMutationGeneration, "native storms do not allocate observation metadata")
	record := newPointPublication(t, "indexed")
	observed := w.scheduleFileMutationWithRecord("edit.go", ChangeModified, record)
	require.Equal(t, observed.Generation, w.observedMutationGeneration["edit.go"])
	native := w.scheduleFileMutation("edit.go", ChangeModified)
	require.Equal(t, observed.Generation, w.observedMutationGeneration["edit.go"], "native admission never rebinds the observed owner")
	require.True(t, w.completeMutationWaitersIfCurrent("edit.go", native.Generation, nil))
	require.Empty(t, w.observedMutationGeneration)
	require.NoError(t, awaitMutationRetryResult(t, observed).Err)
	require.NoError(t, awaitMutationRetryResult(t, native).Err)
	other := w.scheduleFileMutationWithRecord("other.go", ChangeModified, newPointPublication(t, "stopped-index"))
	require.NotEmpty(t, w.observedMutationGeneration)
	w.failMutationWaiters(errWatcherStopped)
	require.Nil(t, w.observedMutationGeneration)
	require.ErrorIs(t, awaitMutationRetryResult(t, other).Err, errWatcherStopped)
}
