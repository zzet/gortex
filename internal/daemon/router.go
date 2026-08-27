package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
)

// LocalServerSentinel is the Router's reserved identity for "this
// daemon's own in-process graph". It can never be a user slug:
// ServersConfig.Validate rejects any [[server]] claiming it, and the
// workspace resolver rejects any workspace slug equal to it (or
// containing ~ or /). Local identity is NO LONGER derived from
// DefaultServer().Slug — so a remote marked default=true is still
// proxied to, never mistaken for local.
const LocalServerSentinel = "@local"

// Router is the "hybrid-read query router". It takes a tool
// invocation plus an optional scope override, decides
// whether the request should run locally or be proxied to a remote
// server, and returns the result bytes the daemon hands back to its
// MCP client.
//
// The router is created once at daemon construction with three
// dependencies:
//
//   - cfg: the parsed `~/.gortex/servers.toml`. nil when the daemon
//     was started without a multi-server config; in that case
//     RouteToolCall always uses the local executor.
//   - rosters: the WorkspaceRosterCache. Populated lazily as
//     workspaces are first looked up; survives across requests so we
//     don't roundtrip on every query.
//   - localSlug: the slug this daemon hosts itself. When the
//     resolved server matches localSlug, the router calls localExec
//     instead of proxying — this is the "hybrid" part. Empty disables
//     the local-fast path; useful for tests and for daemons that only
//     proxy.
//
// Server clients are created lazily on first use and cached; HTTP
// keep-alive in net/http reuses connections across calls.
// routerState is the swappable roster + cache the Router serves. It is
// held behind an atomic.Pointer so ControlProxy can rebuild it from a
// changed servers.toml and publish it without a restart and without a
// lock on the query hot path: an in-flight call loads the pointer once
// and keeps that snapshot even if a concurrent reload swaps in a fresh
// one (no torn router, no mid-call roster flip).
type routerState struct {
	cfg     *ServersConfig
	rosters *WorkspaceRosterCache
}

type Router struct {
	state        atomic.Pointer[routerState]
	resolveCwd   CwdResolver
	localSlug    string
	logger       *zap.Logger
	clientsMu    sync.Mutex
	clients      map[string]*ServerClient
	localExecute LocalExecutor
	// federator augments a LOCAL read result with enabled remotes'
	// results via the read-only fan-out. nil disables federation entirely.
	federator *Federator
}

// LocalExecutor runs a tool against the local server. The daemon
// supplies one wrapping its in-process MCP server. Returning bytes
// (not a decoded struct) lets this stay independent of the mcp-go
// types and matches what ProxyTool yields, so the caller treats both
// paths identically.
type LocalExecutor func(ctx context.Context, toolName string, body []byte) ([]byte, int, error)

// RouterConfig bundles construction inputs.
type RouterConfig struct {
	Servers      *ServersConfig
	Rosters      *WorkspaceRosterCache
	CwdResolver  CwdResolver
	LocalSlug    string
	LocalExecute LocalExecutor
	Logger       *zap.Logger
	// Federation tunes the read-only fan-out. When the daemon builds a
	// router it also builds a Federator over these knobs; a zero value
	// uses the defaults. --oneshot passes no router and so no Federator.
	Federation FederationConfig
}

// NewRouter constructs a Router with the given dependencies. nil
// Logger is replaced with a no-op to keep call sites tidy. nil
// CwdResolver defaults to DefaultCwdResolver.
func NewRouter(rc RouterConfig) *Router {
	r := &Router{
		resolveCwd:   rc.CwdResolver,
		localSlug:    rc.LocalSlug,
		logger:       rc.Logger,
		clients:      make(map[string]*ServerClient),
		localExecute: rc.LocalExecute,
	}
	r.state.Store(&routerState{cfg: rc.Servers, rosters: rc.Rosters})
	if r.logger == nil {
		r.logger = zap.NewNop()
	}
	if r.resolveCwd == nil {
		r.resolveCwd = DefaultCwdResolver
	}
	r.federator = NewFederator(rc.Federation, r.clientFor, r.logger)
	return r
}

