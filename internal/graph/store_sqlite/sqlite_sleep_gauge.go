package store_sqlite

import (
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// Time spent asleep inside SQLite.
//
// SQLite waits without using the CPU in exactly two ways, and both go through
// the VFS's xSleep:
//   - the busy handler (busy_timeout): after SQLITE_BUSY on a lock, it sleeps
//     1, 2, 5, 10, 15, 20, 25, 25, 25, 50, 50, 100… ms, i.e. whole
//     milliseconds, until the timeout (5 s on every pooled connection);
//   - the WAL read-lock retry loop (walTryBeginRead): a reader that cannot
//     take or set a read mark (a checkpointer or a log restart holds the
//     marks it needs, or the wal-index header keeps changing under it) retries
//     up to 100 times, sleeping 1 µs and then (n−9)²×39 µs — never a whole
//     millisecond — for up to about 10 s before failing with SQLITE_PROTOCOL.
//
// Neither is visible to the store's other gauges: the read gate and the pool
// wait end before SQLite runs, block reads count only I/O, and the thread is
// off the CPU. So every registered VFS's xSleep is wrapped once per process
// and the time is counted, split by the delay's shape. The counters are
// process-wide: they cover every connection of every store in the process.

type sqliteSleepCounters struct {
	busyNanos, busyCalls         atomic.Int64
	walRetryNanos, walRetryCalls atomic.Int64
	maxNanos                     atomic.Int64
	installed                    atomic.Bool
}

var sqliteSleeps sqliteSleepCounters

// sqliteSleepOriginal maps a VFS pointer to its original xSleep.
var sqliteSleepOriginal sync.Map // uintptr → uintptr

var sqliteSleepInstallOnce sync.Once

// sqliteSleepWrapper is the xSleep every VFS calls once installed. It must stay
// reachable for the life of the process: SQLite holds its address.
var sqliteSleepWrapper = func(tls *libc.TLS, pVfs uintptr, nMicro int32) int32 {
	orig, _ := sqliteSleepOriginal.Load(pVfs)
	start := time.Now()
	var r int32
	if fp, ok := orig.(uintptr); ok && fp != 0 {
		r = (*(*func(*libc.TLS, uintptr, int32) int32)(unsafe.Pointer(&struct{ uintptr }{fp})))(tls, pVfs, nMicro)
	} else {
		time.Sleep(time.Duration(nMicro) * time.Microsecond)
		r = nMicro
	}
	d := int64(time.Since(start))
	if nMicro > 0 && nMicro%1000 == 0 {
		sqliteSleeps.busyNanos.Add(d)
		sqliteSleeps.busyCalls.Add(1)
	} else {
		sqliteSleeps.walRetryNanos.Add(d)
		sqliteSleeps.walRetryCalls.Add(1)
	}
	for {
		cur := sqliteSleeps.maxNanos.Load()
		if d <= cur || sqliteSleeps.maxNanos.CompareAndSwap(cur, d) {
			break
		}
	}
	return r
}

// funcPointer is the C function pointer modernc's generated code calls for a
// Go func value: the data word of an interface holding it (the same encoding
// as the generated __ccgo_fp).
func funcPointer(f any) uintptr {
	type iface [2]uintptr
	return (*iface)(unsafe.Pointer(&f))[1]
}

// installSQLiteSleepGauge wraps the xSleep of every registered VFS, once per
// process, before the first connection opens. GORTEX_SQLITE_SLEEP_GAUGE=0
// leaves SQLite untouched.
func installSQLiteSleepGauge() {
	sqliteSleepInstallOnce.Do(func() {
		if v := strings.TrimSpace(os.Getenv("GORTEX_SQLITE_SLEEP_GAUGE")); v == "0" || strings.EqualFold(v, "false") {
			log.Printf("store_sqlite: sqlite sleep gauge off (GORTEX_SQLITE_SLEEP_GAUGE=%s)", v)
			return
		}
		tls := libc.NewTLS()
		defer tls.Close()
		wrapper := funcPointer(sqliteSleepWrapper)
		wrapped := 0
		for vfs := sqlite3.Xsqlite3_vfs_find(tls, 0); vfs != 0; vfs = vfsAt(vfs).FpNext {
			v := vfsAt(vfs)
			if v.FxSleep == 0 || v.FxSleep == wrapper {
				continue
			}
			sqliteSleepOriginal.Store(vfs, v.FxSleep)
			v.FxSleep = wrapper
			wrapped++
		}
		sqliteSleeps.installed.Store(true)
		// Logged so a run's zero sleeps can be told from an absent gauge.
		log.Printf("store_sqlite: sqlite sleep gauge installed vfs_wrapped=%d", wrapped)
	})
}

// vfsAt views a C sqlite3_vfs address (memory SQLite owns, never moved by the
// Go collector) as its struct.
func vfsAt(p uintptr) *sqlite3.Tsqlite3_vfs {
	return *(**sqlite3.Tsqlite3_vfs)(unsafe.Pointer(&p))
}

// SQLiteSleepMark is a sample of the process-wide SQLite sleep counters.
type SQLiteSleepMark struct {
	Installed     bool  `json:"installed"`
	BusyNanos     int64 `json:"busy_nanos"`
	BusyCalls     int64 `json:"busy_calls"`
	WALRetryNanos int64 `json:"wal_retry_nanos"`
	WALRetryCalls int64 `json:"wal_retry_calls"`
	MaxNanos      int64 `json:"max_nanos"`
}

func sqliteSleepMark() SQLiteSleepMark {
	return SQLiteSleepMark{
		Installed:     sqliteSleeps.installed.Load(),
		BusyNanos:     sqliteSleeps.busyNanos.Load(),
		BusyCalls:     sqliteSleeps.busyCalls.Load(),
		WALRetryNanos: sqliteSleeps.walRetryNanos.Load(),
		WALRetryCalls: sqliteSleeps.walRetryCalls.Load(),
		MaxNanos:      sqliteSleeps.maxNanos.Load(),
	}
}
