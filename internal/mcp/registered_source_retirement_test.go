package mcp

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// Exercise the same real canonical publication as the direct-source fixture,
// through registered raw and facade dispatch. This controlled transition is
// not a claim about the cause of a historical native projection refusal.
func TestRegisteredSourceCanonicalRetirementKeepsOriginalAllowance(t *testing.T) {
	for _, surface := range []string{"raw", "facade"} {
		for _, transition := range []string{"canonical_promotion", "peer_same_bytes", "changed_bytes", "expired_allowance"} {
			t.Run(surface+"/"+transition, func(t *testing.T) {
				selectedProjectionRegistry(t)
				s, _, path := canonicalPromotionReadFixture(t)
				_, _, entry, raw := acknowledgedCanonicalFileOnFixture(t, s, path)
				require.Equal(t, entry.contentSHA256, fmt.Sprintf("%x", sha256.Sum256(raw)))
				retirement := s.canonicalProjectionRetirement(path)
				require.NotNil(t, retirement)
				const session = "source-retirement-readback"
				require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
				t.Cleanup(func() { s.DisconnectSession(session) })
				base := s.WithClientSession(WithSessionID(t.Context(), session), session)
				s.NoteSessionToolPolicy(session, "all", "defer")
				if surface == "facade" {
					s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
				}
				require.NotNil(t, s.MCPServer().HandleMessage(base, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"source-retirement","version":"1"}}}`)))
				captured, found := s.facades.legacy("get_symbol_source")
				require.True(t, found)
				// Real IndexFile orchestration uses the existing lifecycle fixture
				// allowance, not a production latency or throughput measurement.
				deadline := time.Now().Add(canonicalLifecycleFixtureBudget)
				allowance := &projectedSourceAllowance{tool: "get_symbol_source"}
				ctx := context.WithValue(base, projectedSourceDeadlineKey{}, deadline)
				ctx = context.WithValue(ctx, projectedSourceAllowanceKey{}, allowance)
				var first context.Context
				var onOwner func(func()) bool
				calls := 0
				s.addTool(captured.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
					var result *mcplib.CallToolResult
					var readErr error
					if !onOwner(func() {
						calls++
						require.Same(t, allowance, ctx.Value(projectedSourceAllowanceKey{}))
						require.Equal(t, deadline, ctx.Value(projectedSourceDeadlineKey{}))
						result, readErr = captured.handler(ctx, req)
						require.NoError(t, readErr)
						require.False(t, result.IsError, resultText(result))
						require.Equal(t, "func Target() int {\n\tvalue := 7\n\treturn value\n}", extractTextResult(t, result)["source"])
						if calls != 1 {
							return
						}
						first = ctx
						require.True(t, s.hasRequestProjectionFact(ctx, "main.go"))
						switch transition {
						case "canonical_promotion", "expired_allowance":
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
							held, release, err := acquireMutationPath(t.Context(), path)
							require.NoError(t, err)
							commit, err := s.commitFileMutation(held, "write_file", "", "", "main.go", path, after, 0o600)
							release()
							require.NoError(t, err)
							require.Greater(t, commit.commitSeq, entry.commitSeq)
							require.NotSame(t, entry, liveEntry(pendingFileProjections, path))
						}
						require.ErrorIs(t, validateRequestProjections(ctx), projectionSuperseded)
					}) {
						return mcplib.NewToolResultError("fixture callback aborted"), nil
					}
					return result, readErr
				})
				tool := "get_symbol_source"
				args := map[string]any{"id": "main.go::Target", "context_lines": 0, "format": "json"}
				if surface == "facade" {
					tool = "read"
					args = map[string]any{"operation": "source", "target": map[string]any{"symbol": "main.go::Target"}, "options": map[string]any{"context_lines": 0}, "output": map[string]any{"format": "json"}}
				}
				result, err := canonicalReadWithOwner(t, func(dispatch func(func()) bool) (*mcplib.CallToolResult, error) {
					onOwner = dispatch
					return externalVersionRegisteredCall(s, ctx, 20, tool, args)
				})
				require.NoError(t, err)
				require.NotNil(t, first)
				require.NotNil(t, result)
				t.Logf("surface=%s transition=%s leaf_calls=%d attempts=%d result=%s", surface, transition, calls, allowance.attempts.Load(), resultText(result))
				if transition == "canonical_promotion" {
					require.False(t, result.IsError, "checked retirement must allow a fresh whole source attempt: %s", resultText(result))
					require.Equal(t, 2, calls)
					require.Equal(t, int32(2), allowance.attempts.Load())
				} else {
					require.True(t, result.IsError, "physical supersession or expired allowance cannot gain another source attempt")
					require.Equal(t, 1, calls)
					require.Equal(t, int32(1), allowance.attempts.Load())
				}
				after, err := os.ReadFile(path)
				require.NoError(t, err)
				if transition != "changed_bytes" {
					require.Equal(t, raw, after, "canonical publication and same-byte peer cannot alter ACK content")
				}
				require.ErrorIs(t, validateRequestProjections(context.WithoutCancel(first)), projectionSuperseded, "a fresh attempt must not revive the original selection")
			})
		}
	}
}
