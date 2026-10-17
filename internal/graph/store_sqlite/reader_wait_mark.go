package store_sqlite

import "time"

// ReaderWaitMark is a monotonic, per-process sample of the time the store's
// readers spent waiting before they could read: held at the read gate while
// the WAL reclaim or a checkpoint paused new reads (GateNanos, GateWaits), and
// blocked on the read pool for a free connection (PoolNanos, PoolWaits). Two
// marks bracket a step the way WALWriteMark does for the bytes a step wrote:
// Since gives the readers' wait inside it, attributed to every reader in the
// process, not only the step's own.
//
// The mark also carries what Since does not add up, so a lap whose wall time
// is far above its CPU can be attributed:
//   - WriterPoolNanos/Waits: waits for the writer pool's one connection;
//   - Sleep: time asleep inside SQLite (process-wide, every connection), split
//     into the busy handler (lock contention, whole-millisecond sleeps) and the
//     WAL read-lock retry loop (sqlite_sleep_gauge.go);
//   - ReadTxnNanos/ReadTxns: the wall time of the pool's read transactions
//     (a connection's first statement to its return to the pool), which is
//     the store's share of a read-heavy lap.
type ReaderWaitMark struct {
	GateNanos int64 `json:"gate_nanos"`
	GateWaits int64 `json:"gate_waits"`
	PoolNanos int64 `json:"pool_nanos"`
	PoolWaits int64 `json:"pool_waits"`

	WriterPoolNanos int64           `json:"writer_pool_nanos"`
	WriterPoolWaits int64           `json:"writer_pool_waits"`
	Sleep           SQLiteSleepMark `json:"sleep"`
	ReadTxnNanos    int64           `json:"read_txn_nanos"`
	ReadTxns        int64           `json:"read_txns"`
	// Page reads of the pool's read transactions (sqlite_read_gate.go):
	// found in the connection's page cache, read with a pread (from the log
	// or the database file), and dirty pages spilled; MappedPages are pages
	// served from the memory map instead, process-wide (every connection),
	// which the cache counters never see.
	CacheHits   int64 `json:"cache_hits"`
	CacheMisses int64 `json:"cache_misses"`
	CacheSpills int64 `json:"cache_spills"`
	MappedPages int64 `json:"mapped_pages"`
	// The writer connection's page cache, read at the end of every hold of
	// the write gate (writer_cache_counters.go). A writer spill means a
	// transaction's dirty pages outgrew the cache and went to the log early.
	WriterCacheHits   int64 `json:"writer_cache_hits"`
	WriterCacheMisses int64 `json:"writer_cache_misses"`
	WriterCacheSpills int64 `json:"writer_cache_spills"`
	// WriterCacheSkipped counts releases whose sample was skipped (a holder
	// still had the connection); their pages land in a later sample.
	WriterCacheSkipped int64 `json:"writer_cache_skipped"`
	// VFS is the page I/O timed at the VFS (vfs_io_counters.go),
	// process-wide.
	VFS VFSIOMark `json:"vfs"`
}

// ReaderWaitMark samples the cumulative reader waits. Atomics and the pools'
// statistics: it never blocks.
func (s *Store) ReaderWaitMark() ReaderWaitMark {
	var m ReaderWaitMark
	if s == nil || s.storeCore == nil {
		return m
	}
	if g := s.readGate; g != nil {
		m.GateNanos = g.waitNanos.Load()
		m.GateWaits = g.waits.Load()
		m.ReadTxnNanos = g.readNanos.Load()
		m.ReadTxns = g.readTxns.Load()
		m.CacheHits = g.cacheHits.Load()
		m.CacheMisses = g.cacheMisses.Load()
		m.CacheSpills = g.cacheSpills.Load()
	}
	if s.db != nil {
		st := s.db.Stats()
		m.PoolNanos = int64(st.WaitDuration)
		m.PoolWaits = st.WaitCount
	}
	if s.writerDB != nil && s.writerDB != s.db {
		st := s.writerDB.Stats()
		m.WriterPoolNanos = int64(st.WaitDuration)
		m.WriterPoolWaits = st.WaitCount
	}
	m.Sleep = sqliteSleepMark()
	m.MappedPages = sqliteMappedPages.Load()
	m.WriterCacheHits = s.writerCache.hits.Load()
	m.WriterCacheMisses = s.writerCache.misses.Load()
	m.WriterCacheSpills = s.writerCache.spills.Load()
	m.WriterCacheSkipped = s.writerCache.skipped.Load()
	m.VFS = vfsIOMark()
	return m
}

// Since is the reader wait accumulated between prev and m: the read gate and
// the read pool, as before.
func (m ReaderWaitMark) Since(prev ReaderWaitMark) time.Duration {
	return time.Duration((m.GateNanos - prev.GateNanos) + (m.PoolNanos - prev.PoolNanos))
}

// ReaderWaitSplit is one lap's store waits, each on its own.
type ReaderWaitSplit struct {
	Gate, ReadPool, WriterPool time.Duration
	BusySleep, WALRetrySleep   time.Duration
	BusySleeps, WALRetries     int64
	ReadTxn                    time.Duration
	ReadTxns                   int64
	CacheHits, CacheMisses     int64
	CacheSpills, MappedPages   int64
	WriterCacheHits            int64
	WriterCacheMisses          int64
	WriterCacheSpills          int64
	WriterCacheSkipped         int64
	VFS                        VFSIOSplit
}

// Split is the lap between prev and m, by kind of wait.
func (m ReaderWaitMark) Split(prev ReaderWaitMark) ReaderWaitSplit {
	return ReaderWaitSplit{
		Gate:          time.Duration(m.GateNanos - prev.GateNanos),
		ReadPool:      time.Duration(m.PoolNanos - prev.PoolNanos),
		WriterPool:    time.Duration(m.WriterPoolNanos - prev.WriterPoolNanos),
		BusySleep:     time.Duration(m.Sleep.BusyNanos - prev.Sleep.BusyNanos),
		WALRetrySleep: time.Duration(m.Sleep.WALRetryNanos - prev.Sleep.WALRetryNanos),
		BusySleeps:    m.Sleep.BusyCalls - prev.Sleep.BusyCalls,
		WALRetries:    m.Sleep.WALRetryCalls - prev.Sleep.WALRetryCalls,
		ReadTxn:       time.Duration(m.ReadTxnNanos - prev.ReadTxnNanos),
		ReadTxns:      m.ReadTxns - prev.ReadTxns,
		CacheHits:     m.CacheHits - prev.CacheHits,
		CacheMisses:   m.CacheMisses - prev.CacheMisses,
		CacheSpills:   m.CacheSpills - prev.CacheSpills,
		MappedPages:   m.MappedPages - prev.MappedPages,

		WriterCacheHits:    m.WriterCacheHits - prev.WriterCacheHits,
		WriterCacheMisses:  m.WriterCacheMisses - prev.WriterCacheMisses,
		WriterCacheSpills:  m.WriterCacheSpills - prev.WriterCacheSpills,
		WriterCacheSkipped: m.WriterCacheSkipped - prev.WriterCacheSkipped,
		VFS:                m.VFS.Since(prev.VFS),
	}
}
