package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"sync"
	"time"
)

// Session represents one proxy or CLI connection to the daemon. Per-session
// state (recent activity, symbol history, token stats for this client)
// lives here; shared state (the graph, feedback store, cumulative savings)
// lives on the Server.
//
// A Session is created on a successful handshake and destroyed when its
// socket connection closes. The daemon routes every inbound frame to its
// session by looking up the net.Conn in the session registry.
type Session struct {
	ID string
	// LogicalSessionID is non-empty for an MCP proxy session that may detach
	// from one socket and rebind to another while its proxy PID remains alive.
	// ID equals this token so all daemon/MCP state continues using one key.
	LogicalSessionID string
	Mode             ConnectionMode
	CWD              string
	ClientName       string
	// ClientVersion is the version reported by the MCP client in its
	// `initialize` request (`params.clientInfo.version`). Empty until
	// the daemon dispatcher sees that frame; the env-var sniff in
	// `cmd/gortex/proxy.go::detectClientName` only fills ClientName.
	ClientVersion string
	// ClientNameSource records where ClientName came from so the
	// MCP-frame snooper can decide whether to overwrite it. "handshake"
	// is the env-var fallback the proxy posts at connect time;
	// "initialize" is the authoritative MCP-protocol value. Anything
	// from "initialize" wins over any "handshake" — including the
	// "unknown" string the proxy uses when env-var detection fails.
	ClientNameSource string
	ClientPID        int
	DefaultRepo      string
	ActiveProject    string
	StartedAt        time.Time

	// ToolSpec / ToolMode are the client-forwarded tool-surface
	// preference (GORTEX_TOOLS / --tools + mode) from the handshake. The
	// daemon resolves the effective per-session tool surface from these
	// so a client can scope (or widen) its own pipe while the daemon keeps
	// serving the full graph to everyone else. Empty = no preference.
	ToolSpec string
	ToolMode string

	// Conn is the underlying socket. Kept for close-on-shutdown and
	// logging; handlers should not read from or write to it directly —
	// framing is the transport's job.
	Conn net.Conn

	// Per-session mutable state that will move over from internal/mcp's
	// Server during the session-isolation refactor. Left as interface{}
	// for now so the types can evolve without churning this file every
	// iteration — the refactor will replace this with concrete pointers.
	SessionState any
	SymHistory   any
	TokenStats   any

	// responseMu serializes logical-session MCP dispatch and protects the
	// bounded response cache. Caching before socket write gives reconnect
	// replay at-least-once response semantics without repeating side effects.
	responseMu       sync.Mutex
	responseCache    map[string]cachedMCPResponse
	responseOrder    []string
	responseInFlight map[mcpResponseFlightKey]*mcpResponseFlight
	responseBytes    int

	// remoteOverrides is the per-session enable/disable layer over the
	// global roster: slug -> enabled. An absent slug means "no
	// override" (the global Enabled state wins). It is ephemeral by
	// construction — freed when the *Session is dropped on disconnect
	// via either teardown path (Remove for an AF_UNIX session,
	// RemoveByID for a detached /mcp session) — so no explicit cleanup
	// is needed. Guarded by mu.
	remoteOverrides map[string]bool

	// mu protects ClientName / ClientVersion / ClientNameSource and
	// remoteOverrides, which can be updated by the dispatcher and the
	// proxy-toggle tools mid-session.
	mu sync.RWMutex
}

const (
	sessionResponseCacheEntries       = 64
	sessionResponseCacheBytes         = 8 << 20
	MCPResponseCacheMaxIDBytes        = 4 << 10
	sessionResponseCacheEntryOverhead = sha256.Size + 64
)

type cachedMCPResponse struct {
	requestDigest [sha256.Size]byte
	response      []byte
	size          int
}

type mcpResponseFlightKey struct {
	id     string
	digest [sha256.Size]byte
}

type mcpResponseFlight struct {
	done     chan struct{}
	response []byte
	err      error
}

// dispatchMCPOnce returns a cached serialized response when the same logical
// dispatchMCPOnce returns a response already produced for an identical request
// in the same logical session. Identical concurrent requests share one dispatch,
// while unrelated requests remain independent even if one handler blocks.
func (s *Session) dispatchMCPOnce(request []byte, dispatch func() ([]byte, error)) ([]byte, bool, error) {
	return s.dispatchMCPOnceContext(context.Background(), request, dispatch)
}

