package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
)

func TestStateAwareInstructions_ColdIdentityPendingAndRecovery(t *testing.T) {
	srv, mi, configured := newLoadingServer(t)
	physical := wsRepo(t, "physical", "alias-workspace", "AliasSymbol")
	alias := filepath.Join(t.TempDir(), "different-configured-name")
	require.NoError(t, os.Symlink(physical, alias))
	entries := []config.RepoEntry{{Path: configured}, {Path: alias, Name: "alias-repo"}}
	mi.DeclareRepos(entries)
	for _, cwd := range []string{physical, t.TempDir()} {
		for _, client := range []string{"claude-code", "codex", ""} {
			instructions := srv.stateAwareInstructionsForClient(cwd, client)
			require.Contains(t, instructions, "identity_pending")
			require.NotContains(t, instructions, "INACTIVE")
			require.NotContains(t, instructions, "already configured")
			require.NotContains(t, instructions, "alias-repo")
			require.NotContains(t, instructions, "gortex track")
		}
	}
	ctx := sessionCtx("unresolved-alias", physical)
	ws, _, bound := srv.sessionScope(ctx)
	require.True(t, bound)
	require.True(t, strings.HasPrefix(ws, unresolvedWorkspacePrefix), "uncertainty grants no graph scope")
	mi.ExpectRepos(entries)
	require.Contains(t, srv.stateAwareInstructions(physical), "LOADING alias-repo")
	_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: physical, Name: "alias-repo"})
	require.NoError(t, err)
	ws, _, bound = srv.sessionScope(ctx)
	require.True(t, bound)
	require.Equal(t, "alias-workspace", ws, "the same session re-resolves after identity and registration complete")
	require.NotContains(t, srv.stateAwareInstructions(physical), "identity_pending")
	require.Contains(t, srv.stateAwareInstructions(t.TempDir()), "INACTIVE", "only complete negative identity evidence permits inactive advice")
}

func TestIdentityPendingPreservesRegisteredAutomaticCheckout(t *testing.T) {
	v := newViewStack(t)
	v.srv.multiIndexer.DeclareRepos([]config.RepoEntry{{Path: v.repoRoot}, {Path: v.otherRoot}, {Path: filepath.Join(t.TempDir(), "unknown")}})
	require.True(t, v.srv.multiIndexer.RepoIdentityPending())
	alias := filepath.Join(t.TempDir(), "registered-checkout-alias")
	require.NoError(t, os.Symlink(v.worktreeRoot, alias))
	for _, path := range []string{v.worktreeRoot, alias} {
		served, err := v.srv.CheckoutServesCWDChecked(t.Context(), path)
		require.NoError(t, err)
		require.True(t, served)
		require.NotContains(t, v.srv.stateAwareInstructions(path), "identity_pending")
		ctx := sessionCtx("pending-other-root-"+filepath.Base(path), path)
		ws, _, bound := v.srv.sessionScope(ctx)
		require.True(t, bound)
		require.Equal(t, "main-ws", ws)
	}
}

func TestStateAwareInstructions_PublicationDuringCheckoutLookup(t *testing.T) {
	for _, register := range []bool{false, true} {
		name := "identity"
		if register {
			name = "registration"
		}
		t.Run(name, func(t *testing.T) {
			srv, mi, configured := newLoadingServer(t)
			physical := wsRepo(t, "physical", "published-workspace", "Published")
			alias := filepath.Join(t.TempDir(), "configured-alias")
			require.NoError(t, os.Symlink(physical, alias))
			entries := []config.RepoEntry{{Path: configured}, {Path: alias, Name: "published"}}
			mi.DeclareRepos(entries)
			calls := 0
			instructions := srv.stateAwareInstructionsWithCheckout(t.Context(), physical, serverInstructions, func(ctx context.Context, path string) (bool, error) {
				calls++
				require.Equal(t, physical, path)
				mi.ExpectRepos(entries)
				if register {
					_, err := mi.TrackRepoCtx(ctx, config.RepoEntry{Path: physical, Name: "published"})
					require.NoError(t, err)
				}
				return false, nil
			})
			require.Equal(t, 1, calls)
			require.NotContains(t, instructions, "INACTIVE")
			require.NotContains(t, instructions, "gortex track")
			if register {
				require.NotContains(t, instructions, "LOADING")
			} else {
				require.Contains(t, instructions, "LOADING published")
			}
		})
	}
}

func TestStateAwareInstructions_ReadyFamilyWithoutScopeStillRetries(t *testing.T) {
	srv, mi, primary := newLoadingServer(t)
	linked := t.TempDir()
	admin := filepath.Join(primary, ".git", "worktrees", "linked")
	require.NoError(t, os.MkdirAll(admin, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(admin, "commondir"), []byte("../.."), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+admin), 0644))
	calls := 0
	instructions := srv.stateAwareInstructionsWithCheckout(t.Context(), linked, serverInstructions, func(ctx context.Context, path string) (bool, error) {
		calls++
		if calls == 1 {
			_, err := mi.TrackRepoCtx(ctx, config.RepoEntry{Path: primary, Name: "repo-l"})
			require.NoError(t, err)
		}
		return false, nil
	})
	require.Equal(t, 2, calls)
	require.Contains(t, instructions, "view_building")
	require.Contains(t, instructions, "checkout discovery is pending")
	require.NotContains(t, instructions, "scope_binding_pending", "verified family pending keeps the canonical existing producer")
	require.NotContains(t, instructions, "INACTIVE")
	require.NotContains(t, instructions, "gortex track "+linked)
}
