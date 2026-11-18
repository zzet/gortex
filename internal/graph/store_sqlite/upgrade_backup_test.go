package store_sqlite

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type migrationEvents struct {
	mu     sync.Mutex
	events []MigrationProgress
}

func (m *migrationEvents) observe(p MigrationProgress) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, p)
}

func (m *migrationEvents) phases() []MigrationPhase {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MigrationPhase, 0, len(m.events))
	for _, e := range m.events {
		out = append(out, e.Phase)
	}
	return out
}

func (m *migrationEvents) only(t *testing.T, phase MigrationPhase) MigrationProgress {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	var found []MigrationProgress
	for _, e := range m.events {
		if e.Phase == phase {
			found = append(found, e)
		}
	}
	require.Len(t, found, 1, "want exactly one %s event", phase)
	return found[0]
}

// backupSnapshot reads a backup read-only, the way the previous release
// would see it after it is renamed back over the store.
type backupSnapshot struct {
	version, nodes, edges int
	integrity, journal    string
}

func readBackupSnapshot(t *testing.T, path string) backupSnapshot {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(path, "mode=ro"))
	require.NoError(t, err)
	defer db.Close()
	var snap backupSnapshot
	require.NoError(t, db.QueryRow("PRAGMA user_version").Scan(&snap.version))
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&snap.nodes))
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM edges").Scan(&snap.edges))
	require.NoError(t, db.QueryRow("PRAGMA integrity_check").Scan(&snap.integrity))
	require.NoError(t, db.QueryRow("PRAGMA journal_mode").Scan(&snap.journal))
	return snap
}

func graphCounts(t *testing.T, path string) (nodes, edges int) {
	t.Helper()
	withRawDB(t, path, func(db *sql.DB) {
		require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&nodes))
		require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM edges").Scan(&edges))
	})
	return nodes, edges
}

func requireNoPartialBackup(t *testing.T, path string) {
	t.Helper()
	_, partials, err := scanUpgradeBackups(path)
	require.NoError(t, err)
	require.Empty(t, partials, "no partial copy may outlive Open")
}

func TestUpgradeBackupCreatedWhenSpaceAllows(t *testing.T) {
	path := seedOlderStore(t, 40, 400)
	nodes, edges := graphCounts(t, path)
	var events migrationEvents

	s, err := Open(path, WithRebuild(), WithUpgradeBackup(), WithMigrationObserver(events.observe), withDiskAvail(fixedDiskAvail(1<<50)))
	require.NoError(t, err)
	require.NoError(t, s.Close())

	backup := UpgradeBackupPath(path, 40)
	created := events.only(t, MigrationBackupCreated)
	require.Equal(t, backup, created.Detail)
	require.Equal(t, 40, created.Version)
	require.Equal(t, MigrationBackupCreated, events.phases()[0], "the backup precedes the first step")
	require.Contains(t, events.phases(), MigrationStarted)

	snap := readBackupSnapshot(t, backup)
	require.Equal(t, backupSnapshot{version: 40, nodes: nodes, edges: edges, integrity: "ok", journal: "delete"}, snap)
	for _, companion := range []string{"-wal", "-shm"} {
		require.NoFileExists(t, backup+companion, "the backup is a single self-contained file")
	}
	requireNoPartialBackup(t, path)
	require.Equal(t, currentSchemaVersion, rawUserVersion(t, path), "the upgrade still ran")

	listed, err := ListUpgradeBackups(path)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, backup, listed[0].Path)
	require.Equal(t, 40, listed[0].Version)
}

func TestUpgradeBackupSkippedWhenSpaceCoversOnlyMigration(t *testing.T) {
	path := seedOlderStore(t, 40, 400)
	probe, err := probeStoreSchema(path)
	require.NoError(t, err)
	space, pending := measureUpgradeSpace(probe, currentSchemaVersion, planSchemaMigrationWith(40, currentSchemaVersion, schemaMigrations), fixedDiskAvail(0))
	require.True(t, pending)
	available := space.estimate.Need + 1 // enough to upgrade, not to copy as well
	var events migrationEvents

	s, err := Open(path, WithRebuild(), WithUpgradeBackup(), WithMigrationObserver(events.observe), withDiskAvail(fixedDiskAvail(available)))
	require.NoError(t, err)
	require.NoError(t, s.Close())

	skipped := events.only(t, MigrationBackupSkipped)
	require.Contains(t, skipped.Detail, strconv.FormatUint(available, 10)+" bytes available", "the log line says why")
	require.Contains(t, skipped.Detail, "copy")
	require.NoFileExists(t, UpgradeBackupPath(path, 40))
	requireNoPartialBackup(t, path)
	require.Equal(t, currentSchemaVersion, rawUserVersion(t, path), "a skipped backup does not stop the upgrade")
}

func TestUpgradeBackupRequiresOption(t *testing.T) {
	path := seedOlderStore(t, 40, 50)
	var events migrationEvents
	s, err := Open(path, WithMigrationObserver(events.observe), withDiskAvail(fixedDiskAvail(1<<50)))
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoFileExists(t, UpgradeBackupPath(path, 40), "only a caller holding the store lock asks for a backup")
	require.NotContains(t, events.phases(), MigrationBackupSkipped)
	require.Equal(t, currentSchemaVersion, rawUserVersion(t, path))
}

