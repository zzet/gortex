package indexer

import (
	"context"
	"fmt"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// This is the actual selected predecessor descriptor used by core builders.
// It never substitutes an actor's latest head or folds mutable0 into the
// cumulative positive identity.
func newSelectedContractCoreStorageBackend(ctx context.Context, target *store_sqlite.Store, base graph.Reader, repo, checkout string) (*contractCoreStorageBackend, error) {
	if target == nil || target.ViewGeneration() <= 0 {
		return nil, fmt.Errorf("contract core: positive target required")
	}
	var sources []graphview.GenerationSource
	switch selected := base.(type) {
	case commitLayerBase:
		if selected.cloneCorpusBase != nil {
			sources = append(sources, graphview.GenerationSource{Handle: selected.cloneCorpusBase})
		}
		sources = append(sources, selected.cloneSources...)
		if len(sources) == 0 && selected.corpus != nil {
			sources = append(sources, graphview.GenerationSource{Handle: selected.corpus, Generation: selected.corpus.ViewGeneration()})
		}
	case *store_sqlite.Store:
		sources = append(sources, graphview.GenerationSource{Handle: selected, Generation: selected.ViewGeneration()})
	case nil:
		// A full cold root has no predecessor. All accepted sources are parsed by
		// that root and contract work remains pending, never a certified empty tier.
	default:
		return nil, fmt.Errorf("contract core: selected physical ancestry unavailable")
	}
	actors := make(map[int64]string)
	var cumulative *graph.ContractInputState
	for _, source := range sources {
		if source.Handle == nil {
			return nil, graph.ErrContractInputVector
		}
		generation := source.Handle.ViewGeneration()
		if generation == 0 {
			actors[0] = ""
			continue
		}
		states, e := source.Handle.ContractInputStatesForRepoContext(ctx, repo)
		if e != nil {
			return nil, e
		}
		if len(states) > 1 {
			return nil, graph.ErrContractInputVector
		}
		if len(states) == 1 {
			copyState := states[0]
			cumulative = &copyState
			actors[generation] = copyState.CheckoutID
		} else {
			actors[generation] = source.CheckoutID
		}
	}
	if cumulative == nil {
		cumulative = &graph.ContractInputState{RepoPrefix: repo, CheckoutID: checkout, InputVersion: contractCoreInputVersion,
			InputFingerprint: contractInputHash([]byte(contractCoreInputVersion + ":empty-positive:" + repo)), Accepted: true}
	}
	capture := contractCoreStorageCapture{
		Prior: func(ctx context.Context, path string) (*graph.ContractBoundaryReceipt, *graph.ContractBoundaryReceiptSource, bool, error) {
			for i := len(sources) - 1; i >= 0; i-- {
				physical := sources[i]
				generation := physical.Handle.ViewGeneration()
				actor := actors[generation]
				row, known, e := physical.Handle.ContractBoundaryReceiptForVersionContext(ctx, repo, actor, path, contractBoundaryReceiptVersion)
				if e != nil {
					return nil, nil, false, e
				}
				if row != nil {
					if !row.Accepted {
						return nil, nil, false, graph.ErrContractProjectionStale
					}
					source := graph.ContractBoundaryReceiptSource{GenerationID: generation, Receipt: *row, TargetCheckoutID: checkout}
					return row, &source, true, nil
				}
				if physical.Layer != nil && physical.Layer.HasFile(path) {
					return nil, nil, false, nil
				}
				if generation == 0 {
					return nil, nil, known, nil
				}
			}
			return nil, nil, base == nil, nil
		},
		Dependents: func(ctx context.Context, keys []string) ([]contractCoreDependent, error) {
			var out []contractCoreDependent
			for i, source := range sources {
				rows, e := source.Handle.ContractBoundaryReceiptsForLookupKeysContext(ctx, keys, 4096)
				if e != nil {
					return nil, e
				}
				for _, row := range rows {
					hidden := false
					for _, above := range sources[i+1:] {
						if above.Layer != nil && above.Layer.HasFile(row.FilePath) {
							hidden = true
							break
						}
					}
					if hidden {
						continue
					}
					// Sealed unrelated actors in this physical payload are not selected.
					if source.Handle.ViewGeneration() > 0 && row.CheckoutID != actors[source.Handle.ViewGeneration()] {
						continue
					}
					out = append(out, contractCoreDependent{RepoPrefix: row.RepoPrefix, CheckoutID: row.CheckoutID, FilePath: row.FilePath,
						ReceiptVersion: row.Version, ReceiptFingerprint: row.Fingerprint, SourceFingerprint: row.SourceFingerprint, Accepted: row.Accepted, Scope: row.Scope})
				}
			}
			return out, nil
		},
	}
	backend, e := newContractCoreStorageBackend(target, repo, checkout, target.ViewGeneration(), cumulative, nil, capture)
	if e != nil {
		return nil, e
	}
	carried := *cumulative
	carried.CheckoutID = checkout
	carried.Accepted = true
	if e := target.SetContractInputStateWithWorkReceiptsAndSourcesContext(ctx, nil, carried, nil, nil, nil); e != nil {
		return nil, e
	}
	backend.state = &carried
	backend.expected = &carried
	backend.stateInstalled = true
	return backend, nil
}
func (b *SparseGenerationBuilder) installSelectedContractCoreInputs(ctx context.Context, idx *Indexer, handle *store_sqlite.Store, req BuildRequest) error {
	hooks := b.contractCoreRuntime.Load()
	if hooks == nil {
		return nil
	}
	backend, e := newSelectedContractCoreStorageBackend(ctx, handle, req.Base, req.RepoPrefix, req.Identity.CheckoutID)
	if e != nil {
		return e
	}
	idx.contractCoreInputs, e = newContractCoreInputJournal(ctx, backend)
	if e != nil {
		return e
	}
	idx.contractCoreRuntime.Store(hooks)
	idx.contractProjectionNeedsWitness = false
	idx.contractInputWitness = nil
	return nil
}
