package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/mcp/streamable"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/server"
)

// capturingDispatcher records the context every dispatched frame arrives with,
// so a test can assert what the HTTP mount put on it.
type capturingDispatcher struct {
	identities chan daemon.ProxyIdentity
}

func (d *capturingDispatcher) Dispatch(ctx context.Context, _ *daemon.Session, frame []byte) ([]byte, error) {
	select {
	case d.identities <- daemon.ProxyIdentityFromContext(ctx):
	default:
	}
	var env struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(frame, &env)
	if len(env.ID) == 0 {
		return nil, nil
	}
	return []byte(`{"jsonrpc":"2.0","id":` + string(env.ID) + `,"result":{}}`), nil
}

// TestStreamableMountAttachesTheCallersProxyIdentity is the production trace
// for the /mcp half of the proxy fix.
//
// The transport decides local-vs-remote inside tryRouteToolCall, and builds
// the context it hands to that decision from r.Context(). A remote hop from
// there reaches daemon.ServerClient.ProxyToolCtx, which forwards whatever
// identity the context carries — so the identity has to be on the request
// context before the transport ever looks at the frame. This asserts the
// mount puts it there.
func TestStreamableMountAttachesTheCallersProxyIdentity(t *testing.T) {
	disp := &capturingDispatcher{identities: make(chan daemon.ProxyIdentity, 4)}
	reg := daemon.NewSessionRegistry()
	h := buildDaemonStreamableHandler(disp, reg, nil, zap.NewNop(), nil)

	frame, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "search_symbols", "arguments": map[string]any{"query": "Foo"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(frame))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gortex-Cwd", "  /work/checkout  ")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	select {
	case id := <-disp.identities:
		if id.CWD != "/work/checkout" {
			t.Fatalf("dispatch context carried cwd %q; a proxied hop would tell the remote nothing", id.CWD)
		}
	default:
		t.Fatal("the frame never reached the dispatcher")
	}
}

// TestStreamableMountForwardsTheSessionIdentityToo: the second half of the
// identity, carried on the session header an MCP client sends on every
// post-initialize request.
func TestStreamableMountForwardsTheSessionIdentityToo(t *testing.T) {
	disp := &capturingDispatcher{identities: make(chan daemon.ProxyIdentity, 4)}
	h := buildDaemonStreamableHandler(disp, daemon.NewSessionRegistry(), nil, zap.NewNop(), nil)

	// Mint a real session first: the transport refuses an unknown session id.
	initFrame, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2026-03-26",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "0.0.0"},
		},
	})
	initReq := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(initFrame))
	initReq.Header.Set("Content-Type", "application/json")
	initRec := httptest.NewRecorder()
	h.ServeHTTP(initRec, initReq)
	sid := initRec.Header().Get(streamable.HeaderSessionID)
	if sid == "" {
		t.Fatalf("initialize minted no session: %s", initRec.Body.String())
	}
	// Drain the initialize frame's identity.
	select {
	case <-disp.identities:
	default:
	}

	frame, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "search_symbols", "arguments": map[string]any{}},
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(frame))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(streamable.HeaderSessionID, sid)
	req.Header.Set("X-Gortex-Cwd", "/work/checkout")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	select {
	case id := <-disp.identities:
		if id.SessionID != sid || id.CWD != "/work/checkout" {
			t.Fatalf("identity = %+v, want session %q and cwd /work/checkout", id, sid)
		}
	default:
		t.Fatal("the tools/call frame never reached the dispatcher")
	}
}

// TestProxyIdentityMiddlewareLeavesAnAnonymousRequestAlone: a request that
// names neither a session nor a cwd must not gain an invented identity — the
// remote then keeps resolving from the body, exactly as before.
func TestProxyIdentityMiddlewareLeavesAnAnonymousRequestAlone(t *testing.T) {
	var got daemon.ProxyIdentity
	h := proxyIdentityMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = daemon.ProxyIdentityFromContext(r.Context())
	}), nil)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if !got.Empty() {
		t.Fatalf("an anonymous request gained an identity: %+v", got)
	}
}

// --- header-less client shapes ----------------------------------------------

