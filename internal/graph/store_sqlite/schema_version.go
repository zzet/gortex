package store_sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Schema versioning for the graph store.
//
// Unlike the sidecar (which holds irreplaceable user data and must migrate in
// place), the graph store is a DERIVED CACHE: every row is reconstructable by
// re-indexing the source. So the cheapest *always-correct* reaction to a schema
// change an old on-disk DB can't satisfy is to drop the file and let the daemon
// rebuild it on the next index. A migration may therefore declare rebuild=true
// instead of writing an in-place transform that would have to re-derive the new
// data from source anyway. In-place steps remain the cheap path for purely
// mechanical changes (a new index, a denormalisation, a column with a
// computable default) that spare a large repo a multi-minute reindex.
//
// The whole mechanism keys off SQLite's built-in PRAGMA user_version, read on
// Open before schemaSQL runs. There is no separate version table.
//
// Concurrency: the daemon holds an exclusive flock on <store>.lock around Open
// (see serverstack.NewSharedServer), so reading the version, wiping the file,
// and stamping it cannot race another process. That is why — unlike the
// sidecar — this path needs no BEGIN IMMEDIATE / busy-loop handling.

// currentSchemaVersion is the version a fully-reconciled store reports via
// PRAGMA user_version. Bump it whenever schemaSQL's typed-column shape or an
// index changes in a way an old on-disk DB would not already have, and append a
// matching schemaMigrations entry describing how to bring an older store
// forward (in place, or by rebuild).
const currentSchemaVersion = 19

// schemaMigration is one forward step. Exactly one strategy applies:
//   - rebuild=true: the change introduces structure/data that can only come
//     from re-indexing the source; an older store is wiped and rebuilt.
//   - inPlace!=nil: the change is mechanically derivable from the existing
//     store and is applied in a transaction with no reindex.
//
// Steps are append-only and ascending; never edit or renumber a shipped one.
// Any inPlace step must be idempotent (IF NOT EXISTS / ADD COLUMN guarded).
type schemaMigration struct {
	version int
	name    string
	inPlace func(tx *sql.Tx) error
	rebuild bool
}

// MigrationPhase identifies one observable boundary in an in-place schema
// migration. Observers are advisory: they must not block the migration or
// mutate the graph database.
type MigrationPhase string

const (
	MigrationStarted  MigrationPhase = "started"
	MigrationFinished MigrationPhase = "finished"
	MigrationFailed   MigrationPhase = "failed"
)

// MigrationProgress describes one schema migration step. Elapsed is measured
// from the start of the current step and Error is populated only for a failed
// step. The error is intended for logs and transient startup state; callers
// should avoid persisting it as durable graph data.
type MigrationProgress struct {
	Version int
	Name    string
	Phase   MigrationPhase
	Elapsed time.Duration
	Error   error
}

// MigrationObserver receives synchronous migration boundaries. It may be nil.
// Implementations should return promptly; a slow observer delays store Open.
type MigrationObserver func(MigrationProgress)

// schemaMigrations is the ordered, forward-only registry. Version 1 is the
// implicit baseline (no entry): a v1 store is reconciled entirely by schemaSQL's
// idempotent CREATE ... IF NOT EXISTS plus ensureNodeColumns, so any
// pre-versioning database baseline-stamps to v1 without a rebuild. Append
// entries for version 2 and up as the schema evolves.
var schemaMigrations = []schemaMigration{
	{version: 2, name: "dedupe fn-value placeholder edges", inPlace: dedupeFnValuePlaceholderEdges},
	// Versions through v2 wrote node updates with INSERT OR REPLACE. REPLACE
	// has delete semantics and can invalidate incident-edge integrity when
	// foreign-key enforcement is enabled by a host/connection. Deleted edges
	// cannot be reconstructed from the remaining graph rows, so this is an
	// explicit source-reindex boundary rather than a misleading in-place fix.
	{version: 3, name: "restore topology after node replace writes", rebuild: true},
	{version: 4, name: "add normalized analysis generations", inPlace: createAnalysisGenerationTables},
	{version: 5, name: "backfill flat graph ownership, provenance, and clone corpus", inPlace: backfillSyntheticNodeRepoPrefixes},
	{version: 6, name: "compact resolver edge indexes", inPlace: compactResolverEdgeIndexes},
	// Single-repo (unprefixed) mode is gone: every repo's nodes now carry
	// its prefix. A store written before the flip holds a solo repo's
	// file-backed nodes under repo_prefix='', which nothing else can reach
	// or evict, and the first post-upgrade warmup writes a full prefixed
	// copy beside them. Purge the old population here — see the function
	// for why in-place beats rebuild and why global externals survive.
	{version: 7, name: "purge unprefixed solo-repo rows", inPlace: purgeUnprefixedRepoRows},
	{version: 8, name: "allow duplicate qualified names", inPlace: relaxNodeQualNameUniqueness},
	{version: 9, name: "drop unused semantic pending index", inPlace: dropUnusedSemanticPendingIndex},
	// Vector ownership and chunk-parent identity cannot be reconstructed from
	// the legacy (node_id, dims, vec) rows for every ID shape. Rebuild only the
	// derived vector sidecar rather than discarding otherwise-valid topology.
	{version: 10, name: "rebuild vector corpus ownership and parents", inPlace: rebuildVectorCorpusSchema},
	{version: 11, name: "add symbol FTS normalization state", inPlace: createSymbolFTSNormalizationStateTable},
	{version: 12, name: "normalize dir column separators", inPlace: normalizeDirColumnSeparators},
	// v13 shipped on main before the overlay lineage merged. Never renumber or
	// replace it: old flat stores must run the original purge before any payload
	// table gains a generation key.
	{version: 13, name: "purge legacy slash-spelled coverage artifacts", inPlace: purgeLegacyCoverageSpellings},
	// Feature-lineage v13 stores already have the catalog, while main-lineage
	// v13 stores have neither it nor edges.view_gen. Both operations are
	// idempotent, so one v14 step safely converges both histories.
	{version: 14, name: "add checkout catalog and edges view generation", inPlace: addCheckoutCatalogAndEdgeViewGeneration},
	{version: 15, name: "key payload sidecars by view generation", inPlace: addSidecarViewGenerationKeys},
	{version: 16, name: "key nodes and edges by view generation", inPlace: keyGraphCoreByViewGeneration},
	{version: 17, name: "add sparse view-generation enumeration indexes", inPlace: addGenerationEnumerationIndexes},
	{version: 18, name: "add sparse generation ownership masks", inPlace: createGenerationMaskTables},
	// Feature-lineage stores reached v18 without main's v13 coverage purge.
	// Replay it after the generation re-key; the purge dispatches on schema
	// shape and isolates every decision and delete by view_gen.
	{version: 19, name: "replay generation-scoped legacy coverage purge", inPlace: purgeLegacyCoverageSpellings},
}

