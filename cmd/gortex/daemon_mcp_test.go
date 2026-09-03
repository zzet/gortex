package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"

	"github.com/zzet/gortex/internal/config"
)

// trackedPathMCPSetup builds a minimal Server + MultiIndexer with one
// repo tracked at `root`. Used by the not-tracked-guard tests so we can
// drive the dispatcher end-to-end without spinning up a real daemon.
func trackedPathMCPSetup(t *testing.T, root string) (*mcpDispatcher, *indexer.MultiIndexer) {
	t.Helper()
	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	cm, err := config.NewConfigManager("")
	require.NoError(t, err)

	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	mi := indexer.NewMultiIndexer(g, reg, idx.Search(), cm, zap.NewNop())

	// Register a repo the dispatcher will recognize. We bypass indexing
	// by stuffing metadata directly — the isCWDTracked check only reads
	// AllMetadata.
	if _, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{
		Path: root,
	}); err != nil {
		t.Fatalf("track test repo: %v", err)
	}

	eng := query.NewEngine(g)
	srv := gortexmcp.NewServer(eng, g, idx, nil, zap.NewNop(), nil, gortexmcp.MultiRepoOptions{
		MultiIndexer:  mi,
		ConfigManager: cm,
	})

	return newMCPDispatcher(srv, mi, zap.NewNop()), mi
}

func TestDispatcher_UntrackedCWD_ReturnsStructuredError(t *testing.T) {
	// Tracked root is a directory the test creates; untracked is a
	// sibling path the dispatcher shouldn't know about.
	tracked := t.TempDir()
	untracked := t.TempDir()

	d, _ := trackedPathMCPSetup(t, tracked)

	sess := &daemon.Session{ID: "sess_x", CWD: untracked}
	// A real tool invocation arrives as tools/call — that's the only frame
	// the method-aware gate refuses for an untracked cwd.
	frame := []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"graph_stats","arguments":{}}}`)

	reply, err := d.Dispatch(context.Background(), sess, frame)
	require.NoError(t, err)
	require.NotNil(t, reply, "untracked cwd must produce a reply, not silence")

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(reply, &parsed))

	errObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok, "response must carry an error object: %v", parsed)

	// Machine-readable data for tool UIs.
	data, ok := errObj["data"].(map[string]any)
	require.True(t, ok, "error.data must be present for client-side handling")
	assert.Equal(t, "repo_not_tracked", data["error_code"])
	assert.Equal(t, untracked, data["path"])
	assert.Contains(t, data["suggestion"], "gortex track")

	// The response id must echo the inbound id so the client can pair
	// it with the in-flight request.
	assert.EqualValues(t, 7, parsed["id"])
}

func TestDispatcher_TrackedCWD_Passes(t *testing.T) {
	tracked := t.TempDir()
	d, _ := trackedPathMCPSetup(t, tracked)

	sess := &daemon.Session{ID: "sess_y", CWD: tracked}
	// The method string doesn't matter for this test — we're proving
	// the dispatcher passes the frame through to MCPServer instead of
	// short-circuiting on the tracked-cwd guard. Whatever mcp-go does
	// with the method (including "method not found" for bogus ones) is
	// evidence that our guard let it through.
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"graph_stats","params":{}}`)

	reply, err := d.Dispatch(context.Background(), sess, frame)
	require.NoError(t, err)
	require.NotNil(t, reply)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(reply, &parsed))

	// The response may carry an mcp-go protocol error (method name isn't
	// the right shape — real tool calls go through tools/call), but it
	// must NOT carry OUR "repo_not_tracked" sentinel.
	if errObj, ok := parsed["error"].(map[string]any); ok {
		if data, ok := errObj["data"].(map[string]any); ok {
			assert.NotEqual(t, "repo_not_tracked", data["error_code"],
				"tracked cwd wrongly rejected by guard: %v", parsed)
		}
	}
}

