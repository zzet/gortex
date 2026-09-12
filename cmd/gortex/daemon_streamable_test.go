package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if !got.Empty() {
		t.Fatalf("an anonymous request gained an identity: %+v", got)
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