// createGenerationMaskTables is the explicit v18 migration. The mask tables are
// purely additive — they sit beside the payload tables and re-key nothing — so
// an older store gains them without a reindex, and an existing generation
// simply has no masks until something writes them. schemaSQL owns the canonical
// fresh-store definition and runs first; this step repeats the same idempotent
// DDL so the addition is part of the versioned contract rather than an
// unversioned side effect of Open.
func createGenerationMaskTables(tx *sql.Tx) error {
	_, err := tx.Exec(generationMaskSchemaSQL)
	return err
}

// addGenerationEnumerationIndexes is the explicit migration step for the two
// partial view-generation indexes. createGraphCoreIndexes already builds them
// on every Open from the same shared DDL, so this step exists to make the
// addition part of the versioned contract: a store stamped v17 is one whose
// sparse-generation enumeration path is known to be indexed.
func addGenerationEnumerationIndexes(tx *sql.Tx) error {
	for _, ddl := range []string{nodesByGenerationIndexDDL, edgesByGenerationIndexDDL} {
		if _, err := tx.Exec(ddl); err != nil {
			return err
		}
	}
	return nil
}

// keyGraphCoreByViewGeneration re-keys the two core payload tables on the view
// generation their rows belong to: nodes on an (id, view_gen) primary key, edges
// on a UNIQUE(from_id, to_id, kind, file_path, line, view_gen) dedup key. The
// sidecars gained a generation key in v15; these are the last two payload tables
// where a second generation's row would otherwise collide with the base corpus's
// instead of sitting beside it. Unlike the sidecars, whose reads were retargeted
// in the same change, view_gen goes at the END of both keys so that every
// generation-blind read in the package keeps the identical plan it had on the
// id-only key (see nodesTableBody).
//
// Neither a primary key nor a table constraint can be altered in place, so both
// tables are rebuilt. Every existing row is copied at generation 0 — the single
// base corpus they already belong to — so no data is re-derived and no reindex
// is needed. Both rebuilds and the index recreation share the caller's single
// migration transaction, so a failure anywhere leaves the store exactly as it
// was.
func keyGraphCoreByViewGeneration(tx *sql.Tx) error {
	if err := rebuildNodesAtBaseGeneration(tx); err != nil {
		return fmt.Errorf("nodes: %w", err)
	}
	if err := rebuildEdgesAtBaseGeneration(tx); err != nil {
		return fmt.Errorf("edges: %w", err)
	}
	// Dropping a table drops its indexes with it, so both tables are indexless
	// at this point; one call rebuilds the whole core set under its existing
	// names, which every INDEXED BY site in the package names literally and
	// fails closed on.
	return createGraphCoreIndexes(tx)
}

