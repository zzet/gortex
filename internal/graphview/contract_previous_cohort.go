package graphview

import (
	"context"
	"slices"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// OpenPreviousContractCohort recovers only one physical contributor move from
// exact persisted full-cohort provenance. It returns request-owned views, never
// a publication seq or receipt-stamp proof. Physical folds/legacy ancestry
// mismatch conservatively refuse; the ordinary current-exact path is unchanged.
func (m *Materializer) OpenPreviousContractCohort(ctx context.Context, selected *RepoView, checkout string, repos []string) (views map[string]*ContractAnalysisView, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m == nil || m.Store == nil || m.Catalog == nil || m.Leases == nil || len(repos) == 0 || len(repos) > 1024 {
		return nil, graph.ErrContractInputVector
	}
	seenRepos := map[string]bool{}
	var captures []*SelectedContractInputs
	for _, repo := range repos {
		if seenRepos[repo] {
			return nil, graph.ErrContractInputVector
		}
		seenRepos[repo] = true
		var input *SelectedContractInputs
		if selected != nil && selected.ID.RepoPrefix != repo {
			input, err = m.CaptureContractCompanionInputs(ctx, selected, repo, checkout)
		} else {
			input, err = m.CaptureContractInputs(ctx, selected, repo, checkout)
		}
		if err != nil {
			return nil, err
		}
		captures = append(captures, input)
	}
	inputs, err := ComposeSelectedContractInputs(repos[0], checkout, captures...)
	if err != nil {
		return nil, err
	}
	effective := graph.EffectiveContractInputSlots(inputs.Witnesses)
	physical := map[graph.ContractInputPhysicalSlot]graph.ContractInputWitness{}
	for _, w := range inputs.Witnesses {
		slot := graph.ContractWitnessSlot(w)
		if prior, found := physical[slot]; found && prior != w {
			return nil, graph.ErrContractInputVector
		}
		physical[slot] = w
	}
	var moved graph.ContractInputPhysicalSlot
	count := 0
	for slot := range effective {
		w := physical[slot]
		published, readErr := m.Store.ContractInputIdentityPublishedContext(ctx, w)
		if readErr != nil {
			return nil, readErr
		}
		if !published {
			moved = slot
			count++
		}
	}
	if count != 1 {
		return nil, graph.ErrContractProjectionIncomplete
	}
	candidate, err := graph.ContractWitnessesWithPrevious(inputs.Witnesses, moved)
	if err != nil {
		return nil, err
	}
	// The index is discovery only, never authority for a coherent companion set.
	for _, w := range candidate {
		if graph.ContractWitnessSlot(w) == moved {
			published, readErr := m.Store.ContractInputIdentityPublishedContext(ctx, w)
			if readErr != nil {
				return nil, readErr
			}
			if !published {
				return nil, graph.ErrContractProjectionIncomplete
			}
			break
		}
	}
	expected, err := graph.CanonicalContractWitnesses(candidate)
	if err != nil {
		return nil, err
	}
	type target struct {
		key        graph.ContractAttachmentKey
		generation int64
	}
	var targets []target
	var historical []graph.ContractInputWitness
	for _, repo := range repos {
		logical, composeErr := graph.ComposeContractInputStateWithPrevious(repo, checkout, inputs.Witnesses, moved)
		if composeErr != nil || !logical.Accepted {
			return nil, graph.ErrContractInputVector
		}
		key := graph.ContractAttachmentKey{RepoPrefix: repo, CheckoutID: checkout, InputVersion: logical.InputVersion, InputFingerprint: logical.InputFingerprint}
		header, w, readErr := m.Store.ContractAttachmentProofContext(ctx, key)
		if readErr != nil {
			return nil, readErr
		}
		if header == nil {
			return nil, graph.ErrContractProjectionIncomplete
		}
		canonical, normalizeErr := graph.CanonicalContractWitnesses(w)
		if normalizeErr != nil || !slices.Equal(canonical, expected) {
			return nil, graph.ErrContractInputVector
		}
		if err := graph.ValidateSingleMovedContractWitnesses(inputs.Witnesses, w, moved); err != nil {
			return nil, err
		}
		if historical == nil {
			historical = w
		} else {
			prior, _ := graph.CanonicalContractWitnesses(historical)
			if !slices.Equal(prior, canonical) {
				return nil, graph.ErrContractInputVector
			}
		}
		targets = append(targets, target{key, header.PayloadGeneration})
	}
	historicalCustody := func() error {
		for _, w := range historical {
			if w.GenerationID <= 0 {
				continue
			}
			row, found, readErr := m.Catalog.GetViewGeneration(ctx, w.GenerationID)
			if readErr != nil {
				return readErr
			}
			if !found || row.State == store_sqlite.ViewGenerationRetiring {
				return graph.ErrContractPredecessorRetired
			}
			if row.State != store_sqlite.ViewGenerationReady && row.State != store_sqlite.ViewGenerationSuperseded {
				return graph.ErrContractProjectionStale
			}
		}
		return nil
	}
	sources, err := m.AcquireContractServedSources(ctx, inputs)
	if err != nil {
		if custodyErr := historicalCustody(); custodyErr != nil {
			return nil, custodyErr
		}
		return nil, err
	}
	defer sources.Close()
	generations := make([]int64, 0, len(targets))
	for _, target := range targets {
		generations = append(generations, target.generation)
	}
	payloads := m.Leases.Acquire(generations...)
	defer payloads.Release()
	if err := historicalCustody(); err != nil {
		return nil, err
	}
	views = map[string]*ContractAnalysisView{}
	defer func() {
		if err != nil {
			for _, view := range views {
				view.Close()
			}
			views = nil
		}
	}()
	for _, target := range targets {
		header, w, readErr := m.Store.ContractAttachmentProofContext(ctx, target.key)
		if readErr != nil {
			return views, readErr
		}
		if header == nil {
			return views, graph.ErrContractPredecessorRetired
		}
		canonical, normalizeErr := graph.CanonicalContractWitnesses(w)
		if normalizeErr != nil || header.PayloadGeneration != target.generation || !slices.Equal(canonical, expected) {
			return views, graph.ErrContractProjectionStale
		}
		analysis, openErr := m.OpenServedContractAnalysis(ctx, target.key, target.generation, historical, sources)
		if openErr != nil {
			header, _, recheckErr := m.Store.ContractAttachmentProofContext(ctx, target.key)
			if recheckErr != nil {
				return views, recheckErr
			}
			if header == nil {
				return views, graph.ErrContractPredecessorRetired
			}
			return views, openErr
		}
		views[target.key.RepoPrefix] = analysis
	}
	if err := inputs.Validate(ctx); err != nil {
		return views, err
	}
	return views, nil
}
