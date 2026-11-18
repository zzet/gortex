package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestComposeDaemonHTTPHandler asserts the daemon's single HTTP listener
// routes /mcp to the streamable surface and /v1 to the REST handler, that
// /v1 is bearer-auth gated, and that CORS headers are emitted.
func TestComposeDaemonHTTPHandler(t *testing.T) {
	savedOrigins := daemonHTTPAllowedOrigins
	daemonHTTPAllowedOrigins = []string{"https://app.example.com"}
	t.Cleanup(func() { daemonHTTPAllowedOrigins = savedOrigins })
	streamH := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("MCP"))
	})
	v1 := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("V1"))
	})

	h := composeDaemonHTTPHandler(streamH, v1, func() string { return "secret" }, "*")

	// /mcp (and any non-/v1 path) routes to the streamable surface.
	t.Run("mcp routes to streamable", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
		if rec.Body.String() != "MCP" {
			t.Fatalf("/mcp body = %q, want MCP", rec.Body.String())
		}
	})

	// /v1 without a token is rejected.
	t.Run("v1 requires auth", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("/v1 without token = %d, want 401", rec.Code)
		}
	})

	// /v1 with the bearer token reaches the REST handler.
	t.Run("v1 with token reaches handler", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		req.Header.Set("Authorization", "Bearer secret")
		h.ServeHTTP(rec, req)
		if rec.Body.String() != "V1" {
			t.Fatalf("/v1 with token body = %q, want V1", rec.Body.String())
		}
	})

	// CORS preflight is answered (and exempt from auth) with the origin header.
	t.Run("cors preflight", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodOptions, "/v1/tools/search_symbols", nil)
		req.Header.Set("Origin", "https://app.example.com")
		h.ServeHTTP(rec, req)
		if rec.Header().Get("Access-Control-Allow-Origin") == "" {
			t.Fatalf("missing Access-Control-Allow-Origin header on preflight")
		}
	})
}

// TestComposeDaemonHTTPHandler_NoCORS asserts an empty origin skips the
// CORS wrapper (no header injected).
func TestComposeDaemonHTTPHandler_NoCORS(t *testing.T) {
	v1 := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	h := composeDaemonHTTPHandler(http.NewServeMux(), v1, func() string { return "" }, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Origin", "https://app.example.com")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("untrusted browser origin = %d, want 403", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("CORS header present with empty origin: %q", got)
	}
}