// rebuildNodesAtBaseGeneration replaces nodes with an (id, view_gen)-keyed table
// holding the same rows at generation 0.
//
// The replacement is created from the canonical body and then reconciled by the
// same ensure helpers Open uses, so the promoted / struct columns an older
// store accumulated by ALTER are present before the copy and the generated
// columns are recomputed rather than copied — INSERT ... SELECT may not name a
// generated column on either side. The copy list is the intersection of both
// tables' ordinary columns, so a column one side does not know about can never
// turn the copy into a silent error.
//
// Idempotent: nodes gains view_gen only here, so its presence means the rebuild
// already ran.
func rebuildNodesAtBaseGeneration(tx *sql.Tx) error {
	var count int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_xinfo('nodes') WHERE name = ?`,
		viewGenColumnName,
	).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	const rebuilt = "nodes_view_gen_rebuild"
	if _, err := tx.Exec(`CREATE TABLE ` + rebuilt + nodesTableBody); err != nil {
		return err
	}
	if err := ensureNodeColumns(tx, rebuilt); err != nil {
		return err
	}
	if err := ensureNodeGeneratedColumns(tx, rebuilt); err != nil {
		return err
	}
	columns, err := sharedCopyColumns(tx, "nodes", rebuilt)
	if err != nil {
		return err
	}
	if err := copyRowsAtBaseGeneration(tx, "nodes", rebuilt, columns); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE nodes`); err != nil {
		return err
	}
	_, err = tx.Exec(`ALTER TABLE ` + rebuilt + ` RENAME TO nodes`)
	return err
}

// rebuildEdgesAtBaseGeneration replaces edges with a table whose dedup key ends
// with view_gen, holding the same rows at generation 0.
//
// edges.id stays an AUTOINCREMENT primary key: it names a physical row, not a
// logical edge, and callers page and order by it. AUTOINCREMENT means the next
// id comes from sqlite_sequence, which DROP TABLE deletes and the copy resets
// to the highest id actually carried across — lower than the original whenever
// rows were ever deleted. Capturing the counter before the drop and restoring
// it after the rename keeps ids strictly increasing across the migration, so no
// id a caller still holds can be handed out a second time.
//
// The edges view_gen COLUMN already exists (v14), so its presence proves
// nothing here; the probe instead asks whether the unique constraint's own
// index carries it. Membership, not position — the key order is a plan
// decision and a later step may reorder it without re-running this rebuild.
func rebuildEdgesAtBaseGeneration(tx *sql.Tx) error {
	var keyed int
	if err := tx.QueryRow(`
SELECT COUNT(*)
FROM pragma_index_list('edges') AS il
JOIN pragma_index_info(il.name) AS ii
WHERE il.origin = 'u' AND ii.name = ?`, viewGenColumnName).Scan(&keyed); err != nil {
		return err
	}
	if keyed > 0 {
		return nil
	}
	var sequence sql.NullInt64
	if err := tx.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'edges'`).Scan(&sequence); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return err
	}
	const rebuilt = "edges_view_gen_rebuild"
	if _, err := tx.Exec(`CREATE TABLE ` + rebuilt + edgesTableBody); err != nil {
		return err
	}
	if err := ensureEdgeColumns(tx, rebuilt); err != nil {
		return err
	}
	columns, err := sharedCopyColumns(tx, "edges", rebuilt)
	if err != nil {
		return err
	}
	if err := copyRowsAtBaseGeneration(tx, "edges", rebuilt, columns); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE edges`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE ` + rebuilt + ` RENAME TO edges`); err != nil {
		return err
	}
	return restoreEdgeRowidSequence(tx, sequence)
}

// restoreEdgeRowidSequence lifts the rebuilt edges table's AUTOINCREMENT
// counter back to the value the original carried. sqlite_sequence has no unique
// index on name, so this updates the row the rename carried across rather than
// INSERT OR REPLACE, which would silently leave two rows for one table and let
// SQLite read whichever it found first.
func restoreEdgeRowidSequence(tx *sql.Tx, sequence sql.NullInt64) error {
	if !sequence.Valid {
		return nil
	}
	result, err := tx.Exec(
		`UPDATE sqlite_sequence SET seq = ? WHERE name = 'edges' AND seq < ?`,
		sequence.Int64, sequence.Int64,
	)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated > 0 {
		return nil
	}
	// No row to raise: either the copy already reached a higher id, or it moved
	// no rows at all and the rebuilt table has no counter yet.
	var present int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_sequence WHERE name = 'edges'`).Scan(&present); err != nil {
		return err
	}
	if present > 0 {
		return nil
	}
	_, err = tx.Exec(`INSERT INTO sqlite_sequence(name, seq) VALUES ('edges', ?)`, sequence.Int64)
	return err
}

// sharedCopyColumns returns the ordinary columns both tables carry, in the
// source table's order. Generated columns are excluded on both sides because
// SQLite computes them; view_gen is excluded because the copy supplies it.
func sharedCopyColumns(tx *sql.Tx, source, destination string) ([]string, error) {
	from, err := nonGeneratedColumns(tx, source)
	if err != nil {
		return nil, err
	}
	to, err := nonGeneratedColumns(tx, destination)
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(to))
	for _, name := range to {
		known[name] = true
	}
	shared := make([]string, 0, len(from))
	for _, name := range from {
		if name == viewGenColumnName || !known[name] {
			continue
		}
		shared = append(shared, name)
	}
	if len(shared) == 0 {
		return nil, fmt.Errorf("%s and %s share no copyable column", source, destination)
	}
	return shared, nil
}

// copyRowsAtBaseGeneration moves every row across at generation 0 through an
// explicit column list — never SELECT *, which would silently depend on column
// order matching between two independently built tables.
func copyRowsAtBaseGeneration(tx *sql.Tx, source, destination string, columns []string) error {
	list := strings.Join(columns, ", ")
	_, err := tx.Exec(`INSERT INTO ` + destination + ` (` + viewGenColumnName + `, ` + list + `)
SELECT 0, ` + list + ` FROM ` + source)
	return err
}

