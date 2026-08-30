package store_sqlite

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSchemaV13LineagesConvergeAtV14(t *testing.T) {
	require.Equal(t, 19, currentSchemaVersion)
	plan := planSchemaMigrationWith(13, currentSchemaVersion, schemaMigrations)
	var pending []int
	for _, migration := range plan.inPlace {
		pending = append(pending, migration.version)
	}
	require.Equal(t, []int{14, 15, 16, 17, 18, 19}, pending)

	for _, tc := range []struct {
		name       string
		featureV13 bool
	}{
		{name: "main-v13"},
		{name: "feature-v13", featureV13: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openCoverageMigrationDB(t)
			_, err := db.Exec(`CREATE TABLE edges (id INTEGER PRIMARY KEY)`)
			require.NoError(t, err)
			if tc.featureV13 {
				_, err = db.Exec(checkoutCatalogSchemaSQL)
				require.NoError(t, err, "feature v13 already carries the checkout catalog")
			}

			for pass := 1; pass <= 2; pass++ {
				tx, err := db.Begin()
				require.NoError(t, err)
				require.NoError(t, addCheckoutCatalogAndEdgeViewGeneration(tx), "pass %d", pass)
				require.NoError(t, tx.Commit())
			}

			assertSQLCount(t, db, 1,
				`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'checkouts'`)
			assertSQLCount(t, db, 1,
				`SELECT COUNT(*) FROM pragma_table_xinfo('edges') WHERE name = 'view_gen'`)
		})
	}
}

func TestCoveragePurgeDispatchesToShippedFlatV13Shape(t *testing.T) {
	db := openCoverageMigrationDB(t)
	_, err := db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY, kind TEXT NOT NULL, name TEXT NOT NULL,
			file_path TEXT NOT NULL, repo_prefix TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE edges (
			id INTEGER PRIMARY KEY, from_id TEXT NOT NULL, to_id TEXT NOT NULL,
			kind TEXT NOT NULL, file_path TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE files (
			repo_prefix TEXT NOT NULL DEFAULT '', file_path TEXT NOT NULL,
			PRIMARY KEY (repo_prefix, file_path)
		);
		CREATE VIRTUAL TABLE symbol_fts USING fts5(
			node_id UNINDEXED, repo_prefix UNINDEXED, tokens
		);
		CREATE TABLE symbol_fts_rowid (
			node_id TEXT PRIMARY KEY, repo_prefix TEXT NOT NULL DEFAULT '',
			fts_rowid INTEGER NOT NULL
		);
		CREATE TABLE ref_facts (from_id TEXT NOT NULL, to_id TEXT NOT NULL);
	`)
	require.NoError(t, err)
	for _, table := range coverageNodeSidecarTables {
		_, err := db.Exec(`CREATE TABLE ` + table + ` (node_id TEXT PRIMARY KEY)`)
		require.NoError(t, err, table)
	}

	const (
		native = `r/src\a.go`
		legacy = `r/src/a.go`
		doomed = `same-node-id`
	)
	_, err = db.Exec(`INSERT INTO nodes(id, kind, name, file_path, repo_prefix)
		VALUES (?, 'file', 'a.go', ?, 'r'), (?, 'todo', 'todo', ?, 'r')`,
		native, native, doomed, legacy)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO edges(from_id, to_id, kind, file_path)
		VALUES (?, ?, 'annotated', ?)`, legacy, doomed, legacy)
	require.NoError(t, err)

	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, purgeLegacyCoverageSpellings(tx))
	require.NoError(t, tx.Commit())
	assertSQLCount(t, db, 0, `SELECT COUNT(*) FROM nodes WHERE id = ?`, doomed)
	assertSQLCount(t, db, 0, `SELECT COUNT(*) FROM edges WHERE to_id = ?`, doomed)
	assertSQLCount(t, db, 1, `SELECT COUNT(*) FROM nodes WHERE id = ?`, native)
}

