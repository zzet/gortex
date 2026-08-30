package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestCheckoutLifecycleReloadConfiguredLookupFailureFailsClosed(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	root := f.gitRepo("reload-fail-closed")
	gc := f.cm.Global()
	gc.Projects = map[string]config.ProjectConfig{
		"owner": {Repos: []config.RepoEntry{{Path: root}}},
	}
	require.NoError(t, gc.Save())

	initial, err := f.lc.ApplyReload(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, initial.Added)
	checkout := f.checkoutOf("reload-fail-closed")
	graph := f.familyOf("reload-fail-closed")

	injected := errors.New("injected configured checkout lookup failure")
	lookupCalls := 0
	f.lc.checkoutForPrefixHook = func(context.Context, string) (*store_sqlite.Checkout, error) {
		lookupCalls++
		// Only the configured-entry lookup fails. If reload incorrectly enters
		// the removal pass, the ordinary catalog lookup is available there and
		// exposes the destructive fail-open ordering deterministically.
		f.lc.checkoutForPrefixHook = nil
		return nil, injected
	}

	result, err := f.lc.ApplyReload(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, lookupCalls)
	require.Zero(t, result.Added)
	require.Zero(t, result.Removed)
	require.Zero(t, result.Pending)
	require.NotNil(t, f.mi.GetMetadata("reload-fail-closed"))
	require.Equal(t, graph, f.familyOf("reload-fail-closed"))
	assertConfiguredIntentActivity(t, f, checkout.CheckoutID, map[string]bool{
		"project:owner": true,
	})
}

func TestCheckoutLifecycleSeedRestoresIndependentSurfaceIntentWithoutConfig(t *testing.T) {
	for _, source := range []store_sqlite.IntentSourceKind{TrackSourceCLI, TrackSourceMCP} {
		t.Run(string(source), func(t *testing.T) {
			f := newLifecycleFixture(t)
			defer f.close()
			ctx := context.Background()

			root := f.gitRepo("cold-surface-owned")
			tracked, err := f.lc.Register(ctx,
				config.RepoEntry{Path: root, Name: "cold-surface-owned"}, source)
			require.NoError(t, err)
			checkout := f.checkoutOf(tracked.Prefix)
			graph := f.familyOf(tracked.Prefix)

			// Remove only config membership while the daemon is down. The durable
			// CLI/MCP intent remains authoritative and must restore the shell.
			gc := f.cm.Global()
			gc.Repos = nil
			gc.Projects = nil
			require.NoError(t, gc.Save())
			f.restart()
			require.Empty(t, f.mi.AllMetadata())

			require.NoError(t, f.lc.Seed(ctx))
			require.NotNil(t, f.mi.GetMetadata(tracked.Prefix))
			require.Equal(t, graph, f.familyOf(tracked.Prefix))
			restored := f.checkoutOf(tracked.Prefix)
			require.Equal(t, checkout.CheckoutID, restored.CheckoutID)

			intents, err := f.catalog.ListTrackingIntents(ctx, checkout.CheckoutID)
			require.NoError(t, err)
			active := 0
			for _, intent := range intents {
				if intent.Active {
					active++
					require.Equal(t, source, intent.SourceKind)
				}
			}
			require.Equal(t, 1, active)
		})
	}
}

