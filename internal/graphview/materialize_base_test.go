package graphview

import (
	"context"
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func writeMaterializeDedicatedBaseGeneration(
	t *testing.T,
	store *store_sqlite.Store,
	graphID string,
	createdAt int64,
) int64 {
	t.Helper()
	ctx := context.Background()
	layerID := graphID + ":base"
	generationID, handle, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:      "dedicated_base",
		GraphID:        graphID,
		LayerID:        layerID,
		CheckoutID:     testCheckoutID,
		GenerationKind: "dedicated_base",
		TreeOID:        "tree-" + layerID,
		CreatedAt:      createdAt,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(%s): %v", layerID, err)
	}
	seedStackCorpus(t, handle)
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{RepoPrefix: stackRepo, FilePath: stackKeepFile, Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: stackRepo, FilePath: stackDepFile, Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: stackRepo, FilePath: stackEditFile, Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: stackRepo, FilePath: stackGoneFile, Mode: store_sqlite.OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks(%s): %v", layerID, err)
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, createdAt+1); err != nil {
		t.Fatalf("PublishPayloadGeneration(%s): %v", layerID, err)
	}
	return generationID
}

func writeMaterializeBaseCandidate(
	t *testing.T,
	store *store_sqlite.Store,
	request store_sqlite.PayloadGenerationRequest,
) int64 {
	t.Helper()
	ctx := context.Background()
	generationID, _, err := store.BeginPayloadGeneration(ctx, request)
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(%s): %v", request.LayerID, err)
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, request.CreatedAt+1); err != nil {
		t.Fatalf("PublishPayloadGeneration(%s): %v", request.LayerID, err)
	}
	return generationID
}

// TestMaterializeBasePinsExactlyTheNamedGeneration protects the grace
// fallback boundary: the caller already selected a dedicated base snapshot,
// so materialization must not re-read the graph's newer active generation or
// compose a checkout's later dirty state on top of it.
func TestMaterializeBasePinsExactlyTheNamedGeneration(t *testing.T) {
	ctx := context.Background()
	store := openStackStore(t, "materialize-base-exact")
	base := writeMaterializeDedicatedBaseGeneration(t, store, testGraphID, 11)
	newer := writeMaterializeDedicatedBaseGeneration(t, store, testGraphID, 22)
	commit := writeStackCommitGeneration(t, store, newer)
	dirty := writeStackDirtyGeneration(t, store, commit)
	// Make the named snapshot deliberately older than both the graph's active
	// base and an available checkout route. None of the newer state is part of
	// this view.
	seedStackControlPlane(t, store, newer)
	routeStack(t, store, commit, dirty, store_sqlite.RouteActive)

	materializer := newTestMaterializer(store)
	view, err := materializer.MaterializeBase(ctx, testGraphID, base)
	if err != nil {
		t.Fatalf("MaterializeBase: %v", err)
	}

	if got := view.Generations(); len(got) != 1 || got[0] != base {
		view.Close()
		t.Fatalf("Generations() = %v, want exactly [%d]", got, base)
	}
	if got := view.GenerationSources(); len(got) != 1 || got[0].Generation != base {
		view.Close()
		t.Fatalf("GenerationSources() = %+v, want exactly generation %d", got, base)
	}
	if view.ID.BaseGraphID != testGraphID || view.ID.BaseGeneration != base || view.ID.RepoPrefix != stackRepo {
		view.Close()
		t.Fatalf("identity = %+v, want %s/%s at generation %d", view.ID, stackRepo, testGraphID, base)
	}
	if len(view.ID.Layers) != 0 {
		view.Close()
		t.Fatalf("identity has %d layers, want a bare dedicated base", len(view.ID.Layers))
	}
	if got := view.Reader.GetNode(stackCallerID); got == nil || got.StartLine != 5 {
		view.Close()
		t.Fatalf("named base node = %+v, want the dedicated-base caller at line 5", got)
	}
	if got := view.Reader.GetNode(stackStaleID); got == nil {
		view.Close()
		t.Fatal("commit tombstone leaked into the named base")
	}
	if !materializer.Leases.InUse(base) {
		view.Close()
		t.Fatalf("named generation %d is not leased", base)
	}
	for _, generationID := range []int64{newer, commit, dirty} {
		if materializer.Leases.InUse(generationID) {
			view.Close()
			t.Fatalf("excluded generation %d was leased", generationID)
		}
	}
	if err := store.RetirePayloadGeneration(ctx, base, materializer.Leases.InUse); !errors.Is(err, store_sqlite.ErrPayloadGenerationInUse) {
		view.Close()
		t.Fatalf("retire named base while open = %v, want %v", err, store_sqlite.ErrPayloadGenerationInUse)
	}

	view.Close()
	view.Close() // Close is idempotent and must release exactly one pin.
	if materializer.Leases.InUse(base) {
		t.Fatalf("named generation %d is still leased after Close", base)
	}
	if err := materializer.Leases.WaitDrain(ctx, base); err != nil {
		t.Fatalf("WaitDrain after Close: %v", err)
	}
	if err := store.RetirePayloadGeneration(ctx, base, materializer.Leases.InUse); err != nil {
		t.Fatalf("retire named base after Close: %v", err)
	}
}

