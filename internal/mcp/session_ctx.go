package mcp

import (
	"context"
	"sync"

	"github.com/zzet/gortex/internal/savings"
)

// sessionCtxKey is the private context key under which a caller
// (typically the daemon's MCP dispatcher) stashes the session ID for
// the current request. The value is read by `Server.sessionFor` so
// tool handlers resolve to the correct per-client state.
//
// Unexported so external packages can't inject one accidentally — use
// WithSessionID / SessionIDFromContext.
type sessionCtxKey struct{}

// WithSessionID returns a context carrying id. The daemon's MCP
// dispatcher wraps each inbound frame's context with this before
// calling MCPServer.HandleMessage, giving every tool handler access
// to the per-session state without touching the handler signature.
//
// This is the ONE universal session identity: sessionFor,
// effectiveSessionPolicy (and therefore every tool-policy gate),
// tokenStatsFor, the agent registry, diagnostics/health/readiness/
// stale-refs subscriptions, localization state, query logging, and
// notes/memory scoping all key off SessionIDFromContext. It MUST be
// the caller's real transport/MCP session id (Mcp-Session-Id, or the
// stdio server's implicit single session) — never overridden by an
// unrelated selector. See WithOverlayCohortID for the one narrow
// exception (overlay snapshot binding) that is allowed to diverge.
//
// An empty id is treated as "no session" and returns ctx unchanged —
// that's the path the embedded stdio server takes, where there's only
// one implicit session.
func WithSessionID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionCtxKey{}, id)
}

// SessionIDFromContext returns the session ID attached via
// WithSessionID, or "" when none is present. Callers treat "" as
// "default shared session" — the same state the embedded server uses.
func SessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(sessionCtxKey{}).(string); ok {
		return id
	}
	return ""
}

// overlayCohortCtxKey carries an explicit override for which overlay
// cohort a request's overlay-scoped calls should bind to, when that
// differs from the caller's real session id (see WithOverlayCohortID).
// Unexported: use WithOverlayCohortID / OverlayCohortIDFromContext.
type overlayCohortCtxKey struct{}

// WithOverlayCohortID returns a context carrying an overlay-cohort
// override distinct from the request's real session id
// (SessionIDFromContext). Only the overlay subsystem's own accessors
// (overlaySessionID, snapshotOverlayRequestForCtx,
// prepareOverlayRequest, buildOverlayViewForCtx, and the simulate /
// explore-literal-overlay call sites) consult this — every other
// SessionIDFromContext caller (policy, token stats, notes, agent
// registry, diagnostics subscriptions, ...) is intentionally
// unaffected by it.
//
// This exists for callers that legitimately want to scope overlay
// state to a cohort id that differs from their own transport session
// (e.g. a CI harness that orchestrates several overlay scopes from
// one connection) WITHOUT that selection also silently redirecting
// every other per-session subsystem to the wrong identity — which is
// exactly what happened when a single header-precedence value fed
// both purposes: a session's own tool-policy gate could be bypassed
// by pairing a restricted Mcp-Session-Id with a permissive
// X-Gortex-Overlay-Session.
//
// An empty id returns ctx unchanged; OverlayCohortIDFromContext then
// falls back to SessionIDFromContext, so a caller that never sets
// this behaves byte-identically to before this type existed.
func WithOverlayCohortID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, overlayCohortCtxKey{}, id)
}

// OverlayCohortIDFromContext returns the overlay-cohort override
// attached via WithOverlayCohortID, or SessionIDFromContext(ctx) when
// none was set — the common case, where overlay state binds to the
// caller's own session exactly as it always has.
func OverlayCohortIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(overlayCohortCtxKey{}).(string); ok && id != "" {
		return id
	}
	return SessionIDFromContext(ctx)
}

