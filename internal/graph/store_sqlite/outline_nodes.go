package store_sqlite

import (
	"github.com/zzet/gortex/internal/graph"
)

var _ graph.ScopedOutlineNodeReader = (*Store)(nil)

func (s *Store) OutlineNodeIDsInScope(scope graph.NodeScope, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	data, ok := projectionJSON(ids)
	if !ok {
		return nil, nil
	}
	predicate, args := outlineScopePredicate(scope, "n")
	// One primary-key seek per bounded candidate; only IDs cross the driver.
	q := `SELECT n.id FROM json_each(?) AS wanted
CROSS JOIN nodes n ON n.id = wanted.value AND n.view_gen = ? WHERE ` + predicate
	rows, err := s.db.QueryContext(s.readContext(), q, append([]any{data, s.viewGen}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
func (s *Store) OutlineNodesByName(scope graph.NodeScope, name string) ([]*graph.Node, error) {
	predicate, args := outlineScopePredicate(scope, "n")
	// Match the old exact-name lookup's row order while applying visibility
	// before full payload rows leave SQLite. The name index bounds candidates.
	q := `SELECT ` + qualifiedNodeColumns("n", lookupNodeCols) + ` FROM nodes n INDEXED BY nodes_by_name
WHERE n.name = ? AND n.view_gen = ? AND ` + predicate
	rows, err := outlineRows(s.readContext(), s.db, outlineQuery{sql: q, args: append([]any{name, s.viewGen}, args...)})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*graph.Node
	for rows.Next() {
		n, err := scanNodeCursor(rows)
		if err != nil {
			continue
		} // Same individually undecodable-row contract as node scans.
		out = append(out, n)
	}
	return out, rows.Err()
}
