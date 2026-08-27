package indexer

import (
	"context"
	"fmt"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

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
		if !exists {
			for _, meta := range mi.repos {
				if meta != nil && pathkey.SamePathIdentity(meta.RootPath, absPath) {
					exists = true
					break
				}
			}
		}
		mi.mu.RUnlock()
		if exists {
			return nil
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
		if mi.configMgr != nil {
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
