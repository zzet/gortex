package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func storeCleanupBackupEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// No daemon record may route the command away from the isolated store.
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(dir, "daemon.state.json"))
	store := filepath.Join(dir, "store.sqlite")
	prev := storeBackendPath
	storeBackendPath = store
	t.Cleanup(func() { storeBackendPath = prev })
	require.NoError(t, os.WriteFile(store, []byte("live store"), 0o600))
	return store
}

func TestStoreCleanupBackupRemovesUpgradeBackups(t *testing.T) {
	store := storeCleanupBackupEnv(t)
	v21 := store_sqlite.UpgradeBackupPath(store, 21)
	v40 := store_sqlite.UpgradeBackupPath(store, 40)
	partial := store_sqlite.UpgradeBackupPath(store, 43) + ".partial"
	for _, p := range []string{v21, v40, partial} {
		require.NoError(t, os.WriteFile(p, []byte(filepath.Base(p)), 0o600))
	}

	cmd, buf := newReposCmd()
	require.NoError(t, runStoreCleanupBackup(cmd, nil))

	require.NoFileExists(t, v21)
	require.NoFileExists(t, v40)
	require.FileExists(t, partial, "a partial copy may belong to a starting daemon")
	require.FileExists(t, store, "the live store is never touched")
	out := buf.String()
	require.Contains(t, out, v21)
	require.Contains(t, out, v40)
	require.Equal(t, 2, strings.Count(out, "removed "), out)
}

func TestStoreCleanupBackupReportsNothingToRemove(t *testing.T) {
	store := storeCleanupBackupEnv(t)
	cmd, buf := newReposCmd()
	require.NoError(t, runStoreCleanupBackup(cmd, nil))
	require.Contains(t, buf.String(), "no store backups next to "+store)
}

func TestStoreCleanupBackupFollowsTheDaemonsStore(t *testing.T) {
	storeCleanupBackupEnv(t)
	storeBackendPath = ""
	daemonStore := filepath.Join(t.TempDir(), "daemon-store.sqlite")
	require.NoError(t, daemon.WriteRuntimeState(daemon.RuntimeState{BackendPath: daemonStore}))
	require.Equal(t, daemonStore, resolveStoreBackendPath(""), "a running daemon's recorded store is the default")
	require.Equal(t, "/explicit/store.sqlite", resolveStoreBackendPath(" /explicit/store.sqlite "), "--backend-path wins")

	found, _, err := rootCmd.Find([]string{"store", "cleanup-backup"})
	require.NoError(t, err)
	require.Equal(t, storeCleanupBackupCmd, found)
	require.NotNil(t, found.Flags().Lookup("backend-path"))
}