// sessionCWDCtxKey carries the session's working directory. The
// daemon's MCP dispatcher stashes it alongside the session ID so tool
// handlers can resolve — and enforce — the workspace boundary for the
// session (Server.sessionScope). Unexported: external packages must
// use WithSessionCWD / SessionCWDFromContext.
type sessionCWDCtxKey struct{}

// WithSessionCWD returns a context carrying the session's working
// directory. The daemon dispatcher wraps each inbound frame with this
// before calling MCPServer.HandleMessage, giving every tool handler
// the cwd needed to resolve the session's workspace scope.
//
// An empty cwd returns ctx unchanged — that's the embedded stdio path
// (one implicit session, no cwd) and control clients; both fall back
// to the server-default scope.
func WithSessionCWD(ctx context.Context, cwd string) context.Context {
	if cwd == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionCWDCtxKey{}, cwd)
}

// SessionCWDFromContext returns the session cwd attached via
// WithSessionCWD, or "" when none is present.
func SessionCWDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if cwd, ok := ctx.Value(sessionCWDCtxKey{}).(string); ok {
		return cwd
	}
	return ""
}

// authorizedCallCtxKey carries the tool name of an inbound tools/call that
// the session's own authorization already permitted (see
// Server.IsToolEnabledForSession). It exists because mcp-go re-runs every
// registered tool filter at CALL time (server.passesToolFilters, added in
// mcp-go v0.55.1) with the single requested tool: a filter that only shapes
// tools/list VISIBILITY would otherwise turn a legitimate by-name call into
// "tool '<name>' not found". Unexported key: set it via
// WithAuthorizedToolCall.
type authorizedCallCtxKey struct{}

// WithAuthorizedToolCall returns a context marking name as an authorized
// by-name tools/call for this session. The daemon's MCP dispatcher sets it
// after IsToolEnabledForSession accepts the call and before HandleMessage, so
// toolSurfaceFilter knows the follow-up single-tool filter invocation is
// mcp-go's call-time access check and not a tools/list render.
//
// This never widens what a session may call: the marker is only set for names
// the session's effective surface already permits, and checkToolGate still
// runs the authoritative per-call gate inside the handler wrapper.
func WithAuthorizedToolCall(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, authorizedCallCtxKey{}, name)
}

// authorizedToolCallFromContext returns the tool name attached via
// WithAuthorizedToolCall, or "" when none is present.
func authorizedToolCallFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if name, ok := ctx.Value(authorizedCallCtxKey{}).(string); ok {
		return name
	}
	return ""
}

// repoAllowCtxKey carries the per-request repo allow-set resolved by
// handleAnalyze (resolveScope → ResolvedScope.RepoAllow). The scoped-
// node accessors (scopedNodes / scopedNodesByKinds / scopedNodeSlice)
// read it to narrow within the workspace ceiling without threading a
// param through their ~40 call sites. Unexported: only handleAnalyze
// ever sets it, on the per-request ctx — use withRepoAllow /
// repoAllowFromContext.
type repoAllowCtxKey struct{}

// withRepoAllow returns a context carrying the per-request repo
// allow-set resolved by handleAnalyze. An empty/nil allow-set returns
// ctx unchanged (the common no-narrowing case), so non-analyze callers
// and unnarrowed analyze calls are byte-for-byte unaffected.
func withRepoAllow(ctx context.Context, allow map[string]bool) context.Context {
	if len(allow) == 0 {
		return ctx
	}
	return context.WithValue(ctx, repoAllowCtxKey{}, allow)
}

// repoAllowFromContext returns the repo allow-set attached via
// withRepoAllow, or nil when none is present.
func repoAllowFromContext(ctx context.Context) map[string]bool {
	if ctx == nil {
		return nil
	}
	if a, ok := ctx.Value(repoAllowCtxKey{}).(map[string]bool); ok {
		return a
	}
	return nil
}

