package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// TestCallToolStrict_Success returns the text content with a nil error
// when the tool returns a normal text result.
func TestCallToolStrict_Success(t *testing.T) {
	h := newTestHandler(t)
	text, err := h.CallToolStrict(context.Background(), "echo", map[string]any{"message": "hello"})
	require.NoError(t, err)
	assert.Equal(t, "hello", text)
}

// TestCallToolStrict_MissingTool returns an error when the tool name is not
// registered, instead of silently returning an empty string.
func TestCallToolStrict_MissingTool(t *testing.T) {
	h := newTestHandler(t)
	_, err := h.CallToolStrict(context.Background(), "no-such-tool", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no-such-tool")
	assert.Contains(t, err.Error(), "not registered")
}

// TestCallToolStrict_AnalyzeAliasedKindRoutesThroughFacade pins the
// dashboard fix: a deferred legacy tool (get_processes under the core/defer
// surface) is reachable via the eager `analyze` facade's aliased kind
// (processes → get_processes). CallToolStrict must dispatch the analyze
// handler, whose facade wrapper routes the aliased kind to the captured
// legacy handler — no registry promotion involved.
func TestCallToolStrict_AnalyzeAliasedKindRoutesThroughFacade(t *testing.T) {
	g := graph.New()
	srv := mcpserver.NewMCPServer("gortex-test", "0.0.1-test",
		mcpserver.WithToolCapabilities(false),
	)
	h := NewHandler(srv, g, "0.0.1-test", zap.NewNop())

	// Register an eager `analyze` tool whose handler is the facade
	// wrapper. The wrapper must route kind=processes to the captured
	// legacy handler even though the legacy tool is NOT in the live
	// registry (deferred under core/defer).
	legacyCalled := false
	srv.AddTool(
		mcp.NewTool("analyze", mcp.WithDescription("dispatcher"),
			mcp.WithString("kind", mcp.Required())),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			kind, _ := req.GetArguments()["kind"].(string)
			if kind == "processes" {
				legacyCalled = true
				return mcp.NewToolResultText(`{"processes":[]}`), nil
			}
			return mcp.NewToolResultError("unknown analyze kind: " + kind), nil
		},
	)

	text, err := h.CallToolStrict(context.Background(), "analyze", map[string]any{"kind": "processes"})
	require.NoError(t, err)
	assert.True(t, legacyCalled, "analyze kind=processes must reach the legacy handler")
	assert.Contains(t, text, `"processes"`)
}

