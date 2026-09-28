package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// The first edit after the committed base is published lands.
//
// The daemon publishes a family's committed base after warm-up; from then on
// the linked worktree's primary base is that base, while its route still
// names a commit layer over generation zero until its coordinator recomposes
// the route. An edit selected in that window is refused at admission as a
// moved route, and an immediate reselection returns the same route. The edit
// must ask the coordinator to recompose, wait for it, and be admitted against
// the recomposed route: the first edit after the publication is written and
// served, not refused.
func TestFirstEditAfterTheCommittedBasePublishesLands(t *testing.T) {
	fixture, publisher, prefix := newBasePublishingMutationFixture(t)
	ctx := context.Background()
	before, _, err := fixture.store.Catalog().GetCheckoutRoute(ctx, fixture.checkoutID)
	require.NoError(t, err)

	published := false
	previous := mutationBeforeAdmission
	mutationBeforeAdmission = func(context.Context) {
		if published {
			return
		}
		published = true
		out := publisher.PublishRepo(ctx, prefix)
		require.NoError(t, out.Err)
		require.Empty(t, out.Skipped, "the committed base was not published: %s", out.Skipped)
		require.Positive(t, out.GenerationID)
	}
	t.Cleanup(func() { mutationBeforeAdmission = previous })

	written := fixture.edit(t, fixture.worktree, map[string]any{
		"path": "repo/edit.go", "old_string": "func New() {}", "new_string": "func AfterThePublication() {}",
	})
	require.True(t, published, "the base publication was never injected")
	require.False(t, written.IsError, viewResultText(t, written))
	data, err := os.ReadFile(filepath.Join(fixture.worktree, "edit.go"))
	require.NoError(t, err)
	require.Contains(t, string(data), "func AfterThePublication() {}")
	after, _, err := fixture.store.Catalog().GetCheckoutRoute(ctx, fixture.checkoutID)
	require.NoError(t, err)
	require.NotEqual(t, before.RouteEpoch, after.RouteEpoch, "the route was never recomposed over the published base")
	fixture.awaitMutation(t, fixture.worktree, written)
}

// newBasePublishingMutationFixture is newRealCheckoutMutationFixture with the
// dedicated base runtime mounted the way the daemon mounts it (before any
// owner registers) and an initial base publisher over it, so a test can
// publish the family's committed base at a moment of its choosing.
func newBasePublishingMutationFixture(t *testing.T) (*realCheckoutMutationFixture, *indexer.InitialBasePublisher, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	primary := filepath.Join(base, "repo")
	worktree := filepath.Join(base, "wt")
	require.NoError(t, os.Mkdir(primary, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(primary, "edit.go"), []byte("package repo\n\nfunc Old() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(primary, ".gortex.yaml"), []byte("workspace: checkout-mutations\n"), 0o644))
	checkoutMutationGit(t, primary, "init", "--initial-branch=main")
	checkoutMutationGit(t, primary, "add", "-A")
	checkoutMutationGit(t, primary, "commit", "-m", "base")
	checkoutMutationGit(t, primary, "worktree", "add", "-b", "feature", worktree)
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "edit.go"), []byte(primitiveWorktreeSource), 0o644))

	store, err := store_sqlite.Open(filepath.Join(base, "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	cfgPath := filepath.Join(base, "config.yaml")
	global := &config.GlobalConfig{}
	global.SetConfigPath(cfgPath)
	require.NoError(t, global.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	bm := search.NewNull()
	mi := indexer.NewMultiIndexer(store, registry, bm, cm, zap.NewNop())
	leases := graphview.NewLeaseManager()
	lifecycle, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{
		MultiIndexer: mi, ConfigManager: cm, Graph: store,
		Logger: zap.NewNop(), ViewLeases: leases,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lifecycle.Close() })
	runtime, err := indexer.NewDedicatedBaseRuntime(store, lifecycle.ViewLeases())
	require.NoError(t, err)
	require.NoError(t, lifecycle.SetDedicatedBaseCleanupRuntime(runtime))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	registered, err := lifecycle.Register(ctx, config.RepoEntry{Path: primary, Name: "repo"}, indexer.TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, registered.CatalogErr)
	overview, err := lifecycle.FamiliesOverview(ctx, registered.FamilyID)
	require.NoError(t, err)
	checkoutID := ""
	for _, family := range overview.Families {
		for _, checkout := range family.Checkouts {
			if pathkey.EqualPaths(checkout.RootPath, worktree) {
				checkoutID = checkout.CheckoutID
			}
		}
	}
	require.NotEmpty(t, checkoutID, "linked worktree %q must be discovered through registration", worktree)
	require.True(t, lifecycle.ActivateCheckout(checkoutID, "mutation-test"))
	require.Eventually(t, func() bool {
		route, found, routeErr := store.Catalog().GetCheckoutRoute(ctx, checkoutID)
		return routeErr == nil && found && route.State == store_sqlite.RouteActive &&
			route.CommitGenerationID > 0 && route.DirtyGenerationID > 0
	}, 30*time.Second, 20*time.Millisecond, "automatic worktree did not publish its initial route")

	publisher, err := indexer.NewInitialBasePublisher(lifecycle)
	require.NoError(t, err)
	t.Cleanup(publisher.Close)

	engine := query.NewEngine(store)
	engine.SetSearch(bm)
	srv := NewServer(engine, store, nil, nil, zap.NewNop(), nil, MultiRepoOptions{MultiIndexer: mi, ConfigManager: cm})
	srv.SetMaterializer(&graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: leases})
	srv.lifecycle = lifecycle
	return &realCheckoutMutationFixture{
		srv: srv, store: store, primary: primary, worktree: worktree, checkoutID: checkoutID,
	}, publisher, registered.Prefix
}
