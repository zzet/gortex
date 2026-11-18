package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// Frozen pre-acknowledgement allocation control from c3cc3e8184e1c29575ed0e25241c0e9cdbf8ec6e:
// tool_deadline.go boundToolHandler and edit_operation_observations.go
// observeEditOperationHandler, including the original callbacks and witness
// wrapper. Only these helper names and their reference are changed. A direct
// boundHandler with nil callbacks omits existing per-call work and is not the
// no-additional-allocation baseline. This is not an old full-dispatch benchmark.
func (s *Server) preAcknowledgementBoundToolHandler(h mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	// This wrapper runs INSIDE boundHandler's actual execution, so answering
	// an abandoned caller cannot count an unfinished source operation.
	h = s.preAcknowledgementObserveEditOperationHandler(h)
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := req.Params.Name
		return boundHandler(s, "tool", name, (func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error))(h),
			func(stuck int64, timeout time.Duration) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultError(busyMessage("tool calls", stuck, timeout)), nil
			},
			func(timeout time.Duration, note *mutationCommitNote, retained *requestViewPin) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultError(abandonedToolMessage(name, timeout, note.verdict(), retained)), nil
			},
			func(r any) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultError(fmt.Sprintf("tool %q internal error: %v", name, r)), nil
			},
		)(ctx, req)
	}
}

func (s *Server) preAcknowledgementObserveEditOperationHandler(h mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (result *mcp.CallToolResult, err error) {
		if s == nil || !measuredSourceEdit(req) {
			return h(ctx, req)
		}
		if existing := editOperationFrom(ctx); existing != nil && existing.owner == &s.editOperations {
			return h(ctx, req) // a raw/facade nested handler borrows the owner
		}
		w := &editOperationWitness{owner: &s.editOperations}
		ctx = context.WithValue(ctx, editOperationContextKey{}, w)
		defer func() { w.finish(result, err) }()
		return h(ctx, req)
	}
}
