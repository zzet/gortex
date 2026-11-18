package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"
)

// readinessBroadcaster fans `notifications/workspace_readiness` push
// events to subscribed MCP sessions. Where `diagnostics` is event-
// driven off LSP publishDiagnostics, readiness is phase-driven off the
// daemon warmup pipeline: snapshot_loaded → parallel_parse → resolve →
// ready (references resolved, graph queryable) → deferred_passes_all →
// global_resolve → end_batch → watcher_started → enrichment_complete,
// plus steady-state ticks when re-indexing finishes. `ready` flips true
// at the `ready` phase — ahead of enrichment — and stays true; the later
// phases carry ready:true and report enrichment progress.
//
// Sessions opt in via `subscribe_workspace_readiness`. The current
// state is replayed immediately as `initial_replay: true` so a freshly
// connected client knows where the daemon is in its warmup curve
// without waiting for the next transition. Delta-filtered by payload
// hash so a no-op republish never fans out.
type readinessBroadcaster struct {
	server specificNotificationSender
	logger *zap.Logger

	mu          sync.RWMutex
	subscribers map[string]bool // session ID → subscribed
	state       map[string]any  // last published payload (nil until first publish)
	lastHash    string
}

func newReadinessBroadcaster(srv specificNotificationSender, logger *zap.Logger) *readinessBroadcaster {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &readinessBroadcaster{
		server:      srv,
		logger:      logger,
		subscribers: make(map[string]bool),
	}
}

// publish updates the broadcaster's last-known state and fans the
// payload to every subscriber. Idempotent under delta filter: a
// payload identical to the last published one is suppressed. Always
// stamps `ts` (RFC3339Nano) on the wire payload.
//
// The caller-supplied payload map is copied before storage so a
// subsequent caller-side mutation can't race the next subscribe replay.
func (b *readinessBroadcaster) publish(payload map[string]any) {
	if b == nil || b.server == nil {
		return
	}
	out := copyPayload(payload)
	out["ts"] = time.Now().UTC().Format(time.RFC3339Nano)

	hash := hashPayload(out)

	b.mu.Lock()
	if b.lastHash == hash {
		b.mu.Unlock()
		return
	}
	b.lastHash = hash
	b.state = out
	subs := make([]string, 0, len(b.subscribers))
	for id := range b.subscribers {
		subs = append(subs, id)
	}
	b.mu.Unlock()

	for _, sid := range subs {
		if err := b.server.SendNotificationToSpecificClient(sid, "notifications/workspace_readiness", out); err != nil {
			b.logger.Debug("send workspace_readiness failed",
				zap.String("session", sid), zap.Error(err))
		}
	}
}

