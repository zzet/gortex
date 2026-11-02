package mcp

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestAcknowledgementEnvelopeDisjointLeafChunksAndCleanup(t *testing.T) {
	start := time.Now()
	a := &editAcknowledgement{entered: start, envelope: editAcknowledgementEnvelope{
		observed: true, leafStarted: start.Add(3 * time.Millisecond), leafFinished: start.Add(14 * time.Millisecond),
	}}
	for _, chunk := range []struct{ begin, end time.Duration }{{4 * time.Millisecond, 6 * time.Millisecond}, {9 * time.Millisecond, 12 * time.Millisecond}} {
		lap := a.beginLapAt(editAckDiskWrite, start.Add(chunk.begin))
		lap.finishAt(start.Add(chunk.end))
	}
	a.finishActualAt(start.Add(18 * time.Millisecond))
	a.finishReplyAt(start.Add(20*time.Millisecond), "result")
	snapshot := a.snapshot()
	require.True(t, snapshot.LapsDisjoint)
	require.Equal(t, 3.0, *snapshot.Middleware.AckMS)
	require.Equal(t, 4.0, *snapshot.ReplyPrepare.AckMS)
	require.Equal(t, 5.0, *snapshot.DiskWrite.AckMS)
	ack := buildAcknowledgementPayload(&snapshot, "observed")
	require.Equal(t, 14.0, *ack.SubtotalMS)
	require.Equal(t, 6.0, *ack.UnattributedMS, "only actual18→20 caller delivery is added; synthetic unobserved leaf control stays missing")
	require.Equal(t, "partial", ack.SumState)
	*snapshot.Middleware.AckMS = 999
	require.Equal(t, 3.0, *a.snapshot().Middleware.AckMS)
}

func TestAcknowledgementEnvelopeRejectsMisplacedAndMissingBoundaries(t *testing.T) {
	start := time.Now()
	for _, tc := range []struct {
		name                                 string
		leafBegin, leafEnd, lapBegin, lapEnd time.Duration
	}{
		{"operation in middleware", 3, 8, 1, 2},
		{"operation in cleanup", 3, 8, 8, 9},
		{"no leaf with operation", 0, 0, 4, 5},
		{"reversed leaf", 8, 3, 4, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &editAcknowledgement{entered: start, envelope: editAcknowledgementEnvelope{observed: true}}
			if tc.leafBegin != 0 {
				a.envelope.leafStarted = start.Add(tc.leafBegin * time.Millisecond)
				a.envelope.leafFinished = start.Add(tc.leafEnd * time.Millisecond)
			}
			lap := a.beginLapAt(editAckDiskWrite, start.Add(tc.lapBegin*time.Millisecond))
			lap.finishAt(start.Add(tc.lapEnd * time.Millisecond))
			a.finishActualAt(start.Add(10 * time.Millisecond))
			a.finishReplyAt(start.Add(11*time.Millisecond), "result")
			snapshot := a.snapshot()
			ack := buildAcknowledgementPayload(&snapshot, "observed")
			require.Contains(t, []string{"invalid_overlap", "invalid_interval"}, ack.SumState)
			require.Nil(t, ack.SubtotalMS)
			require.Nil(t, ack.UnattributedMS)
		})
	}
	a := &editAcknowledgement{entered: start, envelope: editAcknowledgementEnvelope{observed: true, leafStarted: start.Add(time.Millisecond)}}
	a.finishReplyAt(start.Add(2*time.Millisecond), "go_error")
	require.Equal(t, "incomplete", a.snapshot().ReplyPrepare.State)
	require.Nil(t, a.snapshot().ReplyPrepare.ObservedMS)
}

func TestAcknowledgementEnvelopeActualMiddlewareAndRecoveredPanic(t *testing.T) {
	for _, panicLeaf := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "panic"}[panicLeaf], func(t *testing.T) {
			s := fastPathTestServer(t)
			s.ToolCallTimeout = -1
			var a *editAcknowledgement
			leafCalls := 0
			wrapped := s.wrapToolHandler(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				leafCalls++
				require.Same(t, a, editAcknowledgementFrom(ctx, s))
				if panicLeaf {
					panic("owned envelope oracle")
				}
				// A nested facade/raw invocation must not replace the first leaf
				// boundaries or include its duration a second time.
				before := a.envelope.leafStarted
				_, err := s.invokeAcknowledgedLeaf(ctx, req, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return mcp.NewToolResultText("nested"), nil
				})
				require.NoError(t, err)
				require.Equal(t, before, a.envelope.leafStarted)
				return mcp.NewToolResultText(`{"ok":true}`), nil
			})
			capture := s.boundToolHandler(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				a = editAcknowledgementFrom(ctx, s)
				return wrapped(ctx, req)
			})
			result, err := capture(t.Context(), writeFileRequest(nil))
			require.NoError(t, err)
			require.Equal(t, panicLeaf, result.IsError)
			require.Equal(t, 1, leafCalls)
			snapshot := a.snapshot()
			require.Equal(t, "measured", snapshot.Middleware.State)
			require.Equal(t, "measured", snapshot.ReplyPrepare.State)
			require.Equal(t, "measured", snapshot.EditControl.State)
			require.NotNil(t, snapshot.EditControl.AckMS)
			leafMS := float64(a.envelope.leafFinished.Sub(a.envelope.leafStarted)) / float64(time.Millisecond)
			require.InDelta(t, leafMS, *snapshot.EditControl.AckMS, 1e-9, "the real leaf, including nested calls/panic unwinding, owns one control interval")
			require.Equal(t, "measured", snapshot.CallerResult.State)
			payload := buildAcknowledgementPayload(&snapshot, "observed")
			require.InDelta(t, *snapshot.ReplyWallMS, *payload.SubtotalMS, 1e-6)
			require.Equal(t, "partial", payload.SumState, "source endpoints are not actual100-edit acceptance")
			require.True(t, snapshot.LapsDisjoint)
			require.False(t, a.envelope.leafFinished.After(a.actual), "actual finish includes all middleware defers")
			require.Equal(t, "measured", snapshot.ActualState)
			require.LessOrEqual(t, *snapshot.Middleware.AckMS+*snapshot.ReplyPrepare.AckMS, *snapshot.ReplyWallMS)
		})
	}
}

