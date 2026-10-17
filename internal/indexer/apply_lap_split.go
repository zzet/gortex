package indexer

import (
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// An apply step's wall time is its work, its waits and the time it was
// runnable but not running. applyLap records, beside each step's wall time,
// the process CPU spent over it (process-wide: with GOMAXPROCS=1 other
// goroutines' CPU lands here too) and, when the graph is a per-file delta,
// the time the step's writes waited for the delta's write lock
// (graph.DeltaWriter.WriteLockWait). Wall well above CPU plus lock wait,
// with no page-ins, is a starved step.

// writeLockWaiter is a graph that accounts its write-lock waits.
type writeLockWaiter interface {
	WriteLockWait() time.Duration
}

// applyLapSplit is one lap's running marks.
type applyLapSplit struct {
	cpu  time.Duration
	wait time.Duration
	// store is the store's cumulative waits (store_sqlite.ReaderWaitMark),
	// when the indexer has a source for them; blockReads the process's block
	// reads.
	store      store_sqlite.ReaderWaitMark
	blockReads int64
	// sched is the process's scheduler-latency histogram
	// (sched_latency_mark.go): the runnable waits of every goroutine.
	sched schedMark
}

func (idx *Indexer) applyLapSplitMark() applyLapSplit {
	mark := applyLapSplit{cpu: processCPUTime(), blockReads: editDeltaProcessIO().blockReads, sched: readSchedMark()}
	if idx.storeWaits != nil {
		mark.store = idx.storeWaits()
	}
	if w, ok := idx.graph.(writeLockWaiter); ok {
		mark.wait = w.WriteLockWait()
	}
	return mark
}

// applyLapSplitFields is the CPU and lock-wait split of the step from start
// to now.
func applyLapSplitFields(name string, start, now applyLapSplit) []zap.Field {
	waits, waited := now.sched.since(start.sched)
	return append([]zap.Field{
		zap.Float64(name+"_cpu_ms", float64((now.cpu-start.cpu).Microseconds())/1000),
		zap.Float64(name+"_lock_wait_ms", float64((now.wait-start.wait).Microseconds())/1000),
		zap.Int64(name+"_block_reads", now.blockReads-start.blockReads),
	}, append(storeWaitFields(name+"_", now.store.Split(start.store)), schedWaitFields(name+"_", waits, waited)...)...)
}

// schedWaitFields renders the process's runnable waits that ended over a
// step: how many, and their estimated total (process-wide; see
// sched_latency_mark.go).
func schedWaitFields(prefix string, waits int64, total time.Duration) []zap.Field {
	return []zap.Field{
		zap.Int64(prefix+"sched_waits", waits),
		zap.Float64(prefix+"sched_wait_ms", float64(total.Microseconds())/1000),
	}
}

// storeWaitFields renders one lap's store waits: reader_wait (the read gate
// and read pool, as before), then each kind on its own — the writer pool,
// SQLite's busy-handler and WAL read-lock retry sleeps with their counts, and
// the read transactions' wall time with their count.
func storeWaitFields(prefix string, w store_sqlite.ReaderWaitSplit) []zap.Field {
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	return []zap.Field{
		zap.Float64(prefix+"reader_wait_ms", ms(w.Gate+w.ReadPool)),
		zap.Float64(prefix+"writer_pool_wait_ms", ms(w.WriterPool)),
		zap.Float64(prefix+"busy_sleep_ms", ms(w.BusySleep)),
		zap.Int64(prefix+"busy_sleeps", w.BusySleeps),
		zap.Float64(prefix+"wal_retry_sleep_ms", ms(w.WALRetrySleep)),
		zap.Int64(prefix+"wal_retries", w.WALRetries),
		zap.Float64(prefix+"read_txn_ms", ms(w.ReadTxn)),
		zap.Int64(prefix+"read_txns", w.ReadTxns),
		// The read pool's page cache (sqlite3_db_status, summed as each
		// connection returns): a miss is a pread from the WAL or from the
		// file beyond the map; a page served from the memory map is neither
		// and counts as mapped (process-wide).
		zap.Int64(prefix+"cache_hits", w.CacheHits),
		zap.Int64(prefix+"cache_misses", w.CacheMisses),
		zap.Int64(prefix+"cache_spills", w.CacheSpills),
		zap.Int64(prefix+"mapped_pages", w.MappedPages),
		// The writer connection's page cache, read at the end of each write:
		// a spill is one transaction outgrowing the 32 MiB cache.
		zap.Int64(prefix+"writer_cache_hits", w.WriterCacheHits),
		zap.Int64(prefix+"writer_cache_misses", w.WriterCacheMisses),
		zap.Int64(prefix+"writer_cache_spills", w.WriterCacheSpills),
	}
}

// storeWaitMillis is storeWaitFields as a map, for a per-phase log field.
func storeWaitMillis(w store_sqlite.ReaderWaitSplit) map[string]float64 {
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	return map[string]float64{
		"reader_wait_ms": ms(w.Gate + w.ReadPool), "writer_pool_wait_ms": ms(w.WriterPool),
		"busy_sleep_ms": ms(w.BusySleep), "busy_sleeps": float64(w.BusySleeps),
		"wal_retry_sleep_ms": ms(w.WALRetrySleep), "wal_retries": float64(w.WALRetries),
		"read_txn_ms": ms(w.ReadTxn), "read_txns": float64(w.ReadTxns),
		"cache_hits": float64(w.CacheHits), "cache_misses": float64(w.CacheMisses),
		"cache_spills": float64(w.CacheSpills), "mapped_pages": float64(w.MappedPages),
		"writer_cache_hits": float64(w.WriterCacheHits), "writer_cache_misses": float64(w.WriterCacheMisses),
		"writer_cache_spills": float64(w.WriterCacheSpills),
		// The store's VFS counters for the lap: reads (calls and time) of
		// the main file and of the log by the writer connection and by the
		// others, the writer's log writes, and the other connections' main
		// file writes (the checkpoint copies). Pages served from the memory
		// map do not pass through a read; they are mapped_pages, and a fault
		// on the map is a major fault of the lap.
		"writer_main_reads": float64(w.VFS.WriterMainReads), "writer_main_read_ms": ms(w.VFS.WriterMainReadTime),
		"writer_wal_reads": float64(w.VFS.WriterWALReads), "writer_wal_read_ms": ms(w.VFS.WriterWALReadTime),
		"other_main_reads": float64(w.VFS.OtherMainReads), "other_main_read_ms": ms(w.VFS.OtherMainReadTime),
		"other_wal_reads": float64(w.VFS.OtherWALReads), "other_wal_read_ms": ms(w.VFS.OtherWALReadTime),
		"writer_wal_writes": float64(w.VFS.WriterWALWrites), "writer_wal_write_ms": ms(w.VFS.WriterWALWriteTime),
		"writer_wal_write_bytes": float64(w.VFS.WriterWALWriteBytes),
		"other_main_writes":      float64(w.VFS.OtherMainWrites), "other_main_write_ms": ms(w.VFS.OtherMainWriteTime),
	}
}

// addVFSIOSplit sums two laps of the store's VFS counters.
func addVFSIOSplit(a, b store_sqlite.VFSIOSplit) store_sqlite.VFSIOSplit {
	return store_sqlite.VFSIOSplit{
		WriterMainReads: a.WriterMainReads + b.WriterMainReads, WriterWALReads: a.WriterWALReads + b.WriterWALReads,
		OtherMainReads: a.OtherMainReads + b.OtherMainReads, OtherWALReads: a.OtherWALReads + b.OtherWALReads,
		WriterMainReadTime: a.WriterMainReadTime + b.WriterMainReadTime, WriterWALReadTime: a.WriterWALReadTime + b.WriterWALReadTime,
		OtherMainReadTime: a.OtherMainReadTime + b.OtherMainReadTime, OtherWALReadTime: a.OtherWALReadTime + b.OtherWALReadTime,
		WriterWALWrites: a.WriterWALWrites + b.WriterWALWrites, OtherMainWrites: a.OtherMainWrites + b.OtherMainWrites,
		WriterWALWriteTime: a.WriterWALWriteTime + b.WriterWALWriteTime, OtherMainWriteTime: a.OtherMainWriteTime + b.OtherMainWriteTime,
		WriterWALWriteBytes: a.WriterWALWriteBytes + b.WriterWALWriteBytes, OtherMainWriteBytes: a.OtherMainWriteBytes + b.OtherMainWriteBytes,
	}
}
