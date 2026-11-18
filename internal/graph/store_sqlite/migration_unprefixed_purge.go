package store_sqlite

import (
	"database/sql"
	"fmt"
)

// purgeUnprefixedRepoRows is the v6→v7 in-place step that retires
// single-repo (unprefixed) mode's on-disk residue.
//
// WHY A MIGRATION IS REQUIRED. Before v7 a workspace's only repo was indexed
// WITHOUT a prefix: its nodes carried an empty repo_prefix and raw
// repo-relative ids ("internal/foo.go::Bar"). The indexer now always
// prefixes, so on the first warmup after upgrading the same repo is
// re-indexed under "<repo>/internal/foo.go::Bar". Nothing removes the old
// rows — they belong to no tracked prefix, so PurgeRepo never sees them and
// OrphanRepoPrefixes deliberately exempts the empty prefix. The store would
// carry both copies: roughly double the nodes, doubled search hits, per-repo
// counts disagreeing with whole-store counts, and index_health reporting
// green over all of it. That is a silent wrong answer, which is why it is a
// schema boundary and not a best-effort cleanup.
//
// WHY IN-PLACE RATHER THAN rebuild:true. A rebuild is three lines and always
// correct, but planSchemaMigrationWith reads the flag statically — there is
// no "rebuild only if this store has unprefixed rows" strategy — so every
// multi-repo workspace would eat a full cold index (hours on a large one)
// for a change that cannot affect it. A solo workspace must cold-index
// either way, since its ids all change; this targets the cost at the stores
// that actually need to pay it.
//
// WHAT IS DELETED, AND WHAT IS DELIBERATELY KEPT. The predicate selects rows
// with an empty repo_prefix AND a non-empty file_path. The file_path guard is
// the whole safety argument: synthetic GLOBAL EXTERNALS (dep::…, external::…,
// non-Go module::…, legacy bare builtin::…) also carry an empty repo_prefix
// and must survive — they model third-party symbols owned by no repository
// and are visible from every repo by construction. They have no source file,
// so requiring one selects exactly the file-backed code nodes that used to be
// a solo repo, and never the shared stub namespace.
func purgeUnprefixedRepoRows(tx *sql.Tx) error {
	// Edges first, while the nodes they reference still exist. An edge is
	// dropped when EITHER endpoint is an unprefixed code node: a half-
	// dangling edge is worse than none, because the resolver would keep
	// re-materialising its missing target as an unresolved stub.
	if _, err := countEdgeWrite(tx.Exec(`
DELETE FROM edges
WHERE from_id IN (SELECT id FROM nodes WHERE repo_prefix = '' AND file_path <> '')
   OR to_id   IN (SELECT id FROM nodes WHERE repo_prefix = '' AND file_path <> '')`)); err != nil {
		return fmt.Errorf("delete unprefixed edges: %w", err)
	}

	// Embeddings are keyed by node_id alone — the vectors table has no
	// repo_prefix column — so they can only be reached through node
	// membership, and only while the nodes are still present.
	if _, err := tx.Exec(`
DELETE FROM vectors
WHERE node_id IN (SELECT id FROM nodes WHERE repo_prefix = '' AND file_path <> '')`); err != nil {
		return fmt.Errorf("delete unprefixed vectors: %w", err)
	}

	if _, err := countNodeWrite(tx.Exec(`DELETE FROM nodes WHERE repo_prefix = '' AND file_path <> ''`)); err != nil {
		return fmt.Errorf("delete unprefixed nodes: %w", err)
	}

	// Sidecar residue written under the empty prefix. Both table sets are
	// dropped rather than relabelled: the re-index rewrites index-time
	// sidecars under the new ids and paths, and the node_id-keyed enrichment
	// tables are dangling against the nodes just deleted. Reuses the same
	// table lists RekeyRepoPrefix maintains so a new sidecar table cannot be
	// added to one path and forgotten in the other.
	//
	// file_mtimes is in rekeyMoveTables and is deleted here on purpose:
	// leaving it would let the next warm restart find empty-prefix mtimes
	// describing nodes no longer in the store, and skip the re-index that
	// has to happen.
	for _, table := range append(append([]string{}, rekeyMoveTables...), rekeyDropTables...) {
		if _, err := tx.Exec(`DELETE FROM ` + table + ` WHERE repo_prefix = ''`); err != nil {
			return fmt.Errorf("delete unprefixed rows from %s: %w", table, err)
		}
	}
	return nil
}
