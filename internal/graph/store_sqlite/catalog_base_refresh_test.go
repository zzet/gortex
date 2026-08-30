package store_sqlite

import (
	"context"
	"errors"
	"testing"
	"time"
)

type dedicatedBaseRefreshFixture struct {
	t              testing.TB
	store          *Store
	catalog        *Catalog
	ctx            context.Context
	now            int64
	familyID       string
	graphID        string
	ownerID        string
	depID          string
	oldBase        int64
	newBase        int64
	ownerOld       CheckoutRoute
	ownerNewCommit int64
	ownerNewDirty  int64
	depOld         CheckoutRoute
}

func newDedicatedBaseRefreshFixture(t testing.TB) *dedicatedBaseRefreshFixture {
	t.Helper()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f := &dedicatedBaseRefreshFixture{
		t: t, store: store, catalog: store.Catalog(), ctx: context.Background(),
		now: time.Now().Unix(), familyID: "refresh-family", graphID: "refresh-graph",
		ownerID: "refresh-owner", depID: "refresh-dependent",
	}
	mustRefreshWrite(t, "family", f.catalog.UpsertRepositoryFamily(f.ctx, RepositoryFamily{
		FamilyID: f.familyID, CommonDirIdentity: "/refresh/common", State: "ready",
		CreatedAt: f.now, LastSeen: f.now,
	}))
	f.addCheckout(f.ownerID, CheckoutModeDedicated)
	f.addCheckout(f.depID, CheckoutModeAutomatic)
	mustRefreshWrite(t, "graph shell", f.catalog.UpsertDedicatedGraph(f.ctx, DedicatedGraph{
		GraphID: f.graphID, OwnerCheckoutID: f.ownerID, RepoPrefix: "refresh",
		FamilyID: f.familyID, IsPrimaryBase: true, State: DedicatedGraphStateReady,
	}))
	f.oldBase = f.generation(ViewGeneration{
		OwnerKind: "dedicated_base", GraphID: f.graphID, LayerID: f.graphID + ":base",
		CheckoutID: f.ownerID, GenerationKind: "dedicated_base", TreeOID: "base-tree",
		ConfigHash: "old-config", ExtractorVersions: "old-extractors", ResolverVersion: "old-resolver",
	})
	f.newBase = f.generation(ViewGeneration{
		OwnerKind: "dedicated_base", GraphID: f.graphID, LayerID: f.graphID + ":base",
		CheckoutID: f.ownerID, GenerationKind: "dedicated_base", TreeOID: "base-tree",
		ConfigHash: "new-config", ExtractorVersions: "new-extractors", ResolverVersion: "new-resolver",
	})
	graph := DedicatedGraph{
		GraphID: f.graphID, OwnerCheckoutID: f.ownerID, RepoPrefix: "refresh",
		FamilyID: f.familyID, IsPrimaryBase: true, ActiveGenerationID: f.oldBase, State: DedicatedGraphStateReady,
	}
	mustRefreshWrite(t, "publish old base", f.catalog.UpsertDedicatedGraph(f.ctx, graph))

	ownerOldCommit := f.layer(f.oldBase, f.ownerID, "owner-old-commit", string(RouteSlotCommit), "head-tree")
	ownerOldDirty := f.layer(ownerOldCommit, f.ownerID, "owner-old-dirty", string(RouteSlotDirty), "head-tree")
	f.ownerOld = CheckoutRoute{
		CheckoutID: f.ownerID, GraphID: f.graphID,
		CommitGenerationID: ownerOldCommit, DirtyGenerationID: ownerOldDirty,
		RouteEpoch: 7, State: RouteActive,
	}
	mustRefreshWrite(t, "owner old route", f.catalog.UpsertCheckoutRoute(f.ctx, f.ownerOld))
	f.ownerNewCommit = f.layer(f.newBase, f.ownerID, "commit-"+f.ownerID, string(RouteSlotCommit), "head-tree")
	f.ownerNewDirty = f.layer(f.ownerNewCommit, f.ownerID, "dirty-"+f.ownerID, string(RouteSlotDirty), "head-tree")

	depOldCommit := f.layer(f.oldBase, f.depID, "dep-old-commit", string(RouteSlotCommit), "dep-tree")
	depOldDirty := f.layer(depOldCommit, f.depID, "dep-old-dirty", string(RouteSlotDirty), "dep-tree")
	f.depOld = CheckoutRoute{
		CheckoutID: f.depID, GraphID: f.graphID,
		CommitGenerationID: depOldCommit, DirtyGenerationID: depOldDirty,
		RouteEpoch: 11, State: RouteActive,
	}
	mustRefreshWrite(t, "dependent old route", f.catalog.UpsertCheckoutRoute(f.ctx, f.depOld))
	return f
}

