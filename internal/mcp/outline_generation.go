package mcp

import (
	"context"
	"errors"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// outlineGenerationCommunities uses only the selected reader's node visibility.
// Contract-core changes edges, not GetNode/GetNodesByIDs, so its underlying
// selected reader is safe for this node-only query. Composed/overlay readers
// deliberately omit the capability and retain the membership-lookup fallback.
func (s *Server) outlineGenerationCommunities(ctx context.Context, reader graph.Reader, scope graph.NodeScope) (*analysis.CommunityResult, bool, error) {
	if wrapped, ok := reader.(contractCoreWrapped); ok {
		reader = wrapped.contractCore().Reader
	}
	reader = graph.BindReadContext(reader, ctx)
	query, ok := reader.(graph.ScopedAnalysisCommunityReader)
	if !ok {
		return nil, false, nil
	}
	_, header, current := s.activeAnalysisQueryForAnswer()
	if !current {
		return nil, false, nil
	}
	if header.CommunityCount == 0 {
		return nil, true, nil
	}
	result := &analysis.CommunityResult{Modularity: header.Modularity}
	cursor := ""
	for {
		rows, next, err := query.ScopedAnalysisCommunitySummaries(header.GenerationID, scope, analysisGenerationQueryPage, cursor)
		if errors.Is(err, graph.ErrAnalysisGenerationInactive) {
			// A selected payload may differ from the server's base analysis,
			// or a mutation may have invalidated its receipt. The legacy path
			// rechecks freshness and selected-reader membership in both cases.
			return nil, false, nil
		}
		if err != nil {
			return nil, true, err
		}
		for _, row := range rows {
			result.Communities = append(result.Communities, analysis.Community{
				ID: row.ID, Label: row.Label, Hub: row.Hub, ParentID: row.ParentID, Size: row.Size, Cohesion: row.Cohesion,
			})
		}
		if next == "" {
			break
		}
		cursor = next
	}
	// Pages must all describe the same still-current publication. Never mix
	// a partition read before a mutation with nodes from a subsequent revision.
	_, after, current := s.activeAnalysisQueryForAnswer()
	if !current || after.GenerationID != header.GenerationID {
		return nil, false, nil
	}
	return result, true, nil
}
