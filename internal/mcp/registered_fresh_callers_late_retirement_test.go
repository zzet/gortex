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

// The ordinary registered-callers late-retirement control, with the actual
// explicit-fresh relations arguments. The real leaf first returns the current
// edge; checked canonical retirement then happens before response decoration.
// This separates normal publication from a new physical execution; it does not
// claim watcher-tier freshness in this held-ticket component fixture.
func TestRegisteredFreshCallersLateProjectionTransitionBoundary(t *testing.T) {
	for _, surface := range []string{"raw", "facade"} {
		for _, transition := range []string{"unchanged", "canonical_retirement", "peer_same_bytes", "changed_bytes", "source_event", "read_cow", "expired_allowance"} {
			t.Run(surface+"/"+transition, func(t *testing.T) {
				selectedProjectionRegistry(t)
				t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", "1")
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
				original, cancel := context.WithTimeout(base, 5*time.Second)
				defer cancel()
				originalDeadline, _ := original.Deadline()
				ctx := context.WithValue(original, projectedSourceDeadlineKey{}, deadline)
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
						case "source_event":
							pendingFileProjections.markSuspect(path)
						case "read_cow":
							pendingFileProjections.markSuspect(path)
							current, err := s.projectCurrentTrackedFile(ctx, ctx, path, "main.go", "main.go")
							require.NoError(t, err)
							require.NotNil(t, current)
							require.NotSame(t, entry, current)
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
				args := sharedReceiptCallersArguments()
				if surface == "facade" {
					tool = "relations"
					args = map[string]any{"operation": "callers", "target": map[string]any{"symbol": "main.go::Target"}, "require_fresh": true, "freshness_scope": "relations", "output": map[string]any{"format": "json", "max_bytes": 0}}
				}
				result, err := canonicalReadWithOwner(t, func(dispatch func(func()) bool) (*mcplib.CallToolResult, error) {
					onOwner = dispatch
					return externalVersionRegisteredCall(s, ctx, 20, tool, args)
				})
				require.NoError(t, err)
				require.NotNil(t, first)
				require.NotNil(t, result)
				t.Logf("surface=%s transition=%s leaf_calls=%d result=%s", surface, transition, calls, resultText(result))
				stillOriginal, _ := ctx.Deadline()
				require.Equal(t, originalDeadline, stillOriginal)
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
