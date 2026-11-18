package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestRepositoryPrefixCollision(t *testing.T) {
	for _, reconcile := range []bool{false, true} {
		name := "track"
		if reconcile {
			name = "reconcile"
		}
		t.Run(name, func(t *testing.T) {
			first := setupRepoDir(t, "src")
			second := setupRepoDir(t, "src")
			writeFile(t, filepath.Join(first, ".gortex.yaml"), "workspace: owner\nexclude:\n  - owner-only\n")
			writeFile(t, filepath.Join(second, ".gortex.yaml"), "workspace: rejected\nexclude:\n  - rejected-only\n")
			writeFile(t, filepath.Join(second, "other.go"), "package main\nfunc Rejected() {}\n")
			g, mi, cm := newWorktreeTestIndexer(t)
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			result, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: first})
			require.NoError(t, err)
			require.NotNil(t, result)
			nodes, edges := g.AllNodes(), g.AllEdges()
			ownerConfig := cm.GetRepoConfig("src")
			ownerMeta := *mi.GetMetadata("src")
			revision := cm.Revision()
			entry := config.RepoEntry{Path: second}
			if reconcile {
				result, err = mi.ReconcileRepoCtx(t.Context(), entry, map[string]int64{"main.go": 1})
			} else {
				result, err = mi.TrackRepoCtx(t.Context(), entry)
			}
			if reconcile {
				var skipped *RepositoryCollisionSkip
				require.ErrorAs(t, err, &skipped, "reconcile must not certify a rejected root as a successful no-op")
			}
			var collision *PrefixCollisionError
			require.ErrorAs(t, err, &collision)
			assert.Equal(t, "src", collision.Prefix)
			assert.Equal(t, first, collision.ExistingPath)
			assert.Equal(t, second, collision.Path)
			assert.Contains(t, err.Error(), first)
			assert.Contains(t, err.Error(), "gortex track --name <other>")
			assert.Nil(t, result)
			assert.Len(t, mi.AllMetadata(), 1)
			assert.Equal(t, ownerMeta, *mi.GetMetadata("src"))
			assert.ElementsMatch(t, nodes, g.AllNodes())
			assert.ElementsMatch(t, edges, g.AllEdges())
			assert.Equal(t, ownerConfig, cm.GetRepoConfig("src"))
			assert.Equal(t, revision, cm.Revision(), "rejection must not publish workspace config")
			assert.Len(t, cm.Global().Repos, 1)

			if reconcile {
				result, err = mi.ReconcileRepoCtx(t.Context(), config.RepoEntry{Path: first}, nil)
			} else {
				result, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: first})
			}
			require.NoError(t, err)
			require.Nil(t, result, "same path remains a no-op")
			result, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: second, Name: "src2"})
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, "src2", result.RepoPrefix)
			assert.Len(t, mi.AllMetadata(), 2)
			assert.NotEmpty(t, g.GetFileNodes("src2/other.go"))
			assert.Empty(t, g.GetFileNodes("src/other.go"))
			assert.Equal(t, "owner", cm.GetRepoConfig("src").Workspace)

			// A plain re-track of the renamed repository stays a no-op.
			if reconcile {
				result, err = mi.ReconcileRepoCtx(t.Context(), config.RepoEntry{Path: second}, map[string]int64{"other.go": 1})
			} else {
				result, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: second})
			}
			require.NoError(t, err)
			require.Nil(t, result)
			assert.Len(t, mi.AllMetadata(), 2)
			assert.Equal(t, "owner", cm.GetRepoConfig("src").Workspace, "a no-op re-track must not touch the owner's config")
		})
	}
}

// A symlinked spelling of the owning directory is the same repository.
func TestRepositoryPrefixCollisionSymlinkAlias(t *testing.T) {
	first := setupRepoDir(t, "src")
	alias := filepath.Join(t.TempDir(), "src")
	if err := os.Symlink(first, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, mi, _ := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: first})
	require.NoError(t, err)
	result, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: alias})
	require.NoError(t, err)
	require.Nil(t, result)
	assert.Len(t, mi.AllMetadata(), 1)
}

// A loser that resolves its prefix after the owner published must not
// replace the owner's cached config.
func TestRepositoryPrefixCollisionLateLoserKeepsOwnerConfig(t *testing.T) {
	first, second := setupRepoDir(t, "src"), setupRepoDir(t, "src")
	writeFile(t, filepath.Join(first, ".gortex.yaml"), "workspace: owner\n")
	writeFile(t, filepath.Join(second, ".gortex.yaml"), "workspace: rejected\n")
	_, mi, cm := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: first})
	require.NoError(t, err)
	entry := config.RepoEntry{Path: second}
	prefix, _ := mi.resolveTrackPrefix(&entry, second, nil, nil, mi.configuredPrefixCheck())
	assert.Equal(t, "src", prefix)
	assert.Equal(t, "owner", cm.GetRepoConfig("src").Workspace)
}

