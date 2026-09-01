package graphview

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The control plane every routed test addresses.
const (
	testGraphID    = "graph-1"
	testCheckoutID = "wt-1"
	testFamilyID   = "fam-1"
)

// seedStackControlPlane installs the family, checkout and dedicated
// graph a route hangs off. The route itself is left to each test, since
// what a route points at is what most of them are about.
func seedStackControlPlane(t testing.TB, store *store_sqlite.Store, activeGenerations ...int64) {
	t.Helper()
	if len(activeGenerations) > 1 {
		t.Fatalf("seedStackControlPlane: got %d active generations, want at most one", len(activeGenerations))
	}
	activeGeneration := int64(0)
	if len(activeGenerations) == 1 {
		activeGeneration = activeGenerations[0]
	}
	ctx := context.Background()
	catalog := store.Catalog()
	if err := catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          testFamilyID,
		CommonDirIdentity: "identity/" + testFamilyID,
		DisplayRemote:     "git@example.invalid:" + testFamilyID + ".git",
		State:             "family_ready",
		CreatedAt:         100,
		LastSeen:          100,
	}); err != nil {
		t.Fatalf("UpsertRepositoryFamily: %v", err)
	}
	if err := catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:    testCheckoutID,
		Incarnation:   "inc-1",
		FamilyID:      testFamilyID,
		RootPath:      "/tmp/" + testCheckoutID,
		GitDir:        "/tmp/" + testCheckoutID + "/.git",
		AdminName:     testCheckoutID,
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated,
		HeadRef:       "refs/heads/main",
		HeadCommit:    "c0ffee",
		HeadTree:      "7ee7",
		LastSeen:      101,
	}); err != nil {
		t.Fatalf("UpsertCheckout: %v", err)
	}
	if err := catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID:            testGraphID,
		OwnerCheckoutID:    testCheckoutID,
		RepoPrefix:         stackRepo,
		FamilyID:           testFamilyID,
		ActiveGenerationID: activeGeneration,
		State:              "graph_ready",
	}); err != nil {
		t.Fatalf("UpsertDedicatedGraph: %v", err)
	}
}

// routeStack points the checkout's route at a pair of generation slots.
// A slot of 0 is an unset pointer, which is how a checkout with no
// working-tree generation is routed.
func routeStack(t testing.TB, store *store_sqlite.Store, commit, dirty int64, state store_sqlite.RouteState) {
	t.Helper()
	ctx := context.Background()
	catalog := store.Catalog()
	route, found, err := catalog.GetCheckoutRoute(ctx, testCheckoutID)
	if err != nil {
		t.Fatalf("GetCheckoutRoute: %v", err)
	}
	if found {
		err = catalog.FlipCheckoutRoute(ctx, store_sqlite.FlipCheckoutRouteRequest{
			CheckoutID:         testCheckoutID,
			ExpectedRouteEpoch: route.RouteEpoch,
			GraphID:            testGraphID,
			CommitGenerationID: commit,
			DirtyGenerationID:  dirty,
			State:              state,
		})
	} else {
		err = catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
			CheckoutID:         testCheckoutID,
			GraphID:            testGraphID,
			CommitGenerationID: commit,
			DirtyGenerationID:  dirty,
			State:              state,
		})
	}
	if err != nil {
		t.Fatalf("route stack: %v", err)
	}
}

func publishStackRootMove(
	t testing.TB, store *store_sqlite.Store, targetRoot string,
) store_sqlite.CheckoutRootMove {
	t.Helper()
	ctx := context.Background()
	catalog := store.Catalog()
	checkout, found, err := catalog.GetCheckout(ctx, testCheckoutID)
	if err != nil || !found {
		t.Fatalf("GetCheckout(root move) = found %v, err %v", found, err)
	}
	if err := catalog.UpdateCheckoutObservation(ctx, store_sqlite.UpdateCheckoutObservationRequest{
		CheckoutID:           checkout.CheckoutID,
		Incarnation:          checkout.Incarnation,
		ExpectedRootPath:     checkout.RootPath,
		State:                checkout.State,
		RootPath:             targetRoot,
		GitDir:               targetRoot + "/.git",
		Locked:               checkout.Locked,
		Prunable:             checkout.Prunable,
		HeadRef:              checkout.HeadRef,
		HeadCommit:           checkout.HeadCommit,
		HeadTree:             checkout.HeadTree,
		LastAccessible:       checkout.LastAccessible,
		UnavailableSince:     checkout.UnavailableSince,
		AvailabilityDeadline: checkout.AvailabilityDeadline,
		RemovalDetectedAt:    checkout.RemovalDetectedAt,
		RemovalDeadline:      checkout.RemovalDeadline,
		RemovalEvidence:      checkout.RemovalEvidence,
		LastSeen:             checkout.LastSeen + 1,
		LastError:            checkout.LastError,
	}); err != nil {
		t.Fatalf("UpdateCheckoutObservation(root move): %v", err)
	}
	move, found, err := catalog.GetCheckoutRootMove(ctx, testCheckoutID)
	if err != nil || !found {
		t.Fatalf("GetCheckoutRootMove = found %v, err %v", found, err)
	}
	return move
}

