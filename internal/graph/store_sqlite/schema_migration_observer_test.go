package store_sqlite

import (
	"database/sql"
	"errors"
	"testing"
)

func TestApplyInPlaceMigrationsObserverOrdering(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	steps := []schemaMigration{
		{version: 17, name: "generation_indexes", inPlace: func(tx *sql.Tx) error { _, err := tx.Exec(`CREATE TABLE first_step (id INTEGER)`); return err }},
		{version: 18, name: "mask_tables", inPlace: func(tx *sql.Tx) error { _, err := tx.Exec(`CREATE TABLE second_step (id INTEGER)`); return err }},
	}
	var events []MigrationProgress
	if err := applyInPlaceMigrations(db, steps, func(p MigrationProgress) { events = append(events, p) }); err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("events=%d: %+v", len(events), events)
	}
	wantVersion := []int{17, 17, 18, 18}
	wantPhase := []MigrationPhase{MigrationStarted, MigrationFinished, MigrationStarted, MigrationFinished}
	for i := range events {
		if events[i].Version != wantVersion[i] || events[i].Phase != wantPhase[i] || events[i].Name != steps[i/2].name {
			t.Fatalf("event %d: %+v", i, events[i])
		}
		if events[i].Phase == MigrationFinished && events[i].Elapsed < 0 {
			t.Fatalf("negative elapsed: %+v", events[i])
		}
	}
}

func TestApplyInPlaceMigrationsObserverFailureAndRollback(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sentinel := errors.New("boom")
	steps := []schemaMigration{
		{version: 17, name: "first", inPlace: func(tx *sql.Tx) error { _, err := tx.Exec(`CREATE TABLE rolled_back (id INTEGER)`); return err }},
		{version: 18, name: "failing", inPlace: func(*sql.Tx) error { return sentinel }},
	}
	var events []MigrationProgress
	err = applyInPlaceMigrations(db, steps, func(p MigrationProgress) { events = append(events, p) })
	if !errors.Is(err, sentinel) {
		t.Fatalf("error=%v", err)
	}
	if len(events) != 4 || events[3].Phase != MigrationFailed || events[3].Version != 18 || !errors.Is(events[3].Error, sentinel) {
		t.Fatalf("events: %+v", events)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='rolled_back'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("migration transaction was not rolled back")
	}
}
