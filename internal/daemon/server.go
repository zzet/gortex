package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/platform"
)

// Server is the long-living Gortex daemon. It owns the Unix socket
// listener, the session registry, and the control-surface dispatcher.
// MCP traffic is plumbed through a ToolDispatcher that's injected at
// construction time — the daemon package deliberately doesn't depend
// on internal/mcp to keep the direction of imports clean.
type Server struct {
	SocketPath string
	Version    string
	Logger     *zap.Logger

	// Dispatcher handles MCP mode traffic (JSON-RPC 2.0) after handshake.
	// A nil Dispatcher means this daemon is control-only — useful for
	// tests and for early integration before the MCP passthrough lands.
	MCPDispatcher MCPDispatcher

	// MCPToolCallTimeout bounds individual tools/call dispatches. Zero uses DefaultMCPToolCallTimeout.
	MCPToolCallTimeout time.Duration

	// ControlTimeout overrides the budget applied to bounded control kinds
	// (see ControlTimeoutFor). Zero uses the per-kind default. Kinds that are
	// unbounded by policy — track / reload / enrich_* — stay unbounded
	// regardless: this tunes how long "should be quick" is allowed to take,
	// it does not put a clock on work the user deliberately started.
	ControlTimeout time.Duration

	// Controller handles control-mode RPCs (track/untrack/reload/status/shutdown).
	Controller Controller

	// Ready, when set, reports whether the daemon has finished warmup and the
	// current warmup phase. The result is surfaced on every successful
	// handshake ack (HandshakeAck.Warming / WarmupPhase) so a connecting proxy
	// or CLI knows the graph is still filling rather than guessing — a session
	// that connects mid-warmup keeps working and self-heals as the graph fills.
	// Optional: a nil probe means "assume ready" (control-only test servers).
	Ready func() (ready bool, phase string)

	// HTTPHandler, when non-nil, is mounted on a TCP listener at
	// HTTPAddr alongside the unix-socket dispatcher. This is how the
	// MCP 2026 Streamable HTTP transport reaches the daemon —
	// internal/mcp/streamable.Transport plugs in here. Nil disables
	// the HTTP face entirely; the unix-socket transport keeps
	// working unchanged. HTTPAddr accepts standard net.Listen
	// addresses; "127.0.0.1:7411" is the recommended default for a
	// single-user dev box.
	HTTPHandler http.Handler
	HTTPAddr    string

	sessions     *SessionRegistry
	listener     net.Listener
	httpListener net.Listener
	httpServer   *http.Server
	started      time.Time
	instanceID   string // unique to this daemon process; exposed in handshake acks

	shutdown chan struct{}
	doneOnce sync.Once
	conns    map[net.Conn]struct{}
	connsMu  sync.Mutex
}

// MCPDispatcher is implemented by whichever layer runs the MCP tool
// handlers. The daemon hands off one JSON-RPC frame at a time (raw bytes,
// newline-delimited) and the dispatcher returns the response bytes to
// write back. Session gives the dispatcher the per-client context it
// needs (scope, session-level state). Return an empty slice to suppress
// the response (notifications with no reply).
type MCPDispatcher interface {
	Dispatch(ctx context.Context, sess *Session, frame []byte) ([]byte, error)
}

// SessionEndedHook is an optional extension that MCPDispatcher
// implementations can satisfy to get a disconnect callback. The daemon
// invokes it in the per-connection goroutine's defer, giving
// implementations a chance to release per-session state (e.g., the
// `*mcp.Server.sessions` map entry) so idle memory doesn't grow with
// total session-count-ever.
//
// Implementations must be fast and non-blocking — this fires during
// connection teardown.
type SessionEndedHook interface {
	SessionEnded(sess *Session)
}