// addSidecarViewGenerationKeys re-keys every WITHOUT ROWID payload sidecar on
// a leading view_gen column. A sidecar row belongs to exactly one payload view
// generation, so the generation has to lead the primary key: without it a
// second generation's row for the same (repo, file) or node id would collide
// with the base corpus's row instead of sitting beside it.
//
// A primary key cannot be altered in place, so each table is rebuilt: create
// the replacement, copy every row across at generation 0 through an explicit
// column list, drop the old table, rename, and recreate its secondary indexes
// under their existing names. Generation 0 is the single base corpus every
// existing row already belongs to, so the copy preserves all data and needs no
// reindex. The whole registry runs inside the one migration transaction, so a
// failure on any table leaves the store exactly as it was.
//
// Idempotent: a table that already carries view_gen is skipped. schemaSQL runs
// before the migration steps, so on a fresh store every sidecar is already
// re-keyed when this runs, and the v10 vector rebuild creates its table from
// the same registry body.
func addSidecarViewGenerationKeys(tx *sql.Tx) error {
	for _, sidecar := range viewGenSidecars {
		if err := rebuildSidecarAtBaseGeneration(tx, sidecar); err != nil {
			return fmt.Errorf("%s: %w", sidecar.table, err)
		}
	}
	return nil
}

func rebuildSidecarAtBaseGeneration(tx *sql.Tx, sidecar viewGenSidecar) error {
	var count int
	probe := fmt.Sprintf(`SELECT COUNT(*) FROM pragma_table_xinfo('%s') WHERE name = ?`, sidecar.table)
	if err := tx.QueryRow(probe, sidecarViewGenColumnName).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	rebuilt := sidecar.table + "_view_gen_rebuild"
	if _, err := tx.Exec(`CREATE TABLE ` + rebuilt + sidecar.body); err != nil {
		return err
	}
	copyRows := `INSERT INTO ` + rebuilt + ` (` + sidecarViewGenColumnName + `, ` + sidecar.columns + `)
SELECT 0, ` + sidecar.columns + ` FROM ` + sidecar.table
	if _, err := tx.Exec(copyRows); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE ` + sidecar.table); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE ` + rebuilt + ` RENAME TO ` + sidecar.table); err != nil {
		return err
	}
	for _, ddl := range sidecar.indexes {
		if _, err := tx.Exec(ddl); err != nil {
			return err
		}
	}
	return nil
}

// addEdgeViewGenerationColumn adds edges.view_gen to a store whose edges table
// was created before the column existed. Purely additive: every existing row
// takes the column default, generation 0, which is the single base corpus they
// already belong to — no backfill, no reindex.
//
// schemaSQL owns the fresh-store definition and runs before the migration
// steps, so this is a no-op there. The probe reads pragma_table_xinfo rather
// than table_info for the same reason ensureEdgeColumns does: table_info omits
// generated columns, and one probe shape that lists every column is the one
// worth reusing. Idempotent — a second run finds the column and returns.
func addEdgeViewGenerationColumn(tx *sql.Tx) error {
	var count int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_xinfo('edges') WHERE name = ?`,
		viewGenColumnName,
	).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	_, err := tx.Exec(`ALTER TABLE edges ADD COLUMN ` + edgeViewGenColumnDDL)
	return err
}

// createCheckoutCatalogTables is the explicit v13 migration for existing
// stores. The catalog is purely additive — it adds tables beside the payload
// ones and re-keys nothing — so an older store gains it without a reindex.
// schemaSQL owns the canonical fresh-store definition and runs first; this
// step repeats the same idempotent DDL so the addition is part of the
// versioned contract rather than an unversioned side effect of Open.
func createCheckoutCatalogTables(tx *sql.Tx) error {
	_, err := tx.Exec(checkoutCatalogSchemaSQL)
	return err
}

// addCheckoutCatalogAndEdgeViewGeneration is the convergence point for the
// two schema-v13 lineages. Main-v13 stores need both additions; feature-v13
// stores already have the catalog. Each operation is idempotent and the schema
// runner wraps this helper in one transaction, so either lineage reaches the
// exact same v14 shape atomically.
func addCheckoutCatalogAndEdgeViewGeneration(tx *sql.Tx) error {
	if err := createCheckoutCatalogTables(tx); err != nil {
		return err
	}
	return addEdgeViewGenerationColumn(tx)
}

// normalizeDirColumnSeparators rebuilds the two generated dir columns whose
// pre-v12 expressions trimmed at '/' only. Stored paths keep the writing
// platform's native separators below the repo prefix, so a Windows-written
// store collapsed both dirs to the repo prefix and the Go receiver-rebind
// join degenerated from "same package dir" to "same repo". A generated
// column cannot be redefined in place: drop the index over file_dir, drop
// and re-add both columns with the current DDL, and recreate the index from
// the shared always-live set so its DDL cannot drift. Idempotent — a re-run
// re-adds the same expressions, and the existence guards tolerate a store
// where a column is already absent or already current.
func normalizeDirColumnSeparators(tx *sql.Tx) error {
	if _, err := tx.Exec(`DROP INDEX IF EXISTS nodes_go_receiver_type`); err != nil {
		return err
	}
	for _, col := range []struct{ table, name, ddl string }{
		{"nodes", "file_dir", fileDirColumnDDL},
		{"edges", "member_receiver_dir", memberReceiverDirColumnDDL},
	} {
		var count int
		q := fmt.Sprintf(`SELECT COUNT(*) FROM pragma_table_xinfo('%s') WHERE name = ?`, col.table)
		if err := tx.QueryRow(q, col.name).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			if _, err := tx.Exec(`ALTER TABLE ` + col.table + ` DROP COLUMN ` + col.name); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`ALTER TABLE ` + col.table + ` ADD COLUMN ` + col.ddl); err != nil {
			return err
		}
	}
	for _, idx := range bulkAlwaysLiveIndexes {
		if idx.name == "nodes_go_receiver_type" {
			_, err := tx.Exec(idx.ddl)
			return err
		}
	}
	return fmt.Errorf("nodes_go_receiver_type missing from bulkAlwaysLiveIndexes")
}

// createSymbolFTSNormalizationStateTable is the explicit v11 migration for
// existing stores. schemaSQL owns the canonical fresh-store definition; this
// idempotent step makes the additive table part of the versioned contract.
func createSymbolFTSNormalizationStateTable(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS symbol_fts_state (
		repo_prefix TEXT PRIMARY KEY,
		normalization TEXT NOT NULL DEFAULT ''
	) WITHOUT ROWID`)
	return err
}

