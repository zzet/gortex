package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func setupWatcher(t *testing.T) (string, *Indexer, *Watcher) {
	t.Helper()
	dir := t.TempDir()

	writeTestFile(t, filepath.Join(dir, "main.go"), `package main

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

	wcfg := config.WatchConfig{
		Enabled:    true,
		Paths:      []string{dir},
		DebounceMs: 50, // short debounce for tests
		Exclude:    []string{"**/*.tmp", "**/.git/**"},
	}

	w, err := NewWatcher(idx, wcfg, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, w.Start([]string{dir}))

	t.Cleanup(func() { _ = w.Stop() })
	return dir, idx, w
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// TestWatcher_ShippedDefaultStillWatches drives config.Default().Watch —
// Enabled: false, exactly what every repo under `gortex daemon` gets with
// no override — through Start() and asserts a file change still reaches
// the graph. Every other watcher test in this package hardcodes
// Enabled: true, so none of them exercise the daemon's actual default;
// that gap once let Enabled: false silently disable fsnotify itself
// (not just the adaptive poller) without any test catching it.
func TestWatcher_ShippedDefaultStillWatches(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "main.go"), `package main

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

	wcfg := cfg.Watch
	require.False(t, wcfg.Enabled, "config.Default().Watch must ship Enabled: false")
	wcfg.Paths = []string{dir}
	wcfg.DebounceMs = 50 // short debounce for tests

	w, err := NewWatcher(idx, wcfg, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, w.Start([]string{dir}))
	t.Cleanup(func() { _ = w.Stop() })

	assert.Nil(t, w.poller,
		"the shipped default disables the adaptive poller only, not fsnotify")

	writeTestFile(t, filepath.Join(dir, "main.go"), `package main

func Modified() {}
`)

	ev := waitForEvent(t, w, 2*time.Second)
	assert.Equal(t, ChangeModified, ev.Kind)
	assert.NotEmpty(t, idx.graph.FindNodesByName("Modified"),
		"a file change under the shipped watch default must reach the graph via fsnotify")
}

func waitForEvent(t *testing.T, w *Watcher, timeout time.Duration) GraphChangeEvent {
	t.Helper()
	select {
	case ev := <-w.Events():
		return ev
	case <-time.After(timeout):
		w.mu.Lock()
		nextGeneration := w.nextGeneration
		pending := len(w.pending)
		pendingGenerations := len(w.pendingGeneration)
		waiters := len(w.mutationWaiters)
		w.mu.Unlock()
		t.Fatalf("timeout waiting for watcher event: next_generation=%d pending=%d pending_generations=%d waiters=%d history=%d",
			nextGeneration, pending, pendingGenerations, waiters, len(w.History()))
		return GraphChangeEvent{}
	}
}

func TestWatcher_OverflowReconcileUsesBatchCoordinator(t *testing.T) {
	_, _, w := setupWatcher(t)
	called := make(chan []string, 1)
	w.batchReindex = func(paths []string) (*IndexResult, error) {
		called <- paths
		return &IndexResult{}, nil
	}

	w.triggerOverflowReconcile("test")
	select {
	case paths := <-called:
		assert.Nil(t, paths, "overflow must request a full-tree batch reconcile")
	case <-time.After(2 * time.Second):
		t.Fatal("overflow reconcile did not invoke the batch coordinator")
	}
	w.asyncWork.Wait()
}

func TestWatcher_OverflowDuringReconcileSchedulesOneFollowUp(t *testing.T) {
	_, _, w := setupWatcher(t)
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var stateMu sync.Mutex
	calls, active, maxActive := 0, 0, 0
	w.reconcileFn = func() {
		stateMu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		calls++
		call := calls
		stateMu.Unlock()
		defer func() {
			stateMu.Lock()
			active--
			stateMu.Unlock()
		}()

		switch call {
		case 1:
			close(firstStarted)
			<-releaseFirst
		case 2:
			close(secondStarted)
		}
	}

	w.triggerOverflowReconcile("first")
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first overflow reconcile did not start")
	}

	for range 3 {
		w.triggerOverflowReconcile("during-walk")
	}
	select {
	case <-secondStarted:
		t.Fatal("overflow follow-up ran concurrently with the active reconcile")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseFirst)
	select {
	case <-secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("overflow during the active walk did not schedule a follow-up")
	}
	w.asyncWork.Wait()

	stateMu.Lock()
	defer stateMu.Unlock()
	require.Equal(t, 2, calls, "a burst during one walk must coalesce into one follow-up")
	require.Equal(t, 1, maxActive, "overflow reconciliation must remain single-flight")
}

