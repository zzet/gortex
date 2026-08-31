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

	// Binary-drift detection: size+mtime of the daemon's own executable
	// captured at construction, so each status request can cheaply tell
	// whether the on-disk image was replaced (brew upgrade, cp over the
	// binary) while this process keeps running the old code. An empty
	// binaryPath means the identity was never captured and status reports
	// the binary state as unknown. binaryStatFn is swappable for tests.
	binaryMu          sync.Mutex
	binaryPath        string
	binaryStartSize   int64
	binaryStartMod    int64
	binaryStatFn      func(path string) (int64, int64, error) // size, mtime unix seconds, err
	binaryLoggedStale bool

	shutdown chan struct{}
	doneOnce sync.Once
	conns    map[net.Conn]struct{}
	connsMu  sync.Mutex

	// processStateOwned is set only after this server successfully publishes
	// its PID file. Shutdown closes transport, but deliberately leaves the PID
	// and runtime records in place: they are the cross-process exclusion guard
	// while the outer daemon owner drains graph producers and closes SQLite.
	// ReleaseProcessState removes them only after that teardown has finished.
	processStateMu    sync.Mutex
	processStateOwned bool
	processStateOnce  sync.Once

	// handlerMu fences positive WaitGroup admission against Serve's terminal
	// wait. Shutdown closes admission before listeners and connections, so the
	// graph owner can safely tear down only after Serve returns.
	handlerMu      sync.Mutex
	handlers       sync.WaitGroup
	handlerClosing bool
	// maintenanceInterval is captured per server so focused tests can shorten
	// it without racing maintenance loops owned by other server fixtures.
	maintenanceInterval time.Duration
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
	s := &Server{
		SocketPath:          socketPath,
		Version:             version,
		Logger:              logger,
		instanceID:          newSessionID(),
		sessions:            NewSessionRegistry(),
		shutdown:            make(chan struct{}),
		maintenanceInterval: deadPeerSweepInterval,
		conns:               make(map[net.Conn]struct{}),
		binaryStatFn:        osStatIdentity,
	}
	// Capture the running image's identity so status requests can detect
	// a later on-disk replace. Best-effort: a failure here leaves
	// binaryPath empty and status reports the binary state as unknown.
	if exe, err := os.Executable(); err == nil {
		s.captureBinaryIdentity(exe)
	}
	return s
}

// osStatIdentity is the production binary-identity probe: os.Stat reduced
// to the (size, mtime) pair drift detection compares. A named func rather
// than an inline closure so Server.binaryStatFn has a swappable default
// and tests can substitute a fake.
func osStatIdentity(path string) (int64, int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	return fi.Size(), fi.ModTime().Unix(), nil
}

// captureBinaryIdentity stats path through binaryStatFn and records the
// result as the start-of-life identity. Any error leaves the identity
// uncaptured (binaryPath empty) — status then reports the binary state as
// unknown rather than guessing fresh.
func (s *Server) captureBinaryIdentity(path string) {
	if s.binaryStatFn == nil {
		s.binaryStatFn = osStatIdentity
	}
	size, mod, err := s.binaryStatFn(path)
	if err != nil {
		return
	}
	s.binaryMu.Lock()
	s.binaryPath = path
	s.binaryStartSize = size
	s.binaryStartMod = mod
	s.binaryMu.Unlock()
}

