package store_sqlite

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The actual checkpoint has reached xSync before the daemon wires its lane.
// A later predicate must still withdraw an ordinary running SQL attempt.
func TestCheckpointAttemptObservesLateBuildLaneInstallation(t *testing.T) {
	for _, kind := range []string{"ordinary", "lease_override", "ignores_cycle", "hard_cap", "disabled"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "disabled" {
				t.Setenv("GORTEX_SQLITE_WAL_CHECKPOINT_CYCLE_DEFER_MS", "0")
			}
			s, db := finalBackfillFixture(t)
			require.False(t, s.hasBuildLanePredicate())
			policy := checkpointYieldsToCycle
			switch kind {
			case "lease_override":
				policy = checkpointOverridesLease
			case "ignores_cycle":
				policy = checkpointIgnoresCycle
			case "hard_cap":
				policy = checkpointOverridesLeaseAndCycle
			}
			attempt, err := s.beginBackgroundCheckpointAttempt(policy)
			require.NoError(t, err)
			entered, release := stallReclaimCheckpointSync(t, db)
			done := make(chan error, 1)
			go func() { _, err := checkpointWALOnceOn(attempt.ctx, db, "PASSIVE"); done <- err }()
			var once sync.Once
			var result error
			joined := false
			cleanup := func() {
				attempt.cancel(context.Canceled)
				once.Do(func() { close(release) })
				if !joined {
					select {
					case result = <-done:
						joined = true
					case <-time.After(5 * time.Second):
						t.Error("owned checkpoint did not join")
					}
				}
				s.finishBackgroundCheckpointAttempt(attempt)
			}
			t.Cleanup(cleanup)
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("actual SQL never reached the VFS sync")
			}
			lane := &fakeBuildLane{}
			lane.install(s)
			lane.held.Store(true)
			yielding := kind == "ordinary" || kind == "lease_override"
			if yielding {
				select {
				case <-attempt.ctx.Done():
					require.ErrorIs(t, context.Cause(attempt.ctx), errWALCheckpointYieldedToCycle)
				case <-time.After(5 * time.Second):
					t.Fatal("pre-install checkpoint permanently lost its cycle watcher")
				}
			} else {
				select {
				case <-attempt.ctx.Done():
					t.Fatal("force/disabled policy was withdrawn")
				case <-time.After(walCheckpointCycleYieldPoll + raceSlack(50*time.Millisecond)):
				}
				once.Do(func() { close(release) })
				select {
				case result = <-done:
					joined = true
				case <-time.After(5 * time.Second):
					t.Fatal("force/disabled checkpoint did not join after its sync was released")
				}
				require.NoError(t, result)
			}
			cleanup()
			if yielding {
				require.True(t, errors.Is(result, context.Canceled), "actual SQL must observe withdrawal after the uninterruptible sync returns")
			}
		})
	}
}

// A reset hold declined on the current lane must not start another real
// writer-free checkpoint in that same cycle, even before the poll watcher runs.
func TestOpenGateRoundRefusesActualCopyInsideLiveCycle(t *testing.T) {
	for _, kind := range []string{"ordinary", "pressure", "hard_cap", "disabled", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "disabled" {
				t.Setenv("GORTEX_SQLITE_WAL_CHECKPOINT_CYCLE_DEFER_MS", "0")
			}
			s, db := finalBackfillFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var syncs atomic.Int64
			observeDelayedReclaimSync(t, db, func(_ int) {
				syncs.Add(1)
				if kind == "pressure" || kind == "hard_cap" || kind == "disabled" {
					cancel() // the real SQL dispatch proves this policy permits work
				}
			})
			lane := &fakeBuildLane{}
			lane.install(s)
			lane.held.Store(true)
			res := &walReclaimResult{urgent: true, pressure: kind == "pressure", hardCap: kind == "hard_cap"}
			if kind == "cancelled" {
				cancel()
			}
			err := s.reclaimWALInLaneOpenGate(ctx, walReclaimConfig{}, db, res)
			t.Logf("kind=%s actual_VFS_syncs=%d error=%v outcome=%s reason=%q", kind, syncs.Load(), err, res.outcome, res.reason)
			switch kind {
			case "ordinary":
				require.ErrorIs(t, err, errWALCheckpointYieldedToCycle)
				require.Zero(t, syncs.Load(), "real checkpoint SQL ran inside the held cycle")
			case "cancelled":
				require.ErrorIs(t, err, context.Canceled)
				require.Zero(t, syncs.Load())
			case "disabled":
				require.False(t, errors.Is(err, errWALCheckpointYieldedToCycle))
				require.Positive(t, syncs.Load(), "explicit yield opt-out did not permit SQL")
			default:
				require.False(t, errors.Is(err, errWALCheckpointYieldedToCycle))
				require.Positive(t, syncs.Load(), "force/pressure path was incorrectly disabled")
			}
		})
	}
}
