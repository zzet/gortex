package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAnalyzeAliasedKindFromLegacySession pins the dashboard fix: a plain
// analyze(kind=processes) call from a NON-facade, session-less caller (the
// HTTP dashboard path — CallToolStrict invokes the tool handler directly
// with no MCP session) must route through the facade to the captured
// get_processes legacy handler instead of falling into the analyze
// dispatcher's "unknown analyze kind" error. This is the reviewer-required
// replacement for generic registry promotion.
//
// Regression: this fails on the pre-rework code — without a facade session
// (clientDefaultPolicy only fires for identified MCP clients) the old
// wrapLegacyFacade routed plain analyze(kind=processes) to the raw
// dispatcher, which rejected the aliased kind.
func TestAnalyzeAliasedKindFromLegacySession(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := context.Background()

	// The legacy tool is deferred under core/defer — the facade must
	// reach it without promoting it into the live registry.
	require.True(t, srv.lazy.IsDeferred("get_processes"))

	// Invoke the analyze tool's registered handler directly with a bare
	// context — exactly what the HTTP dashboard path does via
	// CallToolStrict (no MCP initialize, no session, no client name).
	tool := srv.MCPServer().GetTool("analyze")
	require.NotNil(t, tool, "analyze must be live under the core/defer surface")
	req := makeReq("analyze", map[string]any{"kind": "processes"})
	res, err := tool.Handler(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "analyze kind=processes must not error: %s", toolResultText(res))
	require.Contains(t, toolResultText(res), "processes",
		"the facade must reach the get_processes handler's JSON payload")

	// The legacy tool must NOT have been promoted into the live registry.
	require.True(t, srv.lazy.IsDeferred("get_processes"),
		"facade dispatch must not promote the legacy tool")
	require.Nil(t, srv.MCPServer().GetTool("get_processes"))
}

// TestAnalyzeAliasedKindWithIDReachesProcessDetail covers the web app's
// processDetail path: analyze(kind=processes, id=...) must forward the id
// to the legacy handler. Same session-less direct-handler invocation.
func TestAnalyzeAliasedKindWithIDReachesProcessDetail(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := context.Background()

	tool := srv.MCPServer().GetTool("analyze")
	require.NotNil(t, tool)
	req := makeReq("analyze", map[string]any{"kind": "processes", "id": "proc_1"})
	res, err := tool.Handler(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "analyze kind=processes with id must not error: %s", toolResultText(res))
	require.Contains(t, toolResultText(res), "processes")
}

// TestAnalyzeNativeKindStillUsesDispatcher keeps the non-aliased kinds on
// the dispatcher path: hotspots is a native analyze kind and must NOT be
// rerouted through the facade (its behavior is unchanged).
func TestAnalyzeNativeKindStillUsesDispatcher(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := context.Background()

	tool := srv.MCPServer().GetTool("analyze")
	require.NotNil(t, tool)
	req := makeReq("analyze", map[string]any{"kind": "hotspots"})
	res, err := tool.Handler(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, res)
	// Hotspots is a native kind — the dispatcher answers it. On the tiny
	// fixture it may report "codebase too small", which is a dispatcher
	// result, never an unknown-kind error.
	require.NotContains(t, toolResultText(res), "unknown analyze kind")
}