// announce fans an event to every subscriber without making it the
// last-known state. A per-repository event published through publish would
// replace the warmup phase every in-band `warming` block and late subscriber
// reads, so it rides alongside that state instead of over it.
func (b *readinessBroadcaster) announce(payload map[string]any) {
	if b == nil || b.server == nil {
		return
	}
	out := copyPayload(payload)
	out["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	b.mu.RLock()
	subs := make([]string, 0, len(b.subscribers))
	for id := range b.subscribers {
		subs = append(subs, id)
	}
	b.mu.RUnlock()
	for _, sid := range subs {
		if err := b.server.SendNotificationToSpecificClient(sid, "notifications/workspace_readiness", out); err != nil {
			b.logger.Debug("send workspace_readiness failed",
				zap.String("session", sid), zap.Error(err))
		}
	}
}

// subscribe records sessionID and immediately delivers the last-known
// state (if any) as an initial replay. Returns true when a replay
// payload was sent.
func (b *readinessBroadcaster) subscribe(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	b.mu.Lock()
	b.subscribers[sessionID] = true
	var replay map[string]any
	if b.state != nil {
		replay = copyPayload(b.state)
		replay["initial_replay"] = true
	}
	b.mu.Unlock()

	if replay == nil {
		return false
	}
	if err := b.server.SendNotificationToSpecificClient(sessionID, "notifications/workspace_readiness", replay); err != nil {
		b.logger.Debug("workspace_readiness initial replay failed",
			zap.String("session", sessionID), zap.Error(err))
		return false
	}
	return true
}

func (b *readinessBroadcaster) unsubscribe(sessionID string) {
	if sessionID == "" {
		return
	}
	b.mu.Lock()
	delete(b.subscribers, sessionID)
	b.mu.Unlock()
}

func (b *readinessBroadcaster) subscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// snapshot returns the last-known state for inclusion in status /
// debug surfaces. nil when no publish has happened yet.
func (b *readinessBroadcaster) snapshot() map[string]any {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.state == nil {
		return nil
	}
	return copyPayload(b.state)
}

// copyPayload returns a shallow copy of a notification map. We only
// store scalars / slices / nested maps in these payloads; the call
// sites never put pointers behind values, so a shallow copy is
// enough to keep mutations from racing the next replay.
func copyPayload(p map[string]any) map[string]any {
	if p == nil {
		return nil
	}
	out := make(map[string]any, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

// hashPayload derives a stable fingerprint for delta filtering. Map
// keys are sorted before encoding so two equivalent payloads with
// different insertion orders hash identically.
func hashPayload(p map[string]any) string {
	if len(p) == 0 {
		return "empty"
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		// `ts` is monotonic — every publish would mismatch on it
		// alone, defeating the delta filter. Exclude it from the
		// fingerprint so identical content republishes are
		// suppressed regardless of timestamp.
		if k == "ts" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	buf := make([]byte, 0, 128)
	for _, k := range keys {
		buf = append(buf, k...)
		buf = append(buf, '=')
		enc, err := json.Marshal(p[k])
		if err != nil {
			buf = append(buf, "?"...)
		} else {
			buf = append(buf, enc...)
		}
		buf = append(buf, ';')
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// PublishReadiness is the public entry point the daemon (or in-
// process server) calls at warmup-phase transitions. Safe to call
// before any subscriber has connected — the broadcaster keeps the
// last-known state so a late subscriber gets it as initial replay.
//
// Reserved fields automatically stamped on the wire payload:
//   - `phase` (caller-supplied)
//   - `ready` (caller-supplied bool)
//   - `ts` (server-stamped RFC3339Nano)
//
// Empty phase is rejected — phase is the load-bearing field.
func (s *Server) PublishReadiness(phase string, ready bool, extra map[string]any) {
	if s == nil || s.readinessBroadcaster == nil || phase == "" {
		return
	}
	payload := make(map[string]any, len(extra)+2)
	for k, v := range extra {
		payload[k] = v
	}
	payload["phase"] = phase
	payload["ready"] = ready
	s.readinessBroadcaster.publish(payload)
}

// ReadinessPhase returns the daemon's last-published warmup phase and whether
// warmup has completed, read from the readiness broadcaster's last-known
// state. Returns ("", false) before the first publish. Used by the daemon to
// stamp the handshake ack (HandshakeAck.WarmupPhase) so a connecting client
// sees where the daemon is on its warmup curve.
func (s *Server) ReadinessPhase() (phase string, ready bool) {
	if s == nil || s.readinessBroadcaster == nil {
		return "", false
	}
	snap := s.readinessBroadcaster.snapshot()
	if snap == nil {
		return "", false
	}
	if p, ok := snap["phase"].(string); ok {
		phase = p
	}
	if r, ok := snap["ready"].(bool); ok {
		ready = r
	}
	return phase, ready
}

// registerReadinessTools wires the subscribe / unsubscribe MCP tools
// for the workspace_readiness channel.
func (s *Server) registerReadinessTools() {
	s.addTool(
		mcp.NewTool("subscribe_workspace_readiness",
			mcp.WithDescription("Opt the current MCP session into `notifications/workspace_readiness` push events. Once subscribed, every daemon warmup-phase transition (snapshot_loaded → parallel_parse → resolve → ready → deferred_passes_all → global_resolve → end_batch → watcher_started → enrichment_complete) plus steady-state re-index completions are pushed to your session as `{phase, ready, ts, ...}`. `ready` flips true at the `ready` phase — once references are resolved and the graph is queryable — ahead of the slower semantic enrichment, which completes at `enrichment_complete`. The last-known state is replayed immediately as `initial_replay: true` so a freshly connected client knows where the daemon is on its warmup curve. Pair with `unsubscribe_workspace_readiness` to opt back out."),
		),
		s.handleSubscribeReadiness,
	)
	s.addTool(
		mcp.NewTool("unsubscribe_workspace_readiness",
			mcp.WithDescription("Opt the current MCP session out of `notifications/workspace_readiness` push events. Idempotent."),
		),
		s.handleUnsubscribeReadiness,
	)
}

func (s *Server) handleSubscribeReadiness(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.readinessBroadcaster == nil {
		return mcp.NewToolResultError("workspace_readiness broadcaster is not configured"), nil
	}
	id := SessionIDFromContext(ctx)
	if id == "" {
		id = "embedded"
	}
	replayed := s.readinessBroadcaster.subscribe(id)
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"subscribed":  true,
		"session_id":  id,
		"subscribers": s.readinessBroadcaster.subscriberCount(),
		"replayed":    replayed,
	})
}

func (s *Server) handleUnsubscribeReadiness(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.readinessBroadcaster == nil {
		return mcp.NewToolResultError("workspace_readiness broadcaster is not configured"), nil
	}
	id := SessionIDFromContext(ctx)
	if id == "" {
		id = "embedded"
	}
	s.readinessBroadcaster.unsubscribe(id)
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"subscribed":  false,
		"session_id":  id,
		"subscribers": s.readinessBroadcaster.subscriberCount(),
	})
}
