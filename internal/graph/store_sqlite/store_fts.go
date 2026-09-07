package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// This file implements graph.SymbolSearcher + graph.SymbolBundleSearcher
// on the SQLite backend using the FTS5 virtual table declared in
// schema.go (symbol_fts). It is the on-disk replacement for the
// multi-GB in-heap BM25 index: the FTS5 inverted index lives in
// the same .sqlite file as the graph, and a tier-0 exact-name boost
// short-circuits identifier queries so
// search quality holds or improves while the heap shrinks.
//
// Semantics:
//
//   - BulkUpsertSymbolFTS wipes only the rows owned by repoPrefix
//     before re-inserting, so sibling repos sharing one store don't
//     clobber each other's corpus. Empty prefix wipes the whole table
//     (single-repo / conformance behaviour).
//
//   - SearchSymbols tier 0: an identifier query (no whitespace / path
//     separators) that resolves to one or more nodes by exact name is
//     returned directly with a fixed dominant score, skipping FTS.
//     Misses fall through to the FTS5 MATCH path.
//
//   - SearchSymbolBundles composes the same hit list with batched
//     node + in/out edge fetches the rerank pipeline reads from.
//
// FTS5 maintains its index incrementally on every insert, so the
// Store struct needs no extra state and BuildSymbolIndex is a no-op
// (it only opportunistically merges segments).

// Compile-time assertions: *Store satisfies the symbol-search
// capabilities. The indexer auto-engages these when the active backend
// implements them, routing search_symbols through on-disk FTS5 instead
// of the engine's index-free substring fallback.
var (
	_ graph.SymbolSearcher             = (*Store)(nil)
	_ graph.SymbolFTSBatchUpserter     = (*Store)(nil)
	_ graph.SymbolFTSRepoResetter      = (*Store)(nil)
	_ graph.SymbolFTSRepoReplacer      = (*Store)(nil)
	_ graph.SymbolFTSBatchDeleter      = (*Store)(nil)
	_ graph.SymbolBundleSearcher       = (*Store)(nil)
	_ graph.ScopedSymbolBundleSearcher = (*Store)(nil)
	_ graph.BundleFingerprintSink      = (*Store)(nil)
)

// ftsInsertChunkRows bounds the rows per multi-row INSERT. Each FTS row
// binds 4 host params (explicit rowid, node_id, repo_prefix, tokens); 240
// rows is 960 params, below SQLite's conservative 999-variable limit.
const ftsInsertChunkRows = 240

// The FTS5 virtual table itself is not generation-keyed — a vtable has no
// rewritable primary key — so the docid map carries the generation and every
// statement below resolves docids through it.
const deleteSymbolFTSForRepoSQL = `DELETE FROM symbol_fts
WHERE rowid IN (
    SELECT fts_rowid FROM symbol_fts_rowid WHERE view_gen = ? AND repo_prefix = ?
)`

// nextFTSRowIDTx allocates a contiguous docid range while the caller holds
// writeMu and a write transaction. Supplying rowids explicitly lets bulk FTS
// writers populate their indexed ownership sidecars without selecting the
// just-inserted rows back out of an UNINDEXED virtual-table column.
func nextFTSRowIDTx(tx *sql.Tx, table string) (int64, error) {
	var next int64
	err := tx.QueryRow(`SELECT COALESCE(MAX(rowid), 0) + 1 FROM ` + table).Scan(&next)
	return next, err
}

// UpsertSymbolFTS is the compatibility single-item entry point. Incremental
// indexing uses BatchUpsertSymbolFTS so a file with N symbols does not pay N
// transactions and 2*N point lookups.
func (s *Store) UpsertSymbolFTS(nodeID, tokens string) error {
	return s.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: nodeID, Tokens: tokens}})
}

