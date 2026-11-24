package indexer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A repository lane names its tenant while it holds the lane, counts the
// callers waiting at the shared batch admission and at the lane itself, and
// closes each tenure into its holder kind's totals, whichever door took it.
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

	time.Sleep(5 * time.Millisecond)
	close(release)
	require.NoError(t, <-patchDone)

	// The try door and the coalescing drain each record a tenure.
	releaseTry, free := coordinator.tryExclusive(OutputEntryContractRepair)
	require.True(t, free)
	require.Equal(t, string(OutputEntryContractRepair), status().Holder.Kind)
	releaseTry()
	_, err := coordinator.reconcile(context.Background(), []string{"a.go"})
	require.NoError(t, err)

	idle := status()
	require.Nil(t, idle.Holder, "an idle lane reports a tenant")
	require.Zero(t, idle.LaneWaiting)
	require.Zero(t, idle.BatchAdmissionWaiting)
	require.NotNil(t, idle.Last)
	require.Equal(t, string(OutputEntryRepositoryReconcileLane), idle.Last.Kind)
	totals := make(map[string]RepositoryLaneKindTotals, len(idle.ByKind))
	for _, kind := range idle.ByKind {
		totals[kind.Kind] = kind
	}
	require.Len(t, totals, 3, "tenures by kind: %+v", idle.ByKind)
	for _, kind := range []OutputMutationEntry{OutputEntryWatcherPatchGraph, OutputEntryContractRepair, OutputEntryRepositoryReconcileLane} {
		require.Equal(t, uint64(1), totals[string(kind)].Tenures, "tenures of %s", kind)
	}
	patch := totals[string(OutputEntryWatcherPatchGraph)]
	require.GreaterOrEqual(t, patch.Longest.HeldForMS, float64(5), "the patch's tenure covers its hold")
	require.Equal(t, patch.HeldTotalMS, patch.Longest.HeldForMS)
	require.Equal(t, held.Holder.SinceUnixMS, patch.Longest.SinceUnixMS)
}