func (f *dedicatedBaseRefreshFixture) addCheckout(id string, mode CheckoutMode) {
	f.t.Helper()
	mustRefreshWrite(f.t, "checkout", f.catalog.AllocateCheckout(f.ctx, Checkout{
		CheckoutID: id, Incarnation: "inc-" + id, FamilyID: f.familyID,
		RootPath: "/refresh/" + id, GitDir: "/refresh/git/" + id, AdminName: id,
		State: CheckoutStateReady, DesiredMode: mode, EffectiveMode: mode,
		HeadRef: "refs/heads/main", HeadTree: "head-tree",
		LastAccessible: f.now, LastSeen: f.now,
	}))
}

func (f *dedicatedBaseRefreshFixture) generation(row ViewGeneration) int64 {
	f.t.Helper()
	row.State = ViewGenerationReady
	row.CreatedAt, row.PublishedAt = f.now, f.now
	id, err := f.catalog.CreateViewGeneration(f.ctx, row)
	if err != nil {
		f.t.Fatalf("create generation: %v", err)
	}
	return id
}

func (f *dedicatedBaseRefreshFixture) layer(
	base int64, checkoutID, layerID, generationKind, tree string,
) int64 {
	return f.generation(ViewGeneration{
		OwnerKind: checkoutGenerationOwnerKind, GraphID: f.graphID, LayerID: layerID,
		CheckoutID: checkoutID, GenerationKind: generationKind, BaseGenerationID: base,
		TreeOID: tree, ConfigHash: "new-config", ExtractorVersions: "new-extractors",
		ResolverVersion: "new-resolver",
	})
}

func (f *dedicatedBaseRefreshFixture) request() CommitDedicatedBaseRefreshRequest {
	return CommitDedicatedBaseRefreshRequest{
		CheckoutID: f.ownerID, Incarnation: "inc-" + f.ownerID,
		FamilyID: f.familyID, GraphID: f.graphID, RequiredGraphState: DedicatedGraphStateReady,
		ExpectedBaseGenerationID: f.oldBase, NewBaseGenerationID: f.newBase,
		BaseTreeOID: "base-tree", ConfigHash: "new-config",
		ExtractorVersions: "new-extractors", ResolverVersion: "new-resolver",
		CommitGenerationID: f.ownerNewCommit, DirtyGenerationID: f.ownerNewDirty,
		CommitTreeOID: "head-tree",
		RouteExists:   true, ExpectedRouteEpoch: f.ownerOld.RouteEpoch, LastSeen: f.now + 1,
	}
}

func (f *dedicatedBaseRefreshFixture) replacementStack(
	mutateCommit, mutateDirty func(*ViewGeneration),
) (int64, int64) {
	commit := ViewGeneration{
		OwnerKind: checkoutGenerationOwnerKind, GraphID: f.graphID, LayerID: "commit-" + f.ownerID,
		CheckoutID: f.ownerID, GenerationKind: string(RouteSlotCommit), BaseGenerationID: f.newBase,
		TreeOID: "head-tree", ConfigHash: "new-config", ExtractorVersions: "new-extractors",
		ResolverVersion: "new-resolver",
	}
	if mutateCommit != nil {
		mutateCommit(&commit)
	}
	commitID := f.generation(commit)
	dirty := ViewGeneration{
		OwnerKind: checkoutGenerationOwnerKind, GraphID: f.graphID, LayerID: "dirty-" + f.ownerID,
		CheckoutID: f.ownerID, GenerationKind: string(RouteSlotDirty), BaseGenerationID: commitID,
		TreeOID: "head-tree", ConfigHash: "new-config", ExtractorVersions: "new-extractors",
		ResolverVersion: "new-resolver",
	}
	if mutateDirty != nil {
		mutateDirty(&dirty)
	}
	return commitID, f.generation(dirty)
}

func mustRefreshWrite(t testing.TB, action string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
}

