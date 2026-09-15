package store_sqlite

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

const (
	analysisGenerationBuilding = 0
	analysisGenerationReady    = 1
	analysisGenerationStale    = 2

	analysisGenerationChunkLimit = 5000
	maxSQLiteRevision            = uint64(1<<63 - 1)
)

var (
	_ graph.AnalysisGenerationStore = (*Store)(nil)
	_ graph.AnalysisQueryStore      = (*Store)(nil)
)

var requiredAnalysisComponents = [...]graph.AnalysisComponent{
	graph.AnalysisComponentNodes,
	graph.AnalysisComponentCommunities,
	graph.AnalysisComponentProcesses,
	graph.AnalysisComponentConcepts,
	graph.AnalysisComponentAdjacency,
	graph.AnalysisComponentLeiden,
}

func validAnalysisComponent(component graph.AnalysisComponent) bool {
	for _, required := range requiredAnalysisComponents {
		if component == required {
			return true
		}
	}
	return false
}

func validAnalysisBlobComponent(component graph.AnalysisBlobComponent) bool {
	return component == graph.AnalysisBlobAdjacency || component == graph.AnalysisBlobLeiden
}

func analysisBool(value bool) int {
	if value {
		return 1
	}
	return 0
}

func validAnalysisFloat(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validateAnalysisHeader(header graph.AnalysisGenerationHeader) error {
	if header.GenerationID != 0 {
		return fmt.Errorf("analysis generation: begin header must not set generation id")
	}
	if header.NodeCount < 0 || header.CommunityCount < 0 || header.ProcessCount < 0 || header.ConceptCount < 0 {
		return fmt.Errorf("analysis generation: negative manifest count")
	}
	if !validAnalysisFloat(header.PageRankMax) || !validAnalysisFloat(header.AuthorityMax) ||
		!validAnalysisFloat(header.HubMax) || !validAnalysisFloat(header.Modularity) {
		return fmt.Errorf("analysis generation: non-finite manifest metric")
	}
	return nil
}

// beginAnalysisWrite opens the analysis cache's write transaction on the BASE
// handle while its callers keep stamping their own s.viewGen into the rows.
//
// The analysis cache is a derived plane, not payload: analysisGenerationSchemaSQL
// copies node IDs into generation-local rows precisely so it references no live
// node, and checkoutCatalogSchemaSQL says in as many words that "the same
// reasoning keeps analysis generations detached". Payload writes through a
// published generation's handle are refused by its seal
// (refuseSealedPayloadWrite / resolvePayloadSeal: anything past building is
// sealed), and an analysis computed over a published view is exactly the case
// that must still be cacheable. atBase exists for the catalog for this reason
// (store_generation.go) and the analysis cache is the same kind of plane.
//
// atBase drops only viewGen, the seal, the resolve lane and the managed-write
// restriction; writeMu, the pools and the pinned bulk connection all live on
// the shared core, so the transaction is the same one a base write would take.
func (s *Store) beginAnalysisWrite() (*sql.Tx, error) {
	return s.atBase().beginWrite()
}

func validateAnalysisChunkSize(kind string, size int) error {
	if size > analysisGenerationChunkLimit {
		return fmt.Errorf("analysis generation: %s chunk has %d rows, limit is %d", kind, size, analysisGenerationChunkLimit)
	}
	return nil
}

func (s *Store) BeginAnalysisGeneration(expectedRevision uint64, header graph.AnalysisGenerationHeader) (int64, bool, error) {
	if err := validateAnalysisHeader(header); err != nil {
		return 0, false, err
	}
	if expectedRevision > maxSQLiteRevision {
		return 0, false, fmt.Errorf("analysis generation: revision %d exceeds sqlite integer range", expectedRevision)
	}
	if header.GraphRevision != 0 && header.GraphRevision != expectedRevision {
		return 0, false, fmt.Errorf("analysis generation: header revision %d does not match expected revision %d", header.GraphRevision, expectedRevision)
	}
	createdAt := header.CreatedAtUnix
	if createdAt == 0 {
		createdAt = time.Now().Unix()
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.analysisMutationRevision.Load() != expectedRevision {
		return 0, false, nil
	}
	// A new analysis over a view the retirement sweep is already walking would
	// never be collected: the sweep snapshots the ids it deletes, and the
	// payload generation is gone by the time it finishes.
	if err := s.refuseRetiredAnalysisWrite(); err != nil {
		return 0, false, err
	}
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
	// view_gen stamps the analysis with the payload view its inputs were read
	// from. The projection this generation is computed over is already scoped
	// to s.viewGen (analysis_projection.go binds it on every cursor), so the
	// cache row has to carry the same axis or two views' analyses collide on
	// the one active pointer.
	result, err := tx.Exec(`
		INSERT INTO analysis_generations(
			format_version, build_revision, created_at_unix, state,
			node_count, community_count, process_count, concept_count,
			pagerank_max, authority_max, hub_max, modularity,
			processes_truncated, processes_truncation_reason, view_gen
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		header.FormatVersion, int64(expectedRevision), createdAt, analysisGenerationBuilding,
		header.NodeCount, header.CommunityCount, header.ProcessCount, header.ConceptCount,
		header.PageRankMax, header.AuthorityMax, header.HubMax, header.Modularity,
		analysisBool(header.ProcessesTruncated), header.ProcessesTruncationReason, s.viewGen)
	if err != nil {
		return 0, false, err
	}
	generationID, err := result.LastInsertId()
	if err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	committed = true
	// Include invisible building rows in the mutation latch: the next graph
	// write will mark them stale once, then return to the O(1) no-cache path.
	s.analysisGenerationPresent = true
	return generationID, true, nil
}

// beginAnalysisGenerationWrite starts a short transaction for one bounded
// append/seal operation. The caller holds writeMu. A stale revision or a
// generation no longer in building state is a clean rejected write.
func (s *Store) beginAnalysisGenerationWrite(expectedRevision uint64, generationID int64) (*sql.Tx, bool, error) {
	if generationID <= 0 {
		return nil, false, fmt.Errorf("analysis generation: invalid generation id %d", generationID)
	}
	if s.analysisMutationRevision.Load() != expectedRevision {
		return nil, false, nil
	}
	// Same gate as BeginAnalysisGeneration, and it has to be here too: an
	// analysis begun before the retirement started can still be appended to,
	// sealed or activated while the sweep runs.
	if err := s.refuseRetiredAnalysisWrite(); err != nil {
		return nil, false, err
	}
	tx, err := s.beginAnalysisWrite()
	if err != nil {
		return nil, false, err
	}
	// The view_gen predicate is what keeps one handle from appending to,
	// sealing or activating an analysis generation another payload view is
	// building. Generation ids are globally unique, so a mismatch reads as
	// "does not exist" for this handle, which is exactly what it is.
	var state int
	if err := tx.QueryRow(
		`SELECT state FROM analysis_generations WHERE generation_id = ? AND view_gen = ?`,
		generationID, s.viewGen,
	).Scan(&state); err != nil {
		_ = tx.Rollback()
		if err == sql.ErrNoRows {
			return nil, false, fmt.Errorf("analysis generation: generation %d does not exist", generationID)
		}
		return nil, false, err
	}
	if state != analysisGenerationBuilding {
		_ = tx.Rollback()
		return nil, false, nil
	}
	return tx, true, nil
}

func (s *Store) beginAnalysisComponentWrite(expectedRevision uint64, generationID int64, component graph.AnalysisComponent) (*sql.Tx, bool, error) {
	tx, accepted, err := s.beginAnalysisGenerationWrite(expectedRevision, generationID)
	if err != nil || !accepted {
		return tx, accepted, err
	}
	var sealed int
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM analysis_generation_components WHERE generation_id = ? AND component = ?)`, generationID, string(component)).Scan(&sealed); err != nil {
		_ = tx.Rollback()
		return nil, false, err
	}
	if sealed != 0 {
		_ = tx.Rollback()
		return nil, false, fmt.Errorf("analysis generation: component %s is already sealed", component)
	}
	return tx, true, nil
}

func (s *Store) AppendAnalysisCommunities(expectedRevision uint64, generationID int64, communities []graph.AnalysisCommunitySummary) (bool, error) {
	if err := validateAnalysisChunkSize("community", len(communities)); err != nil {
		return false, err
	}
	seen := make(map[string]struct{}, len(communities))
	for _, community := range communities {
		if community.ID == "" {
			return false, fmt.Errorf("analysis generation: empty community id")
		}
		if community.Size < 0 || !validAnalysisFloat(community.Cohesion) {
			return false, fmt.Errorf("analysis generation: invalid community %q metrics", community.ID)
		}
		if _, duplicate := seen[community.ID]; duplicate {
			return false, fmt.Errorf("analysis generation: duplicate community %q in chunk", community.ID)
		}
		seen[community.ID] = struct{}{}
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, accepted, err := s.beginAnalysisComponentWrite(expectedRevision, generationID, graph.AnalysisComponentCommunities)
	if err != nil || !accepted {
		return accepted, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	communityStmt, err := tx.Prepare(`
		INSERT INTO analysis_communities(generation_id, community_id, label, hub, parent_id, size, cohesion)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(generation_id, community_id) DO UPDATE SET
			label=excluded.label, hub=excluded.hub, parent_id=excluded.parent_id,
			size=excluded.size, cohesion=excluded.cohesion`)
	if err != nil {
		return false, err
	}
	defer communityStmt.Close()
	fileStmt, err := tx.Prepare(`INSERT INTO analysis_community_files(generation_id, community_id, ordinal, file_path) VALUES(?,?,?,?)`)
	if err != nil {
		return false, err
	}
	defer fileStmt.Close()
	for _, community := range communities {
		if _, err := communityStmt.Exec(generationID, community.ID, community.Label, community.Hub, community.ParentID, community.Size, community.Cohesion); err != nil {
			return false, err
		}
		if _, err := tx.Exec(`DELETE FROM analysis_community_files WHERE generation_id = ? AND community_id = ?`, generationID, community.ID); err != nil {
			return false, err
		}
		filesSeen := make(map[string]struct{}, len(community.Files))
		for ordinal, file := range community.Files {
			if file == "" {
				return false, fmt.Errorf("analysis generation: community %q has empty file", community.ID)
			}
			if _, duplicate := filesSeen[file]; duplicate {
				return false, fmt.Errorf("analysis generation: community %q repeats file %q", community.ID, file)
			}
			filesSeen[file] = struct{}{}
			if _, err := fileStmt.Exec(generationID, community.ID, ordinal, file); err != nil {
				return false, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	committed = true
	return true, nil
}

func (s *Store) AppendAnalysisNodes(expectedRevision uint64, generationID int64, nodes []graph.AnalysisNodeMetric) (bool, error) {
	if err := validateAnalysisChunkSize("node", len(nodes)); err != nil {
		return false, err
	}
	seen := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		if node.NodeID == "" {
			return false, fmt.Errorf("analysis generation: empty node id")
		}
		if node.RowID != 0 {
			return false, fmt.Errorf("analysis generation: writer must not set node row id for %q", node.NodeID)
		}
		if !validAnalysisFloat(node.PageRank) || !validAnalysisFloat(node.Authority) || !validAnalysisFloat(node.Hub) {
			return false, fmt.Errorf("analysis generation: non-finite node metric for %q", node.NodeID)
		}
		if _, duplicate := seen[node.NodeID]; duplicate {
			return false, fmt.Errorf("analysis generation: duplicate node %q in chunk", node.NodeID)
		}
		seen[node.NodeID] = struct{}{}
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, accepted, err := s.beginAnalysisComponentWrite(expectedRevision, generationID, graph.AnalysisComponentNodes)
	if err != nil || !accepted {
		return accepted, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	stmt, err := tx.Prepare(`
		INSERT INTO analysis_nodes(generation_id, node_id, community_id, pagerank, authority, hub)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(generation_id, node_id) DO UPDATE SET
			community_id=excluded.community_id, pagerank=excluded.pagerank,
			authority=excluded.authority, hub=excluded.hub`)
	if err != nil {
		return false, err
	}
	defer stmt.Close()
	for _, node := range nodes {
		var community any
		if node.CommunityID != "" {
			community = node.CommunityID
		}
		if _, err := stmt.Exec(generationID, node.NodeID, community, node.PageRank, node.Authority, node.Hub); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	committed = true
	return true, nil
}

func (s *Store) AppendAnalysisProcesses(expectedRevision uint64, generationID int64, processes []graph.AnalysisProcessSummary, steps []graph.AnalysisProcessStep) (bool, error) {
	if err := validateAnalysisChunkSize("process", len(processes)); err != nil {
		return false, err
	}
	if err := validateAnalysisChunkSize("process step", len(steps)); err != nil {
		return false, err
	}
	processSeen := make(map[string]struct{}, len(processes))
	for _, process := range processes {
		if process.ID == "" || process.StepCount < 0 || !validAnalysisFloat(process.Score) {
			return false, fmt.Errorf("analysis generation: invalid process %q", process.ID)
		}
		if _, duplicate := processSeen[process.ID]; duplicate {
			return false, fmt.Errorf("analysis generation: duplicate process %q in chunk", process.ID)
		}
		processSeen[process.ID] = struct{}{}
	}
	stepSeen := make(map[string]struct{}, len(steps))
	for _, step := range steps {
		if step.ProcessID == "" || step.NodeID == "" || step.Ordinal < 0 || step.Depth < 0 {
			return false, fmt.Errorf("analysis generation: invalid process step %+v", step)
		}
		key := fmt.Sprintf("%s\x00%d", step.ProcessID, step.Ordinal)
		if _, duplicate := stepSeen[key]; duplicate {
			return false, fmt.Errorf("analysis generation: duplicate step %s ordinal %d", step.ProcessID, step.Ordinal)
		}
		stepSeen[key] = struct{}{}
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, accepted, err := s.beginAnalysisComponentWrite(expectedRevision, generationID, graph.AnalysisComponentProcesses)
	if err != nil || !accepted {
		return accepted, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	processStmt, err := tx.Prepare(`
		INSERT INTO analysis_processes(generation_id, process_id, name, entry_point, step_count, score, truncated)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(generation_id, process_id) DO UPDATE SET
			name=excluded.name, entry_point=excluded.entry_point, step_count=excluded.step_count,
			score=excluded.score, truncated=excluded.truncated`)
	if err != nil {
		return false, err
	}
	defer processStmt.Close()
	fileStmt, err := tx.Prepare(`INSERT INTO analysis_process_files(generation_id, process_id, ordinal, file_path) VALUES(?,?,?,?)`)
	if err != nil {
		return false, err
	}
	defer fileStmt.Close()
	for _, process := range processes {
		if _, err := processStmt.Exec(generationID, process.ID, process.Name, process.EntryPoint, process.StepCount, process.Score, analysisBool(process.Truncated)); err != nil {
			return false, err
		}
		if _, err := tx.Exec(`DELETE FROM analysis_process_files WHERE generation_id = ? AND process_id = ?`, generationID, process.ID); err != nil {
			return false, err
		}
		filesSeen := make(map[string]struct{}, len(process.Files))
		for ordinal, file := range process.Files {
			if file == "" {
				return false, fmt.Errorf("analysis generation: process %q has empty file", process.ID)
			}
			if _, duplicate := filesSeen[file]; duplicate {
				return false, fmt.Errorf("analysis generation: process %q repeats file %q", process.ID, file)
			}
			filesSeen[file] = struct{}{}
			if _, err := fileStmt.Exec(generationID, process.ID, ordinal, file); err != nil {
				return false, err
			}
		}
	}
	stepStmt, err := tx.Prepare(`
		INSERT INTO analysis_process_steps(generation_id, process_id, ordinal, node_rowid, depth)
		SELECT ?, ?, ?, id, ? FROM analysis_nodes WHERE generation_id = ? AND node_id = ?
		ON CONFLICT(generation_id, process_id, ordinal) DO UPDATE SET
			node_rowid=excluded.node_rowid, depth=excluded.depth`)
	if err != nil {
		return false, err
	}
	defer stepStmt.Close()
	for _, step := range steps {
		result, err := stepStmt.Exec(generationID, step.ProcessID, step.Ordinal, step.Depth, generationID, step.NodeID)
		if err != nil {
			return false, err
		}
		if rows, err := result.RowsAffected(); err != nil || rows != 1 {
			if err != nil {
				return false, err
			}
			return false, fmt.Errorf("analysis generation: process step %s[%d] references unknown node %q", step.ProcessID, step.Ordinal, step.NodeID)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	committed = true
	return true, nil
}

func (s *Store) AppendAnalysisConcepts(expectedRevision uint64, generationID int64, concepts []graph.AnalysisConcept, relations []graph.AnalysisConceptRelation) (bool, error) {
	if err := validateAnalysisChunkSize("concept", len(concepts)); err != nil {
		return false, err
	}
	if err := validateAnalysisChunkSize("concept relation", len(relations)); err != nil {
		return false, err
	}
	conceptSeen := make(map[string]struct{}, len(concepts))
	for _, concept := range concepts {
		if concept.Token == "" {
			return false, fmt.Errorf("analysis generation: empty concept token")
		}
		if _, duplicate := conceptSeen[concept.Token]; duplicate {
			return false, fmt.Errorf("analysis generation: duplicate concept %q in chunk", concept.Token)
		}
		conceptSeen[concept.Token] = struct{}{}
	}
	relationSeen := make(map[string]struct{}, len(relations))
	for _, relation := range relations {
		if relation.Token == "" || relation.RelatedToken == "" || relation.Rank < 0 {
			return false, fmt.Errorf("analysis generation: invalid concept relation %+v", relation)
		}
		key := fmt.Sprintf("%s\x00%d\x00%s", relation.Token, relation.Rank, relation.RelatedToken)
		if _, duplicate := relationSeen[key]; duplicate {
			return false, fmt.Errorf("analysis generation: duplicate concept relation %q", key)
		}
		relationSeen[key] = struct{}{}
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, accepted, err := s.beginAnalysisComponentWrite(expectedRevision, generationID, graph.AnalysisComponentConcepts)
	if err != nil || !accepted {
		return accepted, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	conceptStmt, err := tx.Prepare(`
		INSERT INTO analysis_concepts(generation_id, token, in_vocabulary) VALUES(?,?,?)
		ON CONFLICT(generation_id, token) DO UPDATE SET in_vocabulary=excluded.in_vocabulary`)
	if err != nil {
		return false, err
	}
	defer conceptStmt.Close()
	for _, concept := range concepts {
		if _, err := conceptStmt.Exec(generationID, concept.Token, analysisBool(concept.InVocabulary)); err != nil {
			return false, err
		}
	}
	relationStmt, err := tx.Prepare(`
		INSERT INTO analysis_concept_relations(generation_id, token, related_token, rank) VALUES(?,?,?,?)
		ON CONFLICT(generation_id, token, rank, related_token) DO NOTHING`)
	if err != nil {
		return false, err
	}
	defer relationStmt.Close()
	for _, relation := range relations {
		if _, err := relationStmt.Exec(generationID, relation.Token, relation.RelatedToken, relation.Rank); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	committed = true
	return true, nil
}

func (s *Store) PutAnalysisBlob(expectedRevision uint64, generationID int64, blob graph.AnalysisBlob) (bool, error) {
	if !validAnalysisBlobComponent(blob.Component) {
		return false, fmt.Errorf("analysis generation: unsupported blob component %q", blob.Component)
	}
	if len(blob.Payload) == 0 {
		return false, fmt.Errorf("analysis generation: empty %s blob", blob.Component)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, accepted, err := s.beginAnalysisComponentWrite(expectedRevision, generationID, graph.AnalysisComponent(blob.Component))
	if err != nil || !accepted {
		return accepted, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.Exec(`
		INSERT INTO analysis_blobs(generation_id, component, payload) VALUES(?,?,?)
		ON CONFLICT(generation_id, component) DO UPDATE SET payload=excluded.payload`,
		generationID, string(blob.Component), blob.Payload); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	committed = true
	return true, nil
}

func (s *Store) SealAnalysisComponent(expectedRevision uint64, generationID int64, component graph.AnalysisComponent, expectedRows int) (bool, error) {
	if !validAnalysisComponent(component) {
		return false, fmt.Errorf("analysis generation: unsupported component %q", component)
	}
	if expectedRows < 0 {
		return false, fmt.Errorf("analysis generation: negative expected row count for %s", component)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, accepted, err := s.beginAnalysisGenerationWrite(expectedRevision, generationID)
	if err != nil || !accepted {
		return accepted, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	actual, err := analysisComponentRowsTx(tx, generationID, component)
	if err != nil {
		return false, err
	}
	if actual != expectedRows {
		return false, fmt.Errorf("analysis generation: %s has %d rows, expected %d", component, actual, expectedRows)
	}
	headerExpected, err := analysisHeaderComponentRowsTx(tx, generationID, component)
	if err != nil {
		return false, err
	}
	if headerExpected >= 0 && actual != headerExpected {
		return false, fmt.Errorf("analysis generation: %s has %d rows, manifest requires %d", component, actual, headerExpected)
	}
	if component == graph.AnalysisComponentProcesses {
		var mismatches int
		if err := tx.QueryRow(`
			SELECT COUNT(*) FROM analysis_processes p
			WHERE p.generation_id = ? AND p.step_count != (
				SELECT COUNT(*) FROM analysis_process_steps s
				WHERE s.generation_id = p.generation_id AND s.process_id = p.process_id
			)`, generationID).Scan(&mismatches); err != nil {
			return false, err
		}
		if mismatches != 0 {
			return false, fmt.Errorf("analysis generation: %d process step counts are incomplete", mismatches)
		}
	}
	if _, err := tx.Exec(`
		INSERT INTO analysis_generation_components(generation_id, component, row_count, sealed_at_unix)
		VALUES(?,?,?,?)
		ON CONFLICT(generation_id, component) DO UPDATE SET
			row_count=excluded.row_count, sealed_at_unix=excluded.sealed_at_unix`,
		generationID, string(component), actual, time.Now().Unix()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	committed = true
	return true, nil
}

func analysisComponentRowsTx(tx *sql.Tx, generationID int64, component graph.AnalysisComponent) (int, error) {
	var query string
	var args []any
	switch component {
	case graph.AnalysisComponentNodes:
		query = `SELECT COUNT(*) FROM analysis_nodes WHERE generation_id = ?`
	case graph.AnalysisComponentCommunities:
		query = `SELECT COUNT(*) FROM analysis_communities WHERE generation_id = ?`
	case graph.AnalysisComponentProcesses:
		query = `SELECT COUNT(*) FROM analysis_processes WHERE generation_id = ?`
	case graph.AnalysisComponentConcepts:
		query = `SELECT COUNT(*) FROM analysis_concepts WHERE generation_id = ?`
	case graph.AnalysisComponentAdjacency, graph.AnalysisComponentLeiden:
		query = `SELECT COUNT(*) FROM analysis_blobs WHERE generation_id = ? AND component = ? AND length(payload) > 0`
		args = append(args, string(component))
	default:
		return 0, fmt.Errorf("analysis generation: unsupported component %q", component)
	}
	args = append([]any{generationID}, args...)
	var count int
	if err := tx.QueryRow(query, args...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func analysisHeaderComponentRowsTx(tx *sql.Tx, generationID int64, component graph.AnalysisComponent) (int, error) {
	column := ""
	switch component {
	case graph.AnalysisComponentNodes:
		column = "node_count"
	case graph.AnalysisComponentCommunities:
		column = "community_count"
	case graph.AnalysisComponentProcesses:
		column = "process_count"
	case graph.AnalysisComponentConcepts:
		column = "concept_count"
	case graph.AnalysisComponentAdjacency, graph.AnalysisComponentLeiden:
		return 1, nil
	default:
		return -1, fmt.Errorf("analysis generation: unsupported component %q", component)
	}
	var count int
	if err := tx.QueryRow(`SELECT `+column+` FROM analysis_generations WHERE generation_id = ?`, generationID).Scan(&count); err != nil {
		return -1, err
	}
	return count, nil
}

func loadAnalysisGenerationHeaderTx(tx *sql.Tx, generationID int64) (graph.AnalysisGenerationHeader, int, error) {
	var header graph.AnalysisGenerationHeader
	var buildRevision int64
	var state int
	var truncated int
	err := tx.QueryRow(`
		SELECT generation_id, format_version, build_revision, created_at_unix, state,
			node_count, community_count, process_count, concept_count,
			pagerank_max, authority_max, hub_max, modularity,
			processes_truncated, processes_truncation_reason
		FROM analysis_generations WHERE generation_id = ?`, generationID).Scan(
		&header.GenerationID, &header.FormatVersion, &buildRevision, &header.CreatedAtUnix, &state,
		&header.NodeCount, &header.CommunityCount, &header.ProcessCount, &header.ConceptCount,
		&header.PageRankMax, &header.AuthorityMax, &header.HubMax, &header.Modularity,
		&truncated, &header.ProcessesTruncationReason)
	if err != nil {
		return graph.AnalysisGenerationHeader{}, 0, err
	}
	header.GraphRevision = uint64(buildRevision)
	header.ProcessesTruncated = truncated != 0
	return header, state, nil
}

// validateAnalysisGenerationTx re-counts every sealed primary component and
// verifies subordinate process/node links. Activation therefore cannot expose
// a partial generation even if a writer crashed between chunks.
func validateAnalysisGenerationTx(tx *sql.Tx, generationID int64) (graph.AnalysisGenerationHeader, error) {
	header, _, err := loadAnalysisGenerationHeaderTx(tx, generationID)
	if err != nil {
		return graph.AnalysisGenerationHeader{}, err
	}
	sealed := make(map[graph.AnalysisComponent]int, len(requiredAnalysisComponents))
	rows, err := tx.Query(`SELECT component, row_count FROM analysis_generation_components WHERE generation_id = ?`, generationID)
	if err != nil {
		return graph.AnalysisGenerationHeader{}, err
	}
	for rows.Next() {
		var component graph.AnalysisComponent
		var count int
		if err := rows.Scan(&component, &count); err != nil {
			rows.Close()
			return graph.AnalysisGenerationHeader{}, err
		}
		sealed[component] = count
	}
	// A truncated read here would drop components from `sealed`, and the
	// loop below reads a missing component as a seal defect rather than as
	// the read failure it is.
	if err := rows.Err(); err != nil {
		rows.Close()
		return graph.AnalysisGenerationHeader{}, err
	}
	if err := rows.Close(); err != nil {
		return graph.AnalysisGenerationHeader{}, err
	}
	for _, component := range requiredAnalysisComponents {
		sealedRows, ok := sealed[component]
		if !ok {
			return graph.AnalysisGenerationHeader{}, fmt.Errorf("missing sealed component %s", component)
		}
		actual, err := analysisComponentRowsTx(tx, generationID, component)
		if err != nil {
			return graph.AnalysisGenerationHeader{}, err
		}
		headerRows, err := analysisHeaderComponentRowsTx(tx, generationID, component)
		if err != nil {
			return graph.AnalysisGenerationHeader{}, err
		}
		if sealedRows != actual || actual != headerRows {
			return graph.AnalysisGenerationHeader{}, fmt.Errorf("component %s count sealed=%d actual=%d manifest=%d", component, sealedRows, actual, headerRows)
		}
	}
	var brokenSteps int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM analysis_process_steps s
		LEFT JOIN analysis_nodes n ON n.id = s.node_rowid AND n.generation_id = s.generation_id
		WHERE s.generation_id = ? AND n.id IS NULL`, generationID).Scan(&brokenSteps); err != nil {
		return graph.AnalysisGenerationHeader{}, err
	}
	if brokenSteps != 0 {
		return graph.AnalysisGenerationHeader{}, fmt.Errorf("%d process steps reference a node outside the generation", brokenSteps)
	}
	var mismatchedProcesses int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM analysis_processes p
		WHERE p.generation_id = ? AND p.step_count != (
			SELECT COUNT(*) FROM analysis_process_steps s
			WHERE s.generation_id = p.generation_id AND s.process_id = p.process_id
		)`, generationID).Scan(&mismatchedProcesses); err != nil {
		return graph.AnalysisGenerationHeader{}, err
	}
	if mismatchedProcesses != 0 {
		return graph.AnalysisGenerationHeader{}, fmt.Errorf("%d process step counts differ from their manifests", mismatchedProcesses)
	}
	return header, nil
}

func (s *Store) ActivateAnalysisGeneration(expectedRevision uint64, generationID int64) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, accepted, err := s.beginAnalysisGenerationWrite(expectedRevision, generationID)
	if err != nil || !accepted {
		return accepted, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := validateAnalysisGenerationTx(tx, generationID); err != nil {
		return false, fmt.Errorf("analysis generation %d is incomplete: %w", generationID, err)
	}
	if _, err := tx.Exec(`UPDATE analysis_generations SET state = ? WHERE generation_id = ? AND state = ?`, analysisGenerationReady, generationID, analysisGenerationBuilding); err != nil {
		return false, err
	}
	// One active analysis per payload view generation: the conflict target is
	// the pointer's (view_gen, slot) key, so activating this view's analysis
	// never displaces another view's.
	if _, err := tx.Exec(`
		INSERT INTO analysis_active_generation(view_gen, slot, generation_id) VALUES(?, 1, ?)
		ON CONFLICT(view_gen, slot) DO UPDATE SET generation_id=excluded.generation_id`,
		s.viewGen, generationID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	committed = true
	s.analysisGenerationPresent = true
	return true, nil
}

func (s *Store) AbortAnalysisGeneration(generationID int64) error {
	if generationID <= 0 {
		return fmt.Errorf("analysis generation: invalid generation id %d", generationID)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.writerDB.Exec(`UPDATE analysis_generations SET state = ? WHERE generation_id = ? AND view_gen = ? AND state = ?`, analysisGenerationStale, generationID, s.viewGen, analysisGenerationBuilding)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		var state int
		if err := s.writerDB.QueryRow(`SELECT state FROM analysis_generations WHERE generation_id = ? AND view_gen = ?`, generationID, s.viewGen).Scan(&state); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("analysis generation: generation %d does not exist", generationID)
			}
			return err
		}
		if state == analysisGenerationReady {
			return fmt.Errorf("analysis generation: cannot abort ready generation %d", generationID)
		}
	}
	return nil
}

func sortedUniqueAnalysisStrings(values []string, kind string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) > analysisGenerationChunkLimit {
		return nil, fmt.Errorf("analysis generation: %s batch has %d values, limit is %d", kind, len(values), analysisGenerationChunkLimit)
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
	for i, value := range out {
		if value == "" {
			return nil, fmt.Errorf("analysis generation: empty %s", kind)
		}
		if i > 0 && out[i-1] == value {
			return nil, fmt.Errorf("analysis generation: duplicate %s %q", kind, value)
		}
	}
	return out, nil
}
