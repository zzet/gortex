package graphview

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A derived-row correction rewrites a published generation in place. A view
// opened after it finishes reads the corrected rows, even though the
// generation's masks and rows were cached (and its rows preloaded) before the
// correction; the entry cached at the older epoch is dropped rather than
// retained beside the new one.
func TestCorrectedGenerationIsNotServedFromALayerCachedBeforeTheCorrection(t *testing.T) {
	ctx := context.Background()
	store := openStackStore(t, "layer-cache-correction")
	commit, dirty := seedRoutedStack(t, store)
	materializer := newTestMaterializer(store)
	if _, err := materializer.WarmRoute(ctx, commit, dirty); err != nil {
		t.Fatalf("WarmRoute: %v", err)
	}
	accesses := func() int {
		t.Helper()
		view, err := materializer.MaterializeCheckout(ctx, testCheckoutID)
		if err != nil {
			t.Fatalf("MaterializeCheckout: %v", err)
		}
		defer view.Close()
		for _, source := range view.GenerationSources() {
			if layer, _ := source.Layer.(*GenerationLayer); source.Generation == dirty && (layer == nil || !layer.RowsPreloaded()) {
				t.Fatal("the dirty generation's rows are not preloaded; the test would not reach the cached rows")
			}
		}
		n := 0
		for _, e := range view.Reader.GetOutEdges(stackKeeperID) {
			if e.Kind == graph.EdgeAccessesField {
				n++
			}
		}
		return n
	}
	if n := accesses(); n != 0 {
		t.Fatalf("before the correction the view reads %d accesses_field edges", n)
	}

	correction, err := store.BeginDerivedCorrection(ctx, store_sqlite.DerivedCorrectionRequest{
		GenerationID: dirty, Pass: "capability", FromVersion: 0, ToVersion: 1,
		EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField},
	})
	if err != nil {
		t.Fatalf("BeginDerivedCorrection: %v", err)
	}
	if err := correction.ReplaceSourceEdges(ctx, []string{stackKeeperID}, []*graph.Edge{
		{From: stackKeeperID, To: stackNewID, Kind: graph.EdgeAccessesField, FilePath: stackKeepFile, Line: 6},
	}, nil); err != nil {
		t.Fatalf("ReplaceSourceEdges: %v", err)
	}
	if _, err := correction.Finish(ctx); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if _, err := materializer.WarmRoute(ctx, commit, dirty); err != nil {
		t.Fatalf("WarmRoute after the correction: %v", err)
	}
	if n := accesses(); n != 1 {
		t.Fatalf("after the correction the view reads %d accesses_field edges, want 1: served from a layer cached before it", n)
	}
	cache := materializer.layerCacheFor()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cached := 0
	for key := range cache.entries {
		if key.generation == dirty {
			cached++
			if key.correctionEpoch != store.GenerationCorrectionEpoch(dirty) {
				t.Fatalf("dirty generation cached at epoch %d, store is at %d", key.correctionEpoch, store.GenerationCorrectionEpoch(dirty))
			}
		}
	}
	if cached != 1 {
		t.Fatalf("dirty generation has %d cache entries, want 1", cached)
	}
}

// A row preload names the correction epoch it read before loading: rows
// loaded across a correction are not installed in the entry a later open is
// served from.
func TestRowPreloadInstallsOnlyAtItsCorrectionEpoch(t *testing.T) {
	ctx := context.Background()
	store := openStackStore(t, "layer-cache-preload-epoch")
	_, dirty := seedRoutedStack(t, store)
	cache := newGenerationLayerCache(8, 1_000_000)
	key := layerCacheKey{generation: dirty, correctionEpoch: 1}
	if _, err := cache.open(ctx, key, store.AtGeneration(dirty), NewGenerationLayerContext); err != nil {
		t.Fatalf("open: %v", err)
	}
	if cache.preloadRows(ctx, dirty, 0, store.AtGeneration(dirty)) {
		t.Fatal("rows read at an older epoch were installed in the newer epoch's entry")
	}
	if !cache.preloadRows(ctx, dirty, 1, store.AtGeneration(dirty)) {
		t.Fatal("rows at the entry's own epoch were not installed")
	}
}