func TestRepositoryPrefixCollisionWorktree(t *testing.T) {
	canon, wt := setupCollidingWorktree(t, "src", "task", "task-ws")
	_, mi, cm := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: canon})
	require.NoError(t, err)
	ownerConfig := cm.GetRepoConfig("src")
	result, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: wt})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "src@task-ws", result.RepoPrefix)
	assert.Len(t, mi.AllMetadata(), 2)
	assert.Equal(t, ownerConfig, cm.GetRepoConfig("src"))
}

// Keep the owner's last good parse even if its file disappears before rejection.
func TestRepositoryPrefixCollisionPreservesCachedConfig(t *testing.T) {
	first, second := setupRepoDir(t, "src"), setupRepoDir(t, "src")
	writeFile(t, filepath.Join(first, ".gortex.yaml"), "workspace: owner\n")
	writeFile(t, filepath.Join(second, ".gortex.yaml"), "workspace: rejected\n")
	_, mi, cm := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: first})
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(first, ".gortex.yaml")))
	_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: second})
	require.Error(t, err)
	assert.Equal(t, "owner", cm.GetRepoConfig("src").Workspace)
}

func TestRepositoryPrefixCollisionAfterCoordination(t *testing.T) {
	for _, reconcile := range []bool{false, true} {
		for _, samePath := range []bool{false, true} {
			t.Run(fmt.Sprintf("reconcile=%v/same=%v", reconcile, samePath), func(t *testing.T) {
				first, second := setupRepoDir(t, "src"), setupRepoDir(t, "src")
				if samePath {
					second = first
				}
				writeFile(t, filepath.Join(first, ".gortex.yaml"), "workspace: owner\n")
				if !samePath {
					writeFile(t, filepath.Join(second, ".gortex.yaml"), "workspace: rejected\n")
				}
				g, mi, cm := newWorktreeTestIndexer(t)
				t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
				// Admit the winner after the outer call resolved its prefix and took
				// its first registry snapshot, but before it acquires the stable lane.
				admitted := false
				coordinator := mi.repositoryMutationCoordinator("src")
				coordinator.batchAdmissionHook = func() {
					if admitted {
						return
					}
					admitted = true
					result, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: first})
					require.NoError(t, err)
					require.NotNil(t, result)
				}
				var result *IndexResult
				var err error
				if reconcile {
					result, err = mi.ReconcileRepoCtx(t.Context(), config.RepoEntry{Path: second}, map[string]int64{"main.go": 1})
				} else {
					result, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: second})
				}
				require.True(t, admitted)
				if samePath {
					require.NoError(t, err)
				} else {
					var collision *PrefixCollisionError
					require.ErrorAs(t, err, &collision)
					assert.Equal(t, first, collision.ExistingPath)
					if reconcile {
						var skipped *RepositoryCollisionSkip
						require.ErrorAs(t, err, &skipped, "the coordinated loser must remain a typed unchanged skip")
					}
				}
				assert.Nil(t, result)
				assert.Len(t, mi.AllMetadata(), 1)
				assert.Equal(t, first, mi.GetMetadata("src").RootPath)
				assert.Equal(t, "owner", cm.GetRepoConfig("src").Workspace)
				assert.NotEmpty(t, g.GetFileNodes("src/main.go"))
				assert.Len(t, cm.Global().Repos, 1)
			})
		}
	}
}

