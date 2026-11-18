package store_sqlite

import (
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

// outlineCommunitiesQuery never hydrates the partition's members or files.
// The membership index probes one community at a time and EXISTS stops at its
// first visible member. Node identity includes the selected payload generation.
func outlineCommunitiesQuery(generationID, viewGen int64, scope graph.NodeScope, limit int, cursorID string) outlineQuery {
	predicate, scopeArgs := outlineScopePredicate(scope, "n")
	query := `SELECT c.community_id, c.label, c.hub, c.parent_id, c.size, c.cohesion
		FROM analysis_communities AS c
		WHERE c.generation_id = ? AND c.community_id > ? AND EXISTS (
			SELECT 1 FROM analysis_nodes AS a INDEXED BY analysis_nodes_by_community
			CROSS JOIN nodes AS n ON n.id = a.node_id AND n.view_gen = ?
			WHERE a.generation_id = c.generation_id AND a.community_id = c.community_id AND ` + predicate + `)
		ORDER BY c.community_id LIMIT ?`
	args := append([]any{generationID, cursorID, viewGen}, scopeArgs...)
	return outlineQuery{sql: query, args: append(args, limit+1)}
}

func (s *Store) ScopedAnalysisCommunitySummaries(generationID int64, scope graph.NodeScope, limit int, cursorID string) ([]graph.AnalysisCommunitySummary, string, error) {
	limit, err := boundedAnalysisLimit(limit)
	if err != nil {
		return nil, "", err
	}
	ctx := s.readContext()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback() }()
	// Check the publication and read its rows in one snapshot. A concurrent
	// mutation cannot make an old receipt select a newer partition's members.
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM analysis_active_generation AS a
		JOIN analysis_generations AS g ON g.generation_id = a.generation_id
		WHERE a.view_gen = ? AND a.slot = 1 AND a.generation_id = ? AND g.state = ?`,
		s.viewGen, generationID, analysisGenerationReady).Scan(&one)
	if err == sql.ErrNoRows {
		return nil, "", graph.ErrAnalysisGenerationInactive
	}
	if err != nil {
		return nil, "", err
	}
	query := outlineCommunitiesQuery(generationID, s.viewGen, scope, limit, cursorID)
	rows, err := tx.QueryContext(ctx, query.sql, query.args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	items := make([]graph.AnalysisCommunitySummary, 0, limit+1)
	for rows.Next() {
		var item graph.AnalysisCommunitySummary
		if err := rows.Scan(&item.ID, &item.Label, &item.Hub, &item.ParentID, &item.Size, &item.Cohesion); err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if err := rows.Close(); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = items[len(items)-1].ID
	}
	return items, next, nil
}

var _ graph.ScopedAnalysisCommunityReader = (*Store)(nil)
