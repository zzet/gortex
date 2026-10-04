package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

const analysisGenerationGCDefaultBatch = 1000

type analysisGenerationGCTable struct {
	name   string
	delete string
}

// Child-first order is intentional. The final generation DELETE must never
// trigger a large cascade while holding writeMu; by then every child table is
// proven empty.
var analysisGenerationGCTables = [...]analysisGenerationGCTable{
	{name: "process_steps", delete: `DELETE FROM analysis_process_steps WHERE generation_id = ? AND (process_id, ordinal) IN (SELECT process_id, ordinal FROM analysis_process_steps WHERE generation_id = ? LIMIT ?)`},
	{name: "process_files", delete: `DELETE FROM analysis_process_files WHERE generation_id = ? AND (process_id, ordinal) IN (SELECT process_id, ordinal FROM analysis_process_files WHERE generation_id = ? LIMIT ?)`},
	{name: "processes", delete: `DELETE FROM analysis_processes WHERE generation_id = ? AND process_id IN (SELECT process_id FROM analysis_processes WHERE generation_id = ? LIMIT ?)`},
	{name: "concept_relations", delete: `DELETE FROM analysis_concept_relations WHERE generation_id = ? AND (token, rank, related_token) IN (SELECT token, rank, related_token FROM analysis_concept_relations WHERE generation_id = ? LIMIT ?)`},
	{name: "concepts", delete: `DELETE FROM analysis_concepts WHERE generation_id = ? AND token IN (SELECT token FROM analysis_concepts WHERE generation_id = ? LIMIT ?)`},
	{name: "community_files", delete: `DELETE FROM analysis_community_files WHERE generation_id = ? AND (community_id, ordinal) IN (SELECT community_id, ordinal FROM analysis_community_files WHERE generation_id = ? LIMIT ?)`},
	{name: "nodes", delete: `DELETE FROM analysis_nodes WHERE id IN (SELECT id FROM analysis_nodes WHERE generation_id = ? LIMIT ?)`},
	{name: "communities", delete: `DELETE FROM analysis_communities WHERE generation_id = ? AND community_id IN (SELECT community_id FROM analysis_communities WHERE generation_id = ? LIMIT ?)`},
	{name: "blobs", delete: `DELETE FROM analysis_blobs WHERE generation_id = ? AND component IN (SELECT component FROM analysis_blobs WHERE generation_id = ? LIMIT ?)`},
	{name: "component_seals", delete: `DELETE FROM analysis_generation_components WHERE generation_id = ? AND component IN (SELECT component FROM analysis_generation_components WHERE generation_id = ? LIMIT ?)`},
}

// PruneAnalysisGenerations returns rows deleted by committed chunks, even if
// a later chunk fails. Rolled-back chunks do not contribute to the count.
func (s *Store) PruneAnalysisGenerations(ctx context.Context, keep, batch int) (removed int64, err error) {
	if ctx == nil {
		return removed, fmt.Errorf("analysis generation gc: nil context")
	}
	if keep == 0 {
		keep = 1
	}
	if keep < 1 {
		return removed, fmt.Errorf("analysis generation gc: keep must be at least 1")
	}
	if batch == 0 {
		batch = analysisGenerationGCDefaultBatch
	}
	if batch < 1 || batch > analysisGenerationChunkLimit {
		return removed, fmt.Errorf("analysis generation gc: batch %d outside 1..%d", batch, analysisGenerationChunkLimit)
	}

	// Materialize candidates before acquiring writeMu. Building generations
	// are never collected, and each chunk rechecks that its candidate is still
	// non-active and non-building.
	rows, err := s.db.QueryContext(ctx, `
		SELECT generation_id FROM analysis_generations
		WHERE state != ? AND generation_id NOT IN (
			SELECT generation_id FROM analysis_active_generation
		)
		ORDER BY generation_id DESC LIMIT -1 OFFSET ?`, analysisGenerationBuilding, keep)
	if err != nil {
		return removed, err
	}
	var candidates []int64
	for rows.Next() {
		var generationID int64
		if err := rows.Scan(&generationID); err != nil {
			rows.Close()
			return removed, err
		}
		candidates = append(candidates, generationID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return removed, err
	}
	if err := rows.Close(); err != nil {
		return removed, err
	}

	// Keep selection newest-first above; delete the eligible backlog oldest-first
	// so new generations cannot displace a partially collected one on retry.
	for i := len(candidates) - 1; i >= 0; i-- {
		generationID := candidates[i]
		for _, table := range analysisGenerationGCTables {
			for {
				if err := ctx.Err(); err != nil {
					return removed, err
				}
				chunkRemoved, eligible, err := s.pruneAnalysisGenerationChunk(ctx, generationID, table, batch)
				if err != nil {
					return removed, fmt.Errorf("analysis generation gc: generation %d %s: %w", generationID, table.name, err)
				}
				removed += chunkRemoved
				if !eligible {
					break
				}
				if chunkRemoved == 0 {
					break
				}
			}
		}
		chunkRemoved, err := s.finishPruneAnalysisGeneration(ctx, generationID)
		if err != nil {
			return removed, err
		}
		removed += chunkRemoved
	}
	return removed, nil
}

func (s *Store) pruneAnalysisGenerationChunk(ctx context.Context, generationID int64, table analysisGenerationGCTable, batch int) (int64, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return 0, false, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	eligible, err := analysisGenerationPrunableTx(tx, generationID)
	if err != nil || !eligible {
		return 0, eligible, err
	}
	var result sql.Result
	if table.name == "nodes" {
		result, err = tx.ExecContext(ctx, table.delete, generationID, batch)
	} else {
		result, err = tx.ExecContext(ctx, table.delete, generationID, generationID, batch)
	}
	if err != nil {
		return 0, false, err
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	committed = true
	return removed, true, nil
}

func analysisGenerationPrunableTx(tx *sql.Tx, generationID int64) (bool, error) {
	var count int
	err := tx.QueryRow(`
		SELECT COUNT(*) FROM analysis_generations g
		WHERE g.generation_id = ? AND g.state != ? AND NOT EXISTS (
			SELECT 1 FROM analysis_active_generation a WHERE a.generation_id = g.generation_id
		)`, generationID, analysisGenerationBuilding).Scan(&count)
	return count == 1, err
}

func (s *Store) finishPruneAnalysisGeneration(ctx context.Context, generationID int64) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	eligible, err := analysisGenerationPrunableTx(tx, generationID)
	if err != nil || !eligible {
		return 0, err
	}
	for _, table := range []string{
		"analysis_process_steps", "analysis_process_files", "analysis_processes",
		"analysis_concept_relations", "analysis_concepts",
		"analysis_community_files", "analysis_nodes", "analysis_communities",
		"analysis_blobs", "analysis_generation_components",
	} {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE generation_id = ?`, generationID).Scan(&count); err != nil {
			return 0, err
		}
		if count != 0 {
			return 0, fmt.Errorf("analysis generation gc: generation %d still has %d rows in %s", generationID, count, table)
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM analysis_generations WHERE generation_id = ?`, generationID)
	if err != nil {
		return 0, err
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	committed = true
	return removed, nil
}
