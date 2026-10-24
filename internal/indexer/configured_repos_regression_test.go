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
