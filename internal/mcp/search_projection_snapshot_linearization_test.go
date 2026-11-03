package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/tokens"
	"go.uber.org/zap"
)

func snapshotRegisteredCall(t *testing.T, s *Server, ctx context.Context, id int, name string, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	result, err := externalVersionRegisteredCall(s, ctx, id, name, args)
	require.NoError(t, err)
	require.NotNil(t, result)
	return result
}

func snapshotEditArgs(path, old, replacement string) map[string]any {
	return map[string]any{"operation": "file", "target": map[string]any{"file": path}, "old_string": old, "new_string": replacement, "output": map[string]any{"format": "json"}}
}

// The retained first Linux wire call is search/symbols on facade-v1/hide,
// negotiated before initialize. Keep that profile for every ACK and read in
// this connection, instead of borrowing a legacy all/defer fixture.
func snapshotFacadeFixture(t *testing.T) (*Server, *searchMetadataPoolStore, context.Context, string, *projectionEntry) {
	t.Helper()
	selectedProjectionRegistry(t)
	require.True(t, tokens.EncoderReady())
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("package probe\n\nfunc TargetBefore() int { return 1 }\n\nfunc NeighborBefore() int { return 2 }\n"), 0o600))
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "snapshot-search.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sql.Close()) })
	backend := &searchMetadataPoolStore{Store: sql}
	idx := newProjectionTestIndexer(t, backend)
	_, err = idx.Index(root)
	require.NoError(t, err)
	s := NewServer(query.NewEngine(backend), backend, idx, nil, zap.NewNop(), nil)
	s.editAckMode = config.EditAckLocal
	pending := make(chan indexer.MutationResult)
	s.SetWatcher(mutationTestWatcher{done: pending})
	t.Cleanup(func() { close(pending) })
	const session = "snapshot-search-facade"
	require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(session) })
	ctx := s.WithClientSession(WithSessionID(t.Context(), session), session)
	s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
	require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"gortex-load","version":"1"}}}`)))
	s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	ack := snapshotRegisteredCall(t, s, ctx, 10, "edit", snapshotEditArgs(path, "TargetBefore", "Target"))
	require.False(t, ack.IsError, resultText(ack))
	fields := extractTextResult(t, ack)
	require.Equal(t, "local", fields["ack_mode"])
	require.Equal(t, true, fields["readback_exact"])
	entry := liveEntry(pendingFileProjections, path)
	require.NotNil(t, entry)
	require.Positive(t, entry.commitSeq)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(raw)), fields["content_sha256"])
	require.Nil(t, sql.GetNode("main.go::Target"), "canonical publication must remain held")
	return s, backend, ctx, path, entry
}

func snapshotSearchArgs() map[string]any {
	return map[string]any{"operation": "symbols", "query": "Target", "options": map[string]any{"limit": 10000, "max_bytes": 0, "query_class": "symbol", "expand": "off", "max_per_file": 0}, "output": map[string]any{"format": "json", "max_bytes": 0}}
}

func snapshotTargetRows(t *testing.T, result *mcplib.CallToolResult) []any {
	t.Helper()
	require.NotNil(t, result)
	require.False(t, result.IsError, resultText(result))
	rows, ok := extractTextResult(t, result)["results"].([]any)
	require.True(t, ok)
	found := 0
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		require.True(t, ok)
		require.NotEqual(t, "main.go::TargetBefore", row["id"], "canonical rows for the projected file must stay masked")
		if row["id"] == "main.go::Target" {
			found++
			require.Equal(t, "main.go", row["file_path"])
			require.Equal(t, float64(3), row["start_line"])
		}
	}
	require.Equal(t, 1, found, "the acknowledged distinct symbol must be present exactly once")
	return rows
}

func snapshotPhysicalTarget(t *testing.T, path string) [32]byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, path, raw, 0)
	require.NoError(t, err)
	found := 0
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "Target" {
			found++
			require.Equal(t, 3, positions.Position(fn.Pos()).Line)
		}
	}
	require.Equal(t, 1, found, "actual post-peer physical bytes still contain the queried distinct symbol")
	return sha256.Sum256(raw)
}

// #1015 requires one immutable pending snapshot per request. This test first
// proves a real authorized selection after its own ACK, then makes an actual
// independent publication while that already assembled response is held.
// It does not assert historical membership in the retained Linux failure.
func TestRegisteredSearchCompletesItsCapturedAuthorizedVersion(t *testing.T) {
	for _, transition := range []string{"same_file_distinct_symbol", "sibling_file_distinct_symbol", "checked_same_bytes_COW", "caller_cancellation"} {
		t.Run(transition, func(t *testing.T) {
			s, backend, base, path, original := snapshotFacadeFixture(t)
			peer := path
			if transition == "sibling_file_distinct_symbol" {
				peer = filepath.Join(filepath.Dir(path), "peer.go")
				ack := snapshotRegisteredCall(t, s, base, 12, "edit", map[string]any{"operation": "write", "target": map[string]any{"file": peer}, "content": "package probe\n\nfunc NeighborBefore() int { return 2 }\n", "output": map[string]any{"format": "json"}})
				require.False(t, ack.IsError, resultText(ack))
				require.Equal(t, true, extractTextResult(t, ack)["readback_exact"])
			}
			caller, cancel := context.WithCancel(base)
			defer cancel()
			captured, ok := s.facades.legacy("search_symbols")
			require.True(t, ok)
			type selection struct {
				ctx    context.Context
				result *mcplib.CallToolResult
				err    error
			}
			selected := make(chan selection, 1)
			released := make(chan struct{})
			release := sync.OnceFunc(func() { close(released) })
			leafDone := make(chan struct{})
			s.facades.capture(captured.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				defer close(leafDone)
				result, err := captured.handler(ctx, req)
				selected <- selection{ctx: ctx, result: result, err: err}
				<-released
				return result, err
			})
			type answer struct {
				result *mcplib.CallToolResult
				err    error
			}
			answered := make(chan answer, 1)
			var worker sync.WaitGroup
			worker.Add(1)
			t.Cleanup(func() { release(); worker.Wait() })
			go func() {
				defer worker.Done()
				result, err := externalVersionRegisteredCall(s, caller, 20, "search", snapshotSearchArgs())
				answered <- answer{result: result, err: err}
			}()
			var owned selection
			select {
			case owned = <-selected:
			case early := <-answered:
				raw, marshalErr := json.Marshal(early.result)
				t.Fatalf("registered search returned before its capture barrier: result_json=%s marshal_error=%v protocol_error=%v", raw, marshalErr, early.err)
			case <-time.After(3 * time.Second):
				t.Fatal("actual registered search did not reach its completed leaf")
			}
			leafJSON, leafJSONErr := json.Marshal(owned.result)
			t.Logf("captured_leaf_json=%s marshal_error=%v leaf_error=%v", leafJSON, leafJSONErr, owned.err)
			require.NoError(t, leafJSONErr)
			require.NoError(t, owned.err)
			rows := snapshotTargetRows(t, owned.result)
			require.NoError(t, validateRequestProjections(owned.ctx), "the actual selected file owners were current at bind/leaf")
			before := snapshotPhysicalTarget(t, path)
			facts := owned.ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
			facts.mu.RLock()
			borrowedCount := len(facts.entries)
			facts.mu.RUnlock()
			if transition == "sibling_file_distinct_symbol" {
				require.Equal(t, 2, borrowedCount, "prove the query also borrowed the sibling's pending version")
			}
			switch transition {
			case "caller_cancellation":
				cancel()
			case "checked_same_bytes_COW":
				pendingFileProjections.markSuspect(path)
				current := snapshotRegisteredCall(t, s, base, 30, "read", map[string]any{"operation": "source", "target": map[string]any{"symbol": "main.go::Target"}, "context": map[string]any{"context_lines": 0}, "output": map[string]any{"format": "json"}})
				require.False(t, current.IsError, resultText(current))
				latest := liveEntry(pendingFileProjections, path)
				require.NotSame(t, original, latest)
				require.Zero(t, latest.commitSeq)
				require.Same(t, original.layer, latest.layer)
				require.True(t, latest.readVersionChecked)
			default:
				ack := snapshotRegisteredCall(t, s, base, 30, "edit", snapshotEditArgs(peer, "NeighborBefore", "Neighbor"))
				require.False(t, ack.IsError, resultText(ack))
				fields := extractTextResult(t, ack)
				require.Equal(t, true, fields["readback_exact"])
				t.Logf("actual peer ACK: %v", fields)
			}
			after := snapshotPhysicalTarget(t, path)
			if transition == "same_file_distinct_symbol" {
				require.NotEqual(t, before, after, "the peer physically changed this file, preserving Target")
			} else {
				require.Equal(t, before, after, "the query target's physical bytes remain unchanged")
			}
			if transition != "caller_cancellation" {
				require.ErrorIs(t, validateRequestProjections(owned.ctx), projectionSuperseded, "shared old facts remain invalid; snapshot semantics must be search-specific")
			}
			require.Nil(t, backend.Store.GetNode("main.go::Target"), "the response must use its selected layer, not newly published canonical rows")
			release()
			worker.Wait()
			<-leafDone
			response := <-answered
			finalJSON, finalJSONErr := json.Marshal(response.result)
			t.Logf("transition=%s borrowed=%d before_sha=%x after_sha=%x captured_leaf_json=%s final_json=%s marshal_error=%v protocol_error=%v", transition, borrowedCount, before, after, leafJSON, finalJSON, finalJSONErr, response.err)
			require.NoError(t, finalJSONErr)
			if transition == "caller_cancellation" {
				require.ErrorIs(t, caller.Err(), context.Canceled)
				if response.result != nil {
					require.True(t, response.result.IsError)
				} else {
					require.Error(t, response.err)
				}
				return
			}
			require.NoError(t, response.err)
			require.Equal(t, rows, snapshotTargetRows(t, response.result), "the answer must retain its one authorized immutable selection")
			require.Equal(t, extractTextResult(t, owned.result), extractTextResult(t, response.result), "capture does not fabricate latest/current freshness or alter the selected answer")
			require.NotNil(t, response.result.Meta)
			for key, value := range owned.result.Meta.AdditionalFields {
				require.Equal(t, value, response.result.Meta.AdditionalFields[key], "selected scope metadata must survive: %s", key)
			}
			require.Equal(t, map[string]any{"canonical_tool": "search_symbols", "facade": "search", "operation": "symbols", "surface_version": "facade-v1"}, response.result.Meta.AdditionalFields["gortex_facade"])
			require.ErrorIs(t, validateRequestProjections(context.WithoutCancel(owned.ctx)), projectionSuperseded, "a successful captured answer must not revive old shared facts")
		})
	}
}

func TestSearchCallerOwnedOldFactsCannotHideANewerOwnACK(t *testing.T) {
	s, _, base, path, _ := snapshotFacadeFixture(t)
	leaf, ok := s.facades.legacy("search_symbols")
	require.True(t, ok)
	selected := make(chan context.Context, 2)
	s.facades.capture(leaf.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		result, err := leaf.handler(ctx, req)
		selected <- ctx
		return result, err
	})
	first := snapshotRegisteredCall(t, s, base, 20, "search", snapshotSearchArgs())
	snapshotTargetRows(t, first)
	old := context.WithoutCancel(<-selected) // Remove normal SDK-return cancellation only, retaining the actual sealed slot and old facts.
	capture, _ := old.Value(pendingSymbolCaptureKey{}).(*pendingSymbolCapture)
	require.NotNil(t, capture)
	require.NotEmpty(t, capture.entries, "the first real dispatch must have sealed a capture")
	ack := snapshotRegisteredCall(t, s, base, 30, "edit", snapshotEditArgs(path, "func Target()", "func TargetLater()"))
	require.False(t, ack.IsError, resultText(ack))
	require.Equal(t, true, extractTextResult(t, ack)["readback_exact"])
	args := snapshotSearchArgs()
	args["query"] = "TargetLater"
	result := snapshotRegisteredCall(t, s, old, 40, "search", args)
	require.True(t, result.IsError, "a context captured before the newer own ACK cannot be reused as a fresh request: %s", resultText(result))
	require.Contains(t, resultText(result), "superseded")
	require.NotContains(t, resultText(result), fmt.Sprintf(`"id":%q`, "main.go::TargetLater"))
	text := snapshotRegisteredCall(t, s, old, 41, "search", map[string]any{"operation": "text", "query": "TargetLater", "output": map[string]any{"format": "json", "max_bytes": 0}})
	require.True(t, text.IsError, "a symbol capture cannot exempt a later text scan: %s", resultText(text))
	require.Contains(t, resultText(text), "superseded")
}
