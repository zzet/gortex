package mcp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func beginControlledLeaf(t *testing.T, origin time.Time) *editAcknowledgement {
	t.Helper()
	a := &editAcknowledgement{entered: origin, envelope: editAcknowledgementEnvelope{observed: true, leafStarted: origin.Add(time.Millisecond)}}
	a.mu.Lock()
	a.laps.controlObserved = true
	a.openEditControlLocked(a.envelope.leafStarted)
	a.mu.Unlock()
	return a
}

func endControlledLeaf(a *editAcknowledgement, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.envelope.leafFinished = at
	a.closeEditControlLocked(at)
}

func TestEditControlAccumulatesActualOnlineABALeafIntervals(t *testing.T) {
	origin := time.Now()
	a := beginControlledLeaf(t, origin)
	for _, chunk := range []struct {
		kind       editAcknowledgementLapKind
		start, end time.Duration
	}{
		{editAckParseGate, 2, 4}, {editAckDiskWrite, 6, 7}, {editAckParseGate, 9, 10},
	} {
		lap := a.beginLapAt(chunk.kind, origin.Add(chunk.start*time.Millisecond))
		lap.finishAt(origin.Add(chunk.end * time.Millisecond))
	}
	endControlledLeaf(a, origin.Add(12*time.Millisecond))
	a.finishActualAt(origin.Add(14 * time.Millisecond))
	a.finishReplyAt(origin.Add(15*time.Millisecond), "result")
	snapshot := a.snapshot()
	require.True(t, snapshot.LapsDisjoint)
	require.Equal(t, 3.0, *snapshot.ParseGate.AckMS)
	require.Equal(t, 1.0, *snapshot.DiskWrite.AckMS)
	require.Equal(t, 7.0, *snapshot.EditControl.AckMS, "actual1→2,4→6,7→9,10→12 intervals")
	require.EqualValues(t, 4, snapshot.EditControl.Started)
	require.EqualValues(t, 4, snapshot.EditControl.Completed)
	ack := buildAcknowledgementPayload(&snapshot, "observed")
	require.Equal(t, 15.0, *ack.SubtotalMS)
	require.Equal(t, 0.0, *ack.UnattributedMS, "actual caller-result pair closes this controlled ACK; no residual becomes work")
	require.Equal(t, "partial", ack.SumState)
	require.Equal(t, 7.0, *ack.PhasesMS["edit_control"])
	*snapshot.EditControl.AckMS = 999
	require.Equal(t, 7.0, *a.snapshot().EditControl.AckMS, "snapshots own scalar values")
}

func TestEditControlRecordsRealNotApplicableGuardWithoutInventingParse(t *testing.T) {
	origin := time.Now()
	a := beginControlledLeaf(t, origin)
	gate := a.beginLapAt(editAckParseGate, origin.Add(2*time.Millisecond))
	gate.applicable = false
	gate.finishAt(origin.Add(4 * time.Millisecond))
	endControlledLeaf(a, origin.Add(5*time.Millisecond))
	a.finishActualAt(origin.Add(6 * time.Millisecond))
	a.finishReplyAt(origin.Add(7*time.Millisecond), "result")
	snapshot := a.snapshot()
	require.True(t, snapshot.LapsDisjoint)
	require.Equal(t, "not_applicable", snapshot.ParseGate.State)
	require.Nil(t, snapshot.ParseGate.AckMS)
	require.Equal(t, 4.0, *snapshot.EditControl.AckMS, "guard dispatch is an actually observed control interval")
	require.Equal(t, 7.0, *buildAcknowledgementPayload(&snapshot, "observed").SubtotalMS)
}