// TestMaterializeBaseValidatesGraphAndGenerationInputs ensures an exact base
// request cannot silently open another graph, a missing snapshot, or a dirty
// checkout layer. Every rejection must leave the lease set unchanged.
func TestMaterializeBaseValidatesGraphAndGenerationInputs(t *testing.T) {
	store := openStackStore(t, "materialize-base-inputs")
	base := writeMaterializeDedicatedBaseGeneration(t, store, testGraphID, 44)
	dirty := writeStackDirtyGeneration(t, store, base)
	other := writeMaterializeDedicatedBaseGeneration(t, store, "graph-other", 66)
	wrongLayer := writeMaterializeBaseCandidate(t, store, store_sqlite.PayloadGenerationRequest{
		OwnerKind:      "dedicated_base",
		GraphID:        testGraphID,
		LayerID:        testGraphID + ":not-base",
		CheckoutID:     testCheckoutID,
		GenerationKind: "dedicated_base",
		TreeOID:        "tree-wrong-layer",
		CreatedAt:      77,
	})
	wrongCheckout := writeMaterializeBaseCandidate(t, store, store_sqlite.PayloadGenerationRequest{
		OwnerKind:      "dedicated_base",
		GraphID:        testGraphID,
		LayerID:        testGraphID + ":base",
		CheckoutID:     "checkout-not-owner",
		GenerationKind: "dedicated_base",
		TreeOID:        "tree-wrong-checkout",
		CreatedAt:      88,
	})
	emptyTree := writeMaterializeBaseCandidate(t, store, store_sqlite.PayloadGenerationRequest{
		OwnerKind:      "dedicated_base",
		GraphID:        testGraphID,
		LayerID:        testGraphID + ":base",
		CheckoutID:     testCheckoutID,
		GenerationKind: "dedicated_base",
		CreatedAt:      99,
	})
	seedStackControlPlane(t, store, base)
	materializer := newTestMaterializer(store)

	cases := []struct {
		name         string
		ctx          context.Context
		graphID      string
		generationID int64
		wantCode     string
	}{
		{name: "nil context", graphID: testGraphID, generationID: base, wantCode: CodeInvalidViewSelector},
		{name: "empty graph", ctx: context.Background(), generationID: base, wantCode: CodeInvalidViewSelector},
		{name: "zero generation", ctx: context.Background(), graphID: testGraphID, wantCode: CodePrimaryNotReady},
		{name: "negative generation", ctx: context.Background(), graphID: testGraphID, generationID: -1, wantCode: CodePrimaryNotReady},
		{name: "missing graph", ctx: context.Background(), graphID: "graph-missing", generationID: base},
		{name: "missing generation", ctx: context.Background(), graphID: testGraphID, generationID: other + 5000},
		{name: "generation owned by another graph", ctx: context.Background(), graphID: testGraphID, generationID: other},
		{name: "dirty generation is not a dedicated base", ctx: context.Background(), graphID: testGraphID, generationID: dirty},
		{name: "dedicated base has noncanonical layer", ctx: context.Background(), graphID: testGraphID, generationID: wrongLayer},
		{name: "dedicated base belongs to another checkout", ctx: context.Background(), graphID: testGraphID, generationID: wrongCheckout},
		{name: "dedicated base has no tree", ctx: context.Background(), graphID: testGraphID, generationID: emptyTree},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view, err := materializer.MaterializeBase(tc.ctx, tc.graphID, tc.generationID)
			if err == nil {
				view.Close()
				t.Fatal("MaterializeBase succeeded, want an error")
			}
			if tc.wantCode != "" {
				if got := CodeOf(err); got != tc.wantCode {
					t.Fatalf("code = %q, want %q (%v)", got, tc.wantCode, err)
				}
			} else if got := CodeOf(err); got == "" {
				t.Fatalf("error carries no view code: %v", err)
			}
			for _, generationID := range []int64{base, dirty, other, wrongLayer, wrongCheckout, emptyTree} {
				if materializer.Leases.InUse(generationID) {
					t.Fatalf("rejected input leaked a lease on generation %d", generationID)
				}
			}
		})
	}
}

func writeBenchmarkMaterializeDedicatedBaseGeneration(
	b *testing.B,
	store *store_sqlite.Store,
	graphID string,
) int64 {
	b.Helper()
	ctx := context.Background()
	layerID := graphID + ":base"
	generationID, _, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:      "dedicated_base",
		GraphID:        graphID,
		LayerID:        layerID,
		CheckoutID:     testCheckoutID,
		GenerationKind: "dedicated_base",
		TreeOID:        "tree-" + layerID,
		CreatedAt:      77,
	})
	if err != nil {
		b.Fatalf("BeginPayloadGeneration(%s): %v", layerID, err)
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, 78); err != nil {
		b.Fatalf("PublishPayloadGeneration(%s): %v", layerID, err)
	}
	return generationID
}

// BenchmarkMaterializeBase measures the read-only grace-fallback hot path:
// validate and pin one production-shaped sealed dedicated base, assemble its
// view, then release the lease before the next request.
func BenchmarkMaterializeBase(b *testing.B) {
	ctx := context.Background()
	store := openStackStore(b, "materialize-base-benchmark")
	base := writeBenchmarkMaterializeDedicatedBaseGeneration(b, store, testGraphID)
	seedStackControlPlane(b, store, base)
	materializer := newTestMaterializer(store)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		view, err := materializer.MaterializeBase(ctx, testGraphID, base)
		if err != nil {
			b.Fatalf("MaterializeBase: %v", err)
		}
		view.Close()
	}
}