func TestCheckoutLifecycleSeedProjectOnlyProvenanceColdWarm(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	root := f.gitRepo("project-only")
	alias := filepath.Join(f.dir, "project-only-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	gc := f.cm.Global()
	gc.Projects = map[string]config.ProjectConfig{
		"beta":  {Repos: []config.RepoEntry{{Path: root}}},
		"alpha": {Repos: []config.RepoEntry{{Path: alias}}},
	}
	require.NoError(t, gc.Save())

	coldBuilds := make(chan struct{}, 4)
	f.lc.indexBarrier = func() { coldBuilds <- struct{}{} }
	require.NoError(t, f.lc.Seed(ctx))
	require.Len(t, coldBuilds, 1,
		"two project references to one canonical checkout build one physical corpus")
	require.Len(t, f.mi.AllMetadata(), 1)
	require.Empty(t, f.cm.Global().Repos,
		"project-only startup must not manufacture a top-level manual-config source")

	first := f.checkoutOf("project-only")
	firstGraph := f.familyOf("project-only")
	firstIntents, err := f.catalog.ListTrackingIntents(ctx, first.CheckoutID)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{
		"project:alpha": true,
		"project:beta":  true,
	}, projectIntentActivity(firstIntents))
	for _, intent := range firstIntents {
		assert.Equal(t, store_sqlite.IntentSourceProjectMembership, intent.SourceKind)
	}

	f.restart()
	warmBuilds := make(chan struct{}, 4)
	f.lc.indexBarrier = func() { warmBuilds <- struct{}{} }
	require.NoError(t, f.lc.Seed(ctx))
	require.Empty(t, warmBuilds, "warm project-only startup reuses the published corpus")
	require.Empty(t, f.cm.Global().Repos,
		"warm restoration must retain project-only provenance")

	second := f.checkoutOf("project-only")
	secondGraph := f.familyOf("project-only")
	assert.Equal(t, first.CheckoutID, second.CheckoutID)
	assert.Equal(t, firstGraph.GraphID, secondGraph.GraphID)
	assert.Equal(t, firstGraph.ActiveGenerationID, secondGraph.ActiveGenerationID)
	secondIntents, err := f.catalog.ListTrackingIntents(ctx, second.CheckoutID)
	require.NoError(t, err)
	require.Len(t, secondIntents, 2, "warm replay upserts sources instead of duplicating them")
	require.Equal(t, map[string]bool{
		"project:alpha": true,
		"project:beta":  true,
	}, projectIntentActivity(secondIntents))
}

