package store_sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// hasEdgeColumn probes the edges table for a column by name. table_xinfo, not
// table_info, for the reason spelled out on ensureEdgeColumns: table_info omits
// generated columns, so only xinfo answers the question for every edges column.
func hasEdgeColumn(t *testing.T, db *sql.DB, col string) bool {
	t.Helper()
	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_xinfo('edges') WHERE name = ?`, col,
	).Scan(&count); err != nil {
		t.Fatalf("probe edges column %q: %v", col, err)
	}
	return count > 0
}

// edgeViewGens returns every edge's view_gen keyed by target id.
func edgeViewGens(t *testing.T, db *sql.DB) map[string]int64 {
	t.Helper()
	rows, err := db.Query(`SELECT to_id, view_gen FROM edges`)
	if err != nil {
		t.Fatalf("read edge view generations: %v", err)
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var to string
		var gen int64
		if err := rows.Scan(&to, &gen); err != nil {
			t.Fatalf("scan edge view generation: %v", err)
		}
		out[to] = gen
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate edge view generations: %v", err)
	}
	return out
}

// TestSchemaV13StoreGainsEdgeViewGenerationColumn is the backward-compatibility
// proof for edges.view_gen: a store written before the column existed gains it
// on its next Open, keeps every graph row, does not signal a rebuild, and
// reports each surviving edge in generation 0 — the single base corpus they
// already belonged to. The column is derivable from nothing, but its value for
// existing rows is a constant, so this must be an in-place upgrade rather than
// a wipe.
func TestSchemaV13StoreGainsEdgeViewGenerationColumn(t *testing.T) {
	if currentSchemaVersion < 14 {
		t.Fatalf("currentSchemaVersion = %d, want >= 14 for the edge view generation column", currentSchemaVersion)
	}
	var step *schemaMigration
	for i := range schemaMigrations {
		if schemaMigrations[i].version == 14 {
			step = &schemaMigrations[i]
			break
		}
	}
	if step == nil || step.rebuild || step.inPlace == nil {
		t.Fatalf("v14 migration = %+v, want a registered in-place step", step)
	}

	const (
		callerID = "repo/caller.go::Caller"
		firstID  = "repo/first.go::First"
		secondID = "repo/second.go::Second"
	)

	path := filepath.Join(t.TempDir(), "pre-view-gen.sqlite")
	seed, err := Open(path)
	if err != nil {
		t.Fatalf("create current store: %v", err)
	}
	// A fresh store gets the column straight from schemaSQL's CREATE TABLE.
	if !hasEdgeColumn(t, seed.writerDB, viewGenColumnName) {
		t.Fatalf("fresh store is missing edges.%s", viewGenColumnName)
	}
	seed.AddBatch([]*graph.Node{
		{ID: callerID, Kind: graph.KindFunction, Name: "Caller", FilePath: "repo/caller.go", RepoPrefix: "repo"},
		{ID: firstID, Kind: graph.KindFunction, Name: "First", FilePath: "repo/first.go", RepoPrefix: "repo"},
		{ID: secondID, Kind: graph.KindFunction, Name: "Second", FilePath: "repo/second.go", RepoPrefix: "repo"},
	}, []*graph.Edge{
		{From: callerID, To: firstID, Kind: graph.EdgeCalls, FilePath: "repo/caller.go", Line: 3},
		{From: callerID, To: secondID, Kind: graph.EdgeCalls, FilePath: "repo/caller.go", Line: 4},
	})
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	// Recreate the exact pre-v14 shape: the graph exists, the column does not,
	// and the file is stamped at the version before the column shipped. The
	// column cannot simply be dropped — since v16 it is part of the edges
	// unique key, and SQLite refuses to drop a constrained column — so the
	// table is rebuilt at its v13 shape, the same way the shipped build would
	// have left it. nodes is downgraded alongside it so the v16 step has both
	// tables in their pre-v16 shape.
	withRawDB(t, path, func(db *sql.DB) {
		downgradeCoreTable(t, db, "nodes", legacyNodesTableBody, legacyNodeIndexes,
			func(table string) error {
				if err := ensureNodeColumns(db, table); err != nil {
					return err
				}
				return ensureNodeGeneratedColumns(db, table)
			})
		downgradeCoreTable(t, db, "edges", legacyEdgesTableBodyV13, legacyEdgeIndexes,
			func(table string) error { return ensureEdgeColumns(db, table) })
		if _, err := db.Exec(`PRAGMA user_version = 13`); err != nil {
			t.Fatalf("stamp v13: %v", err)
		}
	})

	migrated, err := Open(path)
	if err != nil {
		t.Fatalf("reopen pre-view-gen store: %v", err)
	}
	t.Cleanup(func() { _ = migrated.Close() })

	if migrated.NeedsRebuild() {
		t.Fatal("an additive column upgrade must not signal a wipe/reindex")
	}
	if !hasEdgeColumn(t, migrated.writerDB, viewGenColumnName) {
		t.Fatalf("migrated store is missing edges.%s", viewGenColumnName)
	}
	if version, err := readUserVersion(migrated.writerDB); err != nil || version != currentSchemaVersion {
		t.Fatalf("post-migration user_version = %d (err %v), want %d", version, err, currentSchemaVersion)
	}
	if got := migrated.NodeCount(); got != 3 {
		t.Fatalf("node count after migration = %d, want 3", got)
	}
	gens := edgeViewGens(t, migrated.writerDB)
	if len(gens) != 2 {
		t.Fatalf("edge count after migration = %d, want 2", len(gens))
	}
	for _, to := range []string{firstID, secondID} {
		gen, ok := gens[to]
		if !ok {
			t.Fatalf("edge to %s did not survive the in-place upgrade", to)
		}
		if gen != 0 {
			t.Fatalf("pre-existing edge to %s has view_gen = %d, want 0", to, gen)
		}
	}

	// The upgraded store still writes edges, and a write that says nothing
	// about generations lands in the base one.
	const thirdID = "repo/third.go::Third"
	migrated.AddBatch([]*graph.Node{
		{ID: thirdID, Kind: graph.KindFunction, Name: "Third", FilePath: "repo/third.go", RepoPrefix: "repo"},
	}, []*graph.Edge{
		{From: callerID, To: thirdID, Kind: graph.EdgeCalls, FilePath: "repo/caller.go", Line: 5},
	})
	gens = edgeViewGens(t, migrated.writerDB)
	if gen, ok := gens[thirdID]; !ok || gen != 0 {
		t.Fatalf("post-migration edge view_gen = %d (present %v), want 0", gen, ok)
	}
}

// TestSchemaEdgeViewGenerationMigrationIsIdempotent covers the case the
// column probe exists for: schemaSQL runs before the migration steps, so a
// store whose edges table was created by this build already carries view_gen
// when the v14 step runs. Re-running the step must be a no-op rather than an
// "duplicate column name" failure.
func TestSchemaEdgeViewGenerationMigrationIsIdempotent(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "idempotent-view-gen.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	for i := 0; i < 2; i++ {
		tx, err := store.writerDB.Begin()
		if err != nil {
			t.Fatalf("begin migration tx %d: %v", i, err)
		}
		if err := addEdgeViewGenerationColumn(tx); err != nil {
			_ = tx.Rollback()
			t.Fatalf("addEdgeViewGenerationColumn run %d: %v", i, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit migration tx %d: %v", i, err)
		}
	}
	if !hasEdgeColumn(t, store.writerDB, viewGenColumnName) {
		t.Fatalf("store is missing edges.%s after repeated migration runs", viewGenColumnName)
	}
}
