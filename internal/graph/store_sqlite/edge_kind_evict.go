package store_sqlite

import "github.com/zzet/gortex/internal/graph"

// EvictEdgesByKinds removes a derived edge generation with one SQLite DELETE.
// Reconciliation uses this before publishing a replacement batch, replacing
// thousands of individual transactions and analysis-generation invalidations.
//
// Scoped to the handle's payload view generation: the replacement batch that
// follows is written at that generation, so the sweep it pairs with must retire
// exactly the rows that batch is about to supersede and nothing else.
func (s *Store) EvictEdgesByKinds(kinds []graph.EdgeKind) int {
	if len(kinds) == 0 {
		return 0
	}
	values := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		values = append(values, string(kind))
	}
	kindsJSON, ok := projectionJSON(values)
	if !ok {
		return 0
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.invalidateAnalysisBeforeMutationLocked() {
		return 0
	}
	tx, err := s.beginWrite()
	if err != nil {
		panicOnFatal(err)
		return 0
	}
	res, err := tx.Exec(`
DELETE FROM edges
WHERE kind IN (SELECT CAST(value AS TEXT) FROM json_each(?))
  AND view_gen = ?`, kindsJSON, s.viewGen)
	if err != nil {
		_ = tx.Rollback()
		panicOnFatal(err)
		return 0
	}
	removed64, err := res.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		panicOnFatal(err)
		return 0
	}
	if err := tx.Commit(); err != nil {
		panicOnFatal(err)
		return 0
	}
	changed := removed64 > 0
	s.finishAnalysisMutationLocked(changed)
	if changed {
		s.markMutationReceiptsIncompleteLocked()
	}
	return int(removed64)
}

var _ graph.EdgeKindEvicter = (*Store)(nil)
