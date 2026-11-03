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
)

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
			s, backend, base, path, original := searchReaderWaitFixture(t)
			peer := path
			if transition == "sibling_file_distinct_symbol" {
				peer = filepath.Join(filepath.Dir(path), "peer.go")
				ack := preselectionRegisteredCall(t, s, base, 12, "write_file", map[string]any{"path": peer, "content": "package probe\n\nfunc NeighborBefore() int { return 2 }\n", "format": "json"})
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
				current := preselectionRegisteredCall(t, s, base, 30, "get_symbol_source", map[string]any{"id": "main.go::Target", "context_lines": 0, "format": "json"})
				require.False(t, current.IsError, resultText(current))
				latest := liveEntry(pendingFileProjections, path)
				require.NotSame(t, original, latest)
				require.Zero(t, latest.commitSeq)
				require.Same(t, original.layer, latest.layer)
				require.True(t, latest.readVersionChecked)
			default:
				ack := preselectionRegisteredCall(t, s, base, 30, "edit_file", map[string]any{"path": peer, "old_string": "NeighborBefore", "new_string": "Neighbor", "format": "json"})
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
			require.ErrorIs(t, validateRequestProjections(context.WithoutCancel(owned.ctx)), projectionSuperseded, "a successful captured answer must not revive old shared facts")
		})
	}
}

func TestSearchCallerOwnedOldFactsCannotHideANewerOwnACK(t *testing.T) {
	s, _, base, path, _ := searchReaderWaitFixture(t)
	old, err := s.bindPendingSearch(base)
	require.NoError(t, err)
	ack := preselectionRegisteredCall(t, s, base, 30, "edit_file", map[string]any{"path": path, "old_string": "func Target()", "new_string": "func TargetLater()", "format": "json"})
	require.False(t, ack.IsError, resultText(ack))
	require.Equal(t, true, extractTextResult(t, ack)["readback_exact"])
	args := snapshotSearchArgs()
	args["query"] = "TargetLater"
	result := preselectionRegisteredCall(t, s, old, 40, "search", args)
	require.True(t, result.IsError, "a context captured before the newer own ACK cannot be reused as a fresh request: %s", resultText(result))
	require.Contains(t, resultText(result), "superseded")
	require.NotContains(t, resultText(result), fmt.Sprintf(`"id":%q`, "main.go::TargetLater"))
}