func completeStackRootMove(
	t *testing.T, store *store_sqlite.Store, move store_sqlite.CheckoutRootMove,
) {
	t.Helper()
	ctx := context.Background()
	catalog := store.Catalog()
	const beforeHash, afterHash = "before-root-move", "after-root-move"
	if err := catalog.PrepareCheckoutRootMoveConfig(ctx, move.CheckoutID, move.Incarnation,
		move.ConfigRootPath, move.CurrentRootPath, beforeHash, afterHash); err != nil {
		t.Fatalf("PrepareCheckoutRootMoveConfig: %v", err)
	}
	if err := catalog.AcknowledgeCheckoutRootMoveConfig(ctx, move.CheckoutID, move.Incarnation,
		move.ConfigRootPath, move.CurrentRootPath, beforeHash, afterHash); err != nil {
		t.Fatalf("AcknowledgeCheckoutRootMoveConfig: %v", err)
	}
	move, found, err := catalog.GetCheckoutRootMove(ctx, move.CheckoutID)
	if err != nil || !found {
		t.Fatalf("GetCheckoutRootMove(acknowledged) = found %v, err %v", found, err)
	}
	if err := catalog.CompleteCheckoutRootMove(ctx, move); err != nil {
		t.Fatalf("CompleteCheckoutRootMove: %v", err)
	}
}

func newTestMaterializer(store *store_sqlite.Store) *Materializer {
	return &Materializer{Store: store, Catalog: store.Catalog(), Leases: NewLeaseManager()}
}

// seedRoutedStack builds the whole fixture a routed view needs: the
// corpus, both generations, the control plane, and the active route.
func seedRoutedStack(t *testing.T, store *store_sqlite.Store) (commit, dirty int64) {
	t.Helper()
	seedStackCorpus(t, store)
	commit = writeStackCommitGeneration(t, store)
	dirty = writeStackDirtyGeneration(t, store, commit)
	seedStackControlPlane(t, store)
	routeStack(t, store, commit, dirty, store_sqlite.RouteActive)
	return commit, dirty
}

// writeStackBaseGeneration publishes a flat dedicated corpus. Every path is
// claimed so generation-local search masks the stale shared corpus exactly as
// graph reads do when the generation is used as a nonzero physical base.
func writeStackBaseGeneration(t *testing.T, store *store_sqlite.Store) int64 {
	t.Helper()
	ctx := context.Background()
	generationID, handle, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:      "dedicated_graph",
		GraphID:        testGraphID,
		LayerID:        "layer-dedicated-base",
		CheckoutID:     testCheckoutID,
		GenerationKind: "dedicated",
		TreeOID:        "tree-base",
		CreatedAt:      500,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(base): %v", err)
	}
	seedStackCorpus(t, handle)
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{RepoPrefix: stackRepo, FilePath: stackKeepFile, Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: stackRepo, FilePath: stackDepFile, Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: stackRepo, FilePath: stackEditFile, Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: stackRepo, FilePath: stackGoneFile, Mode: store_sqlite.OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks(base): %v", err)
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, 750); err != nil {
		t.Fatalf("PublishPayloadGeneration(base): %v", err)
	}
	return generationID
}

func seedNonzeroBaseRoutedStack(t *testing.T, store *store_sqlite.Store) (base, commit, dirty int64) {
	t.Helper()
	seedStackCorpus(t, store)
	// Poison an unchanged path in generation zero. A materializer that still
	// hard-codes the shared corpus beneath the commit layer returns line 99;
	// the dedicated base correctly returns line 5.
	store.AddBatch([]*graph.Node{
		stackSymbol(stackCallerID, "Caller", graph.KindFunction, stackDepFile, 99),
	}, nil)
	base = writeStackBaseGeneration(t, store)
	commit = writeStackCommitGeneration(t, store, base)
	dirty = writeStackDirtyGeneration(t, store, commit)
	seedStackControlPlane(t, store, base)
	routeStack(t, store, commit, dirty, store_sqlite.RouteActive)
	return base, commit, dirty
}

