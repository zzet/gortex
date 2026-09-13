package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A proxied tool call has to tell the remote which view it is about. Before
// this, ProxyToolCtx sent only Authorization / Content-Type / Accept, so the
// remote resolved a view from the body's `cwd` argument alone — a client that
// named its checkout with the `X-Gortex-Cwd` header (which is how both the
// /v1 handler and the Streamable transport learn it) gave the remote nothing
// to resolve from, and the same tools/call answered about the caller's
// checkout locally and about the remote's base corpus when proxied.

// TestProxyToolCtxForwardsTheCallersViewIdentity: the two headers the remote's
// own /v1/tools front door reads are exactly the two this must send.
func TestProxyToolCtxForwardsTheCallersViewIdentity(t *testing.T) {
	type seen struct{ cwd, session, auth string }
	got := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{
			cwd:     r.Header.Get("X-Gortex-Cwd"),
			session: r.Header.Get("Mcp-Session-Id"),
			auth:    r.Header.Get("Authorization"),
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cli, err := NewServerClient(ServerEntry{Slug: "r2", URL: srv.URL, AuthToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithProxyIdentity(context.Background(), ProxyIdentity{
		SessionID: "session-7",
		CWD:       "  /work/checkout  ",
	})
	if _, status, err := cli.ProxyToolCtx(ctx, "search_symbols", []byte(`{}`)); err != nil || status != 200 {
		t.Fatalf("ProxyToolCtx: status=%d err=%v", status, err)
	}
	s := <-got
	if s.cwd != "/work/checkout" {
		t.Fatalf("X-Gortex-Cwd = %q, want the trimmed caller cwd", s.cwd)
	}
	if s.session != "session-7" {
		t.Fatalf("Mcp-Session-Id = %q, want session-7", s.session)
	}
	if s.auth != "Bearer tok" {
		t.Fatalf("Authorization = %q; the identity headers must not displace auth", s.auth)
	}
}

// TestProxyToolCtxWithoutAnIdentitySendsNoViewHeaders: a caller that knows
// nothing about the requester's view must not invent one. An empty identity
// leaves the remote exactly where it was — resolving from the body.
func TestProxyToolCtxWithoutAnIdentitySendsNoViewHeaders(t *testing.T) {
	type seen struct {
		cwd, session       string
		hasCwd, hasSession bool
	}
	got := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasCwd := r.Header["X-Gortex-Cwd"]
		_, hasSession := r.Header["Mcp-Session-Id"]
		got <- seen{
			cwd: r.Header.Get("X-Gortex-Cwd"), session: r.Header.Get("Mcp-Session-Id"),
			hasCwd: hasCwd, hasSession: hasSession,
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cli, err := NewServerClient(ServerEntry{Slug: "r2", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cli.ProxyToolCtx(context.Background(), "search_symbols", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	s := <-got
	if s.hasCwd || s.hasSession {
		t.Fatalf("an identity-free proxy call sent view headers: cwd=%q session=%q", s.cwd, s.session)
	}
}

// TestProxyIdentityHalvesAreIndependent: a transport that knows the cwd but
// has no session (a stateless HTTP client), or the reverse, must forward the
// half it has rather than nothing.
func TestProxyIdentityHalvesAreIndependent(t *testing.T) {
	for _, tc := range []struct {
		name              string
		id                ProxyIdentity
		wantCwd, wantSess bool
	}{
		{"cwd only", ProxyIdentity{CWD: "/work"}, true, false},
		{"session only", ProxyIdentity{SessionID: "s1"}, false, true},
		{"blank is empty", ProxyIdentity{CWD: "   "}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type seen struct{ hasCwd, hasSession bool }
			got := make(chan seen, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, hasCwd := r.Header["X-Gortex-Cwd"]
				_, hasSession := r.Header["Mcp-Session-Id"]
				got <- seen{hasCwd: hasCwd, hasSession: hasSession}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			cli, err := NewServerClient(ServerEntry{Slug: "r2", URL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			ctx := WithProxyIdentity(context.Background(), tc.id)
			if _, _, err := cli.ProxyToolCtx(ctx, "search_symbols", []byte(`{}`)); err != nil {
				t.Fatal(err)
			}
			s := <-got
			if s.hasCwd != tc.wantCwd || s.hasSession != tc.wantSess {
				t.Fatalf("headers: cwd=%v session=%v, want cwd=%v session=%v",
					s.hasCwd, s.hasSession, tc.wantCwd, tc.wantSess)
			}
		})
	}
}

// TestProxyIdentityRoundTripsThroughContext pins the accessor contract the
// transports rely on: an empty identity never occupies the context, and a
// non-empty one comes back trimmed.
func TestProxyIdentityRoundTripsThroughContext(t *testing.T) {
	if got := ProxyIdentityFromContext(context.Background()); !got.Empty() {
		t.Fatalf("a bare context carried an identity: %+v", got)
	}
	if got := ProxyIdentityFromContext(nil); !got.Empty() { //nolint:staticcheck // nil ctx is the contract under test
		t.Fatalf("a nil context carried an identity: %+v", got)
	}
	ctx := WithProxyIdentity(context.Background(), ProxyIdentity{SessionID: " s1 ", CWD: " /w "})
	got := ProxyIdentityFromContext(ctx)
	if got.SessionID != "s1" || got.CWD != "/w" {
		t.Fatalf("identity = %+v, want trimmed halves", got)
	}
}

// TestABodyOnlyCwdStillReachesTheRemotesViewSeam is the second half of the
// header-less-client fix.
//
// Not every producer of a proxied call has a transport-level cwd to attach:
// the unix-socket dispatcher's tryProxyToolCall hands the router a body and no
// ProxyIdentity at all. The remote's ROUTING peek does read the body's cwd, so
// the call lands on the right daemon — but its view seam does not:
// requestViewCWD / requestToolContext (internal/server/handler.go) read the
// `X-Gortex-Cwd` header and the `?cwd=` query only, so a body-only cwd binds
// nothing and the answer comes from the remote's base corpus. Completing the
// identity from the body here is what makes the proxied answer describe the
// same view the local dispatch would have.
func TestABodyOnlyCwdStillReachesTheRemotesViewSeam(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"nested under arguments", `{"arguments":{"query":"Foo","cwd":" /work/nested "}}`, "/work/nested"},
		{"top level", `{"cwd":"/work/top","query":"Foo"}`, "/work/top"},
		{"arguments outrank top level", `{"arguments":{"cwd":"/work/nested"},"cwd":"/work/top"}`, "/work/nested"},
		{"explicit null arguments", `{"arguments":null,"cwd":"/work/top"}`, "/work/top"},
		{"no cwd anywhere", `{"arguments":{"query":"Foo"}}`, ""},
		{"not json", `not json at all`, ""},
		{"empty body", ``, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got <- r.Header.Get("X-Gortex-Cwd")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer srv.Close()

			cli, err := NewServerClient(ServerEntry{Slug: "r2", URL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := cli.ProxyToolCtx(context.Background(), "search_symbols", []byte(tc.body)); err != nil {
				t.Fatal(err)
			}
			if sent := <-got; sent != tc.want {
				t.Fatalf("X-Gortex-Cwd = %q, want %q", sent, tc.want)
			}
		})
	}
}

// TestTheContextIdentityIsNeverOverriddenByTheBody: completion is a fallback,
// not a re-resolution. The /v1 front door already folds the body's cwd into
// the ctx identity before it routes; a caller that deliberately scoped the hop
// must not be second-guessed by a stale `cwd` left in the arguments.
func TestTheContextIdentityIsNeverOverriddenByTheBody(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("X-Gortex-Cwd")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cli, err := NewServerClient(ServerEntry{Slug: "r2", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithProxyIdentity(context.Background(), ProxyIdentity{CWD: "/work/from-ctx"})
	if _, _, err := cli.ProxyToolCtx(ctx, "search_symbols", []byte(`{"arguments":{"cwd":"/work/from-body"}}`)); err != nil {
		t.Fatal(err)
	}
	if sent := <-got; sent != "/work/from-ctx" {
		t.Fatalf("X-Gortex-Cwd = %q, want the context identity to win", sent)
	}
}

// TestBodyCompletionAddsNoSessionIdentity: the body carries a workspace
// boundary, never a session id. Completing the cwd must not manufacture an
// `Mcp-Session-Id` the caller never had — that header drives the remote's
// tool-policy gate.
func TestBodyCompletionAddsNoSessionIdentity(t *testing.T) {
	got := make(chan bool, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, has := r.Header["Mcp-Session-Id"]
		got <- has
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cli, err := NewServerClient(ServerEntry{Slug: "r2", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cli.ProxyToolCtx(context.Background(), "search_symbols",
		[]byte(`{"arguments":{"cwd":"/work/nested"}}`)); err != nil {
		t.Fatal(err)
	}
	if <-got {
		t.Fatal("body completion invented a session id")
	}
}
