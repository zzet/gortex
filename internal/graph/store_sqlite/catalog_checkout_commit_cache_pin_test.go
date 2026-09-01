package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func checkoutCommitCacheTestGeneration(
	t testing.TB,
	catalog *Catalog,
	graphID, suffix string,
) (int64, ReadyGenerationCacheKey) {
	t.Helper()
	key := readyCacheTestKey(graphID, 0)
	key.TreeOID = "tree-" + suffix
	return createReadyCacheGeneration(
		t, catalog, key, "ref_view", "", "commit-"+suffix, "oid-"+suffix,
	), key
}

func checkoutCommitCacheTestRoute(
	t testing.TB,
	catalog *Catalog,
	checkoutID, graphID string,
	commitGenerationID int64,
) CheckoutRoute {
	t.Helper()
	checkoutCommitCacheTestCheckout(t, catalog, checkoutID)
	if err := catalog.UpsertCheckoutRoute(context.Background(), CheckoutRoute{
		CheckoutID:         checkoutID,
		GraphID:            graphID,
		CommitGenerationID: commitGenerationID,
		State:              RouteActive,
	}); err != nil {
		t.Fatalf("upsert checkout route: %v", err)
	}
	route, found, err := catalog.GetCheckoutRoute(context.Background(), checkoutID)
	if err != nil || !found {
		t.Fatalf("get checkout route: found=%v err=%v", found, err)
	}
	return route
}

func checkoutCommitCacheTestCheckout(t testing.TB, catalog *Catalog, checkoutID string) {
	t.Helper()
	seedFamilyAndCheckout(
		t, catalog, "checkout-commit-cache-test-family", checkoutID, "incarnation-"+checkoutID,
	)
}

