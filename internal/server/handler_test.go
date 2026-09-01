package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

func newTestHandler(t *testing.T) *Handler {
	t.Helper()

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "test.go::Foo", Kind: graph.KindFunction,
		Name: "Foo", FilePath: "test.go", Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "test.go::Bar", Kind: graph.KindFunction,
		Name: "Bar", FilePath: "test.go", Language: "go",
	})

	srv := mcpserver.NewMCPServer("gortex-test", "0.0.1-test",
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithRecovery(),
	)
	srv.AddTool(
		mcp.NewTool("echo",
			mcp.WithDescription("Echo tool for testing"),
			mcp.WithString("message", mcp.Description("Message to echo")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			msg, _ := args["message"].(string)
			if msg == "" {
				msg = "no message"
			}
			return mcp.NewToolResultText(msg), nil
		},
	)

	return NewHandler(srv, g, "0.0.1-test", zap.NewNop())
}

func TestHealthEndpoint(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp HealthResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "ok", resp.Status)
	assert.True(t, resp.Indexed)
	assert.Equal(t, 2, resp.Nodes)
	assert.Equal(t, "0.0.1-test", resp.Version)
	assert.Greater(t, resp.UptimeSeconds, float64(0))
}

func TestListToolsEndpoint(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/tools", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var tools []toolInfo
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&tools))
	require.Len(t, tools, 1)
	assert.Equal(t, "echo", tools[0].Name)
	assert.Equal(t, "Echo tool for testing", tools[0].Description)
}

func TestStatsEndpoint(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/stats", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp StatsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 2, resp.TotalNodes)
	assert.Equal(t, 2, resp.ByKind["function"])
	assert.False(t, resp.StartedAt.IsZero(), "StartedAt should be populated")
	assert.Empty(t, resp.ServerID, "ServerID empty until SetServerID is called")
}

func TestStatsEndpoint_WithServerID(t *testing.T) {
	h := newTestHandler(t)
	h.SetServerID("11111111-2222-3333-4444-555555555555")

	req := httptest.NewRequest(http.MethodGet, "/v1/stats", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp StatsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "11111111-2222-3333-4444-555555555555", resp.ServerID)
}

func TestToolCallValid(t *testing.T) {
	h := newTestHandler(t)
	body := `{"arguments":{"message":"hello world"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/echo", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp ToolResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.False(t, resp.IsError)
	require.Len(t, resp.Content, 1)
	assert.Equal(t, "hello world", resp.Content[0].Text)
}

func TestToolCallUnknownTool(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/nonexistent", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "tool_not_found", resp["error"])
	available, ok := resp["available_tools"].([]any)
	require.True(t, ok)
	assert.Contains(t, available, "echo")
}

// TestToolCallAnalyzeAliasedKindRoutesThroughFacade pins the HTTP-facing
// contract: POST /v1/tools/analyze with kind=processes reaches the facade
// (which routes to the captured legacy handler) without any registry
// promotion. This is the dashboard's /v1/processes path under core/defer.
func TestToolCallAnalyzeAliasedKindRoutesThroughFacade(t *testing.T) {
	h := newTestHandler(t)
	legacyCalled := false
	h.mcpServer.AddTool(
		mcp.NewTool("analyze", mcp.WithDescription("dispatcher"),
			mcp.WithString("kind", mcp.Required())),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			kind, _ := req.GetArguments()["kind"].(string)
			if kind == "processes" {
				legacyCalled = true
				return mcp.NewToolResultText(`{"processes":[]}`), nil
			}
			return mcp.NewToolResultError("unknown analyze kind: " + kind), nil
		},
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/tools/analyze",
		strings.NewReader(`{"arguments":{"kind":"processes"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, legacyCalled, "analyze kind=processes must reach the legacy handler")
	var resp ToolResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Content, 1)
	assert.Contains(t, resp.Content[0].Text, `"processes"`)
}

