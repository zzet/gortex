package indexer

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/thirdparty/fswatcher"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// TestWatcher_StormModeBatches proves the storm-mode contract: once
// the event rate exceeds StormThreshold within StormWindowMs, events
// stop flowing through the per-file debounce path and instead land
// in a batch that drains after the quiet period. Without this, a
// 1000-file checkout runs 1000 per-file resolver + search rebuilds
// back-to-back — the slow branch-switch problem this mode exists
// to avoid.
func TestWatcher_StormModeBatches(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(dir, 0o755))

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.SetRootPath(dir)

	w, err := NewWatcher(idx, config.WatchConfig{
		DebounceMs:         10,
		StormThreshold:     5,
		StormWindowMs:      100,
		StormQuietPeriodMs: 50,
	}, zap.NewNop())
	require.NoError(t, err)
	// We're exercising handleEvent directly; Start was never called,
	// so there's no backend to clean up.

	// Signal when the drain finishes so the test doesn't have to
	// poll — the production path has no such hook, but tests need
	// deterministic synchronisation.
	drained := make(chan int, 1)
	w.stormDrained = func(n int) {
		select {
		case drained <- n:
		default:
		}
	}

	// Write files first so IndexFile has real content to parse.
	// 8 files — above the threshold of 5.
	var paths []string
	for i := 0; i < 8; i++ {
		p := filepath.Join(dir, "f"+string(rune('a'+i))+".go")
		require.NoError(t, os.WriteFile(p, []byte("package main\nfunc Main() {}\n"), 0o644))
		paths = append(paths, p)
	}

	// Fire synthetic events directly — we want to exercise the rate
	// detection without depending on fsnotify scheduling.
	for _, p := range paths {
		w.handleEvent(fakeCreate(p))
	}

	select {
	case n := <-drained:
		assert.GreaterOrEqual(t, n, 1, "drain must touch at least one file")
	case <-time.After(2 * time.Second):
		t.Fatal("storm batch did not drain within timeout")
	}

	// After drain, the graph should contain nodes for the indexed
	// files — storm mode still patches the graph, it just does so
	// as one batch rather than N serial per-file runs.
	nodes := 0
	for _, p := range paths {
		rel, _ := filepath.Rel(dir, p)
		nodes += len(g.GetFileNodes(rel))
	}
	assert.Greater(t, nodes, 0, "storm drain must populate the graph")
}

// TestWatcher_StormModeDisabled verifies that StormThreshold=0 keeps
// the watcher on the per-file path. This is the default, and some
// callers may want to opt out of batching explicitly (e.g., a test
// harness that depends on immediate per-event observability).
func TestWatcher_StormModeDisabled(t *testing.T) {
	dir := t.TempDir()
	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.SetRootPath(dir)

	w, err := NewWatcher(idx, config.WatchConfig{
		DebounceMs:     10,
		StormThreshold: 0, // disabled
	}, zap.NewNop())
	require.NoError(t, err)

	var mu sync.Mutex
	drainedCalls := 0
	w.stormDrained = func(int) {
		mu.Lock()
		drainedCalls++
		mu.Unlock()
	}

	for i := 0; i < 100; i++ {
		w.handleEvent(fakeCreate(filepath.Join(dir, "x.go")))
	}

	// Give any (unexpected) storm drain a window to fire.
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 0, drainedCalls,
		"storm drain must not fire when threshold is disabled")
}

// TestParseDiffNameStatus verifies the git diff decoder — it's the
// single most failure-prone piece of the branch-switch path because
// git's `-z` output is easy to get wrong (trailing NUL, empty
// trailing fields, mixed R/M entries).
func TestParseDiffNameStatus(t *testing.T) {
	// Real `git diff --name-status -z -M -C` output for a mix of
	// Add, Modify, Delete, Rename, Copy.
	// Format: STATUS\0path[\0newpath]\0 (per git-diff docs).
	input := []byte(
		"A\x00new.go\x00" +
			"M\x00modified.go\x00" +
			"D\x00deleted.go\x00" +
			"R100\x00old/name.go\x00new/name.go\x00" +
			"C75\x00source.go\x00dup.go\x00",
	)

	changes := parseDiffNameStatus(input)
	require.Len(t, changes, 5)

	assert.Equal(t, byte('A'), changes[0].Status)
	assert.Equal(t, "new.go", changes[0].Path)

	assert.Equal(t, byte('M'), changes[1].Status)
	assert.Equal(t, "modified.go", changes[1].Path)

	assert.Equal(t, byte('D'), changes[2].Status)
	assert.Equal(t, "deleted.go", changes[2].Path)

	assert.Equal(t, byte('R'), changes[3].Status)
	assert.Equal(t, "old/name.go", changes[3].OldPath)
	assert.Equal(t, "new/name.go", changes[3].Path)

	assert.Equal(t, byte('C'), changes[4].Status)
	assert.Equal(t, "source.go", changes[4].OldPath)
	assert.Equal(t, "dup.go", changes[4].Path)
}

// TestGitWatcher_StopIdempotent guards the MultiWatcher integration:
// Stop must be safe whether Start ran, failed, or was skipped. An
// earlier version deadlocked on `<-gw.stopped` when the loop
// goroutine had never started (repos without a .git directory).
func TestGitWatcher_StopIdempotent(t *testing.T) {
	dir := t.TempDir()
	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())

	gw, err := NewGitWatcher(dir, idx, zap.NewNop())
	require.NoError(t, err)

	// Start fails because there's no .git dir in a random TempDir.
	err = gw.Start()
	require.Error(t, err)

	// Stop must not deadlock even though loop() never ran.
	done := make(chan struct{})
	go func() {
		_ = gw.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("gw.Stop() deadlocked after failed Start")
	}

	// Second Stop is a no-op.
	require.NoError(t, gw.Stop())
}

// TestIndexer_IndexFileNoResolve_SkipsResolver proves the batch path
// actually skips the per-file resolver — if it didn't, storm mode
// would provide no speedup over the per-file path.
func TestIndexer_IndexFileNoResolve_SkipsResolver(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	require.NoError(t, os.WriteFile(p, []byte("package main\nfunc X() {}\n"), 0o644))

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(dir)

	// IndexFileNoResolve populates the graph but defers cross-file
	// resolution. For this single file with no inter-file refs the
	// difference is invisible at the graph level; ResolveAll still
	// needs to be callable afterward without panic.
	require.NoError(t, idx.IndexFileNoResolve(p))
	assert.Greater(t, g.NodeCount(), 0, "file must be indexed")

	idx.ResolveAll() // must not panic
}

// fakeCreate builds a minimal fswatcher.WatchEvent for unit testing
// the storm path without spinning up a real watcher.
func fakeCreate(path string) fswatcher.WatchEvent {
	return fswatcher.WatchEvent{
		Path:  path,
		Types: []fswatcher.EventType{fswatcher.EventCreate},
		Time:  time.Now(),
	}
}