// populateBinaryStatus stamps the binary-drift fields onto a status
// response: BinaryChecked reports whether the probe ran, BinaryStale
// whether the on-disk image no longer matches the one this process
// started from. The first stale detection logs the restart hint once —
// every subsequent status stays quiet, so a monitoring loop polling status
// cannot spam the daemon log. Stat failures (and a never-captured
// identity) leave both flags false: unknown, not fresh.
func (s *Server) populateBinaryStatus(st *StatusResponse) {
	s.binaryMu.Lock()
	defer s.binaryMu.Unlock()
	if s.binaryPath == "" || s.binaryStatFn == nil {
		return
	}
	size, mod, err := s.binaryStatFn(s.binaryPath)
	if err != nil {
		return
	}
	st.BinaryChecked = true
	if size != s.binaryStartSize || mod != s.binaryStartMod {
		st.BinaryStale = true
		st.BinaryReplacedAtUnix = mod
		if !s.binaryLoggedStale {
			s.binaryLoggedStale = true
			s.Logger.Warn(fmt.Sprintf(
				"daemon: on-disk binary changed since start (%s) — run 'gortex daemon restart' to upgrade",
				s.binaryPath))
		}
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
		s.releasePIDFileAfterListenFailure()
		return fmt.Errorf("listen: %w", err)
	}
	// chmod the socket to user-only on Unix. Windows has no POSIX mode
	// bits — the socket inherits the ACLs of %USERPROFILE%, which is
	// already user-scoped — so skip it there.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(s.SocketPath, 0o600); err != nil {
			_ = l.Close()
			_ = os.Remove(s.SocketPath)
			s.releasePIDFileAfterListenFailure()
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
			s.releasePIDFileAfterListenFailure()
			return fmt.Errorf("listen http: %w", herr)
		}
		s.httpListener = httpLn
		s.httpServer = &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !s.beginHandler() {
					http.Error(w, "daemon is shutting down", http.StatusServiceUnavailable)
					return
				}
				defer s.handlers.Done()
				s.HTTPHandler.ServeHTTP(w, r)
			}),
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
	defer func() {
		_ = s.Shutdown()
		s.handlers.Wait()
	}()
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
	if s.beginHandler() {
		go func() {
			defer s.handlers.Done()
			s.runMaintenance()
		}()
	}
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
		if !s.beginHandler() {
			_ = conn.Close()
			s.untrackConn(conn)
			continue
		}
		go func() {
			defer s.handlers.Done()
			s.handle(conn)
		}()
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
	tick := s.maintenanceInterval
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
		if !s.beginHandler() {
			return nil, errors.New("daemon is shutting down")
		}
		defer s.handlers.Done()
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
			// teardown must happen even when a client gives up waiting (or dies).
			// Transport closes here; Serve joins admitted handlers and returns to
			// the owner, which then closes the graph stack. Never close the store
			// inside Controller.Shutdown while request handlers are still live.
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
		if !s.beginHandler() {
			return controlErr(ErrInternal, "daemon is shutting down")
		}
		defer s.handlers.Done()
		return s.handleControl(context.Background(), sess, req)
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	done := make(chan ControlResponse, 1)
	if !s.beginHandler() {
		return controlErr(ErrInternal, "daemon is shutting down")
	}
	go func() {
		defer s.handlers.Done()
		done <- s.handleControl(ctx, sess, req)
	}()

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
		var p ProbeParams
		if err := unmarshalParams(req.Params, &p); err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		pr, err := s.Controller.Probe(ctx)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		if p.Path != "" {
			if readiness, ok := s.Controller.(TrackReadinessController); ok {
				track, trackErr := readiness.TrackReadiness(ctx, p.Path)
				if trackErr != nil {
					return controlErr(ErrInternal, trackErr.Error())
				}
				pr.Track = &track
			}
		}
		// Daemon-level fields the controller cannot see, matching Status.
		pr.Version = s.Version
		pr.PID = os.Getpid()
		pr.UptimeSeconds = int64(time.Since(s.started).Seconds())
		pr.Sessions = s.sessions.Count()
		buf, _ := json.Marshal(pr)
		return ControlResponse{OK: true, Result: buf}

	case ControlStatus:
		var sp StatusParams
		if len(req.Params) > 0 {
			// A caller that sends no params, or an older client that sends
			// something else entirely, gets the routine counter-backed poll.
			_ = json.Unmarshal(req.Params, &sp)
		}
		statusFn := s.Controller.Status
		if sp.Exact {
			// Optional: a controller whose backend cannot recount simply does
			// not implement this, and --exact degrades to the ordinary answer
			// rather than failing.
			if exact, ok := s.Controller.(StatusExactController); ok {
				statusFn = exact.StatusExact
			}
		}
		st, err := statusFn(ctx)
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
		// Binary-drift self-report: did the on-disk daemon image get
		// replaced under this running process? See populateBinaryStatus.
		s.populateBinaryStatus(&st)
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

	case ControlFileCoverage:
		coverage, ok := s.Controller.(FileCoverageController)
		if !ok {
			return controlErr(ErrInternal, "this daemon cannot resolve a path to the view that serves it")
		}
		var p FileCoverageParams
		if err := unmarshalParams(req.Params, &p); err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		result, err := coverage.FileCoverage(ctx, p)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		buf, err := json.Marshal(result)
		if err != nil {
			return controlErr(ErrInternal, "marshal file_coverage result: "+err.Error())
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

// Shutdown stops transport admission, closes outstanding connections, and
// removes the socket. Safe to call multiple times.
//
// It intentionally retains the PID and runtime files. The process can still
// own graph workers and an open SQLite store after Serve returns; the outer
// daemon owner must call ReleaseProcessState only after those resources have
// drained. Keeping the PID visible closes the restart race in that interval.
func (s *Server) Shutdown() error {
	var first error
	s.doneOnce.Do(func() {
		s.closeHandlerAdmission()
		close(s.shutdown)
		if s.listener != nil {
			first = s.listener.Close()
		}
		// Close Unix sessions before the HTTP grace wait. Otherwise an
		// established control/MCP connection can admit more work throughout
		// that two-second window even though listener admission is closed.
		s.connsMu.Lock()
		for c := range s.conns {
			_ = c.Close()
		}
		s.connsMu.Unlock()
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
		_ = os.Remove(s.SocketPath)
	})
	return first
}

// ReleaseProcessState relinquishes this daemon's cross-process ownership
// marker after graph teardown has completed. It is separate from Shutdown so
// closing the transport cannot let another start race a store that is still
// open. Servers that successfully called Listen must call this at the end of
// their owner-level teardown; repeated calls are harmless.
func (s *Server) ReleaseProcessState() {
	s.processStateOnce.Do(func() {
		s.processStateMu.Lock()
		owned := s.processStateOwned
		s.processStateOwned = false
		s.processStateMu.Unlock()
		if !owned {
			return
		}
		// The runtime record describes THIS daemon's resolved choices, so it
		// shares the PID file's lifetime. Remove it first and the PID guard
		// last: once the PID disappears a successor may start and publish its
		// own runtime state, which this process must never delete.
		RemoveRuntimeState()
		_ = os.Remove(PIDFilePath())
	})
}

// releasePIDFileAfterListenFailure relinquishes the PID without deleting the
// startup runtime record. Detached starters use that record to report why the
// listener failed.
func (s *Server) releasePIDFileAfterListenFailure() {
	s.processStateMu.Lock()
	owned := s.processStateOwned
	s.processStateOwned = false
	s.processStateMu.Unlock()
	if owned {
		_ = os.Remove(PIDFilePath())
	}
}

func (s *Server) beginHandler() bool {
	s.handlerMu.Lock()
	defer s.handlerMu.Unlock()
	if s.handlerClosing {
		return false
	}
	s.handlers.Add(1)
	return true
}

func (s *Server) closeHandlerAdmission() {
	s.handlerMu.Lock()
	s.handlerClosing = true
	s.handlerMu.Unlock()
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
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return err
	}
	s.processStateMu.Lock()
	s.processStateOwned = true
	s.processStateMu.Unlock()
	return nil
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

// TrackReadinessController is the optional path-scoped extension of Probe.
// The ordinary Probe remains lock-free and store-free; only callers that name
// a path pay for the bounded catalog/materialization checks needed to prove an
// exact routed view is queryable.
type TrackReadinessController interface {
	TrackReadiness(ctx context.Context, path string) (TrackReadiness, error)
}

// FileCoverageController is the opt-in view-scoped coverage answer behind
// ControlFileCoverage. A controller implements it when it can resolve a
// filesystem path to the graph that serves it; one that cannot leaves the
// kind unanswered, and the caller degrades exactly as it does for a daemon
// that is not running at all.
type FileCoverageController interface {
	FileCoverage(ctx context.Context, params FileCoverageParams) (FileCoverageResult, error)
}

// StatusExactController is the opt-in audit half of ControlStatus. A
// controller implements it when its backend can recount the corpus; the
// routine Status path reads maintained counters and never scans, so this is
// the only way a user can ask "are those counters telling the truth".
//
// Implementations are expected to heal what they find: the point of paying
// for a full scan is that the next cheap poll is correct again.
type StatusExactController interface {
	StatusExact(ctx context.Context) (StatusResponse, error)
}
