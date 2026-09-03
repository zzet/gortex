package store_sqlite

import (
	"context"
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

const maxEdgeKindScanBatch = 4096

// ScanEdgesByKindsBatched walks full edge rows through bounded row-id pages.
// The high-water mark is captured before the first page, so delete+insert
// identity rewrites cannot make a just-rewritten row reappear in the same
// pass. Each cursor is closed before yield runs, allowing the callback to
// write even with a one-connection pool or an active pinned bulk connection.
func (s *Store) ScanEdgesByKindsBatched(
	kinds []graph.EdgeKind,
	batchSize int,
	yield func([]*graph.Edge) bool,
) {
	if yield == nil {
		return
	}
	kindValues := sqliteEdgeBatchKinds(kinds)
	if len(kindValues) == 0 {
		return
	}
	if batchSize <= 0 {
		batchSize = 1
	}
	if batchSize > maxEdgeKindScanBatch {
		batchSize = maxEdgeKindScanBatch
	}
	highWater, ok := s.edgeKindHighWater(kindValues)
	if !ok || highWater == 0 {
		return
	}

	var after int64
	for after < highWater {
		edges, next, ok := s.edgeKindPage(kindValues, after, highWater, batchSize)
		if !ok || len(edges) == 0 || next <= after {
			return
		}
		after = next
		if !yield(edges) {
			return
		}
	}
}

func (s *Store) edgeKindHighWater(kinds []string) (int64, bool) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	query := `SELECT COALESCE(MAX(id), 0) FROM edges WHERE kind IN (` +
		inPlaceholders(len(kinds)) + `) AND view_gen = ?`
	rows, err := s.queryActiveWriteLocked(context.Background(), query, append(toAnyArgs(kinds), s.viewGen)...)
	if err != nil {
		panicOnFatal(err)
		return 0, false
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, false
	}
	var highWater int64
	if err := rows.Scan(&highWater); err != nil {
		panicOnFatal(err)
		return 0, false
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
		return 0, false
	}
	return highWater, true
}

func (s *Store) edgeKindPage(
	kinds []string,
	after, highWater int64,
	limit int,
) ([]*graph.Edge, int64, bool) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	query := `SELECT id, from_id, to_id, kind, file_path, line,
	       confidence, confidence_label, origin, tier, cross_repo,
	       meta, resolve_terminal, resolve_terminal_reason, semantic_source
	FROM edges NOT INDEXED
	WHERE id > ? AND id <= ? AND kind IN (` + inPlaceholders(len(kinds)) + `) AND view_gen = ?
	ORDER BY id
	LIMIT ?`
	args := make([]any, 0, len(kinds)+4)
	args = append(args, after, highWater)
	args = append(args, toAnyArgs(kinds)...)
	args = append(args, s.viewGen, limit)
	rows, err := s.queryActiveWriteLocked(context.Background(), query, args...)
	if err != nil {
		panicOnFatal(err)
		return nil, after, false
	}
	defer rows.Close()

	edges := make([]*graph.Edge, 0, limit)
	next := after
	for rows.Next() {
		rowID, edge, err := scanBatchedEdge(rows)
		if err != nil {
			panicOnFatal(err)
			return nil, after, false
		}
		if rowID > next {
			next = rowID
		}
		if edge != nil {
			edges = append(edges, edge)
		}
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
		return nil, after, false
	}
	return edges, next, true
}

func scanBatchedEdge(scanner interface{ Scan(...any) error }) (int64, *graph.Edge, error) {
	var (
		rowID     int64
		edge      graph.Edge
		metaBlob  sql.RawBytes
		crossRepo int64
		promoted  promotedEdgeMeta
	)
	if err := scanner.Scan(
		&rowID, &edge.From, &edge.To, &edge.Kind, &edge.FilePath, &edge.Line,
		&edge.Confidence, &edge.ConfidenceLabel, &edge.Origin, &edge.Tier,
		&crossRepo, &metaBlob, &promoted.resolveTerminal, &promoted.resolveTerminalReason, &promoted.semanticSource,
	); err != nil {
		return 0, nil, err
	}
	edge.CrossRepo = crossRepo != 0
	if len(metaBlob) > 0 {
		meta, err := decodeMeta(metaBlob)
		if err != nil {
			return 0, nil, err
		}
		edge.Meta = meta
	}
	restorePromotedEdgeMeta(&edge, promoted)
	return rowID, &edge, nil
}

func sqliteEdgeBatchKinds(kinds []graph.EdgeKind) []string {
	seen := make(map[graph.EdgeKind]struct{}, len(kinds))
	values := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		if kind == "" {
			continue
		}
		if _, duplicate := seen[kind]; duplicate {
			continue
		}
		seen[kind] = struct{}{}
		values = append(values, string(kind))
	}
	return values
}

var _ graph.EdgesByKindsBatchScanner = (*Store)(nil)

// GetDataflowParamEdgesByOwnerIDs fetches only param_of edges for a bounded
// callee batch. It avoids both per-callee incoming queries and decoding an
// unrelated edge Meta blob.
func (s *Store) GetDataflowParamEdgesByOwnerIDs(ownerIDs []string) map[string][]*graph.Edge {
	return s.dataflowLightEdgesByNodeIDs(ownerIDs, "to_id", graph.EdgeParamOf, func(edge *graph.Edge) string {
		return edge.To
	})
}

// GetDataflowCallEdgesByCallerIDs is the caller-side twin used to join
// returns_to edges against resolved calls in one indexed query per SQL chunk.
func (s *Store) GetDataflowCallEdgesByCallerIDs(callerIDs []string) map[string][]*graph.Edge {
	return s.dataflowLightEdgesByNodeIDs(callerIDs, "from_id", graph.EdgeCalls, func(edge *graph.Edge) string {
		return edge.From
	})
}

func (s *Store) dataflowLightEdgesByNodeIDs(
	ids []string,
	column string,
	kind graph.EdgeKind,
	key func(*graph.Edge) string,
) map[string][]*graph.Edge {
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil
	}
	out := make(map[string][]*graph.Edge, len(uniq))
	for start := 0; start < len(uniq); start += lookupChunkSize {
		end := minInt(start+lookupChunkSize, len(uniq))
		chunk := uniq[start:end]
		query := `SELECT ` + edgeColsLight + ` FROM edges WHERE ` + column + ` IN (` +
			inPlaceholders(len(chunk)) + `) AND kind = ? AND view_gen = ? ORDER BY id`
		args := toAnyArgs(chunk)
		args = append(args, string(kind), s.viewGen)
		for _, edge := range s.queryDataflowLightActive(query, args...) {
			out[key(edge)] = append(out[key(edge)], edge)
		}
	}
	return out
}

func (s *Store) queryDataflowLightActive(query string, args ...any) []*graph.Edge {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.queryActiveWriteLocked(context.Background(), query, args...)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []*graph.Edge
	for rows.Next() {
		edge, err := s.scanEdgeLight(rows)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		if edge != nil {
			out = append(out, edge)
		}
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	return out
}
