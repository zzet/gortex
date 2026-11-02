package store_sqlite

import "github.com/zzet/gortex/internal/graph"

// BulkLoadWindow snapshots the owner while the writer is between drain chunks.
func (s *Store) BulkLoadWindow() string {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	switch {
	case s.generationBulkLoad != 0:
		return "generation_noop"
	case s.coordinatedBulkLoad:
		return "coordinated_noop"
	case s.bulkConn != nil:
		return "cold"
	default:
		return "none"
	}
}

// RecordFirstIndexDrain retains one value per repository, shared by every
// generation handle and temporary indexer over this open store.
func (s *Store) RecordFirstIndexDrain(repo string, observation graph.FirstIndexDrain) {
	s.firstIndexDrainsMu.Lock()
	defer s.firstIndexDrainsMu.Unlock()
	if s.firstIndexDrains == nil {
		s.firstIndexDrains = make(map[string]graph.FirstIndexDrain)
	}
	s.firstIndexDrains[repo] = observation
}

// LastFirstIndexDrains does no SQL or writer admission. Return a detached map
// so status and cached health payloads cannot mutate or race the live record.
func (s *Store) LastFirstIndexDrains() map[string]graph.FirstIndexDrain {
	s.firstIndexDrainsMu.Lock()
	defer s.firstIndexDrainsMu.Unlock()
	if len(s.firstIndexDrains) == 0 {
		return nil
	}
	out := make(map[string]graph.FirstIndexDrain, len(s.firstIndexDrains))
	for repo, observation := range s.firstIndexDrains {
		out[repo] = observation
	}
	return out
}

// DenseIndexesMissing reports the existing atomic repair record without SQL.
func (s *Store) DenseIndexesMissing() []string { return s.denseIndexesMissingNames() }
