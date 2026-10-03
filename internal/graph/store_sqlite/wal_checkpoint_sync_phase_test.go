package store_sqlite

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This control parks at the real VFS xSync boundary;
// it does not claim to manufacture an actual Windows FlushFileBuffers delay.
// It demonstrates why cancellation dispatch and durable SQL return are distinct.
func TestCheckpointYieldDistinguishesBlockedSyncFromPageWork(t *testing.T) {
	s, db := finalBackfillFixture(t)
	growWAL(t, s, 4)
	lane := &fakeBuildLane{}
	lane.install(s)
	interruptInsteadOfPause(t) // existing control: actual cancellation, not copy-pause exemption
	type phase struct{ start, end time.Time }
	var mu sync.Mutex
	var phases []phase
	entered, release := stallReclaimCheckpointSync(t, db, func(_ int, start, end time.Time) {
		mu.Lock()
		phases = append(phases, phase{start, end})
		mu.Unlock()
	})
	attempt, err := s.beginBackgroundCheckpointAttempt(checkpointYieldsToCycle)
	require.NoError(t, err)
	attempt.copy.pausable.Store(true)
	attempt.copy.walPath = s.dbPath + "-wal"
	slack := 60 * time.Millisecond
	if raceDetectorOn {
		slack = 250 * time.Millisecond
	}
	stopBound := walCheckpointCycleYieldPoll + slack // exact existing edit-cycle oracle
	var releaseOnce sync.Once
	unpark := func() { releaseOnce.Do(func() { close(release) }) }
	done := make(chan error, 1)
	joined := make(chan struct{})
	t.Cleanup(func() {
		attempt.cancel(context.Canceled)
		unpark()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("owned checkpoint did not join after cancel/release")
		}
	})
	go func() {
		_, err := s.pacedPassive(attempt.ctx, db, attempt)
		s.finishBackgroundCheckpointAttempt(attempt)
		done <- err
		close(joined)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("checkpoint did not reach actual VFS sync boundary")
	}
	before := vfsIOMark()
	editStart := time.Now()
	lane.held.Store(true)
	defer lane.held.Store(false)
	select {
	case <-attempt.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("yield watcher did not deliver cancellation")
	}
	cancelObserved := time.Since(editStart)
	require.ErrorIs(t, context.Cause(attempt.ctx), errWALCheckpointYieldedToCycle)
	require.LessOrEqual(t, cancelObserved, stopBound)
	// Preserve a meaningful foreground bound, measured over admission AND SQL.
	writerCtx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	writerStart := time.Now()
	lockErr := s.writeMu.LockContext(writerCtx)
	var writeErr error
	if lockErr == nil {
		_, writeErr = s.writerDB.ExecContext(writerCtx, `UPDATE wal_churn SET payload='foreground-during-sync' WHERE id=1`)
		s.writeMu.Unlock()
	}
	writerTook := time.Since(writerStart)
	cancel()
	t.Logf("cancel_observed=%s foreground_gate_and_SQL=%s gate_error=%v SQL_error=%v", cancelObserved, writerTook, lockErr, writeErr)
	require.NoError(t, lockErr)
	require.NoError(t, writeErr)
	require.LessOrEqual(t, writerTook, 500*time.Millisecond)
	// Make the original wall-return oracle fail positively, without increasing it.
	select {
	case <-done:
		t.Fatal("checkpoint returned while xSync was parked")
	case <-time.After(max(0, 400*time.Millisecond-time.Since(editStart))):
	}
	unpark()
	var checkpointErr error
	select {
	case checkpointErr = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("checkpoint did not return after sync release")
	}
	ended := time.Now()
	rawAfterEdit := ended.Sub(editStart)
	mu.Lock()
	var syncAfterEdit time.Duration
	for _, p := range phases {
		// Only a positively recorded xSync entered before the edit qualifies.
		// Later syncs and unknown/unobserved time cannot waive the oracle.
		if p.start.After(editStart) {
			continue
		}
		lo, hi := maxTime(editStart, p.start), minTime(ended, p.end)
		if hi.After(lo) {
			syncAfterEdit += hi.Sub(lo)
		}
	}
	mu.Unlock()
	workAfterEdit := rawAfterEdit - syncAfterEdit
	t.Logf("raw_return_after_edit=%s known_VFS_sync=%s outside_known_sync=%s checkpoint_error=%v", rawAfterEdit, syncAfterEdit, workAfterEdit, checkpointErr)
	require.Greater(t, rawAfterEdit, stopBound, "old wall-only late-stop oracle must fail this actual VFS control")
	require.Positive(t, syncAfterEdit)
	require.LessOrEqual(t, workAfterEdit, stopBound)
	// The page-work proof is independent of time subtraction. This one control
	// owns all nonwriter DB page writes; no maintenance/reclaim observer supplies a reset.
	delta := vfsIOMark().Since(before)
	require.Zero(t, delta.OtherMainWriteBytes, "cancellation must not admit further checkpoint page writes")
	require.Zero(t, s.WALCopyStats().WrittenWhileBusyBytes)
	require.Error(t, checkpointErr)
	// End the simulated edit before the recovery copy/reset.
	lane.held.Store(false)
	// Confirm ordinary durable completion and authoritative reset after the
	// canceled call has joined. These SQL tuples refer to their own current pass;
	// no cross-reset NBackfill/CheckpointSeq comparison supplies a proof.
	recoveryCtx, recoveryCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer recoveryCancel()
	copied, copyErr := checkpointWALOnceOn(recoveryCtx, db, "PASSIVE")
	require.NoError(t, copyErr)
	require.Positive(t, copied.WALFrames)
	require.Equal(t, copied.WALFrames, copied.CheckpointedFrames)
	_, resetErr := checkpointWALOnceOn(recoveryCtx, db, "TRUNCATE")
	require.NoError(t, resetErr)
	require.True(t, walLogIsReset(s.dbPath, s.dbPath+"-wal"))
	var payload string
	require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id=1`).Scan(&payload))
	require.Equal(t, "foreground-during-sync", payload)
}

// A probe qualifies exact checkpoint TLS and Store identity at sync entry and
// return. The same immutable pacer must remain registered throughout xSync.
// Unknown or recreated connections remain subject to the original wall oracle.
type reclaimCheckpointSyncProbe struct {
	store   *Store
	observe func(reclaimCheckpointSyncInterval)
}

var reclaimCheckpointSyncProbeState atomic.Pointer[reclaimCheckpointSyncProbe]

func reclaimCheckpointSyncProbeAt(tls uintptr) (*reclaimCheckpointSyncProbe, *walCopyPacer) {
	probe := reclaimCheckpointSyncProbeState.Load()
	if probe == nil || tls == 0 {
		return nil, nil
	}
	entry, ok := walCopyPacers.Load(tls)
	if !ok {
		return nil, nil
	}
	pacer, ok := entry.(*walCopyPacer)
	if !ok || pacer.store != probe.store {
		return nil, nil
	}
	return probe, pacer
}