func TestEditControlMissingNamedEndOrReversedTransitionCannotProduceCoverage(t *testing.T) {
	for _, name := range []string{"missing_end", "reversed", "overlap"} {
		t.Run(name, func(t *testing.T) {
			origin := time.Now()
			a := beginControlledLeaf(t, origin)
			lap := a.beginLapAt(editAckDiskWrite, origin.Add(2*time.Millisecond))
			switch name {
			case "reversed":
				lap.finishAt(origin.Add(time.Millisecond))
			case "overlap":
				nested := a.beginLapAt(editAckParseGate, origin.Add(3*time.Millisecond))
				nested.finishAt(origin.Add(4 * time.Millisecond))
				lap.finishAt(origin.Add(5 * time.Millisecond))
			}
			endControlledLeaf(a, origin.Add(6*time.Millisecond))
			a.finishActualAt(origin.Add(7 * time.Millisecond))
			a.finishReplyAt(origin.Add(8*time.Millisecond), "go_error")
			snapshot := a.snapshot()
			payload := buildAcknowledgementPayload(&snapshot, "observed")
			require.Nil(t, payload.SubtotalMS)
			require.Nil(t, payload.UnattributedMS)
			require.Contains(t, []string{"invalid_overlap", "invalid_interval"}, payload.SumState)
		})
	}
}

func TestAcknowledgementCallerResultUsesActualEndpointsWithoutDetachedClipping(t *testing.T) {
	origin := time.Now()
	for _, tc := range []struct {
		name          string
		actual, reply time.Duration
		want          string
	}{
		{"delivery", 4, 7, "measured"}, {"zero", 4, 4, "measured"},
		{"detached", 7, 4, "outside_reply_window"}, {"missing_actual", 0, 4, "unobserved"},
		{"pending_reply", 4, 0, "pending_reply"}, {"before_origin", -1, 4, "invalid_interval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var actual, reply time.Time
			if tc.actual != 0 {
				actual = origin.Add(tc.actual * time.Millisecond)
			}
			if tc.reply != 0 {
				reply = origin.Add(tc.reply * time.Millisecond)
			}
			got := acknowledgementCallerResult(origin, actual, reply)
			require.Equal(t, tc.want, got.State)
			if tc.want == "measured" {
				require.Equal(t, float64(tc.reply-tc.actual), *got.AckMS)
				require.Equal(t, *got.AckMS, *got.ObservedMS)
			} else {
				require.Nil(t, got.AckMS)
				require.Nil(t, got.ObservedMS)
			}
		})
	}
}

func TestEditControlFractionalMillisecondsUseObservedNanoseconds(t *testing.T) {
	origin := time.Now()
	a := &editAcknowledgement{entered: origin, envelope: editAcknowledgementEnvelope{observed: true, leafStarted: origin.Add(100 * time.Microsecond)}}
	a.mu.Lock()
	a.laps.controlObserved = true
	a.openEditControlLocked(a.envelope.leafStarted)
	a.mu.Unlock()
	endControlledLeaf(a, origin.Add(300*time.Microsecond))
	a.finishActualAt(origin.Add(300 * time.Microsecond))
	a.finishReplyAt(origin.Add(300*time.Microsecond), "result")
	snapshot := a.snapshot()
	roundedSum := *snapshot.Middleware.AckMS + *snapshot.EditControl.AckMS
	require.Greater(t, roundedSum, *snapshot.ReplyWallMS, "per-lap float0.1+0.2 must not reject actual disjoint300000ns")
	payload := buildAcknowledgementPayload(&snapshot, "observed")
	require.Equal(t, "partial", payload.SumState)
	require.Equal(t, 0.3, *payload.SubtotalMS)
	require.Equal(t, 0.0, *payload.UnattributedMS)

	overflow := a.snapshot()
	overflow.EditControl.ackNS++
	ms := float64(overflow.EditControl.ackNS) / 1e6
	overflow.EditControl.AckMS, overflow.EditControl.ObservedMS = &ms, &ms
	rejected := buildAcknowledgementPayload(&overflow, "observed")
	require.Equal(t, "invalid_total", rejected.SumState, "even1actualns beyond ACK must be refused without tolerance")
	require.Nil(t, rejected.SubtotalMS)

	inconsistent := a.snapshot()
	modified := *inconsistent.EditControl.AckMS + 0.01
	inconsistent.EditControl.AckMS, inconsistent.EditControl.ObservedMS = &modified, &modified
	require.Equal(t, "invalid_interval", buildAcknowledgementPayload(&inconsistent, "observed").SumState, "public scalars must match actual private readings")

	synthetic := snapshot
	synthetic.hasReplyNS = false
	synthetic.Middleware.hasAckNS = false
	synthetic.EditControl.hasAckNS = false
	require.Equal(t, "invalid_total", buildAcknowledgementPayload(&synthetic, "observed").SumState, "missing actualNS evidence keeps the conservative float fallback")
}
