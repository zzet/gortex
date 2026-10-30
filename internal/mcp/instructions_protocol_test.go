package mcp

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/toolref"
)

// Exercise the protocol entry points: helper-only checks cannot catch modern
// clients receiving static instructions because they never send initialize.
func TestProtocolDiscoveryTransientInstructions(t *testing.T) {
	loading, _, repo := newLoadingServer(t)
	loading.PublishReadiness("parallel_parse", false, nil)
	main := filepath.Join(t.TempDir(), "main")
	gitInitWorktreeRepo(t, main)
	linked := filepath.Join(t.TempDir(), "linked")
	gitInit(t, main, "worktree", "add", "-q", "-b", "feature", linked)
	routed, _ := newWorktreeMCPServer(t, config.RepoEntry{Path: main, Name: "main"})
	routed.PublishReadiness("ready", true, nil)

	for _, state := range []struct {
		name string
		srv  *Server
		cwd  string
		want []string
	}{
		{"ready", routed, main, []string{"When Gortex can serve the step:", "Index status: ready"}},
		{"loading", loading, repo, []string{"LOADING repo-l", "parallel_parse, 15%", "no reconnect is needed"}},
		{"pending", routed, linked, []string{"checkout discovery is pending", "view_building", "no reconnect is needed", "Do not run `gortex track`", "Retry after other work", "after a few attempts"}},
	} {
		for _, client := range []string{"claude-code", "codex"} {
			for _, method := range []string{"initialize", "server/discover"} {
				t.Run(state.name+"/"+client+"/"+method, func(t *testing.T) {
					ctx := sessionCtx(t.Name(), state.cwd)
					params := map[string]any{}
					if method == "initialize" {
						params["protocolVersion"] = "2025-06-18"
						params["capabilities"] = map[string]any{}
						params["clientInfo"] = map[string]any{"name": client, "version": "1"}
					} else {
						params["_meta"] = map[string]any{
							"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
							"io.modelcontextprotocol/clientInfo":         map[string]any{"name": client, "version": "1"},
							"io.modelcontextprotocol/clientCapabilities": map[string]any{},
						}
					}
					frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
					require.NoError(t, err)
					reply := state.srv.MCPServer().HandleMessage(ctx, frame)
					require.NotNil(t, reply)
					out, err := json.Marshal(reply)
					require.NoError(t, err)
					var parsed struct {
						Error  json.RawMessage `json:"error"`
						Result struct {
							Instructions string `json:"instructions"`
						} `json:"result"`
					}
					require.NoError(t, json.Unmarshal(out, &parsed))
					require.Empty(t, parsed.Error, "%s", out)
					instructions := parsed.Result.Instructions
					for _, want := range append(state.want, toolref.TransportClause(), toolref.DegradedModeLine()) {
						require.Contains(t, instructions, want)
					}
					for _, stale := range []string{"integration failure and stop", "integration failure, stop", "Wait for reconciliation and reconnect", "To activate: run"} {
						require.NotContains(t, instructions, stale)
					}
					// The modern client's identity must reach the same policy
					// selector as initialize, not just the SDK's session object.
					require.Equal(t, client, state.srv.sessionFor(ctx).snapshotClientName())
					if state.name != "pending" {
						require.Contains(t, instructions, codingAgentInstructions)
					}
				})
			}
		}
	}
}

func TestDiscoverInstructionsPreservesSessionPolicy(t *testing.T) {
	srv := newFullTestServer(t)
	ctx := sessionCtx("discover-explicit-policy", "")
	srv.NoteSessionToolPolicy("discover-explicit-policy", "full", "hide")
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"claude-code","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}}}`)
	reply := srv.MCPServer().HandleMessage(ctx, frame)
	require.NotNil(t, reply)
	out, err := json.Marshal(reply)
	require.NoError(t, err)
	var parsed struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Equal(t, serverInstructions, parsed.Result.Instructions,
		"modern identity must not override the session's explicit tool policy")
	require.Equal(t, "claude-code", srv.sessionFor(ctx).snapshotClientName())

	// A discovery probe can omit clientInfo. Keep the known client rather
	// than replacing it with an empty identity from a later probe.
	reply = srv.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":2,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`))
	require.NotNil(t, reply)
	require.Equal(t, "claude-code", srv.sessionFor(ctx).snapshotClientName())
}
