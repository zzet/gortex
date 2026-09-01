package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// #597: tool schemas promise additionalProperties:false but dispatch read
// only the keys it knew — an unknown option (line_range on read_file)
// vanished silently and the caller paid the full un-windowed response with
// no signal to self-correct. The guard warns by default: the handler still
// runs and an _ignored_options rider names the unknown keys and the valid
// ones. GORTEX_TOOL_ARG_GUARD=reject upgrades that to a refusal BEFORE the
// handler runs.

func guardFixture(t *testing.T) (mcp.Tool, *int) {
	t.Helper()
	tool := mcp.NewTool("probe_tool",
		mcp.WithString("offset", mcp.Description("window start")),
		mcp.WithNumber("limit", mcp.Description("window size")),
	)
	// NewTool leaves additionalProperties unset; prepareTool closes it.
	// The guard-unit fixture models the post-registration state.
	tool.InputSchema.AdditionalProperties = false
	calls := 0
	return tool, &calls
}

func guardHandler(calls *int) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		*calls++
		return mcp.NewToolResultText("ok"), nil
	}
}

func callGuarded(t *testing.T, tool mcp.Tool, calls *int, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	wrapped := wrapToolArgGuard(tool, guardHandler(calls))
	req := mcp.CallToolRequest{}
	req.Params.Name = tool.Name
	req.Params.Arguments = args
	res, err := wrapped(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	return res
}

func guardResultText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// The default is warn: first-party surfaces inject undeclared keys into
// arbitrary tools (the CLI pins format into every legacy frame, the HTTP
// bridge merges ?format=), so rejecting by default breaks callers that work
// today. The handler still runs; the rider gives the agent its signal to
// self-correct.
func TestToolArgGuard_DefaultWarnsAndExecutes(t *testing.T) {
	tool, calls := guardFixture(t)
	res := callGuarded(t, tool, calls, map[string]any{
		"offset":     "10",
		"line_range": []any{120, 160},
	})
	assert.False(t, res.IsError, "the default must not refuse the call")
	assert.Equal(t, 1, *calls, "the handler runs under the warn default")
	text := guardResultText(res)
	assert.Contains(t, text, "line_range", "the rider names the unknown key")
	assert.Contains(t, text, "offset", "the rider lists the valid keys")
	assert.Contains(t, text, "limit", "the rider lists the valid keys")
}

func TestToolArgGuard_RejectOptInRefusesBeforeHandler(t *testing.T) {
	t.Setenv(toolArgGuardEnv, "reject")
	tool, calls := guardFixture(t)
	res := callGuarded(t, tool, calls, map[string]any{
		"offset":     "10",
		"line_range": []any{120, 160},
	})
	assert.True(t, res.IsError, "reject mode refuses the call")
	assert.Zero(t, *calls, "the handler must never run — the expensive wrong answer is never produced")
	text := guardResultText(res)
	assert.Contains(t, text, "line_range", "the error names the unknown key")
	assert.Contains(t, text, "offset", "the error lists the valid keys")
	assert.Contains(t, text, "limit", "the error lists the valid keys")
}

// Response-shaping keys are injected by first-party surfaces into ANY tool
// call and honored by generic layers dispatch shares (the CLI's format pin,
// the HTTP bridge's ?format= merge, effectiveBudget's max_bytes / max_tokens,
// applyFieldsFilter's fields). They are declared-everywhere by decree: no
// warn rider, and even reject mode passes them through.
func TestToolArgGuard_ResponseShapingKeysExempt(t *testing.T) {
	t.Setenv(toolArgGuardEnv, "reject")
	tool, calls := guardFixture(t)
	res := callGuarded(t, tool, calls, map[string]any{
		"offset":     "10",
		"format":     "json",
		"fields":     "a,b",
		"max_bytes":  1024,
		"max_tokens": 200,
		"cursor":     "abc",
	})
	assert.False(t, res.IsError, "shaping keys must survive even reject mode: %s", guardResultText(res))
	assert.Equal(t, 1, *calls)
	assert.NotContains(t, guardResultText(res), "_ignored_options", "no rider for keys dispatch honors")
}

// The rider is a nudge on a SUCCESSFUL result. A result that is already an
// error keeps its error text clean — appending "you also passed an unknown
// option" to a failure helps nobody and pollutes error matching.
func TestToolArgGuard_WarnRiderSkipsErrorResults(t *testing.T) {
	t.Setenv(toolArgGuardEnv, "warn")
	tool := mcp.NewTool("probe_tool", mcp.WithString("offset", mcp.Description("window start")))
	tool.InputSchema.AdditionalProperties = false
	wrapped := wrapToolArgGuard(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultError("boom"), nil
	})
	req := mcp.CallToolRequest{}
	req.Params.Name = tool.Name
	req.Params.Arguments = map[string]any{"line_range": []any{1, 2}}
	res, err := wrapped(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.NotContains(t, guardResultText(res), "_ignored_options",
		"an error result must not gain the rider")
}

// Structured-content readers never see Content, so a rider that lives only
// there is invisible to them. When the result carries a structured map the
// rider is mirrored in under _ignored_options.
func TestToolArgGuard_WarnRiderMirrorsIntoStructuredContent(t *testing.T) {
	tool := mcp.NewTool("probe_tool", mcp.WithString("offset", mcp.Description("window start")))
	tool.InputSchema.AdditionalProperties = false
	wrapped := wrapToolArgGuard(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res := mcp.NewToolResultText("ok")
		res.StructuredContent = map[string]any{"answer": 42}
		return res, nil
	})
	req := mcp.CallToolRequest{}
	req.Params.Name = tool.Name
	req.Params.Arguments = map[string]any{"line_range": []any{1, 2}}
	res, err := wrapped(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	sc, ok := res.StructuredContent.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, 42, sc["answer"], "existing structured payload is untouched")
	assert.Contains(t, sc, "_ignored_options", "the rider must be visible to structured readers")
	assert.Contains(t, fmt.Sprint(sc["_ignored_options"]), "line_range")
}

