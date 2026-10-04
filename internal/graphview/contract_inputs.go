package graphview

import (
	"context"
	"reflect"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// SelectedContractInputs captures contract-specific input authority from the
// actual selected ancestry. Ordinary view materialization never calls this.
// Witnesses retain physical namespaces and absences separately from the stable
// cumulative logical identity used to select an analysis attachment.
type SelectedContractInputs struct {
	State          graph.ContractInputState
	Witnesses      []graph.ContractInputWitness
	sources        []contractInputSource
	dependencies   []*SelectedContractInputs
	store          *store_sqlite.Store
	repo, checkout string
}

// ComposeSelectedContractInputs includes only dependency repos the analysis
// actually consults. It preserves each captured baseline and physical fence;
// companion-repo input changes cannot be hidden by an unchanged local receipt.
func ComposeSelectedContractInputs(repo, checkout string, dependencies ...*SelectedContractInputs) (*SelectedContractInputs, error) {
	var witnesses []graph.ContractInputWitness
	var store *store_sqlite.Store
	for _, input := range dependencies {
		if input == nil {
			return nil, graph.ErrContractInputVector
		}
		if store == nil {
			store = input.store
		}
		if store == nil || input.store != store {
			return nil, graph.ErrContractInputVector
		}
		witnesses = append(witnesses, input.Witnesses...)
	}
	state, err := graph.ComposeContractInputState(repo, checkout, witnesses)
	if err != nil {
		return nil, err
	}
	return &SelectedContractInputs{State: state, Witnesses: witnesses, dependencies: append([]*SelectedContractInputs(nil), dependencies...), store: store, repo: repo, checkout: checkout}, nil
}

type contractInputSource struct {
	handle *store_sqlite.Store
	actor  string
}

type contractInputCohortReader interface {
	ContractInputStatesForRepoContext(context.Context, string) ([]graph.ContractInputState, error)
}

// CaptureContractInputs reads only the requested repo and selected physical
// generations. A dedicated full root excludes mutable generation zero; a sparse
// positive layer cannot substitute for an unknown inherited baseline.
func (m *Materializer) CaptureContractInputs(ctx context.Context, view *RepoView, repo, checkout string) (*SelectedContractInputs, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m == nil || m.Store == nil {
		return nil, NewViewError(CodeRequiredCapabilityIncomplete, "selected contract input authority is unavailable")
	}
	var sources []contractInputSource
	if view == nil {
		sources = append(sources, contractInputSource{handle: m.Store.AtGeneration(BaseCorpusGeneration)})
	} else {
		if view.contractStore != m.Store || view.ID.RepoPrefix != repo {
			return nil, NewViewError(CodeRequiredCapabilityIncomplete, "contract inputs belong to a different selected store")
		}
		if view.ComposesBaseCorpus() {
			sources = append(sources, contractInputSource{handle: m.Store.AtGeneration(BaseCorpusGeneration)})
		}
		for _, source := range view.sources {
			sources = append(sources, contractInputSource{handle: source.Handle, actor: source.CheckoutID})
		}
	}
	if len(sources) == 0 {
		return nil, NewViewError(CodeRequiredCapabilityIncomplete, "selected contract baseline is unavailable")
	}
	witnesses, baseline, err := readSelectedContractInputs(ctx, sources, repo)
	if err != nil {
		return nil, err
	}
	if !baseline {
		return nil, NewViewError(CodeRequiredCapabilityIncomplete, "selected contract baseline is not yet certified")
	}
	state, err := graph.ComposeContractInputState(repo, checkout, witnesses)
	if err != nil {
		return nil, err
	}
	return &SelectedContractInputs{State: state, Witnesses: witnesses, sources: sources, store: m.Store, repo: repo, checkout: checkout}, nil
}

func readSelectedContractInputs(ctx context.Context, sources []contractInputSource, repo string) ([]graph.ContractInputWitness, bool, error) {
	var witnesses []graph.ContractInputWitness
	baseline := false
	for index, source := range sources {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		preferred := graph.ContractInputState{RepoPrefix: repo, CheckoutID: source.actor}
		generation := source.handle.ViewGeneration()
		if generation == BaseCorpusGeneration {
			state, found, err := source.handle.ContractInputStateContext(ctx, repo, "")
			if err != nil {
				return nil, false, err
			}
			if found {
				preferred = state
			}
			witnesses = append(witnesses, graph.ContractInputWitness{GenerationID: generation, State: preferred, Found: found})
			if index == 0 {
				baseline = found
			}
			continue
		}
		reader, ok := any(source.handle).(contractInputCohortReader)
		if !ok {
			return nil, false, graph.ErrContractInputVector
		}
		states, err := reader.ContractInputStatesForRepoContext(ctx, repo)
		if err != nil {
			return nil, false, err
		}
		var selected *graph.ContractInputState
		for _, state := range states {
			if state.RepoPrefix != repo {
				return nil, false, graph.ErrContractInputVector
			}
			if state.CheckoutID == source.actor {
				copyState := state
				selected = &copyState
				break
			}
		}
		if selected != nil {
			witnesses = append(witnesses, graph.ContractInputWitness{GenerationID: generation, State: *selected, Found: true})
		} else {
			// Copy/fold preserves input provenance. Capture the absent preferred
			// actor too, so inserting it later invalidates the publication CAS.
			witnesses = append(witnesses, graph.ContractInputWitness{GenerationID: generation, State: preferred})
			if len(states) > 1 {
				return nil, false, graph.ErrContractInputVector
			}
			if len(states) == 1 {
				witnesses = append(witnesses, graph.ContractInputWitness{GenerationID: generation, State: states[0], Found: true})
				selected = &states[0]
			}
		}
		if index == 0 {
			baseline = selected != nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return witnesses, baseline, nil
}

// Validate rejects changed selected authority without depending on unrelated
// core/store revisions. It is also required after consumer reads before claiming
// the bound contract result fresh.
func (inputs *SelectedContractInputs) Validate(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if inputs == nil {
		return graph.ErrContractInputVector
	}
	if len(inputs.dependencies) > 0 {
		var witnesses []graph.ContractInputWitness
		for _, dependency := range inputs.dependencies {
			if err := dependency.Validate(ctx); err != nil {
				return err
			}
			witnesses = append(witnesses, dependency.Witnesses...)
		}
		state, err := graph.ComposeContractInputState(inputs.repo, inputs.checkout, witnesses)
		if err != nil {
			return err
		}
		if state != inputs.State || !reflect.DeepEqual(witnesses, inputs.Witnesses) {
			return graph.ErrContractProjectionStale
		}
		return nil
	}
	current, baseline, err := readSelectedContractInputs(ctx, inputs.sources, inputs.repo)
	if err != nil {
		return err
	}
	if !baseline || !reflect.DeepEqual(inputs.Witnesses, current) {
		return graph.ErrContractProjectionStale
	}
	state, err := graph.ComposeContractInputState(inputs.repo, inputs.checkout, current)
	if err != nil {
		return err
	}
	if state != inputs.State {
		return graph.ErrContractProjectionStale
	}
	return nil
}
