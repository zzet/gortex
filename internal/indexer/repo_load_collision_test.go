package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
)

func TestRepoLoadCollisionPrecedesQueuedStateFromOneSnapshot(t *testing.T) {
	owner, loser := filepath.Join(t.TempDir(), "src"), filepath.Join(t.TempDir(), "src")
	for _, root := range []string{owner, loser} {
		require.NoError(t, os.MkdirAll(root, 0o755))
	}
	mi := newRepoLoadTestMulti(t)
	mi.ExpectRepos([]config.RepoEntry{{Path: owner}, {Path: loser}})
	before := mi.repoLoads.load()
	mi.markRepoRegistered("src", owner)
	rec, ok := mi.RepoLoadFor(loser)
	require.True(t, ok)
	require.Equal(t, "prefix_collision", rec.ErrorCode)
	require.Equal(t, RepoLoadFailed, rec.State)
	require.Equal(t, owner, rec.OwnerPath)
	require.Equal(t, "present", rec.OwnerState)
	old, ok := before.admissionForPath(loser)
	require.True(t, ok)
	require.Empty(t, old.ErrorCode, "old readers cannot combine a later owner with their old identity set")
	raw := mi.repoLoads.load()
	i, ok := raw.find(loser)
	require.True(t, ok)
	require.Equal(t, RepoLoadQueued, raw.records[i].State, "the producer retains its actual queued state")
}

func TestRepoLoadCollisionDoesNotGuessAliasOrLinkedNaming(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	owner := f.gitRepo("src")
	linked := filepath.Join(t.TempDir(), "src")
	runGit(t, owner, "worktree", "add", "-q", "-b", "review", linked)
	f.mi.ExpectRepos([]config.RepoEntry{{Path: owner}, {Path: linked}})
	f.mi.markRepoRegistered("src", owner)
	rec, ok := f.mi.RepoLoadFor(linked)
	require.True(t, ok)
	require.Empty(t, rec.ErrorCode, "linked worktree basename cannot override lifecycle admin naming")
	alias := filepath.Join(t.TempDir(), "src")
	if err := os.Symlink(owner, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	f.mi.ExpectRepos([]config.RepoEntry{{Path: owner}, {Path: linked}, {Path: alias}})
	rec, ok = f.mi.RepoLoadFor(alias)
	require.True(t, ok)
	require.Empty(t, rec.ErrorCode, "a physical alias never collides with its own root")
}