// TestMaterializeCheckoutReadsNonzeroBaseAncestry compares the physical
// base -> commit -> dirty stack with the same final graph indexed flat. The
// existing fixture carries file overrides, a node tombstone, an edge-source
// replacement, and a dirty override, so agreement covers every masking mode.
func TestMaterializeCheckoutReadsNonzeroBaseAncestry(t *testing.T) {
	store := openStackStore(t, "nonzero-base")
	base, commit, dirty := seedNonzeroBaseRoutedStack(t, store)
	flat := openStackStore(t, "nonzero-base-flat")
	seedStackFlatCorpus(t, flat)

	materializer := newTestMaterializer(store)
	view, err := materializer.MaterializeCheckout(context.Background(), testCheckoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	defer view.Close()

	wantGenerations := []int64{base, commit, dirty}
	if got := view.Generations(); !slicesEqualInt64(got, wantGenerations) {
		t.Fatalf("Generations() = %v, want %v", got, wantGenerations)
	}
	sources := view.GenerationSources()
	if len(sources) != len(wantGenerations) {
		t.Fatalf("GenerationSources() has %d entries, want %d", len(sources), len(wantGenerations))
	}
	for index, want := range wantGenerations {
		if sources[index].Generation != want {
			t.Errorf("GenerationSources()[%d] = %d, want %d", index, sources[index].Generation, want)
		}
	}
	if view.ID.BaseGeneration != commit {
		t.Errorf("identity base generation = %d, want routed commit %d", view.ID.BaseGeneration, commit)
	}
	if got := view.Reader.GetNode(stackCallerID); got == nil || got.StartLine != 5 {
		t.Fatalf("unchanged base node = %+v, want dedicated-base line 5", got)
	}
	if got := view.Reader.GetNode(stackStaleID); got != nil {
		t.Fatalf("commit tombstone leaked node %+v", got)
	}
	assertReadersAgree(t, view.Reader, flat)
}

func slicesEqualInt64(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// TestMaterializeCheckoutReadsTheRoutedStack is the end-to-end: the
// route names two generations, and what the checkout reads through them
// is the tree those generations describe — the same graph a flat index
// of that tree produces.
func TestMaterializeCheckoutReadsTheRoutedStack(t *testing.T) {
	store := openStackStore(t, "routed")
	commit, dirty := seedRoutedStack(t, store)

	flat := openStackStore(t, "flat")
	seedStackFlatCorpus(t, flat)

	view, err := newTestMaterializer(store).MaterializeCheckout(context.Background(), testCheckoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	defer view.Close()

	if got, want := view.Generations(), []int64{commit, dirty}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Generations() = %v, want %v", got, want)
	}
	if view.ID.BaseGeneration != commit {
		t.Errorf("identity base generation = %d, want the commit generation %d", view.ID.BaseGeneration, commit)
	}
	if view.ID.RepoPrefix != stackRepo || view.ID.BaseGraphID != testGraphID {
		t.Errorf("identity names %q/%q, want %q/%q", view.ID.RepoPrefix, view.ID.BaseGraphID, stackRepo, testGraphID)
	}
	if len(view.ID.Layers) != 1 {
		t.Fatalf("identity names %d layers, want the working tree alone", len(view.ID.Layers))
	}
	want := LayerRef{Kind: LayerDirty, LayerID: stackDirtyLayerID, Generation: dirty}
	if !view.ID.Layers[0].Equal(want) {
		t.Errorf("layer = %+v, want %+v", view.ID.Layers[0], want)
	}

	assertReadersAgree(t, view.Reader, flat)
}

func TestMaterializeCheckoutFencesPendingRootMove(t *testing.T) {
	store := openStackStore(t, "pending-root-move")
	commit, dirty := seedRoutedStack(t, store)
	materializer := newTestMaterializer(store)
	move := publishStackRootMove(t, store, "/tmp/wt-1-moved")

	view, err := materializer.MaterializeCheckout(context.Background(), testCheckoutID)
	if view != nil {
		view.Close()
		t.Fatal("pending root move returned an exact checkout view")
	}
	if CodeOf(err) != CodeViewBuilding {
		t.Fatalf("MaterializeCheckout(pending move) = %v, want %s", err, CodeViewBuilding)
	}
	if held := materializer.Leases.Held(); held != 0 {
		t.Fatalf("pending root move held %d generation leases, want 0", held)
	}

	completeStackRootMove(t, store, move)
	view, err = materializer.MaterializeCheckout(context.Background(), testCheckoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout(after move): %v", err)
	}
	defer view.Close()
	if got, want := view.Generations(), []int64{commit, dirty}; !slicesEqualInt64(got, want) {
		t.Fatalf("Generations() = %v, want unchanged route %v", got, want)
	}
}

// TestPinCheckoutRouteRejectsASnapshotThatMovedBeforeLease makes the
// route-read/lease race deterministic. The old stack is already unreferenced
// and gone when its late pins land, so the pin handshake must reject the stale
// snapshot without opening it; the ordinary materializer can then serve the
// replacement route.
func TestPinCheckoutRouteRejectsASnapshotThatMovedBeforeLease(t *testing.T) {
	ctx := context.Background()
	store := openStackStore(t, "route-snapshot")
	oldCommit, oldDirty := seedRoutedStack(t, store)
	materializer := newTestMaterializer(store)

	oldRoute, found, err := store.Catalog().GetCheckoutRoute(ctx, testCheckoutID)
	if err != nil || !found {
		t.Fatalf("GetCheckoutRoute(old) = found %v, err %v", found, err)
	}
	newCommit := writeStackCommitGeneration(t, store)
	newDirty := writeStackDirtyGeneration(t, store, newCommit)
	if err := store.Catalog().FlipCheckoutRoute(ctx, store_sqlite.FlipCheckoutRouteRequest{
		CheckoutID:         testCheckoutID,
		ExpectedRouteEpoch: oldRoute.RouteEpoch,
		GraphID:            testGraphID,
		CommitGenerationID: newCommit,
		DirtyGenerationID:  newDirty,
		State:              store_sqlite.RouteActive,
	}); err != nil {
		t.Fatalf("FlipCheckoutRoute: %v", err)
	}
	if _, err := store.Catalog().PruneCheckoutCommitCachePins(ctx,
		store_sqlite.CheckoutCommitCacheRetention{
			InactiveCutoff:  time.Now().Add(time.Second).Unix(),
			MaxGenerations:  32,
			MaxStorageBytes: 1 << 62,
		}); err != nil {
		t.Fatalf("evict old checkout commit cache pin: %v", err)
	}
	if err := store.RetirePayloadGeneration(ctx, oldDirty, materializer.Leases.InUse); err != nil {
		t.Fatalf("retire old dirty generation: %v", err)
	}
	if err := store.RetirePayloadGeneration(ctx, oldCommit, materializer.Leases.InUse); err != nil {
		t.Fatalf("retire old commit generation: %v", err)
	}

	lease, moved, err := materializer.pinCheckoutRoute(ctx, testCheckoutID, oldRoute, []int64{oldCommit, oldDirty})
	if err != nil {
		t.Fatalf("pinCheckoutRoute: %v", err)
	}
	if lease != nil {
		lease.Release()
		t.Fatal("a moved route returned a lease")
	}
	if !moved {
		t.Fatal("a moved route was accepted as current")
	}
	for _, generationID := range []int64{oldCommit, oldDirty} {
		if materializer.Leases.InUse(generationID) {
			t.Fatalf("old generation %d leaked a lease", generationID)
		}
	}

	view, err := materializer.MaterializeCheckout(ctx, testCheckoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout(replacement): %v", err)
	}
	defer view.Close()
	got := view.Generations()
	if len(got) != 2 || got[0] != newCommit || got[1] != newDirty {
		t.Fatalf("replacement generations = %v, want [%d %d]", got, newCommit, newDirty)
	}
}

// TestMaterializeRefViewStacksOneLayer pins the shape of a view of committed
// state: the graph's corpus with exactly one generation on it, named by that
// generation and carrying no layer above it. A ref selector means the
// committed tree, so there is no working-tree slot to add.
func TestMaterializeRefViewStacksOneLayer(t *testing.T) {
	store := openStackStore(t, "refview")
	seedStackCorpus(t, store)
	commit := writeStackCommitGeneration(t, store)
	seedStackControlPlane(t, store)

	materializer := newTestMaterializer(store)
	view, err := materializer.MaterializeRefView(context.Background(), testGraphID, commit)
	if err != nil {
		t.Fatalf("MaterializeRefView: %v", err)
	}
	defer view.Close()

	if got := view.Generations(); len(got) != 1 || got[0] != commit {
		t.Fatalf("Generations() = %v, want the ref generation alone", got)
	}
	if len(view.ID.Layers) != 0 {
		t.Errorf("identity names %d layers, want none", len(view.ID.Layers))
	}
	if view.ID.BaseGeneration != commit || view.ID.BaseGraphID != testGraphID || view.ID.RepoPrefix != stackRepo {
		t.Errorf("identity = %+v, want %s/%s at %d", view.ID, stackRepo, testGraphID, commit)
	}
	if view.ID.Fingerprint() == "" {
		t.Error("the view has no fingerprint to name its files by")
	}
	if !view.Completeness.IsComplete(CapSyntaxGraph) {
		t.Errorf("%s = %q, want it complete", CapSyntaxGraph, view.Completeness.State(CapSyntaxGraph))
	}
	if len(view.GenerationSources()) != 1 {
		t.Errorf("the search stack has %d corpora, want the one generation", len(view.GenerationSources()))
	}

	// The lease is the same one retirement consults, so the generation a live
	// ref view reads cannot be swept out from under it.
	if err := store.RetirePayloadGeneration(context.Background(), commit, materializer.Leases.InUse); err == nil {
		t.Error("a leased ref-view generation was retired while a view still read it")
	}
}

// TestMaterializeRefViewRefusesWhatItCannotName pins the guards: without a
// graph or a published generation there is nothing to compose.
func TestMaterializeRefViewRefusesWhatItCannotName(t *testing.T) {
	store := openStackStore(t, "refview-guards")
	seedStackCorpus(t, store)
	commit := writeStackCommitGeneration(t, store)
	seedStackControlPlane(t, store)
	materializer := newTestMaterializer(store)

	cases := []struct {
		name         string
		graphID      string
		generationID int64
		code         string
	}{
		{"no graph", "", commit, CodeInvalidViewSelector},
		{"no generation", testGraphID, 0, CodeViewBuilding},
		{"a generation the catalog has no row for", testGraphID, commit + 5000, CodeViewBuilding},
		{"a graph the catalog has no row for", "graph-absent", commit, CodeCheckoutInaccessible},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view, err := materializer.MaterializeRefView(context.Background(), tc.graphID, tc.generationID)
			if err == nil {
				view.Close()
				t.Fatalf("MaterializeRefView succeeded, want %s", tc.code)
			}
			if got := CodeOf(err); got != tc.code {
				t.Fatalf("code = %q, want %q (%v)", got, tc.code, err)
			}
		})
	}
}

