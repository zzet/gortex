package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
	"go.uber.org/zap"
)

// A legitimate external-write projection with no watcher has no mutation
// receipt to initialize the decorator's facts. The registered relation must
// still refuse a later source event instead of returning its old selection.
func TestRegisteredRelationLateSourceEventWithoutReceiptRefuses(t *testing.T) {
	for _, tool := range []string{"get_callers", "get_call_chain"} {
		t.Run(tool, func(t *testing.T) {
			selectedProjectionRegistry(t)
			root := t.TempDir()
			path := filepath.Join(root, "main.go")
			before := "package probe\n\nfunc Caller() int { return Callee() }\n\nfunc Callee() int { return 7 }\n"
			after := "package probe\n\nfunc Caller() int {\n value := 0\n return value + Callee()\n}\n\nfunc Callee() int { return 7 }\n"
			require.NoError(t, os.WriteFile(path, []byte(before), 0600))
			sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "late.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sql.Close()) })
			idx := newProjectionTestIndexer(t, sql)
			_, err = idx.Index(root)
			require.NoError(t, err)
			s := NewServer(query.NewEngine(sql), sql, idx, nil, zap.NewNop(), nil)
			fired := false
			s.facades.mu.RLock()
			captured := s.facades.captured[tool]
			s.facades.mu.RUnlock()
			s.addTool(captured.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				result, err := captured.handler(ctx, req)
				if err == nil && result != nil && !result.IsError {
					fired = true
					pendingFileProjections.markSuspect(path)
				}
				return result, err
			})
			const session = "relation-late-event"
			require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
			t.Cleanup(func() { s.DisconnectSession(session) })
			ctx := s.WithClientSession(WithSessionID(t.Context(), session), session)
			require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"late-relations","version":"1"}}}`)))
			s.NoteSessionToolPolicy(session, "all", "defer")
			require.NoError(t, os.WriteFile(path, []byte(after), 0600))
			warm := preselectionRegisteredCall(t, s, ctx, 10, "get_file_summary", map[string]any{"path": "main.go", "format": "json"})
			require.False(t, warm.IsError, resultText(warm))
			entry := liveEntry(pendingFileProjections, path)
			require.NotNil(t, entry, "warm actual registered summary must publish the external-read projection")
			require.Empty(t, s.laggingMutationReceipts(ctx), "no fabricated pending ticket may initialize the late facts")
			pinned, err := retainRequestProjection(withRequestProjectionFacts(t.Context()), entry)
			require.NoError(t, err)
			target := "main.go::Caller"
			if tool == "get_callers" {
				target = "main.go::Callee"
			}
			result := preselectionRegisteredCall(t, s, ctx, 11, tool, map[string]any{"id": target, "depth": 1, "limit": 1000, "format": "json"})
			require.True(t, fired, "source event must occur after the actual relation leaf read")
			require.True(t, entry.suspect.Load())
			require.ErrorIs(t, validateRequestProjections(pinned), projectionSuperseded, "old fact contexts stay invalid")
			require.True(t, result.IsError, "late source event must refuse the whole public relation response: %s", resultText(result))
		})
	}
}
