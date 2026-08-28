package indexer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

const retirementPaginationTestPageSize = 512

func seedLifecycleListingGeneration(
	t testing.TB,
	store *store_sqlite.Store,
	request store_sqlite.PayloadGenerationRequest,
	state store_sqlite.ViewGenerationState,
) int64 {
	t.Helper()
	generationID, handle, adopted, err := store.BeginPayloadGenerationWithStatus(context.Background(), request)
	if err != nil {
		t.Fatalf("begin generation %s: %v", request.LayerID, err)
	}
	if adopted {
		t.Fatalf("generation %s unexpectedly adopted generation %d", request.LayerID, generationID)
	}
	if handle != nil {
		_ = handle.Close()
	}
	if state != store_sqlite.ViewGenerationBuilding {
		if err := store.Catalog().SetViewGenerationState(
			context.Background(), generationID, state, store_sqlite.ViewGenerationBuilding,
		); err != nil {
			t.Fatalf("mark generation %s %s: %v", request.LayerID, state, err)
		}
	}
	return generationID
}

func requireCatalogGenerationPresent(t testing.TB, store *store_sqlite.Store, generationID int64) {
	t.Helper()
	if _, found, err := store.Catalog().GetViewGeneration(context.Background(), generationID); err != nil {
		t.Fatalf("get generation %d: %v", generationID, err)
	} else if !found {
		t.Fatalf("generation %d was unexpectedly retired", generationID)
	}
}

func requireOnlyRetirementCandidate(t testing.TB, got []int64, want int64) {
	t.Helper()
	if len(got) != 1 || got[0] != want {
		t.Fatalf("retirement candidates = %v, want only %d", got, want)
	}
}

func TestCheckoutLifecyclePaginatesDiscardedPastServedRows(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	ctx := context.Background()
	request := payloadRequestForBuild(fixture.request)
	request.GraphID = ""
	request.CheckoutID = "orphan-discarded"
	request.LayerID = "discarded-orphan"
	orphan := seedLifecycleListingGeneration(t, fixture.store, request, store_sqlite.ViewGenerationSuperseded)

	request.CheckoutID = "served-discarded"
	for i := 0; i <= retirementPaginationTestPageSize; i++ {
		request.LayerID = fmt.Sprintf("discarded-served-%04d", i)
		state := store_sqlite.ViewGenerationSuperseded
		if i%2 != 0 {
			state = store_sqlite.ViewGenerationRetiring
		}
		seedLifecycleListingGeneration(t, fixture.store, request, state)
	}

	lifecycle := newGenerationRetirementLifecycle(fixture.store, time.Now())
	candidates := lifecycle.orphanedGenerations(ctx, map[string]struct{}{"served-discarded": {}}, nil)
	requireOnlyRetirementCandidate(t, candidates, orphan)
}

func TestCheckoutLifecyclePaginatesReadyLayersPastServedRows(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	ctx := context.Background()
	request := payloadRequestForBuild(fixture.request)
	request.GraphID = ""
	request.OwnerKind = checkoutLayerOwnerKind
	request.GenerationKind = CommitLayerGenerationKind
	request.CheckoutID = "orphan-ready-layer"
	request.LayerID = "ready-layer-orphan"
	orphan := seedLifecycleListingGeneration(t, fixture.store, request, store_sqlite.ViewGenerationReady)

	request.CheckoutID = "served-ready-layer"
	for i := 0; i <= retirementPaginationTestPageSize; i++ {
		request.LayerID = fmt.Sprintf("ready-layer-served-%04d", i)
		seedLifecycleListingGeneration(t, fixture.store, request, store_sqlite.ViewGenerationReady)
	}

	lifecycle := newGenerationRetirementLifecycle(fixture.store, time.Now())
	candidates := lifecycle.orphanedGenerations(ctx, map[string]struct{}{"served-ready-layer": {}}, nil)
	requireOnlyRetirementCandidate(t, candidates, orphan)
}