// SessionStartedHook is an optional extension that MCPDispatcher
// implementations can satisfy to receive a connect callback for each
// ModeMCP session, fired after the handshake ack and before the first
// frame is dispatched. write delivers one server-initiated JSON-RPC
// frame (without trailing newline) to the client; it is safe for
// concurrent use with the reply path — the daemon serialises all
// writes to the connection — and returns an error once the connection
// is gone. This is how server-initiated MCP notifications
// (tools/list_changed, graph_invalidated, ...) reach socket clients;
// the matching SessionEnded fires on teardown.
type SessionStartedHook interface {
	SessionStarted(sess *Session, write func([]byte) error)
}

// Controller implements the daemon's control surface. Separated from
// MCPDispatcher so the two can evolve independently and so control-only
// tests don't need a full MCP stack.
type Controller interface {
	Track(ctx context.Context, params TrackParams) (json.RawMessage, error)
	Untrack(ctx context.Context, params UntrackParams) (json.RawMessage, error)
	Reload(ctx context.Context) (json.RawMessage, error)
	// ReloadServers re-reads servers.toml and atomically swaps the
	// daemon's multi-server Router (building or tearing it down as the
	// roster requires), then invalidates the roster cache — applying
	// `gortex proxy on/off/add/remove` to a running daemon without a
	// restart. Distinct from Reload, which reconciles tracked repos.
	ReloadServers(ctx context.Context) (json.RawMessage, error)
	Status(ctx context.Context) (StatusResponse, error)
	// Probe answers liveness + tracked scope without the aggregation
	// Status performs, and — critically — without taking the controller
	// mutex, which a track / reload / enrichment holds for minutes. It is
	// the call a short-lived client on a sub-second budget should make;
	// see ControlProbe for the measurements that motivated it.
	Probe(ctx context.Context) (ProbeResponse, error)
	// SearchSymbols is the cheap probe path used by external clients
	// (Claude Code's Grep-redirect hook) that need a single short answer
	// without setting up a full MCP session.
	SearchSymbols(ctx context.Context, params SearchSymbolsParams) (SearchSymbolsResult, error)
	// EnrichChurn runs the per-symbol / per-file churn enricher against
	// the daemon's in-process graph. Exposed over the control surface so
	// CLI invocations (and the post-commit / post-merge git hook) can
	// trigger it without taking the on-disk store's write lock the daemon owns.
	EnrichChurn(ctx context.Context, params EnrichChurnParams) (EnrichChurnResult, error)
	// EnrichReleases runs the per-file release enricher against the
	// daemon's in-process graph. Same routing rationale as
	// EnrichChurn — keeps the on-disk store's write lock with the daemon.
	EnrichReleases(ctx context.Context, params EnrichReleasesParams) (EnrichReleasesResult, error)
	// EnrichBlame runs the git-blame authorship enricher against the
	// daemon's in-process graph. Same routing rationale as EnrichChurn.
	EnrichBlame(ctx context.Context, params EnrichBlameParams) (EnrichBlameResult, error)
	// EnrichCoverage projects pre-parsed Go cover-profile segments onto
	// the daemon's in-process graph. The CLI parses the profile so the
	// daemon never reads the caller's filesystem.
	EnrichCoverage(ctx context.Context, params EnrichCoverageParams) (EnrichCoverageResult, error)
	// EnrichCochange mines co-change edges against the daemon's
	// in-process graph. Same routing rationale as EnrichChurn.
	EnrichCochange(ctx context.Context, params EnrichCochangeParams) (EnrichCochangeResult, error)
	// Shutdown is invoked via the control surface and should return
	// quickly; the daemon's actual shutdown work happens after the
	// response is written.
	Shutdown(ctx context.Context) error
}

// New builds a Server but does not start listening.
func New(socketPath, version string, logger *zap.Logger) *Server {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Server{
		SocketPath: socketPath,
		Version:    version,
		Logger:     logger,
		instanceID: newSessionID(),
		sessions:   NewSessionRegistry(),
		shutdown:   make(chan struct{}),
		conns:      make(map[net.Conn]struct{}),
	}
}