func TestCommitDedicatedBaseRefreshPublishesOwnerAndInvalidatesDependentsAtomically(t *testing.T) {
	f := newDedicatedBaseRefreshFixture(t)
	if err := f.catalog.BeginDedicatedBaseRefresh(f.ctx, f.graphID, f.oldBase); err != nil {
		t.Fatalf("begin refresh: %v", err)
	}
	req := f.request()
	req.RequiredGraphState = DedicatedGraphStateRefreshing
	req.ExpectedRouteEpoch++
	result, err := f.catalog.CommitDedicatedBaseRefresh(f.ctx, req)
	if err != nil {
		t.Fatalf("commit refresh: %v", err)
	}
	graph, found, err := f.catalog.GetDedicatedGraph(f.ctx, f.graphID)
	if err != nil || !found || graph.ActiveGenerationID != f.newBase ||
		graph.State != DedicatedGraphStateReady {
		t.Fatalf("refreshed graph: found=%v graph=%+v err=%v", found, graph, err)
	}
	owner, found, err := f.catalog.GetCheckoutRoute(f.ctx, f.ownerID)
	if err != nil || !found || owner.CommitGenerationID != f.ownerNewCommit ||
		owner.DirtyGenerationID != f.ownerNewDirty || owner.RouteEpoch != f.ownerOld.RouteEpoch+2 ||
		owner.State != RouteActive {
		t.Fatalf("owner route: found=%v route=%+v err=%v", found, owner, err)
	}
	dependent, found, err := f.catalog.GetCheckoutRoute(f.ctx, f.depID)
	if err != nil || !found || dependent.CommitGenerationID != 0 || dependent.DirtyGenerationID != 0 ||
		dependent.RouteEpoch != f.depOld.RouteEpoch+2 || dependent.State != RoutePending {
		t.Fatalf("dependent route: found=%v route=%+v err=%v", found, dependent, err)
	}
	if len(result.InvalidatedCheckoutIDs) != 1 || result.InvalidatedCheckoutIDs[0] != f.depID {
		t.Fatalf("invalidated checkouts = %v", result.InvalidatedCheckoutIDs)
	}
	for _, want := range []int64{
		f.oldBase, f.ownerOld.CommitGenerationID, f.ownerOld.DirtyGenerationID,
		f.depOld.CommitGenerationID, f.depOld.DirtyGenerationID,
	} {
		found := false
		for _, got := range result.RetiredGenerationIDs {
			found = found || got == want
		}
		if !found {
			t.Fatalf("retirement set %v does not contain %d", result.RetiredGenerationIDs, want)
		}
	}
}

func TestBeginDedicatedBaseRefreshMarksFallbackWithoutDroppingPointers(t *testing.T) {
	f := newDedicatedBaseRefreshFixture(t)
	key := readyCacheTestKey(f.graphID, f.oldBase)
	view, build := seedReadyCacheRefView(t, f.catalog, key, "base-refresh-admission")
	mustRefreshWrite(t, "restore refresh graph after ref fixture seed",
		f.catalog.UpsertDedicatedGraph(f.ctx, DedicatedGraph{
			GraphID: f.graphID, OwnerCheckoutID: f.ownerID, RepoPrefix: "refresh",
			FamilyID: f.familyID, IsPrimaryBase: true,
			ActiveGenerationID: f.oldBase, State: DedicatedGraphStateReady,
		}))
	generationID := createReadyCacheGeneration(t, f.catalog, key,
		"ref_view", "ref-refresh-admission", "commit:ref-refresh-admission", "")
	claim := claimReadyCacheSourceGeneration(t, f.catalog, key, generationID)
	req := bindReadyCacheRefViewRequest(view, build, key, claim)
	req.RequireActiveGraphBase = true
	if err := f.catalog.BindReadyGenerationLeaseToRefView(f.ctx, req); err != nil {
		t.Fatalf("publish ready ref view: %v", err)
	}
	viewBefore, _, _ := f.catalog.GetRefView(f.ctx, view.RefViewID)

	if err := f.catalog.BeginDedicatedBaseRefresh(f.ctx, f.graphID, f.oldBase); err != nil {
		t.Fatalf("begin refresh: %v", err)
	}
	graph, _, _ := f.catalog.GetDedicatedGraph(f.ctx, f.graphID)
	if graph.State != DedicatedGraphStateRefreshing || graph.ActiveGenerationID != f.oldBase {
		t.Fatalf("admitted graph = %+v, want refreshing on base %d", graph, f.oldBase)
	}
	for _, before := range []CheckoutRoute{f.ownerOld, f.depOld} {
		after, _, _ := f.catalog.GetCheckoutRoute(f.ctx, before.CheckoutID)
		if after.State != RoutePending || after.CommitGenerationID != before.CommitGenerationID ||
			after.DirtyGenerationID != before.DirtyGenerationID || after.RouteEpoch != before.RouteEpoch+1 {
			t.Fatalf("admission did not preserve fallback route: before=%+v after=%+v", before, after)
		}
	}
	viewAfter, _, _ := f.catalog.GetRefView(f.ctx, view.RefViewID)
	if viewAfter.State != RefViewBuilding || viewAfter.ExactView ||
		viewAfter.ActiveGenerationID != viewBefore.ActiveGenerationID ||
		viewAfter.RouteEpoch != viewBefore.RouteEpoch+1 {
		t.Fatalf("admission did not label ref fallback: before=%+v after=%+v", viewBefore, viewAfter)
	}

	// Re-admission after a failed/restarted build is idempotent: no second ref
	// epoch bump and no pointer churn.
	if err := f.catalog.BeginDedicatedBaseRefresh(f.ctx, f.graphID, f.oldBase); err != nil {
		t.Fatalf("re-admit refresh: %v", err)
	}
	viewAgain, _, _ := f.catalog.GetRefView(f.ctx, view.RefViewID)
	if viewAgain != viewAfter {
		t.Fatalf("idempotent admission changed ref view: once=%+v twice=%+v", viewAfter, viewAgain)
	}
}

