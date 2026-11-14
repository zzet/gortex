package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
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

func (a warmupRepoAccounting) load(ctx context.Context, state *daemonState, entry config.RepoEntry, priorMtimes map[string]int64, deferrable bool, logger *zap.Logger) (string, error) {
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
			if deferrable && warmupWALRetryable(ctx, err) {
				logger.Info("daemon: startup reconcile refused by WAL pressure; deferred to serial retry",
					zap.String("path", entry.Path), zap.Error(err))
			} else {
				logger.Warn("daemon: startup reconcile failed", zap.String("path", entry.Path), zap.Error(err))
			}
			// A failed reconcile may have partially mutated the graph: preserve
			// the existing conservative whole-workspace invalidation. A WAL
			// refusal needs it too, even when the retry succeeds: the chunks
			// before the refusal committed and persisted their mtimes, so the
			// retry's census sees them as clean, while their pending-enrichment
			// markers and derived invalidations died with the first attempt.
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

// warmupLoadRepo is the per-repository warmup load. A package variable only so
// the in-package cases can inject a refusal; production never assigns it.
// deferrable says the caller will retry a WAL refusal, so load logs it as
// deferred rather than failed.
var warmupLoadRepo = func(ctx context.Context, a warmupRepoAccounting, state *daemonState, entry config.RepoEntry, priorMtimes map[string]int64, deferrable bool, logger *zap.Logger) (string, error) {
	return a.load(ctx, state, entry, priorMtimes, deferrable, logger)
}

// warmupPriorMtimes reads a repository's prior mtimes for its warmup load.
// The backend's FileMtime sidecar table is the sole source. An empty result
// means the store has never seen this repo, which is a cold index rather than
// a reconcile with nothing to do, so it is normalised to nil and the full walk
// runs. A backend that crossed a schema-rebuild migration rung (NeedsRebuild)
// has on-disk rows in the old shape that an incremental reconcile cannot fix,
// so prior mtimes are dropped and every file re-indexes into the new schema.
func warmupPriorMtimes(state *daemonState, entry config.RepoEntry, logger *zap.Logger) map[string]int64 {
	priorMtimes := priorMtimesFromStore(state.graph, state.configManager, entry, logger)
	if len(priorMtimes) == 0 {
		priorMtimes = nil
	}
	if storeNeedsRebuild(state.graph) {
		if len(priorMtimes) > 0 {
			logger.Info("daemon: backend signalled schema rebuild; forcing full re-index",
				zap.String("path", entry.Path))
		}
		priorMtimes = nil
	}
	return priorMtimes
}

// warmupWALRetryable reports a load the warmup WAL boundary refused while the
// warmup itself was still live: the reclaim could not reach its mark because
// of the other parallel loads, not because this repository is broken.
func warmupWALRetryable(batchCtx context.Context, err error) bool {
	return err != nil && batchCtx.Err() == nil && errors.Is(err, store_sqlite.ErrWarmupWALPressure)
}

// warmupWALRetries collects repositories to retry after the parallel phase,
// each with the refusal that deferred it.
type warmupWALRetries struct {
	mu      sync.Mutex
	entries []warmupWALRetry
}

type warmupWALRetry struct {
	entry   config.RepoEntry
	refusal error
}

func (r *warmupWALRetries) add(entry config.RepoEntry, refusal error) {
	r.mu.Lock()
	r.entries = append(r.entries, warmupWALRetry{entry: entry, refusal: refusal})
	r.mu.Unlock()
}

// run retries each collected repository once, serially, so no other warmup
// load competes with it for the WAL. Once the warmup is cancelled, each
// repository not yet retried is passed to skip with its refusal instead, so
// its load state says why it never loaded.
func (r *warmupWALRetries) run(ctx context.Context, retry func(config.RepoEntry), skip func(config.RepoEntry, error)) {
	r.mu.Lock()
	entries := r.entries
	r.entries = nil
	r.mu.Unlock()
	for _, e := range entries {
		if ctx.Err() != nil {
			skip(e.entry, fmt.Errorf("deferred WAL retry skipped, warmup ended: %w", e.refusal))
			continue
		}
		retry(e.entry)
	}
}