// Listen creates the socket, writes the PID file, and installs the
// shutdown-signal handlers for graceful shutdown. The socket permissions
// are 0o600 on Unix — the daemon is user-local and nothing else on the
// machine should reach it; on Windows, %USERPROFILE% ACLs scope it to
// the user instead.
func (s *Server) Listen() error {
	if err := EnsureParentDir(s.SocketPath); err != nil {
		return fmt.Errorf("ensure socket dir: %w", err)
	}
	// Remove stale socket file from a crashed previous run. If the daemon
	// is actually running, the PID check below will catch it and abort.
	_ = os.Remove(s.SocketPath)

	if err := s.writePIDFile(); err != nil {
		return fmt.Errorf("pid file: %w", err)
	}

	lc := &net.ListenConfig{}
	l, err := lc.Listen(context.Background(), "unix", s.SocketPath)
	if err != nil {
		_ = os.Remove(PIDFilePath())
		return fmt.Errorf("listen: %w", err)
	}
	// chmod the socket to user-only on Unix. Windows has no POSIX mode
	// bits — the socket inherits the ACLs of %USERPROFILE%, which is
	// already user-scoped — so skip it there.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(s.SocketPath, 0o600); err != nil {
			_ = l.Close()
			return fmt.Errorf("chmod socket: %w", err)
		}
	}
	s.listener = l
	s.started = time.Now()

	// Optional HTTP listener for the MCP 2026 Streamable transport.
	// We bring it up alongside the unix-socket listener so both
	// transports share the same shutdown / lifecycle plumbing. A
	// listen failure here is fatal — running the unix-socket
	// transport silently while HTTP is down would mask the operator
	// misconfiguration that pointed clients at a port that never
	// answered.
	if s.HTTPHandler != nil && s.HTTPAddr != "" {
		httpLn, herr := net.Listen("tcp", s.HTTPAddr)
		if herr != nil {
			_ = l.Close()
			_ = os.Remove(PIDFilePath())
			return fmt.Errorf("listen http: %w", herr)
		}
		s.httpListener = httpLn
		s.httpServer = &http.Server{
			Handler:           s.HTTPHandler,
			ReadHeaderTimeout: 10 * time.Second,
		}
	}

	// Install signal handlers once the listener is live.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, platform.ShutdownSignals()...)
	go func() {
		<-sigCh
		s.Logger.Info("daemon: received signal, shutting down")
		_ = s.Shutdown()
	}()
	return nil
}

// Serve runs the accept loop. Blocks until Shutdown is called or the
// listener returns an unrecoverable error. When an HTTP listener was
// brought up by Listen it runs concurrently in its own goroutine; an
// HTTP-side failure pushes onto the same shutdown channel so the
// unix-socket loop tears down too.
func (s *Server) Serve() error {
	if s.listener == nil {
		return errors.New("daemon: Listen must be called before Serve")
	}
	if s.httpListener != nil && s.httpServer != nil {
		go func() {
			if err := s.httpServer.Serve(s.httpListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.Logger.Warn("daemon: http serve exited", zap.Error(err))
			}
		}()
		s.Logger.Info("daemon: http listener active",
			zap.String("addr", s.httpListener.Addr().String()))
	}
	s.Logger.Info("daemon: serving", zap.String("socket", s.SocketPath))
	// Background hygiene: reap sessions whose client process died without a
	// clean disconnect, and (opt-in) auto-exit after an idle window.
	go s.runMaintenance()
	var emfileBackoff time.Duration
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			// listener closed during Shutdown — normal exit.
			select {
			case <-s.shutdown:
				return nil
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			// EMFILE means the process is out of file descriptors.
			// Without backoff the loop spins, pinning a CPU and making
			// the FD pressure even worse. The exponential ramp gives
			// in-flight handlers time to release descriptors.
			if isEMFILE(err) {
				if emfileBackoff == 0 {
					emfileBackoff = 5 * time.Millisecond
				} else if emfileBackoff < time.Second {
					emfileBackoff *= 2
				}
				s.Logger.Warn("daemon: accept failed, FD-starved — backing off",
					zap.Error(err), zap.Duration("sleep", emfileBackoff))
				select {
				case <-time.After(emfileBackoff):
				case <-s.shutdown:
					return nil
				}
				continue
			}
			emfileBackoff = 0
			s.Logger.Warn("daemon: accept failed", zap.Error(err))
			continue
		}
		emfileBackoff = 0
		s.trackConn(conn)
		go s.handle(conn)
	}
}

