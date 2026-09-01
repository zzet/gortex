package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/pathkey"
)

// mcpDispatcher routes MCP JSON-RPC frames from daemon sessions to the
// shared *gortexmcp.Server. Every frame returns through
// MCPServer.HandleMessage, which is the public entry point the
// mark3labs/mcp-go library exposes for non-stdio embeddings.
//
// Session isolation is handled by threading the daemon-assigned session
// ID into ctx via gortexmcp.WithSessionID before HandleMessage runs.
// Tool handlers resolve per-client state through Server.sessionFor(ctx).
type mcpDispatcher struct {
	srv          *gortexmcp.Server
	multiIndexer *indexer.MultiIndexer
	logger       *zap.Logger
	// router is held behind an atomic.Pointer so ControlProxy can
	// publish a freshly-built (or torn-down) router live without
	// racing a concurrent dispatch read.
	router atomic.Pointer[daemon.Router]
	// decision is the shared peek→route→outcome helper; it reads the
	// router through the atomic accessor so a live swap is reflected.
	decision *daemon.ProxyDecision
	// loggedUntracked records session IDs for which the "cwd not covered"
	// diagnostic has already been emitted, so it fires once per session
	// rather than on every frame. Cleared in SessionEnded.
	loggedUntracked sync.Map
}

func newMCPDispatcher(srv *gortexmcp.Server, mi *indexer.MultiIndexer, logger *zap.Logger) *mcpDispatcher {
	if logger == nil {
		logger = zap.NewNop()
	}
	d := &mcpDispatcher{srv: srv, multiIndexer: mi, logger: logger}
	d.decision = daemon.NewProxyDecision(func() *daemon.Router { return d.router.Load() })
	return d
}

// SetRouter wires the hybrid-read router into the daemon's MCP
// dispatch. With a router set, tools/call frames carrying
// a `workspace` arg or a session cwd that resolves to a non-local
// server are proxied; all other frames flow through the local
// MCPServer.HandleMessage path unchanged.
func (d *mcpDispatcher) SetRouter(r *daemon.Router) { d.router.Store(r) }

// Router returns the currently-wired router (or nil). Exposed so the
// HTTP-side Streamable transport can share the same router instance
// without rebuilding it from servers.toml a second time.
func (d *mcpDispatcher) Router() *daemon.Router { return d.router.Load() }

