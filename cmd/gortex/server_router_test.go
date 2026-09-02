package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

// executorTestServer builds a real Server (core/defer preset) with a
// one-file indexed repo, returning the server and the local executor.
func executorTestServer(t *testing.T) (*gortexmcp.Server, daemon.LocalExecutor) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

func main() {}
`), 0o644))

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	eng := query.NewEngine(g)
	srv := gortexmcp.NewServer(eng, g, idx, nil, zap.NewNop(), nil)
	return srv, newLocalToolExecutor(srv, zap.NewNop())
}

// TestLocalExecutor_MalformedJSONRejectedBeforePromotion pins reviewer
// concern #3: malformed federation JSON must 400 without promoting the
// tool or running its handler.
func TestLocalExecutor_MalformedJSONRejectedBeforePromotion(t *testing.T) {
	srv, exec := executorTestServer(t)
	handlerRan := false
	srv.MCPServer().AddTool(
		mcp.NewTool("probe_tool", mcp.WithDescription("test")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			handlerRan = true
			return mcp.NewToolResultText("ran"), nil
		},
	)

	out, status, err := exec(context.Background(), "probe_tool", []byte("{bad json"))
	require.NoError(t, err)
	assert.Equal(t, 400, status)
	assert.Contains(t, string(out), "invalid_json")
	assert.False(t, handlerRan, "malformed input must not run the handler")
}

// TestLocalExecutor_MalformedFlatArgsRejected covers the second parse
// branch: a body that is neither a nested {"arguments":...} object nor
// a flat JSON object is rejected too.
func TestLocalExecutor_MalformedFlatArgsRejected(t *testing.T) {
	srv, exec := executorTestServer(t)
	handlerRan := false
	srv.MCPServer().AddTool(
		mcp.NewTool("probe_tool", mcp.WithDescription("test")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			handlerRan = true
			return mcp.NewToolResultText("ran"), nil
		},
	)

	out, status, err := exec(context.Background(), "probe_tool", []byte(`[1,2,3]`))
	require.NoError(t, err)
	assert.Equal(t, 400, status)
	assert.Contains(t, string(out), "invalid_json")
	assert.False(t, handlerRan, "malformed input must not run the handler")
}

// TestLocalExecutor_ValidNestedArgsDispatches covers the happy path: a
// well-formed {"arguments": {...}} body reaches the tool handler.
func TestLocalExecutor_ValidNestedArgsDispatches(t *testing.T) {
	srv, exec := executorTestServer(t)
	srv.MCPServer().AddTool(
		mcp.NewTool("echo_args", mcp.WithDescription("test")),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			msg, _ := req.GetArguments()["message"].(string)
			return mcp.NewToolResultText("got:" + msg), nil
		},
	)

	out, status, err := exec(context.Background(), "echo_args", []byte(`{"arguments":{"message":"hi"}}`))
	require.NoError(t, err)
	assert.Equal(t, 200, status)
	assert.Contains(t, string(out), "got:hi")
}

// TestLocalExecutor_ValidFlatArgsDispatches covers the flat-args body
// shape the executor accepts alongside the nested envelope.
func TestLocalExecutor_ValidFlatArgsDispatches(t *testing.T) {
	srv, exec := executorTestServer(t)
	srv.MCPServer().AddTool(
		mcp.NewTool("echo_args", mcp.WithDescription("test")),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			msg, _ := req.GetArguments()["message"].(string)
			return mcp.NewToolResultText("flat:" + msg), nil
		},
	)

	out, status, err := exec(context.Background(), "echo_args", []byte(`{"message":"hi"}`))
	require.NoError(t, err)
	assert.Equal(t, 200, status)
	assert.Contains(t, string(out), "flat:hi")
}

// TestLocalExecutor_UnknownTool404 keeps the not-found contract for a
// name that is neither live nor deferred.
func TestLocalExecutor_UnknownTool404(t *testing.T) {
	_, exec := executorTestServer(t)
	out, status, err := exec(context.Background(), "no_such_tool", []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, 404, status)
	assert.Contains(t, string(out), "tool_not_found")
}

// TestLocalExecutor_ColdPromotionDispatchesDeferredTool pins reviewer
// concern #4: a cold call to a real deferred tool (not manually
// registered live, not the generic "unknown name" case) must promote
// it and dispatch, not 404.
func TestLocalExecutor_ColdPromotionDispatchesDeferredTool(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, exec := executorTestServer(t)
	require.Nil(t, srv.MCPServer().GetTool("find_clones"), "find_clones must start deferred, not live")

	out, status, err := exec(context.Background(), "find_clones", []byte(`{}`))
	require.NoError(t, err)
	assert.NotEqual(t, 404, status, "a deferred tool must promote and dispatch, not 404: %s", out)
	assert.NotNil(t, srv.MCPServer().GetTool("find_clones"), "find_clones must be live after a successful cold dispatch")
}

// TestLocalExecutor_ConcurrentColdCallsBothDispatch covers two
// concurrent cold callers racing to promote the same deferred tool
// through the router's local executor (as opposed to lazy_tools_test.go's
// TestPromote_ConcurrentCallersNeverFalse404, which exercises the
// registry in isolation) — reviewer concern #4's cold-promotion race.
func TestLocalExecutor_ConcurrentColdCallsBothDispatch(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, exec := executorTestServer(t)
	require.Nil(t, srv.MCPServer().GetTool("find_clones"))

	const n = 2
	statuses := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, status, err := exec(context.Background(), "find_clones", []byte(`{}`))
			statuses[i] = status
			errs[i] = err
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for concurrent cold calls — possible deadlock")
	}

	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		assert.NotEqual(t, 404, statuses[i], "concurrent cold caller %d must not observe a false 404", i)
	}
	assert.NotNil(t, srv.MCPServer().GetTool("find_clones"))
}

// TestLocalExecutor_HiddenSessionDeniedWithoutPromotion pins reviewer
// concern #1: a session whose effective surface hides a tool (the
// exact facade-v1/hide repro fixture from the review) must get 404
// without the call ever promoting the tool into the live registry —
// regardless of whether the tool was already live or still deferred.
func TestLocalExecutor_HiddenSessionDeniedWithoutPromotion(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, exec := executorTestServer(t)
	require.Nil(t, srv.MCPServer().GetTool("find_clones"))

	srv.NoteSessionToolPolicy("facade-session", "facade-v1", "hide")
	ctx := gortexmcp.WithSessionID(context.Background(), "facade-session")

	out, status, err := exec(ctx, "find_clones", []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, 404, status)
	assert.Contains(t, string(out), "tool_not_found")
	assert.Nil(t, srv.MCPServer().GetTool("find_clones"), "a session-hidden tool must never be promoted into the live registry")
}

// TestLocalExecutor_HiddenSessionDeniedEvenWhenAlreadyLive is the
// other half of reviewer concern #1: the pre-fix code only checked
// session policy on a registry miss (guarding promotion), never on an
// already-live tool. Promote it out-of-band first, then confirm a
// hidden session's call is still denied — but via the SAME structured
// tool_blocked_by_mode error every other blocked-by-preset call gets
// (checkToolGate, running inside every registered handler), not a
// bare 404. An executor-level pre-check that turned this into 404
// would be lying about tool existence and would throw away the
// error's recovery guidance — that was a real regression an earlier
// draft of this fix introduced and a later review caught.
func TestLocalExecutor_HiddenSessionDeniedEvenWhenAlreadyLive(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, exec := executorTestServer(t)
	require.True(t, srv.EnsureToolPromoted("find_clones"), "test setup: find_clones must promote cleanly before the policy is applied")
	require.NotNil(t, srv.MCPServer().GetTool("find_clones"), "test setup: find_clones must be live before the policy is applied")

	srv.NoteSessionToolPolicy("facade-session", "facade-v1", "hide")
	ctx := gortexmcp.WithSessionID(context.Background(), "facade-session")

	out, status, err := exec(ctx, "find_clones", []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, 200, status, "an already-live tool blocked by the session's active preset dispatches to its handler, which reports the block structurally — it is not a 404")
	assert.Contains(t, string(out), "tool_blocked_by_mode", "the structured error code must survive, not collapse into a bare not-found")
	assert.Contains(t, string(out), "find_clones")
}

// TestLocalExecutor_NullBodyRejected pins reviewer concern #2: a
// top-level JSON `null` body must 400, not silently dispatch with nil
// arguments — json.Unmarshal treats null as a no-op for both struct
// and map targets, so a naïve parse lets it through.
func TestLocalExecutor_NullBodyRejected(t *testing.T) {
	srv, exec := executorTestServer(t)
	handlerRan := false
	srv.MCPServer().AddTool(
		mcp.NewTool("probe_tool", mcp.WithDescription("test")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			handlerRan = true
			return mcp.NewToolResultText("ran"), nil
		},
	)

	out, status, err := exec(context.Background(), "probe_tool", []byte("null"))
	require.NoError(t, err)
	assert.Equal(t, 400, status)
	assert.Contains(t, string(out), "invalid_json")
	assert.False(t, handlerRan, "a JSON-null body must not run the handler")
}

// TestLocalExecutor_ErrorResponseUsesIsErrorTag pins the fix for a
// second review round's finding #4: the local executor's response
// must serialize the error flag as "isError" (matching the standard
// ToolResponse / Streamable HTTP wrapToolResultAsJSONRPC contract),
// not "is_error". A prior version of the response struct used the
// wrong tag, so wrapToolResultAsJSONRPC's `json:"isError"` field
// never matched, silently defaulting IsError to false — a genuine
// tool error routed through the Streamable HTTP transport's
// local-fast path was reported to the client as success.
func TestLocalExecutor_ErrorResponseUsesIsErrorTag(t *testing.T) {
	srv, exec := executorTestServer(t)
	srv.MCPServer().AddTool(
		mcp.NewTool("boom_tool", mcp.WithDescription("test")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultError("boom"), nil
		},
	)

	out, status, err := exec(context.Background(), "boom_tool", []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, 200, status)
	assert.Contains(t, string(out), `"isError":true`, "must use the standard isError tag")
	assert.NotContains(t, string(out), "is_error", "must not use the old snake_case tag")

	var decoded struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	require.NoError(t, json.Unmarshal(out, &decoded))
	assert.True(t, decoded.IsError, "isError must round-trip through the standard tag")
	require.Len(t, decoded.Content, 1)
	assert.Equal(t, "boom", decoded.Content[0].Text)
}

// TestLocalExecutor_NestedArgumentsNullRejected covers the second null
// form from reviewer concern #2: `{"arguments": null}` must also 400.
func TestLocalExecutor_NestedArgumentsNullRejected(t *testing.T) {
	srv, exec := executorTestServer(t)
	handlerRan := false
	srv.MCPServer().AddTool(
		mcp.NewTool("probe_tool", mcp.WithDescription("test")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			handlerRan = true
			return mcp.NewToolResultText("ran"), nil
		},
	)

	out, status, err := exec(context.Background(), "probe_tool", []byte(`{"arguments": null}`))
	require.NoError(t, err)
	assert.Equal(t, 400, status)
	assert.Contains(t, string(out), "invalid_json")
	assert.False(t, handlerRan, `{"arguments": null} must not run the handler`)
}
