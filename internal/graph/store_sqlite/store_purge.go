package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// This file adds the repo-scoped store hygiene the base eviction path lacks.
// EvictRepo (store.go) deletes ONLY nodes+edges, but a repo owns fifteen
// other sidecar tables (file_mtimes, repo_index_state, enrichment_state,
// contract_state, semantic_binding_types, clone_shingles, constant_values,
// files, ref_facts, vectors, churn/coverage/release/blame_enrichment,
// symbol_fts, symbol_fts_rowid, content_fts — see schema.go). Untracking a repo through
// EvictRepo leaks every one of them, so a long-lived store accumulates
// sidecar rows for repos removed from config long ago. PurgeRepo clears a
// repo whole; OrphanRepoPrefixes finds prefixes that outlived their config
// entry; RekeyRepoPrefix moves a lone repo's residue when it earns a prefix.
//
// INVARIANT — the empty repo_prefix is NEVER purged. In a live multi-repo
// store repo_prefix='' identifies SYNTHETIC GLOBAL EXTERNALS (external_call
// ::dep:* / builtin:: / module:: nodes shared across every repo) and, in a
// single-repo store, the sole repo's live data. Deleting '' rows would strip
// the shared externals out from under every repo, or wipe the lone repo.
// Every method here refuses or excludes ''.

// GENERATION SCOPE — every statement driven by the four table lists in this
// file is deliberately generation-UNSCOPED, as are PurgeRepo and the explicit
// EvictRepoAllGenerations path in store.go. Untracking or re-keying a repository
// removes it from the store entirely, not from one payload view of it: a row
// left behind in another generation would be residue no later call could reach,
// which is exactly what these sweeps exist to prevent. Ordinary EvictRepo and
// per-repo reads and writes carry the caller's view_gen; these do not.
//
// purgeSidecarTables are the repo_prefix-keyed sidecar tables PurgeRepo
// clears for a prefix, alongside nodes+edges. Each carries a repo_prefix
// column a plain `DELETE ... WHERE repo_prefix = ?` keys on. The two FTS5
// vtables (symbol_fts, content_fts) carry repo_prefix UNINDEXED, so their
// delete is a full scan — acceptable for a purge (a rare, whole-repo op),
// unlike the per-edit hot path. Vectors are repo-keyed too; deleting them by
// repo_prefix is essential because synthetic chunk IDs are not graph node IDs.
var purgeSidecarTables = []string{
	"file_mtimes",
	"repo_index_state",
	"symbol_fts_state",
	"enrichment_state",
	"contract_state",
	"semantic_binding_types",
	"clone_shingles",
	"clone_corpus_state",
	"constant_values",
	"files",
	"ref_facts",
	"vectors",
	"churn_enrichment",
	"coverage_enrichment",
	"release_enrichment",
	"blame_enrichment",
	"symbol_fts",
	"symbol_fts_rowid",
	"content_fts",
	"content_fts_rowid",
}

