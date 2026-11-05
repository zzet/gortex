package mcp

import (
	"context"
	"crypto/sha256"
	"os"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/semantic"
)

// This registered boundary control does not identify the native suspect setter.
// It admits a fresh answer only after an existing checked owner transition;
// a source event alone, another physical version, and cancellation stay strict.
func TestRegisteredFullSymbolCommitsOnlyAcceptedAttempt(t *testing.T) {
	for _, transition := range []string{"canonical_retirement", "checked_read_cow", "unverified_event", "changed_physical", "caller_cancel", "enrichment_active"} {
		t.Run(transition, func(t *testing.T) {
			selectedProjectionRegistry(t)
			s, store, path := canonicalPromotionReadFixture(t)
			s.engine.SetSearchProvider(s.indexer.Search)
			require.NotNil(t, s.indexer.Search(), "use the daemon's real native backend")
			fts, err := store.SymbolFTSCount()
			require.NoError(t, err)
			require.Positive(t, fts)
			require.Nil(t, s.semanticMgr, "the native fixture starts without enrichment")
			if transition == "enrichment_active" {
				s.semanticMgr = &semantic.Manager{} // Existing no-provider enrichment admission fixture.
			}
			pending := make(chan indexer.MutationResult)
			s.SetWatcher(mutationTestWatcher{done: pending})
			var receipt *mutationReceipt
			t.Cleanup(func() {
				close(pending)
				if receipt != nil {
					select {
					case <-receipt.done:
					case <-time.After(3 * time.Second):
						t.Error("owned mutation receipt did not join")
					}
				}
			})
			const session = "registered-full-symbol-acceptance"
			require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
			t.Cleanup(func() { s.DisconnectSession(session) })
			base := s.WithClientSession(WithSessionID(t.Context(), session), session)
			s.NoteSessionToolPolicy(session, "all", "defer")
			require.NotNil(t, s.MCPServer().HandleMessage(base, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"symbol-acceptance-control","version":"1"}}}`)))
			s.MCPServer().HandleMessage(base, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
			const body = "func Target() int {\n\tvalue := 7\n\treturn value\n}"
			ack := preselectionRegisteredCall(t, s, base, 10, "edit_symbol", map[string]any{"id": "main.go::Target", "old_source": "func Target() int { return 1 }", "new_source": body, "dry_run": false})
			require.False(t, ack.IsError, resultText(ack))
			fields := extractTextResult(t, ack)
			require.Equal(t, "local", fields["ack_mode"])
			require.Equal(t, true, fields["readback_exact"])
			value, found := s.mutationReceipts.Load(fields["reindex_receipt"].(string))
			require.True(t, found)
			receipt = value.(*mutationReceipt)
			entry := liveEntry(pendingFileProjections, path)
			require.NotNil(t, entry)
			require.Positive(t, entry.sourceCommitSeq)
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			physicalSHA := sha256.Sum256(raw)
			require.Equal(t, 5, store.GetNode("main.go::Neighbor").StartLine)
			retirement := s.canonicalProjectionRetirement(path)
			require.NotNil(t, retirement)
			leases := graphview.NewLeaseManager()
			require.NoError(t, leases.RegisterRepositoryOwner(graphview.RepositoryOwner{GraphID: "symbol-graph", CheckoutID: "symbol-checkout", Incarnation: "symbol-incarnation", RepoPrefix: "symbol-fixture"}))
			s.materializer = &graphview.Materializer{Leases: leases}
			state := s.sessionFor(base)
			state.mu.Lock()
			state.viewedSymbols = nil
			state.mu.Unlock()
			viewed := func() []string {
				state.mu.Lock()
				defer state.mu.Unlock()
				return append([]string(nil), state.viewedSymbols...)
			}
			leaf, found := s.facades.legacy("get_symbol")
			require.True(t, found)
			caller, cancel := context.WithCancel(base)
			defer cancel()
			deadline := time.Now().Add(projectedReadBudget)
			ctx := context.WithValue(caller, projectedSourceDeadlineKey{}, deadline)
			calls, accepted := 0, 0
			var original context.Context
			var heldScopes []*graphview.RepositoryReadLease
			var leakedBeforeAcceptance bool
			var onOwner func(func()) bool
			s.addTool(leaf.tool, func(attempt context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				var result *mcplib.CallToolResult
				var callErr error
				if !onOwner(func() {
					calls++
					scope := requestRepositoryScopeFromContext(attempt)
					require.NotNil(t, scope, "real registered request must hold repository authority")
					require.Positive(t, scope.Holders())
					heldScopes = append(heldScopes, scope)
					require.LessOrEqual(t, calls, 2)
					require.Equal(t, deadline, attempt.Value(projectedSourceDeadlineKey{}))
					result, callErr = leaf.handler(attempt, req)
					require.NoError(t, callErr)
					require.NotNil(t, result)
					require.False(t, result.IsError, resultText(result))
					payload := extractTextResult(t, result)["node"].(map[string]any)
					require.Equal(t, "main.go::Target", payload["id"])
					require.Equal(t, float64(3), payload["start_line"])
					require.Equal(t, float64(6), payload["end_line"])
					afterFreshSymbolAcceptance(attempt, func() { accepted++ })
					if calls != 1 {
						return
					}
					original = attempt
					leakedBeforeAcceptance = len(viewed()) != 0
					require.True(t, s.hasRequestProjectionFact(attempt, "main.go"))
					require.NoError(t, validateRequestProjections(attempt))
					switch transition {
					case "canonical_retirement", "caller_cancel", "enrichment_active":
						require.NoError(t, s.indexer.IndexFile(path))
						require.Equal(t, 8, store.GetNode("main.go::Neighbor").StartLine)
						retirement.run()
						require.True(t, entry.checkedRetired.Load())
						require.False(t, entry.suspect.Load())
						require.Nil(t, liveEntry(pendingFileProjections, path))
					case "checked_read_cow", "unverified_event":
						pendingFileProjections.markSuspect(path)
						require.True(t, entry.suspect.Load())
						if transition == "checked_read_cow" {
							peer, readErr := externalVersionRegisteredCall(s, ctx, 30, "get_symbol_source", map[string]any{"id": "main.go::Neighbor", "context_lines": 0, "format": "json"})
							require.NoError(t, readErr)
							require.False(t, peer.IsError, resultText(peer))
							require.Equal(t, "func Neighbor() int { return 9 }", extractTextResult(t, peer)["source"])
							current := liveEntry(pendingFileProjections, path)
							require.NotNil(t, current)
							require.NotSame(t, entry, current)
							require.Same(t, entry.layer, current.layer)
							require.Equal(t, entry.sourceCommitSeq, current.sourceCommitSeq)
							require.True(t, current.readVersionChecked)
							require.Zero(t, current.sourceEvents)
						}
					case "changed_physical":
						require.NoError(t, os.WriteFile(path, []byte("package probe\n\nfunc Target() int { return 99 }\n"), 0600))
					}
					require.ErrorIs(t, validateRequestProjections(original), projectionSuperseded, "old borrowed facts must stay invalid")
					require.NoError(t, attempt.Err())
					require.True(t, time.Now().Before(deadline), "the real transition must fit the original150ms")
					if transition != "changed_physical" {
						after, diskErr := os.ReadFile(path)
						require.NoError(t, diskErr)
						require.Equal(t, physicalSHA, sha256.Sum256(after))
					}
					t.Logf("SYMBOL_AFTER_LEAF_CAUSAL transition=%s original150_live=true oldfacts_invalid=true leaf_calls=%d effects_before_acceptance=%v", transition, calls, leakedBeforeAcceptance)
					if transition == "caller_cancel" {
						cancel()
					}
				}) {
					return mcplib.NewToolResultError("owned fixture callback aborted"), nil
				}
				return result, callErr
			})
			result, callErr := canonicalReadWithOwner(t, func(dispatch func(func()) bool) (*mcplib.CallToolResult, error) {
				onOwner = dispatch
				return externalVersionRegisteredCall(s, ctx, 20, "get_symbol", map[string]any{"id": "main.go::Target", "detail": "full", "format": "json"})
			})
			require.NotNil(t, original)
			for _, scope := range heldScopes {
				require.Eventually(t, func() bool { return scope.Holders() == 0 }, 3*time.Second, time.Millisecond, "join every discarded or final attempt repository lease after dispatch, including cancelled workers")
			}
			success := transition == "canonical_retirement" || transition == "checked_read_cow"
			if success {
				require.NoError(t, callErr)
				require.NotNil(t, result)
				assert.False(t, result.IsError, resultText(result))
				assert.Equal(t, 2, calls, "only a fresh bounded server attempt can use the checked current view")
				assert.Equal(t, 1, accepted, "discarded assembly cannot commit acceptance callbacks")
				assert.Contains(t, viewed(), "main.go::Target")
			} else {
				assert.True(t, callErr != nil || result != nil && result.IsError, "unproved or cancelled views stay refused")
				assert.Equal(t, 1, calls)
				assert.Zero(t, accepted, "a refused answer receives no accepted bookkeeping")
				if transition != "checked_read_cow" {
					assert.Empty(t, viewed())
				}
			}
			assert.False(t, leakedBeforeAcceptance, "recordSymbol follows accepted answers, not discarded leaf work")
			t.Logf("SYMBOL_AFTER_LEAF_RESULT transition=%s calls=%d accepted_callbacks=%d remaining_original150=%s", transition, calls, accepted, time.Until(deadline))
		})
	}
}

func TestFullSymbolReplayPolicyKeepsOriginalAllowanceAndUnsafeShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
		safe bool
	}{
		{"full_json", map[string]any{"id": "main.go::Target", "detail": "full", "format": "json"}, true},
		{"full_default_json", map[string]any{"id": "main.go::Target", "detail": "full"}, true},
		{"brief", map[string]any{"id": "main.go::Target", "detail": "brief", "format": "json"}, false},
		{"default_brief", map[string]any{"id": "main.go::Target", "format": "json"}, false},
		{"toon", map[string]any{"id": "main.go::Target", "detail": "full", "format": "toon"}, false},
		{"malformed_detail", map[string]any{"id": "main.go::Target", "detail": true, "format": "json"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			req := makeReq("get_symbol", tc.args)
			require.Equal(t, tc.safe, auditedProjectionReadRetry("get_symbol", req))
			deadline := time.Now().Add(projectedReadBudget / 2)
			ctx := context.WithValue(t.Context(), projectedSourceDeadlineKey{}, deadline)
			calls := 0
			_, err := s.withProjectedSourceReadBudget(func(selected context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				calls++
				require.Equal(t, deadline, selected.Value(projectedSourceDeadlineKey{}), "binding never resets the caller's earlier private deadline")
				allowance, ok := selected.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
				require.True(t, ok, "every symbol shape retains the original early-selection allowance")
				require.True(t, allowance.claim())
				require.True(t, allowance.claim())
				require.False(t, allowance.claim(), "full-symbol replay cannot create a third attempt")
				return mcplib.NewToolResultText("policy control"), nil
			})(ctx, req)
			require.NoError(t, err)
			require.Equal(t, 1, calls)
		})
	}
}
