package store_sqlite

import (
	"database/sql"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The legacy* fixtures below are the nodes and edges shapes as schema v15 wrote
// them: nodes keyed on id alone and edges deduped without a generation. The
// secondary indexes did not change in v16, but the downgrade drops both tables
// and so has to put them back. They are spelled out literally rather than
// derived from the current DDL for the same reason legacySidecarShapes is — a
// downgrade computed from the live schema would silently track it and stop
// testing the migration at all.
const legacyNodesTableBody = ` (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,
    name          TEXT NOT NULL,
    qual_name     TEXT NOT NULL DEFAULT '',
    file_path     TEXT NOT NULL,
    start_line    INTEGER NOT NULL DEFAULT 0,
    end_line      INTEGER NOT NULL DEFAULT 0,
    start_column  INTEGER NOT NULL DEFAULT 0,
    end_column    INTEGER NOT NULL DEFAULT 0,
    language      TEXT NOT NULL DEFAULT '',
    repo_prefix   TEXT NOT NULL DEFAULT '',
    workspace_id  TEXT NOT NULL DEFAULT '',
    project_id    TEXT NOT NULL DEFAULT '',
    signature     TEXT,
    visibility    TEXT,
    doc           TEXT,
    external      INTEGER,
    return_type   TEXT,
    is_async      INTEGER,
    is_static     INTEGER,
    is_abstract   INTEGER,
    is_exported   INTEGER,
    updated_at    INTEGER,
    data_class    TEXT,
    clone_sig     TEXT,
    meta          BLOB
) WITHOUT ROWID`

const legacyEdgesTableBodyV15 = ` (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    from_id          TEXT NOT NULL,
    to_id            TEXT NOT NULL,
    kind             TEXT NOT NULL,
    file_path        TEXT NOT NULL DEFAULT '',
    line             INTEGER NOT NULL DEFAULT 0,
    confidence       REAL NOT NULL DEFAULT 1.0,
    confidence_label TEXT NOT NULL DEFAULT '',
    origin           TEXT NOT NULL DEFAULT '',
    tier             TEXT NOT NULL DEFAULT '',
    cross_repo       INTEGER NOT NULL DEFAULT 0,
    view_gen         INTEGER NOT NULL DEFAULT 0,
    meta             BLOB,
    UNIQUE(from_id, to_id, kind, file_path, line)
)`

// legacyEdgesTableBodyV13 is the same table one version earlier, before the
// view_gen column existed at all.
const legacyEdgesTableBodyV13 = ` (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    from_id          TEXT NOT NULL,
    to_id            TEXT NOT NULL,
    kind             TEXT NOT NULL,
    file_path        TEXT NOT NULL DEFAULT '',
    line             INTEGER NOT NULL DEFAULT 0,
    confidence       REAL NOT NULL DEFAULT 1.0,
    confidence_label TEXT NOT NULL DEFAULT '',
    origin           TEXT NOT NULL DEFAULT '',
    tier             TEXT NOT NULL DEFAULT '',
    cross_repo       INTEGER NOT NULL DEFAULT 0,
    meta             BLOB,
    UNIQUE(from_id, to_id, kind, file_path, line)
)`

var legacyNodeIndexes = []string{
	`CREATE INDEX nodes_by_name ON nodes(name)`,
	`CREATE INDEX nodes_by_kind ON nodes(kind)`,
	`CREATE INDEX nodes_by_file ON nodes(file_path)`,
	`CREATE INDEX nodes_by_repo ON nodes(repo_prefix) WHERE repo_prefix <> ''`,
	`CREATE INDEX nodes_by_repo_kind ON nodes(repo_prefix, kind)`,
	`CREATE INDEX nodes_by_repo_language_name ON nodes(repo_prefix, language, name) WHERE name <> ''`,
	`CREATE INDEX nodes_by_qual ON nodes(qual_name) WHERE qual_name <> ''`,
	`CREATE INDEX nodes_repo_files ON nodes(repo_prefix, workspace_id, language, file_path, id) WHERE kind = 'file'`,
	`CREATE INDEX nodes_go_receiver_type ON nodes(repo_prefix, file_dir, name, id) WHERE language = 'go' AND kind IN ('type', 'interface') AND name <> '' AND file_path <> ''`,
}

var legacyEdgeIndexes = []string{
	`CREATE INDEX edges_by_from ON edges(from_id, kind)`,
	`CREATE INDEX edges_by_from_line ON edges(from_id, line)`,
	`CREATE INDEX edges_by_from_line_kind ON edges(from_id, line, kind)`,
	`CREATE INDEX edges_by_to ON edges(to_id, kind)`,
	`CREATE INDEX edges_by_kind ON edges(kind)`,
	`CREATE INDEX edges_by_file ON edges(file_path, kind)`,
	`CREATE INDEX edges_by_unresolved ON edges(is_unresolved) WHERE is_unresolved = 1`,
	`CREATE INDEX edges_fnvalue_prefixed ON edges(to_id) WHERE to_id LIKE '%::unresolved::fnvalue::%'`,
	`CREATE INDEX edges_external ON edges(kind) WHERE ` + externalCallTargetPredicate,
}

// downgradeGraphCoreToV15 rewrites nodes and edges back to their v15 shapes,
// keeping only the base-generation rows, and stamps the file at user_version 15.
// The result is a store the shipped v15 build could have written.
func downgradeGraphCoreToV15(t *testing.T, db *sql.DB) {
	t.Helper()
	downgradeCoreTable(t, db, "nodes", legacyNodesTableBody, legacyNodeIndexes,
		func(table string) error {
			if err := ensureNodeColumns(db, table); err != nil {
				return err
			}
			return ensureNodeGeneratedColumns(db, table)
		})
	downgradeCoreTable(t, db, "edges", legacyEdgesTableBodyV15, legacyEdgeIndexes,
		func(table string) error { return ensureEdgeColumns(db, table) })
	execDDL(t, db, `PRAGMA user_version = 15`)
}

// downgradeCoreTable rebuilds one core table into an older shape. reconcile
// re-adds the ALTER-owned promoted and generated columns to the replacement
// under its temporary name, exactly as the forward migration does.
func downgradeCoreTable(t *testing.T, db *sql.DB, table, body string, indexes []string, reconcile func(string) error) {
	t.Helper()
	legacy := table + "_legacy"
	execDDL(t, db, `CREATE TABLE `+legacy+body)
	if err := reconcile(legacy); err != nil {
		t.Fatalf("reconcile %s columns: %v", legacy, err)
	}
	list := strings.Join(sharedTestColumns(t, db, table, legacy), ", ")
	execDDL(t, db, `INSERT INTO `+legacy+` (`+list+`) SELECT `+list+` FROM `+table+` WHERE view_gen = 0`)
	execDDL(t, db, `DROP TABLE `+table)
	execDDL(t, db, `ALTER TABLE `+legacy+` RENAME TO `+table)
	for _, ddl := range indexes {
		execDDL(t, db, ddl)
	}
}

// sharedTestColumns is the downgrade's copy list: the ordinary columns both
// shapes carry. view_gen drops out of it automatically whenever the older shape
// predates the column.
func sharedTestColumns(t *testing.T, db *sql.DB, source, destination string) []string {
	t.Helper()
	from, err := nonGeneratedColumns(db, source)
	if err != nil {
		t.Fatalf("read %s columns: %v", source, err)
	}
	to, err := nonGeneratedColumns(db, destination)
	if err != nil {
		t.Fatalf("read %s columns: %v", destination, err)
	}
	known := make(map[string]bool, len(to))
	for _, name := range to {
		known[name] = true
	}
	var shared []string
	for _, name := range from {
		if known[name] {
			shared = append(shared, name)
		}
	}
	return shared
}

// coreSchemaObjects returns the normalized CREATE text of every nodes / edges
// table and index, keyed by object name. Quotes and whitespace are normalized
// because a table produced by ALTER TABLE … RENAME carries a quoted name and
// the fresh-store one does not.
func coreSchemaObjects(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT name, tbl_name, sql FROM sqlite_schema WHERE sql IS NOT NULL`)
	if err != nil {
		t.Fatalf("read sqlite_schema: %v", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var name, table, ddl string
		if err := rows.Scan(&name, &table, &ddl); err != nil {
			t.Fatalf("scan sqlite_schema: %v", err)
		}
		if table != "nodes" && table != "edges" {
			continue
		}
		out[name] = strings.Join(strings.Fields(strings.ReplaceAll(ddl, `"`, "")), " ")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_schema: %v", err)
	}
	return out
}

