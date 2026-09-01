package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

// realServerTestHandler builds a Handler backed by a REAL gortexmcp.Server
// (not the bare mark3labs mcpserver.MCPServer newTestHandler uses), so
// session-policy gating (checkToolGate / effectiveSessionPolicy) is
// actually exercised. Needed for tests pinning the session-identity /
// overlay-cohort separation, which lives entirely in that machinery.
func realServerTestHandler(t *testing.T) (*Handler, *gortexmcp.Server) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

func main() {}
`), 0o644))

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	eng := query.NewEngine(g)
	srv := gortexmcp.NewServer(eng, g, idx, nil, zap.NewNop(), nil)
	h := NewHandler(srv.MCPServer(), g, "0.0.1-test", zap.NewNop())
	return h, srv
}

// TestToolCall_OverlayHeaderDoesNotOverridePolicySession pins the fix for
// the reviewer's finding: X-Gortex-Overlay-Session must scope ONLY overlay
// state to a different cohort id — it must never substitute for
// Mcp-Session-Id when evaluating the caller's tool-policy. Before the fix,
// handleToolCall picked one winner (the overlay header, when present) and
// fed it into gortexmcp.WithSessionID, so a restricted session paired with
// a permissive overlay-cohort override had its policy silently bypassed.
//
// Setup: session "restricted" is facade-v1/hide (blocks find_clones,
// pre-promoted here since handler.go's direct-dispatch path doesn't
// promote deferred tools — a documented, separate, pre-existing gap).
// Session "overlay-open" has no policy noted at all (unrestricted). The
// request carries Mcp-Session-Id: restricted and
// X-Gortex-Overlay-Session: overlay-open. If the overlay header were
// still hijacking policy evaluation, the call would succeed under
// overlay-open's unrestricted policy; with the fix, "restricted"'s
// hide-mode policy applies and the call is blocked.
func TestToolCall_OverlayHeaderDoesNotOverridePolicySession(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	h, srv := realServerTestHandler(t)
	require.Nil(t, srv.MCPServer().GetTool("find_clones"), "test setup: find_clones must start deferred")
	require.True(t, srv.EnsureToolPromoted("find_clones"), "test setup: find_clones must promote cleanly")
	require.NotNil(t, srv.MCPServer().GetTool("find_clones"), "test setup: find_clones must be live before the policy is applied")

	srv.NoteSessionToolPolicy("restricted", "facade-v1", "hide")
	// "overlay-open" intentionally gets no NoteSessionToolPolicy call —
	// it is the permissive/default session a pre-fix bug would wrongly
	// evaluate policy against.

	req := httptest.NewRequest(http.MethodPost, "/v1/tools/find_clones", strings.NewReader("{}"))
	req.Header.Set("Mcp-Session-Id", "restricted")
	req.Header.Set("X-Gortex-Overlay-Session", "overlay-open")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "a blocked-by-preset call still dispatches to the handler, which reports the block structurally")
	var resp ToolResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.True(t, resp.IsError, "restricted session's hide-mode policy must apply despite the overlay-cohort override naming a permissive session")
	require.Len(t, resp.Content, 1)
	assert.Contains(t, resp.Content[0].Text, "tool_blocked_by_mode",
		"expected the structured block error from restricted's policy, not overlay-open's unrestricted one")
}

// TestToolCall_OverlayHeaderScopesOverlayState is the positive
// counterpart to the test above: it proves X-Gortex-Overlay-Session
// actually does something, rather than only proving it doesn't leak
// into policy. Without this, WithOverlayCohortID could be deleted
// entirely and the negative test would still pass (the header would
// simply be ignored). Pushes an overlay file under cohort
// "overlay-open" via the real overlay_push tool while authenticating
// as a different Mcp-Session-Id, then confirms the file landed under
// the cohort id, not the caller's own session id.
func TestToolCall_OverlayHeaderScopesOverlayState(t *testing.T) {
	h, srv := realServerTestHandler(t)
	srv.SetOverlayManager(daemon.NewOverlayManager(30 * time.Minute))

	body := `{"arguments":{"path":"scratch.go","content":"package main\n"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/overlay_push", strings.NewReader(body))
	req.Header.Set("Mcp-Session-Id", "caller-session")
	req.Header.Set("X-Gortex-Overlay-Session", "overlay-open")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp ToolResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.False(t, resp.IsError, "overlay_push must succeed: %+v", resp)

	files, err := srv.OverlayManager().Files("overlay-open")
	require.NoError(t, err, "the push must land under the overlay-cohort id")
	require.Contains(t, files, "scratch.go")

	_, err = srv.OverlayManager().Files("caller-session")
	assert.Error(t, err, "the push must NOT land under the caller's own session id")
}

// TestToolCall_SessionCWDReachesHandler pins the other local half of the
// same finding: handleToolCall must attach the resolved cwd to ctx via
// gortexmcp.WithSessionCWD so a tool handler can read (and enforce) the
// session's workspace boundary. Before the fix, cwd was computed
// (peekRouteContext) and used only to build the router's RouteInputs —
// never attached to ctx — so any tool consulting
// gortexmcp.SessionCWDFromContext saw nothing on this path.
func TestToolCall_SessionCWDReachesHandler(t *testing.T) {
	h, srv := realServerTestHandler(t)
	var observedCWD string
	srv.MCPServer().AddTool(
		mcp.NewTool("spy_cwd", mcp.WithDescription("reports the session cwd from context")),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			observedCWD = gortexmcp.SessionCWDFromContext(ctx)
			return mcp.NewToolResultText("ok"), nil
		},
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/tools/spy_cwd", strings.NewReader("{}"))
	req.Header.Set("X-Gortex-Cwd", "/repo/A")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "/repo/A", observedCWD, "the session's resolved cwd must reach the tool handler via context")
}
