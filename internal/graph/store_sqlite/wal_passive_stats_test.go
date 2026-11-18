package store_sqlite

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type passiveCounterWire struct {
	Attempts, Complete, Incomplete, ZeroProgress         int64
	RefusedLease, RefusedInflight, FramesBackfilledTotal int64
	LastOutcome                                          string
}

func readPassiveCounterWire(t *testing.T, s *Store) passiveCounterWire {
	t.Helper()
	// Keep the regression executable against the old producer/API. These
	// counters must appear after actual background-path work, not a fake
	// checkpointWALPassive call or a not-yet-added Go field's compile error.
	method := reflect.ValueOf(s).MethodByName("WALPassiveStats")
	require.True(t, method.IsValid(), "real background PASSIVE work has no counter snapshot")
	values := method.Call(nil)
	require.Len(t, values, 1)
	data, err := json.Marshal(values[0].Interface())
	require.NoError(t, err)
	var wire passiveCounterWire
	require.NoError(t, json.Unmarshal(data, &wire))
	return wire
}

func runPassiveCounterFixture(s *Store, db *sql.DB) (bool, bool) {
	// The actual background-loop producer with its privately owned test pool.
	checkpoint := passiveCheckpointer{store: s, db: db}
	return checkpoint.run(false)
}

func TestPeriodicPassiveStatsCountRealBackgroundTransitions(t *testing.T) {
	t.Run("complete", func(t *testing.T) {
		s, db := finalBackfillFixture(t)
		complete, retry := runPassiveCounterFixture(s, db)
		require.True(t, complete)
		require.False(t, retry)
		st := readPassiveCounterWire(t, s)
		require.EqualValues(t, 1, st.Attempts)
		require.EqualValues(t, 1, st.Complete)
		require.Zero(t, st.Incomplete)
		require.Positive(t, st.FramesBackfilledTotal)
	})

	t.Run("incomplete_then_zero_progress", func(t *testing.T) {
		s, db := finalBackfillFixture(t)
		pinned, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err)
		defer func() { _ = pinned.Rollback() }()
		var count int
		require.NoError(t, pinned.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&count))
		require.NoError(t, churnWriteOnce(s, 1))
		for range 2 {
			complete, retry := runPassiveCounterFixture(s, db)
			require.False(t, complete, "the actual old reader must prevent complete backfill")
			require.True(t, retry)
		}
		st := readPassiveCounterWire(t, s)
		require.EqualValues(t, 2, st.Attempts)
		require.EqualValues(t, 2, st.Incomplete)
		require.Positive(t, st.ZeroProgress, "the second identical prefix is no new backfill, even when SQLite repeats a positive cumulative count")
		require.Positive(t, st.FramesBackfilledTotal)
	})

	t.Run("lease_refusal", func(t *testing.T) {
		s, db := finalBackfillFixture(t)
		lease, err := s.acquireGenerationBulkCheckpointLease()
		require.NoError(t, err)
		defer s.releaseGenerationBulkCheckpointLease(lease)
		complete, retry := runPassiveCounterFixture(s, db)
		require.False(t, complete)
		require.True(t, retry)
		st := readPassiveCounterWire(t, s)
		require.Zero(t, st.Attempts, "a refused lease never admits the SQL producer")
		require.EqualValues(t, 1, st.RefusedLease)
	})

	t.Run("inflight_refusal", func(t *testing.T) {
		s, db := finalBackfillFixture(t)
		held, err := s.beginBackgroundCheckpointAttempt(checkpointYieldsToCycle)
		require.NoError(t, err)
		defer s.finishBackgroundCheckpointAttempt(held)
		complete, retry := runPassiveCounterFixture(s, db)
		require.False(t, complete)
		require.True(t, retry)
		st := readPassiveCounterWire(t, s)
		require.Zero(t, st.Attempts)
		require.EqualValues(t, 1, st.RefusedInflight)
	})
}

