package store_sqlite

import (
	"database/sql"
	"fmt"
	"testing"
)

// BenchmarkRepresentativeV13ToV19Migration is a bounded, disposable proxy for
// the shipped v14-v19 migration shape: column addition, payload re-key copy,
// core copy, sparse indexes, mask tables, and a scoped cleanup. It deliberately
// does not claim to reproduce a user's data distribution; its stable 10k-row
// corpus catches transaction/observer regressions without touching a live store.
func BenchmarkRepresentativeV13ToV19Migration(b *testing.B) {
	for i := 0; i < b.N; i++ {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			b.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE payload (id INTEGER PRIMARY KEY, repo_prefix TEXT, kind TEXT); WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<10000) INSERT INTO payload SELECT x, printf('repo-%d',x%20), CASE WHEN x%10=0 THEN 'legacy' ELSE 'symbol' END FROM n`); err != nil {
			b.Fatal(err)
		}
		steps := []schemaMigration{
			{version: 14, name: "checkout catalog", inPlace: benchSQL(`ALTER TABLE payload ADD COLUMN view_gen INTEGER NOT NULL DEFAULT 0`)},
			{version: 15, name: "payload sidecars", inPlace: benchSQL(`CREATE TABLE sidecar AS SELECT view_gen,id,kind FROM payload`)},
			{version: 16, name: "graph core", inPlace: benchSQL(`CREATE TABLE core AS SELECT view_gen,id,repo_prefix,kind FROM payload`)},
			{version: 17, name: "generation indexes", inPlace: benchSQL(`CREATE INDEX core_generation ON core(view_gen,id); CREATE INDEX core_sparse_generation ON core(view_gen) WHERE view_gen<>0`)},
			{version: 18, name: "generation masks", inPlace: benchSQL(`CREATE TABLE generation_masks (view_gen INTEGER, id INTEGER, PRIMARY KEY(view_gen,id)) WITHOUT ROWID`)},
			{version: 19, name: "coverage purge replay", inPlace: benchSQL(`DELETE FROM core WHERE view_gen<>0 AND kind='legacy'`)},
		}
		var elapsedNanos int64
		if err := applyInPlaceMigrations(db, steps, func(p MigrationProgress) {
			if p.Phase == MigrationFinished {
				elapsedNanos += p.Elapsed.Nanoseconds()
			}
		}); err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(elapsedNanos)/1e6, "migration-ms")
		if err := db.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRealV13ToV19StepsOnCurrentShape invokes the exact shipped v14-v19
// functions through applyInPlaceMigrations. The disposable store begins in the
// converged shape, so this measures idempotency/check cost rather than legacy
// table-copy cost; the representative benchmark above supplies the bounded
// row-moving workload that a complete historical fixture would require.
func BenchmarkRealV13ToV19StepsOnCurrentShape(b *testing.B) {
	steps := pendingBetween(13, 19, schemaMigrations)
	for i := 0; i < b.N; i++ {
		path := b.TempDir() + "/graph.db"
		store, err := Open(path)
		if err != nil {
			b.Fatal(err)
		}
		if err := store.Close(); err != nil {
			b.Fatal(err)
		}
		db, err := sql.Open("sqlite", path)
		if err != nil {
			b.Fatal(err)
		}
		if err := applyInPlaceMigrations(db, steps); err != nil {
			b.Fatal(err)
		}
		if err := db.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func benchSQL(statement string) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("fixture migration: %w", err)
		}
		return nil
	}
}
