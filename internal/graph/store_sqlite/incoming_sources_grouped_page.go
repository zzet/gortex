package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

// Group only a bounded raw page, never the remaining adjacency. Source and
// edge-file provenance jointly determine ownership. First-occurrence ordering
// preserves the order of distinct ownership checks in the raw page.
const scopedIncomingSourceGroupedPageSQL = `SELECT MAX(id), from_id, file_path, COUNT(*)
FROM (
    SELECT id, from_id, file_path
    FROM edges INDEXED BY edges_by_to
    WHERE to_id = ? AND kind = ? AND id > ? AND from_id <> '' AND view_gen = ?
    ORDER BY id
    LIMIT ?
) AS selected_page
GROUP BY from_id, file_path
ORDER BY MIN(id)`

// The count and cursor describe the bounded RAW page, not the grouped result.
// In particular, one returned group can represent a full page and is not EOF.
// The transaction and cursor-close contract match readScopedIncomingSourcePage.
func readScopedIncomingSourceGroupedPage(ctx context.Context, tx *sql.Tx, target string, kind graph.EdgeKind, generation, after int64, limit int, reuse []incomingSourcePageRow) ([]incomingSourcePageRow, int, int64, error) {
	if limit <= 0 || limit > maxIncomingSourcePageRows {
		return nil, 0, after, fmt.Errorf("invalid incoming-source grouped page limit: %d", limit)
	}
	rows, err := tx.QueryContext(ctx, scopedIncomingSourceGroupedPageSQL, target, kind, after, generation, limit)
	if err != nil {
		return nil, 0, after, err
	}
	page := reuse[:0]
	physicalRows, next := 0, after
	for rows.Next() {
		page = append(page, incomingSourcePageRow{})
		row := &page[len(page)-1]
		var count int
		if err := rows.Scan(&row.id, &row.candidate.From, &row.candidate.FilePath, &count); err != nil {
			_ = rows.Close()
			return nil, 0, after, err
		}
		if count <= 0 || count > limit-physicalRows || row.id <= after {
			_ = rows.Close()
			return nil, 0, after, fmt.Errorf("invalid incoming-source grouped page accounting")
		}
		physicalRows += count
		next = max(next, row.id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, 0, after, fmt.Errorf("incoming-source grouped page: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, after, err
	}
	return page, physicalRows, next, nil
}
