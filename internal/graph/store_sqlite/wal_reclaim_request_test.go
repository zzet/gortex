package store_sqlite

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The burst test with the daemon's defaults (threshold 256 MiB, pressure mark
// 1 GiB, 5 s poll, 512 MiB/min copy budget): a log over 1 GiB under a lane
// busy 70% of the time (0.7 s held, 0.3 s free) with ~7 MiB/s of writes (the
// 440 MB/min of measured edit bursts). The log is reset inside the busy lane and
// its size in frames stays under the mark plus one attempt. It writes more
// than 1 GiB, so it runs only with GORTEX_STORE_DEFAULTS_BURST=1: once per
// window, not in the ordinary suite.
func TestReclaimResetsDuringABurstWithTheDaemonDefaults(t *testing.T) {
	if os.Getenv("GORTEX_STORE_DEFAULTS_BURST") != "1" {
		t.Skip("set GORTEX_STORE_DEFAULTS_BURST=1 (writes a >1 GiB log)")
	}
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)
	for k := 0; walFileSize(path+"-wal") < 1100<<20; k++ {
		require.NoError(t, churnWriteOnce(s, k))
	}
	logBytes := func() int64 {
		m := s.WALWriteMark()
		return int64(m.MxFrame) * (int64(m.PageSize) + walFrameHeaderBytes)
	}
	require.Greater(t, logBytes(), int64(1<<30), "precondition: over the default pressure mark")
	stop := make(chan struct{})
	done := make(chan struct{})
	var maxAfterReset int64
	resetsBefore := s.WALReclaimStats().Resets
	go func() {
		defer close(done)
		for k := 0; ; k++ {
			select {
			case <-stop:
				return
			case <-time.After(150 * time.Millisecond):
			}
			held := (k % 7) < 5 // ~70% of the time
			lane.held.Store(held)
			_ = churnWriteOnce(s, k)
			if s.WALReclaimStats().Resets > resetsBefore {
				if b := logBytes(); b > maxAfterReset {
					maxAfterReset = b
				}
			}
		}
	}()
	time.Sleep(90 * time.Second)
	close(stop)
	<-done
	lane.held.Store(false)
	cp := s.WALCopyStats()
	t.Logf("defaults burst: pressure_runs=%d pressure_resets=%d (inside the lane %d) give_ups=%d log_max_after_first_reset=%.0fMiB",
		cp.PressureRuns, cp.PressureResets, cp.PressureLaneResets, cp.PressureGiveUps, float64(maxAfterReset)/(1<<20))
	// One reset inside the burst brings the log under the mark; from there
	// the ordinary reclaim keeps it down in the gaps.
	require.Positive(t, cp.PressureRuns, "no attempt ran through the busy lane over the mark")
	require.Greater(t, s.WALReclaimStats().Resets, resetsBefore, "the log was not reset during the burst")
	require.Less(t, maxAfterReset, int64(1536<<20), "the log outgrew the mark plus one attempt")
}
