package mcp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
)

func TestAcknowledgementDispatchSnapshotKeepsSeparateOriginAndInvalidStates(t *testing.T) {
	start := time.Now()
	a := &editAcknowledgement{entered: start.Add(10 * time.Millisecond), hasDispatch: true,
		dispatch: daemon.MCPDispatchTiming{Entered: start, Granted: start.Add(4 * time.Millisecond)}}
	a.finishActualAt(start.Add(25 * time.Millisecond))
	a.finishReplyAt(start.Add(30*time.Millisecond), "result")
	snapshot := a.snapshot()
	require.Equal(t, 4.0, *snapshot.DispatcherQueueMS)
	require.Equal(t, 6.0, *snapshot.DispatcherPreambleMS)
	require.Equal(t, 20.0, *snapshot.ReplyWallMS, "admission timestamps do not silently move the existing bound-call origin")
	require.Equal(t, 30.0, *snapshot.DispatcherReplyMS)
	ack := buildAcknowledgementPayload(&snapshot, "observed")
	require.Equal(t, "queue_and_preamble_precede_bound_call_subtotal", ack.DispatcherScope)
	require.Equal(t, 4.0, *ack.PhasesMS["dispatcher_queue"])
	require.Equal(t, 6.0, *ack.PhasesMS["dispatcher_preamble"])
	require.Equal(t, 30.0, *ack.DispatcherReplyMS)
	require.Equal(t, 20.0, *ack.ReplyWallMS)
	require.Equal(t, 5.0, *ack.PhasesMS["caller_result"], "actual25→reply30 belongs to A")
	require.Equal(t, 5.0, *ack.SubtotalMS, "outside-bound queue4+preamble6 must not enter the bound-call subtotal")
	require.Equal(t, 15.0, *ack.UnattributedMS)
	a.mu.Lock()
	savedActual := a.actual
	a.actual = time.Time{}
	a.mu.Unlock()
	withoutActual := a.snapshot()
	require.Nil(t, buildAcknowledgementPayload(&withoutActual, "observed").SubtotalMS,
		"outside-bound admission alone cannot become a bound-call subtotal")
	a.mu.Lock()
	a.actual = savedActual
	a.mu.Unlock()
	*snapshot.DispatcherQueueMS = 999
	require.Equal(t, 4.0, *a.snapshot().DispatcherQueueMS)
	for _, tc := range []struct{ entered, granted time.Time }{
		{start.Add(5 * time.Millisecond), start.Add(4 * time.Millisecond)},
		{start, start.Add(11 * time.Millisecond)},
	} {
		a.dispatch = daemon.MCPDispatchTiming{Entered: tc.entered, Granted: tc.granted}
		invalid := a.snapshot()
		require.Equal(t, "invalid_interval", invalid.DispatcherQueueState)
		require.Equal(t, "invalid_interval", invalid.DispatcherState)
		require.Nil(t, invalid.DispatcherQueueMS)
		require.Nil(t, invalid.DispatcherPreambleMS)
		require.Nil(t, buildAcknowledgementPayload(&invalid, "observed").DispatcherReplyMS)
	}
	a.hasDispatch = false
	missing := a.snapshot()
	require.Nil(t, missing.DispatcherQueueMS)
	require.Equal(t, "unavailable_dispatcher_interface", missing.DispatcherQueueState)
	require.Nil(t, buildAcknowledgementPayload(&missing, "observed").PhasesMS["dispatcher_queue"])
}
