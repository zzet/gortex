package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"go.uber.org/zap"
)

// A scheduling component, not a native-run reproduction: actual SQLite,
// registered all/defer dispatch and durable local batch ACK, with the original
// publication ticket held. The resolved original call is a seeded base fact.
// No graph/disk/status read is inserted between ACK and the two public calls.
func TestRegisteredLocalBatchImmediateRelationsUseCurrentPendingFile(t *testing.T) {
	for _, crossFile := range []bool{false, true} {
		name := "same_file"
		if crossFile {
			name = "distinct_callee_file"
		}
		t.Run(name, func(t *testing.T) { runRegisteredLocalBatchImmediateRelations(t, crossFile, false) })
	}
}

func TestRegisteredLocalBatchRemovedCallDoesNotCarry(t *testing.T) {
	for _, crossFile := range []bool{false, true} {
		name := "same_file"
		if crossFile {
			name = "distinct_callee_file"
		}
		t.Run(name, func(t *testing.T) { runRegisteredLocalBatchImmediateRelations(t, crossFile, true) })
	}
}

func runRegisteredLocalBatchImmediateRelations(t *testing.T, crossFile, removeCall bool) {
	selectedProjectionRegistry(t)
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	before := "package probe\n\nfunc Caller() int { return Callee() }\n\nfunc Stable() int { return 2 }\n\nfunc Callee() int { return 7 }\n"
	after := "package probe\n\nfunc Caller() int {\n\tvalue := 0\n\treturn value + Callee()\n}\n\nfunc Stable() int { return 2 }\n\nfunc Callee() int { return 7 }\n"
	calleeFile := "main.go"
	if crossFile {
		calleeFile = "target.go"
		before = strings.Split(before, "\n\nfunc Callee")[0] + "\n"
		after = strings.Split(after, "\n\nfunc Callee")[0] + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(root, calleeFile), []byte("package probe\n\nfunc Callee() int { return 7 }\n"), 0600))
	}
	if removeCall {
		after = strings.Replace(after, "value + Callee()", "value + 1", 1)
	}
	require.NoError(t, os.WriteFile(path, []byte(before), 0600))
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "relations.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sql.Close()) })
	idx := newProjectionTestIndexer(t, sql)
	_, err = idx.Index(root)
	require.NoError(t, err)
	caller, callee := "main.go::Caller", calleeFile+"::Callee"
	require.NotNil(t, sql.GetNode(caller))
	require.NotNil(t, sql.GetNode(callee))
	sql.AddEdge(&graph.Edge{From: caller, To: callee, Kind: graph.EdgeCalls, FilePath: "main.go", Line: 3})
	s := NewServer(query.NewEngine(sql), sql, idx, nil, zap.NewNop(), nil)
	s.editAckMode = config.EditAckLocal
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	pending := make(chan indexer.MutationResult)
	s.SetWatcher(mutationTestWatcher{done: pending})
	t.Cleanup(func() {
		close(pending)
		s.mutationReceipts.Range(func(_, value any) bool {
			receipt := value.(*mutationReceipt)
			select {
			case <-receipt.done:
			case <-time.After(3 * time.Second):
				t.Error("owned batch publication ticket did not join")
			}
			return true
		})
	})
	const session = "registered-batch-immediate-relations"
	require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(session) })
	ctx := s.WithClientSession(WithSessionID(t.Context(), session), session)
	require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"relations-component","version":"1"}}}`)))
	s.NoteSessionToolPolicy(session, "all", "defer")
	// Seed graph correctness is checked before the original mutation.
	for i, name := range []string{"get_call_chain", "get_callers"} {
		target := caller
		if name == "get_callers" {
			target = callee
		}
		result := preselectionRegisteredCall(t, s, ctx, 2+i, name, map[string]any{"id": target, "depth": 1, "limit": 1000, "format": "json"})
		require.False(t, result.IsError, resultText(result))
		body := extractTextResult(t, result)
		require.NotEmpty(t, body["edges"], "original resolved baseline must exist before editing")
	}
	ack := preselectionRegisteredCall(t, s, ctx, 10, "batch_edit", map[string]any{"transaction_id": "immediate-relation-local", "edits": []any{map[string]any{"op": "edit_file", "path": path, "old_string": before, "new_string": after}}})
	// Do not inspect the ACK or disk before launching the immediate burst.
	type observed struct {
		tool   string
		result *mcplib.CallToolResult
		err    error
	}
	answers := make(chan observed, 2)
	var workers sync.WaitGroup
	for i, tool := range []string{"get_callers", "get_call_chain"} {
		workers.Go(func() {
			requestCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
			defer cancel()
			target := caller
			if tool == "get_callers" {
				target = callee
			}
			name := tool
			s.EnsureToolPromotedForSession(requestCtx, name)
			frame, marshalErr := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 20 + i, "method": "tools/call", "params": map[string]any{"name": name, "arguments": map[string]any{"id": target, "depth": 1, "limit": 1000, "format": "json"}}})
			if marshalErr != nil {
				answers <- observed{tool: tool, err: marshalErr}
				return
			}
			response := s.MCPServer().HandleMessage(WithAuthorizedToolCall(requestCtx, name), frame)
			raw, marshalErr := json.Marshal(response)
			if marshalErr != nil {
				answers <- observed{tool: tool, err: marshalErr}
				return
			}
			var envelope struct {
				Result *mcplib.CallToolResult `json:"result"`
				Error  any                    `json:"error"`
			}
			marshalErr = json.Unmarshal(raw, &envelope)
			if envelope.Error != nil {
				marshalErr = &relationComponentProtocolError{value: envelope.Error}
			}
			answers <- observed{tool: tool, result: envelope.Result, err: marshalErr}
		})
	}
	workers.Wait()
	close(answers)
	// Independent actual bytes and AST positions are captured strictly AFTER
	// both public replies; this cannot manufacture catch-up before a probe.
	physical, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := sha256.Sum256(physical)
	t.Logf("post-burst physical SHA=%s bytes=%q", hex.EncodeToString(sum[:]), physical)
	require.Equal(t, after, string(physical))
	set := token.NewFileSet()
	parsed, err := parser.ParseFile(set, path, physical, 0)
	require.NoError(t, err)
	callLine := 0
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok {
			if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "Callee" {
				callLine = set.Position(call.Pos()).Line
			}
		}
		return true
	})
	if removeCall {
		require.Zero(t, callLine)
	} else {
		require.Equal(t, 5, callLine)
	}
	require.False(t, ack.IsError, resultText(ack))
	ackBody := extractTextResult(t, ack)
	require.Equal(t, "committed", ackBody["disk_status"])
	require.Equal(t, "local", ackBody["ack_mode"])
	require.Equal(t, "pending", ackBody["graph_status"])
	files := ackBody["files"].([]any)
	require.Equal(t, hex.EncodeToString(sum[:]), files[0].(map[string]any)["after_sha256"])
	require.Equal(t, 3, sql.GetNode(caller).EndLine, "held canonical publication remains original; this is not early promotion")
	entry := liveEntry(pendingFileProjections, path)
	require.NotNil(t, entry, "the actual local ACK must have a pending owner in this controlled fixture")
	require.Positive(t, entry.commitSeq)
	for answer := range answers {
		t.Run(answer.tool, func(t *testing.T) {
			require.NoError(t, answer.err)
			require.NotNil(t, answer.result)
			require.False(t, answer.result.IsError, resultText(answer.result))
			body := extractTextResult(t, answer.result)
			t.Logf("public %s response=%s", answer.tool, resultText(answer.result))
			var matches []map[string]any
			var edges []any
			if raw := body["edges"]; raw != nil {
				require.IsType(t, edges, raw)
				edges = raw.([]any)
			}
			require.EqualValues(t, len(edges), body["total_edges"], "null edges are valid only with an explicit zero count")
			for _, raw := range edges {
				edge := raw.(map[string]any)
				if edge["from"] == caller && edge["to"] == callee && edge["kind"] == "calls" {
					matches = append(matches, edge)
				}
			}
			if removeCall {
				require.Empty(t, matches, "a removed call cannot be carried from the old canonical rows")
				return
			}
			require.Len(t, matches, 1)
			require.EqualValues(t, callLine, matches[0]["line"])
			require.Equal(t, true, matches[0]["provisional"], "actual held publication requires current provisional carry, not old canonical rows")
		})
	}
}

type relationComponentProtocolError struct{ value any }

func (e *relationComponentProtocolError) Error() string {
	raw, _ := json.Marshal(e.value)
	return string(raw)
}