func TestCheckoutLifecycleReloadRetainsUntilLastConfiguredSource(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	main := f.gitRepo("project-family-main")
	target := f.worktreeOf(main, "project-family-target")
	targetAlias := filepath.Join(f.dir, "project-family-target-alias")
	if err := os.Symlink(target, targetAlias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	gc := f.cm.Global()
	gc.Repos = []config.RepoEntry{
		{Path: main, Name: "project-family-main"},
		{Path: target, Name: "project-family-target"},
	}
	gc.Projects = map[string]config.ProjectConfig{
		"alpha": {Repos: []config.RepoEntry{{Path: targetAlias, Name: "project-family-target"}}},
		"beta":  {Repos: []config.RepoEntry{{Path: target, Name: "project-family-target"}}},
	}
	require.NoError(t, gc.Save())

	added, err := f.lc.ApplyReload(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, added.Added,
		"global and project aliases schedule one physical target registration")
	require.Len(t, f.mi.AllMetadata(), 2)
	targetCheckout := f.checkoutOf("project-family-target")
	targetGraph := f.familyOf("project-family-target")
	require.False(t, targetGraph.IsPrimaryBase)
	assertConfiguredIntentActivity(t, f, targetCheckout.CheckoutID, map[string]bool{
		targetCheckout.RootPath: true,
		"project:alpha":         true,
		"project:beta":          true,
	})

	// Removing alpha withdraws only alpha's provenance. The physical graph and
	// process-local registration remain untouched because two sources remain.
	gc = f.cm.Global()
	gc.Projects["alpha"] = config.ProjectConfig{}
	require.NoError(t, gc.Save())
	require.NoError(t, f.cm.Reload())
	oneRemoved, err := f.lc.ApplyReload(ctx)
	require.NoError(t, err)
	require.Zero(t, oneRemoved.Added)
	require.Zero(t, oneRemoved.Removed)
	require.Zero(t, oneRemoved.Pending)
	require.NotNil(t, f.mi.GetMetadata("project-family-target"))
	require.Equal(t, targetGraph, f.familyOf("project-family-target"))
	assertConfiguredIntentActivity(t, f, targetCheckout.CheckoutID, map[string]bool{
		targetCheckout.RootPath: true,
		"project:alpha":         false,
		"project:beta":          true,
	})

	// Removing the top-level reference still leaves beta as an independent
	// explicit owner, so reload must not retire or demote the checkout.
	gc = f.cm.Global()
	gc.Repos = []config.RepoEntry{{Path: main, Name: "project-family-main"}}
	require.NoError(t, gc.Save())
	require.NoError(t, f.cm.Reload())
	twoRemoved, err := f.lc.ApplyReload(ctx)
	require.NoError(t, err)
	require.Zero(t, twoRemoved.Added)
	require.Zero(t, twoRemoved.Removed)
	require.Zero(t, twoRemoved.Pending)
	require.NotNil(t, f.mi.GetMetadata("project-family-target"))
	assertConfiguredIntentActivity(t, f, targetCheckout.CheckoutID, map[string]bool{
		targetCheckout.RootPath: false,
		"project:alpha":         false,
		"project:beta":          true,
	})

	// Once the last source disappears, the ordinary reload retirement policy
	// applies. This non-primary worktree demotes to the surviving primary.
	gc = f.cm.Global()
	gc.Projects["beta"] = config.ProjectConfig{}
	require.NoError(t, gc.Save())
	require.NoError(t, f.cm.Reload())
	lastRemoved, err := f.lc.ApplyReload(ctx)
	require.NoError(t, err)
	require.Zero(t, lastRemoved.Added)
	require.Zero(t, lastRemoved.Removed)
	require.Zero(t, lastRemoved.Pending)
	require.Nil(t, f.mi.GetMetadata("project-family-target"))
	_, ownsGraph, err := f.catalog.GetDedicatedGraph(ctx, targetGraph.GraphID)
	require.NoError(t, err)
	require.False(t, ownsGraph, "last source retirement removes only the target dedicated graph")
	demoted, found, err := f.catalog.GetCheckout(ctx, targetCheckout.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, store_sqlite.CheckoutModeAutomatic, demoted.EffectiveMode)
	assertConfiguredIntentActivity(t, f, targetCheckout.CheckoutID, map[string]bool{
		targetCheckout.RootPath: false,
		"project:alpha":         false,
		"project:beta":          false,
	})
}

func TestCheckoutLifecycleReloadPreservesIndependentSurfaceIntent(t *testing.T) {
	for _, source := range []store_sqlite.IntentSourceKind{TrackSourceCLI, TrackSourceMCP} {
		t.Run(string(source), func(t *testing.T) {
			f := newLifecycleFixture(t)
			defer f.close()
			ctx := context.Background()

			root := f.gitRepo("surface-owned")
			tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: root, Name: "surface-owned"}, source)
			require.NoError(t, err)
			checkout := f.checkoutOf(tracked.Prefix)
			graph := f.familyOf(tracked.Prefix)

			// Add a second independent project source and let reload materialize
			// the full provenance set for the already-built physical checkout.
			gc := f.cm.Global()
			gc.Projects = map[string]config.ProjectConfig{
				"temporary": {Repos: []config.RepoEntry{{Path: root, Name: tracked.Prefix}}},
			}
			require.NoError(t, gc.Save())
			require.NoError(t, f.cm.Reload())
			_, err = f.lc.ApplyReload(ctx)
			require.NoError(t, err)

			// Remove every config-owned source. The CLI/MCP intent is still an
			// active reason, so reload must not demote or retire the graph.
			gc = f.cm.Global()
			gc.Repos = nil
			gc.Projects = nil
			require.NoError(t, gc.Save())
			require.NoError(t, f.cm.Reload())
			result, err := f.lc.ApplyReload(ctx)
			require.NoError(t, err)
			require.Zero(t, result.Added)
			require.Zero(t, result.Removed)
			require.Zero(t, result.Pending)
			require.NotNil(t, f.mi.GetMetadata(tracked.Prefix))
			require.Equal(t, graph, f.familyOf(tracked.Prefix))

			intents, err := f.catalog.ListTrackingIntents(ctx, checkout.CheckoutID)
			require.NoError(t, err)
			activeByKind := map[store_sqlite.IntentSourceKind]int{}
			for _, intent := range intents {
				if intent.Active {
					activeByKind[intent.SourceKind]++
				}
			}
			require.Equal(t, map[store_sqlite.IntentSourceKind]int{source: 1}, activeByKind,
				"only the independent surface intent remains active")
		})
	}
}

