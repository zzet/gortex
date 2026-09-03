package store_sqlite

import (
	"context"
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

// ScanNodeSearchKeys reads the compact search projection in bounded ID-keyset
// pages. Each page cursor is fully consumed and closed before yield runs, so a
// callback may safely re-enter Store and no long-lived read snapshot pins WAL
// checkpoints while ranking candidates.
func (s *Store) ScanNodeSearchKeys(ctx context.Context, pageSize int, yield func([]graph.NodeSearchKey) bool) error {
	if s.coreless() || yield == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if pageSize <= 0 {
		pageSize = graph.DefaultNodeSearchKeyPageSize
	}

	// Freeze the upper boundary so inserts with larger IDs cannot extend an
	// in-progress scan indefinitely. The query layer compares MutationRevision
	// around scan + hydration and retries once when any committed mutation races.
	var highWater string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM nodes WHERE view_gen = ? ORDER BY id DESC LIMIT 1`, s.viewGen).Scan(&highWater)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		panicOnFatal(err)
		return nil
	}

	after := ""
	firstPage := true
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		var rows *sql.Rows
		if firstPage {
			rows, err = s.db.QueryContext(ctx, `
SELECT id, kind, name
FROM nodes
WHERE id <= ? AND view_gen = ?
ORDER BY id
LIMIT ?`, highWater, s.viewGen, pageSize)
		} else {
			rows, err = s.db.QueryContext(ctx, `
SELECT id, kind, name
FROM nodes
WHERE id > ? AND id <= ? AND view_gen = ?
ORDER BY id
LIMIT ?`, after, highWater, s.viewGen, pageSize)
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			panicOnFatal(err)
			return nil
		}

		page := make([]graph.NodeSearchKey, 0, pageSize)
		for rows.Next() {
			var key graph.NodeSearchKey
			if scanErr := rows.Scan(&key.ID, &key.Kind, &key.Name); scanErr != nil {
				_ = rows.Close()
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				panicOnFatal(scanErr)
				return nil
			}
			page = append(page, key)
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			panicOnFatal(rowsErr)
			return nil
		}
		if closeErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			panicOnFatal(closeErr)
			return nil
		}
		if len(page) == 0 {
			return nil
		}

		after = page[len(page)-1].ID
		if err := ctx.Err(); err != nil {
			return err
		}
		if !yield(page) {
			return nil
		}
		if len(page) < pageSize {
			return nil
		}
		firstPage = false
	}
}

var _ graph.NodeSearchKeyScanner = (*Store)(nil)
