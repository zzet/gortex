package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// containedFixture is the layout from gortexhq/gortex#418: two tracked
// repos side by side under one parent the user never tracked, each its
// own workspace by default, plus a third tracked repo OUTSIDE the parent
// that deliberately shares one of those workspace slugs.
//
// The outsider is what makes the fixture worth having: a session bound at
// the parent must see the two repos underneath it and NOT the outsider,
// which proves the boundary is containment rather than slug membership.
type containedFixture struct {
	srv      *Server
	parent   string
	repoA    string // under parent, workspace "alpha"
	repoB    string // under parent, workspace "beta"
	outsider string // outside parent, workspace "alpha"
}

func newContainedFixture(t *testing.T) containedFixture {
	t.Helper()

	mk := func(dir, workspace, symbol string) string {
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".gortex.yaml"),
			[]byte("workspace: "+workspace+"\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
			[]byte("package main\n\nfunc "+symbol+"() {}\n"), 0o644))
		return dir
	}

	parent := t.TempDir()
	repoA := mk(filepath.Join(parent, "repo-a"), "alpha", "AlphaThing")
	repoB := mk(filepath.Join(parent, "repo-b"), "beta", "BetaThing")
	outsider := mk(filepath.Join(t.TempDir(), "repo-c"), "alpha", "OutsiderThing")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
			{Path: outsider, Name: "repo-c"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	bm := search.NewNull()
	mi := indexer.NewMultiIndexer(g, testRegistry(), bm, cm, zap.NewNop())
	registerMCPFixtureRepos(t, mi, cm)

	eng := query.NewEngine(g)
	eng.SetSearch(bm)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		MultiIndexer:  mi,
		ConfigManager: cm,
	})
	return containedFixture{srv: srv, parent: parent, repoA: repoA, repoB: repoB, outsider: outsider}
}

// nodeNamed returns the first graph node with the given symbol name.
func nodeNamed(t *testing.T, g graph.Store, name string) *graph.Node {
	t.Helper()
	for _, n := range g.AllNodes() {
		if n.Name == name && n.Kind == graph.KindFunction {
			return n
		}
	}
	t.Fatalf("fixture node %q not found in graph", name)
	return nil
}

// TestContainedSession_BindsToContainedRepos is the #418 repro at the
// scope layer: a session opened at a parent above two repos in different
// workspaces binds — to the repos it contains, not to a workspace slug.
func TestContainedSession_BindsToContainedRepos(t *testing.T) {
	f := newContainedFixture(t)
	ctx := sessionCtx("s-parent", f.parent)

	ws, proj, bound := f.srv.sessionScope(ctx)
	require.True(t, bound, "a parent containing tracked repos must bind")
	assert.Empty(t, ws, "a multi-workspace session has no single slug")
	assert.Empty(t, proj)

	ceiling := f.srv.sessionRepoCeiling(ctx)
	assert.Equal(t, map[string]bool{"repo-a": true, "repo-b": true}, ceiling)
	assert.NotContains(t, ceiling, "repo-c",
		"a repo outside the cwd must not be admitted, even sharing workspace \"alpha\"")

	spanned := f.srv.sessionScopeWorkspaces(ctx)
	assert.Equal(t, map[string]bool{"alpha": true, "beta": true}, spanned)
}

// TestContainedSession_NoUnboundedForm is the structural safety property
// the whole design rests on: a bound session must never produce query
// options that filter on nothing. An empty workspace slug AND an empty
// repo allow-set is not a narrow boundary — ScopeAllows reads it as "no
// filter" and hands back the entire multi-workspace graph.
func TestContainedSession_NoUnboundedForm(t *testing.T) {
	f := newContainedFixture(t)

	for _, cwd := range []string{f.parent, f.repoA, f.outsider} {
		opts, bound := f.srv.sessionScopeOptions(sessionCtx("s-"+filepath.Base(cwd), cwd))
		require.True(t, bound, "cwd %s must bind", cwd)
		assert.True(t, opts.WorkspaceID != "" || len(opts.RepoAllow) > 0,
			"bound session for %s has no filter at all — this is the global graph", cwd)
	}
}