// SymbolFTSCount reports how many symbols are actually indexed. It counts the
// symbol_fts_rowid ownership sidecar rather than the FTS5 vtable: the sidecar
// carries exactly one row per indexed node, is WITHOUT ROWID so the primary
// key index is the table, and counting it avoids making the vtable resolve
// every document. One row per document is the invariant the delete path
// maintains, so this is the corpus size.
func (s *Store) SymbolFTSCount() (int, error) {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM symbol_fts_rowid WHERE view_gen = ?`, s.viewGen).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// symbolFTSBatchStats records the actual bounded SQL statements executed by
// one incremental batch. It is returned by the internal implementation so
// tests can enforce that statement count grows by chunks, never by symbols.
type symbolFTSBatchStats struct {
	allocatorQueries    int
	lookupStatements    int
	deleteStatements    int
	insertStatements    int
	ownershipStatements int
	commits             int
}

// ResetSymbolFTS deletes one repository's FTS documents through the indexed
// ownership sidecar. Cold shadow persistence calls this exactly once before
// appending bounded BatchUpsertSymbolFTS chunks, so no chunk can erase an
// earlier one and no whole-repository token slice is retained in Go.
func (s *Store) ResetSymbolFTS(repoPrefix string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op
	if _, err := tx.Exec(deleteSymbolFTSForRepoSQL, s.viewGen, repoPrefix); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM symbol_fts_rowid WHERE view_gen = ? AND repo_prefix = ?`, s.viewGen, repoPrefix); err != nil {
		return err
	}
	return tx.Commit()
}

// BatchUpsertSymbolFTS replaces only the supplied symbols. Unlike
// BulkUpsertSymbolFTS it never wipes a repository, so it is safe for a watcher
// or partial reconcile. IDs are deduped with last-write-wins semantics and all
// SQLite work is set-oriented in bounded chunks under one transaction.
func (s *Store) BatchUpsertSymbolFTS(items []graph.SymbolFTSItem) error {
	_, err := s.batchUpsertSymbolFTS(items)
	return err
}

