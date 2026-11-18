package config

import (
	"fmt"
	"github.com/zzet/gortex/internal/pathkey"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindRepoByPathUsesRootNotPrefix(t *testing.T) {
	owner, loser := t.TempDir(), t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(owner, alias))
	ownerEntry := RepoEntry{Path: alias, Name: "src", Exclude: []string{"owner-only"}}
	gc := &GlobalConfig{
		Repos:    []RepoEntry{{Path: loser, Name: "src"}},
		Projects: map[string]ProjectConfig{"project": {Repos: []RepoEntry{ownerEntry}}},
	}
	assert.Equal(t, &ownerEntry, gc.FindRepoByPath(owner))
	assert.Nil(t, gc.FindRepoByPath(loser+"2"), "a non-boundary path suffix must not match a configured root")
	assert.Nil(t, gc.FindRepoByPath(""))
	assert.Nil(t, (*GlobalConfig)(nil).FindRepoByPath(owner))
	gc.Repos = append(gc.Repos, ownerEntry)
	assert.Same(t, &gc.Repos[1], gc.FindRepoByPath(owner), "top-level entry keeps precedence over project membership")
}

func TestFindRepoByPathKeepsFirstAliasAndOfflineRoot(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(root, alias))
	gc := &GlobalConfig{Repos: []RepoEntry{{Path: alias, Name: "first"}, {Path: root, Name: "second"}}}
	assert.Same(t, &gc.Repos[0], gc.FindRepoByPath(root))
	missing := filepath.Join(root, "missing")
	gc.Repos = []RepoEntry{{Path: filepath.Join(alias, "missing"), Name: "offline"}}
	assert.Same(t, &gc.Repos[0], gc.FindRepoByPath(missing))
}

func TestSnapshotRepoPathsCanonicalizesConfiguredRootsOnce(t *testing.T) {
	gc := &GlobalConfig{}
	for i := 0; i < 200; i++ {
		gc.Repos = append(gc.Repos, RepoEntry{Path: filepath.Join(t.TempDir(), fmt.Sprint(i)), Name: fmt.Sprint(i)})
	}
	calls := 0
	lookup := gc.snapshotRepoPaths(func(root string) string {
		calls++
		return pathkey.CanonicalPath(root)
	})
	for _, entry := range gc.Repos {
		got := lookup(entry.Path)
		require.NotNil(t, got)
		assert.Equal(t, entry, *got)
	}
	assert.Equal(t, len(gc.Repos), calls, "canonicalize configured paths once, not once per lookup")
}

func TestSnapshotRepoPathsMatchesLiveLookup(t *testing.T) {
	owner, other := t.TempDir(), t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(owner, alias))
	gc := &GlobalConfig{Repos: []RepoEntry{{Path: alias, Name: "first"}, {Path: owner, Name: "second"}},
		Projects: map[string]ProjectConfig{"p": {Repos: []RepoEntry{{Path: other, Name: "project"}}}}}
	lookup := gc.SnapshotRepoPaths()
	for _, root := range []string{owner, alias, other, filepath.Join(owner, "missing")} {
		assert.Equal(t, gc.FindRepoByPath(root), lookup(root))
	}
	gc.Repos[0].Name = "changed"
	assert.Equal(t, "first", lookup(owner).Name, "lookup must retain the pass's configuration")
}

func TestSnapshotRepoPathsMissUsesCurrentConfig(t *testing.T) {
	gc := &GlobalConfig{Repos: []RepoEntry{{Path: t.TempDir(), Name: "original"}}}
	lookup := gc.SnapshotRepoPaths()
	added := t.TempDir()
	gc.Repos = append(gc.Repos, RepoEntry{Path: added, Name: "added-during-pass"})
	assert.Equal(t, gc.FindRepoByPath(added), lookup(added))
	assert.Equal(t, "added-during-pass", lookup(added).Name)
}

func TestRepoPathSnapshotReusesCanonicalRoots(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(root, alias))
	gc := &GlobalConfig{Repos: []RepoEntry{{Path: alias, Name: "src"}}}
	calls := 0
	canonicalize := func(path string) string { calls++; return pathkey.CanonicalPath(path) }
	snapshot := gc.repoPathSnapshot(gc.ReposSnapshot(), canonicalize)
	require.Equal(t, 1, calls)
	canonical := pathkey.CanonicalPath(root)
	for range 3 {
		require.Equal(t, canonical, snapshot.CanonicalRoot(alias, canonicalize))
		require.Equal(t, canonical, snapshot.CanonicalRoot(canonical, canonicalize))
		require.Equal(t, "src", snapshot.Lookup(alias).Name)
	}
	require.Equal(t, 1, calls, "both configured and canonical spellings reuse one canonicalization per revision")
	cm, err := NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
	require.NoError(t, err)
	require.NoError(t, cm.Global().AddRepo(gc.Repos[0]))
	first := cm.RepoPathSnapshot()
	require.Same(t, first, cm.RepoPathSnapshot())
	cm.LoadWorkspaceConfig("src", root)
	require.Same(t, first, cm.RepoPathSnapshot(), "workspace publication is not a repository revision")
	require.NoError(t, cm.Global().Save())
	require.NotSame(t, first, cm.RepoPathSnapshot())
	require.NoError(t, cm.Reload())
	require.NotSame(t, first, cm.RepoPathSnapshot())
}