func checkoutCommitCacheTestSetPin(
	t testing.TB,
	catalog *Catalog,
	checkoutID, graphID string,
	generationID, selectedAt, storageBytes int64,
) {
	t.Helper()
	err := catalog.withTx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE view_generations SET storage_bytes = ? WHERE generation_id = ?`,
			storageBytes, generationID); err != nil {
			return err
		}
		return upsertCheckoutCommitCachePinTx(
			context.Background(), tx, checkoutID, graphID, generationID, selectedAt,
		)
	})
	if err != nil {
		t.Fatalf("seed checkout commit cache pin: %v", err)
	}
}

func checkoutCommitCacheTestPinIDs(pins []CheckoutCommitCachePin) map[int64]int {
	out := make(map[int64]int, len(pins))
	for _, pin := range pins {
		out[pin.GenerationID]++
	}
	return out
}

func checkoutCommitCacheTestPins(
	t testing.TB,
	catalog *Catalog,
	graphID string,
) []CheckoutCommitCachePin {
	t.Helper()
	pins, err := catalog.ListCheckoutCommitCachePins(context.Background(), graphID)
	if err != nil {
		t.Fatalf("list checkout commit cache pins: %v", err)
	}
	return pins
}

func TestUpsertCheckoutRouteRefusesRepointAndPreservesCommitCachePins(t *testing.T) {
	ctx := context.Background()
	store := openCatalogStore(t)
	catalog := store.Catalog()
	const checkoutID = "cache-install-only-checkout"
	generationA, _ := checkoutCommitCacheTestGeneration(t, catalog, "cache-install-graph-a", "a")
	generationB, _ := checkoutCommitCacheTestGeneration(t, catalog, "cache-install-graph-b", "b")
	routeA := checkoutCommitCacheTestRoute(
		t, catalog, checkoutID, "cache-install-graph-a", generationA,
	)
	checkoutCommitCacheTestSetPin(
		t, catalog, checkoutID, routeA.GraphID, generationA, 10, 1,
	)

	err := catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
		CheckoutID:         checkoutID,
		GraphID:            "cache-install-graph-b",
		CommitGenerationID: generationB,
		RouteEpoch:         99,
		State:              RoutePending,
	})
	if !errors.Is(err, ErrCatalogStaleGuard) || !strings.Contains(err.Error(), "guarded route transition") {
		t.Fatalf("duplicate route install error=%v, want clear ErrCatalogStaleGuard", err)
	}
	routeAfter, found, err := catalog.GetCheckoutRoute(ctx, checkoutID)
	if err != nil || !found || routeAfter != routeA {
		t.Fatalf("duplicate route changed catalog: found=%v route=%+v err=%v, want %+v",
			found, routeAfter, err, routeA)
	}
	if pins := checkoutCommitCacheTestPins(t, catalog, routeA.GraphID); len(pins) != 1 ||
		pins[0].CheckoutID != checkoutID || pins[0].GenerationID != generationA {
		t.Fatalf("duplicate route changed original pins: %+v", pins)
	}
	if pins := checkoutCommitCacheTestPins(t, catalog, "cache-install-graph-b"); len(pins) != 0 {
		t.Fatalf("duplicate route installed replacement pins: %+v", pins)
	}
}

func TestCheckoutCommitCachePinRequiresOneRetainableGeneration(t *testing.T) {
	ctx := context.Background()
	store := openCatalogStore(t)
	catalog := store.Catalog()
	const (
		checkoutID = "cache-pin-guard-checkout"
		graphID    = "cache-pin-guard-graph"
	)
	checkoutCommitCacheTestCheckout(t, catalog, checkoutID)
	validGeneration, _ := checkoutCommitCacheTestGeneration(t, catalog, graphID, "valid")
	nonCommitGeneration, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
		OwnerKind:      "checkout",
		GraphID:        graphID,
		LayerID:        "cache-pin-guard-dirty",
		CheckoutID:     checkoutID,
		GenerationKind: "dirty",
		State:          ViewGenerationReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	nonReadyGeneration, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
		OwnerKind:      "checkout",
		GraphID:        graphID,
		LayerID:        "cache-pin-guard-building",
		CheckoutID:     checkoutID,
		GenerationKind: "commit",
		State:          ViewGenerationBuilding,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name         string
		graphID      string
		generationID int64
	}{
		{name: "missing", graphID: graphID, generationID: nonReadyGeneration + 1000},
		{name: "wrong graph", graphID: graphID + "-other", generationID: validGeneration},
		{name: "non commit", graphID: graphID, generationID: nonCommitGeneration},
		{name: "non ready", graphID: graphID, generationID: nonReadyGeneration},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := catalog.withTx(ctx, func(tx *sql.Tx) error {
				return upsertCheckoutCommitCachePinTx(
					ctx, tx, checkoutID, tc.graphID, tc.generationID, 20,
				)
			})
			if !errors.Is(err, ErrCatalogStaleGuard) {
				t.Fatalf("pin invalid positive generation error=%v, want ErrCatalogStaleGuard", err)
			}
			if pins := checkoutCommitCacheTestPins(t, catalog, graphID); len(pins) != 0 {
				t.Fatalf("rejected generation left pins: %+v", pins)
			}
		})
	}

	for _, selectedAt := range []int64{30, 40} {
		if err := catalog.withTx(ctx, func(tx *sql.Tx) error {
			return upsertCheckoutCommitCachePinTx(
				ctx, tx, checkoutID, graphID, validGeneration, selectedAt,
			)
		}); err != nil {
			t.Fatalf("pin retainable generation at %d: %v", selectedAt, err)
		}
	}
	pins := checkoutCommitCacheTestPins(t, catalog, graphID)
	if len(pins) != 1 || pins[0].GenerationID != validGeneration || pins[0].LastSelected != 40 {
		t.Fatalf("valid pin upsert=%+v, want one row restamped to 40", pins)
	}
}

func TestFlipCheckoutRouteRollsBackWhenCommitPinIsNotRetainable(t *testing.T) {
	ctx := context.Background()
	store := openCatalogStore(t)
	catalog := store.Catalog()
	const (
		checkoutID = "cache-pin-rollback-checkout"
		graphID    = "cache-pin-rollback-graph"
	)
	checkoutCommitCacheTestCheckout(t, catalog, checkoutID)
	if err := catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
		CheckoutID: checkoutID,
		GraphID:    graphID,
		State:      RoutePending,
	}); err != nil {
		t.Fatal(err)
	}
	original, found, err := catalog.GetCheckoutRoute(ctx, checkoutID)
	if err != nil || !found {
		t.Fatalf("read original route: found=%v err=%v", found, err)
	}
	dirtyGeneration, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
		OwnerKind:      "checkout",
		GraphID:        graphID,
		LayerID:        "cache-pin-rollback-dirty",
		CheckoutID:     checkoutID,
		GenerationKind: "dirty",
		State:          ViewGenerationReady,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = catalog.FlipCheckoutRoute(ctx, FlipCheckoutRouteRequest{
		CheckoutID:         checkoutID,
		ExpectedRouteEpoch: original.RouteEpoch,
		GraphID:            graphID,
		CommitGenerationID: dirtyGeneration,
		State:              RouteActive,
	})
	if !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("publish unretainable commit error=%v, want ErrCatalogStaleGuard", err)
	}
	after, found, err := catalog.GetCheckoutRoute(ctx, checkoutID)
	if err != nil || !found || after != original {
		t.Fatalf("failed pin did not roll back route: found=%v route=%+v err=%v, want %+v",
			found, after, err, original)
	}
	if pins := checkoutCommitCacheTestPins(t, catalog, graphID); len(pins) != 0 {
		t.Fatalf("failed pin left cache ownership: %+v", pins)
	}
}

func TestCheckoutCommitCacheBindPinsBothSidesAtomically(t *testing.T) {
	ctx := context.Background()
	store := openCatalogStore(t)
	catalog := store.Catalog()
	const (
		graphID    = "cache-bind-graph"
		checkoutID = "cache-bind-checkout"
	)
	oldGenerationID, _ := checkoutCommitCacheTestGeneration(t, catalog, graphID, "old")
	newGenerationID, newKey := checkoutCommitCacheTestGeneration(t, catalog, graphID, "new")
	route := checkoutCommitCacheTestRoute(t, catalog, checkoutID, graphID, oldGenerationID)

	claim, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{
		Key:                   newKey,
		CandidateGenerationID: newGenerationID,
		LeaseToken:            "cache-bind-success",
	})
	if err != nil || !found || claim.WinnerGenerationID != newGenerationID {
		t.Fatalf("claim new generation: found=%v claim=%+v err=%v", found, claim, err)
	}
	if err := catalog.BindReadyGenerationLeaseToCheckout(ctx,
		BindReadyGenerationLeaseToCheckoutRequest{
			Key:                newKey,
			LeaseToken:         claim.LeaseToken,
			CheckoutID:         checkoutID,
			ExpectedRouteEpoch: route.RouteEpoch,
			GenerationID:       newGenerationID,
			State:              RouteActive,
		}); err != nil {
		t.Fatalf("bind new generation: %v", err)
	}

	pins := checkoutCommitCacheTestPins(t, catalog, graphID)
	if got := checkoutCommitCacheTestPinIDs(pins); len(got) != 2 ||
		got[oldGenerationID] != 1 || got[newGenerationID] != 1 {
		t.Fatalf("pins=%+v, want one holder for old=%d and new=%d", pins,
			oldGenerationID, newGenerationID)
	}
	for _, pin := range pins {
		if pin.CheckoutID != checkoutID || pin.GraphID != graphID || pin.LastSelected <= 0 {
			t.Fatalf("invalid durable pin after bind: %+v", pin)
		}
	}
	var leases int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ready_generation_leases WHERE lease_token = ?`,
		claim.LeaseToken).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if leases != 0 {
		t.Fatalf("consumed lease rows=%d, want 0", leases)
	}
}