// dropUnusedSemanticPendingIndex removes an experimental index for a query
// shape that was never shipped. It was dense throughout parsing (semantic_type
// starts NULL), adding write and cold-finalization cost without a consumer.
func dropUnusedSemanticPendingIndex(tx *sql.Tx) error {
	_, err := tx.Exec(`DROP INDEX IF EXISTS nodes_semantic_pending`)
	return err
}

// rebuildVectorCorpusSchema intentionally discards only the durable vector
// sidecar. Legacy rows do not carry repository ownership or chunk parents, so
// retaining them would make per-repository replacement and warm de-chunking
// unsound. The graph topology remains intact and the next embedding pass
// repopulates this derived cache.
func rebuildVectorCorpusSchema(tx *sql.Tx) error {
	if _, err := tx.Exec(`DROP TABLE IF EXISTS vectors`); err != nil {
		return err
	}
	_, err := tx.Exec(vectorTableSQL)
	return err
}

// relaxNodeQualNameUniqueness removes the historical assumption that a
// language-level qualified name is a global graph identity. Resource manifests,
// forks, worktrees and overload-like constructs may legitimately repeat one.
// The replacement keeps the same name, key and partial predicate so existing
// point and batch lookups retain their indexed plans.
func relaxNodeQualNameUniqueness(tx *sql.Tx) error {
	if _, err := tx.Exec(`DROP INDEX IF EXISTS nodes_by_qual`); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE INDEX nodes_by_qual ON nodes(qual_name) WHERE qual_name <> ''`)
	return err
}

// compactResolverEdgeIndexes removes the one-shot global Go receiver index and
// replaces the dense Boolean unresolved index with an unresolved-only partial
// index. Both changes are mechanically derivable from existing flat/generated
// columns, so a populated v5 store upgrades transactionally without reindexing
// source. A CREATE failure rolls the DROP operations back with the migration.
func compactResolverEdgeIndexes(tx *sql.Tx) error {
	if _, err := tx.Exec(`DROP INDEX IF EXISTS edges_go_member_receiver`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP INDEX IF EXISTS edges_by_unresolved`); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE INDEX edges_by_unresolved ON edges(is_unresolved) WHERE is_unresolved = 1`)
	return err
}

// backfillSyntheticNodeRepoPrefixes repairs rows written before synthetic
// resolver nodes consistently carried Node.RepoPrefix, then promotes legacy
// edge semantic_source values through a bounded Meta migration. Repo-scoped stub IDs
// have the form <repo>::<stub-kind>::..., so ownership is derivable without
// reading Meta or source files. Shared legacy dep:: / external:: IDs start
// directly with their kind and deliberately remain global.
//
// This is a single set-oriented UPDATE that runs once while upgrading v4 to
// v5. Keeping it in the schema migration avoids both an N+1 rewrite and a
// warm-start scan on every subsequent Open.
func backfillSyntheticNodeRepoPrefixes(tx *sql.Tx) error {
	if err := ensureCloneCorpusColumns(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(`
