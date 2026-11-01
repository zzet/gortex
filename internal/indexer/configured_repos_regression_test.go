package indexer

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
)

func TestConfiguredOwnerOfflineSymlink(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	root := f.gitRepo("offline")
	alias := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, os.Symlink(filepath.Dir(root), alias))
	entry := config.RepoEntry{Path: filepath.Join(alias, filepath.Base(root)), Name: "src"}
	registered, err := f.lc.Register(t.Context(), entry, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, registered.CatalogErr)
	family := f.familyOf("src").FamilyID
	require.NoError(t, os.RemoveAll(root))
	f.restart()
	core, logs := observer.New(zap.WarnLevel)
	f.mi.logger = zap.New(core)
	require.Empty(t, f.mi.AllMetadata())
	families := f.lc.knownFamilies(t.Context())
	require.Len(t, families, 1, "the offline owner must still be swept")
	assert.Equal(t, family, families[0].familyID)
	assert.Empty(t, logs.All(), "an offline alias must not collide with its own canonical root")
}

func TestConfiguredOwnerKeepsEntryOverrides(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	owner, loser := f.gitRepo("owner"), f.gitRepo("loser")
	writeFile(t, filepath.Join(owner, "owner-excluded.go"), "package main\nfunc OwnerExcluded() {}\n")
	writeFile(t, filepath.Join(owner, "loser-excluded.go"), "package main\nfunc OwnerIncluded() {}\n")
	ownerEntry := config.RepoEntry{Path: owner, Name: "src", Workspace: "owner-ws", Project: "owner-project", Exclude: []string{"owner-excluded.go"}}
	_, err := f.lc.Register(t.Context(), ownerEntry, TrackSourceCLI)
	require.NoError(t, err)
	guardConfiguredOwner(t, f, "src")
	f.cm.Global().Repos = []config.RepoEntry{
		{Path: loser, Name: "src", Workspace: "loser-ws", Project: "loser-project", Exclude: []string{"loser-excluded.go"}},
		ownerEntry,
	}
	require.NoError(t, f.cm.Global().Save())
	f.restart()
	selected := f.mi.ResolveConfiguredRepos(t.Context(), f.cm.Global().Repos)
	require.Len(t, selected, 1)
	require.Equal(t, owner, selected[0].Path)
	_, err = f.mi.TrackRepoCtx(t.Context(), selected[0])
	require.NoError(t, err)
	assert.Contains(t, f.cm.EffectiveExclude("src"), "owner-excluded.go")
	assert.NotContains(t, f.cm.EffectiveExclude("src"), "loser-excluded.go")
	assert.Empty(t, f.store.GetFileNodes("src/owner-excluded.go"))
	assert.NotEmpty(t, f.store.GetFileNodes("src/loser-excluded.go"))
	_, err = f.mi.IndexRepo("src")
	require.NoError(t, err)
	assert.Empty(t, f.store.GetFileNodes("src/owner-excluded.go"))
	nodes := f.store.GetFileNodes("src/loser-excluded.go")
	require.NotEmpty(t, nodes)
	for _, node := range nodes {
		assert.Equal(t, "owner-ws", node.WorkspaceID)
		assert.Equal(t, "owner-project", node.ProjectID)
	}
}

