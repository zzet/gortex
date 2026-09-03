package store_sqlite

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// edgeExactDeleteByIdentitySQL deletes edges matching a VALUES list of
// full identities. The IN-over-JOIN shape is load-bearing: a correlated
// WHERE EXISTS against a constant VALUES table cannot be inverted by the
// planner and scans the whole edges table per chunk; driving the join
// from the VALUES side probes the edges primary key per identity.
//
// The generation is the sixth and last column of that key, so binding it after
// the five identity columns completes the seek instead of demoting it to a
// filter. Callers bind it once, after the VALUES arguments.
func edgeExactDeleteByIdentitySQL(values string) string {
	return `WITH wanted(from_id, to_id, kind, file_path, line) AS (VALUES ` + values + `)
DELETE FROM edges
WHERE id IN (
    SELECT e.id
    FROM edges AS e
    JOIN wanted AS w
      ON e.from_id = w.from_id
     AND e.to_id = w.to_id
     AND e.kind = w.kind
     AND e.file_path = w.file_path
     AND e.line = w.line
     AND e.view_gen = ?
)`
}

const exactEdgeRemoveChunkSize = 160 // 160*5 + 1 generation = 801 bound parameters.

// RemoveEdgesExact deletes complete logical edge identities through bounded
// VALUES joins in one transaction. It is the set-oriented sibling of
// RemoveEdge and preserves same-endpoint sibling call sites.
func (s *Store) RemoveEdgesExact(edges []*graph.Edge) int {
	if len(edges) == 0 {
		return 0
	}
	type key struct {
		from, to, kind, file string
		line                 int
	}
	keys := make([]key, 0, len(edges))
	seen := make(map[key]struct{}, len(edges))
	for _, edge := range edges {
		if edge == nil {
			continue
		}
		k := key{edge.From, edge.To, string(edge.Kind), edge.FilePath, edge.Line}
		if _, duplicate := seen[k]; duplicate {
			continue
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
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
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	removed := 0
	for start := 0; start < len(keys); start += exactEdgeRemoveChunkSize {
		end := minInt(start+exactEdgeRemoveChunkSize, len(keys))
		chunk := keys[start:end]
		var values strings.Builder
		args := make([]any, 0, len(chunk)*5+1)
		for i, k := range chunk {
			if i > 0 {
				values.WriteByte(',')
			}
			values.WriteString("(?,?,?,?,?)")
			args = append(args, k.from, k.to, k.kind, k.file, k.line)
		}
		args = append(args, s.viewGen)
		result, execErr := tx.Exec(edgeExactDeleteByIdentitySQL(values.String()), args...)
		if execErr != nil {
			panicOnFatal(execErr)
			return 0
		}
		rows, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			panicOnFatal(rowsErr)
			return 0
		}
		removed += int(rows)
	}
	if err := tx.Commit(); err != nil {
		panicOnFatal(err)
		return 0
	}
	committed = true
	changed := removed > 0
	s.finishAnalysisMutationLocked(changed)
	if changed {
		s.markMutationReceiptsIncompleteLocked()
	}
	return removed
}

var _ graph.ExactEdgeBatchRemover = (*Store)(nil)
