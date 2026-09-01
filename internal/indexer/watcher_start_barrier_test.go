package indexer

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/thirdparty/fswatcher"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

type startupBarrierFakeWatcher struct {
	events  chan fswatcher.WatchEvent
	dropped chan fswatcher.WatchEvent
}

type startupReceiptCountingGraph struct {
	*graph.Graph
	fileMetaCalls int
	fileMetaPaths int
}

func (g *startupReceiptCountingGraph) FileMetasByPaths(repoPrefix string, filePaths []string) (map[string]graph.FileMetaRow, error) {
	g.fileMetaCalls++
	g.fileMetaPaths += len(filePaths)
	return g.Graph.FileMetasByPaths(repoPrefix, filePaths)
}

func newStartupBarrierFakeWatcher() *startupBarrierFakeWatcher {
	return &startupBarrierFakeWatcher{
		events:  make(chan fswatcher.WatchEvent, 16),
		dropped: make(chan fswatcher.WatchEvent, 1),
	}
}

func (*startupBarrierFakeWatcher) Watch(context.Context) error { return nil }
func (*startupBarrierFakeWatcher) AddPath(string, ...fswatcher.PathOption) error {
	return nil
}
func (*startupBarrierFakeWatcher) DropPath(string) error                  { return nil }
func (w *startupBarrierFakeWatcher) Events() <-chan fswatcher.WatchEvent  { return w.events }
func (w *startupBarrierFakeWatcher) Dropped() <-chan fswatcher.WatchEvent { return w.dropped }
func (*startupBarrierFakeWatcher) IsRunning() bool                        { return true }
func (*startupBarrierFakeWatcher) Stats() fswatcher.WatcherStats          { return fswatcher.WatcherStats{} }
func (*startupBarrierFakeWatcher) Paths() []string                        { return nil }
func (*startupBarrierFakeWatcher) Log(fswatcher.Severity, string, ...any) {}
func (*startupBarrierFakeWatcher) Close()                                 {}

func TestWatcherInitialReplayMarkerOrdersObservedTail(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	changedPath := filepath.Join(dir, "changed.go")
	stablePath := filepath.Join(dir, "stable.go")
	writeTestFile(t, changedPath, "package sample\n\nfunc Original() {}\n")
	writeTestFile(t, stablePath, "package sample\n\nfunc Stable() {}\n")

	g := graph.New()
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	cfg := config.Default()
	cfg.Index.Workers = 1
	idx := New(g, registry, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 1}, zap.NewNop())
	require.NoError(t, err)
	fake := newStartupBarrierFakeWatcher()
	w.fsw = fake
	var marker string
	w.initialReplayProbeWritten = func(path string) {
		marker = path
		// One late synthetic replay remains unchanged. A real edit then lands
		// before the ordered marker; both must be settled before Start returns.
		fake.events <- fswatcher.WatchEvent{Path: stablePath, Types: []fswatcher.EventType{fswatcher.EventCreate, fswatcher.EventMod}}
		writeTestFile(t, changedPath, "package sample\n\nfunc Modified() {}\n")
		fake.events <- fswatcher.WatchEvent{Path: changedPath, Types: []fswatcher.EventType{fswatcher.EventMod}}
		fake.events <- fswatcher.WatchEvent{Path: path, Types: []fswatcher.EventType{fswatcher.EventCreate, fswatcher.EventRemove}}
	}

	require.NoError(t, w.reconcileInitialReplayThroughMarkers([]string{dir}, time.Second))
	require.Equal(t, filepath.Join(dir, ".git"), filepath.Dir(marker))
	_, statErr := os.Stat(marker)
	require.ErrorIs(t, statErr, os.ErrNotExist)
	require.Empty(t, g.FindNodesByName("Original"))
	require.NotEmpty(t, g.FindNodesByName("Modified"))
	require.NotEmpty(t, g.FindNodesByName("Stable"))
	require.Len(t, w.History(), 1)
	require.Equal(t, "structural", waitForEvent(t, w, time.Second).Classification)

	w.mu.Lock()
	require.Zero(t, w.nextGeneration)
	require.Empty(t, w.pending)
	require.Empty(t, w.pendingGeneration)
	w.mu.Unlock()

	// The later unlink report is harmless even after the ordinary loop starts.
	w.handleEvent(fswatcher.WatchEvent{Path: marker, Types: []fswatcher.EventType{fswatcher.EventRemove}})
	w.mu.Lock()
	require.Zero(t, w.nextGeneration)
	w.mu.Unlock()
}

func TestWatcherInitialReplayMarkerTimeoutCleansMarker(t *testing.T) {
	dir := t.TempDir()
	idx := New(graph.New(), parser.NewRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRootPath(dir)
	w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 1}, zap.NewNop())
	require.NoError(t, err)
	w.fsw = newStartupBarrierFakeWatcher()
	var marker string
	w.initialReplayProbeWritten = func(path string) { marker = path }

	err = w.reconcileInitialReplayThroughMarkers([]string{dir}, 10*time.Millisecond)
	require.ErrorContains(t, err, "did not complete")
	require.NotEmpty(t, marker)
	_, statErr := os.Stat(marker)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestWatcherInitialReplayDrainPreservesRealWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package sample\n\nfunc Original() {}\n")

	g := graph.New()
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	idx := New(g, registry, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 1}, zap.NewNop())
	require.NoError(t, err)
	fake := newStartupBarrierFakeWatcher()
	w.fsw = fake
	w.initialReplayDrainStarted = func() {
		writeTestFile(t, path, "package sample\n\nfunc ModifiedDuringDrain() {}\n")
		fake.events <- fswatcher.WatchEvent{Path: path, Types: []fswatcher.EventType{fswatcher.EventMod}}
	}

	initial, err := w.drainInitialReplay(time.Millisecond)
	require.NoError(t, err)
	require.Contains(t, initial, path)
	w.initialReplayProbeWritten = func(marker string) {
		fake.events <- fswatcher.WatchEvent{Path: marker, Types: []fswatcher.EventType{fswatcher.EventCreate}}
	}
	require.NoError(t, w.reconcileInitialReplayThroughMarkers([]string{dir}, time.Second, initial))

	require.Empty(t, g.FindNodesByName("Original"))
	require.NotEmpty(t, g.FindNodesByName("ModifiedDuringDrain"))
}