// deadPeerSweepInterval is how often runMaintenance reaps dead-peer sessions.
// A var so tests can shorten it.
var deadPeerSweepInterval = 30 * time.Second

// runMaintenance is the daemon's background hygiene loop: every
// deadPeerSweepInterval it sweeps sessions whose originating client process has
// died (platform.ProcessAlive), and — when GORTEX_DAEMON_IDLE_TIMEOUT is set —
// it shuts the daemon down after that long with no live sessions. Exits on the
// shutdown signal.
func (s *Server) runMaintenance() {
	idle := IdleTimeoutFromEnv()
	tick := deadPeerSweepInterval
	if idle > 0 && idle/4 < tick {
		tick = idle / 4 // sample often enough to honour a short idle window
	}
	if tick <= 0 {
		tick = deadPeerSweepInterval
	}
	t := time.NewTicker(tick)
	defer t.Stop()

	var idleSince time.Time
	for {
		select {
		case <-s.shutdown:
			return
		case <-t.C:
			for _, sd := range s.sessions.SweepDead(platform.ProcessAlive) {
				if hook, ok := s.MCPDispatcher.(SessionEndedHook); ok && hook != nil {
					hook.SessionEnded(sd)
				}
				s.Logger.Info("daemon: swept dead session",
					zap.String("session_id", sd.ID), zap.Int("client_pid", sd.ClientPID))
				if sd.Conn != nil {
					s.untrackConn(sd.Conn)
				}
			}
			if idle <= 0 {
				continue
			}
			if s.sessions.Count() > 0 {
				idleSince = time.Time{}
				continue
			}
			if idleSince.IsZero() {
				idleSince = time.Now()
				continue
			}
			if time.Since(idleSince) >= idle {
				s.Logger.Info("daemon: idle timeout reached, shutting down",
					zap.Duration("idle_timeout", idle))
				_ = s.Shutdown()
				return
			}
		}
	}
}

// IdleTimeoutFromEnv reads the opt-in GORTEX_DAEMON_IDLE_TIMEOUT — a Go
// duration (e.g. "30m", "2h"). Returns 0 (disabled) when unset, empty, or
// unparseable, so the daemon only ever auto-exits when the user asked it to.
func IdleTimeoutFromEnv() time.Duration {
	return parseIdleTimeout(os.Getenv("GORTEX_DAEMON_IDLE_TIMEOUT"))
}

