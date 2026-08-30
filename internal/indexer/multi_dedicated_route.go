package indexer

import (
	"context"
	"fmt"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

// RebindRouteOwnedRepoRoot converges only the process-local shell of a
// dedicated checkout after `git worktree move`. It never walks source files,
// parses code, mutates graph payload, or publishes a view generation. The
// durable graph/route remain authoritative throughout.
//
// The checkout's catalog root is re-read inside the stable repository lane.
// That makes targetRoot a compare-and-set token for runtime state too: a
// delayed A -> B repair cannot overwrite a shell already converged to C.
// Missing shells are restored from the committed dedicated route, which is
// the restart-recovery path after a crash between catalog observation and
// process convergence.
func (mi *MultiIndexer) RebindRouteOwnedRepoRoot(
	ctx context.Context,
	checkoutID, repoPrefix, targetRoot string,
) (bool, error) {
	if mi == nil || checkoutID == "" || repoPrefix == "" || targetRoot == "" {
		return false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	targetRoot = pathkey.CanonicalExistingRoot(targetRoot)
	if err := mi.requireRouteOwnedCheckoutRoot(ctx, checkoutID, repoPrefix, targetRoot); err != nil {
		return false, err
	}

	mi.mu.RLock()
	meta := mi.repos[repoPrefix]
	idx := mi.indexers[repoPrefix]
	mi.mu.RUnlock()
	if meta == nil || idx == nil {
		// Direct move repair must not replace an arbitrary process slot. Startup
		// registration has its own durable-route check and may reuse a stale
		// non-route-owned automatic shell, but a missing-shell rebind has no
		// provenance proving that a different prefix is disposable.
		mi.mu.RLock()
		occupiedBy := ""
		for existingPrefix, existingMeta := range mi.repos {
			if existingPrefix != repoPrefix && existingMeta != nil &&
				pathkey.SamePathIdentity(existingMeta.RootPath, targetRoot) {
				occupiedBy = existingPrefix
				break
			}
		}
		mi.mu.RUnlock()
		if occupiedBy != "" {
			return false, fmt.Errorf(
				"%w: repository shell %s already occupies %s",
				store_sqlite.ErrCatalogStaleGuard, occupiedBy, targetRoot,
			)
		}
		cfg := config.Default()
		if mi.configMgr != nil {
			mi.configMgr.LoadWorkspaceConfig(repoPrefix, targetRoot)
			cfg = mi.configMgr.GetRepoConfig(repoPrefix)
		}
		identity, err := DetectIdentity(targetRoot)
		if err != nil {
			return false, fmt.Errorf("detecting moved repository identity for %s: %w", targetRoot, err)
		}
		_, restored, err := mi.restoreRouteOwnedRepoCtx(
			ctx,
			config.RepoEntry{Path: targetRoot, Name: repoPrefix},
			targetRoot,
			repoPrefix,
			cfg,
			identity,
			nil,
			false,
		)
		if err != nil {
			return false, err
		}
		if restored {
			mi.callRepoTrackedHook(repoPrefix, targetRoot)
		}
		mi.mu.RLock()
		installedMeta := mi.repos[repoPrefix]
		installedIndexer := mi.indexers[repoPrefix]
		mi.mu.RUnlock()
		if installedMeta == nil || installedIndexer == nil ||
			installedMeta.RepoPrefix != repoPrefix ||
			!pathkey.EqualPaths(
				pathkey.CanonicalExistingRoot(installedMeta.RootPath), targetRoot,
			) {
			return false, fmt.Errorf(
				"%w: route-owned shell %s was not restored at %s",
				store_sqlite.ErrCatalogStaleGuard, repoPrefix, targetRoot,
			)
		}
		return restored, nil
	}

	if pathkey.EqualPaths(
		pathkey.CanonicalExistingRoot(meta.RootPath), targetRoot,
	) {
		return false, nil
	}
	coordinator, current := mi.repositoryMutationCoordinatorForSnapshot(repoPrefix, meta, idx)
	if !current || coordinator == nil {
		return false, fmt.Errorf("%w: repository shell %s moved while rebinding",
			store_sqlite.ErrCatalogStaleGuard, repoPrefix)
	}
	oldRoot := meta.RootPath
	err := coordinator.runExclusiveLaneOnly(ctx, func() error {
		if err := mi.requireRouteOwnedCheckoutRoot(ctx, checkoutID, repoPrefix, targetRoot); err != nil {
			return err
		}
		mi.mu.Lock()
		defer mi.mu.Unlock()
		if mi.repos[repoPrefix] != meta || mi.indexers[repoPrefix] != idx {
			return fmt.Errorf("%w: repository shell %s was replaced",
				store_sqlite.ErrCatalogStaleGuard, repoPrefix)
		}
		next := *meta
		next.RootPath = targetRoot
		if meta.Identity != nil {
			identity := *meta.Identity
			identity.FilePath = targetRoot
			if pathkey.EqualPaths(identity.CanonicalID, oldRoot) {
				identity.CanonicalID = targetRoot
			}
			next.Identity = &identity
		}
		idx.SetRootPath(targetRoot)
		mi.repos[repoPrefix] = &next
		return nil
	})
	if err != nil {
		return false, err
	}
	if mi.configMgr != nil {
		mi.configMgr.LoadWorkspaceConfig(repoPrefix, targetRoot)
	}
	mi.callRepoTrackedHook(repoPrefix, targetRoot)
	if mi.semanticMgr != nil {
		mi.semanticMgr.CheckoutWorkspaces().EvictRoot(oldRoot)
	}
	return true, nil
}

func (mi *MultiIndexer) requireRouteOwnedCheckoutRoot(
	ctx context.Context,
	checkoutID, repoPrefix, targetRoot string,
) error {
	store, ok := mi.graph.(*store_sqlite.Store)
	if !ok || store == nil {
		return fmt.Errorf("indexer: dedicated route store is unavailable")
	}
	catalog := store.Catalog()
	graph, found, err := catalog.GetDedicatedGraph(ctx, GraphIDFor(repoPrefix))
	if err != nil {
		return err
	}
	if !found || graph.OwnerCheckoutID != checkoutID || graph.RepoPrefix != repoPrefix {
		return fmt.Errorf("%w: dedicated graph %s no longer owns checkout %s",
			store_sqlite.ErrCatalogStaleGuard, repoPrefix, checkoutID)
	}
	checkout, found, err := catalog.GetCheckout(ctx, checkoutID)
	if err != nil {
		return err
	}
	if !found || !pathkey.EqualPaths(
		pathkey.CanonicalExistingRoot(checkout.RootPath), targetRoot,
	) {
		return fmt.Errorf("%w: checkout %s no longer names root %s",
			store_sqlite.ErrCatalogStaleGuard, checkoutID, targetRoot)
	}
	owned, err := mi.routeOwnsDedicatedCorpus(ctx, repoPrefix)
	if err != nil {
		return err
	}
	if !owned {
		return fmt.Errorf("%w: checkout %s has no committed dedicated route",
			store_sqlite.ErrCatalogStaleGuard, checkoutID)
	}
	return nil
}

func (mi *MultiIndexer) callRepoTrackedHook(repoPrefix, root string) {
	mi.mu.RLock()
	hook := mi.onRepoTracked
	mi.mu.RUnlock()
	if hook != nil {
		hook(repoPrefix, root)
	}
}

// routeOwnsDedicatedCorpus is the durable boundary between the legacy mutable
// repository corpus and a checkout-owned immutable base. Catalog errors fail
// closed: a transient control-plane read must never authorize a filesystem
// reindex over an already-published dedicated base.
func (mi *MultiIndexer) routeOwnsDedicatedCorpus(ctx context.Context, repoPrefix string) (bool, error) {
	if mi == nil || mi.graph == nil || repoPrefix == "" {
		return false, nil
	}
	store, ok := mi.graph.(*store_sqlite.Store)
	if !ok {
		return false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	owned, err := store.Catalog().OwnsActiveDedicatedRoute(ctx, GraphIDFor(repoPrefix), repoPrefix)
	if err != nil {
		return false, fmt.Errorf("checking dedicated route for %s: %w", repoPrefix, err)
	}
	return owned, nil
}

// restoreRouteOwnedRepoCtx rebuilds only the process-local repository shell.
// The graph payload and immutable base marker already live in SQLite, so a warm
// restart must not walk or parse the checkout filesystem. The bool is false
// when the route moved while this call waited for the stable repository lane;
// callers then continue through their ordinary mutable path.
func (mi *MultiIndexer) restoreRouteOwnedRepoCtx(
	ctx context.Context,
	entry config.RepoEntry,
	absPath, prefix string,
	cfg *config.Config,
	identity *RepoIdentity,
	priorMtimes map[string]int64,
	persistConfig bool,
) (*IndexResult, bool, error) {
	idx := mi.newPerRepoIndexerForMutation(ctx, cfg.Index)
	idx.SetRepoPrefix(prefix)
	entryCopy := entry
	idx.SetWorkspaceID(resolveWorkspaceID(&entryCopy, cfg, prefix))
	idx.SetProjectID(resolveProjectID(&entryCopy, cfg, prefix))
	idx.SetRootPath(absPath)
	idx.SetFileMtimes(priorMtimes)

	var result *IndexResult
	installed := false
	restored := false
	// Shell restoration is the other safe route-owned mutation beside exact
	// Git-tree indexing: it installs process state only and never touches payload.
	err := mi.coordinateRepositoryTopologyMutation(
		authorizeImmutableRepositoryMutation(ctx), idx, func() error {
			mi.reapplyBatchModeForMutation(idx)
			finishTopologyMutation := mi.beginRepositoryTopologyMutation(ctx)
			topologyChanged := false
			defer func() { finishTopologyMutation(topologyChanged) }()

			mi.mu.RLock()
			_, exists := mi.repos[prefix]
			conflictingPrefixes := make([]string, 0, 1)
			if !exists {
				for existingPrefix, meta := range mi.repos {
					if meta != nil && pathkey.SamePathIdentity(meta.RootPath, absPath) {
						conflictingPrefixes = append(conflictingPrefixes, existingPrefix)
					}
				}
			}
			mi.mu.RUnlock()
			if exists {
				return nil
			}
			for _, existingPrefix := range conflictingPrefixes {
				owned, err := mi.routeOwnsDedicatedCorpus(ctx, existingPrefix)
				if err != nil {
					return err
				}
				if owned {
					return fmt.Errorf(
						"%w: route-owned shell %s already occupies %s",
						store_sqlite.ErrCatalogStaleGuard, existingPrefix, absPath,
					)
				}
			}

			owned, err := mi.routeOwnsDedicatedCorpus(ctx, prefix)
			if err != nil {
				return err
			}
			if !owned {
				return nil
			}

			result = &IndexResult{RepoPrefix: prefix}
			mi.mu.Lock()
			mi.repos[prefix] = &RepoMetadata{
				RepoPrefix:    prefix,
				RootPath:      absPath,
				Identity:      identity,
				LastIndexTime: time.Now(),
				FileMtimes:    idx.publishFileMtimes(),
				IsWorktree:    ResolveWorktree(absPath).IsWorktree,
			}
			mi.indexers[prefix] = idx
			mi.mu.Unlock()
			installed = true
			restored = true
			topologyChanged = true

			entry.Path = absPath
			if persistConfig && mi.configMgr != nil {
				if err := mi.configMgr.Global().AddRepo(entry); err != nil {
					mi.logger.Warn("failed to add route-owned repo to config")
				}
			}
			return nil
		})
	if !installed {
		idx.Close()
	}
	if err != nil {
		return nil, false, err
	}
	return result, restored, nil
}