func TestWatcher_FileModify(t *testing.T) {
	dir, idx, w := setupWatcher(t)

	require.NotEmpty(t, idx.graph.FindNodesByName("Original"))

	// Modify the file.
	writeTestFile(t, filepath.Join(dir, "main.go"), `package main

func Modified() {}
`)

	ev := waitForEvent(t, w, 2*time.Second)
	assert.Equal(t, ChangeModified, ev.Kind)

	// Graph should reflect the change.
	assert.Empty(t, idx.graph.FindNodesByName("Original"))
	assert.NotEmpty(t, idx.graph.FindNodesByName("Modified"))
}

func TestWatcher_FileCreate(t *testing.T) {
	dir, idx, w := setupWatcher(t)

	nodesBefore := idx.graph.NodeCount()

	writeTestFile(t, filepath.Join(dir, "new.go"), `package main

func NewFunc() {}
`)

	ev := waitForEvent(t, w, 2*time.Second)
	// fsnotify may emit CREATE or WRITE depending on the OS.
	assert.Contains(t, []ChangeKind{ChangeCreated, ChangeModified}, ev.Kind)
	assert.Greater(t, idx.graph.NodeCount(), nodesBefore)
	assert.NotEmpty(t, idx.graph.FindNodesByName("NewFunc"))
}

func TestWatcher_FileDelete(t *testing.T) {
	dir, idx, w := setupWatcher(t)

	require.NotEmpty(t, idx.graph.FindNodesByName("Original"))

	require.NoError(t, os.Remove(filepath.Join(dir, "main.go")))

	ev := waitForEvent(t, w, 2*time.Second)
	assert.Equal(t, ChangeDeleted, ev.Kind)
	assert.Empty(t, idx.graph.FindNodesByName("Original"))
}

func TestWatcher_History(t *testing.T) {
	dir, _, w := setupWatcher(t)

	writeTestFile(t, filepath.Join(dir, "main.go"), `package main

func Changed() {}
`)
	_ = waitForEvent(t, w, 2*time.Second)

	history := w.History()
	require.Len(t, history, 1)
	assert.Equal(t, ChangeModified, history[0].Kind)
}

func TestWatcher_SymbolChangeCallback_Modify(t *testing.T) {
	dir, _, w := setupWatcher(t)

	type callbackData struct {
		filePath   string
		oldSymbols []*graph.Node
		newSymbols []*graph.Node
	}

	callbackDone := make(chan callbackData, 1)
	w.OnSymbolChange(func(filePath string, oldSymbols, newSymbols []*graph.Node) {
		callbackDone <- callbackData{filePath, oldSymbols, newSymbols}
	})

	// Modify the file — changes function name.
	writeTestFile(t, filepath.Join(dir, "main.go"), `package main

func Modified() {}
`)
	_ = waitForEvent(t, w, 2*time.Second)

	var call callbackData
	select {
	case call = <-callbackDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for symbol-change callback")
	}

	// Old symbols should contain "Original", new should contain "Modified".
	var oldNames, newNames []string
	for _, n := range call.oldSymbols {
		oldNames = append(oldNames, n.Name)
	}
	for _, n := range call.newSymbols {
		if n.Kind != graph.KindFile && n.Kind != graph.KindImport {
			newNames = append(newNames, n.Name)
		}
	}
	assert.Contains(t, oldNames, "Original")
	assert.Contains(t, newNames, "Modified")
}

