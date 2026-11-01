package store_sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWALReclaimUrgentResetYieldsToEditBurst(t *testing.T) {
	for _, kind := range []string{"announced", "build_lane"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			var release func()
			if kind == "announced" {
				release = s.AnnounceWrite()
			} else {
				lane := &fakeBuildLane{}
				lane.install(s)
				lane.held.Store(true)
				release = func() { lane.held.Store(false) }
			}
			defer release()
			before := s.WALReclaimStats().WriterHold.Count()
			res := &walReclaimResult{urgent: true}
			done, _ := s.reclaimWALResetHold(t.Context(), walReclaimConfig{}, db, res, true, false)
			require.False(t, done, "urgent reclaim reset during an active edit burst")
			require.Equal(t, before, s.WALReclaimStats().WriterHold.Count(), "urgent reclaim acquired the writer during an active edit burst")
			release()
			done, err := s.reclaimWALResetHold(t.Context(), walReclaimConfig{}, db, &walReclaimResult{urgent: true}, true, false)
			require.NoError(t, err)
			require.True(t, done, "the same owned WAL must reset after the burst ends")
			require.True(t, walLogIsReset(s.dbPath, s.dbPath+"-wal"))
		})
	}
}

func TestWALReclaimUrgentRefusesQueuedWriter(t *testing.T) {
	s, db := finalBackfillFixture(t)
	s.writeMu.Lock()
	writerDone := make(chan struct{})
	go func() { s.writeMu.Lock(); s.writeMu.Unlock(); close(writerDone) }()
	require.Eventually(t, func() bool { return s.writeMu.waiting() > 0 }, time.Second, time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := s.reclaimWALResetHoldOnce(ctx, walReclaimConfig{}, db, &walReclaimResult{urgent: true}, true, false, walReclaimResetHold)
	s.writeMu.Unlock()
	<-writerDone
	require.ErrorIs(t, err, errWALReclaimWriterWaiting, "urgent reclaim must refuse queued edits before writer admission")
}

func TestWALReclaimPressureDrainDefersDuringBuild(t *testing.T) {
	s, db := finalBackfillFixture(t)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)
	defer lane.held.Store(false)
	res := &walReclaimResult{urgent: true, pressure: true}
	err := s.reclaimWALPressureResetAfterReaderDrain(t.Context(), db, res, 100*time.Millisecond)
	require.Error(t, err, "pressure reclaim must defer before closing the read gate")
	require.False(t, res.pauseClosed, "pressure reclaim closed the read gate during a build")
	require.Zero(t, res.writerHolds)
}
