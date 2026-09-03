package store_sqlite

import (
	"database/sql"
	"fmt"
	"strings"
)

// Coverage-domain artifact kinds, split by ownership.
//
//   - per-file artifacts belong to exactly one file: the file's spelling IS
//     their identity, so a legacy-spelled row is unambiguously garbage.
//   - shared targets (a license, a team, a module) are referenced by many
//     files. Their FilePath is a first-sighting breadcrumb, never an
//     ownership claim, so they are NEVER selected by path — they leave only
//     when the purge has removed their last reference.
const (
	coveragePerFileNodeKinds = `'todo','fixture'`
	coverageEdgeKinds        = `'annotated','licensed_as','owns','generated_by','depends_on_module'`
	// coverageOwnedNodeKinds are the kinds whose FilePath may legitimately
	// carry a coverage builder's re-spelling: the per-file artifacts
	// themselves, and the shared targets whose FilePath is a first-sighting
	// breadcrumb rather than an ownership claim. Any OTHER kind at a path
	// means the indexer really parsed a file there.
	coverageOwnedNodeKinds = `'todo','fixture','license','team','module','artifact'`
)

// coverageNodeSidecarTables are the node_id-keyed sidecars a removed node
// would otherwise dangle in. The eviction path clears them through its own
// callers (deleteEnrichmentByNodeIDs, the constant-value writer, the vector
// store); a migration has no such caller, so it deletes them inline — the
// same reasoning purgeUnprefixedRepoRows applies to vectors.
// It must list every node_id-keyed table in schemaSQL;
// TestCoverageNodeSidecarTablesCoverSchema fails when a new one appears,
// so the list cannot drift the way a hand-maintained list usually does.
var coverageNodeSidecarTables = []string{
	"vectors",
	"clone_shingles",
	"constant_values",
	"churn_enrichment",
	"coverage_enrichment",
	"release_enrichment",
	"blame_enrichment",
	// A generation tombstone masks a base node in a sparse view generation.
	// Purging the base node it names leaves that mask pointing at nothing, so
	// it is cleared here alongside the other node_id-keyed sidecars.
	"generation_node_tombstones",
}