func TestPeriodicPassiveStatsKeepFailureAndDeferralTruthful(t *testing.T) {
	t.Run("driver_failure", func(t *testing.T) {
		s, db := finalBackfillFixture(t)
		require.NoError(t, db.Close())
		complete, retry := runPassiveCounterFixture(s, db)
		require.False(t, complete)
		require.False(t, retry)
		st := s.WALPassiveStats()
		require.EqualValues(t, 1, st.Attempts)
		require.EqualValues(t, 1, st.Failed)
		require.Zero(t, st.Complete)
		require.Zero(t, st.ZeroProgress, "a failed SQL call is not an observed zero-progress checkpoint")
	})
	t.Run("build_lane_episode_and_reclaim_owner", func(t *testing.T) {
		s, db := finalBackfillFixture(t)
		logs := captureReclaimLog(t)
		lane := &fakeBuildLane{}
		lane.install(s)
		lane.held.Store(true)
		now := time.Now().Add(-time.Minute)
		schedule := newWALCheckpointSchedule(now, time.Hour, 1)
		schedule.passive = &s.walPassive
		checkpoint := passiveCheckpointer{store: s, db: db}
		for _, elapsed := range []time.Duration{0, time.Second} {
			require.False(t, schedule.attemptYielding(now.Add(elapsed), s.dbPath+"-wal", s.cycleLane(), checkpoint.run))
		}
		held := s.WALPassiveStats()
		require.EqualValues(t, 2, held.DeferredBuildLane)
		require.Zero(t, held.Attempts)
		require.GreaterOrEqual(t, held.LongestBuildLaneDeferral, time.Minute)
		lane.held.Store(false)
		schedule.attemptYielding(now.Add(3*time.Second), s.dbPath+"-wal", s.cycleLane(), checkpoint.run)
		st := s.WALPassiveStats()
		require.EqualValues(t, 1, st.Attempts)
		require.EqualValues(t, 1, st.Complete)
		require.Equal(t, 3*time.Second, st.LongestBuildLaneDeferral, "completed episode excludes the SQL call")
		require.Equal(t, 1, strings.Count(logs.String(), "deferral ended mode=PASSIVE"))
		s.walReclaim.cycle.loopThreshold.Store(1)
		require.NoError(t, churnWriteOnce(s, 1))
		schedule.attemptYielding(now.Add(time.Hour), s.dbPath+"-wal", s.cycleLane(), checkpoint.run)
		require.EqualValues(t, 1, s.WALPassiveStats().StoodAsideReclaim)
		require.EqualValues(t, 1, s.WALPassiveStats().Attempts)
	})
	t.Run("success_log_rate", func(t *testing.T) {
		s, db := finalBackfillFixture(t)
		logs := captureReclaimLog(t)
		for range 2 {
			require.NoError(t, churnWriteOnce(s, 1))
			complete, retry := runPassiveCounterFixture(s, db)
			require.True(t, complete)
			require.False(t, retry)
		}
		require.EqualValues(t, 2, s.WALPassiveStats().Complete)
		require.Equal(t, 1, strings.Count(logs.String(), "wal checkpoint complete mode=PASSIVE"), "counter advances every attempt; repeated success log is rate limited")
	})
}

func TestPeriodicPassiveYieldWinsAfterCompletedSQL(t *testing.T) {
	s, db := finalBackfillFixture(t)
	type leaseResult struct {
		lease generationBulkCheckpointLease
		err   error
	}
	done := make(chan leaseResult, 1)
	prior := walCheckpointResultObserver
	walCheckpointResultObserver = func(mode string, _ time.Time, _ time.Duration, _ walCheckpointResult, _ error) {
		if mode != "PASSIVE" {
			return
		}
		// Actual generation admission cancels the registered producer after SQL
		// returns, before the producer settles its outcome. The lease owner waits
		// for that producer to finish; do not block it by awaiting the lease here.
		s.backgroundCheckpoint.mu.Lock()
		attempt := s.backgroundCheckpoint.active
		s.backgroundCheckpoint.mu.Unlock()
		go func() { lease, err := s.acquireGenerationBulkCheckpointLease(); done <- leaseResult{lease, err} }()
		select {
		case <-attempt.ctx.Done():
		case <-time.After(5 * time.Second):
			t.Error("generation admission did not cancel producer")
		}
	}
	defer func() { walCheckpointResultObserver = prior }()
	complete, retry := runPassiveCounterFixture(s, db)
	require.False(t, complete)
	require.True(t, retry)
	var held leaseResult
	select {
	case held = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("generation admission did not join producer")
	}
	require.NoError(t, held.err)
	defer s.releaseGenerationBulkCheckpointLease(held.lease)
	st := s.WALPassiveStats()
	require.EqualValues(t, 1, st.Attempts)
	require.EqualValues(t, 1, st.YieldCancelled)
	require.Zero(t, st.Complete, "a later coordination cause wins over earlier SQL completion")
	require.Zero(t, st.Failed)
	require.Zero(t, st.Incomplete)
}
