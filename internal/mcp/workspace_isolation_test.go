package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// wsRepo writes a minimal Go repo under a temp dir with a `.gortex.yaml`
// declaring the given workspace slug, plus one source file whose only
// symbol is uniquely named so cross-workspace leakage is observable.
func wsRepo(t *testing.T, name, workspace, symbol string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gortex.yaml"),
		[]byte("workspace: "+workspace+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc "+symbol+"() {}\n"), 0o644))
	return dir
}

// newIsolationServer indexes two repos in two different workspaces
// (alpha → repo-a, beta → repo-b) and returns a Server wired to that
// multi-repo graph, along with the two repo roots.
func newIsolationServer(t *testing.T) (srv *Server, repoA, repoB string) {
	t.Helper()
	repoA = wsRepo(t, "repo-a", "alpha", "AlphaThing")
	repoB = wsRepo(t, "repo-b", "beta", "BetaThing")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
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
	registerMCPFixtureRepos(t, mi, cm) // index every configured repo

	eng := query.NewEngine(g)
	eng.SetSearch(bm)
	srv = NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		MultiIndexer:  mi,
		ConfigManager: cm,
	})
	return srv, repoA, repoB
}

func sessionCtx(id, cwd string) context.Context {
	return WithSessionCWD(WithSessionID(context.Background(), id), cwd)
}

// TestWorkspaceIsolation_SessionScope verifies a session resolves to
// the workspace of its cwd's repo, and stays there.
func TestWorkspaceIsolation_SessionScope(t *testing.T) {
	srv, repoA, repoB := newIsolationServer(t)

	wsA, _, boundA := srv.sessionScope(sessionCtx("s-alpha", repoA))
	require.True(t, boundA)
	assert.Equal(t, "alpha", wsA)

	wsB, _, boundB := srv.sessionScope(sessionCtx("s-beta", repoB))
	require.True(t, boundB)
	assert.Equal(t, "beta", wsB)

	// A session with no cwd (embedded stdio / control client) is
	// unbound and falls back to the server-default scope.
	_, _, bound := srv.sessionScope(context.Background())
	assert.False(t, bound)
}

// TestWorkspaceIsolation_ScopedNodes is the core repro: a session in
// workspace alpha must never see workspace beta's nodes, and vice
// versa — not via scopedNodes, not via nodeInSessionScope.
func TestWorkspaceIsolation_ScopedNodes(t *testing.T) {
	srv, repoA, repoB := newIsolationServer(t)
	ctxA := sessionCtx("s-alpha", repoA)
	ctxB := sessionCtx("s-beta", repoB)

	effectiveWS := func(n *graph.Node) string {
		if n.WorkspaceID != "" {
			return n.WorkspaceID
		}
		return n.RepoPrefix
	}

	alphaNodes := srv.scopedNodes(ctxA)
	require.NotEmpty(t, alphaNodes)
	for _, n := range alphaNodes {
		assert.Equal(t, "alpha", effectiveWS(n),
			"session alpha must only see alpha nodes, leaked: %s", n.ID)
	}

	betaNodes := srv.scopedNodes(ctxB)
	require.NotEmpty(t, betaNodes)
	for _, n := range betaNodes {
		assert.Equal(t, "beta", effectiveWS(n),
			"session beta must only see beta nodes, leaked: %s", n.ID)
	}

	// Symbol-level: alpha sees AlphaThing, never BetaThing.
	assert.True(t, hasSymbol(alphaNodes, "AlphaThing"))
	assert.False(t, hasSymbol(alphaNodes, "BetaThing"))
	assert.True(t, hasSymbol(betaNodes, "BetaThing"))
	assert.False(t, hasSymbol(betaNodes, "AlphaThing"))

	// nodeInSessionScope: a beta node is invisible to an alpha session.
	var betaNode *graph.Node
	for _, n := range betaNodes {
		if n.Name == "BetaThing" {
			betaNode = n
			break
		}
	}
	require.NotNil(t, betaNode)
	assert.False(t, srv.nodeInSessionScope(ctxA, betaNode),
		"alpha session must not be able to reach a beta node by id")
	assert.True(t, srv.nodeInSessionScope(ctxB, betaNode))

	// An unbound session sees every node — legacy single-tenant behaviour.
	all := srv.scopedNodes(context.Background())
	assert.True(t, hasSymbol(all, "AlphaThing") && hasSymbol(all, "BetaThing"))
}

func TestWorkspaceIsolation_GetSymbolRejectsCrossWorkspaceID(t *testing.T) {
	srv, repoA, _ := newIsolationServer(t)
	ctxA := sessionCtx("s-alpha", repoA)
	betaID := symbolIDByName(t, srv, "BetaThing")

	res, err := srv.handleGetSymbol(ctxA, makeReq("get_symbol", map[string]any{"id": betaID}))
	require.NoError(t, err)
	assertRecoverableCondition(t, res, ErrCodeSymbolNotFound)
}

func TestWorkspaceIsolation_GetSymbolFiltersCrossWorkspaceEdges(t *testing.T) {
	srv, repoA, _ := newIsolationServer(t)
	ctxA := sessionCtx("s-alpha", repoA)
	alphaID := symbolIDByName(t, srv, "AlphaThing")
	betaID := symbolIDByName(t, srv, "BetaThing")
	srv.graph.AddEdge(&graph.Edge{From: alphaID, To: betaID, Kind: graph.EdgeCalls})

	res, err := srv.handleGetSymbol(ctxA, makeReq("get_symbol", map[string]any{
		"id":     alphaID,
		"detail": "full",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError, "get_symbol errored: %s", toolResultText(res))

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(toolResultText(res)), &payload))
	assert.Contains(t, payload, "node")
	outEdges, err := json.Marshal(payload["out_edges"])
	require.NoError(t, err)
	inEdges, err := json.Marshal(payload["in_edges"])
	require.NoError(t, err)
	assert.NotContains(t, string(outEdges), betaID, "cross-workspace out_edges must not be disclosed")
	assert.NotContains(t, string(inEdges), betaID, "cross-workspace in_edges must not be disclosed")
}

