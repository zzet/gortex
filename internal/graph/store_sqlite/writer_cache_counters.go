package store_sqlite

import (
	"context"
	"sync/atomic"
	"time"
	"unsafe"

	modernsqlite "modernc.org/sqlite"
)

// The writer connection's page-cache counters.
//
// Every write runs on the writer pool's one connection under the write gate,
// so the gate's release is the end of a write: the holder has committed and
// handed the connection back. There, still holding the gate, the counters of
// that connection are read with reset and added to the store's sums
// (ReaderWaitMark.WriterCache*). The writer's page cache is cache_size
// -32768: 32 MiB, 8,192 pages of 4 KiB.
//
// The connection is taken without waiting: when a holder still has it (a
// transaction or a *sql.Conn kept past the gate, a bulk window's pinned
// connection), the sample is skipped and counted in the next one.

type writerCacheCounters struct {
	hits, misses, spills atomic.Int64
	skipped              atomic.Int64
}

// writerCacheTakeWait bounds the wait for the writer connection at a release:
// a free connection is taken at once, a held one is not waited for.
var writerCacheTakeWait = 100 * time.Microsecond

func (s *Store) installWriterCacheCounters() {
	if s.coreless() || s.writerDB == nil || s.writerDB == s.db {
		return
	}
	f := s.collectWriterCacheCounters
	s.writeMu.onRelease.Store(&f)
}

func (s *Store) collectWriterCacheCounters() {
	if s.bulkConn != nil {
		// A bulk window keeps the writer connection pinned across its
		// holds; its counters are read when the window ends.
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), writerCacheTakeWait)
	defer cancel()
	s.collectWriterCacheCountersContext(ctx)
}

// collectWriterCacheCountersContext samples under the caller-held write gate.
// The release hook supplies its best-effort acquisition deadline; an explicit
// sampler can use its own context when it knows the writer has been returned.
// A failed acquisition leaves SQLite's counters intact for the next sample.
func (s *Store) collectWriterCacheCountersContext(ctx context.Context) {
	if s.bulkConn != nil {
		return
	}
	conn, err := s.writerDB.Conn(ctx)
	if err != nil {
		s.writerCache.skipped.Add(1)
		return
	}
	defer func() { _ = conn.Close() }()
	_ = conn.Raw(func(dc any) error {
		if _, tls, ok := driverConnHandles(dc); ok {
			vfsWriterTLS.Store(uintptr(unsafe.Pointer(tls)))
		}
		st, ok := dc.(modernsqlite.DBStatus)
		if !ok {
			return nil
		}
		if n, _, err := st.Status(modernsqlite.DBStatusCacheHit, true); err == nil {
			s.writerCache.hits.Add(int64(n))
		}
		if n, _, err := st.Status(modernsqlite.DBStatusCacheMiss, true); err == nil {
			s.writerCache.misses.Add(int64(n))
		}
		if n, _, err := st.Status(modernsqlite.DBStatusCacheSpill, true); err == nil {
			s.writerCache.spills.Add(int64(n))
		}
		return nil
	})
}