// initializeStreamableSession mints a real transport session carrying cwd in
// `initialize._meta.cwd` — the canonical Streamable-HTTP client shape, which
// declares its working directory exactly once and never sends a cwd header
// again. Returns the minted session id.
func initializeStreamableSession(t *testing.T, h http.Handler, cwd string) string {
	t.Helper()
	params := map[string]any{
		"protocolVersion": "2026-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0.0.0"},
	}
	if cwd != "" {
		params["_meta"] = map[string]any{"cwd": cwd}
	}
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": params,
	})
	if err != nil {
		t.Fatalf("marshal initialize: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(frame))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	sid := rec.Header().Get(streamable.HeaderSessionID)
	if sid == "" {
		t.Fatalf("initialize minted no session: %s", rec.Body.String())
	}
	return sid
}

// toolsCallFrame builds a tools/call frame, optionally carrying an explicit
// `cwd` argument.
func toolsCallFrame(t *testing.T, id int, args map[string]any) []byte {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": "search_symbols", "arguments": args},
	})
	if err != nil {
		t.Fatalf("marshal tools/call: %v", err)
	}
	return frame
}

// TestHeaderlessStreamableClientStillProxiesItsView is the regression this item
// exists for.
//
// The canonical Streamable-HTTP client sends its cwd once, in
// `initialize._meta.cwd`; the transport persists it as SessionState.CWD and
// every later tools/call binds the LOCAL route from that state
// (internal/mcp/streamable/transport.go, tryRouteToolCall). No such client ever
// sets `X-Gortex-Cwd`. A mount that derives the proxy identity from the header
// alone therefore proxies that client's calls with no view identity at all, and
// the remote answers from its own base corpus while an identical local call
// answers about the session's checkout.
func TestHeaderlessStreamableClientStillProxiesItsView(t *testing.T) {
	disp := &capturingDispatcher{identities: make(chan daemon.ProxyIdentity, 4)}
	h := buildDaemonStreamableHandler(disp, daemon.NewSessionRegistry(), nil, zap.NewNop(), nil)

	sid := initializeStreamableSession(t, h, "/work/session-checkout")
	select { // drain the initialize frame
	case <-disp.identities:
	default:
	}

	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(toolsCallFrame(t, 2, nil)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(streamable.HeaderSessionID, sid)
	// Deliberately NO X-Gortex-Cwd: that is the whole point of the shape.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	select {
	case id := <-disp.identities:
		if id.CWD != "/work/session-checkout" {
			t.Fatalf("identity carried cwd %q; a header-less client's proxied call "+
				"would tell the remote nothing about its view", id.CWD)
		}
		if id.SessionID != sid {
			t.Fatalf("identity carried session %q, want %q", id.SessionID, sid)
		}
	default:
		t.Fatal("the tools/call frame never reached the dispatcher")
	}
}

// TestTheToolsCallCwdArgumentBecomesTheProxiedIdentity: the third source the
// local bind consults, and the one it prefers over both others. A client that
// names a cwd per call (no session cwd, no header) must proxy THAT cwd.
func TestTheToolsCallCwdArgumentBecomesTheProxiedIdentity(t *testing.T) {
	disp := &capturingDispatcher{identities: make(chan daemon.ProxyIdentity, 4)}
	h := buildDaemonStreamableHandler(disp, daemon.NewSessionRegistry(), nil, zap.NewNop(), nil)

	frame := toolsCallFrame(t, 1, map[string]any{"query": "Foo", "cwd": " /work/arg-checkout "})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(frame))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	select {
	case id := <-disp.identities:
		if id.CWD != "/work/arg-checkout" {
			t.Fatalf("identity carried cwd %q, want the call's own cwd argument", id.CWD)
		}
	default:
		t.Fatal("the tools/call frame never reached the dispatcher")
	}
}

// TestTheCwdArgumentOutranksTheSessionAndHeaderOnTheProxiedIdentity pins the
// ORDER, not just the sources. tryRouteToolCall resolves argument > session >
// header; a middleware that resolved them in any other order would forward a
// different view than the one the local route would have bound for the very
// same request.
func TestTheCwdArgumentOutranksTheSessionAndHeaderOnTheProxiedIdentity(t *testing.T) {
	disp := &capturingDispatcher{identities: make(chan daemon.ProxyIdentity, 4)}
	h := buildDaemonStreamableHandler(disp, daemon.NewSessionRegistry(), nil, zap.NewNop(), nil)

	sid := initializeStreamableSession(t, h, "/work/session-checkout")
	select {
	case <-disp.identities:
	default:
	}

	frame := toolsCallFrame(t, 2, map[string]any{"cwd": "/work/arg-checkout"})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(frame))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(streamable.HeaderSessionID, sid)
	req.Header.Set("X-Gortex-Cwd", "/work/header-checkout")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	select {
	case id := <-disp.identities:
		if id.CWD != "/work/arg-checkout" {
			t.Fatalf("identity carried cwd %q, want the argument to outrank "+
				"the session cwd and the header", id.CWD)
		}
	default:
		t.Fatal("the tools/call frame never reached the dispatcher")
	}
}

