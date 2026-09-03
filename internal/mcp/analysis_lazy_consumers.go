package mcp

import (
	"context"
	"sort"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

// analysisNodeMetricsBatched keeps callers on the normalized analysis store
// while allowing bounded operations whose affected set crosses one SQLite
// parameter page. Rows are returned in backend order; callers that expose an
// order must derive it from their original ID slice or sort explicitly.
func (s *Server) analysisNodeMetricsBatched(nodeIDs []string) ([]graph.AnalysisNodeMetric, error) {
	ids := dedupeStrings(nodeIDs)
	rows := make([]graph.AnalysisNodeMetric, 0, len(ids))
	for start := 0; start < len(ids); start += analysisGenerationQueryMax {
		end := min(start+analysisGenerationQueryMax, len(ids))
		page, err := s.analysisNodeMetrics(ids[start:end])
		if err != nil {
			return nil, err
		}
		rows = append(rows, page...)
	}
	return rows, nil
}

func (s *Server) analysisProcessesForNodesBatched(nodeIDs []string) ([]graph.AnalysisProcessMembership, error) {
	ids := dedupeStrings(nodeIDs)
	rows := make([]graph.AnalysisProcessMembership, 0, len(ids))
	for start := 0; start < len(ids); start += analysisGenerationQueryMax {
		end := min(start+analysisGenerationQueryMax, len(ids))
		page, err := s.analysisProcessesForNodes(ids[start:end])
		if err != nil {
			return nil, err
		}
		rows = append(rows, page...)
	}
	return rows, nil
}

func analysisMetricValue(row graph.AnalysisNodeMetric, metric graph.AnalysisMetric) float64 {
	switch metric {
	case graph.AnalysisMetricPageRank:
		return row.PageRank
	case graph.AnalysisMetricAuthority:
		return row.Authority
	case graph.AnalysisMetricHub:
		return row.Hub
	default:
		return 0
	}
}

func (s *Server) topAnalysisMetricValue(metric graph.AnalysisMetric) float64 {
	rows, _, err := s.topAnalysisNodeMetrics(metric, 1, nil)
	if err != nil || len(rows) == 0 {
		return 0
	}
	return analysisMetricValue(rows[0], metric)
}

// analyzeImpactLazy runs the graph traversal without compatibility maps, then
// enriches the bounded result set from normalized rows. It preserves the
// deterministic community/process ordering of analysis.AnalyzeImpactContext
// without retaining whole-graph maps on the Server.
func (s *Server) analyzeImpactLazy(ctx context.Context, symbolIDs []string) *analysis.ImpactResult {
	result := analysis.AnalyzeImpactContext(ctx, s.readerFor(ctx), symbolIDs, nil, nil)
	ids := append([]string(nil), symbolIDs...)
	for depth := 1; depth <= 3; depth++ {
		for _, entry := range result.ByDepth[depth] {
			ids = append(ids, entry.ID)
		}
	}
	ids = dedupeStrings(ids)

	if metrics, err := s.analysisNodeMetricsBatched(ids); err == nil {
		communities := make(map[string]struct{})
		for _, metric := range metrics {
			if metric.CommunityID != "" {
				communities[metric.CommunityID] = struct{}{}
			}
		}
		result.AffectedCommunities = result.AffectedCommunities[:0]
		for id := range communities {
			result.AffectedCommunities = append(result.AffectedCommunities, id)
		}
		sort.Strings(result.AffectedCommunities)
	}

	if memberships, err := s.analysisProcessesForNodesBatched(ids); err == nil {
		processes := make(map[string]struct{})
		for _, membership := range memberships {
			if membership.ProcessID != "" {
				processes[membership.ProcessID] = struct{}{}
			}
		}
		result.AffectedProcesses = result.AffectedProcesses[:0]
		for id := range processes {
			result.AffectedProcesses = append(result.AffectedProcesses, id)
		}
		sort.Strings(result.AffectedProcesses)
	}
	return result
}

func (s *Server) communitySummariesForNodes(nodeIDs []string, scoped bool, limit int) []graph.AnalysisCommunitySummary {
	allowed := make(map[string]struct{})
	if scoped {
		metrics, err := s.analysisNodeMetricsBatched(nodeIDs)
		if err != nil {
			return nil
		}
		for _, metric := range metrics {
			if metric.CommunityID != "" {
				allowed[metric.CommunityID] = struct{}{}
			}
		}
	}
	out := make([]graph.AnalysisCommunitySummary, 0, limit)
	cursor := ""
	for len(out) < limit {
		rows, next, err := s.analysisCommunitySummaries(analysisGenerationQueryPage, cursor)
		if err != nil {
			return nil
		}
		for _, row := range rows {
			if scoped {
				if _, ok := allowed[row.ID]; !ok {
					continue
				}
			}
			out = append(out, row)
			if len(out) == limit {
				break
			}
		}
		if next == "" || next == cursor || len(rows) == 0 {
			break
		}
		cursor = next
	}
	return out
}

func (s *Server) processSummariesForEntries(entryIDs map[string]bool, scoped bool, limit int) []graph.AnalysisProcessSummary {
	out := make([]graph.AnalysisProcessSummary, 0, limit)
	cursor := ""
	for len(out) < limit {
		rows, next, err := s.analysisProcessSummaries(analysisGenerationQueryPage, cursor)
		if err != nil {
			return nil
		}
		for _, row := range rows {
			if scoped && !entryIDs[row.EntryPoint] {
				continue
			}
			out = append(out, row)
			if len(out) == limit {
				break
			}
		}
		if next == "" || next == cursor || len(rows) == 0 {
			break
		}
		cursor = next
	}
	return out
}

func (s *Server) rerankBoundedCentrality(ctx context.Context, seeds, candidateIDs []string) rerank.CentralityResult {
	snapshot, stats := analysis.BuildBoundedAdjacencySnapshot(s.readerFor(ctx), candidateIDs, 2, 4096, 16384)
	return rerank.CentralityResult{
		Scores:      s.personalizedPageRank(snapshot, seeds),
		NodeCount:   stats.NodeCount,
		EdgeCount:   stats.EdgeCount,
		NodeBatches: stats.NodeBatches,
		EdgeBatches: stats.EdgeBatches,
		Truncated:   stats.Truncated,
	}
}

// rerankAnalysisMetrics returns exactly the requested candidates and global
// normalization maxima. The callback is invoked once from rerank.Context.Prepare.
func (s *Server) rerankAnalysisMetrics(nodeIDs []string) map[string]rerank.AnalysisMetric {
	rows, err := s.analysisNodeMetricsBatched(nodeIDs)
	if err != nil || len(rows) == 0 {
		return nil
	}
	maxAuthority := s.topAnalysisMetricValue(graph.AnalysisMetricAuthority)
	maxHub := s.topAnalysisMetricValue(graph.AnalysisMetricHub)
	metrics := make(map[string]rerank.AnalysisMetric, len(rows))
	for _, row := range rows {
		metric := rerank.AnalysisMetric{CommunityID: row.CommunityID}
		if maxAuthority > 0 {
			metric.Authority = row.Authority / maxAuthority
		}
		if maxHub > 0 {
			metric.Hub = row.Hub / maxHub
		}
		metrics[row.NodeID] = metric
	}
	return metrics
}
