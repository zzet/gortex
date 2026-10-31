package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// Client is one end of a daemon connection. Used by:
//   - `gortex mcp --proxy` — stdio MCP → daemon relay.
//   - `gortex daemon status / reload / track / untrack / stop` — control RPC.
//
// A Client is not safe for concurrent use — each caller owns its own.
type Client struct {
	Conn    net.Conn
	reader  *bufio.Reader
	Ack     HandshakeAck
	PeerPID int // kernel socket peer identity; zero when unavailable
}

// Dial connects to the daemon on the default socket path and completes a
// handshake. Returns (nil, err) if the socket doesn't exist, the daemon
// isn't reachable, or the handshake is rejected.
//
// Callers in a fallback path (proxy mode, CLI commands that can work
// standalone) should treat ErrDaemonUnavailable distinctly from other
// errors and continue as if the daemon isn't there.
func Dial(h Handshake) (*Client, error) {
	return DialTo(SocketPath(), h)
}

// DialTo is Dial with an explicit socket path. Useful for tests and for
// clients that want to talk to a daemon on a non-default socket.
func DialTo(socketPath string, h Handshake) (*Client, error) {
	return DialToWithDeadline(socketPath, h, time.Time{})
}

// DialToWithDeadline bounds both socket connection and handshake by one
// absolute deadline. Zero preserves DialTo's existing timeouts. A nonzero
// deadline remains installed after the handshake for an immediate bounded
// exchange; callers may then manage or clear it for a longer-lived connection.
func DialToWithDeadline(socketPath string, h Handshake, deadline time.Time) (*Client, error) {
	d := &net.Dialer{Timeout: 500 * time.Millisecond, Deadline: deadline}
	conn, err := d.Dial("unix", socketPath)
	if err != nil {
		if isNoDaemonErr(err) {
			return nil, fmt.Errorf("%w: %v", ErrDaemonUnavailable, err)
		}
		return nil, err
	}

	// Capture the connected endpoint before a rejecting peer writes its ack
	// and immediately closes; the kernel identity belongs to this connection.
	peerPID := peerProcessID(conn)
	if h.Version == 0 {
		h.Version = ProtocolVersion
	}
	if h.PID == 0 {
		h.PID = os.Getpid()
	}
	// Bound the handshake exchange. The daemon stamps warmup state onto the
	// ack, so this is a real round trip through the server — a wedged daemon
	// would otherwise park every dialler here forever, including the
	// "is the daemon usable?" probes whose whole job is to degrade quickly.
	// Cleared below: the connection that follows is long-lived (an MCP proxy
	// relay streams on it for the life of the session).
	handshakeDeadline := time.Now().Add(controlHandshakeTimeoutForTest)
	if !deadline.IsZero() && deadline.Before(handshakeDeadline) {
		handshakeDeadline = deadline
	}
	handshakeBudget := time.Until(handshakeDeadline)
	_ = conn.SetDeadline(handshakeDeadline)
	if err := WriteJSONLine(conn, h); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("send handshake: %w", err)
	}
	reader := bufio.NewReader(conn)
	ackLine, err := reader.ReadBytes('\n')
	if err != nil {
		_ = conn.Close()
		if isTimeoutErr(err) {
			return nil, fmt.Errorf("%w: no handshake ack within %s (the daemon is up but not answering — it may be busy indexing; check `gortex daemon status`)",
				ErrDaemonUnresponsive, handshakeBudget)
		}
		return nil, fmt.Errorf("read handshake ack: %w", err)
	}
	_ = conn.SetDeadline(deadline)
	var ack HandshakeAck
	if err := json.Unmarshal(ackLine, &ack); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("parse handshake ack: %w", err)
	}
	if !ack.OK {
		_ = conn.Close()
		// Preserve mismatch identity for direction-aware startup handoff. It is
		// never evidence that the daemon is absent or embedded mode is safe.
		if ack.ErrorCode == ErrProtocolMismatch {
			mismatch := protocolMismatch(ack, h.Version)
			mismatch.PeerPID = peerPID
			return nil, mismatch
		}
		return nil, fmt.Errorf("daemon rejected handshake [%s]: %s", ack.ErrorCode, ack.ErrorMsg)
	}
	return &Client{Conn: conn, reader: reader, Ack: ack, PeerPID: peerPID}, nil
}