func parseIdleTimeout(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// handle runs the per-connection lifecycle: handshake → dispatch loop →
// cleanup. Every exit path must remove the session and close the conn.
func (s *Server) handle(conn net.Conn) {
	defer func() {
		_ = conn.Close()
		s.untrackConn(conn)
		if sess := s.sessions.Remove(conn); sess != nil {
			// Fire the optional disconnect hook so implementations can
			// release per-session resources keyed by this ID.
			if hook, ok := s.MCPDispatcher.(SessionEndedHook); ok && hook != nil {
				hook.SessionEnded(sess)
			}
			s.Logger.Debug("daemon: session closed",
				zap.String("session_id", sess.ID),
				zap.String("client", sess.ClientName))
		}
	}()

	reader := bufio.NewReader(conn)
	sess, err := s.handshake(conn, reader)
	if err != nil {
		if errors.Is(err, io.EOF) {
			// Liveness probe (daemon.IsRunningAt and friends): the peer
			// dialed the socket and closed it without sending a handshake
			// frame, so the first read returns a clean EOF. This is an
			// expected "is the socket accepting?" knock, not a fault —
			// keep it at Debug. A partially-written frame yields
			// io.ErrUnexpectedEOF instead and still warns below.
			s.Logger.Debug("daemon: connection closed before handshake", zap.Error(err))
		} else {
			s.Logger.Warn("daemon: handshake failed", zap.Error(err))
		}
		return
	}

	switch sess.Mode {
	case ModeMCP:
		s.serveMCP(conn, reader, sess)
	case ModeControl:
		s.serveControl(conn, reader, sess)
	default:
		s.Logger.Warn("daemon: unknown mode after handshake",
			zap.String("mode", string(sess.Mode)))
	}
}

// handshake reads one handshake frame, validates it, and replies with an
// ack. A rejected handshake writes an error ack then closes the connection.
func (s *Server) handshake(conn net.Conn, reader *bufio.Reader) (*Session, error) {
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read handshake: %w", err)
	}

	var h Handshake
	if err := json.Unmarshal(line, &h); err != nil {
		_ = WriteJSONLine(conn, HandshakeAck{
			ErrorCode: ErrInternal,
			ErrorMsg:  "invalid handshake json: " + err.Error(),
		})
		return nil, fmt.Errorf("parse handshake: %w", err)
	}
	if h.Version != ProtocolVersion {
		_ = WriteJSONLine(conn, HandshakeAck{
			ErrorCode: ErrProtocolMismatch,
			ErrorMsg: fmt.Sprintf("daemon expects protocol %d, client sent %d",
				ProtocolVersion, h.Version),
		})
		return nil, fmt.Errorf("protocol mismatch: %d vs %d", ProtocolVersion, h.Version)
	}
	if h.Mode != ModeMCP && h.Mode != ModeControl {
		_ = WriteJSONLine(conn, HandshakeAck{
			ErrorCode: ErrUnsupportedMode,
			ErrorMsg:  "mode must be 'mcp' or 'control'",
		})
		return nil, fmt.Errorf("unsupported mode: %q", h.Mode)
	}

	sess := s.sessions.Register(conn, h)

	ack := HandshakeAck{
		OK:             true,
		SessionID:      sess.ID,
		DaemonVersion:  s.Version,
		DaemonInstance: s.instanceID,
	}
	// Stamp warmup state so the client can tell a still-warming daemon from a
	// ready one. The session is established either way — Warming is advisory.
	if s.Ready != nil {
		ready, phase := s.Ready()
		ack.Warming = !ready
		ack.WarmupPhase = phase
	}
	if err := WriteJSONLine(conn, ack); err != nil {
		_ = s.sessions.Remove(conn)
		return nil, fmt.Errorf("write ack: %w", err)
	}
	s.Logger.Debug("daemon: session established",
		zap.String("session_id", sess.ID),
		zap.String("mode", string(sess.Mode)),
		zap.String("cwd", sess.CWD),
		zap.String("client", sess.ClientName))
	return sess, nil
}

// serveMCP pumps MCP JSON-RPC frames. Each line on the wire is a single
// message. The Dispatcher gets the raw frame + session context and
// returns the raw reply to write back. Nil reply = no response (the
// client sent a notification).
func (s *Server) serveMCP(conn net.Conn, reader *bufio.Reader, sess *Session) {
	if s.MCPDispatcher == nil {
		_ = WriteJSONLine(conn, map[string]any{
			"jsonrpc": "2.0",
			"error": map[string]any{
				"code":    -32000,
				"message": "daemon started without MCP dispatcher; control-only mode",
			},
			"id": nil,
		})
		return
	}

	hooks := &mcpConnectionHooks{
		onReady: func(write func([]byte) error) {
			if hook, ok := s.MCPDispatcher.(SessionStartedHook); ok && hook != nil {
				hook.SessionStarted(sess, write)
			}
		},
	}
	serveMCPConnectionWithHooks(conn, reader, s.mcpToolCallTimeout(), func(ctx context.Context, line []byte) ([]byte, error) {
		reply, _, err := sess.dispatchMCPOnceContext(ctx, line, func() ([]byte, error) {
			reply, err := s.MCPDispatcher.Dispatch(ctx, sess, line)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return reply, err
		})
		if err != nil && s.Logger != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				outcome := "cancelled"
				if errors.Is(ctxErr, context.DeadlineExceeded) {
					outcome = "deadline"
				}
				s.Logger.Warn("daemon: mcp request lifetime ended",
					zap.String("session_id", sess.ID),
					zap.String("outcome", outcome),
					zap.Duration("deadline", s.mcpToolCallTimeout()),
					zap.Error(ctxErr))
			} else {
				s.Logger.Warn("daemon: dispatch error",
					zap.String("session_id", sess.ID), zap.Error(err))
			}
		}
		return reply, err
	}, hooks)
}

