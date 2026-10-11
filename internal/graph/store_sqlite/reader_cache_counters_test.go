package store_sqlite

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// The read pool's page reads reach ReaderWaitMark: a read of pages whose
// latest version is in the log goes through the page cache (a miss is a
// pread), and a read of checkpointed pages inside the memory map counts as a
// mapped page instead. A lap's split carries the deltas.
func TestReaderWaitMarkCountsPageReads(t *testing.T) {
	s, _ := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	if s.readGate == nil {
		t.Skip("read gate off")
	}
	seedWALChurnTable(t, s)
	growWAL(t, s, 16) // every page of the table has a version in the log

	before := s.ReaderWaitMark()
	var n int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM wal_churn WHERE length(payload) > 0`).Scan(&n))
	logged := s.ReaderWaitMark().Split(before)
	t.Logf("pages in the log: hits=%d misses=%d spills=%d mapped=%d read_txns=%d", logged.CacheHits, logged.CacheMisses, logged.CacheSpills, logged.MappedPages, logged.ReadTxns)
	require.Equal(t, 4000, n)
	require.Positive(t, logged.ReadTxns)
	require.Greater(t, logged.CacheHits+logged.CacheMisses, int64(500), "the table's ~1,400 pages were read through the page cache")

	// Checkpointed and reset: the pages are in the database file, inside
	// the 256 MiB map.
	require.NoError(t, s.CheckpointWAL())
	before = s.ReaderWaitMark()
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM wal_churn WHERE length(payload) > 0`).Scan(&n))
	mapped := s.ReaderWaitMark().Split(before)
	t.Logf("pages in the file: hits=%d misses=%d spills=%d mapped=%d", mapped.CacheHits, mapped.CacheMisses, mapped.CacheSpills, mapped.MappedPages)
	if walCopyMethods.Load() != 0 {
		require.Greater(t, mapped.MappedPages, int64(500), "mapped pages were not counted")
	}
}

// The cost of reading the three counters when a read transaction ends.
func BenchmarkReadGateCacheCounters(b *testing.B) {
	s, err := openPristine(b, b.TempDir()+"/bench.sqlite")
	require.NoError(b, err)
	defer func() { _ = s.Close() }()
	if s.readGate == nil {
		b.Skip("read gate off")
	}
	conn, err := s.db.Conn(context.Background())
	require.NoError(b, err)
	defer func() { _ = conn.Close() }()
	var n int
	require.NoError(b, conn.QueryRowContext(context.Background(), `SELECT 1`).Scan(&n))
	require.NoError(b, conn.Raw(func(dc any) error {
		g, ok := dc.(*gatedConn)
		require.True(b, ok)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			s.readGate.collectCacheCounters(g.inner)
		}
		return nil
	}))
}

// The writer connection's counters are read at every release of the write
// gate: a write through the gate shows up as writer hits or misses, and a
// transaction bigger than the writer's 32 MiB cache spills.
func TestReaderWaitMarkCountsWriterPageReads(t *testing.T) {
	s, _ := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	// These exact counter deltas belong to the writes below. Join the
	// startup/index/reclaim workers before sampling: a slow race run can
	// otherwise count their gate releases while the test pins the writer.
	s.stopCheckpointLoop()
	s.stopMaintenanceLane()
	// Counter values are the subject here. With every competing worker joined,
	// each write returns the sole writer before Unlock, so sample that known-free
	// connection exactly. The runtime hook's 100 us deadline can legitimately
	// expire during scheduler/GC pauses, even though the connection is available.
	runtimeSampler := s.writeMu.onRelease.Load()
	exactSampler := func() { s.collectWriterCacheCountersContext(t.Context()) }
	s.writeMu.onRelease.Store(&exactSampler)
	defer s.writeMu.onRelease.Store(runtimeSampler)
	seedWALChurnTable(t, s)
	before := s.ReaderWaitMark()
	growWAL(t, s, 4)
	d := s.ReaderWaitMark().Split(before)
	t.Logf("writer: hits=%d misses=%d spills=%d skipped=%d", d.WriterCacheHits, d.WriterCacheMisses, d.WriterCacheSpills, d.WriterCacheSkipped)
	require.Positive(t, d.WriterCacheHits+d.WriterCacheMisses, "the writes' page reads were not counted")
	require.Zero(t, d.WriterCacheSpills)

	// One transaction of ~48 MiB of new pages: more than the cache holds.
	before = s.ReaderWaitMark()
	writeOversizedWriterCacheTransaction(t, s)
	d = s.ReaderWaitMark().Split(before)
	t.Logf("big transaction: hits=%d misses=%d spills=%d skipped=%d", d.WriterCacheHits, d.WriterCacheMisses, d.WriterCacheSpills, d.WriterCacheSkipped)
	require.Positive(t, d.WriterCacheSpills, "a transaction larger than the cache did not spill")
	require.Zero(t, d.WriterCacheSkipped)

	// A release of the write gate while a holder still has the writer
	// connection (conn below) skips its sample, and the split says so. The
	// hold itself is what triggers the release hook.
	s.writeMu.onRelease.Store(runtimeSampler)
	before = s.ReaderWaitMark()
	conn, err := s.writerDB.Conn(context.Background())
	require.NoError(t, err)
	s.writeMu.Lock()
	heldDuring := s.writeMu.held()
	s.writeMu.Unlock()
	require.True(t, heldDuring)
	require.NoError(t, conn.Close())
	require.Equal(t, int64(1), s.ReaderWaitMark().Split(before).WriterCacheSkipped)
}

