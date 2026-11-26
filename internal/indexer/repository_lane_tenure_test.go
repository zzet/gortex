package indexer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A repository lane names its tenant while it holds the lane, counts the
// callers waiting at the shared batch admission and at the lane itself and the
// reconcile requests queued for its next pass, and closes each tenure into its
// holder kind's totals, whichever door took it. A reconcile pass carries the
// scope it took on.
func TestRepositoryLaneTenureAttributesHolderAndWaiters(t *testing.T) {
	coordinator := newRepositoryMutationCoordinator(func([]string) (*IndexResult, error) { return &IndexResult{}, nil })
	var batchGate sync.RWMutex
	coordinator.batchMutationGate = &batchGate
	status := func() RepositoryLaneStatus { return coordinator.laneStatus("repo", time.Now()) }
	eventually := func(msg string, ok func(RepositoryLaneStatus) bool) {
		t.Helper()
		require.Eventually(t, func() bool { return ok(status()) }, 5*time.Second, time.Millisecond, msg)
	}

	// A batch writer holds the shared gate: the patch waits at admission.
	batchGate.Lock()
	entered, release := make(chan struct{}), make(chan struct{})
	patchDone := make(chan error, 1)
	go func() {
		patchDone <- coordinator.runExclusiveFor(context.Background(), OutputEntryWatcherPatchGraph, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	eventually("the patch was not counted at batch admission", func(s RepositoryLaneStatus) bool {
		return s.BatchAdmissionWaiting == 1 && s.LaneWaiting == 0 && s.Holder == nil
	})
	batchGate.Unlock()
	<-entered
	// The tenure began before fn ran and ends after it returns, so it covers
	// at least what the test measures between these two readings, on any
	// clock resolution.
	heldFrom := time.Now()
	held := status()
	require.NotNil(t, held.Holder, "the lane's tenant is not reported")
	require.Equal(t, string(OutputEntryWatcherPatchGraph), held.Holder.Kind)
	require.Zero(t, held.BatchAdmissionWaiting)
	require.Positive(t, held.Holder.SinceUnixMS)

	// An undeclared caller queues for the lane, then gives up.
	ctx, cancel := context.WithCancel(context.Background())
	queuedDone := make(chan error, 1)
	go func() {
		queuedDone <- coordinator.runExclusive(ctx, func() error { return nil })
	}()
	eventually("the queued caller was not counted at the lane", func(s RepositoryLaneStatus) bool { return s.LaneWaiting == 1 })
	_, free := coordinator.tryExclusive(OutputEntryContractRepair)
	require.False(t, free, "control: the held lane must refuse a try")
	cancel()
	require.ErrorIs(t, <-queuedDone, context.Canceled)
	eventually("the abandoned caller still counts as waiting", func(s RepositoryLaneStatus) bool { return s.LaneWaiting == 0 })

	// Two reconcile requests queue for one coalesced pass behind the patch.
	reconciled := make(chan error, 2)
	for _, path := range []string{"a.go", "b.go"} {
		go func() {
			_, err := coordinator.reconcile(context.Background(), []string{path})
			reconciled <- err
		}()
	}
	eventually("the queued reconcile requests were not counted", func(s RepositoryLaneStatus) bool {
		return s.ReconcileQueued == 2 && s.LaneWaiting == 1
	})

	time.Sleep(5 * time.Millisecond)
	heldFor := float64(time.Since(heldFrom).Microseconds()) / 1000
	close(release)
	require.NoError(t, <-patchDone)
	require.NoError(t, <-reconciled)
	require.NoError(t, <-reconciled)
	// Having answered both callers, the drain takes the lane once more, finds
	// no waiters and stops; it clears Running just before it hands the lane
	// back. Wait for both, so the try and the idle counts below do not race it.
	require.Eventually(t, func() bool { return !coordinator.stats().Running && len(coordinator.lane) == 1 },
		5*time.Second, time.Millisecond, "the reconcile drain did not stop")

	// The try door records a tenure as well.
	releaseTry, free := coordinator.tryExclusive(OutputEntryContractRepair)
	require.True(t, free)
	require.Equal(t, string(OutputEntryContractRepair), status().Holder.Kind)
	releaseTry()

	idle := status()
	require.Nil(t, idle.Holder, "an idle lane reports a tenant")
	require.Zero(t, idle.LaneWaiting)
	require.Zero(t, idle.BatchAdmissionWaiting)
	require.Zero(t, idle.ReconcileQueued)
	require.NotNil(t, idle.Last)
	require.Equal(t, string(OutputEntryContractRepair), idle.Last.Kind)
	totals := make(map[string]RepositoryLaneKindTotals, len(idle.ByKind))
	for _, kind := range idle.ByKind {
		totals[kind.Kind] = kind
	}
	require.Len(t, totals, 3, "tenures by kind: %+v", idle.ByKind)
	for _, kind := range []OutputMutationEntry{OutputEntryWatcherPatchGraph, OutputEntryContractRepair, OutputEntryRepositoryReconcileLane} {
		require.Equal(t, uint64(1), totals[string(kind)].Tenures, "tenures of %s", kind)
	}
	require.Equal(t, &RepositoryLaneBatch{Requests: 2, Paths: 2},
		totals[string(OutputEntryRepositoryReconcileLane)].Longest.Batch, "the reconcile pass's scope")
	require.Nil(t, totals[string(OutputEntryContractRepair)].Longest.Batch, "a tenant other than a reconcile pass carries a batch")
	patch := totals[string(OutputEntryWatcherPatchGraph)]
	require.GreaterOrEqual(t, patch.Longest.HeldForMS, heldFor, "the patch's tenure covers its hold")
	require.Equal(t, patch.HeldTotalMS, patch.Longest.HeldForMS)
	require.Equal(t, held.Holder.SinceUnixMS, patch.Longest.SinceUnixMS)
}
