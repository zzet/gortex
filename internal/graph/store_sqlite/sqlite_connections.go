package store_sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// modernc applies every _pragma entry when each physical connection opens.
// busy_timeout must be first so it is installed before the writer's one-time
// rollback-journal to WAL transition can need a lock. WAL itself is established
// only by the writer; readers inherit the persistent journal mode and never try
// to change it while other connections are active.
var sqliteBusyPragma = fmt.Sprintf("_pragma=busy_timeout(%d)", sqliteBusyTimeoutMillis)

const sqlitePerConnectionPragmasBase = "_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=cache_size(-32768)&_pragma=temp_store(MEMORY)"

// defaultSQLiteMmapBytes is the historical 256 MiB mmap window. On a 2.2GB
// store the resolver's guard measured 41% of its CPU in pread syscalls —
// every page touch beyond this window pays a syscall even when the OS cache
// is warm — so the window is operator-tunable for measurement and large-
// workspace deployments.
const defaultSQLiteMmapBytes = 268435456

// sqliteMmapBytes resolves the per-connection mmap window: GORTEX_SQLITE_MMAP_MB
// overrides the 256 MiB default (0 disables mmap entirely — a legitimate
// SQLite mode); unparseable or negative input fails open to the default.
// Read once per store open via the DSN builders, so every physical
// connection — writer, each reader, and the bulk connection drawn from the
// writer pool — carries the same window.
func sqliteMmapBytes() int64 {
	raw := strings.TrimSpace(os.Getenv("GORTEX_SQLITE_MMAP_MB"))
	if raw == "" {
		return defaultSQLiteMmapBytes
	}
	mb, err := strconv.Atoi(raw)
	if err != nil || mb < 0 {
		return defaultSQLiteMmapBytes
	}
	// Saturate absurd requests at 4 TiB so the shift cannot wrap negative;
	// SQLite additionally clamps to its compile-time maximum.
	if mb > 1<<22 {
		mb = 1 << 22
	}
	return int64(mb) << 20
}

// sqliteStableReadPoolMode is an operational escape hatch for hosts where
// SQLite's WAL-index shared-memory mapping is unreliable while the read pool
// sheds physical connections. It retains every bounded read connection once
// opened while preserving the concurrency required by store iterators whose
// callbacks perform nested reads.
func sqliteStableReadPoolMode() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GORTEX_SQLITE_STABLE_READ_POOL"))) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

func sqlitePerConnectionPragmas() string {
	return fmt.Sprintf("%s&_pragma=mmap_size(%d)", sqlitePerConnectionPragmasBase, sqliteMmapBytes())
}

// sqliteWALAutoCheckpointPages spaces WAL auto-checkpoints at 8k pages
// (~32 MiB). The SQLite default of 1000 pages (~4 MB) forces a checkpoint —
// frames copied into the main file plus an mmap-view flush — every few MB of
// a scattered write burst, so index-write bursts spend a measurable share of
// their time in flush calls rather than row writes. Wider spacing is NOT
// free, though: pages resident in a large WAL are served to readers by
// wal-index probe + pread instead of the main file's mmap, and the deferred
// frames come due as one big drain — at 100k pages the three-phase seeded
// bench shows read-back +46% and a 330 ms drain, a net production loss.
// 8k pages keeps the write-side win (~12%) with flat read-back and
// negligible drain. journal_size_limit still truncates the WAL after every
// checkpoint; the indexer checkpoints TRUNCATE at the end of each global
// batch, at Compact and at Close. Readers never append to the WAL, so only
// the writer DSN carries the override.
const sqliteWALAutoCheckpointPages = 8000

func sqliteWriterDSN(path string) string {
	// IMMEDIATE reserves the single SQLite writer at BEGIN. It avoids the
	// un-retryable DEFERRED read-to-write promotion/BUSY_SNAPSHOT class.
	params := sqliteBusyPragma + "&_pragma=journal_mode(WAL)&" +
		sqlitePerConnectionPragmas() + "&_pragma=journal_size_limit(67108864)" +
		fmt.Sprintf("&_pragma=wal_autocheckpoint(%d)", sqliteWALAutoCheckpointPages) +
		"&_txlock=immediate"
	return sqliteDSN(path, params)
}

func sqliteReaderDSN(path string) string {
	// mode=ro is a SQLite URI parameter, so sqliteDSN must emit a real file:
	// URI rather than a plain filename followed by a query string. TEMP remains
	// writable, while the persistent main database is physically read-only.
	return sqliteDSN(path, "mode=ro&"+sqliteBusyPragma+"&"+sqlitePerConnectionPragmas())
}

func sqliteDSN(path, rawQuery string) string {
	// Preserve explicit URI and in-memory callers. Their existing query string
	// may carry cache=shared/mode=memory, so append rather than replace it.
	if isMemoryPath(path) || strings.HasPrefix(path, "file:") {
		separator := "?"
		if strings.Contains(path, "?") {
			separator = "&"
		}
		return path + separator + rawQuery
	}

	// Turn a filesystem path into an absolute escaped SQLite URI. This prevents
	// spaces, '#', and '?' in valid filenames from being interpreted as URI
	// syntax and makes SQLite honor mode=ro on the reader connection.
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	slashed := filepath.ToSlash(path)
	if !strings.HasPrefix(slashed, "/") {
		// A Windows drive-letter path ("C:/...") must become the URI path
		// "/C:/..." so url.URL renders file:///C:/... — without the leading
		// slash it renders file:C:/... and SQLite reads "C:" as a URI
		// authority ("invalid uri authority: C:"), so the daemon cannot open
		// a fresh store on Windows at all.
		slashed = "/" + slashed
	}
	escaped := &url.URL{Scheme: "file", Path: slashed, RawQuery: rawQuery}
	return escaped.String()
}

func configureWriterPool(db *sql.DB) {
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
}

func openSQLiteReadPool(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteReaderDSN(path))
	if err != nil {
		return nil, err
	}
	configureConnectionPool(db)
	// Force one physical connection now. This catches an invalid/per-connection
	// pragma while Open can still close both handles without leaking one.
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func closeSQLitePools(readDB, writerDB *sql.DB) error {
	switch {
	case readDB == nil && writerDB == nil:
		return nil
	case readDB == nil:
		return writerDB.Close()
	case writerDB == nil || readDB == writerDB:
		return readDB.Close()
	default:
		return errors.Join(readDB.Close(), writerDB.Close())
	}
}
