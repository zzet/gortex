package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

func TestPendingTextSearchReadsExactDiskAfterProjectionBuildDeadline(t *testing.T) {
	t.Setenv("GORTEX_PROJECTION", "1")
	selectedProjectionRegistry(t)
	extractor := &pendingBudgetExtractor{
		base:    languages.NewGoExtractor(),
		entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{}),
	}
	registry := parser.NewRegistry()
	registry.Register(extractor)
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	const oldName = "BeforeProjectionDeadline"
	const newName = "AfterProjectionDeadlineQuartz"
	before := []byte("package main\n\nfunc " + oldName + "() {}\n")
	after := []byte("package main\n\nfunc " + newName + "() {}\n")
	require.NoError(t, os.WriteFile(path, before, 0o600))
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := indexer.New(store, registry, config.Default().Index, zap.NewNop())
	t.Cleanup(idx.Close)
	_, err = idx.Index(root)
	require.NoError(t, err)
	for range 4 {
		require.Len(t, idx.GrepText("func "+oldName+"(", 10), 1)
	}
	// Establish stale cached postings independently of the registered read.
	require.NoError(t, os.WriteFile(path, after, 0o600))
	require.Empty(t, idx.GrepText("func "+newName+"(", 10))
	require.NoError(t, os.WriteFile(path, before, 0o600))
	server := NewServer(query.NewEngine(store), store, idx, nil, zap.NewNop(), nil, MultiRepoOptions{EditAckMode: config.EditAckLocal})
	publication := make(chan indexer.MutationResult, 1)
	t.Cleanup(func() { close(publication) })
	server.SetWatcher(mutationTestWatcher{done: publication})
	tool := server.MCPServer().GetTool("edit_file")
	require.NotNil(t, tool)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	var once sync.Once
	release := func() { once.Do(func() { close(extractor.release) }) }
	type reply struct {
		result *mcpgo.CallToolResult
		err    error
	}
	response := make(chan reply, 1)
	joined := false
	t.Cleanup(func() {
		cancel()
		release()
		if !joined {
			select {
			case <-response:
			case <-time.After(3 * time.Second):
				t.Error("owned registered mutation did not join")
			}
		}
		select {
		case <-extractor.entered:
			select {
			case <-extractor.finished:
			case <-time.After(3 * time.Second):
				t.Error("owned deadline extraction did not finish after release")
			}
		default:
		}
	})
	extractor.hold.Store(true)
	request := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
		Name:      "edit_file",
		Arguments: map[string]any{"path": path, "old_string": oldName, "new_string": newName, "format": "json"},
	}}
	go func() {
		result, callErr := tool.Handler(ctx, request)
		response <- reply{result: result, err: callErr}
	}()
	select {
	case <-extractor.entered:
	case <-ctx.Done():
		t.Fatal("registered write did not enter the real projection extractor")
	}
	var actual reply
	select {
	case actual = <-response:
		joined = true
	case <-ctx.Done():
		t.Fatal("registered LOCAL acknowledgement waited for the held extraction")
	}
	require.NoError(t, actual.err)
	require.NotNil(t, actual.result)
	require.False(t, actual.result.IsError, "%v", actual.result.Content)
	require.NoError(t, ctx.Err(), "the caller remains live after the private build deadline")
	select {
	case <-extractor.finished:
		t.Fatal("projection completed before the deadline refusal was observed")
	default:
	}
	ack := extractTextResult(t, actual.result)
	require.Equal(t, "local", ack["ack_mode"])
	view, ok := ack["file_view"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "unavailable", view["state"])
	require.Equal(t, "build_deadline", view["reason"])
	require.Equal(t, "build_deadline", ack["file_reason"])
	require.Equal(t, false, ack["readback_exact"])
	require.Positive(t, ack["commit_seq"])
	sum := sha256.Sum256(after)
	require.Equal(t, hex.EncodeToString(sum[:]), ack["content_sha256"])
	require.Empty(t, pendingFileProjections.snap.Load().entries)

	// The first registered post-ACK read must use disk, without waiting for
	// the abandoned projection or the independently held canonical ticket.
	result := callToolByName(t, server, ctx, "search_text", map[string]any{"query": "func " + newName + "(", "limit": 10, "max_bytes": 0, "format": "json"})
	require.False(t, result.IsError, "%v", result.Content)
	payload := extractTextResult(t, result)
	rows, ok := payload["matches"].([]any)
	require.True(t, ok)
	require.Len(t, rows, 1)
	require.Equal(t, float64(1), payload["count"])
	require.NotEqual(t, true, payload["_truncated_by_budget"])
	require.NotEqual(t, true, payload["truncated"])
	row := rows[0].(map[string]any)
	require.Equal(t, "main.go", row["path"])
	require.Equal(t, float64(3), row["line"])
	require.Equal(t, "func "+newName+"() {}", row["text"])
	old := callToolByName(t, server, ctx, "search_text", map[string]any{"query": "func " + oldName + "(", "limit": 10, "max_bytes": 0, "format": "json"})
	require.False(t, old.IsError, "%v", old.Content)
	oldPayload := extractTextResult(t, old)
	require.Empty(t, oldPayload["matches"], "old canonical text cannot survive the committed physical change")
	require.Equal(t, float64(0), oldPayload["count"])
	require.NotEqual(t, true, oldPayload["_truncated_by_budget"])
	require.NotEqual(t, true, oldPayload["truncated"])
	require.Empty(t, oldPayload["next_cursor"])
	physical, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, after, physical)
	require.NotNil(t, store.GetNode("main.go::"+oldName))
	require.Nil(t, store.GetNode("main.go::"+newName), "neither read may depend on canonical publication")
	require.Empty(t, idx.GrepText("func "+newName+"(", 10), "the registered read must not repair canonical trigram state as a side effect")
	release()
	select {
	case <-extractor.finished:
	case <-ctx.Done():
		t.Fatal("owned abandoned extraction did not finish")
	}
	require.Empty(t, pendingFileProjections.snap.Load().entries, "late extraction cannot resurrect a refused projection")
}
