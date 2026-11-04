package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// Exercise the registered legacy tools themselves. delete_file is a supported
// batch_edit operation, not a separately registered tool name.
func mutationSearchFixture(t *testing.T) (*Server, *store_sqlite.Store, context.Context, string) {
	t.Helper()
	selectedProjectionRegistry(t)
	t.Setenv("GORTEX_TOOLS", "full")
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("package sample\n\nfunc MutationOriginal() {}\n\nfunc UnchangedNeighbor() {}\n"), 0o600))
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "mutations.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := indexer.New(store, testRegistry(), config.Default().Index, zap.NewNop())
	t.Cleanup(idx.Close)
	_, err = idx.Index(root)
	require.NoError(t, err)
	backend := search.NewSymbolSearcherBackend(store)
	require.NotEmpty(t, backend.Search("MutationOriginal", 10))
	for range 4 {
		require.Len(t, idx.GrepText("MutationOriginal", 10), 1)
	}
	engine := query.NewEngine(store)
	engine.SetSearch(backend)
	s := NewServer(engine, store, idx, nil, zap.NewNop(), nil, MultiRepoOptions{EditAckMode: config.EditAckLocal})
	pending := make(chan indexer.MutationResult)
	t.Cleanup(func() { close(pending) })
	s.SetWatcher(mutationTestWatcher{done: pending})
	const session = "mutation-search-acceptance"
	require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(session) })
	ctx := s.WithClientSession(WithSessionID(t.Context(), session), session)
	s.NoteSessionToolPolicy(session, "full", "defer")
	require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"mutation-search-test","version":"1"}}}`)))
	s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	return s, store, ctx, path
}

func mutationSearchAnswer(t *testing.T, s *Server, ctx context.Context, tool, name string) *mcplib.CallToolResult {
	t.Helper()
	args := map[string]any{"query": name, "format": "json", "limit": 10000, "max_bytes": 0}
	if tool == "search_symbols" {
		args["query"] = "name:" + name
		args["query_class"] = "symbol"
		args["expand"] = "off"
		args["max_per_file"] = 0
	}
	answer := snapshotRegisteredCall(t, s, ctx, 20, tool, args)
	require.False(t, answer.IsError, resultText(answer))
	if answer.StructuredContent != nil {
		wire, err := json.Marshal(answer.StructuredContent)
		require.NoError(t, err)
		var structured map[string]any
		require.NoError(t, json.Unmarshal(wire, &structured))
		require.Equal(t, extractTextResult(t, answer), structured, "declared search data channels disagree")
	}
	return answer
}

func requireMutationSearchName(t *testing.T, s *Server, ctx context.Context, name string, present bool) {
	t.Helper()
	symbols := extractTextResult(t, mutationSearchAnswer(t, s, ctx, "search_symbols", name))
	raw, exists := symbols["results"]
	require.True(t, exists, "missing results cannot prove absence")
	var rows []any
	if raw != nil {
		var ok bool
		rows, ok = raw.([]any)
		require.True(t, ok)
	}
	require.Equal(t, false, symbols["truncated"], "incomplete pages cannot prove old-name absence")
	require.Empty(t, symbols["next_cursor"])
	require.NotEqual(t, true, symbols["_truncated_by_budget"])
	require.NotEqual(t, true, symbols["_truncated_by_limit"])
	require.Equal(t, float64(len(rows)), symbols["total"])
	matches := 0
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		require.True(t, ok)
		if row["id"] == "main.go::"+name || row["name"] == name {
			require.Equal(t, "main.go::"+name, row["id"])
			require.Equal(t, name, row["name"])
			require.Equal(t, "main.go", row["file_path"])
			require.Equal(t, float64(3), row["start_line"])
			matches++
		}
	}
	if present {
		require.Equal(t, 1, matches)
	} else {
		require.Empty(t, rows)
		require.Zero(t, matches)
	}
	text := extractTextResult(t, mutationSearchAnswer(t, s, ctx, "search_text", name))
	raw, exists = text["matches"]
	require.True(t, exists, "missing text matches cannot prove absence")
	rows = nil
	if raw != nil {
		var ok bool
		rows, ok = raw.([]any)
		require.True(t, ok)
	}
	if present {
		require.Len(t, rows, 1)
		row, ok := rows[0].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "main.go", row["path"])
		require.Equal(t, float64(3), row["line"])
		require.Contains(t, row["text"], "func "+name+"()")
	} else {
		require.Empty(t, rows)
	}
	require.Equal(t, float64(len(rows)), text["count"])
	require.NotEqual(t, true, text["_truncated_by_limit"])
	require.NotEqual(t, true, text["_truncated_by_budget"])
	require.NotEqual(t, false, text["count_is_exact"])
}

func TestRegisteredDeleteAndRenameRemoveOldSearchIdentity(t *testing.T) {
	for _, operation := range []string{"delete_file", "rename_symbol"} {
		t.Run(operation, func(t *testing.T) {
			s, store, ctx, path := mutationSearchFixture(t)
			requireMutationSearchName(t, s, ctx, "MutationOriginal", true)
			var ack *mcplib.CallToolResult
			if operation == "delete_file" {
				ack = snapshotRegisteredCall(t, s, ctx, 10, "batch_edit", map[string]any{"edits": []any{map[string]any{"op": "delete_file", "path": path}}, "format": "json"})
			} else {
				ack = snapshotRegisteredCall(t, s, ctx, 10, "rename_symbol", map[string]any{"id": "main.go::MutationOriginal", "new_name": "MutationRenamed", "format": "json"})
			}
			require.False(t, ack.IsError, resultText(ack))
			body := extractTextResult(t, ack)
			t.Logf("operation=%s actual_ack=%s", operation, resultText(ack))
			if operation == "delete_file" {
				require.Equal(t, "committed", body["disk_status"])
				require.Equal(t, "local", body["ack_mode"])
				_, err := os.Stat(path)
				require.ErrorIs(t, err, os.ErrNotExist)
			} else {
				require.Equal(t, "applied", body["status"])
				files, ok := body["files"].([]any)
				require.True(t, ok)
				require.Len(t, files, 1)
				file, ok := files[0].(map[string]any)
				require.True(t, ok)
				require.Equal(t, "local", file["ack_mode"])
				require.Equal(t, "committed", file["disk_status"])
				actual, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Contains(t, string(actual), "func MutationRenamed()")
				require.NotContains(t, string(actual), "MutationOriginal")
			}
			// No publication was allowed to repair either native postings or the old
			// graph identity before this same-session search.
			require.NotNil(t, store.GetNode("main.go::MutationOriginal"))
			requireMutationSearchName(t, s, ctx, "MutationOriginal", false)
			if operation == "rename_symbol" {
				requireMutationSearchName(t, s, ctx, "MutationRenamed", true)
			}
		})
	}
}

func TestRegisteredEmptyProjectionSearchPreservesDefaultBytes(t *testing.T) {
	s, _, ctx, _ := mutationSearchFixture(t)
	require.Empty(t, pendingFileProjections.snap.Load().entries)
	bound, err := s.bindPendingSearch(ctx)
	require.NoError(t, err)
	require.Same(t, ctx, bound)
	require.Same(t, s.engine, s.engineFor(bound))
	files, err := s.pendingTextFiles(ctx, ResolvedScope{}, nil)
	require.NoError(t, err)
	require.Empty(t, files)
	for _, tool := range []string{"search_symbols", "search_text"} {
		t.Run(tool, func(t *testing.T) {
			// The existing default middleware path omits overlay injection. Compare
			// the complete serialized result, with no field stripping, against the
			// registered call whose projection selection above was actually empty.
			leaf, ok := s.facades.legacy(tool)
			require.True(t, ok)
			args := map[string]any{"query": "MutationOriginal", "format": "json", "limit": 100, "max_bytes": 0}
			if tool == "search_symbols" {
				args["query_class"] = "symbol"
				args["expand"] = "off"
			}
			req := mcplib.CallToolRequest{}
			req.Params.Name, req.Params.Arguments = tool, args
			baseline, err := s.wrapToolHandlerMode(leaf.handler, false)(WithAuthorizedToolCall(ctx, tool), req)
			require.NoError(t, err)
			require.False(t, baseline.IsError, resultText(baseline))
			actual := snapshotRegisteredCall(t, s, ctx, 30, tool, args)
			require.False(t, actual.IsError, resultText(actual))
			before, err := json.Marshal(baseline)
			require.NoError(t, err)
			after, err := json.Marshal(actual)
			require.NoError(t, err)
			require.Equal(t, string(before), string(after), "empty pending selection changed the serialized default-path result")
		})
	}
}
