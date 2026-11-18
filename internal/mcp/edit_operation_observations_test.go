package mcp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func editOperationRequest(name, operation string) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Name = name
	if operation != "" {
		req.Params.Arguments = map[string]any{"operation": operation}
	}
	return req
}

func TestEditOperationMeasuredSurfaces(t *testing.T) {
	for _, pair := range [][2]string{
		{"write_file", ""}, {"edit_file", ""}, {"edit_symbol", ""}, {"batch_edit", ""}, {"move_symbol", ""},
		{"edit", "write"}, {"edit", "file"}, {"edit", "symbol"}, {"edit", "batch"}, {"refactor", "move"}, {"edit", " FILE "}, {"edit", ""},
	} {
		require.True(t, measuredSourceEdit(editOperationRequest(pair[0], pair[1])), "%v", pair)
	}
	for _, args := range []map[string]any{
		{"target": map[string]any{"file": "a.go"}, "content": "new"},
		{"target": map[string]any{"file": "a.go"}, "match": "old", "replacement": "new"},
		{"target": map[string]any{"symbol": "a.go::A"}},
		{"changes": []any{map[string]any{"op": "edit_file"}}},
	} {
		req := editOperationRequest("edit", "")
		req.Params.Arguments = args
		require.True(t, measuredSourceEdit(req), "facade inference must retain operation ownership")
	}
	for _, pair := range [][2]string{{"read", "source"}, {"change", "receipt"}, {"mutation_status", ""}, {"edit", "docs"}, {"refactor", "rename"}} {
		require.False(t, measuredSourceEdit(editOperationRequest(pair[0], pair[1])), "%v", pair)
	}
}

// These are witness/actual-execution contract controls. Real tool write seams
// are covered separately; synthetic marks here do not prove their integration.
func TestEditOperationActualResultAndSourceOutcome(t *testing.T) {
	good := mcp.NewToolResultText("applied")
	bad := mcp.NewToolResultError("failed after write")
	for _, tt := range []struct {
		name                          string
		effect, complete, warning     bool
		rolledBack, unknown           bool
		result                        *mcp.CallToolResult
		err                           error
		completed, committedError     uint64
		partial, rolled, unclassified uint64
	}{
		{name: "clean", effect: true, complete: true, result: good, completed: 1},
		{name: "nil result is not success", effect: true, complete: true, committedError: 1},
		{name: "tool error nil Go error", effect: true, complete: true, result: bad, committedError: 1},
		{name: "Go error", effect: true, complete: true, result: good, err: errors.New("format failed"), committedError: 1},
		{name: "committed journal warning", effect: true, complete: true, warning: true, result: good, committedError: 1},
		{name: "partial source effects", effect: true, result: bad, partial: 1},
		{name: "rolled back", effect: true, rolledBack: true, result: good, partial: 1, rolled: 1},
		{name: "prior bytes unknown", effect: true, complete: true, unknown: true, result: good, unclassified: 1},
		{name: "no writes", result: good},
		{name: "unchanged rewrite", complete: true, result: good},
		{name: "refused before disk", result: bad},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{ToolCallTimeout: -1}
			h := s.boundToolHandler(func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if tt.effect {
					observeEditSourceEffect(ctx, true, !tt.unknown)
				} else {
					observeEditSourceEffect(ctx, false, true)
				}
				observeEditSourceTerminal(ctx, tt.complete, tt.warning, tt.rolledBack)
				return tt.result, tt.err
			})
			result, err := h(t.Context(), editOperationRequest("edit", "file"))
			if tt.result == nil {
				require.Nil(t, result)
			} else {
				require.Same(t, tt.result, result)
			}
			require.Equal(t, tt.err, err)
			require.Equal(t, tt.completed, s.editOperations.completed.Load())
			require.Equal(t, tt.committedError, s.editOperations.committedWithError.Load())
			require.Equal(t, tt.partial, s.editOperations.failedAfterSourceEffect.Load())
			require.Equal(t, tt.rolled, s.editOperations.rolledBack.Load())
			require.Equal(t, tt.unclassified, s.editOperations.unknownSourceComparison.Load())
		})
	}
}

func TestEditOperationNestedOwnerAndServerIsolation(t *testing.T) {
	first, second := &Server{ToolCallTimeout: -1}, &Server{ToolCallTimeout: -1}
	leaf := func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Multiple forward file writes are one logical handler completion.
		observeEditSourceEffect(ctx, true, true)
		observeEditSourceEffect(ctx, true, true)
		observeEditSourceComplete(ctx)
		return mcp.NewToolResultText("applied"), nil
	}
	inner := first.boundToolHandler(leaf)
	other := second.boundToolHandler(leaf)
	outer := first.boundToolHandler(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if _, err := other(ctx, req); err != nil {
			return nil, err
		}
		return inner(ctx, req)
	})
	_, err := outer(t.Context(), editOperationRequest("batch_edit", ""))
	require.NoError(t, err)
	require.EqualValues(t, 1, first.editOperations.completed.Load(), "nested handler borrows its owning Server witness")
	require.EqualValues(t, 1, second.editOperations.completed.Load(), "another Server must not borrow or update the first's counter")
}

func TestEditOperationLateHandlerCompletionIsNotCallerReply(t *testing.T) {
	s := &Server{ToolCallTimeout: 20 * time.Millisecond}
	entered, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() {
		stop()
		select {
		case <-joined:
		case <-time.After(time.Second):
			t.Error("owned actual handler did not join")
		}
	})
	h := s.boundToolHandler(func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		defer close(joined)
		observeEditSourceEffect(ctx, true, true)
		observeEditSourceComplete(ctx)
		close(entered)
		<-release // model the non-cancellable physical/journal completion
		return mcp.NewToolResultText("applied"), nil
	})
	replied := make(chan *mcp.CallToolResult, 1)
	go func() {
		result, _ := h(t.Context(), editOperationRequest("batch_edit", ""))
		replied <- result
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("actual handler never started")
	}
	select {
	case result := <-replied:
		require.True(t, result.IsError)
	case <-time.After(time.Second):
		t.Fatal("caller did not receive its deadline result")
	}
	require.Zero(t, s.editOperations.completed.Load(), "timeout reply is not completion of the actual handler")
	stop()
	require.Eventually(t, func() bool { return s.editOperations.completed.Load() == 1 }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return AbandonedToolCalls() == 0 }, time.Second, time.Millisecond)
	require.Zero(t, s.editOperations.committedWithError.Load(), "the actual handler result, not its caller's timeout, defines this outcome")
}

func TestEditOperationReadWitnessPathAddsNoAllocation(t *testing.T) {
	s := &Server{}
	result := mcp.NewToolResultText("read")
	h := s.observeEditOperationHandler(func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if editOperationFrom(ctx) != nil {
			panic("read received an edit witness")
		}
		return result, nil
	})
	req := editOperationRequest("read", "source")
	ctx := t.Context()
	allocs := testing.AllocsPerRun(100, func() { _, _ = h(ctx, req) })
	require.Zero(t, allocs, "only this narrow observer's read branch, not total middleware overhead")
}
