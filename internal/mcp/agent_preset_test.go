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

// TestAgentPreset_ClientAwareDefault: a known coding-agent client gets the
// lean agent surface with no configuration; an editor / unknown client
// keeps the server's global core default; an explicit forwarded preset
// overrides the client default.
func TestAgentPreset_ClientAwareDefault(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})

	// Known coding-agent client → agent surface (analyze deferred out).
	srv.NoteSessionClient("sess_cc", "claude-code", "1.0")
	cc := listToolNamesForSession(t, srv, "sess_cc")
	require.True(t, cc["search_symbols"])
	require.True(t, cc["edit_file"])
	require.False(t, cc["analyze"], "analyze is not in the lean agent surface")

	// Editor / unknown client → the server's global core default (analyze in).
	srv.NoteSessionClient("sess_ed", "some-editor", "1.0")
	ed := listToolNamesForSession(t, srv, "sess_ed")
	require.True(t, ed["analyze"], "unknown client keeps the core default")
	require.Greater(t, len(ed), len(cc), "core is wider than the lean agent surface")

	// A forwarded GORTEX_TOOLS spec overrides the client-aware default.
	srv.NoteSessionClient("sess_ov", "claude-code", "1.0")
	srv.NoteSessionToolPolicy("sess_ov", "full", "")
	ov := listToolNamesForSession(t, srv, "sess_ov")
	require.True(t, ov["analyze"], "forwarded full overrides the agent default")
	require.True(t, ov["get_architecture"])
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