func TestCoveragePurgeMixedPreRekeyShapeIsFlatOnlyAtGenerationZero(t *testing.T) {
	db := openCoverageMigrationDB(t)
	_, err := db.Exec(`
		CREATE TABLE nodes (id TEXT NOT NULL, view_gen INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (id, view_gen));
		CREATE TABLE edges (id INTEGER PRIMARY KEY, view_gen INTEGER NOT NULL DEFAULT 0);
		CREATE TABLE files (repo_prefix TEXT NOT NULL, file_path TEXT NOT NULL,
			PRIMARY KEY (repo_prefix, file_path));
	`)
	require.NoError(t, err)
	tx, err := db.Begin()
	require.NoError(t, err)
	scoped, err := coveragePurgeUsesViewGeneration(tx)
	require.NoError(t, err)
	require.False(t, scoped, "schemaSQL-created generation-zero tables beside flat v13 tables use the shipped flat purge")
	require.NoError(t, tx.Rollback())

	_, err = db.Exec(`INSERT INTO nodes(id, view_gen) VALUES ('overlay-node', 9)`)
	require.NoError(t, err)
	tx, err = db.Begin()
	require.NoError(t, err)
	_, err = coveragePurgeUsesViewGeneration(tx)
	require.ErrorContains(t, err, "partially generation-keyed schema")
	require.NoError(t, tx.Rollback())
}