func TestBeginDedicatedBaseRefreshFencesCapturedOldBaseRoutePublication(t *testing.T) {
	f := newDedicatedBaseRefreshFixture(t)
	captured := f.depOld
	if err := f.catalog.BeginDedicatedBaseRefresh(f.ctx, f.graphID, f.oldBase); err != nil {
		t.Fatalf("begin refresh: %v", err)
	}
	err := f.catalog.FlipCheckoutRoute(f.ctx, FlipCheckoutRouteRequest{
		CheckoutID: captured.CheckoutID, GraphID: captured.GraphID,
		CommitGenerationID: captured.CommitGenerationID,
		DirtyGenerationID:  captured.DirtyGenerationID,
		ExpectedRouteEpoch: captured.RouteEpoch, State: RouteActive,
		RequireActiveGraphBase: true, ExpectedBaseGenerationID: f.oldBase,
	})
	if !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("old-base route publication error = %v, want ErrCatalogStaleGuard", err)
	}
	after, _, _ := f.catalog.GetCheckoutRoute(f.ctx, captured.CheckoutID)
	if after.State != RoutePending || after.RouteEpoch != captured.RouteEpoch+1 ||
		after.CommitGenerationID != captured.CommitGenerationID ||
		after.DirtyGenerationID != captured.DirtyGenerationID {
		t.Fatalf("stale publication changed fallback route: before=%+v after=%+v", captured, after)
	}

	// Even a caller that somehow learns the admission epoch cannot reactivate
	// its old-base payload while the graph is explicitly refreshing.
	err = f.catalog.FlipCheckoutRoute(f.ctx, FlipCheckoutRouteRequest{
		CheckoutID: captured.CheckoutID, GraphID: captured.GraphID,
		CommitGenerationID: captured.CommitGenerationID,
		DirtyGenerationID:  captured.DirtyGenerationID,
		ExpectedRouteEpoch: after.RouteEpoch, State: RouteActive,
		RequireActiveGraphBase: true, ExpectedBaseGenerationID: f.oldBase,
	})
	if !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("refreshing-graph route publication error = %v, want ErrCatalogStaleGuard", err)
	}
}