func TestWatcher_SymbolChangeCallback_Delete(t *testing.T) {
	dir, _, w := setupWatcher(t)
	require.NotEmpty(t, w.snapshotSymbols("main.go"), "setup must index Original before watching deletion")

	type callbackData struct {
		filePath   string
		oldSymbols []*graph.Node
		newSymbols []*graph.Node
	}

	callbackDone := make(chan callbackData, 1)
	w.OnSymbolChange(func(filePath string, oldSymbols, newSymbols []*graph.Node) {
		callbackDone <- callbackData{filePath, oldSymbols, newSymbols}
	})

	require.NoError(t, os.Remove(filepath.Join(dir, "main.go")))
	_ = waitForEvent(t, w, 2*time.Second)

	var call callbackData
	select {
	case call = <-callbackDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for symbol-change callback")
	}

	// Old symbols should have entries, new should be nil (deleted).
	assert.NotEmpty(t, call.oldSymbols, "callback=%+v", call)
	assert.Nil(t, call.newSymbols)
}

func TestWatcher_DirScanDeleteOverlapPublishesPrimaryOnce(t *testing.T) {
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

	w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 50}, zap.NewNop())
	require.NoError(t, err)

	type callbackData struct {
		oldSymbols []*graph.Node
		newSymbols []*graph.Node
	}
	var calls []callbackData
	w.OnSymbolChange(func(_ string, oldSymbols, newSymbols []*graph.Node) {
		calls = append(calls, callbackData{oldSymbols: oldSymbols, newSymbols: newSymbols})
	})

	// Force the bad ordering deterministically: the file disappears, a
	// new-directory discovery scan observes that absence first, and the real
	// file event is delivered only afterward. Discovery must not consume the
	// deletion or its pre-delete symbol snapshot.
	require.NoError(t, os.Remove(path))
	w.runDirScan(map[string]struct{}{dir: {}}, nil)
	require.NotEmpty(t, g.FindNodesByName("Original"))

	_ = w.patchGraph(path, ChangeDeleted)
	_ = w.patchGraph(path, ChangeDeleted) // redundant producer is suppressed

	require.Len(t, calls, 1)
	require.NotEmpty(t, calls[0].oldSymbols)
	oldNames := make([]string, 0, len(calls[0].oldSymbols))
	for _, symbol := range calls[0].oldSymbols {
		oldNames = append(oldNames, symbol.Name)
	}
	assert.Contains(t, oldNames, "Original")
	assert.Nil(t, calls[0].newSymbols)

	select {
	case ev := <-w.Events():
		assert.Equal(t, ChangeDeleted, ev.Kind)
		assert.Positive(t, ev.NodesRemoved)
	default:
		t.Fatal("primary delete event was not published")
	}
	select {
	case ev := <-w.Events():
		t.Fatalf("duplicate delete event published: %+v", ev)
	default:
	}
}

func TestWatcher_DirScanWaitsForPointMutationRepositoryLane(t *testing.T) {
	dir, idx, watcher := inertTestWatcher(t, "main.go", "package main\n\nfunc value() int { return 0 }\n")
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\n\nfunc value() int { return 1 }\nfunc added() {}\n")

	pointTailEntered := make(chan struct{})
	releasePoint := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releasePoint)
		}
	}()
	watcher.pointReindexRaw = func(filePath string) (*IndexResult, error) {
		result, err := idx.incrementalPointWatcherPath(idx.rootPath, filePath)
		close(pointTailEntered)
		<-releasePoint
		return result, err
	}
	pointDone := make(chan error, 1)
	go func() {
		pointDone <- watcher.patchGraph(path, ChangeModified)
	}()
	select {
	case <-pointTailEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("point patch did not reach its coordinated tail")
	}

	newDir := filepath.Join(dir, "nested")
	require.NoError(t, os.MkdirAll(newDir, 0o755))
	writeTestFile(t, filepath.Join(newDir, "discovered.go"), "package nested\n\nfunc Discovered() {}\n")
	scanStarted := make(chan struct{})
	scanDone := make(chan struct{})
	go func() {
		close(scanStarted)
		watcher.runDirScan(map[string]struct{}{newDir: {}}, nil)
		close(scanDone)
	}()
	<-scanStarted

	select {
	case <-scanDone:
		t.Fatal("directory discovery overlapped an admitted point patch")
	case <-time.After(100 * time.Millisecond):
	}
	require.Empty(t, idx.graph.FindNodesByName("Discovered"))

	close(releasePoint)
	released = true
	select {
	case err := <-pointDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("point patch did not finish after releasing its tail")
	}
	select {
	case <-scanDone:
	case <-time.After(2 * time.Second):
		t.Fatal("directory discovery did not acquire the repository lane")
	}
	require.NotEmpty(t, idx.graph.FindNodesByName("Discovered"))
}

