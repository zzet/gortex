package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"go.uber.org/zap"
)

func TestCanonicalContractKey(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"leaves non-http IDs alone", "grpc::user.v1.UserService/GetUser", "grpc::user.v1.UserService/GetUser"},
		{"leaves unparameterised routes alone", "http::GET::/v1/health", "http::GET::/v1/health"},
		{"rewrites single param", "http::DELETE::/v1/tucks/{id}", "http::DELETE::/v1/tucks/{p1}"},
		{
			"collapses differing param names to the same key — the exact provider/consumer pairing bug",
			"http::DELETE::/v1/workspaces/{wid}/tags/{id}",
			"http::DELETE::/v1/workspaces/{p1}/tags/{p2}",
		},
		{
			"consumer variant canonicalises to the same key",
			"http::DELETE::/v1/workspaces/{workspaceId}/tags/{id}",
			"http::DELETE::/v1/workspaces/{p1}/tags/{p2}",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := canonicalContractKey(tt.in)
			if got != tt.want {
				t.Errorf("canonicalContractKey(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestContractScope(t *testing.T) {
	tests := []struct {
		name     string
		rawType  string
		producer string
		want     string
	}{
		{"go.mod dep always external", "dependency", "core-api", "external"},
		{"http with provider is own", "http", "core-api", "own"},
		{"http with no provider is external", "http", "", "external"},
		{"topic with producer is own", "topic", "worker", "own"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := contractScope(tt.rawType, tt.producer); got != tt.want {
				t.Errorf("contractScope(%q, %q) = %q, want %q", tt.rawType, tt.producer, got, tt.want)
			}
		})
	}
}

// TestToolBackedEndpointsCarryTheCallersSessionScope is the "bind a view" half
// of the /v1 fix.
//
// /v1/processes, /v1/contracts, /v1/contracts/validate, /v1/communities,
// /v1/caveats and /v1/dashboard all answer through CallToolStrict, so the MCP
// tool middleware DOES run for them — it resolves a request view and holds its
// lease for the call. But they used to hand it a bare r.Context(): no session
// id and no workspace boundary, so SelectorAuto had nothing to resolve from
// and every one of them answered from the base corpus no matter which checkout
// the caller was in. The tool must see the same identity the /v1/tools front
// door attaches.
func TestToolBackedEndpointsCarryTheCallersSessionScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
	}{
		{"processes", "/v1/processes"},
		{"contracts", "/v1/contracts"},
		{"contracts validate", "/v1/contracts/validate"},
		{"communities", "/v1/communities"},
		{"caveats", "/v1/caveats"},
		{"dashboard", "/v1/dashboard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type seen struct{ session, cwd, cohort string }
			var (
				mu   sync.Mutex
				obs  []seen
				srv  = mcpserver.NewMCPServer("gortex-test", "0.0.1-test", mcpserver.WithToolCapabilities(false))
				body = `{"processes":[]}`
			)
			srv.AddTool(mcp.NewTool("analyze", mcp.WithDescription("stub")),
				func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					mu.Lock()
					obs = append(obs, seen{
						session: gortexmcp.SessionIDFromContext(ctx),
						cwd:     gortexmcp.SessionCWDFromContext(ctx),
						cohort:  gortexmcp.OverlayCohortIDFromContext(ctx),
					})
					mu.Unlock()
					return mcp.NewToolResultText(body), nil
				})
			h := NewHandler(srv, graph.New(), "0.0.1-test", zap.NewNop())

			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			req.Header.Set("Mcp-Session-Id", "session-7")
			req.Header.Set("X-Gortex-Cwd", "/work/checkout")
			req.Header.Set("X-Gortex-Overlay-Session", "cohort-3")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

			mu.Lock()
			defer mu.Unlock()
			require.NotEmpty(t, obs, "%s never reached the tool", tc.target)
			for i, o := range obs {
				assert.Equal(t, "session-7", o.session, "%s call #%d lost the session id", tc.target, i)
				assert.Equal(t, "/work/checkout", o.cwd,
					"%s call #%d reached the view seam with no workspace boundary", tc.target, i)
				assert.Equal(t, "cohort-3", o.cohort, "%s call #%d lost the overlay cohort", tc.target, i)
			}
		})
	}
}
