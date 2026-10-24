package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestConfiguredStaleCatalogOwner(t *testing.T) {
	for _, scenario := range []string{"offline-untrack", "implicit-auto-index", "rename-owner", "online-track"} {
		t.Run(scenario, func(t *testing.T) {
			f := newLifecycleFixture(t)
			t.Cleanup(f.close)
			old, replacement := f.gitRepo("old"), f.gitRepo("replacement")
			entry := config.RepoEntry{Path: replacement, Name: "src"}
			if scenario == "implicit-auto-index" {
				_, err := f.mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: old, Name: "src"})
				require.NoError(t, err)
				require.NoError(t, f.lc.RecordImplicit(t.Context(), old))
			} else {
				result, err := f.lc.Register(t.Context(), config.RepoEntry{Path: old, Name: "src"}, TrackSourceCLI)
				require.NoError(t, err)
				require.NoError(t, result.CatalogErr)
			}
			oldGraph, found, err := f.catalog.GetDedicatedGraph(t.Context(), GraphIDFor("src"))
			require.NoError(t, err)
			require.True(t, found)
			_, published, err := f.catalog.DedicatedBasePublication(t.Context(), oldGraph.GraphID)
			require.NoError(t, err)
			t.Logf("old publication authority=%v", published)
			f.cm.Global().Repos = []config.RepoEntry{entry}
			if scenario == "rename-owner" {
				f.cm.Global().Repos = []config.RepoEntry{{Path: old, Name: "src-old"}, entry}
			}
			require.NoError(t, f.cm.Global().Save())
			f.restart()
			if scenario == "online-track" {
				result, err := f.lc.Register(t.Context(), entry, TrackSourceCLI)
				require.NoError(t, err)
				t.Logf("online catalog error=%v", result.CatalogErr)
				require.NoError(t, result.CatalogErr)
				f.restart()
			}
			entries := f.cm.Global().ReposSnapshot()
			selected := entries
			// Main dispatches config directly. The branch adds serial ownership selection.
			if selector, ok := any(f.mi).(interface {
				ResolveConfiguredRepos(context.Context, []config.RepoEntry) []config.RepoEntry
			}); ok {
				selected = selector.ResolveConfiguredRepos(t.Context(), entries)
			}
			require.Equal(t, entries, selected, "a stale owner must not hide configured roots")
			err = f.lc.Seed(t.Context())
			t.Logf("seed error=%v", err)
			if scenario == "rename-owner" {
				require.ErrorIs(t, err, graphview.ErrRepositoryAdmissionClosed)
			} else {
				require.NoError(t, err)
			}
			after, found, err := f.catalog.GetDedicatedGraph(t.Context(), oldGraph.GraphID)
			require.NoError(t, err)
			require.True(t, found)
			checkout, found, err := f.catalog.GetCheckout(t.Context(), after.OwnerCheckoutID)
			require.NoError(t, err)
			require.True(t, found)
			t.Logf("binding moved to replacement=%v", pathkey.SamePathIdentity(checkout.RootPath, pathkey.CanonicalPath(replacement)))
			assert.True(t, pathkey.EqualPaths(pathkey.CanonicalPath(replacement), checkout.RootPath), "checkout root %q", checkout.RootPath)
		})
	}
}

func TestSeedLegacyPrefixCollisionKeepsConfiguredOwner(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	owner, loser := f.gitRepo("owner"), f.gitRepo("loser")
	result, err := f.lc.Register(t.Context(), config.RepoEntry{Path: owner, Name: "src"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, result.CatalogErr)
	binding, found, err := f.catalog.GetDedicatedGraph(t.Context(), GraphIDFor("src"))
	require.NoError(t, err)
	require.True(t, found)
	// Explicitly leave a pre-publication row: generic upsert may rebind it on main.
	require.NoError(t, f.catalog.DeleteDedicatedGraph(t.Context(), binding.GraphID))
	binding.ActiveGenerationID = 0
	require.NoError(t, f.catalog.UpsertDedicatedGraph(t.Context(), binding))
	_, published, err := f.catalog.DedicatedBasePublication(t.Context(), binding.GraphID)
	require.NoError(t, err)
	require.False(t, published)
	f.cm.Global().Repos = []config.RepoEntry{{Path: loser, Name: "src"}, {Path: owner, Name: "src"}}
	require.NoError(t, f.cm.Global().Save())
	f.restart()
	require.NoError(t, f.lc.Seed(t.Context()))
	after, found, err := f.catalog.GetDedicatedGraph(t.Context(), binding.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, binding, after)
	families, err := f.catalog.ListRepositoryFamilies(t.Context())
	require.NoError(t, err)
	assert.Len(t, families, 1)
}

func TestPlainLinkedWorktreeDaemonPrefix(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	main := f.gitRepo("src")
	linked := filepath.Join(t.TempDir(), "src")
	runGit(t, main, "worktree", "add", "-q", "-b", "linked", linked)
	result, err := f.lc.Register(t.Context(), config.RepoEntry{Path: main}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, result.CatalogErr)
	result, err = f.lc.Register(t.Context(), config.RepoEntry{Path: linked}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, result.CatalogErr)
	assert.Equal(t, "src@src", result.Prefix)
}