func TestConfiguredOwnerCatalogReadErrorFallsBack(t *testing.T) {
	for _, table := range []string{"dedicated_graphs", "checkouts"} {
		t.Run(table, func(t *testing.T) {
			f := newLifecycleFixture(t)
			t.Cleanup(f.close)
			root := f.gitRepo("owner")
			entry := config.RepoEntry{Path: root, Name: "src"}
			_, err := f.lc.Register(t.Context(), entry, TrackSourceCLI)
			require.NoError(t, err)
			guardConfiguredOwner(t, f, "src")
			f.restart()
			// Remove only the table read by this branch. The isolated fixture
			// keeps the real catalog and produces an actual SQLite read error.
			db, err := sql.Open("sqlite", f.dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			_, err = db.Exec("ALTER TABLE " + table + " RENAME TO unreadable_" + table)
			require.NoError(t, err)
			core, logs := observer.New(zap.WarnLevel)
			f.mi.logger = zap.New(core)
			selected := f.mi.ResolveConfiguredRepos(t.Context(), []config.RepoEntry{entry})
			require.Equal(t, []config.RepoEntry{entry}, selected, "a catalog read error must not drop config")
			require.Len(t, logs.All(), 1)
			assert.Contains(t, logs.All()[0].ContextMap()["error"], "no such table")
		})
	}
}

func TestConfiguredSameWorkspaceWorktrees(t *testing.T) {
	canonical, first := setupCollidingWorktree(t, "src", "first", "team")
	second := filepath.Join(t.TempDir(), "src")
	runGit(t, canonical, "worktree", "add", "-q", "-b", "second", second)
	writeFile(t, filepath.Join(second, ".gortex.yaml"), "workspace: team\n")
	_, mi, cm := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	entries := []config.RepoEntry{{Path: first}, {Path: second}}
	selected := mi.ResolveConfiguredRepos(t.Context(), entries)
	require.Len(t, selected, 2, "warmup must keep both worktree instances")
	assert.Equal(t, "src@team", selected[0].Name)
	assert.Equal(t, "src@team-"+shortPathHash(pathkey.CanonicalPath(second)), selected[1].Name)
	for _, entry := range selected {
		_, err := mi.TrackRepoCtx(t.Context(), entry)
		require.NoError(t, err)
	}
	assert.Len(t, mi.AllMetadata(), 2)
	assert.Equal(t, "team", cm.GetRepoConfig(selected[1].Name).Workspace)
	// Warm selection must preserve the registered suffix, not derive a new one.
	assert.Equal(t, selected, mi.ResolveConfiguredRepos(t.Context(), entries))
	require.NoError(t, cm.Global().Save())
	reloaded, err := config.NewConfigManager(cm.Global().ConfigPath())
	require.NoError(t, err)
	restarted := NewMultiIndexer(graph.New(), newTestRegistry(), search.NewNull(), reloaded, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, restarted.Close(context.Background())) })
	assert.Equal(t, selected, restarted.ResolveConfiguredRepos(t.Context(), reloaded.Global().ReposSnapshot()), "restart must keep the persisted names")
}

func TestConfiguredOwnerFirstWorkspaceConsumers(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	owner, loser := f.gitRepo("owner"), f.gitRepo("loser")
	writeFile(t, filepath.Join(owner, ".gortex.yaml"), `workspace: local-owner
cross_workspace_deps:
  - workspace: shared
    modules: [example.com/shared]
    mode: read-only
`)
	ownerEntry := config.RepoEntry{Path: owner, Name: "src", Workspace: "owner-ws", Project: "owner-project"}
	_, err := f.lc.Register(t.Context(), ownerEntry, TrackSourceCLI)
	require.NoError(t, err)
	f.cm.Global().Repos = []config.RepoEntry{
		ownerEntry,
		{Path: loser, Name: "src", Workspace: "loser-ws", Project: "loser-project"},
	}
	require.NoError(t, f.cm.Global().Save())
	f.restart()
	selected := f.mi.ResolveConfiguredRepos(t.Context(), f.cm.Global().Repos)
	require.Equal(t, []config.RepoEntry{ownerEntry}, selected)
	_, err = f.mi.TrackRepoCtx(t.Context(), selected[0])
	require.NoError(t, err)
	f.store.AddNode(&graph.Node{ID: "src/legacy.go::Legacy", Name: "Legacy", Kind: graph.KindFunction, FilePath: "src/legacy.go", RepoPrefix: "src"})
	f.mi.BackfillWorkspaceSlugs()
	node := f.store.GetNode("src/legacy.go::Legacy")
	require.NotNil(t, node)
	assert.Equal(t, "owner-ws", node.WorkspaceID)
	assert.Equal(t, "owner-project", node.ProjectID)
	rules, count := f.mi.collectCrossWorkspaceRules()
	assert.Equal(t, 1, count)
	assert.Equal(t, map[string][]resolver.CrossWorkspaceDepRule{
		"owner-ws": {{Workspace: "shared", Modules: []string{"example.com/shared"}}},
	}, rules)
}

func TestConfiguredCollisionWarnsOnceAndLiveRulesDoNotResolveOwners(t *testing.T) {
	owner, other := setupRepoDir(t, "src"), setupRepoDir(t, "src")
	_, mi, cm := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	cm.Global().Repos = []config.RepoEntry{{Path: owner}, {Path: other}}
	core, logs := observer.New(zap.WarnLevel)
	mi.logger = zap.New(core)
	mi.ResolveConfiguredRepos(t.Context(), cm.Global().Repos)
	mi.ResolveConfiguredRepos(t.Context(), cm.Global().Repos)
	require.Len(t, logs.FilterMessage("repository not loaded").All(), 1)
	mi.repos["src"] = &RepoMetadata{RepoPrefix: "src", RootPath: owner}
	cm.LoadWorkspaceConfig("src", owner)
	logs.TakeAll()
	mi.collectCrossWorkspaceRules()
	require.Empty(t, logs.All(), "live rule collection must not select or warn about configured owners")
}

