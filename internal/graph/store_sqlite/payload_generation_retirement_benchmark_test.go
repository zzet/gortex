package store_sqlite

import (
	"context"
	"strconv"
	"testing"
)

func BenchmarkBeginPayloadGenerationRetirement(b *testing.B) {
	store := openCatalogStore(b)
	catalog := store.Catalog()
	ctx := context.Background()
	key := readyGenerationCacheLifecycleKey()
	generationID := createReadyCacheGeneration(
		b, catalog, key, "checkout-layer", "retirement-benchmark",
		"commit:"+strconv.FormatInt(int64(b.N), 10), "",
	)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		b.StopTimer()
		if err := catalog.SetViewGenerationState(ctx, generationID, ViewGenerationReady); err != nil {
			b.Fatalf("reset ready state: %v", err)
		}
		b.StartTimer()
		if err := catalog.beginPayloadGenerationRetirement(ctx, generationID); err != nil {
			b.Fatalf("begin retirement: %v", err)
		}
	}
}
