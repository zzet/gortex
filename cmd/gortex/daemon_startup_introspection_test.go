package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
)

// Exercise the same Unix listener and metadata-to-runtime dispatcher handoff
// as daemon start, with a configured caller still loading and a different
// repository already registered in the target runtime.
func TestDaemonStartupIntrospectionFromServing(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "core")
	t.Setenv("GORTEX_TOOLS_MODE", "defer")
	dispatcher, multi, sibling, loading := loadingDispatcherSetup(t)
	cwd := filepath.Join(loading, "nested")
	require.NoError(t, os.Mkdir(cwd, 0o755))
	controller := probeController(t, fmt.Sprintf("repos:\n  - path: %q\n  - path: %q\n", loading, sibling))
	controller.multiIndexer, controller.graph = multi, multi.Graph()

	dir, err := os.MkdirTemp("", "gx-c4")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "p"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(dir, "state"))

	served := make(chan time.Time, 1)
	core, _ := observer.New(zapcore.InfoLevel)
	logger := zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		if entry.Message == "daemon: serving" {
			served <- entry.Time
		}
		return nil
	}))
	reporter := newDaemonStartupReporterWithWriter(logger, func(daemon.RuntimeState) error { return nil })
	t.Cleanup(reporter.Stop)
	metadata := newMigrationMetadataDispatcher(config.Default(), controller.configManager.Global().ReposSnapshot(), logger, reporter)
	socket := filepath.Join(dir, "s")
	srv := daemon.New(socket, "startup-introspection-test", logger)
	srv.MCPDispatcher = metadata
	srv.Ready = func() (bool, string) { return false, metadata.phase() }
	srv.EnableStartupMetadata()
	require.NoError(t, srv.Listen())
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()
	t.Cleanup(func() {
		require.NoError(t, srv.Shutdown())
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(2 * time.Second):
			t.Error("owned listener did not stop")
		}
	})
	var servingAt time.Time
	select {
	case servingAt = <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("owned listener did not log daemon: serving")
	}
	deadline := servingAt.Add(time.Second)
	direct := openStartupIntrospectionSession(t, socket, cwd, "core", "defer", deadline)
	facade := openStartupIntrospectionSession(t, socket, cwd, "facade-v1", "hide", deadline)
	unbound := openStartupIntrospectionSession(t, socket, dir, "core", "defer", deadline)

	for _, phase := range []string{"opening_store", "migrating", "starting"} {
		loadState := indexer.RepoLoadQueued
		switch phase {
		case "migrating":
			reporter.ObserveMigration(store_sqlite.MigrationProgress{Version: 40, Phase: store_sqlite.MigrationStarted})
		case "starting":
			loadState = indexer.RepoLoadRunning
			multi.StartRepoLoad(loading)
			require.NoError(t, srv.PublishStartupRuntime(controller, metadata.handoff(dispatcher),
				func() (bool, string) { return false, "starting" }, nil, nil, daemon.RuntimeStatePath()))
		}
		t.Run(phase, func(t *testing.T) {
			for i, test := range []struct {
				client *daemon.Client
				name   string
				args   map[string]any
				health bool
				code   string
			}{
				{direct, "index_health", nil, true, ""},
				{direct, "tools_search", map[string]any{"query": "index", "promote": false, "max_results": 1}, false, ""},
				{direct, "graph_stats", nil, false, "repo_loading"},
				{facade, "workspace", map[string]any{"operation": "index"}, true, ""},
				{facade, "capabilities", nil, false, ""},
				{facade, "capabilities", map[string]any{"domain": "workspace", "operation": "index"}, false, ""},
				{facade, "workspace", map[string]any{"operation": "graph"}, false, "repo_loading"},
			} {
				params := map[string]any{"name": test.name, "arguments": test.args}
				frame, reply := startupIntrospectionRoundTrip(t, test.client, i+2, "tools/call", params)
				elapsed := time.Since(servingAt)
				t.Logf("surface=%s tool=%s args=%v since_serving=%s response=%s", test.client.Ack.SessionID, test.name, test.args, elapsed, frame)
				assert.LessOrEqual(t, elapsed, time.Second)
				assert.NotContains(t, string(frame), sibling, "a different configured repository must not appear")
				if test.code != "" {
					assert.Equal(t, test.code, errorCode(reply))
					if errorCode(reply) == test.code {
						data := errorData(t, reply)
						assert.Equal(t, true, data["retryable"])
						assert.Equal(t, phase, data["phase"])
					}
					continue
				}
				result, ok := reply["result"].(map[string]any)
				if !assert.True(t, ok, "the introspection call should succeed: %s", frame) {
					continue
				}
				assert.NotEqual(t, true, result["isError"])
				if test.health {
					health := startupIntrospectionHealth(t, reply, phase)
					if assert.Len(t, health.Repos, 1) {
						assert.Equal(t, loading, health.Repos[0].Path)
						assert.Equal(t, loadState, health.Repos[0].State)
					}
				}
			}
			for _, hidden := range []struct {
				client *daemon.Client
				name   string
			}{{direct, "capabilities"}, {facade, "tools_search"}} {
				frame, reply := startupIntrospectionRoundTrip(t, hidden.client, 12, "tools/call", map[string]any{"name": hidden.name})
				result, _ := reply["result"].(map[string]any)
				assert.True(t, reply["error"] != nil || result["isError"] == true, "keep the negotiated discovery contract: %s", frame)
				assert.NotEqual(t, "repo_loading", errorCode(reply), "surface rejection must come from tool policy")
			}
			if phase != "starting" {
				_, reply := startupIntrospectionRoundTrip(t, unbound, 13, "tools/call", map[string]any{"name": "index_health"})
				health := startupIntrospectionHealth(t, reply, phase)
				assert.Empty(t, health.Repos)
				assert.Equal(t, "identity_pending", health.Reason)
			}
		})
	}
}