func TestCheckoutLifecycleRetirementPreservesRouteRefBaseAndLease(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	ctx := context.Background()
	catalog := fixture.store.Catalog()
	const (
		familyID   = "family-retirement-protection"
		checkoutID = "checkout-retirement-protection"
		graphID    = "graph-retirement-protection"
	)
	now := time.Now().Unix()
	if err := catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          familyID,
		CommonDirIdentity: "common-retirement-protection",
		State:             "ready",
		CreatedAt:         now,
		LastSeen:          now,
	}); err != nil {
		t.Fatalf("upsert family: %v", err)
	}
	if err := catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:    checkoutID,
		Incarnation:   "incarnation-retirement-protection",
		FamilyID:      familyID,
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeAutomatic,
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
		LastSeen:      now,
	}); err != nil {
		t.Fatalf("upsert checkout: %v", err)
	}
	if err := catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID:         graphID,
		OwnerCheckoutID: checkoutID,
		RepoPrefix:      "repo-retirement-protection",
		FamilyID:        familyID,
		IsPrimaryBase:   true,
		State:           "ready",
	}); err != nil {
		t.Fatalf("upsert graph: %v", err)
	}

	request := payloadRequestForBuild(fixture.request)
	request.GraphID = graphID
	request.CheckoutID = checkoutID
	request.OwnerKind = checkoutLayerOwnerKind
	request.GenerationKind = CommitLayerGenerationKind
	request.LayerID = "route-protected"
	routeProtected := seedLifecycleListingGeneration(t, fixture.store, request, store_sqlite.ViewGenerationReady)
	if err := catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
		CheckoutID:         checkoutID,
		GraphID:            graphID,
		CommitGenerationID: routeProtected,
		State:              store_sqlite.RouteActive,
	}); err != nil {
		t.Fatalf("upsert route: %v", err)
	}

	request.OwnerKind = "dedicated_graph"
	request.GenerationKind = "base"
	request.LayerID = "base-protected"
	baseProtected := seedLifecycleListingGeneration(t, fixture.store, request, store_sqlite.ViewGenerationSuperseded)
	request.LayerID = "base-child"
	request.BaseGenerationID = baseProtected
	baseChild := seedLifecycleListingGeneration(t, fixture.store, request, store_sqlite.ViewGenerationReady)
	request.BaseGenerationID = 0

	request.LayerID = "ref-protected"
	refProtected := seedLifecycleListingGeneration(t, fixture.store, request, store_sqlite.ViewGenerationSuperseded)
	if err := catalog.UpsertRefView(ctx, store_sqlite.RefView{
		RefViewID:          "ref-retirement-protection",
		GraphID:            graphID,
		SelectorKind:       "branch",
		SelectorValue:      "main",
		ActiveGenerationID: refProtected,
		EnrichmentProfile:  "default",
		State:              store_sqlite.RefViewReady,
	}); err != nil {
		t.Fatalf("upsert ref view: %v", err)
	}

	request.LayerID = "lease-protected"
	leaseProtected := seedLifecycleListingGeneration(t, fixture.store, request, store_sqlite.ViewGenerationSuperseded)
	request.LayerID = "unreferenced-orphan"
	orphan := seedLifecycleListingGeneration(t, fixture.store, request, store_sqlite.ViewGenerationSuperseded)

	lifecycle := newGenerationRetirementLifecycle(fixture.store, time.Now())
	lease := lifecycle.leases.Acquire(leaseProtected)
	defer lease.Release()
	if retired := lifecycle.sweepRetirements(ctx); retired != 1 {
		t.Fatalf("retired generations = %d, want only the unreferenced orphan", retired)
	}
	requireGenerationRetired(t, fixture.store, orphan)
	for _, generationID := range []int64{routeProtected, baseProtected, baseChild, refProtected, leaseProtected} {
		requireCatalogGenerationPresent(t, fixture.store, generationID)
	}
}

func BenchmarkCheckoutLifecycleRetirementPagination(b *testing.B) {
	for _, rows := range []int{512, 1024, 10000} {
		b.Run(fmt.Sprintf("%d_rows", rows), func(b *testing.B) {
			fixture := newSparseBuildFlightFixture(b)
			request := payloadRequestForBuild(fixture.request)
			request.GraphID = ""
			request.CheckoutID = "pagination-orphan"
			request.LayerID = "pagination-orphan"
			seedLifecycleListingGeneration(b, fixture.store, request, store_sqlite.ViewGenerationSuperseded)
			request.CheckoutID = "pagination-served"
			for i := 1; i < rows; i++ {
				request.LayerID = fmt.Sprintf("pagination-served-%05d", i)
				seedLifecycleListingGeneration(b, fixture.store, request, store_sqlite.ViewGenerationSuperseded)
			}

			lifecycle := newGenerationRetirementLifecycle(fixture.store, time.Now())
			served := map[string]struct{}{"pagination-served": {}}
			listingQueries := (rows + retirementPaginationTestPageSize - 1) / retirementPaginationTestPageSize
			if rows%retirementPaginationTestPageSize == 0 {
				listingQueries++
			}
			listingQueries += 4 // failed, building, missing-graph, and ready-layer cohorts

			b.ReportAllocs()
			b.ResetTimer()
			var candidates int
			for i := 0; i < b.N; i++ {
				candidates = len(lifecycle.orphanedGenerations(context.Background(), served, nil))
			}
			b.ReportMetric(float64(candidates), "candidates/op")
			b.ReportMetric(float64(listingQueries), "queries/op")
		})
	}
}
