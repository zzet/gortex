package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/tokens"
)

// A metadata read can overlap checked canonical retirement after capturing its
// expected owner. An unchanged read must not call that historical owner a new
// source invalidation merely because it no longer occupies the registry slot.
func TestUnchangedReadAfterRetirementCannotPoisonBorrowedRetryProof(t *testing.T) {
	selectedProjectionRegistry(t)
	require.True(t, tokens.EncoderReady())
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("package probe\n\nfunc Target() int { return 1 }\n\nfunc Neighbor() int { return 9 }\n"), 0600))
	// Exercise the real SQLite publication/reader boundary without charging
	// temporary-filesystem persistence to this lifecycle interleaving oracle.
	sql, err := store_sqlite.Open(":memory:")
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
				t.Error("owned held publication ticket did not join")
			}
		}
	})
	const session = "unchanged-read-retirement"
	require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(session) })
	ctx := s.WithClientSession(WithSessionID(t.Context(), session), session)
	require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"registered-readback","version":"1"}}}`)))
	s.NoteSessionToolPolicy(session, "all", "defer")
	const body = "func Target() int {\n\tvalue := 7\n\treturn value\n}"
	ack := preselectionRegisteredCall(t, s, ctx, 10, "edit_symbol", map[string]any{"id": "main.go::Target", "old_source": "func Target() int { return 1 }", "new_source": body, "dry_run": false})
	require.False(t, ack.IsError, resultText(ack))
	fields := extractTextResult(t, ack)
	require.Equal(t, "local", fields["ack_mode"])
	require.Equal(t, true, fields["readback_exact"])
	value, ok := s.mutationReceipts.Load(fields["reindex_receipt"].(string))
	require.True(t, ok)
	receipt = value.(*mutationReceipt)
	entry := liveEntry(pendingFileProjections, path)
	require.NotNil(t, entry)
	proof, err := retainRequestProjection(withRequestProjectionFacts(t.Context()), entry)
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	retirement := s.canonicalProjectionRetirement(path)
	require.NotNil(t, retirement)
	backend.publish = func() {
		require.Equal(t, 5, sql.GetNode("main.go::Neighbor").StartLine)
		require.NoError(t, idx.IndexFile(path))
		require.Equal(t, 8, sql.GetNode("main.go::Neighbor").StartLine)
		require.Empty(t, indexer.DiffProjectedNodes(entry.layer.FileNodes("main.go"), sql.GetFileNodes("main.go")))
		retirement.run()
		require.True(t, entry.checkedRetired.Load())
		require.False(t, entry.suspect.Load(), "actual checked retirement starts with unchanged source proof")
		require.Nil(t, liveEntry(pendingFileProjections, path))
	}
	deadline := time.Now().Add(canonicalLifecycleFixtureBudget)
	ctx = context.WithValue(ctx, projectedSourceDeadlineKey{}, deadline)
	allowance := &projectedSourceAllowance{tool: "get_file_summary"}
	ctx = context.WithValue(ctx, projectedSourceAllowanceKey{}, allowance)
	proof = context.WithValue(proof, projectedSourceDeadlineKey{}, deadline)
	proof = context.WithValue(proof, projectedSourceAllowanceKey{}, &projectedSourceAllowance{tool: "get_symbol_source"})
	backend.armed.Store(true)
	result := preselectionRegisteredCall(t, s, ctx, 20, "get_file_summary", map[string]any{"path": "main.go", "format": "json"})
	requireCanonicalDefinitionRows(t, "get_file_summary", result, raw)
	require.Equal(t, int32(1), backend.reads.Load())
	require.LessOrEqual(t, allowance.attempts.Load(), int32(2))
	require.True(t, time.Now().Before(deadline))
	pendingFileProjections.mu.Lock()
	events := entry.sourceEvents
	pendingFileProjections.mu.Unlock()
	t.Logf("actor_attempts=%d original_remaining=%s checked=%v suspect=%v source_events=%d", allowance.attempts.Load(), time.Until(deadline), entry.checkedRetired.Load(), entry.suspect.Load(), events)
	require.Zero(t, events, "no source event happened in this controlled interval")
	require.True(t, entry.checkedRetired.Load())
	require.False(t, entry.suspect.Load(), "an unchanged losing reader must not poison another reader's checked retirement")
	require.ErrorIs(t, validateRequestProjections(proof), projectionSuperseded, "the old context remains invalid")
	require.True(t, retryAfterCheckedProjectionChange(proof), "only a fresh bounded attempt may use the unchanged canonical proof")
}
