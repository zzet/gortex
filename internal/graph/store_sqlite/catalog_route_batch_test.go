package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestGetCheckoutRoutesBatchedBoundsAndDeduplicates(t *testing.T) {
	ids := make([]string, 0, 1028)
	for i := 0; i < 1025; i++ {
		ids = append(ids, fmt.Sprintf("checkout-%05d", i))
	}
	ids = append(ids, "", "checkout-00000", "checkout-01024")

	var batches [][]string
	routes, err := getCheckoutRoutesBatched(
		context.Background(),
		ids,
		func(_ context.Context, batch []string) (map[string]CheckoutRoute, error) {
			batches = append(batches, append([]string(nil), batch...))
			got := make(map[string]CheckoutRoute, len(batch))
			for _, checkoutID := range batch {
				got[checkoutID] = CheckoutRoute{CheckoutID: checkoutID, GraphID: "graph"}
			}
			return got, nil
		},
	)
	if err != nil {
		t.Fatalf("get checkout routes batched: %v", err)
	}
	gotSizes := make([]int, len(batches))
	for i, batch := range batches {
		gotSizes[i] = len(batch)
		if len(batch) > checkoutRouteLookupBatchSize {
			t.Fatalf("batch %d has %d ids, limit %d", i, len(batch), checkoutRouteLookupBatchSize)
		}
	}
	if want := []int{512, 512, 1}; !reflect.DeepEqual(gotSizes, want) {
		t.Fatalf("batch sizes = %v, want %v", gotSizes, want)
	}
	if len(routes) != 1025 {
		t.Fatalf("routes = %d, want 1025 de-duplicated ids", len(routes))
	}
	if routes["checkout-00000"].GraphID != "graph" || routes["checkout-01024"].GraphID != "graph" {
		t.Fatalf("boundary routes missing: first=%+v last=%+v", routes["checkout-00000"], routes["checkout-01024"])
	}
}

func TestGetCheckoutRoutesBatchedEmptyAndCancellation(t *testing.T) {
	called := false
	read := func(_ context.Context, _ []string) (map[string]CheckoutRoute, error) {
		called = true
		return nil, nil
	}
	routes, err := getCheckoutRoutesBatched(context.Background(), nil, read)
	if err != nil {
		t.Fatalf("empty lookup: %v", err)
	}
	if called {
		t.Fatal("empty lookup issued a batch read")
	}
	if routes == nil || len(routes) != 0 {
		t.Fatalf("empty lookup routes = %#v, want a non-nil empty map", routes)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := getCheckoutRoutesBatched(ctx, []string{"checkout"}, read); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lookup error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("canceled lookup issued a batch read")
	}

	betweenBatches, cancelBetweenBatches := context.WithCancel(context.Background())
	many := make([]string, checkoutRouteLookupBatchSize+1)
	for i := range many {
		many[i] = fmt.Sprintf("checkout-cancel-%04d", i)
	}
	batchCalls := 0
	_, err = getCheckoutRoutesBatched(
		betweenBatches,
		many,
		func(_ context.Context, _ []string) (map[string]CheckoutRoute, error) {
			batchCalls++
			cancelBetweenBatches()
			return map[string]CheckoutRoute{}, nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("between-batch cancellation error = %v, want context.Canceled", err)
	}
	if batchCalls != 1 {
		t.Fatalf("batch reads after cancellation = %d, want 1", batchCalls)
	}
}

func TestCatalogGetCheckoutRoutesReturnsFoundAndOmitsMissing(t *testing.T) {
	store, _ := payloadLifecycleRaceStore(t, "catalog-route-batch")
	catalog := store.Catalog()
	ctx := context.Background()
	const (
		familyID = "family-catalog-route-batch"
		graphID  = "graph-catalog-route-batch"
	)
	if err := catalog.UpsertRepositoryFamily(ctx, RepositoryFamily{
		FamilyID:          familyID,
		CommonDirIdentity: "common-catalog-route-batch",
		State:             "ready",
	}); err != nil {
		t.Fatalf("upsert family: %v", err)
	}
	if err := catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
		GraphID:    graphID,
		RepoPrefix: "repo-catalog-route-batch",
		FamilyID:   familyID,
		State:      "ready",
	}); err != nil {
		t.Fatalf("upsert graph: %v", err)
	}
	for i, checkoutID := range []string{"checkout-route-a", "checkout-route-b"} {
		if err := catalog.UpsertCheckout(ctx, Checkout{
			CheckoutID:    checkoutID,
			Incarnation:   fmt.Sprintf("incarnation-%d", i),
			FamilyID:      familyID,
			State:         CheckoutStateReady,
			DesiredMode:   CheckoutModeAutomatic,
			EffectiveMode: CheckoutModeAutomatic,
		}); err != nil {
			t.Fatalf("upsert checkout %s: %v", checkoutID, err)
		}
		if err := catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
			CheckoutID: checkoutID,
			GraphID:    graphID,
			RouteEpoch: int64(i + 7),
			State:      RouteActive,
		}); err != nil {
			t.Fatalf("upsert route %s: %v", checkoutID, err)
		}
	}

	routes, err := catalog.GetCheckoutRoutes(ctx, []string{
		"checkout-route-b", "missing-route", "checkout-route-a", "checkout-route-b", "",
	})
	if err != nil {
		t.Fatalf("get checkout routes: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("routes = %#v, want exactly two found rows", routes)
	}
	if _, found := routes["missing-route"]; found {
		t.Fatal("missing route was returned")
	}
	if got := routes["checkout-route-a"]; got.CheckoutID != "checkout-route-a" || got.GraphID != graphID || got.RouteEpoch != 7 || got.State != RouteActive {
		t.Fatalf("route a = %+v", got)
	}
	if got := routes["checkout-route-b"]; got.CheckoutID != "checkout-route-b" || got.GraphID != graphID || got.RouteEpoch != 8 || got.State != RouteActive {
		t.Fatalf("route b = %+v", got)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := catalog.GetCheckoutRoutes(canceled, []string{"checkout-route-a"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled catalog lookup error = %v, want context.Canceled", err)
	}
}
