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
	candidates := lifecycle.orphanedGenerations(ctx, map[string]struct{}{"served-discarded": {}}, nil, true)
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
	candidates := lifecycle.orphanedGenerations(ctx, map[string]struct{}{"served-ready-layer": {}}, nil, true)
	requireOnlyRetirementCandidate(t, candidates, orphan)
}

func TestCheckoutLifecycleRuntimeSweepDefersUnqueuedReadyPublicationCandidateToSeed(t *testing.T) {
	for _, generationKind := range []string{CommitLayerGenerationKind, DirtyLayerGenerationKind} {
		t.Run(generationKind, func(t *testing.T) {
			fixture := newSparseBuildFlightFixture(t)
			ctx := context.Background()
			request := payloadRequestForBuild(fixture.request)
			request.GraphID = ""
			request.OwnerKind = checkoutLayerOwnerKind
			request.CheckoutID = "publication-window-" + generationKind
			request.GenerationKind = generationKind
			request.LayerID = "publication-window-" + generationKind
			generationID := seedLifecycleListingGeneration(
				t, fixture.store, request, store_sqlite.ViewGenerationReady,
			)

			lifecycle := newGenerationRetirementLifecycle(fixture.store, time.Now())
			if retired := lifecycle.sweepRetirements(ctx); retired != 0 {
				t.Fatalf("runtime sweep retired %d generation(s), want publication candidate deferred", retired)
			}
			if _, found, err := fixture.store.Catalog().GetViewGeneration(ctx, generationID); err != nil {
				t.Fatal(err)
			} else if !found {
				t.Fatalf("runtime sweep collected ready publication candidate %d", generationID)
			}

			if retired := lifecycle.sweepStartupRetirements(ctx); retired != 1 {
				t.Fatalf("startup sweep retired %d generation(s), want 1 prior-process orphan", retired)
			}
			if _, found, err := fixture.store.Catalog().GetViewGeneration(ctx, generationID); err != nil {
				t.Fatal(err)
			} else if found {
				t.Fatalf("startup sweep retained prior-process ready orphan %d", generationID)
			}
		})
	}
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

func TestReadyLayerRetirementCandidatesReadOncePerPageAndCacheMissing(t *testing.T) {
	const pageSize = retirementPaginationTestPageSize
	pageOne := make([]store_sqlite.ViewGeneration, pageSize)
	for i := range pageOne {
		pageOne[i] = store_sqlite.ViewGeneration{
			GenerationID:   int64(i + 1),
			CheckoutID:     fmt.Sprintf("checkout-page-one-%04d", i),
			GenerationKind: CommitLayerGenerationKind,
		}
	}
	pageTwo := make([]store_sqlite.ViewGeneration, pageSize)
	pageTwo[0] = store_sqlite.ViewGeneration{
		GenerationID:   int64(pageSize + 1),
		CheckoutID:     pageOne[1].CheckoutID,
		GenerationKind: CommitLayerGenerationKind,
	}
	for i := 1; i < len(pageTwo); i++ {
		pageTwo[i] = store_sqlite.ViewGeneration{
			GenerationID:   int64(pageSize + i + 1),
			CheckoutID:     fmt.Sprintf("checkout-page-two-%04d", i),
			GenerationKind: CommitLayerGenerationKind,
		}
	}

	var lookupSizes []int
	lookup := func(_ context.Context, checkoutIDs []string) (map[string]store_sqlite.CheckoutRoute, error) {
		lookupSizes = append(lookupSizes, len(checkoutIDs))
		resolved := map[string]store_sqlite.CheckoutRoute{}
		for _, checkoutID := range checkoutIDs {
			if checkoutID == pageOne[0].CheckoutID {
				resolved[checkoutID] = store_sqlite.CheckoutRoute{
					CheckoutID:         checkoutID,
					CommitGenerationID: pageOne[0].GenerationID,
				}
			}
		}
		return resolved, nil
	}
	routes := map[string]store_sqlite.CheckoutRoute{}
	first, err := readyLayerRetirementCandidates(context.Background(), pageOne, nil, routes, lookup)
	if err != nil {
		t.Fatalf("first ready-layer page: %v", err)
	}
	second, err := readyLayerRetirementCandidates(context.Background(), pageTwo, nil, routes, lookup)
	if err != nil {
		t.Fatalf("second ready-layer page: %v", err)
	}
	if len(first) != pageSize-1 || len(second) != pageSize {
		t.Fatalf("candidate counts = (%d, %d), want (%d, %d)", len(first), len(second), pageSize-1, pageSize)
	}
	if len(lookupSizes) != 2 || lookupSizes[0] != pageSize || lookupSizes[1] != pageSize-1 {
		t.Fatalf("route lookup sizes = %v, want one lookup per page with cached repeat: [%d %d]", lookupSizes, pageSize, pageSize-1)
	}
	if len(routes) != pageSize*2-1 {
		t.Fatalf("route cache entries = %d, want %d including missing routes", len(routes), pageSize*2-1)
	}
}

func TestCheckoutLifecycleRouteFlipAfterBatchProtectsGeneration(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	ctx := context.Background()
	catalog := fixture.store.Catalog()
	const (
		familyID   = "family-route-flip-retirement"
		checkoutID = "checkout-route-flip-retirement"
		graphID    = "graph-route-flip-retirement"
	)
	now := time.Now().Unix()
	if err := catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          familyID,
		CommonDirIdentity: "common-route-flip-retirement",
		State:             "ready",
		CreatedAt:         now,
		LastSeen:          now,
	}); err != nil {
		t.Fatalf("upsert family: %v", err)
	}
	if err := catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:    checkoutID,
		Incarnation:   "incarnation-route-flip-retirement",
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
		RepoPrefix:      "repo-route-flip-retirement",
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
	request.LayerID = "route-flip-old"
	oldRouted := seedLifecycleListingGeneration(t, fixture.store, request, store_sqlite.ViewGenerationReady)
	request.LayerID = "route-flip-candidate"
	candidate := seedLifecycleListingGeneration(t, fixture.store, request, store_sqlite.ViewGenerationReady)
	const routeEpoch = 17
	if err := catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
		CheckoutID:         checkoutID,
		GraphID:            graphID,
		CommitGenerationID: oldRouted,
		RouteEpoch:         routeEpoch,
		State:              store_sqlite.RouteActive,
	}); err != nil {
		t.Fatalf("upsert route: %v", err)
	}

	batchRead := make(chan struct{})
	flipDone := make(chan error, 1)
	go func() {
		<-batchRead
		flipDone <- catalog.FlipCheckoutRouteSlot(ctx, store_sqlite.FlipCheckoutRouteSlotRequest{
			CheckoutID:         checkoutID,
			Slot:               store_sqlite.RouteSlotCommit,
			GenerationID:       candidate,
			ExpectedRouteEpoch: routeEpoch,
			State:              store_sqlite.RouteActive,
		})
	}()
	lookup := func(ctx context.Context, checkoutIDs []string) (map[string]store_sqlite.CheckoutRoute, error) {
		routes, err := catalog.GetCheckoutRoutes(ctx, checkoutIDs)
		if err != nil {
			return nil, err
		}
		close(batchRead)
		if err := <-flipDone; err != nil {
			return nil, err
		}
		return routes, nil
	}
	stale, err := readyLayerRetirementCandidates(ctx, []store_sqlite.ViewGeneration{{
		GenerationID:   candidate,
		CheckoutID:     checkoutID,
		GenerationKind: CommitLayerGenerationKind,
	}}, nil, map[string]store_sqlite.CheckoutRoute{}, lookup)
	if err != nil {
		t.Fatalf("classify stale route snapshot: %v", err)
	}
	requireOnlyRetirementCandidate(t, generationIDs(stale), candidate)

	lifecycle := newGenerationRetirementLifecycle(fixture.store, time.Now())
	lifecycle.coordMu.Lock()
	lifecycle.owed[candidate] = struct{}{}
	lifecycle.coordMu.Unlock()
	lifecycle.sweepRetirements(ctx)
	requireCatalogGenerationPresent(t, fixture.store, candidate)
	lifecycle.coordMu.Lock()
	_, stillOwed := lifecycle.owed[candidate]
	lifecycle.coordMu.Unlock()
	if stillOwed {
		t.Fatal("durably pinned route generation remained as process-local retry debt")
	}

	route, found, err := catalog.GetCheckoutRoute(ctx, checkoutID)
	if err != nil || !found {
		t.Fatalf("read candidate route: found=%v err=%v", found, err)
	}
	if err := catalog.FlipCheckoutRouteSlot(ctx, store_sqlite.FlipCheckoutRouteSlotRequest{
		CheckoutID: checkoutID, Slot: store_sqlite.RouteSlotCommit, GenerationID: oldRouted,
		ExpectedRouteEpoch: route.RouteEpoch, State: store_sqlite.RouteActive,
	}); err != nil {
		t.Fatalf("move route off candidate: %v", err)
	}
	if _, err := catalog.PruneCheckoutCommitCachePins(ctx,
		store_sqlite.CheckoutCommitCacheRetention{
			InactiveCutoff:  time.Now().Add(time.Second).Unix(),
			MaxGenerations:  32,
			MaxStorageBytes: 1 << 62,
		}); err != nil {
		t.Fatalf("evict candidate pin: %v", err)
	}
	if retired := lifecycle.sweepRetirements(ctx); retired == 0 {
		t.Fatal("pin deletion did not recreate durable retirement work")
	}
	requireGenerationRetired(t, fixture.store, candidate)
}

