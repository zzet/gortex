package indexer

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func scalarPatchFixture(t *testing.T) (*PublicationPhaseRecord, *PublicationStamps, time.Time) {
	t.Helper()
	origin := time.Now()
	r := NewPublicationPhaseRecorder(1, 1).Begin("", "scalar", PublicationSourceMCPEdit, origin)
	require.True(t, r.bindCanonicalTicket(1, origin.Add(time.Millisecond)))
	s := &PublicationStamps{patchLaps: &canonicalPatchLaps{}}
	s.markAt(PublicationPatchExecutionStarted, origin.Add(2*time.Millisecond))
	return r, s, origin
}

func scalarSpan(s *PublicationStamps, lap canonicalPatchLap, origin time.Time, from, through time.Duration) {
	span := s.beginPatchLapAt(lap, origin.Add(from))
	span.endAt(origin.Add(through))
}

func TestCanonicalPatchScalarsAggregateRepeatedDisjointSpans(t *testing.T) {
	r, s, origin := scalarPatchFixture(t)
	for _, span := range []struct {
		lap  canonicalPatchLap
		a, b time.Duration
	}{
		{patchGateWait, 2, 3}, {patchParse, 3, 4}, {patchApply, 4, 5}, {patchSearch, 5, 6},
		{patchApply, 6, 7}, {patchParse, 8, 9}, {patchResolve, 9, 10}, {patchMtimes, 10, 11}, {patchDerived, 12, 13},
	} {
		scalarSpan(s, span.lap, origin, span.a*time.Millisecond, span.b*time.Millisecond)
	}
	s.markAt(PublicationPatchDerivedStarted, origin.Add(12*time.Millisecond))
	s.markAt(PublicationPatchDerivedFinished, origin.Add(13*time.Millisecond))
	s.markAt(PublicationPatchExecutionFinished, origin.Add(14*time.Millisecond))
	r.Absorb(s)
	r.MarkAt(PublicationTicketCompleted, origin.Add(15*time.Millisecond))
	snapshot := r.Snapshot()
	timings := snapshot.EditTimings()
	require.Equal(t, 2.0, *timings.Milliseconds["apply"])
	require.Equal(t, 2.0, *timings.Milliseconds["patch_parse"])
	require.Equal(t, 1.0, *timings.Milliseconds["search_lap"])
	require.Equal(t, 6.0, *timings.Window.PublicationUnattributedWallMS)
	for _, name := range canonicalPatchLapNames {
		require.Equal(t, "measured", timings.States[name], name)
	}
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	snapshot.patchLaps = nil
	legacy, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.Equal(t, string(legacy), string(encoded), "scalar evidence must not change the legacy timeline wire")
	// The record owns a frozen copy, not the still-accessible local collector.
	scalarSpan(s, patchApply, origin, 16*time.Millisecond, 20*time.Millisecond)
	require.Equal(t, 2.0, *r.Snapshot().EditTimings().Milliseconds["apply"])
}

func TestCanonicalPatchScalarsRejectOverlapAndUnfinishedWork(t *testing.T) {
	for _, unfinished := range []bool{false, true} {
		t.Run(map[bool]string{false: "overlap", true: "unfinished"}[unfinished], func(t *testing.T) {
			r, s, origin := scalarPatchFixture(t)
			parse := s.beginPatchLapAt(patchParse, origin.Add(3*time.Millisecond))
			if !unfinished {
				apply := s.beginPatchLapAt(patchApply, origin.Add(4*time.Millisecond))
				parse.endAt(origin.Add(5 * time.Millisecond))
				apply.endAt(origin.Add(6 * time.Millisecond))
			}
			s.markAt(PublicationPatchExecutionFinished, origin.Add(7*time.Millisecond))
			r.Absorb(s)
			r.MarkAt(PublicationTicketCompleted, origin.Add(8*time.Millisecond))
			out := r.Snapshot().EditTimings()
			require.Nil(t, out.Milliseconds["patch_parse"])
			require.Equal(t, "invalid_order", out.States["patch_parse"])
			require.Nil(t, out.Window.PublicationUnattributedWallMS)
		})
	}
}

func TestCanonicalPatchScalarsKeepCombinedFallbackUnavailable(t *testing.T) {
	r, s, origin := scalarPatchFixture(t)
	scalarSpan(s, patchGateWait, origin, 2*time.Millisecond, 3*time.Millisecond)
	scalarSpan(s, patchParse, origin, 3*time.Millisecond, 4*time.Millisecond)
	scalarSpan(s, patchApply, origin, 4*time.Millisecond, 5*time.Millisecond)
	scalarSpan(s, patchSearch, origin, 5*time.Millisecond, 6*time.Millisecond)
	s.incompletePatchLaps()
	s.markAt(PublicationPatchExecutionFinished, origin.Add(8*time.Millisecond))
	r.Absorb(s)
	out := r.Snapshot().EditTimings()
	for _, name := range []string{"patch_parse", "apply", "search_lap"} {
		require.Nil(t, out.Milliseconds[name], "prior measured chunks do not complete a combined fallback")
		require.Equal(t, "not_observed", out.States[name])
	}
	require.Equal(t, "measured", out.States["gate_wait"])
	require.Equal(t, 1.0, *out.Milliseconds["gate_wait"])
}

func TestCanonicalPatchScalarsValidateDerivedPairAndOwnership(t *testing.T) {
	for _, tc := range []struct {
		name             string
		rawStart, rawEnd time.Duration
	}{
		{"different_pair", 5, 7}, {"outside_executor", 9, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, s, origin := scalarPatchFixture(t)
			scalarSpan(s, patchParse, origin, 3*time.Millisecond, 4*time.Millisecond)
			scalarSpan(s, patchDerived, origin, 5*time.Millisecond, 6*time.Millisecond)
			s.markAt(PublicationPatchDerivedStarted, origin.Add(tc.rawStart*time.Millisecond))
			s.markAt(PublicationPatchDerivedFinished, origin.Add(tc.rawEnd*time.Millisecond))
			s.markAt(PublicationPatchExecutionFinished, origin.Add(8*time.Millisecond))
			r.Absorb(s)
			r.MarkAt(PublicationTicketCompleted, origin.Add(9*time.Millisecond))
			out := r.Snapshot().EditTimings()
			require.Nil(t, out.Milliseconds["derived"])
			require.Nil(t, out.Milliseconds["patch_parse"], "a mismatched derived interval must not bless another overlapping subtotal")
			require.Nil(t, out.Window.PublicationUnattributedWallMS)
		})
	}
}

func TestCanonicalPatchNilObserverIsInert(t *testing.T) {
	var s *PublicationStamps
	span := s.beginPatchLap(patchParse)
	require.Nil(t, span.stamps)
	require.True(t, span.started.IsZero())
	require.True(t, span.end().IsZero())
	s.incompletePatchLaps()
}