func TestBeginDedicatedBaseRefreshFencesAlreadyPendingCheckoutRoute(t *testing.T) {
	f := newDedicatedBaseRefreshFixture(t)
	pending := f.depOld
	pending.State = RoutePending
	mustRefreshWrite(t, "seed pending dependent route", f.catalog.UpsertCheckoutRoute(f.ctx, pending))

	if err := f.catalog.BeginDedicatedBaseRefresh(f.ctx, f.graphID, f.oldBase); err != nil {
		t.Fatalf("begin refresh: %v", err)
	}
	afterAdmission, found, err := f.catalog.GetCheckoutRoute(f.ctx, pending.CheckoutID)
	if err != nil || !found {
		t.Fatalf("pending route after admission: found=%v err=%v", found, err)
	}
	if afterAdmission.RouteEpoch != pending.RouteEpoch+1 || afterAdmission.State != RoutePending ||
		afterAdmission.CommitGenerationID != pending.CommitGenerationID ||
		afterAdmission.DirtyGenerationID != pending.DirtyGenerationID {
		t.Fatalf("pending route was not fenced in place: before=%+v after=%+v", pending, afterAdmission)
	}

	fullErr := f.catalog.FlipCheckoutRoute(f.ctx, FlipCheckoutRouteRequest{
		CheckoutID: pending.CheckoutID, GraphID: pending.GraphID,
		CommitGenerationID: pending.CommitGenerationID,
		DirtyGenerationID:  pending.DirtyGenerationID,
		ExpectedRouteEpoch: pending.RouteEpoch, State: RouteActive,
		RequireActiveGraphBase: true, ExpectedBaseGenerationID: f.oldBase,
	})
	if !errors.Is(fullErr, ErrCatalogStaleGuard) {
		t.Fatalf("captured pending full publication error = %v, want ErrCatalogStaleGuard", fullErr)
	}
	slotErr := f.catalog.FlipCheckoutRouteSlot(f.ctx, FlipCheckoutRouteSlotRequest{
		CheckoutID: pending.CheckoutID, Slot: RouteSlotDirty,
		GenerationID:       pending.DirtyGenerationID,
		ExpectedRouteEpoch: pending.RouteEpoch, State: RouteActive,
		RequireActiveGraphBase: true, ExpectedBaseGenerationID: f.oldBase,
	})
	if !errors.Is(slotErr, ErrCatalogStaleGuard) {
		t.Fatalf("captured pending slot publication error = %v, want ErrCatalogStaleGuard", slotErr)
	}
}

func TestBeginDedicatedBaseRefreshFencesPreviouslyAbsentCheckoutRoute(t *testing.T) {
	f := newDedicatedBaseRefreshFixture(t)
	const checkoutID = "refresh-previously-unrouted"
	f.addCheckout(checkoutID, CheckoutModeAutomatic)
	if _, found, err := f.catalog.GetCheckoutRoute(f.ctx, checkoutID); err != nil || found {
		t.Fatalf("route before admission: found=%v err=%v", found, err)
	}

	if err := f.catalog.BeginDedicatedBaseRefresh(f.ctx, f.graphID, f.oldBase); err != nil {
		t.Fatalf("begin refresh: %v", err)
	}
	err := f.catalog.InstallCheckoutRouteForBase(f.ctx, InstallCheckoutRouteForBaseRequest{
		CheckoutID: checkoutID, GraphID: f.graphID, ExpectedBaseGenerationID: f.oldBase,
	})
	if !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("old-base absent-route install error = %v, want ErrCatalogStaleGuard", err)
	}
	if route, found, err := f.catalog.GetCheckoutRoute(f.ctx, checkoutID); err != nil || found {
		t.Fatalf("stale absent-route publication leaked route: found=%v route=%+v err=%v", found, route, err)
	}
}

func BenchmarkCheckoutRouteBaseEpochFence(b *testing.B) {
	f := newDedicatedBaseRefreshFixture(b)
	route := f.depOld
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := f.catalog.FlipCheckoutRouteSlot(f.ctx, FlipCheckoutRouteSlotRequest{
			CheckoutID: route.CheckoutID, Slot: RouteSlotDirty,
			GenerationID:       route.DirtyGenerationID,
			ExpectedRouteEpoch: route.RouteEpoch, State: RouteActive,
			RequireActiveGraphBase: true, ExpectedBaseGenerationID: f.oldBase,
		}); err != nil {
			b.Fatalf("guarded route publication: %v", err)
		}
		route.RouteEpoch++
	}
}

