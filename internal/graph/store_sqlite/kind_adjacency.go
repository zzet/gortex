package store_sqlite

import (
	"context"
	"database/sql"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.KindAdjacencyReader         = (*Store)(nil)
	_ graph.FilteredKindAdjacencyReader = (*Store)(nil)
)

func kindAdjacencyQuery(generation int64, id string, dir graph.Direction, kinds []graph.EdgeKind, filter graph.OutlineEdgeFilter) outlineQuery {
	col, index, order := "from_id", "edges_by_from", "e.line, e.id"
	if dir == graph.DirectionBackward {
		col, index, order = "to_id", "edges_by_to", "e.kind, e.id"
	}
	predicate, filterArgs := outlineEdgePredicate(filter, "source", "target")
	via, viaArgs := outlineViaPredicate(filter.ExcludeVia)
	from := ` FROM edges e INDEXED BY ` + index
	if len(filter.ExcludeNodeKinds) > 0 {
		from += ` LEFT JOIN nodes source ON source.id = e.from_id AND source.view_gen = e.view_gen
		LEFT JOIN nodes target ON target.id = e.to_id AND target.view_gen = e.view_gen`
	}
	where := ` WHERE e.view_gen = ? AND e.` + col + ` = ? AND NOT ` + strings.NewReplacer("kind", "e.kind", "to_id", "e.to_id").Replace(structuralTargetInvalidSQL) + predicate + via
	args := append([]any{generation, id}, filterArgs...)
	args = append(args, viaArgs...)
	// Count only this anchor's visible adjacency. The covering endpoint index
	// bounds the count; materialization runs it once even for many typed rows.
	// Both reads share one statement snapshot. Total preserves the Go walk's
	// pre-cap ordering decision without decoding irrelevant edge payloads.
	q := `WITH total(n) AS MATERIALIZED (SELECT COUNT(*)` + from + where + `)
	SELECT total.n, ` + lookupQualifiedEdgeCols + from + ` CROSS JOIN total` + where +
		` AND e.kind IN (` + inPlaceholders(len(kinds)) + `) ORDER BY ` + order
	args = append(args, args...)
	for _, kind := range kinds {
		args = append(args, string(kind))
	}
	return outlineQuery{sql: q, args: args}
}

func (s *Store) ReadKindAdjacency(ctx context.Context, id string, dir graph.Direction, kinds []graph.EdgeKind) (graph.KindAdjacency, error) {
	return s.ReadKindAdjacencyFiltered(ctx, id, dir, kinds, graph.OutlineEdgeFilter{})
}

func (s *Store) ReadKindAdjacencyFiltered(ctx context.Context, id string, dir graph.Direction, kinds []graph.EdgeKind, filter graph.OutlineEdgeFilter) (graph.KindAdjacency, error) {
	ctx = liveNameContext(ctx)
	if err := ctx.Err(); err != nil {
		return graph.KindAdjacency{}, err
	}
	kinds = anaDedupeEdgeKinds(kinds)
	if id == "" || len(kinds) == 0 {
		return graph.KindAdjacency{}, nil
	}
	query := kindAdjacencyQuery(s.viewGen, id, dir, kinds, filter)
	rows, err := outlineRows(ctx, s.db, query)
	if err != nil {
		return graph.KindAdjacency{}, err
	}
	defer rows.Close()
	out := graph.KindAdjacency{}
	for rows.Next() {
		edge, err := s.scanEdgeCursor(kindAdjacencyScanner{rows: rows, total: &out.Total})
		if err != nil {
			return graph.KindAdjacency{}, err
		}
		if edge != nil {
			out.Edges = append(out.Edges, edge)
		}
	}
	if err := rows.Err(); err != nil {
		return graph.KindAdjacency{}, err
	}
	return out, ctx.Err()
}

type kindAdjacencyScanner struct {
	rows  *sql.Rows
	total *int
}

func (s kindAdjacencyScanner) Scan(dest ...any) error {
	return s.rows.Scan(append([]any{s.total}, dest...)...)
}
