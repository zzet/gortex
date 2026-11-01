package indexer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Actual observed endpoints drive the same accumulator as production. No
// expected control duration is obtained by subtracting known laps from a wall.
func completeControlledAttempt(t *testing.T) (*PublicationStamps, time.Time, canonicalPatchEvidence) {
	t.Helper()
	origin := time.Now()
	s := &PublicationStamps{patchLaps: &canonicalPatchLaps{origin: origin, ticket: 1, enqueued: origin}}
	at := func(n int) time.Time { return origin.Add(time.Duration(n) * time.Millisecond) }
	s.markAt(PublicationCycleStarted, at(1))
	s.startAttemptControl(at(1))
	cohort := s.beginCanonicalAdmissionAt(canonicalCohortAdmission, at(2))
	cohort.endAt(at(3))
	batch := s.beginCanonicalAdmissionAt(canonicalBatchAdmission, at(4))
	batch.endAt(at(5))
	s.recordCanonicalLaneWaitAt(false, at(6))
	s.recordCanonicalLaneWaitAt(true, at(7))
	s.switchAttemptControlAt(patchInspection, at(8))
	s.markAt(PublicationPatchExecutionStarted, at(10))
	s.switchAttemptControlAt(patchExecutorControl, at(10))
	for kind := patchGateWait; kind <= patchDerived; kind++ {
		start := 12 + 2*int(kind)
		lap := s.beginPatchLapAt(kind, at(start))
		lap.endAt(at(start + 1))
		if kind == patchDerived {
			s.markAt(PublicationPatchDerivedStarted, at(start))
			s.markAt(PublicationPatchDerivedFinished, at(start+1))
		}
	}
	analysis := s.beginPatchLapAt(patchBookkeeping, at(26))
	analysis.endAt(at(27))
	s.markAt(PublicationPatchExecutionFinished, at(28))
	s.switchAttemptControlAt(patchAttemptFinalize, at(28))
	s.mu.Lock()
	early := s.patchLaps.freeze(origin)
	s.patchLaps.finishAttemptControlLocked(at(31))
	s.mu.Unlock()
	return s, origin, early
}

func TestCanonicalAttemptControlUsesOrderedActualScopesAndKeepsEarlyDeepCopy(t *testing.T) {
	s, origin, early := completeControlledAttempt(t)
	require.False(t, early.invalid, "an open outer finalization cannot invalidate completed executor laps")
	s.mu.Lock()
	late := s.patchLaps.freeze(origin)
	counters := *s.patchLaps
	s.mu.Unlock()
	require.False(t, counters.invalid)
	var total time.Duration
	for _, lap := range counters.laps {
		total += lap.total
		require.Equal(t, lap.begun, lap.done)
	}
	require.Equal(t, 4*time.Millisecond, counters.laps[patchAttemptSetup].total)
	require.Equal(t, 2*time.Millisecond, counters.laps[patchInspection].total)
	require.Equal(t, 10*time.Millisecond, counters.laps[patchExecutorControl].total)
	require.Equal(t, 3*time.Millisecond, counters.laps[patchAttemptFinalize].total)
	require.Equal(t, 30*time.Millisecond, total)
	require.Equal(t, early, late, "late control completion never rewrites the early deep evidence")
	offsets := map[PublicationPhase]int64{PublicationTicketEnqueued: 0, PublicationPatchExecutionStarted: 10e6, PublicationPatchExecutionFinished: 28e6, PublicationPatchDerivedStarted: 24e6, PublicationPatchDerivedFinished: 25e6}
	out := EditPhaseTimings{Milliseconds: map[string]*float64{}, States: map[string]string{}}
	early.project(offsets, false, &out)
	for _, name := range canonicalPatchLapNames {
		require.Equal(t, "measured", out.States[name], name)
		require.Equal(t, 1.0, *out.Milliseconds[name], name)
	}
}

func TestCanonicalAttemptControlRealReturnStartsAfterInitialQuiet(t *testing.T) {
	path, idx, w := pointPublicationFixture(t)
	w.config.DebounceMs = 20
	var local *PublicationStamps
	w.pointReindexRaw = func(file string, s *PublicationStamps) (*IndexResult, error) {
		local = s
		return idx.incrementalPointWatcherPathObserved(idx.rootPath, file, s)
	}
	record := newPointPublication(t, "full-control")
	ticket, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
	require.NoError(t, err)
	require.NoError(t, awaitMutationRetryResult(t, ticket).Err)
	require.Eventually(t, func() bool { return record.Snapshot().attemptWindow != nil }, 2*time.Second, time.Millisecond)
	snapshot := record.Snapshot()
	require.Equal(t, snapshot.debounce.enteredNS, snapshot.attemptWindow.startNS, "initial quiet is outside P")
	local.mu.Lock()
	invalid, finished := local.patchLaps.invalid, local.patchLaps.controlFinished
	var total int64
	for _, lap := range local.patchLaps.laps {
		total += lap.total.Nanoseconds()
	}
	local.mu.Unlock()
	require.False(t, invalid)
	require.True(t, finished)
	require.Equal(t, snapshot.attemptWindow.endNS-snapshot.attemptWindow.startNS, total, "every actual callback/control/operation interval is disjoint and owned")
	require.Equal(t, "measured", snapshot.EditTimings().States["derived"])
}

func TestCanonicalAttemptControlRefusesOverlappingAndReversedObservations(t *testing.T) {
	for _, overlap := range []bool{false, true} {
		t.Run(map[bool]string{false: "reversed", true: "overlap"}[overlap], func(t *testing.T) {
			origin := time.Now()
			s := &PublicationStamps{patchLaps: &canonicalPatchLaps{}}
			s.startAttemptControl(origin)
			lap := s.beginPatchLapAt(patchParse, origin.Add(time.Millisecond))
			if overlap {
				s.beginPatchLapAt(patchApply, origin.Add(2*time.Millisecond))
			} else {
				s.switchAttemptControlAt(patchInspection, origin.Add(-time.Millisecond))
			}
			lap.endAt(origin.Add(3 * time.Millisecond))
			s.mu.Lock()
			invalid := s.patchLaps.invalid
			s.mu.Unlock()
			require.True(t, invalid)
		})
	}
}

func TestCanonicalAttemptControlNilAndUnobservedScopeDoesNotAllocate(t *testing.T) {
	for _, s := range []*PublicationStamps{nil, {}, {patchLaps: &canonicalPatchLaps{}}} {
		var switched bool
		allocations := testing.AllocsPerRun(100, func() { switched = s.switchAttemptControl(patchInspection) })
		require.False(t, switched)
		require.Zero(t, allocations)
	}
}