// RouteContext is the per-call routing input. cwd flows from the
// MCP client (typically the user's pwd at the time of the
// invocation); scopeOverride is what the tool args carried (e.g.
// `workspace: "tuck"`). Either may be empty.
type RouteContext struct {
	Cwd           string
	ScopeOverride string
	// SessionID identifies the calling MCP session for the cross-daemon
	// audit log (empty for the HTTP / sessionless paths).
	SessionID string
	// EnabledRemotes is the per-call snapshot of the effective
	// enabled-set: the dialable roster entries that remain enabled
	// after session overrides are applied over the global Enabled
	// state, captured once per call by the dispatch site. The remote
	// hop gates on it — a resolved remote not present here is refused
	// with a structured "remote disabled" envelope (fail-closed),
	// covering BOTH the explicit single-remote route and the
	// federation fan-out. nil means "not pre-computed": the router
	// falls back to its own global-only enabled-set so the gate is
	// never accidentally bypassed.
	EnabledRemotes []ServerEntry
}

// ErrRouteUnresolved is returned when no server can be picked for a
// request. The caller may surface this as a user-visible "no
// workspace declared and no default server set" error.
var ErrRouteUnresolved = errors.New("no server resolves for this request")

// RouteToolCall implements the priority chain:
//
//  1. If the resolved server slug is empty, the daemon has no remote
//     servers configured: fall back to localExecute.
//  2. If the resolved slug equals localSlug, run locally.
//  3. Otherwise proxy to the remote server.
//
// On a cache miss with a non-empty workspace slug, the router also
// triggers a one-shot FetchWorkspaceRoster call so subsequent
// requests for the same workspace skip the discovery roundtrip.
//
// The body bytes passed through are whatever the caller wants to
// send to the tool — typically the JSON the MCP client posted to
// /v1/tools/<name>. The router does NOT re-marshal them.
func (r *Router) RouteToolCall(ctx context.Context, toolName string, body []byte, route RouteContext) ([]byte, int, error) {
	st := r.state.Load()
	if st == nil || st.cfg == nil {
		// No multi-server config: only local makes sense.
		return r.callLocal(ctx, toolName, body)
	}
	lookup := RouteForCwd(st.cfg, st.rosters, r.resolveCwd, route.Cwd, route.ScopeOverride)
	r.logger.Debug("router: resolve",
		zap.String("tool", toolName),
		zap.String("workspace", lookup.Workspace),
		zap.String("source", lookup.Source))
	if lookup.Server == nil {
		// We have a workspace but no server for it: try a roster
		// fetch across every configured server so the next call
		// resolves. If none claim it, fall through to local —
		// better than failing outright when localSlug is the only
		// runtime option.
		if lookup.Workspace != "" && st.rosters != nil {
			r.discoverRoster(lookup.Workspace)
			st = r.state.Load()
			lookup = RouteForCwd(st.cfg, st.rosters, r.resolveCwd, route.Cwd, route.ScopeOverride)
		}
		if lookup.Server == nil {
			if r.localExecute != nil {
				return r.callLocalFederated(ctx, toolName, body, route)
			}
			return nil, 0, fmt.Errorf("%w: workspace=%q", ErrRouteUnresolved, lookup.Workspace)
		}
	}
	if lookup.Server.Slug == r.localSlug {
		// Local fast path: the resolved slug is the reserved local
		// sentinel (the daemon's own in-process graph). A roster row can
		// never carry the sentinel, so a remote marked default=true now
		// proxies correctly instead of being mistaken for local. The
		// "no server resolves" case is already handled above by the
		// lookup.Server == nil fall-through.
		return r.callLocalFederated(ctx, toolName, body, route)
	}

	// Remote hop. Two gates fire here, BEFORE any client is built or
	// any bearer token leaves the process, for BOTH explicit
	// single-remote routing and (later) federation fan-out:
	//   1. enabled-set gate — a disabled remote is never queried.
	//   2. effect-gate — only effect-free reads route to a remote. Durable
	//      writes and volatile session controls both stay machine-local.
	slug := lookup.Server.Slug
	// Audit every remote-routed call (cross-daemon access record),
	// emitted before the gates so a refusal is auditable too.
	r.logger.Info("federation: remote-routed call",
		zap.String("tool", toolName),
		zap.String("target_slug", slug),
		zap.String("cwd", route.Cwd),
		zap.String("session_id", route.SessionID))
	enabled := route.EnabledRemotes
	if enabled == nil {
		// Caller didn't pre-compute; fall back to the router's own
		// global enabled-set so the gate is never silently bypassed.
		enabled = r.EffectiveEnabledRemotes(nil)
	}
	if !remoteEnabledIn(enabled, slug) {
		return remoteDisabledRefusal(slug)
	}
	if IsEffectful(toolName) {
		return remoteReadOnlyRefusal(toolName, slug)
	}

	cli, err := r.clientFor(*lookup.Server)
	if err != nil {
		return nil, 0, err
	}
	return cli.ProxyToolCtx(ctx, toolName, body)
}