// Dispatch implements daemon.MCPDispatcher. It hands the raw JSON-RPC
// frame to MCPServer.HandleMessage and returns the response bytes.
// Empty return value means the client sent a notification (no response).
//
// The session ID from the daemon connection is attached to ctx via
// gortexmcp.WithSessionID so tool handlers reach per-session state
// through sessionFor(ctx) rather than the shared default. This is what
// keeps client A's recent-activity separate from client B's.
func (d *mcpDispatcher) Dispatch(ctx context.Context, sess *daemon.Session, frame []byte) ([]byte, error) {
	if d.srv == nil || d.srv.MCPServer() == nil {
		return nil, fmt.Errorf("mcp dispatcher: no server attached")
	}

	// Fast-path reject untracked cwds. Returns a structured JSON-RPC
	// error the agent can surface in chat ("run `gortex track .`")
	// rather than a silent wrong-result. Skipped when the session has
	// no cwd (the CLI and test harnesses don't set one), so control-
	// flow paths keep working unchanged. With a multi-server router
	// wired, a cwd that resolves to a remote workspace via the roster
	// also counts as reachable — otherwise the cwd-walk priority
	// chain in RouteForCwd would be dead code from the dispatcher's
	// perspective.
	// Method-aware untracked gate (F4): a tracked cwd is REQUIRED only for
	// tools/call — that's the frame that needs the graph. initialize and
	// tools/list flow through so the MCP handshake SURVIVES an untracked cwd:
	// initialize returns the inactive-instructions variant (run `gortex track`)
	// and tools/list answers an empty/track-only list, instead of a
	// connection-poisoning errored initialize. Only tools/call is refused (with
	// the structured not-tracked error the agent can act on).
	untracked := sess.CWD != "" && !d.cwdReachable(sess.CWD)
	if untracked {
		d.logUncoveredCWDOnce(sess)
	}
	if untracked && peekFrameMethod(frame) == "tools/call" && !untrackedBootstrapCall(frame) {
		return d.notTrackedError(sess, frame), nil
	}

	ctx = gortexmcp.WithSessionID(ctx, sess.ID)
	// Carry the session's cwd so MCP tool handlers can resolve — and
	// enforce — the workspace boundary for this session. Without this
	// every query runs against the whole multi-workspace graph and a
	// session in workspace A can see workspace B's nodes.
	ctx = gortexmcp.WithSessionCWD(ctx, sess.CWD)
	// Run session-aware: attaching the connected client session (wired
	// by SessionStarted) lets mcp-go's initialize handler mark it
	// initialized — the gate SendNotificationToAllClients applies
	// before delivering tools/list_changed and other pushes.
	ctx = d.srv.WithClientSession(ctx, sess.ID)

	// Relay the client-forwarded tool-surface preference (GORTEX_TOOLS /
	// --tools of the proxy) so the MCP server resolves THIS session's
	// effective preset authoritatively — the daemon serves a shared graph,
	// so a per-client preset only applies if the client's choice reaches
	// the server. No-op when the client forwarded no preference.
	d.srv.NoteSessionToolPolicy(sess.ID, sess.ToolSpec, sess.ToolMode)

	// Identify the MCP client. The handshake's ClientName is the
	// proxy's env-var-based guess (often "unknown" when no known env
	// var matched). The MCP `initialize` request carries the
	// authoritative `clientInfo.name` and `clientInfo.version`, so we
	// snoop it here and overwrite the session metadata. Subsequent
	// status calls then show "claude-code 1.0.42" instead of
	// "unknown".
	d.maybeSnoopInitialize(sess, frame)

	// For tools/call frames carrying a workspace scope or a cwd that
	// routes elsewhere, the daemon
	// proxies to the right server instead of running locally. Other
	// frames (initialize, tools/list, notifications) flow through
	// the local MCPServer below; routing them across a federation
	// would change semantics that are intentionally machine-local.
	if d.router.Load() != nil {
		if proxied, ok := d.tryProxyToolCall(ctx, sess, frame); ok {
			return proxied, nil
		}
	}

	// Promote-on-demand: a tools/call naming a deferred tool (one held out of
	// the eager tools/list under the defer-mode surface) would otherwise return
	// "tool not found" — the underlying MCP server only knows live tools until
	// tools_search promotes them. A direct call by name (the CLI's `gortex call`
	// and the curated `gortex` verbs reach the daemon this way) promotes it
	// first, so a known tool name is reachable without a discovery round-trip.
	// tools_search stays the discovery path. Check the effective session surface
	// before touching the process-global lazy registry: otherwise a facade-v1
	// client hard-calling a hidden legacy name could promote it (and emit
	// list_changed) before the MCP surface filter rejected the call.
	if name := peekFrameToolName(frame); name != "" {
		if d.srv.IsToolEnabledForSession(ctx, name) {
			newly := d.srv.EnsureToolPromotedForSession(ctx, name)
			// Record only permitted calls to deferred/learned tools so a rejected
			// hidden call cannot refresh learned-surface state.
			d.srv.NoteToolUse(name, sess.CWD, newly)
			// Promotion alone is not enough: mcp-go re-applies the tools/list
			// filter to the requested tool at call time and reports a filtered
			// tool as "not found". Mark the call as authorized so the surface
			// filter recognises that probe (see WithAuthorizedToolCall); the
			// per-call gate inside the handler still decides the outcome.
			ctx = gortexmcp.WithAuthorizedToolCall(ctx, name)
		}
	}

	// HandleMessage returns either a JSONRPCResponse, a JSONRPCError, or
	// nil (the message was a notification). It never panics on malformed
	// JSON — it returns a JSON-RPC parse-error frame instead.
	reply := d.srv.MCPServer().HandleMessage(ctx, json.RawMessage(frame))
	if reply == nil {
		return nil, nil
	}

	out, err := json.Marshal(reply)
	if err != nil {
		d.logger.Warn("dispatch: marshal reply failed",
			zap.String("session_id", sess.ID), zap.Error(err))
		return nil, fmt.Errorf("marshal reply: %w", err)
	}
	// For an untracked cwd, rewrite the surviving initialize / tools/list
	// response into its inactive variant: the agent gets the actionable
	// "run gortex track" instructions and an empty track-only tool list.
	if untracked {
		out = rewriteUntrackedResponse(peekFrameMethod(frame), out, sess.CWD, d.trackedRoots())
	}
	return out, nil
}

// peekFrameMethod extracts the JSON-RPC method from a raw frame, or "" when it
// can't be parsed (a malformed frame the server below will reject anyway).
func peekFrameMethod(frame []byte) string {
	var peek struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(frame, &peek)
	return peek.Method
}

