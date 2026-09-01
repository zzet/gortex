package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

const coldFanoutAutomaticWorktrees = 257

// TestCheckoutLifecycleColdStartupLargeWorktreeFamilyStaysDormant is the
// fleet-scale regression for the a6570f69 cold-start fanout. A clean catalog
// starts with one explicitly configured primary and 257 already-existing
// linked worktrees. Publishing the primary must inventory those worktrees, but
// it must not turn catalog discovery into 257 coordinators, source watchers,
// routes, or sparse builds.
//
// The long workspace debounce is a failure-containment guard, not a timing
// assertion. If eager admission regresses, the coordinators and source watcher
// registrations become visible synchronously, while their initial build
// signals remain inside the quiet window. The test therefore fails and closes
// them without reproducing the storage storm it is designed to prevent.
func TestCheckoutLifecycleColdStartupLargeWorktreeFamilyStaysDormant(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	const prefix = "cold-fanout-primary"
	main := f.gitRepo(prefix)
	writeColdFanoutWorkspaceDebounce(t, main, time.Hour)
	f.cm.LoadWorkspaceConfig(prefix, main)
	createColdFanoutWorktrees(t, f.dir, main, coldFanoutAutomaticWorktrees)
	inventory, err := gitstate.Inventory(ctx, main)
	require.NoError(t, err)
	require.Len(t, inventory.Records, coldFanoutAutomaticWorktrees+1,
		"Git itself must authoritatively enumerate the high-cardinality fixture")
	installColdFanoutReconciler(t, f, main, inventory)

	global := f.cm.Global()
	require.NoError(t, global.AddRepo(config.RepoEntry{Path: main, Name: prefix}))
	require.NoError(t, global.Save())
	require.Len(t, f.cm.RepoRegistrations(), 1,
		"the daemon readiness cohort input must contain only the configured primary")

	gate := NewViewBuildGate()
	f.lc.SetBuildGate(gate)
	var coldBaseBuilds atomic.Int32
	f.lc.indexBarrier = func() { coldBaseBuilds.Add(1) }

	require.NoError(t, f.lc.Seed(ctx))
	primary := f.familyOf(prefix)
	startupFamilies, startupOwners := coldFanoutStartupAdmissionCounts(f.lc, primary.FamilyID)
	require.Equal(t, 1, startupFamilies,
		"cold inventory must freeze one lifecycle admission family")
	require.Equal(t, 1, startupOwners,
		"automatic inventory must not join the configured primary's startup cohort")
	require.Zero(t, primary.ActiveGenerationID,
		"the required primary build must remain behind the closed gate")

	gate.OpenRequired()
	require.Eventually(t, func() bool {
		graph, found, err := f.catalog.GetDedicatedGraph(ctx, primary.GraphID)
		if err != nil || !found || graph.ActiveGenerationID <= 0 {
			return false
		}
		checkouts, err := f.catalog.ListCheckouts(ctx, graph.FamilyID)
		if err != nil || len(checkouts) != coldFanoutAutomaticWorktrees+1 {
			return false
		}
		_, transitionPending, err := f.catalog.GetIntentTransition(ctx, graph.OwnerCheckoutID)
		return err == nil && !transitionPending &&
			!f.lc.startupFamilyAdmissionPending(graph.FamilyID)
	}, 30*time.Second, 10*time.Millisecond,
		"required primary publication did not finish the large cold inventory")

	primary, found, err := f.catalog.GetDedicatedGraph(ctx, primary.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	require.Positive(t, primary.ActiveGenerationID)
	require.Equal(t, int32(1), coldBaseBuilds.Load(),
		"one configured primary must produce exactly one physical base build")

	automatic := coldFanoutAutomaticCheckouts(t, f.catalog, primary)
	require.Len(t, automatic, coldFanoutAutomaticWorktrees)
	automaticIDs := checkoutIDs(automatic)
	routes, err := f.catalog.GetCheckoutRoutes(ctx, automaticIDs)
	require.NoError(t, err)
	require.Empty(t, routes, "cold automatic worktrees must remain route-free")
	require.Equal(t, 1, f.lc.LiveCoordinators(primary.FamilyID),
		"only the configured primary may own a cold-start coordinator")
	require.Zero(t, checkoutSignalWatcherCount(f.lc),
		"dormant automatic worktrees must not own source watchers")

	stats := gate.Stats()
	require.True(t, stats.RequiredOpen)
	require.False(t, stats.Open)
	require.Positive(t, stats.AdmittedRequired)
	require.Zero(t, stats.RequiredQueued)
	require.Zero(t, stats.AdmittedBackground)
	require.Zero(t, stats.BackgroundQueued)
	require.Zero(t, stats.RejectedBackground)
	require.False(t, stats.Active)
	coldRequiredAdmissions := stats.AdmittedRequired
	coldBackgroundAdmissions := stats.AdmittedBackground
	assertColdFanoutHasOnlyPrimaryBase(t, f.catalog, primary, automaticIDs)
	coldGenerations := coldFanoutGenerationIDs(t, f.catalog, primary)

	// Selection is the one progress edge for a dormant automatic checkout.
	// Restore the normal quiet window before opening ordinary build admission.
	writeColdFanoutWorkspaceDebounce(t, main, time.Millisecond)
	f.cm.LoadWorkspaceConfig(primary.RepoPrefix, main)
	gate.Open()
	selected := automatic[0]
	require.True(t, f.lc.ActivateCheckout(selected.CheckoutID, "large-family selection"))
	require.Eventually(t, func() bool {
		route, found, err := f.catalog.GetCheckoutRoute(ctx, selected.CheckoutID)
		return err == nil && found && route.State == store_sqlite.RouteActive &&
			f.lc.LiveCoordinators(primary.FamilyID) == 2 &&
			checkoutSignalWatcherCount(f.lc) == 1
	}, 20*time.Second, 10*time.Millisecond,
		"targeted activation did not publish exactly one automatic checkout")

	routes, err = f.catalog.GetCheckoutRoutes(ctx, automaticIDs)
	require.NoError(t, err)
	require.Len(t, routes, 1)
	selectedRoute, routed := routes[selected.CheckoutID]
	require.True(t, routed)
	require.Equal(t, store_sqlite.RouteActive, selectedRoute.State)
	require.Positive(t, selectedRoute.CommitGenerationID)
	require.Zero(t, selectedRoute.DirtyGenerationID,
		"an unchanged selected checkout must not create a dirty generation")
	_, commitLayerReused := coldGenerations[selectedRoute.CommitGenerationID]
	require.True(t, commitLayerReused,
		"an unchanged selected checkout must reuse the already-built commit layer")
	for _, checkout := range automatic[1:] {
		require.Falsef(t, f.lc.SignalCheckout(checkout.CheckoutID, "dormant sibling probe"),
			"dormant sibling %s unexpectedly owns a coordinator", checkout.CheckoutID)
	}
	require.Equal(t, int32(1), coldBaseBuilds.Load(),
		"selected sparse activation must not rebuild the primary base")
	assertColdFanoutHasOnlyPrimaryBase(t, f.catalog, primary, automaticIDs)
	assertColdFanoutNoGenerationDelta(t, f.catalog, primary, coldGenerations)
	require.Eventually(t, func() bool {
		stats = gate.Stats()
		return !stats.Active && stats.RequiredQueued == 0 && stats.BackgroundQueued == 0
	}, 2*time.Second, 10*time.Millisecond, "selected build admission did not settle")
	require.Equal(t, coldRequiredAdmissions, stats.AdmittedRequired,
		"runtime selection must not expand the frozen required cohort")
	require.Equal(t, coldBackgroundAdmissions+1, stats.AdmittedBackground,
		"only the selected checkout may consume background build admission")
	require.Zero(t, stats.RejectedBackground)
}

func coldFanoutStartupAdmissionCounts(lifecycle *CheckoutLifecycle, familyID string) (int, int) {
	lifecycle.startupAdmissionMu.Lock()
	defer lifecycle.startupAdmissionMu.Unlock()
	return len(lifecycle.startupAdmissionFamilies), len(lifecycle.startupAdmissionFamilies[familyID])
}

func installColdFanoutReconciler(
	t *testing.T,
	f *lifecycleFixture,
	main string,
	inventory *gitstate.FamilyInventory,
) {
	t.Helper()
	mainHEAD, err := gitstate.SampleHEAD(context.Background(), main)
	require.NoError(t, err)
	mainRoot := filepath.Clean(main)
	// Pay once for Git's authoritative 258-record inventory, then replay that
	// immutable snapshot. The regression target is lifecycle fanout; repeating
	// Git's per-record filesystem annotation on every reconcile only bloats the
	// normal suite without changing the topology under test.
	inventorySnapshot := *inventory
	inventorySnapshot.Records = append([]gitstate.WorktreeRecord(nil), inventory.Records...)
	reconciler, err := reconcile.New(
		f.catalog,
		cleanupHooks{l: f.lc},
		lifecycleGrace,
		reconcile.WithClock(f.clock.Now),
		reconcile.WithLogger(zap.NewNop()),
		reconcile.WithInventory(func(context.Context, string) (*gitstate.FamilyInventory, error) {
			copy := inventorySnapshot
			copy.Records = append([]gitstate.WorktreeRecord(nil), inventorySnapshot.Records...)
			return &copy, nil
		}),
		reconcile.WithHEADSampler(func(_ context.Context, root string) (gitstate.HEADState, error) {
			if filepath.Clean(root) == mainRoot {
				return mainHEAD, nil
			}
			return gitstate.HEADState{
				Detached:  true,
				CommitOID: mainHEAD.CommitOID,
				TreeOID:   mainHEAD.TreeOID,
			}, nil
		}),
		reconcile.WithFamilyTopologyGuard(f.lc.reconcileCheckoutFamilyTopologyGuard),
		reconcile.WithCheckoutTopologyGuard(f.lc.reconcileCheckoutTopologyGuard),
	)
	require.NoError(t, err)
	f.lc.rec = reconciler
}

func createColdFanoutWorktrees(t *testing.T, parent, main string, count int) {
	t.Helper()
	commonDir := filepath.Join(main, ".git")
	adminRoot := filepath.Join(commonDir, "worktrees")
	head := builderGit(t, main, "rev-parse", "HEAD")
	for i := 0; i < count; i++ {
		adminName := fmt.Sprintf("cold-fanout-wt-%03d", i)
		root := filepath.Join(parent, adminName)
		adminDir := filepath.Join(adminRoot, adminName)
		require.NoError(t, os.MkdirAll(root, 0o755))
		require.NoError(t, os.MkdirAll(adminDir, 0o755))

		// This is the exact administrative shape produced by
		// `git worktree add --detach --no-checkout`: a per-worktree HEAD,
		// commondir and gitdir plus the root's .git pointer. Constructing the
		// administrative shape directly avoids 257 serialized Git writer processes,
		// while the Inventory assertion above proves Git accepts and enumerates
		// every checkout as a real linked worktree before Gortex sees it.
		require.NoError(t, os.WriteFile(
			filepath.Join(adminDir, "commondir"), []byte("../..\n"), 0o644,
		))
		require.NoError(t, os.WriteFile(
			filepath.Join(adminDir, "HEAD"), []byte(head+"\n"), 0o644,
		))
		require.NoError(t, os.WriteFile(
			filepath.Join(adminDir, "gitdir"), []byte(filepath.Join(root, ".git")+"\n"), 0o644,
		))
		require.NoError(t, os.WriteFile(
			filepath.Join(root, ".git"), []byte("gitdir: "+adminDir+"\n"), 0o644,
		))
	}
}

func writeColdFanoutWorkspaceDebounce(t testing.TB, root string, debounce time.Duration) {
	t.Helper()
	content := fmt.Sprintf("watch:\n  debounce_ms: %d\n", debounce.Milliseconds())
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gortex.yaml"), []byte(content), 0o644))
}

func coldFanoutAutomaticCheckouts(
	t testing.TB,
	catalog *store_sqlite.Catalog,
	primary store_sqlite.DedicatedGraph,
) []store_sqlite.Checkout {
	t.Helper()
	checkouts, err := catalog.ListCheckouts(context.Background(), primary.FamilyID)
	require.NoError(t, err)
	automatic := make([]store_sqlite.Checkout, 0, len(checkouts)-1)
	for _, checkout := range checkouts {
		if checkout.CheckoutID == primary.OwnerCheckoutID {
			continue
		}
		require.Equal(t, store_sqlite.CheckoutModeAutomatic, checkout.EffectiveMode)
		require.Equal(t, store_sqlite.CheckoutStateReady, checkout.State)
		automatic = append(automatic, checkout)
	}
	sort.Slice(automatic, func(i, j int) bool {
		return automatic[i].AdminName < automatic[j].AdminName
	})
	return automatic
}

func checkoutIDs(checkouts []store_sqlite.Checkout) []string {
	ids := make([]string, len(checkouts))
	for i := range checkouts {
		ids[i] = checkouts[i].CheckoutID
	}
	return ids
}

func assertColdFanoutHasOnlyPrimaryBase(
	t testing.TB,
	catalog *store_sqlite.Catalog,
	primary store_sqlite.DedicatedGraph,
	automaticIDs []string,
) {
	t.Helper()
	automatic := make(map[string]struct{}, len(automaticIDs))
	for _, checkoutID := range automaticIDs {
		automatic[checkoutID] = struct{}{}
	}
	generations, err := catalog.ListViewGenerations(context.Background(),
		store_sqlite.ViewGenerationFilter{GraphID: primary.GraphID})
	require.NoError(t, err)
	baseBuilds := 0
	for _, generation := range generations {
		if generation.OwnerKind == dedicatedBaseGenerationKind {
			baseBuilds++
		}
		_, automaticOwner := automatic[generation.CheckoutID]
		require.Falsef(t, automaticOwner,
			"dormant checkout %s unexpectedly owns generation %d",
			generation.CheckoutID, generation.GenerationID)
	}
	require.Equal(t, 1, baseBuilds)
}

func coldFanoutGenerationIDs(
	t testing.TB,
	catalog *store_sqlite.Catalog,
	primary store_sqlite.DedicatedGraph,
) map[int64]struct{} {
	t.Helper()
	generations, err := catalog.ListViewGenerations(context.Background(),
		store_sqlite.ViewGenerationFilter{GraphID: primary.GraphID})
	require.NoError(t, err)
	ids := make(map[int64]struct{}, len(generations))
	for _, generation := range generations {
		ids[generation.GenerationID] = struct{}{}
	}
	return ids
}

func assertColdFanoutNoGenerationDelta(
	t testing.TB,
	catalog *store_sqlite.Catalog,
	primary store_sqlite.DedicatedGraph,
	coldGenerations map[int64]struct{},
) {
	t.Helper()
	generations, err := catalog.ListViewGenerations(context.Background(),
		store_sqlite.ViewGenerationFilter{GraphID: primary.GraphID})
	require.NoError(t, err)
	for _, generation := range generations {
		_, existed := coldGenerations[generation.GenerationID]
		require.Truef(t, existed,
			"dormant sibling work unexpectedly produced generation %d",
			generation.GenerationID)
	}
	require.Len(t, generations, len(coldGenerations),
		"targeted warm reuse must not produce additional generations")
}

// BenchmarkApplyCoordinatorsColdDormantFleet measures the real batched
// catalog/admission path. The older predicate-only benchmark establishes the
// branch cost; this benchmark also pays for ListCheckouts, GetCheckoutRoutes,
// registry inspection, and report traversal at fleet cardinalities.
func BenchmarkApplyCoordinatorsColdDormantFleet(b *testing.B) {
	for _, checkouts := range []int{1, 64, 257, 1000} {
		b.Run(fmt.Sprintf("checkouts_%d", checkouts), func(b *testing.B) {
			ctx := context.Background()
			store, err := store_sqlite.Open(filepath.Join(b.TempDir(), "store.sqlite"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, store.Close()) })
			catalog := store.Catalog()
			const familyID = "benchmark-cold-dormant-family"
			now := time.Now().Unix()
			require.NoError(b, catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
				FamilyID:          familyID,
				CommonDirIdentity: "/benchmark/cold-dormant/.git",
				State:             reconcile.FamilyStateReady,
				CreatedAt:         now,
				LastSeen:          now,
			}))

			report := reconcile.FamilyReport{FamilyID: familyID}
			for i := 0; i < checkouts; i++ {
				checkoutID := fmt.Sprintf("checkout-%04d", i)
				require.NoError(b, catalog.AllocateCheckout(ctx, store_sqlite.Checkout{
					CheckoutID:     checkoutID,
					Incarnation:    "incarnation-" + checkoutID,
					FamilyID:       familyID,
					RootPath:       fmt.Sprintf("/benchmark/cold-dormant/worktree-%04d", i),
					GitDir:         fmt.Sprintf("/benchmark/cold-dormant/.git/worktrees/wt-%04d", i),
					AdminName:      fmt.Sprintf("wt-%04d", i),
					State:          store_sqlite.CheckoutStateReady,
					DesiredMode:    store_sqlite.CheckoutModeAutomatic,
					EffectiveMode:  store_sqlite.CheckoutModeAutomatic,
					HeadCommit:     "0123456789012345678901234567890123456789",
					HeadTree:       "1123456789012345678901234567890123456789",
					LastAccessible: now,
					LastSeen:       now,
				}))
				report.Checkouts = append(report.Checkouts, reconcile.CheckoutReport{
					CheckoutID:  checkoutID,
					Incarnation: "incarnation-" + checkoutID,
					Durable:     true,
					State:       store_sqlite.CheckoutStateReady,
					Action:      reconcile.ActionIdentityAllocated,
				})
			}

			lifecycle := &CheckoutLifecycle{
				store:                    store,
				catalog:                  catalog,
				logger:                   zap.NewNop(),
				coordinators:             map[string]*CheckoutCoordinator{},
				coordinatorHeads:         map[string]checkoutHeadIdentity{},
				startupAdmissionFamilies: map[string]map[string]struct{}{},
			}
			admissionCtx := withCoordinatorAdmissionPolicy(
				ctx, coordinatorAdmissionStartupRoutedOnly,
			)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				lifecycle.applyCoordinators(admissionCtx, report)
			}
			b.StopTimer()
			require.Zero(b, lifecycle.LiveCoordinators(familyID))
			routes, err := catalog.GetCheckoutRoutes(ctx, checkoutIDsFromReport(report))
			require.NoError(b, err)
			require.Empty(b, routes)
			b.ReportMetric(float64(checkouts), "dormant-checkouts/op")
			b.ReportMetric(0, "coordinators/op")
			b.ReportMetric(0, "routes/op")
		})
	}
}

func checkoutIDsFromReport(report reconcile.FamilyReport) []string {
	ids := make([]string, 0, len(report.Checkouts))
	for _, checkout := range report.Checkouts {
		ids = append(ids, checkout.CheckoutID)
	}
	return ids
}
