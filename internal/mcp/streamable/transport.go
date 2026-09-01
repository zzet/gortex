package streamable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/zzet/gortex/internal/daemon"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"go.uber.org/zap"
)

// Header names defined by the MCP 2026 Streamable HTTP transport spec.
// They are case-insensitive on the wire (Go's net/http handles that)
// but are spelled here in the spec's canonical form so log output
// stays grep-friendly.
const (
	HeaderSessionID       = "Mcp-Session-Id"
	HeaderProtocolVersion = "Mcp-Protocol-Version"

	// HeaderSessionIdleTTL advertises the session idle horizon in
	// whole seconds. Not spec-defined (the spec leaves session
	// lifetime to the server), but without it a client's first
	// knowledge of the TTL is the 404/-32001 after its session
	// already expired. Any request on a session resets the clock, so
	// a client that keeps calls at least this frequent never expires.
	HeaderSessionIdleTTL = "Mcp-Session-Idle-Ttl"
)

// DefaultProtocolVersion is what the transport advertises in
// `Mcp-Protocol-Version` when the client did not pin a version on the
// initialize call. Bumped in lockstep with the underlying mcp-go
// library — the 2026-03-26 spec is the current stable line, the
// June-2026 spec will move it forward.
const DefaultProtocolVersion = "2026-03-26"

// Dispatcher is implemented by the in-process MCP server (and the
// daemon's router-aware shim) to turn a JSON-RPC frame into a
// JSON-RPC reply. Returning a nil reply with nil error signals "the
// inbound frame was a notification — write back an HTTP 202 with no
// body" per the spec.
type Dispatcher interface {
	Dispatch(ctx context.Context, frame []byte) ([]byte, error)
}

// MCPServerDispatcher wraps an *mcpserver.MCPServer so the transport
// can dispatch frames without growing a hard dependency on it.
// HandleMessage is goroutine-safe; multiple concurrent requests share
// the same server instance, exactly like the stdio and Unix-socket
// transports already do.
type MCPServerDispatcher struct{ Server *mcpserver.MCPServer }

// Dispatch implements Dispatcher.
func (d MCPServerDispatcher) Dispatch(ctx context.Context, frame []byte) ([]byte, error) {
	if d.Server == nil {
		return nil, errors.New("streamable: MCPServerDispatcher.Server is nil")
	}
	reply := d.Server.HandleMessage(ctx, json.RawMessage(frame))
	if reply == nil {
		return nil, nil
	}
	out, err := json.Marshal(reply)
	if err != nil {
		return nil, fmt.Errorf("streamable: marshal reply: %w", err)
	}
	return out, nil
}

// Config bundles every wire-up knob the transport accepts. Every
// field except Dispatcher and Store has a sane zero-value default; in
// the simplest case the standalone `gortex server` invocation passes
// only those two and a logger.
type Config struct {
	// Dispatcher executes a JSON-RPC frame and returns the reply.
	// Required.
	Dispatcher Dispatcher

	// Store persists per-session state across requests. Required.
	// Pass StatelessStore{} to disable session continuity entirely.
	Store SessionStore

	// Logger receives structured diagnostic events. Nil falls back
	// to zap.NewNop().
	Logger *zap.Logger

	// Router, when non-nil, gets the first crack at every tools/call
	// frame. A remote workspace match short-circuits the dispatch
	// and the upstream JSON response is returned verbatim; an
	// unresolved or local route falls through to Dispatcher. Pass
	// nil for the single-server case (no servers.toml).
	Router *daemon.Router

	// AllowedOrigins, when non-empty, restricts which Origin
	// header values the transport accepts on POST. Defends against
	// DNS-rebinding attacks the spec calls out. An empty list
	// disables the check (the standalone server defaults to
	// localhost-only binding, which provides equivalent protection).
	AllowedOrigins []string

	// ProtocolVersion is the version advertised in
	// `Mcp-Protocol-Version` when the client did not pin one. Empty
	// falls back to DefaultProtocolVersion.
	ProtocolVersion string

	// SessionTTL, when non-zero, is the session idle horizon the
	// transport advertises to clients (HeaderSessionIdleTTL on every
	// POST response, plus `_meta` on the initialize result) and the TTL of
	// the in-memory store the transport falls back to when Store is
	// left nil. A caller supplying its own Store should pass that
	// store's actual TTL here so the advertisement stays truthful;
	// zero advertises nothing.
	SessionTTL time.Duration

	// MaxRequestBytes caps the inbound JSON-RPC body. Zero falls
	// back to 4 MiB, generous enough for the largest WorkspaceEdit
	// payloads we currently see in simulate_chain calls.
	MaxRequestBytes int64

	// InitializeHook, when non-nil, is invoked synchronously after
	// the transport extracts ClientInfo from a successful
	// `initialize` frame. The daemon uses this to propagate the
	// authoritative client name into its per-session metadata
	// (matches the maybeSnoopInitialize behaviour of the
	// stdio-based dispatcher). Errors from the hook are logged but
	// do not fail the request — initialize must always succeed
	// once the inbound frame was well-formed.
	InitializeHook InitializeHook
}