func (s *Session) dispatchMCPOnceContext(ctx context.Context, request []byte, dispatch func() ([]byte, error)) ([]byte, bool, error) {
	if s == nil || s.LogicalSessionID == "" {
		reply, err := dispatchMCPWithContext(ctx, dispatch)
		return reply, false, err
	}
	identity, cacheable := parseMCPReplayIdentity(request)
	if !cacheable {
		reply, err := dispatchMCPWithContext(ctx, dispatch)
		return reply, false, err
	}
	key, digest := identity.key, identity.digest
	if len(key) > MCPResponseCacheMaxIDBytes {
		reply, err := dispatchMCPWithContext(ctx, dispatch)
		return reply, false, err
	}

	flightKey := mcpResponseFlightKey{id: key, digest: digest}

retryFlight:
	s.responseMu.Lock()
	if cached, ok := s.responseCache[key]; ok && cached.requestDigest == digest {
		reply := append([]byte(nil), cached.response...)
		s.responseMu.Unlock()
		signalMCPDispatchAdmission(ctx)
		return reply, true, nil
	}
	if existing, ok := s.responseInFlight[flightKey]; ok {
		s.responseMu.Unlock()
		signalMCPDispatchAdmission(ctx)
		select {
		case <-existing.done:
			if existing.err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, false, ctxErr
				}
				goto retryFlight
			}
			return append([]byte(nil), existing.response...), true, nil
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	if s.responseInFlight == nil {
		s.responseInFlight = make(map[mcpResponseFlightKey]*mcpResponseFlight)
	}
	flight := &mcpResponseFlight{done: make(chan struct{})}
	s.responseInFlight[flightKey] = flight
	s.responseMu.Unlock()

	reply, err := dispatchMCPWithContext(ctx, dispatch)
	copyReply := append([]byte(nil), reply...)

	s.responseMu.Lock()
	current, ownsFlight := s.responseInFlight[flightKey]
	if !ownsFlight || current != flight {
		s.responseMu.Unlock()
		return reply, false, err
	}
	flight.response = copyReply
	flight.err = err
	if err == nil && len(copyReply) > 0 {
		entrySize := len(key) + len(copyReply) + sessionResponseCacheEntryOverhead
		if entrySize <= sessionResponseCacheBytes {
			if s.responseCache == nil {
				s.responseCache = make(map[string]cachedMCPResponse)
			}
			if previous, ok := s.responseCache[key]; ok {
				s.responseBytes -= previous.size
				delete(s.responseCache, key)
				for i, ordered := range s.responseOrder {
					if ordered == key {
						s.responseOrder = append(s.responseOrder[:i], s.responseOrder[i+1:]...)
						break
					}
				}
			}
			for len(s.responseOrder) > 0 &&
				(len(s.responseOrder) >= sessionResponseCacheEntries || s.responseBytes+entrySize > sessionResponseCacheBytes) {
				oldest := s.responseOrder[0]
				s.responseOrder = s.responseOrder[1:]
				if evicted, ok := s.responseCache[oldest]; ok {
					s.responseBytes -= evicted.size
					delete(s.responseCache, oldest)
				}
			}
			s.responseOrder = append(s.responseOrder, key)
			s.responseCache[key] = cachedMCPResponse{
				requestDigest: digest,
				response:      copyReply,
				size:          entrySize,
			}
			s.responseBytes += entrySize
		}
	}
	delete(s.responseInFlight, flightKey)
	close(flight.done)
	s.responseMu.Unlock()
	return reply, false, err
}

type mcpReplayIdentity struct {
	key    string
	digest [sha256.Size]byte
}

func parseMCPReplayIdentity(request []byte) (mcpReplayIdentity, bool) {
	trimmed := bytes.TrimSpace(request)
	var envelope map[string]json.RawMessage
	if json.Unmarshal(trimmed, &envelope) != nil {
		return mcpReplayIdentity{}, false
	}
	var method string
	if raw, ok := envelope["method"]; !ok || json.Unmarshal(raw, &method) != nil || method == "" || method == "subscriptions/listen" {
		return mcpReplayIdentity{}, false
	}
	rawID, present := envelope["id"]
	if !present {
		return mcpReplayIdentity{}, false // notification
	}
	rawID = bytes.TrimSpace(rawID)
	canonicalID, valid := canonicalMCPRequestID(rawID)
	if !valid {
		return mcpReplayIdentity{}, false
	}

	// Normalize only the request ID representation before hashing. The digest
	// still distinguishes different methods, params, and extension fields, while
	// equivalent JSON string escapes share replay and singleflight state.
	normalizedID, err := json.Marshal(canonicalID)
	if err != nil {
		return mcpReplayIdentity{}, false
	}
	envelope["id"] = normalizedID
	normalizedRequest, err := json.Marshal(envelope)
	if err != nil {
		return mcpReplayIdentity{}, false
	}
	return mcpReplayIdentity{
		key:    canonicalID,
		digest: sha256.Sum256(normalizedRequest),
	}, true
}

