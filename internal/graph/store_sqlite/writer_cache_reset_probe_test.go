package store_sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type WriterCacheDelta struct{ Hits, Misses int64 }

// A connection discards its page cache when another connection changes the
// log's header, so a log reset run on the reclaim's checkpoint connection left
// the writer's cache cold for the next write (1,339 misses on this table
// against 0). Every reset path now runs on the writer connection: the write
// after it keeps a warm cache.
func TestAReclaimResetColdsTheWritersPageCache(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0") // drive attempts directly
	// No startup work on the writer (30 s after the open) inside a measured
	// write: it touches pages the probe's writes never cached.
	t.Setenv("GORTEX_SQLITE_LAZY_INDEXES", "off")
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	// Isolate the cold-cache baseline from the store's background PASSIVE.
	lease, leaseErr := s.acquireGenerationBulkCheckpointLease()
	require.NoError(t, leaseErr)
	defer s.releaseGenerationBulkCheckpointLease(lease)
	seedWALChurnTable(t, s)
	lane := &fakeBuildLane{}
	lane.install(s)
	growWAL(t, s, 16) // warm the writer's cache on the whole table
	write := func() WriterCacheDelta {
		before := s.ReaderWaitMark()
		growWAL(t, s, 16)
		d := s.ReaderWaitMark().Split(before)
		return WriterCacheDelta{d.WriterCacheHits, d.WriterCacheMisses}
	}
	require.Zero(t, write().Misses, "precondition: a warm cache")

	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer func() { _ = ckpt.Close() }()

	// The counters see a cold cache: a reset by another connection.
	_, err = checkpointWALOnceOn(context.Background(), ckpt, "TRUNCATE")
	require.NoError(t, err)
	cold := write()
	t.Logf("after a reset on another connection: hits=%d misses=%d", cold.Hits, cold.Misses)
	require.Greater(t, cold.Misses, int64(1000), "precondition: another connection's reset empties the writer's cache")
	require.True(t, s.releaseGenerationBulkCheckpointLease(lease))

	resetCases := []struct {
		name  string
		reset func(t *testing.T)
	}{
		// The writer-free rounds' reset hold; the in-lane rounds (after
		// them) take the same hold, reclaimWALResetHold.
		{"reset hold (writer-free rounds)", func(t *testing.T) {
			cfg := walReclaimConfig{thresholdBytes: 64 << 20, drainDeadline: defaultWALReclaimDrainDeadline, truncateBudget: walReclaimTruncateBudget, readerWait: defaultWALReclaimReaderWait}
			res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
			require.Equal(t, walReclaimReset, res.outcome, res.reason)
			require.True(t, res.resetWriterFree, "the writer-free rounds did not reset")
			t.Logf("idle reset writer_hold=%s", res.writerHold)
			require.LessOrEqual(t, res.writerHold, 4*walReclaimResetHold)
		}},
		{"closed-gate reset (open-gate stages off)", func(t *testing.T) {
			prev := walReclaimSkipOpenGate
			walReclaimSkipOpenGate = true
			defer func() { walReclaimSkipOpenGate = prev }()
			cfg := walReclaimConfig{thresholdBytes: 1 << 20, drainDeadline: defaultWALReclaimDrainDeadline, truncateBudget: walReclaimTruncateBudget, readerWait: defaultWALReclaimReaderWait}
			res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
			require.Equal(t, walReclaimReset, res.outcome, res.reason)
			require.True(t, res.pauseClosed, "not the closed-gate reset")
		}},
		{"pressure reset (busy lane)", func(t *testing.T) {
			if walCopyMethods.Load() == 0 {
				t.Skip("the copy pause is not installed in this process")
			}
			cfg := walReclaimConfig{thresholdBytes: 1 << 20, ceilingBytes: 64 << 20, drainDeadline: defaultWALReclaimDrainDeadline, truncateBudget: walReclaimTruncateBudget, readerWait: defaultWALReclaimReaderWait}
			resets := s.walCopy.pressureLaneResets.Load()
			lane.held.Store(true)
			defer lane.held.Store(false)
			s.RequestWALReclaim()
			res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
			require.Equal(t, walReclaimReset, res.outcome, res.reason)
			require.Greater(t, s.walCopy.pressureLaneResets.Load(), resets, "not the pressure reset")
			t.Logf("pressure reset writer_hold=%s", res.writerHold)
		}},
		{"in-place reset (large log)", func(t *testing.T) {
			s.writeMu.Lock()
			_, err := s.resetWALInPlaceLocked(context.Background())
			s.writeMu.Unlock()
			require.NoError(t, err)
		}},
		{"residue drain", func(t *testing.T) {
			require.NoError(t, s.drainWALResidue(context.Background()))
		}},
	}
	for _, c := range resetCases {
		t.Run(c.name, func(t *testing.T) {
			write() // warm again after the previous case
			c.reset(t)
			snap, ok := readWALIndexSnapshot(path)
			require.True(t, ok)
			require.LessOrEqual(t, snap.MxFrame, uint32(walShrinkMaxRestartFrames), "the log was not reset")
			d := write()
			t.Logf("after the reset: hits=%d misses=%d", d.Hits, d.Misses)
			require.Zero(t, d.Misses, "the reset emptied the writer's page cache")
		})
	}
}