// sessionLocal bundles the per-client state that should not aggregate
// across sessions: recent agent activity (viewed/modified files and
// symbols), and session-scoped token-savings counters. Shared pieces —
// the graph, feedback store, the cumulative savings store on disk —
// stay on *Server directly or are referenced via pointers that all
// sessions share.
type sessionLocal struct {
	session      *sessionState
	tokenStats   *tokenStats
	localization *localizationTerminalState
}

// newSessionLocal constructs a fresh per-session state container. The
// persistent savings store pointer is threaded in so per-session
// record() calls still contribute to cumulative totals on disk — each
// session's in-memory counters are isolated but the file they flush to
// is shared. parent, when non-nil, is the process-wide tokenStats
// aggregate; every per-session record() call also bumps it so the
// shared default reflects daemon-wide live activity.
func newSessionLocal(id string, persistent *savings.Store, repoPath string, parent *tokenStats) *sessionLocal {
	return &sessionLocal{
		session:      newSessionState(),
		localization: newLocalizationTerminalState(),
		tokenStats: &tokenStats{
			persistent: persistent,
			repoPath:   repoPath,
			parent:     parent,
			sessionID:  id,
		},
	}
}

// sessionMap is a thread-safe string→*sessionLocal registry. Used by
// *Server to multiplex session-scoped state when running inside the
// daemon. The embedded / stdio server path doesn't consult this map;
// it reads *Server.session directly.
//
// The map also holds a pointer to the shared persistent savings store,
// so per-session tokenStats created by lazy get() calls inherit it
// automatically. Updating it via setPersistent propagates to every
// existing entry as well.
type sessionMap struct {
	mu         sync.Mutex
	sessions   map[string]*sessionLocal
	persistent *savings.Store
	repoPath   string
	// parent is the process-wide tokenStats aggregate. Each per-session
	// counter created by get() inherits it as its parent so record()
	// calls fan out to the daemon-wide totals.
	parent *tokenStats
}

func newSessionMap() *sessionMap {
	return &sessionMap{sessions: make(map[string]*sessionLocal)}
}

// setParentTokenStats installs the process-wide tokenStats so every
// session created here aggregates into it. Called once at server
// construction (Server.attachSessionMap) before any client connects.
func (m *sessionMap) setParentTokenStats(parent *tokenStats) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.parent = parent
	for _, sl := range m.sessions {
		if sl.tokenStats == nil {
			continue
		}
		sl.tokenStats.mu.Lock()
		sl.tokenStats.parent = parent
		sl.tokenStats.mu.Unlock()
	}
}

// get returns the session state for id, creating it if absent. Never
// returns nil — a missing entry is created lazily. Thread-safe.
func (m *sessionMap) get(id string) *sessionLocal {
	m.mu.Lock()
	defer m.mu.Unlock()
	sl, ok := m.sessions[id]
	if !ok {
		sl = newSessionLocal(id, m.persistent, m.repoPath, m.parent)
		m.sessions[id] = sl
	}
	return sl
}

// release drops the session entry for id. Called when the daemon's
// accept loop sees a proxy disconnect.
func (m *sessionMap) release(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

// snapshotSessions returns every live session's state. The map lock is
// held only while copying the slice, so a caller may take per-session
// locks afterwards without inverting the lock order.
func (m *sessionMap) snapshotSessions() []*sessionState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*sessionState, 0, len(m.sessions))
	for _, sl := range m.sessions {
		if sl != nil && sl.session != nil {
			out = append(out, sl.session)
		}
	}
	return out
}

// setPersistent updates the shared savings store pointer and
// propagates it into every live session so no existing client flushes
// savings to a stale (or nil) store.
func (m *sessionMap) setPersistent(store *savings.Store, repoPath string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.persistent = store
	m.repoPath = repoPath
	for _, sl := range m.sessions {
		if sl.tokenStats == nil {
			continue
		}
		sl.tokenStats.mu.Lock()
		sl.tokenStats.persistent = store
		sl.tokenStats.repoPath = repoPath
		sl.tokenStats.mu.Unlock()
	}
}
