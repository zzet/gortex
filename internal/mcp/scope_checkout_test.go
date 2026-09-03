package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The scope half of the routed-view fixture. newViewStack registers a worktree
// of the indexed repository as an automatic checkout, and that worktree lies
// inside no tracked repository — so the session binding for a cwd there is
// decided by the view catalog, not by the repository registry.

// TestAutomaticCheckoutScope_MatchesThePrimaryRootBinding is the decided rule
// stated as an equality: in scope terms an automatic checkout occupies its
// family primary's repository, so a session inside the worktree resolves the
// same ResolvedScope as one sitting at the primary's own root. Anything less
// is a blind session; anything more is a widening.
func TestAutomaticCheckoutScope_MatchesThePrimaryRootBinding(t *testing.T) {
	v := newViewStack(t)
	inWorktree := sessionCtx("s-worktree", v.worktreeRoot)
	atPrimary := sessionCtx("s-primary", v.repoRoot)

	wsWT, projWT, boundWT := v.srv.sessionScope(inWorktree)
	wsRoot, projRoot, boundRoot := v.srv.sessionScope(atPrimary)
	require.True(t, boundRoot, "the primary root must bind")
	require.True(t, boundWT, "a cwd inside a registered automatic checkout must bind")
	assert.Equal(t, "main-ws", wsWT, "the checkout carries the primary's workspace identity")
	assert.Equal(t, wsRoot, wsWT)
	assert.Equal(t, projRoot, projWT)

	assert.Equal(t, v.srv.sessionRepoCeiling(atPrimary), v.srv.sessionRepoCeiling(inWorktree),
		"the checkout must not carry a ceiling of its own")

	optsWT, _ := v.srv.sessionScopeOptions(inWorktree)
	optsRoot, _ := v.srv.sessionScopeOptions(atPrimary)
	assert.Equal(t, optsRoot, optsWT, "the per-node gate must apply one boundary, not two")

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
			fromWT, err := v.srv.resolveScopeForRequest(inWorktree, makeReq(tc.tool, nil), tc.intent)
			require.NoError(t, err)
			fromRoot, err := v.srv.resolveScopeForRequest(atPrimary, makeReq(tc.tool, nil), tc.intent)
			require.NoError(t, err)
			assert.Equal(t, fromRoot, fromWT,
				"a %s call from the worktree must resolve the primary root's scope", tc.name)
		})
	}
}

// TestAutomaticCheckoutScope_AdmitsTheRepoAndNothingElse proves the binding is
// a real boundary rather than an unfiltered one: the sibling repository in
// another workspace stays invisible from inside the worktree, exactly as it is
// from the primary root.
func TestAutomaticCheckoutScope_AdmitsTheRepoAndNothingElse(t *testing.T) {
	v := newViewStack(t)
	ctx := sessionCtx("s-worktree", v.worktreeRoot)

	opts, bound := v.srv.sessionScopeOptions(ctx)
	require.True(t, bound)
	require.True(t, opts.WorkspaceID != "" || len(opts.RepoAllow) > 0,
		"a bound session with no filter at all is the global graph")

	assert.True(t, v.srv.nodeInSessionScope(ctx, searchNode(searchKeeperID, "Keeper", "repo/keep.go", 3)),
		"the repository the checkout belongs to must be visible")

	outsider := searchNode("other/other.go::Other", "Other", "other/other.go", 3)
	outsider.RepoPrefix = "other"
	outsider.WorkspaceID = "other-ws"
	outsider.ProjectID = "other"
	assert.False(t, v.srv.nodeInSessionScope(ctx, outsider),
		"a sibling workspace must stay outside a checkout-bound session")
}

// TestAutomaticCheckoutScope_UnknownCWDStaysUnresolved is the non-widening
// guard. The catalog probe runs on exactly the cwds that used to fail closed,
// so a directory it does not recognise must come back with the same
// unresolved-workspace form it had before — not a workspace, and not nothing.
func TestAutomaticCheckoutScope_UnknownCWDStaysUnresolved(t *testing.T) {
	v := newViewStack(t)
	stranger := t.TempDir()
	ctx := sessionCtx("s-stranger", stranger)

	ws, _, bound := v.srv.sessionScope(ctx)
	require.True(t, bound, "an uncovered cwd binds — blind, not unbound")
	assert.Contains(t, ws, unresolvedWorkspacePrefix)
	assert.Nil(t, v.srv.sessionRepoCeiling(ctx))
	assert.False(t, v.srv.nodeInSessionScope(ctx, searchNode(searchKeeperID, "Keeper", "repo/keep.go", 3)),
		"an unresolved cwd must see nothing, not everything")

	// A directory under the worktree's own parent is still unknown: the
	// binding is containment in a REGISTERED checkout, not proximity to one.
	sibling := filepath.Join(filepath.Dir(v.worktreeRoot), "not-a-checkout")
	wsSibling, _, bound := v.srv.sessionScope(sessionCtx("s-sibling", sibling))
	require.True(t, bound)
	assert.Contains(t, wsSibling, unresolvedWorkspacePrefix)
}

