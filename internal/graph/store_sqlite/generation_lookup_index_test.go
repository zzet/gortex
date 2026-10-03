package store_sqlite

import (
	"database/sql"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type generationLookupIndexDefinition struct {
	name    string
	legacy  string
	scoped  string // v26 fixture layout
	current string // post-Open schema layout
	columns []string
}

var generationLookupIndexDefinitions = []generationLookupIndexDefinition{
	{"nodes_by_name", `CREATE INDEX nodes_by_name ON nodes(name)`, `CREATE INDEX nodes_by_name ON nodes(name, view_gen)`, `CREATE INDEX nodes_by_name ON nodes(name, view_gen)`, []string{"name", "view_gen"}},
	{"nodes_by_file", `CREATE INDEX nodes_by_file ON nodes(file_path)`, `CREATE INDEX nodes_by_file ON nodes(file_path, view_gen)`, `CREATE INDEX nodes_by_file ON nodes(file_path, view_gen)`, []string{"file_path", "view_gen"}},
	{"nodes_by_repo", `CREATE INDEX nodes_by_repo ON nodes(repo_prefix) WHERE repo_prefix <> ''`, `CREATE INDEX nodes_by_repo ON nodes(repo_prefix, view_gen)`, `CREATE INDEX nodes_by_repo ON nodes(repo_prefix, view_gen)`, []string{"repo_prefix", "view_gen"}},
	{"nodes_by_repo_language_name", `CREATE INDEX nodes_by_repo_language_name ON nodes(repo_prefix, language, name) WHERE name <> ''`, `CREATE INDEX nodes_by_repo_language_name ON nodes(repo_prefix, language, name, view_gen) WHERE name <> ''`, `CREATE INDEX nodes_by_repo_language_name ON nodes(repo_prefix, language, name, view_gen) WHERE name <> ''`, []string{"repo_prefix", "language", "name", "view_gen"}},
	{"edges_by_from", `CREATE INDEX edges_by_from ON edges(from_id, kind)`, `CREATE INDEX edges_by_from ON edges(from_id, view_gen, kind)`, `CREATE INDEX edges_by_from ON edges(view_gen, from_id, kind)`, []string{"view_gen", "from_id", "kind"}},
	{"edges_by_to", `CREATE INDEX edges_by_to ON edges(to_id, kind)`, `CREATE INDEX edges_by_to ON edges(to_id, view_gen, kind)`, `CREATE INDEX edges_by_to ON edges(view_gen, to_id, kind)`, []string{"view_gen", "to_id", "kind"}},
	{nodesByGenerationIndexName, `CREATE INDEX nodes_by_generation ON nodes(view_gen, id) WHERE view_gen > 0`, `CREATE INDEX nodes_by_generation ON nodes(view_gen, id)`, `CREATE INDEX nodes_by_generation ON nodes(view_gen, id)`, []string{"view_gen", "id"}},
	{edgesByGenerationIndexName, `CREATE INDEX edges_by_generation ON edges(view_gen, id) WHERE view_gen > 0`, `CREATE INDEX edges_by_generation ON edges(view_gen, id)`, `CREATE INDEX edges_by_generation ON edges(view_gen, id)`, []string{"view_gen", "id"}},
}

const generationLookupNameQuery = `SELECT id, meta FROM nodes WHERE name = ? AND view_gen = ? ORDER BY id`
const generationLookupFileQuery = `SELECT id, meta FROM nodes WHERE file_path = ? AND view_gen = ? ORDER BY id`
const generationLookupRepoQuery = `SELECT id, meta FROM nodes WHERE repo_prefix = ? AND view_gen = ? ORDER BY id`
const generationLookupRepoLanguageNameQuery = `SELECT id, meta FROM nodes WHERE repo_prefix = ? AND language = ? AND name = ? AND name <> '' AND view_gen = ? ORDER BY id`
const generationLookupNodeGenerationQuery = `SELECT id, meta FROM nodes WHERE view_gen = ? ORDER BY id`
const generationLookupIncomingQuery = `SELECT id, meta FROM edges WHERE to_id = ? AND view_gen = ? ORDER BY kind, id`
const generationLookupOutgoingQuery = `SELECT id, meta FROM edges WHERE from_id = ? AND view_gen = ?`
const generationLookupEdgeGenerationQuery = `SELECT id, meta FROM edges WHERE view_gen = ? ORDER BY id`

const generationLookupRepoProjectionQuery = repoLanguageFileCountsSQL

const generationLookupRepoLanguageCountQuery = `SELECT COUNT(*) FROM nodes WHERE repo_prefix = ? AND language IN (?) AND kind <> ? AND kind <> ? AND view_gen = ?`

var generationLookupNamePlanQuery = `SELECT ` + lookupNodeCols + ` FROM nodes WHERE name = ? AND view_gen = ? ORDER BY id`
var generationLookupFilePlanQuery = `SELECT ` + lookupNodeCols + ` FROM nodes WHERE file_path = ? AND view_gen = ? ORDER BY id`
var generationLookupRepoPlanQuery = `SELECT ` + lookupNodeCols + ` FROM nodes WHERE repo_prefix = ? AND view_gen = ? ORDER BY id`
var generationLookupRepoLanguageNamePlanQuery = `SELECT ` + lookupNodeCols + ` FROM nodes WHERE repo_prefix = ? AND language = ? AND name = ? AND name <> '' AND view_gen = ? ORDER BY id`
var generationLookupNodeGenerationPlanQuery = `SELECT ` + lookupNodeCols + ` FROM nodes WHERE view_gen = ? ORDER BY id`
var generationLookupIncomingPlanQuery = `SELECT ` + lookupEdgeCols + ` FROM edges WHERE to_id = ? AND view_gen = ? ORDER BY kind, id`
var generationLookupOutgoingPlanQuery = `SELECT ` + lookupEdgeCols + ` FROM edges WHERE from_id = ? AND view_gen = ?`

// Keep this plan contract tied to the derived-generation prepared read. The
// unhinted result query above also covers generation zero; AllEdges routes
// derived views through generationAllEdgesSQL, whose literal predicate and
// index fence preserve the bounded ordered enumeration path.
var generationLookupEdgeGenerationPlanQuery = generationAllEdgesSQL

func openGenerationLookupDB(t testing.TB) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lookup.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func seedGenerationLookupRows(t testing.TB, db *sql.DB, first, generations, perGeneration int) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	nodes, err := tx.Prepare(`INSERT INTO nodes(id, view_gen, kind, name, qual_name, file_path, language, repo_prefix, data_class, meta) VALUES (?, ?, 'function', ?, ?, ?, 'go', ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer nodes.Close()
	edges, err := tx.Prepare(`INSERT INTO edges(from_id, to_id, kind, file_path, line, view_gen, meta) VALUES (?, ?, 'calls', ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer edges.Close()
	for generation := first; generation < first+generations; generation++ {
		for i := 0; i < perGeneration; i++ {
			filePath := fmt.Sprintf("repo/source%02d.go", i%8)
			id := fmt.Sprintf("%s::F%05d", filePath, i)
			name := fmt.Sprintf("Name%02d", i%16)
			var repoPrefix string
			switch {
			case i%17 == 0:
				repoPrefix = ""
			case i%16 == 3:
				repoPrefix = "repo"
			default:
				repoPrefix = fmt.Sprintf("other%02d", i%32)
			}
			dataClass := "symbol"
			if i%13 == 0 {
				dataClass = "content"
			}
			meta := []byte(fmt.Sprintf(`{"generation":%d,"payload":"%s"}`, generation, strings.Repeat("x", 96)))
			if _, err := nodes.Exec(id, generation, name, "pkg."+name, filePath, repoPrefix, dataClass, meta); err != nil {
				t.Fatal(err)
			}
			if _, err := edges.Exec(id, fmt.Sprintf("repo/target.go::T%02d", i%16), filePath, i+1, generation, meta); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func replaceGenerationLookupIndexes(t testing.TB, db *sql.DB, scoped bool) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, index := range generationLookupIndexDefinitions {
		if _, err := tx.Exec(`DROP INDEX IF EXISTS ` + index.name); err != nil {
			t.Fatal(err)
		}
	}
	for _, index := range generationLookupIndexDefinitions {
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

type generationLookupResult struct {
	count int
	hash  uint64
}

func readGenerationLookupResult(t testing.TB, db *sql.DB, query string, args ...any) generationLookupResult {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	hash := fnv.New64a()
	count := 0
	for rows.Next() {
		var id any
		var meta []byte
		if err := rows.Scan(&id, &meta); err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintf(hash, "%v\x00", id)
		_, _ = hash.Write(meta)
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return generationLookupResult{count: count, hash: hash.Sum64()}
}

func generationLookupCases(generation int) []struct {
	name  string
	query string
	args  []any
} {
	return []struct {
		name  string
		query string
		args  []any
	}{
		{"name", generationLookupNameQuery, []any{"Name03", generation}},
		{"file", generationLookupFileQuery, []any{"repo/source03.go", generation}},
		{"repo", generationLookupRepoQuery, []any{"repo", generation}},
		{"repo_language_name", generationLookupRepoLanguageNameQuery, []any{"repo", "go", "Name03", generation}},
		{"node_generation", generationLookupNodeGenerationQuery, []any{generation}},
		{"incoming", generationLookupIncomingQuery, []any{"repo/target.go::T03", generation}},
		{"outgoing", generationLookupOutgoingQuery, []any{"repo/source03.go::F00003", generation}},
		{"edge_generation", generationLookupEdgeGenerationQuery, []any{generation}},
	}
}

func generationLookupSnapshot(t testing.TB, db *sql.DB) map[string]generationLookupResult {
	t.Helper()
	out := make(map[string]generationLookupResult, 18)
	for _, generation := range []int{0, 3} {
		for _, lookup := range generationLookupCases(generation) {
			out[fmt.Sprintf("%s/%d", lookup.name, generation)] = readGenerationLookupResult(t, db, lookup.query, lookup.args...)
		}
		out[fmt.Sprintf("repo_projection/%d", generation)] = generationRepoProjectionResult(t, db, generation)
	}
	return out
}

func generationRepoProjectionResult(t testing.TB, db *sql.DB, generation int) generationLookupResult {
	t.Helper()
	rows, err := db.Query(generationLookupRepoProjectionQuery, `["","repo"]`, "directory", "file", generation)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	hash := fnv.New64a()
	count := 0
	prefixes := map[string]bool{}
	for rows.Next() {
		var repoPrefix, filePath, language string
		var nodes int
		if err := rows.Scan(&repoPrefix, &filePath, &language, &nodes); err != nil {
			t.Fatal(err)
		}
		prefixes[repoPrefix] = true
		_, _ = fmt.Fprintf(hash, "%s\x00%s\x00%s\x00%d\x00", repoPrefix, filePath, language, nodes)
		count += nodes
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !prefixes[""] || !prefixes["repo"] {
		t.Fatalf("repo projection prefixes = %v, want empty and repo", prefixes)
	}
	return generationLookupResult{count: count, hash: hash.Sum64()}
}

func generationPayloadDigest(t testing.TB, db *sql.DB) uint64 {
	t.Helper()
	hash := fnv.New64a()
	for _, query := range []string{
		`SELECT id, view_gen, meta FROM nodes ORDER BY view_gen, id`,
		`SELECT id, view_gen, meta FROM edges ORDER BY view_gen, id`,
	} {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id any
			var generation int64
			var meta []byte
			if err := rows.Scan(&id, &generation, &meta); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			_, _ = fmt.Fprintf(hash, "%v\x00%d\x00", id, generation)
			_, _ = hash.Write(meta)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return hash.Sum64()
}

func generationLookupPlan(t testing.TB, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(details, "\n")
}

func requireGenerationLookupPlan(t testing.TB, db *sql.DB, query, index string, allowTemp bool, args []any, constraints ...string) {
	t.Helper()
	plan := generationLookupPlan(t, db, query, args...)
	lower := strings.ToLower(plan)
	if !strings.Contains(lower, strings.ToLower(index)) {
		t.Fatalf("plan does not use %s:\n%s", index, plan)
	}
	for _, constraint := range constraints {
		if !strings.Contains(lower, strings.ToLower(constraint)) {
			t.Fatalf("plan for %s misses %q:\n%s", index, constraint, plan)
		}
	}
	if strings.Contains(strings.ToUpper(plan), "SCAN NODES") || strings.Contains(strings.ToUpper(plan), "SCAN EDGES") {
		t.Fatalf("generation lookup scans a payload table:\n%s", plan)
	}
	if !allowTemp && strings.Contains(strings.ToUpper(plan), "TEMP B-TREE") {
		t.Fatalf("generation lookup lost its index ordering:\n%s", plan)
	}
}

func generationLookupBatchQuery(ids int, projection string) string {
	return `SELECT ` + projection + ` FROM edges WHERE from_id IN (` + inPlaceholders(ids) + `) AND view_gen = ?`
}

func generationLookupBatchArgs(generation, count int) []any {
	args := make([]any, 0, count+1)
	for i := 0; i < count; i++ {
		args = append(args, fmt.Sprintf("repo/source%02d.go::F%05d", i%8, i))
	}
	return append(args, generation)
}

func requireAllGenerationLookupPlans(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, test := range []struct {
		name        string
		query       string
		index       string
		allowTemp   bool
		args        []any
		constraints []string
	}{
		{"name", generationLookupNamePlanQuery, "nodes_by_name", false, []any{"Name03", 3}, []string{"name=?", "view_gen=?"}},
		{"file", generationLookupFilePlanQuery, "nodes_by_file", false, []any{"repo/source03.go", 3}, []string{"file_path=?", "view_gen=?"}},
		{"repo", generationLookupRepoPlanQuery, "nodes_by_repo", false, []any{"repo", 3}, []string{"repo_prefix=?", "view_gen=?"}},
		{"repo_language_name", generationLookupRepoLanguageNamePlanQuery, "nodes_by_repo_language_name", false, []any{"repo", "go", "Name03", 3}, []string{"repo_prefix=?", "language=?", "name=?", "view_gen=?"}},
		{"node_generation", generationLookupNodeGenerationPlanQuery, nodesByGenerationIndexName, false, []any{3}, []string{"view_gen=?"}},
		{"incoming", generationLookupIncomingPlanQuery, "edges_by_to", false, []any{"repo/target.go::T03", 3}, []string{"to_id=?", "view_gen=?"}},
		{"outgoing", generationLookupOutgoingPlanQuery, "edges_by_from", false, []any{"repo/source03.go::F00003", 3}, []string{"from_id=?", "view_gen=?"}},
		{"edge_generation", generationLookupEdgeGenerationPlanQuery, edgesByGenerationIndexName, false, []any{3}, []string{"view_gen=?"}},
		{"repo_projection_gen0", generationLookupRepoProjectionQuery, "nodes_by_repo", true, []any{`["","repo"]`, "directory", "file", 0}, []string{"repo_prefix=?", "view_gen=?"}},
		{"repo_projection_gen3", generationLookupRepoProjectionQuery, "nodes_by_repo", true, []any{`["","repo"]`, "directory", "file", 3}, []string{"repo_prefix=?", "view_gen=?"}},
		{"repo_language_count", generationLookupRepoLanguageCountQuery, "nodes_by_repo", false, []any{"repo", "go", "file", "import", 3}, []string{"repo_prefix=?", "view_gen=?"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireGenerationLookupPlan(t, db, test.query, test.index, test.allowTemp, test.args, test.constraints...)
		})
	}
	batchArgs := generationLookupBatchArgs(3, 128)
	requireGenerationLookupPlan(t, db, generationLookupBatchQuery(128, lookupEdgeCols), "edges_by_from", false, batchArgs, "from_id=?", "view_gen=?")
}

func generationLookupIndexDDL(t testing.TB, db *sql.DB, name string) string {
	t.Helper()
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_schema WHERE type = 'index' AND name = ?`, name).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	return ddl
}

func requireGenerationLookupIndexShapes(t testing.TB, db *sql.DB) {
	t.Helper()
	for _, definition := range generationLookupIndexDefinitions {
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
		if strings.Join(columns, ",") != strings.Join(definition.columns, ",") {
			t.Fatalf("%s columns = %v, want %v", definition.name, columns, definition.columns)
		}
		ddl := generationLookupIndexDDL(t, db, definition.name)
		wantPartial := definition.name == "nodes_by_repo_language_name"
		if gotPartial := strings.Contains(ddl, " WHERE "); gotPartial != wantPartial {
			t.Fatalf("%s partial = %v, want %v: %s", definition.name, gotPartial, wantPartial, ddl)
		}
	}
}

func TestGenerationLookupIndexesSeekWithinGeneration(t *testing.T) {
	db, _ := openGenerationLookupDB(t)
	seedGenerationLookupRows(t, db, 0, 4, 256)

	t.Run("no_statistics", func(t *testing.T) {
		requireAllGenerationLookupPlans(t, db)
	})
	if _, err := db.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}
	t.Run("fresh_statistics", func(t *testing.T) {
		requireAllGenerationLookupPlans(t, db)
	})
	seedGenerationLookupRows(t, db, 4, 4, 256)
	t.Run("valid_stale_statistics", func(t *testing.T) {
		requireAllGenerationLookupPlans(t, db)
	})

	for _, generation := range []int{0, 3} {
		for _, lookup := range generationLookupCases(generation) {
			result := readGenerationLookupResult(t, db, lookup.query, lookup.args...)
			if result.count == 0 {
				t.Errorf("%s generation %d returned no rows", lookup.name, generation)
			}
		}
		projection := generationRepoProjectionResult(t, db, generation)
		if projection.count == 0 {
			t.Errorf("repo projection generation %d returned no rows", generation)
		}
	}
}

func TestGenerationLookupIndexesMigrateWithoutPayloadChanges(t *testing.T) {
	db, path := openGenerationLookupDB(t)
	seedGenerationLookupRows(t, db, 0, 4, 256)
	replaceGenerationLookupIndexes(t, db, false)
	before := generationLookupSnapshot(t, db)
	beforePayload := generationPayloadDigest(t, db)
	if _, err := db.Exec(`PRAGMA user_version = 25`); err != nil {
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
			t.Fatal("index migration requested a source rebuild")
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
		got := generationLookupSnapshot(t, store.db)
		for name, want := range before {
			if got[name] != want {
				t.Errorf("reopen %d %s = %+v, want %+v", reopen, name, got[name], want)
			}
		}
		requireGenerationLookupIndexShapes(t, store.db)
		requireCurrentGenerationLookupDefinitionsShape(t, store.db)
		requireAllGenerationLookupPlans(t, store.db)
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func corruptGenerationLookupRegistryDDL(t *testing.T, name string) func() {
	t.Helper()
	for _, registry := range [][]bulkDroppableIndex{bulkDroppableIndexes, bulkAlwaysLiveIndexes} {
		for i := range registry {
			if registry[i].name != name {
				continue
			}
			old := registry[i].ddl
			registry[i].ddl = `CREATE INDEX ` + name + ` ON missing_generation_lookup_table(view_gen)`
			restored := false
			return func() {
				if restored {
					return
				}
				registry[i].ddl = old
				restored = true
			}
		}
	}
	t.Fatalf("index %s is missing from the production registries", name)
	return func() {}
}

func TestGenerationLookupIndexMigrationRollsBackAtomically(t *testing.T) {
	db, _ := openGenerationLookupDB(t)
	seedGenerationLookupRows(t, db, 0, 4, 128)
	replaceGenerationLookupIndexes(t, db, false)
	beforePayload := generationPayloadDigest(t, db)
	beforeDDL := make(map[string]string, len(generationLookupIndexDefinitions))
	for _, definition := range generationLookupIndexDefinitions {
		beforeDDL[definition.name] = generationLookupIndexDDL(t, db, definition.name)
	}

	restoreRegistry := corruptGenerationLookupRegistryDDL(t, "edges_by_to")
	defer restoreRegistry()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	err = scopeHotGraphIndexesByViewGeneration(tx)
	restoreRegistry()
	if err == nil {
		t.Fatal("migration with invalid canonical DDL succeeded")
	}
	if rollbackErr := tx.Rollback(); rollbackErr != nil {
		t.Fatal(rollbackErr)
	}

	if got := generationPayloadDigest(t, db); got != beforePayload {
		t.Fatalf("failed migration changed payload digest: %x -> %x", beforePayload, got)
	}
	for name, want := range beforeDDL {
		if got := generationLookupIndexDDL(t, db, name); got != want {
			t.Errorf("failed migration changed %s:\n got: %s\nwant: %s", name, got, want)
		}
	}
}

func TestGenerationLookupIndexesRecoverFromTinyStaleStatistics(t *testing.T) {
	if testing.Short() {
		t.Skip("mass-growth planner fixture")
	}
	db, _ := openGenerationLookupDB(t)
	seedGenerationLookupRows(t, db, 0, 1, 13)
	if _, err := db.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}
	seedGenerationLookupRows(t, db, 3, 1, 12000)
	args := generationLookupBatchArgs(3, 128)
	query := generationLookupBatchQuery(128, `id, meta`)
	beforePlan := generationLookupPlan(t, db, query, args...)
	before := readGenerationLookupResult(t, db, query, args...)
	if before.count != 128 {
		t.Fatalf("stale-stat batch returned %d rows, want 128", before.count)
	}
	if strings.Contains(strings.ToUpper(beforePlan), "SCAN EDGES") {
		t.Logf("adversarial stale statistics reproduce the known pre-refresh scan:\n%s", beforePlan)
	} else {
		t.Logf("planner avoided the historical stale-stat scan:\n%s", beforePlan)
	}
	if _, err := db.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}
	requireGenerationLookupPlan(t, db, query, "edges_by_from", false, args, "from_id=?", "view_gen=?")
	after := readGenerationLookupResult(t, db, query, args...)
	if after != before {
		t.Fatalf("statistics refresh changed results: before=%+v after=%+v", before, after)
	}
}

func generationLookupDBBytes(path string) int64 {
	var total int64
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if info, err := os.Stat(candidate); err == nil {
			total += info.Size()
		}
	}
	return total
}

// These benchmarks use real private stores and identical rows for both index
// layouts. They isolate index read/write cost; they do not predict daemon wall
// time or startup latency.
func BenchmarkGenerationLookupIndexReads(b *testing.B) {
	for _, scoped := range []bool{false, true} {
		label := "legacy"
		if scoped {
			label = "generation_scoped"
		}
		b.Run(label, func(b *testing.B) {
			db, path := openGenerationLookupDB(b)
			replaceGenerationLookupIndexes(b, db, false)
			seedGenerationLookupRows(b, db, 0, 16, 2048)
			started := time.Now()
			replaceGenerationLookupIndexes(b, db, scoped)
			buildMillis := float64(time.Since(started).Microseconds()) / 1000
			if _, err := db.Exec(`ANALYZE`); err != nil {
				b.Fatal(err)
			}
			for _, lookup := range generationLookupCases(3) {
				lookup := lookup
				b.Run(lookup.name, func(b *testing.B) {
					want := readGenerationLookupResult(b, db, lookup.query, lookup.args...)
					if want.count == 0 {
						b.Fatal("lookup returned no rows")
					}
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if got := readGenerationLookupResult(b, db, lookup.query, lookup.args...); got != want {
							b.Fatalf("lookup = %+v, want %+v", got, want)
						}
					}
					b.StopTimer()
					b.ReportMetric(float64(want.count), "rows/op")
					b.ReportMetric(buildMillis, "index-build-ms")
					b.ReportMetric(float64(generationLookupDBBytes(path)), "db-bytes")
				})
			}
		})
	}
}

func BenchmarkGenerationLookupIndexWrites(b *testing.B) {
	for _, scoped := range []bool{false, true} {
		label := "legacy"
		if scoped {
			label = "generation_scoped"
		}
		b.Run(label, func(b *testing.B) {
			db, path := openGenerationLookupDB(b)
			replaceGenerationLookupIndexes(b, db, scoped)
			seedGenerationLookupRows(b, db, 0, 4, 1024)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				seedGenerationLookupRows(b, db, i+4, 1, 256)
			}
			b.StopTimer()
			b.ReportMetric(512, "rows/op")
			b.ReportMetric(float64(generationLookupDBBytes(path)), "db-bytes")
		})
	}
}
