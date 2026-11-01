package indexer

import (
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
)

func controlledAttemptWindow(t *testing.T) (*PublicationStamps, time.Time, canonicalAttemptWindow) {
	t.Helper()
	s, origin, _ := completeControlledAttempt(t)
	s.mu.Lock()
	evidence := s.freezeAttemptEvidence(origin)
	s.mu.Unlock()
	return s, origin, canonicalAttemptWindow{startNS: 1e6, endNS: 31e6, ticket: 1, accounting: evidence}
}

func TestCanonicalAttemptAccountingProjectsOnlyActualAcceptedP(t *testing.T) {
	_, _, window := controlledAttemptWindow(t)
	p := window.projectAccounting()
	require.EqualValues(t, 1, p.AttemptTicket)
	require.Zero(t, p.AttemptRetry)
	require.True(t, p.PartitionComplete)
	require.Equal(t, "partial", p.SumState, "local complete P is not actual100-edit/5% acceptance")
	require.Equal(t, "accepted_winning_physical_attempt", p.Scope)
	require.Equal(t, "not_established", p.BackgroundCoverage)
	require.Equal(t, 30.0, *p.SubtotalMS)
	require.Equal(t, 0.0, *p.UnattributedMS)
	require.NotContains(t, p.PhasesMS, "debounce", "quiet/backoff cannot be charged against P")
	require.Equal(t, 1.0, *p.PhasesMS["patch_analysis"])
	require.Equal(t, 4.0, *p.PhasesMS["attempt_setup"])
	require.Equal(t, 3.0, *p.PhasesMS["attempt_finalize"])
	for _, name := range canonicalPatchLapNames {
		require.Equal(t, 1.0, *p.PhasesMS[name], name)
	}
	enc := zapcore.NewMapObjectEncoder()
	require.NoError(t, window.MarshalLogObject(enc))
	require.Equal(t, "partial", enc.Fields["sum_state"])
	require.Equal(t, 30.0, enc.Fields["observed_subtotal_ms"])
	*p.PhasesMS["patch_analysis"] = 999
	require.Equal(t, 1.0, *window.projectAccounting().PhasesMS["patch_analysis"], "projections own their scalar pointers/maps")
}

func TestCanonicalAttemptAccountingMissingDeepCannotHideInsideControl(t *testing.T) {
	_, _, window := controlledAttemptWindow(t)
	window.accounting.laps[patchExecutorControl].totalNS += window.accounting.laps[patchParse].totalNS
	window.accounting.laps[patchParse] = canonicalLapEvidence{}
	p := window.projectAccounting()
	require.False(t, p.PartitionComplete)
	require.Equal(t, "missing_laps", p.SumState)
	require.Nil(t, p.PhasesMS["patch_parse"])
	require.Equal(t, 0.0, *p.UnattributedMS, "numeric fullness never repairs absent applicable deep evidence")
}

func TestCanonicalAttemptAccountingRejectsBoundsOverflowDerivedAndOpenChunks(t *testing.T) {
	for _, name := range []string{"invalid_order", "outside_P", "outside_executor", "overflow", "derived_mismatch", "open_chunk", "not_observed", "gap"} {
		t.Run(name, func(t *testing.T) {
			_, _, w := controlledAttemptWindow(t)
			want := "invalid_order"
			switch name {
			case "invalid_order":
				w.accounting.invalid = true
			case "outside_P":
				w.accounting.laps[patchAttemptSetup].firstNS = 0
			case "outside_executor":
				w.accounting.laps[patchParse].firstNS = 9e6
			case "overflow":
				w.accounting.laps[patchExecutorControl].totalNS = 1<<63 - 1
			case "derived_mismatch":
				w.accounting.derivedEnd++
			case "open_chunk":
				w.accounting.laps[patchResolve].done = 0
				want = "missing_laps"
			case "not_observed":
				w.accounting.observed = false
				want = "not_observed"
			case "gap":
				w.accounting.laps[patchExecutorControl].totalNS--
				want = "unclassified_gap"
			}
			p := w.projectAccounting()
			require.False(t, p.PartitionComplete)
			require.Equal(t, want, p.SumState)
			if want == "invalid_order" {
				require.Nil(t, p.SubtotalMS)
				require.Nil(t, p.UnattributedMS)
			}
		})
	}
}

