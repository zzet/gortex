package store_sqlite

import (
	"database/sql"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.EdgeTerminalStampPersister = (*Store)(nil)

const (
	edgeTerminalStampParamsPerRow = 7
	// Seven values per row plus one trailing generation binding; 140 rows use
	// 981 host parameters, leaving headroom below SQLite's conservative
	// 999-variable limit.
	edgeTerminalStampChunkSize = 140
)

type edgeTerminalStampStats struct {
	changedRows int
	statements  int
}

// PersistEdgeTerminalStamps updates only the resolver's two promoted terminal
// columns. Full confidence/provenance/metadata persistence remains on
// PersistEdgeAttributesBatch for semantic and LSP callers.
func (s *Store) PersistEdgeTerminalStamps(edges []*graph.Edge) {
	if _, err := s.persistEdgeTerminalStamps(edges); err != nil {
		panicOnFatal(err)
	}
}

func (s *Store) persistEdgeTerminalStamps(edges []*graph.Edge) (edgeTerminalStampStats, error) {
	var stats edgeTerminalStampStats
	if len(edges) == 0 {
		return stats, nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	for txStart := 0; txStart < len(edges); txStart += reindexChunkSize {
		txEnd := minInt(txStart+reindexChunkSize, len(edges))
		tx, err := s.beginWrite()
		if err != nil {
			return stats, err
		}
		txChanged := 0
		for start := txStart; start < txEnd; start += edgeTerminalStampChunkSize {
			end := minInt(start+edgeTerminalStampChunkSize, txEnd)
			query, args := edgeTerminalStampUpdateStatement(s.viewGen, edges[start:end])
			if len(args) == 0 {
				continue
			}
			result, err := tx.Exec(query, args...)
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
		stats.changedRows += txChanged
		if invalidatedAnalysis {
			s.analysisGenerationPresent = false
		}
		s.finishAnalysisMutationLocked(txChanged > 0)
	}
	return stats, nil
}

// edgeTerminalStampUpdateStatement builds one bounded exact-identity UPDATE.
// Duplicate identities within a statement retain their last input value;
// duplicates across statements are applied in input order, so the final value
// is likewise last-wins.
func edgeTerminalStampUpdateStatement(viewGen int64, edges []*graph.Edge) (string, []any) {
	updates := make([]*graph.Edge, 0, len(edges))
	positions := make(map[graph.EdgeIdentity]int, len(edges))
	for _, edge := range edges {
		if edge == nil {
			continue
		}
		identity := graph.EdgeIdentityFor(edge)
		if pos, ok := positions[identity]; ok {
			updates[pos] = edge
			continue
		}
		positions[identity] = len(updates)
		updates = append(updates, edge)
	}
	if len(updates) == 0 {
		return "", nil
	}

	var values strings.Builder
	values.Grow(len(updates) * len("(?,?,?,?,?,?,?),"))
	args := make([]any, 0, len(updates)*edgeTerminalStampParamsPerRow)
	for i, edge := range updates {
		if i > 0 {
			values.WriteByte(',')
		}
		values.WriteString("(?,?,?,?,?,?,?)")
		terminal, reason := edgeTerminalStampValues(edge.Meta)
		args = append(args,
			terminal, reason,
			edge.From, edge.To, string(edge.Kind), edge.FilePath, edge.Line,
		)
	}
	// One trailing bound value carries the generation for the whole statement.
	args = append(args, viewGen)

	query := `WITH updates(
		resolve_terminal, resolve_terminal_reason,
		from_id, to_id, kind, file_path, line
	) AS (VALUES ` + values.String() + `)
	UPDATE edges AS e
	SET resolve_terminal = u.resolve_terminal,
		resolve_terminal_reason = u.resolve_terminal_reason
	FROM updates AS u
	WHERE e.from_id = u.from_id
		AND e.to_id = u.to_id
		AND e.kind = u.kind
		AND e.file_path = u.file_path
		AND e.line = u.line
		AND e.view_gen = ?
		AND (e.resolve_terminal IS NOT u.resolve_terminal
			OR e.resolve_terminal_reason IS NOT u.resolve_terminal_reason)`
	return query, args
}

func edgeTerminalStampValues(meta map[string]any) (sql.NullBool, sql.NullString) {
	var terminal sql.NullBool
	if value, ok := meta[graph.EdgeMetaResolveTerminal].(bool); ok {
		terminal = sql.NullBool{Bool: value, Valid: true}
	}
	var reason sql.NullString
	if value, ok := meta[graph.EdgeMetaResolveTerminalReason].(string); ok {
		reason = sql.NullString{String: value, Valid: true}
	}
	return terminal, reason
}
