package indexer

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Hold only the real catch-up boundary: the first structural graph is already
// written, then a second physical tool save arrives before its resolver runs.
// Both callbacks still run the real parser/resolver/publication consumer.
func TestOverlappingToolReceiptsResolveEarlierNewSameFileCalls(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	base := "package main\nfunc Target() {}\n"
	first := base + "func Earlier() { Target() }\n"
	last := first + "func Later() { Target() }\n"
	require.NoError(t, os.WriteFile(path, []byte(base), 0600))
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "coalesced.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	core, logs := observer.New(zap.DebugLevel)
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	idx := New(store, registry, config.Default().Index, zap.New(core))
	t.Cleanup(idx.Close)
	_, err = idx.Index(root)
	require.NoError(t, err)
	require.NoError(t, idx.IndexFile(path))
	watcher, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.New(core))
	require.NoError(t, err)
	// Keep the native frontend alive but idle so this control identifies the
	// two explicit tool callbacks; the shared-file fixture exercises echoes.
	require.NoError(t, watcher.Start([]string{t.TempDir()}))
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var held atomic.Bool
	idx.incrementalCatchupHook = func(kind string, _ []string) {
		if kind == "resolve" && held.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	t.Cleanup(func() {
		unblock()
		require.NoError(t, watcher.Stop())
		if t.Failed() {
			for _, entry := range logs.All() {
				t.Logf("native pipeline %s: %v", entry.Message, entry.ContextMap())
			}
		}
	})
	require.NoError(t, os.WriteFile(path, []byte(first), 0600))
	one, err := watcher.EnqueueToolMutation(t.Context(), path)
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first real structural patch did not reach the held resolver boundary")
	}
	require.NotNil(t, store.GetNode("main.go::Earlier"))
	prior := store.GetOutEdges("main.go::Earlier")
	require.Len(t, prior, 1)
	require.Contains(t, []string{graph.UnresolvedMarker + "Target", "main.go::Target"}, prior[0].To)
	t.Logf("real pre-catch-up Earlier target: %s", prior[0].To)
	require.NoError(t, os.WriteFile(path, []byte(last), 0600))
	two, err := watcher.EnqueueToolMutation(t.Context(), path)
	require.NoError(t, err)
	require.Greater(t, two.Generation, one.Generation)
	unblock()
	for _, ticket := range []*MutationTicket{one, two} {
		result := awaitMutationRetryResult(t, ticket)
		require.NoError(t, result.Err)
		require.GreaterOrEqual(t, result.AppliedGeneration, ticket.Generation)
		require.False(t, ticket.TierState().RelationsAt.IsZero())
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, last, string(raw))
	for _, name := range []string{"Earlier", "Later"} {
		edges := store.GetOutEdges("main.go::" + name)
		require.Len(t, edges, 1)
		require.Equal(t, "main.go::Target", edges[0].To, "a completed own receipt must retain its real caller binding")
	}
}
