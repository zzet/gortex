package indexer

import (
	"context"
	"fmt"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The background owner receives this only after ordinary core/source/output
// acceptance. It captures a new independently leased selected view itself;
// the notification is not a lease, an accepted source snapshot or CAS proof.
type ContractCoreRuntimeHooks struct {
	Published func(context.Context, string, string)
}

// SetContractCoreRuntime is called before indexing starts, after the complete
// background runtime and explicit consumers have been installed.
func (mi *MultiIndexer) SetContractCoreRuntime(hooks ContractCoreRuntimeHooks) {
	mi.mu.Lock()
	defer mi.mu.Unlock()
	mi.contractCoreRuntime.Store(&hooks)
	for _, idx := range mi.indexers {
		idx.contractCoreRuntime.Store(&hooks)
	}
}

func (idx *Indexer) beginInstalledContractCoreInputs(ctx context.Context) (func(), error) {
	hooks := idx.contractCoreRuntime.Load()
	if hooks == nil || idx.contractCoreInputs != nil {
		return func() {}, nil
	}
	store, ok := idx.graph.(*store_sqlite.Store)
	if !ok || store.ViewGeneration() != 0 {
		return nil, fmt.Errorf("contract core runtime: primary mutation lacks its physical store")
	}
	backend, err := newPrimaryContractCoreStorageBackend(ctx, store, idx.repoPrefix)
	if err != nil {
		return nil, err
	}
	journal, err := newContractCoreInputJournal(ctx, backend)
	if err != nil {
		return nil, err
	}
	idx.contractCoreInputs = journal
	return func() { idx.contractCoreInputs = nil }, nil
}

func (b *SparseGenerationBuilder) SetContractCoreRuntime(hooks ContractCoreRuntimeHooks) {
	b.contractCoreRuntime.Store(&hooks)
}

func (idx *Indexer) SetContractCoreRuntime(hooks ContractCoreRuntimeHooks) {
	idx.contractCoreRuntime.Store(&hooks)
}

// Install before checkout/ref coordinator construction: those owners freeze
// their producer identity when they are created. Updating only their builder
// afterward would make new receipts carry the old, synchronous identity.
func (l *CheckoutLifecycle) SetContractCoreRuntime(hooks ContractCoreRuntimeHooks) error {
	l.refViewMu.Lock()
	defer l.refViewMu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.coordinators) != 0 || len(l.refViews) != 0 {
		return fmt.Errorf("contract core runtime: install before checkout or ref coordinators are constructed")
	}
	l.contractCoreRuntime.Store(&hooks)
	return nil
}

func (l *CheckoutLifecycle) installContractCoreBuilder(builder *SparseGenerationBuilder) *SparseGenerationBuilder {
	builder.contractCoreRuntime.Store(l.contractCoreRuntime.Load())
	return builder
}

// Called only by the outer MI output receipt after its core source acceptance.
// Raw candidate Indexers may replace the previous live pointer during the body.
func (mi *MultiIndexer) acceptInstalledContractCoreInputs(ctx context.Context, repo string) error {
	mi.mu.RLock()
	idx := mi.indexers[repo]
	mi.mu.RUnlock()
	if idx == nil || idx.contractCoreInputs == nil {
		return nil
	}
	journal := idx.contractCoreInputs
	if err := journal.accept(nil); err != nil {
		return err
	}
	idx.contractCoreInputs = nil
	if hooks := mi.contractCoreRuntime.Load(); hooks != nil && hooks.Published != nil {
		hooks.Published(ctx, repo, "")
	}
	return nil
}
