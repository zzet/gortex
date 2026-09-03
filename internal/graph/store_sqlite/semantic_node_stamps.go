package store_sqlite

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.SemanticNodeStampWriter = (*Store)(nil)

const (
	semanticNodeStampParamsPerRow = 4
	// Four values per row plus one trailing generation binding; 240 rows use
	// 961 host parameters, leaving headroom below SQLite's conservative
	// 999-variable limit.
	semanticNodeStampChunkSize = 240
)

type semanticNodeStampStats struct {
	enriched    int
	changedRows int
	statements  int
}

// PersistSemanticNodeStamps applies compiler-derived type stamps without
// fetching or rewriting complete node rows. Its return value preserves the
// provider coverage contract: existing nodes with a non-empty SemanticType are
// counted even when their persisted value was already identical.
func (s *Store) PersistSemanticNodeStamps(stamps []graph.SemanticNodeStamp) int {
	stats, err := s.persistSemanticNodeStamps(stamps)
	if err != nil {
		panicOnFatal(err)
		return 0
	}
	return stats.enriched
}

func (s *Store) persistSemanticNodeStamps(stamps []graph.SemanticNodeStamp) (semanticNodeStampStats, error) {
	var stats semanticNodeStampStats
	if len(stamps) == 0 {
		return stats, nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	for txStart := 0; txStart < len(stamps); txStart += reindexChunkSize {
		txEnd := minInt(txStart+reindexChunkSize, len(stamps))
		tx, err := s.beginWrite()
		if err != nil {
			return stats, err
		}
		txEnriched := 0
		txChanged := 0
		for start := txStart; start < txEnd; start += semanticNodeStampChunkSize {
			end := minInt(start+semanticNodeStampChunkSize, txEnd)
			countQuery, updateQuery, args := semanticNodeStampStatements(s.viewGen, stamps[start:end])
			if len(args) == 0 {
				continue
			}

			var enriched int
			if err := tx.QueryRow(countQuery, args...).Scan(&enriched); err != nil {
				_ = tx.Rollback()
				return stats, err
			}
			stats.statements++
			txEnriched += enriched

			result, err := tx.Exec(updateQuery, args...)
			stats.statements++
			if err != nil {
				_ = tx.Rollback()
				return stats, err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				_ = tx.Rollback()
				return stats, err
			}
			txChanged += int(changed)
		}

		invalidatedAnalysis := false
		if txChanged > 0 && s.analysisGenerationPresent {
			if err := invalidateAnalysisGenerationTx(tx); err != nil {
				_ = tx.Rollback()
				return stats, err
			}
			invalidatedAnalysis = true
		}
		if err := tx.Commit(); err != nil {
			return stats, err
		}
		stats.enriched += txEnriched
		stats.changedRows += txChanged
		if invalidatedAnalysis {
			s.analysisGenerationPresent = false
		}
		s.finishAnalysisMutationLocked(txChanged > 0)
	}
	return stats, nil
}

// semanticNodeStampStatements returns one predicate-shaped count and one
// set-oriented update over the same VALUES relation. Empty type fields preserve
// their columns; semantic_source changes only when at least one type is set.
//
// Both statements take the same argument list, so the trailing generation
// binding pairs the joined node row with the writing handle for the count and
// the update alike — a count over a generation the update cannot reach would
// report coverage the store never gained.
func semanticNodeStampStatements(viewGen int64, stamps []graph.SemanticNodeStamp) (countQuery, updateQuery string, args []any) {
	updates := make([]graph.SemanticNodeStamp, 0, len(stamps))
	positions := make(map[string]int, len(stamps))
	for _, stamp := range stamps {
		if stamp.NodeID == "" || (stamp.SemanticType == "" && stamp.ReturnType == "") {
			continue
		}
		if pos, ok := positions[stamp.NodeID]; ok {
			updates[pos] = stamp
			continue
		}
		positions[stamp.NodeID] = len(updates)
		updates = append(updates, stamp)
	}
	if len(updates) == 0 {
		return "", "", nil
	}

	var values strings.Builder
	values.Grow(len(updates) * len("(?,?,?,?),"))
	args = make([]any, 0, len(updates)*semanticNodeStampParamsPerRow)
	for i, stamp := range updates {
		if i > 0 {
			values.WriteByte(',')
		}
		values.WriteString("(?,?,?,?)")
		args = append(args, stamp.NodeID, stamp.SemanticType, stamp.ReturnType, stamp.SemanticSource)
	}
	args = append(args, viewGen)

	withUpdates := `WITH updates(node_id, semantic_type, return_type, semantic_source) AS (VALUES ` + values.String() + `)`
	countQuery = withUpdates + `
	SELECT COUNT(*)
	FROM nodes AS n
	JOIN updates AS u ON u.node_id = n.id
	WHERE u.semantic_type <> ''
		AND n.view_gen = ?`
	updateQuery = withUpdates + `
	UPDATE nodes AS n
	SET semantic_type = CASE WHEN u.semantic_type <> '' THEN u.semantic_type ELSE n.semantic_type END,
		return_type = CASE WHEN u.return_type <> '' THEN u.return_type ELSE n.return_type END,
		semantic_source = u.semantic_source
	FROM updates AS u
	WHERE n.id = u.node_id
		AND n.view_gen = ?
		AND (u.semantic_type <> '' OR u.return_type <> '')
		AND ((u.semantic_type <> '' AND n.semantic_type IS NOT u.semantic_type)
			OR (u.return_type <> '' AND n.return_type IS NOT u.return_type)
			OR n.semantic_source IS NOT u.semantic_source)`
	return countQuery, updateQuery, args
}
