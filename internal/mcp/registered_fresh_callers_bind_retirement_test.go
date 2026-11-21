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

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type callersRetirementCarryStore struct {
	*store_sqlite.Store
	afterRead func(context.Context) bool
}

func (s *callersRetirementCarryStore) LayerContractFileNodesContext(ctx context.Context, repo string, paths []string) (map[string][]*graph.Node, error) {
	rows, err := s.Store.LayerContractFileNodesContext(ctx, repo, paths)
	if err == nil && !s.afterRead(ctx) {
		return nil, context.Canceled
	}
	return rows, err
}

func TestRegisteredFreshCallersRetirementDuringProjectionBind(t *testing.T) {
	for _, surface := range []string{"raw", "facade"} {
		for _, transition := range []string{"canonical_retirement", "read_built_retirement", "unchecked_removal", "physical_peer", "changed_bytes", "source_event", "read_cow"} {
			t.Run(surface+"/"+transition, func(t *testing.T) {
				selectedProjectionRegistry(t)
				if transition == "read_built_retirement" {
					t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", "0")
				} else {
					t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", "1")
				}
				s, store, path := canonicalPromotionReadFixture(t)
				before, err := os.ReadFile(path)
				require.NoError(t, err)
				before = append(before, []byte("\nfunc Caller() int { return Target() }\n")...)
				require.NoError(t, os.WriteFile(path, before, 0600))
				require.NoError(t, s.indexer.IndexFile(path))
				oldCaller := store.GetNode("main.go::Caller")
				require.NotNil(t, oldCaller)
				_, _, entry, raw := acknowledgedCanonicalFileOnFixture(t, s, path)
				if transition == "read_built_retirement" {
					sourceSeq := entry.sourceCommitSeq
					pendingFileProjections.markSuspect(path)
					entry, err = s.projectCurrentTrackedFile(t.Context(), t.Context(), path, "main.go", "main.go")
					require.NoError(t, err)
					require.NotNil(t, entry)
					require.Zero(t, entry.commitSeq)
					require.Positive(t, sourceSeq)
					require.Equal(t, sourceSeq, entry.sourceCommitSeq)
					require.True(t, entry.readVersionChecked)
				}
				callerOffset := strings.Index(string(raw), "func Caller()")
				require.NotEqual(t, -1, callerOffset)
				callerLine := strings.Count(string(raw[:callerOffset]), "\n") + 1
				require.Less(t, oldCaller.StartLine, callerLine)
				retirement := s.canonicalProjectionRetirement(path)
				require.NotNil(t, retirement)

				const session = "fresh-callers-bind-retirement"
				require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
				t.Cleanup(func() { s.DisconnectSession(session) })
				base := s.WithClientSession(WithSessionID(t.Context(), session), session)
				s.NoteSessionToolPolicy(session, "all", "defer")
				if surface == "facade" {
					s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
				}
				require.NotNil(t, s.MCPServer().HandleMessage(base, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"callers-bind-retirement","version":"1"}}}`)))

				var onOwner func(func()) bool
				var allowance *projectedSourceAllowance
				var deadline time.Time
				observed := false
				s.graph = &callersRetirementCarryStore{Store: store, afterRead: func(ctx context.Context) bool {
					return onOwner(func() {
						require.False(t, observed, "a rebound must not carry the removed projection")
						observed = true
						allowance, _ = ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
						require.NotNil(t, allowance)
						require.True(t, allowance.afterFreshness)
						deadline = ctx.Value(projectedSourceDeadlineKey{}).(time.Time)
						require.False(t, deadline.IsZero())
						require.Equal(t, int32(1), allowance.attempts.Load())
						require.Same(t, entry, liveEntry(pendingFileProjections, path))
						require.False(t, s.hasRequestProjectionFact(ctx, "main.go"))
						switch transition {
						case "canonical_retirement", "read_built_retirement":
							require.NoError(t, s.indexer.IndexFile(path))
							retirement.run()
							require.Nil(t, liveEntry(pendingFileProjections, path))
							require.True(t, entry.checkedRetired.Load())
							require.False(t, entry.suspect.Load())
							current := store.GetNode("main.go::Caller")
							require.NotNil(t, current)
							require.Equal(t, callerLine, current.StartLine)
						case "unchecked_removal":
							pendingFileProjections.drop(entry)
							require.Nil(t, liveEntry(pendingFileProjections, path))
							require.False(t, entry.checkedRetired.Load())
						case "source_event":
							pendingFileProjections.markSuspect(path)
							require.Same(t, entry, liveEntry(pendingFileProjections, path))
							require.True(t, entry.suspect.Load())
						case "read_cow":
							pendingFileProjections.markSuspect(path)
							current, err := s.projectCurrentTrackedFile(ctx, ctx, path, "main.go", "main.go")
							require.NoError(t, err)
							require.NotNil(t, current)
							require.NotSame(t, entry, current)
							require.Zero(t, current.commitSeq)
						case "physical_peer", "changed_bytes":
							after := raw
							if transition == "changed_bytes" {
								after = []byte(strings.Replace(string(raw), "value := 7", "value := 8", 1))
							}
							held, release, err := acquireMutationPath(t.Context(), path)
							require.NoError(t, err)
							commit, err := s.commitFileMutation(held, "write_file", "", "", "main.go", path, after, 0600)
							release()
							require.NoError(t, err)
							require.Greater(t, commit.commitSeq, entry.commitSeq)
							require.NotSame(t, entry, liveEntry(pendingFileProjections, path))
						}
						require.True(t, time.Now().Before(deadline))
					})
				}}

				tool, args := "get_callers", sharedReceiptCallersArguments()
				if surface == "facade" {
					tool = "relations"
					args = map[string]any{"operation": "callers", "target": map[string]any{"symbol": "main.go::Target"}, "require_fresh": true, "freshness_scope": "relations", "output": map[string]any{"format": "json", "max_bytes": 0}}
				}
				ctx, cancel := context.WithTimeout(base, 5*time.Second)
				defer cancel()
				original, _ := ctx.Deadline()
				result, err := canonicalReadWithOwner(t, func(dispatch func(func()) bool) (*mcplib.CallToolResult, error) {
					onOwner = dispatch
					return externalVersionRegisteredCall(s, ctx, 20, tool, args)
				})
				require.NoError(t, err)
				require.NotNil(t, result)
				require.True(t, observed, "the actual carry read must move the selected owner before retain")
				require.Equal(t, deadline, allowance.deadline)
				require.True(t, allowance.deadline.Before(original))
				t.Logf("surface=%s transition=%s attempts=%d response=%s meta=%v", surface, transition, allowance.attempts.Load(), resultText(result), result.Meta)
				if transition != "canonical_retirement" && transition != "read_built_retirement" {
					require.True(t, result.IsError)
					require.Equal(t, int32(1), allowance.attempts.Load())
					return
				}

				require.False(t, result.IsError, "a checked retirement before retain should allow one whole rebound")
				require.Equal(t, int32(2), allowance.attempts.Load())
				var response struct {
					Edges []struct {
						From, To, Kind string
						Line           int
					}
				}
				require.NoError(t, json.Unmarshal([]byte(resultText(result)), &response))
				count := 0
				for _, edge := range response.Edges {
					if edge.From == "main.go::Caller" && edge.To == "main.go::Target" && edge.Kind == "calls" {
						require.Equal(t, callerLine, edge.Line)
						count++
					}
				}
				require.Equal(t, 1, count, "the rebound must see the exact shifted canonical caller")
			})
		}
	}
}
