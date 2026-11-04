package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/tokens"
)

// Install the observer on an actual registered request, retaining the normal
// selected-store capabilities and the handler's original context/deadline.
func observeRegisteredCheckedRead(s *Server, name string, observe checkedProjectionReadObserver) {
	s.facades.mu.RLock()
	captured := s.facades.captured[name]
	s.facades.mu.RUnlock()
	leaf := captured.handler
	s.addTool(captured.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		return leaf(context.WithValue(ctx, checkedProjectionReadObserverKey{}, observe), req)
	})
}

type preselectionRetirementStore struct {
	*store_sqlite.Store
	armed    atomic.Bool
	reads    atomic.Int32
	observed *projectionEntry
	publish  func()
}

func (s *preselectionRetirementStore) observe(_ context.Context, expected *projectionEntry) {
	if s.armed.CompareAndSwap(true, false) {
		s.reads.Add(1)
		s.observed = expected
		s.publish()
	}
}

// This is the production daemon's promote-on-demand and per-call authorization
// before registered MCP dispatch, not a direct leaf call.
func preselectionRegisteredCall(t *testing.T, s *Server, ctx context.Context, id int, name string, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	started := time.Now()
	require.True(t, s.IsToolEnabledForSession(ctx, name), "the entire initialized connection uses all/defer")
	s.EnsureToolPromotedForSession(ctx, name)
	if deadline, ok := ctx.Value(projectedSourceDeadlineKey{}).(time.Time); ok {
		t.Logf("registered tool=%s stage=before_sdk_dispatch authorization_and_promotion=%s remaining_original_budget=%s", name, time.Since(started), time.Until(deadline))
	}
	return guardE2ECall(t, s, WithAuthorizedToolCall(ctx, name), id, name, args)
}

func TestRegisteredPreselectionRetirementKeepsCurrentFileRows(t *testing.T) {
	for _, tool := range []string{"get_file_summary", "get_editing_context"} {
		t.Run(tool, func(t *testing.T) {
			selectedProjectionRegistry(t)
			require.True(t, tokens.EncoderReady())
			root := t.TempDir()
			path := filepath.Join(root, "main.go")
			require.NoError(t, os.WriteFile(path, []byte("package probe\n\nfunc Target() int { return 1 }\n\nfunc Neighbor() int { return 9 }\n"), 0600))
			sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "registered.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sql.Close()) })
			backend := &preselectionRetirementStore{Store: sql}
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
			session := "registered-preselection-" + tool
			require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
			t.Cleanup(func() { s.DisconnectSession(session) })
			ctx := s.WithClientSession(WithSessionID(t.Context(), session), session)
			require.Equal(t, session, SessionIDFromContext(ctx))
			require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"registered-readback","version":"1"}}}`)))
			s.NoteSessionToolPolicy(session, "all", "defer")
			ack := preselectionRegisteredCall(t, s, ctx, 10, "edit_file", map[string]any{"path": path, "old_string": "func Target() int { return 1 }", "new_string": "func Target() int {\n\tvalue := 7\n\treturn value\n}", "format": "json"})
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
			// Use the real registered all/defer operations, retaining one connection.
			for i, name := range []string{"get_symbol", "read_file", "get_symbol_source"} {
				args := map[string]any{"format": "json"}
				if name == "read_file" {
					args["path"] = "main.go"
				} else {
					args["id"] = "main.go::Target"
					if name == "get_symbol" {
						args["detail"] = "full"
					}
				}
				result := preselectionRegisteredCall(t, s, ctx, 11+i, name, args)
				require.False(t, result.IsError, resultText(result))
			}
			retirement := s.canonicalProjectionRetirement(path)
			require.NotNil(t, retirement)
			var beforeNode, afterNode *graph.Node
			var publicationErr error
			var publicationElapsed time.Duration
			var publicationFinished, callbackEntered time.Time
			var checkedRetired bool
			var current *projectionEntry
			backend.publish = func() {
				callbackEntered = time.Now()
				beforeNode = sql.GetNode("main.go::Neighbor")
				started := time.Now()
				publicationErr = idx.IndexFile(path)
				publicationElapsed = time.Since(started)
				afterNode = sql.GetNode("main.go::Neighbor")
				retirement.run()
				checkedRetired = entry.checkedRetired.Load()
				current = liveEntry(pendingFileProjections, path)
				publicationFinished = time.Now()
			}
			deadline := time.Now().Add(projectedReadBudget)
			ctx = context.WithValue(ctx, projectedSourceDeadlineKey{}, deadline)
			observeRegisteredCheckedRead(s, tool, backend.observe)
			backend.armed.Store(true)
			result := preselectionRegisteredCall(t, s, ctx, 20, tool, map[string]any{"path": "main.go", "format": "json"})
			require.Equal(t, int32(1), backend.reads.Load(), "actual verified pre-CAS request boundary must be reached")
			require.Same(t, entry, backend.observed)
			t.Logf("registered stage=callback_entry remaining_original_budget=%s publication_elapsed=%s remaining_at_publication=%s", deadline.Sub(callbackEntered), publicationElapsed, deadline.Sub(publicationFinished))
			require.NotNil(t, beforeNode)
			require.Equal(t, 5, beforeNode.StartLine)
			require.NoError(t, publicationErr)
			require.NotNil(t, afterNode)
			require.Equal(t, 8, afterNode.StartLine)
			require.True(t, checkedRetired)
			require.Nil(t, current)
			t.Logf("registered tool=%s result=%s remaining_original_budget=%s", tool, resultText(result), time.Until(deadline))
			requireCanonicalDefinitionRows(t, tool, result, raw)
		})
	}
}
