package main

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/daemon"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/mcp/streamable"
	"github.com/zzet/gortex/internal/server"
	"go.uber.org/zap"
)

// composeDaemonHTTPHandler combines the daemon's two HTTP surfaces onto a
// single listener: the MCP Streamable transport (streamH, serving /mcp
// and /healthz with its own auth) under the catch-all, and the /v1 REST
// API (v1, the former `gortex server`) under /v1/ behind a bearer-auth
// wrapper. CORS, when an origin is set, wraps the whole mux so browser
// clients (the web UI) can reach either surface cross-origin.
// The /v1 handler is built by the daemon from the same MCP server the
// streamable surface dispatches into, but it is handed only the mcp-go server
// and the store — not the routed-view machinery. Its non-tool endpoints
// (/v1/graph, /v1/subgraph, /v1/stats, /v1/health, /v1/repos,
// /v1/workspaces/{ws}/repos) read the store directly, so without a lease
// manager they scan an unleased whole store and can splice two generations
// into one response. The streamable surface carries the lease manager across
// (daemonHTTPSurface.baseLeases, lifted off the dispatcher's MCP server) so
// the composition step can wire it in without either side reaching for a
// global.
func composeDaemonHTTPHandler(streamH, v1 http.Handler, tokenFn func() string, corsOrigin string) http.Handler {
	if h, ok := v1.(*server.Handler); ok && h != nil {
		if s, ok := streamH.(*daemonHTTPSurface); ok && s != nil && s.baseLeases != nil {
			h.SetBaseCorpusLeases(s.baseLeases)
		}
	}
	top := http.NewServeMux()
	top.Handle("/", streamH)
	top.Handle("/v1/", server.WithAuthFunc(v1, tokenFn))
	if corsOrigin != "" {
		return server.WithCORS(top, server.CORSOptions{AllowOrigins: []string{corsOrigin}})
	}
	return top
}

// daemonStreamableDispatcher bridges streamable.Transport (which has
// its own session model keyed by `Mcp-Session-Id`) to the daemon's
// existing MCPDispatcher (which wants a *daemon.Session). For every
// streamable session ID it lazily registers a detached daemon
// session, so existing daemon-level features — status visibility,
// per-session MCP state on the in-process server, router proxying —
// see HTTP-arriving traffic exactly the same way they see unix-socket
// traffic.
//
// The bridge is owned by cmd/gortex (not the daemon package) so it
// can reach across to the streamable package without introducing a
// daemon→streamable import edge. Symmetric with the existing
// mcpDispatcher pattern.
type daemonStreamableDispatcher struct {
	inner    daemon.MCPDispatcher
	registry *daemon.SessionRegistry
	logger   *zap.Logger

	mu      sync.Mutex
	bridged map[string]*daemon.Session // streamableSessionID → synthetic daemon.Session
}

func newDaemonStreamableDispatcher(inner daemon.MCPDispatcher, reg *daemon.SessionRegistry, logger *zap.Logger) *daemonStreamableDispatcher {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &daemonStreamableDispatcher{
		inner:    inner,
		registry: reg,
		logger:   logger,
		bridged:  make(map[string]*daemon.Session),
	}
}

// Dispatch implements streamable.Dispatcher. It pulls the streamable
// session ID out of the context (set by the transport's
// localDispatch), pairs it with a synthetic daemon.Session, and hands
// the frame down to the wrapped dispatcher. The synthetic session is
// created on first use; subsequent calls in the same streamable
// session reuse it so per-session in-process state (savings counters,
// recent activity, frecency feedback) accumulates across requests.
func (d *daemonStreamableDispatcher) Dispatch(ctx context.Context, frame []byte) ([]byte, error) {
	if d.inner == nil {
		return nil, nil
	}
	sid := gortexmcp.SessionIDFromContext(ctx)
	cwd := gortexmcp.SessionCWDFromContext(ctx)
	sess := d.acquire(sid, cwd)
	if sess == nil {
		// Stateless mode (no Mcp-Session-Id header). Run the
		// frame through a one-shot session so the dispatcher sees
		// a valid pointer; tear it down after the call so memory
		// doesn't grow unbounded.
		sess = d.registry.RegisterDetached("", daemon.Handshake{
			Mode: daemon.ModeMCP,
			CWD:  cwd,
		})
		defer d.unbridge(sess.ID)
		defer d.registry.RemoveByID(sess.ID)
	}
	return d.inner.Dispatch(ctx, sess, frame)
}