func TestCheckoutCommitCacheStaleBindTouchesNeitherPinsNorLease(t *testing.T) {
	ctx := context.Background()
	store := openCatalogStore(t)
	catalog := store.Catalog()
	const (
		graphID    = "cache-stale-graph"
		checkoutID = "cache-stale-checkout"
	)
	oldGenerationID, _ := checkoutCommitCacheTestGeneration(t, catalog, graphID, "stale-old")
	newGenerationID, newKey := checkoutCommitCacheTestGeneration(t, catalog, graphID, "stale-new")
	route := checkoutCommitCacheTestRoute(t, catalog, checkoutID, graphID, oldGenerationID)
	claim, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{
		Key:                   newKey,
		CandidateGenerationID: newGenerationID,
		LeaseToken:            "cache-bind-stale",
	})
	if err != nil || !found {
		t.Fatalf("claim new generation: found=%v claim=%+v err=%v", found, claim, err)
	}

	// Moving only the dirty slot invalidates the captured epoch without
	// publishing or pinning either commit generation.
	if err := catalog.FlipCheckoutRouteSlot(ctx, FlipCheckoutRouteSlotRequest{
		CheckoutID:         checkoutID,
		Slot:               RouteSlotDirty,
		GenerationID:       0,
		ExpectedRouteEpoch: route.RouteEpoch,
		State:              RouteActive,
	}); err != nil {
		t.Fatalf("move route epoch: %v", err)
	}
	err = catalog.BindReadyGenerationLeaseToCheckout(ctx,
		BindReadyGenerationLeaseToCheckoutRequest{
			Key:                newKey,
			LeaseToken:         claim.LeaseToken,
			CheckoutID:         checkoutID,
			ExpectedRouteEpoch: route.RouteEpoch,
			GenerationID:       newGenerationID,
			State:              RouteActive,
		})
	if !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale bind error=%v, want ErrCatalogStaleGuard", err)
	}
	if pins := checkoutCommitCacheTestPins(t, catalog, graphID); len(pins) != 0 {
		t.Fatalf("stale bind wrote pins: %+v", pins)
	}
	var leases int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ready_generation_leases WHERE lease_token = ?`,
		claim.LeaseToken).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if leases != 1 {
		t.Fatalf("stale bind lease rows=%d, want 1 untouched lease", leases)
	}
	if err := catalog.ReleaseReadyGenerationLease(ctx, claim.LeaseToken); err != nil {
		t.Fatalf("release untouched lease: %v", err)
	}
}

func TestCheckoutCommitCachePinGuardsRetirementUntilPruned(t *testing.T) {
	ctx := context.Background()
	store := openCatalogStore(t)
	catalog := store.Catalog()
	const (
		graphID    = "cache-retire-graph"
		checkoutID = "cache-retire-checkout"
	)
	generationID, _ := checkoutCommitCacheTestGeneration(t, catalog, graphID, "retire")
	route := checkoutCommitCacheTestRoute(t, catalog, checkoutID, graphID, 0)
	if err := catalog.FlipCheckoutRouteSlot(ctx, FlipCheckoutRouteSlotRequest{
		CheckoutID: checkoutID, Slot: RouteSlotCommit, GenerationID: generationID,
		ExpectedRouteEpoch: route.RouteEpoch, State: RouteActive,
	}); err != nil {
		t.Fatalf("publish commit route: %v", err)
	}
	route, _, _ = catalog.GetCheckoutRoute(ctx, checkoutID)
	if err := catalog.FlipCheckoutRouteSlot(ctx, FlipCheckoutRouteSlotRequest{
		CheckoutID: checkoutID, Slot: RouteSlotCommit, GenerationID: 0,
		ExpectedRouteEpoch: route.RouteEpoch, State: RoutePending,
	}); err != nil {
		t.Fatalf("clear commit route: %v", err)
	}
	if _, err := store.writerDB.ExecContext(ctx,
		`UPDATE checkout_commit_cache_pins SET last_selected = 1 WHERE generation_id = ?`,
		generationID); err != nil {
		t.Fatal(err)
	}
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); !errors.Is(err, ErrCatalogGenerationReferenced) {
		t.Fatalf("retire pinned generation error=%v, want referenced refusal", err)
	}

	pruned, err := catalog.PruneCheckoutCommitCachePins(ctx, CheckoutCommitCacheRetention{
		InactiveCutoff:  2,
		MaxGenerations:  32,
		MaxStorageBytes: 1 << 30,
	})
	if err != nil {
		t.Fatalf("prune checkout commit cache: %v", err)
	}
	if len(pruned.EvictedGenerationIDs) != 1 ||
		pruned.EvictedGenerationIDs[0] != generationID || pruned.DeletedPins != 1 {
		t.Fatalf("prune result=%+v, want generation %d and one pin", pruned, generationID)
	}
	if candidates, err := catalog.ListCheckoutCommitCacheRetirementCandidates(ctx, 0, 10); err != nil {
		t.Fatal(err)
	} else if len(candidates) != 1 || candidates[0] != generationID {
		t.Fatalf("prune retirement queue=%v, want %d", candidates, generationID)
	}
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); err != nil {
		t.Fatalf("retire after pin prune: %v", err)
	}
	if _, found, err := catalog.GetViewGeneration(ctx, generationID); err != nil || found {
		t.Fatalf("generation after retirement: found=%v err=%v", found, err)
	}
}

func TestCheckoutCommitCacheSharedHoldersDeleteIndependently(t *testing.T) {
	ctx := context.Background()
	store := openCatalogStore(t)
	catalog := store.Catalog()
	const graphID = "cache-shared-graph"
	generationID, _ := checkoutCommitCacheTestGeneration(t, catalog, graphID, "shared")
	for _, checkoutID := range []string{"cache-shared-a", "cache-shared-b"} {
		route := checkoutCommitCacheTestRoute(t, catalog, checkoutID, graphID, 0)
		if err := catalog.FlipCheckoutRouteSlot(ctx, FlipCheckoutRouteSlotRequest{
			CheckoutID: checkoutID, Slot: RouteSlotCommit, GenerationID: generationID,
			ExpectedRouteEpoch: route.RouteEpoch, State: RouteActive,
		}); err != nil {
			t.Fatalf("pin %s: %v", checkoutID, err)
		}
		route, _, _ = catalog.GetCheckoutRoute(ctx, checkoutID)
		if err := catalog.FlipCheckoutRouteSlot(ctx, FlipCheckoutRouteSlotRequest{
			CheckoutID: checkoutID, Slot: RouteSlotCommit, GenerationID: 0,
			ExpectedRouteEpoch: route.RouteEpoch, State: RouteRetired,
		}); err != nil {
			t.Fatalf("clear %s route: %v", checkoutID, err)
		}
	}
	if pins := checkoutCommitCacheTestPins(t, catalog, graphID); len(pins) != 2 {
		t.Fatalf("shared pins=%+v, want two holders", pins)
	}

	if err := catalog.DeleteCheckoutRoute(ctx, "cache-shared-a"); err != nil {
		t.Fatal(err)
	}
	if err := catalog.DeleteCheckout(ctx, "cache-shared-a"); err != nil {
		t.Fatalf("delete first holder: %v", err)
	}
	pins := checkoutCommitCacheTestPins(t, catalog, graphID)
	if len(pins) != 1 || pins[0].CheckoutID != "cache-shared-b" {
		t.Fatalf("pins after deleting first holder=%+v, want only holder b", pins)
	}
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); !errors.Is(err, ErrCatalogGenerationReferenced) {
		t.Fatalf("retire with second holder error=%v, want referenced refusal", err)
	}

	if err := catalog.DeleteCheckoutRoute(ctx, "cache-shared-b"); err != nil {
		t.Fatal(err)
	}
	if err := catalog.DeleteCheckout(ctx, "cache-shared-b"); err != nil {
		t.Fatalf("delete second holder: %v", err)
	}
	if pins := checkoutCommitCacheTestPins(t, catalog, graphID); len(pins) != 0 {
		t.Fatalf("pins after deleting last holder=%+v, want none", pins)
	}
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); err != nil {
		t.Fatalf("retire after last holder deletion: %v", err)
	}
}

func TestCheckoutCommitCacheRetentionUsesUniqueGenerations(t *testing.T) {
	t.Run("age", func(t *testing.T) {
		store := openCatalogStore(t)
		catalog := store.Catalog()
		const graphID = "cache-retention-age"
		checkoutCommitCacheTestCheckout(t, catalog, "cache-age-holder")
		oldID, _ := checkoutCommitCacheTestGeneration(t, catalog, graphID, "age-old")
		freshID, _ := checkoutCommitCacheTestGeneration(t, catalog, graphID, "age-fresh")
		checkoutCommitCacheTestSetPin(t, catalog, "cache-age-holder", graphID, oldID, 5, 10)
		checkoutCommitCacheTestSetPin(t, catalog, "cache-age-holder", graphID, freshID, 20, 10)

		got, err := catalog.PruneCheckoutCommitCachePins(context.Background(), CheckoutCommitCacheRetention{
			InactiveCutoff: 10, MaxGenerations: 10, MaxStorageBytes: 1 << 20,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.EvictedGenerationIDs) != 1 || got.EvictedGenerationIDs[0] != oldID {
			t.Fatalf("age prune=%+v, want only old generation %d", got, oldID)
		}
		if pins := checkoutCommitCacheTestPinIDs(checkoutCommitCacheTestPins(t, catalog, graphID)); len(pins) != 1 || pins[freshID] != 1 {
			t.Fatalf("age survivors=%v, want fresh generation %d", pins, freshID)
		}
	})

	t.Run("count counts a shared generation once", func(t *testing.T) {
		store := openCatalogStore(t)
		catalog := store.Catalog()
		const graphID = "cache-retention-count"
		checkoutCommitCacheTestCheckout(t, catalog, "cache-count-a")
		checkoutCommitCacheTestCheckout(t, catalog, "cache-count-b")
		oldID, _ := checkoutCommitCacheTestGeneration(t, catalog, graphID, "count-old")
		middleID, _ := checkoutCommitCacheTestGeneration(t, catalog, graphID, "count-middle")
		newID, _ := checkoutCommitCacheTestGeneration(t, catalog, graphID, "count-new")
		checkoutCommitCacheTestSetPin(t, catalog, "cache-count-a", graphID, oldID, 10, 10)
		checkoutCommitCacheTestSetPin(t, catalog, "cache-count-b", graphID, oldID, 10, 10)
		checkoutCommitCacheTestSetPin(t, catalog, "cache-count-a", graphID, middleID, 20, 10)
		checkoutCommitCacheTestSetPin(t, catalog, "cache-count-a", graphID, newID, 30, 10)

		got, err := catalog.PruneCheckoutCommitCachePins(context.Background(), CheckoutCommitCacheRetention{
			InactiveCutoff: 0, MaxGenerations: 2, MaxStorageBytes: 1 << 20,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.EvictedGenerationIDs) != 1 || got.EvictedGenerationIDs[0] != oldID ||
			got.DeletedPins != 2 {
			t.Fatalf("count prune=%+v, want shared old generation and two holder rows", got)
		}
		pins := checkoutCommitCacheTestPinIDs(checkoutCommitCacheTestPins(t, catalog, graphID))
		if len(pins) != 2 || pins[middleID] != 1 || pins[newID] != 1 {
			t.Fatalf("count survivors=%v, want middle=%d and new=%d", pins, middleID, newID)
		}
	})

	t.Run("bytes count a shared generation once", func(t *testing.T) {
		store := openCatalogStore(t)
		catalog := store.Catalog()
		const graphID = "cache-retention-bytes"
		checkoutCommitCacheTestCheckout(t, catalog, "cache-bytes-a")
		checkoutCommitCacheTestCheckout(t, catalog, "cache-bytes-b")
		oldID, _ := checkoutCommitCacheTestGeneration(t, catalog, graphID, "bytes-old")
		middleID, _ := checkoutCommitCacheTestGeneration(t, catalog, graphID, "bytes-middle")
		newID, _ := checkoutCommitCacheTestGeneration(t, catalog, graphID, "bytes-new")
		checkoutCommitCacheTestSetPin(t, catalog, "cache-bytes-a", graphID, oldID, 10, 60)
		checkoutCommitCacheTestSetPin(t, catalog, "cache-bytes-b", graphID, oldID, 10, 60)
		checkoutCommitCacheTestSetPin(t, catalog, "cache-bytes-a", graphID, middleID, 20, 60)
		checkoutCommitCacheTestSetPin(t, catalog, "cache-bytes-a", graphID, newID, 30, 20)

		got, err := catalog.PruneCheckoutCommitCachePins(context.Background(), CheckoutCommitCacheRetention{
			InactiveCutoff: 0, MaxGenerations: 10, MaxStorageBytes: 100,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.EvictedGenerationIDs) != 1 || got.EvictedGenerationIDs[0] != oldID ||
			got.DeletedPins != 2 {
			t.Fatalf("byte prune=%+v, want shared 60-byte generation and two pins", got)
		}
		pins := checkoutCommitCacheTestPinIDs(checkoutCommitCacheTestPins(t, catalog, graphID))
		if len(pins) != 2 || pins[middleID] != 1 || pins[newID] != 1 {
			t.Fatalf("byte survivors=%v, want middle=%d and new=%d", pins, middleID, newID)
		}
	})
}

func TestCheckoutCommitCacheConcurrentPruneAndRouteBindPreservesWinnerPin(t *testing.T) {
	ctx := context.Background()
	store := openCatalogStore(t)
	catalog := store.Catalog()
	const (
		graphID    = "cache-prune-bind-graph"
		checkoutID = "cache-prune-bind-checkout"
		iterations = 50
	)
	generationA, keyA := checkoutCommitCacheTestGeneration(t, catalog, graphID, "prune-bind-a")
	generationB, _ := checkoutCommitCacheTestGeneration(t, catalog, graphID, "prune-bind-b")
	checkoutCommitCacheTestRoute(t, catalog, checkoutID, graphID, generationB)
	checkoutCommitCacheTestSetPin(t, catalog, checkoutID, graphID, generationA, 1, 1)
	checkoutCommitCacheTestSetPin(t, catalog, checkoutID, graphID, generationB, 2, 1)

	for i := 0; i < iterations; i++ {
		route, found, err := catalog.GetCheckoutRoute(ctx, checkoutID)
		if err != nil || !found {
			t.Fatalf("iteration %d read B route: found=%v err=%v", i, found, err)
		}
		if route.CommitGenerationID != generationB {
			t.Fatalf("iteration %d starts on generation %d, want B=%d", i, route.CommitGenerationID, generationB)
		}
		if _, err := store.writerDB.ExecContext(ctx, `
