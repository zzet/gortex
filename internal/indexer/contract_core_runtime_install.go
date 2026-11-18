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
	journal.logger = idx.logger
	journal.sources = idx.repositoryMutationOwner.contractAcceptedSources()
	idx.contractCoreInputs = journal
	return func() { idx.contractCoreInputs = nil }, nil
}

func (b *SparseGenerationBuilder) SetContractCoreRuntime(hooks ContractCoreRuntimeHooks) {
	// Configure before using this builder. Coordinators and ref managers take
	// their own builder snapshot, so reinstalling a caller-owned builder does
	// not change an owner's frozen producer identity or published callback.
	b.contractCoreRuntime = &hooks
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
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	if len(l.coordinators) != 0 || len(l.started) != 0 || len(l.coordinatorActivating) != 0 || len(l.refViews) != 0 || l.coordinatorClosing || l.refViewsClosed {
		return fmt.Errorf("contract core runtime: install before checkout or ref coordinators are constructed")
	}
	l.contractCoreRuntime.Store(&hooks)
	return nil
}

func (l *CheckoutLifecycle) installContractCoreBuilder(builder *SparseGenerationBuilder) *SparseGenerationBuilder {
	builder.contractCoreRuntime = l.contractCoreRuntime.Load()
	builder.contractSources = nil
	if builder.contractCoreRuntime != nil {
		builder.contractSources = l.mi.contractAcceptedSources()
	}
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

// installedContractCoreInputs is the journal the outer MI output receipt
// installed on the repository's live Indexer, or nil.
func (mi *MultiIndexer) installedContractCoreInputs(repo string) *contractCoreInputJournal {
	mi.mu.RLock()
	idx := mi.indexers[repo]
	mi.mu.RUnlock()
	if idx == nil {
		return nil
	}
	return idx.contractCoreInputs
}

// endInstalledContractCoreWrites closes the core-write windows of the files
// the outer MI output receipt's completed body began.
func (mi *MultiIndexer) endInstalledContractCoreWrites(repo string) {
	if journal := mi.installedContractCoreInputs(repo); journal != nil {
		journal.written()
	}
}

// orphanInstalledContractCoreWrites marks the core-write windows the outer
// MI output receipt's failed body left open.
func (mi *MultiIndexer) orphanInstalledContractCoreWrites(repo string) {
	if journal := mi.installedContractCoreInputs(repo); journal != nil {
		journal.orphan()
	}
}

// settleInstalledContractCoreInputs settles the receipts of a completed body
// a newer mutation overtook: they are never accepted.
func (mi *MultiIndexer) settleInstalledContractCoreInputs(repo string) {
	if journal := mi.installedContractCoreInputs(repo); journal != nil {
		journal.settle()
	}
}