func TestCommitDedicatedBaseRefreshInvalidatesReadyAndInFlightRefViews(t *testing.T) {
	f := newDedicatedBaseRefreshFixture(t)
	restoreGraph := func() {
		t.Helper()
		mustRefreshWrite(t, "restore refresh graph after ref fixture seed",
			f.catalog.UpsertDedicatedGraph(f.ctx, DedicatedGraph{
				GraphID: f.graphID, OwnerCheckoutID: f.ownerID, RepoPrefix: "refresh",
				FamilyID: f.familyID, IsPrimaryBase: true,
				ActiveGenerationID: f.oldBase, State: DedicatedGraphStateReady,
			}))
	}
	readyKey := readyCacheTestKey(f.graphID, f.oldBase)
	readyView, readyBuild := seedReadyCacheRefView(t, f.catalog, readyKey, "base-refresh-ready")
	restoreGraph()
	readyGeneration := createReadyCacheGeneration(t, f.catalog, readyKey,
		"ref_view", "ref-refresh-ready", "commit:ref-refresh-ready", "")
	readyClaim := claimReadyCacheSourceGeneration(t, f.catalog, readyKey, readyGeneration)
	readyReq := bindReadyCacheRefViewRequest(readyView, readyBuild, readyKey, readyClaim)
	readyReq.RequireActiveGraphBase = true
	graphBefore, found, err := f.catalog.GetDedicatedGraph(f.ctx, f.graphID)
	if err != nil || !found || graphBefore.ActiveGenerationID != f.oldBase ||
		graphBefore.State != DedicatedGraphStateReady {
		t.Fatalf("dedicated graph before ref publication: found=%v graph=%+v err=%v", found, graphBefore, err)
	}
	if err := f.catalog.BindReadyGenerationLeaseToRefView(f.ctx, readyReq); err != nil {
		t.Fatalf("publish ready ref view: %v", err)
	}
	readyBefore, found, err := f.catalog.GetRefView(f.ctx, readyView.RefViewID)
	if err != nil || !found {
		t.Fatalf("ready ref before refresh: found=%v err=%v", found, err)
	}

	inFlightKey := readyCacheTestKey(f.graphID, f.oldBase)
	inFlightKey.TreeOID = "ref-in-flight-tree"
	inFlightView, inFlightBuild := seedReadyCacheRefView(t, f.catalog, inFlightKey, "base-refresh-flight")
	restoreGraph()
	inFlightGeneration := createReadyCacheGeneration(t, f.catalog, inFlightKey,
		"ref_view", "ref-refresh-flight", "commit:ref-refresh-flight", "")
	inFlightClaim := claimReadyCacheSourceGeneration(t, f.catalog, inFlightKey, inFlightGeneration)

	if err := f.catalog.BeginDedicatedBaseRefresh(f.ctx, f.graphID, f.oldBase); err != nil {
		t.Fatalf("begin refresh: %v", err)
	}
	refreshReq := f.request()
	refreshReq.RequiredGraphState = DedicatedGraphStateRefreshing
	refreshReq.ExpectedRouteEpoch++
	result, err := f.catalog.CommitDedicatedBaseRefresh(f.ctx, refreshReq)
	if err != nil {
		t.Fatalf("commit refresh: %v", err)
	}
	readyAfter, found, err := f.catalog.GetRefView(f.ctx, readyView.RefViewID)
	if err != nil || !found {
		t.Fatalf("ready ref after refresh: found=%v err=%v", found, err)
	}
	if readyAfter.ActiveGenerationID != 0 || readyAfter.ExactView ||
		readyAfter.State != RefViewBuilding || readyAfter.RouteEpoch != readyBefore.RouteEpoch+2 {
		t.Fatalf("ready ref remained serveable after base refresh: before=%+v after=%+v", readyBefore, readyAfter)
	}
	attempt, found, err := f.catalog.GetRefViewBuild(f.ctx, inFlightBuild.BuildID)
	if err != nil || !found {
		t.Fatalf("in-flight build after refresh: found=%v err=%v", found, err)
	}
	if attempt.State != ViewGenerationSuperseded {
		t.Fatalf("in-flight build state = %q, want %q", attempt.State, ViewGenerationSuperseded)
	}
	inFlightReq := bindReadyCacheRefViewRequest(inFlightView, inFlightBuild, inFlightKey, inFlightClaim)
	inFlightReq.RequireActiveGraphBase = true
	if err := f.catalog.BindReadyGenerationLeaseToRefView(f.ctx, inFlightReq); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("old-base in-flight publication error = %v, want ErrCatalogStaleGuard", err)
	}
	for _, want := range []string{readyView.RefViewID, inFlightView.RefViewID} {
		found := false
		for _, got := range result.InvalidatedRefViewIDs {
			found = found || got == want
		}
		if !found {
			t.Fatalf("invalidated ref views %v do not contain %s", result.InvalidatedRefViewIDs, want)
		}
	}
	retiredReady, retiredInFlight := false, false
	for _, generationID := range result.RetiredGenerationIDs {
		retiredReady = retiredReady || generationID == readyGeneration
		retiredInFlight = retiredInFlight || generationID == inFlightGeneration
	}
	if !retiredReady || !retiredInFlight {
		t.Fatalf("retirement set %v does not contain ready=%d and in-flight=%d ref generations",
			result.RetiredGenerationIDs, readyGeneration, inFlightGeneration)
	}
}

