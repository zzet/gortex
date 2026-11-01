package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
)

func TestRegisterPoisonedProbeKeepsConfig(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	root := f.gitRepo("safe-primary")
	tracked, err := f.lc.Register(context.Background(), config.RepoEntry{Path: root, Name: "safe-primary"}, TrackSourceCLI)
	require.NoError(t, err)
	f.worktreeOf(root, "0lane")
	_, err = f.lc.ReconcileFamily(context.Background(), tracked.FamilyID)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(root, ".git", "worktrees", "0lane", "HEAD")))
	report, err := f.lc.ReconcileFamily(context.Background(), tracked.FamilyID)
	require.NoError(t, err)
	require.True(t, report.InventoryUsable, "a damaged linked checkout must not be the only family probe")
	require.Contains(t, f.configPaths(), root)
}

func TestInRepoLaneForgetKeepsPrimaryWatcher(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	root := f.gitRepo("watched-primary")
	tracked, err := f.lc.Register(context.Background(), config.RepoEntry{Path: root, Name: "watched-primary"}, TrackSourceCLI)
	require.NoError(t, err)
	lane := filepath.Join(root, ".lanes", "nested")
	runGit(t, root, "worktree", "add", "-q", "-b", "nested", lane)
	_, err = f.lc.ReconcileFamily(context.Background(), tracked.FamilyID)
	require.NoError(t, err)
	checkouts, err := f.catalog.ListCheckouts(context.Background(), tracked.FamilyID)
	require.NoError(t, err)
	found := false
	for _, checkout := range checkouts {
		if checkout.AdminName != "nested" {
			continue
		}
		found = true
		require.NoError(t, f.lc.Reconciler().ForgetCheckout(context.Background(), checkout.CheckoutID, checkout.Incarnation))
	}
	require.True(t, found)
	require.True(t, f.watcher.isAttached(tracked.Prefix), "forgetting a nested automatic lane detached the primary watcher")
}

func TestApplyReloadCanonicalRootNoRedundantRegister(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	root := f.gitRepo("reload-root")
	_, err := f.lc.Register(context.Background(), config.RepoEntry{Path: root, Name: "reload-root"}, TrackSourceCLI)
	require.NoError(t, err)
	alias := filepath.Join(f.dir, "root-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	for i := range f.cm.Global().Repos {
		if f.cm.Global().Repos[i].Path == root {
			f.cm.Global().Repos[i].Path = alias
		}
	}
	require.NoError(t, f.cm.Global().Save())
	result, err := f.lc.ApplyReload(context.Background())
	require.NoError(t, err)
	require.Zero(t, result.Added, "an alias of an already tracked root was registered again")
	require.Zero(t, result.Removed)
}
