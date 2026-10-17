package store_sqlite

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Checkpoint-lock contention returns unknown frame counts. A small scheduled
// residue must retry that result, not delegate it to a size-gated reclaim loop.
func TestScheduledDrainRetriesCheckpointContention(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", "64")
	type observation struct {
		result  walCheckpointResult
		scanErr error
	}
	observed := make(chan observation, 1)
	var armed atomic.Bool
	previous := walCheckpointResultObserver
	walCheckpointResultObserver = func(mode string, start time.Time, took time.Duration, result walCheckpointResult, scanErr error) {
		if mode == "TRUNCATE" && armed.Load() {
			select {
			case observed <- observation{result, scanErr}:
			default:
			}
		}
		if previous != nil {
			previous(mode, start, took, result, scanErr)
		}
	}
	// Installed before Open and restored after Close joins owned workers.
	t.Cleanup(func() { walCheckpointResultObserver = previous })
	s, db := finalBackfillFixture(t) // reclaim disabled, so a false handoff cannot rescue itself
	growWAL(t, s, 4)
	require.Less(t, walFileSize(s.dbPath+"-wal"), defaultWALReclaimThresholdBytes)
	entered, release := stallReclaimCheckpointSync(t, db)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	parked := make(chan observation, 1)
	go func() {
		result, err := checkpointWALOnceOn(ctx, db, "PASSIVE")
		parked <- observation{result, err}
	}()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	joined := false
	t.Cleanup(func() {
		armed.Store(false)
		cancel()
		unblock()
		if !joined {
			select {
			case <-parked:
			case <-time.After(5 * time.Second):
				t.Error("owned parked PASSIVE did not join")
			}
		}
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("real PASSIVE never acquired its checkpoint lock and entered VFS sync")
	}
	armed.Store(true)
	drainsBefore, handoffsBefore := s.walDrains.Load(), s.walDrainHandoffs.Load()
	t.Cleanup(func() {
		t.Logf("contention drain: completed_delta=%d handoffs_delta=%d WAL_bytes=%d", s.walDrains.Load()-drainsBefore, s.walDrainHandoffs.Load()-handoffsBefore, walFileSize(s.dbPath+"-wal"))
	})
	s.scheduleWALDrain("checkpoint_contention_test")
	var first observation
	select {
	case first = <-observed:
	case <-time.After(10 * time.Second):
		t.Fatal("scheduled drain never attempted its reset")
	}
	require.NoError(t, first.scanErr)
	require.Equal(t, 1, first.result.Busy)
	require.Equal(t, -1, first.result.WALFrames)
	require.Equal(t, -1, first.result.CheckpointedFrames)
	select {
	case <-parked:
		joined = true
		t.Fatal("causal checkpoint lock owner returned before release")
	default:
	}
	t.Logf("actual first TRUNCATE Scan tuple=%+v; lock owner remains parked at VFS sync", first.result)
	unblock()
	select {
	case result := <-parked:
		joined = true
		require.NoError(t, result.scanErr)
	case <-time.After(5 * time.Second):
		t.Fatal("released checkpoint did not finish")
	}
	// Preserve the existing completion observation budget. No retry, writer
	// hold, reader pause, or operation deadline is changed by this fixture.
	waitForCondition(t, "the existing bounded retry completes the small scheduled drain", func() bool {
		return s.walDrains.Load() > drainsBefore && walFileSize(s.dbPath+"-wal") == 0
	})
	require.Equal(t, handoffsBefore, s.walDrainHandoffs.Load())
	var count int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM wal_churn`).Scan(&count))
	require.Equal(t, 4000, count)
}
