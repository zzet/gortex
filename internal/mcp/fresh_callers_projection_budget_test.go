package mcp

import (
	"context"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestFreshCallersProjectionWindowStartsAfterWaitAndIsNotRenewed(t *testing.T) {
	s, _, _ := canonicalPromotionReadFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request := makeReq("get_callers", sharedReceiptCallersArguments())
	var first, second time.Time
	_, err := s.withProjectedSourceReadBudget(func(call context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		allowance := call.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
		require.True(t, allowance.afterFreshness)
		require.True(t, allowance.deadline.IsZero())
		require.Nil(t, call.Value(projectedSourceDeadlineKey{}), "no projection ceiling before the freshness barrier")
		originalWait := call.Value(freshSymbolDeadlineKey{}).(time.Time)
		started := time.Now()
		selected := projectedReadBudgetAfterFreshness(call)
		finished := time.Now()
		first = selected.Value(projectedSourceDeadlineKey{}).(time.Time)
		require.False(t, first.Before(started))
		require.False(t, first.After(finished.Add(projectedReadBudget)))
		require.False(t, first.After(originalWait))
		// A complete rebound receives the original outer context. Neither a
		// later inherited ceiling nor another barrier can renew its window.
		rebound := context.WithValue(call, projectedSourceDeadlineKey{}, originalWait)
		second = projectedReadBudgetAfterFreshness(rebound).Value(projectedSourceDeadlineKey{}).(time.Time)
		require.Equal(t, first, second)
		require.Equal(t, originalWait, rebound.Value(freshSymbolDeadlineKey{}))
		return mcplib.NewToolResultText("observed"), nil
	})(ctx, request)
	require.NoError(t, err)
}

func TestFreshCallersWholeAttemptCapAndOriginalDeadline(t *testing.T) {
	s, _, _ := canonicalPromotionReadFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request := makeReq("get_callers", sharedReceiptCallersArguments())
	calls := 0
	var originalWait, projection time.Time
	leaf := func(call context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		calls++
		wait := call.Value(freshSymbolDeadlineKey{}).(time.Time)
		call = projectedReadBudgetAfterFreshness(call)
		window := call.Value(projectedSourceDeadlineKey{}).(time.Time)
		if calls == 1 {
			originalWait, projection = wait, window
		} else {
			require.Equal(t, originalWait, wait)
			require.Equal(t, projection, window)
		}
		// Exercise the real whole-attempt controller's exhaustion path. The
		// production proof for emitting this signal is covered separately by
		// the actual canonical/physical transition fixture.
		return nil, errProjectedSourceMoved
	}
	result, err := s.withProjectedSourceReadBudget(s.retryProjectedSymbolSource(leaf))(ctx, request)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.True(t, result.IsError, "a third transition cannot create an unbounded success loop")
	deadline, _ := ctx.Deadline()
	require.False(t, originalWait.After(deadline))
}

func TestFreshCallersProjectionWindowHonorsExpiredInheritedBoundAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		if canceled {
			cancel()
		}
		original := time.Now().Add(-time.Millisecond)
		allowance := &projectedSourceAllowance{tool: "get_callers", afterFreshness: true}
		ctx = context.WithValue(ctx, projectedSourceAllowanceKey{}, allowance)
		ctx = context.WithValue(ctx, projectedSourceDeadlineKey{}, original)
		selected := projectedReadBudgetAfterFreshness(ctx)
		require.Equal(t, original, selected.Value(projectedSourceDeadlineKey{}))
		require.False(t, retryAfterCheckedProjectionChange(selected))
		cancel()
	}
}

func TestFreshCallersReboundWaitUsesStartedProjectionWindow(t *testing.T) {
	s, _, _ := canonicalPromotionReadFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	originalDeadline, _ := ctx.Deadline()
	request := makeReq("get_callers", sharedReceiptCallersArguments())
	calls := 0
	var projectionDeadline time.Time
	leaf := func(call context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		calls++
		if calls == 1 {
			call = projectedReadBudgetAfterFreshness(call)
			projectionDeadline = call.Value(projectedSourceDeadlineKey{}).(time.Time)
			return nil, errProjectedSourceMoved
		}
		deadline, ok := call.Deadline()
		require.True(t, ok)
		require.Equal(t, projectionDeadline, deadline)
		// Model the real repeated freshness barrier's wait on its context,
		// rather than accepting a newly pending scope or renewing the clock.
		<-call.Done()
		require.ErrorIs(t, call.Err(), context.DeadlineExceeded)
		return mcplib.NewToolResultError("controlled rebound wait expired"), nil
	}
	result, err := s.withProjectedSourceReadBudget(s.retryProjectedSymbolSource(leaf))(ctx, request)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.True(t, result.IsError)
	require.NoError(t, ctx.Err(), "the original five-second caller must not be canceled or renewed")
	stillOriginal, _ := ctx.Deadline()
	require.Equal(t, originalDeadline, stillOriginal)
}

func TestFreshCallersWatchedWaitSurvivesZeroWaitReboundWithoutAssemblyTime(t *testing.T) {
	allowance := &projectedSourceAllowance{tool: "get_callers", afterFreshness: true}
	ctx := context.WithValue(t.Context(), projectedSourceAllowanceKey{}, allowance)
	// These are the two observed barrier durations, not invented RPC elapsed
	// time. A zero-wait first call or rebound must remain exactly zero.
	require.Zero(t, recordProjectedWatchedWait(ctx, 0))
	first := recordProjectedWatchedWait(ctx, 25*time.Millisecond)
	require.Equal(t, 25*time.Millisecond, first)
	// Starting or reattaching an assembly budget must add no watched time.
	selected := projectedReadBudgetAfterFreshness(ctx)
	require.Equal(t, first, recordProjectedWatchedWait(selected, 0))
	rebound := projectedReadBudgetAfterFreshness(ctx)
	require.Equal(t, first, recordProjectedWatchedWait(rebound, 0))
	require.Equal(t, 28*time.Millisecond, recordProjectedWatchedWait(rebound, 3*time.Millisecond))
	// A different request and the ordinary controller cannot inherit this sum.
	other := context.WithValue(t.Context(), projectedSourceAllowanceKey{}, &projectedSourceAllowance{tool: "get_callers", afterFreshness: true})
	require.Zero(t, recordProjectedWatchedWait(other, 0))
	require.Equal(t, 2*time.Millisecond, recordProjectedWatchedWait(t.Context(), 2*time.Millisecond))
}
