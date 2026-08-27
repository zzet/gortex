package store_sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestReadyGenerationCacheSchemaInitRetriesAfterCancellation(t *testing.T) {
	store := openReadyGenerationCacheLifecycleStore(t, filepath.Join(t.TempDir(), "graph.sqlite"))
	defer func() { _ = store.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ensureReadyGenerationCacheSchema(ctx, store.storeCore); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled first schema initialization error = %v, want context.Canceled", err)
	}
	if err := ensureReadyGenerationCacheSchema(context.Background(), store.storeCore); err != nil {
		t.Fatalf("retry schema initialization: %v", err)
	}
}

func TestReadyGenerationCacheSchemaStateDiesWithStore(t *testing.T) {
	for i := 0; i < 32; i++ {
		store := openReadyGenerationCacheLifecycleStore(t, filepath.Join(t.TempDir(), "graph.sqlite"))
		if err := ensureReadyGenerationCacheSchema(context.Background(), store.storeCore); err != nil {
			t.Fatalf("initialize store %d: %v", i, err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("close store %d: %v", i, err)
		}
	}
}

func TestReadyGenerationCacheLeaseSurvivesStoreReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	store := openReadyGenerationCacheLifecycleStore(t, path)
	key := readyGenerationCacheLifecycleKey()
	generationID := createReadyCacheGeneration(t, store.Catalog(), key, "checkout", "layer", "check", "commit")
	claim, _, err := store.Catalog().ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{Key: key, LeaseToken: "durable-lease"})
	if err != nil {
		t.Fatalf("claim ready generation: %v", err)
	}
	if claim.WinnerGenerationID != generationID {
		t.Fatalf("winner = %d, want %d", claim.WinnerGenerationID, generationID)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	reopened := openReadyGenerationCacheLifecycleStore(t, path)
	defer func() { _ = reopened.Close() }()
	if _, err := reopened.writerDB.ExecContext(ctx, `DELETE FROM view_generations WHERE generation_id = ?`, generationID); err == nil {
		t.Fatal("live durable lease did not block deletion after reopen")
	}
	if err := reopened.Catalog().ReleaseReadyGenerationLease(ctx, "durable-lease"); err != nil {
		t.Fatalf("release durable lease after reopen: %v", err)
	}
	if _, err := reopened.writerDB.ExecContext(ctx, `DELETE FROM view_generations WHERE generation_id = ?`, generationID); err != nil {
		t.Fatalf("delete generation after release: %v", err)
	}
}

func TestReadyGenerationCacheClaimPrunesExpiredLease(t *testing.T) {
	ctx := context.Background()
	store := openReadyGenerationCacheLifecycleStore(t, filepath.Join(t.TempDir(), "graph.sqlite"))
	defer func() { _ = store.Close() }()
	key := readyGenerationCacheLifecycleKey()
	generationID := createReadyCacheGeneration(t, store.Catalog(), key, "checkout", "layer", "check", "commit")
	if _, _, err := store.Catalog().ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{Key: key, LeaseToken: "expired-lease"}); err != nil {
		t.Fatalf("claim expiring lease: %v", err)
	}
	if _, err := store.writerDB.ExecContext(ctx, `UPDATE ready_generation_leases SET expires_at = unixepoch() - 1 WHERE lease_token = ?`, "expired-lease"); err != nil {
		t.Fatalf("expire lease using database clock: %v", err)
	}
	claim, _, err := store.Catalog().ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{Key: key, LeaseToken: "replacement-lease"})
	if err != nil {
		t.Fatalf("claim after expiry: %v", err)
	}
	if claim.WinnerGenerationID != generationID {
		t.Fatalf("winner after expiry = %d, want %d", claim.WinnerGenerationID, generationID)
	}
	var expiredRows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ready_generation_leases WHERE lease_token = ?`, "expired-lease").Scan(&expiredRows); err != nil {
		t.Fatalf("count expired lease rows: %v", err)
	}
	if expiredRows != 0 {
		t.Fatalf("expired lease rows = %d, want 0 after next claim", expiredRows)
	}
}

func openReadyGenerationCacheLifecycleStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store
}

func readyGenerationCacheLifecycleKey() ReadyGenerationCacheKey {
	return ReadyGenerationCacheKey{
		GraphID:              "lifecycle-graph",
		BaseGenerationID:     0,
		TreeOID:              "lifecycle-tree",
		IndexConfigHash:      "lifecycle-config",
		ExtractorFingerprint: "lifecycle-extractors",
		SchemaPipelineEpoch:  "lifecycle-epoch",
	}
}
