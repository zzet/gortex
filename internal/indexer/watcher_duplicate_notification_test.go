package indexer

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// TestWatcher_DuplicateNotificationDuringPatchStillAnnounces pins the
// announcement contract for a patch that raced a duplicate native
// notification.
//
// FSEvents reports one save as several notifications, so a second one for the
// same path routinely lands while the first debounced patch is still running.
// The reindex that pass already applied is real graph state, so dropping its
// GraphChangeEvent loses the change from Events(), from History(), and from
// the symbol-change callback — the newer generation cannot make up for it,
// because it finds the persisted receipt already matching and correctly does
// nothing.
//
// The duplicate is injected from inside the reindex rather than timed, so the
// window is exercised on every run instead of only on a loaded machine.
func TestWatcher_DuplicateNotificationDuringPatchStillAnnounces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, `package main

func Original() {}
`)

	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cfg := config.Default()
	cfg.Index.Workers = 1

	idx := New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	w, err := NewWatcher(idx, config.WatchConfig{
		Enabled:    true,
		Paths:      []string{dir},
		DebounceMs: 50,
		Exclude:    []string{"**/*.tmp", "**/.git/**"},
	}, zap.NewNop())
	require.NoError(t, err)

	// Armed only once the watcher is live, so the Darwin startup barrier
	// never sees an injected mutation. Installed before Start so the field
	// is never written while a patch goroutine reads it.
	var armed atomic.Bool
	var once sync.Once
	w.pointReindexRaw = func(p string, _ *PublicationStamps) (*IndexResult, error) {
		result, err := idx.incrementalPointWatcherPath(idx.rootPath, p)
		if armed.Load() {
			// The window between "graph mutated" and "is my generation
			// still current" is exactly where a repeated FSEvents
			// notification for the same save lands.
			once.Do(func() { w.scheduleFileMutation(p, ChangeModified) })
		}
		return result, err
	}

	require.NoError(t, w.Start([]string{dir}))
	t.Cleanup(func() { _ = w.Stop() })

	armed.Store(true)
	writeTestFile(t, path, `package main

func Duplicated() {}
`)

	ev := waitForEvent(t, w, 5*time.Second)
	assert.Equal(t, ChangeModified, ev.Kind)
	assert.Equal(t, path, ev.FilePath)

	require.Eventually(t, func() bool {
		return len(g.FindNodesByName("Duplicated")) > 0
	}, 5*time.Second, 20*time.Millisecond, "the applied reindex must be in the graph")

	require.NotEmpty(t, w.History(), "an applied patch must leave a history row")
}