func writeOversizedWriterCacheTransaction(t *testing.T, s *Store) {
	t.Helper()
	s.writeMu.Lock()
	_, err := s.writerDB.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 12000)
		INSERT INTO wal_churn(id, payload) SELECT 100000+i, zeroblob(4000) FROM n`)
	s.writeMu.Unlock()
	require.NoError(t, err)
}

func TestWriterCacheSkippedSampleCatchesUpAndResets(t *testing.T) {
	s, _ := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	s.stopCheckpointLoop()
	s.stopMaintenanceLane()
	// Drive collection explicitly, still under the writer gate, to force an
	// acquisition deadline independently of the platform's timer resolution.
	runtimeSampler := s.writeMu.onRelease.Load()
	s.writeMu.onRelease.Store(nil)
	defer s.writeMu.onRelease.Store(runtimeSampler)
	sample := func(ctx context.Context) {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		s.collectWriterCacheCountersContext(ctx)
	}
	seedWALChurnTable(t, s)
	sample(t.Context())
	before := s.ReaderWaitMark()
	writeOversizedWriterCacheTransaction(t, s)
	expired, cancel := context.WithCancel(t.Context())
	cancel()
	sample(expired)
	missedMark := s.ReaderWaitMark()
	missed := missedMark.Split(before)
	t.Logf("missed sample: hits=%d misses=%d spills=%d skipped=%d", missed.WriterCacheHits, missed.WriterCacheMisses, missed.WriterCacheSpills, missed.WriterCacheSkipped)
	require.Equal(t, int64(1), missed.WriterCacheSkipped)
	require.Zero(t, missed.WriterCacheHits)
	require.Zero(t, missed.WriterCacheMisses)
	require.Zero(t, missed.WriterCacheSpills)

	sample(t.Context())
	caughtUpMark := s.ReaderWaitMark()
	caughtUp := caughtUpMark.Split(missedMark)
	t.Logf("catch-up sample: hits=%d misses=%d spills=%d skipped=%d", caughtUp.WriterCacheHits, caughtUp.WriterCacheMisses, caughtUp.WriterCacheSpills, caughtUp.WriterCacheSkipped)
	require.Positive(t, caughtUp.WriterCacheHits+caughtUp.WriterCacheMisses)
	require.Positive(t, caughtUp.WriterCacheSpills, "skipping collection must retain the transaction's spills for the next sample")
	require.Zero(t, caughtUp.WriterCacheSkipped)

	sample(t.Context())
	reset := s.ReaderWaitMark().Split(caughtUpMark)
	require.Zero(t, reset.WriterCacheHits)
	require.Zero(t, reset.WriterCacheMisses)
	require.Zero(t, reset.WriterCacheSpills)
	require.Zero(t, reset.WriterCacheSkipped)
}

// The cost of one release's sample of the writer's counters.
func BenchmarkWriterCacheCounters(b *testing.B) {
	s, err := openPristine(b, b.TempDir()+"/bench.sqlite")
	require.NoError(b, err)
	defer func() { _ = s.Close() }()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.collectWriterCacheCounters()
	}
	b.StopTimer()
	require.Zero(b, s.writerCache.skipped.Load())
}