const coreSeedRepo = "repo"

const (
	coreCallerID = "repo/pkg/caller.go::Caller"
	coreCalleeID = "repo/pkg/callee.go::Callee"
	coreTypeID   = "repo/pkg/types.go::Widget"
	coreStubID   = "repo::stdlib::fmt"
)

// seedCoreRows writes a graph that exercises everything the rebuild has to
// carry: promoted meta columns, values every generated column derives from
// (a stub id, a directory-bearing path, an unresolved target, a member_of
// receiver), and an edge id gap so the AUTOINCREMENT counter sits above the
// highest surviving row.
func seedCoreRows(t *testing.T, store *Store) {
	t.Helper()
	store.AddBatch([]*graph.Node{
		{
			ID: coreCallerID, Kind: graph.KindFunction, Name: "Caller", QualName: "pkg.Caller",
			FilePath: "repo/pkg/caller.go", StartLine: 10, EndLine: 20, Language: "go",
			RepoPrefix: coreSeedRepo, WorkspaceID: "ws", ProjectID: "proj",
			Meta: map[string]any{
				"signature":        "func Caller()",
				"doc":              "Caller calls.",
				"visibility":       "public",
				"entry_point":      true,
				"entry_point_kind": "cli",
				"search_signature": "caller",
				"section_text":     "body text",
				"custom_blob_key":  "stays in the blob",
			},
		},
		{
			ID: coreCalleeID, Kind: graph.KindFunction, Name: "Callee",
			FilePath: "repo/pkg/callee.go", Language: "go", RepoPrefix: coreSeedRepo,
		},
		{
			ID: coreTypeID, Kind: graph.KindType, Name: "Widget",
			FilePath: "repo/pkg/types.go", Language: "go", RepoPrefix: coreSeedRepo,
		},
		{ID: coreStubID, Kind: graph.KindModule, Name: "fmt", FilePath: "", RepoPrefix: coreSeedRepo},
	}, []*graph.Edge{
		{
			From: coreCallerID, To: coreCalleeID, Kind: graph.EdgeCalls,
			FilePath: "repo/pkg/caller.go", Line: 11, Confidence: 0.9,
			ConfidenceLabel: "ast", Origin: "parser", Tier: "ast",
			Meta: map[string]any{"resolve_terminal": true, "resolve_terminal_reason": "exact", "semantic_source": "lsp"},
		},
		{From: coreCallerID, To: "unresolved::Missing", Kind: graph.EdgeCalls, FilePath: "repo/pkg/caller.go", Line: 12},
		{From: coreCallerID, To: coreTypeID, Kind: graph.EdgeMemberOf, FilePath: "repo/pkg/caller.go", Line: 13},
		{From: coreCallerID, To: coreStubID, Kind: graph.EdgeCalls, FilePath: "repo/pkg/caller.go", Line: 14},
	})
	// Delete one edge so the AUTOINCREMENT counter outruns the highest id the
	// rebuild's copy can reach on its own.
	store.AddBatch(nil, []*graph.Edge{
		{From: coreCallerID, To: coreCalleeID, Kind: graph.EdgeReferences, FilePath: "repo/pkg/caller.go", Line: 15},
	})
	if !store.RemoveEdge(coreCallerID, coreCalleeID, graph.EdgeReferences) {
		t.Fatal("seed: removing the gap edge changed nothing")
	}
}