// writeProducerGeneration publishes one generation whose only content is
// the producer states it declares, so a completeness test is not also a
// test of what the layer masks.
func writeProducerGeneration(
	t *testing.T,
	store *store_sqlite.Store,
	kind, layerID string,
	baseGeneration, createdAt int64,
	rows ...store_sqlite.ProducerCompleteness,
) int64 {
	t.Helper()
	ctx := context.Background()
	generationID, handle, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:        "dedicated_graph",
		GraphID:          testGraphID,
		LayerID:          layerID,
		CheckoutID:       testCheckoutID,
		GenerationKind:   kind,
		BaseGenerationID: baseGeneration,
		TreeOID:          "tree-" + kind,
		CreatedAt:        createdAt,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(%s): %v", kind, err)
	}
	for _, row := range rows {
		if err := handle.SetProducerState(row); err != nil {
			t.Fatalf("SetProducerState(%s, %s): %v", kind, row.Producer, err)
		}
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, createdAt+1); err != nil {
		t.Fatalf("PublishPayloadGeneration(%s): %v", kind, err)
	}
	return generationID
}

// TestMaterializeCheckoutCompletenessTakesTheWorstState pins the direction
// of the union: a generation stacked on top cannot repair a capability the
// generation below it declared narrowed, so the view reports the worst
// state any generation in the stack declares.
//
// The first case is the one a live daemon served: the commit generation
// truncated its closure and declared incoming edges incomplete, the
// working-tree generation built its own small change whole and declared
// them complete, and a last-writer-wins union reported a knowingly partial
// view as complete — so require_complete and a required-capability request
// both passed on it.
func TestMaterializeCheckoutCompletenessTakesTheWorstState(t *testing.T) {
	cases := []struct {
		name   string
		commit store_sqlite.ProducerState
		dirty  store_sqlite.ProducerState
		want   CapabilityState
	}{
		{
			name:   "the lower generation narrows",
			commit: store_sqlite.ProducerStateIncomplete,
			dirty:  store_sqlite.ProducerStateComplete,
			want:   StateIncomplete,
		},
		{
			name:   "the upper generation narrows",
			commit: store_sqlite.ProducerStateComplete,
			dirty:  store_sqlite.ProducerStateIncomplete,
			want:   StateIncomplete,
		},
		{
			name:   "a terminal denial outranks a partial one",
			commit: store_sqlite.ProducerStateDisabledByConfig,
			dirty:  store_sqlite.ProducerStateIncomplete,
			want:   StateDisabledByConfig,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := openStackStore(t, "worst-state")
			seedStackControlPlane(t, store)
			commit := writeProducerGeneration(t, store, "commit", stackCommitLayerID, 0, 1000,
				store_sqlite.ProducerCompleteness{
					Producer: string(CapIncomingEdges),
					State:    tc.commit,
					Reason:   "the commit closure stopped at the file budget",
				})
			dirty := writeProducerGeneration(t, store, "dirty", stackDirtyLayerID, commit, 3000,
				store_sqlite.ProducerCompleteness{
					Producer: string(CapIncomingEdges),
					State:    tc.dirty,
					Reason:   "the working-tree build covered everything it touched",
				})
			routeStack(t, store, commit, dirty, store_sqlite.RouteActive)

			view, err := newTestMaterializer(store).MaterializeCheckout(context.Background(), testCheckoutID)
			if err != nil {
				t.Fatalf("MaterializeCheckout: %v", err)
			}
			defer view.Close()

			if got := view.Completeness.State(CapIncomingEdges); got != tc.want {
				t.Fatalf("%s = %q, want %q", CapIncomingEdges, got, tc.want)
			}
			err = view.Completeness.Evaluate([]CapabilityID{CapIncomingEdges}, nil)
			wantCode := CodeRequiredCapabilityIncomplete
			if tc.want.Terminal() {
				wantCode = CodeCapabilityUnavailable
			}
			if code := CodeOf(err); code != wantCode {
				t.Fatalf("Evaluate(%s) = %v, want %s", CapIncomingEdges, err, wantCode)
			}
			// A capability no generation narrowed still evaluates, so the
			// union narrows what the stack declared and nothing else.
			if err := view.Completeness.Evaluate([]CapabilityID{CapSyntaxGraph}, nil); err != nil {
				t.Errorf("a capability nothing narrowed did not evaluate: %v", err)
			}
		})
	}
}

