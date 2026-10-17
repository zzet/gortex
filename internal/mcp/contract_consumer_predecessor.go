package mcp

import (
	"context"
	"slices"
	"sort"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

// openPublishedContractConsumerBinding never selects independent latest repo
// payloads. One retained publication proves the entire admitted cohort.
func (s *Server) openPublishedContractConsumerBinding(ctx context.Context, selected *graphview.RepoView, checkout string, repos []string, actor indexer.ContractActor) (*contractAnalysisContext, *indexer.ContractPublishedCohort, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	proof, release, err := s.contractAnalysisRuntime.AcquireLastPublishedCohort(ctx, actor)
	if err != nil {
		return nil, nil, err
	}
	binding := &contractAnalysisContext{views: make(map[string]*graphview.ContractAnalysisView), releaseCohort: release, served: true}
	fail := func(err error) (*contractAnalysisContext, *indexer.ContractPublishedCohort, error) {
		binding.close()
		return nil, nil, err
	}
	if proof == nil || len(proof.Targets) != len(repos) || release == nil {
		return fail(graph.ErrContractProjectionIncomplete)
	}
	actual := make([]string, 0, len(proof.Targets))
	for _, target := range proof.Targets {
		if target.Key.CheckoutID != checkout {
			return fail(graph.ErrContractProjectionStale)
		}
		actual = append(actual, target.Key.RepoPrefix)
	}
	sort.Strings(actual)
	if !slices.Equal(actual, repos) {
		return fail(graph.ErrContractProjectionIncomplete)
	}
	// Current source composition comes only from existing materializer captures.
	// They may be logically unaccepted; missing/legacy ancestry still refuses.
	// Reader ownership stays with this RPC: overlay middleware registers
	// binding.close after view.close/scope.Release, so LIFO cleanup closes this
	// binding first. The deadline firewall retains the actual executing request
	// scope; this binding is never handed to independent background work.
	captures := make([]*graphview.SelectedContractInputs, 0, len(repos))
	for _, repo := range repos {
		var input *graphview.SelectedContractInputs
		if selected != nil && selected.ID.RepoPrefix != repo {
			input, err = s.materializer.CaptureContractCompanionInputs(ctx, selected, repo, checkout)
		} else {
			input, err = s.materializer.CaptureContractInputs(ctx, selected, repo, checkout)
		}
		if err != nil {
			return fail(err)
		}
		captures = append(captures, input)
	}
	inputs, err := graphview.ComposeSelectedContractInputs(repos[0], checkout, captures...)
	if err != nil {
		return fail(err)
	}
	sources, err := s.materializer.AcquireContractServedSources(ctx, inputs)
	if err != nil {
		return fail(err)
	}
	defer sources.Close()
	for _, target := range proof.Targets {
		analysis, openErr := s.materializer.OpenServedContractAnalysis(ctx, target.Key, target.PayloadGeneration, proof.Witnesses, sources)
		if openErr != nil {
			return fail(openErr)
		}
		binding.views[target.Key.RepoPrefix] = analysis
	}
	if err := binding.validate(ctx); err != nil {
		return fail(err)
	}
	return binding, proof, nil
}