func edgeRowidSequence(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var seq int64
	if err := db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'edges'`).Scan(&seq); err != nil {
		t.Fatalf("read edges rowid sequence: %v", err)
	}
	return seq
}

// TestSchemaV15StoreGainsCoreViewGenerationKeys is the backward-compatibility
// proof for v16: a store whose nodes and edges were written before either table
// was keyed by view generation is re-keyed on its next Open, every row survives
// in generation 0 with its promoted values and recomputed generated columns, the
// edge id counter keeps climbing, no rebuild is signalled, and the resulting
// shape is byte-identical (modulo quoting) to a store created fresh.
func TestSchemaV15StoreGainsCoreViewGenerationKeys(t *testing.T) {
	if currentSchemaVersion < 16 {
		t.Fatalf("currentSchemaVersion = %d, want >= 16 for the core view generation keys", currentSchemaVersion)
	}
	var step *schemaMigration
	for i := range schemaMigrations {
		if schemaMigrations[i].version == 16 {
			step = &schemaMigrations[i]
			break
		}
	}
	if step == nil || step.rebuild || step.inPlace == nil {
		t.Fatalf("v16 migration = %+v, want a registered in-place step", step)
	}

	path := filepath.Join(t.TempDir(), "pre-core-view-gen.sqlite")
	seed, err := Open(path)
	if err != nil {
		t.Fatalf("create current store: %v", err)
	}
	seedCoreRows(t, seed)
	fresh := coreSchemaObjects(t, seed.writerDB)
	wantNodes := seed.NodeCount()
	wantEdges := seed.EdgeCount()
	wantSequence := edgeRowidSequence(t, seed.writerDB)
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	withRawDB(t, path, func(db *sql.DB) {
		downgradeGraphCoreToV15(t, db)
		// The v15 file's counter is the one the seeded deletion left behind; the
		// downgrade's own copy cannot reproduce it, so restate it here.
		if _, err := db.Exec(`UPDATE sqlite_sequence SET seq = ? WHERE name = 'edges'`, wantSequence); err != nil {
			t.Fatalf("restate the v15 edge id counter: %v", err)
		}
		if hasNodeColumn(t, db, viewGenColumnName) {
			t.Fatal("downgraded nodes table still carries view_gen")
		}
	})

	migrated, err := Open(path)
	if err != nil {
		t.Fatalf("reopen v15 store: %v", err)
	}
	t.Cleanup(func() { _ = migrated.Close() })

	if migrated.NeedsRebuild() {
		t.Fatal("re-keying the core tables in place must not signal a wipe/reindex")
	}
	if version, err := readUserVersion(migrated.writerDB); err != nil || version != currentSchemaVersion {
		t.Fatalf("post-migration user_version = %d (err %v), want %d", version, err, currentSchemaVersion)
	}
	if got := migrated.NodeCount(); got != wantNodes {
		t.Fatalf("node count after migration = %d, want %d", got, wantNodes)
	}
	if got := migrated.EdgeCount(); got != wantEdges {
		t.Fatalf("edge count after migration = %d, want %d", got, wantEdges)
	}
	for _, table := range []string{"nodes", "edges"} {
		if got := scalarInt(t, migrated.writerDB, `SELECT COUNT(*) FROM `+table+` WHERE view_gen <> 0`); got != 0 {
			t.Fatalf("%s has %d rows outside the base generation after migration", table, got)
		}
	}

	// Promoted columns and the JSON blob both survived the copy.
	node := migrated.GetNode(coreCallerID)
	if node == nil {
		t.Fatal("caller node did not survive the migration")
	}
	if node.Meta["signature"] != "func Caller()" || node.Meta["doc"] != "Caller calls." {
		t.Fatalf("promoted node meta after migration = %v", node.Meta)
	}
	if node.Meta["entry_point"] != true || node.Meta["section_text"] != "body text" {
		t.Fatalf("promoted node meta after migration = %v", node.Meta)
	}
	if node.Meta["custom_blob_key"] != "stays in the blob" {
		t.Fatalf("blob node meta after migration = %v", node.Meta)
	}
	if node.QualName != "pkg.Caller" || node.StartLine != 10 || node.WorkspaceID != "ws" {
		t.Fatalf("node identity after migration = %+v", node)
	}

	// Generated columns were re-added and recomputed, not copied.
	if got := scalarInt(t, migrated.writerDB, `SELECT is_stub FROM nodes WHERE id = ?`, coreStubID); got != 1 {
		t.Fatalf("is_stub for the stub node after migration = %d, want 1", got)
	}
	var fileDir string
	if err := migrated.writerDB.QueryRow(`SELECT file_dir FROM nodes WHERE id = ?`, coreCallerID).Scan(&fileDir); err != nil {
		t.Fatalf("read file_dir after migration: %v", err)
	}
	if fileDir != "repo/pkg" {
		t.Fatalf("file_dir after migration = %q, want %q", fileDir, "repo/pkg")
	}
	if got := scalarInt(t, migrated.writerDB, `SELECT COUNT(*) FROM edges WHERE is_unresolved = 1`); got != 1 {
		t.Fatalf("unresolved edges after migration = %d, want 1", got)
	}
	if got := scalarInt(t, migrated.writerDB,
		`SELECT COUNT(*) FROM edges WHERE member_receiver = 'Widget'`); got != 1 {
		t.Fatalf("member_receiver projections after migration = %d, want 1", got)
	}

	// Promoted edge columns rode across too.
	var reason, source string
	if err := migrated.writerDB.QueryRow(
		`SELECT resolve_terminal_reason, semantic_source FROM edges WHERE to_id = ?`, coreCalleeID,
	).Scan(&reason, &source); err != nil {
		t.Fatalf("read promoted edge columns after migration: %v", err)
	}
	if reason != "exact" || source != "lsp" {
		t.Fatalf("promoted edge columns after migration = (%q, %q)", reason, source)
	}

	// The AUTOINCREMENT counter did not rewind: the next edge id is above every
	// id the migration carried across, and above the pre-migration counter.
	if got := edgeRowidSequence(t, migrated.writerDB); got != wantSequence {
		t.Fatalf("edges rowid sequence after migration = %d, want %d", got, wantSequence)
	}
	migrated.AddBatch(nil, []*graph.Edge{
		{From: coreCallerID, To: coreTypeID, Kind: graph.EdgeReferences, FilePath: "repo/pkg/caller.go", Line: 16},
	})
	var newID int64
	if err := migrated.writerDB.QueryRow(
		`SELECT id FROM edges WHERE from_id = ? AND to_id = ? AND kind = ?`,
		coreCallerID, coreTypeID, string(graph.EdgeReferences),
	).Scan(&newID); err != nil {
		t.Fatalf("read post-migration edge id: %v", err)
	}
	if newID <= wantSequence {
		t.Fatalf("post-migration edge id = %d, want above the preserved sequence %d", newID, wantSequence)
	}

	// The migrated shape matches a store this build creates from scratch.
	got := coreSchemaObjects(t, migrated.writerDB)
	if len(got) != len(fresh) {
		t.Fatalf("migrated store has %d core schema objects, fresh store has %d", len(got), len(fresh))
	}
	names := make([]string, 0, len(fresh))
	for name := range fresh {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if got[name] != fresh[name] {
			t.Fatalf("core object %q differs after migration:\n migrated: %s\n    fresh: %s", name, got[name], fresh[name])
		}
	}
}

// TestSchemaCoreViewGenerationMigrationIsIdempotent covers the case the probes
// exist for: schemaSQL runs before the migration steps, so a store built by this
// build already carries both keys when the v16 step runs. Re-running it must be
// a no-op rather than a duplicated table or a lost row.
func TestSchemaCoreViewGenerationMigrationIsIdempotent(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "idempotent-core-view-gen.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	seedCoreRows(t, store)
	wantNodes := store.NodeCount()
	wantEdges := store.EdgeCount()

	for i := 0; i < 2; i++ {
		tx, err := store.writerDB.Begin()
		if err != nil {
			t.Fatalf("begin migration tx %d: %v", i, err)
		}
		if err := keyGraphCoreByViewGeneration(tx); err != nil {
			_ = tx.Rollback()
			t.Fatalf("keyGraphCoreByViewGeneration run %d: %v", i, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit migration tx %d: %v", i, err)
		}
	}

	if got := store.NodeCount(); got != wantNodes {
		t.Fatalf("node count after repeated migration runs = %d, want %d", got, wantNodes)
	}
	if got := store.EdgeCount(); got != wantEdges {
		t.Fatalf("edge count after repeated migration runs = %d, want %d", got, wantEdges)
	}
	if got := scalarInt(t, store.writerDB,
		`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name LIKE '%_view_gen_rebuild'`); got != 0 {
		t.Fatalf("repeated migration runs left %d rebuild tables behind", got)
	}
}

// TestCoreUpsertsRetargetConflictToViewGeneration proves the write path follows
// the new keys through both entry points: writing the same logical row twice
// still leaves one row carrying the second write's payload, and re-adding an
// identical batch is still a no-op. Before the conflict target named view_gen
// the second node write failed the constraint outright.
func TestCoreUpsertsRetargetConflictToViewGeneration(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "core-upsert.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	store.AddNode(&graph.Node{
		ID: coreCallerID, Kind: graph.KindFunction, Name: "Caller",
		FilePath: "repo/pkg/caller.go", RepoPrefix: coreSeedRepo,
	})
	store.AddNode(&graph.Node{
		ID: coreCallerID, Kind: graph.KindFunction, Name: "CallerRenamed",
		FilePath: "repo/pkg/caller.go", RepoPrefix: coreSeedRepo,
	})
	if got := scalarInt(t, store.writerDB, `SELECT COUNT(*) FROM nodes WHERE id = ?`, coreCallerID); got != 1 {
		t.Fatalf("node rows after re-upsert = %d, want 1", got)
	}
	if node := store.GetNode(coreCallerID); node == nil || node.Name != "CallerRenamed" {
		t.Fatalf("node after re-upsert = %+v, want the second write's payload", node)
	}

	batch := []*graph.Node{{
		ID: coreCalleeID, Kind: graph.KindFunction, Name: "Callee",
		FilePath: "repo/pkg/callee.go", RepoPrefix: coreSeedRepo,
	}}
	edges := []*graph.Edge{{
		From: coreCallerID, To: coreCalleeID, Kind: graph.EdgeCalls,
		FilePath: "repo/pkg/caller.go", Line: 11,
	}}
	store.AddBatch(batch, edges)
	stats, err := store.addBatchSetOriented(batch, edges)
	if err != nil {
		t.Fatalf("re-add identical batch: %v", err)
	}
	if stats.nodeRowsChanged != 0 || stats.edgeRowsInserted != 0 {
		t.Fatalf("re-adding an identical batch changed rows: %+v", stats)
	}
	if got := scalarInt(t, store.writerDB, `SELECT COUNT(*) FROM edges`); got != 1 {
		t.Fatalf("edge rows after re-add = %d, want 1", got)
	}
}

// TestAtGenerationCoreWriteIsInvisibleToBasePointReads is the write-scoping
// proof: a derived handle's node and edge land in its own generation, and the
// two point statements that probe the changed identities — the nodes primary
// key and the edges unique key — do not see them from the base handle. Every
// other read stays generation-blind at this stage, so this deliberately checks
// only those two.
func TestAtGenerationCoreWriteIsInvisibleToBasePointReads(t *testing.T) {
	base, err := Open(filepath.Join(t.TempDir(), "core-generation-scope.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer base.Close()

	baseEdge := &graph.Edge{
		From: coreCallerID, To: coreCalleeID, Kind: graph.EdgeCalls,
		FilePath: "repo/pkg/caller.go", Line: 11,
	}
	base.AddBatch([]*graph.Node{{
		ID: coreCallerID, Kind: graph.KindFunction, Name: "Caller",
		FilePath: "repo/pkg/caller.go", RepoPrefix: coreSeedRepo,
	}}, []*graph.Edge{baseEdge})

	derived := base.AtGeneration(1)
	if derived == nil || derived.ViewGeneration() != 1 {
		t.Fatalf("AtGeneration(1) = %v, want a handle pinned to generation 1", derived)
	}
	derivedEdge := &graph.Edge{
		From: coreCallerID, To: coreTypeID, Kind: graph.EdgeCalls,
		FilePath: "repo/pkg/caller.go", Line: 12,
	}
	derived.AddBatch([]*graph.Node{{
		ID: coreTypeID, Kind: graph.KindType, Name: "Widget",
		FilePath: "repo/pkg/types.go", RepoPrefix: coreSeedRepo,
	}}, []*graph.Edge{derivedEdge})

	var gen int64
	if err := base.writerDB.QueryRow(`SELECT view_gen FROM nodes WHERE id = ?`, coreTypeID).Scan(&gen); err != nil {
		t.Fatalf("read derived node generation: %v", err)
	}
	if gen != 1 {
		t.Fatalf("derived handle wrote a node at view_gen %d, want 1", gen)
	}
	if err := base.writerDB.QueryRow(`SELECT view_gen FROM edges WHERE to_id = ?`, coreTypeID).Scan(&gen); err != nil {
		t.Fatalf("read derived edge generation: %v", err)
	}
	if gen != 1 {
		t.Fatalf("derived handle wrote an edge at view_gen %d, want 1", gen)
	}

	if base.GetNode(coreTypeID) != nil {
		t.Fatal("base handle's point lookup sees the derived generation's node")
	}
	if derived.GetNode(coreTypeID) == nil {
		t.Fatal("derived handle cannot read back its own node")
	}
	if base.GetNode(coreCallerID) == nil {
		t.Fatal("base handle lost its own node")
	}
	if derived.GetNode(coreCallerID) != nil {
		t.Fatal("derived handle sees the base generation's node")
	}

	if base.EdgeExists(derivedEdge.From, derivedEdge.To, derivedEdge.Kind, derivedEdge.FilePath, derivedEdge.Line) {
		t.Fatal("base handle's identity probe sees the derived generation's edge")
	}
	if !derived.EdgeExists(derivedEdge.From, derivedEdge.To, derivedEdge.Kind, derivedEdge.FilePath, derivedEdge.Line) {
		t.Fatal("derived handle cannot see its own edge")
	}
	if !base.EdgeExists(baseEdge.From, baseEdge.To, baseEdge.Kind, baseEdge.FilePath, baseEdge.Line) {
		t.Fatal("base handle lost its own edge")
	}
	if derived.EdgeExists(baseEdge.From, baseEdge.To, baseEdge.Kind, baseEdge.FilePath, baseEdge.Line) {
		t.Fatal("derived handle sees the base generation's edge")
	}

	// Both generations' rows coexist rather than one having replaced the other.
	if got := scalarInt(t, base.writerDB, `SELECT COUNT(*) FROM nodes`); got != 2 {
		t.Fatalf("node rows across both generations = %d, want 2", got)
	}
	if got := scalarInt(t, base.writerDB, `SELECT COUNT(*) FROM edges`); got != 2 {
		t.Fatalf("edge rows across both generations = %d, want 2", got)
	}
}