// peekFrameToolName returns the tool name of a tools/call frame, or "" when the
// frame is not a tools/call or can't be parsed. Used to promote a deferred tool
// on demand before local dispatch (see Dispatch).
func peekFrameToolName(frame []byte) string {
	var peek struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(frame, &peek) != nil || peek.Method != "tools/call" {
		return ""
	}
	return peek.Params.Name
}

// untrackedBootstrapCall permits only the two facade calls that can explain or
// repair an uncovered cwd. Every graph-backed call remains fail-closed.
func untrackedBootstrapCall(frame []byte) bool {
	var peek struct {
		Method string `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if json.Unmarshal(frame, &peek) != nil || peek.Method != "tools/call" {
		return false
	}
	switch peek.Params.Name {
	case "capabilities":
		return true
	case "track_repository", "index_repository":
		// The legacy-surface equivalents of workspace_admin(track). A
		// client on the legacy surface could otherwise see the repair
		// advice in the initialize instructions and have no tool able
		// to carry it out.
		return true
	case "workspace_admin":
		return strings.EqualFold(strings.TrimSpace(fmt.Sprint(peek.Params.Arguments["operation"])), "track")
	}
	return false
}

// rewriteUntrackedResponse swaps a successful initialize response for its
// untracked-cwd variant. tools/list is deliberately preserved so facade clients
// can discover capabilities and workspace_admin.track without reconnecting;
// every other tools/call remains blocked by Dispatch until tracking succeeds.
func rewriteUntrackedResponse(method string, out []byte, cwd string, roots []string) []byte {
	if len(out) == 0 {
		return out
	}
	var resp map[string]any
	if json.Unmarshal(out, &resp) != nil {
		return out
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		return out // an error response or non-object result — leave it
	}
	if method != "initialize" {
		return out
	}
	result["instructions"] = gortexmcp.ServerInstructionsUntracked(cwd, roots...)
	resp["result"] = result
	if rewritten, mErr := json.Marshal(resp); mErr == nil {
		return rewritten
	}
	return out
}

// SessionStarted implements daemon.SessionStartedHook: register the
// connection as a live mcp-go client session so server-initiated
// notifications (tools/list_changed after a tools_search promotion,
// graph_invalidated, diagnostics pushes, ...) reach this client over
// the socket. Best-effort — on failure the session still works
// request/response, it just misses pushes.
func (d *mcpDispatcher) SessionStarted(sess *daemon.Session, write func([]byte) error) {
	if d.srv == nil || sess == nil {
		return
	}
	if err := d.srv.ConnectSession(sess.ID, write); err != nil {
		d.logger.Debug("daemon: notification session unavailable",
			zap.String("session_id", sess.ID), zap.Error(err))
	}
}

// SessionEnded implements daemon.SessionEndedHook. When a proxy
// disconnects, unregister its notification session and drop its entry
// from the MCP server's session map so idle per-session state doesn't
// accumulate for the daemon's lifetime.
func (d *mcpDispatcher) SessionEnded(sess *daemon.Session) {
	if sess == nil {
		return
	}
	if d.srv != nil {
		d.srv.DisconnectSession(sess.ID)
		d.srv.ReleaseSession(sess.ID)
	}
	d.loggedUntracked.Delete(sess.ID)
}

// cwdReachable reports whether a session cwd has any chance of
// being served by this daemon — locally or remotely. The local arm
// is isCWDTracked. The remote arm consults the router so a cwd
// whose .gortex.yaml::workspace is hosted by a server in
// servers.toml is treated as reachable; the call will be proxied by
// tryProxyToolCall later in Dispatch. Without this, the cwd-walk
// priority chain in RouteForCwd would never trigger from the MCP
// path because the cwd-tracked guard rejects first.
//
// Reachable when:
//   - cwd is empty (no opinion — control-style sessions),
//   - cwd is inside a locally tracked repo,
//   - or the router resolves cwd to a known workspace via an
//     explicit signal: scope-override, .gortex.yaml::workspace, or a
//     server's declared workspaces / cached roster.
//
// The "default-server fall-through" case is intentionally NOT
// treated as reachable: a cwd that nobody claims would otherwise
// silently route to whatever server happens to be marked default
// in servers.toml, which is the same wrong-result class
// repo_not_tracked is meant to prevent.
func (d *mcpDispatcher) cwdReachable(cwd string) bool {
	if cwd == "" {
		return true
	}
	if d.isCWDTracked(cwd) {
		return true
	}
	// Workspace-root fallback: a cwd that CONTAINS tracked repos — an
	// agent opened at the root above its repos — is reachable when
	// either resolution binds it, and it MUST be the same pair
	// sessionScope applies later, or the gate admits sessions the scope
	// blanks out (silent zeros) and refuses ones it could serve.
	//
	// ScopeForCWD binds only when the contained repos agree on one
	// workspace slug, because a session scope is a single slug.
	// ContainedReposScope needs no agreement: it binds to the contained
	// repo SET, which is a filesystem fact and strictly narrower than
	// any slug. Two unrelated repos under one parent are the common
	// shape — each is its own workspace by default — and refusing them
	// left the session with no working call and a remedy (`gortex track
	// <parent>`) that would have indexed every child a second time.
	if d.multiIndexer != nil {
		if _, _, _, ok := d.multiIndexer.ScopeForCWD(cwd); ok {
			return true
		}
		if _, _, ok := d.multiIndexer.ContainedReposScope(cwd); ok {
			return true
		}
	}
	rtr := d.router.Load()
	if rtr == nil {
		return false
	}
	lookup := rtr.LookupForCwd(cwd, "")
	switch lookup.Source {
	case "scope-override", "config-yaml", "roster":
		return true
	}
	return false
}

// isCWDTracked reports whether the proxy's cwd lies inside any tracked
// repo. Equal paths or any subdirectory of a tracked root qualify —
// e.g. a proxy in ~/projects/myapp/internal counts as tracked when
// ~/projects/myapp is in the tracked set.
//
// Returns true when the daemon has no multi-indexer (single-repo mode,
// anything-goes) so we don't accidentally reject valid embedded-style
// sessions during the rollout.
func (d *mcpDispatcher) isCWDTracked(cwd string) bool {
	if d.multiIndexer == nil {
		return true
	}
	// Fold-aware containment: on a case-insensitive filesystem (macOS,
	// Windows) the cwd the editor hands us can differ from the stored
	// root only in letter case or drive-letter case (e.g. VS Code's
	// `c:\repo` vs the config's `C:\repo`). A byte compare would reject
	// it and publish zero tools; HasPathPrefix folds both first (#277).
	for _, meta := range d.multiIndexer.AllMetadata() {
		if pathkey.HasPathPrefix(cwd, meta.RootPath) {
			return true
		}
	}
	return false
}

// logUncoveredCWDOnce emits, at most once per session, a diagnostic that
// names the session cwd and every tracked repo root — so an operator
// looking at an INACTIVE session (zero tools published) can immediately
// see the cwd the daemon was handed and the roots it was compared
// against. The mismatch is most often a path-case or drive-letter
// difference (#277).
func (d *mcpDispatcher) logUncoveredCWDOnce(sess *daemon.Session) {
	if sess == nil || sess.ID == "" {
		return
	}
	if _, loaded := d.loggedUntracked.LoadOrStore(sess.ID, struct{}{}); loaded {
		return
	}
	d.logger.Info("mcp session cwd is not covered by any tracked repo",
		zap.String("session_id", sess.ID),
		zap.String("cwd", sess.CWD),
		zap.Strings("tracked_roots", d.trackedRoots()))
}

// trackedRoots returns the sorted absolute root of every locally tracked
// repo, for the INACTIVE diagnostics.
func (d *mcpDispatcher) trackedRoots() []string {
	if d.multiIndexer == nil {
		return nil
	}
	var roots []string
	for _, meta := range d.multiIndexer.AllMetadata() {
		if meta != nil && meta.RootPath != "" {
			roots = append(roots, meta.RootPath)
		}
	}
	sort.Strings(roots)
	return roots
}

// tryProxyToolCall inspects a JSON-RPC frame and, if it's a
// tools/call that the router resolves to a remote server, proxies it
// and returns the wrapped JSON-RPC response. Returns ok=false when
// the frame is not a tools/call, the router returns ErrRouteUnresolved
// (local-fast path), or the proxy itself errors (we let the local
// path handle it as a fallback so transient network blips don't
// break the user's session).
func (d *mcpDispatcher) tryProxyToolCall(ctx context.Context, sess *daemon.Session, frame []byte) ([]byte, bool) {
	var peek struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &peek); err != nil {
		return nil, false
	}
	if peek.Method != "tools/call" || peek.Params.Name == "" {
		return nil, false
	}
	scope, _ := peek.Params.Arguments["workspace"].(string)
	// A no-arguments call (MCP allows omitting params.arguments) leaves
	// peek.Params.Arguments nil, which json.Marshal renders as literal
	// `"arguments":null` — indistinguishable, to a body-shape validator,
	// from a malformed caller-sent null. Normalize to an empty object so
	// the executor's "arguments must be an object when present" check
	// (added for reviewer concern #2) never rejects a legitimate no-arg
	// call.
	args := peek.Params.Arguments
	if args == nil {
		args = map[string]any{}
	}
	body, err := json.Marshal(map[string]any{"arguments": args})
	if err != nil {
		return nil, false
	}
	outcome := d.decision.Decide(ctx, daemon.RouteInputs{
		ToolName: peek.Params.Name,
		Body:     body,
		Cwd:      sess.CWD,
		Scope:    scope,
	}, sess)
	if !outcome.Proxied {
		// ErrRouteUnresolved or some other failure — let the local
		// HandleMessage path take over (the same body works there).
		return nil, false
	}
	out, status := outcome.Out, outcome.Status
	if status >= 400 {
		// Surface the upstream error as a JSON-RPC error so the
		// client sees a structured failure instead of a 4xx that
		// gets swallowed.
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      peek.ID,
			"error": map[string]any{
				"code":    -32000,
				"message": fmt.Sprintf("proxy %s/%s: status %d", "remote", peek.Params.Name, status),
				"data": map[string]any{
					"upstream_status": status,
					"upstream_body":   string(out),
				},
			},
		}
		buf, _ := json.Marshal(resp)
		return buf, true
	}
	// Success — wrap the proxied bytes as a JSON-RPC result.
	var result any
	if err := json.Unmarshal(out, &result); err != nil {
		// Non-JSON upstream — surface as text content for visibility.
		result = map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(out)}},
		}
	}
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      peek.ID,
		"result":  result,
	}
	buf, _ := json.Marshal(resp)
	return buf, true
}