// TestMaterializeCheckoutCompletenessRunsBottomUp pins the union: the
// corpus underneath contributes completeness for everything, and the one
// capability the working-tree generation declares narrowed is the only
// one the view reports narrowed.
func TestMaterializeCheckoutCompletenessRunsBottomUp(t *testing.T) {
	store := openStackStore(t, "completeness")
	seedRoutedStack(t, store)

	view, err := newTestMaterializer(store).MaterializeCheckout(context.Background(), testCheckoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	defer view.Close()

	if got := view.Completeness.State(CapResolutionCrossRepo); got != StateIncomplete {
		t.Errorf("%s = %q, want %q", CapResolutionCrossRepo, got, StateIncomplete)
	}
	for _, id := range KnownCapabilities() {
		if id == CapResolutionCrossRepo {
			continue
		}
		if got := view.Completeness.State(id); got != StateComplete {
			t.Errorf("%s = %q, want %q", id, got, StateComplete)
		}
	}
	if err := view.Completeness.Evaluate([]CapabilityID{CapSyntaxGraph}, nil); err != nil {
		t.Errorf("a capability nothing narrowed did not evaluate: %v", err)
	}
	if err := view.Completeness.Evaluate([]CapabilityID{CapResolutionCrossRepo}, nil); err == nil {
		t.Error("the narrowed capability evaluated as servable")
	}
}

// TestMaterializeCheckoutRefusesPartialStacks pins the rule that a route
// whose slots cannot all be served yields a typed failure rather than a
// thinner stack. Every case here would otherwise answer out of the wrong
// state of the world while looking like a success.
func TestMaterializeCheckoutRefusesPartialStacks(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(t *testing.T, store *store_sqlite.Store, commit, dirty int64)
		code  string
	}{
		{
			name:  "checkout is not routed",
			setup: func(t *testing.T, store *store_sqlite.Store, _, _ int64) { deleteRoute(t, store) },
			code:  CodeCheckoutInaccessible,
		},
		{
			name: "route has retired",
			setup: func(t *testing.T, store *store_sqlite.Store, commit, dirty int64) {
				routeStack(t, store, commit, dirty, store_sqlite.RouteRetired)
			},
			code: CodeCheckoutInaccessible,
		},
		{
			name: "no commit generation",
			setup: func(t *testing.T, store *store_sqlite.Store, _, dirty int64) {
				routeStack(t, store, 0, dirty, store_sqlite.RouteActive)
			},
			code: CodeViewBuilding,
		},
		{
			name: "working-tree generation is still building",
			setup: func(t *testing.T, store *store_sqlite.Store, _, dirty int64) {
				setGenerationState(t, store, dirty, store_sqlite.ViewGenerationBuilding, store_sqlite.ViewGenerationReady)
			},
			code: CodeViewBuilding,
		},
		{
			name: "working-tree generation is retiring",
			setup: func(t *testing.T, store *store_sqlite.Store, _, dirty int64) {
				setGenerationState(t, store, dirty, store_sqlite.ViewGenerationRetiring, store_sqlite.ViewGenerationReady)
			},
			code: CodeCheckoutInaccessible,
		},
		{
			name: "commit generation is retiring",
			setup: func(t *testing.T, store *store_sqlite.Store, commit, _ int64) {
				setGenerationState(t, store, commit, store_sqlite.ViewGenerationRetiring, store_sqlite.ViewGenerationReady)
			},
			code: CodeCheckoutInaccessible,
		},
		{
			name:  "graph has no catalog row",
			setup: func(t *testing.T, store *store_sqlite.Store, _, _ int64) { deleteDedicatedGraph(t, store) },
			code:  CodeCheckoutInaccessible,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := openStackStore(t, "partial")
			commit, dirty := seedRoutedStack(t, store)
			tc.setup(t, store, commit, dirty)

			materializer := newTestMaterializer(store)
			view, err := materializer.MaterializeCheckout(ctx, testCheckoutID)
			if err == nil {
				view.Close()
				t.Fatalf("MaterializeCheckout succeeded, want %s", tc.code)
			}
			if code := CodeOf(err); code != tc.code {
				t.Fatalf("error code = %q, want %q (%v)", code, tc.code, err)
			}
			for _, id := range []int64{commit, dirty} {
				if materializer.Leases.InUse(id) {
					t.Fatalf("generation %d stayed leased after a refused materialization", id)
				}
			}
		})
	}
}