func TestGenerationScopedCoveragePurgeIsolatesSiblingGenerationsAndMasks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	store, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	db := openCoverageMigrationDBAt(t, path)
	const (
		doomedGen  = int64(101)
		siblingGen = int64(202)
		sparseGen  = int64(303)
		baseGen    = int64(0)
		native     = `r/src\a.go`
		legacy     = `r/src/a.go`
		nodeID     = `same-node-id`
		sparseID   = `sparse-overlay-node`
	)

	_, err = db.Exec(`INSERT INTO nodes(id, view_gen, kind, name, file_path, repo_prefix) VALUES
			(?, ?, 'file', 'a.go', ?, 'r'),
			(?, ?, 'todo', 'legacy', ?, 'r'),
			(?, ?, 'file', 'a.go', ?, 'r'),
			(?, ?, 'todo', 'live sibling', ?, 'r'),
			(?, ?, 'file', 'base a.go', ?, 'r'),
			('r/other\marker.go', ?, 'file', 'marker.go', 'r/other\marker.go', 'r'),
			(?, ?, 'todo', 'inherited twin unavailable', ?, 'r')
	`,
		native, doomedGen, native,
		nodeID, doomedGen, legacy,
		native, siblingGen, native,
		nodeID, siblingGen, native,
		native, baseGen, native,
		sparseGen,
		sparseID, sparseGen, legacy)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO edges(from_id, to_id, kind, file_path, view_gen) VALUES
			(?, ?, 'annotated', ?, ?),
			(?, ?, 'annotated', ?, ?),
			(?, ?, 'annotated', ?, ?)`,
		legacy, nodeID, legacy, doomedGen,
		native, nodeID, native, siblingGen,
		legacy, sparseID, legacy, sparseGen)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO files(view_gen, repo_prefix, file_path) VALUES
			(?, 'r', ?), (?, 'r', ?), (?, 'r', ?)`,
		doomedGen, native, siblingGen, native, baseGen, native)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO symbol_fts(rowid, node_id, repo_prefix, tokens) VALUES
			(1101, ?, 'r', 'doomed'), (1202, ?, 'r', 'sibling')`, nodeID, nodeID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO symbol_fts_rowid(view_gen, node_id, repo_prefix, fts_rowid) VALUES
			(?, ?, 'r', 1101), (?, ?, 'r', 1202)`,
		doomedGen, nodeID, siblingGen, nodeID,
	)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO constant_values(view_gen, node_id, repo_prefix, file_path, value) VALUES
			(?, ?, 'r', ?, 'doomed'), (?, ?, 'r', ?, 'sibling')`,
		doomedGen, nodeID, legacy, siblingGen, nodeID, native,
	)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO ref_facts(view_gen, repo_prefix, from_id, to_id, kind) VALUES
			(?, 'r', ?, ?, 'references'), (?, 'r', ?, ?, 'references')`,
		doomedGen, nodeID, nodeID, siblingGen, nodeID, nodeID,
	)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO generation_file_masks(view_gen, repo_prefix, file_path, ownership_mode) VALUES
			(?, 'r', ?, 'replace'), (?, 'r', ?, 'replace')`,
		doomedGen, legacy, siblingGen, native,
	)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO generation_node_tombstones(view_gen, node_id) VALUES
			(?, ?), (?, ?)`,
		doomedGen, nodeID, siblingGen, nodeID,
	)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO generation_edge_sources(view_gen, source_id, ownership_mode) VALUES
			(?, ?, 'replace'), (?, ?, 'replace')`,
		doomedGen, nodeID, siblingGen, nodeID,
	)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO generation_producer_completeness(view_gen, producer, state, reason) VALUES
			(?, 'coverage', 'incomplete', 'preserve negative state'),
			(?, 'coverage', 'complete', '')`, doomedGen, siblingGen)
	require.NoError(t, err)

	for pass := 1; pass <= 2; pass++ {
		tx, err := db.Begin()
		require.NoError(t, err)
		require.NoError(t, purgeLegacyCoverageSpellings(tx), "pass %d", pass)
		require.NoError(t, tx.Commit())
	}

	for _, tableAndKey := range []struct {
		table string
		key   string
	}{
		{table: "nodes", key: "id"},
		{table: "symbol_fts_rowid", key: "node_id"},
		{table: "constant_values", key: "node_id"},
	} {
		assertSQLCount(t, db, 0,
			fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE view_gen = ? AND %s = ?`, tableAndKey.table, tableAndKey.key),
			doomedGen, nodeID)
		assertSQLCount(t, db, 1,
			fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE view_gen = ? AND %s = ?`, tableAndKey.table, tableAndKey.key),
			siblingGen, nodeID)
	}
	assertSQLCount(t, db, 0, `SELECT COUNT(*) FROM edges WHERE view_gen = ? AND to_id = ?`, doomedGen, nodeID)
	assertSQLCount(t, db, 1, `SELECT COUNT(*) FROM edges WHERE view_gen = ? AND to_id = ?`, siblingGen, nodeID)
	assertSQLCount(t, db, 0, `SELECT COUNT(*) FROM ref_facts WHERE view_gen = ? AND to_id = ?`, doomedGen, nodeID)
	assertSQLCount(t, db, 1, `SELECT COUNT(*) FROM ref_facts WHERE view_gen = ? AND to_id = ?`, siblingGen, nodeID)
	assertSQLCount(t, db, 1, `SELECT COUNT(*) FROM generation_node_tombstones WHERE view_gen = ? AND node_id = ?`, doomedGen, nodeID)
	assertSQLCount(t, db, 1, `SELECT COUNT(*) FROM generation_node_tombstones WHERE view_gen = ? AND node_id = ?`, siblingGen, nodeID)
	assertSQLCount(t, db, 1, `SELECT COUNT(*) FROM generation_edge_sources WHERE view_gen = ? AND source_id = ?`, doomedGen, nodeID)
	assertSQLCount(t, db, 1, `SELECT COUNT(*) FROM generation_edge_sources WHERE view_gen = ? AND source_id = ?`, siblingGen, nodeID)
	assertSQLCount(t, db, 1, `SELECT COUNT(*) FROM generation_producer_completeness WHERE view_gen = ? AND producer = 'coverage'`, doomedGen)
	assertSQLCount(t, db, 1, `SELECT COUNT(*) FROM generation_producer_completeness WHERE view_gen = ? AND producer = 'coverage'`, siblingGen)
	assertSQLString(t, db, "delete", `SELECT ownership_mode FROM generation_file_masks WHERE view_gen = ? AND file_path = ?`, doomedGen, legacy)
	assertSQLString(t, db, "replace", `SELECT ownership_mode FROM generation_file_masks WHERE view_gen = ? AND file_path = ?`, siblingGen, native)
	assertSQLCount(t, db, 0, `SELECT COUNT(*) FROM symbol_fts WHERE rowid = 1101`)
	assertSQLCount(t, db, 1, `SELECT COUNT(*) FROM symbol_fts WHERE rowid = 1202`)

	// The sparse generation sees a Windows repo, but its native twin lives
	// only in the base generation. The migration cannot safely reconstruct the
	// stack, so both the node and its edge are deliberately preserved.
	assertSQLCount(t, db, 1, `SELECT COUNT(*) FROM nodes WHERE view_gen = ? AND id = ?`, sparseGen, sparseID)
	assertSQLCount(t, db, 1, `SELECT COUNT(*) FROM edges WHERE view_gen = ? AND to_id = ?`, sparseGen, sparseID)

	maskStore, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, maskStore.AtGeneration(doomedGen).ValidateGenerationMasks())
	require.NoError(t, maskStore.AtGeneration(siblingGen).ValidateGenerationMasks())
	require.NoError(t, maskStore.Close())
}

