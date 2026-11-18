package indexer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCanonicalLaneWaitObservesActualBlockedGrantAfterBatchAdmission(t *testing.T) {
	origin := time.Now()
	record := NewPublicationPhaseRecorder(1, 1).Begin("", "lane", PublicationSourceMCPEdit, origin)
	require.True(t, record.bindCanonicalTicket(1, time.Now()))
	stamps := record.newCanonicalPatchStamps()
	stamps.markAt(PublicationCycleStarted, time.Now())
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), publicationStampsKey{}, stamps))
	coordinator := newRepositoryMutationCoordinator(nil)
	<-coordinator.lane // real lane occupied before this attempt enters
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { coordinator.lane <- struct{}{} }) }
	hookSawStart := false
	coordinator.batchAdmissionHook = func() {
		stamps.mu.Lock()
		hookSawStart = !stamps.laneWait.started.IsZero()
		stamps.mu.Unlock()
	}
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- coordinator.runExclusive(ctx, func() error {
			stamps.markAt(PublicationPatchExecutionStarted, time.Now())
			stamps.markAt(PublicationPatchExecutionFinished, time.Now())
			return nil
		})
	}()
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("lane observation did not join")
		}
	})
	require.Eventually(t, func() bool { stamps.mu.Lock(); defer stamps.mu.Unlock(); return !stamps.laneWait.started.IsZero() }, 2*time.Second, time.Millisecond)
	grantedAfter := time.Now()
	release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("occupied lane did not grant")
	}
	require.False(t, hookSawStart, "batch admission/hook work is outside pure lane wait")
	record.Absorb(stamps)
	record.Mark(PublicationTicketCompleted)
	snapshot := record.Snapshot()
	require.NotNil(t, snapshot.laneWait)
	require.True(t, snapshot.laneWait.acquired)
	require.GreaterOrEqual(t, snapshot.laneWait.acquiredNS, grantedAfter.Sub(origin).Nanoseconds())
	timing := snapshot.EditTimings()
	require.Equal(t, "measured", timing.States["lane_wait"])
	require.NotNil(t, timing.Milliseconds["lane_wait"])
	require.Nil(t, timing.Window.PatchWallMS, "the lane pair is not a whole patch wall")
}

func TestCanonicalLaneWaitCancellationHasNoGrant(t *testing.T) {
	origin := time.Now()
	stamps := &PublicationStamps{patchLaps: &canonicalPatchLaps{}}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), publicationStampsKey{}, stamps))
	coordinator := newRepositoryMutationCoordinator(nil)
	<-coordinator.lane
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- coordinator.runExclusive(ctx, func() error { t.Error("canceled wait entered executor"); return nil })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("canceled lane wait did not join")
		}
		coordinator.lane <- struct{}{}
	})
	require.Eventually(t, func() bool { stamps.mu.Lock(); defer stamps.mu.Unlock(); return !stamps.laneWait.started.IsZero() }, 2*time.Second, time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("lane wait ignored cancellation")
	}
	stamps.mu.Lock()
	evidence := stamps.laneWait.freeze(origin)
	stamps.mu.Unlock()
	require.True(t, evidence.started)
	require.False(t, evidence.acquired)
	out := EditPhaseTimings{Milliseconds: map[string]*float64{}, States: map[string]string{}}
	evidence.project(nil, true, &out)
	require.Nil(t, out.Milliseconds["lane_wait"], "a canceled attempt cannot report a zero-duration grant")
	require.Equal(t, "not_observed", out.States["lane_wait"])
}

func TestCanonicalLaneWaitDoesNotObserveForeignOrUninstrumentedContexts(t *testing.T) {
	empty := context.WithValue(context.Background(), publicationStampsKey{}, &PublicationStamps{})
	for _, ctx := range []context.Context{context.Background(), empty} {
		allocs := testing.AllocsPerRun(100, func() {
			token := beginCanonicalLaneWait(ctx)
			if token != nil {
				t.Fatal("noncanonical context observed a lane")
			}
			token.canonicalLaneAcquired()
		})
		require.Zero(t, allocs)
	}
}

