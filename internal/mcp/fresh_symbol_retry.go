package mcp

import (
	"context"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

type freshSymbolAttemptKey struct{}
type freshSymbolDeadlineKey struct{}
type freshSymbolAttempt struct {
	withdrawn    bool
	replayUnsafe bool
	finish       []func()
	page         *provisionalSymbolPage
	accepted     []func()
	decorate     func(*mcplib.CallToolResult) *mcplib.CallToolResult
}

// Replay only audited logical reads. Traversal renderers have no continuation
// state; on-demand refs confirmation and proxy hydration fill idempotent derived
// caches, not user mutations. File-access bookkeeping follows acceptance below.
// Late-refusal safety alone is not a replay contract for every read tool.
func freshReadReplayAllowed(legacy string) bool {
	switch legacy {
	case "search_symbols", "get_callers", "get_call_chain", "get_dependencies", "get_dependents", "read_file", "get_symbol_source":
		return true
	default:
		return false
	}
}

func (s *Server) retryFreshSymbolSearch(attempt mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		freshness := takeRequestFreshness(&req)
		legacy, known := s.legacyToolName(&req)
		cursor, _ := requestRawArg(&req, "cursor")
		if !known || !freshReadReplayAllowed(legacy) || !freshness.requireExact || !freshness.requireFresh || freshness.err != nil || (cursor != nil && cursor != "") {
			return attempt(ctx, req)
		}
		started := time.Now()
		deadline := freshness.effectiveDeadline(started, ctx)
		ctx = withToolReceivedAt(ctx, started)
		ctx = withLocalizationFileRequestBudget(ctx)
		bounded := context.WithValue(ctx, freshSymbolDeadlineKey{}, deadline)
		var last *mcplib.CallToolResult
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !time.Now().Before(deadline) {
				if last != nil {
					return last, nil
				}
				return mcplib.NewToolResultError(freshnessExactRefusal(freshReasonDeadlineExceeded, time.Since(started)).Error()), nil
			}
			copy := req
			args := cloneFreshSymbolArguments(req.GetArguments())
			copy.Params.Arguments = args
			state := &freshSymbolAttempt{}
			result, err := runFreshSymbolAttempt(bounded, copy, state, attempt)
			if err != nil || !state.withdrawn || state.replayUnsafe {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return result, err
			}
			last = result
			if err := waitFreshnessRetry(bounded, deadline); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return last, nil
			}
		}
	}
}

func cloneFreshSymbolArguments(args map[string]any) map[string]any {
	copy := make(map[string]any, len(args)+1)
	for key, value := range args {
		copy[key] = cloneFreshSymbolValue(value)
	}
	return copy
}

func cloneFreshSymbolValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneFreshSymbolArguments(value)
	case []any:
		copy := make([]any, len(value))
		for i, item := range value {
			copy[i] = cloneFreshSymbolValue(item)
		}
		return copy
	default:
		return value
	}
}

func runFreshSymbolAttempt(ctx context.Context, req mcplib.CallToolRequest, state *freshSymbolAttempt, attempt mcpserver.ToolHandlerFunc) (*mcplib.CallToolResult, error) {
	defer func() {
		if state.page != nil {
			state.page.entry.retire()
		}
		for _, finish := range state.finish {
			finish()
		}
	}()
	return attempt(context.WithValue(ctx, freshSymbolAttemptKey{}, state), req)
}

func noteFreshSymbolWithdrawal(ctx context.Context, view *requestView, result *mcplib.CallToolResult) {
	state, _ := ctx.Value(freshSymbolAttemptKey{}).(*freshSymbolAttempt)
	if state == nil || result == nil || result.IsError || view == nil || view.rider == nil {
		return
	}
	view.mu.Lock()
	state.withdrawn = view.rider.FallbackReason == baseChangedFallbackReason
	view.mu.Unlock()
}

func (s *Server) commitFreshSymbolPage(ctx context.Context, result *mcplib.CallToolResult, tool string, view *requestView) (*mcplib.CallToolResult, error) {
	state, _ := ctx.Value(freshSymbolAttemptKey{}).(*freshSymbolAttempt)
	if state == nil {
		return result, nil
	}
	if state.page != nil {
		page := state.page
		state.page = nil
		var err error
		result, err = page.publish(ctx, func() *mcplib.CallToolResult {
			refused := s.refuseWithdrawnExactness(tool, true, view)
			if refused != nil {
				noteFreshSymbolWithdrawal(ctx, view, result)
			}
			return refused
		})
		if state.decorate != nil {
			result = state.decorate(result)
		}
		if err != nil || result == nil || result.IsError {
			return result, err
		}
	}
	for _, commit := range state.accepted {
		commit()
	}
	state.accepted = nil
	return result, nil
}

// Session savings, file access and last-search bookkeeping follow accepted answers, not
// discarded assembly attempts. Ordinary unwrapped calls retain their behavior.
func afterFreshSymbolAcceptance(ctx context.Context, commit func()) {
	if state, _ := ctx.Value(freshSymbolAttemptKey{}).(*freshSymbolAttempt); state != nil {
		state.accepted = append(state.accepted, commit)
	} else {
		commit()
	}
}
