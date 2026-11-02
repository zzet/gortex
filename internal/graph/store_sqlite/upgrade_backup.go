package store_sqlite

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Pre-upgrade backups.
//
// An older binary refuses a newer store (SchemaTooNewError), so once a store
// is upgraded the only way back to the previous release is a full reindex.
// When the caller holds the store lock and the volume has room, Open first
// copies the store to <store>.v<old>.backup: a single self-contained SQLite
// file at the old schema version that the previous release opens as-is
// after it is renamed back over the store.
//
// The copy is written as <backup>.partial and renamed into place only once
// it is complete and durable, so a finalized name never holds a torn copy.
// A partial left by a crash is removed by the next upgrade attempt or by the
// daemon's start-up prune, both of which run under the store lock.

// UpgradeBackupRetention is how long the daemon keeps a pre-upgrade backup
// before its start-up prune removes it.
const UpgradeBackupRetention = 14 * 24 * time.Hour

const (
	upgradeBackupSuffix        = ".backup"
	upgradeBackupPartialSuffix = ".partial"
)

// UpgradeBackupPath names the backup Open takes of storePath before
// upgrading it from schema version.
func UpgradeBackupPath(storePath string, version int) string {
	return storePath + ".v" + strconv.Itoa(version) + upgradeBackupSuffix
}

// UpgradeBackup is one finalized pre-upgrade backup on disk.
type UpgradeBackup struct {
	Path    string
	Version int // schema version the backed-up store had
	Size    int64
	ModTime time.Time
}

// ListUpgradeBackups returns the finalized backups next to storePath,
// ordered by schema version. Interrupted partial copies are not listed.
func ListUpgradeBackups(storePath string) ([]UpgradeBackup, error) {
	backups, _, err := scanUpgradeBackups(storePath)
	return backups, err
}

// scanUpgradeBackups lists the finalized backups of storePath and the
// partial copies an interrupted backup left next to it.
func scanUpgradeBackups(storePath string) (backups []UpgradeBackup, partials []string, err error) {
	dir := filepath.Dir(storePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	prefix := filepath.Base(storePath) + ".v"
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, prefix) {
			continue
		}
		// A partial is recognised by any file of its set, so a crash that
		// removed the main copy but not its WAL still leaves it findable.
		partialSet := name
		for _, companion := range []string{"-wal", "-shm", "-journal"} {
			partialSet = strings.TrimSuffix(partialSet, companion)
		}
		partial := strings.HasSuffix(partialSet, upgradeBackupSuffix+upgradeBackupPartialSuffix)
		if partial {
			name = partialSet
		} else if !strings.HasSuffix(name, upgradeBackupSuffix) {
			continue
		}
		digits := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(name, prefix), upgradeBackupPartialSuffix), upgradeBackupSuffix)
		version, err := strconv.Atoi(digits)
		if err != nil || version <= 0 || strconv.Itoa(version) != digits {
			continue
		}
		if partial {
			if path := filepath.Join(dir, name); !slices.Contains(partials, path) {
				partials = append(partials, path)
			}
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, nil, err
		}
		backups = append(backups, UpgradeBackup{Path: filepath.Join(dir, name), Version: version, Size: info.Size(), ModTime: info.ModTime()})
	}
	slices.SortFunc(backups, func(a, b UpgradeBackup) int { return a.Version - b.Version })
	return backups, partials, nil
}

// RemoveUpgradeBackups deletes every finalized backup of storePath and
// returns the ones it removed. It leaves partial copies alone: one may be
// in the middle of being written by a daemon that holds the store lock.
func RemoveUpgradeBackups(storePath string) ([]UpgradeBackup, error) {
	backups, err := ListUpgradeBackups(storePath)
	if err != nil {
		return nil, err
	}
	var removed []UpgradeBackup
	var errs []error
	for _, b := range backups {
		if err := removeSQLiteFileSet(b.Path); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, b)
	}
	return removed, errors.Join(errs...)
}

// PruneUpgradeBackups deletes the backups of storePath older than maxAge at
// now, plus any partial copy an interrupted upgrade left behind, and returns
// the paths it removed. The caller must hold the store lock: partial copies
// are only ever written under it, so holding it proves none is in progress.
func PruneUpgradeBackups(storePath string, maxAge time.Duration, now time.Time) ([]string, error) {
	backups, partials, err := scanUpgradeBackups(storePath)
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	for _, b := range backups {
		if now.Sub(b.ModTime) <= maxAge {
			continue
		}
		if err := removeSQLiteFileSet(b.Path); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, b.Path)
	}
	for _, partial := range partials {
		if err := removeSQLiteFileSet(partial); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, partial)
	}
	return removed, errors.Join(errs...)
}

