package store_sqlite

import (
	"context"
	"database/sql"
	"strings"
)

// This bounds plan admission and parameters, never the search's candidate set.
// An overflowing ownership census declines to the existing unrestricted plan.
const symbolRepoMembershipRows = 1024

func symbolRepoMembershipIDs(ctx context.Context, tx *sql.Tx, generation int64, repo string) ([]int64, bool, error) {
	var ids []int64
	owners := []string{""}
	if repo != "" {
		owners = append(owners, repo)
	}
	for _, owner := range owners {
		// Exact-owner seeks use (view_gen, repo_prefix, fts_rowid). Counting
		// the extra row proves overflow without scanning a foreign corpus.
		rows, err := tx.QueryContext(ctx, `SELECT fts_rowid FROM symbol_fts_rowid
WHERE view_gen = ? AND repo_prefix = ? ORDER BY fts_rowid LIMIT ?`, generation, owner, symbolRepoMembershipRows-len(ids)+1)
		if err != nil {
			return nil, false, err
		}
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, false, err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, false, err
		}
		if err = ctx.Err(); err != nil {
			return nil, false, err
		}
		if len(ids) > symbolRepoMembershipRows {
			return nil, false, nil
		}
	}
	return ids, true, nil
}

func symbolRepoMembershipQuery(ids int) string {
	// CASE keeps IN as a residual membership test, not a rowid constraint
	// that opens one MATCH cursor per ID. BM25 still uses the shared corpus.
	// Sorting after ownership filtering avoids FTS5's internal global rank
	// sorter; rowid preserves the original rank stream's tie order.
	return `SELECT symbol_fts.node_id, bm25(symbol_fts)
FROM symbol_fts
JOIN symbol_fts_rowid ON symbol_fts_rowid.fts_rowid = symbol_fts.rowid AND symbol_fts_rowid.view_gen = ?
WHERE symbol_fts MATCH ?
 AND CASE WHEN symbol_fts.rowid IN (?` + strings.Repeat(`,?`, ids-1) + `) THEN 1 ELSE 0 END = 1
 AND symbol_fts.repo_prefix IN ('',?)
 AND symbol_fts.rank MATCH 'bm25()'
ORDER BY bm25(symbol_fts), symbol_fts.rowid LIMIT ?`
}