func TestDispatcher_FacadeHiddenDeferredCallDoesNotPromote(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	tracked := t.TempDir()
	d, _ := trackedPathMCPSetup(t, tracked)
	sess := &daemon.Session{
		ID:       "sess_facade_no_promote",
		CWD:      tracked,
		ToolSpec: "facade-v1",
	}

	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"codex","version":"1.0"}}}`)
	initReply, err := d.Dispatch(context.Background(), sess, initFrame)
	require.NoError(t, err)
	require.NotNil(t, initReply)

	const hidden = "get_architecture"
	_, liveBefore := d.srv.MCPServer().ListTools()[hidden]
	require.False(t, liveBefore, "fixture must start with the legacy tool deferred")

	hiddenFrame := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_architecture","arguments":{}}}`)
	hiddenReply, err := d.Dispatch(context.Background(), sess, hiddenFrame)
	require.NoError(t, err)
	require.NotNil(t, hiddenReply)
	var rejected struct {
		Error any `json:"error"`
	}
	require.NoError(t, json.Unmarshal(hiddenReply, &rejected))
	require.NotNil(t, rejected.Error, "hidden legacy call must be rejected by the facade surface")

	_, liveAfter := d.srv.MCPServer().ListTools()[hidden]
	require.False(t, liveAfter,
		"dispatcher must not globally promote a hidden legacy tool before rejecting the call")

	// The supported facade route reaches the captured cold handler directly
	// and likewise leaves the legacy schema deferred.
	facadeFrame := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"analyze","arguments":{"kind":"architecture"}}}`)
	facadeReply, err := d.Dispatch(context.Background(), sess, facadeFrame)
	require.NoError(t, err)
	require.NotNil(t, facadeReply)
	_, liveAfterFacade := d.srv.MCPServer().ListTools()[hidden]
	require.False(t, liveAfterFacade, "facade dispatch must not promote the captured legacy schema")
}

func TestDispatcher_SubdirectoryOfTrackedRoot_Passes(t *testing.T) {
	tracked := t.TempDir()
	// A nested path inside a tracked root also counts as tracked — an
	// agent opened in repo/internal/auth is still "in" the repo.
	nested := filepath.Join(tracked, "internal", "auth")

	d, _ := trackedPathMCPSetup(t, tracked)

	assert.True(t, d.isCWDTracked(nested),
		"subdirectory of tracked root must be recognized as tracked")
	assert.True(t, d.isCWDTracked(tracked),
		"tracked root itself must be recognized as tracked")
	assert.False(t, d.isCWDTracked(filepath.Dir(tracked)),
		"parent of tracked root must NOT be recognized as tracked")
}

func TestDispatcher_NilMultiIndexer_AllowsEverything(t *testing.T) {
	// Single-repo mode has no multi-indexer. The guard must not reject
	// in that case — otherwise we'd break the embedded stdio path.
	d := newMCPDispatcher(nil, nil, zap.NewNop())
	assert.True(t, d.isCWDTracked("/anywhere"))
	assert.True(t, d.isCWDTracked(""))
}

// TestDispatcher_RemoteRoutableCWD_Passes covers the multi-server
// case: a cwd that is NOT in any locally tracked repo but DOES
// resolve to a workspace declared by a server in the roster must
// pass the cwd guard so the request reaches tryProxyToolCall.
//
// Without this, the four-step priority chain in RouteForCwd is
// dead code from the MCP dispatcher's perspective: any user who
// `cd`s into a remote-served repo gets a repo_not_tracked error
// even though the daemon could have proxied happily.
func TestDispatcher_RemoteRoutableCWD_Passes(t *testing.T) {
	// Local daemon tracks nothing.
	tracked := t.TempDir()
	d, _ := trackedPathMCPSetup(t, tracked)

	// A repo on disk that the daemon does NOT track but whose
	// .gortex.yaml declares a workspace claimed by a roster entry.
	remote := t.TempDir()
	require.NoError(t,
		writeFile(filepath.Join(remote, ".gortex.yaml"), "workspace: remote-ws\n"))

	cfg := &daemon.ServersConfig{
		Server: []daemon.ServerEntry{
			{Slug: "self", URL: "unix:///tmp/never-dialed.sock", Default: true},
			{Slug: "remote", URL: "https://remote.example.com", Workspaces: []string{"remote-ws"}},
		},
	}
	require.NoError(t, cfg.Validate())
	router := daemon.NewRouter(daemon.RouterConfig{
		Servers:   cfg,
		Rosters:   daemon.NewWorkspaceRosterCache(0),
		LocalSlug: "self",
		Logger:    zap.NewNop(),
	})
	d.SetRouter(router)

	// Sanity: cwdReachable agrees the remote-routable cwd passes.
	assert.True(t, d.cwdReachable(context.Background(), remote),
		"cwd inside remote-served repo must be reachable")
	assert.False(t, d.cwdReachable(context.Background(), filepath.Join(t.TempDir(), "nowhere")),
		"cwd with no .gortex.yaml + no roster match must be rejected")

	// End-to-end: Dispatch must NOT short-circuit with repo_not_tracked
	// for the remote-routable cwd. We don't need the proxy to actually
	// succeed (the URL is unreachable in the test) — proving the guard
	// passed is enough; tryProxyToolCall's failure path falls through
	// to the local executor which returns a normal mcp-go reply.
	sess := &daemon.Session{ID: "sess_remote", CWD: remote}
	frame := []byte(`{"jsonrpc":"2.0","id":3,"method":"graph_stats","params":{}}`)
	reply, err := d.Dispatch(context.Background(), sess, frame)
	require.NoError(t, err)
	require.NotNil(t, reply)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(reply, &parsed))
	if errObj, ok := parsed["error"].(map[string]any); ok {
		if data, ok := errObj["data"].(map[string]any); ok {
			assert.NotEqual(t, "repo_not_tracked", data["error_code"],
				"remote-routable cwd wrongly rejected by guard: %v", parsed)
		}
	}
}

// TestDispatcher_LocalWorkspaceUmbrellaCWD_Passes covers the case
// where the cwd is the umbrella directory of a multi-repo workspace:
// it has a `.gortex.yaml` declaring `workspace: <slug>` but is NOT
// itself a tracked repo, AND no server in servers.toml claims that
// workspace. The repos belonging to the workspace are tracked
// individually as siblings/subdirectories.
//
// Without this, agents started in the umbrella get a `repo_not_tracked`
// error even though RouteForCwd resolves Source="config-yaml" from
// the .gortex.yaml — making local-only workspace declarations useless
// from the dispatcher's perspective. The contract is: any "config-yaml"
// resolution is reachable, regardless of whether a server claimed it.
func TestDispatcher_LocalWorkspaceUmbrellaCWD_Passes(t *testing.T) {
	// Local daemon tracks something unrelated — proves the guard does
	// not rely on isCWDTracked seeing the umbrella itself.
	tracked := t.TempDir()
	d, _ := trackedPathMCPSetup(t, tracked)

	// Umbrella directory with .gortex.yaml::workspace = "vio". The
	// umbrella itself is NOT tracked. No server in servers.toml claims
	// "vio" — only some other workspace.
	umbrella := t.TempDir()
	require.NoError(t,
		writeFile(filepath.Join(umbrella, ".gortex.yaml"), "workspace: vio\n"))

	cfg := &daemon.ServersConfig{
		Server: []daemon.ServerEntry{
			{Slug: "self", URL: "unix:///tmp/never.sock", Workspaces: []string{"gortex"}, Default: true},
		},
	}
	require.NoError(t, cfg.Validate())
	router := daemon.NewRouter(daemon.RouterConfig{
		Servers:   cfg,
		Rosters:   daemon.NewWorkspaceRosterCache(0),
		LocalSlug: "self",
		Logger:    zap.NewNop(),
	})
	d.SetRouter(router)

	assert.True(t, d.cwdReachable(context.Background(), umbrella),
		"workspace-umbrella cwd must be reachable when .gortex.yaml declares a workspace, even when no server claims it")

	sess := &daemon.Session{ID: "sess_umbrella", CWD: umbrella}
	// Must be a tools/call: the gate only refuses that method, so a
	// frame with any other method can never exercise it.
	frame := []byte(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"graph_stats","arguments":{}}}`)
	reply, err := d.Dispatch(context.Background(), sess, frame)
	require.NoError(t, err)
	require.NotNil(t, reply)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(reply, &parsed))
	if errObj, ok := parsed["error"].(map[string]any); ok {
		if data, ok := errObj["data"].(map[string]any); ok {
			assert.NotEqual(t, "repo_not_tracked", data["error_code"],
				"workspace-umbrella cwd wrongly rejected by guard: %v", parsed)
		}
	}
}

