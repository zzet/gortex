package store_sqlite

import (
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertion: *Store serves the value-reference placeholder scan.
var _ graph.ValueRefPlaceholderScanner = (*Store)(nil)

// ValueRefPlaceholderEdges implements graph.ValueRefPlaceholderScanner. The
// extractor emits these reads into one bare target namespace; applyRepoPrefix
// has preserved unresolved targets since before value-ref extraction existed,
// so snapshots containing value-ref candidates use the same form. The range
// rides the existing edges_by_to(to_id, kind) index and keeps the full edge
// payload because the resolver revalidates Meta["via"] and reads Meta["name"].
func (s *Store) ValueRefPlaceholderEdges() iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		edges := s.queryEdgesSQL(`SELECT `+lookupEdgeCols+`
FROM edges
WHERE to_id >= 'unresolved::valueref::'
  AND to_id < 'unresolved::valueref:;'
  AND kind = 'reads'
  AND view_gen = ?`, s.viewGen)
		for _, edge := range edges {
			if !yield(edge) {
				return
			}
		}
	}
}
