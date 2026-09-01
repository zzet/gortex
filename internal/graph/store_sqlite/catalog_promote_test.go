package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

type authorizedPromotionFixture struct {
	store   *Store
	catalog *Catalog
	req     CommitAuthorizedPromotionRequest
}

func newAuthorizedPromotionFixture(tb testing.TB, suffix string) authorizedPromotionFixture {
	tb.Helper()
	store, err := Open(filepath.Join(tb.TempDir(), "promotion-"+suffix+".sqlite"))
	if err != nil {
		tb.Fatalf("Open promotion catalog: %v", err)
	}
	tb.Cleanup(func() { _ = store.Close() })
	catalog := store.Catalog()
	ctx := context.Background()

	familyID := "family-" + suffix
	checkoutID := "checkout-" + suffix
	incarnation := "incarnation-" + suffix
	transitionID := "transition-" + suffix
	graphID := "graph-" + suffix
	repoPrefix := "repo@" + suffix
	treeOID := "tree-" + suffix

	mustCatalogWrite(tb, "upsert family", catalog.UpsertRepositoryFamily(ctx, RepositoryFamily{
		FamilyID:          familyID,
		CommonDirIdentity: "identity-" + suffix,
		DisplayRemote:     "git@example.invalid:" + familyID + ".git",
		State:             "family_ready",
		CreatedAt:         100,
		LastSeen:          100,
	}))
	mustCatalogWrite(tb, "upsert checkout", catalog.UpsertCheckout(ctx, Checkout{
		CheckoutID:    checkoutID,
		Incarnation:   incarnation,
		FamilyID:      familyID,
		RootPath:      "/tmp/" + checkoutID,
		GitDir:        "/tmp/" + checkoutID + "/.git",
		AdminName:     checkoutID,
		State:         CheckoutStateReady,
		DesiredMode:   CheckoutModeAutomatic,
		EffectiveMode: CheckoutModeAutomatic,
		HeadRef:       "refs/heads/main",
		HeadCommit:    "commit-" + suffix,
		HeadTree:      treeOID,
		LastSeen:      101,
	}))
	mustCatalogWrite(tb, "begin promotion transition", catalog.BeginIntentTransition(ctx, IntentTransition{
		TransitionID:       transitionID,
		CheckoutID:         checkoutID,
		Cause:              "promote_checkout",
		PriorDesiredMode:   CheckoutModeAutomatic,
		PriorEffectiveMode: CheckoutModeAutomatic,
		RequestedMode:      CheckoutModeDedicated,
		PriorCheckoutState: CheckoutStateReady,
		State:              IntentTransitionPending,
		CreatedAt:          200,
		LastProgress:       200,
	}))
	mustCatalogWrite(tb, "upsert dedicated graph", catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
		GraphID:         graphID,
		OwnerCheckoutID: checkoutID,
		RepoPrefix:      repoPrefix,
		FamilyID:        familyID,
		State:           "graph_ready",
	}))

	baseID := createReadyPromotionGeneration(tb, catalog, ViewGeneration{
		OwnerKind:      "dedicated_graph",
		GraphID:        graphID,
		CheckoutID:     checkoutID,
		GenerationKind: "base",
		TreeOID:        treeOID,
		CreatedAt:      201,
	})
	commitID := createReadyPromotionGeneration(tb, catalog, ViewGeneration{
		OwnerKind:        "checkout_commit",
		GraphID:          graphID,
		CheckoutID:       checkoutID,
		GenerationKind:   "commit",
		BaseGenerationID: baseID,
		TreeOID:          treeOID,
		CreatedAt:        202,
	})
	dirtyID := createReadyPromotionGeneration(tb, catalog, ViewGeneration{
		OwnerKind:        "checkout_dirty",
		GraphID:          graphID,
		CheckoutID:       checkoutID,
		GenerationKind:   "dirty",
		BaseGenerationID: commitID,
		TreeOID:          treeOID,
		CreatedAt:        203,
	})

	return authorizedPromotionFixture{
		store:   store,
		catalog: catalog,
		req: CommitAuthorizedPromotionRequest{
			CheckoutID:         checkoutID,
			Incarnation:        incarnation,
			FamilyID:           familyID,
			TransitionID:       transitionID,
			RequiredCause:      "promote_checkout",
			GraphID:            graphID,
			RequiredGraphState: "graph_ready",
			BaseGenerationID:   baseID,
			BaseTreeOID:        treeOID,
			CommitGenerationID: commitID,
			DirtyGenerationID:  dirtyID,
			State:              CheckoutStateReady,
			LastSeen:           300,
		},
	}
}

