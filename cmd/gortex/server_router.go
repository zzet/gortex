package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/server"
)

// newLocalToolExecutor builds the daemon.LocalExecutor closure used by
// the multi-server router. It wraps the in-process MCP server's tool
// dispatch with the same body-shape the HTTP handler
// accepts (`{"arguments": {...}}` or flat-args), so a router-routed
// local call returns bytes shaped exactly like a remote-proxied
// response — the caller can't tell the difference and the response
// content type stays JSON.
//
// We re-use the local mcp.Server's MCPServer() rather than calling
// back into the HTTP handler so this path doesn't recursively flow
// through the router again. The router's localExec is the single
// canonical "run this tool here" entrypoint; everything else goes
// through it.
func newLocalToolExecutor(srv *gortexmcp.Server, logger *zap.Logger) daemon.LocalExecutor {
	if srv == nil || srv.MCPServer() == nil {
		return func(_ context.Context, _ string, _ []byte) ([]byte, int, error) {
			return nil, 500, fmt.Errorf("local executor: no MCP server attached")
		}
	}
	return func(ctx context.Context, toolName string, body []byte) ([]byte, int, error) {
		// Validate the request body before any lookup, promotion, or
		// invocation: malformed JSON must 400 without touching the
		// registry or running a handler. A JSON-null body (top-level
		// `null` or `{"arguments": null}`) is rejected explicitly —
		// json.Unmarshal treats null as a silent no-op for both struct
		// and map targets, so it would otherwise sail through as "no
		// arguments" instead of being flagged as malformed input.
		var args map[string]any
		if len(body) > 0 {
			var probe any
			if err := json.Unmarshal(body, &probe); err != nil {
				payload := map[string]any{
					"error":   "invalid_json",
					"message": fmt.Sprintf("malformed request body: %s", err.Error()),
				}
				out, _ := json.Marshal(payload)
				return out, 400, nil
			}
			obj, ok := probe.(map[string]any)
			if !ok {
				payload := map[string]any{
					"error":   "invalid_json",
					"message": "malformed request body: expected a JSON object",
				}
				out, _ := json.Marshal(payload)
				return out, 400, nil
			}
			if rawArgs, present := obj["arguments"]; present {
				nested, ok := rawArgs.(map[string]any)
				if !ok {
					payload := map[string]any{
						"error":   "invalid_json",
						"message": `malformed request body: "arguments" must be a JSON object`,
					}
					out, _ := json.Marshal(payload)
					return out, 400, nil
				}
				args = nested
			} else {
				args = obj
			}
		}

		// An already-live tool (whether generally allowed or blocked by
		// the session's active preset/facade surface) dispatches
		// straight to its handler with NO gate here: every production
		// registration path (addTool, addControlTool, lazy promote,
		// facade_tools) wraps the handler with wrapToolHandlerMode,
		// which runs checkToolGate on every call — including this one,
		// now that ctx carries the caller's session id (see the
		// handleToolCall / tryRouteToolCall ctx-ordering fix). That gate
		// is what should decide a blocked-by-preset call: it returns a
		// structured tool_blocked_by_mode error the client can act on
		// (which preset, how to reconnect). Adding a coarser gate here
		// too previously collapsed that structured error into a bare
		// 404 "not found" — a lie for a tool that IS registered — so
		// this path deliberately does not duplicate the check for an
		// already-live tool.
		//
		// A NOT-yet-live (deferred) tool is different: promoting it is
		// itself a side effect (it mutates the shared lazy registry
		// process-wide), so that side effect must stay gated on the
		// session's effective surface — EnsureToolPromotedForSession
		// checks IsToolEnabledForSession before promoting, so a session
		// whose surface hides the tool never promotes it (and gets a
		// 404, since there's nothing live to dispatch to and nothing to
		// promote on its behalf).
		tool := srv.MCPServer().GetTool(toolName)
		if tool == nil {
			if srv.EnsureToolPromotedForSession(ctx, toolName) {
				ctx = gortexmcp.WithAuthorizedToolCall(ctx, toolName)
				tool = srv.MCPServer().GetTool(toolName)
			}
		}
		if tool == nil {
			payload := map[string]any{
				"error":   "tool_not_found",
				"message": fmt.Sprintf("tool '%s' not found", toolName),
			}
			out, _ := json.Marshal(payload)
			return out, 404, nil
		}

		mcpReq := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name:      toolName,
				Arguments: args,
			},
		}
		result, err := tool.Handler(ctx, mcpReq)
		if err != nil {
			logger.Warn("local executor: tool call error",
				zap.String("tool", toolName), zap.Error(err))
			payload := map[string]any{
				"error":   "tool_error",
				"message": err.Error(),
			}
			out, _ := json.Marshal(payload)
			return out, 500, nil
		}

		// Reuse the SAME response type internal/server's HTTP handler
		// and the Streamable HTTP transport's wrapToolResultAsJSONRPC
		// both serialize/parse (server.ToolResponse: "content"/
		// "isError") — not an independently-typed lookalike. A prior
		// version of this struct tagged the error field "is_error"
		// (snake_case); wrapToolResultAsJSONRPC only recognizes
		// "isError" and silently defaults IsError to false on a
		// mismatch, so a genuine tool error routed through the
		// Streamable HTTP transport's local-fast path was reported to
		// the client as a successful result. Sharing one type makes
		// that class of drift a compile error instead of a silent
		// wire-format bug.
		resp := server.ToolResponse{IsError: result.IsError}
		for _, c := range result.Content {
			if tc, ok := c.(mcp.TextContent); ok {
				resp.Content = append(resp.Content, server.ToolContent{
					Type: "text",
					Text: tc.Text,
				})
			}
		}
		out, err := json.Marshal(resp)
		if err != nil {
			return nil, 500, err
		}
		return out, 200, nil
	}
}