func TestToolArgGuard_ValidKeysPassThrough(t *testing.T) {
	tool, calls := guardFixture(t)
	res := callGuarded(t, tool, calls, map[string]any{"offset": "10", "limit": 40})
	assert.False(t, res.IsError)
	assert.Equal(t, 1, *calls)
}

func TestToolArgGuard_NilArgumentsPassThrough(t *testing.T) {
	tool, calls := guardFixture(t)
	res := callGuarded(t, tool, calls, nil)
	assert.False(t, res.IsError)
	assert.Equal(t, 1, *calls)
}

// A tool that explicitly opens its top-level schema opts out of the guard —
// the contract is whatever the schema says, in both directions.
func TestToolArgGuard_ExplicitOpenSchemaUnenforced(t *testing.T) {
	tool, calls := guardFixture(t)
	tool.InputSchema.AdditionalProperties = true
	res := callGuarded(t, tool, calls, map[string]any{"anything": 1})
	assert.False(t, res.IsError)
	assert.Equal(t, 1, *calls)
}

// Raw-schema tools are outside the guard: no shipped tool uses one (the
// #597 stamp closes only structured schemas), so parsing raw JSON at
// registration would be dead surface. Authored closed or open, the call
// passes through untouched.
func TestToolArgGuard_RawSchemasNotGuarded(t *testing.T) {
	t.Setenv(toolArgGuardEnv, "reject")
	closed := mcp.NewToolWithRawSchema("closed_tool", "d", json.RawMessage(
		`{"type":"object","properties":{"operation":{"type":"string"}},"additionalProperties":false}`))

	calls := 0
	res := callGuarded(t, closed, &calls, map[string]any{"operaton": "file"})
	assert.False(t, res.IsError, "raw schemas are not enforced by this guard")
	assert.Equal(t, 1, calls)
	assert.NotContains(t, guardResultText(res), "_ignored_options")
}

func TestToolArgGuard_WarnModeCallsHandlerWithRider(t *testing.T) {
	t.Setenv(toolArgGuardEnv, "warn")
	tool, calls := guardFixture(t)
	res := callGuarded(t, tool, calls, map[string]any{"line_range": []any{1, 2}})
	assert.False(t, res.IsError)
	assert.Equal(t, 1, *calls, "warn mode still runs the handler")
	assert.Contains(t, guardResultText(res), "line_range", "the rider names what was ignored")
}

// The off vocabulary matches the repo's boolean-env idiom (parse_gate.go):
// 0 / false / off / no.
func TestToolArgGuard_OffModeDisables(t *testing.T) {
	for _, mode := range []string{"off", "0", "false", "no"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(toolArgGuardEnv, mode)
			tool, calls := guardFixture(t)
			res := callGuarded(t, tool, calls, map[string]any{"line_range": []any{1, 2}})
			assert.False(t, res.IsError)
			assert.Equal(t, 1, *calls)
			assert.NotContains(t, guardResultText(res), "line_range")
		})
	}
}

