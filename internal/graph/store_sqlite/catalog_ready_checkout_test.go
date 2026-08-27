package store_sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func upsertReadyCacheCheckout(t *testing.T, catalog *Catalog, checkoutID string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), checkoutID)
	ctx := context.Background()
	if err := catalog.UpsertRepositoryFamily(ctx, RepositoryFamily{
		FamilyID:          "ready-cache-family",
		CommonDirIdentity: "ready-cache-common-dir",
		State:             "family_ready",
	}); err != nil {
		t.Fatalf("upsert repository family: %v", err)
	}
	if err := catalog.UpsertCheckout(ctx, Checkout{
		CheckoutID:    checkoutID,
		Incarnation:   "incarnation-1",
		FamilyID:      "ready-cache-family",
		RootPath:      root,
		GitDir:        filepath.Join(root, ".git"),
		AdminName:     checkoutID,
		State:         "checkout_ready",
		DesiredMode:   "automatic",
		EffectiveMode: "automatic",
	}); err != nil {
		t.Fatalf("upsert checkout: %v", err)
	}
}

func TestBindReadyGenerationLeaseToCheckoutConsumesLeaseWithRouteCAS(t *testing.T) {
	store := openCatalogStore(t)
	catalog := store.Catalog()
	key := readyGenerationCacheLifecycleKey()
	generationID := createReadyCacheGeneration(t, catalog, key,
		"checkout-layer", "commit:cache-route", "cache-route", "")

	ctx := context.Background()
	upsertReadyCacheCheckout(t, catalog, "cache-route")
	if err := catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
		CheckoutID: "cache-route",
		GraphID:    key.GraphID,
		State:      RoutePending,
	}); err != nil {
		t.Fatalf("upsert checkout route: %v", err)
	}
	route, found, err := catalog.GetCheckoutRoute(ctx, "cache-route")
	if err != nil || !found {
		t.Fatalf("get checkout route: found=%v err=%v", found, err)
	}
	claim, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{
		Key:                   key,
		CandidateGenerationID: generationID,
	})
	if err != nil || !found {
		t.Fatalf("claim ready generation: found=%v err=%v", found, err)
	}
	if err := catalog.BindReadyGenerationLeaseToCheckout(ctx,
		BindReadyGenerationLeaseToCheckoutRequest{
			Key:                key,
			LeaseToken:         claim.LeaseToken,
			CheckoutID:         route.CheckoutID,
			ExpectedRouteEpoch: route.RouteEpoch,
			GenerationID:       claim.WinnerGenerationID,
			State:              RouteActive,
		}); err != nil {
		t.Fatalf("bind ready generation: %v", err)
	}
	got, found, err := catalog.GetCheckoutRoute(ctx, route.CheckoutID)
	if err != nil || !found {
		t.Fatalf("get bound route: found=%v err=%v", found, err)
	}
	if got.CommitGenerationID != generationID || got.DirtyGenerationID != 0 || got.State != RouteActive {
		t.Fatalf("bound route = %+v, want commit=%d dirty=0 active", got, generationID)
	}
	if got.RouteEpoch != route.RouteEpoch+1 {
		t.Fatalf("route epoch=%d, want %d", got.RouteEpoch, route.RouteEpoch+1)
	}
	if err := catalog.ReleaseReadyGenerationLease(ctx, claim.LeaseToken); err != nil {
		t.Fatalf("consumed lease must release idempotently: %v", err)
	}
}