// TestContainedSession_NodeVisibility proves the boundary actually holds
// at the per-node gate every by-id and whole-graph handler routes through.
func TestContainedSession_NodeVisibility(t *testing.T) {
	f := newContainedFixture(t)
	ctx := sessionCtx("s-parent", f.parent)

	alpha := nodeNamed(t, f.srv.graph, "AlphaThing")
	beta := nodeNamed(t, f.srv.graph, "BetaThing")
	outsider := nodeNamed(t, f.srv.graph, "OutsiderThing")

	assert.True(t, f.srv.nodeInSessionScope(ctx, alpha), "contained repo-a must be visible")
	assert.True(t, f.srv.nodeInSessionScope(ctx, beta), "contained repo-b must be visible")
	assert.False(t, f.srv.nodeInSessionScope(ctx, outsider),
		"a repo outside the session cwd must stay invisible")

	names := map[string]bool{}
	for _, n := range f.srv.scopedNodes(ctx) {
		names[n.Name] = true
	}
	assert.True(t, names["AlphaThing"])
	assert.True(t, names["BetaThing"])
	assert.False(t, names["OutsiderThing"], "scopedNodes leaked a repo outside the session")
}

// TestContainedSession_IntentDefaultsKeepCeiling guards the single most
// dangerous regression in this design. applyIntentDefault used to clear
// RepoAllow unconditionally, which is correct only when the session's
// workspace slug still bounds the query. A contained-repo session has no
// slug, so clearing the allow-set there turns every un-narrowed reach and
// analyze call into a full-graph pass.
func TestContainedSession_IntentDefaultsKeepCeiling(t *testing.T) {
	f := newContainedFixture(t)
	ctx := sessionCtx("s-parent", f.parent)
	want := map[string]bool{"repo-a": true, "repo-b": true}

	for _, tc := range []struct {
		name   string
		tool   string
		intent ToolIntent
	}{
		{"locate", "search_symbols", IntentLocate},
		{"reach", "get_callers", IntentReach},
		{"analyze", "analyze", IntentAnalyze},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := f.srv.resolveScopeForRequest(ctx, makeReq(tc.tool, nil), tc.intent)
			require.NoError(t, err)
			assert.Empty(t, resolved.WorkspaceID, "no slug to fall back on")
			assert.Equal(t, want, resolved.RepoAllow,
				"an un-narrowed %s call must stay inside the session's repos", tc.name)
		})
	}
}

// TestContainedSession_SelectorsStayInside covers the selectors an agent
// reaches for once the session is admitted: they must narrow within the
// contained set and must not become an escape hatch out of it.
func TestContainedSession_SelectorsStayInside(t *testing.T) {
	f := newContainedFixture(t)
	ctx := sessionCtx("s-parent", f.parent)

	t.Run("explicit repo narrows to that repo", func(t *testing.T) {
		resolved, err := f.srv.resolveScopeForRequest(ctx,
			makeReq("search_symbols", map[string]any{"repo": "repo-a"}), IntentLocate)
		require.NoError(t, err)
		assert.Equal(t, map[string]bool{"repo-a": true}, resolved.RepoAllow)
	})

	t.Run("repo widening cannot escape the session", func(t *testing.T) {
		resolved, err := f.srv.resolveScopeForRequest(ctx,
			makeReq("search_symbols", map[string]any{"repo": "*"}), IntentLocate)
		require.NoError(t, err)
		assert.Equal(t, map[string]bool{"repo-a": true, "repo-b": true}, resolved.RepoAllow,
			`repo:"*" must widen only to the session's own repos`)
	})

	t.Run("a repo outside the session is refused", func(t *testing.T) {
		_, err := f.srv.resolveScopeForRequest(ctx,
			makeReq("search_symbols", map[string]any{"repo": "repo-c"}), IntentLocate)
		require.Error(t, err, "a repo outside the session cwd must not resolve")
	})

	t.Run("workspace selector narrows to its slice of the ceiling", func(t *testing.T) {
		resolved, err := f.srv.resolveScopeForRequest(ctx,
			makeReq("search_symbols", map[string]any{"workspace": "alpha"}), IntentLocate)
		require.NoError(t, err)
		assert.Equal(t, "alpha", resolved.WorkspaceID)
		assert.Equal(t, map[string]bool{"repo-a": true}, resolved.RepoAllow,
			"workspace alpha also names repo-c, which the session does not contain")
	})

	t.Run("a workspace the session does not span is refused", func(t *testing.T) {
		_, err := f.srv.resolveScopeForRequest(ctx,
			makeReq("search_symbols", map[string]any{"workspace": "gamma"}), IntentLocate)
		require.Error(t, err)
	})
}

