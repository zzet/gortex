package indexer

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/thirdparty/fswatcher"
)

func TestLandedSupersededPatchCompletesItsOwnWaiters(t *testing.T) {
	for _, storm := range []bool{false, true} {
		t.Run(map[bool]string{false: "point", true: "storm"}[storm], func(t *testing.T) {
			dir, watcher := toolAdmissionFixture(t)
			path := filepath.Join(dir, "main.go")
			writeTestFile(t, path, "package main\nfunc Landed() {}\n")
			successors := make(chan *MutationTicket, 1)
			errors := make(chan error, 1)
			admitSuccessor := func() error {
				if err := os.WriteFile(path, []byte("package main\nfunc Later() {}\n"), 0o644); err != nil {
					return err
				}
				ticket, err := watcher.EnqueueFileMutation(t.Context(), path)
				if err == nil {
					successors <- ticket
				}
				return err
			}
			var ticket *MutationTicket
			if storm {
				other := filepath.Join(dir, "other.go")
				writeTestFile(t, other, "package main\nfunc Other() {}\n")
				// This existing hook runs after the batch's actual executor
				// returned and before its waiter-completion boundary.
				watcher.stormDrained = func(int) { errors <- admitSuccessor() }
				tickets, err := watcher.EnqueueFileMutations(t.Context(), []string{path, other})
				require.NoError(t, err)
				ticket = tickets[path]
			} else {
				watcher.pointReindexRaw = func(file string, _ *PublicationStamps) (*IndexResult, error) {
					result, err := watcher.indexer.incrementalPointWatcherPath(dir, file)
					if err == nil {
						err = admitSuccessor()
					}
					errors <- err
					return result, err
				}
				var err error
				ticket, err = watcher.EnqueueToolMutation(t.Context(), path)
				require.NoError(t, err)
			}
			require.NotNil(t, ticket)
			select {
			case err := <-errors:
				require.NoError(t, err)
			case <-time.After(2 * time.Second):
				t.Fatal("the graph executor did not reach the landed boundary")
			}
			var successor *MutationTicket
			select {
			case successor = <-successors:
				require.NotNil(t, successor)
			case <-time.After(time.Second):
				t.Fatal("the post-patch successor was not admitted")
			}
			require.Greater(t, successor.Generation, ticket.Generation)
			require.NotNil(t, nodeByName(t, watcher.indexer, "main.go", "Landed"), "the older graph patch must actually have landed")
			select {
			case result := <-ticket.Done:
				require.NoError(t, result.Err)
				require.True(t, result.Reindexed)
				require.Equal(t, ticket.Generation, result.RequestedGeneration)
				require.Equal(t, ticket.Generation, result.AppliedGeneration, "only this patch has executed")
			case <-time.After(2 * time.Second):
				t.Fatal("landed patch left its own waiter dependent on a later debounced generation")
			}
			select {
			case result := <-successor.Done:
				t.Fatalf("covered completion consumed an unexecuted successor: %+v", result)
			default:
			}
			watcher.mu.Lock()
			pending := watcher.pendingGeneration[path]
			watcher.mu.Unlock()
			require.Equal(t, successor.Generation, pending, "covering the old receipt must preserve later pending work")
		})
	}
}

func TestEchoDuringToolPatchAttachesWithoutNewGeneration(t *testing.T) {
	dir, watcher := toolAdmissionFixture(t)
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\nfunc Landed() {}\n")
	landed, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock) // registered after the fixture, so Stop can join the callback
	watcher.pointReindexRaw = func(file string, _ *PublicationStamps) (*IndexResult, error) {
		result, err := watcher.indexer.incrementalPointWatcherPath(dir, file)
		close(landed)
		<-release
		return result, err
	}
	ticket, err := watcher.EnqueueToolMutation(t.Context(), path)
	require.NoError(t, err)
	require.NotNil(t, ticket)
	select {
	case <-landed:
	case <-time.After(2 * time.Second):
		t.Fatal("tool patch did not enter the held post-executor boundary")
	}
	watcher.handleEvent(fswatcher.WatchEvent{Path: path, Types: []fswatcher.EventType{fswatcher.EventMod}})
	watcher.mu.Lock()
	afterEcho := watcher.nextGeneration
	watcher.mu.Unlock()
	require.Equal(t, ticket.Generation, afterEcho, "an echo during tool work must attach without a successor ordinal")
	unblock()
	select {
	case result := <-ticket.Done:
		require.NoError(t, result.Err)
		require.True(t, result.Reindexed)
	case <-time.After(2 * time.Second):
		t.Fatal("the tool ticket did not finish after its patch was released")
	}
	joined := make(chan struct{})
	go func() { watcher.asyncWork.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("a content-identical held echo left another timer or callback outstanding")
	}
	watcher.mu.Lock()
	finalGeneration := watcher.nextGeneration
	watcher.mu.Unlock()
	require.Equal(t, ticket.Generation, finalGeneration, "receipt checking must not create a delayed echo generation")
}
