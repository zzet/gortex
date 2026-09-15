package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

const analysisGenerationGCDefaultBatch = 1000

// analysisRetentionViewCap bounds the retention window in aggregate.
//
// Partitioning the window by view generation is what stops one busy view from
// evicting another's history, but it also makes the retained population scale
// as views × keep, and the number of live payload view generations is a
// runtime quantity, not a constant. This cap closes that: only the newest
// analysisRetentionViewCap views — ranked by the newest analysis each one
// holds — keep history at all. Older views keep their ACTIVE analysis (the
// window never ranks an active generation, so it is never a candidate here)
// and nothing else.
//
// The resulting bound on collectable history is analysisRetentionViewCap ×
// keep rows, plus at most one active generation per live view, and those are
// collected by the retirement sweep when their payload generation goes.
//
// 8 against the shipped keep of 2 (internal/mcp: analysisGenerationPruneKeep)
// is 16 retained manifest rows — comfortably above the handful of views a
// daemon routes at once, so in practice the cap costs nothing and only exists
// to make the population bounded.
//
// That 16 is asserted absolutely, not in terms of this constant, by
// TestPruneAnalysisGenerationsBoundsRetainedHistoryAcrossViews. Raising the cap
// goes red there on purpose: the bound is cited as the reason
// analysis_generations carries no view_gen index (schema.go), so widening it is
// a decision about that index too, not a free knob.
const analysisRetentionViewCap = 8

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