// serveControl drains ControlRequest messages, invokes the Controller,
// and writes paired ControlResponse messages.
func (s *Server) serveControl(conn net.Conn, reader *bufio.Reader, sess *Session) {
	if s.Controller == nil {
		_ = WriteJSONLine(conn, ControlResponse{
			ErrorCode: ErrInternal,
			ErrorMsg:  "daemon started without controller",
		})
		return
	}
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var req ControlRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = WriteJSONLine(conn, ControlResponse{
				ErrorCode: ErrInternal,
				ErrorMsg:  "malformed request: " + err.Error(),
			})
			continue
		}
		resp := s.handleControlBounded(sess, req)
		writeErr := WriteJSONLine(conn, resp)
		if req.Kind == ControlShutdown && resp.OK {
			// Scheduled regardless of whether the ack reached the client. The
			// controller flushes and closes the store before answering, so a
			// client that gave up waiting (or died) has already left by the
			// time we get here — and skipping the teardown on a failed write
			// would leave a daemon that flushed its store and then kept
			// running, holding the on-disk lock the next start needs.
			//
			// The short delay gives a client that IS still listening a moment
			// to read the ack before the listener goes away.
			go func() {
				time.Sleep(100 * time.Millisecond)
				_ = s.Shutdown()
			}()
			return
		}
		if writeErr != nil {
			return
		}
	}
}

