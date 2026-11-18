package mcp

import (
	"context"
	"os"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

// A hypothesis regression, not a claim about the untraced native refusal:
// the summary has borrowed the real local ACK's immutable owner, then a source
// event arrives after the leaf returns. No peer read earns a refresh for it.
func TestRegisteredSummaryLateSameBytesSuspectUsesOriginalAllowance(t *testing.T) {
	for _, surface := range []string{"raw", "facade"} {
		t.Run(surface, func(t *testing.T) {
			selectedProjectionRegistry(t)
			s, store, path := canonicalPromotionReadFixture(t)
			pending := make(chan indexer.MutationResult)
			s.SetWatcher(mutationTestWatcher{done: pending})
			var receipt *mutationReceipt
			t.Cleanup(func() {
				close(pending)
				if receipt != nil {
					select {
					case <-receipt.done:
					case <-time.After(3 * time.Second):
						t.Error("owned held mutation did not join")
					}
				}
			})
			const session = "late-summary-source-event"
			require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
			t.Cleanup(func() { s.DisconnectSession(session) })
			base := s.WithClientSession(WithSessionID(t.Context(), session), session)
			s.NoteSessionToolPolicy(session, "all", "defer")
			if surface == "facade" {
				s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
			}
			require.NotNil(t, s.MCPServer().HandleMessage(base, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"late-summary","version":"1"}}}`)))
			const body = "func Target() int {\n\tvalue := 7\n\treturn value\n}"
			editTool := "edit_symbol"
			editArgs := map[string]any{
				"id": "main.go::Target", "old_source": "func Target() int { return 1 }", "new_source": body, "dry_run": false,
			}
			if surface == "facade" {
				editTool = "edit"
				editArgs = map[string]any{"operation": "symbol", "target": map[string]any{"symbol": "main.go::Target"}, "options": map[string]any{"old_source": "func Target() int { return 1 }", "new_source": body, "dry_run": false}}
			}
			ack := preselectionRegisteredCall(t, s, base, 10, editTool, editArgs)
			require.False(t, ack.IsError, resultText(ack))
			fields := extractTextResult(t, ack)
			require.Equal(t, "local", fields["ack_mode"])
			require.Equal(t, true, fields["readback_exact"])
			value, found := s.mutationReceipts.Load(fields["reindex_receipt"].(string))
			require.True(t, found)
			receipt = value.(*mutationReceipt)
			owner := liveEntry(pendingFileProjections, path)
			require.NotNil(t, owner)
			require.Positive(t, owner.commitSeq)
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, 5, store.GetNode("main.go::Neighbor").StartLine, "canonical rows still precede the acknowledged line shift")

			captured, found := s.facades.legacy("get_file_summary")
			require.True(t, found)
			allowance := &projectedSourceAllowance{tool: "get_file_summary"}
			deadline := time.Now().Add(projectedReadBudget)
			ctx := context.WithValue(base, projectedSourceDeadlineKey{}, deadline)
			ctx = context.WithValue(ctx, projectedSourceAllowanceKey{}, allowance)
			var first context.Context
			var invalidated error
			var remaining time.Duration
			var selected []*projectedSourceAllowance
			var deadlines []time.Time
			calls := 0
			s.addTool(captured.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				calls++
				selected = append(selected, ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance))
				deadlines = append(deadlines, ctx.Value(projectedSourceDeadlineKey{}).(time.Time))
				result, readErr := captured.handler(ctx, req)
				if calls == 1 && readErr == nil && result != nil && !result.IsError {
					first = ctx
					// This is the actual registry event boundary, not a fabricated
					// successor, a peer response, or a cleared suspect flag.
					pendingFileProjections.markSuspect(path)
					invalidated = validateRequestProjections(ctx)
					remaining = time.Until(deadline)
				}
				return result, readErr
			})
			tool := "get_file_summary"
			args := map[string]any{"path": "main.go", "format": "json"}
			if surface == "facade" {
				tool = "read"
				args = map[string]any{"operation": "summary", "target": map[string]any{"file": "main.go"}, "options": map[string]any{"format": "json"}}
			}
			result, err := externalVersionRegisteredCall(s, ctx, 20, tool, args)
			require.NoError(t, err)
			require.NotNil(t, first, "the registered summary leaf must select before the event")
			require.ErrorIs(t, invalidated, projectionSuperseded)
			require.Positive(t, remaining, "the causal boundary must fit the original 150ms")
			t.Logf("surface=%s leaf_calls=%d attempts=%d remaining_original150=%s result=%s", surface, calls, allowance.attempts.Load(), remaining, resultText(result))
			requireCanonicalDefinitionRows(t, "get_file_summary", result, raw)
			require.Equal(t, 2, calls, "only one fresh whole-response attempt, without a client or peer read")
			require.Equal(t, int32(2), allowance.attempts.Load())
			for i := range selected {
				require.Same(t, allowance, selected[i])
				require.Equal(t, deadline, deadlines[i], "never reset the shared 150ms deadline")
			}
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, raw, after)
			require.True(t, owner.suspect.Load(), "fresh proof cannot revive the historical ACK owner")
			require.Error(t, validateRequestProjections(first), "the original SDK context remains unusable")
			require.ErrorIs(t, validateRequestProjections(context.WithoutCancel(first)), projectionSuperseded, "cancellation cannot hide revived old projection facts")
			current := liveEntry(pendingFileProjections, path)
			require.NotNil(t, current)
			require.NotSame(t, owner, current)
			require.Zero(t, current.commitSeq)
			require.Equal(t, owner.sourceCommitSeq, current.sourceCommitSeq)
			require.Same(t, owner.layer, current.layer)
			require.Equal(t, owner.rawHash, current.rawHash)
			require.False(t, current.suspect.Load())
			require.Equal(t, 5, store.GetNode("main.go::Neighbor").StartLine, "no canonical publication changed the control")
		})
	}
}