// removeSQLiteFileSet deletes a database file and the companions SQLite may
// have created next to it. A missing file is not an error.
func removeSQLiteFileSet(path string) error {
	var errs []error
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// backupBeforeUpgrade snapshots the store the pending upgrade described by
// space is about to change. It never fails Open: a backup that cannot be
// taken is skipped with the reason reported, and the upgrade proceeds.
func backupBeforeUpgrade(space upgradeSpace, observe MigrationObserver, hook upgradeHook) {
	final := UpgradeBackupPath(space.path, space.from)
	started := time.Now()
	report := func(phase MigrationPhase, detail string) {
		progress := MigrationProgress{Version: space.from, Name: "pre-upgrade backup", Phase: phase, Elapsed: time.Since(started), Detail: detail}
		if observe != nil {
			observe(progress)
			return
		}
		log.Printf("store_sqlite: upgrade backup phase=%s from=v%d to=v%d elapsed=%s detail=%q", phase, space.from, space.to, progress.Elapsed.Round(time.Millisecond), detail)
	}
	if err := removeSQLiteFileSet(final + upgradeBackupPartialSuffix); err != nil {
		report(MigrationBackupSkipped, fmt.Sprintf("could not clear an interrupted backup: %v", err))
		return
	}
	if _, err := os.Stat(final); err == nil {
		// A restart after an interrupted upgrade must not replace the
		// snapshot of the store as it was before the first attempt.
		report(MigrationBackupKept, final)
		return
	}
	copyBytes := space.stats.mainBytes + space.stats.walBytes
	if space.measured && space.available < space.estimate.Need+copyBytes {
		report(MigrationBackupSkipped, fmt.Sprintf(
			"%d bytes available cover the upgrade (%d bytes) but not also a %d-byte copy of the store",
			space.available, space.estimate.Need, copyBytes))
		return
	}
	if err := writeUpgradeBackup(space.path, final, func() { hook.at(upgradeBackupCopied, space.from) }); err != nil {
		report(MigrationBackupSkipped, fmt.Sprintf("backup failed: %v", err))
		return
	}
	report(MigrationBackupCreated, final)
}

// writeUpgradeBackup copies the quiescent store (main file plus any WAL) to
// final's partial name, folds the WAL into a single self-contained file, and
// renames it into place durably. Every path out on error removes the partial.
// copied runs once both files are in the partial copy.
func writeUpgradeBackup(storePath, final string, copied func()) (err error) {
	partial := final + upgradeBackupPartialSuffix
	defer func() {
		if err != nil {
			err = errors.Join(err, removeSQLiteFileSet(partial))
		}
	}()
	if err := copyFileDurably(storePath, partial); err != nil {
		return err
	}
	if err := copyFileDurably(storePath+"-wal", partial+"-wal"); err != nil && !os.IsNotExist(err) {
		return err
	}
	copied()
	if err := foldBackupWAL(partial); err != nil {
		return err
	}
	if err := os.Rename(partial, final); err != nil {
		return err
	}
	return syncDir(filepath.Dir(final))
}

// copyFileDurably copies src to a new file dst with src's permissions and
// fsyncs it. A missing src is reported as such before dst is created.
func copyFileDurably(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// foldBackupWAL checkpoints a copied WAL into the copy's main file and
// switches the copy out of WAL mode, so the backup is one file that stays
// complete even if it is later moved without its companions.
func foldBackupWAL(path string) error {
	db, err := openSQLiteStoreDB(sqliteDSN(path, sqliteBusyPragma))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	var busy, frames, checkpointed int
	if err := db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &checkpointed); err != nil {
		_ = db.Close()
		return fmt.Errorf("fold backup WAL: %w", err)
	}
	if busy != 0 {
		_ = db.Close()
		return errors.New("fold backup WAL: checkpoint blocked")
	}
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		_ = db.Close()
		return fmt.Errorf("fold backup WAL: %w", err)
	}
	if err := db.Close(); err != nil {
		return err
	}
	if !strings.EqualFold(mode, "delete") {
		return fmt.Errorf("fold backup WAL: journal mode stayed %q", mode)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// syncDir makes a rename in dir durable. Windows has no directory fsync;
// its rename is journaled by the filesystem instead.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