func mustCatalogWrite(tb testing.TB, action string, err error) {
	tb.Helper()
	if err != nil {
		tb.Fatalf("%s: %v", action, err)
	}
}

func createReadyPromotionGeneration(
	tb testing.TB, catalog *Catalog, generation ViewGeneration,
) int64 {
	tb.Helper()
	generation.State = ViewGenerationBuilding
	generationID, err := catalog.CreateViewGeneration(context.Background(), generation)
	if err != nil {
		tb.Fatalf("CreateViewGeneration(%s): %v", generation.GenerationKind, err)
	}
	if err := catalog.PublishViewGeneration(context.Background(), generationID, generation.CreatedAt+10); err != nil {
		tb.Fatalf("PublishViewGeneration(%s): %v", generation.GenerationKind, err)
	}
	return generationID
}

func requirePromotionRollback(
	t *testing.T, fixture authorizedPromotionFixture, expectedRoute *CheckoutRoute,
) {
	t.Helper()
	ctx := context.Background()
	graph, found, err := fixture.catalog.GetDedicatedGraph(ctx, fixture.req.GraphID)
	if err != nil || !found {
		t.Fatalf("GetDedicatedGraph = %+v, found=%v, err=%v", graph, found, err)
	}
	if graph.ActiveGenerationID != 0 {
		t.Fatalf("graph active generation = %d, want 0 after rollback", graph.ActiveGenerationID)
	}
	checkout, found, err := fixture.catalog.GetCheckout(ctx, fixture.req.CheckoutID)
	if err != nil || !found {
		t.Fatalf("GetCheckout = %+v, found=%v, err=%v", checkout, found, err)
	}
	if checkout.DesiredMode != CheckoutModeAutomatic || checkout.EffectiveMode != CheckoutModeAutomatic {
		t.Fatalf("checkout modes = %s/%s, want automatic/automatic after rollback",
			checkout.DesiredMode, checkout.EffectiveMode)
	}
	route, routed, err := fixture.catalog.GetCheckoutRoute(ctx, fixture.req.CheckoutID)
	if err != nil {
		t.Fatalf("GetCheckoutRoute: %v", err)
	}
	if expectedRoute == nil {
		if routed {
			t.Fatalf("route survived rollback: %+v", route)
		}
		return
	}
	if !routed || route != *expectedRoute {
		t.Fatalf("route after rollback = %+v, found=%v, want %+v", route, routed, *expectedRoute)
	}
}

func TestCommitAuthorizedPromotionRollsBackWhenRoutePublicationFails(t *testing.T) {
	fixture := newAuthorizedPromotionFixture(t, "route-failure")
	ctx := context.Background()
	if _, err := fixture.store.writerDB.ExecContext(ctx, `
CREATE TRIGGER fail_promotion_route
BEFORE INSERT ON checkout_routes
BEGIN
  SELECT RAISE(ABORT, 'injected route publication failure');
END`); err != nil {
		t.Fatalf("create route failure trigger: %v", err)
	}

	if err := fixture.catalog.CommitAuthorizedPromotion(ctx, fixture.req); err == nil {
		t.Fatal("CommitAuthorizedPromotion succeeded despite route failure trigger")
	}
	requirePromotionRollback(t, fixture, nil)
}

func TestCommitAuthorizedPromotionRollsBackRouteWhenModeFlipFails(t *testing.T) {
	fixture := newAuthorizedPromotionFixture(t, "mode-failure")
	ctx := context.Background()
	existingRoute := CheckoutRoute{
		CheckoutID: fixture.req.CheckoutID,
		GraphID:    fixture.req.GraphID,
		RouteEpoch: 7,
		State:      RouteActive,
	}
	mustCatalogWrite(t, "seed existing route", fixture.catalog.UpsertCheckoutRoute(ctx, existingRoute))
	fixture.req.RouteExists = true
	fixture.req.ExpectedRouteEpoch = existingRoute.RouteEpoch
	if _, err := fixture.store.writerDB.ExecContext(ctx, `
CREATE TRIGGER fail_promotion_mode
BEFORE UPDATE OF effective_mode ON checkouts
WHEN NEW.effective_mode = 'dedicated'
BEGIN
  SELECT RAISE(ABORT, 'injected mode flip failure');
END`); err != nil {
		t.Fatalf("create mode failure trigger: %v", err)
	}

	if err := fixture.catalog.CommitAuthorizedPromotion(ctx, fixture.req); err == nil {
		t.Fatal("CommitAuthorizedPromotion succeeded despite mode failure trigger")
	}
	requirePromotionRollback(t, fixture, &existingRoute)
}