func TestRepositoryPrefixCollisionLifecycleHasNoSideEffects(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	first, second := setupRepoDir(t, "src"), setupRepoDir(t, "src")
	_, err := f.lc.Register(t.Context(), config.RepoEntry{Path: first}, TrackSourceCLI)
	require.NoError(t, err)
	before, err := os.ReadFile(f.cfgPath)
	require.NoError(t, err)
	calls := append([]string(nil), f.watcher.calls...)
	invalidated, analysed := f.notify.counts()
	result, err := f.lc.Register(t.Context(), config.RepoEntry{Path: second}, TrackSourceCLI)
	var collision *PrefixCollisionError
	require.ErrorAs(t, err, &collision)
	assert.Equal(t, RegisterResult{}, result)
	after, err := os.ReadFile(f.cfgPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Equal(t, calls, f.watcher.calls)
	afterInvalidated, afterAnalysed := f.notify.counts()
	assert.Equal(t, invalidated, afterInvalidated)
	assert.Equal(t, analysed, afterAnalysed)
}

func TestPromotionPrefixCollisionKeepsOtherCorpus(t *testing.T) {
	f := newFamilyFixture(t, "collision")
	t.Cleanup(f.close)
	prefix := f.mainPrefix + "@" + f.automatic.AdminName
	other := f.gitRepo("other-owner")
	f.lc.indexBarrier = func() {
		_, err := f.lc.Register(t.Context(), config.RepoEntry{Path: other, Name: prefix}, TrackSourceCLI)
		require.NoError(t, err)
	}
	result, err := f.lc.PromoteCheckout(t.Context(), f.automatic.CheckoutID, TrackSourceCLI)
	var collision *PrefixCollisionError
	require.ErrorAs(t, err, &collision)
	assert.True(t, result.Retryable)
	transition, found, journalErr := f.catalog.GetIntentTransition(t.Context(), f.automatic.CheckoutID)
	require.NoError(t, journalErr)
	require.True(t, found)
	assert.Contains(t, transition.LastError, "promotion will retry under a suffixed prefix")
	assert.NotContains(t, transition.LastError, "track --name")
	require.NotNil(t, f.mi.GetMetadata(prefix), "collision must not purge the other root")
	assert.Equal(t, other, f.mi.GetMetadata(prefix).RootPath)
	assert.NotEmpty(t, f.store.GetRepoNodes(prefix))
	got := f.checkoutOf(prefix).RootPath
	assert.True(t, pathkey.EqualPaths(pathkey.CanonicalExistingRoot(other), got), "checkout root %q", got)
}

func TestSeedPrefixCollisionKeepsCatalogOwner(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	first, second := f.gitRepo("first"), f.gitRepo("second")
	owner, err := f.lc.Register(t.Context(), config.RepoEntry{Path: first, Name: "src"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, owner.CatalogErr)
	before := f.familyOf("src")
	require.NoError(t, f.cm.Global().AddRepo(config.RepoEntry{Path: second, Name: "src"}))
	require.NoError(t, f.lc.Seed(t.Context()))
	assert.Equal(t, before, f.familyOf("src"))
	families, err := f.catalog.ListRepositoryFamilies(t.Context())
	require.NoError(t, err)
	assert.Len(t, families, 1, "rejected config must not mint a family")
	_, err = f.lc.PreviewUntrack(t.Context(), second)
	assert.ErrorIs(t, err, ErrCheckoutNotTracked, "the rejected root must not target the owner's corpus")
	assert.Equal(t, before, f.familyOf("src"))
	assert.NotEmpty(t, f.store.GetRepoNodes("src"))
}

func TestEffectiveRepoPrefixDoesNotPublishConfig(t *testing.T) {
	first, second := setupRepoDir(t, "src"), setupRepoDir(t, "src")
	writeFile(t, filepath.Join(first, ".gortex.yaml"), "workspace: owner\nexclude:\n  - owner-only\n")
	writeFile(t, filepath.Join(second, ".gortex.yaml"), "workspace: rejected\nexclude:\n  - rejected-only\n")
	_, mi, cm := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: first})
	require.NoError(t, err)
	before, revision := cm.GetRepoConfig("src"), cm.Revision()
	assert.Equal(t, "src", EffectiveRepoPrefix(cm, config.RepoEntry{Path: second}))
	assert.Equal(t, before, cm.GetRepoConfig("src"))
	assert.Equal(t, revision, cm.Revision())
}

func TestSeedPrefixCollisionKeepsPersistedOwner(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	first, second := f.gitRepo("first"), f.gitRepo("second")
	_, err := f.lc.Register(t.Context(), config.RepoEntry{Path: first, Name: "src"}, TrackSourceCLI)
	require.NoError(t, err)
	guardConfiguredOwner(t, f, "src")
	before := f.familyOf("src")
	f.cm.Global().Repos = []config.RepoEntry{{Path: second, Name: "src"}, {Path: first, Name: "src"}}
	require.NoError(t, f.cm.Global().Save())
	f.restart()
	require.Empty(t, f.mi.AllMetadata())
	require.NoError(t, f.lc.Seed(t.Context()))
	assert.Equal(t, before, f.familyOf("src"))
	families, err := f.catalog.ListRepositoryFamilies(t.Context())
	require.NoError(t, err)
	assert.Len(t, families, 1)
	// With the owner removed from config, its stale row no longer hides the replacement.
	f.cm.Global().Repos = []config.RepoEntry{{Path: second, Name: "src"}}
	assert.Len(t, f.mi.ResolveConfiguredRepos(t.Context(), f.cm.Global().Repos), 1)
}

