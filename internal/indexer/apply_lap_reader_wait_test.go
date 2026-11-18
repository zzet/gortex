package indexer

import (
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Each apply step records, beside its wall time, CPU and lock wait, the
// store's waits over it by kind — the read gate and pool (reader wait), the
// writer pool, SQLite's busy-handler and WAL read-lock retry sleeps, the read
// transactions' time — and the process's block reads, so a step that waited
// can be told apart as a gate or pool wait, a SQLite lock wait, I/O, or a
// starved processor.
func TestApplyStepsRecordTheStoreWaitsByKind(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	idx := &Indexer{logger: zap.New(core)}
	var mark store_sqlite.ReaderWaitMark
	idx.storeWaits = func() store_sqlite.ReaderWaitMark { return mark }
	idx.startApplyLaps()
	mark.GateNanos += int64(200 * time.Millisecond)
	mark.PoolNanos += int64(50 * time.Millisecond)
	mark.Sleep.BusyNanos += int64(30 * time.Millisecond)
	mark.Sleep.BusyCalls += 3
	mark.ReadTxnNanos += int64(400 * time.Millisecond)
	mark.ReadTxns += 7
	mark.CacheHits += 90
	mark.CacheMisses += 12
	mark.MappedPages += 300
	mark.WriterCacheMisses += 5
	idx.applyLap("prior_view")
	mark.PoolNanos += int64(40 * time.Millisecond)
	mark.WriterPoolNanos += int64(15 * time.Millisecond)
	mark.Sleep.WALRetryNanos += int64(5 * time.Millisecond)
	mark.Sleep.WALRetryCalls++
	idx.applyLap("evict")
	idx.finishApplyLaps(1, 0, 0)
	entries := logs.FilterMessage("indexer: graph apply steps").All()
	if len(entries) != 1 {
		t.Fatalf("apply records = %d", len(entries))
	}
	f := entries[0].ContextMap()
	want := map[string]any{
		"prior_view_reader_wait_ms": 250.0, "prior_view_busy_sleep_ms": 30.0, "prior_view_busy_sleeps": int64(3),
		"prior_view_read_txn_ms": 400.0, "prior_view_read_txns": int64(7), "prior_view_writer_pool_wait_ms": 0.0,
		"prior_view_cache_hits": int64(90), "prior_view_cache_misses": int64(12), "prior_view_mapped_pages": int64(300),
		"prior_view_cache_spills": int64(0), "evict_cache_misses": int64(0),
		"prior_view_writer_cache_misses": int64(5), "prior_view_writer_cache_spills": int64(0), "evict_writer_cache_hits": int64(0),
		"evict_reader_wait_ms": 40.0, "evict_writer_pool_wait_ms": 15.0, "evict_wal_retry_sleep_ms": 5.0,
		"evict_wal_retries": int64(1), "evict_busy_sleep_ms": 0.0,
	}
	for k, v := range want {
		if f[k] != v {
			t.Fatalf("%s = %v (%T), want %v", k, f[k], f[k], v)
		}
	}
	for _, k := range []string{"prior_view_block_reads", "prior_view_sched_waits", "prior_view_sched_wait_ms", "evict_sched_waits", "evict_sched_wait_ms"} {
		if _, ok := f[k]; !ok {
			t.Fatalf("the step lacks %s: %v", k, f)
		}
	}
}