// acquire returns the synthetic daemon.Session for a streamable
// session ID, creating it on demand. Returns nil when sid == "" so
// the caller can fall back to one-shot semantics.
func (d *daemonStreamableDispatcher) acquire(sid, cwd string) *daemon.Session {
	if sid == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if sess, ok := d.bridged[sid]; ok {
		// Refresh the cwd if the client moved between requests.
		if cwd != "" && sess.CWD != cwd {
			sess.CWD = cwd
		}
		return sess
	}
	// Use the streamable session id verbatim — that way the daemon
	// status command's MCP-sessions block shows the same id the
	// client saw on its first response header.
	sess := d.registry.RegisterDetached(sid, daemon.Handshake{
		Mode:       daemon.ModeMCP,
		CWD:        cwd,
		ClientName: "http",
	})
	d.bridged[sid] = sess
	return sess
}

func (d *daemonStreamableDispatcher) unbridge(sid string) {
	if sid == "" {
		return
	}
	d.mu.Lock()
	delete(d.bridged, sid)
	d.mu.Unlock()
}

// onSessionEnded mirrors the unix-socket transport's defer cleanup:
// when a streamable session is dropped (DELETE /mcp or store
// eviction) the daemon-side bookkeeping must let go too. Wired into
// the streamable transport via the on-delete cleanup path defined
// below.
func (d *daemonStreamableDispatcher) onSessionEnded(sid string) {
	sess := d.registry.RemoveByID(sid)
	d.unbridge(sid)
	if sess != nil {
		if hook, ok := d.inner.(daemon.SessionEndedHook); ok {
			hook.SessionEnded(sess)
		}
	}
}

// observeStoreCleanup wraps a streamable.SessionStore so the daemon
// gets a callback every time a session is dropped (explicit DELETE or
// TTL eviction). The wrapper preserves the interface contract while
// letting the daemon-side dispatcher purge its bridged map.
type observingStore struct {
	inner streamable.SessionStore
	on    func(string)
}

func wrapStreamableStoreWithCleanup(inner streamable.SessionStore, on func(string)) streamable.SessionStore {
	if inner == nil || on == nil {
		return inner
	}
	return &observingStore{inner: inner, on: on}
}

func (s *observingStore) Create(state streamable.SessionState) (string, error) {
	return s.inner.Create(state)
}

func (s *observingStore) Get(id string) (streamable.SessionState, bool) {
	return s.inner.Get(id)
}

func (s *observingStore) Update(state streamable.SessionState) error {
	return s.inner.Update(state)
}

func (s *observingStore) Delete(id string) {
	s.inner.Delete(id)
	if id != "" {
		s.on(id)
	}
}

func (s *observingStore) Len() int { return s.inner.Len() }

// buildDaemonStreamableHandler stitches the transport, store, and
// session bridge into a single http.Handler the daemon can mount on
// /mcp. Pulled out so cmd/gortex/daemon.go stays terse: one call
// returns the handler ready to assign to daemon.Server.HTTPHandler.
func buildDaemonStreamableHandler(disp daemon.MCPDispatcher, reg *daemon.SessionRegistry, router *daemon.Router, logger *zap.Logger, tokenFn func() string) *daemonHTTPSurface {
	bridge := newDaemonStreamableDispatcher(disp, reg, logger)
	// The MCP session TTL is its own policy knob (default 30m,
	// GORTEX_MCP_SESSION_IDLE_TTL to tune) — it used to borrow the
	// overlay subsystem's constant, which dodged even that knob's env
	// override. The resolved value rides Config.SessionTTL so the
	// transport can advertise it to clients.
	sessionTTL := streamable.SessionIdleTTLFromEnv(0)
	store := streamable.NewMemoryStore(sessionTTL)
	wrapped := wrapStreamableStoreWithCleanup(store, bridge.onSessionEnded)
	transport := streamable.New(streamable.Config{
		Dispatcher: bridge,
		Store:      wrapped,
		Logger:     logger,
		Router:     router,
		SessionTTL: sessionTTL,
		InitializeHook: func(_ context.Context, state *streamable.SessionState) {
			// Bridge the new streamable session into the daemon's
			// session registry up-front so the daemon-status block
			// shows it before the first tool call lands.
			bridge.acquire(state.ID, state.CWD)
		},
	})
	mux := http.NewServeMux()
	mux.Handle("POST /mcp", transport)
	mux.Handle("GET /mcp", transport)
	mux.Handle("DELETE /mcp", transport)
	mux.Handle("OPTIONS /mcp", transport)
	// Health probe so operators (and `gortex daemon http-status`
	// scripts) can verify the listener is up without dispatching an
	// MCP frame.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","transport":"streamable-http","spec":"mcp-2026-03-26"}`))
	})
	// Always wrap: the middleware resolves the token per request, so a
	// request with no configured token is served unauthenticated and the
	// token can be rotated (added/changed/removed) without a restart.
	// The origin guard sits outside it, because the request it refuses is
	// one a browser makes with the user's own credentials — an unauthenticated
	// loopback bind is reachable from any page the user visits.
	//
	// proxyIdentityMiddleware sits INSIDE both, on the request path only:
	// it costs nothing for a refused request and everything downstream —
	// including the transport's own tryRouteToolCall, which builds its ctx
	// from r.Context() — then sees the caller's view identity.
	handler := browserOriginGuard(
		bearerAuthMiddleware(proxyIdentityMiddleware(mux), tokenFn),
		daemonHTTPAllowedOrigins)
	return &daemonHTTPSurface{
		Handler:    handler,
		baseLeases: baseCorpusLeasesFor(disp),
	}
}

