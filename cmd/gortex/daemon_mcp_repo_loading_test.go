package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
)

// loadingDispatcherSetup builds a dispatcher whose daemon has one registered
// repository and one configured repository warmup has not registered yet —
// the shape of every configured repository right after a restart.
func loadingDispatcherSetup(t *testing.T) (d *mcpDispatcher, mi *indexer.MultiIndexer, registered, loading string) {
	t.Helper()
	registered = filepath.Join(t.TempDir(), "registered")
	require.NoError(t, os.MkdirAll(registered, 0o755))
	require.NoError(t, writeFile(filepath.Join(registered, "a.go"), "package a\n\nfunc RegisteredOnly() {}\n"))
	loading = filepath.Join(t.TempDir(), "loading")
	require.NoError(t, os.MkdirAll(loading, 0o755))
	require.NoError(t, writeFile(filepath.Join(loading, "b.go"), "package b\n\nfunc LoadProbe() {}\n"))

	d, mi = trackedPathMCPSetup(t, registered)
	mi.ExpectRepos([]config.RepoEntry{{Path: registered}, {Path: loading}})
	return d, mi, registered, loading
}

func dispatchJSON(t *testing.T, d *mcpDispatcher, sess *daemon.Session, frame string) map[string]any {
	t.Helper()
	reply, err := d.Dispatch(context.Background(), sess, []byte(frame))
	require.NoError(t, err)
	require.NotNil(t, reply, "frame %s must produce a reply", frame)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(reply, &parsed))
	return parsed
}

func errorData(t *testing.T, parsed map[string]any) map[string]any {
	t.Helper()
	errObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok, "expected a JSON-RPC error: %v", parsed)
	data, ok := errObj["data"].(map[string]any)
	require.True(t, ok, "error must carry data: %v", parsed)
	return data
}

func errorCode(parsed map[string]any) string {
	errObj, _ := parsed["error"].(map[string]any)
	data, _ := errObj["data"].(map[string]any)
	code, _ := data["error_code"].(string)
	return code
}

func toolResultText(t *testing.T, parsed map[string]any) string {
	t.Helper()
	result, ok := parsed["result"].(map[string]any)
	require.True(t, ok, "expected a tool result: %v", parsed)
	content, _ := result["content"].([]any)
	require.NotEmpty(t, content, "tool result must carry content: %v", parsed)
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	return text
}

func TestDispatcher_LoadingRepo_AnswersByMethod(t *testing.T) {
	d, _, registered, loading := loadingDispatcherSetup(t)
	sess := &daemon.Session{ID: "sess_loading", CWD: filepath.Join(loading)}

	refused := map[string]string{
		"graph read":         `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_symbols","arguments":{"query":"LoadProbe"}}}`,
		"facade graph":       `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"workspace","arguments":{"operation":"graph"}}}`,
		"facade read":        `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"explore","arguments":{"task":"find LoadProbe"}}}`,
		"edit":               `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"edit_file","arguments":{"path":"b.go","old_string":"b","new_string":"c"}}}`,
		"resource read":      `{"jsonrpc":"2.0","id":5,"method":"resources/read","params":{"uri":"gortex://index-health"}}`,
		"prompt":             `{"jsonrpc":"2.0","id":6,"method":"prompts/get","params":{"name":"anything"}}`,
		"graph_stats blocks": `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"graph_stats","arguments":{}}}`,
	}
	for name, frame := range refused {
		t.Run(name, func(t *testing.T) {
			data := errorData(t, dispatchJSON(t, d, sess, frame))
			assert.Equal(t, "repo_loading", data["error_code"])
			assert.Equal(t, true, data["retryable"])
			assert.Equal(t, loading, data["repo_path"])
			assert.NotEmpty(t, data["phase"])
			suggestion, _ := data["suggestion"].(string)
			assert.Contains(t, suggestion, "no reconnect needed")
			assert.NotContains(t, suggestion, "gortex track", "a configured repository is never told to track itself")
			assert.NotContains(t, suggestion, "reconcile")
		})
	}

	for name, frame := range map[string]string{
		"index_health": `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"index_health","arguments":{}}}`,
		"facade index": `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"workspace","arguments":{"operation":"index"}}}`,
	} {
		t.Run(name+" answers scoped to the caller", func(t *testing.T) {
			text := toolResultText(t, dispatchJSON(t, d, sess, frame))
			var health map[string]any
			require.NoError(t, json.Unmarshal([]byte(text), &health))
			assert.Equal(t, "loading", health["status"])
			repos, _ := health["repos"].([]any)
			require.Len(t, repos, 1)
			assert.Equal(t, loading, repos[0].(map[string]any)["path"])
			assert.NotContains(t, text, registered, "another repository's identity must not leak to an unbound session")
		})
	}

	t.Run("capabilities answers", func(t *testing.T) {
		parsed := dispatchJSON(t, d, sess, `{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"capabilities","arguments":{}}}`)
		assert.Empty(t, errorCode(parsed), "capabilities must answer while loading: %v", parsed)
	})

	t.Run("track of the configured root is already_configured", func(t *testing.T) {
		path, err := json.Marshal(loading)
		require.NoError(t, err)
		frame := `{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"workspace_admin","arguments":{"operation":"track","path":` + string(path) + `}}}`
		text := toolResultText(t, dispatchJSON(t, d, sess, frame))
		assert.Contains(t, text, "already_configured")
		assert.Contains(t, text, "loading")
	})

	for method, frame := range map[string]string{
		"initialize":      `{"jsonrpc":"2.0","id":30,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`,
		"server/discover": `{"jsonrpc":"2.0","id":32,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"claude-code","version":"2.1.311"},"io.modelcontextprotocol/clientCapabilities":{}}}}`,
	} {
		t.Run(method+" carries loading instructions", func(t *testing.T) {
			parsed := dispatchJSON(t, d, sess, frame)
			result, ok := parsed["result"].(map[string]any)
			require.True(t, ok, "loading discovery must succeed: %v", parsed)
			instr, _ := result["instructions"].(string)
			assert.Contains(t, instr, "LOADING")
			assert.Contains(t, instr, "no reconnect is needed")
			assert.NotContains(t, instr, "INACTIVE")
			assert.NotContains(t, instr, "gortex track")
		})
	}

	t.Run("tools/list answers", func(t *testing.T) {
		parsed := dispatchJSON(t, d, sess, `{"jsonrpc":"2.0","id":31,"method":"tools/list","params":{}}`)
		_, ok := parsed["result"].(map[string]any)
		assert.True(t, ok, "tools/list must answer while loading: %v", parsed)
	})
}