// Readers: every commit already makes a reader connection discard its page
// cache at its next read transaction (the log's header changed), so the
// connection that resets the log changes nothing for them. A pinned reader
// connection reads the same pages after a commit and a reset on another
// connection (the former path) as after a commit and a reset on the writer.
func TestAReclaimResetOnTheWriterLeavesReadersAsTheyWere(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, _ := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	growWAL(t, s, 4)
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	// The pinned connection's page-cache counters are collected only when it
	// returns to the pool; the VFS reads and the mapped pages are per read.
	type readerIO struct{ mapped, fileReads int64 }
	read := func() readerIO {
		before := s.ReaderWaitMark()
		var n int64
		require.NoError(t, conn.QueryRowContext(ctx, `SELECT sum(length(payload)) FROM wal_churn`).Scan(&n))
		d := s.ReaderWaitMark().Split(before)
		return readerIO{d.MappedPages, d.VFS.OtherMainReads + d.VFS.OtherWALReads}
	}
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer func() { _ = ckpt.Close() }()
	read()
	idle := read()
	growWAL(t, s, 4)
	commit := read()
	growWAL(t, s, 4)
	s.writeMu.Lock()
	_, err = checkpointWALOnceOn(ctx, ckpt, "TRUNCATE")
	s.writeMu.Unlock()
	require.NoError(t, err)
	otherReset := read()
	growWAL(t, s, 4)
	s.writeMu.Lock()
	_, err = s.resetWALForReclaim(ctx)
	s.writeMu.Unlock()
	require.NoError(t, err)
	writerReset := read()
	t.Logf("reader (mapped pages, file reads): no commit %+v; after a commit %+v; after a commit and a reset on another connection %+v; after a commit and a reset on the writer %+v",
		idle, commit, otherReset, writerReset)
	require.Zero(t, idle.fileReads+idle.mapped, "precondition: with no commit the reader's cache serves the read")
	require.Greater(t, commit.fileReads, int64(1000), "precondition: a commit discards the reader's cache")
	require.InDelta(t, otherReset.fileReads, writerReset.fileReads, float64(otherReset.fileReads)/20+2, "the writer's reset changed what the reader reads")
	require.InDelta(t, otherReset.mapped, writerReset.mapped, float64(otherReset.mapped)/20+2, "the writer's reset changed what the reader maps")
}