UPDATE nodes
SET repo_prefix = substr(id, 1, instr(id, '::') - 1)
WHERE repo_prefix = ''
  AND instr(id, '::') > 0
  AND (
      substr(id, instr(id, '::') + 2) LIKE 'module::%'
      OR substr(id, instr(id, '::') + 2) LIKE 'stdlib::%'
      OR substr(id, instr(id, '::') + 2) LIKE 'builtin::%'
      OR substr(id, instr(id, '::') + 2) LIKE 'external_call::%'
  )`); err != nil {
		return err
	}
	return backfillEdgeSemanticSources(tx)
}

// The v4→v5 edge provenance migration is deliberately bounded in two
// dimensions. A page never retains more than pageRows candidate blobs or
// pageBytes of their encoded Meta (apart from one individually oversized row),
// while updateRows keeps each VALUES statement below SQLite's conservative
// 999-host-parameter ceiling (300 * 3 = 900).
//
// The SQL query first searches the BLOB for the literal key bytes. All three
// codecs accepted by decodeMeta — flat, JSON, and legacy gob — store map keys as
// their UTF-8 bytes, so this is a no-false-negative prefilter. A value or nested
// key may produce a harmless false positive; Go still performs the authoritative
// top-level type check before rewriting a row.
const (
	edgeSemanticSourceMigrationPageRows   = 4096
	edgeSemanticSourceMigrationPageBytes  = int64(8 << 20)
	edgeSemanticSourceMigrationUpdateRows = 300
	edgeSemanticSourceMetaMarker          = "semantic_source"
)

type edgeSemanticSourceMigrationRow struct {
	id     int64
	source string
	meta   []byte
}

type edgeSemanticSourceMigrationLimits struct {
	pageRows   int
	pageBytes  int64
	updateRows int
}

var defaultEdgeSemanticSourceMigrationLimits = edgeSemanticSourceMigrationLimits{
	pageRows:   edgeSemanticSourceMigrationPageRows,
	pageBytes:  edgeSemanticSourceMigrationPageBytes,
	updateRows: edgeSemanticSourceMigrationUpdateRows,
}

// edgeSemanticSourceMigrationStats is intentionally package-private test
// instrumentation. It makes the warm-up cost contract observable without a
// driver-specific query hook or production logging.
type edgeSemanticSourceMigrationStats struct {
	PageQueries      int
	RowsDecoded      int
	BytesDecoded     int64
	RowsUpdated      int
	UpdateStatements int
	MaxPageRows      int
	MaxPageBytes     int64
}

// backfillEdgeSemanticSources lifts Meta["semantic_source"] into its flat edge
// column without ever materialising the edge corpus. Pages advance by the
// stable integer edge id, and each page is rewritten with one VALUES-driven
// UPDATE in the caller's migration transaction. A failed page rolls the whole
// migration back; rerunning is idempotent because already-promoted rows no
// longer satisfy semantic_source IS NULL.
func backfillEdgeSemanticSources(tx *sql.Tx) error {
	_, err := backfillEdgeSemanticSourcesWithLimits(tx, defaultEdgeSemanticSourceMigrationLimits)
	return err
}

func backfillEdgeSemanticSourcesWithLimits(
	tx *sql.Tx,
	limits edgeSemanticSourceMigrationLimits,
) (edgeSemanticSourceMigrationStats, error) {
	var stats edgeSemanticSourceMigrationStats
	if limits.pageRows <= 0 || limits.pageBytes <= 0 || limits.updateRows <= 0 {
		return stats, fmt.Errorf("invalid edge semantic_source migration limits: rows=%d bytes=%d updates=%d",
			limits.pageRows, limits.pageBytes, limits.updateRows)
	}

	var afterID int64
	for {
		stats.PageQueries++
		rows, err := tx.Query(`
SELECT id, meta
FROM edges
WHERE id > ?
  AND semantic_source IS NULL
  AND meta IS NOT NULL
  AND instr(CAST(meta AS BLOB), ?) > 0
ORDER BY id
LIMIT ?`, afterID, []byte(edgeSemanticSourceMetaMarker), limits.pageRows)
		if err != nil {
			return stats, err
		}

		read := 0
		var pageBytes int64
		byteLimited := false
		updates := make([]edgeSemanticSourceMigrationRow, 0, min(limits.pageRows, limits.updateRows))
		for rows.Next() {
			var id int64
			var blob sql.RawBytes
			if err := rows.Scan(&id, &blob); err != nil {
				_ = rows.Close()
				return stats, err
			}
			// Do not advance afterID until this row has actually been decoded. If
			// accepting it would cross the byte cap, close this result and let
			// the next keyset query return the same row as its first candidate.
			// One row larger than the cap is accepted alone to guarantee progress.
			blobBytes := int64(len(blob))
			if read > 0 && pageBytes+blobBytes > limits.pageBytes {
				byteLimited = true
				break
			}
			read++
			afterID = id
			pageBytes += blobBytes
			stats.RowsDecoded++
			stats.BytesDecoded += blobBytes
			meta, err := decodeMeta(blob)
			if err != nil {
				_ = rows.Close()
				return stats, fmt.Errorf("decode edge %d meta: %w", id, err)
			}
			source, ok := meta["semantic_source"].(string)
			if !ok {
				if pageBytes >= limits.pageBytes {
					byteLimited = true
					break
				}
				continue
			}
			delete(meta, edgeSemanticSourceMetaMarker)
			remaining, err := encodeMeta(meta)
			if err != nil {
				_ = rows.Close()
				return stats, fmt.Errorf("encode edge %d meta: %w", id, err)
			}
			updates = append(updates, edgeSemanticSourceMigrationRow{id: id, source: source, meta: remaining})
			if pageBytes >= limits.pageBytes {
				byteLimited = true
				break
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return stats, err
		}
		if err := rows.Close(); err != nil {
			return stats, err
		}

		if read > stats.MaxPageRows {
			stats.MaxPageRows = read
		}
		if pageBytes > stats.MaxPageBytes {
			stats.MaxPageBytes = pageBytes
		}
		for start := 0; start < len(updates); start += limits.updateRows {
			end := min(start+limits.updateRows, len(updates))
			if err := applyEdgeSemanticSourceMigrationUpdates(tx, updates[start:end]); err != nil {
				return stats, err
			}
			stats.UpdateStatements++
			stats.RowsUpdated += end - start
		}
		if !byteLimited && read < limits.pageRows {
			return stats, nil
		}
	}
}

func applyEdgeSemanticSourceMigrationUpdates(tx *sql.Tx, updates []edgeSemanticSourceMigrationRow) error {
	if len(updates) == 0 {
		return nil
	}
	var values strings.Builder
	args := make([]any, 0, len(updates)*3)
	for i, update := range updates {
		if i > 0 {
			values.WriteByte(',')
		}
		values.WriteString("(?,?,?)")
		args = append(args, update.id, update.source, update.meta)
	}
	query := `WITH updates(id, semantic_source, meta) AS (VALUES ` + values.String() + `)
