package store_sqlite

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The current dense view-generation indexes lead with view_gen then id. They
// serve the two operations that ask about one generation rather than about a
// symbol: enumerating that generation's rows and dropping them again.

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

// TestGenerationIndexesExistOnFreshStore pins the dense shape a fresh store
// gets: both indexes lead with view_gen then id and cover generation zero as
// well as derived generations.
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
			t.Fatalf("%s must lead with view_gen then id: %s", tc.name, ddl)
		}
		if strings.Contains(ddl, "WHERE view_gen > 0") {
			t.Fatalf("%s must remain dense for generation 0: %s", tc.name, ddl)
		}
	}
}

// TestGenerationIndexInstallerIsIdempotent drops the current indexes then
// invokes their direct installer twice. Historical schema transitions belong
// to the migration tests; this fixture checks only current installer behavior.
func TestGenerationIndexInstallerIsIdempotent(t *testing.T) {
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

// TestGenerationIndexesServeGenerationZeroAndDerivedRows proves that both
// populated generations return the same ordered IDs as a table scan and that
// the production-shaped query takes the bounded, order-preserving index path.
func TestGenerationIndexesServeGenerationZeroAndDerivedRows(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "generation-index-dense.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedCoreRows(t, store)
	store.AtGeneration(1).AddBatch(genReadNodes(genOneMark), genReadEdges(genOneMark))

	for _, tc := range []struct{ table, index string }{
		{"nodes", nodesByGenerationIndexName},
		{"edges", edgesByGenerationIndexName},
	} {
		for _, generation := range []int64{0, 1} {
			want := generationTableIDs(t, store, tc.table, generation, true)
			if len(want) == 0 {
				t.Fatalf("%s has no rows at generation %d", tc.table, generation)
			}
			got := generationTableIDs(t, store, tc.table, generation, false)
			if !slices.Equal(got, want) {
				t.Fatalf("%s generation %d IDs=%v, want %v", tc.table, generation, got, want)
			}
			plan := generationEqualityPlan(t, store, tc.table, generation)
			if !strings.Contains(plan, "SEARCH "+tc.table) || !strings.Contains(plan, "view_gen=?") {
				t.Fatalf("generation %d enumeration of %s must be bounded by view_gen:\n%s", generation, tc.table, plan)
			}
			if !strings.Contains(plan, tc.index) {
				t.Fatalf("the bounded generation %d enumeration of %s must use %s:\n%s", generation, tc.table, tc.index, plan)
			}
			if strings.Contains(strings.ToUpper(plan), "TEMP B-TREE") {
				t.Fatalf("generation %d enumeration of %s sorted outside %s:\n%s", generation, tc.table, tc.index, plan)
			}
		}
	}
}

func generationTableIDs(t *testing.T, store *Store, table string, generation int64, notIndexed bool) []string {
	t.Helper()
	from := table
	if notIndexed {
		from += " NOT INDEXED"
	}
	rows, err := store.db.Query(
		`SELECT id FROM `+from+` WHERE view_gen = ? ORDER BY id`, generation)
	if err != nil {
		t.Fatalf("read %s generation %d: %v", table, generation, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan %s generation %d: %v", table, generation, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s generation %d: %v", table, generation, err)
	}
	return ids
}

func generationEqualityPlan(t *testing.T, store *Store, table string, generation int64) string {
	t.Helper()
	rows, err := store.db.Query(
		`EXPLAIN QUERY PLAN SELECT id FROM `+table+` WHERE view_gen = ? ORDER BY id`, generation)
	if err != nil {
		t.Fatalf("explain %s generation %d enumeration: %v", table, generation, err)
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
