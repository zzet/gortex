package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
)

func TestRepoLoadAdmissionUsesOneSnapshotAcrossPublication(t *testing.T) {
	physical := t.TempDir()
	alias := filepath.Join(t.TempDir(), "configured-alias")
	require.NoError(t, os.Symlink(physical, alias))
	mi := newRepoLoadTestMulti(t)
	entries := []config.RepoEntry{{Path: alias}}
	mi.DeclareRepos(entries)
	before := mi.repoLoads.load()
	mi.ExpectRepos(entries)
	// Publication after an ownership miss must not combine the old miss with
	// the new all-resolved flag and produce a definitive untracked answer.
	pending, classified := before.admissionForPath(physical)
	require.True(t, classified)
	require.Equal(t, RepoLoadIdentityPending, pending.State)
	require.Empty(t, pending.Root)
	loading, classified := mi.RepoLoadAdmissionForPath(physical)
	require.True(t, classified)
	require.Equal(t, RepoLoadQueued, loading.State)
	require.Equal(t, alias, loading.Root)
	mi.markRepoRegistered("ready", alias)
	ready, classified := mi.RepoLoadAdmissionForPath(physical)
	require.True(t, classified)
	require.Equal(t, RepoLoadReady, ready.State, "final admission must expose a registration that raced positive checks")
	_, pendingOwnership := mi.PendingRepoForPath(physical)
	require.False(t, pendingOwnership, "existing ownership-only API must not reinterpret ready as loading")
}

func TestRepoLoadAdmissionRetainsReadyLinkedFamilyIdentity(t *testing.T) {
	root, linked := linkedWorktreeLayout(t)
	mi := newRepoLoadTestMulti(t)
	mi.ExpectRepos([]config.RepoEntry{{Path: root}})
	mi.markRepoRegistered("ready", root)
	rec, classified := mi.RepoLoadAdmissionForPath(linked)
	require.True(t, classified)
	require.Equal(t, RepoLoadReady, rec.State)
	require.Equal(t, root, rec.Root)
	_, pending := mi.PendingRepoForPath(linked)
	require.False(t, pending, "a ready family is identity evidence, not a pending ownership verdict")
	nested := filepath.Join(linked, "nested")
	require.NoError(t, os.MkdirAll(filepath.Join(nested, ".git"), 0755))
	_, classified = mi.RepoLoadAdmissionForPath(nested)
	require.False(t, classified, "a different nearest git identity cannot inherit ready family evidence")
	// A specific pending family match keeps its existing priority; adding ready
	// identity evidence must not displace it merely because ready was recorded first.
	loading := t.TempDir()
	admin := filepath.Join(root, ".git", "worktrees", "another")
	require.NoError(t, os.MkdirAll(admin, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(admin, "commondir"), []byte("../.."), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(loading, ".git"), []byte("gitdir: "+admin), 0644))
	mi.ExpectRepos([]config.RepoEntry{{Path: root}, {Path: loading, Name: "pending-family"}})
	rec, classified = mi.RepoLoadAdmissionForPath(linked)
	require.True(t, classified)
	require.Equal(t, RepoLoadQueued, rec.State)
	require.Equal(t, loading, rec.Root)
}
