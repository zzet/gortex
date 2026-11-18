package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestSnapshotRepoPathsRetainsProjectDuringUntrack(t *testing.T) {
	top, project := t.TempDir(), t.TempDir()
	gc := &GlobalConfig{
		Repos:    []RepoEntry{{Path: top, Name: "top"}},
		Projects: map[string]ProjectConfig{"p": {Repos: []RepoEntry{{Path: project, Name: "project"}}}},
	}
	gc.SetConfigPath(filepath.Join(t.TempDir(), "global.yaml"))
	require.NoError(t, gc.Save())
	removed := false
	lookup := gc.snapshotRepoPaths(func(root string) string {
		if !removed {
			var err error
			removed, err = gc.RemoveRepoAndSaveIfPresent(project)
			require.NoError(t, err)
			require.True(t, removed)
		}
		return pathkey.CanonicalPath(root)
	})
	require.Nil(t, gc.FindRepoByPath(project), "untrack must update live intent")
	got := lookup(project)
	require.NotNil(t, got, "warmup must retain the project entry captured before untrack")
	assert.Equal(t, "project", got.Name)
	assert.Equal(t, "top", lookup(top).Name)
}

func TestPrepareWorkspaceConfigMalformedNewPrefixMatchesPublished(t *testing.T) {
	root := t.TempDir()
	cm, err := NewConfigManager(filepath.Join(t.TempDir(), "global.yaml"))
	require.NoError(t, err)
	path := filepath.Join(root, ".gortex.yaml")
	require.NoError(t, os.WriteFile(path, []byte("workspace: team\nexclude:\n  - kept\n"), 0o644))
	cm.LoadWorkspaceConfig("src", root)
	revision := cm.Revision()
	prepared, publish := cm.PrepareWorkspaceConfig("src", root, nil)
	publish("src")
	assert.Equal(t, prepared, cm.GetRepoConfig("src"), "unchanged config must stay equivalent")
	assert.Equal(t, revision, cm.Revision(), "unchanged publication must not advance revision")
	require.NoError(t, os.WriteFile(path, []byte("workspace: ["), 0o644))
	prepared, publish = cm.PrepareWorkspaceConfig("src", root, nil)
	require.Equal(t, "team", prepared.Workspace)
	publish("src@team")
	cached := cm.GetRepoConfig("src@team")
	assert.Equal(t, prepared.Workspace, cached.Workspace, "prepared and published workspace must agree after rekey")
	assert.Equal(t, prepared.Index.Exclude, cached.Index.Exclude)
}

func TestPrepareWorkspaceConfigSnapshotOverridesMergedCache(t *testing.T) {
	root := t.TempDir()
	cm, err := NewConfigManager(filepath.Join(t.TempDir(), "global.yaml"))
	require.NoError(t, err)
	gc := cm.Global()
	gc.Repos = []RepoEntry{{Path: root, Name: "src", Exclude: []string{"snapshot-only"}}}
	cm.LoadWorkspaceConfig("src", root)
	lookup := gc.SnapshotRepoPaths()
	gc.Repos[0].Exclude = []string{"live-only"}
	live := cm.GetRepoConfig("src")
	require.Contains(t, live.Index.Exclude, "live-only")
	prepared, _ := cm.PrepareWorkspaceConfig("src", root, lookup)
	assert.Contains(t, prepared.Index.Exclude, "snapshot-only")
	assert.NotContains(t, prepared.Index.Exclude, "live-only")
	assert.Equal(t, live, cm.GetRepoConfig("src"), "snapshot preparation must not poison the live cache")
	fresh, _ := cm.PrepareWorkspaceConfig("src", root, gc.SnapshotRepoPaths())
	assert.Equal(t, live, fresh, "a no-change snapshot must match live config")
}

func TestSnapshotRepoPathsMissReturnsDetachedEntry(t *testing.T) {
	gc := &GlobalConfig{}
	lookup := gc.SnapshotRepoPaths()
	root := t.TempDir()
	require.NoError(t, gc.AddRepo(RepoEntry{Path: root, Name: "late"}))
	entry := lookup(root)
	require.NotNil(t, entry)
	globalConfigMu.Lock()
	gc.Repos[0] = RepoEntry{Path: root, Name: "replacement"}
	globalConfigMu.Unlock()
	assert.Equal(t, "late", entry.Name, "a returned snapshot entry must not alias a mutable live slice")
	assert.Equal(t, "replacement", lookup(root).Name, "the next miss must see current config")
}

func TestEffectiveExcludeForRootKeepsRegisteredRepoOverrides(t *testing.T) {
	registered, checkout := t.TempDir(), t.TempDir()
	cm, err := NewConfigManager(filepath.Join(t.TempDir(), "global.yaml"))
	require.NoError(t, err)
	cm.Global().Repos = []RepoEntry{{Path: registered, Name: "src", Exclude: []string{"selected/secrets/**"}}}
	cm.LoadWorkspaceConfig("src", registered)
	require.NoError(t, os.WriteFile(filepath.Join(checkout, ".gitignore"), []byte("checkout-only\n"), 0o644))
	patterns := cm.EffectiveExcludeForRoot("src", checkout)
	assert.Contains(t, patterns, "selected/secrets/**")
	assert.Contains(t, patterns, "checkout-only")
}

func TestPrepareWorkspaceConfigMalformedKeepsFinalPrefix(t *testing.T) {
	root := t.TempDir()
	cm, err := NewConfigManager(filepath.Join(t.TempDir(), "global.yaml"))
	require.NoError(t, err)
	path := filepath.Join(root, ".gortex.yaml")
	require.NoError(t, os.WriteFile(path, []byte("exclude: [kept]\n"), 0o644))
	cm.LoadWorkspaceConfig("src@team", root)
	require.NoError(t, os.WriteFile(path, []byte("workspace: ["), 0o644))
	_, publish := cm.PrepareWorkspaceConfig("src", root, nil)
	publish("src@team")
	assert.Contains(t, cm.GetRepoConfig("src@team").Index.Exclude, "kept")
}
