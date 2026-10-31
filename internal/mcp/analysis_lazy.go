package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

const (
	analysisGenerationQueryPage = 512
	analysisGenerationQueryMax  = 1000
)

var errAnalysisGenerationUnavailable = errors.New("active analysis generation is unavailable or stale")

// activeAnalysisQuery returns an immutable generation receipt and its bounded
// query backend. The generation header is useful only while the published
// graph token remains current; a graph mutation invalidates both the SQLite
// active pointer and this in-memory receipt.
func (s *Server) activeAnalysisQuery() (graph.AnalysisQueryStore, graph.AnalysisGenerationHeader, bool) {
	s.analysisMu.RLock()
	defer s.analysisMu.RUnlock()
	if !s.analysisGenerationReady || s.analysisGeneration.GenerationID <= 0 ||
		s.analysisGeneration.FormatVersion != analysisGenerationFormatVersion ||
		s.analysisGeneration.GraphRevision != s.communitiesToken.analysisRevision ||
		!s.analysisSnapshotCurrentLocked() {
		return nil, graph.AnalysisGenerationHeader{}, false
	}
	_, query := s.analysisGenerationBackends()
	if query == nil {
		return nil, graph.AnalysisGenerationHeader{}, false
	}
	return query, s.analysisGeneration, true
}

func boundedAnalysisLimit(limit int) int {
	if limit <= 0 {
		return analysisGenerationQueryPage
	}
	if limit > analysisGenerationQueryMax {
		return analysisGenerationQueryMax
	}
	return limit
}

