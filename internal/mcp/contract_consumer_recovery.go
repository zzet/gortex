package mcp

import (
	"context"
	"errors"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// openContractPredecessorBinding first honors owned coordinator metadata. Only
// its explicit absence permits one-moved physical provenance recovery; a known
// invalid/retired predecessor cannot silently select a different historical key.
func (s *Server) openContractPredecessorBinding(ctx context.Context, selected *graphview.RepoView, checkout string, repos []string, actor indexer.ContractActor) (*contractAnalysisContext, *indexer.ContractPublishedCohort, error) {
	binding, proof, err := s.openPublishedContractConsumerBinding(ctx, selected, checkout, repos, actor)
	if errors.Is(err, indexer.ErrContractPublishedCohortAbsent) {
		views, openErr := s.materializer.OpenPreviousContractCohort(ctx, selected, checkout, repos)
		err = openErr
		if err == nil {
			binding = &contractAnalysisContext{views: views, served: true, recovered: true}
			if err = binding.validate(ctx); err != nil {
				binding.close()
				binding = nil
			}
		}
		// Recovery has neither publication Seq nor receipt history. In particular
		// it can never satisfy an arrival-sequence fresh-success comparison.
		proof = nil
	}
	if errors.Is(err, graph.ErrContractPredecessorRetired) {
		viewmetrics.Count(viewmetrics.ContractAnalysisPredecessorRetiredTotal)
	}
	return binding, proof, err
}