func TestConfiguredOwnerRejectsTrackBeforeWarmup(t *testing.T) {
	for _, surface := range []string{"track", "register", "coordinated-recheck"} {
		t.Run(surface, func(t *testing.T) {
			f := newLifecycleFixture(t)
			t.Cleanup(f.close)
			owner, other := f.gitRepo("owner"), f.gitRepo("other")
			writeFile(t, filepath.Join(owner, "owner.go"), "package owner\nfunc Kept() {}\n")
			writeFile(t, filepath.Join(other, "other.go"), "package other\nfunc Rejected() {}\n")
			result, err := f.lc.Register(t.Context(), config.RepoEntry{Path: owner, Name: "src"}, TrackSourceCLI)
			require.NoError(t, err)
			require.NoError(t, result.CatalogErr)
			f.restart()
			entry := config.RepoEntry{Path: other, Name: "src"}
			var collision *PrefixCollisionError
			switch surface {
			case "track":
				_, err = f.mi.TrackRepoCtx(t.Context(), entry)
			case "register":
				_, err = f.lc.Register(t.Context(), entry, TrackSourceCLI)
			case "coordinated-recheck":
				f.cm.Global().Repos = nil
				require.NoError(t, f.cm.Global().Save())
				prepared, prepErr := f.mi.prepareTrackRepo(entry)
				require.NoError(t, prepErr)
				require.NotNil(t, prepared)
				f.cm.Global().Repos = []config.RepoEntry{{Path: owner, Name: "src"}}
				require.NoError(t, f.cm.Global().Save())
				_, err = f.mi.trackPreparedRepoSourceCtx(t.Context(), prepared, nil)
			}
			require.ErrorAs(t, err, &collision)
			assert.True(t, pathkey.SamePathIdentity(pathkey.CanonicalPath(collision.ExistingPath), pathkey.CanonicalPath(owner)))
			assert.NotEmpty(t, f.store.GetFileNodes("src/owner.go"))
			assert.Empty(t, f.store.GetFileNodes("src/other.go"))
		})
	}
}