// Close tears down the connection.
func (c *Client) Close() error {
	if c == nil || c.Conn == nil {
		return nil
	}
	return c.Conn.Close()
}

// Control sends one control request and returns the paired response.
// Fails if the Client was opened in ModeMCP.
//
// The round trip is bounded by ControlTimeoutFor(kind) — status /
// search_symbols and friends must answer promptly, while track / reload /
// enrichment and shutdown are allowed to run as long as they need (see
// ControlTimeoutFor for why shutdown is in that list, and note that
// `gortex daemon stop` passes its own budget rather than relying on this
// default). A bounded kind that overruns returns ErrDaemonUnresponsive rather
// than blocking forever, which is what kept `gortex daemon stop` and
// `gortex call` wedged behind a busy controller with nothing printed.
func (c *Client) Control(kind string, params any) (ControlResponse, error) {
	return c.ControlWithTimeout(kind, params, ControlTimeoutFor(kind))
}

// ControlWithTimeout is Control with an explicit round-trip budget. Callers on
// a latency-sensitive path (advisory lookups whose result only decorates output
// or picks a backend) should pass a short budget and degrade on
// ErrDaemonUnresponsive instead of making the user wait for a busy daemon.
//
// A zero or negative timeout means "no bound", and in that case the connection's
// own deadline is left untouched — so a caller that manages Conn deadlines
// itself keeps full control. With a positive timeout the deadline is set for the
// round trip and cleared afterwards, which would override any deadline the
// caller had already installed; pass the budget here instead of setting one.
func (c *Client) ControlWithTimeout(kind string, params any, timeout time.Duration) (ControlResponse, error) {
	var raw json.RawMessage
	if params != nil {
		var err error
		raw, err = json.Marshal(params)
		if err != nil {
			return ControlResponse{}, fmt.Errorf("marshal params: %w", err)
		}
	}
	if timeout > 0 {
		_ = c.Conn.SetDeadline(time.Now().Add(timeout))
		defer func() { _ = c.Conn.SetDeadline(time.Time{}) }()
	}
	req := ControlRequest{Kind: kind, Params: raw}
	if err := WriteJSONLine(c.Conn, req); err != nil {
		return ControlResponse{}, c.controlErr(kind, timeout, "send control request", err)
	}
	line, err := c.reader.ReadBytes('\n')
	if err != nil {
		return ControlResponse{}, c.controlErr(kind, timeout, "read control response", err)
	}
	var resp ControlResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return ControlResponse{}, fmt.Errorf("parse control response: %w", err)
	}
	return resp, nil
}

// controlErr turns a deadline expiry into ErrDaemonUnresponsive with a
// message that names the likely cause, so the failure is actionable instead
// of a bare "i/o timeout".
func (c *Client) controlErr(kind string, timeout time.Duration, stage string, err error) error {
	if isTimeoutErr(err) {
		return fmt.Errorf("%w: %q got no response within %s (the daemon is up but not answering — a long track / reload / enrichment may be holding it; check `gortex daemon status`)",
			ErrDaemonUnresponsive, kind, timeout)
	}
	return fmt.Errorf("%s: %w", stage, err)
}

// WriteMCPFrame writes a raw MCP JSON-RPC frame to the daemon. Caller is
// responsible for ensuring the frame is valid JSON-RPC; the daemon
// passes it through to the session's MCPDispatcher.
func (c *Client) WriteMCPFrame(frame []byte) error {
	if _, err := c.Conn.Write(frame); err != nil {
		return err
	}
	// Ensure newline-delimited framing.
	if n := len(frame); n == 0 || frame[n-1] != '\n' {
		if _, err := c.Conn.Write([]byte{'\n'}); err != nil {
			return err
		}
	}
	return nil
}

// ReadMCPFrame reads one MCP JSON-RPC frame from the daemon. Returns
// io.EOF when the daemon closes the connection.
func (c *Client) ReadMCPFrame() ([]byte, error) {
	if c.reader == nil {
		if c.Conn == nil {
			return nil, net.ErrClosed
		}
		c.reader = bufio.NewReader(c.Conn)
	}
	line, err := c.reader.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return line, nil
}