// BatchDeleteSymbolFTS removes the supplied node documents through the
// indexed ownership sidecar. Both the FTS rows and their ownership rows are
// deleted in one transaction; duplicate IDs are harmless and empty IDs are
// ignored.
func (s *Store) BatchDeleteSymbolFTS(nodeIDs []string) error {
	ids := make([]string, 0, len(nodeIDs))
	seen := make(map[string]struct{}, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if nodeID == "" {
			continue
		}
		if _, duplicate := seen[nodeID]; duplicate {
			continue
		}
		seen[nodeID] = struct{}{}
		ids = append(ids, nodeID)
	}
	if len(ids) == 0 {
		return nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	if err := s.deleteSymbolFTSTx(tx, ids); err != nil {
		return err
	}
	return tx.Commit()
}

// deleteSymbolFTSTx deletes only the supplied IDs in the caller's existing
// transaction and payload generation. Mapping rows must outlive virtual-row
// deletion; this helper never locks, begins, commits or rolls back a transaction.
func (s *Store) deleteSymbolFTSTx(tx *sql.Tx, ids []string) error {
	for start := 0; start < len(ids); start += ftsInsertChunkRows {
		end := minInt(start+ftsInsertChunkRows, len(ids))
		chunk := ids[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, 0, len(chunk)+1)
		args = append(args, s.viewGen)
		for _, nodeID := range chunk {
			args = append(args, nodeID)
		}
		if _, err := tx.Exec(`DELETE FROM symbol_fts WHERE rowid IN (
SELECT fts_rowid FROM symbol_fts_rowid WHERE view_gen = ? AND node_id IN (`+placeholders+`)
)`, args...); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM symbol_fts_rowid WHERE view_gen = ? AND node_id IN (`+placeholders+`)`, args...); err != nil {
			return err
		}
	}
	return nil
}

// dedupeSymbolFTSItems drops empty IDs and collapses repeats with last-write-
// wins, mirroring UpsertSymbolFTS's delete-then-insert semantics.
func dedupeSymbolFTSItems(items []graph.SymbolFTSItem) []graph.SymbolFTSItem {
	positions := make(map[string]int, len(items))
	deduped := make([]graph.SymbolFTSItem, 0, len(items))
	for _, item := range items {
		if item.NodeID == "" {
			continue
		}
		if pos, ok := positions[item.NodeID]; ok {
			deduped[pos] = item
			continue
		}
		positions[item.NodeID] = len(deduped)
		deduped = append(deduped, item)
	}
	return deduped
}

// upsertSymbolFTSChunkTx writes one bounded, already-deduped chunk inside an
// open write transaction, advancing *nextRowid over the docids it allocates.
// Shared by the incremental batch path and the whole-repository replacement so
// the two cannot drift in how they derive ownership or reuse docids.
func upsertSymbolFTSChunkTx(tx *sql.Tx, viewGen int64, chunk []graph.SymbolFTSItem, nextRowid *int64, stats *symbolFTSBatchStats) error {
	type rowState struct {
		repoPrefix string
		rowid      int64
		exists     bool
	}

	// Fetch owning repo prefixes and prior FTS docids with one indexed
	// VALUES join for the whole chunk. This replaces the old two SELECTs
	// per symbol while retaining the exact old rowid when one exists.
	var lookup strings.Builder
	lookup.WriteString(`WITH wanted(ord, node_id) AS (VALUES `)
	lookupArgs := make([]any, 0, len(chunk)*2)
	for i, item := range chunk {
		if i > 0 {
			lookup.WriteByte(',')
		}
		lookup.WriteString(`(?, ?)`)
		lookupArgs = append(lookupArgs, i, item.NodeID)
	}
	// Both LEFT JOINs bind the same generation: the owning repo prefix must be
	// read from the node this generation carries, not a same-id node another
	// generation happens to hold, or the row would be filed under the wrong
	// repository in this generation's index.
	lookup.WriteString(`)
SELECT wanted.ord, COALESCE(nodes.repo_prefix, ''), symbol_fts_rowid.fts_rowid
FROM wanted
LEFT JOIN nodes ON nodes.id = wanted.node_id AND nodes.view_gen = ?
LEFT JOIN symbol_fts_rowid
       ON symbol_fts_rowid.view_gen = ? AND symbol_fts_rowid.node_id = wanted.node_id
ORDER BY wanted.ord`)
	lookupArgs = append(lookupArgs, viewGen, viewGen)
	rows, err := tx.Query(lookup.String(), lookupArgs...)
	if err != nil {
		return err
	}
	states := make([]rowState, len(chunk))
	seen := 0
	for rows.Next() {
		var ord int
		var repoPrefix string
		var oldRowid sql.NullInt64
		if err := rows.Scan(&ord, &repoPrefix, &oldRowid); err != nil {
			_ = rows.Close()
			return err
		}
		if ord < 0 || ord >= len(states) {
			_ = rows.Close()
			return fmt.Errorf("symbol FTS batch lookup returned invalid ordinal %d", ord)
		}
		states[ord].repoPrefix = repoPrefix
		if oldRowid.Valid {
			states[ord].rowid = oldRowid.Int64
			states[ord].exists = true
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	stats.lookupStatements++
	if seen != len(chunk) {
		return fmt.Errorf("symbol FTS batch lookup returned %d of %d rows", seen, len(chunk))
	}

	oldRowids := make([]any, 0, len(chunk))
	for i := range states {
		if states[i].exists {
			oldRowids = append(oldRowids, states[i].rowid)
			continue
		}
		states[i].rowid = *nextRowid
		*nextRowid++
	}
	if len(oldRowids) > 0 {
		var wipe strings.Builder
		wipe.WriteString(`DELETE FROM symbol_fts WHERE rowid IN (`)
		for i := range oldRowids {
			if i > 0 {
				wipe.WriteByte(',')
			}
			wipe.WriteByte('?')
		}
		wipe.WriteByte(')')
		if _, err := tx.Exec(wipe.String(), oldRowids...); err != nil {
			return err
		}
		stats.deleteStatements++
	}

	var insert strings.Builder
	insert.WriteString(`INSERT INTO symbol_fts (rowid, node_id, repo_prefix, tokens) VALUES `)
	insertArgs := make([]any, 0, len(chunk)*4)
	for i, item := range chunk {
		if i > 0 {
			insert.WriteByte(',')
		}
		insert.WriteString(`(?, ?, ?, ?)`)
		insertArgs = append(insertArgs, states[i].rowid, item.NodeID, states[i].repoPrefix, item.Tokens)
	}
	if _, err := tx.Exec(insert.String(), insertArgs...); err != nil {
		return err
	}
	stats.insertStatements++

	var ownership strings.Builder
	ownership.WriteString(`INSERT OR REPLACE INTO symbol_fts_rowid (view_gen, node_id, repo_prefix, fts_rowid) VALUES `)
	ownershipArgs := make([]any, 0, len(chunk)*4)
	for i, item := range chunk {
		if i > 0 {
			ownership.WriteByte(',')
		}
		ownership.WriteString(`(?, ?, ?, ?)`)
		ownershipArgs = append(ownershipArgs, viewGen, item.NodeID, states[i].repoPrefix, states[i].rowid)
	}
	if _, err := tx.Exec(ownership.String(), ownershipArgs...); err != nil {
		return err
	}
	stats.ownershipStatements++
	return nil
}

func (s *Store) batchUpsertSymbolFTS(items []graph.SymbolFTSItem) (symbolFTSBatchStats, error) {
	var stats symbolFTSBatchStats
	if len(items) == 0 {
		return stats, nil
	}
	items = dedupeSymbolFTSItems(items)
	if len(items) == 0 {
		return stats, nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return stats, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	nextRowid, err := nextFTSRowIDTx(tx, "symbol_fts")
	if err != nil {
		return stats, err
	}
	stats.allocatorQueries++

	for start := 0; start < len(items); start += ftsInsertChunkRows {
		end := minInt(start+ftsInsertChunkRows, len(items))
		if err := upsertSymbolFTSChunkTx(tx, s.viewGen, items[start:end], &nextRowid, &stats); err != nil {
			return stats, err
		}
	}

	if err := tx.Commit(); err != nil {
		return stats, err
	}
	stats.commits++
	return stats, nil
}

// ReplaceSymbolFTS swaps one repository's whole symbol corpus atomically. The
// wipe and every document produce streams through emit share one transaction,
// so an interrupted rebuild — a failing chunk, a producer that gives up
// half-way, a failed commit — leaves the previous corpus untouched instead of
// stranding the repository with a truncated index that reads as a set of
// legitimate misses.
//
// produce runs while this call holds writeMu and the write transaction, so it
// must not write through this store. Reading is safe on an on-disk store,
// whose readers use a separate pool; an in-process database shares one handle
// between readers and writers, so a streaming producer there would wait on the
// connection its own transaction is holding. That case is refused rather than
// hung.
func (s *Store) ReplaceSymbolFTS(repoPrefix string, produce func(emit func([]graph.SymbolFTSItem) error) error) error {
	if produce == nil {
		return nil
	}
	if s.db == s.writerDB {
		return fmt.Errorf("store_sqlite: ReplaceSymbolFTS needs independent read and write pools; %q shares one handle", s.dbPath)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Drive the wipe through the indexed rowid sidecar, and drop the sidecar
	// rows in lockstep so the two can never diverge.
	if _, err := tx.Exec(deleteSymbolFTSForRepoSQL, s.viewGen, repoPrefix); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM symbol_fts_rowid WHERE view_gen = ? AND repo_prefix = ?`, s.viewGen, repoPrefix); err != nil {
		return err
	}
	// Allocated after the wipe: every surviving docid is below this, so newly
	// inserted rows never collide with a sibling repository's.
	nextRowid, err := nextFTSRowIDTx(tx, "symbol_fts")
	if err != nil {
		return err
	}

	var stats symbolFTSBatchStats
	stats.allocatorQueries++
	emit := func(items []graph.SymbolFTSItem) error {
		items = dedupeSymbolFTSItems(items)
		for start := 0; start < len(items); start += ftsInsertChunkRows {
			end := minInt(start+ftsInsertChunkRows, len(items))
			if err := upsertSymbolFTSChunkTx(tx, s.viewGen, items[start:end], &nextRowid, &stats); err != nil {
				return err
			}
		}
		return nil
	}
	if err := produce(emit); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// BulkUpsertSymbolFTS is the cold-start fast path: wipe this repo's
// stale rows, then chunked multi-row INSERT of the deduped items. The
// whole thing runs in one transaction under writeMu so a concurrent
// reader never observes the table mid-wipe.
//
// repoPrefix scopes the pre-insert wipe: a non-empty prefix deletes
// only rows owned by that repo,
// leaving siblings untouched; an empty prefix wipes the whole table
// (single-repo / conformance behaviour — the conformance suite calls
// this with ""). Items are deduped by NodeID with last-write-wins,
// matching UpsertSymbolFTS's replace semantics.
func (s *Store) BulkUpsertSymbolFTS(repoPrefix string, items []graph.SymbolFTSItem) error {
	if len(items) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// Dedup by ID — last write wins, mirroring UpsertSymbolFTS's
	// delete-then-insert. Guards the edge case where a re-parse of a
	// file emitted the same ID twice.
	pos := make(map[string]int, len(items))
	deduped := items[:0]
	for _, it := range items {
		if it.NodeID == "" {
			continue
		}
		if p, ok := pos[it.NodeID]; ok {
			deduped[p] = it
		} else {
			pos[it.NodeID] = len(deduped)
			deduped = append(deduped, it)
		}
	}
	items = deduped
	if len(items) == 0 {
		return nil
	}

	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	commit := false
	defer func() {
		if !commit {
			_ = tx.Rollback()
		}
	}()

	// Drive the wipe through the indexed rowid sidecar. Filtering the FTS5
	// table's UNINDEXED repo_prefix column here used to rescan the growing
	// corpus once per repository during a cold multi-repo build.
	if _, err := tx.Exec(deleteSymbolFTSForRepoSQL, s.viewGen, repoPrefix); err != nil {
		return err
	}
	// Drop this repo's rowid-map entries in lockstep with the symbol_fts
	// wipe so the two never diverge; they are rebuilt from the freshly
	// inserted rows below.
	if _, err := tx.Exec(`DELETE FROM symbol_fts_rowid WHERE view_gen = ? AND repo_prefix = ?`, s.viewGen, repoPrefix); err != nil {
		return err
	}
	nextRowid, err := nextFTSRowIDTx(tx, "symbol_fts")
	if err != nil {
		return err
	}

	for start := 0; start < len(items); start += ftsInsertChunkRows {
		end := minInt(start+ftsInsertChunkRows, len(items))
		chunk := items[start:end]

		var b strings.Builder
		b.WriteString(`INSERT INTO symbol_fts (rowid, node_id, repo_prefix, tokens) VALUES `)
		args := make([]any, 0, len(chunk)*4)
		for i, it := range chunk {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`(?,?,?,?)`)
			args = append(args, nextRowid+int64(start+i), it.NodeID, repoPrefix, it.Tokens)
		}
		if _, err := tx.Exec(b.String(), args...); err != nil {
			return err
		}

		var rowids strings.Builder
		rowids.WriteString(`INSERT INTO symbol_fts_rowid (view_gen, node_id, repo_prefix, fts_rowid) VALUES `)
		mapArgs := make([]any, 0, len(chunk)*4)
		for i, it := range chunk {
			if i > 0 {
				rowids.WriteByte(',')
			}
			rowids.WriteString(`(?,?,?,?)`)
			mapArgs = append(mapArgs, s.viewGen, it.NodeID, repoPrefix, nextRowid+int64(start+i))
		}
		if _, err := tx.Exec(rowids.String(), mapArgs...); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	commit = true
	return nil
}

// backfillSymbolFTSRowidMap populates symbol_fts_rowid from symbol_fts for
// a database built before the sidecar existed. Without it, the first
// incremental UpsertSymbolFTS for an already-indexed symbol would find no
// map entry, skip the delete, and leak a duplicate FTS row. It is a
// one-time cost: skipped once the map has any row (steady state) or when
// the FTS index is empty (a fresh DB the bulk path will populate with the
// map maintained inline). Runs at Open, before any reader or writer — and so
// before the migration steps, which is why it names no view_gen column: on a
// store that already carries one the column's DEFAULT 0 puts the recovered
// rows in the base corpus they belong to, and on an older store the column is
// not there yet to name.
func backfillSymbolFTSRowidMap(db *sql.DB) error {
	var mapped bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM symbol_fts_rowid)`).Scan(&mapped); err != nil {
		return err
	}
	if mapped {
		return nil
	}
	var hasFTS bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM symbol_fts)`).Scan(&hasFTS); err != nil {
		return err
	}
	if !hasFTS {
		return nil
	}
	_, err := db.Exec(
		`INSERT OR REPLACE INTO symbol_fts_rowid (node_id, repo_prefix, fts_rowid)
		 SELECT node_id, repo_prefix, rowid FROM symbol_fts`)
	return err
}

// BuildSymbolIndex is a no-op for FTS5: the index is maintained
// incrementally on every insert, so there is nothing to build after the
// bulk parse phase. We opportunistically run the FTS5 'optimize'
// command to merge segments (purely a read-latency improvement); any
// error is ignored because the index is already correct without it.
// Idempotent — safe to call any number of times.
func (s *Store) BuildSymbolIndex() error {
	// A generation's rows are correct as soon as its bounded inserts commit.
	// FTS5 optimize is global maintenance, so running it for every unpublished
	// generation turns a view burst into repeated whole-table writer holds.
	if s.viewGen > 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.coordinatedBulkLoad {
		// Multi-repository cold indexing calls this once per repository. A
		// full optimize of the growing corpus after every repo is quadratic;
		// the outer boundary performs one corpus-size-independent bounded merge.
		s.deferredFTSOptimize = true
		return nil
	}
	_, _ = s.execActiveWriteLocked(context.Background(), `INSERT INTO symbol_fts(symbol_fts) VALUES('optimize')`)
	return nil
}

// SearchSymbols runs a symbol query and returns hits ordered by
// descending relevance (higher Score = more relevant).
//
// Tier 0 (exact-name boost): when the
// query looks like a literal identifier and resolves to one or more
// nodes by exact name, return those directly with a fixed dominant
// score (100.0) — an O(1)-ish index seek that beats FTS ranking for
// the common "type the symbol name" case. Misses fall through to FTS5.
//
// Otherwise tokenise on the read side with the SAME splitter as the
// write side (search.Tokenize) so a camelCase query lands on the
// split corpus, build a prefix-OR MATCH expression, and rank by BM25.
// SQLite's bm25() returns lower-is-better, so the stored Score is its
// negation (higher-is-better, matching the SymbolHit contract).
func (s *Store) SearchSymbols(query string, limit int) ([]graph.SymbolHit, error) {
	return s.SearchSymbolsRepoScoped(query, nil, limit)
}

// SearchSymbolsRepoScoped is SearchSymbols narrowed to the repoAllow
// prefixes inside the queries themselves — a post-fetch scope filter
// starves whenever another repo owns the whole BM25 head, which can
// run deeper than any bounded over-fetch. Filtering the UNINDEXED
// repo_prefix column here is free: the rows are already materialised
// by the MATCH + bm25 ORDER BY (the plan is identical with and
// without the IN). A nil / empty repoAllow is the exact unscoped
// behaviour.
func (s *Store) SearchSymbolsRepoScoped(query string, repoAllow []string, limit int) ([]graph.SymbolHit, error) {
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	var allowed map[string]struct{}
	if len(repoAllow) > 0 {
		allowed = make(map[string]struct{}, len(repoAllow))
		for _, r := range repoAllow {
			allowed[r] = struct{}{}
		}
	}

	// Tier 0: exact-name lookup. Only engage for identifier-shaped
	// queries (no whitespace / path separators); multi-word queries are
	// concept searches that need BM25 ranking. We only short-circuit
	// when the lookup hits at least one IN-SCOPE, SYMBOL-LIKE node —
	// out-of-scope exact matches must not mask the scoped FTS tier
	// below, and neither may container/binding kinds: a .NET project
	// module named "Extensions" would otherwise eclipse every
	// `*Extensions` class the FTS ranks first.
	if isIdentifierQuery(query) {
		ns := s.FindNodesByName(query)
		if len(ns) > 0 {
			out := make([]graph.SymbolHit, 0, minInt(len(ns), limit))
			for _, n := range ns {
				if n == nil || n.ID == "" || !tier0ShortCircuitKind(n.Kind) {
					continue
				}
				// Unowned nodes (empty prefix) pass every repo narrow —
				// the ScopeAllows carve-out.
				if allowed != nil && n.RepoPrefix != "" {
					if _, ok := allowed[n.RepoPrefix]; !ok {
						continue
					}
				}
				out = append(out, graph.SymbolHit{NodeID: n.ID, Score: 100.0})
				if len(out) >= limit {
					break
				}
			}
			if len(out) > 0 {
				return out, nil
			}
		}
	}

	match := s.buildFTSMatch(query, true)
	if match == "" {
		return nil, nil
	}

	// symbol_fts is one shared virtual table across every generation, so the
	// MATCH alone would rank rows this handle cannot see. The rowid map carries
	// the generation and its symbol_fts_rowid_by_rowid index is UNIQUE on
	// fts_rowid, so the join is a per-candidate point seek and the bm25
	// ordering below is untouched.
	q := `SELECT symbol_fts.node_id, bm25(symbol_fts)
FROM symbol_fts
JOIN symbol_fts_rowid
  ON symbol_fts_rowid.fts_rowid = symbol_fts.rowid AND symbol_fts_rowid.view_gen = ?
WHERE symbol_fts MATCH ?`
	args := []any{s.viewGen, match}
	if len(repoAllow) > 0 {
		// The empty prefix always passes: unowned rows (synthetic
		// externals) are admitted by every repo-narrow predicate — see
		// QueryOptions.ScopeAllows for the invariant.
		q += ` AND symbol_fts.repo_prefix IN ('', ?` + strings.Repeat(`,?`, len(repoAllow)-1) + `)`
		for _, r := range repoAllow {
			args = append(args, r)
		}
	}
	q += ` ORDER BY bm25(symbol_fts) LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hits []graph.SymbolHit
	for rows.Next() {
		var (
			id    string
			score float64
		)
		if err := rows.Scan(&id, &score); err != nil {
			return nil, err
		}
		if id == "" {
			continue
		}
		// bm25() is negative-better in SQLite; negate so higher = better,
		// matching the SymbolHit contract. Rows already arrive in bm25
		// (best-first) order from the ORDER BY.
		hits = append(hits, graph.SymbolHit{NodeID: id, Score: -score})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hits, nil
}

// tier0ShortCircuitKind reports whether an exact-name hit may
// short-circuit the FTS tier. Container and binding kinds are excluded:
// they share names with the symbols users actually search for (a
// project/module named X vs the X* classes inside it) and a search page
// of containers eclipses the ranked symbols outright. They remain
// reachable through the FTS tier's own ranking.
func tier0ShortCircuitKind(k graph.NodeKind) bool {
	switch k {
	case graph.KindModule, graph.KindPackage, graph.KindFile, graph.KindImport,
		graph.KindLocal, graph.KindParam, graph.KindGenericParam, graph.KindClosure,
		graph.KindBuiltin:
		return false
	}
	return true
}

// buildFTSMatch tokenises the query with the write-side splitter and
// builds an FTS5 MATCH expression: each token becomes a quoted prefix
// term ("tok"*) and the terms are OR-joined so any token match counts.
// normalize selects the symbol-FTS normalization pass. Content FTS keeps raw
// bodies for snippets and scans, so its query path must remain unnormalised.
// Returns "" when the query degenerates to no tokens.
func (s *Store) buildFTSMatch(query string, normalize bool) string {
	tokens := search.Tokenize(query)
	if len(tokens) == 0 {
		// Fallback: when Tokenize drops everything (e.g. a single
		// sub-2-char token like "go"), use the looser query tokeniser so
		// the search still reaches the engine instead of returning empty.
		tokens = search.TokenizeQuery(query)
	}
	if normalize {
		tokens = search.NormalizeFTSTokens(tokens)
	}
	if len(tokens) == 0 {
		return ""
	}
	parts := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if t == "" {
			continue
		}
		parts = append(parts, `"`+escapeFTSQuote(t)+`"*`)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " OR ")
}

// escapeFTSQuote escapes a token for use inside an FTS5 double-quoted
// string literal: a literal double quote is doubled ("" inside "...").
func escapeFTSQuote(t string) string {
	return strings.ReplaceAll(t, `"`, `""`)
}

// SearchSymbolBundles is the rerank-shaped fast path: it runs
// SearchSymbols to get the ranked id list (preserving order) plus a
// score-by-id map, then materialises the nodes and their in/out edges
// in batched fetches the rerank pipeline reads from. The engine routes
// through this when the backend implements SymbolBundleSearcher,
// pre-seeding rerank.Context's edge caches.
func (s *Store) SearchSymbolBundles(query string, limit int) ([]graph.SymbolBundle, error) {
	hits, err := s.SearchSymbols(query, limit)
	if err != nil {
		return nil, err
	}
	return s.bundlesForHits(hits)
}

// SearchSymbolBundlesRepoScoped is SearchSymbolBundles over the
// repo-scoped hit query — see SearchSymbolsRepoScoped for why the
// narrowing must happen inside the FTS query.
func (s *Store) SearchSymbolBundlesRepoScoped(query string, repoAllow []string, limit int) ([]graph.SymbolBundle, error) {
	hits, err := s.SearchSymbolsRepoScoped(query, repoAllow, limit)
	if err != nil {
		return nil, err
	}
	return s.bundlesForHits(hits)
}

// bundlesForHits materialises ranked hits into SymbolBundles through
// the content-addressed cache + batched node/edge fetches.
func (s *Store) bundlesForHits(hits []graph.SymbolHit) ([]graph.SymbolBundle, error) {
	if len(hits) == 0 {
		return nil, nil
	}

	ids := make([]string, 0, len(hits))
	scoreByID := make(map[string]float64, len(hits))
	for _, h := range hits {
		if h.NodeID == "" {
			continue
		}
		if _, dup := scoreByID[h.NodeID]; dup {
			// First hit keeps the score / position; defend against a
			// future ranker that returns an id more than once.
			continue
		}
		scoreByID[h.NodeID] = h.Score
		ids = append(ids, h.NodeID)
	}
	if len(ids) == 0 {
		return nil, nil
	}

	// Content-addressed cache: serve cached bundles for IDs whose
	// package fingerprint is unchanged and fetch only the misses. The
	// cache is nil until the daemon wires fingerprints, in which case
	// every ID is a miss and the path is exactly the legacy fetch.
	cached := make(map[string]graph.SymbolBundle, len(ids))
	missIDs := ids
	if s.bundles != nil {
		missIDs = missIDs[:0:0]
		for _, id := range ids {
			if b, ok := s.bundles.lookup(s.viewGen, id); ok {
				cached[id] = b
				continue
			}
			missIDs = append(missIDs, id)
		}
	}

	// Fetch the misses' nodes + in/out edges in one batched round-trip
	// each. A full cache hit skips all three fetches entirely.
	var nodes map[string]*graph.Node
	var out, in map[string][]*graph.Edge
	if len(missIDs) > 0 {
		nodes = s.GetNodesByIDs(missIDs)
		out = s.GetOutEdgesByNodeIDs(missIDs)
		in = s.GetInEdgesByNodeIDs(missIDs)
	}

	bundles := make([]graph.SymbolBundle, 0, len(ids))
	for _, id := range ids {
		if b, ok := cached[id]; ok {
			// The cached bundle's score is whatever it was first cached
			// with; the live FTS score for THIS query is authoritative,
			// so re-stamp it (the score is query-specific, the node +
			// edges are not).
			b.Score = scoreByID[id]
			bundles = append(bundles, b)
			continue
		}
		n := nodes[id]
		if n == nil {
			// Hit references a node evicted between the search and the
			// node fetch — skip; the caller does its own dedup / filter.
			continue
		}
		b := graph.SymbolBundle{
			Node:     n,
			Score:    scoreByID[id],
			OutEdges: out[id],
			InEdges:  in[id],
		}
		if s.bundles != nil {
			s.bundles.store(s.viewGen, b)
		}
		bundles = append(bundles, b)
	}
	return bundles, nil
}

// isIdentifierQuery reports whether a query looks like a literal symbol
// name (no whitespace, no path separators, no dots, no colons, no
// commas). The tier-0 exact-name fast path engages only on such
// queries; multi-token / path / qualified queries always go to FTS.
func isIdentifierQuery(q string) bool {
	if q == "" {
		return false
	}
	for _, r := range q {
		switch r {
		case ' ', '\t', '\n', '/', '.', ':', ',':
			return false
		}
	}
	return true
}
