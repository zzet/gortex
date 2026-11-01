package store_sqlite

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.CallAncestorReader         = (*Store)(nil)
	_ graph.FilteredCallAncestorReader = (*Store)(nil)
)

func callAncestorsQuery(generation int64, candidates []string, filter graph.OutlineEdgeFilter) outlineQuery {
	data, _ := projectionJSON(candidates)
	predicate, filterArgs := outlineEdgePredicate(filter, "source", "target")
	via, viaArgs := outlineViaPredicate(filter.ExcludeVia)
	edges := ` CROSS JOIN edges e INDEXED BY edges_by_to ON e.view_gen = ? AND e.to_id = a.id AND e.kind IN ('calls', 'references')`
	joins := ""
	if len(filter.ExcludeNodeKinds) > 0 {
		joins = ` LEFT JOIN nodes source ON source.id = e.from_id AND source.view_gen = e.view_gen
		LEFT JOIN nodes target ON target.id = e.to_id AND target.view_gen = e.view_gen`
	}
	q := `WITH RECURSIVE ancestors(id) AS (
		SELECT CAST(value AS TEXT) FROM json_each(?)
		UNION
		SELECT e.from_id FROM ancestors a` + edges + joins + `
		WHERE e.from_id <> ''` + predicate + via + `)
		SELECT e.from_id, e.to_id, COALESCE(source.kind, ''), COALESCE(source.file_path, '')
		FROM ancestors a` + edges + `
		LEFT JOIN nodes source ON source.id = e.from_id AND source.view_gen = e.view_gen`
	if len(filter.ExcludeNodeKinds) > 0 {
		q += ` LEFT JOIN nodes target ON target.id = e.to_id AND target.view_gen = e.view_gen`
	}
	q += ` WHERE 1` + predicate + via
	args := append([]any{data, generation}, filterArgs...)
	args = append(args, viaArgs...)
	args = append(args, generation)
	args = append(args, filterArgs...)
	args = append(args, viaArgs...)
	return outlineQuery{sql: q, args: args}
}

func (s *Store) CallAncestorsContext(ctx context.Context, candidates []string) ([]graph.CallAncestorRow, error) {
	return s.CallAncestorsFilteredContext(ctx, candidates, graph.OutlineEdgeFilter{})
}

// One recursive, generation-pinned statement enumerates only calls/references
// that can reach the candidate identities. UNION deduplicates identities so
// cycles terminate, while the final indexed join retains every edge row and
// its source classification for exact fan-in and Go's test-path predicate.
func (s *Store) CallAncestorsFilteredContext(ctx context.Context, candidates []string, filter graph.OutlineEdgeFilter) ([]graph.CallAncestorRow, error) {
	ctx = liveNameContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	candidates = dedupeNonEmpty(candidates)
	if len(candidates) == 0 {
		return nil, nil
	}
	query := callAncestorsQuery(s.viewGen, candidates, filter)
	rows, err := outlineRows(ctx, s.db, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.CallAncestorRow
	for rows.Next() {
		var row graph.CallAncestorRow
		if err := rows.Scan(&row.From, &row.To, &row.FromKind, &row.FromFile); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, ctx.Err()
}