// TestMaterializeCheckoutValidatesItsInputs pins the guards that turn a
// missing dependency into a typed error here instead of a nil
// dereference inside a read.
func TestMaterializeCheckoutValidatesItsInputs(t *testing.T) {
	store := openStackStore(t, "inputs")
	full := newTestMaterializer(store)

	cases := map[string]struct {
		materializer *Materializer
		ctx          context.Context
		checkoutID   string
	}{
		"nil materializer": {nil, context.Background(), testCheckoutID},
		"no store":         {&Materializer{Catalog: store.Catalog(), Leases: NewLeaseManager()}, context.Background(), testCheckoutID},
		"no catalog":       {&Materializer{Store: store, Leases: NewLeaseManager()}, context.Background(), testCheckoutID},
		"no lease manager": {&Materializer{Store: store, Catalog: store.Catalog()}, context.Background(), testCheckoutID},
		"no context":       {full, nil, testCheckoutID},
		"no checkout id":   {full, context.Background(), ""},
		"unknown checkout": {full, context.Background(), "nobody"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			view, err := tc.materializer.MaterializeCheckout(tc.ctx, tc.checkoutID)
			if err == nil {
				view.Close()
				t.Fatal("MaterializeCheckout succeeded, want an error")
			}
			if CodeOf(err) == "" {
				t.Fatalf("error carries no view code: %v", err)
			}
		})
	}
}

