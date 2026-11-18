package mcp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestDeferredReadFileCompressionNeverUsesOldKeepRanges(t *testing.T) {
	selectedProjectionRegistry(t)
	original := "package sample\nfunc Keep() {\n println(1)\n println(2)\n println(3)\n}\nfunc Other() {\n println(4)\n println(5)\n println(6)\n}\n"
	s, root := setupMoveInlineRepo(t, map[string]string{"sample.go": original})
	path := filepath.Join(root, "sample.go")
	current := "package sample\nfunc Intruder() {\n println(7)\n println(8)\n println(9)\n}\n" + strings.TrimPrefix(original, "package sample\n")
	require.NoError(t, os.WriteFile(path, []byte(current), 0600))
	s.SetWatcher(projectedStormWatcher{path: path})
	req := makeReq("read_file", map[string]any{"path": "sample.go", "compress_bodies": true, "keep": "sample.go::Keep", "format": "json"})
	result, err := s.wrapToolHandler(s.handleReadFile)(t.Context(), req)
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", resultText(result))
	fields := extractTextResult(t, result)
	require.Equal(t, current, fields["content"], "unavailable current ranges cannot authorize graph-dependent elision")
	require.Equal(t, "pending", fields["compression_state"])
	require.Empty(t, fields["kept_symbols"])
	require.Empty(t, pendingFileProjections.snap.Load().entries)
	require.Nil(t, s.graph.GetNode("sample.go::Intruder"))
	freshness := fields["freshness"].(map[string]any)
	require.Equal(t, "storm", freshness["file_reason"])
	require.NotEqual(t, true, freshness["readback_exact"])
}

func TestProjectedSourceInvalidationCannotPoisonPublisherHoldingLock(t *testing.T) {
	selectedProjectionRegistry(t)
	p := pendingFileProjections
	path := filepath.Join(t.TempDir(), "sample.go")
	rawA := []byte("package sample\nfunc A() {}\n")
	rawB := []byte("package sample\nfunc B() {}\n")
	require.NoError(t, os.WriteFile(path, rawA, 0600))
	stampA, err := statDiskStamp(path)
	require.NoError(t, err)
	a := syntheticProjectionEntry(path, "sample.go", "a", 0, stampA, rawA)
	require.NoError(t, p.publishExpected(a, nil))
	require.NoError(t, os.WriteFile(path, rawB, 0600))
	stampB, err := statDiskStamp(path)
	require.NoError(t, err)
	b := syntheticProjectionEntry(path, "sample.go", "b", 0, stampB, rawB)
	// Model the real publisher after its expected-old-pointer check, before
	// publishLocked. The invalidator must serialize at that same boundary.
	p.mu.Lock()
	var unlock sync.Once
	release := func() { unlock.Do(p.mu.Unlock) }
	done := make(chan struct{})
	id := make(chan string, 1)
	go func() {
		defer close(done)
		stack := make([]byte, 256)
		n := runtime.Stack(stack, false)
		id <- strings.Fields(string(stack[:n]))[1]
		_ = invalidateProjectedSource(a)
	}()
	t.Cleanup(func() { release(); <-done })
	goroutine := <-id
	require.Eventually(t, func() bool {
		stack := make([]byte, 1<<20)
		n := runtime.Stack(stack, true)
		for _, block := range strings.Split(string(stack[:n]), "\n\n") {
			if strings.HasPrefix(block, "goroutine "+goroutine+" ") {
				return strings.Contains(block, "invalidateProjectedSource") && strings.Contains(block, "Mutex")
			}
		}
		return false
	}, time.Second, time.Millisecond, "actual invalidation must be waiting at the held publication lock")
	markedBeforePublish := a.suspect.Load()
	err = p.publishLocked(b)
	release()
	<-done
	require.NoError(t, err)
	require.False(t, markedBeforePublish, "invalidation cannot mark the old entry outside publication synchronization")
	require.Same(t, b, p.snap.Load().entries[b.key])
	require.False(t, b.suspect.Load(), "a stale invalidator must not poison the newer publisher")
	require.True(t, liveProjectionForAcknowledgement(b))
	require.False(t, liveProjectionForAcknowledgement(a))
}

