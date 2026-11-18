package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentPreset_Membership(t *testing.T) {
	p := newToolPolicy(ToolPolicyConfig{Preset: "agent"}, nil)
	require.Equal(t, "agent", p.preset)
	require.True(t, p.lean, "the agent preset is lean")
	require.True(t, p.allows("search_symbols"))
	require.True(t, p.allows("edit_file"))
	require.True(t, p.allows("tool_profile")) // always kept
	require.True(t, p.allows(LazyToolsSearchName))
	require.False(t, p.allows("analyze"), "analyze is not in the agent floor")
	require.False(t, p.allows("get_architecture"))
	// The negotiable memory tail is deferred (cut from the tail, not the
	// floor) so it stays out of the eager surface but reachable by name.
	for _, tail := range agentTailTools {
		require.Falsef(t, p.allows(tail), "tail tool %q must be deferred, not in the agent floor", tail)
	}
	// The alias resolves to the same preset.
	require.Equal(t, "agent", newToolPolicy(ToolPolicyConfig{Preset: "coding-agent"}, nil).preset)
}

// TestClientAwareDefault: every identified client gets the same compact closed
// surface; a bare session id with no client connection (an internal caller)
// keeps the compatibility default; and an explicit forwarded preset still wins.
func TestClientAwareDefault(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})

	// Known coding-agent client → compact domain surface.
	srv.NoteSessionClient("sess_cc", "claude-code", "1.0")
	cc := listToolNamesForSession(t, srv, "sess_cc")
	require.Equal(t, mapKeysAsSet(facadeToolNames()), cc)
	require.True(t, cc["read"])
	require.False(t, cc["read_file"])

	// An unknown editor is still an identified MCP client and gets the same
	// JSON-safe compact surface.
	srv.NoteSessionClient("sess_ed", "some-editor", "1.0")
	ed := listToolNamesForSession(t, srv, "sess_ed")
	require.Equal(t, mapKeysAsSet(facadeToolNames()), ed)
	require.True(t, ed["read"])
	require.False(t, ed["read_file"])

	// Before initialize supplies clientInfo, the server global remains in force.
	anonymous := listToolNamesForSession(t, srv, "sess_anonymous")
	require.True(t, anonymous["read_file"])
	require.False(t, anonymous["read"])
	require.Greater(t, len(anonymous), len(cc), "core is wider than the compact surface")

	// A forwarded GORTEX_TOOLS spec overrides the client-aware default.
	srv.NoteSessionClient("sess_ov", "claude-code", "1.0")
	srv.NoteSessionToolPolicy("sess_ov", "full", "")
	ov := listToolNamesForSession(t, srv, "sess_ov")
	require.True(t, ov["analyze"], "forwarded full overrides the coding-client default")
	require.True(t, ov["get_architecture"])
	require.True(t, ov["read_file"])
	require.False(t, ov["read"])
}

func TestExplicitCoreDeferPolicyOverridesNamedClientDefault(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{
		Preset: "core", Mode: "defer", OperatorPinned: true,
	})
	srv.NoteSessionClient("sess_rollback", "any-agent", "1.0")
	tools := listToolNamesForSession(t, srv, "sess_rollback")
	require.True(t, tools["read_file"])
	require.False(t, tools["read"])
}

func TestCompactSurfaceIgnoresDeltasToKeepListAndGateAligned(t *testing.T) {
	cfg := parseToolSpec("facade-v1,+read_file,-read")
	p := newToolPolicy(cfg, nil)
	require.True(t, p.allows("read"))
	require.False(t, p.allows("read_file"))

	srv := setupPresetServer(t, cfg)
	srv.NoteSessionClient("sess_closed", "any-agent", "1.0")
	require.Equal(t, mapKeysAsSet(facadeToolNames()), listToolNamesForSession(t, srv, "sess_closed"))
}

// paramDescLen returns the length of a parameter's description in one tool's
// serialized schema, or -1 if absent.
func paramDescLen(t *testing.T, srv *Server, tool, param string) int {
	t.Helper()
	reply := srv.MCPServer().HandleMessage(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	out, _ := json.Marshal(reply)
	var parsed struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties map[string]struct {
						Description string `json:"description"`
					} `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(out, &parsed))
	for _, tl := range parsed.Result.Tools {
		if tl.Name != tool {
			continue
		}
		p, ok := tl.InputSchema.Properties[param]
		if !ok {
			return -1
		}
		return len(p.Description)
	}
	return -1
}

// TestAgentPreset_LeanizationShrinksParams: the lean surface compacts every
// parameter description, and never mutates the server's shared schema (a
// full server still serves the long prose).
func TestAgentPreset_LeanizationShrinksParams(t *testing.T) {
	agentSrv := setupPresetServer(t, ToolPolicyConfig{Preset: "agent", Mode: "defer"})
	fullSrv := setupPresetServer(t, ToolPolicyConfig{Preset: "full"})

	// A long bespoke param (search_symbols.expand) is compacted on the lean
	// surface but full-length on the full surface.
	lean := paramDescLen(t, agentSrv, "search_symbols", "expand")
	full := paramDescLen(t, fullSrv, "search_symbols", "expand")
	require.Greater(t, lean, 0)
	require.LessOrEqual(t, lean, agentParamCap+4, "lean expand param compacted")
	require.Greater(t, full, agentParamCap+40, "full surface keeps the long expand prose (shared schema untouched)")
}
