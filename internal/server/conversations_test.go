package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/llm/conversationlog"
)

func newConversationHandler(t *testing.T, dir string) *Handler {
	t.Helper()
	g := graph.New()
	srv := mcpserver.NewMCPServer("gortex-test", "0.0.1-test", mcpserver.WithToolCapabilities(false))
	h := NewHandler(srv, g, "0.0.1-test", zap.NewNop())
	h.SetConversationDir(dir)
	return h
}

// seedSession writes a session JSONL file directly into dir so the route
// tests don't need a live LLM.
func seedSession(t *testing.T, dir, session string, recs []conversationlog.Record) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, session+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, rec := range recs {
		if err := enc.Encode(&rec); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConversations_ListAndLoad(t *testing.T) {
	dir := t.TempDir()
	seedSession(t, dir, "sess-A", []conversationlog.Record{
		{Session: "sess-A", File: "a.go", Phase: "plan", Response: "r1", InputTokens: 10, OutputTokens: 2, Estimated: true},
		{Session: "sess-A", File: "b.go", Phase: "main", Response: "r2", InputTokens: 20, OutputTokens: 4, Estimated: true},
	})
	h := newConversationHandler(t, dir)
	// Loopback host so the guard allows without a token.
	srv := httptest.NewServer(h)
	defer srv.Close()

	// List.
	resp, err := http.Get(srv.URL + "/v1/conversations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	var list struct {
		Sessions []conversationlog.SessionSummary `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].Session != "sess-A" {
		t.Fatalf("list = %+v, want one session sess-A", list.Sessions)
	}
	if list.Sessions[0].Records != 2 {
		t.Fatalf("session records = %d, want 2", list.Sessions[0].Records)
	}

	// Load session — full set.
	resp2, err := http.Get(srv.URL + "/v1/conversations/sess-A")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var got struct {
		Session  string                   `json:"session"`
		Records  []conversationlog.Record `json:"records"`
		Total    int                      `json:"total"`
		Filtered int                      `json:"filtered"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Total != 2 || got.Filtered != 2 || len(got.Records) != 2 {
		t.Fatalf("load = %+v, want 2 records", got)
	}
	if got.Records[0].InputTokens != 10 || !got.Records[0].Estimated {
		t.Fatalf("usage not returned: %+v", got.Records[0])
	}

	// Filter by file.
	resp3, err := http.Get(srv.URL + "/v1/conversations/sess-A?file=b.go")
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	var gotF struct {
		Records  []conversationlog.Record `json:"records"`
		Total    int                      `json:"total"`
		Filtered int                      `json:"filtered"`
	}
	if err := json.NewDecoder(resp3.Body).Decode(&gotF); err != nil {
		t.Fatal(err)
	}
	if gotF.Total != 2 || gotF.Filtered != 1 || len(gotF.Records) != 1 {
		t.Fatalf("file filter = %+v, want 1 of 2", gotF)
	}
	if gotF.Records[0].File != "b.go" {
		t.Fatalf("file filter returned %q", gotF.Records[0].File)
	}
}

func TestConversations_UI_HTML(t *testing.T) {
	h := newConversationHandler(t, t.TempDir())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/conversations/ui", nil)
	req.Host = "localhost"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ui status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("ui content-type = %q", ct)
	}
	if body := rec.Body.String(); len(body) == 0 || body[:9] != "<!DOCTYPE" {
		t.Fatalf("ui body does not look like HTML: %.40q", body)
	}
}

// TestConversations_RouteGuard is the route-scoped DNS-rebind table test.
func TestConversations_RouteGuard(t *testing.T) {
	const token = "s3cret-token"
	cases := []struct {
		name      string
		host      string
		bearer    string
		allow     []string
		tokenFn   func() string
		wantAllow bool
	}{
		{name: "loopback ip", host: "127.0.0.1:7411", wantAllow: true},
		{name: "localhost name", host: "localhost:7411", wantAllow: true},
		{name: "ipv6 loopback", host: "[::1]:7411", wantAllow: true},
		{name: "rebind no token", host: "evil.example", tokenFn: func() string { return token }, wantAllow: false},
		{name: "rebind no token configured", host: "evil.example", wantAllow: false},
		{name: "allowlisted host", host: "dash.internal", allow: []string{"dash.internal"}, wantAllow: true},
		{name: "token authed non-loopback", host: "evil.example", bearer: token, tokenFn: func() string { return token }, wantAllow: true},
		{name: "wrong token non-loopback", host: "evil.example", bearer: "nope", tokenFn: func() string { return token }, wantAllow: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newConversationHandler(t, t.TempDir())
			h.SetConversationGuard(tc.allow, tc.tokenFn)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/conversations", nil)
			req.Host = tc.host
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			h.ServeHTTP(rec, req)

			if tc.wantAllow && rec.Code != http.StatusOK {
				t.Fatalf("want allow (200), got %d: %s", rec.Code, rec.Body.String())
			}
			if !tc.wantAllow {
				if rec.Code != http.StatusForbidden {
					t.Fatalf("want 403, got %d", rec.Code)
				}
				var e map[string]string
				_ = json.Unmarshal(rec.Body.Bytes(), &e)
				if e["error"] != "forbidden host" {
					t.Fatalf("403 body = %v, want forbidden host", e)
				}
			}
		})
	}
}

// TestConversations_GuardIsRouteScoped asserts the guard does NOT touch
// other routes: a token-authed (or even token-less) non-loopback request
// to a non-conversation route is not 403'd by this guard.
func TestConversations_GuardIsRouteScoped(t *testing.T) {
	const token = "s3cret-token"
	h := newConversationHandler(t, t.TempDir())
	h.SetConversationGuard(nil, func() string { return token })

	// A non-loopback request to /v1/health must NOT be blocked by the
	// conversation route guard (the guard is route-scoped).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Host = "evil.example"
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("non-conversation route blocked by conversation guard: %d", rec.Code)
	}

	// And even with no token at all, /v1/health is untouched by this guard.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req2.Host = "evil.example"
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("non-conversation route blocked without token: %d", rec2.Code)
	}
}

func TestConversations_BearerTokenAuth(t *testing.T) {
	// The token is accepted only via the Authorization header (the ?token=
	// query fallback was dropped because it leaks into logs and Referrer
	// headers), so a Bearer header should let a non-loopback conversation
	// request pass.
	const token = "s3cret-token"
	h := newConversationHandler(t, t.TempDir())
	h.SetConversationGuard(nil, func() string { return token })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/conversations", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Host = "evil.example"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer-token auth should pass on a conversation route, got %d", rec.Code)
	}
}
