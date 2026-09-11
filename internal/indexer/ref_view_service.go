package indexer

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The lifecycle's ref-view entry point.
//
// A ref view needs three things a selector does not carry: the repository the
// selector resolves against, the namespace its payload is stamped in, and the
// index configuration it is built under. All three belong to the repository
// the graph serves, and the lifecycle is what already knows them — it builds a
// checkout coordinator out of exactly the same parts. Routing selections
// through here is what keeps a ref view's payload in the same namespace and
// under the same rules as the corpus it composes over.

// RefViewSelection is one request for a view of committed state, as a caller
// that only knows the selector can express it.
type RefViewSelection struct {
	// GraphID is the dedicated graph whose corpus the view composes over.
	GraphID string
	// SelectorKind and SelectorValue are the committed state to pin.
	SelectorKind  gitstate.ViewSelectorKind
	SelectorValue string
	// EnrichmentProfile is how deeply the view is enriched. Empty takes the
	// default profile.
	EnrichmentProfile string
}

// EnsureRefView makes one ref view current and reports what serving it reads.
//
// The manager is cached per repository prefix, not per selection: it holds no
// per-request state, and building one costs a configuration digest and an
// extractor fingerprint that would otherwise be recomputed on every selection.
func (l *CheckoutLifecycle) EnsureRefView(ctx context.Context, sel RefViewSelection) (RefViewResult, error) {
	if l == nil || l.store == nil || l.catalog == nil {
		return RefViewResult{}, fmt.Errorf("indexer: this daemon serves no ref views")
	}
	ownerRead, err := l.AcquireRepositoryRead(sel.GraphID)
	if err != nil {
		return RefViewResult{}, err
	}
	defer ownerRead.Release()
	dedicated, found, err := l.catalog.GetDedicatedGraph(ctx, sel.GraphID)
	if err != nil {
		return RefViewResult{}, err
	}
	if !found || dedicated.RepoPrefix == "" {
		return RefViewResult{}, fmt.Errorf("indexer: graph %s serves no repository", sel.GraphID)
	}
	idx := l.mi.GetIndexer(dedicated.RepoPrefix)
	if idx == nil {
		return RefViewResult{}, fmt.Errorf("indexer: repository %s is not served yet", dedicated.RepoPrefix)
	}
	repoDir := idx.RootPath()
	if repoDir == "" {
		return RefViewResult{}, fmt.Errorf("indexer: repository %s has no root path", dedicated.RepoPrefix)
	}
	manager, err := l.refViewManager(dedicated.RepoPrefix, idx)
	if err != nil {
		return RefViewResult{}, err
	}
	return manager.EnsureRefView(ctx, RefViewRequest{
		GraphID:           sel.GraphID,
		SelectorKind:      sel.SelectorKind,
		SelectorValue:     sel.SelectorValue,
		RepoDir:           repoDir,
		EnrichmentProfile: sel.EnrichmentProfile,
		RepoPrefix:        dedicated.RepoPrefix,
		WorkspaceID:       idx.WorkspaceID(),
		ProjectID:         idx.ProjectID(),
	})
}

// RefViewGeneration reads the generation a ref view currently serves, and the
// state it is in. It is the read a caller makes when it wants to serve what is
// already published rather than drive a new selection — the older generation a
// building view can still answer from.
func (l *CheckoutLifecycle) RefViewGeneration(ctx context.Context, refViewID string) (store_sqlite.RefView, bool, error) {
	if l == nil || l.catalog == nil || refViewID == "" {
		return store_sqlite.RefView{}, false, nil
	}
	return l.catalog.GetRefView(ctx, refViewID)
}

// refViewManager returns the cached manager for one repository, building it on
// first use from the same index configuration and parser registry the
// repository's own coordinator builds with.
func (l *CheckoutLifecycle) refViewManager(repoPrefix string, idx *Indexer) (*RefViewManager, error) {
	l.refViewMu.Lock()
	defer l.refViewMu.Unlock()
	if _, closing := l.closingRefViews[repoPrefix]; l.refViewsClosed || closing {
		return nil, ErrRefViewManagerClosed
	}
	if manager, cached := l.refViews[repoPrefix]; cached {
		return manager, nil
	}
	repoCfg := config.Default()
	if l.cfgMgr != nil {
		repoCfg = l.cfgMgr.GetRepoConfig(repoPrefix)
	}
	index := repoCfg.Index
	manager, err := NewRefViewManager(RefViewManagerConfig{
		Store: l.store,
		Builder: &SparseGenerationBuilder{
			Store:      l.store,
			Registry:   l.mi.registry,
			Config:     index,
			Logger:     l.logger,
			Admissions: idx,
			Embedder:   l.mi.embedder,
		},
		Config: index,
		// The two inputs a ref view's generation identity needs to be a real
		// claim rather than a degraded one. The lifecycle holds both — it hands
		// a checkout coordinator exactly the same pair — and a ref view
		// composes over the same corpus under the same rules.
		//
		// Leases is what moves the revision: without it
		// dependencyCohortSource.describe refuses before it reads anything, so
		// EVERY ref-view generation carried the degraded revision.
		//
		// The configuration sections are passed as a SOURCE rather than as a
		// value, and that is the load-bearing part. A manager is cached per
		// repository for the life of the daemon while a reload swaps that
		// repository's whole config.Config underneath it, so a list frozen here
		// would keep keying generations on the artifacts / semantic / LSP /
		// workspace / project domains as they stood when some first selection
		// happened to build this manager — and a view built after the reload
		// would reuse a generation produced under rules that no longer apply.
		// repoConfigSections re-reads the ConfigManager on each description, so
		// a configuration change re-keys. The frozen list stays as the fallback
		// for a lifecycle with no ConfigManager at all, where the source
		// answers nothing and an empty section list would otherwise collapse
		// the widened digest back onto config.IndexConfig alone.
		//
		// Safe to certify here only because the manager's memo now refreshes:
		// the lifecycle invalidates it on owner registration, registry teardown
		// and configuration reload, a membership change is self-observed
		// through the topology token, and a refusal is never cached. The
		// sibling HEAD/tree source is still the git watcher's to wire.
		ConfigSections:    dedicatedBaseConfigSections(repoCfg),
		ConfigSectionsFor: l.repoConfigSections,
		Leases:            l.leases,
		Logger:            l.logger,
		Gate:              l.buildGate(),
	})
	if err != nil {
		return nil, err
	}
	if l.refViews == nil {
		l.refViews = map[string]*RefViewManager{}
	}
	l.refViews[repoPrefix] = manager
	return manager, nil
}

// repoConfigSections renders one repository's output-affecting configuration
// domains as they stand NOW.
//
// This is the source a cached ref-view manager describes its cohort through,
// so the answer follows a configuration reload instead of the manager's
// construction. It reads the same ConfigManager the reload refreshed
// (MultiIndexer.RefreshRepoConfigs re-reads each repository's `.gortex.yaml`
// into it), which is what makes ApplyReload's invalidation produce a new
// digest rather than the same one recomputed.
//
// nil for a lifecycle with no ConfigManager: the caller then keeps the list it
// was constructed with.
func (l *CheckoutLifecycle) repoConfigSections(repoPrefix string) []DependencyRevisionConfigSection {
	if l == nil || l.cfgMgr == nil || repoPrefix == "" {
		return nil
	}
	return dedicatedBaseConfigSections(l.cfgMgr.GetRepoConfig(repoPrefix))
}