func TestDispatcher_LoadingRepo_WorktreeOfLoadingPrimary(t *testing.T) {
	d, mi, _, _ := loadingDispatcherSetup(t)
	base := t.TempDir()
	primary := filepath.Join(base, "primary")
	worktree := filepath.Join(base, "wt")
	admin := filepath.Join(primary, ".git", "worktrees", "wt")
	require.NoError(t, os.MkdirAll(admin, 0o755))
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, writeFile(filepath.Join(admin, "commondir"), "../..\n"))
	require.NoError(t, writeFile(filepath.Join(worktree, ".git"), "gitdir: "+admin+"\n"))
	mi.ExpectRepos(append(configuredEntries(mi), config.RepoEntry{Path: primary}))

	sess := &daemon.Session{ID: "sess_wt", CWD: worktree}
	data := errorData(t, dispatchJSON(t, d, sess,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_symbols","arguments":{"query":"x"}}}`))
	assert.Equal(t, "repo_loading", data["error_code"])
	assert.Equal(t, primary, data["repo_path"])
}

func configuredEntries(mi *indexer.MultiIndexer) []config.RepoEntry {
	var out []config.RepoEntry
	for _, rec := range mi.RepoLoads() {
		out = append(out, config.RepoEntry{Path: rec.Root})
	}
	return out
}

// A loading cwd whose .gortex.yaml names a workspace a roster claims must get
// repo_loading, not the router arm's ACTIVE admission and its blind scope.
func TestDispatcher_LoadingRepo_WinsOverRouterArm(t *testing.T) {
	d, _, _, loading := loadingDispatcherSetup(t)
	require.NoError(t, writeFile(filepath.Join(loading, ".gortex.yaml"), "workspace: remote-ws\n"))
	cfg := &daemon.ServersConfig{Server: []daemon.ServerEntry{
		{Slug: "self", URL: "unix:///tmp/never-dialed.sock", Default: true},
		{Slug: "remote", URL: "https://remote.example.com", Workspaces: []string{"remote-ws"}},
	}}
	require.NoError(t, cfg.Validate())
	d.SetRouter(daemon.NewRouter(daemon.RouterConfig{
		Servers: cfg, Rosters: daemon.NewWorkspaceRosterCache(0), LocalSlug: "self", Logger: zap.NewNop(),
	}))

	sess := &daemon.Session{ID: "sess_router", CWD: loading}
	data := errorData(t, dispatchJSON(t, d, sess,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_symbols","arguments":{"query":"LoadProbe"}}}`))
	assert.Equal(t, "repo_loading", data["error_code"])
}

func TestDispatcher_FailedRepo_ReturnsRepoUnavailable(t *testing.T) {
	d, mi, _, loading := loadingDispatcherSetup(t)
	mi.StartRepoLoad(loading)
	mi.FinishRepoLoad(loading, errors.New("indexing exploded"))
	sess := &daemon.Session{ID: "sess_failed", CWD: loading}

	data := errorData(t, dispatchJSON(t, d, sess,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_symbols","arguments":{"query":"LoadProbe"}}}`))
	assert.Equal(t, "repo_unavailable", data["error_code"])
	assert.Equal(t, false, data["retryable"])
	assert.Equal(t, "indexing exploded", data["reason"])

	parsed := dispatchJSON(t, d, sess, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"capabilities","arguments":{}}}`)
	assert.Empty(t, errorCode(parsed), "the repair path stays open: %v", parsed)

	parsed = dispatchJSON(t, d, sess, `{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`)
	result, _ := parsed["result"].(map[string]any)
	instr, _ := result["instructions"].(string)
	assert.Contains(t, instr, "could not load")
	assert.Contains(t, instr, "indexing exploded")
}

func TestDispatcher_UnconfiguredPath_StillNotTracked(t *testing.T) {
	d, _, _, _ := loadingDispatcherSetup(t)
	sess := &daemon.Session{ID: "sess_untracked", CWD: t.TempDir()}
	data := errorData(t, dispatchJSON(t, d, sess,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_symbols","arguments":{"query":"x"}}}`))
	assert.Equal(t, "repo_not_tracked", data["error_code"])
}

// A session that connected while its repository was loading starts working in
// place once the repository registers: the same session, no reconnect, told
// once that the repository is ready.
func TestDispatcher_LoadingRepo_ToolsWorkInPlaceAfterRegistration(t *testing.T) {
	d, mi, _, loading := loadingDispatcherSetup(t)
	installRepoLoadTracking(&daemonState{multiIndexer: mi, mcpServer: d.srv}, d)

	var mu sync.Mutex
	var pushed []string
	sess := &daemon.Session{ID: "sess_in_place", CWD: loading}
	d.SessionStarted(sess, func(frame []byte) error {
		mu.Lock()
		pushed = append(pushed, string(frame))
		mu.Unlock()
		return nil
	})
	t.Cleanup(func() { d.SessionEnded(sess) })

	dispatchJSON(t, d, sess, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`)
	search := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"query":"LoadProbe"}}}`
	assert.Equal(t, "repo_loading", errorCode(dispatchJSON(t, d, sess, search)))

	mi.StartRepoLoad(loading)
	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: loading})
	require.NoError(t, err)
	mi.FinishRepoLoad(loading, nil)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, frame := range pushed {
			if strings.Contains(frame, "notifications/message") && strings.Contains(frame, "finished loading") {
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond, "the session must be told its repository is ready")

	parsed := dispatchJSON(t, d, sess, search)
	assert.Empty(t, errorCode(parsed), "the same session must be served after registration: %v", parsed)
	assert.Contains(t, toolResultText(t, parsed), "LoadProbe", "the session's scope must bind to the repository it is in")
}

func TestDispatcher_ColdTrackFailureBecomesUnavailablePromptly(t *testing.T) {
	d, mi, registered, loading := loadingDispatcherSetup(t)
	mi.StartRepoLoad(loading)
	// Use the real cold-track path and a genuine ownership failure, not a
	// synthetic FinishRepoLoad error. The existing owner must stay registered.
	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: loading, Name: filepath.Base(registered)})
	var collision *indexer.PrefixCollisionError
	require.ErrorAs(t, err, &collision)
	failed := time.Now()
	mi.FinishRepoLoad(loading, err)
	sess := &daemon.Session{ID: "cold_failure", CWD: loading}
	data := errorData(t, dispatchJSON(t, d, sess,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_symbols","arguments":{"query":"LoadProbe"}}}`))
	assert.Equal(t, "repo_unavailable", data["error_code"])
	assert.Equal(t, false, data["retryable"])
	assert.Contains(t, data["reason"], "already used")
	assert.Less(t, time.Since(failed), time.Second, "a completed cold-track failure must not remain repo_loading")
	assert.NotNil(t, mi.GetMetadata(filepath.Base(registered)), "failure must not displace the prefix owner")
}
