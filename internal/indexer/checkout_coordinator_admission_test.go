package indexer

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

func TestApplyCoordinatorsWaitsForPublishedDedicatedBase(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()
	root := f.gitRepo("coordinator-publication-gate")
	const prefix = "coordinator-publication-gate"

	identity, err := f.lc.recordCheckout(ctx, prefix, root, TrackSourceConfig, true)
	require.NoError(t, err)
	checkout, found, err := f.catalog.GetCheckout(ctx, identity.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	graph, found, err := f.catalog.GetDedicatedGraph(ctx, identity.graphID)
	require.NoError(t, err)
	require.True(t, found)
	require.Zero(t, graph.ActiveGenerationID, "recording creates only a provisional graph shell")

	report := reconcile.FamilyReport{
		FamilyID:       checkout.FamilyID,
		PrimaryGraphID: graph.GraphID,
		Checkouts: []reconcile.CheckoutReport{{
			CheckoutID: checkout.CheckoutID,
			Durable:    true,
			State:      checkout.State,
		}},
	}
	f.lc.applyCoordinators(ctx, report)
	f.lc.coordMu.Lock()
	provisional := f.lc.coordinators[checkout.CheckoutID]
	f.lc.coordMu.Unlock()
	assert.Nil(t, provisional, "a generation-zero graph must not start a coordinator")

	result, err := f.lc.PromoteCheckout(ctx, checkout.CheckoutID, TrackSourceConfig)
	require.NoError(t, err)
	require.Equal(t, graph.GraphID, result.GraphID)
	published, found, err := f.catalog.GetDedicatedGraph(ctx, graph.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	require.Positive(t, published.ActiveGenerationID)

	// Promotion installs the owner coordinator itself. Drop it so this assertion
	// exercises topology admission, not the transition's direct install path.
	f.lc.dropCoordinator(checkout.CheckoutID)
	checkout, found, err = f.catalog.GetCheckout(ctx, checkout.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	report.Checkouts[0].State = checkout.State
	f.lc.applyCoordinators(ctx, report)
	f.lc.coordMu.Lock()
	active := f.lc.coordinators[checkout.CheckoutID]
	f.lc.coordMu.Unlock()
	require.NotNil(t, active, "the published exact base admits its coordinator")
	assert.True(t, active.Running())
}

func TestRollbackStopsOnlyCoordinatorBoundToProvisionalGraph(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()
	root := f.gitRepo("rollback-provisional-coordinator")
	const prefix = "rollback-provisional-coordinator"

	identity, err := f.lc.recordCheckout(ctx, prefix, root, TrackSourceConfig, true)
	require.NoError(t, err)
	checkout, found, err := f.catalog.GetCheckout(ctx, identity.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	graph, found, err := f.catalog.GetDedicatedGraph(ctx, identity.graphID)
	require.NoError(t, err)
	require.True(t, found)
	require.Zero(t, graph.ActiveGenerationID)
	require.NoError(t, f.lc.ensurePromotedRepoShell(ctx, checkout, prefix))

	// Model the exact old race: topology installs a loop after transient graph
	// binding but before publication, then the promotion rolls back.
	coordinator, err := f.lc.buildCoordinatorWithPoll(ctx, graph.GraphID, checkout, -time.Nanosecond)
	require.NoError(t, err)
	require.NotNil(t, coordinator)
	require.True(t, f.lc.installCoordinatorAtHead(checkout, coordinator))
	require.True(t, coordinator.Running())

	require.NoError(t, f.lc.rollbackPromotion(
		ctx, prefix, graph.GraphID, checkout.CheckoutID, checkout.Incarnation,
	))
	assert.False(t, coordinator.Running(), "rollback closes the raced provisional coordinator")
	f.lc.coordMu.Lock()
	registered := f.lc.coordinators[checkout.CheckoutID]
	f.lc.coordMu.Unlock()
	assert.Nil(t, registered)
	_, found, err = f.catalog.GetDedicatedGraph(ctx, graph.GraphID)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, f.mi.GetMetadata(prefix))
}

func TestApplyCoordinatorsLosesStaleReportToReadyRecovery(t *testing.T) {
	f := newFamilyFixture(t, "coordinator-stale-report")
	defer f.close()
	ctx := context.Background()

	before := liveCoordinatorOrNil(f.lc, f.automatic.CheckoutID)
	require.NotNil(t, before)
	require.True(t, before.Running())

	// This report was produced while the checkout was unavailable. Recovery
	// has already restored the catalog row by the time runtime convergence sees
	// it, so the report is only a wake-up and must lose to current catalog truth.
	f.lc.applyCoordinators(ctx, reconcile.FamilyReport{
		FamilyID:       f.familyID,
		PrimaryGraphID: f.primaryGraph,
		Checkouts: []reconcile.CheckoutReport{{
			CheckoutID:  f.automatic.CheckoutID,
			Incarnation: f.automatic.Incarnation,
			Durable:     true,
			State:       store_sqlite.CheckoutStateAvailabilityGrace,
		}},
	})

	after := liveCoordinatorOrNil(f.lc, f.automatic.CheckoutID)
	require.Same(t, before, after, "a stale non-ready report retired the recovered coordinator")
	require.True(t, after.Running())
}

func TestCoordinatorReportAdmissionPolicy(t *testing.T) {
	automatic := store_sqlite.Checkout{
		CheckoutID:    "automatic",
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
		State:         store_sqlite.CheckoutStateReady,
	}
	dedicated := automatic
	dedicated.EffectiveMode = store_sqlite.CheckoutModeDedicated
	transitioning := automatic
	transitioning.ActiveIntentTransitionID = "transition"

	tests := []struct {
		name     string
		policy   coordinatorAdmissionPolicy
		entry    reconcile.CheckoutReport
		checkout store_sqlite.Checkout
		found    bool
		routed   bool
		live     bool
		want     bool
	}{
		{
			name:     "cold route-free automatic remains catalog-only",
			policy:   coordinatorAdmissionStartupRoutedOnly,
			entry:    reconcile.CheckoutReport{Action: reconcile.ActionIdentityAllocated},
			checkout: automatic, found: true,
		},
		{
			name:     "cold routed automatic is restored",
			policy:   coordinatorAdmissionStartupRoutedOnly,
			entry:    reconcile.CheckoutReport{Action: reconcile.ActionReadyConfirmed},
			checkout: automatic, found: true, routed: true, want: true,
		},
		{
			name:     "cold live automatic is retained",
			policy:   coordinatorAdmissionStartupRoutedOnly,
			entry:    reconcile.CheckoutReport{Action: reconcile.ActionReadyConfirmed},
			checkout: automatic, found: true, live: true, want: true,
		},
		{
			name:     "cold required transition is admitted",
			policy:   coordinatorAdmissionStartupRoutedOnly,
			entry:    reconcile.CheckoutReport{Action: reconcile.ActionReadyConfirmed},
			checkout: transitioning, found: true, want: true,
		},
		{
			name:     "runtime discovery is admitted",
			policy:   coordinatorAdmissionRuntime,
			entry:    reconcile.CheckoutReport{Action: reconcile.ActionIdentityAllocated},
			checkout: automatic, found: true, want: true,
		},
		{
			name:     "runtime recovery is admitted",
			policy:   coordinatorAdmissionRuntime,
			entry:    reconcile.CheckoutReport{Action: reconcile.ActionAvailabilityRecovered},
			checkout: automatic, found: true, want: true,
		},
		{
			name:     "runtime removal cancellation is admitted",
			policy:   coordinatorAdmissionRuntime,
			entry:    reconcile.CheckoutReport{Action: reconcile.ActionRemovalCancelled},
			checkout: automatic, found: true, want: true,
		},
		{
			name:     "runtime root move is admitted",
			policy:   coordinatorAdmissionRuntime,
			entry:    reconcile.CheckoutReport{Action: reconcile.ActionReadyConfirmed, RootMoved: true},
			checkout: automatic, found: true, want: true,
		},
		{
			name:     "routine runtime inventory does not wake dormant automatic",
			policy:   coordinatorAdmissionRuntime,
			entry:    reconcile.CheckoutReport{Action: reconcile.ActionReadyConfirmed},
			checkout: automatic, found: true,
		},
		{
			name:     "dedicated checkout always converges",
			policy:   coordinatorAdmissionStartupRoutedOnly,
			entry:    reconcile.CheckoutReport{Action: reconcile.ActionReadyConfirmed},
			checkout: dedicated, found: true, want: true,
		},
		{
			name:     "missing checkout always converges teardown",
			policy:   coordinatorAdmissionStartupRoutedOnly,
			entry:    reconcile.CheckoutReport{Action: reconcile.ActionReadyConfirmed},
			checkout: automatic, want: true,
		},
		{
			name:   "non-ready checkout always converges teardown",
			policy: coordinatorAdmissionStartupRoutedOnly,
			entry:  reconcile.CheckoutReport{Action: reconcile.ActionReadyConfirmed},
			checkout: func() store_sqlite.Checkout {
				checkout := automatic
				checkout.State = store_sqlite.CheckoutStateAvailabilityGrace
				return checkout
			}(),
			found: true, want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, coordinatorReportAdmitted(
				tt.policy, tt.entry, tt.checkout, tt.found, tt.routed, tt.live,
			))
		})
	}
	assert.Equal(t, coordinatorAdmissionRuntime, coordinatorAdmissionPolicyFrom(nil),
		"a defensive nil context retains runtime admission semantics")
}

func TestCheckoutActivationAdmissionCoalesces(t *testing.T) {
	lifecycle := &CheckoutLifecycle{
		coordinators:          map[string]*CheckoutCoordinator{},
		coordinatorHeads:      map[string]checkoutHeadIdentity{},
		coordinatorActivating: map[string]struct{}{},
		started:               map[string][]*CheckoutCoordinator{},
	}

	admitted, active := lifecycle.beginCheckoutActivation("checkout")
	require.True(t, admitted)
	require.True(t, active)
	admitted, active = lifecycle.beginCheckoutActivation("checkout")
	assert.False(t, admitted)
	assert.True(t, active, "a coalesced selector observes scheduled activation")

	lifecycle.finishCheckoutActivation("checkout")
	admitted, active = lifecycle.beginCheckoutActivation("checkout")
	assert.True(t, admitted, "a completed failed attempt may be retried")
	assert.True(t, active)
	lifecycle.finishCheckoutActivation("checkout")
}

func TestFailedStartupPromotionReleasesAdmissionMarker(t *testing.T) {
	lifecycle := &CheckoutLifecycle{
		startupAdmissionFamilies: map[string]map[string]struct{}{},
	}
	run := &modeTransitionRun{done: make(chan struct{})}
	lifecycle.markStartupFamilyAdmission("family", "checkout")
	lifecycle.watchStartupFamilyAdmission("family", "checkout", run)
	require.True(t, lifecycle.startupFamilyAdmissionPending("family"))

	run.outcome.err = errors.New("terminal attempt failure")
	close(run.done)
	require.Eventually(t, func() bool {
		return !lifecycle.startupFamilyAdmissionPending("family")
	}, time.Second, time.Millisecond,
		"a failed cold promotion left runtime discovery under startup policy")
}

func BenchmarkStartupCoordinatorAdmissionRouteFreeAutomatic(b *testing.B) {
	entry := reconcile.CheckoutReport{Action: reconcile.ActionIdentityAllocated}
	checkout := store_sqlite.Checkout{
		CheckoutID:    "automatic",
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
		State:         store_sqlite.CheckoutStateReady,
	}
	for _, checkouts := range []int{256, 10_000} {
		b.Run(strconv.Itoa(checkouts), func(b *testing.B) {
			for iteration := 0; iteration < b.N; iteration++ {
				admitted := 0
				for checkoutIndex := 0; checkoutIndex < checkouts; checkoutIndex++ {
					if coordinatorReportAdmitted(
						coordinatorAdmissionStartupRoutedOnly,
						entry, checkout, true, false, false,
					) {
						admitted++
					}
				}
				if admitted != 0 {
					b.Fatalf("admitted %d route-free automatic checkouts", admitted)
				}
			}
		})
	}
}