// TestTheSessionCwdOutranksTheHeaderOnTheProxiedIdentity: second rung of the
// same ladder. The transport prefers state.CWD over the header, so the proxied
// identity must too.
func TestTheSessionCwdOutranksTheHeaderOnTheProxiedIdentity(t *testing.T) {
	disp := &capturingDispatcher{identities: make(chan daemon.ProxyIdentity, 4)}
	h := buildDaemonStreamableHandler(disp, daemon.NewSessionRegistry(), nil, zap.NewNop(), nil)

	sid := initializeStreamableSession(t, h, "/work/session-checkout")
	select {
	case <-disp.identities:
	default:
	}

	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(toolsCallFrame(t, 2, nil)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(streamable.HeaderSessionID, sid)
	req.Header.Set("X-Gortex-Cwd", "/work/header-checkout")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	select {
	case id := <-disp.identities:
		if id.CWD != "/work/session-checkout" {
			t.Fatalf("identity carried cwd %q, want the session cwd to outrank the header", id.CWD)
		}
	default:
		t.Fatal("the tools/call frame never reached the dispatcher")
	}
}

// TestTheIdentityPeekLeavesTheFrameIntact: the middleware now reads the request
// body before the transport does. If it failed to hand those bytes back, every
// POST /mcp would break — so assert the frame still arrives whole, arguments
// included, and that a body too large to peek still flows through untouched.
func TestTheIdentityPeekLeavesTheFrameIntact(t *testing.T) {
	var seen []byte
	mw := proxyIdentityMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen, _ = io.ReadAll(r.Body)
	}), nil)

	t.Run("small frame", func(t *testing.T) {
		frame := toolsCallFrame(t, 1, map[string]any{"query": "Foo", "cwd": "/work/x"})
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(frame))
		mw.ServeHTTP(httptest.NewRecorder(), req)
		if !bytes.Equal(seen, frame) {
			t.Fatalf("frame reached the transport as %q, want %q", seen, frame)
		}
	})

	t.Run("body past the peek bound", func(t *testing.T) {
		big := bytes.Repeat([]byte("x"), maxProxyIdentityPeek+4096)
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(big))
		mw.ServeHTTP(httptest.NewRecorder(), req)
		if !bytes.Equal(seen, big) {
			t.Fatalf("oversized body reached the transport truncated: %d of %d bytes",
				len(seen), len(big))
		}
	})
}

// TestANonToolFrameNamesNoCwd: only tools/call is routed, so only tools/call is
// peeked. A resources/read frame that happens to carry a `cwd` argument must
// not manufacture a proxy identity out of it.
func TestANonToolFrameNamesNoCwd(t *testing.T) {
	var got daemon.ProxyIdentity
	mw := proxyIdentityMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = daemon.ProxyIdentityFromContext(r.Context())
	}), nil)
	frame, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "resources/read",
		"params": map[string]any{"arguments": map[string]any{"cwd": "/work/x"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(frame))
	mw.ServeHTTP(httptest.NewRecorder(), req)
	if !got.Empty() {
		t.Fatalf("a non-tools/call frame produced an identity: %+v", got)
	}
}