func TestWatcherInitialReplayUsesOneReceiptQuery(t *testing.T) {
	dir := t.TempDir()
	paths := []string{
		filepath.Join(dir, "a.go"),
		filepath.Join(dir, "b.go"),
		filepath.Join(dir, "c.go"),
	}
	writeTestFile(t, paths[0], "package sample\n\nfunc OriginalA() {}\n")
	writeTestFile(t, paths[1], "package sample\n\nfunc StableB() {}\n")
	writeTestFile(t, paths[2], "package sample\n\nfunc StableC() {}\n")

	g := &startupReceiptCountingGraph{Graph: graph.New()}
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	idx := New(g, registry, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	g.fileMetaCalls = 0
	g.fileMetaPaths = 0

	// Preserve the indexed mtime so the changed file and the two synthetic
	// replay paths all enter the same ambiguous-receipt query. The changed hash
	// must still reconcile, without a second point lookup from patchGraph.
	info, err := os.Stat(paths[0])
	require.NoError(t, err)
	writeTestFile(t, paths[0], "package sample\n\nfunc ModifiedA() {}\n")
	require.NoError(t, os.Chtimes(paths[0], info.ModTime(), info.ModTime()))
	events := make(map[string]fswatcher.WatchEvent, len(paths))
	for _, path := range paths {
		events[path] = fswatcher.WatchEvent{
			Path:  path,
			Types: []fswatcher.EventType{fswatcher.EventCreate, fswatcher.EventMod},
		}
	}
	w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 1}, zap.NewNop())
	require.NoError(t, err)

	require.NoError(t, w.reconcileInitialReplayEvents(events))
	require.Equal(t, 1, g.fileMetaCalls, "startup replay receipts must be fetched set-wise")
	require.Equal(t, len(paths), g.fileMetaPaths)
	require.Empty(t, g.FindNodesByName("OriginalA"))
	require.NotEmpty(t, g.FindNodesByName("ModifiedA"))
	require.Len(t, w.History(), 1, "unchanged replay paths must not enter patchGraph")
}

func TestWatcherInitialReplayMarkerPermissionFailureIsFailClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permission failure is not enforceable as root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package sample\n\nfunc Original() {}\n")
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	idx := New(graph.New(), parser.NewRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRootPath(dir)
	w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 1}, zap.NewNop())
	require.NoError(t, err)
	w.fsw = newStartupBarrierFakeWatcher()
	writeSucceeded := false
	w.initialReplayMarkerCreating = func(string) {
		// Directory mode forbids creating the marker, but the existing 0644
		// source remains writable. Silently skipping the barrier would lose this
		// exact concurrent edit.
		require.NoError(t, os.WriteFile(path, []byte("package sample\n\nfunc ChangedDuringMarker() {}\n"), 0o644))
		writeSucceeded = true
	}

	err = w.reconcileInitialReplayThroughMarkers([]string{dir}, time.Second)
	require.ErrorContains(t, err, "create startup stream marker")
	require.True(t, writeSucceeded)
}

func TestWatcherStartDarwinMarkerIsImmediatelyUnlinked(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("FSEvents startup marker is Darwin-specific")
	}
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	writeTestFile(t, filepath.Join(dir, "main.go"), "package sample\n\nfunc Value() {}\n")

	g := graph.New()
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	cfg := config.Default()
	idx := New(g, registry, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 1}, zap.NewNop())
	require.NoError(t, err)
	start := time.Now()
	require.NoError(t, w.Start([]string{dir}))
	t.Cleanup(func() { _ = w.Stop() })
	t.Logf("Darwin watcher Start with ordered marker: %s", time.Since(start))

	entries, err := os.ReadDir(filepath.Join(dir, ".git"))
	require.NoError(t, err)
	for _, entry := range entries {
		require.False(t, strings.Contains(entry.Name(), probeMarker), "startup marker survived Start")
	}
}

func TestWatcherStartDarwinDirectoryWithoutCreatePermissionFailsClosed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("FSEvents startup marker is Darwin-specific")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package sample\n\nfunc Value() {}\n")

	g := graph.New()
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	cfg := config.Default()
	idx := New(g, registry, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 1}, zap.NewNop())
	require.NoError(t, err)
	markersWritten := 0
	w.initialReplayProbeWritten = func(string) { markersWritten++ }
	err = w.Start([]string{dir})
	require.ErrorContains(t, err, "create startup stream marker")
	require.Zero(t, markersWritten)
	require.NoError(t, w.Stop(), "direct cleanup after a pre-loop Start error must not wait forever")
}