type startupIntrospectionHealthPayload struct {
	Status string `json:"status"`
	Phase  string `json:"phase"`
	Reason string `json:"reason"`
	Repos  []struct {
		Path  string                `json:"path"`
		State indexer.RepoLoadState `json:"state"`
	} `json:"repos"`
}

func startupIntrospectionHealth(t *testing.T, reply map[string]any, phase string) startupIntrospectionHealthPayload {
	t.Helper()
	var health startupIntrospectionHealthPayload
	require.NoError(t, json.Unmarshal([]byte(toolResultText(t, reply)), &health))
	assert.Equal(t, "loading", health.Status)
	assert.Equal(t, phase, health.Phase)
	return health
}

func openStartupIntrospectionSession(t *testing.T, socket, cwd, tools, mode string, deadline time.Time) *daemon.Client {
	t.Helper()
	client, err := daemon.DialToWithDeadline(socket, daemon.Handshake{
		Mode: daemon.ModeMCP, CWD: cwd, ClientName: "startup-introspection-test", Tools: tools, ToolsMode: mode,
	}, deadline)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	_, reply := startupIntrospectionRoundTrip(t, client, 1, "initialize", map[string]any{
		"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "startup-introspection-test", "version": "1.0"},
	})
	require.Contains(t, reply, "result")
	require.NoError(t, client.WriteMCPFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)))
	return client
}

func startupIntrospectionRoundTrip(t *testing.T, client *daemon.Client, id int, method string, params map[string]any) ([]byte, map[string]any) {
	t.Helper()
	request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	require.NoError(t, err)
	require.NoError(t, client.WriteMCPFrame(request))
	for {
		frame, err := client.ReadMCPFrame()
		require.NoError(t, err)
		var reply map[string]any
		require.NoError(t, json.Unmarshal(frame, &reply))
		if reply["id"] == float64(id) {
			return frame, reply
		}
	}
}