func TestRepositoryPrefixPublicationRestoresOwnerConfig(t *testing.T) {
	for _, reconcile := range []bool{false, true} {
		t.Run(fmt.Sprint(reconcile), func(t *testing.T) {
			first, second := setupRepoDir(t, "src"), setupRepoDir(t, "src")
			writeFile(t, filepath.Join(first, ".gortex.yaml"), "workspace: owner\n")
			writeFile(t, filepath.Join(second, ".gortex.yaml"), "workspace: rejected\n")
			_, mi, cm := newWorktreeTestIndexer(t)
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			resolvedLoser := false
			mi.repositoryMutationCoordinator("src").batchAdmissionHook = func() {
				resolvedLoser = true
				// The winner has resolved, but is not yet published. Resolve
				// a competing root now so the publication must restore its config.
				entry := config.RepoEntry{Path: second}
				mi.resolveTrackPrefix(&entry, second, nil, nil, mi.configuredPrefixCheck())
				require.Equal(t, "rejected", cm.GetRepoConfig("src").Workspace)
			}
			var err error
			if reconcile {
				_, err = mi.ReconcileRepoCtx(t.Context(), config.RepoEntry{Path: first}, map[string]int64{"main.go": 1})
			} else {
				_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: first})
			}
			require.NoError(t, err)
			require.True(t, resolvedLoser)
			require.NotNil(t, mi.GetMetadata("src"))
			assert.Equal(t, first, mi.GetMetadata("src").RootPath)
			assert.Equal(t, "owner", cm.GetRepoConfig("src").Workspace)
		})
	}
}

func TestPrefixCollisionMissingOwnerSuggestsUntrack(t *testing.T) {
	first, second := setupRepoDir(t, "src"), setupRepoDir(t, "src")
	_, mi, _ := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: first})
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(first))
	_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: second})
	var collision *PrefixCollisionError
	require.ErrorAs(t, err, &collision)
	assert.Contains(t, err.Error(), "no longer exists")
	assert.Contains(t, err.Error(), "gortex untrack "+first)
	assert.NotContains(t, err.Error(), "track --name")
	assert.Len(t, mi.AllMetadata(), 1)
}

func TestRepositoryPrefixCollisionGitLifecycleHasNoSideEffects(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	first, second := f.gitRepo("first"), f.gitRepo("second")
	_, err := f.lc.Register(t.Context(), config.RepoEntry{Path: first, Name: "src"}, TrackSourceCLI)
	require.NoError(t, err)
	before := f.familyOf("src")
	_, err = f.lc.Register(t.Context(), config.RepoEntry{Path: second, Name: "src"}, TrackSourceCLI)
	var collision *PrefixCollisionError
	require.ErrorAs(t, err, &collision)
	assert.Equal(t, before, f.familyOf("src"))
	families, err := f.catalog.ListRepositoryFamilies(t.Context())
	require.NoError(t, err)
	assert.Len(t, families, 1)
}

func TestResolveConfiguredReposUsesOrderThenLiveOwner(t *testing.T) {
	first, second := setupRepoDir(t, "src"), setupRepoDir(t, "src")
	_, mi, _ := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	entries := []config.RepoEntry{{Path: first}, {Path: second}}
	selected := mi.ResolveConfiguredRepos(t.Context(), entries)
	require.Len(t, selected, 1)
	assert.Equal(t, first, selected[0].Path)
	assert.Empty(t, entries[0].Name, "selection must not mutate config")
	_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: second})
	require.NoError(t, err)
	selected = mi.ResolveConfiguredRepos(t.Context(), entries)
	require.Len(t, selected, 1)
	assert.Equal(t, second, selected[0].Path)
	selected = mi.ResolveConfiguredRepos(t.Context(), []config.RepoEntry{{Path: second, Name: "other"}})
	require.Len(t, selected, 1)
	assert.Equal(t, "src", selected[0].Name, "an already-loaded root keeps its registered prefix")
}

func TestPrefixCollisionErrorKeepsOwnerAvailabilitySnapshot(t *testing.T) {
	owner := t.TempDir()
	collision := NewPrefixCollisionError("src", owner, t.TempDir())
	before := collision.Error()
	require.NoError(t, os.RemoveAll(owner))
	assert.Equal(t, before, collision.Error(), "formatting must not consult the filesystem again")
	missing := NewPrefixCollisionError("src", owner, t.TempDir())
	assert.True(t, missing.OwnerMissing)
	assert.Contains(t, missing.Error(), "no longer exists")
}