// ErrDaemonUnavailable is the sentinel Dial returns when the daemon
// isn't reachable. Callers in fallback paths should errors.Is() against
// this to distinguish "daemon just isn't running" from real errors
// (permissions, handshake rejection, etc.) where silent fallback would
// mask a bug.
var ErrDaemonUnavailable = errors.New("daemon unavailable")

// ErrProtocolVersionMismatch is returned by Dial when the daemon answers the
// handshake with a protocol_mismatch rejection — the running daemon speaks a
// different wire version than this binary (a stale daemon after an upgrade).
// It never permits embedded fallback: the live daemon still owns its store.
var ErrProtocolVersionMismatch = errors.New("daemon protocol version mismatch")

// ErrDaemonUnresponsive is returned when the daemon accepted the connection
// but did not answer inside the request's budget. It is deliberately distinct
// from ErrDaemonUnavailable: the daemon is running and holding its store lock,
// so falling back to an embedded server or a fresh start is *not* automatically
// safe — the caller has to decide whether to degrade, retry, or report.
var ErrDaemonUnresponsive = errors.New("daemon unresponsive")

// controlHandshakeTimeoutForTest is ControlHandshakeTimeout as a var so the
// liveness tests can exercise the stall path without a ten-second wait.
var controlHandshakeTimeoutForTest = ControlHandshakeTimeout

// isTimeoutErr reports whether err is a socket deadline expiry.
func isTimeoutErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// ShouldFallBackToEmbedded reports whether a Dial error permits the MCP
// startup policy to consider an embedded in-process server: the daemon isn't
// running. A mismatch still means a live daemon owns its store. Any other error
// (permissions, a genuinely broken socket) is a real failure the caller must
// surface.
func ShouldFallBackToEmbedded(err error) bool {
	return errors.Is(err, ErrDaemonUnavailable) && !errors.Is(err, ErrProtocolVersionMismatch)
}

// isNoDaemonErr returns true for the subset of dial errors that mean
// "the daemon probably isn't running" as opposed to "your system is
// broken." We treat connection-refused, no-such-file, and timeout as
// "unavailable" because spinning up the daemon or falling back is the
// right answer to all three.
//
// The errno set is per-platform (unavailableDialErrnos): Winsock reports its
// own refusal code, which shares no value with the POSIX name.
func isNoDaemonErr(err error) bool {
	if isUnavailableDialErrno(err) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	// net.OpError wrapping "no such file or directory" is how Go reports
	// a missing socket path on some platforms.
	var op *net.OpError
	if errors.As(err, &op) && isUnavailableDialErrno(op.Err) {
		return true
	}
	return false
}

// isUnavailableDialErrno reports whether err carries (at any depth) an errno
// this platform uses for "nothing is listening there".
func isUnavailableDialErrno(err error) bool {
	var se syscall.Errno
	if !errors.As(err, &se) {
		return false
	}
	for _, candidate := range unavailableDialErrnos {
		if se == candidate {
			return true
		}
	}
	return false
}

// ProbeAvailability checks the default daemon socket while preserving whether
// a failed dial means "not running" or a real system error. Startup selection
// must not hide permissions or a broken socket behind embedded fallback.
func ProbeAvailability() error {
	return probeAvailabilityAt(SocketPath())
}

func probeAvailabilityAt(socketPath string) error {
	d := &net.Dialer{Timeout: 200 * time.Millisecond}
	conn, err := d.Dial("unix", socketPath)
	if err != nil {
		return classifyDaemonProbeError(err)
	}
	_ = conn.Close()
	return nil
}

func classifyDaemonProbeError(err error) error {
	if err == nil {
		return nil
	}
	if isNoDaemonErr(err) {
		return fmt.Errorf("%w: %v", ErrDaemonUnavailable, err)
	}
	return err
}

// IsRunning returns true when a daemon is reachable on the default socket.
// A thin convenience for CLI paths that want to branch on availability
// without constructing a full handshake.
func IsRunning() bool {
	return ProbeAvailability() == nil
}

// IsRunningAt is IsRunning with an explicit socket path.
func IsRunningAt(socketPath string) bool {
	return probeAvailabilityAt(socketPath) == nil
}
