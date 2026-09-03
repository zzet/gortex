package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.BoundedEdgeExistenceReader = (*Store)(nil)

func canonicalBoundedEdgeExistenceKeys(
	ctx context.Context,
	endpoints []graph.TypedEdgeEndpoint,
	limit int,
) ([]graph.TypedEdgeEndpoint, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > graph.MaxBoundedEdgeExistencePredicates {
		return nil, &graph.BoundedLocalizationLimitError{
			Resource: "SQLite edge-existence predicates",
			Limit:    graph.MaxBoundedEdgeExistencePredicates,
		}
	}
	if len(endpoints) == 0 {
		return nil, nil
	}
	capacity := len(endpoints)
	if capacity > limit+1 {
		capacity = limit + 1
	}
	seen := make(map[graph.TypedEdgeEndpoint]struct{}, capacity)
	keys := make([]graph.TypedEdgeEndpoint, 0, capacity)
	for index, endpoint := range endpoints {
		if index&127 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if endpoint.From == "" || endpoint.To == "" || endpoint.Kind == "" {
			continue
		}
		if _, duplicate := seen[endpoint]; duplicate {
			continue
		}
		seen[endpoint] = struct{}{}
		if len(seen) > limit {
			return nil, &graph.BoundedLocalizationLimitError{
				Resource: "SQLite edge-existence predicates",
				Limit:    limit,
			}
		}
		keys = append(keys, endpoint)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].From != keys[j].From {
			return keys[i].From < keys[j].From
		}
		if keys[i].To != keys[j].To {
			return keys[i].To < keys[j].To
		}
		return keys[i].Kind < keys[j].Kind
	})
	return keys, nil
}

func boundedEdgeExistenceQuery(rows int) string {
	var query strings.Builder
	query.Grow(192 + rows*8)
	query.WriteString("WITH wanted(from_id, to_id, kind) AS (VALUES ")
	for index := 0; index < rows; index++ {
		if index > 0 {
			query.WriteByte(',')
		}
		query.WriteString("(?,?,?)")
	}
	// The generation predicate belongs inside the correlated EXISTS: the
	// wanted CTE is a constant VALUES relation carrying no generation, so an
	// outer filter would have nothing to filter and an edge from a hidden
	// generation would answer the existence question.
	query.WriteString(`)
SELECT wanted.from_id, wanted.to_id, wanted.kind
FROM wanted
WHERE EXISTS (
    SELECT 1
    FROM edges
    WHERE edges.from_id = wanted.from_id
      AND edges.to_id = wanted.to_id
      AND edges.kind = wanted.kind
      AND edges.view_gen = ?
    LIMIT 1
)`)
	return query.String()
}

// FindExistingEdgeEndpoints projects only exact typed endpoint identities. A
// short read-only transaction freezes the chunked lookup; each cursor is fully
// closed before the next query, and correlated EXISTS stops at the first
// physical edge row for each requested predicate.
func (s *Store) FindExistingEdgeEndpoints(
	ctx context.Context,
	endpoints []graph.TypedEdgeEndpoint,
	limit int,
) (map[graph.TypedEdgeEndpoint]struct{}, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	keys, err := canonicalBoundedEdgeExistenceKeys(ctx, endpoints, limit)
	if err != nil {
		return nil, err
	}
	found := make(map[graph.TypedEdgeEndpoint]struct{}, len(keys))
	if len(keys) == 0 {
		return found, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	for start := 0; start < len(keys); start += lookupChunkSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := start + lookupChunkSize
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]
		args := make([]any, 0, len(chunk)*3+1)
		for _, endpoint := range chunk {
			args = append(args, endpoint.From, endpoint.To, string(endpoint.Kind))
		}
		args = append(args, s.viewGen)
		rows, queryErr := tx.QueryContext(ctx, boundedEdgeExistenceQuery(len(chunk)), args...)
		if queryErr != nil {
			return nil, queryErr
		}
		for rows.Next() {
			var from, to, kind string
			if scanErr := rows.Scan(&from, &to, &kind); scanErr != nil {
				_ = rows.Close()
				return nil, scanErr
			}
			found[graph.TypedEdgeEndpoint{From: from, To: to, Kind: graph.EdgeKind(kind)}] = struct{}{}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("bounded edge-existence query: %w", rowsErr)
		}
		if closeErr := rows.Close(); closeErr != nil {
			return nil, closeErr
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return found, nil
}
