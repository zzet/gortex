package store_sqlite

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// EvictEdgesFromSourcesByKinds reconciles a derived-edge frontier with one
// indexed DELETE. Both source and kind scopes stay inside SQLite; no edge rows
// cross into Go merely to be deleted.
//
// Scoped to the handle's payload view generation: the frontier comes from an
// indexing pass over one checkout, and the replacement edges land at that same
// generation.
func (s *Store) EvictEdgesFromSourcesByKinds(
	ctx context.Context,
	sourceIDs []string,
	kinds []graph.EdgeKind,
) (int, error) {
	if len(sourceIDs) == 0 || len(kinds) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	sources := make([]string, 0, len(sourceIDs))
	seenSources := make(map[string]struct{}, len(sourceIDs))
	for _, id := range sourceIDs {
		if id == "" {
			continue
		}
		if _, seen := seenSources[id]; seen {
			continue
		}
		seenSources[id] = struct{}{}
		sources = append(sources, id)
	}
	kindValues := make([]string, 0, len(kinds))
	seenKinds := make(map[graph.EdgeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		if _, seen := seenKinds[kind]; seen {
			continue
		}
		seenKinds[kind] = struct{}{}
		kindValues = append(kindValues, string(kind))
	}
	if len(sources) == 0 || len(kindValues) == 0 {
		return 0, nil
	}
	sourcesJSON, ok := projectionJSON(sources)
	if !ok {
		return 0, nil
	}
	kindsJSON, ok := projectionJSON(kindValues)
	if !ok {
		return 0, nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !s.invalidateAnalysisBeforeMutationLocked() {
		return 0, nil
	}
	tx, err := s.beginWrite()
	if err != nil {
		panicOnFatal(err)
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
DELETE FROM edges
WHERE from_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))
  AND kind IN (SELECT CAST(value AS TEXT) FROM json_each(?))
  AND view_gen = ?`, sourcesJSON, kindsJSON, s.viewGen)
	if err != nil {
		_ = tx.Rollback()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		panicOnFatal(err)
		return 0, err
	}
	removed64, err := res.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		panicOnFatal(err)
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		panicOnFatal(err)
		return 0, err
	}
	changed := removed64 > 0
	s.finishAnalysisMutationLocked(changed)
	if changed {
		s.markMutationReceiptsIncompleteLocked()
	}
	return int(removed64), nil
}

var _ graph.ScopedEdgeKindEvicter = (*Store)(nil)