func TestToolCallMalformedJSON(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/echo", strings.NewReader("{bad"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestToolCallEmptyName(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestToolCallAcceptsQueryFormat(t *testing.T) {
	h := newTestHandler(t)
	// echo returns its "message" argument; here we smuggle the format
	// through "message" to assert the merge happened. Use a tool that
	// echoes the argument map — simplest path is to register a fresh
	// tool that stringifies args.
	body := `{"arguments":{"message":"fmt"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/echo?format=gcx", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// The echo tool ignores "format", so the ok status is the
	// only observable contract here — the point is that the merge
	// didn't corrupt the request. Dedicated merge logic is covered by
	// TestToolCall_MergesQueryFormat.
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestToolCall_MergesQueryFormat(t *testing.T) {
	g := graph.New()
	srv := mcpserver.NewMCPServer("gortex-test", "0.0.1-test",
		mcpserver.WithToolCapabilities(false),
	)
	srv.AddTool(
		mcp.NewTool("spy",
			mcp.WithDescription("returns its format arg"),
			mcp.WithString("format"),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			fmtArg, _ := args["format"].(string)
			return mcp.NewToolResultText(fmtArg), nil
		},
	)
	h := NewHandler(srv, g, "0.0.1-test", zap.NewNop())

	// Query param.
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/spy?format=gcx", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp ToolResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Content, 1)
	assert.Equal(t, "gcx", resp.Content[0].Text)

	// Body-level format field.
	req = httptest.NewRequest(http.MethodPost, "/v1/tools/spy",
		strings.NewReader(`{"format":"toon"}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Content, 1)
	assert.Equal(t, "toon", resp.Content[0].Text)

	// Explicit arguments.format wins over query.
	req = httptest.NewRequest(http.MethodPost, "/v1/tools/spy?format=gcx",
		strings.NewReader(`{"arguments":{"format":"json"}}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Content, 1)
	assert.Equal(t, "json", resp.Content[0].Text)
}

func TestPanicRecovery(t *testing.T) {
	g := graph.New()
	srv := mcpserver.NewMCPServer("gortex-test", "0.0.1-test",
		mcpserver.WithToolCapabilities(false),
	)
	srv.AddTool(
		mcp.NewTool("panic_tool", mcp.WithDescription("panics")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			panic("test panic")
		},
	)
	h := NewHandler(srv, g, "0.0.1-test", zap.NewNop())

	req := httptest.NewRequest(http.MethodPost, "/v1/tools/panic_tool", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	assert.NotPanics(t, func() { h.ServeHTTP(rec, req) })
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestCallToolHelper(t *testing.T) {
	h := newTestHandler(t)
	result := h.CallTool(context.Background(), "echo", map[string]any{"message": "test"})
	assert.Equal(t, "test", result)

	result = h.CallTool(context.Background(), "nonexistent", nil)
	assert.Empty(t, result)
}

func TestV1GraphEndpoint_FullDump(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/graph", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp GraphResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.Nodes, 2)
	assert.Equal(t, 2, resp.Stats.TotalNodes)
	// Heavy fields stripped.
	for _, n := range resp.Nodes {
		assert.Empty(t, n.QualName)
		assert.Zero(t, n.EndLine)
	}
}

func TestV1GraphEndpoint_RepoFilter(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "frontend::App", Kind: graph.KindFunction, Name: "App",
		FilePath: "src/app.ts", Language: "typescript", RepoPrefix: "frontend",
	})
	g.AddNode(&graph.Node{
		ID: "backend::Server", Kind: graph.KindFunction, Name: "Server",
		FilePath: "cmd/main.go", Language: "go", RepoPrefix: "backend",
	})
	srv := mcpserver.NewMCPServer("gortex-test", "0.0.1-test",
		mcpserver.WithToolCapabilities(false),
	)
	h := NewHandler(srv, g, "0.0.1-test", zap.NewNop())

	req := httptest.NewRequest(http.MethodGet, "/v1/graph?repo=frontend", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp GraphResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Nodes, 1)
	assert.Equal(t, "frontend::App", resp.Nodes[0].ID)
	// Filtered dump: stats are zeroed; counts come from the nodes array.
	assert.Equal(t, 0, resp.Stats.TotalNodes)
}

func TestV1GraphEndpoint_ProjectWithoutConfigManager(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/graph?project=my-saas", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var body map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Contains(t, body["message"], "multi-repo config")
}

func TestV1EventsEndpoint_NoHub(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Body.String(), "watch mode not active")
}