func mcpResponseCacheIdentity(request []byte) (string, [sha256.Size]byte, bool) {
	identity, ok := parseMCPReplayIdentity(request)
	return identity.key, identity.digest, ok
}

// SetClientInfo updates the session's client metadata from the MCP
// `initialize` or `server/discover` / `_meta` frame. Called by the daemon
// dispatcher when it sees the client identity on this session. Idempotent — a
// second call (e.g. on protocol re-init) just overwrites.
func (s *Session) SetClientInfo(name, version string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if name != "" {
		s.ClientName = name
		s.ClientNameSource = "initialize"
	}
	if version != "" {
		s.ClientVersion = version
	}
	s.mu.Unlock()
}

// HasInitializeClientInfo reports whether client metadata has already been
// populated from an MCP protocol frame (`initialize`, `server/discover`, or
// per-request `_meta` in MCP 2026-07-28).
func (s *Session) HasInitializeClientInfo() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ClientNameSource == "initialize"
}

// SetRemoteOverride sets a per-session enable/disable override for a
// remote slug. It wins over the remote's global Enabled state for the
// lifetime of this session only.
func (s *Session) SetRemoteOverride(slug string, enabled bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.remoteOverrides == nil {
		s.remoteOverrides = make(map[string]bool)
	}
	s.remoteOverrides[slug] = enabled
	s.mu.Unlock()
}

// ClearRemoteOverride removes a per-session override so the remote
// reverts to its global Enabled state.
func (s *Session) ClearRemoteOverride(slug string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.remoteOverrides, slug)
	s.mu.Unlock()
}

// RemoteOverrides returns a copy of the per-session override map under
// the read lock, so callers can iterate without racing a concurrent
// toggle. nil when no override has been set.
func (s *Session) RemoteOverrides() map[string]bool {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.remoteOverrides) == 0 {
		return nil
	}
	out := make(map[string]bool, len(s.remoteOverrides))
	for k, v := range s.remoteOverrides {
		out[k] = v
	}
	return out
}

// SnapshotClientInfo returns the current client name/version pair
// safely under the session lock. Used by the status path which reads
// while the dispatcher may be writing.
func (s *Session) SnapshotClientInfo() (name, version string) {
	if s == nil {
		return "", ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ClientName, s.ClientVersion
}

// SessionRegistry tracks active sessions. Safe for concurrent access from
// the accept goroutine and the control-surface handlers.
type SessionRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*Session // session_id → Session
	byConn   map[net.Conn]*Session
}

func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{
		sessions: make(map[string]*Session),
		byConn:   make(map[net.Conn]*Session),
	}
}

// Register creates and stores a new session for the given connection.
// Called after a successful handshake. Generates the session ID.
func (r *SessionRegistry) Register(conn net.Conn, h Handshake) *Session {
	logicalID := ""
	if h.Mode == ModeMCP && h.PID > 0 {
		logicalID = h.LogicalSessionID
	}

	r.mu.Lock()
	if logicalID != "" {
		if existing := r.sessions[logicalID]; existing != nil &&
			existing.LogicalSessionID == logicalID &&
			existing.Mode == h.Mode &&
			existing.ClientPID == h.PID &&
			existing.CWD == h.CWD &&
			existing.ToolSpec == h.Tools &&
			existing.ToolMode == h.ToolsMode {
			oldConn := existing.Conn
			if oldConn != nil {
				delete(r.byConn, oldConn)
			}
			existing.Conn = conn
			r.byConn[conn] = existing
			r.mu.Unlock()
			if oldConn != nil && oldConn != conn {
				_ = oldConn.Close()
			}
			return existing
		}
		// Never overwrite a non-logical session that happens to share the
		// requested token. Fall back to an ordinary generated ID instead.
		if r.sessions[logicalID] != nil {
			logicalID = ""
		}
	}

	id := newSessionID()
	if logicalID != "" {
		id = logicalID
	}
	s := &Session{
		ID:               id,
		LogicalSessionID: logicalID,
		Mode:             h.Mode,
		CWD:              h.CWD,
		ClientName:       h.ClientName,
		ClientNameSource: "handshake",
		ClientPID:        h.PID,
		ToolSpec:         h.Tools,
		ToolMode:         h.ToolsMode,
		StartedAt:        time.Now(),
		Conn:             conn,
	}
	r.sessions[s.ID] = s
	r.byConn[conn] = s
	r.mu.Unlock()
	return s
}