UPDATE checkout_commit_cache_pins SET last_selected = 1
 WHERE checkout_id = ? AND generation_id = ?`, checkoutID, generationA); err != nil {
			t.Fatal(err)
		}
		claim, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{
			Key: keyA, CandidateGenerationID: generationA,
			LeaseToken: fmt.Sprintf("cache-prune-bind-%d", i),
		})
		if err != nil || !found {
			t.Fatalf("iteration %d claim A: found=%v claim=%+v err=%v", i, found, claim, err)
		}

		start := make(chan struct{})
		errs := make(chan error, 2)
		var workers sync.WaitGroup
		workers.Add(2)
		go func() {
			defer workers.Done()
			<-start
			_, pruneErr := catalog.PruneCheckoutCommitCachePins(ctx, CheckoutCommitCacheRetention{
				InactiveCutoff: 2, MaxGenerations: 32, MaxStorageBytes: 1 << 20,
			})
			errs <- pruneErr
		}()
		go func() {
			defer workers.Done()
			<-start
			errs <- catalog.BindReadyGenerationLeaseToCheckout(ctx,
				BindReadyGenerationLeaseToCheckoutRequest{
					Key: keyA, LeaseToken: claim.LeaseToken, CheckoutID: checkoutID,
					ExpectedRouteEpoch: route.RouteEpoch, GenerationID: generationA,
					State: RouteActive,
				})
		}()
		close(start)
		workers.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("iteration %d concurrent prune/bind: %v", i, err)
			}
		}

		route, found, err = catalog.GetCheckoutRoute(ctx, checkoutID)
		if err != nil || !found || route.CommitGenerationID != generationA {
			t.Fatalf("iteration %d final route: found=%v route=%+v err=%v, want A=%d",
				i, found, route, err, generationA)
		}
		pins := checkoutCommitCacheTestPinIDs(checkoutCommitCacheTestPins(t, catalog, graphID))
		if pins[generationA] != 1 {
			t.Fatalf("iteration %d winner A pin count=%d, want 1", i, pins[generationA])
		}
		var queued int
		if err := store.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM checkout_commit_cache_retirements WHERE generation_id = ?`,
			generationA).Scan(&queued); err != nil {
			t.Fatal(err)
		}
		if queued != 0 {
			t.Fatalf("iteration %d winner A retained %d retirement queue row(s)", i, queued)
		}

		if err := catalog.FlipCheckoutRouteSlot(ctx, FlipCheckoutRouteSlotRequest{
			CheckoutID: checkoutID, Slot: RouteSlotCommit, GenerationID: generationB,
			ExpectedRouteEpoch: route.RouteEpoch, State: RouteActive,
		}); err != nil {
			t.Fatalf("iteration %d reset B route: %v", i, err)
		}
	}
}

