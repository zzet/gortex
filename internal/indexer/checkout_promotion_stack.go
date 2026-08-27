package indexer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

const dedicatedBaseGenerationKind = "dedicated_base"

func (l *CheckoutLifecycle) publishDedicatedBase(
	ctx context.Context,
	checkout store_sqlite.Checkout,
	graphID string,
	sample checkoutSample,
) (int64, error) {
	generation := store_sqlite.ViewGeneration{
		OwnerKind:            dedicatedBaseGenerationKind,
		GraphID:              graphID,
		LayerID:              graphID + ":base",
		CheckoutID:           checkout.CheckoutID,
		GenerationKind:       dedicatedBaseGenerationKind,
		LowerViewFingerprint: sample.tree,
		TreeOID:              sample.tree,
		ProvenanceCommitOID:  sample.commit,
		State:                store_sqlite.ViewGenerationBuilding,
		CreatedAt:            l.now().Unix(),
	}
	generationID, _, err := l.catalog.AdoptOrCreateViewGeneration(ctx, generation)
	if err != nil {
		return 0, err
	}
	if err := l.catalog.PublishViewGeneration(ctx, generationID, l.now().Unix()); err != nil {
		if !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
			return 0, err
		}
		row, found, readErr := l.catalog.GetViewGeneration(ctx, generationID)
		if readErr != nil {
			return 0, readErr
		}
		if !found || row.State != store_sqlite.ViewGenerationReady ||
			row.GraphID != graphID || row.CheckoutID != checkout.CheckoutID ||
			row.TreeOID != sample.tree {
			return 0, err
		}
	}
	return generationID, nil
}

func (l *CheckoutLifecycle) prepareAndPublishPromotion(
	ctx context.Context,
	checkout store_sqlite.Checkout,
	transition store_sqlite.IntentTransition,
	graphID string,
	baseGenerationID int64,
	sample checkoutSample,
) (CheckoutCycle, error) {
	// Stop the automatic coordinator before reading its route epoch. Queries
	// continue to use the old ready route, but no loop can advance it between
	// preparation and the guarded publication transaction.
	l.dropCoordinator(checkout.CheckoutID)

	coordinator, err := l.buildCoordinatorWithPoll(ctx, graphID, checkout, -time.Nanosecond)
	if err != nil {
		l.restoreAutomaticCoordinator(ctx, checkout)
		return CheckoutCycle{}, err
	}
	if coordinator == nil {
		l.restoreAutomaticCoordinator(ctx, checkout)
		return CheckoutCycle{}, fmt.Errorf("indexer: dedicated graph %s cannot build a checkout stack", graphID)
	}
	defer coordinator.Close()

	base := primaryBase{graphID: graphID, generationID: baseGenerationID, treeOID: sample.tree}
	cycle, err := coordinator.preparePromotion(ctx, base, sample.tree,
		func(ctx context.Context, route store_sqlite.CheckoutRoute, routed bool, routeGraphID string,
			commitGeneration, dirtyGeneration int64) error {
			return l.catalog.CommitAuthorizedPromotion(ctx, store_sqlite.CommitAuthorizedPromotionRequest{
				CheckoutID:         checkout.CheckoutID,
				Incarnation:        checkout.Incarnation,
				FamilyID:           checkout.FamilyID,
				TransitionID:       transition.TransitionID,
				RequiredCause:      promotionTransitionCause,
				GraphID:            routeGraphID,
				RequiredGraphState: reconcile.GraphStateReady,
				BaseGenerationID:   baseGenerationID,
				BaseTreeOID:        sample.tree,
				CommitGenerationID: commitGeneration,
				DirtyGenerationID:  dirtyGeneration,
				RouteExists:        routed,
				ExpectedRouteEpoch: route.RouteEpoch,
				State:              store_sqlite.CheckoutStateReady,
				LastSeen:           l.now().Unix(),
			})
		})
	if err != nil {
		l.restoreAutomaticCoordinator(ctx, checkout)
		return cycle, err
	}
	if err := l.installDedicatedCoordinator(ctx, graphID, checkout); err != nil {
		return cycle, err
	}
	return cycle, nil
}

func (l *CheckoutLifecycle) installDedicatedCoordinator(
	ctx context.Context, graphID string, checkout store_sqlite.Checkout,
) error {
	l.coordMu.Lock()
	current := l.coordinators[checkout.CheckoutID]
	l.coordMu.Unlock()
	if current != nil && current.Running() && current.graphID == graphID {
		return nil
	}
	if current != nil {
		l.dropCoordinator(checkout.CheckoutID)
	}
	coordinator, err := l.buildCoordinator(ctx, graphID, checkout)
	if err != nil {
		return err
	}
	if coordinator == nil {
		return fmt.Errorf("indexer: dedicated graph %s cannot start checkout coordinator", graphID)
	}
	if !l.installCoordinator(checkout.CheckoutID, coordinator) {
		if existing := l.coordinatorFor(checkout.CheckoutID); existing != nil &&
			existing.Running() && existing.graphID == graphID {
			return nil
		}
		return fmt.Errorf("indexer: checkout %s coordinator moved", checkout.CheckoutID)
	}
	coordinator.Signal("dedicated checkout installed")
	return nil
}

func (l *CheckoutLifecycle) restoreAutomaticCoordinator(
	ctx context.Context, checkout store_sqlite.Checkout,
) {
	graphs, err := l.catalog.ListDedicatedGraphs(ctx, checkout.FamilyID)
	if err != nil {
		return
	}
	for _, graph := range graphs {
		if graph.IsPrimaryBase {
			l.ensureCoordinator(ctx, graph.GraphID, checkout)
			return
		}
	}
}

func (l *CheckoutLifecycle) requireDedicatedRoute(
	ctx context.Context, checkoutID, graphID string,
) error {
	route, found, err := l.catalog.GetCheckoutRoute(ctx, checkoutID)
	if err != nil {
		return err
	}
	if !found || route.GraphID != graphID || route.State != store_sqlite.RouteActive ||
		route.CommitGenerationID <= 0 || route.DirtyGenerationID <= 0 {
		return fmt.Errorf("%w: checkout %s has no ready dedicated route",
			store_sqlite.ErrCatalogStaleGuard, checkoutID)
	}
	graph, found, err := l.catalog.GetDedicatedGraph(ctx, graphID)
	if err != nil {
		return err
	}
	if !found || graph.ActiveGenerationID <= 0 {
		return fmt.Errorf("%w: dedicated graph %s has no immutable base",
			store_sqlite.ErrCatalogStaleGuard, graphID)
	}
	return nil
}

func (l *CheckoutLifecycle) coordinatorFor(checkoutID string) *CheckoutCoordinator {
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	return l.coordinators[checkoutID]
}
