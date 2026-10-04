package graphview

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// TestMaterializeCheckoutReusesGenerationMasksAcrossViews pins the per-request
// cost: a second view over the same published stack opens every generation
// from the cache, and the view it composes reads exactly what an uncached
// materialization reads.
func TestMaterializeCheckoutReusesGenerationMasksAcrossViews(t *testing.T) {
	ctx := context.Background()
	store := openStackStore(t, "layer-cache-reuse")
	commit, dirty := seedRoutedStack(t, store)
	flat := openStackStore(t, "layer-cache-flat")
	seedStackFlatCorpus(t, flat)
	materializer := newTestMaterializer(store)

	first, err := materializer.MaterializeCheckout(ctx, testCheckoutID)
	if err != nil {
		t.Fatalf("first MaterializeCheckout: %v", err)
	}
	first.Close()
	hits, misses, entries := materializer.LayerCacheStats()
	if hits != 0 || misses != 2 || entries != 2 {
		t.Fatalf("after the first view: hits=%d misses=%d entries=%d, want 0/2/2", hits, misses, entries)
	}

	second, err := materializer.MaterializeCheckout(ctx, testCheckoutID)
	if err != nil {
		t.Fatalf("second MaterializeCheckout: %v", err)
	}
	defer second.Close()
	hits, misses, _ = materializer.LayerCacheStats()
	if hits != 2 || misses != 2 {
		t.Fatalf("after the second view: hits=%d misses=%d, want 2/2 (no mask reads)", hits, misses)
	}
	if got := second.Generations(); len(got) != 2 || got[0] != commit || got[1] != dirty {
		t.Fatalf("Generations() = %v, want [%d %d]", got, commit, dirty)
	}
	assertReadersAgree(t, second.Reader, flat)

	// The cached layers are fresh objects over fresh handles: nothing the
	// first view memoized is visible to the second.
	for i, source := range second.GenerationSources() {
		layer, ok := source.Layer.(*GenerationLayer)
		if !ok {
			t.Fatalf("source %d layer is %T", i, source.Layer)
		}
		if layer.handle != source.Handle {
			t.Fatalf("source %d layer reads another handle than the view's", i)
		}
		if !layer.failureRepoScoped || layer.failureRepoPrefix != stackRepo {
			t.Fatalf("source %d layer lost its failure scope", i)
		}
	}
}