func (s *Store) PruneAnalysisGenerations(ctx context.Context, keep, batch int) error {
	if ctx == nil {
		return fmt.Errorf("analysis generation gc: nil context")
	}
	if keep == 0 {
		keep = 1
	}
	if keep < 1 {
		return fmt.Errorf("analysis generation gc: keep must be at least 1")
	}
	if batch == 0 {
		batch = analysisGenerationGCDefaultBatch
	}
	if batch < 1 || batch > analysisGenerationChunkLimit {
		return fmt.Errorf("analysis generation gc: batch %d outside 1..%d", batch, analysisGenerationChunkLimit)
	}

	// Materialize candidates before acquiring writeMu. Building generations
	// are never collected, and each chunk rechecks that its candidate is still
	// non-active and non-building.
	//
	// Retention is per PAYLOAD view generation. A global "newest keep" window
	// would let one busy view's analyses evict another view's entire history
	// the moment two views share the store, which is the collision this axis
	// exists to prevent.
	//
	// The second predicate is the aggregate bound. Per-view retention alone
	// scales as views × keep with no constant ceiling, so views are themselves
	// ranked — newest analysis first, which is why the rank rides a join on
	// MAX(generation_id) per view rather than the row's own id — and anything
	// past analysisRetentionViewCap keeps no history. DENSE_RANK, not
	// ROW_NUMBER: every row of one view has to share that view's rank.
	// Active generations are excluded before either window is computed, so
	// neither predicate can ever name the pointer's target.
	rows, err := s.db.QueryContext(ctx, `
		SELECT generation_id FROM (
			SELECT g.generation_id,
				ROW_NUMBER() OVER (PARTITION BY g.view_gen ORDER BY g.generation_id DESC) AS rank_in_view,
				DENSE_RANK() OVER (ORDER BY newest.newest_generation_id DESC) AS rank_of_view
			FROM analysis_generations g
			JOIN (
				SELECT view_gen, MAX(generation_id) AS newest_generation_id
				FROM analysis_generations GROUP BY view_gen
			) newest ON newest.view_gen = g.view_gen
			WHERE g.state != ? AND g.generation_id NOT IN (
				SELECT generation_id FROM analysis_active_generation
			)
		)
		WHERE rank_in_view > ? OR rank_of_view > ?
		ORDER BY generation_id DESC`, analysisGenerationBuilding, keep, analysisRetentionViewCap)
	if err != nil {
		return err
	}
	var candidates []int64
	for rows.Next() {
		var generationID int64
		if err := rows.Scan(&generationID); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, generationID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, generationID := range candidates {
		for _, table := range analysisGenerationGCTables {
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				removed, eligible, err := s.pruneAnalysisGenerationChunk(ctx, generationID, table, batch)
				if err != nil {
					return fmt.Errorf("analysis generation gc: generation %d %s: %w", generationID, table.name, err)
				}
				if !eligible {
					break
				}
				if removed == 0 {
					break
				}
			}
		}
		if err := s.finishPruneAnalysisGeneration(ctx, generationID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) pruneAnalysisGenerationChunk(ctx context.Context, generationID int64, table analysisGenerationGCTable, batch int) (int64, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginAnalysisWrite()
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

func (s *Store) finishPruneAnalysisGeneration(ctx context.Context, generationID int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginAnalysisWrite()
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	eligible, err := analysisGenerationPrunableTx(tx, generationID)
	if err != nil || !eligible {
		return err
	}
	for _, table := range []string{
		"analysis_process_steps", "analysis_process_files", "analysis_processes",
		"analysis_concept_relations", "analysis_concepts",
		"analysis_community_files", "analysis_nodes", "analysis_communities",
		"analysis_blobs", "analysis_generation_components",
	} {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE generation_id = ?`, generationID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("analysis generation gc: generation %d still has %d rows in %s", generationID, count, table)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM analysis_generations WHERE generation_id = ?`, generationID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// --- retirement fan-out -------------------------------------------------
//
// PruneAnalysisGenerations is the retention window: it keeps the newest few
// per view and never touches an active generation. Retiring a payload
// generation is the other axis — the corpus the analysis describes is being
// deleted, so every analysis stamped with that view goes, active pointer
// included.
//
// The analysis cache is the one generation-keyed population the payload sweep
// registries cannot name. payloadSweepTables is derived from viewGenSidecars
// plus generationMaskTables, and every table there carries view_gen in its own
// primary key; the analysis child rows hang off analysis_generations.
// generation_id instead, so they are addressed through the manifest.

// analysisGenerationIDsForView lists every analysis generation stamped with
// one payload view generation, newest first. The list is materialized before
// any gate is taken: the sweep's own chunk runner acquires writeMu per chunk.
func (s *Store) analysisGenerationIDsForView(ctx context.Context, viewGen int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT generation_id FROM analysis_generations WHERE view_gen = ? ORDER BY generation_id DESC`, viewGen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// deleteAnalysisPointerChunk clears one view generation's active analysis
// pointer. It runs before the manifest rows because the pointer holds
// ON DELETE RESTRICT against them; leaving it would refuse the manifest
// delete rather than cascade it.
func deleteAnalysisPointerChunk(viewGen int64) payloadSweepChunk {
	return func(ctx context.Context, tx *sql.Tx) (int64, error) {
		result, err := tx.ExecContext(ctx,
			`DELETE FROM analysis_active_generation WHERE view_gen = ?`, viewGen)
		if err != nil {
			return 0, err
		}
		return result.RowsAffected()
	}
}

// deleteAnalysisChildChunk reuses one retention-GC delete statement for the
// retirement sweep. The statements are identical — child-first, bounded by
// their own LIMIT — only the eligibility rule differs, and here eligibility is
// the payload generation's retiring state, which the chunk runner rechecks.
func deleteAnalysisChildChunk(table analysisGenerationGCTable, analysisGenerationID int64) payloadSweepChunk {
	return func(ctx context.Context, tx *sql.Tx) (int64, error) {
		var result sql.Result
		var err error
		if table.name == "nodes" {
			result, err = tx.ExecContext(ctx, table.delete, analysisGenerationID, analysisGenerationGCDefaultBatch)
		} else {
			result, err = tx.ExecContext(ctx, table.delete, analysisGenerationID, analysisGenerationID, analysisGenerationGCDefaultBatch)
		}
		if err != nil {
			return 0, err
		}
		return result.RowsAffected()
	}
}

// deleteAnalysisManifestChunk removes one analysis generation's manifest row
// once its children are gone. It is a single row, so the second pass the chunk
// runner makes removes nothing and ends the loop.
func deleteAnalysisManifestChunk(analysisGenerationID int64) payloadSweepChunk {
	return func(ctx context.Context, tx *sql.Tx) (int64, error) {
		result, err := tx.ExecContext(ctx,
			`DELETE FROM analysis_generations WHERE generation_id = ?`, analysisGenerationID)
		if err != nil {
			return 0, err
		}
		return result.RowsAffected()
	}
}
