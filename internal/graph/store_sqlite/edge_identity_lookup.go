package store_sqlite

import (
	"cmp"
	"slices"

	"github.com/zzet/gortex/internal/graph"
)

const (
	exactEdgeIdentityParamsPerRow = 5
	// The runtime variable limit is the primary bound. This additional row cap
	// prevents an unusually permissive SQLite build from retaining an
	// unbounded VALUES relation in the driver.
	exactEdgeIdentityMaxRows = 2048
)

// sqliteEdgeIdentityLookupStats is package-private instrumentation for tests.
// Statements counts actual query attempts (including a failed oversized
// prepare); Retries makes adaptive variable-limit fallback observable.
type sqliteEdgeIdentityLookupStats struct {
	Statements    int
	Retries       int
	MaxKeys       int
	MaxBoundBytes int
}

var _ graph.EdgeIdentityBatchFinder = (*Store)(nil)

// FindEdgesByIdentities fetches current edges by the complete five-column
// logical key. The projection is optional on graph.Store so other backends
// remain compatible; resolver liveness uses it when present and retains its
// set-oriented candidate fallback otherwise.
func (s *Store) FindEdgesByIdentities(identities []graph.EdgeIdentity) map[graph.EdgeIdentity]*graph.Edge {
	edges, _ := s.findEdgesByIdentities(identities)
	return edges
}

func (s *Store) findEdgesByIdentities(identities []graph.EdgeIdentity) (map[graph.EdgeIdentity]*graph.Edge, sqliteEdgeIdentityLookupStats) {
	found := make(map[graph.EdgeIdentity]*graph.Edge)
	var stats sqliteEdgeIdentityLookupStats
	if len(identities) == 0 {
		return found, stats
	}

	// Deduplicate before binding so repeated resolver work neither consumes
	// SQLite variables nor inflates the bounded-query count.
	unique := make([]graph.EdgeIdentity, 0, len(identities))
	seen := make(map[graph.EdgeIdentity]struct{}, len(identities))
	for _, identity := range identities {
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}
		unique = append(unique, identity)
	}

	// The UNIQUE edge identity index has this exact key order. Sorting the
	// compact keys turns the inner exact-match probes from corpus-order random
	// walks into a forward B-tree walk. The query below independently restores
	// row-id order before decoding full rows, so index locality does not trade
	// away table-page locality and callers still recover their own order from
	// the result map.
	slices.SortFunc(unique, compareEdgeIdentityLookupKey)

	variableLimit := s.edgeIdentityVariableLimit()
	for start := 0; start < len(unique); {
		maxRows := batchRowsForVariableLimit(variableLimit, exactEdgeIdentityParamsPerRow, exactEdgeIdentityMaxRows)
		if remaining := len(unique) - start; maxRows > remaining {
			maxRows = remaining
		}

		args := make([]any, 0, maxRows*exactEdgeIdentityParamsPerRow)
		boundBytes := 0
		keyCount := 0
		for keyCount < maxRows {
			identity := unique[start+keyCount]
			rowArgs := []any{identity.From, identity.To, identity.Kind, identity.FilePath, identity.Line}
			rowBytes := sqliteBoundArgsBytes(rowArgs)
			if keyCount > 0 && boundBytes+rowBytes > sqliteBatchMaxBoundBytes {
				break
			}
			args = append(args, rowArgs...)
			boundBytes += rowBytes
			keyCount++
		}

		// The generation binds after the VALUES rows, matching its place in
		// the query text. It fits inside sqliteBatchVariableHeadroom.
		args = append(args, s.viewGen)

		query := exactEdgeIdentityQuery(keyCount)

		stats.Statements++
		if keyCount > stats.MaxKeys {
			stats.MaxKeys = keyCount
		}
		if boundBytes > stats.MaxBoundBytes {
			stats.MaxBoundBytes = boundBytes
		}

		rows, err := s.db.Query(query, args...)
		if tooManySQLVariables(err) && keyCount > 1 {
			stats.Retries++
			lowerBatchVariableLimit(&variableLimit, exactEdgeIdentityParamsPerRow, keyCount)
			s.rememberEdgeIdentityVariableLimit(variableLimit)
			continue
		}
		if err != nil {
			panicOnFatal(err)
			return found, stats
		}

		for rows.Next() {
			edge, scanErr := s.scanEdgeCursor(rows)
			if scanErr != nil {
				_ = rows.Close()
				panicOnFatal(scanErr)
				return found, stats
			}
			if edge == nil {
				continue
			}
			found[graph.EdgeIdentityFor(edge)] = edge
		}
		rowsErr := rows.Err()
		_ = rows.Close()
		if rowsErr != nil {
			panicOnFatal(rowsErr)
			return found, stats
		}
		start += keyCount
	}
	return found, stats
}

func compareEdgeIdentityLookupKey(left, right graph.EdgeIdentity) int {
	if order := cmp.Compare(left.From, right.From); order != 0 {
		return order
	}
	if order := cmp.Compare(left.To, right.To); order != 0 {
		return order
	}
	if order := cmp.Compare(left.Kind, right.Kind); order != 0 {
		return order
	}
	if order := cmp.Compare(left.FilePath, right.FilePath); order != 0 {
		return order
	}
	return cmp.Compare(left.Line, right.Line)
}

// exactEdgeIdentityQuery separates identity-index locality from payload-page
// locality in one SQLite snapshot. The bounded IN subquery materializes only
// integer row IDs; the outer primary-key walk then decodes full rows in table
// order. ORDER BY is satisfied by that row-ID walk (the plan test forbids a
// full-row temp sort).
//
// The generation is the sixth column of the edges UNIQUE key, so binding it in
// the inner probe lengthens that seek instead of filtering after it. The outer
// walk needs no filter of its own: edge ids are unique across generations, so
// the inner match already decided which rows it may fetch.
func exactEdgeIdentityQuery(rows int) string {
	return `WITH wanted(from_id, to_id, kind, file_path, line) AS (VALUES ` +
		multiValues(rows, exactEdgeIdentityParamsPerRow) + `)
SELECT ` + lookupQualifiedEdgeCols + `
FROM edges AS e
WHERE e.id IN (
      SELECT matched.id
      FROM wanted AS w
      JOIN edges AS matched
        ON matched.from_id = w.from_id
       AND matched.to_id = w.to_id
       AND matched.kind = w.kind
       AND matched.file_path = w.file_path
       AND matched.line = w.line
       AND matched.view_gen = ?
)
ORDER BY e.id`
}

func (s *Store) edgeIdentityVariableLimit() int {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.sqliteBatchVariableLimitLocked()
}

func (s *Store) rememberEdgeIdentityVariableLimit(limit int) {
	if limit < 1 {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.batchVariableLimit == 0 || limit < s.batchVariableLimit {
		s.batchVariableLimit = limit
	}
}
