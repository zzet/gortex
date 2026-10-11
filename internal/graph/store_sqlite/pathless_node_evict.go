package store_sqlite

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// EvictPathlessNodesByIDs removes the nodes of the supplied bounded ID set and
// every incident edge in one transaction, scoped to the handle's payload view
// generation. It is the SQLite side of graph.PathlessNodeBatchEvicter: the
// caller names identities that live at no source file, which no file eviction
// reaches. Each DELETE is driven by the nodes primary key, in chunks of
// contractNodeEvictChunkSize.
func (s *Store) EvictPathlessNodesByIDs(ids []string) (nodesRemoved, edgesRemoved int) {
	if len(ids) == 0 {
		return 0, 0
	}
	unique := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return 0, 0
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.invalidateAnalysisBeforeMutationLocked() {
		return 0, 0
	}
	tx, err := s.beginWrite()
	if err != nil {
		panicOnFatal(err)
		return 0, 0
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for start := 0; start < len(unique); start += contractNodeEvictChunkSize {
		end := minInt(start+contractNodeEvictChunkSize, len(unique))
		chunk := unique[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, 0, len(chunk)+1)
		for _, id := range chunk {
			args = append(args, id)
		}
		args = append(args, s.viewGen)
		pathlessIDs := `SELECT id FROM nodes WHERE id IN (` + placeholders + `) AND view_gen = ?`
		edgeArgs := append(append([]any{}, args...), s.viewGen)
		for _, column := range []string{"from_id", "to_id"} {
			result, execErr := tx.Exec(`DELETE FROM edges WHERE `+column+` IN (`+pathlessIDs+`) AND view_gen = ?`, edgeArgs...)
			if execErr != nil {
				panicOnFatal(execErr)
				return 0, edgesRemoved
			}
			rows, rowsErr := result.RowsAffected()
			if rowsErr != nil {
				panicOnFatal(rowsErr)
				return 0, edgesRemoved
			}
			edgesRemoved += int(rows)
		}
		result, execErr := tx.Exec(`DELETE FROM nodes WHERE id IN (`+placeholders+`) AND view_gen = ?`, args...)
		if execErr != nil {
			panicOnFatal(execErr)
			return nodesRemoved, edgesRemoved
		}
		rows, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			panicOnFatal(rowsErr)
			return nodesRemoved, edgesRemoved
		}
		nodesRemoved += int(rows)
	}
	if err := tx.Commit(); err != nil {
		panicOnFatal(err)
		return 0, 0
	}
	committed = true
	changed := nodesRemoved > 0 || edgesRemoved > 0
	s.finishAnalysisMutationLocked(changed)
	if changed {
		s.markMutationReceiptsIncompleteLocked()
	}
	return nodesRemoved, edgesRemoved
}

var _ graph.PathlessNodeBatchEvicter = (*Store)(nil)
