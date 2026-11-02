package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestCachedAcknowledgedSourceSurvivesQueuedStormWithoutParsing(t *testing.T) {
	for _, mode := range []string{"current", "suspect_same_bytes", "changed_bytes"} {
		t.Run(mode, func(t *testing.T) {
			selectedProjectionRegistry(t)
			extractor := &sourceNameCountingExtractor{Extractor: languages.NewGoExtractor()}
			registry := parser.NewRegistry()
			registry.Register(extractor)
			s, root := setupTestServerWithRegistry(t, registry)
			s.editAckMode = config.EditAckLocal
			pending := make(chan indexer.MutationResult)
			defer close(pending)
			s.SetWatcher(mutationTestWatcher{done: pending})
			path := filepath.Join(root, "main.go")
			req := makeReq("edit_file", map[string]any{"path": path, "old_string": "func helper() {}", "new_string": "func helper() { println(7) }", "format": "json"})
			ack, err := s.runAcknowledgedEdit(t.Context(), req, s.handleEditFile)
			require.NoError(t, err)
			require.False(t, ack.IsError, resultText(ack))
			require.Equal(t, "committed", extractTextResult(t, ack)["disk_status"])
			cached := pendingFileProjections.snap.Load().entries[pathkey.MutationKey(path)]
			require.NotNil(t, cached)
			require.Positive(t, cached.commitSeq)
			require.True(t, liveProjectionForAcknowledgement(cached))
			extractor.resetPaths()
			if mode == "suspect_same_bytes" {
				pendingFileProjections.markSuspect(path)
			}
			if mode == "changed_bytes" {
				info, err := os.Stat(path)
				require.NoError(t, err)
				raw, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(raw), "println(7)", "println(8)", 1)), 0600))
				require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
			}
			s.SetWatcher(projectedStormWatcher{mutationTestWatcher: mutationTestWatcher{done: pending}, path: path})
			read := makeReq("get_symbol_source", map[string]any{"id": "main.go::helper", "context_lines": 0, "format": "json"})
			result, err := s.wrapToolHandler(s.handleGetSymbolSource)(t.Context(), read)
			require.NoError(t, err)
			require.False(t, result.IsError, resultText(result))
			fields := extractTextResult(t, result)
			if mode == "changed_bytes" {
				require.Nil(t, fields["source"], "a storm cannot authorize a changed cached body")
				freshness := fields["freshness"].(map[string]any)
				require.Equal(t, "storm", freshness["file_reason"])
				require.NotEqual(t, true, freshness["readback_exact"])
				require.Empty(t, extractor.snapshotPaths(), "actual drift must stay pending without parsing during a storm")
				require.True(t, cached.suspect.Load())
				return
			}
			require.Equal(t, "func helper() { println(7) }", fields["source"], "queued work must not suppress an already checked committed-file projection")
			require.Empty(t, extractor.snapshotPaths(), "a cached current file must not start another parse during a storm")
			current := pendingFileProjections.snap.Load().entries[cached.key]
			if mode == "suspect_same_bytes" {
				require.NotSame(t, cached, current)
				require.Same(t, cached.layer, current.layer)
				require.True(t, cached.suspect.Load())
				require.False(t, current.suspect.Load())
				require.Zero(t, current.commitSeq)
			} else {
				require.Same(t, cached, current)
			}
			require.Equal(t, true, fields["freshness"].(map[string]any)["readback_exact"])
		})
	}
}

func TestSymbolSourceRetriesByteDriftBeforeProjectionSelection(t *testing.T) {
	selectedProjectionRegistry(t)
	extractor := &pendingBudgetExtractor{base: languages.NewGoExtractor(), entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	registry := parser.NewRegistry()
	registry.Register(extractor)
	s, root := setupTestServerWithRegistry(t, registry)
	s.editAckMode = config.EditAckLocal
	pending := make(chan indexer.MutationResult)
	defer close(pending)
	s.SetWatcher(mutationTestWatcher{done: pending})
	path := filepath.Join(root, "main.go")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(raw), "func helper() {}", "func helper() { println(1) }", 1)), 0600))
	extractor.hold.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	var once sync.Once
	release := func() { once.Do(func() { close(extractor.release) }) }
	type reply struct {
		result *mcplib.CallToolResult
		err    error
	}
	responses := make(chan reply, 1)
	joined := make(chan struct{})
	var deadlines []time.Time
	handler := s.wrapToolHandler(func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		deadlines = append(deadlines, ctx.Value(projectedSourceDeadlineKey{}).(time.Time))
		return s.handleGetSymbolSource(ctx, req)
	})
	go func() {
		defer close(joined)
		result, err := handler(ctx, makeReq("get_symbol_source", map[string]any{"id": "main.go::helper", "context_lines": 0, "format": "json"}))
		responses <- reply{result, err}
	}()
	t.Cleanup(func() {
		cancel()
		release()
		<-joined
		select {
		case <-extractor.entered:
			<-extractor.finished
		default:
		}
	})
	select {
	case <-extractor.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	write := makeReq("edit_file", map[string]any{"path": path, "old_string": "println(1)", "new_string": "println(2)", "format": "json"})
	ack, err := s.runAcknowledgedEdit(ctx, write, s.handleEditFile)
	require.NoError(t, err)
	require.False(t, ack.IsError, resultText(ack))
	require.Equal(t, "committed", extractTextResult(t, ack)["disk_status"])
	release()
	var actual reply
	select {
	case actual = <-responses:
	case <-ctx.Done():
		<-responses
		t.Fatal(ctx.Err())
	}
	require.NoError(t, actual.err)
	require.NotNil(t, actual.result)
	require.False(t, actual.result.IsError, resultText(actual.result))
	fields := extractTextResult(t, actual.result)
	require.Equal(t, "func helper() { println(2) }", fields["source"], "a typed preselection byte mismatch gets the one audited whole-call retry")
	require.Len(t, deadlines, 2)
	require.Equal(t, deadlines[0], deadlines[1], "retry must not renew its original projection deadline")
}
