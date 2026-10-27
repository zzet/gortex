// Package serverstack is the single construction path for Gortex's server
// stack: it builds graph.Store -> parser.Registry -> indexer -> query.Engine
// -> mcp.Server plus every side-effect init, so the daemon, the HTTP
// surface, and the one-shot embedded path all share one wiring instead of
// three near-identical copies.
//
// It lives in its own leaf package (not internal/daemon) because the
// constructor builds an *mcp.Server, and internal/mcp already imports
// internal/daemon one-way; hosting the constructor in internal/daemon
// would form the cycle daemon -> mcp -> daemon.
package serverstack

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/platform"
)

// OpenBackend constructs the graph.Store the server will run against. The
// pure-Go modernc.org/sqlite store is the only implementation: an empty
// name selects it, and it lives at the resolved path (an empty path means
// ~/.gortex/store/store.sqlite).
//
// Returns the store, a cleanup func the caller must defer (it closes the
// handle on disk), and any open error.
// allowRebuild permits rebuilding supported older schemas when necessary and
// backing an existing store up before an in-place schema upgrade; a schema
// newer than this binary is always refused and retained. The caller must
// hold the store lock. NewSharedServer passes true only after acquiring the
// exclusive flock.
func OpenBackend(name, path string, logger *zap.Logger, allowRebuild bool, observers ...store_sqlite.MigrationObserver) (graph.Store, func() error, error) {
	return openBackendWithUpgradeOption(name, path, logger, allowRebuild, nil, observers...)
}

func openBackendWithUpgradeOption(name, path string, logger *zap.Logger, allowRebuild bool, upgradeOption store_sqlite.Option, observers ...store_sqlite.MigrationObserver) (graph.Store, func() error, error) {
	if err := checkBackend(name); err != nil {
		return nil, nil, err
	}
	resolved, err := resolveBackendPath(path, "store.sqlite")
	if err != nil {
		return nil, nil, err
	}
	if logger != nil {
		logger.Info("opening backend", zap.String("backend", name), zap.String("path", resolved))
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "memory", "mem", "in-memory":
		// Unreachable while checkBackend keeps the name gated (review
		// zzet/gortex#901 R2): the dispatch stays here so the #902 wiring is
		// a one-line gate removal, and so the memory store keeps exactly one
		// production construction path.
		return openMemoryBackend(resolved, logger, observers...)
	}
	if upgradeOption == nil {
		return openSqliteBackend(resolved, allowRebuild, observers...)
	}
	return openSqliteBackendWithUpgradeOption(resolved, allowRebuild, upgradeOption, observers...)
}

// checkBackend validates a requested backend name. It is split out of
// OpenBackend so a caller that takes the cross-process store lock first can
// reject an unusable name before touching the lock or the filesystem.
func checkBackend(name string) error {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "sqlite", "sqlite3":
		return nil
	case "memory", "mem", "in-memory":
		// Tier-3 memory-resident backend (odd/tasks/tier3-memory-store.md,
		// issue davalillo/gortex#19): graph state in RAM, journaled
		// mutations, sqlite snapshot on close. The store layer is complete,
		// but the daemon does not wire it yet (#902): contract analysis,
		// checkout-lifecycle views, the dedicated base runtime, edit deltas
		// and shadow-drain retirement are SQLite-only, and daemon startup
		// aborts on the first of them ("contract analysis: SQLite store
		// required", daemon_contract_analysis.go). Offering the name today
		// selects a backend that cannot run the daemon at all, so the name
		// stays unselectable until the wiring lands (review zzet/gortex#901
		// R2). The store itself remains usable by tests and by #902 through
		// openMemoryBackend.
		return fmt.Errorf("backend %q is not selectable yet: the memory backend (also spelled mem or in-memory) is wired into the daemon in zzet/gortex#902; use --backend sqlite", name)
	default:
		return fmt.Errorf("unknown backend %q (expected: sqlite or memory)", name)
	}
}

