package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestSchemaV20CheckoutRootMoveJournalMigrationParityAndDurability(t *testing.T) {
	ctx := context.Background()
	freshPath := filepath.Join(t.TempDir(), "fresh.sqlite")
	fresh, err := Open(freshPath)
	if err != nil {
		t.Fatal(err)
	}
	freshSQL := sqliteSchemaObject(t, fresh.writerDB, "table", "checkout_root_moves")
	if freshSQL == "" {
		t.Fatal("fresh store did not create checkout_root_moves")
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}

	migratedPath := filepath.Join(t.TempDir(), "migrated.sqlite")
	legacy, err := Open(migratedPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog := legacy.Catalog()
	seedFamilyAndCheckout(t, catalog, "family-v19", "checkout-v19", "inc-v19")
	if err := catalog.UpsertTrackingIntent(ctx, TrackingIntent{
		IntentID:      "intent-v19",
		CheckoutID:    "checkout-v19",
		SourceKind:    IntentSourceCLITrack,
		SourceLocator: "/tmp/checkout-v19",
		Active:        true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	downgradeCheckoutRootMoveFixture(t, migratedPath)

	migrated, err := Open(migratedPath)
	if err != nil {
		t.Fatalf("Open v19 fixture: %v", err)
	}
	if got := sqliteSchemaObject(t, migrated.writerDB, "table", "checkout_root_moves"); got != freshSQL {
		t.Fatalf("fresh/migrated checkout_root_moves differ:\nfresh: %s\nmigrated: %s", freshSQL, got)
	}
	if version, err := readUserVersion(migrated.writerDB); err != nil || version != currentSchemaVersion {
		t.Fatalf("migrated user_version = %d, err %v", version, err)
	}
	checkout, found, err := migrated.Catalog().GetCheckout(ctx, "checkout-v19")
	if err != nil || !found {
		t.Fatalf("populated checkout survived = found %t, err %v", found, err)
	}
	if err := migrated.Catalog().UpdateCheckoutObservation(
		ctx, checkoutMoveObservation(checkout, "/tmp/checkout-v19-moved"),
	); err != nil {
		t.Fatal(err)
	}
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(migratedPath)
	if err != nil {
		t.Fatal(err)
	}
	move, found, err := reopened.Catalog().GetCheckoutRootMove(ctx, "checkout-v19")
	if err != nil || !found {
		t.Fatalf("journal after reopen = found %t, err %v", found, err)
	}
	if err := reopened.Catalog().CompleteCheckoutRootMove(ctx, move); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	completed, err := Open(migratedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer completed.Close()
	if _, found, err := completed.Catalog().GetCheckoutRootMove(ctx, "checkout-v19"); err != nil || found {
		t.Fatalf("completed journal after reopen = found %t, err %v", found, err)
	}
	if version, err := readUserVersion(completed.writerDB); err != nil || version != currentSchemaVersion {
		t.Fatalf("durable user_version = %d, err %v", version, err)
	}
}

func sqliteSchemaObject(t testing.TB, db *sql.DB, kind, name string) string {
	t.Helper()
	var definition string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = ? AND name = ?`, kind, name,
	).Scan(&definition); err != nil {
		t.Fatalf("read schema object %s %s: %v", kind, name, err)
	}
	return definition
}

func downgradeCheckoutRootMoveFixture(t testing.TB, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE checkout_root_moves; PRAGMA user_version = 19`); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkOpenV19CheckoutRootMoveJournal1024(b *testing.B) {
	dir := b.TempDir()
	templatePath := filepath.Join(dir, "template.sqlite")
	store, err := Open(templatePath)
	if err != nil {
		b.Fatal(err)
	}
	tx, err := store.writerDB.Begin()
	if err != nil {
		b.Fatal(err)
	}
	if _, err := tx.Exec(`
INSERT INTO repository_families
  (family_id, common_dir_identity, state, created_at, last_seen)
VALUES ('benchmark-family', '/tmp/benchmark-family.git', 'ready', 1, 1)`); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 1024; i++ {
		checkoutID := fmt.Sprintf("checkout-%04d", i)
		root := fmt.Sprintf("/tmp/worktree-%04d", i)
		if _, err := tx.Exec(`
INSERT INTO checkouts
  (checkout_id, incarnation, family_id, root_path, git_dir, admin_name,
   state, desired_mode, effective_mode, last_seen)
VALUES (?, ?, 'benchmark-family', ?, ?, ?, 'checkout_ready', 'automatic', 'automatic', 1)`,
			checkoutID, "inc-"+checkoutID, root, root+"/.git", checkoutID); err != nil {
			b.Fatal(err)
		}
		for source := 0; source < 3; source++ {
			if _, err := tx.Exec(`
INSERT INTO tracking_intents
  (intent_id, checkout_id, source_kind, source_locator, active)
VALUES (?, ?, 'cli_track', ?, 1)`,
				fmt.Sprintf("intent-%04d-%d", i, source), checkoutID,
				fmt.Sprintf("%s/source-%d", root, source)); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	if err := store.Close(); err != nil {
		b.Fatal(err)
	}
	downgradeCheckoutRootMoveFixture(b, templatePath)
	template, err := os.ReadFile(templatePath)
	if err != nil {
		b.Fatal(err)
	}

	b.ReportMetric(1024, "checkouts")
	b.ReportMetric(3072, "intents")
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
		if err := migrated.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
