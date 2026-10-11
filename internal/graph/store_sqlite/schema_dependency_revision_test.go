package store_sqlite

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// This fixture tests the additive column transaction itself, not full Open or
// schema-version routing. The real full-schema upgrade test is a separate gate.
func newDependencyMigrationDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "migration.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE view_generations(generation_id INTEGER PRIMARY KEY, payload_marker TEXT NOT NULL)`,
		`CREATE TABLE dedicated_base_publications(graph_id TEXT PRIMARY KEY, attempt_state TEXT NOT NULL)`,
		`INSERT INTO view_generations VALUES(7,'preserve-generation')`,
		`INSERT INTO dedicated_base_publications VALUES('graph','building')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestDependencyRevisionMigrationDefaultsAndIdempotence(t *testing.T) {
	db := newDependencyMigrationDB(t)
	for range 2 {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := addDependencyRevisionColumns(tx); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	var generationMarker, generationRevision, state, desiredRevision string
	if err := db.QueryRow(`SELECT payload_marker,dependency_revision FROM view_generations WHERE generation_id=7`).Scan(&generationMarker, &generationRevision); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT attempt_state,dependency_revision FROM dedicated_base_publications WHERE graph_id='graph'`).Scan(&state, &desiredRevision); err != nil {
		t.Fatal(err)
	}
	if generationMarker != "preserve-generation" || state != "building" || generationRevision != "" || desiredRevision != "" {
		t.Fatalf("migration changed prior data or certified legacy output: %q %q %q %q", generationMarker, state, generationRevision, desiredRevision)
	}
	for _, stmt := range []string{
		`UPDATE view_generations SET dependency_revision='cohort-v1:retained' WHERE generation_id=7`,
		`UPDATE dedicated_base_publications SET dependency_revision='cohort-v1:retained' WHERE graph_id='graph'`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := addDependencyRevisionColumns(tx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT dependency_revision FROM view_generations WHERE generation_id=7`).Scan(&generationRevision); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT dependency_revision FROM dedicated_base_publications WHERE graph_id='graph'`).Scan(&desiredRevision); err != nil {
		t.Fatal(err)
	}
	if generationRevision != "cohort-v1:retained" || desiredRevision != generationRevision {
		t.Fatal("idempotent migration overwrote existing revisions")
	}
}

func TestDependencyRevisionMigrationRollsBackBothColumns(t *testing.T) {
	db := newDependencyMigrationDB(t)
	want := errors.New("later migration failed")
	err := applyInPlaceMigrations(db, []schemaMigration{
		{version: 23, name: "dependency revision", inPlace: addDependencyRevisionColumns},
		{version: 24, name: "injected later failure", inPlace: func(*sql.Tx) error { return want }},
	})
	if !errors.Is(err, want) {
		t.Fatalf("migration error=%v, want wrapped injected failure", err)
	}
	for _, query := range []string{
		`SELECT COUNT(*) FROM pragma_table_xinfo('view_generations') WHERE name='dependency_revision'`,
		`SELECT COUNT(*) FROM pragma_table_xinfo('dedicated_base_publications') WHERE name='dependency_revision'`,
	} {
		var count int
		if err := db.QueryRow(query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("partial migration survived transaction rollback: %s", query)
		}
	}
}