// TestMaterializeCheckoutLeaseBlocksRetirement is the lease acceptance:
// retirement consults the same manager the materializer pins through, so
// a generation a live view reads cannot be swept out from under it, and
// the drain completes once the view closes.
func TestMaterializeCheckoutLeaseBlocksRetirement(t *testing.T) {
	ctx := context.Background()
	store := openStackStore(t, "leases")
	commit, dirty := seedRoutedStack(t, store)

	materializer := newTestMaterializer(store)
	view, err := materializer.MaterializeCheckout(ctx, testCheckoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	for _, id := range []int64{commit, dirty} {
		if !materializer.Leases.InUse(id) {
			t.Fatalf("generation %d is not leased by the materialized view", id)
		}
	}

	// Un-route the working-tree generation so the catalog's own reference
	// guard passes: what must refuse the retire from here on is the lease
	// alone, not the route still pointing at it.
	unrouteDirty(t, store)
	if err := store.RetirePayloadGeneration(ctx, dirty, materializer.Leases.InUse); !errors.Is(err, store_sqlite.ErrPayloadGenerationInUse) {
		t.Fatalf("retire while the view is open = %v, want %v", err, store_sqlite.ErrPayloadGenerationInUse)
	}

	// The drain cannot finish while the view holds the lease, so a bounded
	// wait must expire rather than report the generation released.
	bounded, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := materializer.Leases.WaitDrain(bounded, view.Generations()...); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitDrain while the view is open = %v, want %v", err, context.DeadlineExceeded)
	}

	view.Close()
	view.Close() // idempotent: a second close must not drop another pin
	if err := materializer.Leases.WaitDrain(ctx, commit, dirty); err != nil {
		t.Fatalf("WaitDrain after Close: %v", err)
	}
	if materializer.Leases.InUse(dirty) {
		t.Fatal("the working-tree generation is still leased after Close")
	}
	if err := store.RetirePayloadGeneration(ctx, dirty, materializer.Leases.InUse); err != nil {
		t.Fatalf("retire after Close: %v", err)
	}
}

// TestMaterializeCheckoutLeasePinsNonzeroBase isolates the physical base
// pin after catalog references have been removed. Retirement must still
// refuse the base until the materialized view closes.
func TestMaterializeCheckoutLeasePinsNonzeroBase(t *testing.T) {
	ctx := context.Background()
	store := openStackStore(t, "nonzero-base-lease")
	base, commit, dirty := seedNonzeroBaseRoutedStack(t, store)
	materializer := newTestMaterializer(store)
	view, err := materializer.MaterializeCheckout(ctx, testCheckoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	defer view.Close()

	for _, generationID := range []int64{base, commit, dirty} {
		if !materializer.Leases.InUse(generationID) {
			t.Fatalf("generation %d is not leased", generationID)
		}
	}
	deleteRoute(t, store)
	deleteDedicatedGraph(t, store)
	// Retire the upper layers without the lease callback solely to isolate the
	// base pin. The view is not read again after its payload is dismantled.
	if err := store.RetirePayloadGeneration(ctx, dirty, nil); err != nil {
		t.Fatalf("retire dirty fixture: %v", err)
	}
	if err := store.RetirePayloadGeneration(ctx, commit, nil); err != nil {
		t.Fatalf("retire commit fixture: %v", err)
	}
	if err := store.RetirePayloadGeneration(ctx, base, materializer.Leases.InUse); !errors.Is(err, store_sqlite.ErrPayloadGenerationInUse) {
		t.Fatalf("retire leased base = %v, want %v", err, store_sqlite.ErrPayloadGenerationInUse)
	}

	view.Close()
	if err := store.RetirePayloadGeneration(ctx, base, materializer.Leases.InUse); err != nil {
		t.Fatalf("retire base after Close: %v", err)
	}
}

// writeBenchmarkGeneration publishes an empty generation: the benchmark
// measures catalog ancestry, pinning, and composition rather than fixture IO.
func writeBenchmarkGeneration(t testing.TB, store *store_sqlite.Store, kind, layerID string, base int64) int64 {
	t.Helper()
	ctx := context.Background()
	generationID, _, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:        "dedicated_graph",
		GraphID:          testGraphID,
		LayerID:          layerID,
		CheckoutID:       testCheckoutID,
		GenerationKind:   kind,
		BaseGenerationID: base,
		TreeOID:          "tree-" + kind,
		CreatedAt:        100,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(%s): %v", kind, err)
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, 200); err != nil {
		t.Fatalf("PublishPayloadGeneration(%s): %v", kind, err)
	}
	return generationID
}