func TestCanonicalLaneWaitProjectionRequiresOwnedContainingBoundaries(t *testing.T) {
	valid := canonicalLaneWaitEvidence{started: true, acquired: true, startNS: int64(4 * time.Millisecond), acquiredNS: int64(5 * time.Millisecond)}
	for _, tc := range []struct {
		name   string
		change func(*canonicalLaneWaitEvidence, map[PublicationPhase]int64)
		state  string
	}{
		{name: "valid", state: "measured"},
		{name: "actual zero", state: "measured", change: func(v *canonicalLaneWaitEvidence, _ map[PublicationPhase]int64) { v.acquiredNS = v.startNS }},
		{name: "before cycle", state: "invalid_order", change: func(v *canonicalLaneWaitEvidence, _ map[PublicationPhase]int64) { v.startNS = 0 }},
		{name: "after executor", state: "invalid_order", change: func(v *canonicalLaneWaitEvidence, _ map[PublicationPhase]int64) {
			v.acquiredNS = int64(9 * time.Millisecond)
		}},
		{name: "reversed", state: "invalid_order", change: func(v *canonicalLaneWaitEvidence, _ map[PublicationPhase]int64) { v.acquiredNS = 1 }},
		{name: "missing grant", state: "not_observed", change: func(v *canonicalLaneWaitEvidence, _ map[PublicationPhase]int64) { v.acquired = false }},
		{name: "missing enqueue", state: "not_observed", change: func(_ *canonicalLaneWaitEvidence, m map[PublicationPhase]int64) { delete(m, PublicationTicketEnqueued) }},
		{name: "missing cycle", state: "not_observed", change: func(_ *canonicalLaneWaitEvidence, m map[PublicationPhase]int64) { delete(m, PublicationCycleStarted) }},
		{name: "missing executor", state: "not_observed", change: func(_ *canonicalLaneWaitEvidence, m map[PublicationPhase]int64) {
			delete(m, PublicationPatchExecutionStarted)
		}},
		{name: "terminal before grant", state: "invalid_order", change: func(_ *canonicalLaneWaitEvidence, m map[PublicationPhase]int64) {
			m[PublicationTicketCompleted] = int64(2 * time.Millisecond)
		}},
		{name: "missing terminal", state: "not_observed", change: func(_ *canonicalLaneWaitEvidence, m map[PublicationPhase]int64) {
			delete(m, PublicationTicketCompleted)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pair := valid
			offsets := map[PublicationPhase]int64{PublicationTicketEnqueued: int64(time.Millisecond), PublicationCycleStarted: int64(2 * time.Millisecond), PublicationPatchExecutionStarted: int64(8 * time.Millisecond), PublicationTicketCompleted: int64(10 * time.Millisecond)}
			if tc.change != nil {
				tc.change(&pair, offsets)
			}
			out := EditPhaseTimings{Milliseconds: map[string]*float64{}, States: map[string]string{}}
			pair.project(offsets, true, &out)
			require.Equal(t, tc.state, out.States["lane_wait"])
			if tc.state == "measured" {
				require.NotNil(t, out.Milliseconds["lane_wait"])
			} else {
				require.Nil(t, out.Milliseconds["lane_wait"])
			}
		})
	}
}

func TestCanonicalLaneWaitPhysicalAttemptLogDoesNotRequireTerminal(t *testing.T) {
	pair := canonicalLaneWaitEvidence{started: true, acquired: true, startNS: int64(4 * time.Millisecond), acquiredNS: int64(5 * time.Millisecond)}
	offsets := map[PublicationPhase]int64{PublicationTicketEnqueued: int64(time.Millisecond), PublicationCycleStarted: int64(2 * time.Millisecond), PublicationPatchExecutionStarted: int64(8 * time.Millisecond)}
	out := EditPhaseTimings{Milliseconds: map[string]*float64{}, States: map[string]string{}}
	pair.project(offsets, false, &out)
	require.Equal(t, "measured", out.States["lane_wait"])
	require.Equal(t, 1.0, *out.Milliseconds["lane_wait"])
}
