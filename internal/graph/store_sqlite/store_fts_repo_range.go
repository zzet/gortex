package store_sqlite

import (
	"context"
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

// This is an indexed plan-admission bound, never a result limit. Larger
// ownership sets retain the original shared rank stream.
const symbolRepoSpanRows = 1024

func (s *Store) searchSymbolRepoSpanPlan(ctx context.Context, match, repo string, limit int) ([]graph.SymbolHit, bool, error) {
	return s.searchSymbolRepoSpanSnapshot(ctx, match, repo, limit, nil)
}

// Both the ownership census and the rank cursor use one read snapshot. Base
// rows are mutable: reusing bounds outside this transaction could omit a newly
// accepted document. afterProbe is used only by the snapshot regression test.
func (s *Store) searchSymbolRepoSpanSnapshot(ctx context.Context, match, repo string, limit int, afterProbe func(*sql.Tx) error) ([]graph.SymbolHit, bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, true, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT fts_rowid FROM symbol_fts_rowid
WHERE view_gen = ? AND repo_prefix IN ('', ?) LIMIT ?`, s.viewGen, repo, symbolRepoSpanRows+1)
	if err != nil {
		return nil, true, err
	}
	span := symbolFTSSpan{generation: s.viewGen}
	count := 0
	for rows.Next() {
		var rowid int64
		if err = rows.Scan(&rowid); err != nil {
			rows.Close()
			return nil, true, err
		}
		if !span.present || rowid < span.lo {
			span.lo = rowid
		}
		if !span.present || rowid > span.hi {
			span.hi = rowid
		}
		span.present = true
		count++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, true, err
	}
	if err = ctx.Err(); err != nil {
		return nil, true, err
	}
	if count > symbolRepoSpanRows {
		return nil, false, nil
	}
	if span.empty() {
		return nil, true, ctx.Err()
	}
	if err = tx.QueryRowContext(ctx, `SELECT fts_rowid FROM symbol_fts_rowid ORDER BY fts_rowid DESC LIMIT 1`).Scan(&span.tableMax); err != nil {
		return nil, true, err
	}
	if !span.dense() {
		return nil, false, nil
	}
	if afterProbe != nil {
		if err = afterProbe(tx); err != nil {
			return nil, true, err
		}
	}
	// The range only bounds the cursor. Generation/repository ownership still
	// decides membership, and BM25 continues to use the complete shared table.
	rows, err = tx.QueryContext(ctx, symbolFTSSpanQuery(1, true), match, span.lo, span.hi, span.generation, repo, limit)
	if err != nil {
		return nil, true, err
	}
	defer rows.Close()
	var hits []graph.SymbolHit
	for rows.Next() {
		var id string
		var score float64
		if err = rows.Scan(&id, &score); err != nil {
			return nil, true, err
		}
		if id != "" {
			hits = append(hits, graph.SymbolHit{NodeID: id, Score: -score})
		}
	}
	if err = rows.Err(); err != nil {
		return nil, true, err
	}
	return hits, true, ctx.Err()
}
