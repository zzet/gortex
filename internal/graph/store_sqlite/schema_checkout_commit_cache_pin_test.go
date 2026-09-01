package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func downgradeCheckoutCommitCacheFixture(t testing.TB, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
DROP TABLE checkout_commit_cache_pins;
DROP TABLE checkout_commit_cache_retirements;
PRAGMA user_version = 20`); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaV21CheckoutCommitCachePinsMigrateInPlaceAndBackfill(t *testing.T) {
	ctx := context.Background()
	freshPath := filepath.Join(t.TempDir(), "fresh.sqlite")
	fresh, err := Open(freshPath)
	if err != nil {
		t.Fatal(err)
	}
	freshTable := sqliteSchemaObject(t, fresh.writerDB, "table", "checkout_commit_cache_pins")
	freshGenerationIndex := sqliteSchemaObject(
		t, fresh.writerDB, "index", "checkout_commit_cache_pins_by_generation",
	)
	freshRecencyIndex := sqliteSchemaObject(
		t, fresh.writerDB, "index", "checkout_commit_cache_pins_by_graph_recency",
	)
	freshRetirementQueue := sqliteSchemaObject(
		t, fresh.writerDB, "table", "checkout_commit_cache_retirements",
	)
	freshRetirementTrigger := sqliteSchemaObject(
		t, fresh.writerDB, "trigger", "checkout_commit_cache_pin_retirement",
	)
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}

	migratedPath := filepath.Join(t.TempDir(), "migrated.sqlite")
	store, err := Open(migratedPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog := store.Catalog()
	const checkoutID = "cache-migration-checkout"
	seedFamilyAndCheckout(t, catalog, "cache-migration-family", checkoutID, "cache-migration-incarnation")
	firstKey := readyCacheTestKey("cache-migration-graph", 0)
	firstKey.TreeOID = "cache-migration-tree-first"
	firstID := createReadyCacheGeneration(
		t, catalog, firstKey, "checkout", checkoutID,
		"cache-migration-layer-first", "cache-migration-commit-first",
	)
	secondKey := firstKey
	secondKey.TreeOID = "cache-migration-tree-second"
	secondID := createReadyCacheGeneration(
		t, catalog, secondKey, "checkout", checkoutID,
		"cache-migration-layer-second", "cache-migration-commit-second",
	)
	if err := catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
		CheckoutID: checkoutID, GraphID: firstKey.GraphID,
		CommitGenerationID: secondID, State: RouteActive,
	}); err != nil {
		t.Fatal(err)
	}
	store.AddBatch([]*graph.Node{{
		ID: "cache-migration-sentinel", Kind: graph.KindFunction, Name: "MigrationSentinel",
	}}, nil)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	downgradeCheckoutCommitCacheFixture(t, migratedPath)

	var events []MigrationProgress
	migrated, err := Open(migratedPath, WithMigrationObserver(func(progress MigrationProgress) {
		events = append(events, progress)
	}))
	if err != nil {
		t.Fatalf("open v20 fixture: %v", err)
	}
	defer migrated.Close()
	if migrated.NeedsRebuild() {
		t.Fatal("additive v21 checkout commit cache migration requested a source rebuild")
	}
	if migrated.NodeCount() != 1 {
		t.Fatalf("node count after additive migration=%d, want sentinel preserved", migrated.NodeCount())
	}
	if version, err := readUserVersion(migrated.writerDB); err != nil || version != 21 {
		t.Fatalf("user_version after migration=%d err=%v, want 21", version, err)
	}
	if len(events) != 2 || events[0].Version != 21 || events[0].Phase != MigrationStarted ||
		events[1].Version != 21 || events[1].Phase != MigrationFinished {
		t.Fatalf("v21 migration events=%+v, want started/finished only", events)
	}
	if got := sqliteSchemaObject(t, migrated.writerDB, "table", "checkout_commit_cache_pins"); got != freshTable {
		t.Fatalf("fresh/migrated pin table differ:\nfresh: %s\nmigrated: %s", freshTable, got)
	}
	if got := sqliteSchemaObject(t, migrated.writerDB, "index", "checkout_commit_cache_pins_by_generation"); got != freshGenerationIndex {
		t.Fatalf("fresh/migrated generation index differ:\nfresh: %s\nmigrated: %s", freshGenerationIndex, got)
	}
	if got := sqliteSchemaObject(t, migrated.writerDB, "index", "checkout_commit_cache_pins_by_graph_recency"); got != freshRecencyIndex {
		t.Fatalf("fresh/migrated recency index differ:\nfresh: %s\nmigrated: %s", freshRecencyIndex, got)
	}
	if got := sqliteSchemaObject(t, migrated.writerDB, "table", "checkout_commit_cache_retirements"); got != freshRetirementQueue {
		t.Fatalf("fresh/migrated retirement queue differ:\nfresh: %s\nmigrated: %s", freshRetirementQueue, got)
	}
	if got := sqliteSchemaObject(t, migrated.writerDB, "trigger", "checkout_commit_cache_pin_retirement"); got != freshRetirementTrigger {
		t.Fatalf("fresh/migrated retirement trigger differ:\nfresh: %s\nmigrated: %s", freshRetirementTrigger, got)
	}
	pins, err := migrated.Catalog().ListCheckoutCommitCachePins(ctx, firstKey.GraphID)
	if err != nil {
		t.Fatal(err)
	}
	ids := checkoutCommitCacheTestPinIDs(pins)
	if len(pins) != 2 || ids[firstID] != 1 || ids[secondID] != 1 {
		t.Fatalf("migration backfill pins=%+v, want owned generations %d and %d", pins, firstID, secondID)
	}
}

func BenchmarkOpenV20CheckoutCommitCachePins10000(b *testing.B) {
	benchmarkOpenV20CheckoutCommitCachePins(b, 10_000)
}

func BenchmarkOpenV20CheckoutCommitCachePins100000(b *testing.B) {
	benchmarkOpenV20CheckoutCommitCachePins(b, 100_000)
}

func benchmarkOpenV20CheckoutCommitCachePins(b *testing.B, generations int) {
	dir := b.TempDir()
	templatePath := filepath.Join(dir, "template.sqlite")
	store, err := Open(templatePath)
	if err != nil {
		b.Fatal(err)
	}
	catalog := store.Catalog()
	const (
		checkouts = 100
		graphID   = "cache-migration-bench-graph"
	)
	for checkoutIndex := 0; checkoutIndex < checkouts; checkoutIndex++ {
		checkoutID := fmt.Sprintf("cache-migration-bench-%04d", checkoutIndex)
		seedFamilyAndCheckout(b, catalog, "cache-migration-bench-family", checkoutID,
			"cache-migration-bench-incarnation-"+checkoutID)
	}
	ctx := context.Background()
	err = catalog.withTx(ctx, func(tx *sql.Tx) error {
		insert, err := tx.PrepareContext(ctx, insertViewGenerationSQL)
		if err != nil {
			return err
		}
		defer insert.Close()
		routed := make([]int64, checkouts)
		for generationIndex := 0; generationIndex < generations; generationIndex++ {
			checkoutIndex := generationIndex % checkouts
			checkoutID := fmt.Sprintf("cache-migration-bench-%04d", checkoutIndex)
			result, err := insert.ExecContext(ctx, viewGenerationInsertArgs(ViewGeneration{
				OwnerKind:         "checkout",
				GraphID:           graphID,
				LayerID:           fmt.Sprintf("cache-migration-bench-layer-%05d", generationIndex),
				CheckoutID:        checkoutID,
				GenerationKind:    "commit",
				TreeOID:           fmt.Sprintf("cache-migration-bench-tree-%05d", generationIndex),
				ConfigHash:        "cache-migration-bench-config",
				ExtractorVersions: "cache-migration-bench-extractors",
				ResolverVersion:   "cache-migration-bench-pipeline",
				State:             ViewGenerationReady,
				CreatedAt:         int64(generationIndex + 1),
				PublishedAt:       int64(generationIndex + 1),
				LastSelected:      int64(generationIndex + 1),
			})...)
			if err != nil {
				return err
			}
			generationID, err := result.LastInsertId()
			if err != nil {
				return err
			}
			routed[checkoutIndex] = generationID
		}
		for checkoutIndex, generationID := range routed {
			checkoutID := fmt.Sprintf("cache-migration-bench-%04d", checkoutIndex)
			if _, err := tx.ExecContext(ctx, `
INSERT INTO checkout_routes(
    checkout_id, graph_id, commit_generation_id, route_epoch, state
) VALUES (?, ?, ?, 0, ?)`, checkoutID, graphID, generationID, string(RouteActive)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	if err := store.Close(); err != nil {
		b.Fatal(err)
	}
	downgradeCheckoutCommitCacheFixture(b, templatePath)
	template, err := os.ReadFile(templatePath)
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		path := filepath.Join(dir, fmt.Sprintf("iteration-%d.sqlite", i))
		if err := os.WriteFile(path, template, 0o600); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		migrated, err := Open(path)
		if err != nil {
			b.Fatal(err)
		}
		if migrated.NeedsRebuild() {
			b.Fatal("v20 pin migration unexpectedly requested rebuild")
		}
		var walBytes int64
		if info, statErr := os.Stat(path + "-wal"); statErr == nil {
			walBytes = info.Size()
		} else if !os.IsNotExist(statErr) {
			b.Fatal(statErr)
		}
		if info, statErr := os.Stat(path); statErr == nil {
			b.ReportMetric(float64(info.Size()), "db_bytes/op")
		} else {
			b.Fatal(statErr)
		}
		b.ReportMetric(float64(walBytes), "wal_bytes/op")
		if err := migrated.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(generations), "generations/op")
}
