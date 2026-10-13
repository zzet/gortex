package serverstack

import (
	"fmt"
	"os"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

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
func openMemoryBackend(snapshotPath string, logger *zap.Logger) (graph.Store, func() error, error) {
	store := graph.NewMemoryStore()

	// 1. Cold start from the sqlite snapshot, if one exists. A sqlite file
	// written by either engine has the same schema, so this is also the
	// engine-switch continuity path.
	if _, err := os.Stat(snapshotPath); err == nil {
		snap, closeSnap, err := openSqliteBackend(snapshotPath, false)
		if err != nil {
			return nil, nil, fmt.Errorf("memory backend: open snapshot: %w", err)
		}
		store.AddBatch(snap.AllNodes(), snap.AllEdges())
		closeSnap()
	}

	// 2. Replay the journal on top of the snapshot (or onto the empty graph).
	journalPath := memoryJournalPath(snapshotPath)
	ops, err := store.ReplayJournal(journalPath)
	if err != nil {
		return nil, nil, fmt.Errorf("memory backend: replay journal: %w", err)
	}
	if logger != nil && ops > 0 {
		logger.Info("memory backend: journal replayed", zap.Int("ops", ops))
	}

	// 3. Attach the journal for this session.
	if err := store.OpenJournal(journalPath); err != nil {
		return nil, nil, fmt.Errorf("memory backend: open journal: %w", err)
	}
	if logger != nil {
		logger.Info("memory backend: open",
			zap.String("snapshot", snapshotPath),
			zap.String("journal", journalPath))
	}

	closeFn := func() error {
		if err := snapshotMemoryStore(store, snapshotPath); err != nil {
			if logger != nil {
				logger.Error("memory backend: snapshot dump failed", zap.Error(err))
			}
		} else if logger != nil {
			logger.Info("memory backend: snapshot dumped",
				zap.String("snapshot", snapshotPath))
		}
		if err := store.CloseJournal(); err != nil && logger != nil {
			logger.Warn("memory backend: journal close", zap.Error(err))
		}
		return nil
	}
	return store, closeFn, nil
}

// snapshotMemoryStore dumps the in-memory graph into a fresh sqlite file and
// atomically renames it over the snapshot path; on success the journal is
// reset (the snapshot subsumes every mutation journaled so far).
func snapshotMemoryStore(store *graph.MemoryStore, snapshotPath string) error {
	tmp := snapshotPath + ".snapshot.tmp"
	_ = os.Remove(tmp)
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
	closeSnap()
	if err := os.Rename(tmp, snapshotPath); err != nil {
		return fmt.Errorf("memory backend: publish snapshot: %w", err)
	}
	return store.ResetJournal()
}
