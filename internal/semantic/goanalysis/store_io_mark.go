package goanalysis

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// storeIOMarker is the store's cumulative wait and VFS counters
// (store_sqlite.ReaderWaitMark); the apply's log line reports the difference
// across the apply, so its store reads and writes stand beside its wall
// time, CPU and faults.
type storeIOMarker interface {
	ReaderWaitMark() store_sqlite.ReaderWaitMark
}

func storeIOMarkOf(g graph.Store) *store_sqlite.ReaderWaitMark {
	m, ok := g.(storeIOMarker)
	if !ok {
		return nil
	}
	mark := m.ReaderWaitMark()
	return &mark
}

// storeIOSince renders the store's reads and writes since start, in ms and
// counts; nil when the store keeps no counters.
func storeIOSince(g graph.Store, start *store_sqlite.ReaderWaitMark) map[string]float64 {
	if start == nil {
		return nil
	}
	m, ok := g.(storeIOMarker)
	if !ok {
		return nil
	}
	split := m.ReaderWaitMark().Split(*start)
	ms := func(ns int64) float64 { return float64(ns) / 1e6 }
	v := split.VFS
	return map[string]float64{
		"read_txn_ms": ms(split.ReadTxn.Nanoseconds()), "read_txns": float64(split.ReadTxns),
		"cache_hits": float64(split.CacheHits), "cache_misses": float64(split.CacheMisses),
		"mapped_pages":      float64(split.MappedPages),
		"writer_cache_hits": float64(split.WriterCacheHits), "writer_cache_misses": float64(split.WriterCacheMisses),
		"writer_main_reads": float64(v.WriterMainReads), "writer_main_read_ms": ms(v.WriterMainReadTime.Nanoseconds()),
		"writer_wal_reads": float64(v.WriterWALReads), "writer_wal_read_ms": ms(v.WriterWALReadTime.Nanoseconds()),
		"other_main_reads": float64(v.OtherMainReads), "other_main_read_ms": ms(v.OtherMainReadTime.Nanoseconds()),
		"other_wal_reads": float64(v.OtherWALReads), "other_wal_read_ms": ms(v.OtherWALReadTime.Nanoseconds()),
		"writer_wal_writes": float64(v.WriterWALWrites), "writer_wal_write_ms": ms(v.WriterWALWriteTime.Nanoseconds()),
		"writer_wal_write_bytes": float64(v.WriterWALWriteBytes),
	}
}