// TestContainedSession_DisjointRepoNarrowDeniesAll pins the sharp edge in
// folding a handler repo narrow into a session ceiling: an intersection
// that comes out empty must reject everything, not fall through to "no
// filter set". The empty allow-set is indistinguishable from an absent
// one, so the collapse has to be expressed on the workspace axis.
func TestContainedSession_DisjointRepoNarrowDeniesAll(t *testing.T) {
	f := newContainedFixture(t)
	ctx := withRepoAllow(sessionCtx("s-parent", f.parent), map[string]bool{"repo-c": true})

	opts, active := f.srv.scopeOptionsWithRepoNarrow(ctx, map[string]bool{"repo-c": true})
	require.True(t, active)
	for _, name := range []string{"AlphaThing", "BetaThing", "OutsiderThing"} {
		assert.False(t, opts.ScopeAllows(nodeNamed(t, f.srv.graph, name)),
			"a narrow disjoint from the session ceiling must admit nothing, got %s", name)
	}
	assert.Empty(t, f.srv.scopeNodes(ctx, f.srv.graph.AllNodes()))
}

// TestContainedSession_SideStoreScope covers notes and memories, which
// have no repo axis at all: their only boundary is the workspace slug,
// and a contained session's slug is empty — which reads as "every
// workspace" unless the set-valued filter is supplied.
func TestContainedSession_SideStoreScope(t *testing.T) {
	f := newContainedFixture(t)

	ws, in, bound := f.srv.sessionSideStoreScope(sessionCtx("s-parent", f.parent))
	require.True(t, bound)
	assert.Empty(t, ws)
	assert.Equal(t, map[string]bool{"alpha": true, "beta": true}, in,
		"a session with no single slug must filter on the set it spans")

	ws, in, bound = f.srv.sessionSideStoreScope(sessionCtx("s-a", f.repoA))
	require.True(t, bound)
	assert.Equal(t, "alpha", ws)
	assert.Nil(t, in, "a single-slug session keeps the scalar filter")

	_, _, bound = f.srv.sessionSideStoreScope(context.Background())
	assert.False(t, bound)
}

// TestSessionScope_ReresolvesWhenCWDChanges covers the Streamable HTTP
// transport, which carries the cwd per request rather than per
// connection. The binding was resolved once and cached forever, so a
// session that handshook without a cwd stayed UNBOUND — and then served
// the entire multi-workspace graph to every later request that did carry
// one.
func TestSessionScope_ReresolvesWhenCWDChanges(t *testing.T) {
	f := newContainedFixture(t)
	const id = "s-streamable"

	_, _, bound := f.srv.sessionScope(WithSessionID(context.Background(), id))
	require.False(t, bound, "no cwd yet — unbound")

	ws, _, bound := f.srv.sessionScope(sessionCtx(id, f.repoA))
	require.True(t, bound, "the same session must bind once a cwd arrives")
	assert.Equal(t, "alpha", ws)

	ws, _, bound = f.srv.sessionScope(sessionCtx(id, f.repoB))
	require.True(t, bound)
	assert.Equal(t, "beta", ws, "a revised cwd must re-resolve, not replay the old binding")
	assert.Nil(t, f.srv.sessionRepoCeiling(sessionCtx(id, f.repoB)),
		"stale ceiling from a previous cwd must be cleared")
}

