package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestOpenV19CheckoutRootMoveJournalAlreadyPresentIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("create current store: %v", err)
	}
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "family-existing-journal", "checkout-existing-journal", "inc-existing-journal")
	if err := catalog.UpsertTrackingIntent(ctx, TrackingIntent{
		IntentID:      "intent-existing-journal",
		CheckoutID:    "checkout-existing-journal",
		SourceKind:    IntentSourceCLITrack,
		SourceLocator: "/tmp/checkout-existing-journal",
		Active:        true,
	}); err != nil {
		t.Fatalf("seed tracking intent: %v", err)
	}
	checkout, found, err := catalog.GetCheckout(ctx, "checkout-existing-journal")
	if err != nil || !found {
		t.Fatalf("read seeded checkout: found %t, err %v", found, err)
	}
	if err := catalog.UpdateCheckoutObservation(
		ctx,
		checkoutMoveObservation(checkout, "/tmp/checkout-existing-journal-moved"),
	); err != nil {
		t.Fatalf("publish root move: %v", err)
	}
	wantMove, found, err := catalog.GetCheckoutRootMove(ctx, checkout.CheckoutID)
	if err != nil || !found {
		t.Fatalf("read seeded root-move journal: found %t, err %v", found, err)
	}
	wantSchema := sqliteSchemaObject(t, store.writerDB, "table", "checkout_root_moves")
	if err := store.Close(); err != nil {
		t.Fatalf("close current store: %v", err)
	}

	// Simulate a feature-lineage v19 store that already received the additive
	// table before the schema-version bump. The migration must preserve both
	// the table's row and the catalog rows it protects.
	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`PRAGMA user_version = 19`); err != nil {
			t.Fatalf("stamp v19 fixture: %v", err)
		}
	})

	var firstEvents []MigrationProgress
	migrated, err := Open(path, WithMigrationObserver(func(progress MigrationProgress) {
		firstEvents = append(firstEvents, progress)
	}))
	if err != nil {
		t.Fatalf("first Open of v19 fixture: %v", err)
	}
	assertCheckoutRootMoveMigrationFixture(t, migrated, wantMove, wantSchema)
	if len(firstEvents) != 4 ||
		firstEvents[0].Version != 20 || firstEvents[0].Phase != MigrationStarted ||
		firstEvents[1].Version != 20 || firstEvents[1].Phase != MigrationFinished ||
		firstEvents[2].Version != 21 || firstEvents[2].Phase != MigrationStarted ||
		firstEvents[3].Version != 21 || firstEvents[3].Phase != MigrationFinished {
		t.Fatalf("first Open migration events = %+v, want v20 then v21 started/finished", firstEvents)
	}
	if err := migrated.Close(); err != nil {
		t.Fatalf("close migrated store: %v", err)
	}

	var secondEvents []MigrationProgress
	reopened, err := Open(path, WithMigrationObserver(func(progress MigrationProgress) {
		secondEvents = append(secondEvents, progress)
	}))
	if err != nil {
		t.Fatalf("second Open of migrated fixture: %v", err)
	}
	defer reopened.Close()
	assertCheckoutRootMoveMigrationFixture(t, reopened, wantMove, wantSchema)
	if len(secondEvents) != 0 {
		t.Fatalf("second Open reran an already-complete migration: %+v", secondEvents)
	}
}

func TestCheckoutRootMoveJournalCascadesWithCheckoutDeletion(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "family-cascade", "checkout-cascade", "inc-cascade")
	checkout, found, err := catalog.GetCheckout(ctx, "checkout-cascade")
	if err != nil || !found {
		t.Fatalf("read seeded checkout: found %t, err %v", found, err)
	}
	if err := catalog.UpdateCheckoutObservation(
		ctx,
		checkoutMoveObservation(checkout, "/tmp/checkout-cascade-moved"),
	); err != nil {
		t.Fatalf("publish root move: %v", err)
	}
	if _, found, err := catalog.GetCheckoutRootMove(ctx, checkout.CheckoutID); err != nil || !found {
		t.Fatalf("journal before checkout deletion: found %t, err %v", found, err)
	}

	if err := catalog.DeleteCheckout(ctx, checkout.CheckoutID); err != nil {
		t.Fatalf("delete checkout: %v", err)
	}
	if _, found, err := catalog.GetCheckoutRootMove(ctx, checkout.CheckoutID); err != nil || found {
		t.Fatalf("journal after checkout deletion: found %t, err %v", found, err)
	}
	var journalRows int
	if err := store.writerDB.QueryRow(
		`SELECT COUNT(*) FROM checkout_root_moves WHERE checkout_id = ?`, checkout.CheckoutID,
	).Scan(&journalRows); err != nil {
		t.Fatalf("count journal after checkout deletion: %v", err)
	}
	if journalRows != 0 {
		t.Fatalf("journal rows after checkout deletion = %d, want 0", journalRows)
	}
}

