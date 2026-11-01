package indexer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCanonicalAdmissionObservesRealCohortGrant(t *testing.T) {
	path, _, w := pointPublicationFixture(t)
	slots := w.mutationSlots()
	for i := 0; i < cap(slots); i++ {
		slots <- struct{}{}
	}
	t.Cleanup(func() {
		if !w.mutationAdmissionStopped() {
			require.NoError(t, w.Stop())
		}
		for len(slots) > 0 {
			<-slots
		}
	})
	entered := make(chan struct{}, 1)
	w.mutationAdmissionWaitFn = func() time.Duration {
		select {
		case entered <- struct{}{}:
		default:
		}
		return 5 * time.Second
	}
	record := newPointPublication(t, "cohort-admission")
	ticket, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
	require.NoError(t, err)
	awaitPublicationSignal(t, entered)
	require.Nil(t, record.Snapshot().admissions, "blocked unvalidated attempt cannot publish")
	grantedAfter := time.Now()
	<-slots // actual occupied semaphore cannot grant before this release
	require.NoError(t, awaitMutationRetryResult(t, ticket).Err)
	snapshot := record.Snapshot()
	require.NotNil(t, snapshot.admissions)
	pair := snapshot.admissions.pairs[canonicalCohortAdmission]
	require.True(t, pair.started)
	require.True(t, pair.finished)
	require.GreaterOrEqual(t, pair.endNS, grantedAfter.Sub(record.origin).Nanoseconds())
	require.Equal(t, "measured", snapshot.EditTimings().States["cohort_admission"])
	require.NotNil(t, snapshot.EditTimings().Milliseconds["cohort_admission"])
}

func TestCanonicalAdmissionObservesRealBatchLockBeforeLane(t *testing.T) {
	record := newPointPublication(t, "batch-admission")
	require.True(t, record.bindCanonicalTicket(1, time.Now()))
	stamps := record.newCanonicalPatchStamps()
	stamps.markAt(PublicationCycleStarted, time.Now())
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), publicationStampsKey{}, stamps))
	var gate sync.RWMutex
	gate.Lock()
	var once sync.Once
	release := func() { once.Do(gate.Unlock) }
	coordinator := newRepositoryMutationCoordinator(nil)
	coordinator.batchMutationGate = &gate
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
			t.Error("batch admission did not join")
		}
	})
	require.Eventually(t, func() bool {
		stamps.mu.Lock()
		defer stamps.mu.Unlock()
		return !stamps.admissions[canonicalBatchAdmission].started.IsZero()
	}, 2*time.Second, time.Millisecond)
	stamps.mu.Lock()
	unfinished := stamps.admissions[canonicalBatchAdmission].finished.IsZero()
	laneNotStarted := stamps.laneWait.started.IsZero()
	stamps.mu.Unlock()
	require.True(t, unfinished)
	require.True(t, laneNotStarted, "batch lock must not be charged to lane_wait")
	grantedAfter := time.Now()
	release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("batch lock did not grant")
	}
	record.Absorb(stamps)
	record.Mark(PublicationTicketCompleted)
	snapshot := record.Snapshot()
	pair := snapshot.admissions.pairs[canonicalBatchAdmission]
	require.GreaterOrEqual(t, pair.endNS, grantedAfter.Sub(record.origin).Nanoseconds())
	require.LessOrEqual(t, pair.endNS, snapshot.laneWait.startNS)
	require.Equal(t, "measured", snapshot.EditTimings().States["batch_admission"])
}

func TestCanonicalAdmissionProjectionRejectsOverlapAndMissingOwners(t *testing.T) {
	for _, name := range []string{"measured", "zero", "cohort_batch_overlap", "batch_lane_overlap", "before_cycle", "after_executor", "missing_executor", "missing_terminal", "outside_terminal", "incomplete", "negative", "repeated"} {
		t.Run(name, func(t *testing.T) {
			var p canonicalAdmissionEvidence
			p.pairs[0].started, p.pairs[0].finished = true, true
			p.pairs[0].startNS, p.pairs[0].endNS = 2, 3
			p.pairs[1].started, p.pairs[1].finished = true, true
			p.pairs[1].startNS, p.pairs[1].endNS = 4, 5
			lane := canonicalLaneWaitEvidence{started: true, acquired: true, startNS: 6, acquiredNS: 7}
			offsets := map[PublicationPhase]int64{PublicationTicketEnqueued: 0, PublicationCycleStarted: 1, PublicationPatchExecutionStarted: 8, PublicationTicketCompleted: 9}
			terminal := true
			want := "invalid_order"
			switch name {
			case "measured":
				want = "measured"
			case "zero":
				p.pairs[0].endNS = 2
				want = "measured"
			case "cohort_batch_overlap":
				p.pairs[0].endNS = 5
			case "batch_lane_overlap":
				p.pairs[1].endNS = 7
			case "before_cycle":
				p.pairs[0].startNS = 0
			case "after_executor":
				offsets[PublicationPatchExecutionStarted] = 2
			case "missing_executor":
				delete(offsets, PublicationPatchExecutionStarted)
				want = "not_observed"
			case "missing_terminal":
				delete(offsets, PublicationTicketCompleted)
				want = "not_observed"
			case "outside_terminal":
				offsets[PublicationTicketCompleted] = 2
			case "incomplete":
				p.pairs[0].finished = false
				terminal = false
				want = "pending"
			case "negative":
				p.pairs[0].startNS = -1
			case "repeated":
				p.pairs[0].invalid = true
			}
			out := EditPhaseTimings{Milliseconds: map[string]*float64{}, States: map[string]string{}}
			p.project(offsets, terminal, &lane, &out)
			require.Equal(t, want, out.States["cohort_admission"])
			if want == "measured" {
				require.NotNil(t, out.Milliseconds["cohort_admission"])
			} else {
				require.Nil(t, out.Milliseconds["cohort_admission"])
			}
		})
	}
}

func TestCanonicalAdmissionNilAndForeignObserversDoNotAllocate(t *testing.T) {
	for _, stamps := range []*PublicationStamps{nil, {}} {
		var span canonicalAdmissionSpan
		allocations := testing.AllocsPerRun(100, func() { span = stamps.beginCanonicalAdmission(canonicalCohortAdmission); span.end() })
		require.Nil(t, span.stamps)
		require.Zero(t, allocations)
	}
}
