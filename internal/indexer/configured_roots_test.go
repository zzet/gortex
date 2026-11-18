package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
)

func TestConfiguredRepoRootsRefreshesAfterTrackUntrackAndReload(t *testing.T) {
	cm := newTestConfigManager(t)
	mi := &MultiIndexer{configMgr: cm}
	first, second, reloaded := t.TempDir(), t.TempDir(), t.TempDir()
	gc := cm.Global()
	require.NoError(t, gc.AddRepo(config.RepoEntry{Path: first}))
	require.Equal(t, []string{first}, mi.ConfiguredRepoRoots())
	require.NoError(t, gc.RemoveRepo(first))
	require.NoError(t, gc.AddRepo(config.RepoEntry{Path: second}))
	require.Equal(t, []string{second}, mi.ConfiguredRepoRoots())
	fresh := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: reloaded}}}
	fresh.SetConfigPath(gc.ConfigPath())
	require.NoError(t, fresh.Save())
	require.NoError(t, cm.Reload())
	require.Equal(t, []string{reloaded}, mi.ConfiguredRepoRoots(), "must use the new config pointer, not startup declarations")
}
