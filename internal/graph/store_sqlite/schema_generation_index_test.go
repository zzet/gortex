package store_sqlite

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The v17 step adds the two sparse view-generation indexes. They are the only
// keys in the package that lead with view_gen, and they exist for the two
// operations that ask about a generation rather than about a symbol:
// enumerating a derived generation's rows and dropping them again.

func generationIndexDDLByName(t *testing.T, db *sql.DB, name string) (string, bool) {
	t.Helper()
	var ddl sql.NullString
	err := db.QueryRow(
		`SELECT sql FROM sqlite_schema WHERE type = 'index' AND name = ?`, name,
	).Scan(&ddl)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("read index %s: %v", name, err)
	}
	return ddl.String, true
}

// TestGenerationIndexesExistOnFreshStore pins the shape a fresh store gets:
// both indexes present, both leading with view_gen, both partial on
// view_gen > 0 so a store that has only ever been plainly indexed maintains
// nothing.
func TestGenerationIndexesExistOnFreshStore(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "generation-index-fresh.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	for _, tc := range []struct{ name, table string }{
		{nodesByGenerationIndexName, "nodes"},
		{edgesByGenerationIndexName, "edges"},
	} {
		ddl, ok := generationIndexDDLByName(t, store.writerDB, tc.name)
		if !ok {
			t.Fatalf("fresh store is missing %s", tc.name)
		}
		if !strings.Contains(ddl, "ON "+tc.table+"(view_gen, id)") {
			t.Fatalf("%s must lead with view_gen: %s", tc.name, ddl)
		}
		if !strings.Contains(ddl, "WHERE view_gen > 0") {
			t.Fatalf("%s must stay partial so generation 0 pays nothing: %s", tc.name, ddl)
		}
	}
}

// TestGenerationIndexMigrationBringsForwardAnOlderStore drops both indexes to
// reproduce a store stamped at v16, runs the step, and checks the result
// matches a fresh store's. A second run must be a no-op.
func TestGenerationIndexMigrationBringsForwardAnOlderStore(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "generation-index-migrate.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedCoreRows(t, store)

	fresh := map[string]string{}
	for _, name := range []string{nodesByGenerationIndexName, edgesByGenerationIndexName} {
		ddl, ok := generationIndexDDLByName(t, store.writerDB, name)
		if !ok {
			t.Fatalf("fresh store is missing %s", name)
		}
		fresh[name] = ddl
		if _, err := store.writerDB.Exec("DROP INDEX " + name); err != nil {
			t.Fatalf("drop %s: %v", name, err)
		}
	}
	if _, err := store.writerDB.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, currentSchemaVersion-1)); err != nil {
		t.Fatalf("stamp pre-v17 version: %v", err)
	}

	for run := 0; run < 2; run++ {
		tx, err := store.writerDB.Begin()
		if err != nil {
			t.Fatalf("begin migration tx %d: %v", run, err)
		}
		if err := addGenerationEnumerationIndexes(tx); err != nil {
			_ = tx.Rollback()
			t.Fatalf("addGenerationEnumerationIndexes run %d: %v", run, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit migration tx %d: %v", run, err)
		}
	}

	for name, want := range fresh {
		got, ok := generationIndexDDLByName(t, store.writerDB, name)
		if !ok {
			t.Fatalf("migration did not restore %s", name)
		}
		if got != want {
			t.Fatalf("migrated %s differs from a fresh store's:\n migrated: %s\n    fresh: %s", name, got, want)
		}
	}
	if got := scalarInt(t, store.writerDB,
		`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name IN (?, ?)`,
		nodesByGenerationIndexName, edgesByGenerationIndexName); got != 2 {
		t.Fatalf("repeated migration runs left %d generation indexes, want 2", got)
	}
}

// TestGenerationIndexesStayEmptyAtGenerationZero is the cost argument as an
// assertion: a plainly indexed store writes no entry into either index, so the
// addition is free until a derived generation exists.
func TestGenerationIndexesStayEmptyAtGenerationZero(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "generation-index-empty.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedCoreRows(t, store)

	// The partial predicate has to be restated literally for SQLite to admit
	// the index — a bound parameter cannot be proven greater than zero.
	for _, tc := range []struct{ table, index string }{
		{"nodes", nodesByGenerationIndexName},
		{"edges", edgesByGenerationIndexName},
	} {
		if got := scalarInt(t, store.writerDB,
			`SELECT COUNT(*) FROM `+tc.table+` WHERE view_gen > 0`); got != 0 {
			t.Fatalf("%s holds %d rows above generation 0 before any derived write", tc.table, got)
		}
		plan := generationIndexPlan(t, store, tc.table)
		if !strings.Contains(plan, tc.index) {
			t.Fatalf("the sparse-generation enumeration of %s must ride %s:\n%s", tc.table, tc.index, plan)
		}
	}

	store.AtGeneration(1).AddBatch(genReadNodes(genOneMark), genReadEdges(genOneMark))
	for _, table := range []string{"nodes", "edges"} {
		if got := scalarInt(t, store.writerDB,
			`SELECT COUNT(*) FROM `+table+` WHERE view_gen > 0`); got == 0 {
			t.Fatalf("%s recorded no rows above generation 0 after a derived write", table)
		}
	}
}

func generationIndexPlan(t *testing.T, store *Store, table string) string {
	t.Helper()
	rows, err := store.db.Query(
		`EXPLAIN QUERY PLAN SELECT id FROM ` + table + ` WHERE view_gen > 0 ORDER BY view_gen, id`)
	if err != nil {
		t.Fatalf("explain %s enumeration: %v", table, err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		lines = append(lines, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return strings.Join(lines, "\n")
}
