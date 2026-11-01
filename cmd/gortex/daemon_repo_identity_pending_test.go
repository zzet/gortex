package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"go.uber.org/zap"
)

func TestDispatcher_ColdIdentityPendingHasNoInventedScope(t *testing.T) {
	d, mi, registered, _ := loadingDispatcherSetup(t)
	physical := filepath.Join(t.TempDir(), "physical-root")
	require.NoError(t, os.MkdirAll(physical, 0755))
	alias := filepath.Join(t.TempDir(), "different-configured-name")
	require.NoError(t, os.Symlink(physical, alias))
	entries := []config.RepoEntry{{Path: registered}, {Path: alias, Name: "private-config-name"}}
	mi.DeclareRepos(entries)
	for _, cwd := range []string{physical, t.TempDir()} {
		sess := &daemon.Session{ID: "identity-" + filepath.Base(cwd), CWD: cwd}
		d.SessionStarted(sess, func([]byte) error { return nil })
		t.Cleanup(func() { d.SessionEnded(sess) })
		init := dispatchJSON(t, d, sess, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"identity-test","version":"1"}}}`)
		body, err := json.Marshal(init)
		require.NoError(t, err)
		require.Contains(t, string(body), "identity_pending")
		require.NotContains(t, string(body), "INACTIVE")
		require.NotContains(t, string(body), "already configured")
		require.NotContains(t, string(body), "private-config-name")
		require.NotContains(t, string(body), "gortex track")
		discover := dispatchJSON(t, d, sess, `{"jsonrpc":"2.0","id":8,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"claude-code","version":"2.1.312"},"io.modelcontextprotocol/clientCapabilities":{}}}}`)
		discovery, marshalErr := json.Marshal(discover)
		require.NoError(t, marshalErr)
		require.Contains(t, string(discovery), "identity_pending")
		require.NotContains(t, string(discovery), "INACTIVE")
		require.NotContains(t, string(discovery), "private-config-name")
		start := time.Now()
		reply := dispatchJSON(t, d, sess, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"query":"RegisteredOnly"}}}`)
		require.Less(t, time.Since(start), 100*time.Millisecond, "negative identity admission never waits for resolution")
		data := errorData(t, reply)
		require.Equal(t, "repo_loading", data["error_code"])
		require.Equal(t, true, data["retryable"])
		require.Equal(t, "identity_resolution", data["phase"])
		require.EqualValues(t, 0, data["percent"])
		require.Equal(t, "identity_pending", data["reason"])
		require.NotContains(t, data, "repo")
		require.NotContains(t, data, "repo_path")
		require.NotContains(t, reply, "result")
		for _, tool := range []string{
			`"name":"index_health","arguments":{}`,
			`"name":"workspace","arguments":{"operation":"index"}`,
		} {
			health := dispatchJSON(t, d, sess, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{`+tool+`}}`)
			text := toolResultText(t, health)
			var payload map[string]any
			require.NoError(t, json.Unmarshal([]byte(text), &payload))
			require.Equal(t, "identity_pending", payload["reason"])
			require.Empty(t, payload["repos"], "an unbound diagnostic must not disclose named repositories")
		}
		track := dispatchJSON(t, d, sess, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"track_repository","arguments":{"path":"."}}}`)
		require.Equal(t, "identity_pending", errorData(t, track)["reason"], "unknown ownership must not duplicate a configured alias through bootstrap tracking")
	}
	mi.ExpectRepos(entries)
	physicalSess := &daemon.Session{ID: "resolved-alias", CWD: physical}
	data := errorData(t, dispatchJSON(t, d, physicalSess, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search","arguments":{"query":"RegisteredOnly"}}}`))
	require.Equal(t, "repo_loading", data["error_code"])
	require.Equal(t, alias, data["repo_path"], "completed owner resolution can now prove configured ownership")
	unknown, err := d.cwdReachableChecked(t.Context(), t.TempDir())
	require.NoError(t, err)
	require.False(t, unknown, "a completed identity set permits a definitive negative")
}

func TestDispatcher_IdentityPendingPreservesAuthoritativePositives(t *testing.T) {
	d, mi, registered, _ := loadingDispatcherSetup(t)
	mi.DeclareRepos([]config.RepoEntry{{Path: registered}, {Path: filepath.Join(t.TempDir(), "unresolved")}})
	require.True(t, mi.RepoIdentityPending())
	ok, err := d.cwdReachableChecked(t.Context(), registered)
	require.NoError(t, err)
	require.True(t, ok)
	healthySession := &daemon.Session{ID: "registered-while-identity-pending", CWD: registered}
	d.SessionStarted(healthySession, func([]byte) error { return nil })
	t.Cleanup(func() { d.SessionEnded(healthySession) })
	dispatchJSON(t, d, healthySession, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"positive-control","version":"1"}}}`)
	healthyReply := dispatchJSON(t, d, healthySession, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"query":"RegisteredOnly"}}}`)
	require.Empty(t, errorCode(healthyReply))
	require.Contains(t, toolResultText(t, healthyReply), "RegisteredOnly")
	checkout := t.TempDir()
	d.checkoutCWDProbe = func(_ context.Context, cwd string) (bool, error) { return cwd == checkout, nil }
	ok, err = d.cwdReachableChecked(t.Context(), checkout)
	require.NoError(t, err)
	require.True(t, ok, "an authoritative existing checkout still serves while another root is unresolved")
	remote := t.TempDir()
	require.NoError(t, writeFile(filepath.Join(remote, ".gortex.yaml"), "workspace: remote-ws\n"))
	cfg := &daemon.ServersConfig{Server: []daemon.ServerEntry{
		{Slug: "self", URL: "unix:///tmp/never-dialed.sock", Default: true},
		{Slug: "remote", URL: "https://remote.example.com", Workspaces: []string{"remote-ws"}},
	}}
	require.NoError(t, cfg.Validate())
	d.SetRouter(daemon.NewRouter(daemon.RouterConfig{Servers: cfg, Rosters: daemon.NewWorkspaceRosterCache(0), LocalSlug: "self", Logger: zap.NewNop()}))
	ok, err = d.cwdReachableChecked(t.Context(), remote)
	require.NoError(t, err)
	require.True(t, ok, "router authority takes precedence over an unresolved negative")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = d.cwdReachableChecked(ctx, t.TempDir())
	require.ErrorIs(t, err, context.Canceled)
}

