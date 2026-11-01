package indexer

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func requireEditTimingKeys(t *testing.T, timings EditPhaseTimings) {
	t.Helper()
	// Keep this census independent of the production arrays: adding or dropping
	// a required lap must be an explicit wire-contract decision.
	want := []string{"dispatcher_queue", "middleware", "parse", "disk_write", "publish", "reindex_wait", "edit_control", "caller_result",
		"debounce", "cohort_admission", "batch_admission", "lane_wait", "gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived"}
	require.Len(t, timings.Milliseconds, len(want))
	require.Len(t, timings.States, len(want))
	for _, key := range want {
		require.Contains(t, timings.Milliseconds, key)
		require.NotEmpty(t, timings.States[key], key)
	}
}

func TestEditTimingsOnlyProjectProvenDisjointIntervals(t *testing.T) {
	origin := time.Now()
	record := NewPublicationPhaseRecorder(1, 1).Begin("checkout", "edit", PublicationSourceMCPEdit, origin)
	record.MarkAt(PublicationDiskWriteStarted, origin.Add(time.Millisecond))
	record.MarkAt(PublicationReceiptCommitted, origin.Add(1250500*time.Nanosecond))
	record.MarkAt(PublicationTicketEnqueued, origin.Add(3*time.Millisecond))
	record.MarkAt(PublicationCycleStarted, origin.Add(4*time.Millisecond))
	record.MarkAt(PublicationAdmitted, origin.Add(6*time.Millisecond))
	record.MarkAt(PublicationTicketCompleted, origin.Add(10*time.Millisecond))
	snapshot := record.Snapshot()
	before, err := json.Marshal(snapshot)
	require.NoError(t, err)
	timings := snapshot.EditTimings()
	requireEditTimingKeys(t, timings)
	require.Equal(t, "monotonic", timings.Window.Clock)
	require.Equal(t, "partial", timings.Window.Coverage)
	require.Equal(t, "measured", timings.States["disk_write"])
	require.Equal(t, 0.2505, *timings.Milliseconds["disk_write"], "derive from nanoseconds, not rounded offset_ms")
	require.Equal(t, 1.7495, *timings.Milliseconds["publish"])
	require.Nil(t, timings.Milliseconds["lane_wait"], "cycle start→admitted includes more than pure lane wait")
	require.Equal(t, "not_observed", timings.States["lane_wait"])
	require.Nil(t, timings.Window.AcknowledgementWallMS, "publication completion is not the response time")
	require.Nil(t, timings.Window.PatchWallMS, "the receipt does not delimit a physical patch")
	require.Equal(t, 10.0, *timings.Window.PublicationWallMS)
	require.Equal(t, 8.0, *timings.Window.PublicationUnattributedWallMS)
	after, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "duration projection must not alter the legacy timeline")
	*timings.Milliseconds["disk_write"] = 99
	require.Equal(t, 0.2505, *snapshot.EditTimings().Milliseconds["disk_write"], "callers own their projection")
}

func TestEditTimingsUnknownPendingZeroAndTerminalMissingAreDistinct(t *testing.T) {
	origin := time.Now()
	record := NewPublicationPhaseRecorder(1, 1).Begin("checkout", "edit", PublicationSourceMCPEdit, origin)
	empty := record.Snapshot().EditTimings()
	requireEditTimingKeys(t, empty)
	require.Nil(t, empty.Milliseconds["disk_write"])
	require.Equal(t, "not_observed", empty.States["disk_write"])
	record.MarkAt(PublicationDiskWriteStarted, origin.Add(time.Millisecond))
	pending := record.Snapshot().EditTimings()
	require.Nil(t, pending.Milliseconds["disk_write"])
	require.Equal(t, "pending", pending.States["disk_write"])
	require.Nil(t, pending.Window.PublicationWallMS)
	record.MarkAt(PublicationReceiptCommitted, origin.Add(time.Millisecond))
	committed := record.Snapshot().EditTimings()
	require.NotNil(t, committed.Milliseconds["disk_write"])
	require.Zero(t, *committed.Milliseconds["disk_write"], "two observed simultaneous boundaries are measured zero")
	require.Equal(t, "measured", committed.States["disk_write"])
	require.Equal(t, "pending", committed.States["publish"])
	record.MarkAt(PublicationTicketFailed, origin.Add(2*time.Millisecond))
	failed := record.Snapshot().EditTimings()
	require.Equal(t, "not_observed", failed.States["publish"], "terminal failure cannot imply successful admission")
	require.Nil(t, failed.Milliseconds["publish"])
	encoded, err := json.Marshal(failed)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"disk_write":0`)
	require.Contains(t, string(encoded), `"publish":null`)
}

func TestEditTimingsRejectInvalidOrderingWithoutClamping(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		snapshot := PublicationPhaseSnapshot{Clock: "monotonic", Source: PublicationSourceMCPEdit, Terminal: terminal,
			Phases: []PublicationPhaseOffset{
				{Phase: PublicationReceived},
				{Phase: PublicationDiskWriteStarted, OffsetNS: 3e6},
				{Phase: PublicationReceiptCommitted, OffsetNS: 2e6},
				{Phase: PublicationTicketEnqueued, OffsetNS: 1e6},
				{Phase: PublicationTicketFailed, OffsetNS: 4e6},
			}}
		timings := snapshot.EditTimings()
		for _, name := range []string{"disk_write", "publish"} {
			require.Nil(t, timings.Milliseconds[name])
			require.Equal(t, "invalid_order", timings.States[name])
		}
		require.Nil(t, timings.Window.PublicationUnattributedWallMS)
	}
}

func TestEditTimingsNoTimelineAndNonEditSources(t *testing.T) {
	missing := (PublicationPhaseSnapshot{}).EditTimings()
	requireEditTimingKeys(t, missing)
	require.Equal(t, "unavailable", missing.Window.Clock)
	for name, ms := range missing.Milliseconds {
		require.Nil(t, ms, name)
		require.Equal(t, "not_observed", missing.States[name])
	}
	for _, source := range []string{PublicationSourceFreshRequest, PublicationSourceBackgroundCompaction} {
		timings := (PublicationPhaseSnapshot{Source: source, Clock: "monotonic"}).EditTimings()
		for _, name := range []string{"dispatcher_queue", "middleware", "parse", "disk_write", "publish", "reindex_wait"} {
			require.Nil(t, timings.Milliseconds[name])
			require.Equal(t, "not_applicable", timings.States[name])
		}
	}
}
