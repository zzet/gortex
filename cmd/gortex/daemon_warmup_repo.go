package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"go.uber.org/zap"
)

// warmupRepoAccounting points at the existing worker-loop counters. Keeping
// dispatch and its effects together makes a typed collision exit before any
// change/frontier/publication accounting in either the warm or cold branch.
type warmupRepoAccounting struct {
	changed  *atomic.Int64
	files    *atomic.Int64
	unknown  *atomic.Bool
	prefixes *sync.Map
	frontier *warmupDeltaFrontier
}

func (a warmupRepoAccounting) load(ctx context.Context, state *daemonState, entry config.RepoEntry, priorMtimes map[string]int64, logger *zap.Logger) (string, error) {
	pathFn := "track"
	var res *indexer.IndexResult
	var err error
	if priorMtimes != nil {
		pathFn = "reconcile"
		res, err = state.multiIndexer.ReconcileRepoCtx(ctx, entry, priorMtimes)
	} else {
		res, err = state.multiIndexer.TrackRepoCtx(ctx, entry)
	}
	var collision *indexer.PrefixCollisionError
	if errors.As(err, &collision) {
		if pathFn == "track" {
			logger.Warn("daemon: startup track rejected prefix collision",
				zap.String("prefix", collision.Prefix), zap.String("owner", collision.ExistingPath),
				zap.String("path", entry.Path), zap.String("action", "skipped"), zap.Error(err))
		}
		return pathFn, &indexer.RepositoryCollisionSkip{Collision: collision}
	}

	indexed := ""
	if priorMtimes != nil {
		switch {
		case err != nil:
			logger.Warn("daemon: startup reconcile failed", zap.String("path", entry.Path), zap.Error(err))
			// Other failures may have partially mutated the graph: preserve
			// the existing conservative whole-workspace invalidation.
			a.changed.Add(1)
			a.unknown.Store(true)
			a.frontier.invalidate()
		case res != nil && (res.StaleFileCount > 0 || res.DeletedFileCount > 0 || len(res.FailedFiles) > 0 || res.FullRetrack):
			a.changed.Add(1)
			a.files.Add(int64(reconcileFileCount(res)))
			a.frontier.record(res)
			indexed = warmupRepoPrefix(state, entry, res.RepoPrefix)
			if res.RepoPrefix != "" {
				a.prefixes.Store(res.RepoPrefix, struct{}{})
			} else {
				a.unknown.Store(true)
			}
		default:
			// A genuine warm no-op registered this root. A rejected root
			// already returned through the typed skip above.
			prefix := ""
			if res != nil {
				prefix = res.RepoPrefix
			}
			indexed = warmupRepoPrefix(state, entry, prefix)
		}
	} else {
		// A non-collision cold result retains the previous accounting: it
		// performed a full index or failed with potentially partial work.
		a.frontier.invalidate()
		a.changed.Add(1)
		switch {
		case err != nil:
			logger.Warn("daemon: startup track failed", zap.String("path", entry.Path), zap.Error(err))
			a.unknown.Store(true)
		case res != nil && res.RepoPrefix != "":
			a.files.Add(int64(res.FileCount))
			a.prefixes.Store(res.RepoPrefix, struct{}{})
			indexed = warmupRepoPrefix(state, entry, res.RepoPrefix)
		default:
			a.unknown.Store(true)
		}
	}
	// Scheduling does no indexing here; the existing publisher drains only
	// after readiness. A skipped root must never enqueue the owner's prefix.
	if indexed != "" {
		state.basePublisher.Schedule(indexed)
	}
	return pathFn, err
}
