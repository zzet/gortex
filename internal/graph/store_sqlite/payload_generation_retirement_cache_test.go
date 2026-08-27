package store_sqlite

import (
	"context"
	"errors"
	"testing"
)

func retirementClaimBarrier() (func(), <-chan struct{}, func()) {
	atClaim := make(chan struct{})
	resume := make(chan struct{})
	barrier := func() {
		close(atClaim)
		<-resume
	}
	release := func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	}
	return barrier, atClaim, release
}

func TestRetirePayloadGenerationRefusesGenerationBoundAfterPrecheck(t *testing.T) {
	store := openCatalogStore(t)
	catalog := store.Catalog()
	ctx := context.Background()
	key := readyGenerationCacheLifecycleKey()
	const checkoutID = "retirement-route-race"

	upsertReadyCacheCheckout(t, catalog, checkoutID)
	generationID := createReadyCacheGeneration(
		t, catalog, key, "checkout-layer", checkoutID, "commit:"+checkoutID, "",
	)
	if err := catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
		CheckoutID: checkoutID,
		GraphID:    key.GraphID,
		State:      RoutePending,
	}); err != nil {
		t.Fatalf("upsert pending route: %v", err)
	}
	route, found, err := catalog.GetCheckoutRoute(ctx, checkoutID)
	if err != nil || !found {
		t.Fatalf("get pending route: found=%v err=%v", found, err)
	}

	barrier, atClaim, release := retirementClaimBarrier()
	defer release()
	retired := make(chan error, 1)
	go func() {
		retired <- store.retirePayloadGeneration(ctx, generationID, nil, barrier)
	}()
	<-atClaim

	claim, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{
		Key: key, CandidateGenerationID: generationID,
	})
	if err != nil || !found {
		t.Fatalf("claim ready generation: found=%v err=%v", found, err)
	}
	if err := catalog.BindReadyGenerationLeaseToCheckout(ctx,
		BindReadyGenerationLeaseToCheckoutRequest{
			Key: key, LeaseToken: claim.LeaseToken,
			CheckoutID: checkoutID, ExpectedRouteEpoch: route.RouteEpoch,
			GenerationID: claim.WinnerGenerationID, State: RouteActive,
		}); err != nil {
		t.Fatalf("bind ready generation: %v", err)
	}
	release()

	if err := <-retired; !errors.Is(err, ErrCatalogGenerationReferenced) {
		t.Fatalf("retirement returned %v, want referenced refusal", err)
	}
	row, found, err := catalog.GetViewGeneration(ctx, generationID)
	if err != nil || !found || row.State != ViewGenerationReady {
		t.Fatalf("routed generation was changed or removed: found=%v err=%v row=%+v", found, err, row)
	}
	route, found, err = catalog.GetCheckoutRoute(ctx, checkoutID)
	if err != nil || !found || route.CommitGenerationID != generationID || route.State != RouteActive {
		t.Fatalf("winning route was not preserved: found=%v err=%v route=%+v", found, err, route)
	}
}

func TestRetirePayloadGenerationRefusesActiveReadyLeaseAfterPrecheck(t *testing.T) {
	store := openCatalogStore(t)
	catalog := store.Catalog()
	ctx := context.Background()
	key := readyGenerationCacheLifecycleKey()
	generationID := createReadyCacheGeneration(
		t, catalog, key, "checkout-layer", "lease-only", "commit:lease-only", "",
	)

	barrier, atClaim, release := retirementClaimBarrier()
	defer release()
	retired := make(chan error, 1)
	go func() {
		retired <- store.retirePayloadGeneration(ctx, generationID, nil, barrier)
	}()
	<-atClaim

	claim, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{
		Key: key, CandidateGenerationID: generationID,
	})
	if err != nil || !found {
		t.Fatalf("claim ready generation: found=%v err=%v", found, err)
	}
	defer func() { _ = catalog.ReleaseReadyGenerationLease(context.Background(), claim.LeaseToken) }()
	release()

	if err := <-retired; !errors.Is(err, ErrPayloadGenerationInUse) {
		t.Fatalf("retirement returned %v, want live-lease refusal", err)
	}
	row, found, err := catalog.GetViewGeneration(ctx, generationID)
	if err != nil || !found || row.State != ViewGenerationReady {
		t.Fatalf("leased generation was changed or removed: found=%v err=%v row=%+v", found, err, row)
	}
}
