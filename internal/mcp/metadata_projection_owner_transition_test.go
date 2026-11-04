package mcp

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/tokens"
)

// An actual read-built admission can publish another checked same-byte owner
// before the metadata handler retains its first selection. No response has
// been assembled, and this must not authorize reuse of any caller's old facts.
func TestRegisteredMetadataSelectsCheckedOwnerBeforeRetain(t *testing.T) {
	runRegisteredMetadataOwnerTransition(t, false)
}

func TestRegisteredMetadataSelectsCheckedOwnerDuringPhysicalCheck(t *testing.T) {
	runRegisteredMetadataOwnerTransition(t, true)
}

func runRegisteredMetadataOwnerTransition(t *testing.T, duringPhysicalCheck bool) {
	t.Helper()
	for _, tool := range []string{"get_symbol", "get_editing_context"} {
		t.Run(tool, func(t *testing.T) {
			selectedProjectionRegistry(t)
			require.True(t, tokens.EncoderReady())
			root := t.TempDir()
			path := filepath.Join(root, "main.go")
			const before = "package probe\n\nfunc Target() int { return 1 }\n\nfunc Neighbor() int { return 9 }\n"
			require.NoError(t, os.WriteFile(path, []byte(before), 0o600))
			backend, err := store_sqlite.Open(filepath.Join(t.TempDir(), "metadata.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, backend.Close()) })
			idx := newProjectionTestIndexer(t, backend)
			_, err = idx.Index(root)
			require.NoError(t, err)
			s := NewServer(query.NewEngine(backend), backend, idx, nil, zap.NewNop(), nil)
			s.engine.SetSearchProvider(idx.Search)
			fts, err := backend.SymbolFTSCount()
			require.NoError(t, err)
			require.Positive(t, fts)
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
						t.Error("held mutation receipt did not join")
					}
				}
			})
			const session = "metadata-owner-transition"
			require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
			t.Cleanup(func() { s.DisconnectSession(session) })
			ctx := s.WithClientSession(WithSessionID(t.Context(), session), session)
			s.NoteSessionToolPolicy(session, "all", "defer")
			require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"metadata-readback","version":"1"}}}`)))
			const body = "func Target() int {\n\tvalue := 7\n\treturn value\n}"
			ack := preselectionRegisteredCall(t, s, ctx, 10, "edit_symbol", map[string]any{"id": "main.go::Target", "old_source": "func Target() int { return 1 }", "new_source": body, "dry_run": false})
			require.False(t, ack.IsError, resultText(ack))
			fields := extractTextResult(t, ack)
			require.Equal(t, true, fields["readback_exact"])
			require.Equal(t, "local", fields["ack_mode"])
			value, ok := s.mutationReceipts.Load(fields["reindex_receipt"].(string))
			require.True(t, ok)
			receipt = value.(*mutationReceipt)
			original := liveEntry(pendingFileProjections, path)
			require.NotNil(t, original)
			oldFacts, err := retainRequestProjection(ctx, original)
			require.NoError(t, err)
			physical, err := os.ReadFile(path)
			require.NoError(t, err)
			physicalHash := sha256.Sum256(physical)
			var admitted, successor *projectionEntry
			var peer *mcplib.CallToolResult
			var originalErr error
			var remaining time.Duration
			callbacks := 0
			watcher := &searchBindAdmissionWatcher{mutationTestWatcher: mutationTestWatcher{done: pending}}
			observe := func(build context.Context, admittedPath string) {
				callbacks++
				require.Equal(t, path, admittedPath)
				admitted = liveEntry(pendingFileProjections, path)
				pendingFileProjections.markSuspect(path)
				peer = preselectionRegisteredCall(t, s, ctx, 30, "get_symbol_source", map[string]any{"id": "main.go::Target", "context_lines": 0, "compress_bodies": false, "format": "json"})
				successor = liveEntry(pendingFileProjections, path)
				originalErr = build.Err()
				deadline, ok := build.Value(projectedSourceDeadlineKey{}).(time.Time)
				require.True(t, ok)
				remaining = time.Until(deadline)
				// A second independently checked reader may win again before
				// the next initial selection. Never arm during the peer call.
				if !duringPhysicalCheck {
					watcher.armed.Store(callbacks < 2)
				}
			}
			if duringPhysicalCheck {
				observeRegisteredCheckedRead(s, tool, func(build context.Context, expected *projectionEntry) {
					if callbacks < 2 {
						require.Same(t, liveEntry(pendingFileProjections, path), expected)
						observe(build, path)
					}
				})
			} else {
				watcher.observe = observe
				s.SetWatcher(watcher)
				watcher.armed.Store(true)
			}
			pendingFileProjections.markSuspect(path)
			args := map[string]any{"path": "main.go", "format": "json"}
			if tool == "get_symbol" {
				args = map[string]any{"id": "main.go::Target", "detail": "full", "format": "json"}
			}
			require.True(t, s.IsToolEnabledForSession(ctx, tool))
			s.EnsureToolPromotedForSession(ctx, tool)
			result, callErr := externalVersionRegisteredCall(s, WithAuthorizedToolCall(ctx, tool), 20, tool, args)
			require.GreaterOrEqual(t, callbacks, 1)
			require.LessOrEqual(t, callbacks, 2)
			require.NotNil(t, admitted)
			if !duringPhysicalCheck {
				require.NotSame(t, original, admitted)
			}
			require.NotSame(t, admitted, successor)
			require.Same(t, original.layer, successor.layer)
			require.Equal(t, original.sourceCommitSeq, successor.sourceCommitSeq)
			require.Zero(t, successor.commitSeq)
			require.True(t, successor.readVersionChecked)
			require.Zero(t, successor.sourceEvents)
			require.True(t, liveProjectionForAcknowledgement(successor))
			require.False(t, peer.IsError, resultText(peer))
			require.NoError(t, originalErr)
			require.Positive(t, remaining)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, physicalHash, sha256.Sum256(after))
			require.Equal(t, 5, backend.GetNode("main.go::Neighbor").StartLine)
			require.ErrorIs(t, validateRequestProjections(oldFacts), projectionSuperseded)
			t.Logf("CAUSAL_PREMISES_VERIFIED tool=%s FTS=%d checked_replacements=%d original_remaining=%s call_error=%v result=%v", tool, fts, callbacks, remaining, callErr, result)
			require.NoError(t, callErr)
			require.NotNil(t, result)
			require.False(t, result.IsError, resultText(result))
			if duringPhysicalCheck {
				require.Equal(t, 1, callbacks, "select the checked owner before spending a response attempt")
			}
			if tool == "get_symbol" {
				node := extractTextResult(t, result)["node"].(map[string]any)
				require.Equal(t, "main.go::Target", node["id"])
				require.Equal(t, float64(3), node["start_line"])
				require.Equal(t, float64(6), node["end_line"])
			} else {
				requireCanonicalDefinitionRows(t, tool, result, physical)
			}
		})
	}
}
