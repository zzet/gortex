package store_sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

func TestBeginPayloadGenerationWithStatusPreservesAdoptionAndLegacyAPI(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	req := PayloadGenerationRequest{
		OwnerKind:         "test",
		GraphID:           "graph",
		LayerID:           "layer",
		CheckoutID:        "checkout",
		GenerationKind:    "test",
		TreeOID:           "tree",
		ConfigHash:        "config",
		ExtractorVersions: "extractors",
		ResolverVersion:   "resolver",
		CreatedAt:         1,
	}
	firstID, firstHandle, adopted, err := store.BeginPayloadGenerationWithStatus(context.Background(), req)
	if err != nil {
		t.Fatalf("begin fresh generation: %v", err)
	}
	if adopted {
		t.Fatal("fresh generation reported as adopted")
	}
	if firstID <= 0 || firstHandle == nil || firstHandle.ViewGeneration() != firstID {
		t.Fatalf("fresh handle = (%d, %v), want generation-pinned handle", firstID, firstHandle)
	}

	secondID, secondHandle, adopted, err := store.BeginPayloadGenerationWithStatus(context.Background(), req)
	if err != nil {
		t.Fatalf("adopt generation: %v", err)
	}
	if !adopted {
		t.Fatal("matching building generation was not reported as adopted")
	}
	if secondID != firstID || secondHandle == nil || secondHandle.ViewGeneration() != firstID {
		t.Fatalf("adopted handle = (%d, %v), want generation %d", secondID, secondHandle, firstID)
	}

	legacyID, legacyHandle, err := store.BeginPayloadGeneration(context.Background(), req)
	if err != nil {
		t.Fatalf("legacy begin: %v", err)
	}
	if legacyID != firstID || legacyHandle == nil || legacyHandle.ViewGeneration() != firstID {
		t.Fatalf("legacy handle = (%d, %v), want generation %d", legacyID, legacyHandle, firstID)
	}
}