// prepareTool closes any structured schema that never took a position, so
// the published contract says what dispatch now enforces. Raw schemas are
// left exactly as authored.
func TestPrepareToolStampsClosedSchema(t *testing.T) {
	srv, _ := setupTestServer(t)
	tool := mcp.NewTool("stamp_probe_tool", mcp.WithString("offset", mcp.Description("window start")))
	require.Nil(t, tool.InputSchema.AdditionalProperties)
	srv.prepareTool(&tool, guardHandler(new(int)))

	out, err := json.Marshal(tool)
	require.NoError(t, err)
	var m struct {
		InputSchema struct {
			AdditionalProperties any `json:"additionalProperties"`
		} `json:"inputSchema"`
	}
	require.NoError(t, json.Unmarshal(out, &m))
	assert.Equal(t, false, m.InputSchema.AdditionalProperties,
		"an unset structured schema is published closed, matching enforcement")
}

// guardE2ECall drives one tools/call frame through the real MCP dispatch
// path (initialize + HandleMessage), returning the decoded result.
func guardE2ECall(t *testing.T, srv *Server, ctx context.Context, id int, name string, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": arguments},
	})
	require.NoError(t, err)
	reply := srv.MCPServer().HandleMessage(ctx, frame)
	require.NotNil(t, reply)
	raw, err := json.Marshal(reply)
	require.NoError(t, err)
	var envelope struct {
		Error  any                 `json:"error"`
		Result *mcp.CallToolResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &envelope))
	require.Nil(t, envelope.Error, "protocol error: %v", envelope.Error)
	require.NotNil(t, envelope.Result)
	return envelope.Result
}

func guardE2ESession(t *testing.T, srv *Server, session string) context.Context {
	t.Helper()
	ctx := WithSessionID(context.Background(), session)
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"integration-harness","version":"1.0"}}}`)
	require.NotNil(t, srv.MCPServer().HandleMessage(ctx, initFrame))
	return ctx
}

// The issue's measured shape, end to end through real MCP frames on the
// full tool surface: under the reject opt-in the exact
// read_file(line_range: ...) call that used to return the whole file
// silently refuses before the read; the corrected call still works. Under
// the warn default the same call executes and carries the rider instead.
func TestReadFileUnknownWindowOptionE2E(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "full") // legacy names are session-gated off the default surface
	srv, _ := setupTestServer(t)
	ctx := guardE2ESession(t, srv, "arg_guard_e2e")

	t.Setenv(toolArgGuardEnv, "reject")
	res := guardE2ECall(t, srv, ctx, 2, "read_file", map[string]any{"path": "main.go", "line_range": []any{120, 160}})
	require.True(t, res.IsError, "line_range must refuse under reject, not return the full file: %s", guardResultText(res))
	require.Contains(t, guardResultText(res), "line_range")
	require.NotContains(t, guardResultText(res), "func helper", "no file content may ride an arg-guard refusal")

	res = guardE2ECall(t, srv, ctx, 3, "read_file", map[string]any{"path": "main.go"})
	require.False(t, res.IsError, guardResultText(res))

	t.Setenv(toolArgGuardEnv, "")
	res = guardE2ECall(t, srv, ctx, 4, "read_file", map[string]any{"path": "main.go", "line_range": []any{120, 160}})
	require.False(t, res.IsError, "the warn default executes the call: %s", guardResultText(res))
	require.Contains(t, guardResultText(res), "_ignored_options", "the rider gives the self-correct signal")
	require.Contains(t, guardResultText(res), "line_range")
}

// The CLI's legacy-surface frames pin format:"json" into every tool call
// (buildToolCallFrameWithDefault), and the HTTP bridge merges ?format= the
// same way. Those frames must keep working against the guarded dispatch
// path: no refusal, no rider — format is a response-shaping key generic
// layers honor.
func TestCLIShapedFramesSurviveGuardE2E(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "full")
	srv, _ := setupTestServer(t)
	ctx := guardE2ESession(t, srv, "arg_guard_cli_e2e")

	calls := []struct {
		name string
		args map[string]any
	}{
		{"audit_health", map[string]any{"format": "json"}},
		{"verify_change", map[string]any{"format": "json", "changes": `[{"symbol_id":"main.go::helper","new_signature":"func helper(n int)"}]`}},
		{"get_test_targets", map[string]any{"format": "json", "ids": "main.go::helper"}},
	}
	for i, c := range calls {
		res := guardE2ECall(t, srv, ctx, 10+i, c.name, c.args)
		require.False(t, res.IsError, "%s with the CLI's format pin must not error: %s", c.name, guardResultText(res))
		require.NotContains(t, guardResultText(res), "does not accept option", c.name)
		require.NotContains(t, guardResultText(res), "_ignored_options", "%s must not warn on the format pin", c.name)
	}
}

// The warn rider must survive the response decorators.
// decorateResultWithWarming and decorateResultWithFreshness both end in
// rebuildTextResult, which rebuilds the result from Content[0] and drops
// every other block — so a rider appended mid-chain vanishes exactly when
// the freshness rider fires: a file drifted on disk mid-edit, the #597
// poster child. This drives the real dispatch path with a drifted file and
// pins that BOTH signals arrive.
func TestWarnRiderSurvivesFreshnessRebuildE2E(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "full")
	srv, dir := setupTestServer(t)
	ctx := guardE2ESession(t, srv, "arg_guard_drift_e2e")
	t.Setenv(toolArgGuardEnv, "")

	// Drift the file on disk after indexing — the normal mid-edit state.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

type Config struct {
	Port int
	Host string
}

func main() {
	helper()
}

func helper() {}
`), 0o644))

	res := guardE2ECall(t, srv, ctx, 2, "read_file", map[string]any{"path": "main.go", "line_range": []any{1, 2}})
	require.False(t, res.IsError, guardResultText(res))
	text := guardResultText(res)
	require.Contains(t, text, "freshness",
		"fixture sanity: the drifted file must fire the freshness rider")
	assert.Contains(t, text, "_ignored_options",
		"the warn rider must survive the freshness rebuild")
	assert.Contains(t, text, "line_range")
}

