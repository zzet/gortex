package mcp

import (
	"context"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"
)

// HealthSnapshotFn returns the daemon's current health snapshot — a
// JSON-able map of `{uptime_seconds, alloc_bytes, sys_bytes,
// num_goroutine, num_gc, tracked_repos, sessions, lsp_alive, …}`. The
// shape is open-ended: every key contributes to the delta hash, so
// callers can extend the payload without bookkeeping. Returning nil or
// an empty map is treated as "no signal yet" and skipped.
type HealthSnapshotFn func() map[string]any

// healthBroadcaster fans `notifications/daemon_health` push events to
// subscribed MCP sessions. Unlike `diagnostics` (event-driven) and
// `workspace_readiness` (phase-driven), health is timer-driven: every
// `interval` an internal ticker pulls a fresh snapshot from `snapFn`
// and publishes it. Repeated identical snapshots are suppressed by the
// payload-hash delta filter, so a steady-state daemon doesn't fan out
// the same `num_goroutine: 42` over and over.
//
// The ticker only runs while at least one subscriber is connected.
// The first subscribe starts it; the last unsubscribe stops it. This
// keeps a quiescent daemon from burning a ticker goroutine just to
// repeat identical no-op publishes into the void.
//
// Default `interval` is 15 s — long enough to dampen the
// `num_goroutine` flutter that fires on every per-shard scan, short
// enough that a watcher catches health drift well inside the
// human-attention window. Override via the `interval_ms` arg on
// `subscribe_daemon_health` (clamped to [1 s, 5 min]).
type healthBroadcaster struct {
	logger *zap.Logger
	server specificNotificationSender
	snapFn HealthSnapshotFn

	mu          sync.Mutex
	subscribers map[string]bool
	lastHash    string
	lastSnap    map[string]any
	interval    time.Duration
	stopTicker  chan struct{}
	tickerDone  chan struct{}
	closed      bool
	workers     sync.WaitGroup // ticker instances and admitted snapshot/delivery calls
}

const (
	defaultHealthInterval = 15 * time.Second
	minHealthInterval     = time.Second
	maxHealthInterval     = 5 * time.Minute
)

func newHealthBroadcaster(srv specificNotificationSender, snapFn HealthSnapshotFn, logger *zap.Logger) *healthBroadcaster {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &healthBroadcaster{
		logger:      logger,
		server:      srv,
		snapFn:      snapFn,
		subscribers: make(map[string]bool),
		interval:    defaultHealthInterval,
	}
}

// setSnapshotFn injects (or replaces) the snapshot function. The
// broadcaster is constructed in NewServer before the daemon has wired
// its controller / status surfaces, so the daemon entrypoint installs
// the real snapshot fn later via Server.AttachHealthSnapshot.
func (b *healthBroadcaster) setSnapshotFn(fn HealthSnapshotFn) {
	b.mu.Lock()
	if !b.closed {
		b.snapFn = fn
	}
	b.mu.Unlock()
}

// setInterval clamps and replaces the publish interval. If the
// ticker is currently running, it is restarted so the new cadence
// takes effect immediately.
func (b *healthBroadcaster) setInterval(d time.Duration) {
	if d < minHealthInterval {
		d = minHealthInterval
	} else if d > maxHealthInterval {
		d = maxHealthInterval
	}
	b.mu.Lock()
	if b.closed || b.interval == d {
		b.mu.Unlock()
		return
	}
	b.interval = d
	wasRunning := b.stopTicker != nil
	b.mu.Unlock()
	if wasRunning {
		b.stop()
		b.start()
	}
}

// subscribe records sessionID. If the ticker is not yet running and
// there is a snapshot function, the ticker is started. Immediately
// delivers a snapshot (initial replay) so a new subscriber sees a
// payload without waiting one tick.
func (b *healthBroadcaster) subscribe(sessionID string) bool {
	if sessionID == "" || b.server == nil {
		return false
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return false
	}
	b.workers.Add(1)
	b.subscribers[sessionID] = true
	startTicker := b.stopTicker == nil && b.snapFn != nil
	snapFn := b.snapFn
	b.mu.Unlock()
	defer b.workers.Done()

	if startTicker {
		b.start()
	}

	if snapFn == nil {
		return false
	}
	snap := snapFn()
	if len(snap) == 0 {
		return false
	}
	out := copyPayload(snap)
	out["initial_replay"] = true
	out["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := b.server.SendNotificationToSpecificClient(sessionID, "notifications/daemon_health", out); err != nil {
		b.logger.Debug("daemon_health initial replay failed",
			zap.String("session", sessionID), zap.Error(err))
		return false
	}
	return true
}

// unsubscribe removes sessionID. Stops the ticker when the last
// subscriber leaves so a quiescent daemon doesn't keep a goroutine
// running for no listener.
func (b *healthBroadcaster) unsubscribe(sessionID string) {
	if sessionID == "" {
		return
	}
	b.mu.Lock()
	delete(b.subscribers, sessionID)
	stopTicker := len(b.subscribers) == 0 && b.stopTicker != nil
	b.mu.Unlock()
	if stopTicker {
		b.stop()
	}
}

func (b *healthBroadcaster) subscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}

