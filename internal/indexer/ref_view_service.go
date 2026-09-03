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
	if manager, cached := l.refViews[repoPrefix]; cached {
		return manager, nil
	}
	index := config.Default().Index
	if l.cfgMgr != nil {
		index = l.cfgMgr.GetRepoConfig(repoPrefix).Index
	}
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
		Logger: l.logger,
		Gate:   l.buildGate(),
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