func BenchmarkMaterializeCheckoutBaseGeneration(b *testing.B) {
	for _, test := range []struct {
		name        string
		nonzeroBase bool
	}{
		{name: "generation0"},
		{name: "nonzero_base", nonzeroBase: true},
	} {
		b.Run(test.name, func(b *testing.B) {
			store := openStackStore(b, test.name)
			base := int64(0)
			if test.nonzeroBase {
				base = writeBenchmarkGeneration(b, store, "dedicated", "bench-base", 0)
			}
			commit := writeBenchmarkGeneration(b, store, "commit", "bench-commit", base)
			dirty := writeBenchmarkGeneration(b, store, "dirty", "bench-dirty", commit)
			seedStackControlPlane(b, store, base)
			routeStack(b, store, commit, dirty, store_sqlite.RouteActive)
			materializer := newTestMaterializer(store)

			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				view, err := materializer.MaterializeCheckout(context.Background(), testCheckoutID)
				if err != nil {
					b.Fatalf("MaterializeCheckout: %v", err)
				}
				view.Close()
			}
		})
	}
}

func BenchmarkMaterializeCheckoutRootMoveFence(b *testing.B) {
	for _, test := range []struct {
		name    string
		pending bool
	}{
		{name: "ready_miss"},
		{name: "pending_hit", pending: true},
	} {
		b.Run(test.name, func(b *testing.B) {
			store := openStackStore(b, "root-move-"+test.name)
			commit := writeBenchmarkGeneration(b, store, "commit", "bench-commit", 0)
			dirty := writeBenchmarkGeneration(b, store, "dirty", "bench-dirty", commit)
			seedStackControlPlane(b, store)
			routeStack(b, store, commit, dirty, store_sqlite.RouteActive)
			if test.pending {
				publishStackRootMove(b, store, "/tmp/wt-1-moved")
			}
			materializer := newTestMaterializer(store)

			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				view, err := materializer.MaterializeCheckout(context.Background(), testCheckoutID)
				if test.pending {
					if view != nil || CodeOf(err) != CodeViewBuilding {
						b.Fatalf("pending materialization = view %v, err %v", view, err)
					}
					if materializer.Leases.Held() != 0 {
						b.Fatal("pending move acquired a generation lease")
					}
					continue
				}
				if err != nil {
					b.Fatalf("MaterializeCheckout: %v", err)
				}
				view.Close()
			}
		})
	}
}

// deleteRoute withdraws the checkout's route row.
func deleteRoute(t *testing.T, store *store_sqlite.Store) {
	t.Helper()
	if err := store.Catalog().DeleteCheckoutRoute(context.Background(), testCheckoutID); err != nil {
		t.Fatalf("DeleteCheckoutRoute: %v", err)
	}
}

// unrouteDirty clears the working-tree slot through the route's own
// compare-and-set, which is how a reconciler retires a generation.
func unrouteDirty(t *testing.T, store *store_sqlite.Store) {
	t.Helper()
	ctx := context.Background()
	catalog := store.Catalog()
	route, found, err := catalog.GetCheckoutRoute(ctx, testCheckoutID)
	if err != nil || !found {
		t.Fatalf("GetCheckoutRoute: %v (found=%v)", err, found)
	}
	if err := catalog.FlipCheckoutRouteSlot(ctx, store_sqlite.FlipCheckoutRouteSlotRequest{
		CheckoutID:         testCheckoutID,
		Slot:               store_sqlite.RouteSlotDirty,
		GenerationID:       0,
		ExpectedRouteEpoch: route.RouteEpoch,
		State:              store_sqlite.RouteActive,
	}); err != nil {
		t.Fatalf("FlipCheckoutRouteSlot: %v", err)
	}
}

// deleteDedicatedGraph removes the graph row the view identity is named
// from, leaving the route pointing at a graph nothing describes.
func deleteDedicatedGraph(t *testing.T, store *store_sqlite.Store) {
	t.Helper()
	if err := store.Catalog().DeleteDedicatedGraph(context.Background(), testGraphID); err != nil {
		t.Fatalf("DeleteDedicatedGraph: %v", err)
	}
}

func setGenerationState(t *testing.T, store *store_sqlite.Store, generationID int64, next, expected store_sqlite.ViewGenerationState) {
	t.Helper()
	if err := store.Catalog().SetViewGenerationState(context.Background(), generationID, next, expected); err != nil {
		t.Fatalf("SetViewGenerationState(%d, %s): %v", generationID, next, err)
	}
}