func TestSymbolSourceMixedRetryPathsShareTwoActualAttempts(t *testing.T) {
	selectedProjectionRegistry(t)
	s, root := setupMoveInlineRepo(t, map[string]string{"sample.go": "package sample\nfunc Probe() int { return 0 }\n"})
	s.SetWatcher(mutationTestWatcher{})
	path := filepath.Join(root, "sample.go")
	require.NoError(t, os.WriteFile(path, []byte("package sample\nfunc Probe() int { return 1 }\n"), 0600))
	calls := 0
	var deadlines []time.Time
	leaf := func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		calls++
		deadlines = append(deadlines, ctx.Value(projectedSourceDeadlineKey{}).(time.Time))
		var err error
		ctx, err = s.ensureProjected(ctx, []string{"sample.go"})
		if err != nil {
			return nil, err
		}
		if calls == 1 {
			info, err := os.Stat(path)
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(path, []byte("package sample\nfunc Probe() int { return 2 }\n"), 0600); err != nil {
				return nil, err
			}
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				return nil, err
			}
		}
		result, err := s.handleGetSymbolSource(ctx, req)
		if calls == 2 && err == nil && result != nil && !result.IsError {
			// The same late-withdrawal seam exercised by fresh_symbol_retry_test:
			// a completed source attempt must consume the outer replay allowance too.
			ctx.Value(freshSymbolAttemptKey{}).(*freshSymbolAttempt).withdrawn = true
		}
		return result, err
	}
	args := map[string]any{"id": "sample.go::Probe", "context_lines": 0, "format": "json", requireExactArgName: true, requireFreshArgName: true, waitDeadlineArgName: time.Now().Add(time.Second).Format(time.RFC3339Nano)}
	// Exercise both real replay controllers over the actual source handler.
	// This flat fixture has no working-copy generation coordinator, so the
	// public require_fresh route would correctly refuse before any leaf.
	result, err := s.retryFreshSymbolSearch(s.retryProjectedSymbolSource(leaf))(t.Context(), makeReq("get_symbol_source", args))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, calls, "typed mismatch plus late withdrawal cannot permit a third actual handler invocation")
	require.True(t, result.IsError, "%s", resultText(result))
	for _, deadline := range deadlines {
		require.Equal(t, deadlines[0], deadline, "outer replay cannot renew the original projection budget")
	}
	require.NotContains(t, resultText(result), "readback_exact")
	require.Equal(t, "sample.go::Probe", args["id"])
}

// Reuse the real Server.SetWatcher callback seam; the actual native write is
// deliberately same-length/same-mtime, so an event must invalidate the ACK view.
type externalAfterToolWatcher struct {
	mutationTestWatcher
	sourceEvent func(string)
}

func (w *externalAfterToolWatcher) OnSourceEvent(callback func(string)) { w.sourceEvent = callback }

func TestExternalWriteAfterToolEdit(t *testing.T) {
	selectedProjectionRegistry(t)
	s, root := ackModeServer(t, "local")
	pending := make(chan indexer.MutationResult)
	defer close(pending)
	watcher := &externalAfterToolWatcher{mutationTestWatcher: mutationTestWatcher{done: pending}}
	s.SetWatcher(watcher)
	path := filepath.Join(root, "main.go")
	replacement := "func NativeAfterTool() int { return 1 }"
	req := makeReq("edit_file", map[string]any{"path": path, "old_string": "func helper() {}", "new_string": replacement, "format": "json"})
	result, err := s.runAcknowledgedEdit(t.Context(), req, s.handleEditFile)
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", resultText(result))
	ack := extractTextResult(t, result)
	require.Equal(t, "committed", ack["disk_status"])
	require.Equal(t, "local", ack["ack_mode"])
	old := pendingFileProjections.snap.Load().entries[pathkey.MutationKey(path)]
	require.NotNil(t, old)
	require.Positive(t, old.commitSeq)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	current := strings.Replace(string(raw), replacement, "func NativeAfterTool() int { return 2 }", 1)
	require.NotEqual(t, string(raw), current)
	require.NoError(t, os.WriteFile(path, []byte(current), 0600))
	require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
	require.NotNil(t, watcher.sourceEvent)
	watcher.sourceEvent(path)
	require.True(t, old.suspect.Load())
	read := makeReq("get_symbol_source", map[string]any{"id": "main.go::NativeAfterTool", "context_lines": 0, "format": "json"})
	result, err = s.wrapToolHandler(s.handleGetSymbolSource)(t.Context(), read)
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", resultText(result))
	fields := extractTextResult(t, result)
	require.Equal(t, "func NativeAfterTool() int { return 2 }", fields["source"])
	fresh := pendingFileProjections.snap.Load().entries[pathkey.MutationKey(path)]
	require.NotNil(t, fresh)
	require.NotSame(t, old, fresh)
	require.Zero(t, fresh.commitSeq, "native read-built projection cannot reuse the prior tool's physical receipt")
	require.False(t, liveProjectionForAcknowledgement(old))
	require.NotNil(t, s.graph.GetNode("main.go::helper"))
	require.Nil(t, s.graph.GetNode("main.go::NativeAfterTool"), "neither exact read nor event queues may publish the held graph")
	freshness := fields["freshness"].(map[string]any)
	require.Equal(t, true, freshness["readback_exact"])
	require.Equal(t, "projected", freshness["file_view"])
}
