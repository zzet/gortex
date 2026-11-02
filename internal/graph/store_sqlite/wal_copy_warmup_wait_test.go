package store_sqlite

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

// A warm producer can enter its boundary after an ordinary pressure copy has
// already spent the editing bucket. Its wait must let that same real copy
// finish, without raising the configured background budget or WAL mark.
func TestBlockedWarmupResumesAPacedCopy(t *testing.T) {
	s, _ := warmupWALFixture(t)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)
	defer lane.held.Store(false)
	require.NoError(t, churnWriteOnce(s, 0))
	require.NoError(t, churnWriteOnce(s, 1))
	require.Greater(t, s.retirementLogBytes(), int64(1<<20))
	require.Less(t, s.retirementLogBytes(), int64(4<<20))
	setCopyBudget(t, 1<<20, 16<<10, 0)
	t.Setenv("GORTEX_SQLITE_WAL_COPY_BUDGET_MB", "1")
	s.walCopy.sawBusy(time.Now())
	startWarmupFixtureReclaimer(t, s, "32")
	s.RequestWALReclaim()
	require.Eventually(t, func() bool {
		s.walCopy.mu.Lock()
		defer s.walCopy.mu.Unlock()
		return s.walCopy.tokens < 0
	}, 2*time.Second, time.Millisecond, "the real page-copy callback never spent the editing bucket")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	require.NoError(t, s.AwaitWarmupWAL(ctx), "blocked warm producer remained on the editing copy budget")
	require.Zero(t, s.warmupWALWaiters.Load())
	require.Less(t, s.retirementLogBytes(), int64(1<<20))
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&count))
	require.Equal(t, 4000, count)
}

func TestBlockedWarmupCopyHintKeepsForegroundEditingBudget(t *testing.T) {
	s, _ := warmupWALFixture(t)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)
	defer lane.held.Store(false)
	setCopyBudget(t, 1<<20, 16<<10, 0)
	s.warmupWALWaiters.Add(1)
	defer s.warmupWALWaiters.Add(-1)
	synctest.Test(t, func(t *testing.T) {
		release := s.AnnounceWrite()
		defer release()
		s.walCopy.sawBusy(time.Now())
		reserved := s.walCopy.take(time.Now(), 32<<10, float64(1<<20)/60)
		require.Greater(t, reserved, 100*time.Millisecond)
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		pacer := walCopyPacer{store: s, ctx: ctx, stop: make(chan struct{}), rate: float64(1<<20) / 60,
			noPause: true, hardCapBytes: 4 << 20, walPath: s.dbPath + "-wal"}
		pacer.beforeWrite(4096)
		require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded, "warm hint bypassed an announced foreground writer's budget")
		require.GreaterOrEqual(t, pacer.budgetNs, 100*time.Millisecond)
		require.False(t, pacer.overPacingMark)
	})
}
