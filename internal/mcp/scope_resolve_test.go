package mcp

// Tests for the intent-based scope resolver added in the scope-intent-defaults
// change (Layer A + Layer B).  See internal/query/scope_allows_test.go for
// the lower-level ScopeAllows node-predicate matrix.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/trigram"
)

// ─── helpers ───────────────────────────────────────────────────────────────

// wsRepoFull writes a minimal Go repo whose .gortex.yaml declares
// both a workspace and an optional project slug.  When project is "",
// the project: line is omitted (the repo falls back to its prefix as
// project slug).
func wsRepoFull(t *testing.T, name, workspace, project, symbol string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	yaml := fmt.Sprintf("workspace: %s\n", workspace)
	if project != "" {
		yaml += fmt.Sprintf("project: %s\n", project)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gortex.yaml"), []byte(yaml), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc "+symbol+"() {}\n"), 0o644))
	return dir
}

// sharedWSOptions holds the server + repo roots for a two-repo single-workspace
// fixture where both repos share the same project slug "backend".
type sharedWSOptions struct {
	srv          *Server
	repoA, repoB string // absolute paths (= session CWD anchors)
}

// newSharedWorkspaceServer creates two repos in workspace "shared-ws",
// project "backend".  It returns a *Server whose scopeIntentDefaults
// is controlled by the flagOn argument.
func newSharedWorkspaceServer(t *testing.T, flagOn bool) sharedWSOptions {
	t.Helper()

	repoA := wsRepoFull(t, "repo-a", "shared-ws", "backend", "RepoAThing")
	repoB := wsRepoFull(t, "repo-b", "shared-ws", "backend", "RepoBThing")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a", Project: "backend"},
			{Path: repoB, Name: "repo-b", Project: "backend"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	reg := testRegistry()
	bm := search.NewNull()
	mi := indexer.NewMultiIndexer(g, reg, bm, cm, zap.NewNop())
	_, err = mi.IndexScoped("", "")
	require.NoError(t, err)

	eng := query.NewEngine(g)
	eng.SetSearch(bm)

	flagVal := flagOn
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		MultiIndexer:        mi,
		ConfigManager:       cm,
		ScopeIntentDefaults: &flagVal,
	})
	return sharedWSOptions{srv: srv, repoA: repoA, repoB: repoB}
}

func newSplitProjectWorkspaceServer(t *testing.T, flagOn bool) sharedWSOptions {
	t.Helper()

	repoA := wsRepoFull(t, "repo-a", "shared-ws", "frontend", "RepoAThing")
	repoB := wsRepoFull(t, "repo-b", "shared-ws", "backend", "RepoBThing")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a", Project: "frontend"},
			{Path: repoB, Name: "repo-b", Project: "backend"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	reg := testRegistry()
	bm := search.NewNull()
	mi := indexer.NewMultiIndexer(g, reg, bm, cm, zap.NewNop())
	_, err = mi.IndexScoped("", "")
	require.NoError(t, err)

	eng := query.NewEngine(g)
	eng.SetSearch(bm)

	flagVal := flagOn
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		MultiIndexer:        mi,
		ConfigManager:       cm,
		ScopeIntentDefaults: &flagVal,
	})
	return sharedWSOptions{srv: srv, repoA: repoA, repoB: repoB}
}

// makeReq builds a minimal CallToolRequest carrying the given
// argument map, with the tool name set.
func makeReq(toolName string, args map[string]any) mcplib.CallToolRequest {
	req := mcplib.CallToolRequest{}
	req.Params.Name = toolName
	if args == nil {
		args = map[string]any{}
	}
	req.Params.Arguments = args
	return req
}

// ─── toolIntentForName table ────────────────────────────────────────────────

func TestToolIntentForName_Locate(t *testing.T) {
	locate := []string{"search_symbols", "search_text", "find_files"}
	for _, name := range locate {
		if got := toolIntentForName(name); got != IntentLocate {
			t.Errorf("toolIntentForName(%q) = %v, want IntentLocate", name, got)
		}
	}
}

func TestToolIntentForName_Reach(t *testing.T) {
	reach := []string{
		"find_usages", "get_callers", "get_call_chain",
		"get_dependencies", "get_dependents",
		"find_implementations", "find_overrides",
		"contracts",
	}
	for _, name := range reach {
		if got := toolIntentForName(name); got != IntentReach {
			t.Errorf("toolIntentForName(%q) = %v, want IntentReach", name, got)
		}
	}
}

