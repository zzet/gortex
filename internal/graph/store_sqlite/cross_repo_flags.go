package store_sqlite

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const crossRepoFlagChunkSize = 190 // 190 rows * 5 values = 950 host parameters.

var _ graph.CrossRepoFlagMarker = (*Store)(nil)

// MarkEdgesCrossRepo promotes only the cross_repo column of existing logical
// edges. It deliberately does not reuse PersistEdgeAttributesBatch: candidate
// projections omit opaque metadata, and a full attribute rewrite would erase
// fields the detector never read.
func (s *Store) MarkEdgesCrossRepo(edges []*graph.Edge) int {
	changed, _, err := s.markEdgesCrossRepo(edges)
	if err != nil {
		panicOnFatal(err)
	}
	return changed
}

// markEdgesCrossRepo also reports statement count so focused tests can lock in
// the set-oriented contract without exposing instrumentation through graph.
func (s *Store) markEdgesCrossRepo(edges []*graph.Edge) (changed, statements int, err error) {
	if len(edges) == 0 {
		return 0, 0, nil
	}

	unique := make([]*graph.Edge, 0, len(edges))
	seen := make(map[edgeAttributeKey]struct{}, len(edges))
	for _, edge := range edges {
		if edge == nil {
			continue
		}
		key := edgeAttributeKey{from: edge.From, to: edge.To, kind: string(edge.Kind), filePath: edge.FilePath, line: edge.Line}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, edge)
	}
	if len(unique) == 0 {
		return 0, 0, nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	for txStart := 0; txStart < len(unique); txStart += reindexChunkSize {
		txEnd := minInt(txStart+reindexChunkSize, len(unique))
		tx, beginErr := s.beginWrite()
		if beginErr != nil {
			return changed, statements, beginErr
		}
		txChanged := 0
		for start := txStart; start < txEnd; start += crossRepoFlagChunkSize {
			end := minInt(start+crossRepoFlagChunkSize, txEnd)
			query, args := crossRepoFlagStatement(s.viewGen, unique[start:end])
			if len(args) == 0 {
				continue
			}
			result, execErr := tx.Exec(query, args...)
			statements++
			if execErr != nil {
				_ = tx.Rollback()
				return changed, statements, execErr
			}
			rows, rowsErr := result.RowsAffected()
			if rowsErr != nil {
				_ = tx.Rollback()
				return changed, statements, rowsErr
			}
			txChanged += int(rows)
		}

		invalidatedAnalysis := false
		if txChanged > 0 && s.analysisGenerationPresent {
			if invalidateErr := invalidateAnalysisGenerationTx(tx); invalidateErr != nil {
				_ = tx.Rollback()
				return changed, statements, invalidateErr
			}
			invalidatedAnalysis = true
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return changed, statements, commitErr
		}
		changed += txChanged
		if invalidatedAnalysis {
			s.analysisGenerationPresent = false
		}
		s.finishAnalysisMutationLocked(txChanged > 0)
	}
	return changed, statements, nil
}

func crossRepoFlagStatement(viewGen int64, edges []*graph.Edge) (string, []any) {
	var query strings.Builder
	query.WriteString("WITH requested(from_id,to_id,kind,file_path,line) AS (VALUES ")
	args := make([]any, 0, len(edges)*5)
	rows := 0
	for _, edge := range edges {
		if edge == nil {
			continue
		}
		if rows > 0 {
			query.WriteByte(',')
		}
		query.WriteString("(?,?,?,?,?)")
		args = append(args, edge.From, edge.To, string(edge.Kind), edge.FilePath, edge.Line)
		rows++
	}
	if rows == 0 {
		return "", nil
	}
	args = append(args, viewGen)
	query.WriteString(`)
UPDATE edges
SET cross_repo = 1
WHERE cross_repo = 0
  AND id IN (
    SELECT e.id
    FROM requested AS r
    JOIN edges AS e
      ON e.from_id = r.from_id
     AND e.to_id = r.to_id
     AND e.kind = r.kind
     AND e.file_path = r.file_path
     AND e.line = r.line
     AND e.view_gen = ?
  )`)
	return query.String(), args
}