func TestCoverage_IdentityPendingIsNeitherTrackedNorGraphAnswer(t *testing.T) {
	_, mi, registered, _ := loadingDispatcherSetup(t)
	mi.DeclareRepos([]config.RepoEntry{{Path: registered}, {Path: filepath.Join(t.TempDir(), "unresolved")}})
	c := &realController{multiIndexer: mi}
	path := filepath.Join(t.TempDir(), "file.go")
	file, err := c.FileCoverage(t.Context(), daemon.FileCoverageParams{Path: path})
	require.NoError(t, err)
	require.Equal(t, daemon.RepoStateIdentityPending, file.RepoState)
	require.Empty(t, file.Repo)
	require.False(t, file.Tracked)
	require.False(t, file.Answered)
	require.False(t, file.Covered)
	dir, err := c.DirCoverage(t.Context(), daemon.DirCoverageParams{Path: filepath.Dir(path)})
	require.NoError(t, err)
	require.Equal(t, daemon.RepoStateIdentityPending, dir.RepoState)
	require.Empty(t, dir.Repo)
	require.False(t, dir.Tracked)
	require.False(t, dir.Answered)
	require.False(t, dir.HasSource)
	healthy, err := c.FileCoverage(t.Context(), daemon.FileCoverageParams{Path: filepath.Join(registered, "a.go")})
	require.NoError(t, err)
	require.Empty(t, healthy.RepoState)
}

func TestExtractToolResult_IdentityPendingDoesNotClaimTracking(t *testing.T) {
	_, err := extractToolResult([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"identity pending","data":{"error_code":"repo_loading","retryable":true,"reason":"identity_pending","phase":"identity_resolution","percent":0}}}`))
	var loading *repoLoadingError
	require.ErrorAs(t, err, &loading)
	require.Equal(t, exitTempFail, loading.ExitCode())
	require.Contains(t, err.Error(), "identity_pending")
	require.NotContains(t, err.Error(), "already tracked")
	require.NotContains(t, err.Error(), "gortex track")
}

func TestDispatcher_ColdDarwinTmpAliasUsesIdentityPending(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("requires Darwin's /tmp -> /private/tmp alias")
	}
	canonical, err := filepath.EvalSymlinks("/tmp")
	require.NoError(t, err)
	if canonical != "/private/tmp" {
		t.Skip("host does not expose the /tmp -> /private/tmp alias")
	}
	root, err := os.MkdirTemp("/tmp", "g1023-wire-alias-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	d, mi := trackedPathMCPSetup(t, t.TempDir())
	mi.DeclareRepos([]config.RepoEntry{{Path: root}})
	sess := &daemon.Session{ID: "actual-tmp-alias", CWD: filepath.Join(canonical, filepath.Base(root))}
	d.SessionStarted(sess, func([]byte) error { return nil })
	t.Cleanup(func() { d.SessionEnded(sess) })
	initialized := dispatchJSON(t, d, sess, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"alias-test","version":"1"}}}`)
	raw, err := json.Marshal(initialized)
	require.NoError(t, err)
	require.Contains(t, string(raw), "identity_pending")
	require.NotContains(t, string(raw), "INACTIVE")
	data := errorData(t, dispatchJSON(t, d, sess, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_symbols","arguments":{"query":"Anything"}}}`))
	require.Equal(t, "repo_loading", data["error_code"])
	require.Equal(t, "identity_pending", data["reason"])
	require.NotContains(t, data, "repo_path")
}