// TestAutomaticCheckoutScope_OnlyLiveAutomaticCheckoutsBind covers the two
// states the shared lane does not answer for. A dedicated checkout is read
// from the indexed corpus under its own repository entry, and a checkout that
// is not ready has no view to occupy — neither may borrow the primary's scope.
func TestAutomaticCheckoutScope_OnlyLiveAutomaticCheckoutsBind(t *testing.T) {
	t.Run("a checkout that is not live falls back to unresolved", func(t *testing.T) {
		v := newViewStack(t)
		v.setWorktreeState(t, store_sqlite.CheckoutStateUnavailable)

		ws, _, bound := v.srv.sessionScope(sessionCtx("s-unavailable", v.worktreeRoot))
		require.True(t, bound)
		assert.Contains(t, ws, unresolvedWorkspacePrefix)
	})

	t.Run("no view catalog keeps the previous behaviour", func(t *testing.T) {
		v := newViewStack(t)
		v.srv.SetMaterializer(nil)

		ws, _, bound := v.srv.sessionScope(sessionCtx("s-no-materializer", v.worktreeRoot))
		require.True(t, bound)
		assert.Contains(t, ws, unresolvedWorkspacePrefix)
	})
}

// TestAutomaticCheckoutScope_CachedAndInvalidated pins the binding to the same
// cache the rest of scope resolution uses: resolved once per cwd, dropped by
// InvalidateSessionScopes so a checkout registered after the session bound is
// picked up on the next call rather than latched away.
func TestAutomaticCheckoutScope_CachedAndInvalidated(t *testing.T) {
	v := newViewStack(t)
	const id = "s-lifecycle"
	ctx := sessionCtx(id, v.worktreeRoot)

	// Bind while the checkout is live, then take it away: the cached binding
	// survives until something invalidates it.
	ws, _, bound := v.srv.sessionScope(ctx)
	require.True(t, bound)
	require.Equal(t, "main-ws", ws)

	v.setWorktreeState(t, store_sqlite.CheckoutStateUnavailable)
	ws, _, _ = v.srv.sessionScope(ctx)
	assert.Equal(t, "main-ws", ws, "precondition: the binding is cached, not re-read per call")

	v.srv.InvalidateSessionScopes()
	ws, _, bound = v.srv.sessionScope(ctx)
	require.True(t, bound)
	assert.Contains(t, ws, unresolvedWorkspacePrefix,
		"InvalidateSessionScopes must drop the checkout binding with every other one")

	// And the reverse direction: a session that bound blind sees the
	// checkout once it exists again.
	v.setWorktreeState(t, store_sqlite.CheckoutStateReady)
	ws, _, _ = v.srv.sessionScope(ctx)
	require.Contains(t, ws, unresolvedWorkspacePrefix, "precondition: still cached")
	v.srv.InvalidateSessionScopes()
	ws, _, _ = v.srv.sessionScope(ctx)
	assert.Equal(t, "main-ws", ws)

	// A revised cwd re-resolves on its own, as it does for every other shape.
	ws, _, _ = v.srv.sessionScope(sessionCtx(id, v.otherRoot))
	assert.Equal(t, "other-ws", ws)
}

// TestAutomaticCheckoutScope_UnboundSessionStaysUnbound: the probe must not
// turn a session with no cwd into a bound one. An embedded stdio client has no
// working directory to bind, and inventing one would clamp the server-default
// scope.
func TestAutomaticCheckoutScope_UnboundSessionStaysUnbound(t *testing.T) {
	v := newViewStack(t)

	_, _, bound := v.srv.sessionScope(WithSessionID(context.Background(), "s-no-cwd"))
	assert.False(t, bound)

	_, _, _, ok := v.srv.scopeForAutomaticCheckout(context.Background(), "")
	assert.False(t, ok, "an empty path names no checkout")
}