// remoteEnabledIn reports whether slug is in the per-call enabled-set.
func remoteEnabledIn(enabled []ServerEntry, slug string) bool {
	for i := range enabled {
		if enabled[i].Slug == slug {
			return true
		}
	}
	return false
}

// remoteDisabledRefusal is the structured envelope returned when a route
// resolves to a remote the effective enabled-set excludes. Status 403 so
// the dispatch site frames it as a response, not a local fall-through;
// distinct error_code so a client tells "disabled" from "unreachable".
func remoteDisabledRefusal(slug string) ([]byte, int, error) {
	b, _ := json.Marshal(map[string]any{
		"error":      "remote_disabled",
		"error_code": "remote_disabled",
		"slug":       slug,
		"message":    fmt.Sprintf("remote %q is disabled; run `gortex proxy on %s` to enable it", slug, slug),
	})
	return b, http.StatusForbidden, nil
}

// remoteReadOnlyRefusal is the structured envelope returned when an
// effectful tool resolves to a remote. In v1 neither durable writes nor
// volatile session controls route to a remote, regardless of flags. Fires
// before any outbound HTTP.
func remoteReadOnlyRefusal(tool, slug string) ([]byte, int, error) {
	b, _ := json.Marshal(map[string]any{
		"error":       "remote_read_only",
		"error_code":  "remote_read_only",
		"tool":        tool,
		"target_slug": slug,
		"message":     fmt.Sprintf("%q changes state and remote %q is read-only — effectful tools never route to a remote", tool, slug),
	})
	return b, http.StatusForbidden, nil
}

// callLocal invokes the local executor or returns an error if the
// router was constructed without one. The caller's ctx is forwarded
// verbatim so per-session values attached upstream (notably
// `mcp.WithSessionID`) reach the tool handler — discarding ctx here
// would let local-fast-path tool calls run under context.Background
// and lose every per-session signal (client name → default wire
// format, in-flight cancellation, deadlines, etc.).
func (r *Router) callLocal(ctx context.Context, toolName string, body []byte) ([]byte, int, error) {
	if r.localExecute == nil {
		return nil, 0, fmt.Errorf("%w: no local executor wired", ErrRouteUnresolved)
	}
	return r.localExecute(ctx, toolName, body)
}

// callLocalFederated runs the local executor and, for an allowlisted
// read tool, augments the result with the enabled remotes' results via
// the read-only fan-out — the post-step that lives on the LOCAL path
// only. The verbatim remote-route path is never federated (no fan-out
// recursion).
func (r *Router) callLocalFederated(ctx context.Context, toolName string, body []byte, route RouteContext) ([]byte, int, error) {
	localBody := body
	if r.federator != nil {
		// A non-mergeable-format request gets its local budget shrunk by
		// the local-only note's size, so the note the response must carry
		// has headroom by construction. The fan-out and the annotation
		// keep the ORIGINAL body — the caller's cap is what the combined
		// response is held to.
		localBody = r.federator.ReserveLocalNoteBudget(toolName, body, len(route.EnabledRemotes))
	}
	out, status, err := r.callLocal(ctx, toolName, localBody)
	if err != nil || r.federator == nil {
		return out, status, err
	}
	// Carry the caller's cwd + session id so the fan-out audit records the
	// same access tuple as the single-remote proxy route.
	ctx = withAuditInfo(ctx, route.Cwd, route.SessionID)
	return r.federator.Augment(ctx, toolName, body, out, route.EnabledRemotes), status, nil
}

