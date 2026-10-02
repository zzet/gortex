package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	sqlite "modernc.org/sqlite"
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

func sqlitePerConnectionPragmas() string {
	return fmt.Sprintf("%s&_pragma=mmap_size(%d)", sqlitePerConnectionPragmasBase, sqliteMmapBytes())
}

// defaultSQLiteWALAutoCheckpointPages is the application-owned WAL pressure
// line: 8k 4-KiB pages is about 32 MiB. SQLite's commit hook is deliberately
// disabled on every writer connection because it charges an unbounded WAL copy
// to whichever graph or catalog transaction happens to cross the line. The
// checkpoint loop polls the WAL file cheaply and runs the existing bounded
// PASSIVE attempt in the background once this line is exceeded; generation
// finalization additionally schedules its bounded TRUNCATE drain.
//
// Wider spacing is not free: WAL-resident pages bypass the main-file mmap and
// make the eventual drain larger. The historical three-phase seeded benchmark
// found 8k pages kept the write-side win (~12%) with flat read-back, while 100k
// pages regressed read-back by 46%. journal_size_limit still truncates the file
// after a successful checkpoint.
const defaultSQLiteWALAutoCheckpointPages = 8000

// sqliteWALAutoCheckpointPages resolves the application pressure line.
// GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES overrides the 8,000-page default;
// zero disables pressure-triggered attempts while leaving the ordinary
// periodic/final checkpoints intact. Unparseable or negative input fails open
// to the default. The historical name and environment variable are retained
// so existing measurements keep selecting the same boundary.
func sqliteWALAutoCheckpointPages() int {
	raw := strings.TrimSpace(os.Getenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES"))
	if raw == "" {
		return defaultSQLiteWALAutoCheckpointPages
	}
	pages, err := strconv.Atoi(raw)
	if err != nil || pages < 0 {
		return defaultSQLiteWALAutoCheckpointPages
	}
	return pages
}

func sqliteWriterDSN(path string) string {
	// IMMEDIATE reserves the single SQLite writer at BEGIN. It avoids the
	// un-retryable DEFERRED read-to-write promotion/BUSY_SNAPSHOT class.
	// wal_autocheckpoint stays off on every replacement physical connection;
	// the bounded checkpoint loop owns WAL copying outside commit latency.
	params := sqliteBusyPragma + "&_pragma=journal_mode(WAL)&" +
		sqlitePerConnectionPragmas() + "&_pragma=journal_size_limit(67108864)" +
		"&_pragma=wal_autocheckpoint(0)&_txlock=immediate"
	return sqliteDSN(path, params)
}

type sqliteWALFileState struct {
	size         int64
	modTimeNanos int64
}

func readSQLiteWALFileState(path string) (sqliteWALFileState, bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return sqliteWALFileState{}, false, nil
	}
	if err != nil {
		return sqliteWALFileState{}, false, err
	}
	return sqliteWALFileState{size: info.Size(), modTimeNanos: info.ModTime().UnixNano()}, true, nil
}

// sqliteWALFallbackPath converts a supported explicit file: URI into the path
// os.Stat needs when SQLite cannot answer PRAGMA database_list inside the
// bounded startup budget. A successful probe remains authoritative. Invalid or
// remote-authority URIs are returned unchanged; Open would reject those before
// the checkpoint loop starts.
func sqliteWALFallbackPath(path string) string {
	if !strings.HasPrefix(path, "file:") {
		return path
	}
	parsed, err := url.Parse(path)
	if err != nil || parsed.Scheme != "file" || (parsed.Host != "" && parsed.Host != "localhost") {
		return path
	}
	candidate := parsed.Path // url.Parse already decodes an ordinary URI path.
	if candidate == "" && parsed.Opaque != "" {
		candidate, err = url.PathUnescape(parsed.Opaque)
		if err != nil {
			return path
		}
	}
	if candidate == "" {
		return path
	}
	candidate = filepath.FromSlash(candidate)
	if os.PathSeparator == '\\' && len(candidate) >= 3 &&
		(candidate[0] == '\\' || candidate[0] == '/') && candidate[2] == ':' {
		candidate = candidate[1:]
	}
	return candidate
}

// sqliteWALPressureTarget resolves the real main-database filename (including
// URI opens) and converts a frame count into the exact WAL byte boundary for
// the database's actual page size. Both PRAGMA probes share one bounded startup
// budget so an exhausted read pool cannot keep Store.Close waiting forever.
// Failed probes retain a conservative 4-KiB page fallback; a stat failure later
// causes more checkpointing, never less.
func sqliteWALPressureTarget(db *sql.DB, fallbackPath string, pages int) (string, int64) {
	path := sqliteWALFallbackPath(fallbackPath)
	var ctx context.Context
	var cancel context.CancelFunc
	if db != nil {
		ctx, cancel = context.WithTimeout(context.Background(), walPassiveCheckpointTimeout)
		defer cancel()
		rows, err := db.QueryContext(ctx, `PRAGMA database_list`)
		if err == nil {
			mainFile := ""
			for rows.Next() {
				var seq int
				var name, file string
				if err := rows.Scan(&seq, &name, &file); err == nil && name == "main" && file != "" {
					mainFile = file
					break
				}
			}
			// A failed iteration keeps the fallback path rather than trusting a
			// partially read database list.
			if rows.Err() == nil && mainFile != "" {
				path = mainFile
			}
			_ = rows.Close()
		}
	}
	// SQLite's Windows VFS may preserve a drive path's 8.3 spelling in
	// database_list. Resolve the existing parent so WAL pressure/reclaim use
	// the same physical path as an ordinary long-name open of that database.
	if runtime.GOOS == "windows" {
		if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
			path = filepath.Join(dir, filepath.Base(path))
		}
	}
	if pages <= 0 {
		return path + "-wal", 0
	}
	pageSize := int64(4096)
	if db != nil {
		var observed int64
		if err := db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&observed); err == nil && observed > 0 {
			pageSize = observed
		}
	}
	const walHeaderBytes int64 = 32
	frameBytes := pageSize + 24
	maxInt64 := int64(^uint64(0) >> 1)
	if int64(pages) > (maxInt64-walHeaderBytes)/frameBytes {
		return path + "-wal", maxInt64
	}
	return path + "-wal", walHeaderBytes + int64(pages)*frameBytes
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
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
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

// openSQLiteReadPool opens the bounded read pool. When gate is non-nil every
// physical connection is wrapped so the start of each read passes through it
// (see sqliteReadGate); the WAL reclaim closes that gate for a bounded moment
// so a TRUNCATE checkpoint can reset a log no pool reader is holding.
func openSQLiteReadPool(path string, gate *sqliteReadGate) (*sql.DB, error) {
	var db *sql.DB
	if gate == nil {
		opened, err := sql.Open("sqlite", sqliteReaderDSN(path))
		if err != nil {
			return nil, err
		}
		db = opened
	} else {
		connector, err := sqlite.NewConnector(sqliteReaderDSN(path))
		if err != nil {
			return nil, err
		}
		db = sql.OpenDB(gatedConnector{inner: connector, gate: gate})
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