func TestCommitAuthorizedPromotionIsIdempotentAndProtectsItsRoute(t *testing.T) {
	fixture := newAuthorizedPromotionFixture(t, "idempotent")
	ctx := context.Background()

	owned, err := fixture.catalog.OwnsActiveDedicatedRoute(ctx, fixture.req.GraphID, "repo@idempotent")
	if err != nil || !owned {
		t.Fatalf("standing promotion route ownership = %v, err=%v", owned, err)
	}
	if err := fixture.catalog.CommitAuthorizedPromotion(ctx, fixture.req); err != nil {
		t.Fatalf("first CommitAuthorizedPromotion: %v", err)
	}
	route, found, err := fixture.catalog.GetCheckoutRoute(ctx, fixture.req.CheckoutID)
	if err != nil || !found {
		t.Fatalf("published route = %+v, found=%v, err=%v", route, found, err)
	}
	graph, found, err := fixture.catalog.GetDedicatedGraph(ctx, fixture.req.GraphID)
	if err != nil || !found {
		t.Fatalf("published graph = %+v, found=%v, err=%v", graph, found, err)
	}
	if graph.ActiveGenerationID != fixture.req.BaseGenerationID ||
		route.GraphID != fixture.req.GraphID ||
		route.CommitGenerationID != fixture.req.CommitGenerationID ||
		route.DirtyGenerationID != fixture.req.DirtyGenerationID ||
		route.State != RouteActive {
		t.Fatalf("published stack = graph %+v route %+v", graph, route)
	}
	targetCached, _ := checkoutCommitCacheTestGeneration(
		t, fixture.catalog, fixture.req.GraphID, "promotion-replay-target-cache",
	)
	checkoutCommitCacheTestSetPin(
		t, fixture.catalog, fixture.req.CheckoutID, fixture.req.GraphID,
		targetCached, fixture.req.LastSeen+1, 1,
	)
	const foreignGraphID = "promotion-replay-foreign-graph"
	foreignCached, _ := checkoutCommitCacheTestGeneration(
		t, fixture.catalog, foreignGraphID, "promotion-replay-foreign-cache",
	)
	checkoutCommitCacheTestSetPin(
		t, fixture.catalog, fixture.req.CheckoutID, foreignGraphID,
		foreignCached, fixture.req.LastSeen+1, 1,
	)

	if err := fixture.catalog.CommitAuthorizedPromotion(ctx, fixture.req); err != nil {
		t.Fatalf("idempotent CommitAuthorizedPromotion: %v", err)
	}
	replayed, found, err := fixture.catalog.GetCheckoutRoute(ctx, fixture.req.CheckoutID)
	if err != nil || !found || replayed != route {
		t.Fatalf("route after idempotent resume = %+v, found=%v, err=%v; want %+v",
			replayed, found, err, route)
	}
	targetPins := checkoutCommitCacheTestPinIDs(
		checkoutCommitCacheTestPins(t, fixture.catalog, fixture.req.GraphID),
	)
	if targetPins[fixture.req.CommitGenerationID] != 1 || targetPins[targetCached] != 1 {
		t.Fatalf("promotion replay target pins=%v, want current=%d and cached=%d",
			targetPins, fixture.req.CommitGenerationID, targetCached)
	}
	if foreignPins := checkoutCommitCacheTestPins(t, fixture.catalog, foreignGraphID); len(foreignPins) != 0 {
		t.Fatalf("promotion replay retained cross-graph pins: %+v", foreignPins)
	}
	if err := fixture.catalog.CompleteIntentTransition(
		ctx, fixture.req.CheckoutID, fixture.req.TransitionID,
	); err != nil {
		t.Fatalf("CompleteIntentTransition: %v", err)
	}
	owned, err = fixture.catalog.OwnsActiveDedicatedRoute(ctx, fixture.req.GraphID, "repo@idempotent")
	if err != nil || !owned {
		t.Fatalf("committed route ownership = %v, err=%v", owned, err)
	}
	wrongPrefix, err := fixture.catalog.OwnsActiveDedicatedRoute(ctx, fixture.req.GraphID, "repo@other")
	if err != nil || wrongPrefix {
		t.Fatalf("wrong-prefix route ownership = %v, err=%v", wrongPrefix, err)
	}
}

