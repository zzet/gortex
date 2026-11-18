package indexer

import (
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/thirdparty/fswatcher"
)

func mutationCounterFixture(t *testing.T) *Watcher {
	t.Helper()
	dir, w := toolAdmissionFixture(t)
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\nfunc Landed() {}\n")
	first, err := w.EnqueueFileMutation(t.Context(), path)
	require.NoError(t, err)
	second, err := w.EnqueueFileMutation(t.Context(), path)
	require.NoError(t, err)
	landed, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	w.pointReindexRaw = func(file string, _ *PublicationStamps) (*IndexResult, error) {
		result, patchErr := w.indexer.incrementalPointWatcherPath(dir, file)
		close(landed)
		<-release
		return result, patchErr
	}
	tool, err := w.EnqueueToolMutation(t.Context(), path)
	require.NoError(t, err)
	select {
	case <-landed:
	case <-time.After(2 * time.Second):
		t.Fatal("tool graph body did not land")
	}
	w.handleEvent(fswatcher.WatchEvent{Path: path, Types: []fswatcher.EventType{fswatcher.EventMod}})
	// Explicit drift repair overtakes the landed tool; its quiet timer stays
	// pending while the old body covers only its own and prior receipts.
	drift, err := w.EnqueueFileMutation(t.Context(), path)
	require.NoError(t, err)
	require.Greater(t, drift.Generation, tool.Generation)
	unblock()
	for _, ticket := range []*MutationTicket{first, second, tool} {
		result := awaitMutationRetryResult(t, ticket)
		require.NoError(t, result.Err)
		require.Equal(t, tool.Generation, result.AppliedGeneration)
	}
	select {
	case value := <-drift.Done:
		t.Fatalf("covered completion consumed unexecuted drift: %+v", value)
	default:
	}
	return w
}

func TestWatcherMutationCountersDescribeActualCompletions(t *testing.T) {
	w := mutationCounterFixture(t)
	counters, ok := any(w).(interface{ MutationCounters() map[string]uint64 })
	require.True(t, ok, "watcher must expose bounded aggregate mutation counters")
	want := map[string]uint64{"echoes_attached": 1, "generations_superseded": 1, "waiters_completed_by_later_patch": 2}
	require.Eventually(t, func() bool { return reflect.DeepEqual(want, counters.MutationCounters()) }, 2*time.Second, time.Millisecond, "receipt-validated echo attachment follows completion outside watcher locks")
	require.Equal(t, want, counters.MutationCounters(), "reading counters must not drain them")
}

func TestMultiWatcherMutationCountersSumActiveWatchers(t *testing.T) {
	a, b := mutationCounterFixture(t), mutationCounterFixture(t)
	mw := &MultiWatcher{watchers: map[string]*Watcher{"a": a, "b": b}}
	counters, ok := any(mw).(interface{ MutationCounters() map[string]uint64 })
	require.True(t, ok, "multi-watcher must aggregate the active watcher instances")
	want := map[string]uint64{"echoes_attached": 2, "generations_superseded": 2, "waiters_completed_by_later_patch": 4}
	require.Eventually(t, func() bool { return reflect.DeepEqual(want, counters.MutationCounters()) }, 2*time.Second, time.Millisecond)
}

func TestFailedStormDoesNotCountPositiveLaterCompletion(t *testing.T) {
	dir, w := toolAdmissionFixture(t)
	path := filepath.Join(dir, "main.go")
	first, err := w.EnqueueFileMutation(t.Context(), path)
	require.NoError(t, err)
	second, err := w.EnqueueFileMutation(t.Context(), path)
	require.NoError(t, err)
	w.completeStormMutationWaiters(map[string]uint64{path: second.Generation}, &IndexResult{FailedFiles: []string{path}}, nil, DerivedFanoutCompleteness{Observed: true, Complete: true})
	for _, ticket := range []*MutationTicket{first, second} {
		require.Error(t, awaitMutationRetryResult(t, ticket).Err)
	}
	counters, ok := any(w).(interface{ MutationCounters() map[string]uint64 })
	require.True(t, ok)
	require.Zero(t, counters.MutationCounters()["waiters_completed_by_later_patch"], "a failed batch cannot count a positive covered receipt")
}