func generationIDs(rows []store_sqlite.ViewGeneration) []int64 {
	ids := make([]int64, len(rows))
	for i, row := range rows {
		ids[i] = row.GenerationID
	}
	return ids
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
				candidates = len(lifecycle.orphanedGenerations(context.Background(), served, nil, true))
			}
			b.ReportMetric(float64(candidates), "candidates/op")
			b.ReportMetric(float64(listingQueries), "queries/op")
		})
	}
}

func BenchmarkCheckoutLifecycleReadyLayerRouteBatching(b *testing.B) {
	for _, rows := range []int{512, 1024, 10000} {
		b.Run(fmt.Sprintf("%d_distinct_checkouts", rows), func(b *testing.B) {
			fixture := newSparseBuildFlightFixture(b)
			request := payloadRequestForBuild(fixture.request)
			request.GraphID = ""
			request.OwnerKind = checkoutLayerOwnerKind
			request.GenerationKind = CommitLayerGenerationKind
			for i := 0; i < rows; i++ {
				request.CheckoutID = fmt.Sprintf("ready-route-checkout-%05d", i)
				request.LayerID = fmt.Sprintf("ready-route-layer-%05d", i)
				seedLifecycleListingGeneration(b, fixture.store, request, store_sqlite.ViewGenerationReady)
			}

			lifecycle := newGenerationRetirementLifecycle(fixture.store, time.Now())
			routeBatches := (rows + retirementPaginationTestPageSize - 1) / retirementPaginationTestPageSize
			b.ReportAllocs()
			b.ResetTimer()
			var candidates int
			for i := 0; i < b.N; i++ {
				candidates = len(lifecycle.orphanedGenerations(context.Background(), nil, nil, true))
			}
			b.ReportMetric(float64(candidates), "candidates/op")
			b.ReportMetric(float64(routeBatches), "route_batches/op")
		})
	}
}

func BenchmarkCheckoutLifecycleRuntimeRetirementScanWith10000Ready(b *testing.B) {
	fixture := newSparseBuildFlightFixture(b)
	request := payloadRequestForBuild(fixture.request)
	request.GraphID = ""
	request.OwnerKind = checkoutLayerOwnerKind
	request.GenerationKind = CommitLayerGenerationKind
	for i := 0; i < 10_000; i++ {
		request.CheckoutID = fmt.Sprintf("runtime-ready-checkout-%05d", i)
		request.LayerID = fmt.Sprintf("runtime-ready-layer-%05d", i)
		seedLifecycleListingGeneration(b, fixture.store, request, store_sqlite.ViewGenerationReady)
	}

	lifecycle := newGenerationRetirementLifecycle(fixture.store, time.Now())
	b.ReportAllocs()
	b.ResetTimer()
	var candidates int
	for i := 0; i < b.N; i++ {
		candidates = len(lifecycle.orphanedGenerations(context.Background(), nil, nil, false))
	}
	b.ReportMetric(float64(candidates), "candidates/op")
	b.ReportMetric(4, "queries/op")
}