// purgeLegacyCoverageSpellings removes the coverage-domain rows that
// pre-fix binaries minted under a re-spelled file path.
//
// Until the builders preserved the extractor's spelling, todos / licenses /
// ownership / codegen / fixtures / modules ran their relPath through
// filepath.ToSlash before minting node IDs, FilePath fields, and edge
// endpoints. Everything else in the pipeline keys file identity by the
// indexer's exact spelling — OS-native separators below the repo prefix —
// so on Windows these rows are invisible to eviction, which matches nodes by
// file_path and edges by evicted-endpoint touch. They are never swept, never
// replaced, and never re-created in the new spelling until their file is
// re-parsed, so an upgraded store keeps serving stale TODO text and dangling
// licensed_as / owns / generated_by / depends_on_module edges indefinitely —
// including for files nothing touches again.
//
// Scope, deliberately narrow in three directions:
//
//  1. Per-repository guard. A repository's rows are judged only when THAT
//     repository's own paths are backslash-spelled, i.e. it was indexed on
//     Windows. A repository indexed on POSIX is never touched, even when it
//     shares a store with a Windows-indexed one — the scope is deliberately
//     per-repo rather than store-wide, because `fixture` nodes reuse the
//     file node's ID (see internal/fixtures: "the fixture is the file", and
//     ReclassifyFileToFixture upgrades a file node in place). Judging a
//     POSIX repository by the Windows rule would therefore delete live file
//     nodes and orphan every symbol they define.
//  2. Path-level predicate. Below the repo prefix a Windows-written
//     repository spells separators with a backslash, so a forward slash
//     there marks a row no current builder could have produced. Top-level
//     files (no separator below the prefix) were never damaged and never
//     match. Synthetic paths are excluded outright: a `::` in a FilePath
//     marks a stub namespace (external::, module::, license::), not a file.
//  3. Kind-level predicate. Only the six coverage domains' own kinds are
//     selected. A shared target is removed only after the edge purge leaves
//     it with no references at all — never because a purged file happened
//     to be its first sighting.
//
// Idempotent: a second run finds no legacy-spelled rows. Bounded to the
// coverage kinds: language-extractor nodes and edges are never candidates.
func purgeLegacyCoverageSpellings(tx *sql.Tx) error {
	scope, err := windowsWrittenScope(tx)
	if err != nil || scope.empty() {
		return err
	}
	// The node candidate additionally has to be absent from `files`, the
	// indexer's own record of the paths it parsed. That is the check that
	// covers a fixture with no symbols in it, which nothing else can tell
	// apart from a legacy artifact. Matched on (repo_prefix, file_path) so
	// it seeks the primary key.
	legacyNodePath := scope.legacyRowPredicate("nodes.file_path", "nodes.id") +
		` AND NOT EXISTS (SELECT 1 FROM files f
			WHERE f.repo_prefix = nodes.repo_prefix AND f.file_path = nodes.file_path)`
	legacyEdgePath := scope.legacyRowPredicate("edges.file_path", "")

	// Per-file artifacts whose own spelling is legacy. Collected before any
	// delete so the edge sweep below can clear their endpoints too.
	//
	// The `defines` exclusion is the last line of defence against deleting
	// a live file. A `fixture` node reuses the file node's ID, and a
	// repository's Windows verdict is drawn from rows that eviction never
	// removes, so a repository re-indexed on POSIX inside a store that
	// still holds its old Windows rows would keep being judged by the
	// Windows rule. A legacy artifact node defines nothing; a file node
	// with symbols in it always does. Cheap, and it fails safe in exactly
	// the case where the classification is wrong.
	//
	// The DROPs make the step re-entrant on a connection that carried a
	// temp table over from an earlier attempt; they name `temp.` so they
	// can never reach a same-named table in the main schema. Their
	// deferred errors are dropped deliberately: nothing else consults
	// these names, and the leading DROP makes a survivor harmless.
	if _, err := tx.Exec(`DROP TABLE IF EXISTS temp.covdom_doomed_nodes`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TEMP TABLE covdom_doomed_nodes AS
		SELECT id FROM nodes
		WHERE kind IN (` + coveragePerFileNodeKinds + `) AND ` + legacyNodePath + `
		  AND NOT EXISTS (SELECT 1 FROM edges e
		                  WHERE e.from_id = nodes.id AND e.kind = 'defines')`); err != nil {
		return err
	}
	defer func() { _, _ = tx.Exec(`DROP TABLE IF EXISTS temp.covdom_doomed_nodes`) }()

	// Shared targets the legacy edges point at. Snapshotted BEFORE the edge
	// delete, because afterwards nothing links them to this purge.
	if _, err := tx.Exec(`DROP TABLE IF EXISTS temp.covdom_shared_targets`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TEMP TABLE covdom_shared_targets AS
		SELECT DISTINCT to_id AS id FROM edges
		WHERE kind IN ('licensed_as','generated_by','depends_on_module') AND ` + legacyEdgePath + `
		UNION
		SELECT DISTINCT from_id AS id FROM edges
		WHERE kind = 'owns' AND ` + legacyEdgePath); err != nil {
		return err
	}
	defer func() { _, _ = tx.Exec(`DROP TABLE IF EXISTS temp.covdom_shared_targets`) }()

	// Legacy coverage edges, selected by kind AND their own FilePath
	// spelling — never by touching an evicted endpoint, which would take
	// a shared target's other, still-valid edges with it.
	result, err := tx.Exec(`DELETE FROM edges
		WHERE kind IN (` + coverageEdgeKinds + `) AND ` + legacyEdgePath)
	if err != nil {
		return err
	}
	removedEdges, err := result.RowsAffected()
	if err != nil {
		return err
	}
	// Any remaining edge on a doomed per-file artifact: its node is going,
	// so leaving the edge would strand a dangling endpoint. Safe here (and
	// only here) because these nodes are owned by exactly one file. Two
	// statements rather than one OR: each side then seeks through its own
	// endpoint index instead of scanning the edge table.
	for _, column := range []string{"from_id", "to_id"} {
		if _, err := tx.Exec(`DELETE FROM edges
			WHERE ` + column + ` IN (SELECT id FROM covdom_doomed_nodes)`); err != nil {
			return err
		}
	}

	// A shared target joins the doomed set only once the purge has left it
	// with no references at all. The two NOT EXISTS probes are likewise
	// kept apart so each rides an endpoint index.
	if _, err := tx.Exec(`INSERT INTO covdom_doomed_nodes(id)
		SELECT t.id FROM covdom_shared_targets t
		WHERE EXISTS (SELECT 1 FROM nodes n WHERE n.id = t.id)
		  AND t.id NOT IN (SELECT id FROM covdom_doomed_nodes)
		  AND NOT EXISTS (SELECT 1 FROM edges e WHERE e.from_id = t.id)
		  AND NOT EXISTS (SELECT 1 FROM edges e WHERE e.to_id = t.id)`); err != nil {
		return err
	}

	// Everything below is keyed by the doomed node ids, so an empty set
	// makes all of it a no-op. Skipping it outright matters because this
	// step runs before healPlannerStats: on a store with no sqlite_stat1
	// the ref_facts deletes plan as full scans of a table that holds one
	// row per resolved reference edge.
	var doomed int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM covdom_doomed_nodes`).Scan(&doomed); err != nil {
		return err
	}
	if doomed == 0 {
		if removedEdges == 0 {
			// The store held nothing legacy: leave the persisted analysis
			// generation alone rather than forcing a needless recompute.
			return nil
		}
		return invalidateAnalysisGenerationIfPresent(tx)
	}

	// Symbol FTS rows outlive their node unless deleted explicitly (see
	// BatchDeleteSymbolFTS, the eviction lane's equivalent) — a purged todo
	// would otherwise keep answering searches with its stale text.
	if _, err := tx.Exec(`DELETE FROM symbol_fts WHERE rowid IN (
		SELECT fts_rowid FROM symbol_fts_rowid
		WHERE node_id IN (SELECT id FROM covdom_doomed_nodes))`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM symbol_fts_rowid
		WHERE node_id IN (SELECT id FROM covdom_doomed_nodes)`); err != nil {
		return err
	}
	// Reference facts are keyed by the endpoint ids, not by file.
	for _, column := range []string{"from_id", "to_id"} {
		if _, err := tx.Exec(`DELETE FROM ref_facts
			WHERE ` + column + ` IN (SELECT id FROM covdom_doomed_nodes)`); err != nil {
			return err
		}
	}
	// Node-keyed sidecars, deleted while the nodes still exist.
	for _, table := range coverageNodeSidecarTables {
		if _, err := tx.Exec(`DELETE FROM ` + table + `
			WHERE node_id IN (SELECT id FROM covdom_doomed_nodes)`); err != nil {
			return fmt.Errorf("delete purged coverage rows from %s: %w", table, err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM nodes WHERE id IN (SELECT id FROM covdom_doomed_nodes)`); err != nil {
		return err
	}
	// The persisted analysis generation was computed over the rows just
	// removed, so it is stale by construction. Eviction invalidates it for
	// exactly this reason (see evictByPredicateResult); a migration has no
	// store handle to do that through, so it clears the marker directly.
	return invalidateAnalysisGenerationIfPresent(tx)
}

// invalidateAnalysisGenerationIfPresent drops the active analysis generation
// when the analysis tables exist. A store upgraded from a version that
// predates them has none, so their absence is not an error.
func invalidateAnalysisGenerationIfPresent(tx *sql.Tx) error {
	var present int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'analysis_active_generation'`).Scan(&present); err != nil {
		return err
	}
	if present == 0 {
		return nil
	}
	return invalidateAnalysisGenerationTx(tx)
}

// coverageSpellingScope names the repositories whose paths are judged. A
// repository qualifies only when its OWN nodes carry backslash-spelled
// paths; one Windows-indexed repository must never put a POSIX-indexed
// neighbour in the same store at risk.
type coverageSpellingScope struct {
	// windowsPrefixes are the repo prefixes whose own paths are
	// backslash-spelled.
	windowsPrefixes []string
	// unprefixedIsWindows reports the same for rows carrying no repo
	// prefix at all (a single-repo store).
	unprefixedIsWindows bool
	// knownPrefixes is every repo prefix present, Windows-written or not.
	// The unprefixed arm has to exclude all of them, or a POSIX
	// repository's rows would be judged as if they had no prefix.
	knownPrefixes []string
}

func (s coverageSpellingScope) empty() bool {
	return len(s.windowsPrefixes) == 0 && !s.unprefixedIsWindows
}

// windowsWrittenScope groups the store's FILE nodes by repository and
// reports which ones were written by an indexer whose separator is not
// '/'. One pass rather than a probe per repository, and restricted to
// kind='file' for two reasons: only file-backed nodes evidence the
// indexer's separator at all, and the restriction lets the scan ride the
// covering nodes_repo_files index instead of fetching every row in a
// table whose payload includes the meta blob.
func windowsWrittenScope(tx *sql.Tx) (coverageSpellingScope, error) {
	var scope coverageSpellingScope
	rows, err := tx.Query(`SELECT repo_prefix,
		MAX(CASE WHEN instr(file_path, '\') > 0 THEN 1 ELSE 0 END)
		FROM nodes WHERE kind = 'file' GROUP BY repo_prefix`)
	if err != nil {
		return scope, err
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	for rows.Next() {
		var prefix string
		var windows int
		if err := rows.Scan(&prefix, &windows); err != nil {
			return scope, err
		}
		if prefix == "" {
			scope.unprefixedIsWindows = windows == 1
			continue
		}
		scope.knownPrefixes = append(scope.knownPrefixes, prefix)
		if windows == 1 {
			scope.windowsPrefixes = append(scope.windowsPrefixes, prefix)
		}
	}
	return scope, rows.Err()
}

// legacyPathPredicate builds the SQL test "this path is a pre-fix
// re-spelling". A path qualifies when it belongs to a Windows-written
// repository AND carries a forward slash below that repository's prefix:
// there, separators are backslashes, so a forward slash cannot come from a
// current builder. Multi-repo paths are `<repo>/<path>` and that first
// separator is always a forward slash on every platform, so it is stripped
// before the remainder is judged.
//
// Paths containing `::` are excluded on every arm. That sequence marks a
// synthetic stub namespace (`external::`, `module::`, `license::`) rather
// than a file, and such a value can carry forward slashes of its own —
// an import path, for instance — which have nothing to do with separators.
//
// Prefixes are embedded as escaped literals rather than bound parameters
// because the predicate is spliced into CREATE TEMP TABLE ... AS SELECT
// statements. The values are the store's own repo_prefix column, and
// quoteSQLLiteral doubles any embedded quote. Exact string comparison, not
// LIKE, so a prefix containing '%' or '_' cannot match a sibling repo.
func (s coverageSpellingScope) legacyPathPredicate(column string) string {
	if s.empty() {
		return "0"
	}
	var arms []string
	for _, prefix := range s.windowsPrefixes {
		lit := quoteSQLLiteral(prefix + "/")
		arm := "substr(" + column + ", 1, length(" + lit + ")) = " + lit
		// A repo prefix may itself contain a separator, so one prefix can
		// be a leading substring of another ("foo" and "foo/bar"). Without
		// this exclusion the shorter repo's arm would judge the longer
		// repo's paths and read its prefix separator as a legacy spelling.
		for _, nested := range s.knownPrefixes {
			if nested == prefix || !strings.HasPrefix(nested, prefix+"/") {
				continue
			}
			nestedLit := quoteSQLLiteral(nested + "/")
			arm += " AND substr(" + column + ", 1, length(" + nestedLit + ")) <> " + nestedLit
		}
		arm += " AND instr(substr(" + column + ", length(" + lit + ") + 1), '/') > 0"
		arms = append(arms, "("+arm+")")
	}
	if s.unprefixedIsWindows {
		var b strings.Builder
		b.WriteString("(instr(" + column + ", '/') > 0")
		for _, prefix := range s.knownPrefixes {
			lit := quoteSQLLiteral(prefix + "/")
			b.WriteString(" AND substr(" + column + ", 1, length(" + lit + ")) <> " + lit)
		}
		b.WriteString(")")
		arms = append(arms, b.String())
	}
	return "(" + strings.Join(arms, " OR ") + ") AND instr(" + column + ", '::') = 0"
}

// legacyRowPredicate is what the migration actually selects on. The path
// shape alone is NOT enough, and that is the whole lesson of this step: a
// store written on Windows and then re-indexed on POSIX (a synced home, a
// container mounting the host's store) keeps its stale Windows rows,
// because eviction is spelling-exact. Those stale rows set the
// repository's verdict AND vouch for the new forward-slash rows as their
// "twin", at which point a LIVE POSIX row and a stale legacy row are
// identical in every path field.
//
// What actually separates them is ownership. When the indexer parses a
// file it records that path in `files` and hangs the file node and every
// symbol in it off the same path. A coverage builder's re-spelled path was
// never any of those things: it was minted by the builder and referenced
// by nothing else. So a row is legacy only when
//
//   - its natively spelled twin exists (it is a re-spelling of something), AND
//   - `files` does not record its path (the indexer never parsed a file there), AND
//   - no node outside the coverage domains claims that path (no file node,
//     no symbol) - selfID excludes the candidate row itself, because a
//     fixture node IS its own path.
//
// Verified against a real Windows store: of 1,088 legacy artifact rows,
// zero had their path in `files` and zero were claimed by a non-coverage
// node, while all 1,088 of their twins had both. The delete set is
// unchanged and the ambiguous cases are now excluded by construction.
//
// selfID is the candidate's id column, or "" where the candidate is not a
// node (the edge sweep).
func (s coverageSpellingScope) legacyRowPredicate(column, selfID string) string {
	if s.empty() {
		return "0"
	}
	pred := s.legacyPathPredicate(column) +
		" AND EXISTS (SELECT 1 FROM nodes twin WHERE twin.file_path = " +
		s.nativeTwinExpr(column) + ")" +
		" AND NOT EXISTS (SELECT 1 FROM nodes claimant WHERE claimant.file_path = " + column +
		" AND claimant.kind NOT IN (" + coverageOwnedNodeKinds + ")"
	if selfID != "" {
		pred += " AND claimant.id <> " + selfID
	}
	return pred + ")"
}

// nativeTwinExpr renders the path a row WOULD have carried had the builder
// preserved the indexer's spelling: the repo prefix kept verbatim, every
// forward slash below it turned back into a separator.
//
// Requiring that twin to exist is what makes the purge safe rather than
// merely narrow. A legacy row is by definition a RE-spelling of a file the
// indexer also recorded natively, so its twin is present; a row that is
// simply a POSIX-indexed file has no twin, because its own spelling is the
// native one. So a repository misjudged as Windows-written - the sticky
// verdict a store carried across platforms can produce - loses nothing,
// including the fixture nodes that reuse a file node's ID.
//
// Measured against a real Windows store before adopting it: all 1,126
// legacy-spelled artifact rows had their twin, so the stricter rule costs
// no healing. What it does give up is a row whose file has since been
// deleted, whose twin went with it. That row is unreachable residue either
// way, and trading it for the guarantee that no live file is ever removed
// is the right side of that bargain.
func (s coverageSpellingScope) nativeTwinExpr(column string) string {
	// A single-repo store carries no prefix to preserve, and SQL CASE
	// requires at least one WHEN, so this branch is not just a shortcut.
	if len(s.windowsPrefixes) == 0 {
		return "replace(" + column + ", '/', '\\')"
	}
	var b strings.Builder
	b.WriteString("CASE")
	for _, prefix := range s.windowsPrefixes {
		lit := quoteSQLLiteral(prefix + "/")
		b.WriteString(" WHEN substr(" + column + ", 1, length(" + lit + ")) = " + lit +
			" THEN " + lit + " || replace(substr(" + column + ", length(" + lit + ") + 1), '/', '\\')")
	}
	b.WriteString(" ELSE replace(" + column + ", '/', '\\') END")
	return b.String()
}

// quoteSQLLiteral renders s as a single-quoted SQLite string literal.
func quoteSQLLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
