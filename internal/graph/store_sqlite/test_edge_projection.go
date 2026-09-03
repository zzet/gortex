package store_sqlite

import (
	"database/sql"
	"encoding/json"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

const (
	// Projection rows contain only identifiers and source locations. A 4096-row
	// page stays bounded while amortizing cursor setup across the whole-graph
	// node and annotation censuses.
	defaultTestProjectionPageSize = 4096
	maxTestProjectionPageSize     = 4096
	defaultTestCallSourceBatch    = 512
	maxTestCallSourceBatch        = 4096
)

func boundedTestProjectionPageSize(pageSize int) int {
	if pageSize <= 0 {
		return defaultTestProjectionPageSize
	}
	return min(pageSize, maxTestProjectionPageSize)
}

func boundedTestCallSourceBatchSize(batchSize int) int {
	if batchSize <= 0 {
		return defaultTestCallSourceBatch
	}
	return min(batchSize, maxTestCallSourceBatch)
}

var (
	_ graph.TestProjectionScanner     = (*Store)(nil)
	_ graph.TestCallProjectionScanner = (*Store)(nil)
)

// ScanTestNodeProjections pages metadata-free node rows by the WITHOUT ROWID
// primary key. For a fixed kind, nodes_by_kind implicitly carries the primary
// key and supports this keyset order without decoding or copying Meta.
func (s *Store) ScanTestNodeProjections(kinds []graph.NodeKind, pageSize int, yield func([]graph.TestNodeProjection) bool) {
	if yield == nil {
		return
	}
	pageSize = boundedTestProjectionPageSize(pageSize)
	seen := make(map[graph.NodeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		if kind == "" {
			continue
		}
		if _, duplicate := seen[kind]; duplicate {
			continue
		}
		seen[kind] = struct{}{}

		var highWater sql.NullString
		if err := s.db.QueryRow(`SELECT MAX(id) FROM nodes WHERE kind = ? AND view_gen = ?`, kind, s.viewGen).Scan(&highWater); err != nil {
			panicOnFatal(err)
			return
		}
		if !highWater.Valid || highWater.String == "" {
			continue
		}

		after := ""
		for after < highWater.String {
			rows, err := s.db.Query(`SELECT id, kind, name, file_path, language
FROM nodes
WHERE kind = ? AND id > ? AND id <= ? AND view_gen = ?
ORDER BY id
LIMIT ?`, kind, after, highWater.String, s.viewGen, pageSize)
			if err != nil {
				panicOnFatal(err)
				return
			}
			page := make([]graph.TestNodeProjection, 0, pageSize)
			last := after
			for rows.Next() {
				var row graph.TestNodeProjection
				if err := rows.Scan(&row.ID, &row.Kind, &row.Name, &row.FilePath, &row.Language); err != nil {
					_ = rows.Close()
					panicOnFatal(err)
					return
				}
				last = row.ID
				page = append(page, row)
			}
			rowsErr := rows.Err()
			_ = rows.Close()
			if rowsErr != nil {
				panicOnFatal(rowsErr)
				return
			}
			if len(page) == 0 {
				break
			}
			// The cursor is closed before yielding, so the callback may fetch
			// full nodes or write metadata without database/sql re-entry stalls.
			if !yield(page) {
				return
			}
			if last <= after {
				break
			}
			after = last
		}
	}
}

// ScanTestEdgeProjections pages only the endpoint/location columns consumed by
// annotation classification. The frozen integer high-water mark excludes rows
// inserted by a callback from the current pass.
func (s *Store) ScanTestEdgeProjections(kinds []graph.EdgeKind, pageSize int, yield func([]graph.TestEdgeProjection) bool) {
	if yield == nil {
		return
	}
	pageSize = boundedTestProjectionPageSize(pageSize)
	seen := make(map[graph.EdgeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		if kind == "" {
			continue
		}
		if _, duplicate := seen[kind]; duplicate {
			continue
		}
		seen[kind] = struct{}{}

		var highWater sql.NullInt64
		if err := s.db.QueryRow(`SELECT MAX(id) FROM edges WHERE kind = ? AND view_gen = ?`, kind, s.viewGen).Scan(&highWater); err != nil {
			panicOnFatal(err)
			return
		}
		if !highWater.Valid || highWater.Int64 <= 0 {
			continue
		}

		var after int64
		for after < highWater.Int64 {
			rows, err := s.db.Query(`SELECT id, from_id, to_id, kind, file_path, line
FROM edges
WHERE kind = ? AND id > ? AND id <= ? AND view_gen = ?
ORDER BY id
LIMIT ?`, kind, after, highWater.Int64, s.viewGen, pageSize)
			if err != nil {
				panicOnFatal(err)
				return
			}
			page := make([]graph.TestEdgeProjection, 0, pageSize)
			last := after
			for rows.Next() {
				var row graph.TestEdgeProjection
				if err := rows.Scan(&last, &row.From, &row.To, &row.Kind, &row.FilePath, &row.Line); err != nil {
					_ = rows.Close()
					panicOnFatal(err)
					return
				}
				page = append(page, row)
			}
			rowsErr := rows.Err()
			_ = rows.Close()
			if rowsErr != nil {
				panicOnFatal(rowsErr)
				return
			}
			if len(page) == 0 {
				break
			}
			if !yield(page) {
				return
			}
			if last <= after {
				break
			}
			after = last
		}
	}
}

const testCallProjectionSQL = `
WITH requested(from_id) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
)
SELECT e.from_id, e.to_id, e.file_path, e.line
FROM requested AS r
CROSS JOIN edges AS e INDEXED BY edges_by_from
WHERE e.from_id = r.from_id
  AND e.kind = ?
  AND e.view_gen = ?
  AND e.id <= ?
ORDER BY e.from_id, e.id`

// ScanTestCallProjections projects calls only from known test symbols. Source
// IDs are deduplicated and queried in bounded batches through
// edges_by_from(from_id, kind); no edge metadata crosses the driver. Each
// cursor is fully closed before its compact results are yielded, and a frozen
// high-water mark keeps callback writes out of the current scan.
func (s *Store) ScanTestCallProjections(sourceIDs []string, sourceBatchSize, pageSize int, yield func([]graph.TestEdgeProjection) bool) {
	if yield == nil || len(sourceIDs) == 0 {
		return
	}
	sourceBatchSize = boundedTestCallSourceBatchSize(sourceBatchSize)
	pageSize = boundedTestProjectionPageSize(pageSize)

	seen := make(map[string]struct{}, len(sourceIDs))
	sources := make([]string, 0, len(sourceIDs))
	for _, sourceID := range sourceIDs {
		if sourceID == "" {
			continue
		}
		if _, duplicate := seen[sourceID]; duplicate {
			continue
		}
		seen[sourceID] = struct{}{}
		sources = append(sources, sourceID)
	}
	if len(sources) == 0 {
		return
	}
	sort.Strings(sources)

	var highWater sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(id) FROM edges WHERE kind = ? AND view_gen = ?`, graph.EdgeCalls, s.viewGen).Scan(&highWater); err != nil {
		panicOnFatal(err)
		return
	}
	if !highWater.Valid || highWater.Int64 <= 0 {
		return
	}

	for start := 0; start < len(sources); start += sourceBatchSize {
		end := min(start+sourceBatchSize, len(sources))
		payload, err := json.Marshal(sources[start:end])
		if err != nil {
			panicOnFatal(err)
			return
		}
		rows, err := s.db.Query(testCallProjectionSQL, string(payload), graph.EdgeCalls, s.viewGen, highWater.Int64)
		if err != nil {
			panicOnFatal(err)
			return
		}
		projected := make([]graph.TestEdgeProjection, 0, pageSize)
		for rows.Next() {
			var row graph.TestEdgeProjection
			if err := rows.Scan(&row.From, &row.To, &row.FilePath, &row.Line); err != nil {
				_ = rows.Close()
				panicOnFatal(err)
				return
			}
			row.Kind = graph.EdgeCalls
			projected = append(projected, row)
		}
		rowsErr := rows.Err()
		_ = rows.Close()
		if rowsErr != nil {
			panicOnFatal(rowsErr)
			return
		}

		for pageStart := 0; pageStart < len(projected); pageStart += pageSize {
			pageEnd := min(pageStart+pageSize, len(projected))
			if !yield(projected[pageStart:pageEnd]) {
				return
			}
		}
	}
}