func TestGenerationScopedCoveragePurgeOrphanPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	store, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	db := openCoverageMigrationDBAt(t, path)

	const (
		baseGen      = int64(1)
		sparseGen    = int64(404)
		dedicatedGen = int64(505)
		sparseNative = `r/sparse\a.go`
		sparseLegacy = `r/sparse/a.go`
		sparseTarget = `r/license::sparse`
		baseFile     = `r/base\a.go`
		dedNative    = `r/dedicated\a.go`
		dedLegacy    = `r/dedicated/a.go`
		dedTarget    = `r/license::dedicated`
	)
	_, err = db.Exec(`INSERT INTO view_generations(
		generation_id, owner_kind, generation_kind, base_generation_id, state
	) VALUES
		(?, 'dedicated_base', 'dedicated_base', NULL, 'ready'),
		(?, 'commit_overlay', 'commit_overlay', ?, 'ready'),
		(?, 'dedicated_base', 'dedicated_base', NULL, 'ready')`,
		baseGen, sparseGen, baseGen, dedicatedGen)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO nodes(id, view_gen, kind, name, file_path, repo_prefix) VALUES
		(?, ?, 'file', 'base.go', ?, 'r'),
		(?, ?, 'license', 'sparse base', ?, 'r'),
		(?, ?, 'file', 'sparse.go', ?, 'r'),
		(?, ?, 'license', 'sparse overlay', ?, 'r'),
		(?, ?, 'file', 'dedicated.go', ?, 'r'),
		(?, ?, 'license', 'dedicated target', ?, 'r')`,
		baseFile, baseGen, baseFile,
		sparseTarget, baseGen, baseFile,
		sparseNative, sparseGen, sparseNative,
		sparseTarget, sparseGen, sparseLegacy,
		dedNative, dedicatedGen, dedNative,
		dedTarget, dedicatedGen, dedLegacy)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO edges(from_id, to_id, kind, file_path, view_gen) VALUES
		(?, ?, 'licensed_as', ?, ?),
		(?, ?, 'licensed_as', ?, ?),
		(?, ?, 'licensed_as', ?, ?)`,
		baseFile, sparseTarget, baseFile, baseGen,
		sparseLegacy, sparseTarget, sparseLegacy, sparseGen,
		dedLegacy, dedTarget, dedLegacy, dedicatedGen)
	require.NoError(t, err)

	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, purgeLegacyCoverageSpellings(tx))
	require.NoError(t, tx.Commit())

	assertSQLCount(t, db, 0, `SELECT COUNT(*) FROM edges WHERE view_gen = ? AND file_path = ?`, sparseGen, sparseLegacy)
	assertSQLCount(t, db, 1, `SELECT COUNT(*) FROM nodes WHERE view_gen = ? AND id = ?`, sparseGen, sparseTarget)
	assertSQLCount(t, db, 1, `SELECT COUNT(*) FROM nodes WHERE view_gen = ? AND id = ?`, baseGen, sparseTarget)
	assertSQLCount(t, db, 0, `SELECT COUNT(*) FROM edges WHERE view_gen = ? AND file_path = ?`, dedicatedGen, dedLegacy)
	assertSQLCount(t, db, 0, `SELECT COUNT(*) FROM nodes WHERE view_gen = ? AND id = ?`, dedicatedGen, dedTarget)
}