func TestToolIntentForName_Analyze(t *testing.T) {
	analyze := []string{"analyze", "review", "sast", "hotspots", "dead_code"}
	for _, name := range analyze {
		if got := toolIntentForName(name); got != IntentAnalyze {
			t.Errorf("toolIntentForName(%q) = %v, want IntentAnalyze", name, got)
		}
	}
}

func TestToolIntentForName_Unknown_DefaultsToAnalyze(t *testing.T) {
	// Any unregistered tool name must fall through to IntentAnalyze —
	// the safest default (workspace-breadth, not narrowed).
	for _, name := range []string{"unknown_tool", "", "get_symbol", "edit_file"} {
		got := toolIntentForName(name)
		if got != IntentAnalyze {
			t.Errorf("toolIntentForName(%q) = %v, want IntentAnalyze (safe default)", name, got)
		}
	}
}

// ─── resolveScope — intent defaults ON (Layer B) ────────────────────────────

// TestResolveScope_IntentDefaultsON_Locate_ReturnsHomeRepo verifies that
// a bound session with intent=locate and no explicit narrowing yields a
// RepoAllow set containing ONLY the home repo.
func TestResolveScope_IntentDefaultsON_Locate_ReturnsHomeRepo(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", fx.repoA) // session home = repo-a

	scope, errRes := fx.srv.resolveScope(ctx, makeReq("search_symbols", nil), IntentLocate)
	require.Nil(t, errRes, "resolveScope must not error for a normal locate request")

	assert.Equal(t, map[string]bool{"repo-a": true}, scope.RepoAllow,
		"locate intent default must narrow to the home repo only")
	assert.Equal(t, "repo:repo-a", scope.Applied)
}

// TestResolveScope_IntentDefaultsON_Reach_ReturnsWorkspace verifies that
// reach-intent tools default to workspace breadth (no repo narrowing).
func TestResolveScope_IntentDefaultsON_Reach_ReturnsWorkspace(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", fx.repoA)

	scope, errRes := fx.srv.resolveScope(ctx, makeReq("find_usages", nil), IntentReach)
	require.Nil(t, errRes)

	assert.Nil(t, scope.RepoAllow, "reach intent must not narrow to a repo")
	assert.Equal(t, "workspace", scope.Applied)
}

// TestResolveScope_IntentDefaultsON_Analyze_ReturnsWorkspace verifies that
// analyze-intent tools also default to workspace breadth.
func TestResolveScope_IntentDefaultsON_Analyze_ReturnsWorkspace(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", fx.repoA)

	scope, errRes := fx.srv.resolveScope(ctx, makeReq("analyze", nil), IntentAnalyze)
	require.Nil(t, errRes)

	assert.Nil(t, scope.RepoAllow, "analyze intent must not narrow to a repo")
	assert.Equal(t, "workspace", scope.Applied)
}

// TestResolveScope_IntentDefaultsON_RepoStar_WidensLocate verifies that
// an explicit repo:"*" clears the home-repo narrowing produced by the
// locate default.
func TestResolveScope_IntentDefaultsON_RepoStar_WidensLocate(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", fx.repoA)

	req := makeReq("search_symbols", map[string]any{"repo": "*"})
	scope, errRes := fx.srv.resolveScope(ctx, req, IntentLocate)
	require.Nil(t, errRes)

	assert.Nil(t, scope.RepoAllow, "repo:* sentinel must clear the repo narrowing")
	assert.Equal(t, "workspace", scope.Applied,
		"widened locate should be reported as workspace")
}

// TestResolveScope_IntentDefaultsON_ExplicitProject_NarrowsLocate verifies
// that an explicit project: arg is honoured even under intent defaults ON.
func TestResolveScope_IntentDefaultsON_ExplicitProject_NarrowsLocate(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", fx.repoA)

	req := makeReq("search_symbols", map[string]any{"project": "backend"})
	scope, errRes := fx.srv.resolveScope(ctx, req, IntentLocate)
	require.Nil(t, errRes)

	assert.Equal(t, "backend", scope.ProjectID)
	assert.Equal(t, map[string]bool{"repo-a": true, "repo-b": true}, scope.RepoAllow,
		"explicit project must resolve to a concrete repo allow-set")
	assert.Equal(t, "project:backend", scope.Applied)
}

