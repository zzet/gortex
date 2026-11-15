package indexer

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/thirdparty/fswatcher"
)

// Freeze the scheduler clock as well as the filesystem timestamp. A native
// save arriving while the original tool callback is still returning must be
// compared by content, even though neither the clock nor file size advanced.
func TestToolEchoSameTickExternalSaveReachesGraph(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir, watcher := toolAdmissionFixture(t)
		watcher.config.DebounceMs = 5
		path := filepath.Join(dir, "main.go")
		tick := time.Now()
		require.NoError(t, os.WriteFile(path, []byte("package main\nfunc Next() {}\n"), 0600))
		require.NoError(t, os.Chtimes(path, tick, tick))
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
		<-landed
		require.NotNil(t, nodeByName(t, watcher.indexer, "main.go", "Next"))
		before, err := os.Stat(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, []byte("package main\nfunc Save() {}\n"), 0600))
		require.NoError(t, os.Chtimes(path, tick, tick))
		after, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, tick, time.Now(), "both writes occurred in one frozen clock tick")
		require.Equal(t, before.ModTime(), after.ModTime())
		require.Equal(t, before.Size(), after.Size())
		require.False(t, watcher.indexer.fileMtimeMatches(path))
		watcher.handleEvent(fswatcher.WatchEvent{Path: path, Types: []fswatcher.EventType{fswatcher.EventMod}})
		watcher.mu.Lock()
		generation, held := watcher.nextGeneration, watcher.heldNative[path]
		watcher.mu.Unlock()
		require.Equal(t, ticket.Generation, generation)
		require.Equal(t, ChangeModified, held, "the real native event is retained behind its tool owner")
		unblock()
		require.NoError(t, awaitMutationRetryResult(t, ticket).Err)
		time.Sleep(5 * time.Millisecond)
		synctest.Wait()
		require.NotNil(t, nodeByName(t, watcher.indexer, "main.go", "Save"))
		for _, node := range watcher.indexer.graph.GetFileNodes("main.go") {
			require.NotEqual(t, "Next", node.Name, "the stale tool graph must be replaced")
		}
		watcher.mu.Lock()
		generation = watcher.nextGeneration
		watcher.mu.Unlock()
		require.Greater(t, generation, ticket.Generation)
	})
}