func TestOpenReplaysGenerationScopedCoveragePurgeFromFeatureV18(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	store, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	const (
		generation = int64(77)
		native     = `r/src\a.go`
		legacy     = `r/src/a.go`
		doomed     = `feature-v18-legacy-node`
	)
	withRawDB(t, path, func(db *sql.DB) {
		_, err := db.Exec(`INSERT INTO nodes(id, view_gen, kind, name, file_path, repo_prefix) VALUES
				(?, ?, 'file', 'a.go', ?, 'r'),
				(?, ?, 'todo', 'legacy', ?, 'r')`,
			native, generation, native, doomed, generation, legacy)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO edges(from_id, to_id, kind, file_path, view_gen)
			VALUES (?, ?, 'annotated', ?, ?)`, legacy, doomed, legacy, generation)
		require.NoError(t, err)
		_, err = db.Exec(`PRAGMA user_version = 18`)
		require.NoError(t, err)
	})

	store, err = Open(path)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	withRawDB(t, path, func(db *sql.DB) {
		assertSQLCount(t, db, 0, `SELECT COUNT(*) FROM nodes WHERE view_gen = ? AND id = ?`, generation, doomed)
		var version int
		require.NoError(t, db.QueryRow(`PRAGMA user_version`).Scan(&version))
		require.Equal(t, 19, version)
	})
}

func BenchmarkPurgeLegacyCoverageSpellingsV18Noop(b *testing.B) {
	path := filepath.Join(b.TempDir(), "store.sqlite")
	store, err := Open(path)
	require.NoError(b, err)
	require.NoError(b, store.Close())

	db, err := sql.Open("sqlite", path)
	require.NoError(b, err)
	b.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	for generation := int64(1); generation <= 8; generation++ {
		path := fmt.Sprintf(`r/src\file-%d.go`, generation)
		_, err := db.Exec(`INSERT INTO nodes(id, view_gen, kind, name, file_path, repo_prefix)
			VALUES (?, ?, 'file', 'file.go', ?, 'r')`, path, generation, path)
		require.NoError(b, err)
	}
	_, err = db.Exec(`PRAGMA user_version = 18`)
	require.NoError(b, err)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx, err := db.Begin()
		if err != nil {
			b.Fatal(err)
		}
		if err := purgeLegacyCoverageSpellings(tx); err != nil {
			_ = tx.Rollback()
			b.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			b.Fatal(err)
		}
	}
}

func openCoverageMigrationDB(t *testing.T) *sql.DB {
	t.Helper()
	return openCoverageMigrationDBAt(t, filepath.Join(t.TempDir(), "store.sqlite"))
}

func openCoverageMigrationDBAt(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func assertSQLCount(t *testing.T, db *sql.DB, want int, query string, args ...any) {
	t.Helper()
	var got int
	require.NoError(t, db.QueryRow(query, args...).Scan(&got))
	require.Equal(t, want, got, query)
}

func assertSQLString(t *testing.T, db *sql.DB, want, query string, args ...any) {
	t.Helper()
	var got string
	require.NoError(t, db.QueryRow(query, args...).Scan(&got))
	require.Equal(t, want, got, query)
}