// InitializeHook is the signature the daemon-side adapter implements
// to enrich its per-session bookkeeping with the authoritative
// clientInfo carried in an initialize frame.
type InitializeHook func(ctx context.Context, state *SessionState)

// Transport is an http.Handler exposing a single POST/GET/DELETE
// endpoint speaking the MCP 2026 Streamable HTTP wire format. One
// instance is safe to share across goroutines; the SessionStore is
// the only shared mutable state and is itself goroutine-safe by
// contract.
type Transport struct {
	dispatcher      Dispatcher
	store           SessionStore
	logger          *zap.Logger
	router          *daemon.Router
	allowedOrigins  map[string]struct{}
	protocolVersion string
	maxRequestBytes int64
	sessionTTL      time.Duration
	initializeHook  InitializeHook

	streamsMu sync.Mutex
	streams   map[string]*serverStream
}

// New builds a Transport from its Config. Panics when Dispatcher is
// nil — that's a programmer error caught at startup, not an
// operational failure to log and continue past. Store may be left
// nil, in which case a process-local MemoryStore is allocated with
// the TTL resolved by SessionIdleTTLFromEnv(cfg.SessionTTL).
func New(cfg Config) *Transport {
	if cfg.Dispatcher == nil {
		panic("streamable: Config.Dispatcher is nil")
	}
	store := cfg.Store
	sessionTTL := cfg.SessionTTL
	if store == nil {
		// The fallback store owns its TTL, so the resolved value is
		// also the one to advertise.
		sessionTTL = SessionIdleTTLFromEnv(cfg.SessionTTL)
		store = NewMemoryStore(sessionTTL)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	version := strings.TrimSpace(cfg.ProtocolVersion)
	if version == "" {
		version = DefaultProtocolVersion
	}
	maxBytes := cfg.MaxRequestBytes
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	allowed := make(map[string]struct{}, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		allowed[strings.ToLower(o)] = struct{}{}
	}
	return &Transport{
		dispatcher:      cfg.Dispatcher,
		store:           store,
		logger:          logger,
		router:          cfg.Router,
		allowedOrigins:  allowed,
		protocolVersion: version,
		maxRequestBytes: maxBytes,
		sessionTTL:      sessionTTL,
		initializeHook:  cfg.InitializeHook,
		streams:         make(map[string]*serverStream),
	}
}

// Store exposes the session store so callers can wire SSE notifiers,
// metrics, or admin tools onto it without growing the Transport
// surface.
func (t *Transport) Store() SessionStore { return t.store }

// ServeHTTP implements http.Handler. The spec mandates that the
// transport surface ALL three verbs at the same path; routes split by
// method to keep the dispatch logic in dedicated helpers.
func (t *Transport) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		t.handlePost(w, r)
	case http.MethodGet:
		t.handleGet(w, r)
	case http.MethodDelete:
		t.handleDelete(w, r)
	case http.MethodOptions:
		// Pre-flight CORS support — the spec does not mandate it
		// but every modern HTTP client (browsers, Cursor, VS Code
		// extensions) sends one. Replying 204 with the methods we
		// support keeps them from bailing on the first call.
		w.Header().Set("Allow", "POST, GET, DELETE, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "POST, GET, DELETE, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// originAllowed enforces the Origin allowlist when one is configured.
// Same-origin requests typically omit Origin entirely; a missing
// header is therefore treated as allowed. When AllowedOrigins is
// empty (the default for localhost binds) the check is skipped.
func (t *Transport) originAllowed(r *http.Request) bool {
	if len(t.allowedOrigins) == 0 {
		return true
	}
	origin := strings.ToLower(strings.TrimSpace(r.Header.Get("Origin")))
	if origin == "" {
		return true
	}
	_, ok := t.allowedOrigins[origin]
	return ok
}

// injectSessionTTLMeta stamps the session idle TTL into an initialize
// result's `_meta` (key "gortex/session_idle_ttl_seconds") so typed
// MCP clients — which never see response headers — can schedule
// keep-alives under the horizon. Surgery is RawMessage-scoped: every
// byte the transport did not add round-trips verbatim. Any parse
// hiccup returns the reply unchanged — advertising is best-effort and
// initialize must not fail over it.
func (t *Transport) injectSessionTTLMeta(reply []byte) []byte {
	if t.sessionTTL <= 0 || len(reply) == 0 {
		return reply
	}
	var frame map[string]json.RawMessage
	if err := json.Unmarshal(reply, &frame); err != nil {
		return reply
	}
	resRaw, ok := frame["result"]
	if !ok {
		return reply // error reply — nothing to advertise
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(resRaw, &result); err != nil {
		return reply
	}
	meta := map[string]json.RawMessage{}
	if raw, ok := result["_meta"]; ok {
		if err := json.Unmarshal(raw, &meta); err != nil {
			return reply
		}
	}
	seconds, err := json.Marshal(int64(t.sessionTTL / time.Second))
	if err != nil {
		return reply
	}
	meta["gortex/session_idle_ttl_seconds"] = seconds
	metaRaw, err := json.Marshal(meta)
	if err != nil {
		return reply
	}
	result["_meta"] = metaRaw
	resultRaw, err := json.Marshal(result)
	if err != nil {
		return reply
	}
	frame["result"] = resultRaw
	out, err := json.Marshal(frame)
	if err != nil {
		return reply
	}
	return out
}

// handlePost is the workhorse: parses one or more JSON-RPC frames
// from the body, resolves or mints a session, dispatches each frame
// through the configured Dispatcher (or the multi-server Router), and
// serializes the replies back as either a single JSON object or a
// JSON array, matching the JSON-RPC 2.0 batch convention.
func (t *Transport) handlePost(w http.ResponseWriter, r *http.Request) {
	if !t.originAllowed(r) {
		writeJSONRPCError(w, http.StatusForbidden, nil, -32600,
			"origin not allowed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, t.maxRequestBytes))
	if err != nil {
		writeJSONRPCError(w, http.StatusBadRequest, nil, -32700,
			"failed to read request body")
		return
	}
	if len(body) == 0 {
		writeJSONRPCError(w, http.StatusBadRequest, nil, -32700,
			"empty request body")
		return
	}
	frames, batched, err := splitJSONRPC(body)
	if err != nil {
		writeJSONRPCError(w, http.StatusBadRequest, nil, -32700, err.Error())
		return
	}

	// Set the protocol version response header up-front so even
	// error paths carry it. Clients use it to confirm they're
	// talking to a compatible server before bothering with the
	// JSON-RPC layer.
	clientVersion := strings.TrimSpace(r.Header.Get(HeaderProtocolVersion))
	if clientVersion != "" {
		w.Header().Set(HeaderProtocolVersion, clientVersion)
	} else {
		w.Header().Set(HeaderProtocolVersion, t.protocolVersion)
	}
	// Same up-front treatment for the idle horizon: every response —
	// including the expiry 404 itself — tells the client how long an
	// idle session lives, so late-attaching clients still learn it.
	if t.sessionTTL > 0 {
		w.Header().Set(HeaderSessionIdleTTL,
			strconv.FormatInt(int64(t.sessionTTL/time.Second), 10))
	}

	// Resolve the session once for the whole request — every frame
	// in a batch shares it, which is also how stdio and SSE
	// transports behave.
	sessionID := strings.TrimSpace(r.Header.Get(HeaderSessionID))
	state, found := t.store.Get(sessionID)
	if sessionID != "" && !found {
		writeSessionNotFound(w, sessionID, frames, batched)
		return
	}

	// Dispatch each frame in order and collect replies. We never
	// fan-out across goroutines: JSON-RPC ordering matters when
	// notifications mutate session state mid-batch.
	replies := make([]json.RawMessage, 0, len(frames))
	for _, frame := range frames {
		replyBytes, status, err := t.dispatchFrame(r, &state, frame)
		if err != nil {
			t.logger.Warn("streamable: dispatch failed",
				zap.String("session_id", sessionID), zap.Error(err))
			id, _ := peekJSONRPCID(frame)
			replyBytes = jsonRPCErrorBytes(id, -32603, err.Error())
		}
		if status != 0 && status != http.StatusOK {
			// A remote upstream returned a non-2xx; surface that
			// to the client as a JSON-RPC error frame so the
			// batch shape stays intact.
			if len(replyBytes) == 0 {
				id, _ := peekJSONRPCID(frame)
				replyBytes = jsonRPCErrorBytes(id, -32603,
					fmt.Sprintf("upstream status %d", status))
			}
		}
		if len(replyBytes) == 0 {
			// Notifications have no reply; the frame still
			// counts so a batch retains its slot ordering on
			// the response side via skip semantics.
			continue
		}
		replies = append(replies, replyBytes)
	}

	// Echo the known session id on successful responses. The specification
	// assigns it during initialize; preserving it here also matches Gortex's
	// established response contract.
	if id := state.ID; id != "" {
		w.Header().Set(HeaderSessionID, id)
	}

	// All-notifications batch — respond 202 Accepted, empty body.
	if len(replies) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if batched {
		// Reassemble as a JSON array — encoding/json sees the
		// pre-marshalled bytes verbatim via json.RawMessage.
		_ = json.NewEncoder(w).Encode(replies)
		return
	}
	_, _ = w.Write(replies[0])
	_, _ = w.Write([]byte("\n"))
}

// dispatchFrame runs the Router / InitializeHook / Dispatcher chain
// for a single JSON-RPC frame and returns its (possibly empty) reply
// plus the upstream HTTP status when the router proxied the call.
// state is updated in-place when the frame is an initialize request
// or carries clientInfo metadata worth persisting.
func (t *Transport) dispatchFrame(r *http.Request, state *SessionState, frame []byte) ([]byte, int, error) {
	method, _ := peekJSONRPCMethod(frame)

	switch method {
	case "initialize":
		// Gortex mints a fresh session for initialize. A known session ID is
		// replaced; unknown IDs are rejected by handlePost before dispatch.
		if state.ID != "" {
			t.store.Delete(state.ID)
			*state = SessionState{}
		}
		fresh := newInitializeState(frame, r)
		id, err := t.store.Create(fresh)
		if err != nil {
			return nil, 0, fmt.Errorf("create session: %w", err)
		}
		fresh.ID = id
		*state = fresh
		if t.initializeHook != nil {
			t.initializeHook(r.Context(), state)
			_ = t.store.Update(*state)
		}
		// Fall through to local dispatch — the initialize
		// response is what tells the client the server's
		// capabilities, and the underlying mcp-go server owns
		// that payload. The transport adds the one fact only it
		// knows: the session idle TTL.
		reply, status, err := t.localDispatch(r, *state, frame)
		if err == nil {
			reply = t.injectSessionTTLMeta(reply)
		}
		return reply, status, err

	case "notifications/initialized":
		if state.ID != "" {
			state.Initialized = true
			_ = t.store.Update(*state)
		}
		return t.localDispatch(r, *state, frame)
	}

	// Tool-call frames go through the multi-server router first.
	// Other frames (tools/list, ping, resources/*, prompts/*) flow
	// to the in-process MCP server — federating them would change
	// semantics.
	if method == "tools/call" && t.router != nil {
		if out, status, ok := t.tryRouteToolCall(r, *state, frame); ok {
			return out, status, nil
		}
	}

	return t.localDispatch(r, *state, frame)
}

// localDispatch attaches the session id / cwd / workspace to the
// context and hands the frame to the configured Dispatcher.
func (t *Transport) localDispatch(r *http.Request, state SessionState, frame []byte) ([]byte, int, error) {
	ctx := r.Context()
	if state.ID != "" {
		ctx = gortexmcp.WithSessionID(ctx, state.ID)
	}
	cwd := state.CWD
	if cwd == "" {
		cwd = strings.TrimSpace(r.Header.Get("X-Gortex-Cwd"))
	}
	if cwd != "" {
		ctx = gortexmcp.WithSessionCWD(ctx, cwd)
	}
	reply, err := t.dispatcher.Dispatch(ctx, frame)
	if err != nil {
		return nil, 0, err
	}
	return reply, 0, nil
}

// tryRouteToolCall mirrors the daemon dispatcher's tryProxyToolCall:
// peek at the inbound tools/call arguments for an explicit workspace
// scope or a cwd that resolves to a remote server in the multi-server
// roster, and proxy the call there. A return value of (_, _, false)
// means "fall through to local dispatch".
func (t *Transport) tryRouteToolCall(r *http.Request, state SessionState, frame []byte) ([]byte, int, bool) {
	// Decode the JSON-RPC envelope keeping the inbound `arguments`
	// object as raw bytes — we MUST forward every caller-supplied key
	// (e.g. `query`, `limit`, etc.) to the downstream executor, not
	// just the workspace+cwd peek fields. A previous version
	// re-marshalled only the typed peek struct, which silently
	// stripped every other argument and made every router-routed tool
	// call see an empty args map ("X is required" failures). Mirror
	// the daemon dispatcher's tryProxyToolCall: peek workspace+cwd
	// without dropping the rest.
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		return nil, 0, false
	}
	if envelope.Params.Name == "" {
		return nil, 0, false
	}
	// Second decode is only used to peek the routing hints.
	var peek struct {
		Workspace string `json:"workspace"`
		Cwd       string `json:"cwd"`
	}
	if len(envelope.Params.Arguments) > 0 {
		_ = json.Unmarshal(envelope.Params.Arguments, &peek)
	}
	scope := peek.Workspace
	if scope == "" {
		scope = state.Workspace
	}
	cwd := peek.Cwd
	if cwd == "" {
		cwd = state.CWD
	}
	if cwd == "" {
		cwd = strings.TrimSpace(r.Header.Get("X-Gortex-Cwd"))
	}
	// Wrap the original raw arguments under `{"arguments": {...}}` so
	// the local executor's nested-arguments unmarshal path (see
	// cmd/gortex/server_router.go newLocalToolExecutor) finds them.
	// This matches cmd/gortex/daemon_mcp.go:tryProxyToolCall exactly.
	// A missing `arguments` key AND an explicit JSON `null` both mean
	// "no arguments" at the MCP layer (params.arguments is optional);
	// normalize both to `{}` so the executor's "arguments must be an
	// object when present" check (added for reviewer concern #2) never
	// rejects a legitimate no-arg call.
	rawArgs := envelope.Params.Arguments
	if len(rawArgs) == 0 || strings.TrimSpace(string(rawArgs)) == "null" {
		rawArgs = json.RawMessage(`{}`)
	}
	body, err := json.Marshal(map[string]json.RawMessage{"arguments": rawArgs})
	if err != nil {
		return nil, 0, false
	}
	// Attach the session id AND cwd to ctx before the routing decision —
	// the local-fast path (Decide -> RouteToolCall -> callLocal ->
	// newLocalToolExecutor) threads this ctx straight into the
	// session-policy gate and the handler itself. Session id alone
	// isn't enough: localDispatch below (and the daemon dispatcher's
	// tryProxyToolCall) also attach WithSessionCWD, because handlers
	// use it as a workspace boundary — without it, a session in
	// workspace A could see workspace B's nodes on this routed path.
	ctx := r.Context()
	if state.ID != "" {
		ctx = gortexmcp.WithSessionID(ctx, state.ID)
	}
	if cwd != "" {
		ctx = gortexmcp.WithSessionCWD(ctx, cwd)
	}
	decision := daemon.NewProxyDecision(func() *daemon.Router { return t.router })
	outcome := decision.Decide(ctx, daemon.RouteInputs{
		ToolName: envelope.Params.Name,
		Body:     body,
		Cwd:      cwd,
		Scope:    scope,
	}, nil)
	if !outcome.Proxied {
		// Local route — let the in-process MCP server handle it.
		return nil, 0, false
	}
	out, status := outcome.Out, outcome.Status
	// The router returned an /v1/tools/<name>-shaped response;
	// translate it into a JSON-RPC `result` frame so the client
	// sees the same envelope every other tool/call produces.
	id, _ := peekJSONRPCID(frame)
	wrapped := wrapToolResultAsJSONRPC(id, out, status)
	return wrapped, status, true
}

// handleGet opens an SSE stream the server can use to deliver
// notifications (progress updates, diagnostics subscriptions, sampling
// requests) to the client without waiting for the next POST. The
// stream is bound to the session ID supplied in the header.
func (t *Transport) handleGet(w http.ResponseWriter, r *http.Request) {
	if !t.originAllowed(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	sid := strings.TrimSpace(r.Header.Get(HeaderSessionID))
	if sid == "" {
		http.Error(w, "missing Mcp-Session-Id", http.StatusBadRequest)
		return
	}
	state, ok := t.store.Get(sid)
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set(HeaderSessionID, state.ID)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	stream := t.attachStream(state.ID)
	defer t.detachStream(state.ID, stream)

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stream.done:
			return
		case msg, ok := <-stream.ch:
			if !ok {
				return
			}
			t.writeSSE(w, flusher, msg)
		case <-keepalive.C:
			_, _ = io.WriteString(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// handleDelete is the spec's session-termination verb. Idempotent —
// deleting an unknown session returns 204 just like deleting a known
// one. The transport also tears down any in-flight SSE stream so the
// client's GET unblocks immediately.
func (t *Transport) handleDelete(w http.ResponseWriter, r *http.Request) {
	if !t.originAllowed(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	sid := strings.TrimSpace(r.Header.Get(HeaderSessionID))
	if sid != "" {
		t.store.Delete(sid)
		t.closeStream(sid)
	}
	w.WriteHeader(http.StatusNoContent)
}

// Push sends a server-initiated JSON-RPC message to the SSE stream
// for the given session id (if one is open). It is non-blocking: when
// no listener is attached or the buffer is full the message is
// dropped and the call returns false. Callers wire this from the
// notification bus (diagnostics, progress) the in-process MCP server
// already owns.
func (t *Transport) Push(sessionID string, frame json.RawMessage) bool {
	t.streamsMu.Lock()
	stream, ok := t.streams[sessionID]
	t.streamsMu.Unlock()
	if !ok {
		return false
	}
	select {
	case stream.ch <- frame:
		return true
	default:
		return false
	}
}

// serverStream is the per-session SSE listener state. Buffered so a
// burst of progress notifications doesn't block the in-process MCP
// server's notification loop.
type serverStream struct {
	ch   chan json.RawMessage
	done chan struct{}
}

func (t *Transport) attachStream(sid string) *serverStream {
	stream := &serverStream{
		ch:   make(chan json.RawMessage, 32),
		done: make(chan struct{}),
	}
	t.streamsMu.Lock()
	if existing, ok := t.streams[sid]; ok {
		// Only one GET per session per spec — close the previous
		// stream so the new GET sees clean delivery semantics.
		close(existing.done)
	}
	t.streams[sid] = stream
	t.streamsMu.Unlock()
	return stream
}

func (t *Transport) detachStream(sid string, stream *serverStream) {
	t.streamsMu.Lock()
	defer t.streamsMu.Unlock()
	if current, ok := t.streams[sid]; ok && current == stream {
		delete(t.streams, sid)
	}
}

func (t *Transport) closeStream(sid string) {
	t.streamsMu.Lock()
	defer t.streamsMu.Unlock()
	if stream, ok := t.streams[sid]; ok {
		close(stream.done)
		delete(t.streams, sid)
	}
}

func (t *Transport) writeSSE(w io.Writer, flusher http.Flusher, frame json.RawMessage) {
	// Each SSE message is one or more "data:" lines followed by a
	// blank line. JSON-RPC frames are single-line by convention so
	// one data line is enough.
	_, _ = io.WriteString(w, "event: message\ndata: ")
	_, _ = w.Write(frame)
	_, _ = io.WriteString(w, "\n\n")
	flusher.Flush()
}

// --- helpers --------------------------------------------------------

// splitJSONRPC accepts the request body and returns one frame per
// JSON-RPC message, plus a boolean indicating whether the original
// body was a batch (JSON array) or a single object. Each returned
// frame is a complete, self-contained JSON document the dispatcher
// can hand to mcp-go's HandleMessage verbatim.
func splitJSONRPC(body []byte) ([][]byte, bool, error) {
	trimmed := skipWhitespace(body)
	if len(trimmed) == 0 {
		return nil, false, errors.New("empty request body")
	}
	switch trimmed[0] {
	case '{':
		return [][]byte{body}, false, nil
	case '[':
		var raw []json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, false, fmt.Errorf("parse batch: %w", err)
		}
		if len(raw) == 0 {
			return nil, true, errors.New("empty batch")
		}
		frames := make([][]byte, 0, len(raw))
		for _, r := range raw {
			frames = append(frames, []byte(r))
		}
		return frames, true, nil
	default:
		return nil, false, fmt.Errorf("expected JSON object or array, got %q", trimmed[0])
	}
}

func skipWhitespace(b []byte) []byte {
	for i, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return b[i:]
		}
	}
	return nil
}

// peekJSONRPCMethod extracts the `method` string from a frame
// without unmarshalling the entire envelope. Returns ("", false) when
// the frame is not a JSON-RPC request/notification.
func peekJSONRPCMethod(frame []byte) (string, bool) {
	var env struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(frame, &env); err != nil {
		return "", false
	}
	return env.Method, env.Method != ""
}

// peekJSONRPCID extracts the `id` raw value (number, string, or
// null) from a frame so error responses can echo it back unchanged.
func peekJSONRPCID(frame []byte) (json.RawMessage, bool) {
	var env struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(frame, &env); err != nil {
		return nil, false
	}
	if len(env.ID) == 0 {
		return nil, false
	}
	return env.ID, true
}

// peekJSONRPCRequestID extracts the raw id from a JSON-RPC request. Responses
// also carry ids but must not receive correlated session errors.
func peekJSONRPCRequestID(frame []byte) (json.RawMessage, bool) {
	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
	}
	if err := json.Unmarshal(frame, &env); err != nil {
		return nil, false
	}
	if env.JSONRPC != "2.0" || env.Method == "" || len(env.ID) == 0 {
		return nil, false
	}
	return env.ID, true
}

// jsonRPCErrorBytes returns a marshalled JSON-RPC 2.0 error envelope
// suitable for inclusion in a batch reply. The id may be nil for
// requests where parsing failed before the id could be recovered.
func jsonRPCErrorBytes(id json.RawMessage, code int, message string) []byte {
	env := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{
		JSONRPC: "2.0",
		ID:      id,
	}
	env.Error.Code = code
	env.Error.Message = message
	out, _ := json.Marshal(env)
	return out
}

// writeJSONRPCError writes a top-level error reply for failures that
// happen before per-frame dispatch (body parse errors, origin
// rejection, …). It always sets Content-Type to application/json so
// the client doesn't have to guess.
func writeJSONRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(jsonRPCErrorBytes(id, code, message))
	_, _ = w.Write([]byte("\n"))
}

// writeSessionNotFound rejects an entire stale-session POST before dispatch.
// Request IDs keep their batch order. Responses and notifications never receive
// JSON-RPC replies, so input containing only those carries only the HTTP 404.
func writeSessionNotFound(w http.ResponseWriter, sessionID string, frames [][]byte, batched bool) {
	message := fmt.Sprintf("session %q not found", sessionID)
	replies := make([]json.RawMessage, 0, len(frames))
	for _, frame := range frames {
		id, ok := peekJSONRPCRequestID(frame)
		if !ok {
			continue
		}
		replies = append(replies, jsonRPCErrorBytes(id, -32001, message))
	}

	if len(replies) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	if batched {
		_ = json.NewEncoder(w).Encode(replies)
		return
	}
	_, _ = w.Write(replies[0])
	_, _ = w.Write([]byte("\n"))
}

// wrapToolResultAsJSONRPC takes the HTTP body the router proxied
// from a remote server's /v1/tools/<name> endpoint and re-emits it as
// the JSON-RPC `result` frame mcp-go's HandleMessage would have
// produced locally. The remote returns a `ToolResponse` shape with
// `content`/`isError` fields — those are exactly the fields a
// `mcp.CallToolResult` carries, so the rewrap is a structural map.
func wrapToolResultAsJSONRPC(id json.RawMessage, upstream []byte, status int) []byte {
	if status >= 400 || len(upstream) == 0 {
		msg := "remote tool call failed"
		if status >= 400 {
			msg = fmt.Sprintf("remote tool call failed (HTTP %s)", strconv.Itoa(status))
		}
		return jsonRPCErrorBytes(id, -32603, msg)
	}
	// Try to parse as a {content, isError} object first; fall back
	// to opaque passthrough so a future router that returns a
	// different shape still produces a JSON-RPC-shaped frame.
	var toolReply struct {
		Content []mcp.TextContent `json:"content"`
		IsError bool              `json:"isError"`
	}
	if err := json.Unmarshal(upstream, &toolReply); err == nil && len(toolReply.Content) > 0 {
		result := struct {
			Content []map[string]string `json:"content"`
			IsError bool                `json:"isError,omitempty"`
		}{
			IsError: toolReply.IsError,
		}
		for _, c := range toolReply.Content {
			result.Content = append(result.Content, map[string]string{
				"type": "text",
				"text": c.Text,
			})
		}
		env := struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id,omitempty"`
			Result  any             `json:"result"`
		}{JSONRPC: "2.0", ID: id, Result: result}
		out, _ := json.Marshal(env)
		return out
	}
	env := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Result  json.RawMessage `json:"result"`
	}{JSONRPC: "2.0", ID: id, Result: upstream}
	out, _ := json.Marshal(env)
	return out
}

// newInitializeState extracts everything we can about the client from
// an initialize frame: declared protocol version, clientInfo, raw
// capabilities map, and the optional `_meta.cwd` / `_meta.workspace`
// hints the proxy injects when it dials in on behalf of a CLI user.
func newInitializeState(frame []byte, r *http.Request) SessionState {
	var env struct {
		Params struct {
			ProtocolVersion string          `json:"protocolVersion"`
			Capabilities    json.RawMessage `json:"capabilities"`
			ClientInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
			Meta struct {
				CWD       string `json:"cwd"`
				Workspace string `json:"workspace"`
			} `json:"_meta"`
		} `json:"params"`
	}
	_ = json.Unmarshal(frame, &env)

	cwd := env.Params.Meta.CWD
	if cwd == "" {
		cwd = strings.TrimSpace(r.Header.Get("X-Gortex-Cwd"))
	}
	workspace := env.Params.Meta.Workspace
	if workspace == "" {
		workspace = strings.TrimSpace(r.Header.Get("X-Gortex-Workspace"))
	}

	// Persist the full params block so an external store can
	// replay the entire initialize call onto a fresh worker.
	var rawParams json.RawMessage
	var envRaw struct {
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frame, &envRaw); err == nil {
		rawParams = envRaw.Params
	}

	return SessionState{
		ProtocolVersion: env.Params.ProtocolVersion,
		ClientName:      env.Params.ClientInfo.Name,
		ClientVersion:   env.Params.ClientInfo.Version,
		Capabilities:    env.Params.Capabilities,
		InitParams:      rawParams,
		CWD:             cwd,
		Workspace:       workspace,
	}
}