// PurgeRepo deletes EVERY row a repo owns — nodes, edges, every
// repo_prefix-keyed sidecar table, and vectors — across all generations in one
// transaction. It is the sidecar-complete form of EvictRepoAllGenerations,
// wired into UntrackRepo so removing a repo from config leaves no residue.
// Refuses prefix=="" (shared global externals / solo-mode live data — see the
// file-level INVARIANT).
func (s *Store) PurgeRepo(prefix string) error {
	if prefix == "" {
		return fmt.Errorf("store_sqlite: PurgeRepo refuses empty repo prefix (would delete shared global externals / solo-repo data)")
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// PurgeRepo bypasses the ordinary Add/Evict entry points, so invalidate a
	// persisted whole-graph analysis before the transaction can delete live graph
	// rows. The preflight is safe under writeMu and avoids touching analysis state
	// for sidecar-only purges.
	conn, release, err := s.activeWriteConnLocked(context.Background())
	if err != nil {
		return fmt.Errorf("store_sqlite: PurgeRepo writer preflight connection: %w", err)
	}
	var hasGraphRows int
	err = conn.QueryRowContext(context.Background(), `SELECT EXISTS(SELECT 1 FROM nodes WHERE repo_prefix = ? LIMIT 1)`, prefix).Scan(&hasGraphRows)
	release()
	if err != nil {
		return fmt.Errorf("store_sqlite: PurgeRepo graph preflight: %w", err)
	}
	if hasGraphRows != 0 && !s.invalidateAnalysisBeforeMutationLocked() {
		return fmt.Errorf("store_sqlite: PurgeRepo could not invalidate active analysis")
	}

	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	// Collect this repo's node IDs first for edge deletion. Vectors are removed
	// below by repo_prefix so synthetic chunk rows are covered too. Edge deletion
	// semantics mirror scope eviction's (store.go): delete every edge touching
	// one of these nodes, then the nodes themselves.
	ids, err := repoNodeIDsTx(tx, prefix)
	if err != nil {
		return err
	}
	if err := deleteByIDColumnsTx(tx, "edges", []string{"from_id", "to_id"}, ids); err != nil {
		return fmt.Errorf("store_sqlite: PurgeRepo edges: %w", err)
	}
	changed := len(ids) > 0
	for _, table := range purgeSidecarTables {
		res, err := tx.Exec(`DELETE FROM `+table+` WHERE repo_prefix = ?`, prefix)
		if err != nil {
			return fmt.Errorf("store_sqlite: PurgeRepo %s: %w", table, err)
		}
		if n, rowsErr := res.RowsAffected(); rowsErr == nil && n > 0 {
			changed = true
		}
	}

	res, err := tx.Exec(`DELETE FROM nodes WHERE repo_prefix = ?`, prefix)
	if err != nil {
		return fmt.Errorf("store_sqlite: PurgeRepo nodes: %w", err)
	}
	if n, rowsErr := res.RowsAffected(); rowsErr == nil && n > 0 {
		changed = true
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	s.finishAnalysisMutationLocked(len(ids) > 0)
	if changed {
		s.markMutationReceiptsIncompleteLocked()
	}
	return nil
}

// orphanScanTables are the tables OrphanRepoPrefixes unions DISTINCT
// repo_prefix over. They span the primary graph, warm-restart provenance,
// enrichment/file metadata, compiler-derived bindings, clone corpus state,
// and durable vectors. Vector coverage matters because a failed legacy
// untrack can leave only synthetic chunk rows after graph nodes are gone.
// A prefix whose nodes are gone but whose sidecars remain is invisible to a
// nodes-only scan, which is why the sidecar tables are unioned in.
var orphanScanTables = []string{
	"nodes",
	"file_mtimes",
	"repo_index_state",
	"symbol_fts_state",
	"enrichment_state",
	"files",
	"semantic_binding_types",
	"clone_corpus_state",
	"vectors",
}

// OrphanRepoPrefixes returns every repo_prefix present in the store but
// absent from known — repos whose rows outlived their config entry (an
// untrack that predated PurgeRepo, or a repo dropped straight from config
// with no untrack at all). The empty prefix is NEVER reported (shared global
// externals / solo data). known is matched case-insensitively as a safety
// net, so a case-only spelling drift on a case-insensitive filesystem can
// never flag a still-tracked repo as an orphan (the #270 failure mode).
// Startup warmup feeds the result to PurgeRepo.
func (s *Store) OrphanRepoPrefixes(known []string) []string {
	knownFold := make(map[string]struct{}, len(known))
	for _, k := range known {
		if k == "" {
			continue
		}
		knownFold[strings.ToLower(k)] = struct{}{}
	}

	seen := make(map[string]struct{})
	var out []string
	for _, table := range orphanScanTables {
		// WHERE repo_prefix <> '' both excludes the protected empty prefix
		// and lets the nodes scan ride the partial nodes_by_repo index
		// (defined WHERE repo_prefix <> ''). A table absent on an older
		// schema simply contributes nothing.
		rows, err := s.db.Query(`SELECT DISTINCT repo_prefix FROM ` + table + ` WHERE repo_prefix <> ''`)
		if err != nil {
			continue
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				break
			}
			if p == "" {
				continue
			}
			if _, ok := knownFold[strings.ToLower(p)]; ok {
				continue
			}
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
		// The result drives PurgeRepo, so a truncated scan can only
		// under-report orphans — it can never promote a tracked repo into
		// the purge list. Losing a candidate defers the purge to the next
		// warmup, which is the safe direction for a destructive caller.
		_ = rows.Err()
		_ = rows.Close()
	}
	return out
}

// rekeyMoveTables are the sidecar tables RekeyRepoPrefix relabels from old
// to new. Every one is keyed by repo_prefix (+ file_path or provider), NOT
// by node_id, so its row content survives a node-id change: file_mtimes /
// files by (repo_prefix, file_path); repo_index_state / contract_state /
// enrichment_state by repo_prefix (+ provider). At a solo->multi migration
// every ” row in these belongs to the one migrating repo — global externals
// live in the NODES table and hold NO rows here — so moving them wholesale
// is safe. UPDATE OR REPLACE folds any row the re-mint re-index already wrote
// under new (identical content: same files, same mtimes, same commit) instead
// of tripping the primary-key conflict a plain UPDATE would.
var rekeyMoveTables = []string{
	"file_mtimes",
	"files",
	"repo_index_state",
	"enrichment_state",
	"contract_state",
}

// rekeyDropTables are the sidecar tables RekeyRepoPrefix DROPS (rather than
// relabels) for old. Most are keyed by node_id, and a re-key accompanies a
// re-mint that changes every node id (`<old>/pkg.go::X` -> `<new>/pkg.go::X`),
// so their old-id rows are already dangling against the evicted nodes.
// semantic_binding_types is the exception: it is keyed by repo_prefix and
// file_path, but a re-mint rewrites every file path too, so relabeling would
// preserve unusable stale keys.
// Dropping these tables is correct: re-index rewrites index-time sidecars
// and semantic bindings under the new IDs and paths, while enrichment
// sidecars must re-run for the new IDs regardless. The FTS vtables sit here
// too — their rows carry the old node ids, and UPDATE over an FTS5 UNINDEXED
// column is awkward, so delete-then-reindex is the clean path.
var rekeyDropTables = []string{
	"semantic_binding_types",
	"clone_shingles",
	"clone_corpus_state",
	"constant_values",
	"ref_facts",
	"churn_enrichment",
	"coverage_enrichment",
	"release_enrichment",
	"blame_enrichment",
	"symbol_fts",
	"symbol_fts_rowid",
	"content_fts",
	"content_fts_rowid",
}

// RekeyRepoPrefix moves a repo's sidecar residue from one prefix to another.
// The prefix/path-keyed provenance tables (rekeyMoveTables) are relabeled so
// warm restart finds the repo's mtimes + freshness under new instead of
// full-re-tracking it; stale ID/path-keyed tables (rekeyDropTables) are
// dropped because a re-mint changes every node ID and file path out from
// under them (see the two table lists for the per-table rationale).
//
// It was written for the solo→multi transition, where a lone unprefixed repo
// earned a real prefix the moment a second repo joined. Every repo is
// prefixed from its first index now, so that caller is gone; this stays as
// the general re-key primitive.
//
// Refuses new=="" (cannot rekey INTO the protected empty prefix). old=="" is
// allowed, and is safe because this method touches SIDECAR tables ONLY — the
// synthetic global externals that also carry an empty repo_prefix live in the
// NODES table, which RekeyRepoPrefix never writes.
func (s *Store) RekeyRepoPrefix(oldPrefix, newPrefix string) error {
	if newPrefix == "" {
		return fmt.Errorf("store_sqlite: RekeyRepoPrefix refuses empty destination prefix")
	}
	if oldPrefix == newPrefix {
		return nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	changed := false
	for _, table := range rekeyMoveTables {
		res, err := tx.Exec(`UPDATE OR REPLACE `+table+` SET repo_prefix = ? WHERE repo_prefix = ?`, newPrefix, oldPrefix)
		if err != nil {
			return fmt.Errorf("store_sqlite: RekeyRepoPrefix move %s: %w", table, err)
		}
		if n, rowsErr := res.RowsAffected(); rowsErr == nil && n > 0 {
			changed = true
		}
	}
	for _, table := range rekeyDropTables {
		res, err := tx.Exec(`DELETE FROM `+table+` WHERE repo_prefix = ?`, oldPrefix)
		if err != nil {
			return fmt.Errorf("store_sqlite: RekeyRepoPrefix drop %s: %w", table, err)
		}
		if n, rowsErr := res.RowsAffected(); rowsErr == nil && n > 0 {
			changed = true
		}
	}
	// The symbol FTS corpus above is dropped rather than relabeled because its
	// node IDs change. Invalidate both possible markers in the same transaction:
	// moving the old marker would falsely certify the now-empty destination,
	// while retaining a prior destination marker would do the same after merge.
	stateRes, err := tx.Exec(`DELETE FROM symbol_fts_state WHERE repo_prefix IN (?, ?)`, oldPrefix, newPrefix)
	if err != nil {
		return fmt.Errorf("store_sqlite: RekeyRepoPrefix invalidate symbol FTS normalization: %w", err)
	}
	if n, rowsErr := stateRes.RowsAffected(); rowsErr == nil && n > 0 {
		changed = true
	}
	// Vectors are handled explicitly instead of joining rekeyDropTables because
	// that shared list is also used by the historical v6→v7 migration, whose
	// vector schema predates repo_prefix. At the current schema the old node and
	// parent IDs cannot be relabeled safely, so drop the complete old corpus.
	res, err := tx.Exec(`DELETE FROM vectors WHERE repo_prefix = ?`, oldPrefix)
	if err != nil {
		return fmt.Errorf("store_sqlite: RekeyRepoPrefix drop vectors: %w", err)
	}
	if n, rowsErr := res.RowsAffected(); rowsErr == nil && n > 0 {
		changed = true
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	if changed {
		s.markMutationReceiptsIncompleteLocked()
	}
	return nil
}

// repoNodeIDsTx returns every node id in repoPrefix, read inside tx. The
// caller holds writeMu. Rows are fully drained + closed before the caller
// issues writes on the same tx — SQLite forbids an open read cursor while
// writing on the same connection.
func repoNodeIDsTx(tx *sql.Tx, repoPrefix string) ([]string, error) {
	rows, err := tx.Query(`SELECT id FROM nodes WHERE repo_prefix = ?`, repoPrefix)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	return ids, nil
}

// deleteByIDColumnsTx deletes rows from table where ANY of cols matches one
// of ids, chunked so each statement stays under SQLite's 999 bound-variable
// limit. Mirrors scope eviction's separate from_id/to_id edge deletes
// (store.go) — the semantics source for edge eviction. Empty ids is a no-op.
func deleteByIDColumnsTx(tx *sql.Tx, table string, cols, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	const chunk = 900
	for _, col := range cols {
		for start := 0; start < len(ids); start += chunk {
			end := minInt(start+chunk, len(ids))
			batch := ids[start:end]
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
			args := make([]any, len(batch))
			for i, id := range batch {
				args[i] = id
			}
			if _, err := tx.Exec(`DELETE FROM `+table+` WHERE `+col+` IN (`+placeholders+`)`, args...); err != nil {
				return err
			}
		}
	}
	return nil
}
