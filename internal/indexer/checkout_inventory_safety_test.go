package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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
	mw, err := NewMultiWatcher(f.mi, map[string]config.WatchConfig{tracked.Prefix: {Enabled: true, DebounceMs: 10}}, zap.NewNop())
	require.NoError(t, err)
	f.lc.SetWatcherSource(func() RepoWatcher { return mw })
	require.NoError(t, mw.Start())
	defer func() { require.NoError(t, mw.Stop()) }()
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
		require.Empty(t, f.lc.prefixForCheckout(context.Background(), checkout.CheckoutID))
		require.NoError(t, f.lc.Reconciler().ForgetCheckout(context.Background(), checkout.CheckoutID, checkout.Incarnation))
	}
	require.True(t, found)
	mw.mu.Lock()
	_, attached := mw.watchers[tracked.Prefix]
	mw.mu.Unlock()
	require.True(t, attached, "forgetting a nested automatic lane detached the primary watcher")
	runGit(t, root, "worktree", "remove", "--force", lane)
	require.NoError(t, os.WriteFile(filepath.Join(root, "watched-primary.go"), []byte("package a\n\nfunc AfterLaneRemoval() {}\n"), 0o600))
	require.Eventually(t, func() bool {
		for _, node := range f.store.GetFileNodes(tracked.Prefix + "/watched-primary.go") {
			if node.Name == "AfterLaneRemoval" {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "primary edit was not indexed after lane removal")
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

func TestPrimaryUnavailableRecovery(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	root := f.gitRepo("recover-primary")
	tracked, err := f.lc.Register(context.Background(), config.RepoEntry{Path: root, Name: "recover-primary"}, TrackSourceCLI)
	require.NoError(t, err)
	held := root + "-away"
	require.NoError(t, os.Rename(root, held))
	_, err = f.lc.ReconcileFamily(context.Background(), tracked.FamilyID)
	require.NoError(t, err)
	f.clock.advance(lifecycleGrace.AvailabilityGrace + time.Second)
	_, err = f.lc.ReconcileFamily(context.Background(), tracked.FamilyID)
	require.NoError(t, err)
	row, found, err := f.catalog.GetCheckout(context.Background(), tracked.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, store_sqlite.CheckoutStateUnavailable, row.State)
	require.Contains(t, f.configPaths(), root)
	require.NoError(t, os.WriteFile(filepath.Join(held, "recover-primary.go"), []byte("package a\n\nfunc EditedWhileAway() {}\n"), 0o600))
	require.NoError(t, os.Rename(held, root))
	_, err = f.lc.ReconcileFamily(context.Background(), tracked.FamilyID)
	require.NoError(t, err)
	require.True(t, f.watcher.isAttached(tracked.Prefix), "recovery did not reattach the watcher")
	require.Eventually(t, func() bool {
		for _, node := range f.store.GetFileNodes(tracked.Prefix + "/recover-primary.go") {
			if node.Name == "EditedWhileAway" {
				return true
			}
		}
		return false
	}, time.Second, 10*time.Millisecond, "offline edit was not indexed on recovery")
	row, found, err = f.catalog.GetCheckout(context.Background(), tracked.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, store_sqlite.CheckoutStateReady, row.State)
}

func TestConfigRemovalCauseLogsOnce(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	f := newLifecycleFixture(t, zap.New(core))
	defer f.close()
	root := f.gitRepo("logged-untrack")
	tracked, err := f.lc.Register(context.Background(), config.RepoEntry{Path: root, Name: "logged-untrack"}, TrackSourceCLI)
	require.NoError(t, err)
	_, err = f.lc.Untrack(context.Background(), tracked.Prefix)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(f.configPaths()) == 0 }, 10*time.Second, 10*time.Millisecond)
	removals := logs.FilterMessage("config: removed tracked repo").All()
	require.Len(t, removals, 1)
	require.Equal(t, "untrack", removals[0].ContextMap()["cause"])
	require.NotEmpty(t, removals[0].ContextMap()["caller"])
	_, _ = f.lc.Untrack(context.Background(), tracked.Prefix)
	require.Len(t, logs.FilterMessage("config: removed tracked repo").All(), 1, "idempotent untrack logged another removal")
}