// TestDispatcher_UnreachableCWD_StillRejected guards against the
// fix becoming too permissive. A cwd that's neither locally tracked
// nor matches any workspace declared in the roster (and has no
// .gortex.yaml) must still get the repo_not_tracked error.
func TestDispatcher_UnreachableCWD_StillRejected(t *testing.T) {
	tracked := t.TempDir()
	d, _ := trackedPathMCPSetup(t, tracked)

	cfg := &daemon.ServersConfig{
		Server: []daemon.ServerEntry{
			{Slug: "self", URL: "unix:///tmp/never.sock", Default: true},
		},
	}
	require.NoError(t, cfg.Validate())
	router := daemon.NewRouter(daemon.RouterConfig{
		Servers:   cfg,
		Rosters:   daemon.NewWorkspaceRosterCache(0),
		LocalSlug: "self",
		Logger:    zap.NewNop(),
	})
	d.SetRouter(router)

	stranger := t.TempDir() // no .gortex.yaml, not tracked, no roster match
	assert.False(t, d.cwdReachable(context.Background(), stranger))

	sess := &daemon.Session{ID: "sess_stranger", CWD: stranger}
	frame := []byte(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"graph_stats","arguments":{}}}`)
	reply, err := d.Dispatch(context.Background(), sess, frame)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(reply, &parsed))
	errObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok, "stranger cwd must still be rejected: %v", parsed)
	data, ok := errObj["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "repo_not_tracked", data["error_code"])
}

// TestDispatcher_WorkspaceRootCWD_Passes covers the reverse-containment
// case: the session cwd is not inside any tracked repo but is a PARENT
// of one (an agent opened at the workspace root above its repos, e.g.
// `<root>` over `<root>/projects/<repo>`). The guard must let the frame
// through — ScopeForCWD binds such a session to the contained repo's
// workspace, so refusing here contradicts the scope the session would
// actually get. A parent whose repos span different workspaces stays
// rejected: sessionScope cannot express that union, and passing the
// gate would trade the actionable error for silently empty results.
func TestDispatcher_WorkspaceRootCWD_Passes(t *testing.T) {
	parent := t.TempDir()
	app := filepath.Join(parent, "projects", "app")
	require.NoError(t, os.MkdirAll(app, 0o755))

	d, mi := trackedPathMCPSetup(t, app)

	assert.True(t, d.cwdReachable(context.Background(), parent),
		"workspace root above one tracked repo must be reachable")

	sess := &daemon.Session{ID: "sess_wsroot", CWD: parent}
	frame := []byte(`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"graph_stats","arguments":{}}}`)
	reply, err := d.Dispatch(context.Background(), sess, frame)
	require.NoError(t, err)
	require.NotNil(t, reply)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(reply, &parsed))
	if errObj, ok := parsed["error"].(map[string]any); ok {
		if data, ok := errObj["data"].(map[string]any); ok {
			assert.NotEqual(t, "repo_not_tracked", data["error_code"],
				"workspace-root cwd wrongly rejected by guard: %v", parsed)
		}
	}

	// Track a second repo under the same parent. Each repo is its own
	// singleton workspace, so the parent now spans two — the shape
	// ScopeForCWD fails closed on. It must STILL be reachable: the
	// session binds to the contained repo set instead of to a slug
	// (gortexhq/gortex#418). Two unrelated repos under one folder is
	// the ordinary case, and refusing it left the session with no
	// working call and a remedy that would have re-indexed both repos
	// a second time under the parent.
	other := filepath.Join(parent, "projects", "other")
	require.NoError(t, os.MkdirAll(other, 0o755))
	_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: other})
	require.NoError(t, err)
	_, _, _, singleSlug := mi.ScopeForCWD(parent)
	require.False(t, singleSlug, "precondition: the parent must span two workspaces")
	assert.True(t, d.cwdReachable(context.Background(), parent),
		"parent containing repos in different workspaces must bind to the contained repo set")

	sess = &daemon.Session{ID: "sess_wsroot_mixed", CWD: parent}
	reply, err = d.Dispatch(context.Background(), sess, frame)
	require.NoError(t, err)
	require.NotNil(t, reply)
	parsed = nil
	require.NoError(t, json.Unmarshal(reply, &parsed))
	if errObj, ok := parsed["error"].(map[string]any); ok {
		if data, ok := errObj["data"].(map[string]any); ok {
			assert.NotEqual(t, "repo_not_tracked", data["error_code"],
				"mixed-workspace parent wrongly rejected: %v", parsed)
		}
	}
}

// TestDispatcher_GateMatchesSessionScope pins the invariant the gate's own
// doc comment asserts: cwdReachable admits exactly the cwds the MCP server
// can bind to a non-sentinel scope. The two resolutions live in different
// packages and drifted apart before — a cwd the gate admitted but the scope
// blanked produced silent zeros, which is the failure mode the loud
// repo_not_tracked error exists to prevent.
func TestDispatcher_GateMatchesSessionScope(t *testing.T) {
	parent := t.TempDir()
	repoA := filepath.Join(parent, "a")
	repoB := filepath.Join(parent, "b")
	require.NoError(t, os.MkdirAll(repoA, 0o755))
	require.NoError(t, os.MkdirAll(repoB, 0o755))

	d, mi := trackedPathMCPSetup(t, repoA)
	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: repoB})
	require.NoError(t, err)

	stranger := t.TempDir()

	cases := []struct {
		name string
		cwd  string
		want bool
	}{
		{"inside a tracked repo", repoA, true},
		{"subdirectory of a tracked repo", filepath.Join(repoA, "internal", "deep"), true},
		{"parent containing two tracked repos", parent, true},
		{"unrelated directory", stranger, false},
	}
	aliasParent := filepath.Join(t.TempDir(), "parent-alias")
	if err := os.Symlink(parent, aliasParent); err == nil {
		cases = append(cases,
			struct {
				name string
				cwd  string
				want bool
			}{"canonical alias of a tracked repo", filepath.Join(aliasParent, "a"), true},
		)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := d.cwdReachable(context.Background(), tc.cwd)
			assert.Equal(t, tc.want, gate, "gate verdict")

			_, _, _, inside := mi.ScopeForCWD(tc.cwd)
			_, _, contains := mi.ContainedReposScope(tc.cwd)
			assert.Equal(t, gate, inside || contains,
				"the gate and the session-scope resolution must agree for %s", tc.cwd)
		})
	}
}

// TestDispatcher_UntrackedHandshake_SurvivesWithInactiveVariant proves the
// F4 contract: an MCP session opened in a cwd that no tracked repo covers must
// still complete its handshake. initialize flows through and the response
// carries the inactive-instructions variant (telling the agent to run `gortex
// track <cwd>`); tools/list preserves the stable facade so clients can discover
// capabilities and the track operation before a graph exists. Graph-dependent
// tools/call requests are still refused with the structured repo_not_tracked
// error. This beats a silent empty-list — the agent gets an actionable
// affordance, not a dead connection.
func TestDispatcher_UntrackedHandshake_SurvivesWithInactiveVariant(t *testing.T) {
	tracked := t.TempDir()
	untracked := t.TempDir() // a sibling the dispatcher does not cover

	d, _ := trackedPathMCPSetup(t, tracked)
	sess := &daemon.Session{ID: "sess_handshake", CWD: untracked}

	dispatch := func(t *testing.T, frame string) map[string]any {
		t.Helper()
		reply, err := d.Dispatch(context.Background(), sess, []byte(frame))
		require.NoError(t, err)
		require.NotNil(t, reply, "handshake frame must produce a reply")
		var parsed map[string]any
		require.NoError(t, json.Unmarshal(reply, &parsed))
		return parsed
	}

	t.Run("initialize_flows_through_with_inactive_instructions", func(t *testing.T) {
		parsed := dispatch(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`)

		// The handshake SUCCEEDS — no error object, the connection lives.
		_, isErr := parsed["error"]
		assert.False(t, isErr, "untracked initialize must not error: %v", parsed)

		result, ok := parsed["result"].(map[string]any)
		require.True(t, ok, "initialize must return a result object: %v", parsed)

		instr, ok := result["instructions"].(string)
		require.True(t, ok, "initialize result must carry instructions")
		assert.Contains(t, instr, "INACTIVE", "instructions must flag the inactive state")
		assert.Contains(t, instr, "gortex track "+untracked,
			"instructions must carry the cwd-specific track affordance")
	})

	t.Run("tools_list_preserves_facade_surface", func(t *testing.T) {
		parsed := dispatch(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)

		result, ok := parsed["result"].(map[string]any)
		require.True(t, ok, "tools/list must return a result object: %v", parsed)
		tools, ok := result["tools"].([]any)
		require.True(t, ok, "tools/list result must carry a tools array")
		names := make(map[string]bool, len(tools))
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			require.True(t, ok, "tool entry must be an object: %v", raw)
			name, _ := tool["name"].(string)
			names[name] = true
		}
		assert.True(t, names["capabilities"], "bootstrap discovery must remain visible")
		assert.True(t, names["workspace_admin"], "workspace_admin.track must remain discoverable")
		assert.True(t, names["explore"], "the stable facade must not drift when the cwd is untracked")
		assert.False(t, names["graph_stats"], "legacy tools must remain hidden")
	})

	t.Run("tools_call_still_refused", func(t *testing.T) {
		parsed := dispatch(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"explore","arguments":{"task":"locate startup"}}}`)

		errObj, ok := parsed["error"].(map[string]any)
		require.True(t, ok, "untracked tools/call must still be refused: %v", parsed)
		data, ok := errObj["data"].(map[string]any)
		require.True(t, ok, "refusal must carry machine-readable data")
		assert.Equal(t, "repo_not_tracked", data["error_code"])
		assert.Equal(t, untracked, data["path"])
	})
}

// writeFile is a tiny helper to keep test setup readable.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
