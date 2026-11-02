package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/platform"
)

// storeBackendPath is the graph store `gortex store` subcommands act on.
// Empty defers to resolveStoreBackendPath. Set by --backend-path.
var storeBackendPath string

var storeCmd = &cobra.Command{
	Use:   "store",
	Short: "Maintain the on-disk graph store",
}

var storeCleanupBackupCmd = &cobra.Command{
	Use:   "cleanup-backup",
	Short: "Remove the store backups taken before schema upgrades",
	Long: `Removes the backups the daemon takes of the graph store before it upgrades
the store's schema. Each sits next to the store as
<store>.v<old schema version>.backup and is the rollback path to the release
that wrote it; the daemon also removes backups older than 14 days when it
starts.

The store is located in this order: --backend-path if given, else the store a
running daemon recorded at startup, else ~/.gortex/store/store.sqlite.
A partial copy (*.backup.partial) is left alone, since a starting daemon may
be writing it; the next daemon start removes an abandoned one.`,
	Args: cobra.NoArgs,
	RunE: runStoreCleanupBackup,
}

func init() {
	storeCleanupBackupCmd.Flags().StringVar(&storeBackendPath, "backend-path", "",
		"remove the backups of this store file (default: the running daemon's recorded store, else ~/.gortex/store/store.sqlite)")
	storeCmd.AddCommand(storeCleanupBackupCmd)
	rootCmd.AddCommand(storeCmd)
}

func runStoreCleanupBackup(cmd *cobra.Command, _ []string) error {
	storePath := resolveStoreBackendPath(storeBackendPath)
	removed, err := store_sqlite.RemoveUpgradeBackups(storePath)
	out := cmd.OutOrStdout()
	for _, backup := range removed {
		fmt.Fprintf(out, "removed %s (schema v%d, %s)\n", backup.Path, backup.Version, formatBytes(uint64(backup.Size)))
	}
	if err != nil {
		return fmt.Errorf("remove store backups of %s: %w", storePath, err)
	}
	if len(removed) == 0 {
		fmt.Fprintf(out, "no store backups next to %s\n", storePath)
	}
	return nil
}

// resolveStoreBackendPath locates the graph store an out-of-band command
// reads or maintains: the explicit path if given, else the store a running
// daemon recorded at startup (so a daemon started with --backend-path is
// followed without repeating the flag), else the platform default.
func resolveStoreBackendPath(explicit string) string {
	if path := strings.TrimSpace(explicit); path != "" {
		return path
	}
	if st, ok := daemon.ReadRuntimeState(); ok && st.BackendPath != "" {
		return st.BackendPath
	}
	return filepath.Join(platform.StoreDir(), "store.sqlite")
}
