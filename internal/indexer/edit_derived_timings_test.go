package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func derivedTimingSnapshot() PublicationPhaseSnapshot {
	return PublicationPhaseSnapshot{Clock: "monotonic", Source: PublicationSourceMCPEdit, Terminal: true,
		Phases: []PublicationPhaseOffset{
			{Phase: PublicationReceived},
			{Phase: PublicationDiskWriteStarted, OffsetNS: 1e6},
			{Phase: PublicationReceiptCommitted, OffsetNS: 2e6},
			{Phase: PublicationTicketEnqueued, OffsetNS: 3e6},
			{Phase: PublicationPatchExecutionStarted, OffsetNS: 4e6},
			{Phase: PublicationPatchDerivedStarted, OffsetNS: 5e6},
			{Phase: PublicationPatchDerivedFinished, OffsetNS: 7e6},
			{Phase: PublicationPatchExecutionFinished, OffsetNS: 8e6},
			{Phase: PublicationTicketCompleted, OffsetNS: 10e6},
		}}
}

func TestEditTimingsDerivedSubtractsOnlyContainedDisjointTail(t *testing.T) {
	snapshot := derivedTimingSnapshot()
	timings := snapshot.EditTimings()
	require.Equal(t, "measured", timings.States["derived"])
	require.Equal(t, 2.0, *timings.Milliseconds["derived"])
	require.Equal(t, 6.0, *timings.Window.PublicationUnattributedWallMS, "10ms wall minus disjoint disk/publish/derived; do not add outer execution")
	require.Nil(t, timings.Window.PatchWallMS)
	require.Nil(t, timings.Window.AcknowledgementWallMS)
	for _, phase := range []string{"debounce", "lane_wait", "gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes"} {
		require.Nil(t, timings.Milliseconds[phase], phase)
		require.Equal(t, "not_observed", timings.States[phase], phase)
	}
	*timings.Milliseconds["derived"] = 99
	require.Equal(t, 2.0, *snapshot.EditTimings().Milliseconds["derived"], "projections own their values")
}

func TestEditTimingsDerivedRequiresItsActualOwnershipBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		phase    PublicationPhase
		offset   int64
		remove   bool
		omit     PublicationPhase
		live     bool
		state    string
		measured *float64
	}{
		{name: "observed_zero", phase: PublicationPatchDerivedFinished, offset: 5e6, state: "measured", measured: new(float64)},
		{name: "reversed_pair", phase: PublicationPatchDerivedFinished, offset: 4e6, state: "invalid_order"},
		{name: "negative_start", phase: PublicationPatchDerivedStarted, offset: -1, state: "invalid_order"},
		{name: "before_executor", phase: PublicationPatchDerivedStarted, offset: 3e6, state: "invalid_order"},
		{name: "after_executor", phase: PublicationPatchDerivedFinished, offset: 9e6, state: "invalid_order"},
		{name: "executor_before_admission", phase: PublicationTicketEnqueued, offset: 6e6, state: "invalid_order"},
		{name: "executor_after_terminal", phase: PublicationPatchExecutionFinished, offset: 11e6, state: "invalid_order"},
		{name: "missing_executor", phase: PublicationPatchExecutionStarted, remove: true, state: "not_observed"},
		{name: "missing_admission", phase: PublicationTicketEnqueued, remove: true, state: "not_observed"},
		{name: "missing_admission_with_known_reversal", phase: PublicationPatchDerivedStarted, offset: 3e6, omit: PublicationTicketEnqueued, state: "invalid_order"},
		{name: "pending_pair", phase: PublicationPatchDerivedFinished, remove: true, live: true, state: "pending"},
		{name: "terminal_missing_end", phase: PublicationPatchDerivedFinished, remove: true, state: "not_observed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := derivedTimingSnapshot()
			phases := snapshot.Phases[:0]
			for _, phase := range snapshot.Phases {
				if phase.Phase == tc.omit {
					continue
				}
				if tc.live && phase.Phase == PublicationTicketCompleted {
					continue
				}
				if phase.Phase == tc.phase {
					if tc.remove {
						continue
					}
					phase.OffsetNS = tc.offset
				}
				phases = append(phases, phase)
			}
			snapshot.Phases, snapshot.Terminal = phases, !tc.live
			timings := snapshot.EditTimings()
			require.Equal(t, tc.state, timings.States["derived"])
			require.Equal(t, tc.measured, timings.Milliseconds["derived"], "missing or contradictory evidence must not become measured zero")
			if tc.state == "invalid_order" || tc.live {
				require.Nil(t, timings.Window.PublicationUnattributedWallMS)
			}
		})
	}
}
