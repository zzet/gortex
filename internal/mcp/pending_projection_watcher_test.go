package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
)

// A watcher event for a projected file marks its entry suspect as the event
// arrives, before any debounce, so the next lookup re-reads the file even
// when an external write kept its size and modification time.
func TestWatcherEventMarksPendingProjectionSuspect(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	content := []byte("package p\n\nfunc New() {}\n")
	require.NoError(t, os.WriteFile(path, content, 0o644))
	g := graph.New()
	idx := newProjectionTestIndexer(t, g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	prev := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = prev })
	stamp, err := statDiskStamp(path)
	require.NoError(t, err)
	e, err := projectFile(context.Background(), idx, path, content, 1, stamp)
	require.NoError(t, err)
	require.NoError(t, pendingFileProjections.publish(e))

	// A long debounce: the mark must not wait for it.
	w, err := indexer.NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 60_000}, zap.NewNop())
	require.NoError(t, err)
	s := newTestServer(t)
	s.graph = g
	s.SetWatcher(w)
	require.NoError(t, w.Start([]string{dir}))
	t.Cleanup(func() { _ = w.Stop() })

	require.NoError(t, os.WriteFile(path, []byte("package p\n\nfunc External() {}\n"), 0o644))
	require.Eventually(t, e.suspect.Load, 5*time.Second, 10*time.Millisecond,
		"the watcher event for %s did not mark its projection suspect", path)
}