UPDATE edges AS e
SET semantic_source = u.semantic_source, meta = u.meta
FROM updates AS u
WHERE e.id = u.id AND e.semantic_source IS NULL`
	_, err := tx.Exec(query, args...)
	return err
}

// createAnalysisGenerationTables is the explicit v4 in-place migration.
// schemaSQL runs first and is intentionally idempotent, so this is a no-op on
// fresh stores and a defensive create on older stores opened by migration
// tests or future alternate open paths.
func createAnalysisGenerationTables(tx *sql.Tx) error {
	if _, err := tx.Exec(analysisGenerationSchemaSQL); err != nil {
		return err
	}
	// Builds used during development briefly created a blob-only cache under
	// schema v3. It was never released; remove the artifact instead of carrying
	// a conversion or compatibility API into v4.
	_, err := tx.Exec(`DROP TABLE IF EXISTS analysis_cache`)
	return err
}

// dedupeFnValuePlaceholderEdges collapses duplicate function-as-value gate
// placeholder edges (graph.FnValuePlaceholderMarker, `unresolved::fnvalue::
// <name>`) to one row per (from_id, to_id), keeping the MIN(id) survivor. The
// capture path now dedups per (from, name) before it emits, but stores written
// earlier accumulated one placeholder per call site — a live store held
// millions — and EdgesWithUnresolvedTarget plus the resolver's terminal
// reconcile materialised every one on each warm restart, the dominant warmup
// heap transient this step drains. The keep set is small (tens of thousands of
// distinct pairs), so the NOT IN materialisation is cheap; the ph filter rides
// the edges_by_to(to_id) range for the bare form and the is_unresolved index for
// the multi-repo infix form. Idempotent: a second run finds no duplicates. Freed
// pages return to the freelist and are reused by later writes; the file itself
// shrinks only under a manual VACUUM, deliberately out of scope for a derived
// cache that reclaims the space on its own.
func dedupeFnValuePlaceholderEdges(tx *sql.Tx) error {
	_, err := tx.Exec(`
WITH ph AS (
    SELECT id, from_id, to_id FROM edges
    WHERE (to_id >= 'unresolved::fnvalue::' AND to_id < 'unresolved::fnvalue:;')
       OR (is_unresolved = 1 AND to_id LIKE '%::unresolved::fnvalue::%')
), keep AS (
    SELECT MIN(id) AS id FROM ph GROUP BY from_id, to_id
)
DELETE FROM edges WHERE id IN (SELECT id FROM ph) AND id NOT IN (SELECT id FROM keep)`)
	return err
}

// schemaPlan is the decision planSchemaMigration derives from the stored
// PRAGMA user_version. It mutates nothing on its own.
type schemaPlan struct {
	wipe    bool              // drop the on-disk DB and rebuild from source
	inPlace []schemaMigration // ordered in-place steps to run after schemaSQL
	stamp   bool              // write currentSchemaVersion once reconciled
}

