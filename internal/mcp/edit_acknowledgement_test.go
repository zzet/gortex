package mcp

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestEditAcknowledgementSnapshotFirstWinsAndOwnedValues(t *testing.T) {
	start := time.Now()
	a := &editAcknowledgement{id: 9, entered: start}
	pending := a.snapshot()
	require.Equal(t, "pending", pending.ReplyState)
	require.Nil(t, pending.ReplyWallMS)
	require.Nil(t, pending.ActualWallMS)
	require.Nil(t, pending.DispatcherQueueMS)
	require.Nil(t, pending.DispatcherPreambleMS)
	require.Equal(t, "unavailable_dispatcher_interface", pending.DispatcherQueueState)
	require.Equal(t, "unavailable_dispatcher_interface", pending.DispatcherState)
	a.finishReplyAt(start.Add(4*time.Millisecond), "go_error")
	a.finishReplyAt(start.Add(10*time.Millisecond), "result")
	frozen := a.snapshot()
	require.Equal(t, "mcp_bound_call", frozen.Scope)
	require.EqualValues(t, 9, frozen.RPCID)
	require.Equal(t, 4.0, *frozen.ReplyWallMS)
	require.Equal(t, "go_error", frozen.ReplyOutcome)
	a.finishActualAt(start.Add(12 * time.Millisecond))
	a.finishActualAt(start.Add(20 * time.Millisecond))
	complete := a.snapshot()
	require.Equal(t, frozen.ReplyWallMS, complete.ReplyWallMS, "physical completion cannot move caller ready")
	require.True(t, complete.ActualFinishedLate)
	require.Equal(t, 12.0, *complete.ActualWallMS)
	require.Nil(t, frozen.ActualWallMS, "previous snapshots are immutable")
	*complete.ReplyWallMS, *complete.ActualWallMS = 99, 99
	require.Equal(t, 4.0, *a.snapshot().ReplyWallMS)
	require.Equal(t, 12.0, *a.snapshot().ActualWallMS)
}

func TestEditAcknowledgementInvalidOrderIsNotClipped(t *testing.T) {
	start := time.Now()
	a := &editAcknowledgement{entered: start}
	a.finishActualAt(start.Add(-time.Millisecond))
	a.finishReplyAt(start.Add(-2*time.Millisecond), "result")
	snapshot := a.snapshot()
	require.Equal(t, "invalid_order", snapshot.ReplyState)
	require.Equal(t, "invalid_order", snapshot.ActualState)
	require.Nil(t, snapshot.ReplyWallMS)
	require.Nil(t, snapshot.ActualWallMS)
	require.False(t, snapshot.ActualFinishedLate)
	zero := &editAcknowledgement{entered: start}
	zero.finishReplyAt(start, "result")
	require.Equal(t, "measured", zero.snapshot().ReplyState)
	require.Zero(t, *zero.snapshot().ReplyWallMS, "valid zero differs from missing or invalid")
	missingOrigin := &editAcknowledgement{}
	missingOrigin.finishReplyAt(start, "result")
	require.Equal(t, "unavailable_origin", missingOrigin.snapshot().ReplyState)
	require.Nil(t, missingOrigin.snapshot().ReplyWallMS)
}

func TestEditAcknowledgementNestedOwnershipAndLegacyOrigin(t *testing.T) {
	first, other := &Server{ToolCallTimeout: -1}, &Server{ToolCallTimeout: -1}
	legacyOrigin := time.Now().Add(-time.Second)
	ctx := withToolReceivedAt(t.Context(), legacyOrigin)
	var outerRecord, nestedRecord, otherRecord *editAcknowledgement
	inner := first.boundToolHandler(func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		nestedRecord = editAcknowledgementFrom(ctx, first)
		require.Equal(t, legacyOrigin, toolReceivedAt(ctx))
		return mcp.NewToolResultText("nested"), nil
	})
	foreign := other.boundToolHandler(func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		otherRecord = editAcknowledgementFrom(ctx, other)
		require.Nil(t, editAcknowledgementFrom(ctx, first), "another Server cannot freeze the parent's record")
		return mcp.NewToolResultText("foreign"), nil
	})
	outer := first.boundToolHandler(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		outerRecord = editAcknowledgementFrom(ctx, first)
		require.NotNil(t, outerRecord)
		_, err := inner(ctx, req)
		require.NoError(t, err)
		require.Equal(t, "pending", outerRecord.snapshot().ActualState, "nested return is not outer actual completion")
		require.Equal(t, "pending", outerRecord.snapshot().ReplyState, "nested reply cannot freeze the shared RPC wall")
		_, err = foreign(ctx, req)
		require.NoError(t, err)
		observeEditSourceEffect(ctx, true, true)
		observeEditSourceComplete(ctx)
		return mcp.NewToolResultText("outer"), nil
	})
	_, err := outer(ctx, editOperationRequest("edit", "file"))
	require.NoError(t, err)
	require.Same(t, outerRecord, nestedRecord)
	require.NotSame(t, outerRecord, otherRecord)
	require.NotEqual(t, outerRecord.id, otherRecord.id)
	require.Equal(t, "measured", outerRecord.snapshot().ReplyState)
	require.Equal(t, "measured", outerRecord.snapshot().ActualState)
	require.EqualValues(t, 1, first.editOperations.completed.Load(), "actual source counting stays unchanged")
	require.Zero(t, other.editOperations.completed.Load(), "a reply alone is not a completed edit")
}

