package store_sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The idle reset now takes the write gate (it ran without it when it reset
// from the checkpoint connection). With the daemon's defaults, an edit that
// arrives while it holds the gate waits at most the idle hold
// (walReclaimIdleResetHold, 50 ms): the reset gives the gate back as soon as
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
	require.LessOrEqual(t, longest, walReclaimIdleResetHold)
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