// maybeSnoopInitialize parses one inbound JSON-RPC frame and, if
// it's an MCP `initialize` request carrying `params.clientInfo`,
// updates the session's ClientName/ClientVersion. Non-initialize
// frames are ignored. Errors swallowed — this is best-effort
// metadata enrichment, not a correctness path.
func (d *mcpDispatcher) maybeSnoopInitialize(sess *daemon.Session, frame []byte) {
	if sess == nil || len(frame) == 0 {
		return
	}
	var peek struct {
		Method string `json:"method"`
		Params struct {
			ClientInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &peek); err != nil {
		return
	}
	if peek.Method != "initialize" {
		return
	}
	if peek.Params.ClientInfo.Name == "" && peek.Params.ClientInfo.Version == "" {
		return
	}
	sess.SetClientInfo(peek.Params.ClientInfo.Name, peek.Params.ClientInfo.Version)
	if d.srv != nil {
		// Propagate the client name into the per-session MCP state so
		// tool handlers can resolve a default wire format (e.g.
		// claude-code → gcx) when a request omits the explicit
		// `format` arg.
		d.srv.NoteSessionClient(sess.ID, peek.Params.ClientInfo.Name, peek.Params.ClientInfo.Version)
	}
	d.logger.Info("daemon: identified MCP client",
		zap.String("session_id", sess.ID),
		zap.String("client", peek.Params.ClientInfo.Name),
		zap.String("version", peek.Params.ClientInfo.Version))
}

// notTrackedError builds a JSON-RPC error frame the agent surfaces to
// the user. The id is echoed from the inbound frame when it's a request;
// a zero id for notifications is fine (clients ignore responses to
// notifications anyway).
//
// Kept structured — error.code uses the MCP-convention -32000 range for
// server-defined errors; error.data carries machine-readable fields
// (error_code, path, suggestion) so a tool UI can offer a one-click
// "track this repo" button without regex-parsing the message string.
func (d *mcpDispatcher) notTrackedError(sess *daemon.Session, inbound []byte) []byte {
	// Pull the request id out of the inbound frame so the response
	// pairs correctly. If parsing fails (malformed frame), send a
	// null id — JSON-RPC clients treat that as "error with no
	// matching request" which is still more informative than
	// silence.
	var peek struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(inbound, &peek)

	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      peek.ID,
		"error": map[string]any{
			"code":    -32000,
			"message": fmt.Sprintf("repository not tracked: %s", sess.CWD),
			"data": map[string]any{
				"error_code": "repo_not_tracked",
				"path":       sess.CWD,
				"suggestion": fmt.Sprintf("Run `gortex track %s` to include this repo in the shared graph.", sess.CWD),
			},
		},
	}
	out, _ := json.Marshal(resp)
	return out
}
