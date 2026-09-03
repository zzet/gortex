package store_sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/viewmetrics"
)

// Why a generation still exists, as a counter.
//
// A retirement that is refused is not an error — the generation is being read,
// or something still points at it — so the only trace it leaves is the refusal
// class. These pin that the three holders the guard checks are reported apart:
// a leased generation calls for waiting, a routed one calls for looking at the
// route, and a based-upon one calls for retiring the layer above it first.

func retireRefusalDelta(before, after viewmetrics.Snapshot, reason string) int64 {
	key := viewmetrics.GenerationRetireRefusedTotal + "{reason=" + reason + "}"
	return after.Counters[key] - before.Counters[key]
}

func retiredDelta(before, after viewmetrics.Snapshot, owner string) int64 {
	key := viewmetrics.GenerationRetiredTotal + "{owner=" + owner + "}"
	return after.Counters[key] - before.Counters[key]
}

// publishedPayloadGeneration builds one ready generation over the base corpus
// and returns its id.
func publishedPayloadGeneration(t *testing.T, store *Store) int64 {
	t.Helper()
	generationID, handle, err := store.BeginPayloadGeneration(context.Background(), payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)
	if err := store.PublishPayloadGeneration(context.Background(), generationID, 5000); err != nil {
		t.Fatalf("PublishPayloadGeneration: %v", err)
	}
	return generationID
}

// TestRetireRefusalCountsTheLease pins the leased class, and the retirement
// that follows once the lease drops.
func TestRetireRefusalCountsTheLease(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	generationID := publishedPayloadGeneration(t, store)

	leased := true
	inUse := func(candidate int64) bool { return leased && candidate == generationID }

	before := viewmetrics.Read()
	if err := store.RetirePayloadGeneration(ctx, generationID, inUse); !errors.Is(err, ErrPayloadGenerationInUse) {
		t.Fatalf("retire while leased = %v, want %v", err, ErrPayloadGenerationInUse)
	}
	refused := viewmetrics.Read()
	if got := retireRefusalDelta(before, refused, viewmetrics.RefusedLeased); got != 1 {
		t.Fatalf("leased refusals = %d, want 1", got)
	}
	if got := retireRefusalDelta(before, refused, viewmetrics.RefusedRouted); got != 0 {
		t.Fatalf("a leased refusal was also counted as routed (%d)", got)
	}

	leased = false
	if err := store.RetirePayloadGeneration(ctx, generationID, inUse); err != nil {
		t.Fatalf("retire after the lease dropped: %v", err)
	}
	collected := viewmetrics.Read()
	if got := retiredDelta(refused, collected, viewmetrics.OwnerCheckout); got != 1 {
		t.Fatalf("checkout generations retired = %d, want 1", got)
	}
}

// TestRetireRefusalCountsTheRoute pins the routed class: the guard's first
// question is whether a checkout is being served from the generation.
func TestRetireRefusalCountsTheRoute(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)
	if err := store.PublishAndRoute(ctx, generationID, payloadCheckoutID, 0, RouteSlotDirty); err != nil {
		t.Fatalf("PublishAndRoute: %v", err)
	}

	before := viewmetrics.Read()
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); !errors.Is(err, ErrCatalogGenerationReferenced) {
		t.Fatalf("retire while routed = %v, want %v", err, ErrCatalogGenerationReferenced)
	}
	after := viewmetrics.Read()

	if got := retireRefusalDelta(before, after, viewmetrics.RefusedRouted); got != 1 {
		t.Fatalf("routed refusals = %d, want 1", got)
	}
	if got := retireRefusalDelta(before, after, viewmetrics.RefusedBased); got != 0 {
		t.Fatalf("a routed refusal was also counted as based (%d)", got)
	}
	if got := retireRefusalDelta(before, after, viewmetrics.RefusedLeased); got != 0 {
		t.Fatalf("a routed refusal was also counted as leased (%d)", got)
	}
}

// TestRetireRefusalCountsTheLayerAbove pins the based class, which is the one
// a reader cannot guess: nothing is serving the generation and nothing is
// reading it, but a layer above names it as its base and must go first.
func TestRetireRefusalCountsTheLayerAbove(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	lower := publishedPayloadGeneration(t, store)
	upperReq := payloadRequest()
	upperReq.LayerID = "layer-above"
	upperReq.GenerationKind = "commit"
	upperReq.BaseGenerationID = lower
	upper, _, err := store.BeginPayloadGeneration(ctx, upperReq)
	if err != nil {
		t.Fatalf("BeginPayloadGeneration for the layer above: %v", err)
	}
	if upper == lower {
		t.Fatal("the layer above adopted the generation it sits on")
	}

	before := viewmetrics.Read()
	if err := store.RetirePayloadGeneration(ctx, lower, nil); !errors.Is(err, ErrCatalogGenerationReferenced) {
		t.Fatalf("retire under a layer = %v, want %v", err, ErrCatalogGenerationReferenced)
	}
	after := viewmetrics.Read()

	if got := retireRefusalDelta(before, after, viewmetrics.RefusedBased); got != 1 {
		t.Fatalf("based refusals = %d, want 1", got)
	}
	if got := retireRefusalDelta(before, after, viewmetrics.RefusedRouted); got != 0 {
		t.Fatalf("a based refusal was also counted as routed (%d)", got)
	}
}

// TestRetireRefusalCountsAMissingGeneration pins the class that is not a
// refusal at all: an id nothing knows about is a retirement offer for payload
// that is already gone, and reading it as a holder would be wrong.
func TestRetireRefusalCountsAMissingGeneration(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	before := viewmetrics.Read()
	if err := store.RetirePayloadGeneration(ctx, 4242, nil); !errors.Is(err, ErrCatalogNotFound) {
		t.Fatalf("retire an unknown generation = %v, want %v", err, ErrCatalogNotFound)
	}
	after := viewmetrics.Read()

	if got := retireRefusalDelta(before, after, viewmetrics.RefusedMissing); got != 1 {
		t.Fatalf("missing refusals = %d, want 1", got)
	}
}

// TestViewGenerationReferencesSplitsTheGuard pins the read the classification
// is built on: one query, the delete guard's clauses kept apart, and the
// boolean the guard enforces derived from them rather than asked separately.
func TestViewGenerationReferencesSplitsTheGuard(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)

	catalog := store.Catalog()
	refs, err := catalog.ViewGenerationReferences(ctx, generationID)
	if err != nil {
		t.Fatalf("ViewGenerationReferences: %v", err)
	}
	if refs.Any() {
		t.Fatalf("an unrouted generation reports references: %+v", refs)
	}

	if err := store.PublishAndRoute(ctx, generationID, payloadCheckoutID, 0, RouteSlotDirty); err != nil {
		t.Fatalf("PublishAndRoute: %v", err)
	}
	refs, err = catalog.ViewGenerationReferences(ctx, generationID)
	if err != nil {
		t.Fatalf("ViewGenerationReferences after routing: %v", err)
	}
	if !refs.Routed || refs.Based || refs.RefViewed {
		t.Fatalf("a routed generation reports %+v, want Routed alone", refs)
	}
	referenced, err := catalog.ViewGenerationReferenced(ctx, generationID)
	if err != nil || !referenced {
		t.Fatalf("ViewGenerationReferenced = %v, %v; want true", referenced, err)
	}
}
