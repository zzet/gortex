package indexer

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRequestedBackgroundOwnCallbackStartsAtObservedAdmission(t *testing.T) {
	for _, preexisting := range []bool{false, true} {
		name := "clean_direct"
		if preexisting {
			name = "stopped_unobserved_native_predecessor"
		}
		t.Run(name, func(t *testing.T) {
			path, _, w := pointPublicationFixture(t)
			w.config.DebounceMs = int(time.Hour / time.Millisecond)
			var predecessor *MutationTicket
			if preexisting {
				predecessor = w.scheduleFileMutation(path, ChangeModified)
			}
			record := newPointPublication(t, "direct-background")
			ticket, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
			require.NoError(t, err)
			require.NotNil(t, ticket)
			w.mu.Lock()
			observation := w.mutationWaiters[path][ticket.Generation].timerObservation
			require.NotNil(t, observation)
			owner := observation.background
			w.pending[path].Reset(time.Millisecond)
			w.mu.Unlock()
			require.NotNil(t, owner)
			owner.mu.Lock()
			origin, arm := owner.origin, owner.armStart
			owner.mu.Unlock()
			require.False(t, origin.IsZero())
			result := awaitMutationRetryResult(t, ticket)
			require.NoError(t, result.Err)
			require.Equal(t, ticket.Generation, result.AppliedGeneration)
			if predecessor != nil {
				require.NoError(t, awaitMutationRetryResult(t, predecessor).Err)
			}
			require.Eventually(t, func() bool { return record.RequestedBackground() != nil }, time.Second, time.Millisecond, "actual own requested callback/return must have its own B")
			background := record.RequestedBackground()
			require.False(t, arm.Before(origin))
			require.Equal(t, "direct_requested_attempt", background.Coverage)
			require.Equal(t, ticket.Generation, background.RequestedGeneration)
			require.Equal(t, ticket.Generation, background.AppliedGeneration)
			require.Zero(t, background.NativeHandoffs, "no count inferred from predecessor generation")
			require.Equal(t, origin.UTC().Format(time.RFC3339Nano), background.OriginWall)
			require.True(t, background.PartitionComplete)
			requirePointExecution(t, record, true)
			sum := 0.0
			for _, value := range background.PhasesMS {
				sum += value
			}
			require.InDelta(t, background.WallMS, sum, 0.000001)
			require.Greater(t, background.PhasesMS["requested_quiet_wait"], 0.0)
			require.Nil(t, record.CoalescedPublication(), "own P must not be relabeled as a foreign winner")
			snapshot := record.RequestedBackground()
			snapshot.PhasesMS["requested_quiet_wait"] = 999
			require.NotEqual(t, 999.0, record.RequestedBackground().PhasesMS["requested_quiet_wait"])
		})
	}
}

func TestRequestedBackgroundRefusesActuallyStartedPredecessorTimer(t *testing.T) {
	path, _, w := pointPublicationFixture(t)
	w.config.DebounceMs = int(time.Hour / time.Millisecond)
	native := w.scheduleFileMutation(path, ChangeModified)
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	t.Cleanup(release)
	// Replace only this test-owned, successfully stopped quiet timer with a real
	// fired callback. It remains in the same actual watcher slot until admission;
	// Stop must return false without an extra production hook or fake bool.
	w.mu.Lock()
	require.True(t, w.pending[path].Stop())
	w.asyncWork.Done()
	w.asyncWork.Add(1)
	w.pending[path] = time.AfterFunc(0, func() { defer w.asyncWork.Done(); close(entered); <-resume })
	w.mu.Unlock()
	awaitPublicationSignal(t, entered)
	record := newPointPublication(t, "failed-predecessor-stop")
	ticket, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
	require.NoError(t, err)
	require.NotNil(t, ticket)
	release()
	w.mu.Lock()
	w.pending[path].Reset(time.Millisecond)
	w.mu.Unlock()
	require.NoError(t, awaitMutationRetryResult(t, ticket).Err)
	require.NoError(t, awaitMutationRetryResult(t, native).Err)
	require.Nil(t, record.RequestedBackground(), "already-started predecessor cannot supply observed replacement coverage")
	_, unavailable := record.CoalescedPublicationEvidence()
	require.NotNil(t, unavailable)
}
