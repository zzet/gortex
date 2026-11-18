package indexer

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func toolAdmissionFixture(t *testing.T) (string, *Watcher) {
	t.Helper()
	dir, _, watcher := inertTestWatcher(t, "main.go", "package main\nfunc Main() {}\n")
	// This fixture has no native watch loop. Stop still owns and joins every
	// point/storm callback, but must not wait for a loop that was never started.
	watcher.degradedNoFsnotify = true
	watcher.config.DebounceMs = 30_000
	stop := sync.OnceFunc(func() { require.NoError(t, watcher.Stop()) })
	t.Cleanup(stop)
	return dir, watcher
}

func TestMutationAdmissionUsesActualTimerDelay(t *testing.T) {
	for _, entry := range []string{"native", "tool", "singleton_set", "duplicate_set", "multi_file_set"} {
		t.Run(entry, func(t *testing.T) {
			dir, watcher := toolAdmissionFixture(t)
			path := filepath.Join(dir, "main.go")
			delays := make(chan time.Duration, 4)
			watcher.mutationTimerArmed = func(delay time.Duration) { delays <- delay }
			markers := make(chan bool, 1)
			watcher.pointMutationPatch = func(path string, _ ChangeKind, generation uint64) error {
				watcher.mu.Lock()
				tool := watcher.mutationWaiters[path][generation].tool
				watcher.mu.Unlock()
				markers <- tool
				return nil
			}
			watcher.stormBeforeLock = func() {
				watcher.mu.Lock()
				allTool := true
				for _, waiters := range watcher.mutationWaiters {
					for _, waiter := range waiters {
						allTool = allTool && waiter.tool
					}
				}
				watcher.mu.Unlock()
				markers <- allTool
			}
			// These are the actual arguments passed to time.AfterFunc, not a
			// timing benchmark or evidence for the separate 10ms acceptance gate.
			var err error
			switch entry {
			case "native":
				_, err = watcher.EnqueueFileMutation(t.Context(), path)
			case "tool":
				_, err = watcher.EnqueueToolMutation(t.Context(), path)
			case "singleton_set":
				_, err = watcher.EnqueueFileMutations(t.Context(), []string{path})
			case "duplicate_set":
				_, err = watcher.EnqueueFileMutations(t.Context(), []string{path, path})
			case "multi_file_set":
				other := filepath.Join(dir, "other.go")
				writeTestFile(t, other, "package main\nfunc Other() {}\n")
				_, err = watcher.EnqueueFileMutations(t.Context(), []string{path, other})
			}
			require.NoError(t, err)
			select {
			case delay := <-delays:
				if entry == "native" {
					require.Equal(t, 30*time.Second, delay)
				} else {
					require.Zero(t, delay)
				}
			default:
				t.Fatal("admission returned without arming its timer")
			}
			if entry == "native" {
				watcher.mu.Lock()
				tool := watcher.mutationWaiters[path][watcher.pendingGeneration[path]].tool
				watcher.mu.Unlock()
				require.False(t, tool)
			} else {
				select {
				case tool := <-markers:
					require.True(t, tool, "tool ownership must survive timer claim")
				case <-time.After(2 * time.Second):
					t.Fatal("zero-delay callback did not claim its admitted work")
				}
			}
		})
	}
}

func TestToolMutationAdmissionRejectsUnsupportedWork(t *testing.T) {
	for _, failure := range []string{"cancelled", "outside_root", "excluded", "unknown_language", "stopping"} {
		t.Run(failure, func(t *testing.T) {
			dir, watcher := toolAdmissionFixture(t)
			ctx := t.Context()
			path := filepath.Join(dir, "main.go")
			switch failure {
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "outside_root":
				path = filepath.Join(t.TempDir(), "outside.go")
			case "excluded":
				path = filepath.Join(dir, "node_modules", "ignored.go")
			case "unknown_language":
				path = filepath.Join(dir, "unknown.unsupported")
			case "stopping":
				watcher.mu.Lock()
				watcher.stopping = true
				watcher.mu.Unlock()
			}
			watcher.mutationTimerArmed = func(time.Duration) { t.Error("rejected work armed a timer") }
			ticket, err := watcher.EnqueueToolMutation(ctx, path)
			switch failure {
			case "cancelled":
				require.ErrorIs(t, err, context.Canceled)
			case "stopping":
				require.ErrorIs(t, err, errWatcherStopped)
			default:
				require.NoError(t, err)
			}
			require.Nil(t, ticket)
			watcher.mu.Lock()
			generation := watcher.nextGeneration
			watcher.mu.Unlock()
			require.Zero(t, generation, "rejected admission must publish no generation")
		})
	}
}

func TestMultiWatcherToolAdmissionPreservesOwnerRouting(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "unstarted", true: "started"}[started], func(t *testing.T) {
			dir, owner := toolAdmissionFixture(t)
			multi := &MultiWatcher{
				watchers: map[string]*Watcher{"repo": owner},
				started:  map[string]bool{"repo": started},
				multi: &MultiIndexer{repos: map[string]*RepoMetadata{
					"repo": {RepoPrefix: "repo", RootPath: dir},
				}},
			}
			delays := make(chan time.Duration, 1)
			owner.mutationTimerArmed = func(delay time.Duration) { delays <- delay }
			ticket, err := multi.EnqueueToolMutation(t.Context(), filepath.Join(dir, "main.go"))
			require.NoError(t, err)
			if !started {
				require.Nil(t, ticket)
				require.Empty(t, delays)
				return
			}
			require.NotNil(t, ticket)
			select {
			case delay := <-delays:
				require.Zero(t, delay)
			default:
				t.Fatal("started owner returned without arming the tool timer")
			}
		})
	}
}

func TestMutationAdmissionSetRejectsWholeFrontier(t *testing.T) {
	dir, watcher := toolAdmissionFixture(t)
	watcher.mutationTimerArmed = func(time.Duration) { t.Error("rejected frontier armed a timer") }
	tickets, err := watcher.EnqueueFileMutations(t.Context(), []string{
		filepath.Join(dir, "main.go"), filepath.Join(t.TempDir(), "outside.go"),
	})
	require.NoError(t, err)
	require.Nil(t, tickets)
	watcher.mu.Lock()
	generation := watcher.nextGeneration
	watcher.mu.Unlock()
	require.Zero(t, generation, "valid members must not escape a rejected set")
}