func TestResolveScope_ExplicitUnknownProject_Errors(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", fx.repoA)

	req := makeReq("search_symbols", map[string]any{"project": "does-not-exist"})
	_, errRes := fx.srv.resolveScope(ctx, req, IntentLocate)
	require.NotNil(t, errRes, "explicit unknown project must not degrade to an unfiltered scope")
	assert.Contains(t, errResultBody(errRes), "does-not-exist")
}

func TestResolveScope_UnboundExplicitProject_UsesRepoAllow(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true)

	req := makeReq("search_symbols", map[string]any{"project": "backend"})
	scope, errRes := fx.srv.resolveScope(context.Background(), req, IntentLocate)
	require.Nil(t, errRes)

	assert.Equal(t, "backend", scope.ProjectID)
	assert.Equal(t, map[string]bool{"repo-a": true, "repo-b": true}, scope.RepoAllow)
	assert.Equal(t, "project:backend", scope.Applied)
}

// ─── resolveScope — intent defaults OFF (Layer-A / today's behavior) ────────

// TestResolveScope_IntentDefaultsOFF_Locate_StaysWorkspace verifies that
// with the flag off, a locate call in a bound session returns ALL workspace
// repos — today's project-or-workspace behavior preserved byte-for-byte.
func TestResolveScope_IntentDefaultsOFF_Locate_StaysWorkspace(t *testing.T) {
	fx := newSharedWorkspaceServer(t, false) // flag OFF
	ctx := sessionCtx("s-a", fx.repoA)

	scope, errRes := fx.srv.resolveScope(ctx, makeReq("search_symbols", nil), IntentLocate)
	require.Nil(t, errRes)

	// With the flag off the resolver returns ALL repos in the workspace
	// (the project default = both repos share project "backend", so no
	// narrowing beyond the workspace boundary).
	wantRepos := map[string]bool{"repo-a": true, "repo-b": true}
	assert.Equal(t, wantRepos, scope.RepoAllow,
		"flag-off locate must not narrow to home repo; should keep workspace repos")
}

// TestResolveScope_IntentDefaultsOFF_Reach_StaysWorkspace verifies that
// reach-intent behavior is unchanged when the flag is off.
func TestResolveScope_IntentDefaultsOFF_Reach_StaysWorkspace(t *testing.T) {
	fx := newSharedWorkspaceServer(t, false)
	ctx := sessionCtx("s-a", fx.repoA)

	scope, errRes := fx.srv.resolveScope(ctx, makeReq("find_usages", nil), IntentReach)
	require.Nil(t, errRes)

	// Flag-off: same as flag-on for reach — both keep workspace repos.
	wantRepos := map[string]bool{"repo-a": true, "repo-b": true}
	assert.Equal(t, wantRepos, scope.RepoAllow,
		"flag-off reach must also stay at workspace breadth")
}

// ─── resolveScope — unbound-session fallback ────────────────────────────────

// TestResolveScope_UnboundSession_Locate_NoNarrowing verifies that when
// there is no session CWD (unbound session, no home repo), a locate intent
// falls back to workspace-breadth rather than erroring or incorrectly
// narrowing.
func TestResolveScope_UnboundSession_Locate_NoNarrowing(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true) // flag ON
	// context.Background() = unbound: no session CWD, no home repo
	ctx := context.Background()

	scope, errRes := fx.srv.resolveScope(ctx, makeReq("search_symbols", nil), IntentLocate)
	require.Nil(t, errRes, "unbound session locate must not error")

	// Unbound session: no home repo → applyIntentDefault returns workspace.
	assert.Nil(t, scope.RepoAllow,
		"locate with no home repo must fall back to workspace (no narrowing)")
	assert.Equal(t, "workspace", scope.Applied)
}

// ─── resolveScope — cross-workspace rejection ────────────────────────────────

