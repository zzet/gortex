package indexer

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/thirdparty/fswatcher"
)

func TestUnlandedSupersededPointKeepsItsWaitersPending(t *testing.T) {
	dir, watcher := toolAdmissionFixture(t)
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\nfunc Unapplied() {}\n")
	record := newPointPublication(t, "unlanded")
	first, err := watcher.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
	require.NoError(t, err)
	second, err := watcher.EnqueueFileMutation(t.Context(), path)
	require.NoError(t, err)
	called := false
	watcher.pointReindexRaw = func(string, *PublicationStamps) (*IndexResult, error) {
		called = true
		return nil, errors.New("superseded work must not reach the executor")
	}
	// Both actual timers remain in native quiet time. Run only the older
	// generation synchronously so the pre-write guard, rather than a timeout,
	// proves that it performed no graph work and covered no receipt.
	watcher.runPointMutation(path, ChangeModified, first.Generation, 0)
	require.False(t, called)
	require.NotNil(t, nodeByName(t, watcher.indexer, "main.go", "Main"))
	requirePointExecution(t, record, false)
	for _, ticket := range []*MutationTicket{first, second} {
		select {
		case result := <-ticket.Done:
			t.Fatalf("unlanded predecessor completed a ticket: %+v", result)
		default:
		}
	}
}

func TestFailedSupersededStormCannotClaimCoveredSuccess(t *testing.T) {
	for _, failure := range []string{"failed_path", "batch_error", "missing_result"} {
		t.Run(failure, func(t *testing.T) {
			dir, watcher := toolAdmissionFixture(t)
			path := filepath.Join(dir, "main.go")
			first, err := watcher.EnqueueFileMutation(t.Context(), path)
			require.NoError(t, err)
			second, err := watcher.EnqueueFileMutation(t.Context(), path)
			require.NoError(t, err)
			var result *IndexResult
			var batchErr error
			switch failure {
			case "failed_path":
				result = &IndexResult{FailedFiles: []string{path}}
			case "batch_error":
				result = &IndexResult{}
				batchErr = errors.New("batch failed")
			}
			fanout := DerivedFanoutCompleteness{Observed: true, Complete: true}
			watcher.completeStormMutationWaiters(map[string]uint64{path: first.Generation}, result, batchErr, fanout)
			for _, ticket := range []*MutationTicket{first, second} {
				select {
				case value := <-ticket.Done:
					t.Fatalf("failed predecessor consumed successor-owned waiters: %+v", value)
				default:
				}
			}
			// When the current attempt itself fails, every attached waiter
			// receives that error without any positive fan-out evidence.
			watcher.completeStormMutationWaiters(map[string]uint64{path: second.Generation}, result, batchErr, fanout)
			for _, ticket := range []*MutationTicket{first, second} {
				value := awaitMutationRetryResult(t, ticket)
				require.Error(t, value.Err)
				require.False(t, value.Reindexed)
				require.False(t, value.DerivedFanout.Observed)
				require.False(t, value.DerivedFanout.Complete)
			}
		})
	}
}

func TestHeldEchoRechecksContentAfterSameSizeSaveAndABA(t *testing.T) {
	for _, restored := range []string{"Save", "Main"} {
		t.Run(restored, func(t *testing.T) {
			dir, watcher := toolAdmissionFixture(t)
			watcher.config.DebounceMs = 5
			path := filepath.Join(dir, "main.go")
			writeTestFile(t, path, "package main\nfunc Next() {}\n")
			landed, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			t.Cleanup(unblock)
			var first atomic.Bool
			watcher.pointReindexRaw = func(file string, _ *PublicationStamps) (*IndexResult, error) {
				result, err := watcher.indexer.incrementalPointWatcherPath(dir, file)
				if first.CompareAndSwap(false, true) {
					close(landed)
					<-release
				}
				return result, err
			}
			ticket, err := watcher.EnqueueToolMutation(t.Context(), path)
			require.NoError(t, err)
			select {
			case <-landed:
			case <-time.After(2 * time.Second):
				t.Fatal("tool patch did not land")
			}
			before, err := os.Stat(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, []byte("package main\nfunc "+restored+"() {}\n"), 0o644))
			require.NoError(t, os.Chtimes(path, before.ModTime(), before.ModTime()))
			after, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, before.Size(), after.Size())
			require.Equal(t, before.ModTime(), after.ModTime())
			require.False(t, watcher.indexer.fileMtimeMatches(path), "the existing content receipt must distinguish these bytes")
			watcher.handleEvent(fswatcher.WatchEvent{Path: path, Types: []fswatcher.EventType{fswatcher.EventMod}})
			watcher.mu.Lock()
			inFlightGeneration := watcher.nextGeneration
			watcher.mu.Unlock()
			require.Equal(t, ticket.Generation, inFlightGeneration, "native events wait for the tool's receipt decision")
			unblock()
			require.NoError(t, awaitMutationRetryResult(t, ticket).Err)
			require.Eventually(t, func() bool {
				for _, node := range watcher.indexer.graph.GetFileNodes("main.go") {
					if node.Name == restored {
						return true
					}
				}
				return false
			}, 2*time.Second, time.Millisecond, "a held changed-content echo must schedule a real successor patch")
			watcher.mu.Lock()
			finalGeneration := watcher.nextGeneration
			watcher.mu.Unlock()
			require.Greater(t, finalGeneration, ticket.Generation)
		})
	}
}

func TestNativeAdmissionRechecksToolOwnership(t *testing.T) {
	for _, storm := range []bool{false, true} {
		t.Run(map[bool]string{false: "point", true: "storm"}[storm], func(t *testing.T) {
			dir, watcher := toolAdmissionFixture(t)
			path := filepath.Join(dir, "main.go")
			entered, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			t.Cleanup(unblock)
			watcher.pointMutationPatch = func(string, ChangeKind, uint64) error {
				close(entered)
				<-release
				return nil
			}
			tool, err := watcher.EnqueueToolMutation(t.Context(), path)
			require.NoError(t, err)
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("tool callback did not claim its work")
			}
			// Exercise the final admission boundary directly: a tool can arrive
			// after the native handler's initial check and before either route.
			if storm {
				watcher.recordInStormEvent(path, ChangeModified, true)
			} else {
				require.Nil(t, watcher.scheduleMutationEnteredEvent(path, ChangeModified, nil, time.Time{}, false, true))
			}
			watcher.drainHeldNative(path) // an outstanding tool keeps the hold
			watcher.mu.Lock()
			generation, held := watcher.nextGeneration, watcher.heldNative[path]
			watcher.mu.Unlock()
			require.Equal(t, tool.Generation, generation)
			require.Equal(t, ChangeModified, held)
			watcher.stormMu.Lock()
			active := watcher.stormActive
			watcher.stormMu.Unlock()
			require.False(t, active, "held echoes must not start a storm timer")
			// Explicit drift repair is independent of native echo suppression.
			drift, err := watcher.EnqueueFileMutation(t.Context(), path)
			require.NoError(t, err)
			require.Greater(t, drift.Generation, tool.Generation)
			unblock()
			result := awaitMutationRetryResult(t, tool)
			require.NoError(t, result.Err)
			require.Equal(t, tool.Generation, result.AppliedGeneration)
			select {
			case value := <-drift.Done:
				t.Fatalf("covering the tool consumed unexecuted drift repair: %+v", value)
			default:
			}
		})
	}
}
