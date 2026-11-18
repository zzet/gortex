package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
)

// TestLegacyToolAnnotationsMatchEffectRegistry guards #883: every legacy tool
// in tools/list must advertise readOnlyHint / destructiveHint derived from the
// daemon effect registry, not the mcp-go NewTool default of
// readOnlyHint:false / destructiveHint:true (which made Claude Code plan mode
// prompt on every pure read).
func TestLegacyToolAnnotationsMatchEffectRegistry(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "full"})
	reply := srv.MCPServer().HandleMessage(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	require.NotNil(t, reply)
	out, err := json.Marshal(reply)
	require.NoError(t, err)
	var parsed struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Annotations struct {
					ReadOnly    *bool `json:"readOnlyHint"`
					Destructive *bool `json:"destructiveHint"`
				} `json:"annotations"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(out, &parsed))

	seen := map[string]bool{}
	legacy := 0
	for _, tool := range parsed.Result.Tools {
		seen[tool.Name] = true
		if isFacadeToolName(tool.Name) {
			continue
		}
		legacy++
		require.NotNil(t, tool.Annotations.ReadOnly, "%s: readOnlyHint missing", tool.Name)
		require.NotNil(t, tool.Annotations.Destructive, "%s: destructiveHint missing", tool.Name)
		require.Equal(t, !daemon.IsEffectful(tool.Name), *tool.Annotations.ReadOnly,
			"%s: readOnlyHint disagrees with daemon.ToolEffects", tool.Name)
		require.Equal(t, daemon.IsDestructive(tool.Name), *tool.Annotations.Destructive,
			"%s: destructiveHint disagrees with daemon.IsDestructive", tool.Name)
	}
	require.Greater(t, legacy, 100, "full preset should list the legacy catalog")

	// Spot-check the tools named in the issue so a registry regression that
	// happens to stay self-consistent is still caught.
	for _, name := range []string{
		"query_memories", "read_file", "search_text", "search_symbols", "get_symbol_source",
		"find_files", "find_usages", "smart_context", "graph_stats", "index_health", "list_repos",
	} {
		require.True(t, seen[name], "%s not in full tools/list", name)
		require.False(t, daemon.IsEffectful(name), "%s should be read-only", name)
	}
	for _, name := range []string{"edit_file", "write_file", "rename_symbol"} {
		require.True(t, daemon.IsDestructive(name), "%s should be destructive", name)
	}
	for _, name := range []string{"store_memory", "save_note", "surface_memories"} {
		require.True(t, daemon.IsEffectful(name), "%s writes state", name)
		require.False(t, daemon.IsDestructive(name), "%s is additive", name)
	}
}