func boundedAnalysisIDs(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, min(len(ids), analysisGenerationQueryMax))
	out := make([]string, 0, min(len(ids), analysisGenerationQueryMax))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		if len(out) == analysisGenerationQueryMax {
			return nil, fmt.Errorf("analysis query exceeds %d unique IDs", analysisGenerationQueryMax)
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

func (s *Server) analysisNodeMetrics(nodeIDs []string) ([]graph.AnalysisNodeMetric, error) {
	ids, err := boundedAnalysisIDs(nodeIDs)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	query, header, ok := s.activeAnalysisQuery()
	if !ok {
		return nil, errAnalysisGenerationUnavailable
	}
	return query.AnalysisNodeMetrics(header.GenerationID, ids)
}

func (s *Server) topAnalysisNodeMetrics(metric graph.AnalysisMetric, limit int, cursor *graph.AnalysisMetricCursor) ([]graph.AnalysisNodeMetric, *graph.AnalysisMetricCursor, error) {
	query, header, ok := s.activeAnalysisQuery()
	if !ok {
		return nil, nil, errAnalysisGenerationUnavailable
	}
	return query.TopAnalysisNodeMetrics(header.GenerationID, metric, boundedAnalysisLimit(limit), cursor)
}

func (s *Server) analysisCommunitySummaries(limit int, cursorID string) ([]graph.AnalysisCommunitySummary, string, error) {
	query, header, ok := s.activeAnalysisQuery()
	if !ok {
		return nil, "", errAnalysisGenerationUnavailable
	}
	return query.ListAnalysisCommunitySummaries(header.GenerationID, boundedAnalysisLimit(limit), cursorID)
}

func (s *Server) analysisCommunityMembers(communityID string, limit int, cursorNodeID string) ([]graph.AnalysisNodeMetric, string, error) {
	if communityID == "" {
		return nil, "", nil
	}
	query, header, ok := s.activeAnalysisQuery()
	if !ok {
		return nil, "", errAnalysisGenerationUnavailable
	}
	return query.AnalysisCommunityMembers(header.GenerationID, communityID, boundedAnalysisLimit(limit), cursorNodeID)
}

func (s *Server) analysisProcessSummaries(limit int, cursorID string) ([]graph.AnalysisProcessSummary, string, error) {
	query, header, ok := s.activeAnalysisQuery()
	if !ok {
		return nil, "", errAnalysisGenerationUnavailable
	}
	return query.ListAnalysisProcessSummaries(header.GenerationID, boundedAnalysisLimit(limit), cursorID)
}

func (s *Server) analysisProcessSteps(processID string, limit, cursorOrdinal int) ([]graph.AnalysisProcessStep, int, error) {
	if processID == "" {
		return nil, cursorOrdinal, nil
	}
	query, header, ok := s.activeAnalysisQuery()
	if !ok {
		return nil, cursorOrdinal, errAnalysisGenerationUnavailable
	}
	return query.AnalysisProcessSteps(header.GenerationID, processID, boundedAnalysisLimit(limit), cursorOrdinal)
}

func (s *Server) analysisProcessesForNodes(nodeIDs []string) ([]graph.AnalysisProcessMembership, error) {
	ids, err := boundedAnalysisIDs(nodeIDs)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	query, header, ok := s.activeAnalysisQuery()
	if !ok {
		return nil, errAnalysisGenerationUnavailable
	}
	return query.AnalysisProcessesForNodes(header.GenerationID, ids)
}

// releaseTransientAnalysisIfIdle drops request-scoped compatibility views only
// after every tracked foreground/background operation is quiet. The compact
// generation receipt and graph tokens remain published, so the next request can
// query bounded rows without a whole-graph recomputation.
func (s *Server) releaseTransientAnalysisIfIdle() bool {
	// This runs on the tail of every tool call while an analysis pass holds
	// analysisMu for its whole multi-minute run, so taking the lock
	// unconditionally stalls every request — including ones that never touch
	// analysis — behind that pass. The release is pure opportunistic
	// housekeeping, so a contended lock defers it to the next call.
	if !s.analysisMu.TryLock() {
		return false
	}
	defer s.analysisMu.Unlock()
	if !s.analysisGenerationReady {
		return false
	}
	// The durable generation is the source of truth once published. Dropping
	// these compatibility views is safe even while requests are active: a
	// request that already copied a pointer keeps that object alive, while new
	// requests materialize bounded rows from the generation store. Waiting for
	// process-wide idleness can retain the entire analysis corpus forever when
	// a cancelled MCP handler ignores its context.
	s.communities = nil
	s.leidenCache = nil
	s.processes = nil
	s.pageRank = nil
	s.adjacency = nil
	s.autoConcepts = nil
	s.hits = nil
	// Retain the complete default ranking only within a fixed memory budget.
	// A truncated prefix could hide a workspace's first visible result. This
	// keeps no partition/member maps; epoch/token checks still govern hits.
	if !s.hotspotsReady || !hotspotResultRetainable(s.hotspots) {
		s.hotspots = nil
		s.hotspotsReady = false
		s.hotspotsOwned = false
	} else if !s.hotspotsOwned {
		// Own only the bounded result bytes, never a substring's source or
		// a leading/trailing slice allocation. Existing callers retain their
		// immutable result; later callers share this independent compact copy.
		s.hotspots = compactHotspotResult(s.hotspots)
		s.hotspotsOwned = true
	}
	return true
}

func (s *Server) logAnalysisMaterializationError(component string, err error) {
	if err != nil && s.logger != nil {
		s.logger.Warn("mcp: lazy analysis materialization failed", zap.String("component", component), zap.Error(err))
	}
}

func (s *Server) installMaterializedGeneration(header graph.AnalysisGenerationHeader, install func()) bool {
	s.analysisMu.Lock()
	defer s.analysisMu.Unlock()
	if !s.analysisGenerationReady || s.analysisGeneration.GenerationID != header.GenerationID ||
		!s.analysisSnapshotCurrentLocked() {
		return false
	}
	install()
	return true
}

func (s *Server) ensureNodeMetricsMaterialized() bool {
	s.analysisMu.RLock()
	ready := s.analysisSnapshotCurrentLocked() && s.pageRank != nil && s.hits != nil
	s.analysisMu.RUnlock()
	if ready {
		return true
	}

	s.analysisMaterializeMu.Lock()
	defer s.analysisMaterializeMu.Unlock()
	s.analysisMu.RLock()
	ready = s.analysisSnapshotCurrentLocked() && s.pageRank != nil && s.hits != nil
	s.analysisMu.RUnlock()
	if ready {
		return true
	}
	query, header, ok := s.activeAnalysisQuery()
	if !ok {
		return false
	}

	pageRank := &analysis.PageRankResult{Scores: make(map[string]float64, header.NodeCount), Max: header.PageRankMax}
	hits := &analysis.HITSResult{
		Authorities: make(map[string]float64, header.NodeCount),
		Hubs:        make(map[string]float64, header.NodeCount),
		MaxAuth:     header.AuthorityMax, MaxHub: header.HubMax,
	}
	cursor := ""
	for {
		rows, next, err := query.ListAnalysisNodeMetrics(header.GenerationID, analysisGenerationQueryPage, cursor)
		if err != nil {
			s.logAnalysisMaterializationError("node_metrics", err)
			return false
		}
		for _, row := range rows {
			pageRank.Scores[row.NodeID] = row.PageRank
			hits.Authorities[row.NodeID] = row.Authority
			hits.Hubs[row.NodeID] = row.Hub
		}
		if len(rows) == 0 || next == "" || next == cursor {
			break
		}
		cursor = next
	}
	return s.installMaterializedGeneration(header, func() {
		s.pageRank = pageRank
		s.hits = hits
	})
}

func (s *Server) ensureCommunitiesMaterialized() bool {
	return s.ensureCommunitiesMaterializedContext(context.Background())
}

func (s *Server) ensureCommunitiesMaterializedContext(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	s.analysisMu.RLock()
	ready := s.analysisSnapshotCurrentLocked() && s.communities != nil
	s.analysisMu.RUnlock()
	if ready {
		return true
	}

	if !lockAnalysisPreparation(ctx, &s.analysisMaterializeMu) {
		return false
	}
	defer s.analysisMaterializeMu.Unlock()
	s.analysisMu.RLock()
	ready = s.analysisSnapshotCurrentLocked() && s.communities != nil
	s.analysisMu.RUnlock()
	if ready {
		return true
	}
	query, header, ok := s.activeAnalysisQuery()
	if !ok {
		return false
	}
	if ctx.Done() != nil {
		if reader, ok := query.(graph.Reader); ok {
			if bound, ok := graph.BindReadContext(reader, ctx).(graph.AnalysisQueryStore); ok {
				query = bound
			}
		}
	}

	result := &analysis.CommunityResult{
		Communities: make([]analysis.Community, 0, header.CommunityCount),
		NodeToComm:  make(map[string]string, header.NodeCount),
		Modularity:  header.Modularity,
	}
	cursor := ""
	for {
		if ctx.Err() != nil {
			return false
		}
		summaries, next, err := query.ListAnalysisCommunitySummaries(header.GenerationID, analysisGenerationQueryPage, cursor)
		if err != nil {
			if ctx.Err() == nil {
				s.logAnalysisMaterializationError("communities", err)
			}
			return false
		}
		for _, summary := range summaries {
			community := analysis.Community{
				ID: summary.ID, Label: summary.Label, Hub: summary.Hub, ParentID: summary.ParentID,
				Size: summary.Size, Cohesion: summary.Cohesion, Files: append([]string(nil), summary.Files...),
				Members: make([]string, 0, summary.Size),
			}
			memberCursor := ""
			for {
				if ctx.Err() != nil {
					return false
				}
				members, memberNext, memberErr := query.AnalysisCommunityMembers(header.GenerationID, summary.ID, analysisGenerationQueryPage, memberCursor)
				if memberErr != nil {
					if ctx.Err() == nil {
						s.logAnalysisMaterializationError("community_members", memberErr)
					}
					return false
				}
				for _, member := range members {
					community.Members = append(community.Members, member.NodeID)
					result.NodeToComm[member.NodeID] = summary.ID
				}
				if len(members) == 0 || memberNext == "" || memberNext == memberCursor {
					break
				}
				memberCursor = memberNext
			}
			result.Communities = append(result.Communities, community)
		}
		if len(summaries) == 0 || next == "" || next == cursor {
			break
		}
		cursor = next
	}
	if ctx.Err() != nil {
		return false
	}
	installed := s.installMaterializedGeneration(header, func() {
		if ctx.Err() == nil {
			s.communities = result
		}
	})
	return installed && ctx.Err() == nil
}

func (s *Server) ensureProcessesMaterialized() bool {
	s.analysisMu.RLock()
	ready := s.analysisSnapshotCurrentLocked() && s.processes != nil
	s.analysisMu.RUnlock()
	if ready {
		return true
	}

	s.analysisMaterializeMu.Lock()
	defer s.analysisMaterializeMu.Unlock()
	s.analysisMu.RLock()
	ready = s.analysisSnapshotCurrentLocked() && s.processes != nil
	s.analysisMu.RUnlock()
	if ready {
		return true
	}
	query, header, ok := s.activeAnalysisQuery()
	if !ok {
		return false
	}

	result := &analysis.ProcessResult{
		Processes:        make([]analysis.Process, 0, header.ProcessCount),
		NodeToProcs:      make(map[string][]string),
		Truncated:        header.ProcessesTruncated,
		TruncationReason: header.ProcessesTruncationReason,
	}
	cursor := ""
	for {
		summaries, next, err := query.ListAnalysisProcessSummaries(header.GenerationID, analysisGenerationQueryPage, cursor)
		if err != nil {
			s.logAnalysisMaterializationError("processes", err)
			return false
		}
		for _, summary := range summaries {
			process := analysis.Process{
				ID: summary.ID, Name: summary.Name, EntryPoint: summary.EntryPoint,
				StepCount: summary.StepCount, Score: summary.Score, Truncated: summary.Truncated,
				Files: append([]string(nil), summary.Files...), Steps: make([]analysis.Step, 0, summary.StepCount),
			}
			stepCursor := -1
			for {
				steps, stepNext, stepErr := query.AnalysisProcessSteps(header.GenerationID, summary.ID, analysisGenerationQueryPage, stepCursor)
				if stepErr != nil {
					s.logAnalysisMaterializationError("process_steps", stepErr)
					return false
				}
				for _, step := range steps {
					process.Steps = append(process.Steps, analysis.Step{ID: step.NodeID, Depth: step.Depth})
					result.NodeToProcs[step.NodeID] = append(result.NodeToProcs[step.NodeID], summary.ID)
				}
				if len(steps) == 0 || stepNext <= stepCursor {
					break
				}
				stepCursor = stepNext
			}
			result.Processes = append(result.Processes, process)
		}
		if len(summaries) == 0 || next == "" || next == cursor {
			break
		}
		cursor = next
	}
	return s.installMaterializedGeneration(header, func() { s.processes = result })
}

func (s *Server) ensureAutoConceptsMaterialized() bool {
	s.analysisMu.RLock()
	ready := s.analysisSnapshotCurrentLocked() && s.autoConcepts != nil
	s.analysisMu.RUnlock()
	if ready {
		return true
	}

	s.analysisMaterializeMu.Lock()
	defer s.analysisMaterializeMu.Unlock()
	s.analysisMu.RLock()
	ready = s.analysisSnapshotCurrentLocked() && s.autoConcepts != nil
	s.analysisMu.RUnlock()
	if ready {
		return true
	}
	query, header, ok := s.activeAnalysisQuery()
	if !ok {
		return false
	}

	combined := graph.AnalysisConceptQueryResult{}
	cursor := ""
	for {
		page, next, err := query.ListAnalysisConcepts(header.GenerationID, analysisGenerationQueryPage, cursor)
		if err != nil {
			s.logAnalysisMaterializationError("concepts", err)
			return false
		}
		combined.Concepts = append(combined.Concepts, page.Concepts...)
		combined.Relations = append(combined.Relations, page.Relations...)
		if len(page.Concepts) == 0 || next == "" || next == cursor {
			break
		}
		cursor = next
	}
	concepts := conceptsFromQuery(combined)
	return s.installMaterializedGeneration(header, func() { s.autoConcepts = concepts })
}

func (s *Server) ensureAdjacencyMaterialized() bool {
	s.analysisMu.RLock()
	ready := s.analysisSnapshotCurrentLocked() && s.adjacency != nil
	s.analysisMu.RUnlock()
	if ready {
		return true
	}
	s.analysisMaterializeMu.Lock()
	defer s.analysisMaterializeMu.Unlock()
	s.analysisMu.RLock()
	ready = s.analysisSnapshotCurrentLocked() && s.adjacency != nil
	s.analysisMu.RUnlock()
	if ready {
		return true
	}
	query, header, ok := s.activeAnalysisQuery()
	if !ok {
		return false
	}
	payload, found, err := query.LoadAnalysisBlob(header.GenerationID, graph.AnalysisBlobAdjacency)
	if err != nil || !found {
		if err == nil {
			err = errors.New("adjacency blob is missing")
		}
		s.logAnalysisMaterializationError("adjacency", err)
		return false
	}
	adjacency, err := restoreAdjacencyBlob(payload)
	if err != nil {
		s.logAnalysisMaterializationError("adjacency", err)
		return false
	}
	return s.installMaterializedGeneration(header, func() { s.adjacency = adjacency })
}

func (s *Server) ensureLeidenMaterialized() bool {
	s.analysisMu.RLock()
	ready := s.analysisSnapshotCurrentLocked() && s.leidenCache != nil
	s.analysisMu.RUnlock()
	if ready {
		return true
	}
	s.analysisMaterializeMu.Lock()
	defer s.analysisMaterializeMu.Unlock()
	s.analysisMu.RLock()
	ready = s.analysisSnapshotCurrentLocked() && s.leidenCache != nil
	s.analysisMu.RUnlock()
	if ready {
		return true
	}
	query, header, ok := s.activeAnalysisQuery()
	if !ok {
		return false
	}
	payload, found, err := query.LoadAnalysisBlob(header.GenerationID, graph.AnalysisBlobLeiden)
	if err != nil || !found {
		if err == nil {
			err = errors.New("leiden blob is missing")
		}
		s.logAnalysisMaterializationError("leiden", err)
		return false
	}
	leiden, err := restoreLeidenBlob(payload, s.currentCommunityToken().edgeIdentity)
	if err != nil {
		s.logAnalysisMaterializationError("leiden", err)
		return false
	}
	return s.installMaterializedGeneration(header, func() { s.leidenCache = leiden })
}
