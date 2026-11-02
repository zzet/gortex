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
type projectedSourceAllowance struct {
	attempts atomic.Int32
	tool     string
}

// Only these nonpaginated file reads have audited replay-safe response work.
// Editing-context compression still uses its legacy physical source reader;
// it keeps the existing refusal behavior rather than gaining another attempt.
func auditedProjectionReadRetry(tool string, req mcplib.CallToolRequest) bool {
	switch tool {
	case "get_symbol_source", "get_file_summary":
		return true
	case "get_editing_context":
		args := req.GetArguments()
		var options map[string]any
		if value, present := args["options"]; present {
			var valid bool
			options, valid = value.(map[string]any)
			if !valid {
				return false
			}
		}
		for _, values := range []map[string]any{args, options} {
			if value, present := values["compress_bodies"]; present {
				compressed, valid := value.(bool)
				if !valid || compressed {
					return false
				}
			}
		}
		return true
	default:
		return false
	}
}

func (s *Server) withProjectedSourceReadBudget(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		legacy, known := s.legacyToolName(&req)
		// Full symbol metadata may restart only its early projection selection,
		// before recordSymbol/enrichNodeOnDemand. Keep it out of the audited
		// after-leaf replay set, which would include those response side effects.
		if !known || !auditedProjectionReadRetry(legacy, req) && legacy != "get_symbol" {
			return next(ctx, req)
		}
		if _, exists := ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance); !exists {
			deadline := time.Now().Add(projectedReadBudget)
			if prior, ok := ctx.Value(projectedSourceDeadlineKey{}).(time.Time); ok && prior.Before(deadline) {
				deadline = prior
			}
			ctx = context.WithValue(ctx, projectedSourceDeadlineKey{}, deadline)
			ctx = context.WithValue(ctx, projectedSourceAllowanceKey{}, &projectedSourceAllowance{tool: legacy})
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

// Permission to revalidate from scratch, not freshness authority for an old
// response. Peer owners, invalidation and unchecked eviction remain refusals.
func retrySourceAfterCheckedRetirement(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	if _, ok := ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance); !ok {
		return false
	}
	deadline, ok := ctx.Value(projectedSourceDeadlineKey{}).(time.Time)
	if !ok || !time.Now().Before(deadline) {
		return false
	}
	facts, ok := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
	if !ok {
		return false
	}
	facts.mu.RLock()
	defer facts.mu.RUnlock()
	retired := false
	for _, entry := range facts.entries {
		if entry == nil {
			return false
		}
		current := pendingFileProjections.snap.Load().entries[entry.key]
		if current == entry && liveProjectionForAcknowledgement(entry) {
			continue
		}
		if current != nil || !entry.checkedRetired.Load() || entry.suspect.Load() {
			return false
		}
		retired = true
	}
	return retired
}

// Audited file reads retry once after checked retirement; source also retains
// its typed byte-mismatch retry. Every attempt has fresh facts/layers/leases
// under the original shared deadline and two-attempt allowance.
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