func TestConfiguredLegacyWrongBindingUpgrade(t *testing.T) {
	for _, guard := range []string{"unpublished", "published", "live"} {
		t.Run(guard, func(t *testing.T) {
			f := newLifecycleFixture(t)
			t.Cleanup(f.close)
			owner, other := f.gitRepo("owner"), f.gitRepo("other")
			writeFile(t, filepath.Join(owner, "owner.go"), "package owner\nfunc Kept() {}\n")
			registered, err := f.lc.Register(t.Context(), config.RepoEntry{Path: owner, Name: "src"}, TrackSourceCLI)
			require.NoError(t, err)
			require.NoError(t, registered.CatalogErr)
			// Simulate main's last Seed: its metadata points at the last config
			// entry while the stored corpus still belongs to the first one.
			_, err = f.lc.recordCheckout(t.Context(), "other", other, TrackSourceConfig, true)
			require.NoError(t, err)
			otherBinding, found, err := f.catalog.GetDedicatedGraph(t.Context(), GraphIDFor("other"))
			require.NoError(t, err)
			require.True(t, found)
			binding, found, err := f.catalog.GetDedicatedGraph(t.Context(), GraphIDFor("src"))
			require.NoError(t, err)
			require.True(t, found)
			require.NoError(t, f.catalog.DeleteDedicatedGraph(t.Context(), binding.GraphID))
			require.NoError(t, f.catalog.DeleteDedicatedGraph(t.Context(), otherBinding.GraphID))
			binding.OwnerCheckoutID, binding.FamilyID = otherBinding.OwnerCheckoutID, otherBinding.FamilyID
			binding.ActiveGenerationID = 0
			require.NoError(t, f.catalog.UpsertDedicatedGraph(t.Context(), binding))
			f.cm.Global().Repos = []config.RepoEntry{{Path: owner, Name: "src"}, {Path: other, Name: "src"}}
			require.NoError(t, f.cm.Global().Save())
			if guard == "published" {
				checkout, found, err := f.catalog.GetCheckout(t.Context(), binding.OwnerCheckoutID)
				require.NoError(t, err)
				require.True(t, found)
				_, err = f.catalog.AcquireDedicatedBaseAuthority(t.Context(), store_sqlite.AcquireDedicatedBaseAuthorityRequest{
					GraphID: binding.GraphID, Owner: store_sqlite.DedicatedBaseOwner{CheckoutID: checkout.CheckoutID, Incarnation: checkout.Incarnation}, Token: "guarded",
				})
				require.NoError(t, err)
			}
			f.restart()
			if guard == "live" {
				f.mi.repos["src"] = &RepoMetadata{RepoPrefix: "src", RootPath: other}
			}
			core, logs := observer.New(zap.WarnLevel)
			f.lc.logger = zap.New(core)
			selected := f.mi.ResolveConfiguredRepos(t.Context(), f.cm.Global().ReposSnapshot())
			require.Len(t, selected, 1)
			want := owner
			if guard != "unpublished" {
				want = other
			}
			require.True(t, pathkey.SamePathIdentity(want, selected[0].Path))
			require.NoError(t, f.lc.Seed(t.Context()))
			after, found, err := f.catalog.GetDedicatedGraph(t.Context(), binding.GraphID)
			require.NoError(t, err)
			require.True(t, found)
			checkout, found, err := f.catalog.GetCheckout(t.Context(), after.OwnerCheckoutID)
			require.NoError(t, err)
			require.True(t, found)
			assert.True(t, pathkey.SamePathIdentity(pathkey.CanonicalPath(want), checkout.RootPath))
			assert.NotEmpty(t, f.store.GetFileNodes("src/owner.go"), "metadata repair must never purge corpus")
			warnings := logs.FilterMessage("repaired unpublished configured repository owner").All()
			if guard == "unpublished" {
				require.Len(t, warnings, 1)
				assert.Equal(t, "src", warnings[0].ContextMap()["prefix"])
				assert.True(t, pathkey.SamePathIdentity(pathkey.CanonicalPath(other), warnings[0].ContextMap()["old_root"].(string)))
				assert.True(t, pathkey.SamePathIdentity(pathkey.CanonicalPath(owner), warnings[0].ContextMap()["new_root"].(string)))
			} else {
				assert.Empty(t, warnings)
			}
		})
	}
}

func TestDedicatedGraphBindingRejectsDifferentOwner(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	owner, other := f.gitRepo("owner"), f.gitRepo("other")
	require.NoError(t, f.cm.Global().AddRepo(config.RepoEntry{Path: owner, Name: "src"}))
	require.NoError(t, f.cm.Global().Save())
	for i, root := range []string{owner, other} {
		name := []string{"src", "other"}[i]
		_, err := f.lc.recordCheckout(t.Context(), name, root, TrackSourceConfig, true)
		require.NoError(t, err)
	}
	first, found, err := f.catalog.GetDedicatedGraph(t.Context(), GraphIDFor("src"))
	require.NoError(t, err)
	require.True(t, found)
	second, found, err := f.catalog.GetDedicatedGraph(t.Context(), GraphIDFor("other"))
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, f.catalog.DeleteDedicatedGraph(t.Context(), second.GraphID))
	_, err = f.lc.bindDedicatedGraph(t.Context(), second.FamilyID, second.OwnerCheckoutID, "src")
	assert.ErrorIs(t, err, store_sqlite.ErrCatalogStaleGuard)
	after, found, err := f.catalog.GetDedicatedGraph(t.Context(), first.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, first, after)
}

func guardConfiguredOwner(t *testing.T, f *lifecycleFixture, prefix string) {
	t.Helper()
	binding, found, err := f.catalog.GetDedicatedGraph(t.Context(), GraphIDFor(prefix))
	require.NoError(t, err)
	require.True(t, found)
	owner, found, err := f.catalog.GetCheckout(t.Context(), binding.OwnerCheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	_, err = f.catalog.AcquireDedicatedBaseAuthority(t.Context(), store_sqlite.AcquireDedicatedBaseAuthorityRequest{
		GraphID: binding.GraphID, Owner: store_sqlite.DedicatedBaseOwner{CheckoutID: owner.CheckoutID, Incarnation: owner.Incarnation}, Token: "configured-owner",
	})
	require.NoError(t, err)
}

func TestKnownFamiliesSweepKeepsUnloadedCollisionInstances(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	var entries []config.RepoEntry
	for _, family := range []string{"one", "two"} {
		main := f.gitRepo(family)
		linked := filepath.Join(t.TempDir(), "src")
		runGit(t, main, "worktree", "add", "-q", "-b", "task", linked)
		writeFile(t, filepath.Join(linked, ".gortex.yaml"), "workspace: team\n")
		linked = pathkey.CanonicalExistingRoot(linked)
		result, err := f.lc.Register(t.Context(), config.RepoEntry{Path: linked}, TrackSourceCLI)
		require.NoError(t, err)
		require.NoError(t, result.CatalogErr)
		entries = append(entries, config.RepoEntry{Path: linked})
	}
	// Legacy config has no pinned instance names. Both entries declare
	// src@team, while the catalog retains the second instance's hash suffix.
	f.cm.Global().Repos = entries
	require.NoError(t, f.cm.Global().Save())
	f.restart()
	require.Empty(t, f.mi.AllMetadata(), "neither owner has been loaded yet")
	report, err := f.lc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, report.Families, "both dedicated families need their janitor pass")
}