func TestUpgradeBackupKeepsExistingSnapshot(t *testing.T) {
	path := seedOlderStore(t, 40, 50)
	backup := UpgradeBackupPath(path, 40)
	require.NoError(t, os.WriteFile(backup, []byte("the first attempt's snapshot"), 0o600))
	require.NoError(t, os.WriteFile(backup+upgradeBackupPartialSuffix, []byte("torn"), 0o600))
	var events migrationEvents

	s, err := Open(path, WithRebuild(), WithUpgradeBackup(), WithMigrationObserver(events.observe), withDiskAvail(fixedDiskAvail(1<<50)))
	require.NoError(t, err)
	require.NoError(t, s.Close())

	require.Equal(t, backup, events.only(t, MigrationBackupKept).Detail)
	got, err := os.ReadFile(backup)
	require.NoError(t, err)
	require.Equal(t, "the first attempt's snapshot", string(got), "a retried upgrade must not replace the pre-upgrade snapshot")
	requireNoPartialBackup(t, path)
}

func TestUpgradeBackupFoldsLeftoverWAL(t *testing.T) {
	path := seedOlderStore(t, 43, 100)
	// Stamp the old version through a connection that never checkpoints, the
	// way a crashed process leaves it: the version and the newest rows exist
	// only in the -wal file.
	db, err := openSQLiteStoreDB(sqliteWriterDSN(path))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO nodes (id, kind, name, file_path, repo_prefix) SELECT id || '#wal', kind, name, file_path, repo_prefix FROM nodes`)
	require.NoError(t, err)
	require.NoError(t, setUserVersion(db, 40))
	require.NoError(t, db.Close())
	info, err := os.Stat(path + "-wal")
	require.NoError(t, err)
	require.Positive(t, info.Size(), "the fixture must leave committed frames in the WAL")
	probe, err := probeStoreSchema(path)
	require.NoError(t, err)
	require.Equal(t, 40, probe.stored)
	var events migrationEvents

	s, err := Open(path, WithRebuild(), WithUpgradeBackup(), WithMigrationObserver(events.observe), withDiskAvail(fixedDiskAvail(1<<50)))
	require.NoError(t, err)
	require.NoError(t, s.Close())

	backup := UpgradeBackupPath(path, 40)
	require.Equal(t, backup, events.only(t, MigrationBackupCreated).Detail)
	snap := readBackupSnapshot(t, backup)
	require.Equal(t, 40, snap.version, "the version held only in the WAL is in the backup")
	require.Equal(t, 200, snap.nodes, "rows held only in the WAL are in the backup")
	require.Equal(t, "ok", snap.integrity)
	require.Equal(t, "delete", snap.journal)
	require.NoFileExists(t, backup+"-wal")
}

func TestPruneUpgradeBackupsRemovesOnlyExpired(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "store.sqlite")
	now := time.Now()
	write := func(name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(name), 0o600))
		require.NoError(t, os.Chtimes(p, now.Add(-age), now.Add(-age)))
		return p
	}
	expired := write("store.sqlite.v21.backup", 15*24*time.Hour)
	recent := write("store.sqlite.v40.backup", 5*24*time.Hour)
	partial := write("store.sqlite.v40.backup.partial", time.Minute)
	partialWAL := write("store.sqlite.v40.backup.partial-wal", time.Minute)
	orphanWAL := write("store.sqlite.v39.backup.partial-wal", time.Minute)
	unrelated := []string{
		write("store.sqlite.bak", 30*24*time.Hour),
		write("other.sqlite.v21.backup", 30*24*time.Hour),
		write("store.sqlite.vX.backup", 30*24*time.Hour),
		write("store.sqlite", 30*24*time.Hour),
	}

	removed, err := PruneUpgradeBackups(store, UpgradeBackupRetention, now)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{expired, partial, filepath.Join(dir, "store.sqlite.v39.backup.partial")}, removed)
	for _, gone := range []string{expired, partial, partialWAL, orphanWAL} {
		require.NoFileExists(t, gone)
	}
	require.FileExists(t, recent, "a backup inside the retention window is kept")
	for _, keep := range unrelated {
		require.FileExists(t, keep)
	}
}

func TestRemoveUpgradeBackupsLeavesPartialCopies(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "store.sqlite")
	for _, name := range []string{"store.sqlite.v21.backup", "store.sqlite.v40.backup", "store.sqlite.v43.backup.partial"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600))
	}

	removed, err := RemoveUpgradeBackups(store)
	require.NoError(t, err)
	require.Len(t, removed, 2)
	require.Equal(t, []int{21, 40}, []int{removed[0].Version, removed[1].Version})
	require.NoFileExists(t, filepath.Join(dir, "store.sqlite.v21.backup"))
	require.NoFileExists(t, filepath.Join(dir, "store.sqlite.v40.backup"))
	require.FileExists(t, filepath.Join(dir, "store.sqlite.v43.backup.partial"), "a partial may be in progress under another daemon's lock")

	removed, err = RemoveUpgradeBackups(store)
	require.NoError(t, err)
	require.Empty(t, removed)
}