func TestAcknowledgementEnvelopeActualPreLeafRefusal(t *testing.T) {
	s := fastPathTestServer(t)
	s.ToolCallTimeout = -1
	var a *editAcknowledgement
	wrapped := s.wrapToolHandler(func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		t.Error("invalid freshness input must refuse before the leaf")
		return mcp.NewToolResultText("unexpected"), nil
	})
	capture := s.boundToolHandler(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		a = editAcknowledgementFrom(ctx, s)
		return wrapped(ctx, req)
	})
	result, err := capture(t.Context(), writeFileRequest(map[string]any{"wait_deadline": "invalid-duration"}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	snapshot := a.snapshot()
	require.Equal(t, "measured", snapshot.Middleware.State)
	require.Equal(t, "not_applicable", snapshot.ReplyPrepare.State)
	require.Equal(t, "unobserved", snapshot.EditControl.State)
	require.Nil(t, snapshot.EditControl.AckMS)
	require.Nil(t, snapshot.ReplyPrepare.ObservedMS)
	require.True(t, a.envelope.leafStarted.IsZero())
	require.Equal(t, *snapshot.ActualWallMS, *snapshot.Middleware.AckMS)
}

func TestAcknowledgementEnvelopeLateLeafDoesNotClipCleanupIntoReply(t *testing.T) {
	s := fastPathTestServer(t)
	s.ToolCallTimeout = time.Minute
	entered := make(chan *editAcknowledgement, 1)
	release, done := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var a *editAcknowledgement
	t.Cleanup(func() {
		unblock()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("owned late leaf did not join")
		}
		if a != nil {
			require.Eventually(t, func() bool { return a.snapshot().ActualState == "measured" }, 5*time.Second, time.Millisecond,
				"cleanup joins the actual middleware after the caller goroutine returned")
		}
	})
	wrapped := s.wrapToolHandler(func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		entered <- editAcknowledgementFrom(ctx, s)
		<-release
		return mcp.NewToolResultText(`{"ok":true}`), nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		defer close(done)
		_, err := wrapped(ctx, writeFileRequest(nil))
		returned <- err
	}()
	select {
	case a = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("leaf did not enter")
	}
	cancel()
	select {
	case err := <-returned:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("caller did not receive cancellation")
	}
	frozen := a.snapshot()
	require.Nil(t, frozen.ReplyPrepare.ObservedMS)
	require.Equal(t, "incomplete", frozen.EditControl.State)
	require.Nil(t, frozen.EditControl.AckMS)
	waitPastAcknowledgementReply(t, a)
	unblock()
	require.Eventually(t, func() bool { return a.snapshot().ActualState == "measured" }, 5*time.Second, time.Millisecond)
	after := a.snapshot()
	require.Equal(t, frozen.ReplyWallMS, after.ReplyWallMS)
	require.True(t, after.ActualFinishedLate)
	require.NotNil(t, after.ReplyPrepare.ObservedMS)
	require.Nil(t, after.ReplyPrepare.AckMS)
	require.Equal(t, "outside_reply_window", after.ReplyPrepare.State)
	require.NotNil(t, after.EditControl.ObservedMS)
	require.Nil(t, after.EditControl.AckMS, "late physical leaf control must not be clipped into ACK")
	require.Equal(t, "outside_reply_window", after.EditControl.State)
	require.Equal(t, "outside_reply_window", after.CallerResult.State)
	require.Nil(t, after.CallerResult.AckMS)
	require.Equal(t, "incomplete", frozen.EditControl.State, "earlier snapshots remain immutable")
}

func TestAcknowledgementEnvelopeUnobservedReadsHaveNoIdentityOrAllocation(t *testing.T) {
	s := &Server{}
	req := editOperationRequest("read", "file")
	result := mcp.NewToolResultText("read")
	h := func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) { return result, nil }
	ctx := t.Context()
	before := editAcknowledgementSequence.Load()
	allocations := testing.AllocsPerRun(100, func() {
		s.noteAcknowledgementMiddleware(ctx)
		_, _ = s.invokeAcknowledgedLeaf(ctx, req, h)
	})
	require.Zero(t, allocations, "nil observer must not allocate an envelope/token/closure")
	require.Equal(t, before, editAcknowledgementSequence.Load())
}