// daemonHTTPSurface is the daemon's /mcp handler plus the one collaborator the
// /v1 surface cannot reach on its own: the lease manager materialized views
// and the retirement sweep share. Carrying it here keeps the wiring explicit —
// composeDaemonHTTPHandler hands it to the REST handler — instead of making
// internal/server reach for a process-global.
type daemonHTTPSurface struct {
	http.Handler
	baseLeases server.BaseCorpusLeases
}

// baseCorpusLeasesFor lifts the lease manager off the dispatcher's in-process
// MCP server. It is deliberately THAT manager and no other: retirement runs in
// the checkout coordinators with it as the in-use predicate (see
// internal/serverstack/shared_server.go, "the lease manager must be the
// lifecycle's own"), so a reader pinning through a freshly-made manager would
// be invisible to the sweep and its generation could be deleted mid-read.
// Returns nil when the backend carries no view catalog — the /v1 reads then
// report pinned:false rather than claiming a hold they do not have.
func baseCorpusLeasesFor(disp daemon.MCPDispatcher) server.BaseCorpusLeases {
	d, ok := disp.(*mcpDispatcher)
	if !ok || d == nil || d.srv == nil {
		return nil
	}
	mat := d.srv.Materializer()
	if mat == nil || mat.Leases == nil {
		return nil
	}
	return mat.Leases
}

// proxyIdentityMiddleware attaches the caller's view identity to the request
// context so a tools/call the router proxies to a remote carries the same
// `X-Gortex-Cwd` / `Mcp-Session-Id` the caller sent here.
//
// The streamable transport decides local-vs-remote inside tryRouteToolCall,
// which builds its context from r.Context(); a remote hop from there reaches
// daemon.ServerClient.ProxyToolCtx, which forwards whatever identity the
// context carries. Attaching it at the mount means the /mcp surface and the
// /v1 surface agree on what a proxied call tells the remote about the caller's
// view — before this, a proxied call arrived with neither header and the
// remote answered from its own base corpus.
func proxyIdentityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := daemon.ProxyIdentity{
			SessionID: r.Header.Get("Mcp-Session-Id"),
			CWD:       strings.TrimSpace(r.Header.Get("X-Gortex-Cwd")),
		}
		if !id.Empty() {
			r = r.WithContext(daemon.WithProxyIdentity(r.Context(), id))
		}
		next.ServeHTTP(w, r)
	})
}

// browserOriginGuard refuses a request carrying a cross-origin Origin header.
//
// A loopback HTTP bind is not private: every page in the user's browser can
// reach 127.0.0.1, and the transport's own Origin allowlist
// (streamable.Config.AllowedOrigins) was never wired to anything, so it
// admitted every origin. With the surface's default of no auth token on a
// localhost bind, that made the full MCP tool catalogue — file reads and
// writes included — reachable from any site the user happened to open.
//
// A request with NO Origin header is allowed: browsers always set it
// cross-origin, and ordinary MCP clients (which are not browsers) never send
// it. So the guard costs nothing for real clients and closes the browser path.
// Operators serving a genuine web front end name its origin explicitly.
func browserOriginGuard(next http.Handler, allowed []string) http.Handler {
	allowSet := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		if o = strings.ToLower(strings.TrimSpace(o)); o != "" {
			allowSet[o] = struct{}{}
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.ToLower(strings.TrimSpace(r.Header.Get("Origin")))
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := allowSet[origin]; ok {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "forbidden: cross-origin requests are not accepted; "+
			"pass --http-allowed-origin to permit a specific web origin", http.StatusForbidden)
	})
}

// bearerAuthMiddleware gates every request behind a bearer token resolved
// per request via tokenFn (so the token can rotate without a restart).
// When tokenFn returns "", the request is served unauthenticated.
// /healthz is exempt so liveness probes don't need a token.
func bearerAuthMiddleware(next http.Handler, tokenFn func() string) http.Handler {
	if tokenFn == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		token := tokenFn()
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gortex"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Compile-time check: the dispatcher satisfies streamable.Dispatcher.
var _ streamable.Dispatcher = (*daemonStreamableDispatcher)(nil)

// Compile-time check: the observing store satisfies SessionStore.
var _ streamable.SessionStore = (*observingStore)(nil)
