package indexer

import (
	"context"
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