// TestResolveScope_CrossWorkspace_Rejected verifies that a `workspace:` arg
// naming a different workspace than the session's is a hard error — the
// workspace boundary is never escapable.
func TestResolveScope_CrossWorkspace_Rejected(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", fx.repoA) // session workspace = "shared-ws"

	req := makeReq("search_symbols", map[string]any{"workspace": "other-ws"})
	_, errRes := fx.srv.resolveScope(ctx, req, IntentLocate)
	require.NotNil(t, errRes, "cross-workspace arg must produce an error result")

	// The error text must mention "cross-workspace" so the agent understands why.
	body := errResultBody(errRes)
	assert.Contains(t, body, "cross-workspace")
}

// ─── resolveScope — git-diff scope values are stripped ──────────────────────

// TestResolveScope_GitDiffScope_Cleared verifies that values in
// gitDiffScopes ("staged", "unstaged", "all", "compare") do NOT trigger
// the saved-scope lookup — they are stripped early so diff-family tools
// can reuse the `scope` arg for a different purpose.
func TestResolveScope_GitDiffScope_Cleared(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", fx.repoA)

	for _, diffScope := range []string{"staged", "unstaged", "all", "compare"} {
		req := makeReq("search_symbols", map[string]any{"scope": diffScope})
		// Must NOT error with "unknown scope" — the value is cleared before
		// the saved-scope lookup runs.
		_, errRes := fx.srv.resolveScope(ctx, req, IntentLocate)
		assert.Nilf(t, errRes,
			"git-diff scope value %q must be stripped, not looked up as a saved scope", diffScope)
	}
}

// ─── resolveScope — explicit repo / project intersect-and-clamp ─────────────

// TestResolveScope_ExplicitRepo_Clamps verifies that an explicit repo: arg
// inside the workspace is honoured and clamped to the session workspace.
func TestResolveScope_ExplicitRepo_Clamps(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", fx.repoA)

	req := makeReq("search_symbols", map[string]any{"repo": "repo-b"})
	scope, errRes := fx.srv.resolveScope(ctx, req, IntentLocate)
	require.Nil(t, errRes)

	assert.Equal(t, map[string]bool{"repo-b": true}, scope.RepoAllow,
		"explicit repo arg must override the intent default")
}

// TestResolveScope_ExplicitRepo_OutsideWorkspace_Rejected verifies that a
// repo: arg not in the session's workspace is rejected as a cross-workspace
// escape attempt.
func TestResolveScope_ExplicitRepo_OutsideWorkspace_Rejected(t *testing.T) {
	// Use the isolation server which has repo-a in "alpha" and repo-b in "beta".
	srv, repoA, _ := newIsolationServer(t)
	ctx := sessionCtx("s-alpha", repoA) // session workspace = "alpha"

	// "repo-b" is in workspace "beta" — must be rejected.
	req := makeReq("search_symbols", map[string]any{"repo": "repo-b"})
	_, errRes := srv.resolveScope(ctx, req, IntentLocate)
	require.NotNil(t, errRes, "repo from another workspace must be rejected")
}

// ─── workspace hard-isolation invariant ─────────────────────────────────────

// TestWorkspaceHardBoundary_ScopedNodesNeverNarrowToRepo asserts that
// scopedNodes and nodeInSessionScope enforce workspace isolation only —
// they are NOT affected by the intent-defaults repo narrowing.
// This proves the hard isolation boundary is never narrowed by Layer B.
func TestWorkspaceHardBoundary_ScopedNodesNeverNarrowToRepo(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true) // flag ON

	// Session bound to repo-a (workspace "shared-ws").
	ctxA := sessionCtx("s-a", fx.repoA)

	// scopedNodes must return nodes from BOTH repos in the workspace —
	// not just repo-a — because the hard boundary is workspace-shaped,
	// not repo-shaped.
	nodes := fx.srv.scopedNodes(ctxA)
	require.NotEmpty(t, nodes, "scopedNodes must not be empty for a bound session")

	hasRepoA, hasRepoB := false, false
	for _, n := range nodes {
		if n.RepoPrefix == "repo-a" {
			hasRepoA = true
		}
		if n.RepoPrefix == "repo-b" {
			hasRepoB = true
		}
	}
	assert.True(t, hasRepoA, "scopedNodes must include repo-a nodes")
	assert.True(t, hasRepoB,
		"scopedNodes must include repo-b nodes — workspace hard boundary is NOT narrowed to home repo")

	// nodeInSessionScope must see both repos' nodes.
	var repoBNode *graph.Node
	for _, n := range nodes {
		if n.Name == "RepoBThing" {
			repoBNode = n
			break
		}
	}
	require.NotNil(t, repoBNode, "RepoBThing must be in the workspace graph")
	assert.True(t, fx.srv.nodeInSessionScope(ctxA, repoBNode),
		"nodeInSessionScope must NOT narrow to repo — it must pass all nodes in the workspace")
}

