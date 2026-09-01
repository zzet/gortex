package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEnsureToolPromoted_MakesDeferredToolCallable is the regression test for
// the deferred-tool-unreachable bug: under the defer-mode surface a tool held
// out of the eager tools/list lives in the lazy registry and is unknown to the
// underlying MCP server, so a direct tools/call (the path the CLI's
// `gortex call` and curated verbs take) returned "tool not found".
// EnsureToolPromoted must promote it on demand so a known name is reachable
// without a tools_search round-trip.
func TestEnsureToolPromoted_MakesDeferredToolCallable(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	require.NotNil(t, srv.lazy)
	require.True(t, srv.lazy.Enabled())

	// Pick a tool the lazy split defers (safe_delete_symbol is asserted
	// deferred elsewhere; assert the precondition here too so this test
	// fails loudly if the hot set ever absorbs it).
	const tool = "safe_delete_symbol"
	require.True(t, srv.lazy.IsDeferred(tool), "precondition: %s must be deferred", tool)
	require.NotContains(t, srv.mcpServer.ListTools(), tool, "deferred tool must be absent from the live list before promotion")

	// Promote on demand — the fix.
	require.True(t, srv.EnsureToolPromoted(tool), "EnsureToolPromoted must report it promoted a deferred tool")

	// Now it is a live, callable tool: appearing in the live tools/list is the
	// proof that a direct tools/call by name will resolve. (IsDeferred is a
	// static classification — "should this tool be lazy" — so it stays true;
	// promotion is tracked separately and reflected by the live registry.)
	require.Contains(t, srv.mcpServer.ListTools(), tool, "promoted tool must appear in the live tools/list")

	// Idempotent: a second promote is a no-op on the registry, but the
	// return value reports liveness — the tool is still live, so it
	// returns true. Callers use this as "re-check GetTool", never as
	// "I transitioned it" (the pre-race contract that caused false 404s
	// when a concurrent caller did the transition).
	require.True(t, srv.EnsureToolPromoted(tool), "an already-promoted tool is still live")
}

// TestEnsureToolPromoted_NoopCases covers the guards: a live tool, an unknown
// tool, an empty name, and a nil/lazy-less server are all no-ops (false).
func TestEnsureToolPromoted_NoopCases(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)

	// A hot/eager tool is live, never deferred.
	require.False(t, srv.EnsureToolPromoted("graph_stats"), "a live tool is not deferred → no-op")
	// An unknown tool name.
	require.False(t, srv.EnsureToolPromoted("definitely_not_a_real_tool"))
	// Empty name.
	require.False(t, srv.EnsureToolPromoted(""))
	// nil receiver / no lazy registry.
	var nilSrv *Server
	require.False(t, nilSrv.EnsureToolPromoted("safe_delete_symbol"))
	require.False(t, (&Server{}).EnsureToolPromoted("safe_delete_symbol"))
}