func TestBindReadyGenerationLeaseToCheckoutRejectsMovedRouteWithoutConsumingLease(t *testing.T) {
	store := openCatalogStore(t)
	catalog := store.Catalog()
	key := readyGenerationCacheLifecycleKey()
	generationID := createReadyCacheGeneration(t, catalog, key,
		"checkout-layer", "commit:stale-route", "stale-route", "")

	ctx := context.Background()
	upsertReadyCacheCheckout(t, catalog, "stale-route")
	if err := catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
		CheckoutID: "stale-route",
		GraphID:    key.GraphID,
		State:      RoutePending,
	}); err != nil {
		t.Fatalf("upsert checkout route: %v", err)
	}
	route, found, err := catalog.GetCheckoutRoute(ctx, "stale-route")
	if err != nil || !found {
		t.Fatalf("get checkout route: found=%v err=%v", found, err)
	}
	claim, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{
		Key:                   key,
		CandidateGenerationID: generationID,
	})
	if err != nil || !found {
		t.Fatalf("claim ready generation: found=%v err=%v", found, err)
	}
	if err := catalog.FlipCheckoutRouteSlot(ctx, FlipCheckoutRouteSlotRequest{
		CheckoutID:         route.CheckoutID,
		Slot:               RouteSlotCommit,
		GenerationID:       generationID,
		ExpectedRouteEpoch: route.RouteEpoch,
		State:              RouteActive,
	}); err != nil {
		t.Fatalf("move route first: %v", err)
	}
	err = catalog.BindReadyGenerationLeaseToCheckout(ctx,
		BindReadyGenerationLeaseToCheckoutRequest{
			Key:                key,
			LeaseToken:         claim.LeaseToken,
			CheckoutID:         route.CheckoutID,
			ExpectedRouteEpoch: route.RouteEpoch,
			GenerationID:       claim.WinnerGenerationID,
			State:              RouteActive,
		})
	if !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("bind moved route error=%v, want ErrCatalogStaleGuard", err)
	}
	// The failed transaction must leave lease disposal to the caller.
	if err := catalog.ReleaseReadyGenerationLease(ctx, claim.LeaseToken); err != nil {
		t.Fatalf("release lease after failed publication: %v", err)
	}
}

func BenchmarkReadyGenerationCheckoutAdoption(b *testing.B) {
	store := openCatalogStore(b)
	catalog := store.Catalog()
	key := readyGenerationCacheLifecycleKey()
	generationID := createReadyCacheGeneration(b, catalog, key,
		"checkout-layer", "commit:benchmark-route", "benchmark-route", "")
	ctx := context.Background()
	root := filepath.Join(b.TempDir(), "benchmark-route")
	if err := catalog.UpsertRepositoryFamily(ctx, RepositoryFamily{
		FamilyID:          "ready-cache-benchmark-family",
		CommonDirIdentity: "ready-cache-benchmark-common-dir",
		State:             "family_ready",
	}); err != nil {
		b.Fatal(err)
	}
	if err := catalog.UpsertCheckout(ctx, Checkout{
		CheckoutID:    "benchmark-route",
		Incarnation:   "incarnation-1",
		FamilyID:      "ready-cache-benchmark-family",
		RootPath:      root,
		GitDir:        filepath.Join(root, ".git"),
		AdminName:     "benchmark-route",
		State:         "checkout_ready",
		DesiredMode:   "automatic",
		EffectiveMode: "automatic",
	}); err != nil {
		b.Fatal(err)
	}
	if err := catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
		CheckoutID: "benchmark-route",
		GraphID:    key.GraphID,
		State:      RoutePending,
	}); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i > 0 {
			b.StopTimer()
			route, found, err := catalog.GetCheckoutRoute(ctx, "benchmark-route")
			if err != nil || !found {
				b.Fatalf("get benchmark route: found=%v err=%v", found, err)
			}
			if err := catalog.FlipCheckoutRoute(ctx, FlipCheckoutRouteRequest{
				CheckoutID:         route.CheckoutID,
				ExpectedRouteEpoch: route.RouteEpoch,
				GraphID:            route.GraphID,
				CommitGenerationID: 0,
				DirtyGenerationID:  0,
				State:              RoutePending,
			}); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
		}
		route, found, err := catalog.GetCheckoutRoute(ctx, "benchmark-route")
		if err != nil || !found {
			b.Fatalf("get benchmark route: found=%v err=%v", found, err)
		}
		claim, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{Key: key})
		if err != nil || !found {
			b.Fatalf("claim ready generation: found=%v err=%v", found, err)
		}
		if err := catalog.BindReadyGenerationLeaseToCheckout(ctx,
			BindReadyGenerationLeaseToCheckoutRequest{
				Key:                key,
				LeaseToken:         claim.LeaseToken,
				CheckoutID:         route.CheckoutID,
				ExpectedRouteEpoch: route.RouteEpoch,
				GenerationID:       generationID,
				State:              RouteActive,
			}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(0, "builds/op")
}
