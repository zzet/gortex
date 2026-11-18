package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTrackCandidatesAreLexicalImmutableAndRevisionScoped(t *testing.T) {
	cm, err := NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
	require.NoError(t, err)
	root := filepath.Join(t.TempDir(), "offline", "src") // deliberately absent
	require.NoError(t, cm.Global().AddRepo(RepoEntry{Path: root, Name: "owner", Exclude: []string{"kept"}}))
	first := cm.RepoTrackSnapshot()
	require.Same(t, first, cm.RepoTrackSnapshot())
	entry := first.RootCandidates(filepath.Join(t.TempDir(), "src"))
	require.Len(t, entry, 1)
	require.Equal(t, root, entry[0].Path)
	entry[0].Name, entry[0].Exclude[0] = "changed", "changed"
	require.Equal(t, "owner", first.PrefixCandidates("owner")[0].Name)
	require.Equal(t, []string{"kept"}, first.RootCandidates(root)[0].Exclude)
	require.Nil(t, cm.pathSnapshot, "admission must not build the eager canonical-root snapshot")
	require.NoError(t, cm.Global().AddRepo(RepoEntry{Path: filepath.Join(t.TempDir(), "next")}))
	require.NotSame(t, first, cm.RepoTrackSnapshot())
	require.Empty(t, first.PrefixCandidates("next"), "old snapshots stay immutable")
	require.NoError(t, cm.Global().AddRepo(RepoEntry{Path: filepath.Join(t.TempDir(), "src@component")}))
	require.Len(t, cm.RepoTrackSnapshot().InstanceCandidates("src@component@review"), 1)
}
