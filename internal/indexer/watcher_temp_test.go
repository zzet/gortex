package indexer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/thirdparty/fswatcher"

	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestIsGortexAtomicTemp(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"internal/mcp/tools_explore.go.gortex.tmp-1725706750",
		"/repo/internal/mcp/.gortex.tmp-42",
		`C:\\repo\\watcher.go.gortex.tmp-7`,
		`C:\\repo/mixed\\watcher.go.gortex.tmp-8`,
	} {
		if !isGortexAtomicTemp(path) {
			t.Errorf("isGortexAtomicTemp(%q) = false, want true", path)
		}
	}

	for _, path := range []string{
		"internal/mcp/tools_explore.go",
		"internal/mcp/gortex.tmp.go",
		"internal/mcp/file.tmp-42",
		"internal/mcp/file.go.gortex.tmp-",
		"internal/mcp/file.go.gortex.tmp-backup",
		"internal/mcp/file.go.gortex.tmp-42.go",
		"/repo/.gortex.tmp-42/real.go",
		`C:\\repo\\.gortex.tmp-42\\real.go`,
	} {
		if isGortexAtomicTemp(path) {
			t.Errorf("isGortexAtomicTemp(%q) = true, want false", path)
		}
	}
}

func TestWatcherHandleEventIgnoresAtomicTempSequenceAndProcessesTarget(t *testing.T) {
	root := t.TempDir()
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	watcher := &Watcher{
		indexer:         &Indexer{rootPath: root, registry: registry},
		pending:         make(map[string]*time.Timer),
		stormBatch:      make(map[string]ChangeKind),
		pendingScanDirs: make(map[string]struct{}),
	}
	watcher.config.DebounceMs = 60_000
	watcher.config.StormThreshold = 1_000
	watcher.config.StormWindowMs = 60_000
	// If a temp directory ever reaches enqueueDirScan, keep its drainer active
	// until cleanup so the state assertion below cannot race with completion.
	scanRelease := make(chan struct{})
	watcher.scanFn = func(map[string]struct{}) { <-scanRelease }
	defer close(scanRelease)

	target := filepath.Join(root, "sample.go")
	temp := target + ".gortex.tmp-42"
	tempDir := filepath.Join(root, ".gortex.tmp-43")
	if err := os.Mkdir(tempDir, 0o755); err != nil {
		t.Fatal(err)
	}
	events := []fswatcher.WatchEvent{
		{Path: temp, Types: []fswatcher.EventType{fswatcher.EventCreate}},
		{Path: temp, Types: []fswatcher.EventType{fswatcher.EventMod}},
		{Path: temp, Types: []fswatcher.EventType{fswatcher.EventRename}},
		{Path: temp, Types: []fswatcher.EventType{fswatcher.EventRemove}},
		{Path: tempDir, Types: []fswatcher.EventType{fswatcher.EventCreate}},
	}
	for _, event := range events {
		watcher.handleEvent(event)
		watcher.mu.Lock()
		pending := len(watcher.pending)
		watcher.mu.Unlock()
		if pending != 0 {
			t.Fatalf("temp %v event entered debounce queue: pending=%d", event.Types, pending)
		}
		watcher.stormMu.Lock()
		stormEvents, stormPaths, stormActive := len(watcher.eventTimes), len(watcher.stormBatch), watcher.stormActive
		watcher.stormMu.Unlock()
		if stormEvents != 0 || stormPaths != 0 || stormActive {
			t.Fatalf("temp %v event entered storm state: events=%d paths=%d active=%v", event.Types, stormEvents, stormPaths, stormActive)
		}
		watcher.reconcileMu.Lock()
		scanDirs, scanActive := len(watcher.pendingScanDirs), watcher.dirScanActive
		watcher.reconcileMu.Unlock()
		if scanDirs != 0 || scanActive {
			t.Fatalf("temp %v event entered directory scan: dirs=%d active=%v", event.Types, scanDirs, scanActive)
		}
	}

	for _, eventType := range []fswatcher.EventType{fswatcher.EventCreate, fswatcher.EventMod} {
		watcher.handleEvent(fswatcher.WatchEvent{Path: target, Types: []fswatcher.EventType{eventType}})
		watcher.mu.Lock()
		timer, queued := watcher.pending[target]
		if queued {
			delete(watcher.pending, target)
		}
		watcher.mu.Unlock()
		if !queued {
			t.Fatalf("target %s event did not enter debounce queue", eventType)
		}
		if !timer.Stop() {
			t.Fatalf("target %s debounce timer fired before assertion", eventType)
		}
	}
}
