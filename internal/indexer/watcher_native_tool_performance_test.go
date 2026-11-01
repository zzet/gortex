//go:build performance

package indexer

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type nativeToolBarrier struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *nativeToolBarrier) unblock() { b.once.Do(func() { close(b.release) }) }

// This drives the real native watcher, disk writes and graph executor. The
// post-executor barrier makes native delivery observable instead of assuming a
// quiet sleep proves that an OS event was delivered. It is not a latency run.
func TestSequentialToolEditsOnRealWatcherSupersedeNothing(t *testing.T) {
	dir, idx, w := inertTestWatcher(t, "main.go", "package main\nfunc Original(){Callee()}\nfunc Callee(){}\n")
	w.config.DebounceMs = 150
	var current atomic.Pointer[nativeToolBarrier]
	w.pointMutationPatch = func(path string, kind ChangeKind, generation uint64) error {
		w.mu.Lock()
		tool := w.mutationWaiters[path][generation].tool
		w.mu.Unlock()
		err := w.patchGraph(path, kind, generation)
		if tool {
			barrier := current.Load()
			close(barrier.entered)
			<-barrier.release
		}
		return err
	}
	require.NoError(t, w.Start([]string{dir}))
	stop := sync.OnceFunc(func() { require.NoError(t, w.Stop()) })
	t.Cleanup(stop)
	t.Cleanup(func() {
		if barrier := current.Load(); barrier != nil {
			barrier.unblock()
		}
	})
	path := filepath.Join(dir, "main.go")
	const edits = 200
	for n := range edits {
		barrier := &nativeToolBarrier{entered: make(chan struct{}), release: make(chan struct{})}
		current.Store(barrier)
		w.mu.Lock()
		before := w.nextGeneration
		w.mu.Unlock()
		marker := fmt.Sprintf("ToolEdit%03d", n)
		writeTestFile(t, path, "package main\nfunc "+marker+"(){Callee()}\nfunc Callee(){}\n")
		ticket, err := w.EnqueueToolMutation(t.Context(), path)
		require.NoError(t, err)
		select {
		case <-barrier.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("real tool patch did not land")
		}
		require.Eventually(t, func() bool {
			w.mu.Lock()
			_, held := w.heldNative[path]
			w.mu.Unlock()
			// A native event may also have arrived before tool admission. Its quiet
			// timer was canceled by the tool, rather than superseding tool work.
			return held || ticket.Generation > before+1
		}, 2*time.Second, time.Millisecond, "real native delivery was not observed")
		barrier.unblock()
		result := awaitMutationRetryResult(t, ticket)
		require.NoError(t, result.Err)
		require.True(t, result.Reindexed)
		caller := nodeByName(t, idx, "main.go", marker)
		callee := nodeByName(t, idx, "main.go", "Callee")
		calls := 0
		for _, edge := range idx.graph.GetInEdges(callee.ID) {
			if edge.Kind == graph.EdgeCalls {
				calls++
				require.Equal(t, caller.ID, edge.From)
			}
		}
		require.Equal(t, 1, calls, "each accepted edit has its exact caller relation")
	}
	stop() // closes admission and joins all native/timer work before the census
	counters := w.MutationCounters()
	require.Zero(t, counters["generations_superseded"], "native echoes must not supersede any sequential tool generation")
	t.Logf("sequential real-watcher tool edits=%d counters=%v", edits, counters)
}