func TestCheckoutLifecycleSeedWithdrawsProjectRemovedWhileOffline(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	main := f.gitRepo("offline-main")
	target := f.worktreeOf(main, "offline-project-target")
	gc := f.cm.Global()
	gc.Repos = []config.RepoEntry{{Path: main, Name: "offline-main"}}
	gc.Projects = map[string]config.ProjectConfig{
		"temporary": {Repos: []config.RepoEntry{{Path: target, Name: "offline-project-target"}}},
	}
	require.NoError(t, gc.Save())
	require.NoError(t, f.lc.Seed(ctx))

	targetCheckout := f.checkoutOf("offline-project-target")
	targetGraph := f.familyOf("offline-project-target")
	require.False(t, targetGraph.IsPrimaryBase)
	assertConfiguredIntentActivity(t, f, targetCheckout.CheckoutID, map[string]bool{
		"project:temporary": true,
	})

	// The membership disappears while the daemon is down. A warm Seed must
	// reconcile the durable catalog against the new snapshot, rather than
	// preserving a ghost project intent forever.
	gc = f.cm.Global()
	gc.Projects = nil
	require.NoError(t, gc.Save())
	f.restart()
	require.NoError(t, f.lc.Seed(ctx))

	_, ownsGraph, err := f.catalog.GetDedicatedGraph(ctx, targetGraph.GraphID)
	require.NoError(t, err)
	require.False(t, ownsGraph, "offline removal retires the non-primary dedicated graph")
	demoted, found, err := f.catalog.GetCheckout(ctx, targetCheckout.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, store_sqlite.CheckoutModeAutomatic, demoted.EffectiveMode)
	require.Nil(t, f.mi.GetMetadata("offline-project-target"))
	assertConfiguredIntentActivity(t, f, targetCheckout.CheckoutID, map[string]bool{
		"project:temporary": false,
	})
	require.NotNil(t, f.mi.GetMetadata("offline-main"),
		"offline project removal must not disturb the surviving primary")
	require.Empty(t, f.cm.Global().Projects)
	require.Len(t, f.cm.Global().Repos, 1)
}

func TestCheckoutLifecycleSeedRetainsProjectIntentForUnresolvedAlias(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	realRoot := f.gitRepo("temporarily-unresolved-real")
	alias := filepath.Join(f.dir, "temporarily-unresolved-alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	gc := f.cm.Global()
	gc.Projects = map[string]config.ProjectConfig{
		"retained": {Repos: []config.RepoEntry{{Path: alias}}},
	}
	require.NoError(t, gc.Save())
	require.NoError(t, f.lc.Seed(ctx))

	prefix := filepath.Base(realRoot)
	checkout := f.checkoutOf(prefix)
	graph := f.familyOf(prefix)
	require.NoError(t, os.Remove(alias), "make only the configured spelling unavailable")
	f.restart()
	require.NoError(t, f.lc.Seed(ctx))

	require.Equal(t, graph, f.familyOf(prefix),
		"an unresolved configured alias is not authoritative membership removal")
	assertConfiguredIntentActivity(t, f, checkout.CheckoutID, map[string]bool{
		"project:retained": true,
	})
	_, transition, err := f.catalog.GetIntentTransition(ctx, checkout.CheckoutID)
	require.NoError(t, err)
	require.False(t, transition, "temporary alias loss must not schedule retirement")
}

func projectIntentActivity(intents []store_sqlite.TrackingIntent) map[string]bool {
	out := make(map[string]bool, len(intents))
	for _, intent := range intents {
		out[intent.SourceLocator] = intent.Active
	}
	return out
}

func assertConfiguredIntentActivity(
	t *testing.T,
	f *lifecycleFixture,
	checkoutID string,
	want map[string]bool,
) {
	t.Helper()
	intents, err := f.catalog.ListTrackingIntents(context.Background(), checkoutID)
	require.NoError(t, err)
	require.Equal(t, want, projectIntentActivity(intents))
}