// TestStormThresholdDefaultsOn pins the tri-state: batching is the default
// because the per-file path arms one timer, and so one goroutine, per
// changed path — and Go never returns a goroutine's stack descriptor to
// the heap, making a burst's peak a permanent cost.
func TestStormThresholdDefaultsOn(t *testing.T) {
	tmp := t.TempDir()

	// Unset (zero) takes the built-in default rather than disabling.
	w, err := NewWatcher(&Indexer{rootPath: tmp}, config.WatchConfig{Paths: []string{tmp}}, zap.NewNop())
	require.NoError(t, err)
	require.Equal(t, config.DefaultStormThreshold(), w.config.StormThreshold)
	require.Positive(t, w.config.StormThreshold, "storm batching must be on by default")

	// An explicit positive value wins.
	w, err = NewWatcher(&Indexer{rootPath: tmp},
		config.WatchConfig{Paths: []string{tmp}, StormThreshold: 7}, zap.NewNop())
	require.NoError(t, err)
	require.Equal(t, 7, w.config.StormThreshold)

	// A negative value is the explicit opt-out, coerced to the disabled zero.
	w, err = NewWatcher(&Indexer{rootPath: tmp},
		config.WatchConfig{Paths: []string{tmp}, StormThreshold: -1}, zap.NewNop())
	require.NoError(t, err)
	require.Zero(t, w.config.StormThreshold)
	require.False(t, w.shouldEnterStorm(), "a disabled watcher never enters storm mode")
}

// TestMutationAdmissionBoundsConcurrency verifies the cohort semaphore
// caps how many debounce callbacks are in the patch path at once, and
// that a stopping watcher releases waiters instead of parking them.
func TestMutationAdmissionBoundsConcurrency(t *testing.T) {
	w := &Watcher{logger: zap.NewNop(), done: make(chan struct{})}

	slots := cap(w.mutationSlots())
	require.Equal(t, mutationWorkSlots(), slots)
	require.GreaterOrEqual(t, slots, 2)

	releases := make([]func(), 0, slots)
	for i := 0; i < slots; i++ {
		release, err := w.admitMutationWork("held.go")
		require.NoError(t, err, "the first %d admissions must succeed", slots)
		releases = append(releases, release)
	}

	// The semaphore is full: a further admission blocks until a slot frees.
	admitted := make(chan error, 1)
	go func() {
		release, err := w.admitMutationWork("queued.go")
		if err == nil {
			release()
		}
		admitted <- err
	}()
	select {
	case <-admitted:
		t.Fatal("admission succeeded while every slot was held")
	case <-time.After(50 * time.Millisecond):
	}

	releases[0]()
	require.NoError(t, <-admitted, "freeing a slot admits the waiter")
	for _, release := range releases[1:] {
		release()
	}
}

func TestMutationAdmissionReleasedOnStop(t *testing.T) {
	w := &Watcher{logger: zap.NewNop(), done: make(chan struct{})}
	held := make([]func(), 0, cap(w.mutationSlots()))
	for i := 0; i < cap(w.mutationSlots()); i++ {
		release, err := w.admitMutationWork("held.go")
		require.NoError(t, err)
		held = append(held, release)
	}

	admitted := make(chan error, 1)
	go func() {
		_, err := w.admitMutationWork("stopping.go")
		admitted <- err
	}()
	close(w.done)
	require.ErrorIs(t, <-admitted, errWatcherStopped,
		"a stopping watcher must not admit new work")
	for _, release := range held {
		release()
	}
}

