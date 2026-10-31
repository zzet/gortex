package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

func TestIndexMultiRepoWithoutConfigManager(t *testing.T) {
	mi := newColdOrchestrationMulti(graph.New(), zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	results, err := mi.indexFixtureRepos(nil)
	require.NoError(t, err)
	assert.Empty(t, results)
	repos := coldOrchestrationRepos(t, 1)
	results, err = mi.indexFixtureRepos(repos)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.NotNil(t, mi.GetMetadata(config.ResolvePrefix(repos[0])))
}

func TestEffectiveRepoPrefixKeepsLastGoodWorkspace(t *testing.T) {
	_, wt := setupCollidingWorktree(t, "src", "task", "team")
	_, mi, cm := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	cm.LoadWorkspaceConfig("src", wt)
	revision := cm.Revision()
	entry := config.RepoEntry{Path: wt}
	assert.Equal(t, "src@team", EffectiveRepoPrefix(cm, entry))
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".gortex.yaml"), []byte("workspace: ["), 0o644))
	assert.Equal(t, "src@team", EffectiveRepoPrefix(cm, entry), "transient malformed config must keep the last good workspace")
	assert.Equal(t, "team", cm.GetRepoConfig("src").Workspace)
	assert.Equal(t, revision, cm.Revision(), "prefix lookup must not publish config")
	assert.Equal(t, "custom", EffectiveRepoPrefix(cm, config.RepoEntry{Path: wt, Name: "custom"}))
	assert.Equal(t, "src@override", EffectiveRepoPrefix(cm, config.RepoEntry{Path: wt, Workspace: "override"}))
}

func TestResolveTrackPrefixMalformedRekeyKeepsConfigConsistent(t *testing.T) {
	root := setupRepoDir(t, "src")
	_, mi, cm := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	path := filepath.Join(root, ".gortex.yaml")
	require.NoError(t, os.WriteFile(path, []byte("workspace: team\nexclude:\n  - kept\n"), 0o644))
	cm.LoadWorkspaceConfig("src", root)
	require.NoError(t, os.WriteFile(path, []byte("workspace: ["), 0o644))
	entry := config.RepoEntry{Path: root, AsWorktree: true}
	prefix, cfg := mi.resolveTrackPrefix(&entry, root, nil, nil, mi.configuredPrefixCheck())
	require.Equal(t, "src@team", prefix)
	cached := cm.GetRepoConfig(prefix)
	assert.Equal(t, cfg.Workspace, cached.Workspace)
	assert.Equal(t, cfg.Index.Exclude, cached.Index.Exclude)
}

func TestEffectiveRepoPrefixMalformedOtherRootKeepsOwner(t *testing.T) {
	owner, other := setupRepoDir(t, "src"), setupRepoDir(t, "src")
	_, mi, cm := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	writeFile(t, filepath.Join(owner, ".gortex.yaml"), "workspace: team\n")
	cm.LoadWorkspaceConfig("src", owner)
	revision := cm.Revision()
	writeFile(t, filepath.Join(other, ".gortex.yaml"), "workspace: [")
	want, _ := WorktreeInstanceName(other, "src", "", true)
	assert.Equal(t, want, EffectiveRepoPrefix(cm, config.RepoEntry{Path: other, AsWorktree: true}))
	assert.Equal(t, "team", cm.GetRepoConfig("src").Workspace)
	assert.Equal(t, revision, cm.Revision())
}