func TestCommitDedicatedBaseRefreshRollsBackEveryPointerOnStaleGuard(t *testing.T) {
	f := newDedicatedBaseRefreshFixture(t)
	req := f.request()
	req.ConfigHash = "wrong-config"
	_, err := f.catalog.CommitDedicatedBaseRefresh(f.ctx, req)
	if !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("error = %v, want ErrCatalogStaleGuard", err)
	}
	graph, _, _ := f.catalog.GetDedicatedGraph(f.ctx, f.graphID)
	owner, _, _ := f.catalog.GetCheckoutRoute(f.ctx, f.ownerID)
	dependent, _, _ := f.catalog.GetCheckoutRoute(f.ctx, f.depID)
	if graph.ActiveGenerationID != f.oldBase || owner != f.ownerOld || dependent != f.depOld {
		t.Fatalf("stale refresh changed state: graph=%+v owner=%+v dependent=%+v", graph, owner, dependent)
	}
}

func TestCommitDedicatedBaseRefreshRejectsNoncanonicalOwnerStack(t *testing.T) {
	tests := []struct {
		name         string
		mutateCommit func(*ViewGeneration)
		mutateDirty  func(*ViewGeneration)
	}{
		{
			name: "wrong kind",
			mutateDirty: func(row *ViewGeneration) {
				row.GenerationKind = string(RouteSlotCommit)
			},
		},
		{
			name: "wrong checkout",
			mutateCommit: func(row *ViewGeneration) {
				row.CheckoutID = "another-checkout"
			},
		},
		{
			name: "wrong tree",
			mutateCommit: func(row *ViewGeneration) {
				row.TreeOID = "another-tree"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDedicatedBaseRefreshFixture(t)
			commitID, dirtyID := f.replacementStack(tt.mutateCommit, tt.mutateDirty)
			req := f.request()
			req.CommitGenerationID, req.DirtyGenerationID = commitID, dirtyID
			_, err := f.catalog.CommitDedicatedBaseRefresh(f.ctx, req)
			if !errors.Is(err, ErrCatalogStaleGuard) {
				t.Fatalf("error = %v, want ErrCatalogStaleGuard", err)
			}
			graph, _, _ := f.catalog.GetDedicatedGraph(f.ctx, f.graphID)
			owner, _, _ := f.catalog.GetCheckoutRoute(f.ctx, f.ownerID)
			dependent, _, _ := f.catalog.GetCheckoutRoute(f.ctx, f.depID)
			if graph.ActiveGenerationID != f.oldBase || owner != f.ownerOld || dependent != f.depOld {
				t.Fatalf("invalid replacement changed state: graph=%+v owner=%+v dependent=%+v",
					graph, owner, dependent)
			}
		})
	}
}

func TestCommitDedicatedBaseRefreshRequiresCompletePipelineIdentity(t *testing.T) {
	for _, field := range []string{"config", "extractors", "resolver"} {
		t.Run(field, func(t *testing.T) {
			f := newDedicatedBaseRefreshFixture(t)
			req := f.request()
			switch field {
			case "config":
				req.ConfigHash = ""
			case "extractors":
				req.ExtractorVersions = ""
			case "resolver":
				req.ResolverVersion = ""
			}
			if _, err := f.catalog.CommitDedicatedBaseRefresh(f.ctx, req); err == nil {
				t.Fatal("empty pipeline identity was accepted")
			}
			graph, _, _ := f.catalog.GetDedicatedGraph(f.ctx, f.graphID)
			if graph.ActiveGenerationID != f.oldBase {
				t.Fatalf("invalid request moved base to %d", graph.ActiveGenerationID)
			}
		})
	}
}

