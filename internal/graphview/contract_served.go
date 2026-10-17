package graphview

import (
	"context"
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// ContractServedSources pins the positive generations of admitted current core
// composition independently of a historical attachment key. Readers remain
// borrowed from the request's selected view and repository admission scope: this
// object must close before those owners, and does not authorize detached work.
// It does not assert current input equality.
type ContractServedSources struct {
	materializer *Materializer
	readers      map[string]graph.Reader
	lease        *Lease
	generations  []int64
	once         sync.Once
}

// AcquireContractServedSources accepts only materializer-owned captures. An
// unaccepted logical state is allowed; unavailable ancestry is never replaced
// with a primary source. The current captures establish authority before pins
// and catalog lifecycle checks establish positive-generation custody. The caller
// must retain the originating request view and repository scope until Close;
// generation pins alone do not own their composed readers or base-corpus owner.
func (m *Materializer) AcquireContractServedSources(ctx context.Context, inputs *SelectedContractInputs) (*ContractServedSources, error) {
	if m == nil || m.Store == nil || m.Leases == nil || m.Catalog == nil || inputs == nil || inputs.store != m.Store {
		return nil, graph.ErrContractInputVector
	}
	if err := inputs.Validate(ctx); err != nil {
		return nil, err
	}
	ids := map[int64]bool{}
	for _, witness := range inputs.Witnesses {
		if witness.GenerationID > 0 {
			ids[witness.GenerationID] = true
		}
	}
	generations := make([]int64, 0, len(ids))
	for generation := range ids {
		generations = append(generations, generation)
	}
	sources := &ContractServedSources{materializer: m, readers: inputs.SourceReaders(), generations: generations, lease: m.Leases.Acquire(generations...)}
	if len(sources.readers) == 0 {
		sources.Close()
		return nil, graph.ErrContractInputVector
	}
	if err := sources.Validate(ctx); err != nil {
		sources.Close()
		return nil, err
	}
	return sources, nil
}

func (s *ContractServedSources) Validate(ctx context.Context) error {
	if s == nil || s.materializer == nil || s.lease == nil || s.lease.Holders() == 0 {
		return graph.ErrContractProjectionStale
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, generation := range s.generations {
		row, found, err := s.materializer.Catalog.GetViewGeneration(ctx, generation)
		if err != nil {
			return err
		}
		if !found || row.State != store_sqlite.ViewGenerationReady && row.State != store_sqlite.ViewGenerationSuperseded {
			return graph.ErrContractProjectionStale
		}
	}
	return nil
}

func (s *ContractServedSources) Close() {
	if s != nil {
		s.once.Do(func() { s.lease.Release() })
	}
}

// OpenServedContractAnalysis opens an exact historical payload while core
// identity reads use admitted CURRENT request-borrowed selected composition.
// The request must keep its selected view and repository scope alive through the
// analysis view's Close; source-generation pins alone cannot detach these readers.
// No historical live-input capture is fabricated here.
func (m *Materializer) OpenServedContractAnalysis(ctx context.Context, key graph.ContractAttachmentKey, expectedGeneration int64, witnesses []graph.ContractInputWitness, sources *ContractServedSources) (*ContractAnalysisView, error) {
	if sources == nil || sources.materializer != m || sources.readers[key.RepoPrefix] == nil || expectedGeneration <= 0 {
		return nil, graph.ErrContractInputVector
	}
	logical, err := graph.ComposeContractInputState(key.RepoPrefix, key.CheckoutID, witnesses)
	if err != nil || !logical.Accepted || logical.InputVersion != key.InputVersion || logical.InputFingerprint != key.InputFingerprint {
		return nil, graph.ErrContractProjectionStale
	}
	if err := sources.Validate(ctx); err != nil {
		return nil, err
	}
	analysis, err := m.OpenContractAnalysis(ctx, sources.readers[key.RepoPrefix], ContractAnalysisInput{RepoPrefix: key.RepoPrefix, CheckoutID: key.CheckoutID, InputVersion: key.InputVersion, InputFingerprint: key.InputFingerprint})
	if err != nil {
		return nil, err
	}
	if analysis.Attachment.PayloadGeneration != expectedGeneration {
		analysis.Close()
		return nil, graph.ErrContractProjectionStale
	}
	analysis.RegistryReader.(*contractRegistryReader).sources = sources.readers
	// Share source custody through an independent lease, so closing one repo does not
	// release another repo's source handles.
	analysis.servedSources = &ContractServedSources{materializer: m, readers: sources.readers, generations: sources.generations, lease: m.Leases.Acquire(sources.generations...)}
	if err := analysis.ValidateServed(ctx); err != nil {
		analysis.Close()
		return nil, err
	}
	return analysis, nil
}
