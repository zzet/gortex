//go:build performance

package store_sqlite

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This continuous-load peak envelope depends on producer/read/reset throughput.
// Keep the original scenario and limits in the existing opt-in TestPerformance
// workflow; ordinary CI tests admission/spacing separately without a peak SLA.
// Under a lane that never goes idle the log passes its ceiling (the reclaim
// waits for gaps between edit cycles), and over the hard cap (4x the ceiling)
// one bounded attempt runs despite the lane, spaced and logged, so the log
// still cannot grow without bound.
func TestPerformanceWALReclaimRunsDespiteTheLaneOnlyOverTheHardCap(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "4")
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB", "12")
	prevFloor, prevSpacing := walReclaimCeilingFloor, walReclaimHardCapSpacing
	walReclaimCeilingFloor, walReclaimHardCapSpacing = 0, time.Second
	t.Cleanup(func() { walReclaimCeilingFloor, walReclaimHardCapSpacing = prevFloor, prevSpacing })
	setWALReclaimCadence(t, 50*time.Millisecond, 50*time.Millisecond, 400*time.Millisecond)
	logs := captureReclaimLog(t)
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true) // a cycle that never ends

	churn := startWALChurn(t, s, path, 4, 150*time.Millisecond, 300*time.Millisecond)
	churn.runUntil(t, 160, 120*time.Second)
	churn.halt(t)

	const ceiling, hardCap = 12 << 20, 4 * (12 << 20)
	stats := s.WALReclaimStats()
	t.Logf("held lane: writes=%d wal_max=%.1fMiB ceiling=%.0fMiB hard_cap_runs=%d resets=%d refusals=%d writer_hold_max=%s",
		churn.writes.Load(), float64(churn.maxWAL.Load())/(1<<20), float64(stats.CeilingBytes)/(1<<20),
		stats.CycleCeilingRuns, stats.Resets, stats.CycleRefusals, stats.WriterHoldMax)
	require.Equal(t, int64(ceiling), stats.CeilingBytes)
	require.Greater(t, churn.maxWAL.Load(), int64(ceiling), "precondition: the log passed its ceiling under the held lane")
	require.Positive(t, stats.CycleCeilingRuns, "no reclaim ran over the hard cap")
	require.Positive(t, stats.Resets, "the hard-cap runs must reset the log")
	require.Positive(t, stats.CycleRefusals, "below the hard cap the held lane must refuse the reclaim")
	require.LessOrEqual(t, churn.maxWAL.Load(), int64(2*hardCap), "the WAL grew past twice its hard cap under a held lane")
	require.LessOrEqual(t, stats.WriterHoldMax, walReclaimMaxWriterHold+250*time.Millisecond, "the writer-hold cap bounds a hard-cap run")
	out := logs.String()
	require.Contains(t, out, "reason=wal_hard_cap")
	require.NotContains(t, out, "reason=wal_ceiling wal_bytes", "a ceiling run started during the edit cycle")
}
