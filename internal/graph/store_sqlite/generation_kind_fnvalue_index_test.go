package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type generationSelectiveIndexDefinition struct {
	name    string
	legacy  string
	scoped  string
	columns []string
	partial bool
}

var generationSelectiveIndexDefinitions = []generationSelectiveIndexDefinition{
	{
		name:    "nodes_by_kind",
		legacy:  `CREATE INDEX nodes_by_kind ON nodes(kind)`,
		scoped:  `CREATE INDEX nodes_by_kind ON nodes(kind, view_gen)`,
		columns: []string{"kind", "view_gen"},
	},
	{
		name:    "edges_fnvalue_prefixed",
		legacy:  `CREATE INDEX edges_fnvalue_prefixed ON edges(to_id) WHERE to_id LIKE '%::unresolved::fnvalue::%'`,
		scoped:  `CREATE INDEX edges_fnvalue_prefixed ON edges(view_gen, to_id) WHERE to_id LIKE '%::unresolved::fnvalue::%'`,
		columns: []string{"view_gen", "to_id"},
		partial: true,
	},
}

var generationSelectiveKindPlanQuery = nodesByKindSQL
var generationSelectiveFnvaluePlanQuery = `SELECT ` + lookupEdgeCols + ` FROM edges WHERE to_id LIKE '%::unresolved::fnvalue::%' AND view_gen = ?`

const generationSelectiveKindResultQuery = `SELECT id, meta FROM nodes WHERE kind = ? AND view_gen = ? ORDER BY id`
const generationSelectiveFnvalueResultQuery = `SELECT id, meta FROM edges WHERE to_id LIKE '%::unresolved::fnvalue::%' AND view_gen = ? ORDER BY id`

