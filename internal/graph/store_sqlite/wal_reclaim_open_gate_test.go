package store_sqlite

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// runLongReaderChurn drives continuous writes against readers whose read
// transactions always outlast the closed-gate drain deadline (500–1,200 ms
// against 250 ms), with the reclaim polling at a test cadence.
func runLongReaderChurn(t *testing.T, skipOpenGate bool, writes int64) (WALReclaimStats, *walChurn, int64) {
	t.Helper()
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "16")
	setWALReclaimCadence(t, 50*time.Millisecond, 50*time.Millisecond, 400*time.Millisecond)
	prev := walReclaimSkipOpenGate
	walReclaimSkipOpenGate = skipOpenGate
	t.Cleanup(func() { walReclaimSkipOpenGate = prev })
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)

	churn := startWALChurn(t, s, path, 4, 500*time.Millisecond, 1200*time.Millisecond)
	churn.runUntil(t, writes, 90*time.Second)
	churn.halt(t)
	stats := s.WALReclaimStats()
	p50, p99, maxLat, n := churn.latencies()
	t.Logf("skip_open_gate=%v writes=%d wal_final=%.1fMiB wal_max=%.1fMiB resets=%d open_gate_resets=%d deferrals=%d attempts=%d pause_n=%d pause_max=%s reader_waits=%d reader_wait_max=%s writer_hold_max=%s read_lat p50=%s p99=%s max=%s n=%d",
		skipOpenGate, churn.writes.Load(), float64(walFileSize(path+"-wal"))/(1<<20), float64(churn.maxWAL.Load())/(1<<20),
		stats.Resets, stats.OpenGateResets, stats.Deferrals, stats.Attempts, stats.PauseCount, stats.PauseMax,
		stats.ReaderWaits, stats.ReaderWaitMax, stats.WriterHoldMax, p50, p99, maxLat, n)
	return stats, churn, 16 << 20
}

// Readers that never go quiet for the closed-gate drain (every read
// transaction outlasts it) and a writer that never pauses: no gap lets a
// short hold reset the log, so the last resort does (at
// walReclaimLastResortBytes, here 4 × a 12 MiB ceiling). The log stays within
// the last-resort mark plus one attempt's growth, no reader is paused, and
// only the last resort holds the writer longer than a short hold.
//
// It replaced a contract under which every urgent attempt (2 × the threshold)
// held the writer up to 2 s, and the log stayed within three times the
// threshold.
func TestWALReclaimResetsUnderReadersLongerThanTheDrain(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB", "12")
	prevFloor := walReclaimCeilingFloor
	walReclaimCeilingFloor = 0
	t.Cleanup(func() { walReclaimCeilingFloor = prevFloor })
	stats, churn, _ := runLongReaderChurn(t, false, 240)
	mark := int64(walReclaimLastResortFactor * (12 << 20))
	require.GreaterOrEqual(t, stats.Resets, int64(2), "the log must reset repeatedly under long readers")
	require.GreaterOrEqual(t, stats.OpenGateResets, int64(2), "the resets must come from the open-gate stages")
	require.Positive(t, stats.LastResortRuns, "precondition: no gap, so the last resort ran")
	require.LessOrEqual(t, churn.maxWAL.Load(), 2*mark, "the WAL must stay within the last-resort mark plus one attempt's growth")
	cfg := resolveWALReclaimConfig()
	bound := cfg.drainDeadline + cfg.truncateBudget + 100*time.Millisecond
	require.LessOrEqual(t, stats.PauseMax, bound, "gate closed longer than drain deadline + TRUNCATE budget")
	require.LessOrEqual(t, stats.ReaderWaitMax, bound, "a reader waited at the gate longer than the bound")
	require.LessOrEqual(t, stats.WriterHoldMax, walReclaimLaneBudget, "the writer was held past the lane budget")
}

// The same churn with only the closed-gate drain: reads rarely drain inside
// 250 ms, so the reclaim mostly defers and the WAL outgrows twice the
// threshold — the live daemon's `resets=0 deferrals=8`.
func TestWALReclaimClosedGateAloneNeverResetsUnderLongReaders(t *testing.T) {
	stats, churn, threshold := runLongReaderChurn(t, true, 80)
	require.Zero(t, stats.OpenGateResets)
	require.Greater(t, stats.Deferrals, int64(0))
	require.Greater(t, churn.maxWAL.Load(), 2*threshold, "without the open-gate stage the WAL outgrows twice the threshold")
}