// handleControlBounded runs one control request under the kind's budget (see
// ControlTimeoutFor). Handlers that block past it — the common case being a
// controller mutex held for the length of a track / reload / enrichment — are
// abandoned rather than waited on, so the caller gets a terminal ErrTimeout
// response instead of an open-ended silence. The abandoned handler keeps
// running and completes normally; only this connection's turn is given back.
//
// Unbounded kinds (track / reload / enrich_*) run inline, exactly as before:
// they are long by design and a caller that starts one is waiting on purpose.
func (s *Server) handleControlBounded(sess *Session, req ControlRequest) ControlResponse {
	budget := ControlTimeoutFor(req.Kind)
	if budget > 0 && s.ControlTimeout > 0 {
		budget = s.ControlTimeout
	}
	if budget <= 0 {
		return s.handleControl(context.Background(), sess, req)
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	done := make(chan ControlResponse, 1)
	go func() { done <- s.handleControl(ctx, sess, req) }()

	select {
	case resp := <-done:
		return resp
	case <-ctx.Done():
		s.Logger.Warn("daemon: control request exceeded its budget",
			zap.String("kind", req.Kind), zap.Duration("budget", budget))
		return controlErr(ErrTimeout, fmt.Sprintf(
			"control request %q did not complete within %s; the daemon is busy (a track / reload / enrichment may be holding the controller) — retry, or run `gortex daemon status` for progress",
			req.Kind, budget))
	}
}

func (s *Server) handleControl(ctx context.Context, _ *Session, req ControlRequest) ControlResponse {
	switch req.Kind {
	case ControlTrack:
		var p TrackParams
		if err := unmarshalParams(req.Params, &p); err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		result, err := s.Controller.Track(ctx, p)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		return ControlResponse{OK: true, Result: result}

	case ControlUntrack:
		var p UntrackParams
		if err := unmarshalParams(req.Params, &p); err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		result, err := s.Controller.Untrack(ctx, p)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		return ControlResponse{OK: true, Result: result}

	case ControlProxy:
		result, err := s.Controller.ReloadServers(ctx)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		return ControlResponse{OK: true, Result: result}

	case ControlReload:
		result, err := s.Controller.Reload(ctx)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		return ControlResponse{OK: true, Result: result}

	case ControlProbe:
		pr, err := s.Controller.Probe(ctx)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		// Daemon-level fields the controller cannot see, matching Status.
		pr.Version = s.Version
		pr.PID = os.Getpid()
		pr.UptimeSeconds = int64(time.Since(s.started).Seconds())
		pr.Sessions = s.sessions.Count()
		buf, _ := json.Marshal(pr)
		return ControlResponse{OK: true, Result: buf}

	case ControlStatus:
		st, err := s.Controller.Status(ctx)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		// Daemon-level fields the controller doesn't know about.
		st.Version = s.Version
		st.PID = os.Getpid()
		st.UptimeSeconds = int64(time.Since(s.started).Seconds())
		st.SocketPath = s.SocketPath
		st.Sessions = s.sessions.Count()
		// Per-session detail (cwd, client name, connect time) for the
		// status command's "sessions" block. The controller can't see
		// these — sessions live on the daemon server, not the
		// MultiIndexer — so we attach them here. Sorted newest-first
		// so the list reads as "what's connected right now".
		if all := s.sessions.All(); len(all) > 0 {
			now := time.Now()
			rows := make([]MCPSessionStatus, 0, len(all))
			for _, sess := range all {
				if sess == nil {
					continue
				}
				name, version := sess.SnapshotClientInfo()
				row := MCPSessionStatus{
					ID:            sess.ID,
					Cwd:           sess.CWD,
					ClientName:    name,
					ClientVersion: version,
				}
				if !sess.StartedAt.IsZero() {
					row.ConnectedSecs = int64(now.Sub(sess.StartedAt).Seconds())
				}
				rows = append(rows, row)
			}
			st.MCPSessions = rows
		}
		buf, _ := json.Marshal(st)
		return ControlResponse{OK: true, Result: buf}

	case ControlSearchSymbols:
		var p SearchSymbolsParams
		if err := unmarshalParams(req.Params, &p); err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		result, err := s.Controller.SearchSymbols(ctx, p)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		buf, err := json.Marshal(result)
		if err != nil {
			return controlErr(ErrInternal, "marshal search result: "+err.Error())
		}
		return ControlResponse{OK: true, Result: buf}

	case ControlShutdown:
		if err := s.Controller.Shutdown(ctx); err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		return ControlResponse{OK: true}

	case ControlEnrichChurn:
		var p EnrichChurnParams
		if err := unmarshalParams(req.Params, &p); err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		result, err := s.Controller.EnrichChurn(ctx, p)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		buf, err := json.Marshal(result)
		if err != nil {
			return controlErr(ErrInternal, "marshal enrich_churn result: "+err.Error())
		}
		return ControlResponse{OK: true, Result: buf}

	case ControlEnrichReleases:
		var p EnrichReleasesParams
		if err := unmarshalParams(req.Params, &p); err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		result, err := s.Controller.EnrichReleases(ctx, p)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		buf, err := json.Marshal(result)
		if err != nil {
			return controlErr(ErrInternal, "marshal enrich_releases result: "+err.Error())
		}
		return ControlResponse{OK: true, Result: buf}

	case ControlEnrichBlame:
		var p EnrichBlameParams
		if err := unmarshalParams(req.Params, &p); err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		result, err := s.Controller.EnrichBlame(ctx, p)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		buf, err := json.Marshal(result)
		if err != nil {
			return controlErr(ErrInternal, "marshal enrich_blame result: "+err.Error())
		}
		return ControlResponse{OK: true, Result: buf}

	case ControlEnrichCoverage:
		var p EnrichCoverageParams
		if err := unmarshalParams(req.Params, &p); err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		result, err := s.Controller.EnrichCoverage(ctx, p)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		buf, err := json.Marshal(result)
		if err != nil {
			return controlErr(ErrInternal, "marshal enrich_coverage result: "+err.Error())
		}
		return ControlResponse{OK: true, Result: buf}

	case ControlEnrichCochange:
		var p EnrichCochangeParams
		if err := unmarshalParams(req.Params, &p); err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		result, err := s.Controller.EnrichCochange(ctx, p)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		buf, err := json.Marshal(result)
		if err != nil {
			return controlErr(ErrInternal, "marshal enrich_cochange result: "+err.Error())
		}
		return ControlResponse{OK: true, Result: buf}
	}
	return controlErr(ErrInternal, "unknown control kind: "+req.Kind)
}

// Shutdown stops the accept loop, closes outstanding connections, and
// removes the socket and PID files. Safe to call multiple times.
func (s *Server) Shutdown() error {
	var first error
	s.doneOnce.Do(func() {
		close(s.shutdown)
		if s.listener != nil {
			first = s.listener.Close()
		}
		// Tear down the HTTP listener with a short grace window so
		// in-flight Streamable responses can finish flushing. We
		// don't propagate the http error unless the unix-socket
		// listener succeeded — the operator already sees a
		// unix-socket close error in the same path.
		if s.httpServer != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if herr := s.httpServer.Shutdown(ctx); herr != nil && first == nil {
				first = herr
			}
			cancel()
		}
		// Close all live conns so per-conn goroutines exit their read loops.
		s.connsMu.Lock()
		for c := range s.conns {
			_ = c.Close()
		}
		s.connsMu.Unlock()
		_ = os.Remove(s.SocketPath)
		_ = os.Remove(PIDFilePath())
	})
	return first
}

