package serverstack

import (
	"fmt"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The memory backend's FTS engine is a full store_sqlite.Store on a
// ":memory:" DSN (issue davalillo/gortex#21): the engine answers FTS ranking
// with the sqlite backend's own SQL, so symbol-search parity between the
// backends is structural, and every FTS byte stays in RAM. The engine only
// ever holds the FTS sidecars — bundle bodies come from the resident graph.
var _ graph.FTSEngine = (*store_sqlite.Store)(nil)

// The memory backend (odd/tasks/tier3-memory-store.md, issue
// davalillo/gortex#19) is a memory-resident graph.Store: primary state lives
// in a graph.Graph, every mutation is appended to a JSONL journal, and a
// sqlite snapshot dumped through the bulk loader is the durability boundary.
//
// The snapshot path is the SAME store file the sqlite backend uses. That is
// deliberate: both engines are serialized by the same cross-process store
// flock, a memory-mode shutdown leaves the file in exactly the schema a
// sqlite-mode boot expects, and vice versa a memory-mode cold start can load
// whatever a sqlite session last persisted. Switching engines is then just a
// restart with a different --backend.

// memoryJournalPath sits beside the snapshot: <snapshot>.journal.
func memoryJournalPath(snapshotPath string) string {
	return snapshotPath + ".journal"
}

// openMemoryBackend builds the memory-resident backend: cold start loads the
// sqlite snapshot (if any), replays the journal on top, and returns a store
// whose close dumps a fresh snapshot and resets the journal.
func openMemoryBackend(snapshotPath string, logger *zap.Logger, observers ...store_sqlite.MigrationObserver) (store graph.Store, closeFn func(), err error) {
	// 0. The in-RAM FTS engine (issue #21). Derived state: not journaled,
	// not persisted; rebuilt from resident graph state by the warmup FTS
	// step and closed with the backend.
	engine, err := store_sqlite.Open(":memory:")
	if err != nil {
		return nil, nil, fmt.Errorf("memory backend: open fts engine: %w", err)
	}
	// Any error return below must not leak the engine (issue
	// davalillo/gortex#25 A5): the store holds no cleanup chain until the
	// final return, so the defer owns the engine until then.
	defer func() {
		if err != nil {
			_ = engine.Close()
		}
	}()
	// The snapshot generation (issue davalillo/gortex#23) comes from the
	// epoch sidecar beside the snapshot; 0 on a fresh store or a missing
	// sidecar. The journal header pairs against it at replay time.
	epochPath := memoryEpochPath(snapshotPath)
	loadedEpoch := readJournalEpoch(epochPath)
	mstore := graph.NewMemoryStore(graph.WithFTSEngine(engine), graph.WithJournalEpoch(loadedEpoch))

	// 1. Cold start from the sqlite snapshot, if one exists. A sqlite file
	// written by either engine has the same schema, so this is also the
	// engine-switch continuity path. The file-mtime sidecar (issue
	// davalillo/gortex#26) rides along: without it the next warm boot sees
	// an empty delta frontier and re-reconciles the whole corpus. The
	// snapshot open takes the migration observers so schema work on the
	// shared store file stays observable on the memory path too (issue
	// davalillo/gortex#25 A7).
	if _, err := os.Stat(snapshotPath); err == nil {
		var snap graph.Store
		var closeSnap func()
		snap, closeSnap, err = openSqliteBackend(snapshotPath, false, observers...)
		if err != nil {
			err = fmt.Errorf("memory backend: open snapshot: %w", err)
			return nil, nil, err
		}
		// Construction is single-goroutine by contract, but the cold load
		// still runs under the store's write lock (issue #25 A8).
		mstore.ColdLoadBatch(snap.AllNodes(), snap.AllEdges())
		restoreFileMtimes(mstore, snap)
		closeSnap()
	}

	// 2. Replay the journal on top of the snapshot (or onto the empty graph).
	journalPath := memoryJournalPath(snapshotPath)
	ops, rerr := mstore.ReplayJournal(journalPath)
	if rerr != nil {
		err = fmt.Errorf("memory backend: replay journal: %w", rerr)
		return nil, nil, err
	}
	unknownOps, corruptMidFile, corruptLine, skippedStale, epochLagged := mstore.ReplayAnomalies()
	if logger != nil && (ops > 0 || unknownOps > 0 || corruptMidFile || skippedStale || epochLagged) {
		logger.Info("memory backend: journal replayed",
			zap.Int("ops", ops),
			zap.Int("unknown_ops", unknownOps),
			zap.Bool("corrupt_mid_file", corruptMidFile),
			zap.Int("corrupt_line", corruptLine),
			zap.Bool("skipped_stale_journal", skippedStale),
			zap.Bool("epoch_sidecar_lagged", epochLagged))
	}

	// 3. Attach the journal for this session.
	if err := mstore.OpenJournal(journalPath); err != nil {
		err = fmt.Errorf("memory backend: open journal: %w", err)
		return nil, nil, err
	}
	// A replay skipped as stale leaves the old journal attached with a
	// header from a superseded generation — start it fresh so new mutations
	// journal under the loaded snapshot's generation (issue
	// davalillo/gortex#23).
	if skippedStale {
		if err := mstore.ResetJournal(); err != nil && logger != nil {
			logger.Warn("memory backend: stale journal reset failed", zap.Error(err))
		}
	}
	if logger != nil {
		logger.Info("memory backend: open",
			zap.String("snapshot", snapshotPath),
			zap.String("journal", journalPath),
			zap.Uint64("epoch", mstore.JournalEpoch()))
	}

	// Periodic snapshot triggers (issue davalillo/gortex#23): janitor-style
	// ticker, never on the mutation hot path. The dump itself pauses
	// mutation traffic for its duration via the store's write barrier —
	// that is what keeps "journal ⊆ snapshot" provable for the epoch check.
	var dumpMu sync.Mutex
	snapshotAndRotate := func(reason string) {
		dumpErr := mstore.SnapshotBarrier(func() error {
			return snapshotMemoryStore(mstore, snapshotPath)
		})
		if dumpErr != nil {
			if logger != nil {
				logger.Error("memory backend: snapshot dump failed",
					zap.String("reason", reason), zap.Error(dumpErr))
			}
			// The journal is the only durable record of mutations the
			// failed snapshot never captured — it must survive (issue
			// davalillo/gortex#25). Skip reset AND the epoch sidecar write:
			// the next boot then replays the journal against the previous
			// snapshot generation as usual.
			return
		}
		// Reset only after a successful dump (issue davalillo/gortex#25 A6):
		// its failure must not masquerade as a dump failure.
		if resetErr := mstore.ResetJournal(); resetErr != nil {
			if logger != nil {
				logger.Warn("memory backend: journal reset after snapshot failed",
					zap.String("reason", reason), zap.Error(resetErr))
			}
			return
		}
		// Publish the sidecar for the generation the reset just started.
		// A crash before this write leaves the sidecar one generation
		// behind: the next boot's replay runs (the safe direction — pre-
		// dump ops re-apply idempotently, post-dump ops are still needed).
		if epochErr := writeJournalEpoch(epochPath, mstore.JournalEpoch()); epochErr != nil && logger != nil {
			logger.Warn("memory backend: epoch sidecar write failed", zap.Error(epochErr))
		}
		if logger != nil {
			logger.Info("memory backend: snapshot dumped",
				zap.String("snapshot", snapshotPath),
				zap.String("reason", reason),
				zap.Uint64("epoch", mstore.JournalEpoch()))
		}
	}

	stopped := make(chan struct{})
	var stopOnce sync.Once
	cfg := snapshotTriggerConfig{interval: 30 * time.Second}
	cfg.loadFromEnv(logger)
	go func() {
		ticker := time.NewTicker(cfg.tick)
		defer ticker.Stop()
		var lastDump = time.Now() // the loaded snapshot is fresh as of open
		for {
			select {
			case <-stopped:
				return
			case <-ticker.C:
			}
			reason := cfg.triggerReason(time.Since(lastDump), int64(mstore.JournalBytes()))
			if reason == "" {
				continue
			}
			// A close in progress owns dumpMu; it dumps anyway.
			if !dumpMu.TryLock() {
				continue
			}
			lastDump = time.Now()
			snapshotAndRotate(reason)
			dumpMu.Unlock()
		}
	}()

	closeFn = func() {
		stopOnce.Do(func() { close(stopped) })
		dumpMu.Lock()
		snapshotAndRotate("close")
		if err := mstore.CloseJournal(); err != nil && logger != nil {
			logger.Warn("memory backend: journal close", zap.Error(err))
		}
		if err := engine.Close(); err != nil && logger != nil {
			logger.Warn("memory backend: fts engine close", zap.Error(err))
		}
	}
	return mstore, closeFn, nil
}

// snapshotMemoryStore dumps the in-memory graph into a fresh sqlite file and
// atomically renames it over the snapshot path. The caller owns the journal
// reset: a successful rename means the snapshot is published even if the
// follow-up reset fails, and the two failures must not be conflated (issue
// davalillo/gortex#25 A6).
func snapshotMemoryStore(store *graph.MemoryStore, snapshotPath string) error {
	tmp := snapshotPath + ".snapshot.tmp"
	_ = os.Remove(tmp)
	published := false
	// A failed dump must not leave the tmp file behind (issue
	// davalillo/gortex#25 A4): the next successful dump removes it anyway,
	// but between the two the stale file is dead weight next to the live
	// snapshot.
	defer func() {
		if !published {
			_ = os.Remove(tmp)
		}
	}()
	snap, closeSnap, err := openSqliteBackend(tmp, true)
	if err != nil {
		return fmt.Errorf("memory backend: open snapshot target: %w", err)
	}
	bl, hasBulk := snap.(graph.BulkLoader)
	if hasBulk {
		bl.BeginBulkLoad()
	}
	snap.AddBatch(store.AllNodes(), store.AllEdges())
	if hasBulk {
		if err := bl.FlushBulk(); err != nil {
			closeSnap()
			return fmt.Errorf("memory backend: snapshot bulk flush: %w", err)
		}
	}
	// Persist the file-mtime sidecar (issue davalillo/gortex#26). The
	// sqlite snapshot file already carries the file_mtimes table — the
	// bulk loader just never writes it. ReplaceFileMtimes is the right
	// primitive: the memory map IS the authoritative set, and its
	// per-prefix replace prunes rows for files that no longer exist.
	// Empty input is a no-op inside the replacer, so a repo with no rows
	// cannot wipe anything.
	replacer, hasReplace := snap.(graph.FileMtimeReplacer)
	if hasReplace {
		for _, prefix := range store.FileMtimePrefixes() {
			if err := replacer.ReplaceFileMtimes(prefix, store.LoadFileMtimes(prefix)); err != nil {
				closeSnap()
				return fmt.Errorf("memory backend: snapshot mtimes for %q: %w", prefix, err)
			}
		}
	}
	closeSnap()
	if err := os.Rename(tmp, snapshotPath); err != nil {
		return fmt.Errorf("memory backend: publish snapshot: %w", err)
	}
	published = true
	return nil
}

// restoreFileMtimes copies the snapshot's per-repo file-mtime rows into the
// memory store (issue davalillo/gortex#26). The snapshot store implements
// FileMtimeReader and answers its own mtime-holding prefixes via
// FileMtimePrefixes (sidecar-derived, not node-derived). Best-effort by
// contract: a partial restore only costs one extra reconcile on the next
// boot — the same safe-direction degradation as a crash between snapshots.
func restoreFileMtimes(store *graph.MemoryStore, snap graph.Store) {
	reader, hasRead := snap.(graph.FileMtimeReader)
	if !hasRead {
		return
	}
	lister, hasPrefixes := snap.(interface{ FileMtimePrefixes() []string })
	if !hasPrefixes {
		return
	}
	for _, prefix := range lister.FileMtimePrefixes() {
		mtimes := reader.LoadFileMtimes(prefix)
		if len(mtimes) == 0 {
			continue
		}
		_ = store.BulkSetFileMtimes(prefix, mtimes)
	}
}