func TestConfiguredWorktreeReconcileCanonicalizesRootsOnce(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	main := f.gitRepo("proj")
	worktrees := []string{f.worktreeOf(main, "one"), f.worktreeOf(main, "two")}
	entries := []config.RepoEntry{{Path: main}}
	prior := make(map[string]map[string]int64)
	for _, root := range worktrees {
		entries = append(entries, config.RepoEntry{Path: root, Name: "proj@" + filepath.Base(root)})
	}
	f.cm.Global().Repos = entries
	require.NoError(t, f.cm.Global().Save())
	for _, entry := range entries {
		_, err := f.mi.TrackRepoCtx(t.Context(), entry)
		require.NoError(t, err)
		prior[entry.Path] = f.mi.GetMetadata(config.ResolvePrefix(entry)).FileMtimes
	}
	f.restart()
	calls := 0
	f.mi.canonicalRoot = func(root string) string { calls++; return pathkey.CanonicalPath(root) }
	f.mi.BeginBatch()
	defer f.mi.EndBatch()
	for _, entry := range entries {
		before := calls
		_, err := f.mi.ReconcileRepoCtx(t.Context(), entry, prior[entry.Path])
		require.NoError(t, err)
		require.Equal(t, before, calls, "warm reconcile must reuse the per-revision canonical roots, including live family members")
	}
	require.Len(t, f.mi.AllMetadata(), len(entries))
}

func TestConfiguredPrefixCheckReusesResolutionUntilConfigChanges(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	owner, first, second := f.gitRepo("owner"), f.gitRepo("first"), f.gitRepo("second")
	_, err := f.mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: owner, Name: "src"})
	require.NoError(t, err)
	f.cm.Global().Repos = []config.RepoEntry{{Path: first, Name: "src"}, {Path: second, Name: "src"}}
	require.NoError(t, f.cm.Global().Save())
	calls := 0
	f.mi.prefixStats.stat = func(root string) (os.FileInfo, error) { calls++; return os.Stat(root) }
	check := f.mi.configuredPrefixCheck()
	require.NoError(t, check("src", second))
	firstCalls := calls
	require.Positive(t, firstCalls, "the non-configured live owner must be resolved")
	require.NoError(t, check("src", second))
	require.NoError(t, check("src", second))
	require.Equal(t, firstCalls, calls, "publication and coordinated rechecks reuse the same resolution")
	require.NoError(t, f.cm.Global().Save())
	require.NoError(t, check("src", second))
	require.Greater(t, calls, firstCalls, "a repository revision requires a fresh ownership decision")
}

func TestConfiguredPrefixCheckKeysArguments(t *testing.T) {
	for _, changed := range []string{"prefix", "root"} {
		t.Run(changed, func(t *testing.T) {
			f := newLifecycleFixture(t)
			t.Cleanup(f.close)
			owner, other := f.gitRepo("owner"), f.gitRepo("other")
			f.cm.Global().Repos = []config.RepoEntry{{Path: owner, Name: "src"}}
			require.NoError(t, f.cm.Global().Save())
			check := f.mi.configuredPrefixCheck()
			if changed == "prefix" {
				require.NoError(t, check("unused", other))
			} else {
				require.NoError(t, check("src", owner))
			}
			var collision *PrefixCollisionError
			require.ErrorAs(t, check("src", other), &collision)
			assert.Equal(t, "src", collision.Prefix)
			assert.Equal(t, owner, collision.ExistingPath)
			require.NoError(t, check("src", owner), "a cached refusal must not reject its owner")
		})
	}
}