func TestWorkspaceIsolation_ReachToolsRejectCrossWorkspaceSeedIDs(t *testing.T) {
	srv, repoA, _ := newIsolationServer(t)
	ctxA := sessionCtx("s-alpha", repoA)
	betaID := symbolIDByName(t, srv, "BetaThing")

	tests := []struct {
		name    string
		handler func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error)
	}{
		{name: "get_dependencies", handler: srv.handleGetDependencies},
		{name: "get_call_chain", handler: srv.handleGetCallChain},
		{name: "find_usages", handler: srv.handleFindUsages},
		{name: "walk_graph", handler: srv.handleWalkGraph},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := tt.handler(ctxA, makeReq(tt.name, map[string]any{"id": betaID}))
			require.NoError(t, err)
			assertRecoverableCondition(t, res, ErrCodeSymbolNotFound)
		})
	}
}

// TestWorkspaceIsolation_ResolveRepoFilter verifies the repo-prefix
// allow-set is bounded to the session's workspace and that args cannot
// escape it.
func TestWorkspaceIsolation_ResolveRepoFilter(t *testing.T) {
	srv, repoA, _ := newIsolationServer(t)
	ctxA := sessionCtx("s-alpha", repoA)

	emptyReq := func() mcplib.CallToolRequest {
		r := mcplib.CallToolRequest{}
		r.Params.Arguments = map[string]any{}
		return r
	}
	argReq := func(args map[string]any) mcplib.CallToolRequest {
		r := mcplib.CallToolRequest{}
		r.Params.Arguments = args
		return r
	}

	// No explicit narrowing → the allow-set is exactly alpha's repos.
	allowed, err := srv.resolveRepoFilter(ctxA, emptyReq())
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"repo-a": true}, allowed)

	// A `workspace` arg naming another workspace is rejected.
	_, err = srv.resolveRepoFilter(ctxA, argReq(map[string]any{"workspace": "beta"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cross-workspace")

	// A `repo` arg naming a repo outside the workspace is rejected.
	_, err = srv.resolveRepoFilter(ctxA, argReq(map[string]any{"repo": "repo-b"}))
	require.Error(t, err)

	// A `repo` arg inside the workspace narrows correctly.
	allowed, err = srv.resolveRepoFilter(ctxA, argReq(map[string]any{"repo": "repo-a"}))
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"repo-a": true}, allowed)

	// The session's own workspace as an explicit arg is accepted.
	allowed, err = srv.resolveRepoFilter(ctxA, argReq(map[string]any{"workspace": "alpha"}))
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"repo-a": true}, allowed)
}

// TestWorkspaceIsolation_Introspection verifies get_active_project /
// workspace_info report the session's real resolved scope rather than
// a process-global default that would mask whether scoping is active.
func TestWorkspaceIsolation_Introspection(t *testing.T) {
	srv, repoA, repoB := newIsolationServer(t)

	payloadA := srv.buildActiveProjectPayload(sessionCtx("s-alpha", repoA))
	assert.Equal(t, "alpha", payloadA["workspace"])
	assert.Equal(t, true, payloadA["bound"])

	payloadB := srv.buildActiveProjectPayload(sessionCtx("s-beta", repoB))
	assert.Equal(t, "beta", payloadB["workspace"])

	wsInfoA := srv.buildWorkspaceInfoPayload(sessionCtx("s-alpha", repoA))
	assert.Equal(t, "alpha", wsInfoA["workspace"])
}

// TestWorkspaceIsolation_ConcurrentSessions runs two sessions in
// different workspaces against the one shared server concurrently —
// `go test -race` proves the per-session scope cache is race-free and
// the two sessions never bleed into each other.
func TestWorkspaceIsolation_ConcurrentSessions(t *testing.T) {
	srv, repoA, repoB := newIsolationServer(t)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			ws, _, bound := srv.sessionScope(sessionCtx("s-alpha", repoA))
			assert.True(t, bound)
			assert.Equal(t, "alpha", ws)
		}()
		go func() {
			defer wg.Done()
			ws, _, bound := srv.sessionScope(sessionCtx("s-beta", repoB))
			assert.True(t, bound)
			assert.Equal(t, "beta", ws)
		}()
	}
	wg.Wait()
}

func hasSymbol(nodes []*graph.Node, name string) bool {
	for _, n := range nodes {
		if n.Name == name {
			return true
		}
	}
	return false
}

func symbolIDByName(t *testing.T, srv *Server, name string) string {
	t.Helper()
	for _, n := range srv.graph.AllNodes() {
		if n.Name == name {
			return n.ID
		}
	}
	t.Fatalf("symbol %q not found", name)
	return ""
}

func assertRecoverableCondition(t *testing.T, res *mcplib.CallToolResult, condition ErrorCode) {
	t.Helper()
	require.NotNil(t, res)
	require.False(t, res.IsError, "recoverable guidance should not be an MCP error: %s", toolResultText(res))
	var guidance RecoverableGuidance
	require.NoError(t, json.Unmarshal([]byte(toolResultText(res)), &guidance))
	assert.Equal(t, condition, guidance.Condition)
}
