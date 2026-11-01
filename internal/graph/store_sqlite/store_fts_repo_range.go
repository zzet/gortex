package store_sqlite

import (
	"context"
	"database/sql"
	"strconv"

	"github.com/zzet/gortex/internal/graph"
)

// Keep the dense base-repository sort outside FTS5's internal rank stream:
// generation and repository rejection happens before scoring retained rows.
// BM25 still uses the shared corpus; rowid retains the rank stream's tie order.
const symbolRepoDenseSpanQuery = `SELECT symbol_fts.node_id, bm25(symbol_fts)
FROM symbol_fts
CROSS JOIN symbol_fts_rowid INDEXED BY symbol_fts_rowid_by_rowid
WHERE symbol_fts MATCH ?
  AND symbol_fts.rowid BETWEEN ? AND ?
  AND symbol_fts_rowid.fts_rowid = symbol_fts.rowid
  AND CASE WHEN symbol_fts_rowid.view_gen = ?
           THEN symbol_fts.repo_prefix IN ('', ?) ELSE 0 END = 1
  AND symbol_fts.rank MATCH 'bm25()'
ORDER BY bm25(symbol_fts), symbol_fts.rowid LIMIT ?`

func (s *Store) searchSymbolRepoSpanPlan(ctx context.Context, match, repo string, limit int) ([]graph.SymbolHit, bool, error) {
	return s.searchSymbolRepoSpanSnapshot(ctx, match, repo, limit, nil)
}

// Both the ownership extrema and the rank cursor use one read snapshot. Base
// rows are mutable: reusing bounds outside this transaction could omit a newly
// accepted document. afterProbe is used only by the snapshot regression test.
func (s *Store) searchSymbolRepoSpanSnapshot(ctx context.Context, match, repo string, limit int, afterProbe func(*sql.Tx) error) ([]graph.SymbolHit, bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, true, err
	}
	defer func() { _ = tx.Rollback() }()
	span := symbolFTSSpan{generation: s.viewGen}
	// Indexed extrema establish complete bounds for both admitted ownership
	// buckets without enumerating their rows. The same snapshot ranks below.
	for _, owner := range []string{"", repo} {
		for _, order := range []string{"ASC", "DESC"} {
			var rowid int64
			err = tx.QueryRowContext(ctx, `SELECT fts_rowid FROM symbol_fts_rowid
WHERE view_gen = ? AND repo_prefix = ? ORDER BY fts_rowid `+order+` LIMIT 1`, s.viewGen, owner).Scan(&rowid)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				return nil, true, err
			}
			if !span.present || rowid < span.lo {
				span.lo = rowid
			}
			if !span.present || rowid > span.hi {
				span.hi = rowid
			}
			span.present = true
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, true, err
	}
	if span.empty() {
		return nil, true, ctx.Err()
	}
	if err = tx.QueryRowContext(ctx, `SELECT fts_rowid FROM symbol_fts_rowid ORDER BY fts_rowid DESC LIMIT 1`).Scan(&span.tableMax); err != nil {
		return nil, true, err
	}
	query := symbolFTSSpanQuery(1, true)
	args := []any{match, span.lo, span.hi, span.generation, repo, limit}
	if !span.dense() {
		// Only the base single-repository door uses this plan. A complete
		// small ownership set can filter one MATCH cursor before it reads
		// foreign content; it must not become repeated rowid point queries.
		if s.viewGen != baseViewGeneration {
			return nil, false, nil
		}
		ids, complete, readErr := symbolRepoMembershipIDs(ctx, tx, s.viewGen, repo)
		if readErr != nil {
			return nil, true, readErr
		}
		// A large repository keeps the bounded query above: FTS5's own rank
		// sorter over the run returns the unbounded stream's in-scope rows in
		// the same order, and never reads more of the table than the MATCH
		// alone would, however wide the run.
		if complete {
			if len(ids) == 0 {
				return nil, true, ctx.Err()
			}
			query = symbolRepoMembershipQuery(len(ids))
			args = []any{s.viewGen, match}
			for _, id := range ids {
				args = append(args, id)
			}
			args = append(args, repo, limit)
		}
	} else if s.viewGen == baseViewGeneration {
		query = symbolRepoDenseSpanQuery
	}
	if afterProbe != nil {
		if err = afterProbe(tx); err != nil {
			return nil, true, err
		}
	}
	key := s.symbolRepoPageKey(ctx, tx, match, repo, limit)
	if err = ctx.Err(); err != nil {
		return nil, true, err
	}
	if key != "" {
		if hits, ok := s.baseFTSPages.page(key, limit, true); ok {
			return hits, true, ctx.Err()
		}
	}
	// Generation/repository ownership still decides membership in both plans,
	// and BM25 continues to use the complete shared table.
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, true, err
	}
	defer rows.Close()
	var hits []graph.SymbolHit
	var ranked []symbolFTSRow
	for rows.Next() {
		var id string
		var score float64
		if err = rows.Scan(&id, &score); err != nil {
			return nil, true, err
		}
		if key != "" {
			ranked = append(ranked, symbolFTSRow{id: id, score: score})
		}
		if id != "" {
			hits = append(hits, graph.SymbolHit{NodeID: id, Score: -score})
		}
	}
	if err = rows.Err(); err != nil {
		return nil, true, err
	}
	if err = rows.Close(); err != nil {
		return nil, true, err
	}
	if err = ctx.Err(); err != nil {
		return nil, true, err
	}
	if key != "" {
		// The original SQL limit counts empty IDs too. An exact-limit page
		// is conservatively incomplete; a larger request runs SQL again.
		s.baseFTSPages.store(key, ranked, len(ranked) < limit)
	}
	return hits, true, nil
}

// symbolRepoPageKey names only a fully known snapshot. Every authority read is
// on the ranking transaction, including the global FTS blocks: a foreign
// generation write can change native BM25 even when base ownership is unchanged.
// Missing installation/state/revision falls back to the ordinary SQL cursor.
func (s *Store) symbolRepoPageKey(ctx context.Context, tx *sql.Tx, match, repo string, limit int) string {
	if s.viewGen != baseViewGeneration || limit <= 0 || limit > symbolFTSPageCacheRows || !s.rowCountersReady.Load() {
		return ""
	}
	var seeded, triggers int
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT
 (SELECT seeded FROM `+rowCountsState+` WHERE id = 1),
 (SELECT rev FROM `+symbolFTSRevisionsTable+` WHERE view_gen = 0),
 (SELECT count(*) FROM sqlite_schema WHERE type = 'trigger' AND name IN (?, ?, ?))`,
		symbolFTSRevisionsTable+"_insert", symbolFTSRevisionsTable+"_delete", symbolFTSRevisionsTable+"_update").Scan(&seeded, &revision, &triggers); err != nil || seeded != 1 || revision <= 0 || triggers != 3 {
		return ""
	}
	stats, err := symbolFTSStatsTx(ctx, tx)
	if err != nil || stats.Stamp == "" {
		return ""
	}
	// Length prefixes preserve exact raw spelling, including delimiter bytes.
	key := "0/" + strconv.FormatInt(revision, 10)
	for _, value := range []string{stats.Stamp, match, repo} {
		key += "/" + strconv.Itoa(len(value)) + ":" + value
	}
	return key
}
