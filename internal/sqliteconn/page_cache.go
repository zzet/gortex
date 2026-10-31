package sqliteconn

import (
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// PageCacheBytes reports how many bytes the page caches of every SQLite
// connection in the process hold now, and the most they have held since the
// process started. The memory comes from SQLite's C allocator, outside the Go
// heap and its accounting.
//
// The pinned driver builds SQLite with SQLITE_DEFAULT_MEMSTATUS=0, so
// sqlite3_memory_used stays 0; turning the allocator statistics on would put
// a mutex on every SQLite allocation for the life of the process. The page
// cache keeps its own counter regardless: with no SQLITE_CONFIG_PAGECACHE
// buffer every page is an "overflow" allocation, counted under the page-cache
// mutex. Only each cache's small initial bulk block (20 pages) escapes it.
func PageCacheBytes() (current, peak int64) {
	tls := libc.NewTLS()
	defer tls.Close()
	out := tls.Alloc(16)
	defer tls.Free(16)
	high := out + 8
	if sqlite3.Xsqlite3_status64(tls, sqlite3.SQLITE_STATUS_PAGECACHE_OVERFLOW, out, high, 0) != sqlite3.SQLITE_OK {
		return 0, 0
	}
	return **(**int64)(unsafe.Pointer(&out)), **(**int64)(unsafe.Pointer(&high))
}
