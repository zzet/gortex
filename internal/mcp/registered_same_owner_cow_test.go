package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/tokens"
	"go.uber.org/zap"
)

type sameOwnerReadCOWStore struct {
	*store_sqlite.Store
	armed    atomic.Bool
	arrivals atomic.Int32
	captured chan *projectionEntry
	release  chan struct{}
	path     string
}

func (s *sameOwnerReadCOWStore) FileMetasByPaths(prefix string, paths []string) (map[string]graph.FileMetaRow, error) {
	rows, err := s.Store.FileMetasByPaths(prefix, paths)
	if err == nil && s.armed.Load() && s.arrivals.Add(1) <= 3 {
		s.captured <- liveEntry(pendingFileProjections, s.path)
		<-s.release
	}
	return rows, err
}

// The actual daemon admits independent Fast requests from one connection.
// All three reads see the same already-observed source event and capture the
// same ACK owner before any same-byte read-COW publication can win its CAS.
func TestConcurrentRegisteredReadsRevalidateSameACKOwner(t *testing.T) {
	selectedProjectionRegistry(t)
	require.True(t, tokens.EncoderReady())
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("package probe\n\nfunc Target() int { return 1 }\n\nfunc Neighbor() int { return 9 }\n"), 0600))
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "cow.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sql.Close()) })
	backend := &sameOwnerReadCOWStore{Store: sql, captured: make(chan *projectionEntry, 3), release: make(chan struct{}), path: path}
	idx := newProjectionTestIndexer(t, backend)
	_, err = idx.Index(root)
	require.NoError(t, err)
	s := NewServer(query.NewEngine(backend), backend, idx, nil, zap.NewNop(), nil)
	s.editAckMode = config.EditAckLocal
	pending := make(chan indexer.MutationResult)
	s.SetWatcher(mutationTestWatcher{done: pending})
	var receipt *mutationReceipt
	t.Cleanup(func() {
		close(pending)
		if receipt != nil {
			select {
			case <-receipt.done:
			case <-time.After(3 * time.Second):
				t.Error("owned held ticket did not join")
			}
		}
	})
	const session = "same-owner-read-cow"
	require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(session) })
	ctx := s.WithClientSession(WithSessionID(t.Context(), session), session)
	require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"registered-readback","version":"1"}}}`)))
	s.NoteSessionToolPolicy(session, "all", "defer")
	const body = "func Target() int {\n\tvalue := 7\n\treturn value\n}"
	ack := preselectionRegisteredCall(t, s, ctx, 10, "edit_file", map[string]any{"path": path, "old_string": "func Target() int { return 1 }", "new_string": body, "format": "json"})
	require.False(t, ack.IsError, resultText(ack))
	fields := extractTextResult(t, ack)
	require.Equal(t, "local", fields["ack_mode"])
	require.Equal(t, true, fields["readback_exact"])
	value, ok := s.mutationReceipts.Load(fields["reindex_receipt"].(string))
	require.True(t, ok)
	receipt = value.(*mutationReceipt)
	entry := liveEntry(pendingFileProjections, path)
	require.NotNil(t, entry)
	require.Positive(t, entry.commitSeq)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, 5, sql.GetNode("main.go::Neighbor").StartLine, "the independent canonical rows precede this actual local ACK")
	oldCtx, err := retainRequestProjection(withRequestProjectionFacts(t.Context()), entry)
	require.NoError(t, err)
	pendingFileProjections.markSuspect(path)
	require.True(t, entry.suspect.Load())
	require.ErrorIs(t, validateRequestProjections(oldCtx), projectionSuperseded)
	originalDeadline := time.Now().Add(projectedReadBudget)
	ctx = context.WithValue(ctx, projectedSourceDeadlineKey{}, originalDeadline)
	backend.armed.Store(true)
	release := sync.OnceFunc(func() { close(backend.release) })
	type answer struct {
		tool   string
		result *mcplib.CallToolResult
	}
	answers := make(chan answer, 3)
	allowances := make(map[string]*projectedSourceAllowance)
	var workers sync.WaitGroup
	t.Cleanup(func() { release(); workers.Wait() })
	for i, tool := range []string{"get_file_summary", "get_editing_context", "get_symbol_source"} {
		allowances[tool] = &projectedSourceAllowance{tool: tool}
		allowance := allowances[tool]
		workers.Add(1)
		go func() {
			defer workers.Done()
			args := map[string]any{"path": "main.go", "format": "json"}
			if tool == "get_symbol_source" {
				args = map[string]any{"id": "main.go::Target", "context_lines": 0, "compress_bodies": false, "format": "json"}
			}
			requestCtx := context.WithValue(ctx, projectedSourceAllowanceKey{}, allowance)
			result := preselectionRegisteredCall(t, s, requestCtx, 20+i, tool, args)
			answers <- answer{tool: tool, result: result}
		}()
	}
	for range 3 {
		select {
		case captured := <-backend.captured:
			require.Same(t, entry, captured, "both metadata reads and source must capture the same old pointer before the actual CAS")
		case <-time.After(3 * time.Second):
			t.Fatal("owned registered reads did not reach the common metadata barrier")
		}
	}
	release()
	results := map[string]*mcplib.CallToolResult{}
	for range 3 {
		select {
		case a := <-answers:
			results[a.tool] = a.result
		case <-time.After(3 * time.Second):
			t.Fatal("owned registered read did not join")
		}
	}
	workers.Wait()
	winner := liveEntry(pendingFileProjections, path)
	require.NotNil(t, winner)
	require.NotSame(t, entry, winner)
	require.Zero(t, winner.commitSeq)
	require.Same(t, entry.layer, winner.layer)
	require.True(t, entry.suspect.Load())
	require.ErrorIs(t, validateRequestProjections(oldCtx), projectionSuperseded, "another read cannot revive the old context")
	require.Equal(t, 5, sql.GetNode("main.go::Neighbor").StartLine, "no canonical publication or fabricated Done result changed this control")
	t.Logf("remaining original whole-call allowance=%s", time.Until(originalDeadline))
	for _, tool := range []string{"get_file_summary", "get_editing_context", "get_symbol_source"} {
		t.Logf("actual registered tool=%s attempts=%d result=%s", tool, allowances[tool].attempts.Load(), resultText(results[tool]))
		require.GreaterOrEqual(t, allowances[tool].attempts.Load(), int32(1))
		require.LessOrEqual(t, allowances[tool].attempts.Load(), int32(2), "all retry paths share the original two-attempt allowance")
	}
	source := results["get_symbol_source"]
	require.False(t, source.IsError, resultText(source))
	require.Equal(t, body, extractTextResult(t, source)["source"])
	for _, tool := range []string{"get_file_summary", "get_editing_context"} {
		t.Run(tool, func(t *testing.T) { requireCanonicalDefinitionRows(t, tool, results[tool], raw) })
	}
}

// The newly enabled full-metadata allowance stops at early selection errors.
// A later withdrawal must not replay recordSymbol or on-demand enrichment.
func TestFullSymbolCASAllowanceDoesNotGrantAfterLeafReplay(t *testing.T) {
	selectedProjectionRegistry(t)
	s, path, entry, _ := acknowledgedCanonicalFile(t)
	retirement := s.canonicalProjectionRetirement(path)
	require.NotNil(t, retirement)
	calls := 0
	var selected context.Context
	leaf := func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		calls++
		selected = ctx
		result, err := s.handleGetSymbol(ctx, req)
		require.NoError(t, err)
		require.False(t, result.IsError, resultText(result))
		require.EqualValues(t, 8, extractTextResult(t, result)["node"].(map[string]any)["start_line"])
		require.NoError(t, s.indexer.IndexFile(path))
		retirement.run()
		require.True(t, entry.checkedRetired.Load())
		return result, nil
	}
	result, err := s.wrapToolHandler(leaf)(t.Context(), makeReq("get_symbol", map[string]any{"id": "main.go::Neighbor", "detail": "full", "format": "json"}))
	require.NoError(t, err)
	require.True(t, result.IsError, resultText(result))
	require.Equal(t, 1, calls)
	require.Error(t, validateRequestProjections(selected), "a late old selection remains unusable")
	facts := selected.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
	facts.mu.RLock()
	old := facts.entries[entry.key]
	facts.mu.RUnlock()
	require.Same(t, entry, old)
}