func TestCommitCheckoutStackRejectsGenerationFromPreviousBaseEpoch(t *testing.T) {
	f := newDedicatedBaseRefreshFixture(t)
	if _, err := f.catalog.CommitDedicatedBaseRefresh(f.ctx, f.request()); err != nil {
		t.Fatalf("commit refresh: %v", err)
	}
	route, found, err := f.catalog.GetCheckoutRoute(f.ctx, f.depID)
	if err != nil || !found {
		t.Fatalf("dependent route: found=%v err=%v", found, err)
	}
	staleCommit := f.layer(f.oldBase, f.depID, "stale-commit", string(RouteSlotCommit), "dep-tree")
	staleDirty := f.layer(staleCommit, f.depID, "stale-dirty", string(RouteSlotDirty), "dep-tree")
	err = f.catalog.CommitCheckoutStack(f.ctx, CommitCheckoutStackRequest{
		CheckoutID: f.depID, GraphID: f.graphID, ExpectedBaseGenerationID: f.oldBase,
		CommitGenerationID: staleCommit, DirtyGenerationID: staleDirty,
		RouteExists: true, ExpectedRouteEpoch: route.RouteEpoch, State: RouteActive,
	})
	if !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("error = %v, want ErrCatalogStaleGuard", err)
	}
	after, _, _ := f.catalog.GetCheckoutRoute(f.ctx, f.depID)
	if after != route {
		t.Fatalf("stale stack changed route: before=%+v after=%+v", route, after)
	}
}

func TestReadyCheckoutPublicationRejectsGraphLeavingReadyAtSameBaseEpoch(t *testing.T) {
	f := newDedicatedBaseRefreshFixture(t)
	key := readyCacheTestKey(f.graphID, f.oldBase)
	generationID := createReadyCacheGeneration(t, f.catalog, key,
		"checkout-layer", f.depID, "commit:graph-state-race", "")
	claim, found, err := f.catalog.ClaimReadyGeneration(f.ctx, ClaimReadyGenerationRequest{
		Key: key, CandidateGenerationID: generationID,
	})
	if err != nil || !found {
		t.Fatalf("claim ready generation: found=%v err=%v", found, err)
	}

	graph, found, err := f.catalog.GetDedicatedGraph(f.ctx, f.graphID)
	if err != nil || !found {
		t.Fatalf("get dedicated graph: found=%v err=%v", found, err)
	}
	graph.State = "graph_retiring"
	mustRefreshWrite(t, "move graph out of ready", f.catalog.UpsertDedicatedGraph(f.ctx, graph))

	err = f.catalog.BindReadyGenerationLeaseToCheckout(f.ctx,
		BindReadyGenerationLeaseToCheckoutRequest{
			Key: key, LeaseToken: claim.LeaseToken, GenerationID: claim.WinnerGenerationID,
			CheckoutID: f.depID, ExpectedRouteEpoch: f.depOld.RouteEpoch,
			State: RouteActive, RequireActiveGraphBase: true,
		})
	if !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("error = %v, want ErrCatalogStaleGuard", err)
	}
	after, _, _ := f.catalog.GetCheckoutRoute(f.ctx, f.depID)
	if after != f.depOld {
		t.Fatalf("graph-state race changed checkout route: before=%+v after=%+v", f.depOld, after)
	}
}

func TestReadyRefPublicationRejectsGraphLeavingReadyAtSameBaseEpoch(t *testing.T) {
	f := newDedicatedBaseRefreshFixture(t)
	key := readyCacheTestKey(f.graphID, f.oldBase)
	view, build := seedReadyCacheRefView(t, f.catalog, key, "graph-state-race")
	generationID := createReadyCacheGeneration(t, f.catalog, key,
		"checkout-layer", "ref-state-race", "commit:graph-state-race", "")
	claim := claimReadyCacheSourceGeneration(t, f.catalog, key, generationID)

	graph, found, err := f.catalog.GetDedicatedGraph(f.ctx, f.graphID)
	if err != nil || !found {
		t.Fatalf("get dedicated graph: found=%v err=%v", found, err)
	}
	graph.State = "graph_retiring"
	mustRefreshWrite(t, "move graph out of ready", f.catalog.UpsertDedicatedGraph(f.ctx, graph))

	req := bindReadyCacheRefViewRequest(view, build, key, claim)
	req.RequireActiveGraphBase = true
	err = f.catalog.BindReadyGenerationLeaseToRefView(f.ctx, req)
	if !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("error = %v, want ErrCatalogStaleGuard", err)
	}
	after, _, _ := f.catalog.GetRefView(f.ctx, view.RefViewID)
	if after.ActiveGenerationID != view.ActiveGenerationID || after.RouteEpoch != view.RouteEpoch {
		t.Fatalf("graph-state race changed ref route: before=%+v after=%+v", view, after)
	}
}
