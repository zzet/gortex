package store_sqlite

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const (
	analysisQueryDefaultLimit = 100
	analysisQueryMaxLimit     = 1000
)

func boundedAnalysisLimit(limit int) (int, error) {
	if limit == 0 {
		return analysisQueryDefaultLimit, nil
	}
	if limit < 0 || limit > analysisQueryMaxLimit {
		return 0, fmt.Errorf("analysis generation: limit %d outside 1..%d", limit, analysisQueryMaxLimit)
	}
	return limit, nil
}

func analysisPlaceholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func (s *Store) LoadActiveAnalysisHeader(formatVersion uint32) (graph.AnalysisGenerationHeader, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginAnalysisWrite()
	if err != nil {
		return graph.AnalysisGenerationHeader{}, false, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var generationID int64
	if err := tx.QueryRow(
		`SELECT generation_id FROM analysis_active_generation WHERE view_gen = ? AND slot = 1`,
		s.viewGen,
	).Scan(&generationID); err != nil {
		if err == sql.ErrNoRows {
			return graph.AnalysisGenerationHeader{}, false, nil
		}
		return graph.AnalysisGenerationHeader{}, false, err
	}
	header, state, err := loadAnalysisGenerationHeaderTx(tx, generationID)
	if err == nil && state == analysisGenerationReady && header.FormatVersion == formatVersion {
		header, err = validateAnalysisGenerationTx(tx, generationID)
	}
	if err != nil || state != analysisGenerationReady {
		reason := err
		if reason == nil {
			reason = fmt.Errorf("state is %d, want ready", state)
		}
		if _, clearErr := tx.Exec(`DELETE FROM analysis_active_generation WHERE view_gen = ? AND slot = 1 AND generation_id = ?`, s.viewGen, generationID); clearErr != nil {
			return graph.AnalysisGenerationHeader{}, false, clearErr
		}
		if _, staleErr := tx.Exec(`UPDATE analysis_generations SET state = ? WHERE generation_id = ?`, analysisGenerationStale, generationID); staleErr != nil {
			return graph.AnalysisGenerationHeader{}, false, staleErr
		}
		// analysisGenerationPresent is the mutation hot path's latch and it
		// lives on the shared storeCore, so it may only be cleared once NO
		// view's pointer remains. Dropping it because this view's pointer went
		// away would let the next graph mutation skip durable invalidation
		// while another view still has an active analysis — the one way a
		// restart could resurrect stale analysis.
		remaining, remainingErr := analysisPointerPresentTx(tx)
		if remainingErr != nil {
			return graph.AnalysisGenerationHeader{}, false, remainingErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return graph.AnalysisGenerationHeader{}, false, commitErr
		}
		committed = true
		s.analysisGenerationPresent = remaining
		return graph.AnalysisGenerationHeader{}, false, fmt.Errorf("%w: generation %d: %v", graph.ErrAnalysisGenerationCorrupt, generationID, reason)
	}
	if header.FormatVersion != formatVersion {
		return graph.AnalysisGenerationHeader{}, false, nil
	}
	if err := tx.Commit(); err != nil {
		return graph.AnalysisGenerationHeader{}, false, err
	}
	committed = true
	// A persisted build revision belongs to the previous process after reopen.
	// Return the current process revision as the publication receipt instead.
	header.GraphRevision = s.analysisMutationRevision.Load()
	return header, true, nil
}

// ensureAnalysisGenerationReadableLocked is the single gate every bounded
// analysis query passes. The view_gen predicate is the generation axis on the
// read side: an analysis built over another payload view is not merely stale
// for this handle, it describes a different corpus, so it reads as inactive
// rather than as a hit.
func (s *Store) ensureAnalysisGenerationReadableLocked(generationID int64) error {
	var one int
	err := s.db.QueryRow(`
		SELECT 1 FROM analysis_active_generation a
		JOIN analysis_generations g ON g.generation_id = a.generation_id
		WHERE a.view_gen = ? AND a.slot = 1 AND a.generation_id = ? AND g.state = ?`,
		s.viewGen, generationID, analysisGenerationReady).Scan(&one)
	if err == sql.ErrNoRows {
		return graph.ErrAnalysisGenerationInactive
	}
	return err
}

// analysisPointerPresentTx reports whether any view generation still has an
// active analysis pointer. It is the durable half of the shared
// analysisGenerationPresent latch.
func analysisPointerPresentTx(tx *sql.Tx) (bool, error) {
	var present int
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM analysis_active_generation LIMIT 1)`).Scan(&present); err != nil {
		return false, err
	}
	return present != 0, nil
}

func scanAnalysisNode(scanner interface{ Scan(...any) error }) (graph.AnalysisNodeMetric, error) {
	var node graph.AnalysisNodeMetric
	var community sql.NullString
	err := scanner.Scan(&node.RowID, &node.NodeID, &community, &node.PageRank, &node.Authority, &node.Hub)
	if community.Valid {
		node.CommunityID = community.String
	}
	return node, err
}

func (s *Store) AnalysisNodeMetrics(generationID int64, nodeIDs []string) ([]graph.AnalysisNodeMetric, error) {
	ids, err := sortedUniqueAnalysisStrings(nodeIDs, "node id")
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureAnalysisGenerationReadableLocked(generationID); err != nil {
		return nil, err
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, generationID)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.Query(`
		SELECT id, node_id, community_id, pagerank, authority, hub
		FROM analysis_nodes
		WHERE generation_id = ? AND node_id IN (`+analysisPlaceholders(len(ids))+`)
		ORDER BY node_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]graph.AnalysisNodeMetric, 0, len(ids))
	for rows.Next() {
		node, err := scanAnalysisNode(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	return result, rows.Err()
}

func (s *Store) ListAnalysisNodeMetrics(generationID int64, limit int, cursorNodeID string) ([]graph.AnalysisNodeMetric, string, error) {
	limit, err := boundedAnalysisLimit(limit)
	if err != nil {
		return nil, "", err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureAnalysisGenerationReadableLocked(generationID); err != nil {
		return nil, "", err
	}
	rows, err := s.db.Query(`
		SELECT id, node_id, community_id, pagerank, authority, hub
		FROM analysis_nodes WHERE generation_id = ? AND node_id > ?
		ORDER BY node_id LIMIT ?`, generationID, cursorNodeID, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	items := make([]graph.AnalysisNodeMetric, 0, limit+1)
	for rows.Next() {
		node, err := scanAnalysisNode(rows)
		if err != nil {
			return nil, "", err
		}
		items = append(items, node)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = items[len(items)-1].NodeID
	}
	return items, next, nil
}

func analysisMetricColumn(metric graph.AnalysisMetric) (string, error) {
	switch metric {
	case graph.AnalysisMetricPageRank:
		return "pagerank", nil
	case graph.AnalysisMetricAuthority:
		return "authority", nil
	case graph.AnalysisMetricHub:
		return "hub", nil
	default:
		return "", fmt.Errorf("analysis generation: unsupported metric %q", metric)
	}
}

func analysisMetricValue(node graph.AnalysisNodeMetric, metric graph.AnalysisMetric) float64 {
	switch metric {
	case graph.AnalysisMetricPageRank:
		return node.PageRank
	case graph.AnalysisMetricAuthority:
		return node.Authority
	default:
		return node.Hub
	}
}

func (s *Store) TopAnalysisNodeMetrics(generationID int64, metric graph.AnalysisMetric, limit int, cursor *graph.AnalysisMetricCursor) ([]graph.AnalysisNodeMetric, *graph.AnalysisMetricCursor, error) {
	column, err := analysisMetricColumn(metric)
	if err != nil {
		return nil, nil, err
	}
	limit, err = boundedAnalysisLimit(limit)
	if err != nil {
		return nil, nil, err
	}
	if cursor != nil && (!validAnalysisFloat(cursor.Score) || cursor.RowID <= 0) {
		return nil, nil, fmt.Errorf("analysis generation: invalid metric cursor")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureAnalysisGenerationReadableLocked(generationID); err != nil {
		return nil, nil, err
	}
	query := `SELECT id, node_id, community_id, pagerank, authority, hub FROM analysis_nodes WHERE generation_id = ?`
	args := []any{generationID}
	if cursor != nil {
		query += ` AND (` + column + ` < ? OR (` + column + ` = ? AND id > ?))`
		args = append(args, cursor.Score, cursor.Score, cursor.RowID)
	}
	query += ` ORDER BY ` + column + ` DESC, id ASC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := make([]graph.AnalysisNodeMetric, 0, limit+1)
	for rows.Next() {
		node, err := scanAnalysisNode(rows)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, node)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *graph.AnalysisMetricCursor
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = &graph.AnalysisMetricCursor{Score: analysisMetricValue(last, metric), RowID: last.RowID}
	}
	return items, next, nil
}

func (s *Store) ListAnalysisCommunitySummaries(generationID int64, limit int, cursorID string) ([]graph.AnalysisCommunitySummary, string, error) {
	limit, err := boundedAnalysisLimit(limit)
	if err != nil {
		return nil, "", err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureAnalysisGenerationReadableLocked(generationID); err != nil {
		return nil, "", err
	}
	rows, err := s.db.Query(`
		SELECT community_id, label, hub, parent_id, size, cohesion
		FROM analysis_communities WHERE generation_id = ? AND community_id > ?
		ORDER BY community_id LIMIT ?`, generationID, cursorID, limit+1)
	if err != nil {
		return nil, "", err
	}
	items := make([]graph.AnalysisCommunitySummary, 0, limit+1)
	for rows.Next() {
		var item graph.AnalysisCommunitySummary
		if err := rows.Scan(&item.ID, &item.Label, &item.Hub, &item.ParentID, &item.Size, &item.Cohesion); err != nil {
			rows.Close()
			return nil, "", err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, "", err
	}
	if err := rows.Close(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = items[len(items)-1].ID
	}
	if err := s.loadAnalysisCommunityFilesLocked(generationID, items); err != nil {
		return nil, "", err
	}
	return items, next, nil
}

func (s *Store) loadAnalysisCommunityFilesLocked(generationID int64, items []graph.AnalysisCommunitySummary) error {
	if len(items) == 0 {
		return nil
	}
	args := make([]any, 0, len(items)+1)
	args = append(args, generationID)
	positions := make(map[string]int, len(items))
	for i := range items {
		args = append(args, items[i].ID)
		positions[items[i].ID] = i
	}
	rows, err := s.db.Query(`
		SELECT community_id, file_path FROM analysis_community_files
		WHERE generation_id = ? AND community_id IN (`+analysisPlaceholders(len(items))+`)
		ORDER BY community_id, ordinal`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, file string
		if err := rows.Scan(&id, &file); err != nil {
			return err
		}
		items[positions[id]].Files = append(items[positions[id]].Files, file)
	}
	return rows.Err()
}

func (s *Store) AnalysisCommunityMembers(generationID int64, communityID string, limit int, cursorNodeID string) ([]graph.AnalysisNodeMetric, string, error) {
	if communityID == "" {
		return nil, "", fmt.Errorf("analysis generation: empty community id")
	}
	limit, err := boundedAnalysisLimit(limit)
	if err != nil {
		return nil, "", err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureAnalysisGenerationReadableLocked(generationID); err != nil {
		return nil, "", err
	}
	rows, err := s.db.Query(`
		SELECT id, node_id, community_id, pagerank, authority, hub
		FROM analysis_nodes
		WHERE generation_id = ? AND community_id = ? AND node_id > ?
		ORDER BY node_id LIMIT ?`, generationID, communityID, cursorNodeID, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	items := make([]graph.AnalysisNodeMetric, 0, limit+1)
	for rows.Next() {
		node, err := scanAnalysisNode(rows)
		if err != nil {
			return nil, "", err
		}
		items = append(items, node)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = items[len(items)-1].NodeID
	}
	return items, next, nil
}

func (s *Store) ListAnalysisProcessSummaries(generationID int64, limit int, cursorID string) ([]graph.AnalysisProcessSummary, string, error) {
	limit, err := boundedAnalysisLimit(limit)
	if err != nil {
		return nil, "", err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureAnalysisGenerationReadableLocked(generationID); err != nil {
		return nil, "", err
	}
	rows, err := s.db.Query(`
		SELECT process_id, name, entry_point, step_count, score, truncated
		FROM analysis_processes WHERE generation_id = ? AND process_id > ?
		ORDER BY process_id LIMIT ?`, generationID, cursorID, limit+1)
	if err != nil {
		return nil, "", err
	}
	items := make([]graph.AnalysisProcessSummary, 0, limit+1)
	for rows.Next() {
		var item graph.AnalysisProcessSummary
		var truncated int
		if err := rows.Scan(&item.ID, &item.Name, &item.EntryPoint, &item.StepCount, &item.Score, &truncated); err != nil {
			rows.Close()
			return nil, "", err
		}
		item.Truncated = truncated != 0
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, "", err
	}
	if err := rows.Close(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = items[len(items)-1].ID
	}
	if err := s.loadAnalysisProcessFilesLocked(generationID, items); err != nil {
		return nil, "", err
	}
	return items, next, nil
}

func (s *Store) loadAnalysisProcessFilesLocked(generationID int64, items []graph.AnalysisProcessSummary) error {
	if len(items) == 0 {
		return nil
	}
	args := make([]any, 0, len(items)+1)
	args = append(args, generationID)
	positions := make(map[string]int, len(items))
	for i := range items {
		args = append(args, items[i].ID)
		positions[items[i].ID] = i
	}
	rows, err := s.db.Query(`
		SELECT process_id, file_path FROM analysis_process_files
		WHERE generation_id = ? AND process_id IN (`+analysisPlaceholders(len(items))+`)
		ORDER BY process_id, ordinal`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, file string
		if err := rows.Scan(&id, &file); err != nil {
			return err
		}
		items[positions[id]].Files = append(items[positions[id]].Files, file)
	}
	return rows.Err()
}

// cursorOrdinal is exclusive. Pass -1 for the first page; the returned cursor
// is the final ordinal in the page and can be fed directly into the next call.
func (s *Store) AnalysisProcessSteps(generationID int64, processID string, limit int, cursorOrdinal int) ([]graph.AnalysisProcessStep, int, error) {
	if processID == "" || cursorOrdinal < -1 {
		return nil, cursorOrdinal, fmt.Errorf("analysis generation: invalid process step cursor")
	}
	limit, err := boundedAnalysisLimit(limit)
	if err != nil {
		return nil, cursorOrdinal, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureAnalysisGenerationReadableLocked(generationID); err != nil {
		return nil, cursorOrdinal, err
	}
	rows, err := s.db.Query(`
		SELECT s.process_id, n.node_id, s.ordinal, s.depth
		FROM analysis_process_steps s JOIN analysis_nodes n ON n.id = s.node_rowid
		WHERE s.generation_id = ? AND s.process_id = ? AND s.ordinal > ?
		ORDER BY s.ordinal LIMIT ?`, generationID, processID, cursorOrdinal, limit+1)
	if err != nil {
		return nil, cursorOrdinal, err
	}
	defer rows.Close()
	items := make([]graph.AnalysisProcessStep, 0, limit+1)
	for rows.Next() {
		var item graph.AnalysisProcessStep
		if err := rows.Scan(&item.ProcessID, &item.NodeID, &item.Ordinal, &item.Depth); err != nil {
			return nil, cursorOrdinal, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, cursorOrdinal, err
	}
	next := -1
	if len(items) > limit {
		items = items[:limit]
		next = items[len(items)-1].Ordinal
	}
	return items, next, nil
}

func (s *Store) AnalysisProcessesForNodes(generationID int64, nodeIDs []string) ([]graph.AnalysisProcessMembership, error) {
	ids, err := sortedUniqueAnalysisStrings(nodeIDs, "node id")
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureAnalysisGenerationReadableLocked(generationID); err != nil {
		return nil, err
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, generationID)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.Query(`
		SELECT DISTINCT n.node_id, s.process_id
		FROM analysis_process_steps s JOIN analysis_nodes n ON n.id = s.node_rowid
		WHERE s.generation_id = ? AND n.node_id IN (`+analysisPlaceholders(len(ids))+`)
		ORDER BY n.node_id, s.process_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]graph.AnalysisProcessMembership, 0)
	for rows.Next() {
		var item graph.AnalysisProcessMembership
		if err := rows.Scan(&item.NodeID, &item.ProcessID); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func validAnalysisConceptDirection(direction graph.AnalysisConceptDirection) bool {
	return direction == graph.AnalysisConceptForward || direction == graph.AnalysisConceptReverse
}

func (s *Store) AnalysisConcepts(generationID int64, tokens []string, direction graph.AnalysisConceptDirection) (graph.AnalysisConceptQueryResult, error) {
	if !validAnalysisConceptDirection(direction) {
		return graph.AnalysisConceptQueryResult{}, fmt.Errorf("analysis generation: unsupported concept direction %q", direction)
	}
	ordered, err := sortedUniqueAnalysisStrings(tokens, "concept token")
	if err != nil || len(ordered) == 0 {
		return graph.AnalysisConceptQueryResult{}, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureAnalysisGenerationReadableLocked(generationID); err != nil {
		return graph.AnalysisConceptQueryResult{}, err
	}
	return s.analysisConceptsLocked(generationID, ordered, direction)
}

func (s *Store) analysisConceptsLocked(generationID int64, tokens []string, direction graph.AnalysisConceptDirection) (graph.AnalysisConceptQueryResult, error) {
	result := graph.AnalysisConceptQueryResult{Concepts: make([]graph.AnalysisConcept, len(tokens))}
	positions := make(map[string]int, len(tokens))
	args := make([]any, 0, len(tokens)+1)
	args = append(args, generationID)
	for i, token := range tokens {
		result.Concepts[i].Token = token
		positions[token] = i
		args = append(args, token)
	}
	rows, err := s.db.Query(`SELECT token, in_vocabulary FROM analysis_concepts WHERE generation_id = ? AND token IN (`+analysisPlaceholders(len(tokens))+`)`, args...)
	if err != nil {
		return graph.AnalysisConceptQueryResult{}, err
	}
	for rows.Next() {
		var token string
		var vocabulary int
		if err := rows.Scan(&token, &vocabulary); err != nil {
			rows.Close()
			return graph.AnalysisConceptQueryResult{}, err
		}
		result.Concepts[positions[token]].InVocabulary = vocabulary != 0
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return graph.AnalysisConceptQueryResult{}, err
	}
	if err := rows.Close(); err != nil {
		return graph.AnalysisConceptQueryResult{}, err
	}
	column := "token"
	if direction == graph.AnalysisConceptReverse {
		column = "related_token"
	}
	relationRows, err := s.db.Query(`
		SELECT token, related_token, rank FROM analysis_concept_relations
		WHERE generation_id = ? AND `+column+` IN (`+analysisPlaceholders(len(tokens))+`)
		ORDER BY token, rank, related_token`, args...)
	if err != nil {
		return graph.AnalysisConceptQueryResult{}, err
	}
	defer relationRows.Close()
	for relationRows.Next() {
		var relation graph.AnalysisConceptRelation
		if err := relationRows.Scan(&relation.Token, &relation.RelatedToken, &relation.Rank); err != nil {
			return graph.AnalysisConceptQueryResult{}, err
		}
		result.Relations = append(result.Relations, relation)
	}
	return result, relationRows.Err()
}

func (s *Store) ListAnalysisConcepts(generationID int64, limit int, cursorToken string) (graph.AnalysisConceptQueryResult, string, error) {
	limit, err := boundedAnalysisLimit(limit)
	if err != nil {
		return graph.AnalysisConceptQueryResult{}, "", err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureAnalysisGenerationReadableLocked(generationID); err != nil {
		return graph.AnalysisConceptQueryResult{}, "", err
	}
	rows, err := s.db.Query(`
		SELECT token FROM analysis_concepts WHERE generation_id = ? AND token > ?
		ORDER BY token LIMIT ?`, generationID, cursorToken, limit+1)
	if err != nil {
		return graph.AnalysisConceptQueryResult{}, "", err
	}
	tokens := make([]string, 0, limit+1)
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			rows.Close()
			return graph.AnalysisConceptQueryResult{}, "", err
		}
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return graph.AnalysisConceptQueryResult{}, "", err
	}
	if err := rows.Close(); err != nil {
		return graph.AnalysisConceptQueryResult{}, "", err
	}
	next := ""
	if len(tokens) > limit {
		tokens = tokens[:limit]
		next = tokens[len(tokens)-1]
	}
	if len(tokens) == 0 {
		return graph.AnalysisConceptQueryResult{}, next, nil
	}
	result, err := s.analysisConceptsLocked(generationID, tokens, graph.AnalysisConceptForward)
	return result, next, err
}

func (s *Store) LoadAnalysisBlob(generationID int64, component graph.AnalysisBlobComponent) ([]byte, bool, error) {
	if !validAnalysisBlobComponent(component) {
		return nil, false, fmt.Errorf("analysis generation: unsupported blob component %q", component)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureAnalysisGenerationReadableLocked(generationID); err != nil {
		return nil, false, err
	}
	var payload []byte
	err := s.db.QueryRow(`SELECT payload FROM analysis_blobs WHERE generation_id = ? AND component = ?`, generationID, string(component)).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return payload, true, nil
}
