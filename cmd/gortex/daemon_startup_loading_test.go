package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
)

// Exercise the real pre-listen helper and public MCP socket before ANY warmup
// announcement, seeding or repository indexing. Starting registration by hand
// here would conceal the startup window this test is intended to cover.
func TestConfiguredLoadingPublishedBeforeServing(t *testing.T) {
	registered := t.TempDir()
	d, mi := trackedPathMCPSetup(t, registered)
	root := filepath.Join(t.TempDir(), "configured")
	admin := filepath.Join(root, ".git", "worktrees", "linked")
	linked := filepath.Join(t.TempDir(), "linked")
	require.NoError(t, os.MkdirAll(admin, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(linked, "pkg"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+admin+"\n"), 0o600))
	c := probeController(t, fmt.Sprintf("repos:\n  - path: %q\n    name: startup\n", root))
	state := &daemonState{multiIndexer: mi, configManager: c.configManager}

	file, err := os.CreateTemp(os.TempDir(), "gx-startup-*.sock")
	require.NoError(t, err)
	socket := file.Name()
	require.NoError(t, file.Close())
	require.NoError(t, os.Remove(socket))
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(t.TempDir(), "daemon.pid"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(t.TempDir(), "daemon.state.json"))
	srv := daemon.New(socket, "test", zap.NewNop())
	srv.MCPDispatcher = d
	require.NoError(t, listenConfiguredDaemon(srv, state))
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	t.Cleanup(func() {
		require.NoError(t, srv.Shutdown())
		select {
		case err := <-served:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("owned startup test server did not stop")
		}
	})

	for name, cwd := range map[string]string{
		"primary": root, "subdirectory": filepath.Join(root, "pkg"),
		"linked": linked, "linked subdirectory": filepath.Join(linked, "pkg"),
	} {
		t.Run(name, func(t *testing.T) {
			client, err := daemon.DialTo(socket, daemon.Handshake{Mode: daemon.ModeMCP, CWD: cwd})
			require.NoError(t, err)
			defer client.Close()
			require.NoError(t, client.Conn.SetDeadline(time.Now().Add(5*time.Second)))
			require.NoError(t, client.WriteMCPFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"startup-probe","version":"1"}}}`)))
			initialized, err := client.ReadMCPFrame()
			require.NoError(t, err)
			var initReply struct {
				Result struct {
					Instructions string `json:"instructions"`
				} `json:"result"`
			}
			require.NoError(t, json.Unmarshal(initialized, &initReply))
			expectedInstructions := gortexmcp.ServerInstructionsLoading("startup", "starting", 0)
			if strings.HasPrefix(name, "linked") {
				expectedInstructions = gortexmcp.ServerInstructionsIdentityPending()
			}
			assert.Contains(t, initReply.Result.Instructions, expectedInstructions,
				"the first real initialize must carry an honest loading variant before identity warmup")
			require.NoError(t, client.WriteMCPFrame([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_symbols","arguments":{"query":"Startup"}}}`)))
			frame, err := client.ReadMCPFrame()
			require.NoError(t, err)
			var reply map[string]any
			require.NoError(t, json.Unmarshal(frame, &reply))
			data := errorData(t, reply)
			assert.Equal(t, "repo_loading", data["error_code"])
			assert.Equal(t, true, data["retryable"])
			if strings.HasPrefix(name, "linked") {
				assert.Equal(t, "identity_pending", data["reason"])
				assert.NotContains(t, data, "repo_path", "an unresolved lexical declaration cannot name a linked request's family")
			} else {
				assert.Equal(t, root, data["repo_path"])
			}
			assert.Contains(t, data, "phase")
			assert.Contains(t, data, "percent")
			assert.NotContains(t, string(frame), "gortex track")
			assert.NotContains(t, string(frame), "repos reconcile")
		})
	}
	assert.Empty(t, mi.GetMetadata("startup"), "pending declarations must never register or admit graph access")
	untracked := &daemon.Session{ID: "unconfigured", CWD: t.TempDir()}
	before := errorData(t, dispatchJSON(t, d, untracked,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_symbols","arguments":{"query":"Startup"}}}`))
	assert.Equal(t, "repo_loading", before["error_code"])
	assert.Equal(t, "identity_pending", before["reason"])
	assert.NotContains(t, before, "repo_path")
	// The real existing warmup owner now resolves identity. A linked path can
	// then name its configured family and an unrelated path is truly negative.
	announceConfiguredRepos(state)
	linkedSession := &daemon.Session{ID: "resolved-linked", CWD: linked}
	after := errorData(t, dispatchJSON(t, d, linkedSession,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search_symbols","arguments":{"query":"Startup"}}}`))
	assert.Equal(t, "repo_loading", after["error_code"])
	assert.Equal(t, root, after["repo_path"])
	assert.Equal(t, "repo_not_tracked", errorCode(dispatchJSON(t, d, untracked,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_symbols","arguments":{"query":"Startup"}}}`)))
}
