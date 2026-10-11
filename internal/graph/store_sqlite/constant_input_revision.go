package store_sqlite

import "sync/atomic"

// constantInputCounter covers committed constant sidecar changes separately
// from graph analysis clocks. Row-copy, ownership-mask and cleanup operations
// also advance it when they bypass ordinary graph mutation hooks. Access and
// increments are serialized by writeMu.
func (s *Store) constantInputCounter(g int64) *atomic.Uint64 {
	v, _ := s.constantInputRevisions.LoadOrStore(g, &atomic.Uint64{})
	return v.(*atomic.Uint64)
}
