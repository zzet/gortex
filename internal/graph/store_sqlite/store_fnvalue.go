package store_sqlite

import (
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertion: *Store serves the fn-value placeholder scan.
var _ graph.FnValuePlaceholderScanner = (*Store)(nil)

// FnValuePlaceholderEdges implements graph.FnValuePlaceholderScanner: it yields
// only the fn-value gate's placeholder edges, the exact inverse of the
// fn-value exclusion EdgesWithUnresolvedTarget applies. The predicate is the
// SAME two-form filter the v2 migration's dedupeFnValuePlaceholderEdges uses:
// the bare `unresolved::fnvalue::` range rides edges_by_to(to_id) (the ':;'
// range end is ':'+1, one past the marker); the multi-repo COPY-rewrite infix
// form is caught by is_unresolved = 1 + LIKE. Full column set incl. meta — the
// gate reads Meta["via"] and the captured fn_value_name off each placeholder.
//
// The whole point is that the gate no longer has to scan the entire
// EdgeReferences kind (placeholders + every real reference) and Go-filter on
// every whole-graph synthesizer pass; it pulls the handful of placeholders
// straight off the index instead.
// The two forms run as SEPARATE queries: inside an OR, the planner's
// implication prover would not bind the partial edges_fnvalue_prefixed
// index to the infix arm and walked every unresolved edge instead
// (measured against a production store). Standalone, each arm rides its
// own index; the namespaces are disjoint (the bare form starts at the
// marker, the infix form requires a repo prefix before it), so no dedupe.
func (s *Store) FnValuePlaceholderEdges() iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		bare := s.queryEdgesSQL(`SELECT `+lookupEdgeCols+`
FROM edges
WHERE to_id >= 'unresolved::fnvalue::' AND to_id < 'unresolved::fnvalue:;' AND view_gen = ?`, s.viewGen)
		for _, e := range bare {
			if !yield(e) {
				return
			}
		}
		prefixed := s.queryEdgesSQL(`SELECT `+lookupEdgeCols+`
FROM edges
WHERE to_id LIKE '%::unresolved::fnvalue::%' AND view_gen = ?`, s.viewGen)
		for _, e := range prefixed {
			if !yield(e) {
				return
			}
		}
	}
}