// ─── bound-session integration: the proposal scenario ───────────────────────

// TestIntentDefaults_SearchSymbols_NarrowsToHomeRepo tests the core
// proposal scenario: with the flag ON, search_symbols from a session
// bound to repo-a returns ONLY repo-a symbols — even though repo-b is in
// the same workspace and project.
func TestIntentDefaults_SearchSymbols_NarrowsToHomeRepo(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true) // flag ON
	ctx := sessionCtx("s-a", fx.repoA)

	req := makeReq("search_symbols", map[string]any{"query": "Thing"})
	res, err := fx.srv.handleSearchSymbols(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, "search_symbols must not error")

	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	ids := resultIDs(resp)
	require.NotEmpty(t, ids, "search_symbols must find symbols in repo-a")

	for _, id := range ids {
		assert.NotContains(t, id, "repo-b",
			"flag-ON search_symbols must not return repo-b results: %s", id)
	}
	// Sanity: repo-a result is present.
	foundA := false
	for _, id := range ids {
		if containsStr(id, "RepoAThing") {
			foundA = true
		}
	}
	assert.True(t, foundA, "RepoAThing from repo-a must be in results")
}

func TestSearchSymbols_InlineRepoClauseOverridesLocateDefault(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", fx.repoA)

	req := makeReq("search_symbols", map[string]any{"query": "repo:repo-b Thing"})
	res, err := fx.srv.handleSearchSymbols(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	ids := resultIDs(resp)
	require.NotEmpty(t, ids)

	foundA, foundB := false, false
	for _, id := range ids {
		if containsStr(id, "RepoAThing") {
			foundA = true
		}
		if containsStr(id, "RepoBThing") {
			foundB = true
		}
	}
	assert.False(t, foundA, "inline repo:repo-b must not fall back to the home repo")
	assert.True(t, foundB, "inline repo:repo-b must search repo-b")
	require.NotNil(t, res.Meta)
	assert.Equal(t, "repo:repo-b", res.Meta.AdditionalFields["scope_applied"])
}

func TestSearchSymbols_InlineProjectClauseOverridesLocateDefault(t *testing.T) {
	fx := newSplitProjectWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", fx.repoA)

	req := makeReq("search_symbols", map[string]any{"query": "project:backend Thing"})
	res, err := fx.srv.handleSearchSymbols(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	ids := resultIDs(resp)
	require.NotEmpty(t, ids)

	foundA, foundB := false, false
	for _, id := range ids {
		if containsStr(id, "RepoAThing") {
			foundA = true
		}
		if containsStr(id, "RepoBThing") {
			foundB = true
		}
	}
	assert.False(t, foundA, "inline project:backend must not stay clamped to the frontend home repo")
	assert.True(t, foundB, "inline project:backend must search the backend project")
	require.NotNil(t, res.Meta)
	assert.Equal(t, "project:backend", res.Meta.AdditionalFields["scope_applied"])
}

// TestIntentDefaults_FindUsages_SpansWorkspace verifies that find_usages
// (IntentReach) is NOT narrowed to the home repo — it spans the whole
// workspace so cross-repo usage of shared-lib symbols is visible.
func TestIntentDefaults_FindUsages_SpansWorkspace(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true) // flag ON
	ctx := sessionCtx("s-a", fx.repoA)

	// resolveScope for find_usages must give workspace breadth.
	scope, errRes := fx.srv.resolveScope(ctx, makeReq("find_usages", nil), IntentReach)
	require.Nil(t, errRes)
	assert.Nil(t, scope.RepoAllow,
		"find_usages (reach) must span whole workspace, not narrow to home repo")
}

// TestIntentDefaults_RepoStarWidens verifies that repo:"*" on a locate
// tool widens back to the whole workspace.
func TestIntentDefaults_RepoStarWidens(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true) // flag ON
	ctx := sessionCtx("s-a", fx.repoA)

	req := makeReq("search_symbols", map[string]any{"query": "Thing", "repo": "*"})
	res, err := fx.srv.handleSearchSymbols(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	ids := resultIDs(resp)
	require.NotEmpty(t, ids)

	// After widening, results from BOTH repos should be present.
	foundA, foundB := false, false
	for _, id := range ids {
		if containsStr(id, "RepoAThing") {
			foundA = true
		}
		if containsStr(id, "RepoBThing") {
			foundB = true
		}
	}
	assert.True(t, foundA, "widened search must include repo-a results")
	assert.True(t, foundB, "widened search must include repo-b results")
}

func TestSearchSymbols_InlineRepoStarWidens(t *testing.T) {
	fx := newSharedWorkspaceServer(t, true)
	ctx := sessionCtx("s-a", fx.repoA)

	req := makeReq("search_symbols", map[string]any{"query": "repo:* Thing"})
	res, err := fx.srv.handleSearchSymbols(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	ids := resultIDs(resp)
	require.NotEmpty(t, ids)

	foundA, foundB := false, false
	for _, id := range ids {
		if containsStr(id, "RepoAThing") {
			foundA = true
		}
		if containsStr(id, "RepoBThing") {
			foundB = true
		}
	}
	assert.True(t, foundA, "inline repo:* must keep home-repo results")
	assert.True(t, foundB, "inline repo:* must widen to workspace results")
	require.NotNil(t, res.Meta)
	assert.Equal(t, "workspace", res.Meta.AdditionalFields["scope_applied"])
	require.Nil(t, resp["filters_relaxed"], "repo:* is a scope sentinel, not a fuzzy post-filter")
}

// TestIntentDefaults_FlagOff_ReproducesTodaysBehavior verifies that with
// the flag OFF, search_symbols returns symbols from ALL repos in the
// workspace/project — matching today's project-scoped behavior.
func TestIntentDefaults_FlagOff_ReproducesTodaysBehavior(t *testing.T) {
	fx := newSharedWorkspaceServer(t, false) // flag OFF
	ctx := sessionCtx("s-a", fx.repoA)

	req := makeReq("search_symbols", map[string]any{"query": "Thing"})
	res, err := fx.srv.handleSearchSymbols(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	ids := resultIDs(resp)
	require.NotEmpty(t, ids, "flag-off search must find symbols")

	// With the flag off, the resolver returns ALL workspace repos.
	// Both repo-a and repo-b results must be present.
	foundA, foundB := false, false
	for _, id := range ids {
		if containsStr(id, "RepoAThing") {
			foundA = true
		}
		if containsStr(id, "RepoBThing") {
			foundB = true
		}
	}
	assert.True(t, foundA, "flag-off: repo-a symbols must be found")
	assert.True(t, foundB, "flag-off: repo-b symbols must also be found (no home-repo narrowing)")
}

// ─── misc helper ────────────────────────────────────────────────────────────

// containsStr reports whether sub is a substring of s.
// Named to avoid collision with tools_dataflow_test.go's contains().
func containsStr(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ─── single-repo (unprefixed) regression ────────────────────────────────────

// newLoneRepoServer builds the fresh-install shape: a daemon tracking
// exactly ONE repo, no .gortex.yaml. Single-repo indexing mints
// UNPREFIXED node IDs (RepoMetadata.Unprefixed) while the registry —
// and therefore ScopeForCWD / every RepoAllow set — keys the repo by
// its config name.
func newLoneRepoServer(t *testing.T, flagOn bool) (*Server, string) {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "lone-repo")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc LoneThing() {}\n"), 0o644))

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{{Path: dir, Name: "lone-repo"}},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	reg := testRegistry()
	bm := search.NewNull()
	mi := indexer.NewMultiIndexer(g, reg, bm, cm, zap.NewNop())
	_, err = mi.IndexScoped("", "")
	require.NoError(t, err)

	eng := query.NewEngine(g)
	eng.SetSearch(bm)

	flagVal := flagOn
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		MultiIndexer:        mi,
		ConfigManager:       cm,
		ScopeIntentDefaults: &flagVal,
	})
	return srv, dir
}

// TestIntentDefaults_SearchSymbols_LoneRepo is the fresh-install
// zero-rows regression: with intent defaults ON, a session bound inside the
// lone tracked repo runs search_symbols and the Locate default narrows
// RepoAllow to the registry prefix ("lone-repo"). The narrow must not hide
// the only repo's own symbols.
//
// The narrow used to be vacuous — every node carried RepoPrefix == "" while
// RepoAllow was keyed on "lone-repo", so the filter could only match by the
// prefix-less carve-out. Now the node's prefix IS the allowed key, so the
// narrow matches directly.
func TestIntentDefaults_SearchSymbols_LoneRepo(t *testing.T) {
	srv, dir := newLoneRepoServer(t, true)
	ctx := sessionCtx("s-lone", dir)

	// Precondition: a lone repo's nodes carry its prefix like any other's.
	sym := srv.graph.GetNode("lone-repo/main.go::LoneThing")
	require.NotNil(t, sym, "a lone repo's node IDs carry its prefix")
	require.Equal(t, "lone-repo", sym.RepoPrefix)

	// And the Locate intent default really narrows to the registry prefix.
	scope, errRes := srv.resolveScope(ctx, makeReq("search_symbols", nil), IntentLocate)
	require.Nil(t, errRes)
	require.True(t, scope.RepoAllow["lone-repo"],
		"bound-session Locate default must narrow to the home repo prefix")

	req := makeReq("search_symbols", map[string]any{"query": "LoneThing"})
	res, err := srv.handleSearchSymbols(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, "search_symbols must not error")

	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	ids := resultIDs(resp)
	require.NotEmpty(t, ids,
		"search_symbols must find the lone repo's symbols (fresh-install zero-rows regression)")
	foundLone := false
	for _, id := range ids {
		if containsStr(id, "LoneThing") {
			foundLone = true
		}
	}
	assert.True(t, foundLone, "LoneThing must be in results")
}

// TestIntentDefaults_ExplicitRepoArg_LoneRepo covers the
// explicit-narrowing variant of the same regression: `repo:` naming the lone
// repo (what the CLI's inline field query lowers to) must not hide its nodes.
func TestIntentDefaults_ExplicitRepoArg_LoneRepo(t *testing.T) {
	srv, dir := newLoneRepoServer(t, true)
	ctx := sessionCtx("s-lone", dir)

	req := makeReq("search_symbols", map[string]any{"query": "LoneThing", "repo": "lone-repo"})
	res, err := srv.handleSearchSymbols(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, "search_symbols must not error")

	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	require.NotEmpty(t, resultIDs(resp),
		"explicit repo narrowing must not hide the lone repo's own nodes")
}

// TestFilterTextMatches_LoneRepoAttribution covers the search_text arm:
// GrepTextForRepos stamps the registry prefix onto match paths
// ("lone-repo/main.go"), and node attribution must find the graph node for
// that path rather than fail-closed-dropping every match.
//
// The stamped path and the graph key now agree by construction. They used to
// diverge for a lone repo — the graph keyed files unprefixed ("main.go")
// while grep stamped the prefix — and attribution needed a provenance-gated
// strip to reconcile them.
func TestFilterTextMatches_LoneRepoAttribution(t *testing.T) {
	srv, _ := newLoneRepoServer(t, true)

	require.NotNil(t, srv.graph.GetNode("lone-repo/main.go"),
		"the file node is keyed by its prefixed path")

	matches := []trigram.Match{{Path: "lone-repo/main.go", Line: 3, Text: "func LoneThing() {}"}}
	resolved := ResolvedScope{
		WorkspaceID: "lone-repo",
		RepoAllow:   map[string]bool{"lone-repo": true},
	}
	kept := srv.filterTextMatchesByResolvedScope(context.Background(), matches, resolved)
	require.Len(t, kept, 1,
		"a stamped match in the lone repo must attribute to its graph node")
}

// newTwoRepoServer builds a server tracking two repos ("alpha" holding
// AlphaThing, "beta" holding ZebraWidget) — multi-repo mode, so nodes carry
// registry prefixes and the Locate intent default genuinely narrows.
func newTwoRepoServer(t *testing.T) (*Server, string) {
	t.Helper()

	alphaDir := filepath.Join(t.TempDir(), "alpha")
	betaDir := filepath.Join(t.TempDir(), "beta")
	require.NoError(t, os.MkdirAll(alphaDir, 0o755))
	require.NoError(t, os.MkdirAll(betaDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(alphaDir, "main.go"),
		[]byte("package main\n\nfunc AlphaThing() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(betaDir, "main.go"),
		[]byte("package main\n\nfunc ZebraWidget() {}\n"), 0o644))

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	// Both repos share one declared workspace: repo:"*" widens RepoAllow
	// but never crosses workspaces, so the "candidates outside the scope"
	// recheck is only meaningful inside a shared workspace.
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: alphaDir, Name: "alpha", Workspace: "shared"},
			{Path: betaDir, Name: "beta", Workspace: "shared"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	// A real sqlite store: search runs off its native symbol FTS, the same
	// corpus the daemon serves, so the scope narrowing is applied to
	// production-shaped hits.
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "fts.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	backend := search.NewSymbolSearcherBackend(store)
	mi := indexer.NewMultiIndexer(store, testRegistry(), backend, cm, zap.NewNop())
	_, err = mi.IndexScoped("", "")
	require.NoError(t, err)
	requireSymbolFTS(t, store)

	eng := query.NewEngine(store)
	eng.SetSearch(backend)

	flagVal := true
	srv := NewServer(eng, store, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		MultiIndexer:        mi,
		ConfigManager:       cm,
		ScopeIntentDefaults: &flagVal,
	})
	return srv, alphaDir
}

// TestScopeNote_SearchSymbols_NarrowedZeroIsBodyVisible: a session bound
// in repo alpha searches for a symbol that lives only in repo beta. The
// Locate default narrows to alpha → 0 results — and the response BODY
// must say so, including that candidates exist outside the scope. The
// _meta-only disclosure is invisible in CLI output and most clients.
func TestScopeNote_SearchSymbols_NarrowedZeroIsBodyVisible(t *testing.T) {
	srv, alphaDir := newTwoRepoServer(t)
	ctx := sessionCtx("s-note", alphaDir)

	req := makeReq("search_symbols", map[string]any{"query": "ZebraWidget"})
	res, err := srv.handleSearchSymbols(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	require.Empty(t, resultIDs(resp), "ZebraWidget must be filtered out by the alpha narrowing")

	note, _ := resp["scope_note"].(string)
	require.NotEmpty(t, note, "a scope-narrowed zero must carry a body-visible scope_note")
	assert.Contains(t, note, "outside", "the recheck must report candidates beyond the scope")
	assert.Contains(t, note, `repo:"*"`, "the note must teach the widen escape hatch")
}

// TestScopeNote_SearchSymbols_AbsentOnHitsAndOnTrueZero: no note rides a
// result that has hits, and a query matching nothing anywhere reports the
// recheck came back empty too.
func TestScopeNote_SearchSymbols_AbsentOnHitsAndOnTrueZero(t *testing.T) {
	srv, alphaDir := newTwoRepoServer(t)
	ctx := sessionCtx("s-note2", alphaDir)

	res, err := srv.handleSearchSymbols(ctx, makeReq("search_symbols", map[string]any{"query": "AlphaThing"}))
	require.NoError(t, err)
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	require.NotEmpty(t, resultIDs(resp))
	_, hasNote := resp["scope_note"]
	assert.False(t, hasNote, "results with hits must not carry a scope_note")

	res, err = srv.handleSearchSymbols(ctx, makeReq("search_symbols", map[string]any{"query": "NoSuchIdentifierAnywhere"}))
	require.NoError(t, err)
	resp = map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	if note, ok := resp["scope_note"].(string); ok {
		assert.Contains(t, note, "also found nothing",
			"a true zero must say the workspace-wide recheck found nothing")
	}
}

func TestScopeZeroNote_Branches(t *testing.T) {
	scope := ResolvedScope{RepoAllow: map[string]bool{"alpha": true}, Applied: "repo:alpha"}
	assert.Contains(t, scopeZeroNote(scope, 3), "3 candidate(s)")
	assert.Contains(t, scopeZeroNote(scope, 0), "also found nothing")
	assert.Contains(t, scopeZeroNote(scope, -1), `widen with repo:"*"`)
}
