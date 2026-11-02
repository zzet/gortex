package mcp

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

var errProjectedSourceMoved = errors.New("selected source bytes changed during the read")
var errProjectedSourceDeferred = errors.New("source is pending after the projection budget expired")

func invalidateProjectedSource(entry *projectionEntry) error {
	pendingFileProjections.invalidateCurrent(entry)
	return errProjectedSourceMoved
}

type projectedSourceDeadlineKey struct{}
type projectedSourceAllowanceKey struct{}
type projectedSourceAllowance struct{ attempts atomic.Int32 }

func (s *Server) withProjectedSourceReadBudget(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		legacy, known := s.legacyToolName(&req)
		if !known || legacy != "get_symbol_source" {
			return next(ctx, req)
		}
		if _, exists := ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance); !exists {
			deadline := time.Now().Add(projectedReadBudget)
			if prior, ok := ctx.Value(projectedSourceDeadlineKey{}).(time.Time); ok && prior.Before(deadline) {
				deadline = prior
			}
			ctx = context.WithValue(ctx, projectedSourceDeadlineKey{}, deadline)
			ctx = context.WithValue(ctx, projectedSourceAllowanceKey{}, &projectedSourceAllowance{})
		}
		return next(ctx, req)
	}
}

func (a *projectedSourceAllowance) claim() bool {
	for {
		before := a.attempts.Load()
		if before >= 2 {
			return false
		}
		if a.attempts.CompareAndSwap(before, before+1) {
			return true
		}
	}
}

// Only this audited source read retries, once, for a typed byte mismatch. Each
// attempt runs the whole middleware with fresh facts/layers and releases its
// leases before the next; the first attempt's projection deadline is retained.
func (s *Server) retryProjectedSymbolSource(attempt mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		allowance, ok := ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
		if !ok {
			return attempt(ctx, req)
		}
		for {
			if !allowance.claim() {
				return mcplib.NewToolResultError(errProjectedSourceMoved.Error()), nil
			}
			copy := req
			copy.Params.Arguments = cloneFreshSymbolArguments(req.GetArguments())
			result, err := runProjectedSourceAttempt(ctx, copy, attempt)
			if !errors.Is(err, errProjectedSourceMoved) {
				return result, err
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// A caller-owned selection cannot be rebound by this nested invocation.
			if ctx.Value(requestProjectionFactsKey{}) != nil {
				return mcplib.NewToolResultError(errProjectedSourceMoved.Error()), nil
			}
		}
	}
}

func runProjectedSourceAttempt(ctx context.Context, req mcplib.CallToolRequest, attempt mcpserver.ToolHandlerFunc) (*mcplib.CallToolResult, error) {
	state, _ := ctx.Value(freshSymbolAttemptKey{}).(*freshSymbolAttempt)
	if state == nil {
		return runFreshSymbolAttempt(ctx, req, &freshSymbolAttempt{}, attempt)
	}
	accepted, finish := len(state.accepted), len(state.finish)
	result, err := attempt(ctx, req)
	if errors.Is(err, errProjectedSourceMoved) {
		state.accepted = state.accepted[:accepted]
		for _, release := range state.finish[finish:] {
			release()
		}
		state.finish = state.finish[:finish]
	}
	return result, err
}