func BenchmarkCommitAuthorizedPromotion(b *testing.B) {
	store, err := Open(filepath.Join(b.TempDir(), "promotion-benchmark.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	catalog := store.Catalog()
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		suffix := fmt.Sprintf("bench-%06d", i)
		req := seedAuthorizedPromotionBenchmark(b, catalog, suffix)
		b.StartTimer()
		if err := catalog.CommitAuthorizedPromotion(ctx, req); err != nil {
			b.Fatalf("CommitAuthorizedPromotion(%s): %v", suffix, err)
		}
	}
}

func seedAuthorizedPromotionBenchmark(
	b *testing.B, catalog *Catalog, suffix string,
) CommitAuthorizedPromotionRequest {
	b.Helper()
	ctx := context.Background()
	familyID := "bench-family"
	if _, found, err := catalog.GetRepositoryFamily(ctx, familyID); err != nil {
		b.Fatal(err)
	} else if !found {
		mustCatalogWrite(b, "upsert benchmark family", catalog.UpsertRepositoryFamily(ctx, RepositoryFamily{
			FamilyID: familyID, CommonDirIdentity: familyID, State: "family_ready",
			CreatedAt: 1, LastSeen: 1,
		}))
	}
	checkoutID := "checkout-" + suffix
	incarnation := "incarnation-" + suffix
	transitionID := "transition-" + suffix
	graphID := "graph-" + suffix
	treeOID := "tree-" + suffix
	mustCatalogWrite(b, "upsert benchmark checkout", catalog.UpsertCheckout(ctx, Checkout{
		CheckoutID: checkoutID, Incarnation: incarnation, FamilyID: familyID,
		RootPath: "/tmp/" + checkoutID, GitDir: "/tmp/" + checkoutID + "/.git",
		AdminName: checkoutID, State: CheckoutStateReady,
		DesiredMode: CheckoutModeAutomatic, EffectiveMode: CheckoutModeAutomatic,
		HeadCommit: "commit-" + suffix, HeadTree: treeOID, LastSeen: 1,
	}))
	mustCatalogWrite(b, "begin benchmark transition", catalog.BeginIntentTransition(ctx, IntentTransition{
		TransitionID: transitionID, CheckoutID: checkoutID, Cause: "promote_checkout",
		PriorDesiredMode: CheckoutModeAutomatic, PriorEffectiveMode: CheckoutModeAutomatic,
		RequestedMode: CheckoutModeDedicated, PriorCheckoutState: CheckoutStateReady,
		State: IntentTransitionPending, CreatedAt: 1, LastProgress: 1,
	}))
	mustCatalogWrite(b, "upsert benchmark graph", catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
		GraphID: graphID, OwnerCheckoutID: checkoutID, RepoPrefix: "repo@" + suffix,
		FamilyID: familyID, State: "graph_ready",
	}))
	baseID := createReadyPromotionGeneration(b, catalog, ViewGeneration{
		OwnerKind: "dedicated_graph", GraphID: graphID, CheckoutID: checkoutID,
		GenerationKind: "base", TreeOID: treeOID, CreatedAt: 1,
	})
	commitID := createReadyPromotionGeneration(b, catalog, ViewGeneration{
		OwnerKind: "checkout_commit", GraphID: graphID, CheckoutID: checkoutID,
		GenerationKind: "commit", BaseGenerationID: baseID, TreeOID: treeOID, CreatedAt: 1,
	})
	dirtyID := createReadyPromotionGeneration(b, catalog, ViewGeneration{
		OwnerKind: "checkout_dirty", GraphID: graphID, CheckoutID: checkoutID,
		GenerationKind: "dirty", BaseGenerationID: commitID, TreeOID: treeOID, CreatedAt: 1,
	})
	return CommitAuthorizedPromotionRequest{
		CheckoutID: checkoutID, Incarnation: incarnation, FamilyID: familyID,
		TransitionID: transitionID, RequiredCause: "promote_checkout",
		GraphID: graphID, RequiredGraphState: "graph_ready",
		BaseGenerationID: baseID, BaseTreeOID: treeOID,
		CommitGenerationID: commitID, DirtyGenerationID: dirtyID,
		State: CheckoutStateReady, LastSeen: 2,
	}
}
