package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/profiles"
	"github.com/zzet/gortex/internal/toolref"
)

func TestInstalledGuidanceFollowsActualSessionToolPolicy(t *testing.T) {
	// Reproduce the installed full-profile + reported MCP environment together.
	// A named modern client must not turn this explicit legacy choice into a
	// compact surface, and installed compact examples must not demand its aliases.
	t.Setenv(profiles.ActiveEnv, "full")
	dir := t.TempDir()
	require.NoError(t, profiles.Generate(dir))
	_, err := profiles.Switch(dir, "full")
	require.NoError(t, err)
	installed := []string{agents.InstructionsBody, agents.GlobalInlineBody(dir)}
	for _, tc := range []struct {
		name, spec        string
		wantEdit, compact bool
	}{
		{"reported core and memory extras", "core,+distill_session,+query_notes,+query_memories", true, false},
		{"explicit compact", "compact", true, true},
		{"read only", "readonly", false, false},
		{"explicit editor denial", "core,-edit_file", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GORTEX_TOOLS", tc.spec)
			t.Setenv("GORTEX_TOOLS_MODE", "hide")
			srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
			t.Cleanup(srv.DrainBackground)
			ctx := sessionCtx(t.Name(), "")
			reply := srv.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"chatgpt","version":"1"}}}`))
			data, err := json.Marshal(reply)
			require.NoError(t, err)
			var parsed struct {
				Error  json.RawMessage `json:"error"`
				Result struct {
					Instructions string `json:"instructions"`
				} `json:"result"`
			}
			require.NoError(t, json.Unmarshal(data, &parsed))
			require.Empty(t, parsed.Error)
			names := listToolNamesForSession(t, srv, t.Name())
			editor := "edit_file"
			if tc.compact {
				editor = "edit"
			}
			require.Equal(t, tc.wantEdit, names[editor], "actual advertised editor follows exact policy")
			if tc.compact {
				require.Contains(t, parsed.Result.Instructions, codingAgentInstructions)
				require.False(t, names["edit_file"])
			} else {
				require.Contains(t, parsed.Result.Instructions, serverInstructions)
				require.False(t, names["change"])
			}
			if tc.name == "reported core and memory extras" {
				for _, name := range []string{"edit_file", "edit_symbol", "write_file", "batch_edit", "rename_symbol", "distill_session", "query_notes", "query_memories"} {
					require.True(t, names[name], name)
				}
			}
			for _, body := range installed {
				require.Contains(t, body, toolref.ToolSurfacePolicyText, "installed guidance must defer examples to this real session policy")
				if i := strings.Index(body, "change(operation:"); i >= 0 {
					require.Less(t, strings.Index(body, toolref.ToolSurfacePolicyText), i, "qualify compact requirements before teaching them")
				}
			}
			if !tc.wantEdit {
				require.NotNil(t, srv.checkToolPresetGate(ctx, editor), "instructions must not bypass denied writes")
			}
		})
	}
}
