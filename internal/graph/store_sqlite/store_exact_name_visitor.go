package store_sqlite

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// VisitNodesByNameContext visits this generation's exact-name rows in the same
// order as FindNodesByNameContext. Returning false stops iteration. The rows
// are closed before a successful early-stop is reported.
func (s *Store) VisitNodesByNameContext(ctx context.Context, name string, yield func(*graph.Node) bool) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if yield == nil {
		return nil
	}
	rows, err := s.stmtFindByName.QueryContext(ctx, name, s.viewGen)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := rows.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		node, scanErr := scanNodeCursor(rows)
		if scanErr != nil {
			return scanErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !yield(node) {
			return ctx.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return ctx.Err()
}
