package store_sqlite

import (
	"context"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// FindNodesByNameContext is the cancellable form of FindNodesByName.
func (s *Store) FindNodesByNameContext(ctx context.Context, name string) ([]*graph.Node, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.stmtFindByName.QueryContext(ctx, name, s.viewGen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*graph.Node
	for rows.Next() {
		node, scanErr := scanNodeCursor(rows)
		if scanErr != nil {
			return out, scanErr
		}
		out = append(out, node)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// FindNodesByNameContainingContext is the cancellable form of
// FindNodesByNameContaining. It preserves the same full-scan LIKE, ordering,
// literal wildcard escaping, generation scope, and result limit.
func (s *Store) FindNodesByNameContainingContext(ctx context.Context, substr string, limit int) ([]*graph.Node, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if substr == "" {
		return nil, nil
	}
	pattern := "%" + escapeLikePattern(substr) + "%"
	q := `SELECT ` + lookupNodeCols + ` FROM nodes WHERE name LIKE ? ESCAPE '\' AND view_gen = ? ORDER BY id`
	if limit > 0 {
		return s.queryNodesSQLContext(ctx, q+` LIMIT ?`, pattern, s.viewGen, limit)
	}
	return s.queryNodesSQLContext(ctx, q, pattern, s.viewGen)
}

// VisitNodesByNameContainingFoldedContext is the cancellable form of
// VisitNodesByNameContainingFolded. Returning false from yield stops iteration
// successfully.
func (s *Store) VisitNodesByNameContainingFoldedContext(ctx context.Context, substr string, yield func(*graph.Node) bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if substr == "" || yield == nil {
		return nil
	}
	needle := strings.ToLower(substr)
	nonASCII := false
	for i := 0; i < len(needle); i++ {
		if needle[i] >= 0x80 {
			nonASCII = true
			break
		}
	}

	q := `SELECT ` + lookupNodeCols + ` FROM nodes WHERE view_gen = ? AND length(name) != length(CAST(name AS BLOB)) ORDER BY id`
	args := []any{s.viewGen}
	if !nonASCII {
		q = `SELECT ` + lookupNodeCols + ` FROM nodes WHERE view_gen = ? AND (name LIKE ? ESCAPE '\' OR length(name) != length(CAST(name AS BLOB))) ORDER BY id`
		args = append(args, "%"+escapeLikePattern(needle)+"%")
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		node, scanErr := scanNodeCursor(rows)
		if scanErr != nil {
			return scanErr
		}
		if node == nil || node.Name == "" || !strings.Contains(strings.ToLower(node.Name), needle) {
			continue
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

func (s *Store) queryNodesSQLContext(ctx context.Context, q string, args ...any) ([]*graph.Node, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*graph.Node
	for rows.Next() {
		node, scanErr := scanNodeCursor(rows)
		if scanErr != nil {
			return out, scanErr
		}
		if node == nil {
			continue
		}
		out = append(out, node)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	return out, nil
}