// RegisterDetached creates a session that isn't bound to a net.Conn —
// used by HTTP-side transports (Streamable HTTP, future SSE/WebSocket
// adapters) where the daemon doesn't own a persistent socket. The
// supplied id becomes the session ID verbatim so the transport's own
// session-id space (e.g. streamable.SessionStore) lines up with the
// daemon's status/metrics surface; pass "" to auto-generate one.
func (r *SessionRegistry) RegisterDetached(id string, h Handshake) *Session {
	if id == "" {
		id = newSessionID()
	}
	s := &Session{
		ID:               id,
		Mode:             h.Mode,
		CWD:              h.CWD,
		ClientName:       h.ClientName,
		ClientNameSource: "handshake",
		ClientPID:        h.PID,
		ToolSpec:         h.Tools,
		ToolMode:         h.ToolsMode,
		StartedAt:        time.Now(),
	}
	r.mu.Lock()
	r.sessions[s.ID] = s
	r.mu.Unlock()
	return s
}

// RemoveByID deletes a session by id (used by detached sessions which
// have no net.Conn to key off). Idempotent.
func (r *SessionRegistry) RemoveByID(id string) *Session {
	if id == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[id]
	if s == nil {
		return nil
	}
	delete(r.sessions, id)
	if s.Conn != nil {
		delete(r.byConn, s.Conn)
	}
	return s
}

// GetByID returns a session by its id, or nil when no session is
// registered under that id. Used by detached-session lookup paths.
func (r *SessionRegistry) GetByID(id string) *Session {
	if id == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sessions[id]
}

// IsAttached reports whether id currently owns a live socket. It is the safe
// observation point for reconnect coordination; callers must not read
// Session.Conn directly while Register/Remove may rebind it.
func (r *SessionRegistry) IsAttached(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s := r.sessions[id]
	return s != nil && s.Conn != nil
}

// Remove deletes the session for a connection. Idempotent — safe to call
// from both the accept-loop's defer and the shutdown path.
func (r *SessionRegistry) Remove(conn net.Conn) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.byConn[conn]
	if s == nil {
		return nil
	}
	delete(r.byConn, conn)
	if s.LogicalSessionID != "" && s.Conn == conn {
		// Keep the logical session and its MCP-owned state while the proxy
		// process lives. A later handshake rebinds it; SweepDead performs the
		// final removal and SessionEnded cleanup after the proxy PID exits.
		s.Conn = nil
		return nil
	}
	if s.Conn != conn {
		// This was an old socket that lost a rebind race. The new connection
		// owns the session and must not be torn down by the old handler.
		return nil
	}
	delete(r.sessions, s.ID)
	return s
}

// Get returns the session for a connection, or nil if the connection hasn't
// completed its handshake yet (or was already removed).
func (r *SessionRegistry) Get(conn net.Conn) *Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byConn[conn]
}

// Count returns the number of live sessions — used by the status command
// and for metrics.
func (r *SessionRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sessions)
}

// SweepDead removes every session whose originating client process (by its
// handshake PID) is no longer alive, closing the session's connection. Sessions
// with no recorded PID (0 — detached/HTTP transports, or a client that reported
// none) are left untouched, since liveness can't be judged from a PID we don't
// have. Returns the removed sessions so the caller can log / adjust metrics.
// isAlive is injected (platform.ProcessAlive in production) so the sweep is
// testable without spawning real processes.
func (r *SessionRegistry) SweepDead(isAlive func(int) bool) []*Session {
	r.mu.Lock()
	var dead []*Session
	for id, s := range r.sessions {
		if s.ClientPID <= 0 || isAlive(s.ClientPID) {
			continue
		}
		dead = append(dead, s)
		delete(r.sessions, id)
		if s.Conn != nil {
			delete(r.byConn, s.Conn)
		}
	}
	r.mu.Unlock()
	// Close connections outside the lock — Close can block.
	for _, s := range dead {
		if s.Conn != nil {
			_ = s.Conn.Close()
		}
	}
	return dead
}

// All returns a snapshot of every live session. The caller must not
// mutate the returned Session objects; they're shared with the registry.
func (r *SessionRegistry) All() []*Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	return out
}

// newSessionID generates a short URL-safe identifier. 8 bytes of entropy
// gives us 16 hex chars — collision-resistant enough for a per-user
// single-process registry without bloating log lines.
func newSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "sess_" + hex.EncodeToString(b[:])
}
