package store_sqlite

import "sync/atomic"

// constantInputCounter covers committed constant sidecar changes separately
// from graph analysis clocks. Row-copy, ownership-mask and cleanup operations
// also advance it when they bypass ordinary graph mutation hooks. Access and
// increments are serialized by writeMu.
type generationInputRevisions struct {
	constants atomic.Uint64
	bindings  atomic.Uint64
}

func (s *Store) generationInputCounters(g int64) *generationInputRevisions {
	if v, ok := s.constantInputRevisions.Load(g); ok {
		return v.(*generationInputRevisions)
	}
	v, _ := s.constantInputRevisions.LoadOrStore(g, &generationInputRevisions{})
	return v.(*generationInputRevisions)
}

func (s *Store) constantInputCounter(g int64) *atomic.Uint64 {
	return &s.generationInputCounters(g).constants
}

// Compiler binding sidecars do not change a node/edge/mask projection, so
// they have their own clock. Locked publication witnesses include both clocks.
func (s *Store) semanticBindingInputCounter(g int64) *atomic.Uint64 {
	return &s.generationInputCounters(g).bindings
}
