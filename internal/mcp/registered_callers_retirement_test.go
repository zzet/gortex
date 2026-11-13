package mcp

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// Isolate a checked canonical retirement from physical peer replacement. This
// fixture does not identify the transition behind a retained paired refusal.
func TestRegisteredCallersProjectionTransitionBoundary(t *testing.T) {
	for _, surface := range []string{"raw", "facade"} {
		for _, transition := range []string{"unchanged", "canonical_retirement", "peer_same_bytes", "changed_bytes", "expired_allowance", "active_enrichment"} {
			t.Run(surface+"/"+transition, func(t *testing.T) {
				selectedProjectionRegistry(t)
				s, store, path := canonicalPromotionReadFixture(t)
				before, err := os.ReadFile(path)
				require.NoError(t, err)
				before = append(before, []byte("\nfunc Caller() int { return Target() }\n")...)
				require.NoError(t, os.WriteFile(path, before, 0600))
				require.NoError(t, s.indexer.IndexFile(path))
				require.NotNil(t, store.GetNode("main.go::Caller"))
				_, _, entry, raw := acknowledgedCanonicalFileOnFixture(t, s, path)
				callerOffset := strings.Index(string(raw), "func Caller()")
				require.NotEqual(t, -1, callerOffset)
				callerLine := strings.Count(string(raw[:callerOffset]), "\n") + 1
				requireCallerEdge := func(result *mcplib.CallToolResult) {
					t.Helper()
					var body struct {
						Edges []struct {
							From     string `json:"from"`
							To       string `json:"to"`
							Kind     string `json:"kind"`
							FilePath string `json:"file_path"`
							Line     int    `json:"line"`
						} `json:"edges"`
					}
					require.NoError(t, json.Unmarshal([]byte(resultText(result)), &body))
					found := false
					for _, edge := range body.Edges {
						if edge.From == "main.go::Caller" && edge.To == "main.go::Target" && edge.Kind == "calls" {
							found = true
							require.Equal(t, "main.go", edge.FilePath)
							require.Equal(t, callerLine, edge.Line)
						}
					}
					require.True(t, found, "the real caller edge must survive the publication")
				}
				retirement := s.canonicalProjectionRetirement(path)
				require.NotNil(t, retirement)
				const session = "caller-retirement-readback"
				require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
				t.Cleanup(func() { s.DisconnectSession(session) })
				base := s.WithClientSession(WithSessionID(t.Context(), session), session)
				s.NoteSessionToolPolicy(session, "all", "defer")
				if surface == "facade" {
					s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
				}
				require.NotNil(t, s.MCPServer().HandleMessage(base, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"caller-retirement","version":"1"}}}`)))
				captured, found := s.facades.legacy("get_callers")
				require.True(t, found)
				if transition == "active_enrichment" {
					// Real output admission before a passive provider startup failure
					// must preserve the existing no-replay veto.
					registerFailingLSPCandidate(t, s)
				}
				// Retain the production projection allowance; no test-only extension.
				deadline := time.Now().Add(projectedReadBudget)
				ctx := context.WithValue(base, projectedSourceDeadlineKey{}, deadline)
				var first context.Context
				var onOwner func(func()) bool
				calls := 0
				s.addTool(captured.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
					var result *mcplib.CallToolResult
					var readErr error
					if !onOwner(func() {
						calls++
						require.Equal(t, deadline, ctx.Value(projectedSourceDeadlineKey{}))
						result, readErr = captured.handler(ctx, req)
						require.NoError(t, readErr)
						require.False(t, result.IsError, resultText(result))
						requireCallerEdge(result)
						if transition == "active_enrichment" {
							require.True(t, ctx.Value(freshSymbolAttemptKey{}).(*freshSymbolAttempt).replayUnsafe)
						}
						if calls != 1 {
							return
						}
						first = ctx
						require.True(t, s.hasRequestProjectionFact(ctx, "main.go"))
						switch transition {
						case "unchanged":
							return
						case "canonical_retirement", "expired_allowance", "active_enrichment":
							require.NoError(t, s.indexer.IndexFile(path))
							retirement.run()
							require.Nil(t, liveEntry(pendingFileProjections, path))
							require.True(t, entry.checkedRetired.Load())
							require.False(t, entry.suspect.Load())
							require.True(t, time.Now().Before(deadline), "controlled publication must fit the original allowance")
							if transition == "expired_allowance" {
								timer := time.NewTimer(time.Until(deadline))
								defer timer.Stop()
								<-timer.C
							}
						default:
							after := raw
							if transition == "changed_bytes" {
								after = []byte(strings.Replace(string(raw), "value := 7", "value := 8", 1))
							}
							held, release, lockErr := acquireMutationPath(t.Context(), path)
							require.NoError(t, lockErr)
							commit, commitErr := s.commitFileMutation(held, "write_file", "", "", "main.go", path, after, 0600)
							release()
							require.NoError(t, commitErr)
							require.Greater(t, commit.commitSeq, entry.commitSeq)
							require.NotSame(t, entry, liveEntry(pendingFileProjections, path))
						}
						require.ErrorIs(t, validateRequestProjections(ctx), projectionSuperseded)
					}) {
						return mcplib.NewToolResultError("fixture callback aborted"), nil
					}
					return result, readErr
				})
				tool := "get_callers"
				args := map[string]any{"id": "main.go::Target", "format": "json", "max_bytes": 0}
				if surface == "facade" {
					tool = "relations"
					args = map[string]any{"operation": "callers", "target": map[string]any{"symbol": "main.go::Target"}, "output": map[string]any{"format": "json", "max_bytes": 0}}
				}
				result, err := canonicalReadWithOwner(t, func(dispatch func(func()) bool) (*mcplib.CallToolResult, error) {
					onOwner = dispatch
					return externalVersionRegisteredCall(s, ctx, 20, tool, args)
				})
				require.NoError(t, err)
				require.NotNil(t, first)
				require.NotNil(t, result)
				t.Logf("surface=%s transition=%s leaf_calls=%d result=%s", surface, transition, calls, resultText(result))
				if transition == "unchanged" || transition == "canonical_retirement" {
					require.False(t, result.IsError, "an unchanged canonical publication should remain available: %s", resultText(result))
					requireCallerEdge(result)
					if transition == "canonical_retirement" {
						require.Equal(t, 2, calls, "a fresh whole attempt must replace the retired selection")
					} else {
						require.Equal(t, 1, calls)
					}
				} else {
					require.True(t, result.IsError, "physical supersession or expired allowance must refuse")
					require.Equal(t, 1, calls)
				}
				if transition != "unchanged" {
					require.ErrorIs(t, validateRequestProjections(context.WithoutCancel(first)), projectionSuperseded, "completion must not revive the old selection")
				}
			})
		}
	}
}

// The ordinary-call repair must not replace the existing opt-in freshness
// controller's deadline or replay policy with the projection allowance.
func TestCallersProjectionBudgetPreservesExplicitFreshness(t *testing.T) {
	for _, surface := range []string{"raw", "facade"} {
		for _, mode := range []string{"ordinary", "fresh", "exact", "fresh_exact", "deadline"} {
			t.Run(surface+"/"+mode, func(t *testing.T) {
				s, _, _ := canonicalPromotionReadFixture(t)
				options := map[string]any{}
				if mode == "fresh" || mode == "fresh_exact" {
					options[requireFreshArgName] = true
				}
				if mode == "exact" || mode == "fresh_exact" {
					options[requireExactArgName] = true
				}
				if mode == "deadline" {
					options[waitDeadlineArgName] = time.Now().Add(time.Minute).Format(time.RFC3339Nano)
				}
				req := makeReq("get_callers", options)
				if surface == "facade" {
					req = makeReq("relations", map[string]any{"operation": "callers", "options": options})
				}
				original := time.Now().Add(time.Minute)
				ctx := context.WithValue(t.Context(), projectedSourceDeadlineKey{}, original)
				called := false
				handler := s.withProjectedSourceReadBudget(func(ctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
					called = true
					allowance, exists := ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
					if mode == "ordinary" {
						require.True(t, exists)
						require.Equal(t, "get_callers", allowance.tool)
						require.True(t, ctx.Value(projectedSourceDeadlineKey{}).(time.Time).Before(original))
					} else {
						require.False(t, exists, "explicit freshness keeps its existing controller")
						require.Equal(t, original, ctx.Value(projectedSourceDeadlineKey{}))
					}
					return mcplib.NewToolResultText("ok"), nil
				})
				result, err := handler(ctx, req)
				require.NoError(t, err)
				require.True(t, called)
				require.False(t, result.IsError)
			})
		}
	}
}