// TestInvalidateSessionScopes_UnlatchesAfterTrack proves the bootstrap
// exemption is real. A session in an uncovered cwd is allowed to call
// workspace_admin(track) to repair itself — but the binding it latched
// on the first blocked call would keep it blind to the repo it just
// added, making the exemption decorative.
func TestInvalidateSessionScopes_UnlatchesAfterTrack(t *testing.T) {
	f := newContainedFixture(t)
	stranger := t.TempDir()
	ctx := sessionCtx("s-stranger", stranger)

	ws, _, bound := f.srv.sessionScope(ctx)
	require.True(t, bound)
	require.Contains(t, ws, unresolvedWorkspacePrefix, "an uncovered cwd fails closed")

	_, err := f.srv.multiIndexer.TrackRepoCtx(context.Background(), config.RepoEntry{Path: stranger})
	require.NoError(t, err)

	// Without invalidation the session replays its cached sentinel.
	ws, _, _ = f.srv.sessionScope(ctx)
	require.Contains(t, ws, unresolvedWorkspacePrefix, "precondition: the binding is cached")

	f.srv.InvalidateSessionScopes()
	ws, _, bound = f.srv.sessionScope(ctx)
	require.True(t, bound)
	assert.NotContains(t, ws, unresolvedWorkspacePrefix,
		"after tracking its own cwd the session must see the repo it added")
}

// TestContainedSession_DiffRootDoesNotFallBackToDaemonCWD covers the
// diff-driven handlers. Their working-tree resolution ends in a `"."`
// fallback, which is the daemon PROCESS's cwd — wherever `gortex daemon
// start` happened to run. That is a legitimate answer only for a
// standalone, indexer-less server started inside the tree it serves.
//
// A session bound to the repos its cwd contains has no home repo, so every
// earlier branch misses and the fallback was reached on the ordinary path.
// detect_changes then diffed an unrelated tree the session is not scoped
// to — a wrong answer and a disclosure, in one call.
func TestContainedSession_DiffRootDoesNotFallBackToDaemonCWD(t *testing.T) {
	f := newContainedFixture(t)
	ctx := sessionCtx("s-parent", f.parent)

	t.Run("several contained repos are ambiguous, not the daemon cwd", func(t *testing.T) {
		root, _, err := f.srv.resolveDiffRoot(ctx, "")
		require.Error(t, err, "an ambiguous session must not resolve to a working tree")
		assert.Empty(t, root)
		assert.NotEqual(t, ".", root)
		// The error has to name the candidates, or the agent cannot act.
		assert.Contains(t, err.Error(), "repo-a")
		assert.Contains(t, err.Error(), "repo-b")
		assert.Contains(t, err.Error(), "repo:")
	})

	t.Run("an explicit selector inside the session resolves", func(t *testing.T) {
		root, prefix, err := f.srv.resolveDiffRoot(ctx, "repo-a")
		require.NoError(t, err)
		assert.Equal(t, "repo-a", prefix)
		assert.Equal(t, f.repoA, root)
	})

	t.Run("a selector outside the session is refused", func(t *testing.T) {
		_, _, err := f.srv.resolveDiffRoot(ctx, "repo-c")
		require.Error(t, err, "a repo the session does not contain must not resolve a working tree")
	})

	t.Run("a session bound inside one repo still resolves it", func(t *testing.T) {
		root, prefix, err := f.srv.resolveDiffRoot(sessionCtx("s-a", f.repoA), "")
		require.NoError(t, err)
		assert.Equal(t, "repo-a", prefix)
		assert.Equal(t, f.repoA, root)
	})
}
