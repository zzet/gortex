package store_sqlite

import (
	"context"
	"encoding/json"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/graph"
)

// The requested names drive name/view seeks, independent of planner statistics.
var exactNamesSeekSQL = `SELECT ` + qualifiedNodeColumns("n", lookupNodeCols) + `
  FROM json_each(?) AS w
  CROSS JOIN nodes AS n
 WHERE n.name = w.value AND n.view_gen = ?`

// VisitNodesByNamesContext streams bounded exact-name pages with authoritative
// query, scan, iteration and close errors. Duplicate names are read once.
func (s *Store) VisitNodesByNamesContext(ctx context.Context, names []string, yield func(*graph.Node) bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if yield == nil || len(names) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(names))
	uniq := make([]string, 0, len(names))
	rawNames := false
	for _, name := range names {
		if _, duplicate := seen[name]; !duplicate {
			seen[name] = struct{}{}
			uniq = append(uniq, name)
			rawNames = rawNames || !utf8.ValidString(name)
		}
	}
	if len(uniq) == 1 {
		return s.VisitNodesByNameContext(ctx, uniq[0], yield)
	}
	if rawNames {
		// JSON replaces malformed UTF-8 with U+FFFD. Bind the original
		// spelling directly so an exact-name miss cannot match a neighbor.
		for _, name := range uniq {
			stopped := false
			if err := s.VisitNodesByNameContext(ctx, name, func(node *graph.Node) bool {
				stopped = !yield(node)
				return !stopped
			}); err != nil {
				return err
			}
			if stopped {
				break
			}
		}
		return ctx.Err()
	}
	for start := 0; start < len(uniq); start += lookupChunkSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		payload, err := json.Marshal(uniq[start:min(start+lookupChunkSize, len(uniq))])
		if err != nil {
			return err
		}
		stopped, err := s.visitExactNamesPage(ctx, string(payload), yield)
		if err != nil {
			return err
		}
		if stopped {
			break
		}
	}
	return ctx.Err()
}

func (s *Store) visitExactNamesPage(ctx context.Context, payload string, yield func(*graph.Node) bool) (stopped bool, err error) {
	return s.visitExactNamesQuery(ctx, exactNamesSeekSQL, []any{payload, s.viewGen}, yield)
}

func (s *Store) visitExactNamesQuery(ctx context.Context, query string, args []any, yield func(*graph.Node) bool) (stopped bool, err error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := rows.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		node, scanErr := scanNodeCursor(rows)
		if scanErr != nil {
			return false, scanErr
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !yield(node) {
			return true, ctx.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, ctx.Err()
}