// resolveBackendPath turns an empty backend path into a default under the
// unified store directory (~/.gortex/store/<filename>, or the XDG_DATA_HOME
// equivalent). Otherwise expands ~ and returns the absolute path, creating
// the parent directory so the on-disk store can open the leaf.
func resolveBackendPath(in, filename string) (string, error) {
	in = strings.TrimSpace(in)
	if in == "" {
		in = filepath.Join(platform.StoreDir(), filename)
	} else if strings.HasPrefix(in, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		in = filepath.Join(home, in[2:])
	}
	abs, err := filepath.Abs(in)
	if err != nil {
		return "", fmt.Errorf("abs path %q: %w", in, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("mkdir parent %q: %w", filepath.Dir(abs), err)
	}
	return abs, nil
}

// ResolveStorePath applies the same path/default/parent rules as the daemon
// stack without opening SQLite. Detached startup needs this before exec so
// SQLITE_TMPDIR is present in SQLite's process-start environment.
func ResolveStorePath(path string) (string, error) {
	return resolveBackendPath(path, "store.sqlite")
}

// openSqliteBackend opens (or creates) the SQLite store at path. The
// pure-Go modernc.org/sqlite driver keeps the binary CGo-free while still
// getting a real query planner that drives the graph's secondary indexes.
func openSqliteBackend(path string, allowRebuild bool, observers ...store_sqlite.MigrationObserver) (graph.Store, func() error, error) {
	return openSqliteBackendWithUpgradeOption(path, allowRebuild, nil, observers...)
}

func openSqliteBackendWithUpgradeOption(path string, allowRebuild bool, upgradeOption store_sqlite.Option, observers ...store_sqlite.MigrationObserver) (graph.Store, func() error, error) {
	var opts []store_sqlite.Option
	if len(observers) > 0 && observers[0] != nil {
		opts = append(opts, store_sqlite.WithMigrationObserver(observers[0]))
	}
	if allowRebuild {
		// Both need exclusive access to the store, which allowRebuild attests.
		opts = append(opts, store_sqlite.WithRebuild(), store_sqlite.WithUpgradeBackup())
	}
	if upgradeOption != nil {
		opts = append(opts, upgradeOption)
	}
	s, err := store_sqlite.Open(path, opts...)
	if err != nil {
		if errors.Is(err, store_sqlite.ErrSchemaTooNew) || errors.Is(err, store_sqlite.ErrInsufficientUpgradeSpace) {
			// Restarting another process cannot make this binary compatible,
			// nor free the disk space an upgrade needs.
			return nil, nil, fmt.Errorf("open sqlite store at %q: %w", path, err)
		}
		hint := "if another gortex daemon is using this store, stop it first (`gortex daemon status` / `gortex daemon stop`)"
		if pid, ok := daemon.RunningPID(); ok {
			hint = fmt.Sprintf("a gortex daemon is already running (pid %d) — stop it with `gortex daemon stop`, or use `gortex daemon restart`", pid)
		}
		return nil, nil, fmt.Errorf("open sqlite store at %q: %w (%s)", path, err, hint)
	}
	return s, s.Close, nil
}

// pruneStoreUpgradeBackups removes the pre-upgrade backups of storePath that
// have outlived store_sqlite.UpgradeBackupRetention at now, along with any
// partial copy an interrupted upgrade left behind. The caller must hold the
// store lock. Failures are logged, never fatal: a backup that cannot be
// removed costs disk space, not correctness.
func pruneStoreUpgradeBackups(storePath string, logger *zap.Logger, now time.Time) {
	removed, err := store_sqlite.PruneUpgradeBackups(storePath, store_sqlite.UpgradeBackupRetention, now)
	for _, path := range removed {
		logger.Info("serverstack: removed expired store upgrade backup", zap.String("path", path))
	}
	if err != nil {
		logger.Warn("serverstack: could not prune store upgrade backups", zap.String("store", storePath), zap.Error(err))
	}
}
