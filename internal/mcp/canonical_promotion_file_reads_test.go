package mcp

import (
	"context"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"os"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

func acknowledgedCanonicalFile(t *testing.T) (*Server, string, *projectionEntry, []byte) {
	t.Helper()
	s, _, path := canonicalPromotionReadFixture(t)
	pending := make(chan indexer.MutationResult)
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
	s.SetWatcher(mutationTestWatcher{done: pending})
	ack, err := s.runAcknowledgedEdit(t.Context(), makeReq("edit_file", map[string]any{
		"path": path, "old_string": "func Target() int { return 1 }",
		"new_string": "func Target() int {\n\tvalue := 7\n\treturn value\n}", "format": "json",
	}), s.handleEditFile)
	require.NoError(t, err)
	require.False(t, ack.IsError, resultText(ack))
	fields := extractTextResult(t, ack)
	require.Equal(t, "local", fields["ack_mode"])
	require.Equal(t, "committed", fields["disk_status"])
	value, found := s.mutationReceipts.Load(fields["reindex_receipt"].(string))
	require.True(t, found)
	receipt = value.(*mutationReceipt)
	entry := liveEntry(pendingFileProjections, path)
	require.NotNil(t, entry)
	require.Positive(t, entry.commitSeq)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return s, path, entry, raw
}

func requireCanonicalDefinitionRows(t *testing.T, tool string, result *mcplib.CallToolResult, raw []byte) {
	t.Helper()
	require.NotNil(t, result)
	require.False(t, result.IsError, resultText(result))
	field := "nodes"
	if tool == "get_editing_context" {
		field = "defines"
	}
	rows, ok := extractTextResult(t, result)[field].([]any)
	require.True(t, ok)
	byID := make(map[string]map[string]any)
	for _, value := range rows {
		row := value.(map[string]any)
		byID[row["id"].(string)] = row
	}
	files := token.NewFileSet()
	parsed, err := goparser.ParseFile(files, "main.go", raw, 0)
	require.NoError(t, err)
	functions := 0
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		functions++
		row := byID["main.go::"+function.Name.Name]
		require.NotNil(t, row, "local definitions cannot substitute for the required function")
		require.Equal(t, function.Name.Name, row["name"])
		require.Equal(t, "function", row["kind"])
		require.Equal(t, float64(files.Position(function.Pos()).Line), row["start_line"])
		if tool == "get_file_summary" {
			require.Equal(t, float64(files.Position(function.End()).Line), row["end_line"])
		}
	}
	require.Equal(t, 2, functions, "the independent source oracle includes target and shifted neighbor")
}

func TestCanonicalPromotionFileReadsKeepIndependentDefinitionOracle(t *testing.T) {
	for _, tool := range []string{"get_file_summary", "get_editing_context"} {
		for _, transition := range []string{"canonical_promotion", "peer_same_bytes", "same_bytes_event", "unchecked_eviction"} {
			t.Run(tool+"/"+transition, func(t *testing.T) {
				selectedProjectionRegistry(t)
				s, path, entry, raw := acknowledgedCanonicalFile(t)
				retirement := s.canonicalProjectionRetirement(path)
				require.NotNil(t, retirement)
				deadline := time.Now().Add(projectedReadBudget)
				ctx := context.WithValue(t.Context(), projectedSourceDeadlineKey{}, deadline)
				beforeCount := s.tokenStatsFor(t.Context()).snapshot()["calls_counted"].(int64)
				calls := 0
				var first context.Context
				leaf := func(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
					calls++
					require.Equal(t, deadline, ctx.Value(projectedSourceDeadlineKey{}))
					var result *mcplib.CallToolResult
					var err error
					if tool == "get_file_summary" {
						result, err = s.handleGetFileSummary(ctx, request)
					} else {
						result, err = s.handleGetEditingContext(ctx, request)
					}
					require.NoError(t, err)
					requireCanonicalDefinitionRows(t, tool, result, raw)
					if calls != 1 {
						return result, nil
					}
					first = ctx
					require.True(t, s.hasRequestProjectionFact(ctx, "main.go"))
					switch transition {
					case "canonical_promotion":
						require.Equal(t, 5, s.graph.GetNode("main.go::Neighbor").StartLine)
						require.NoError(t, s.indexer.IndexFile(path))
						require.Equal(t, 8, s.graph.GetNode("main.go::Neighbor").StartLine)
						retirement.run()
						require.True(t, entry.checkedRetired.Load())
						require.Nil(t, liveEntry(pendingFileProjections, path))
						require.True(t, time.Now().Before(deadline))
					case "peer_same_bytes":
						held, release, err := acquireMutationPath(t.Context(), path)
						require.NoError(t, err)
						commit, writeErr := s.commitFileMutation(held, "write_file", "", "", "main.go", path, raw, 0600)
						release()
						require.NoError(t, writeErr)
						require.Greater(t, commit.commitSeq, entry.commitSeq)
					case "same_bytes_event":
						pendingFileProjections.markSuspect(path)
					case "unchecked_eviction":
						pendingFileProjections.drop(entry)
						require.False(t, entry.checkedRetired.Load())
					}
					require.ErrorIs(t, validateRequestProjections(first), projectionSuperseded)
					return result, nil
				}
				result, err := s.wrapToolHandler(leaf)(ctx, makeReq(tool, map[string]any{"path": "main.go", "format": "json"}))
				require.NoError(t, err)
				require.NotNil(t, result)
				t.Logf("tool=%s transition=%s attempts=%d error=%v", tool, transition, calls, result.IsError)
				require.LessOrEqual(t, calls, 2)
				facts := first.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
				facts.mu.RLock()
				borrowed := facts.entries[entry.key]
				facts.mu.RUnlock()
				require.Same(t, entry, borrowed)
				if transition == "canonical_promotion" {
					requireCanonicalDefinitionRows(t, tool, result, raw)
					require.Equal(t, 2, calls)
					afterCount := s.tokenStatsFor(t.Context()).snapshot()["calls_counted"].(int64)
					require.Equal(t, int64(1), afterCount-beforeCount, "discarded attempts must not book a second accepted file read")
				} else {
					require.True(t, result.IsError)
					require.Contains(t, resultText(result), "superseded")
					require.Equal(t, 1, calls)
				}
			})
		}
	}
}
