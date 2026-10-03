package store_sqlite

import (
	"context"
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

// AnalysisMembershipsContext reads only requested nodes. The active-generation
// check and memberships share a read snapshot, without waiting for writeMu.
func (s *Store) AnalysisMembershipsContext(ctx context.Context, generationID int64, nodeIDs []string) ([]graph.AnalysisMembership, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids, err := sortedUniqueAnalysisStrings(nodeIDs, "node id")
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var one int
	err = tx.QueryRowContext(ctx, `
		SELECT 1 FROM analysis_active_generation a
		JOIN analysis_generations g ON g.generation_id = a.generation_id
		WHERE a.slot = 1 AND a.generation_id = ? AND g.state = ?`, generationID, analysisGenerationReady).Scan(&one)
	if err == sql.ErrNoRows {
		return nil, graph.ErrAnalysisGenerationInactive
	}
	if err != nil {
		return nil, err
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, generationID)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT n.node_id, n.community_id, s.process_id
		FROM analysis_nodes n
		LEFT JOIN analysis_process_steps s
		  ON s.generation_id = n.generation_id AND s.node_rowid = n.id
		WHERE n.generation_id = ? AND n.node_id IN (`+analysisPlaceholders(len(ids))+`)
		ORDER BY n.node_id, s.process_id, s.ordinal`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var memberships []graph.AnalysisMembership
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var row graph.AnalysisMembership
		var community, process sql.NullString
		if err := rows.Scan(&row.NodeID, &community, &process); err != nil {
			return nil, err
		}
		row.CommunityID, row.ProcessID = community.String, process.String
		memberships = append(memberships, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return memberships, nil
}