// TestSessionCWDLookupIsTheTransportsOwnStore: the middleware's session source
// has to be the store the transport itself reads, or the two would answer the
// same request with different checkouts.
func TestSessionCWDLookupIsTheTransportsOwnStore(t *testing.T) {
	store := streamable.NewMemoryStore(0)
	lookup := sessionCWDLookup(store)
	if lookup == nil {
		t.Fatal("a real store produced no lookup")
	}
	id, err := store.Create(streamable.SessionState{CWD: "/work/from-store"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if got := lookup(id); got != "/work/from-store" {
		t.Fatalf("lookup(%q) = %q, want /work/from-store", id, got)
	}
	if got := lookup("no-such-session"); got != "" {
		t.Fatalf("lookup of an unknown session returned %q", got)
	}
	if sessionCWDLookup(nil) != nil {
		t.Fatal("a nil store produced a non-nil lookup")
	}
}

// TestComposeWiresTheLeaseManagerIntoTheRESTSurface is the production trace
// for the /v1 half: the daemon builds the REST handler from the mcp-go server
// and the store alone, so without this wiring its direct-store endpoints scan
// an unleased store. The composition step hands it the manager the streamable
// surface lifted off the same in-process MCP server.
func TestComposeWiresTheLeaseManagerIntoTheRESTSurface(t *testing.T) {
	leases := graphview.NewLeaseManager()
	streamH := &daemonHTTPSurface{
		Handler:    http.NewServeMux(),
		baseLeases: leases,
	}
	v1 := server.NewHandler(
		mcpserver.NewMCPServer("gortex-test", "0.0.0", mcpserver.WithToolCapabilities(false)),
		graph.New(), "0.0.0", zap.NewNop())

	composed := composeDaemonHTTPHandler(streamH, v1, func() string { return "" }, "")

	req := httptest.NewRequest(http.MethodGet, "/v1/stats", nil)
	req.Header.Set("X-Gortex-Cwd", "/work/checkout")
	rec := httptest.NewRecorder()
	composed.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		View *server.BaseScopedRider `json:"view"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if body.View == nil {
		t.Fatalf("no rider on a cwd-bound /v1 read: %s", rec.Body.String())
	}
	if !body.View.Pinned {
		t.Fatal("the composed /v1 surface read the store with no base-corpus pin")
	}
	if body.View.Exact {
		t.Fatal("the /v1 surface presented the base corpus as an exact answer to a checkout request")
	}
}

// TestComposeWithoutALeaseManagerStillServes: an unwired compose (a test
// double, or a backend with no view catalog) must keep serving and must not
// claim a pin.
func TestComposeWithoutALeaseManagerStillServes(t *testing.T) {
	v1 := server.NewHandler(
		mcpserver.NewMCPServer("gortex-test", "0.0.0", mcpserver.WithToolCapabilities(false)),
		graph.New(), "0.0.0", zap.NewNop())
	composed := composeDaemonHTTPHandler(http.NewServeMux(), v1, func() string { return "" }, "")

	req := httptest.NewRequest(http.MethodGet, "/v1/stats", nil)
	req.Header.Set("X-Gortex-Cwd", "/work/checkout")
	rec := httptest.NewRecorder()
	composed.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		View *server.BaseScopedRider `json:"view"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.View == nil || body.View.Pinned {
		t.Fatalf("an unwired surface misreported its pin: %+v", body.View)
	}
}

// TestBaseCorpusLeasesForTakesTheServersOwnManager: the manager handed to the
// REST surface must be the lifecycle's own — retirement runs with THAT manager
// as its in-use predicate, so a reader pinning through any other one is
// invisible to the sweep and its generation can be deleted mid-read.
func TestBaseCorpusLeasesForTakesTheServersOwnManager(t *testing.T) {
	g := graph.New()
	reg := parser.NewRegistry()
	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	srv := gortexmcp.NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)

	if got := baseCorpusLeasesFor(newMCPDispatcher(srv, nil, zap.NewNop())); got != nil {
		t.Fatalf("a server with no materializer produced a lease manager: %#v", got)
	}
	if got := baseCorpusLeasesFor(nil); got != nil {
		t.Fatalf("a nil dispatcher produced a lease manager: %#v", got)
	}
	if got := baseCorpusLeasesFor(&capturingDispatcher{}); got != nil {
		t.Fatalf("a foreign dispatcher produced a lease manager: %#v", got)
	}

	leases := graphview.NewLeaseManager()
	srv.SetMaterializer(&graphview.Materializer{Leases: leases})
	got := baseCorpusLeasesFor(newMCPDispatcher(srv, nil, zap.NewNop()))
	if got == nil {
		t.Fatal("the server's own lease manager was not lifted onto the HTTP surface")
	}
	if got != server.BaseCorpusLeases(leases) {
		t.Fatalf("a DIFFERENT lease manager reached the HTTP surface: %#v", got)
	}
}

// TestGuideDoesNotClaimGortexServerMountsTheStreamableEndpoint: the shipped
// guide told agents `gortex server` always mounts /mcp. It does not — that
// command manages the servers.toml roster (daemon_servers.go), and
// server.Handler.SetStreamableTransport, which would mount /mcp on the REST
// handler, has no production caller. The only production mount is the one
// buildDaemonStreamableHandler builds for `gortex daemon --http-addr`.
func TestGuideDoesNotClaimGortexServerMountsTheStreamableEndpoint(t *testing.T) {
	text := gortexmcp.GuideText("capabilities")
	if text == "" {
		t.Fatal("the capabilities guide is empty")
	}
	if strings.Contains(text, "`gortex server` always mounts") {
		t.Error("the guide still claims `gortex server` mounts the streamable endpoint")
	}
	if !strings.Contains(text, "gortex daemon --http-addr") {
		t.Error("the guide no longer names the one command that mounts /mcp")
	}
}