// TestDeferredPatchLoggingIsThrottled pins that a jammed lane reports itself
// once per window rather than once per deferred patch. A repo with a large
// generated tree can defer thousands of patches a minute; logging each one
// buries the storm-drain and reconcile records that explain the jam.
func TestDeferredPatchLoggingIsThrottled(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	w := &Watcher{logger: zap.New(core), done: make(chan struct{})}

	w.noteDeferredPatch("first.go", time.Second)
	require.Equal(t, 1, logs.Len(), "the first deferral must report immediately")

	for i := range 500 {
		w.noteDeferredPatch(fmt.Sprintf("burst-%d.go", i), time.Second)
	}
	require.Equal(t, 1, logs.Len(), "deferrals inside the window must not each log")

	// Reopening the window publishes the suppressed count and a sample.
	w.shedMu.Lock()
	w.shedWindowStart = time.Now().Add(-2 * mutationShedLogInterval)
	w.shedMu.Unlock()
	w.noteDeferredPatch("last.go", time.Second)

	require.Equal(t, 2, logs.Len())
	entry := logs.All()[1]
	fields := entry.ContextMap()
	require.Equal(t, int64(501), fields["deferred"],
		"the throttled line must account for every suppressed deferral")
	require.Equal(t, "burst-0.go", fields["sample_path"],
		"the sample must name a real deferred path")
	require.Contains(t, fields, "window")

	// The first line carries no window — there is no prior one to measure.
	require.NotContains(t, logs.All()[0].ContextMap(), "window")
	require.Equal(t, int64(1), logs.All()[0].ContextMap()["deferred"])
}

// TestDeferredPatchLoggingSurvivesNilLogger keeps the accounting path inert
// for a watcher built without a logger rather than panicking on a deferral.
func TestDeferredPatchLoggingSurvivesNilLogger(t *testing.T) {
	require.NotPanics(t, func() { (&Watcher{}).noteDeferredPatch("x.go", time.Second) })
	require.NotPanics(t, func() { (*Watcher)(nil).noteDeferredPatch("x.go", time.Second) })
}

// TestMutationLaneContextCarriesDeadline pins the escape hatch: a patch
// waiting on a stuck repository lane must not park a goroutine forever.
func TestMutationLaneContextCarriesDeadline(t *testing.T) {
	w := &Watcher{logger: zap.NewNop(), done: make(chan struct{})}
	ctx, cancel := w.mutationLaneContext()
	defer cancel()

	deadline, ok := ctx.Deadline()
	require.True(t, ok, "the lane wait must be bounded")
	require.WithinDuration(t, time.Now().Add(mutationLaneTimeout), deadline, time.Minute)

	// Stopping the watcher releases the wait immediately.
	close(w.done)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stopping the watcher did not cancel the lane wait")
	}
}

// TestWatcherExcludeFilterDropsIgnoredTrees pins that the backend-side
// filter agrees with the watcher's own exclude matcher, so bulk traffic in
// .git / node_modules is dropped before it reaches the event channel.
func TestWatcherExcludeFilterDropsIgnoredTrees(t *testing.T) {
	root := t.TempDir()
	w, err := NewWatcher(&Indexer{rootPath: root}, config.WatchConfig{Paths: []string{root}}, zap.NewNop())
	require.NoError(t, err)

	filter := &watcherExcludeFilter{w: w}
	for _, rel := range []string{"node_modules/pkg/index.js", ".git/objects/ab/cdef"} {
		require.False(t, filter.ShouldInclude(filepath.Join(root, rel)),
			"%s should be filtered at the backend", rel)
	}
	for _, rel := range []string{"main.go", "internal/pkg/service.go"} {
		require.True(t, filter.ShouldInclude(filepath.Join(root, rel)),
			"%s must still be delivered", rel)
	}

	// A nil-matcher watcher is inert rather than filtering everything out.
	require.True(t, (&watcherExcludeFilter{}).ShouldInclude("/anything"))
}
