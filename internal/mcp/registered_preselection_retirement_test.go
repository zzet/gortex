package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/tokens"
)

// Observe one real metadata read without replacing the selected store's other
// capabilities. The callback is armed only after the registered same-session
// edit and preliminary reads; its old result is a legitimate pre-publication
// observation, not a fabricated hash or node row.
type preselectionRetirementStore struct {
	*store_sqlite.Store
	armed   atomic.Bool
	reads   atomic.Int32
	publish func()
}

func (s *preselectionRetirementStore) FileMetasByPaths(prefix string, paths []string) (map[string]graph.FileMetaRow, error) {
	rows, err := s.Store.FileMetasByPaths(prefix, paths)
	if err == nil && s.armed.CompareAndSwap(true, false) {
		s.reads.Add(1)
		s.publish()
	}
	return rows, err
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
			initializeSession(t, s, session)
			s.NoteSessionToolPolicy(session, "all", "defer")
			ctx := s.WithClientSession(t.Context(), session)
			ack := guardE2ECall(t, s, ctx, 10, "edit_file", map[string]any{"path": path, "old_string": "func Target() int { return 1 }", "new_string": "func Target() int {\n\tvalue := 7\n\treturn value\n}", "format": "json"})
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
				result := guardE2ECall(t, s, ctx, 11+i, name, args)
				require.False(t, result.IsError, resultText(result))
			}
			retirement := s.canonicalProjectionRetirement(path)
			require.NotNil(t, retirement)
			backend.publish = func() {
				require.Equal(t, 5, sql.GetNode("main.go::Neighbor").StartLine)
				require.NoError(t, idx.IndexFile(path))
				require.Equal(t, 8, sql.GetNode("main.go::Neighbor").StartLine)
				retirement.run()
				require.True(t, entry.checkedRetired.Load())
				require.Nil(t, liveEntry(pendingFileProjections, path))
			}
			deadline := time.Now().Add(projectedReadBudget)
			ctx = context.WithValue(ctx, projectedSourceDeadlineKey{}, deadline)
			backend.armed.Store(true)
			result := guardE2ECall(t, s, ctx, 20, tool, map[string]any{"path": "main.go", "format": "json"})
			require.Equal(t, int32(1), backend.reads.Load(), "actual preselection metadata seam must be reached")
			t.Logf("registered tool=%s result=%s remaining_original_budget=%s", tool, resultText(result), time.Until(deadline))
			requireCanonicalDefinitionRows(t, tool, result, raw)
		})
	}
}
