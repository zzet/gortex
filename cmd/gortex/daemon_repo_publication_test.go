package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
)

func TestDispatcher_IdentityPublicationDuringCheckoutProbe(t *testing.T) {
	for _, register := range []bool{false, true} {
		name := "identity"
		if register {
			name = "registration"
		}
		t.Run(name, func(t *testing.T) {
			d, mi, registered, _ := loadingDispatcherSetup(t)
			physical := t.TempDir()
			require.NoError(t, writeFile(filepath.Join(physical, "source.go"), "package source\nfunc Published() {}\n"))
			alias := filepath.Join(t.TempDir(), "configured-alias")
			require.NoError(t, os.Symlink(physical, alias))
			entries := []config.RepoEntry{{Path: registered}, {Path: alias, Name: "published"}}
			mi.DeclareRepos(entries)
			calls := 0
			d.checkoutCWDProbe = func(ctx context.Context, path string) (bool, error) {
				calls++
				require.Equal(t, physical, path)
				mi.ExpectRepos(entries)
				if register {
					_, err := mi.TrackRepoCtx(ctx, config.RepoEntry{Path: physical, Name: "published"})
					require.NoError(t, err)
				}
				return false, nil // The earlier negative probe is now stale.
			}
			reachable, err := d.cwdReachableChecked(t.Context(), physical)
			require.Equal(t, 1, calls)
			if register {
				require.NoError(t, err)
				require.True(t, reachable, "normal registered scope must be rechecked after publication")
			} else {
				var pending *repoPendingAdmission
				require.ErrorAs(t, err, &pending)
				require.False(t, reachable)
				require.Equal(t, alias, pending.load.Root)
				require.Equal(t, indexer.RepoLoadQueued, pending.load.State)
			}
		})
	}
}

func TestCoverage_IdentityPublicationDuringPublicViewProbe(t *testing.T) {
	for _, door := range []string{"file", "directory"} {
		t.Run(door, func(t *testing.T) {
			_, mi, registered, _ := loadingDispatcherSetup(t)
			physical := t.TempDir()
			alias := filepath.Join(t.TempDir(), "configured-alias")
			require.NoError(t, os.Symlink(physical, alias))
			entries := []config.RepoEntry{{Path: registered}, {Path: alias, Name: "published"}}
			mi.DeclareRepos(entries)
			c := &realController{multiIndexer: mi}
			probe := func(ctx context.Context, path string) probeView {
				view := c.resolveProbeView(ctx, path)
				mi.ExpectRepos(entries)
				return view
			}
			if door == "file" {
				result, err := c.fileCoverageWithProbe(t.Context(), daemon.FileCoverageParams{Path: filepath.Join(physical, "source.go")}, probe)
				require.NoError(t, err)
				require.Equal(t, daemon.RepoStateLoading, result.RepoState)
				require.Equal(t, "published", result.Repo)
				require.False(t, result.Covered)
			} else {
				result, err := c.dirCoverageWithProbe(t.Context(), daemon.DirCoverageParams{Path: physical}, probe)
				require.NoError(t, err)
				require.Equal(t, daemon.RepoStateLoading, result.RepoState)
				require.Equal(t, "published", result.Repo)
				require.False(t, result.HasSource)
			}
		})
	}
}

func TestDispatcher_ReadyFamilyIdentityDoesNotGrantUnprovenCheckoutScope(t *testing.T) {
	d, mi, registered, primary := loadingDispatcherSetup(t)
	linked := t.TempDir()
	admin := filepath.Join(primary, ".git", "worktrees", "linked")
	require.NoError(t, os.MkdirAll(admin, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(admin, "commondir"), []byte("../.."), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+admin), 0644))
	entries := []config.RepoEntry{{Path: registered}, {Path: primary, Name: "loading"}}
	mi.DeclareRepos(entries)
	calls := 0
	d.checkoutCWDProbe = func(ctx context.Context, path string) (bool, error) {
		calls++
		require.Equal(t, linked, path)
		if calls == 1 {
			mi.ExpectRepos(entries)
			_, err := mi.TrackRepoCtx(ctx, config.RepoEntry{Path: primary, Name: "loading"})
			require.NoError(t, err)
		}
		return false, nil // No authoritative checkout binding is available yet.
	}
	sess := &daemon.Session{ID: "registered-family-not-route", CWD: linked}
	reply := dispatchJSON(t, d, sess, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_symbols","arguments":{"query":"LoadProbe"}}}`)
	require.Equal(t, 2, calls, "a matched-ready identity permits only one ordinary authority recheck")
	data := errorData(t, reply)
	require.Equal(t, "view_building", data["error_code"])
	require.Equal(t, "family_pending", data["reason"])
	require.Equal(t, "discovery_pending", data["state"])
	require.NotContains(t, data, "repo_path")
	require.NotContains(t, reply, "result", "family identity alone grants no graph scope")
	// Retain the actual typed result before a later filesystem change. Metadata
	// rendering and graph refusal must not re-probe and become untracked.
	d.checkoutCWDProbe = nil
	_, err := d.cwdReachableChecked(t.Context(), linked)
	var classified *repoPendingAdmission
	require.ErrorAs(t, err, &classified)
	require.NotEmpty(t, classified.familyInstructions)
	require.NoError(t, os.Remove(filepath.Join(linked, ".git")))
	frame := []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"read","arguments":{}}}`)
	require.Contains(t, string(d.pendingRepoRefusal(sess, frame, classified)), `"error_code":"view_building"`)
	initialized := rewriteMCPAdmissionInstructions("initialize", []byte(`{"jsonrpc":"2.0","id":8,"result":{"instructions":"INACTIVE"}}`), classified.familyInstructions)
	require.Contains(t, string(initialized), "view_building")
	require.NotContains(t, string(initialized), "INACTIVE")
	unknown := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(unknown, ".git"), 0755))
	require.False(t, mi.RepoIdentityPending())
	outside := &daemon.Session{ID: "foreign-family", CWD: unknown}
	negative := dispatchJSON(t, d, outside, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"read","arguments":{}}}`)
	require.Equal(t, "repo_not_tracked", errorCode(negative), "a genuine foreign family stays definitively untracked after identity completion")
}

func TestExtractToolResult_ScopeBindingPendingDoesNotClaimTracking(t *testing.T) {
	_, err := extractToolResult([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"scope binding pending","data":{"error_code":"repo_loading","retryable":true,"reason":"scope_binding_pending","phase":"scope_binding","percent":0}}}`))
	var loading *repoLoadingError
	require.ErrorAs(t, err, &loading)
	require.Equal(t, exitTempFail, loading.ExitCode())
	require.Contains(t, err.Error(), "scope_binding_pending")
	require.NotContains(t, err.Error(), "already tracked")
	require.NotContains(t, err.Error(), "gortex track")
}