// The warming decorator is the second rebuild seam: a graph tool called
// mid-warmup gets its result rebuilt around the `warming` envelope, and the
// rider must survive that rebuild exactly as it survives freshness.
func TestWarnRiderSurvivesWarmingRebuildE2E(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "full")
	srv, _ := setupTestServer(t)
	// Put the server mid-warmup the way the daemon does: a published
	// readiness phase that is not yet ready.
	srv.readinessBroadcaster = newReadinessBroadcaster(&fakeSpecificSender{}, zap.NewNop())
	srv.readinessBroadcaster.publish(map[string]any{"phase": "parallel_parse", "ready": false})
	ctx := guardE2ESession(t, srv, "arg_guard_warming_e2e")
	t.Setenv(toolArgGuardEnv, "")

	res := guardE2ECall(t, srv, ctx, 2, "read_file", map[string]any{"path": "main.go", "line_range": []any{1, 2}})
	require.False(t, res.IsError, guardResultText(res))
	text := guardResultText(res)
	require.Contains(t, text, "warming",
		"fixture sanity: a mid-warmup read must carry the warming envelope")
	assert.Contains(t, text, "_ignored_options",
		"the warn rider must survive the warming rebuild")
	assert.Contains(t, text, "line_range")
}

// The echoed unknown-key list is caller-controlled, so the rider and the
// reject error bound it in both count and per-key length.
func TestGuardEchoedKeyListIsCapped(t *testing.T) {
	tool, calls := guardFixture(t)
	long := strings.Repeat("k", 300)
	args := map[string]any{
		"aaa": 1, "bbb": 1, "ccc": 1, "ddd": 1, "eee": 1, "fff": 1, "ggg": 1,
		long: 1,
	}

	t.Setenv(toolArgGuardEnv, "")
	res := callGuarded(t, tool, calls, args)
	text := guardResultText(res)
	assert.Contains(t, text, "_ignored_options")
	assert.Contains(t, text, "more)", "overflow keys collapse to a (+N more) tail")
	assert.NotContains(t, text, long, "an oversized key is truncated, never echoed whole")

	t.Setenv(toolArgGuardEnv, "reject")
	res = callGuarded(t, tool, calls, args)
	require.True(t, res.IsError)
	text = guardResultText(res)
	assert.Contains(t, text, "more)")
	assert.NotContains(t, text, long)
}

// Handler-honored keys must be declared: read_file reads max_chars
// (capReadFileContent) — an undeclared honored option is exactly the
// #597 shape in reverse, a working key the schema calls unknown.
func TestReadFileDeclaresMaxChars(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "full")
	srv, _ := setupTestServer(t)
	ctx := guardE2ESession(t, srv, "arg_guard_max_chars")

	t.Setenv(toolArgGuardEnv, "reject")
	res := guardE2ECall(t, srv, ctx, 2, "read_file", map[string]any{"path": "main.go", "max_chars": 32})
	require.False(t, res.IsError, "max_chars is handler-honored and must be declared: %s", guardResultText(res))
	require.NotContains(t, guardResultText(res), "_ignored_options")
}