func TestOpenV19CheckoutRootMoveMigrationFailureRetriesWithoutCatalogLoss(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("create current store: %v", err)
	}
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "family-retry", "checkout-retry", "inc-retry")
	if err := catalog.UpsertTrackingIntent(ctx, TrackingIntent{
		IntentID:      "intent-retry",
		CheckoutID:    "checkout-retry",
		SourceKind:    IntentSourceProjectMembership,
		SourceLocator: "project:retry",
		Active:        true,
	}); err != nil {
		t.Fatalf("seed tracking intent: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close current store: %v", err)
	}
	downgradeCheckoutRootMoveFixture(t, path)

	injected := errors.New("injected checkout-root-move migration failure")
	failingV20 := schemaMigration{
		version: 20,
		name:    "add checkout root move recovery journal",
		inPlace: func(tx *sql.Tx) error {
			if err := createCheckoutRootMoveJournal(tx); err != nil {
				return err
			}
			return injected
		},
	}
	var failureEvents []MigrationProgress
	failed, err := openWithObserver(
		path,
		20,
		[]schemaMigration{failingV20},
		false,
		func(progress MigrationProgress) { failureEvents = append(failureEvents, progress) },
	)
	if failed != nil {
		_ = failed.Close()
		t.Fatal("failing migration returned a usable store")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("failing migration error = %v, want injected error", err)
	}
	if len(failureEvents) != 2 ||
		failureEvents[0].Version != 20 || failureEvents[0].Phase != MigrationStarted ||
		failureEvents[1].Version != 20 || failureEvents[1].Phase != MigrationFailed ||
		!errors.Is(failureEvents[1].Error, injected) {
		t.Fatalf("failure migration events = %+v, want v20 started/failed", failureEvents)
	}

	// schemaSQL may create the additive table before the injected failure, but
	// user_version must remain the old durable completion marker. Both catalog
	// rows must also remain available for the retry.
	withRawDB(t, path, func(db *sql.DB) {
		if version, err := readUserVersion(db); err != nil || version != 19 {
			t.Fatalf("version after failed migration = %d, err %v; want 19", version, err)
		}
		for table, want := range map[string]int{
			"checkouts":        1,
			"tracking_intents": 1,
		} {
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
				t.Fatalf("count %s after failed migration: %v", table, err)
			}
			if count != want {
				t.Fatalf("%s rows after failed migration = %d, want %d", table, count, want)
			}
		}
	})

	retried, err := Open(path)
	if err != nil {
		t.Fatalf("retry Open after injected failure: %v", err)
	}
	defer retried.Close()
	if version, err := readUserVersion(retried.writerDB); err != nil || version != currentSchemaVersion {
		t.Fatalf("version after retry = %d, err %v; want %d", version, err, currentSchemaVersion)
	}
	checkout, found, err := retried.Catalog().GetCheckout(ctx, "checkout-retry")
	if err != nil || !found || checkout.Incarnation != "inc-retry" {
		t.Fatalf("checkout after retry = found %t, checkout %+v, err %v", found, checkout, err)
	}
	intents, err := retried.Catalog().ListTrackingIntents(ctx, checkout.CheckoutID)
	if err != nil {
		t.Fatalf("list intents after retry: %v", err)
	}
	if len(intents) != 1 || intents[0].IntentID != "intent-retry" ||
		intents[0].SourceKind != IntentSourceProjectMembership ||
		intents[0].SourceLocator != "project:retry" || !intents[0].Active {
		t.Fatalf("tracking intents after retry = %+v", intents)
	}
}

func assertCheckoutRootMoveMigrationFixture(
	t *testing.T,
	store *Store,
	wantMove CheckoutRootMove,
	wantSchema string,
) {
	t.Helper()
	ctx := context.Background()
	if version, err := readUserVersion(store.writerDB); err != nil || version != currentSchemaVersion {
		t.Fatalf("user_version = %d, err %v; want %d", version, err, currentSchemaVersion)
	}
	if got := sqliteSchemaObject(t, store.writerDB, "table", "checkout_root_moves"); got != wantSchema {
		t.Fatalf("checkout_root_moves schema changed across migration:\nwant: %s\ngot:  %s", wantSchema, got)
	}
	checkout, found, err := store.Catalog().GetCheckout(ctx, wantMove.CheckoutID)
	if err != nil || !found {
		t.Fatalf("checkout after migration: found %t, err %v", found, err)
	}
	if checkout.Incarnation != wantMove.Incarnation || checkout.RootPath != wantMove.CurrentRootPath {
		t.Fatalf("checkout after migration = %+v, want incarnation %q root %q",
			checkout, wantMove.Incarnation, wantMove.CurrentRootPath)
	}
	intents, err := store.Catalog().ListTrackingIntents(ctx, checkout.CheckoutID)
	if err != nil {
		t.Fatalf("list intents after migration: %v", err)
	}
	if len(intents) != 1 || intents[0].IntentID != "intent-existing-journal" ||
		intents[0].SourceKind != IntentSourceCLITrack ||
		intents[0].SourceLocator != "/tmp/checkout-existing-journal" || !intents[0].Active {
		t.Fatalf("tracking intents after migration = %+v", intents)
	}
	move, found, err := store.Catalog().GetCheckoutRootMove(ctx, checkout.CheckoutID)
	if err != nil || !found {
		t.Fatalf("root-move journal after migration: found %t, err %v", found, err)
	}
	if move != wantMove {
		t.Fatalf("root-move journal after migration = %+v, want %+v", move, wantMove)
	}
}