func TestEditAcknowledgementCallerReadyPrecedesLatePhysicalHandler(t *testing.T) {
	s := &Server{ToolCallTimeout: time.Minute}
	entered, release, finished := make(chan *editAcknowledgement, 1), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("owned actual handler did not join")
		}
	})
	h := s.boundToolHandler(func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		defer close(finished)
		entered <- editAcknowledgementFrom(ctx, s)
		<-release // same non-cancellable completion contract as a held batch commit
		observeEditSourceEffect(ctx, true, true)
		observeEditSourceComplete(ctx)
		return mcp.NewToolResultText("committed"), nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type reply struct {
		result *mcp.CallToolResult
		err    error
	}
	got := make(chan reply, 1)
	go func() {
		result, err := h(ctx, editOperationRequest("batch_edit", ""))
		got <- reply{result, err}
	}()
	var record *editAcknowledgement
	select {
	case record = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("actual handler did not enter")
	}
	cancel()
	select {
	case result := <-got:
		require.ErrorIs(t, result.err, context.Canceled)
		require.Nil(t, result.result)
	case <-time.After(5 * time.Second):
		t.Fatal("caller did not receive its cancellation reply")
	}
	frozen := record.snapshot()
	require.Equal(t, "measured", frozen.ReplyState)
	require.Equal(t, "go_error", frozen.ReplyOutcome)
	require.Equal(t, "pending", frozen.ActualState)
	require.Zero(t, s.editOperations.completed.Load())
	waitPastAcknowledgementReply(t, record)
	unblock()
	require.Eventually(t, func() bool { return record.snapshot().ActualState == "measured" }, 5*time.Second, time.Millisecond)
	after := record.snapshot()
	require.Equal(t, frozen.ReplyWallMS, after.ReplyWallMS)
	require.True(t, after.ActualFinishedLate)
	require.EqualValues(t, 1, s.editOperations.completed.Load())
}

// Channel ordering alone need not advance a coarse monotonic clock. Keep the
// actual handler held until a real reading is later than the frozen reply, so
// the strict late/outside-window assertions have measured separation.
func waitPastAcknowledgementReply(t *testing.T, record *editAcknowledgement) {
	t.Helper()
	record.mu.Lock()
	reply := record.reply
	record.mu.Unlock()
	require.False(t, reply.IsZero(), "caller reply must already be frozen")
	require.Eventually(t, func() bool { return time.Now().After(reply) }, 5*time.Second, time.Millisecond,
		"monotonic clock did not advance past the frozen caller reply")
}

func TestEditAcknowledgementReadsKeepBaselineAllocationsAndNoIdentity(t *testing.T) {
	s := &Server{ToolCallTimeout: -1}
	ctx, req := t.Context(), editOperationRequest("read_file", "")
	result := mcp.NewToolResultText("read")
	noop := func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if editAcknowledgementFrom(ctx, s) != nil {
			t.Error("read acquired a mutation acknowledgement")
		}
		return result, nil
	}
	baseline := s.preAcknowledgementBoundToolHandler(noop)
	observed := s.boundToolHandler(noop)
	before := editAcknowledgementSequence.Load()
	plain := testing.AllocsPerRun(100, func() { _, _ = baseline(ctx, req) })
	wrapped := testing.AllocsPerRun(100, func() { _, _ = observed(ctx, req) })
	t.Logf("allocations/read: pre-feature wrapper=%g, acknowledgement wrapper=%g", plain, wrapped)
	require.LessOrEqual(t, wrapped, plain, "read wrapper must add no allocation versus the complete pre-feature wrapper")
	require.Equal(t, before, editAcknowledgementSequence.Load(), "read never creates an acknowledgement identity")
}