// The idle reset now takes the write gate (it ran without it when it reset
// from the checkpoint connection). With the daemon's defaults, an edit that
// arrives while it holds the gate waits at most the idle hold
// (walReclaimResetHold, 50 ms): the reset gives the gate back as soon as
// a writer queues, and never holds it longer than the hold. The logs are
// ~128 MiB, under the default threshold and the in-place size, so each reset
// is the idle TRUNCATE; the attempts are called directly rather than by the
// loop's 256 MiB trigger. The edit starts when the reset has taken the gate
// (walIdleResetHook) and waits for it as a mutation does. A reset that
// completed before the edit's interrupt arrived is counted as a reset.
func TestAnEditDuringAnIdleResetWaitsAtMostTheHoldWithTheDaemonDefaults(t *testing.T) {
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	lane := &fakeBuildLane{} // installed, as in the daemon; never held
	lane.install(s)
	cfg := resolveWALReclaimConfig()
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer func() { _ = ckpt.Close() }()
	arrived := make(chan struct{}, 1)
	walIdleResetHook = func() {
		select {
		case arrived <- struct{}{}:
		default:
		}
	}
	defer func() { walIdleResetHook = nil }()
	const rounds = 8
	var waits []time.Duration
	outcomes := map[string]int{}
	for r := 0; r < rounds; r++ {
		for k := 0; walFileSize(path+"-wal") < 128<<20; k++ {
			require.NoError(t, churnWriteOnce(s, k))
		}
		done := make(chan walReclaimResult, 1)
		go func() {
			for {
				// The store's own background checkpoint may be in flight
				// (its loop runs with the defaults); the daemon's loop
				// retries, so does this.
				res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
				if res.reason != "checkpoint_in_flight" {
					done <- res
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		select {
		case <-arrived:
			start := time.Now()
			release, err := s.HoldWriteGate(context.Background())
			require.NoError(t, err)
			waits = append(waits, time.Since(start))
			release()
		case res := <-done:
			t.Fatalf("round %d: the attempt ended without an idle reset: outcome=%s reason=%q", r, res.outcome, res.reason)
		}
		res := <-done
		outcomes[fmt.Sprintf("%s/%s idle_reset=%v", res.outcome, res.reason, res.resetWriterFree)]++
		// The count matches the log: nothing wrote since the attempt (the
		// edit only waited for the gate), so a reset log is a counted reset
		// and a skipped attempt left the log as it was.
		require.Equal(t, walLogIsReset(s.dbPath, path+"-wal"), res.outcome == walReclaimReset,
			"round %d: outcome=%s reason=%q against the log", r, res.outcome, res.reason)
		if res.outcome != walReclaimReset {
			// The edit took the gate back first; reset now, unhindered.
			res = s.reclaimWALOnce(cfg, ckpt, path+"-wal")
			require.True(t, res.outcome == walReclaimReset || res.reason == "nothing_to_reclaim", "%s %s", res.outcome, res.reason)
		}
	}
	var longest time.Duration
	for _, w := range waits {
		longest = max(longest, w)
	}
	t.Logf("idle resets with an edit arriving during the hold=%d, outcomes=%v, edit waits=%v, longest=%s",
		len(waits), outcomes, waits, longest)
	require.Len(t, waits, rounds)
	require.LessOrEqual(t, longest, walReclaimResetHold)
}

// An idle reset that completed and then saw its interrupt (a writer queued,
// or the hold ran out, after the log was already reset) is counted as a reset,
// not as a skipped attempt: the log is at its start. The interrupt is injected
// after a real reset (walIdleResetResultHook).
func TestAnIdleResetInterruptedAfterItCompletedCountsAsAReset(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0") // drive attempts directly
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	lane := &fakeBuildLane{}
	lane.install(s)
	growWAL(t, s, 16)
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer func() { _ = ckpt.Close() }()
	walIdleResetResultHook = func(err error) error {
		if err == nil {
			return context.Canceled
		}
		return err
	}
	defer func() { walIdleResetResultHook = nil }()
	resetsBefore := s.WALReclaimStats().Resets
	cfg := walReclaimConfig{thresholdBytes: 64 << 20, drainDeadline: defaultWALReclaimDrainDeadline, truncateBudget: walReclaimTruncateBudget, readerWait: defaultWALReclaimReaderWait}
	res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	require.True(t, walLogIsReset(s.dbPath, path+"-wal"), "precondition: the log was reset")
	require.Equal(t, walReclaimReset, res.outcome, "counted as %s %q", res.outcome, res.reason)
	require.True(t, res.resetWriterFree)
	require.Equal(t, resetsBefore+1, s.WALReclaimStats().Resets)
}
