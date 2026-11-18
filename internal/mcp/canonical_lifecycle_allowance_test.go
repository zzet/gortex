package mcp

import (
	"context"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// The lifecycle fixtures' explicit allowance must not change an ordinary
// request's production default or install a new allowance on a nested pass.
func TestProjectedReadDefaultAllowanceRemains150Milliseconds(t *testing.T) {
	for _, tool := range []string{"get_symbol_source", "get_file_summary", "get_editing_context"} {
		t.Run(tool, func(t *testing.T) {
			s := &Server{}
			var observed context.Context
			leaf := func(ctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				observed = ctx
				return mcplib.NewToolResultText("budget observed"), nil
			}
			request := makeReq(tool, map[string]any{})
			before := time.Now()
			_, err := s.withProjectedSourceReadBudget(leaf)(t.Context(), request)
			after := time.Now()
			require.NoError(t, err)
			require.Equal(t, 150*time.Millisecond, projectedReadBudget)
			deadline := observed.Value(projectedSourceDeadlineKey{}).(time.Time)
			require.False(t, deadline.Before(before.Add(150*time.Millisecond)))
			require.False(t, deadline.After(after.Add(150*time.Millisecond)))
			allowance := observed.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
			require.Equal(t, tool, allowance.tool)
			require.Zero(t, allowance.attempts.Load())
			_, err = s.withProjectedSourceReadBudget(leaf)(observed, request)
			require.NoError(t, err)
			require.Equal(t, deadline, observed.Value(projectedSourceDeadlineKey{}))
			require.Same(t, allowance, observed.Value(projectedSourceAllowanceKey{}))
			require.True(t, allowance.claim())
			require.True(t, allowance.claim())
			require.False(t, allowance.claim(), "an ordinary request gets at most two attempts")
		})
	}
}

func TestInjectedLifecycleAllowanceCannotCreateAThirdAttempt(t *testing.T) {
	for _, tool := range []string{"get_symbol_source", "get_file_summary", "get_editing_context"} {
		t.Run(tool, func(t *testing.T) {
			s := &Server{}
			deadline := time.Now().Add(canonicalLifecycleFixtureBudget)
			allowance := &projectedSourceAllowance{tool: tool}
			require.True(t, allowance.claim())
			require.True(t, allowance.claim())
			ctx := context.WithValue(t.Context(), projectedSourceDeadlineKey{}, deadline)
			ctx = context.WithValue(ctx, projectedSourceAllowanceKey{}, allowance)
			calls := 0
			leaf := func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				calls++
				return mcplib.NewToolResultText("unexpected third attempt"), nil
			}
			observe := func(call context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				require.Equal(t, deadline, call.Value(projectedSourceDeadlineKey{}))
				require.Same(t, allowance, call.Value(projectedSourceAllowanceKey{}))
				return s.retryProjectedSymbolSource(leaf)(call, request)
			}
			result, err := s.withProjectedSourceReadBudget(observe)(ctx, makeReq(tool, map[string]any{}))
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.IsError)
			require.Zero(t, calls)
			require.EqualValues(t, 2, allowance.attempts.Load())
			require.NotContains(t, resultText(result), "readback_exact")
		})
	}
}
