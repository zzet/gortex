package store_sqlite

import (
	"context"
	"testing"
)

func TestRetirePayloadGenerationStillCollectsCrashedBuildingGeneration(t *testing.T) {
	store := openCatalogStore(t)
	catalog := store.Catalog()
	ctx := context.Background()
	key := readyGenerationCacheLifecycleKey()
	generationID := createReadyCacheGeneration(
		t, catalog, key, "checkout-layer", "crashed-builder", "commit:crashed-builder", "",
	)
	if err := catalog.SetViewGenerationState(ctx, generationID, ViewGenerationBuilding); err != nil {
		t.Fatalf("restore building state: %v", err)
	}

	if err := store.RetirePayloadGeneration(ctx, generationID, nil); err != nil {
		t.Fatalf("retire crashed building generation: %v", err)
	}
	if row, found, err := catalog.GetViewGeneration(ctx, generationID); err != nil || found {
		t.Fatalf("building generation survived retirement: found=%v err=%v row=%+v", found, err, row)
	}
}