// writePIDFile fails if a live daemon is already running, so starting
// twice is a loud "already running" error rather than a silent overwrite.
func (s *Server) writePIDFile() error {
	path := PIDFilePath()
	if err := EnsureParentDir(path); err != nil {
		return err
	}
	if existing, err := os.ReadFile(path); err == nil {
		if pid, _ := strconv.Atoi(string(existing)); pid > 0 {
			if platform.ProcessAlive(pid) {
				return fmt.Errorf("daemon already running (pid %d)", pid)
			}
			// Stale pid file — old daemon crashed without cleanup.
			_ = os.Remove(path)
		}
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
}

// RunningPID reports the PID of a live daemon recorded in the PID file, or
// (0, false) when none is. Unlike IsRunning — which only probes the control
// socket — this still reports a daemon that is *mid-shutdown*: the
// ControlShutdown handler tears the listener down ~100ms after acking, but
// the process stays alive while it flushes and closes the store, and it
// holds the store's on-disk lock until it exits. That window is exactly what
// turned a quick restart into a "failed to open database" lock conflict, so
// callers that must not start a second daemon over the top of a dying one —
// or that need to wait for it to exit — consult this, not the socket.
//
// A PID file whose process is dead is stale (the owner crashed without
// cleanup) and reported as not-running, mirroring writePIDFile's own
// staleness handling.
func RunningPID() (int, bool) {
	b, err := os.ReadFile(PIDFilePath())
	if err != nil {
		return 0, false
	}
	// TrimSpace so a PID file written with a trailing newline — by a shell
	// `echo`, a process manager, or a hand edit — still parses. The daemon
	// writes it without one, but tolerating both is free and the silent
	// failure mode (guard never fires, restart races the lock again) is
	// exactly the bug this helper exists to prevent.
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if !platform.ProcessAlive(pid) {
		return 0, false
	}
	return pid, true
}

func (s *Server) trackConn(c net.Conn) {
	s.connsMu.Lock()
	s.conns[c] = struct{}{}
	s.connsMu.Unlock()
}

func (s *Server) untrackConn(c net.Conn) {
	s.connsMu.Lock()
	delete(s.conns, c)
	s.connsMu.Unlock()
}

// Sessions exposes the registry for inspection (status command, tests).
func (s *Server) Sessions() *SessionRegistry { return s.sessions }

// StartedAt returns the time Listen() completed — used for uptime math.
func (s *Server) StartedAt() time.Time { return s.started }

// unmarshalParams decodes RawMessage into a typed struct, treating empty
// or null params as an empty struct (zero value) so callers don't need
// to special-case missing params.
func unmarshalParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, v)
}

func controlErr(code, msg string) ControlResponse {
	return ControlResponse{ErrorCode: code, ErrorMsg: msg}
}