func TestCanonicalAttemptAccountingLateCopyRequiresAgreementWithEarlyCarriers(t *testing.T) {
	for _, name := range []string{"unchanged", "deep_changed", "admission_changed", "lane_changed"} {
		t.Run(name, func(t *testing.T) {
			s, origin, _ := controlledAttemptWindow(t)
			recorder := NewPublicationPhaseRecorder(1, 1)
			t.Cleanup(func() { runtime.KeepAlive(recorder) })
			record := recorder.Begin("", "accounting", PublicationSourceMCPEdit, origin)
			require.True(t, record.bindCanonicalTicket(1, origin))
			record.Absorb(s)
			record.MarkAt(PublicationTicketCompleted, origin.Add(29*time.Millisecond))
			early := record.Snapshot()
			earlyTiming := early.EditTimings()
			require.Nil(t, earlyTiming.Window.PatchAccounting)
			s.mu.Lock()
			switch name {
			case "deep_changed":
				s.patchLaps.laps[patchParse].total++
			case "admission_changed":
				s.admissions[canonicalCohortAdmission].finished = origin.Add(4 * time.Millisecond)
			case "lane_changed":
				s.laneWait.acquired = origin.Add(9 * time.Millisecond)
			}
			s.mu.Unlock()
			_, ok := record.completePhysicalAttempt(s, origin.Add(time.Millisecond), origin.Add(31*time.Millisecond), 0)
			require.True(t, ok, "actual wall can remain known even when classification carriers disagree")
			late := record.Snapshot().EditTimings()
			require.Equal(t, earlyTiming, early.EditTimings(), "no previously exposed snapshot changes")
			if name == "unchanged" {
				require.True(t, late.Window.PatchAccounting.PartitionComplete)
			} else {
				require.False(t, late.Window.PatchAccounting.PartitionComplete)
				require.Equal(t, "invalid_order", late.Window.PatchAccounting.SumState)
			}
		})
	}
}

func TestCanonicalAttemptAccountingRefusesDifferentCapturedCallbackOrigin(t *testing.T) {
	s, origin, _ := controlledAttemptWindow(t)
	recorder := NewPublicationPhaseRecorder(1, 1)
	t.Cleanup(func() { runtime.KeepAlive(recorder) })
	record := recorder.Begin("", "origin-fence", PublicationSourceMCPEdit, origin)
	require.True(t, record.bindCanonicalTicket(1, origin))
	record.Absorb(s)
	_, ok := record.completePhysicalAttempt(s, origin.Add(500*time.Microsecond), origin.Add(31*time.Millisecond), 0)
	require.False(t, ok, "an endpoint request cannot replace the independently captured callback origin")
	require.Nil(t, record.Snapshot().attemptWindow)
}

func TestCanonicalAttemptAccountingFractionalTotalsStayInIntegerBounds(t *testing.T) {
	_, _, w := controlledAttemptWindow(t)
	w.startNS /= 100
	w.endNS /= 100
	p := &w.accounting
	p.startNS /= 100
	p.endNS /= 100
	p.executorStart /= 100
	p.executorEnd /= 100
	p.derivedStart /= 100
	p.derivedEnd /= 100
	for i := range p.laps {
		p.laps[i].totalNS /= 100
		p.laps[i].firstNS /= 100
		p.laps[i].lastNS /= 100
	}
	got := w.projectAccounting()
	require.True(t, got.PartitionComplete)
	require.Equal(t, 0.3, *got.SubtotalMS)
	require.Equal(t, 0.0, *got.UnattributedMS)
	p.laps[patchAttemptFinalize].totalNS++
	require.Equal(t, "invalid_order", w.projectAccounting().SumState, "one excess actual nanosecond is not tolerated or clipped")
}
