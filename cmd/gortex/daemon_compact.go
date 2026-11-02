package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/platform"
)

// Guarded one-time store compaction at daemon boot.
//
// Row-shedding maintenance (orphan-repo purges, the duplicate-collapse
// migration, resolver cleanups) returns pages to SQLite's freelist, where new
// writes reuse them — but only VACUUM returns them to the filesystem, and
// nothing ran it: a live store carried 4.4 GB of freelist inside a 6.8 GB
// file. This is not a correctness problem (freelist pages are fully
// reusable), so the trigger is deliberately conservative: compaction runs
// only when the dead fraction dominates the file AND is large in absolute
// terms AND the filesystem can absorb VACUUM's temporary full copy. Anything
// smaller is left to organic reuse.

// storeCompactor is the optional store capability the boot-time compaction
// probes for. It is a type assertion rather than a method on graph.Store so
// that stores with nothing to reclaim — the indexer's in-process staging
// graph, test fixtures — simply do not implement it.
type storeCompactor interface {
	CompactStats() (freeBytes, totalBytes int64)
	CompactContext(context.Context) error
	Path() string
}

const (
	// compactMinFreeBytes is the absolute floor: below 1 GiB of reclaimable
	// space a VACUUM (minutes of exclusive I/O on a store this size) costs
	// more than the disk it returns.
	compactMinFreeBytes = int64(1) << 30
)

// shouldCompactStore is the pure trigger predicate: compact only when the
// freelist is BOTH the majority of the file (> 50% of pages — organic reuse
// would take a long time to grow back into that much dead space) AND large in
// absolute terms (> 1 GiB — a small file's majority is still cheap to leave
// alone), AND the filesystem has room for VACUUM's transient full copy
// (available > 1.5 × the current file, headroom over the worst case of the
// rewrite temporarily doubling the footprint). Pure so the policy is
// table-testable without a store or a filesystem.
func shouldCompactStore(freeBytes, totalBytes int64, diskAvailBytes uint64) bool {
	if totalBytes <= 0 || freeBytes <= 0 {
		return false
	}
	if freeBytes*2 <= totalBytes { // freelist must exceed half the pages
		return false
	}
	if freeBytes <= compactMinFreeBytes {
		return false
	}
	need := uint64(totalBytes) + uint64(totalBytes)/2 // total × 1.5
	return diskAvailBytes > need
}

// maybeCompactStore probes the store for the compaction capability and runs
// a one-time VACUUM when shouldCompactStore says the file is mostly dead
// space. Production calls the context-aware form only after the entire warmup
// producer has joined and enrichment is published. Pages reused by indexing
// therefore reduce the measured freelist before this decision.
func maybeCompactStore(g graph.Store, logger *zap.Logger) {
	maybeCompactStoreContext(context.Background(), g, false, logger)
}

func maybeCompactStoreContext(ctx context.Context, g graph.Store, migrated bool, logger *zap.Logger) {
	if ctx.Err() != nil {
		return
	}
	if migrated {
		logger.Info("daemon: store compaction deferred to next boot", zap.String("reason", "after_migration"))
		return
	}
	if os.Getenv("GORTEX_SKIP_STORE_COMPACT") == "1" {
		logger.Debug("daemon: store compaction disabled (GORTEX_SKIP_STORE_COMPACT=1)", zap.String("reason", "disabled"))
		return
	}
	c, ok := g.(storeCompactor)
	if !ok {
		return // memory-mode store: nothing on disk to compact
	}
	path := c.Path()
	if path == "" {
		return // no file backing — no filesystem to reason about
	}
	free, total := c.CompactStats()
	avail, err := platform.DiskAvailBytes(filepath.Dir(path))
	if err != nil {
		// Unknown headroom means the ×1.5 guard cannot hold; VACUUM without it
		// risks filling the volume mid-rewrite, so skip.
		logger.Debug("daemon: store compaction skipped — disk headroom unknown",
			zap.String("reason", "disk_headroom_unknown"), zap.String("path", path), zap.Error(err))
		return
	}
	if !shouldCompactStore(free, total, avail) {
		logger.Debug("daemon: store compaction not warranted",
			zap.String("reason", "below_threshold_after_warmup"),
			zap.Int64("reclaimable_bytes", free),
			zap.Int64("store_bytes", total),
			zap.Uint64("disk_avail_bytes", avail))
		return
	}
	logger.Info("daemon: one-time store compaction starting — VACUUM may take minutes on a multi-GB store",
		zap.Int64("reclaimable_bytes", free),
		zap.Int64("store_bytes", total),
		zap.Uint64("disk_avail_bytes", avail))
	start := time.Now()
	if err := c.CompactContext(ctx); err != nil {
		// A deferral is not a failure and must not be logged as one: the store
		// was mid-publish or mid-build, the maintenance lane declined to
		// rewrite the file underneath it, and the freelist is still there to
		// reclaim at the next boot. Distinguishing the two is what keeps the
		// warning meaningful — an operator seeing it should be able to trust
		// that VACUUM actually tried and lost.
		if errors.Is(err, store_sqlite.ErrMaintenanceBusy) {
			logger.Info("daemon: store compaction deferred — the store was busy",
				zap.String("reason", "busy"), zap.Duration("elapsed", time.Since(start)), zap.Error(err))
			return
		}
		if errors.Is(err, store_sqlite.ErrVacuumTempDirectory) {
			logger.Info("daemon: store compaction skipped — temporary volume unconfirmed",
				zap.String("reason", "temp_directory_unconfirmed"), zap.Error(err))
			return
		}
		if ctx.Err() != nil {
			logger.Info("daemon: store compaction canceled", zap.Error(err))
			return
		}
		// Non-fatal by design: a failed VACUUM leaves the store exactly as it
		// was (the freelist remains reusable), so boot continues.
		logger.Warn("daemon: store compaction failed — continuing boot",
			zap.Duration("elapsed", time.Since(start)), zap.Error(err))
		return
	}
	_, after := c.CompactStats()
	logger.Info("daemon: store compaction finished",
		zap.Duration("elapsed", time.Since(start)),
		zap.Int64("store_bytes", after),
		zap.Int64("reclaimed_bytes", total-after))
}

// scheduleDaemonCompaction admits at most one owned task. Even the warmup's
// deferred memory-release tail finishes before it measures or rewrites the
// store. Cancellation and joining use the existing startup owner, so teardown
// cannot close the backend beneath the dedicated connection.
func scheduleDaemonCompaction(task, warmup *daemonWarmup, state *daemonState, logger *zap.Logger) bool {
	return task.start(func(ctx context.Context) {
		warmup.wait()
		maybeCompactStoreContext(ctx, state.graph, state.migratedStore, logger)
	})
}

func joinDaemonCompaction(task *daemonWarmup, logger *zap.Logger) {
	started := time.Now()
	logger.Info("daemon: shutdown phase start", zap.String("phase", "post_warmup_compaction"))
	task.wait()
	logger.Info("daemon: shutdown phase complete", zap.String("phase", "post_warmup_compaction"), zap.Duration("elapsed", time.Since(started)))
}
