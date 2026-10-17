package store_sqlite

import (
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

// EdgesByKindKindFirst is EdgesByKind driven by the kind index whatever the
// handle's generation. EdgesByKind reads a derived generation through its
// generation index because derived generations are usually sparse working-tree
// layers; a dedicated root generation is a whole corpus, and that path then
// reads every edge of it for one kind. A caller that knows the handle is the
// bottom of a composed view (a whole corpus) asks for the kind-first plan: the
// rows of one kind, filtered to the generation.
func (s *Store) EdgesByKindKindFirst(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		for _, e := range s.queryEdgesSQL(baseEdgesByKindSQL, string(kind), s.viewGen) {
			if !yield(e) {
				return
			}
		}
	}
}
