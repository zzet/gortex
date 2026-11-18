package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// The binding control exposes the real producer's typed refusal; the normal
// handler case must preserve that same refusal through registered dispatch.
// This isolates ordering and does not attribute a retained workload failure.
func TestRegisteredCallersEarlyBindPreservesTypedRetirement(t *testing.T) {
	for _, surface := range []string{"raw", "facade"} {
		for _, boundary := range []string{"production", "binding_control"} {
			for _, transition := range []string{"unchanged", "canonical_retirement"} {
				t.Run(surface+"/"+boundary+"/"+transition, func(t *testing.T) {
					selectedProjectionRegistry(t)
					s, store, path := newAssemblyRetirementFixture(t)
					before, err := os.ReadFile(path)
					require.NoError(t, err)
					before = append(before, []byte("\nfunc Caller() int { return Target() }\n")...)
					require.NoError(t, os.WriteFile(path, before, 0600))
					require.NoError(t, s.indexer.IndexFile(path))
					oldCaller := store.GetNode("main.go::Caller")
					require.NotNil(t, oldCaller)
					_, _, entry, raw := acknowledgedCanonicalFileOnFixture(t, s, path)
					callerOffset := strings.Index(string(raw), "func Caller()")
					require.NotEqual(t, -1, callerOffset)
					callerLine := strings.Count(string(raw[:callerOffset]), "\n") + 1
					require.Less(t, oldCaller.StartLine, callerLine)
					retirement := s.canonicalProjectionRetirement(path)
					require.NotNil(t, retirement)
					const session = "caller-early-bind"
					require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
					t.Cleanup(func() { s.DisconnectSession(session) })
					base := s.WithClientSession(WithSessionID(t.Context(), session), session)
					s.NoteSessionToolPolicy(session, "all", "defer")
					if surface == "facade" {
						s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
					}
					require.NotNil(t, s.MCPServer().HandleMessage(base, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"caller-early-bind","version":"1"}}}`)))
					captured, found := s.facades.legacy("get_callers")
					require.True(t, found)
					var deadline time.Time
					var onOwner func(func()) bool
					var first context.Context
					var firstBindErr error
					calls, observations := 0, 0
					observer := checkedProjectionReadObserver(func(build context.Context, expected *projectionEntry) {
						onOwner(func() {
							observations++
							require.Equal(t, 1, observations)
							require.Same(t, entry, expected)
							require.Equal(t, deadline, build.Value(projectedSourceDeadlineKey{}))
							require.NoError(t, build.Err())
							if transition == "unchanged" {
								return
							}
							require.NoError(t, s.indexer.IndexFile(path))
							retirement.run()
							require.Nil(t, liveEntry(pendingFileProjections, path))
							require.True(t, entry.checkedRetired.Load())
							require.False(t, entry.suspect.Load())
							currentCaller := store.GetNode("main.go::Caller")
							require.NotNil(t, currentCaller)
							require.Equal(t, callerLine, currentCaller.StartLine)
							require.True(t, time.Now().Before(deadline), "real publication must fit the ORIGINAL allowance")
						})
					})
					s.addTool(captured.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
						calls++
						if calls == 1 {
							first = ctx
						}
						ctx = context.WithValue(ctx, checkedProjectionReadObserverKey{}, observer)
						if boundary == "binding_control" {
							bound, bindErr := s.bindPendingRelationReads(ctx)
							if calls == 1 {
								firstBindErr = bindErr
							}
							if bindErr != nil {
								return nil, bindErr
							}
							ctx = bound
						}
						return captured.handler(ctx, req)
					})
					tool := "get_callers"
					args := map[string]any{"id": "main.go::Target", "format": "json", "max_bytes": 0}
					if surface == "facade" {
						tool = "relations"
						args = map[string]any{"operation": "callers", "target": map[string]any{"symbol": "main.go::Target"}, "output": map[string]any{"format": "json", "max_bytes": 0}}
					}
					deadline = time.Now().Add(projectedReadBudget)
					ctx := context.WithValue(base, projectedSourceDeadlineKey{}, deadline)
					result, err := canonicalReadWithOwner(t, func(dispatch func(func()) bool) (*mcplib.CallToolResult, error) {
						onOwner = dispatch
						return externalVersionRegisteredCall(s, ctx, 20, tool, args)
					})
					require.NoError(t, err)
					require.NotNil(t, result)
					require.NotNil(t, first)
					require.Equal(t, 1, observations, "the actual pre-live-owner checked read must execute")
					allowance := first.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
					t.Logf("surface=%s boundary=%s transition=%s leaf_calls=%d attempts=%d bind_moved=%v remaining=%s result=%s", surface, boundary, transition, calls, allowance.attempts.Load(), errors.Is(firstBindErr, errProjectedSourceMoved), time.Until(deadline), resultText(result))
					if boundary == "binding_control" {
						if transition == "canonical_retirement" {
							require.ErrorIs(t, firstBindErr, errProjectedSourceMoved)
						} else {
							require.NoError(t, firstBindErr)
						}
					}
					require.False(t, result.IsError, "the existing typed producer signal must reach the bounded whole-attempt controller: %s", resultText(result))
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
					foundEdge := false
					for _, edge := range body.Edges {
						if edge.From == "main.go::Caller" && edge.To == "main.go::Target" && edge.Kind == "calls" {
							foundEdge = true
							require.Equal(t, "main.go", edge.FilePath)
							require.Equal(t, callerLine, edge.Line)
						}
					}
					require.True(t, foundEdge, "current independently shifted caller edge must survive publication")
					if transition == "canonical_retirement" {
						require.Equal(t, 2, calls)
						require.Equal(t, int32(2), allowance.attempts.Load())
					} else {
						require.Equal(t, 1, calls)
						require.Equal(t, int32(1), allowance.attempts.Load())
					}
				})
			}
		}
	}
}