func BenchmarkCheckoutCommitCachePinOperations(b *testing.B) {
	b.Run("Bind", func(b *testing.B) {
		ctx := context.Background()
		store := openCatalogStore(b)
		catalog := store.Catalog()
		const (
			graphID    = "cache-bind-benchmark"
			checkoutID = "cache-bind-benchmark-checkout"
		)
		oldGenerationID, _ := checkoutCommitCacheTestGeneration(b, catalog, graphID, "bench-old")
		newGenerationID, newKey := checkoutCommitCacheTestGeneration(b, catalog, graphID, "bench-new")
		checkoutCommitCacheTestRoute(b, catalog, checkoutID, graphID, oldGenerationID)

		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			route, found, err := catalog.GetCheckoutRoute(ctx, checkoutID)
			if err != nil || !found {
				b.Fatalf("get route: found=%v err=%v", found, err)
			}
			if route.CommitGenerationID != oldGenerationID {
				if err := catalog.FlipCheckoutRouteSlot(ctx, FlipCheckoutRouteSlotRequest{
					CheckoutID: checkoutID, Slot: RouteSlotCommit, GenerationID: oldGenerationID,
					ExpectedRouteEpoch: route.RouteEpoch, State: RouteActive,
				}); err != nil {
					b.Fatal(err)
				}
				route, _, _ = catalog.GetCheckoutRoute(ctx, checkoutID)
			}
			claim, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{
				Key: newKey, CandidateGenerationID: newGenerationID,
				LeaseToken: fmt.Sprintf("cache-bind-benchmark-%d", i),
			})
			if err != nil || !found {
				b.Fatalf("claim: found=%v err=%v", found, err)
			}
			b.StartTimer()
			if err := catalog.BindReadyGenerationLeaseToCheckout(ctx,
				BindReadyGenerationLeaseToCheckoutRequest{
					Key: newKey, LeaseToken: claim.LeaseToken, CheckoutID: checkoutID,
					ExpectedRouteEpoch: route.RouteEpoch, GenerationID: newGenerationID,
					State: RouteActive,
				}); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(2, "pin-upserts/op")
	})

	b.Run("Prune96To32", func(b *testing.B) {
		ctx := context.Background()
		store := openCatalogStore(b)
		catalog := store.Catalog()
		const (
			graphID    = "cache-prune-benchmark"
			checkoutID = "cache-prune-benchmark-checkout"
			candidates = 96
		)
		checkoutCommitCacheTestCheckout(b, catalog, checkoutID)
		generationIDs := make([]int64, candidates)
		for i := range generationIDs {
			generationIDs[i], _ = checkoutCommitCacheTestGeneration(
				b, catalog, graphID, fmt.Sprintf("prune-%03d", i),
			)
		}
		seed := func() {
			b.Helper()
			err := catalog.withTx(ctx, func(tx *sql.Tx) error {
				for i, generationID := range generationIDs {
					if err := upsertCheckoutCommitCachePinTx(
						ctx, tx, checkoutID, graphID, generationID, int64(i+1),
					); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				b.Fatal(err)
			}
		}

		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			seed()
			b.StartTimer()
			got, err := catalog.PruneCheckoutCommitCachePins(ctx, CheckoutCommitCacheRetention{
				InactiveCutoff: 0, MaxGenerations: 32, MaxStorageBytes: 1 << 30,
			})
			if err != nil {
				b.Fatal(err)
			}
			if len(got.EvictedGenerationIDs) != candidates-32 {
				b.Fatalf("evicted=%d, want %d", len(got.EvictedGenerationIDs), candidates-32)
			}
		}
		b.ReportMetric(candidates, "generations/op")
		b.ReportMetric(candidates-32, "evictions/op")
	})

	b.Run("GraphTransitionReplay32Cached", func(b *testing.B) {
		ctx := context.Background()
		store := openCatalogStore(b)
		catalog := store.Catalog()
		const (
			checkoutID   = "cache-transition-replay-checkout"
			targetGraph  = "cache-transition-replay-target"
			foreignGraph = "cache-transition-replay-foreign"
			targetCached = 32
		)
		checkoutCommitCacheTestCheckout(b, catalog, checkoutID)
		current, _ := checkoutCommitCacheTestGeneration(b, catalog, targetGraph, "replay-current")
		for i := 0; i < targetCached; i++ {
			generationID, _ := checkoutCommitCacheTestGeneration(
				b, catalog, targetGraph, fmt.Sprintf("replay-target-%02d", i),
			)
			checkoutCommitCacheTestSetPin(
				b, catalog, checkoutID, targetGraph, generationID, int64(i+1), 1,
			)
		}
		foreign, _ := checkoutCommitCacheTestGeneration(b, catalog, foreignGraph, "replay-foreign")

		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			checkoutCommitCacheTestSetPin(
				b, catalog, checkoutID, foreignGraph, foreign, int64(i+1), 1,
			)
			b.StartTimer()
			err := catalog.withTx(ctx, func(tx *sql.Tx) error {
				if err := deleteCheckoutCommitCachePinsOutsideGraphTx(
					ctx, tx, checkoutID, targetGraph,
				); err != nil {
					return err
				}
				return upsertCheckoutCommitCachePinTx(
					ctx, tx, checkoutID, targetGraph, current, int64(i+1),
				)
			})
			if err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(targetCached, "preserved-target-pins/op")
		b.ReportMetric(1, "removed-foreign-pins/op")
	})
}