// publishOnce takes one snapshot and fans it out (subject to delta
// filter). Called from the internal ticker and from tests.
func (b *healthBroadcaster) publishOnce() {
	if b == nil || b.server == nil {
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.workers.Add(1)
	snapFn := b.snapFn
	b.mu.Unlock()
	defer b.workers.Done()
	if snapFn == nil {
		return
	}
	snap := snapFn()
	if len(snap) == 0 {
		return
	}
	out := copyPayload(snap)
	out["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	hash := hashPayload(out)

	b.mu.Lock()
	if b.lastHash == hash {
		b.mu.Unlock()
		return
	}
	b.lastHash = hash
	b.lastSnap = out
	subs := make([]string, 0, len(b.subscribers))
	for id := range b.subscribers {
		subs = append(subs, id)
	}
	b.mu.Unlock()

	for _, sid := range subs {
		if err := b.server.SendNotificationToSpecificClient(sid, "notifications/daemon_health", out); err != nil {
			b.logger.Debug("send daemon_health failed",
				zap.String("session", sid), zap.Error(err))
		}
	}
}

// start kicks off the publish ticker. Safe to call once. Idempotent
// only via the subscribe / unsubscribe gating above — direct callers
// must check `stopTicker == nil` themselves.
func (b *healthBroadcaster) start() {
	b.mu.Lock()
	if b.closed || b.stopTicker != nil {
		b.mu.Unlock()
		return
	}
	stop, done := make(chan struct{}), make(chan struct{})
	b.stopTicker, b.tickerDone = stop, done
	interval := b.interval
	b.workers.Add(1) // admission and Add precede terminal Wait under the same gate
	b.mu.Unlock()
	go func() {
		defer b.workers.Done()
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				b.publishOnce()
			case <-stop:
				return
			}
		}
	}()
}

// stop halts the publish ticker. Waits for the in-flight goroutine to
// exit so subsequent start calls can't race the previous one. Safe
// to call when no ticker is running.
func (b *healthBroadcaster) stop() {
	b.mu.Lock()
	stop, done := b.stopTicker, b.tickerDone
	b.stopTicker, b.tickerDone = nil, nil
	if stop != nil {
		close(stop)
	}
	b.mu.Unlock()
	if done != nil {
		<-done
	}
}

// beginClose is terminal and nonjoining. Detached old ticker instances are
// already canceled by stop; workers retains their actual callback completion.
func (b *healthBroadcaster) beginClose() {
	b.mu.Lock()
	b.closed = true
	if b.stopTicker != nil {
		close(b.stopTicker)
	}
	b.stopTicker, b.tickerDone = nil, nil
	b.mu.Unlock()
}

func (b *healthBroadcaster) close() {
	b.beginClose()
	b.workers.Wait()
}

// snapshot returns the last published snapshot for inclusion in
// status surfaces. nil before any publish has happened.
func (b *healthBroadcaster) snapshot() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastSnap == nil {
		return nil
	}
	return copyPayload(b.lastSnap)
}

// AttachHealthSnapshot wires the daemon's health snapshot function
// into the broadcaster. Called from the daemon entrypoint after the
// controller / multi-indexer / LSP router are constructed so the
// snapshot can include their state.
//
// Replacing an existing function is supported — a daemon that
// transitions configuration (e.g. snapshot reload) can re-install the
// fn without restarting the broadcaster.
func (s *Server) AttachHealthSnapshot(fn HealthSnapshotFn) {
	if s == nil || s.healthBroadcaster == nil {
		return
	}
	s.healthBroadcaster.setSnapshotFn(fn)
}

// StopHealthBroadcaster halts the internal ticker. Called from the
// daemon shutdown path and terminally refuses new starts/replays. All
// admitted ticker and snapshot/delivery owners are joined. Idempotent.
func (s *Server) StopHealthBroadcaster() {
	if s == nil || s.healthBroadcaster == nil {
		return
	}
	s.healthBroadcaster.close()
}

// registerHealthTools wires the subscribe / unsubscribe MCP tools.
func (s *Server) registerHealthTools() {
	s.addTool(
		mcp.NewTool("subscribe_daemon_health",
			mcp.WithDescription("Opt the current MCP session into `notifications/daemon_health` push events. A periodic ticker (default 15 s) snapshots `{uptime_seconds, alloc_bytes, sys_bytes, num_goroutine, num_gc, tracked_repos, sessions, lsp_alive, …}` and pushes it to subscribed sessions. Repeated-identical snapshots are suppressed by a payload-hash delta filter. The current snapshot is replayed immediately as `initial_replay: true`. Optional `interval_ms` overrides the cadence (clamped to [1000, 300000]). Pair with `unsubscribe_daemon_health` to opt back out."),
			mcp.WithNumber("interval_ms", mcp.Description("Publish cadence in milliseconds. Default 15000; clamped to [1000, 300000].")),
		),
		s.handleSubscribeHealth,
	)
	s.addTool(
		mcp.NewTool("unsubscribe_daemon_health",
			mcp.WithDescription("Opt the current MCP session out of `notifications/daemon_health` push events. Idempotent. When the last subscriber unsubscribes, the internal ticker stops."),
		),
		s.handleUnsubscribeHealth,
	)
}

func (s *Server) handleSubscribeHealth(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.healthBroadcaster == nil {
		return mcp.NewToolResultError("daemon_health broadcaster is not configured"), nil
	}
	id := SessionIDFromContext(ctx)
	if id == "" {
		id = "embedded"
	}
	if ms := req.GetFloat("interval_ms", 0); ms > 0 {
		s.healthBroadcaster.setInterval(time.Duration(ms) * time.Millisecond)
	}
	replayed := s.healthBroadcaster.subscribe(id)
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"subscribed":  true,
		"session_id":  id,
		"subscribers": s.healthBroadcaster.subscriberCount(),
		"replayed":    replayed,
	})
}

func (s *Server) handleUnsubscribeHealth(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.healthBroadcaster == nil {
		return mcp.NewToolResultError("daemon_health broadcaster is not configured"), nil
	}
	id := SessionIDFromContext(ctx)
	if id == "" {
		id = "embedded"
	}
	s.healthBroadcaster.unsubscribe(id)
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"subscribed":  false,
		"session_id":  id,
		"subscribers": s.healthBroadcaster.subscriberCount(),
	})
}