// TestCallToolStrict_UnknownKindStillErrors keeps the dispatcher's
// unknown-kind error for non-aliased kinds — the facade must not swallow
// them into a silent empty result.
func TestCallToolStrict_UnknownKindStillErrors(t *testing.T) {
	g := graph.New()
	srv := mcpserver.NewMCPServer("gortex-test", "0.0.1-test",
		mcpserver.WithToolCapabilities(false),
	)
	h := NewHandler(srv, g, "0.0.1-test", zap.NewNop())

	srv.AddTool(
		mcp.NewTool("analyze", mcp.WithDescription("dispatcher"),
			mcp.WithString("kind", mcp.Required())),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			kind, _ := req.GetArguments()["kind"].(string)
			return mcp.NewToolResultError("unknown analyze kind: " + kind), nil
		},
	)

	_, err := h.CallToolStrict(context.Background(), "analyze", map[string]any{"kind": "bogus_kind"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown analyze kind")
}

// TestCallToolStrict_ToolErrorResult promotes an MCP IsError=true result to
// a Go error. This is the contract that handleContracts depends on to surface
// 5xx instead of pretending the call succeeded with empty content.
func TestCallToolStrict_ToolErrorResult(t *testing.T) {
	g := graph.New()
	srv := mcpserver.NewMCPServer("gortex-test", "0.0.1-test",
		mcpserver.WithToolCapabilities(false),
	)
	srv.AddTool(
		mcp.NewTool("erroring_tool", mcp.WithDescription("returns IsError=true")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultError("project not found: \"gortex\""), nil
		},
	)
	h := NewHandler(srv, g, "0.0.1-test", zap.NewNop())

	text, err := h.CallToolStrict(context.Background(), "erroring_tool", nil)
	require.Error(t, err, "IsError=true must be promoted to a Go error")
	assert.Contains(t, err.Error(), "project not found")
	// The text content is preserved so callers can put it in the response body.
	assert.Contains(t, text, "project not found")
}

// TestCallToolStrict_HandlerError surfaces a Go-level handler error.
func TestCallToolStrict_HandlerError(t *testing.T) {
	g := graph.New()
	srv := mcpserver.NewMCPServer("gortex-test", "0.0.1-test",
		mcpserver.WithToolCapabilities(false),
	)
	srv.AddTool(
		mcp.NewTool("failing_tool", mcp.WithDescription("returns Go-level error")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, errors.New("upstream blew up")
		},
	)
	h := NewHandler(srv, g, "0.0.1-test", zap.NewNop())

	_, err := h.CallToolStrict(context.Background(), "failing_tool", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upstream blew up")
}

// TestCallTool_BackwardsCompatible — the legacy best-effort wrapper still
// returns "" on error and never panics. Eval/http_handler.go and similar
// callers depend on this contract.
func TestCallTool_BackwardsCompatible(t *testing.T) {
	g := graph.New()
	srv := mcpserver.NewMCPServer("gortex-test", "0.0.1-test",
		mcpserver.WithToolCapabilities(false),
	)
	srv.AddTool(
		mcp.NewTool("erroring_tool", mcp.WithDescription("")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultError("nope"), nil
		},
	)
	h := NewHandler(srv, g, "0.0.1-test", zap.NewNop())

	// IsError=true: legacy CallTool returns the text (best-effort) and
	// callers cannot tell it was an error. This is the contract callers
	// like eval/http_handler.go rely on; the ones that need to know now
	// use CallToolStrict.
	got := h.CallTool(context.Background(), "erroring_tool", nil)
	assert.Equal(t, "nope", got)

	// Missing tool: still returns "".
	got = h.CallTool(context.Background(), "no-such-tool", nil)
	assert.Empty(t, got)
}

// TestHandleContracts_ToolError_500 verifies the user-visible bug fix: when
// the underlying contracts tool returns an error result (e.g. project not
// found), /v1/contracts must return HTTP 500 with the error in the body, not
// a fake empty 200.
func TestHandleContracts_ToolError_500(t *testing.T) {
	g := graph.New()
	srv := mcpserver.NewMCPServer("gortex-test", "0.0.1-test",
		mcpserver.WithToolCapabilities(false),
	)
	srv.AddTool(
		mcp.NewTool("analyze", mcp.WithDescription("dispatcher"),
			mcp.WithString("kind", mcp.Required())),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			kind, _ := req.GetArguments()["kind"].(string)
			if kind != "contracts" {
				return mcp.NewToolResultError("unknown analyze kind: " + kind), nil
			}
			return mcp.NewToolResultError(`project not found: "gortex" (available: )`), nil
		},
	)
	h := NewHandler(srv, g, "0.0.1-test", zap.NewNop())

	req := httptest.NewRequest(http.MethodGet, "/v1/contracts", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"tool error must surface as 5xx, not 200 with empty body")

	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	msg, _ := body["message"].(string)
	assert.True(t, strings.Contains(msg, "project not found"),
		"error body must include the underlying tool message; got: %q", msg)
}

// TestHandleContracts_Success_200 verifies the happy path still works:
// contracts tool returns a valid JSON payload → handler 200s with the
// flattened list.
func TestHandleContracts_Success_200(t *testing.T) {
	g := graph.New()
	srv := mcpserver.NewMCPServer("gortex-test", "0.0.1-test",
		mcpserver.WithToolCapabilities(false),
	)
	srv.AddTool(
		mcp.NewTool("analyze", mcp.WithDescription("dispatcher"),
			mcp.WithString("kind", mcp.Required())),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			kind, _ := req.GetArguments()["kind"].(string)
			if kind != "contracts" {
				return mcp.NewToolResultError("unknown analyze kind: " + kind), nil
			}
			payload := `{"by_repo":{"alpha":{"contracts":{"http":[{"id":"GET /foo","type":"http","role":"provider","symbol_id":"alpha/x.go::H","file_path":"alpha/x.go","line":10,"repo_prefix":"alpha"}]},"total":1}}}`
			return mcp.NewToolResultText(payload), nil
		},
	)
	h := NewHandler(srv, g, "0.0.1-test", zap.NewNop())

	req := httptest.NewRequest(http.MethodGet, "/v1/contracts", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Contracts []map[string]any `json:"contracts"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	require.Len(t, body.Contracts, 1)
	assert.Equal(t, "GET /foo", body.Contracts[0]["id"])
}