// clientFor returns the ServerClient for the given entry, building
// it on first use. The cache key is the slug; entries that share a
// slug are not allowed by ServersConfig.Validate so this is safe.
func (r *Router) clientFor(entry ServerEntry) (*ServerClient, error) {
	r.clientsMu.Lock()
	defer r.clientsMu.Unlock()
	if c, ok := r.clients[entry.Slug]; ok {
		return c, nil
	}
	c, err := NewServerClient(entry)
	if err != nil {
		return nil, err
	}
	r.clients[entry.Slug] = c
	return c, nil
}

// discoverRoster walks every configured server and asks who hosts
// `workspace`. Caches the answer so the next routing call hits the
// cache. Errors are logged but not surfaced — the caller's lookup
// will simply not find a server and the fallback path takes over.
func (r *Router) discoverRoster(workspace string) {
	st := r.state.Load()
	if st == nil || st.cfg == nil || st.rosters == nil {
		return
	}
	for _, slug := range st.cfg.AllSlugs() {
		entry := st.cfg.FindBySlug(slug)
		if entry == nil {
			continue
		}
		cli, err := r.clientFor(*entry)
		if err != nil {
			r.logger.Warn("router: build client failed",
				zap.String("slug", slug),
				zap.Error(err))
			continue
		}
		repos, err := cli.FetchWorkspaceRoster(workspace)
		if err != nil {
			if errors.Is(err, ErrWorkspaceNotFound) {
				st.rosters.SetNotFound(slug, workspace)
				continue
			}
			r.logger.Debug("router: roster fetch failed",
				zap.String("slug", slug),
				zap.String("workspace", workspace),
				zap.Error(err))
			continue
		}
		st.rosters.Set(slug, workspace, repos)
		return
	}
}

// CurrentConfig returns the roster the router is currently serving (the
// last-published servers.toml). The returned pointer is a read-only
// snapshot — callers must not mutate it. nil when the router holds no
// roster.
func (r *Router) CurrentConfig() *ServersConfig {
	if r == nil {
		return nil
	}
	st := r.state.Load()
	if st == nil {
		return nil
	}
	return st.cfg
}

// ReloadConfig atomically swaps in a freshly-parsed roster + a fresh
// roster cache, invalidates the old cache, and drops the cached server
// clients so a removed or re-pointed remote is never reused. Applied
// live by ControlProxy with no restart and no lock on the query hot
// path; an in-flight RouteToolCall keeps the snapshot it already loaded.
func (r *Router) ReloadConfig(cfg *ServersConfig, rosters *WorkspaceRosterCache) {
	if r == nil {
		return
	}
	old := r.state.Swap(&routerState{cfg: cfg, rosters: rosters})
	if old != nil && old.rosters != nil {
		old.rosters.Invalidate()
	}
	r.clientsMu.Lock()
	r.clients = make(map[string]*ServerClient)
	r.clientsMu.Unlock()
}

// EffectiveEnabledRemotes computes the per-call enabled-set: the
// dialable roster entries that remain enabled after a session's
// overrides are applied over the global Enabled state. Fail-closed —
// only enabled entries are returned, and a session override naming a
// slug no longer in the roster is ignored (the builder iterates the
// published roster only). Reads off the published roster; never
// re-parses servers.toml.
func (r *Router) EffectiveEnabledRemotes(sess *Session) []ServerEntry {
	cfg := r.CurrentConfig()
	if cfg == nil {
		return nil
	}
	var ov map[string]bool
	if sess != nil {
		ov = sess.RemoteOverrides()
	}
	out := make([]ServerEntry, 0, len(cfg.Server))
	for _, s := range cfg.Server {
		on := s.IsEnabled()
		if v, set := ov[s.Slug]; set {
			on = v
		}
		if on {
			out = append(out, s)
		}
	}
	return out
}

// LookupForCwd exposes RouteForCwd against the router's own
// servers.toml + roster cache + cwd resolver. Callers (notably the
// daemon's MCP dispatcher) use this to decide whether a session's
// cwd has any chance of being routable — locally OR remotely —
// before applying their own "is this cwd tracked?" gate. Returns a
// zero LookupResult when the router has no servers.toml.
func (r *Router) LookupForCwd(cwd, scopeOverride string) LookupResult {
	if r == nil {
		return LookupResult{}
	}
	st := r.state.Load()
	if st == nil || st.cfg == nil {
		return LookupResult{}
	}
	return RouteForCwd(st.cfg, st.rosters, r.resolveCwd, cwd, scopeOverride)
}