func replaceGenerationSelectiveIndexes(t testing.TB, db *sql.DB, scoped bool) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, index := range generationSelectiveIndexDefinitions {
		if _, err := tx.Exec(`DROP INDEX IF EXISTS ` + index.name); err != nil {
			t.Fatal(err)
		}
	}
	for _, index := range generationSelectiveIndexDefinitions {
		ddl := index.legacy
		if scoped {
			ddl = index.scoped
		}
		if _, err := tx.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func requireGenerationSelectiveIndexShapes(t testing.TB, db *sql.DB, scoped bool) {
	t.Helper()
	for _, definition := range generationSelectiveIndexDefinitions {
		rows, err := db.Query(`PRAGMA index_info(` + quoteSQLiteIdentifier(definition.name) + `)`)
		if err != nil {
			t.Fatal(err)
		}
		var columns []string
		for rows.Next() {
			var sequence, columnID int
			var name string
			if err := rows.Scan(&sequence, &columnID, &name); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			columns = append(columns, name)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		wantColumns := definition.columns
		if !scoped && definition.name == "nodes_by_kind" {
			wantColumns = []string{"kind"}
		}
		if !scoped && definition.name == "edges_fnvalue_prefixed" {
			wantColumns = []string{"to_id"}
		}
		if strings.Join(columns, ",") != strings.Join(wantColumns, ",") {
			t.Fatalf("%s columns = %v, want %v", definition.name, columns, wantColumns)
		}
		ddl := generationLookupIndexDDL(t, db, definition.name)
		if gotPartial := strings.Contains(ddl, " WHERE "); gotPartial != definition.partial {
			t.Fatalf("%s partial = %v, want %v: %s", definition.name, gotPartial, definition.partial, ddl)
		}
		if definition.partial && !strings.Contains(ddl, "WHERE to_id LIKE '%::unresolved::fnvalue::%'") {
			t.Fatalf("%s lost its canonical predicate: %s", definition.name, ddl)
		}
	}
}

func requireGenerationLookupFixtureDefinitionsShape(t testing.TB, db *sql.DB, scoped bool) {
	t.Helper()
	for _, definition := range generationLookupIndexDefinitions {
		want := definition.legacy
		if scoped {
			want = definition.scoped
		}
		if got := generationLookupIndexDDL(t, db, definition.name); got != want {
			t.Fatalf("%s DDL:\n got: %s\nwant: %s", definition.name, got, want)
		}
	}
}

func requireCurrentGenerationLookupDefinitionsShape(t testing.TB, db *sql.DB) {
	t.Helper()
	for _, definition := range generationLookupIndexDefinitions {
		if got := generationLookupIndexDDL(t, db, definition.name); got != definition.current {
			t.Fatalf("%s DDL:\n got: %s\nwant: %s", definition.name, got, definition.current)
		}
	}
}

func seedGenerationSelectiveTarget(t testing.TB, db *sql.DB, generation, rows int) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	nodes, err := tx.Prepare(`INSERT INTO nodes(id, view_gen, kind, name, file_path, language, repo_prefix, meta) VALUES (?, ?, ?, ?, ?, 'go', 'repo', ?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer nodes.Close()
	edges, err := tx.Prepare(`INSERT INTO edges(from_id, to_id, kind, file_path, line, view_gen, meta) VALUES (?, ?, 'references', ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer edges.Close()
	for i := 0; i < rows; i++ {
		id := fmt.Sprintf("repo/target%02d.go::G%dN%04d", i%8, generation, i)
		kind := "function"
		if i%2 == 0 {
			kind = "file"
		}
		filePath := fmt.Sprintf("repo/target%02d.go", i%8)
		meta := []byte(fmt.Sprintf(`{"generation":%d,"row":%d}`, generation, i))
		if _, err := nodes.Exec(id, generation, kind, fmt.Sprintf("Name%04d", i), filePath, meta); err != nil {
			t.Fatal(err)
		}
		toID := fmt.Sprintf("repo::resolved::target%04d", i)
		if i < 2 {
			toID = fmt.Sprintf("repo::unresolved::fnvalue::target%04d", i)
		}
		if _, err := edges.Exec(id, toID, filePath, i+1, generation, meta); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func seedGenerationSelectiveHistory(t testing.TB, db *sql.DB, firstGeneration, generations, rows int) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	insert, err := tx.Prepare(`INSERT INTO edges(from_id, to_id, kind, file_path, line, view_gen, meta) VALUES (?, ?, 'references', ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer insert.Close()
	for generation := firstGeneration; generation < firstGeneration+generations; generation++ {
		for i := 0; i < rows; i++ {
			fromID := fmt.Sprintf("repo/history/g%d.go::N%05d", generation, i)
			toID := fmt.Sprintf("repo::unresolved::fnvalue::history%05d", i)
			meta := []byte(fmt.Sprintf(`{"generation":%d,"history":%d}`, generation, i))
			if _, err := insert.Exec(fromID, toID, fmt.Sprintf("repo/history/g%d.go", generation), i+1, generation, meta); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func generationSelectiveSnapshot(t testing.TB, db *sql.DB) map[string]generationLookupResult {
	t.Helper()
	out := make(map[string]generationLookupResult, 4)
	for _, generation := range []int{0, 3} {
		out[fmt.Sprintf("kind/%d", generation)] = readGenerationLookupResult(t, db, generationSelectiveKindResultQuery, "file", generation)
		out[fmt.Sprintf("fnvalue/%d", generation)] = readGenerationLookupResult(t, db, generationSelectiveFnvalueResultQuery, generation)
	}
	return out
}

func requireGenerationSelectivePlans(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, generation := range []int{0, 3} {
		plan := generationLookupPlan(t, db, generationSelectiveKindPlanQuery, "file", generation)
		// Both dense indexes seek exactly the requested kind and generation.
		// This unordered API does not require either equality's key order.
		selective := false
		for _, line := range strings.Split(plan, "\n") {
			if (strings.Contains(line, "SEARCH nodes USING INDEX nodes_by_kind (") ||
				strings.Contains(line, "SEARCH nodes USING INDEX nodes_stats_histogram (")) &&
				strings.Contains(line, "kind=?") && strings.Contains(line, "view_gen=?") {
				selective = true
			}
		}
		if !selective || strings.Contains(plan, "SCAN nodes") || strings.Contains(plan, "TEMP B-TREE") {
			t.Fatalf("kind read lost its exact kind/generation seek:\n%s", plan)
		}
		requireGenerationLookupPlan(t, db, generationSelectiveFnvaluePlanQuery, "edges_fnvalue_prefixed", false,
			[]any{generation}, "view_gen=?")
	}
}

func countGenerationSelectiveStoreResults(t testing.TB, store *Store, generation int64) (files, fnvalues int) {
	t.Helper()
	view := store.AtGeneration(generation)
	for range view.NodesByKind(graph.NodeKind("file")) {
		files++
	}
	for range view.FnValuePlaceholderEdges() {
		fnvalues++
	}
	return files, fnvalues
}

func TestGenerationSelectiveIndexesBoundGenerationAndHistory(t *testing.T) {
	db, path := openGenerationLookupDB(t)
	replaceGenerationSelectiveIndexes(t, db, true)
	seedGenerationSelectiveTarget(t, db, 0, 128)
	seedGenerationSelectiveTarget(t, db, 3, 128)

	t.Run("no_statistics", func(t *testing.T) {
		requireGenerationSelectivePlans(t, db)
	})
	if _, err := db.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}
	t.Run("fresh_statistics", func(t *testing.T) {
		requireGenerationSelectivePlans(t, db)
	})

	seedGenerationSelectiveHistory(t, db, 4, 8, 2048)
	t.Run("growing_unrelated_history_with_valid_stale_statistics", func(t *testing.T) {
		requireGenerationSelectivePlans(t, db)
	})
	for _, generation := range []int64{0, 3} {
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		files, fnvalues := countGenerationSelectiveStoreResults(t, store, generation)
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if files != 64 {
			t.Errorf("generation %d files = %d, want 64", generation, files)
		}
		if fnvalues != 2 {
			t.Errorf("generation %d fn-value placeholders = %d, want 2", generation, fnvalues)
		}
	}
}

func TestGenerationSelectiveIndexMigration(t *testing.T) {
	for _, fromVersion := range []int{25, 26} {
		t.Run(fmt.Sprintf("v%d_to_v27", fromVersion), func(t *testing.T) {
			db, path := openGenerationLookupDB(t)
			seedGenerationLookupRows(t, db, 0, 4, 128)
			seedGenerationSelectiveTarget(t, db, 0, 128)
			seedGenerationSelectiveTarget(t, db, 3, 128)
			replaceGenerationLookupIndexes(t, db, fromVersion >= 26)
			replaceGenerationSelectiveIndexes(t, db, false)
			requireGenerationLookupFixtureDefinitionsShape(t, db, fromVersion >= 26)
			requireGenerationSelectiveIndexShapes(t, db, false)
			beforePayload := generationPayloadDigest(t, db)
			beforeSelective := generationSelectiveSnapshot(t, db)
			if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, fromVersion)); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			for reopen := 0; reopen < 2; reopen++ {
				store, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				if store.NeedsRebuild() {
					t.Fatal("selective index migration requested a source rebuild")
				}
				var version int
				if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
					t.Fatal(err)
				}
				if version != currentSchemaVersion {
					t.Fatalf("reopen %d schema version = %d, want %d", reopen, version, currentSchemaVersion)
				}
				if got := generationPayloadDigest(t, store.db); got != beforePayload {
					t.Fatalf("reopen %d payload digest = %x, want %x", reopen, got, beforePayload)
				}
				gotSelective := generationSelectiveSnapshot(t, store.db)
				for name, want := range beforeSelective {
					if gotSelective[name] != want {
						t.Errorf("reopen %d %s = %+v, want %+v", reopen, name, gotSelective[name], want)
					}
				}
				requireGenerationLookupIndexShapes(t, store.db)
				requireCurrentGenerationLookupDefinitionsShape(t, store.db)
				requireGenerationSelectiveIndexShapes(t, store.db, true)
				requireGenerationSelectivePlans(t, store.db)
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestGenerationSelectiveIndexMigrationRollsBackAtomically(t *testing.T) {
	db, path := openGenerationLookupDB(t)
	seedGenerationLookupRows(t, db, 0, 4, 64)
	seedGenerationSelectiveTarget(t, db, 0, 64)
	seedGenerationSelectiveTarget(t, db, 3, 64)
	replaceGenerationLookupIndexes(t, db, true)
	replaceGenerationSelectiveIndexes(t, db, false)
	requireGenerationLookupFixtureDefinitionsShape(t, db, true)
	requireGenerationSelectiveIndexShapes(t, db, false)
	beforePayload := generationPayloadDigest(t, db)
	beforeDDL := make(map[string]string, len(generationSelectiveIndexDefinitions))
	for _, definition := range generationSelectiveIndexDefinitions {
		beforeDDL[definition.name] = generationLookupIndexDDL(t, db, definition.name)
	}
	if _, err := db.Exec(`PRAGMA user_version = 26`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	restoreRegistry := corruptGenerationLookupRegistryDDL(t, "edges_fnvalue_prefixed")
	defer restoreRegistry()
	failedStore, err := Open(path)
	if failedStore != nil {
		_ = failedStore.Close()
	}
	restoreRegistry()
	if err == nil {
		t.Fatal("Open with invalid v27 canonical DDL succeeded")
	}
	if !strings.Contains(err.Error(), "scope kind and fn-value indexes by view generation") {
		t.Fatalf("Open failed before the v27 migration exercised transactional rebuild: %v", err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 26 {
		t.Fatalf("failed migration schema version = %d, want 26", version)
	}
	if got := generationPayloadDigest(t, raw); got != beforePayload {
		t.Fatalf("failed migration changed payload digest: %x -> %x", beforePayload, got)
	}
	requireGenerationLookupFixtureDefinitionsShape(t, raw, true)
	requireGenerationSelectiveIndexShapes(t, raw, false)
	for name, want := range beforeDDL {
		if got := generationLookupIndexDDL(t, raw, name); got != want {
			t.Errorf("failed migration changed %s:\n got: %s\nwant: %s", name, got, want)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after rolled-back migration: %v", err)
	}
	defer upgraded.Close()
	if got := generationPayloadDigest(t, upgraded.db); got != beforePayload {
		t.Fatalf("successful retry changed payload digest: %x -> %x", beforePayload, got)
	}
	if err := upgraded.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("successful retry schema version = %d, want %d", version, currentSchemaVersion)
	}
	requireCurrentGenerationLookupDefinitionsShape(t, upgraded.db)
	requireGenerationSelectiveIndexShapes(t, upgraded.db, true)
}

func TestGenerationSelectiveIndexesRespectBulkLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bulk.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if !store.BeginCoordinatedBulkLoad() {
		t.Fatal("fresh store did not enter coordinated bulk load")
	}
	ended := false
	defer func() {
		if !ended {
			_ = store.AbortCoordinatedBulkLoad()
		}
	}()

	ctx := context.Background()
	var kindPresent, fnvaluePresent int
	if err := store.bulkConn.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='index' AND name='nodes_by_kind')`).Scan(&kindPresent); err != nil {
		t.Fatal(err)
	}
	if err := store.bulkConn.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='index' AND name='edges_fnvalue_prefixed')`).Scan(&fnvaluePresent); err != nil {
		t.Fatal(err)
	}
	if kindPresent != 0 {
		t.Fatal("nodes_by_kind stayed installed during droppable-index bulk window")
	}
	if fnvaluePresent != 1 {
		t.Fatal("edges_fnvalue_prefixed was dropped during bulk window")
	}

	node := &graph.Node{
		ID:         "repo/file.go::File",
		Kind:       graph.NodeKind("file"),
		Name:       "File",
		FilePath:   "repo/file.go",
		RepoPrefix: "repo",
	}
	edge := &graph.Edge{
		From:     node.ID,
		To:       "repo::unresolved::fnvalue::File",
		Kind:     graph.EdgeKind("references"),
		FilePath: "repo/file.go",
		Line:     1,
	}
	if err := store.AddBatchChecked([]*graph.Node{node}, []*graph.Edge{edge}); err != nil {
		t.Fatal(err)
	}
	files, fnvalues := countGenerationSelectiveStoreResults(t, store, 0)
	if files != 1 || fnvalues != 1 {
		t.Fatalf("bulk-window results files=%d fnvalues=%d, want 1/1", files, fnvalues)
	}

	if err := store.EndCoordinatedBulkLoad(); err != nil {
		t.Fatal(err)
	}
	ended = true
	requireGenerationSelectiveIndexShapes(t, store.db, true)
	files, fnvalues = countGenerationSelectiveStoreResults(t, store, 0)
	if files != 1 || fnvalues != 1 {
		t.Fatalf("post-bulk results files=%d fnvalues=%d, want 1/1", files, fnvalues)
	}
}
