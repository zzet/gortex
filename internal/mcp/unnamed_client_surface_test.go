package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// initializeOn sends an MCP initialize frame carrying clientInfo.name on ctx
// and returns the instructions from the reply.
func initializeOn(t *testing.T, srv *Server, ctx context.Context, name string) string {
	t.Helper()
	frame := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":%q,"version":""}}}`, name))
	reply := srv.MCPServer().HandleMessage(ctx, frame)
	require.NotNil(t, reply)
	raw, err := json.Marshal(reply)
	require.NoError(t, err)
	var parsed struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed))
	return parsed.Result.Instructions
}

// listToolNamesOn answers tools/list on ctx.
func listToolNamesOn(t *testing.T, srv *Server, ctx context.Context) map[string]bool {
	t.Helper()
	reply := srv.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
	require.NotNil(t, reply)
	raw, err := json.Marshal(reply)
	require.NoError(t, err)
	var parsed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed))
	names := make(map[string]bool, len(parsed.Result.Tools))
	for _, tool := range parsed.Result.Tools {
		names[tool.Name] = true
	}
	return names
}

// clientCtx is a daemon-dispatched frame context: a session id plus the
// client-connection mark the dispatcher stamps on every frame.
func clientCtx(sessionID string) context.Context {
	return WithClientConnection(WithSessionID(context.Background(), sessionID))
}

// TestUnnamedClientGetsNamedClientSurface is the #661 regression: the tool
// surface a client connection gets must not depend on whether its
// initialize carried a clientInfo.name. Before the fix an empty name got the
// flat legacy catalogue while any non-empty name got the facade surface.
func TestUnnamedClientGetsNamedClientSurface(t *testing.T) {
	stubActiveProfilePreset(t, "")
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})

	namedCtx := clientCtx("named_client")
	namedInstructions := initializeOn(t, srv, namedCtx, "probe")
	named := listToolNamesOn(t, srv, namedCtx)

	unnamedCtx := clientCtx("unnamed_client")
	unnamedInstructions := initializeOn(t, srv, unnamedCtx, "")
	unnamed := listToolNamesOn(t, srv, unnamedCtx)

	require.Equal(t, mapKeysAsSet(facadeToolNames()), named)
	require.Equal(t, named, unnamed, "an empty clientInfo.name must get the named-client surface")
	require.Equal(t, namedInstructions, unnamedInstructions)
	require.Equal(t, FacadeSurfaceVersion, srv.effectiveSessionPolicy(unnamedCtx).preset)
	require.Equal(t, toolPolicyModeHide, srv.effectiveSessionPolicy(unnamedCtx).mode)
}

// TestUnnamedClientSurfaceStableAcrossReinitialize pins determinism over a
// session's lifetime: re-initializing the same session (with or without a
// name) and reconnecting as a fresh session all resolve the same surface.
func TestUnnamedClientSurfaceStableAcrossReinitialize(t *testing.T) {
	stubActiveProfilePreset(t, "")
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})

	ctx := clientCtx("reinit_client")
	initializeOn(t, srv, ctx, "")
	want := listToolNamesOn(t, srv, ctx)
	for _, name := range []string{"", "probe", ""} {
		initializeOn(t, srv, ctx, name)
		require.Equal(t, want, listToolNamesOn(t, srv, ctx), "re-initialize as %q", name)
	}

	// A reconnect mints a new session id (Streamable HTTP re-initialize).
	reconnect := clientCtx("reinit_client_reconnected")
	initializeOn(t, srv, reconnect, "")
	require.Equal(t, want, listToolNamesOn(t, srv, reconnect))
}

// TestUnnamedStdioClientGetsNamedClientSurface covers the embedded stdio
// shape: no gortex session id, but an mcp-go client session bound to ctx.
// Internal session-less callers (dashboard / REST) carry neither and keep
// the server default.
func TestUnnamedStdioClientGetsNamedClientSurface(t *testing.T) {
	stubActiveProfilePreset(t, "")
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})

	internal := listToolNamesOn(t, srv, context.Background())
	require.True(t, internal["read_file"], "an internal session-less caller keeps the server default")
	require.False(t, internal["read"])

	require.NoError(t, srv.ConnectSession("stdio_sim", func([]byte) error { return nil }))
	t.Cleanup(func() { srv.DisconnectSession("stdio_sim") })
	stdioCtx := srv.WithClientSession(context.Background(), "stdio_sim")
	require.Empty(t, SessionIDFromContext(stdioCtx))

	initializeOn(t, srv, stdioCtx, "")
	require.Equal(t, mapKeysAsSet(facadeToolNames()), listToolNamesOn(t, srv, stdioCtx))
}

// TestUnnamedClientExplicitPolicyStillWins keeps explicit configuration
// authoritative: a forwarded per-client spec and an operator-pinned server
// policy both outrank the client default for an unnamed client.
func TestUnnamedClientExplicitPolicyStillWins(t *testing.T) {
	stubActiveProfilePreset(t, "")

	t.Run("forwarded spec", func(t *testing.T) {
		srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
		srv.NoteSessionToolPolicy("forwarded", "nav", "")
		ctx := clientCtx("forwarded")
		initializeOn(t, srv, ctx, "")
		require.Equal(t, "nav", srv.effectiveSessionPolicy(ctx).preset)
	})

	t.Run("operator pinned", func(t *testing.T) {
		srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer", OperatorPinned: true})
		ctx := clientCtx("pinned")
		initializeOn(t, srv, ctx, "")
		require.Same(t, srv.toolPolicy, srv.effectiveSessionPolicy(ctx))
		require.True(t, listToolNamesOn(t, srv, ctx)["read_file"])
	})

	t.Run("instruction profile", func(t *testing.T) {
		stubActiveProfilePreset(t, "localization")
		srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
		ctx := clientCtx("profile")
		initializeOn(t, srv, ctx, "")
		require.Equal(t, "localization", srv.effectiveSessionPolicy(ctx).preset)
	})
}