// planSchemaMigrationWith decides how to reconcile a store at the stored
// PRAGMA user_version to current, given the migration registry. It mutates
// nothing. Open passes (currentSchemaVersion, schemaMigrations); tests pass
// fixtures.
func planSchemaMigrationWith(stored, current int, migrations []schemaMigration) schemaPlan {
	switch {
	case stored == current:
		return schemaPlan{} // up to date, nothing to do
	case stored > current:
		// Written by a newer build than this binary understands; the shape may
		// have changed under us. For a cache the safe move is to rebuild.
		return schemaPlan{wipe: true, stamp: true}
	case stored == 0:
		// Fresh DB, or a pre-versioning store of unknown shape. schemaSQL's
		// idempotent CREATE ... IF NOT EXISTS plus ensureNodeColumns /
		// ensureEdgeColumns reconcile the base shape either way, so a stored==0
		// store needs a wipe only when a pending step is a REBUILD whose data can
		// only come from re-indexing source. With nothing pending, stamp; with
		// only in-place steps pending, run them and stamp — an in-place step is
		// idempotent and mechanically derivable, so it upgrades a pre-versioning
		// store in place (preserving its rows) exactly as it upgrades a known
		// prior version. Wiping a stored==0 store on any migration instead would
		// force every non-daemon Open (tests, read-only tools) to pass WithRebuild
		// the moment the first migration ships.
		pending := pendingBetween(0, current, migrations)
		if len(pending) == 0 {
			return schemaPlan{stamp: true}
		}
		if anyRebuild(pending) {
			return schemaPlan{wipe: true, stamp: true}
		}
		return schemaPlan{inPlace: pending, stamp: true}
	default: // 0 < stored < current: a known prior version
		pending := pendingBetween(stored, current, migrations)
		if anyRebuild(pending) {
			return schemaPlan{wipe: true, stamp: true}
		}
		return schemaPlan{inPlace: pending, stamp: true}
	}
}

func pendingBetween(stored, current int, migrations []schemaMigration) []schemaMigration {
	var out []schemaMigration
	for _, m := range migrations {
		if m.version > stored && m.version <= current {
			out = append(out, m)
		}
	}
	return out
}

func anyRebuild(ms []schemaMigration) bool {
	for _, m := range ms {
		if m.rebuild {
			return true
		}
	}
	return false
}

// validateSchemaMigrations checks the registry is well-formed. A test asserts
// this against the shipped (currentSchemaVersion, schemaMigrations) so the
// dangerous mistake — bumping currentSchemaVersion without appending a matching
// entry — fails CI instead of silently baseline-stamping an un-migrated store
// to the new version at runtime. Rules:
//   - versions are >= 2 (v1 is the implicit baseline, never an entry) and
//     strictly ascending;
//   - each step sets exactly one strategy (inPlace xor rebuild);
//   - the highest version equals current, so the registry actually defines how
//     to reach it. An empty registry is valid only at version 1.
func validateSchemaMigrations(current int, migs []schemaMigration) error {
	if len(migs) == 0 {
		if current != 1 {
			return fmt.Errorf("schema version %d has no migrations: only v1 may have an empty registry", current)
		}
		return nil
	}
	prev := 0
	for i, m := range migs {
		if m.version < 2 {
			return fmt.Errorf("migration %q has version %d: entries must be >= 2 (v1 is the implicit baseline)", m.name, m.version)
		}
		if i > 0 && m.version <= prev {
			return fmt.Errorf("migrations must be strictly ascending: v%d (%s) does not follow v%d", m.version, m.name, prev)
		}
		if (m.inPlace != nil) == m.rebuild {
			return fmt.Errorf("migration v%d (%s) must set exactly one of inPlace / rebuild", m.version, m.name)
		}
		prev = m.version
	}
	if prev != current {
		return fmt.Errorf("highest migration version %d != currentSchemaVersion %d: a version bump needs a matching migration entry", prev, current)
	}
	return nil
}

// readUserVersion reads PRAGMA user_version (0 on a fresh database).
func readUserVersion(db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

// setUserVersion stamps the schema version. PRAGMA takes no bound parameters;
// v is an int we control, so the format is safe.
func setUserVersion(db *sql.DB, v int) error {
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		return err
	}
	return nil
}

// applyInPlaceMigrations runs the in-place steps in a single transaction.
func applyInPlaceMigrations(db *sql.DB, steps []schemaMigration, observers ...MigrationObserver) error {
	if len(steps) == 0 {
		return nil
	}
	var observe MigrationObserver
	if len(observers) > 0 {
		observe = observers[0]
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op once Commit succeeds
	for _, m := range steps {
		started := time.Now()
		if observe != nil {
			observe(MigrationProgress{Version: m.version, Name: m.name, Phase: MigrationStarted})
		}
		if err := m.inPlace(tx); err != nil {
			if observe != nil {
				observe(MigrationProgress{
					Version: m.version,
					Name:    m.name,
					Phase:   MigrationFailed,
					Elapsed: time.Since(started),
					Error:   err,
				})
			}
			return fmt.Errorf("schema migration v%d (%s): %w", m.version, m.name, err)
		}
		if observe != nil {
			observe(MigrationProgress{
				Version: m.version,
				Name:    m.name,
				Phase:   MigrationFinished,
				Elapsed: time.Since(started),
			})
		}
	}
	return tx.Commit()
}

// removeStoreFiles deletes the SQLite database and its companions. A missing
// file is not an error. Never called for ":memory:".
//
// The suffix list covers the files the DSN's journal_mode(WAL) produces (-wal,
// -shm) plus the rollback -journal a non-WAL fallback would use; keep it in
// sync if the journal_mode in Open's DSN ever changes.
func removeStoreFiles(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path+suffix, err)
		}
	}
	return nil
}

// isMemoryPath reports whether path is an in-process SQLite database (no file
// on disk to wipe, always built fresh by schemaSQL).
func isMemoryPath(path string) bool {
	return strings.Contains(path, ":memory:")
}