// TestMaterializeCheckoutCachedLayerAgreesWithUncached compares every
// ownership answer of a cache-served layer with a freshly read one.
func TestMaterializeCheckoutCachedLayerAgreesWithUncached(t *testing.T) {
	ctx := context.Background()
	store := openStackStore(t, "layer-cache-agree")
	_, dirty := seedRoutedStack(t, store)
	materializer := newTestMaterializer(store)
	for range 2 {
		view, err := materializer.MaterializeCheckout(ctx, testCheckoutID)
		if err != nil {
			t.Fatalf("MaterializeCheckout: %v", err)
		}
		view.Close()
	}
	view, err := materializer.MaterializeCheckout(ctx, testCheckoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	defer view.Close()
	var cached *GenerationLayer
	for _, source := range view.GenerationSources() {
		if source.Generation == dirty {
			cached = source.Layer.(*GenerationLayer)
		}
	}
	fresh, err := NewGenerationLayerContext(ctx, store.AtGeneration(dirty))
	if err != nil {
		t.Fatalf("NewGenerationLayerContext: %v", err)
	}
	if got, want := cached.FilePaths(), fresh.FilePaths(); !equalStrings(got, want) {
		t.Fatalf("FilePaths = %v, want %v", got, want)
	}
	if got, want := cached.ContextPaths(), fresh.ContextPaths(); !equalStrings(got, want) {
		t.Fatalf("ContextPaths = %v, want %v", got, want)
	}
	for _, path := range fresh.FilePaths() {
		if cached.HasFile(path) != fresh.HasFile(path) || cached.IsTombstone(path) != fresh.IsTombstone(path) {
			t.Fatalf("path %q answers differently", path)
		}
		for _, node := range fresh.FileNodes(path) {
			if cached.OwnsNodeIdentity(node.ID) != fresh.OwnsNodeIdentity(node.ID) ||
				cached.CoversNodeID(node.ID) != fresh.CoversNodeID(node.ID) {
				t.Fatalf("identity %q answers differently", node.ID)
			}
		}
	}
	if len(cached.removed) != len(fresh.removed) || len(cached.edgeSources) != len(fresh.edgeSources) ||
		len(cached.detachedNodes) != len(fresh.detachedNodes) {
		t.Fatal("cached mask set differs from a fresh read")
	}
}

// TestMaterializeCheckoutDropsMasksOfAnUnservableGeneration keeps the cache
// from ever outliving servability: a generation that is no longer servable is
// refused on the next open and its masks are forgotten.
func TestMaterializeCheckoutDropsMasksOfAnUnservableGeneration(t *testing.T) {
	ctx := context.Background()
	store := openStackStore(t, "layer-cache-unservable")
	_, dirty := seedRoutedStack(t, store)
	materializer := newTestMaterializer(store)
	view, err := materializer.MaterializeCheckout(ctx, testCheckoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	view.Close()
	setGenerationState(t, store, dirty, store_sqlite.ViewGenerationFailed, store_sqlite.ViewGenerationReady)

	if _, err := materializer.MaterializeCheckout(ctx, testCheckoutID); err == nil {
		t.Fatal("a view over a failed generation was served from cached masks")
	}
	// An open that does reach the generation drops its masks; so does an
	// explicit retirement hook.
	if _, _, _, err := materializer.openGeneration(ctx, dirty); err == nil {
		t.Fatal("openGeneration served a failed generation")
	}
	assertNotCached := func() {
		t.Helper()
		materializer.layerCache.mu.Lock()
		defer materializer.layerCache.mu.Unlock()
		for key := range materializer.layerCache.entries {
			if key.generation == dirty {
				t.Fatalf("masks of failed generation %d are still cached", dirty)
			}
		}
	}
	assertNotCached()
	materializer.ForgetGeneration(dirty)
	assertNotCached()
}

// TestGenerationLayerCacheSharesOneLoadAndRetriesFailures covers the
// concurrency contract: concurrent misses on one key run one load, and a
// failed load is not cached, so the next open loads again under its own
// context.
func TestGenerationLayerCacheSharesOneLoadAndRetriesFailures(t *testing.T) {
	store := openStackStore(t, "layer-cache-load")
	commit, _ := seedRoutedStack(t, store)
	cache := newGenerationLayerCache(8, 1_000_000)
	key := layerCacheKey{generation: commit}

	var loads atomic.Int32
	release := make(chan struct{})
	load := func(ctx context.Context, handle *store_sqlite.Store) (*GenerationLayer, error) {
		loads.Add(1)
		<-release
		return NewGenerationLayerContext(ctx, handle)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cache.open(context.Background(), key, store.AtGeneration(commit), load)
			errs <- err
		}()
	}
	for loads.Load() == 0 {
		runtime.Gosched() // wait until the first loader is inside load
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("open: %v", err)
		}
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("concurrent misses ran %d loads, want 1", got)
	}

	failing := layerCacheKey{generation: commit, createdAt: 1}
	boom := errors.New("boom")
	if _, err := cache.open(context.Background(), failing, store.AtGeneration(commit),
		func(context.Context, *store_sqlite.Store) (*GenerationLayer, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("failing load error = %v", err)
	}
	if _, err := cache.open(context.Background(), failing, store.AtGeneration(commit), NewGenerationLayerContext); err != nil {
		t.Fatalf("open after a failed load: %v", err)
	}
}

// TestGenerationLayerCacheEvictsByCountAndWeight bounds retention.
func TestGenerationLayerCacheEvictsByCountAndWeight(t *testing.T) {
	store := openStackStore(t, "layer-cache-evict")
	commit, dirty := seedRoutedStack(t, store)
	cache := newGenerationLayerCache(1, 1_000_000)
	for _, generation := range []int64{commit, dirty} {
		if _, err := cache.open(context.Background(), layerCacheKey{generation: generation},
			store.AtGeneration(generation), NewGenerationLayerContext); err != nil {
			t.Fatalf("open %d: %v", generation, err)
		}
	}
	if _, _, entries := cache.stats(); entries != 1 {
		t.Fatalf("count-bounded cache retains %d entries, want 1", entries)
	}
	if _, ok := cache.entries[layerCacheKey{generation: dirty}]; !ok {
		t.Fatal("the most recently used entry was evicted")
	}
	tiny := newGenerationLayerCache(8, 0)
	if _, err := tiny.open(context.Background(), layerCacheKey{generation: dirty},
		store.AtGeneration(dirty), NewGenerationLayerContext); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, entries := tiny.stats(); entries != 0 || tiny.weight != 0 {
		t.Fatalf("weight-bounded cache retains %d entries (weight %d), want none", entries, tiny.weight)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
