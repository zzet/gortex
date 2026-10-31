package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/procio"
)

// Client manages a JSON-RPC 2.0 connection to an LSP server.
//
// The transport behind the read/write pair is pluggable: it can be a
// spawned subprocess (SpawnTransport — the original behaviour) or a
// dialed network connection (DialTransport — passive attach to an IDE-
// managed server). The Client never touches the transport directly
// past construction; Shutdown delegates the close semantics.
type Client struct {
	transport Transport
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	reqID     atomic.Int64
	pending   sync.Map // reqID → chan *jsonRPCResponse
	logger    *zap.Logger
	done      chan struct{}

	// callTimeout bounds how long a single Call waits for a reply
	// before giving up. Zero means unbounded (the historical
	// behaviour). It is set after the initialize handshake completes
	// (see Provider.ensureClient) so a wedged server — e.g. csharp-ls
	// stuck loading an MSBuild workspace, alive but never replying —
	// can no longer block an enrichment hover / findReferences Call
	// forever. Stored atomically: SetCallTimeout may race the read
	// loop and concurrent Call goroutines.
	callTimeout atomic.Int64 // time.Duration nanoseconds

	mu      sync.Mutex // state only; never held across pipe writes
	writeMu sync.Mutex // serializes complete wire frames
	closing bool
	closed  bool

	shutdownOnce sync.Once
	shutdownDone chan struct{}
	shutdownErr  error
	readerDone   chan struct{}

	// notifHandlers route server → client notifications. Keyed by
	// LSP method name. Each handler receives the raw params and
	// runs synchronously on the read goroutine — keep them fast,
	// or hand off to a buffered channel.
	notifMu       sync.RWMutex
	notifHandlers map[string]NotificationHandler

	// reqHandlers route server → client *requests* (reverse RPC).
	// LSP servers issue these for things like
	// `workspace/applyEdit`, `workspace/configuration`, and
	// `client/registerCapability`. The handler returns a result
	// (or an error) which we send back framed as the response.
	reqMu       sync.RWMutex
	reqHandlers map[string]RequestHandler
}

// Transport abstracts the I/O carrier underneath a Client. The
// subprocess transport pipes stdin/stdout of a spawned LSP server;
// the dial transport returns the read/write halves of an established
// net.Conn. Stop() runs the transport-appropriate teardown — wait on
// the subprocess, or close the socket cleanly.
type Transport interface {
	// Start establishes the carrier and returns a write-end for
	// sending and a read-end for receiving JSON-RPC frames.
	Start() (io.WriteCloser, io.Reader, error)
	// Stop tears the carrier down. For a subprocess, this waits for
	// exit; for a network connection, it closes the socket. The
	// caller has already issued any protocol-level shutdown.
	Stop() error
	// SendsShutdown reports whether Client.Shutdown should issue the
	// LSP shutdown/exit handshake before closing. True for spawned
	// servers (we own their lifetime); false for dialed servers (the
	// IDE owns them — we just disconnect).
	SendsShutdown() bool
	// Description returns a human-readable identifier (used in error
	// messages, e.g. "gopls" or "tcp 127.0.0.1:7677").
	Description() string
}

// SpawnTransport launches an LSP server as a subprocess and uses its
// stdin/stdout for JSON-RPC framing. This is the original behaviour
// that long predates passive attach.
type SpawnTransport struct {
	Command       string
	Args          []string
	Env           []string
	WorkspaceRoot string

	// Logger receives the subprocess's stderr, routed through
	// procio.StderrWatcher instead of being inherited raw. Nil drains
	// stderr silently (no log spam, but also no visibility).
	Logger *zap.Logger

	started  atomic.Bool // transport is single-use, including a failed Start
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stopOnce sync.Once
	stopDone chan struct{}
	stopErr  error
}

// Start spawns the subprocess and returns its stdin / stdout. Errors
// from pipe construction or exec.Start are returned verbatim. Start is single-use
// even on failure; reconnect constructs a fresh transport. It must return before
// Stop is called. Stderr is not inherited: a per-process scanner routes it through
// Logger as structured, rate-limited Warn entries so a disconnect spam
// or crash backtrace can't flood the daemon's log with raw text.
func (s *SpawnTransport) Start() (io.WriteCloser, io.Reader, error) {
	if !s.started.CompareAndSwap(false, true) {
		return nil, nil, fmt.Errorf("LSP spawn transport is single-use")
	}
	cmd := exec.Command(s.Command, s.Args...)
	platform.ConfigureBackgroundCommand(cmd)
	cmd.Dir = s.WorkspaceRoot
	if len(s.Env) > 0 {
		cmd.Env = append(os.Environ(), s.Env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start %s: %w", s.Command, err)
	}
	s.cmd = cmd
	s.stdin = stdin
	procio.StderrWatcher{Logger: s.Logger, Tag: s.Description()}.Watch(stderr)
	return stdin, stdout, nil
}

// Stop closes owned stdin, gives the child a bounded exit opportunity, then
// kills only this exact child if necessary and joins its actual Wait result.
// It does not claim ownership of arbitrary descendant processes.
func (s *SpawnTransport) Stop() error {
	return s.stopBefore(time.Now().Add(lspShutdownBudget))
}

// SendsShutdown returns true: gortex owns the subprocess, so it must
// issue the LSP shutdown/exit handshake before tearing down the pipe.
func (s *SpawnTransport) SendsShutdown() bool { return true }

// Description returns the executable name for error/log surfaces.
func (s *SpawnTransport) Description() string { return s.Command }

// DialTransport opens a TCP or Unix-domain-socket connection to an
// already-running LSP server (e.g. one started by the user's IDE) and
// uses the resulting net.Conn as the JSON-RPC carrier.
type DialTransport struct {
	ctx     context.Context // optional owner cancellation while dialing
	Network string          // "tcp" or "unix"
	Address string          // host:port (tcp) or socket path (unix)

	conn net.Conn
	// connWriter wraps conn so closing the write half does not also
	// close the read half. We need this because Client.send closes
	// stdin to signal end-of-stream when SendsShutdown is true, and
	// for dial transports that pattern would tear down receive too.
	// In practice we set SendsShutdown to false, but the writer split
	// keeps the lifecycle clean either way.
	connWriter *dialWriter
}

// Start dials the configured network/address and returns paired
// read/write halves of the connection.
func (d *DialTransport) Start() (io.WriteCloser, io.Reader, error) {
	ctx := d.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, d.Network, d.Address)
	if err != nil {
		return nil, nil, fmt.Errorf("dial %s %s: %w", d.Network, d.Address, err)
	}
	d.conn = conn
	d.connWriter = &dialWriter{conn: conn}
	return d.connWriter, conn, nil
}

// Stop closes the underlying connection. The IDE keeps its LSP server
// alive — we just disconnect.
func (d *DialTransport) Stop() error {
	if d.conn == nil {
		return nil
	}
	return d.conn.Close()
}

// SendsShutdown returns false: the LSP server is owned by the IDE, so
// gortex must not send the shutdown/exit sequence — that would tear
// down the IDE's session.
func (d *DialTransport) SendsShutdown() bool { return false }

// Description returns "<network> <address>" — useful in spawn/dial
// failure messages.
func (d *DialTransport) Description() string { return d.Network + " " + d.Address }

// dialWriter is an io.WriteCloser that writes to the connection but
// closes nothing — the connection's full lifecycle is owned by the
// DialTransport itself. This prevents the framing layer's stdin.Close()
// from prematurely tearing down receive on shutdown.
type dialWriter struct {
	conn net.Conn
}

// Write forwards bytes to the underlying connection.
func (w *dialWriter) Write(p []byte) (int, error) { return w.conn.Write(p) }

// Close is a no-op: the connection lifecycle is owned by the
// DialTransport, not the framing layer.
func (w *dialWriter) Close() error { return nil }

// NotificationHandler processes a notification from the server.
type NotificationHandler func(method string, params json.RawMessage)

// RequestHandler processes a request from the server. Either result
// or err must be set; nil/nil is treated as a null success result.
type RequestHandler func(method string, params json.RawMessage) (result any, err *jsonRPCError)

// jsonRPCRequest is a JSON-RPC 2.0 request.
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// jsonRPCResponse is a JSON-RPC 2.0 response.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// jsonRPCNotification is a JSON-RPC 2.0 notification (no ID).
type jsonRPCNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *jsonRPCError) Error() string {
	return fmt.Sprintf("LSP error %d: %s", e.Code, e.Message)
}

// NewClient spawns an LSP server subprocess and returns a connected
// client. env carries extra KEY=VALUE entries appended to the daemon's
// own environment — used to pin a JRE for jdtls and similar.
//
// Kept for source compatibility with existing call sites and tests.
// Internally constructs a SpawnTransport and delegates to
// NewClientWithTransport.
func NewClient(command string, args, env []string, workspaceRoot string, logger *zap.Logger) (*Client, error) {
	return NewClientWithTransport(&SpawnTransport{
		Command:       command,
		Args:          args,
		Env:           env,
		WorkspaceRoot: workspaceRoot,
		Logger:        logger,
	}, logger)
}

// NewClientWithTransport builds a Client on top of any Transport
// implementation — spawned subprocess or dialed socket. The transport
// is started before returning, so a non-nil error means the carrier
// did not come up.
func NewClientWithTransport(t Transport, logger *zap.Logger) (*Client, error) {
	stdin, stdout, err := t.Start()
	if err != nil {
		return nil, err
	}

	c := &Client{
		transport:     t,
		stdin:         stdin,
		stdout:        bufio.NewReader(stdout),
		logger:        logger,
		done:          make(chan struct{}),
		readerDone:    make(chan struct{}),
		notifHandlers: make(map[string]NotificationHandler),
		reqHandlers:   make(map[string]RequestHandler),
	}

	// Start response reader goroutine.
	go c.readResponses()

	return c, nil
}

// OnNotification registers a handler for server→client notifications
// for the given method (e.g. "textDocument/publishDiagnostics"). One
// handler per method; later registrations replace earlier ones.
func (c *Client) OnNotification(method string, h NotificationHandler) {
	c.notifMu.Lock()
	defer c.notifMu.Unlock()
	c.notifHandlers[method] = h
}

// OnRequest registers a handler for server→client requests (reverse
// RPC). The reply is framed and sent back automatically.
func (c *Client) OnRequest(method string, h RequestHandler) {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()
	c.reqHandlers[method] = h
}

// Done closes when the connection becomes unavailable (EOF, error or shutdown).
// Shutdown additionally joins the actual response-reader and owned transport.
func (c *Client) Done() <-chan struct{} { return c.done }

// SetCallTimeout bounds how long subsequent Call invocations wait for a
// reply. A non-positive duration restores the unbounded behaviour.
// Callers typically set this only after the initialize handshake has
// completed, leaving the (possibly slow) cold-workspace load unbounded.
func (c *Client) SetCallTimeout(d time.Duration) { c.callTimeout.Store(int64(d)) }

// Call sends a request and waits for the response.
func (c *Client) Call(method string, params any, result any) error {
	return c.call(method, params, result, false, time.Duration(c.callTimeout.Load()))
}

func (c *Client) call(method string, params any, result any, shutdown bool, timeoutBudget time.Duration) error {
	id := c.reqID.Add(1)

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	respCh := make(chan *jsonRPCResponse, 1)
	c.pending.Store(id, respCh)
	defer c.pending.Delete(id)

	if err := c.sendFrame(req, shutdown); err != nil {
		return fmt.Errorf("send %s: %w", method, err)
	}

	// Wait for response. A bounded callTimeout (set after the
	// initialize handshake) guards against a server that is alive but
	// never replies. A nil timer channel — callTimeout <= 0 — blocks
	// forever in the select, preserving the historical unbounded
	// behaviour for callers that never set a timeout.
	var timeout <-chan time.Time
	if timeoutBudget > 0 {
		t := time.NewTimer(timeoutBudget)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case resp := <-respCh:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	case <-c.done:
		return fmt.Errorf("LSP server exited")
	case <-timeout:
		return fmt.Errorf("LSP call %s: timeout after %s", method, timeoutBudget)
	}
}

// Notify sends a notification (no response expected).
func (c *Client) Notify(method string, params any) error {
	notif := jsonRPCNotification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	return c.send(notif)
}

// send writes a JSON-RPC message using the LSP content-length framing.
func (c *Client) send(msg any) error {
	return c.sendFrame(msg, false)
}

func (c *Client) sendFrame(msg any, shutdown bool) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	// Refuse new work before queuing behind a possibly blocked old writer.
	c.mu.Lock()
	refused := c.closed || c.closing && !shutdown
	c.mu.Unlock()
	if refused {
		return fmt.Errorf("client is closed or shutting down")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	refused = c.closed || c.closing && !shutdown
	c.mu.Unlock()
	if refused {
		return fmt.Errorf("client is closed or shutting down")
	}

	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	if _, err := io.WriteString(c.stdin, header); err != nil {
		return err
	}
	if _, err := c.stdin.Write(data); err != nil {
		return err
	}
	return nil
}

// readResponses continuously reads responses from the LSP server.
//
// Three message shapes are framed identically:
//   - response (has "id" + "result" or "error"): routed to pending Call.
//   - notification (has "method" but no "id"): dispatched to
//     OnNotification handlers.
//   - request (has both "method" and "id"): the server is asking the
//     client to do something (e.g. workspace/applyEdit). The handler
//     in OnRequest returns a result that we frame and send back.
func (c *Client) readResponses() {
	defer func() {
		c.markClosed()
		if c.readerDone != nil {
			close(c.readerDone)
		}
	}()

	// malformedFrames counts consecutive header blocks that yielded no
	// usable Content-Length. A healthy server never does this; a
	// desynced or chatty one can emit a run of blank / garbage lines
	// that would otherwise spin this loop on `continue`, burning a core
	// (and, since the read loop never returns, never closing c.done to
	// unblock pending Call()s). We tolerate a bounded run so a transient
	// desync self-heals, then drop the connection.
	const maxMalformedFrames = 64
	malformedFrames := 0

	for {
		// Read headers.
		contentLength := -1
		for {
			line, err := c.stdout.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break // End of headers.
			}
			if strings.HasPrefix(line, "Content-Length:") {
				val := strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
				contentLength, _ = strconv.Atoi(val)
			}
		}

		if contentLength < 0 {
			malformedFrames++
			if malformedFrames >= maxMalformedFrames {
				c.logger.Debug("LSP: too many malformed frames, dropping connection",
					zap.Int("count", malformedFrames))
				return
			}
			continue
		}
		malformedFrames = 0

		// Read body.
		body := make([]byte, contentLength)
		if _, err := io.ReadFull(c.stdout, body); err != nil {
			return
		}

		// Inspect the message to decide if it's a response or a
		// server-initiated message (notification or request).
		var probe struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(body, &probe); err != nil {
			c.logger.Debug("LSP: failed to parse message", zap.Error(err))
			continue
		}

		// Server-initiated notification: method present, id absent.
		if probe.Method != "" && len(probe.ID) == 0 {
			var notif struct {
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(body, &notif); err != nil {
				continue
			}
			c.dispatchNotification(notif.Method, notif.Params)
			continue
		}

		// Server-initiated request: method present and id present.
		if probe.Method != "" && len(probe.ID) > 0 {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				continue
			}
			c.dispatchRequest(req.ID, req.Method, req.Params)
			continue
		}

		// Otherwise it's a response to one of our requests.
		var resp jsonRPCResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			c.logger.Debug("LSP: failed to parse response", zap.Error(err))
			continue
		}
		if ch, ok := c.pending.Load(resp.ID); ok {
			// Best-effort, non-blocking — pending channel is buffered.
			select {
			case ch.(chan *jsonRPCResponse) <- &resp:
			default:
			}
		}
	}
}

// dispatchNotification fans a server notification out to its handler.
func (c *Client) dispatchNotification(method string, params json.RawMessage) {
	c.notifMu.RLock()
	h, ok := c.notifHandlers[method]
	c.notifMu.RUnlock()
	if !ok {
		return
	}
	defer func() {
		// A panicking handler must not kill the read loop.
		if r := recover(); r != nil {
			c.logger.Debug("LSP: notification handler panicked",
				zap.String("method", method),
				zap.Any("recover", r),
			)
		}
	}()
	h(method, params)
}

// dispatchRequest answers a server-initiated request. When no handler
// is registered we reply with a JSON-RPC method-not-found error so the
// server doesn't hang waiting forever.
func (c *Client) dispatchRequest(rawID json.RawMessage, method string, params json.RawMessage) {
	c.reqMu.RLock()
	h, ok := c.reqHandlers[method]
	c.reqMu.RUnlock()

	// Result is a json.RawMessage, not `any`, so we control exactly what
	// lands on the wire. A JSON-RPC 2.0 *success* response MUST carry a
	// "result" member — even when it is null. A nil handler result is a
	// null success (the correct ack for client/registerCapability and
	// workspace/applyEdit's negative case), NOT an absent field. Marshal
	// it explicitly to "null" so the field is always present on success;
	// `omitempty` then only drops Result on the error path (where it must
	// be absent — the spec forbids result+error together). Strict servers
	// (StreamJsonRpc, used by Roslyn / the C# server) reject a bare
	// {"jsonrpc":"2.0","id":N} as "Unrecognized JSON-RPC 2.0 message" and
	// tear down the whole connection, which is how a single nil ack to
	// registerCapability used to kill every in-flight request.
	type respWithRawID struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result,omitempty"`
		Error   *jsonRPCError   `json:"error,omitempty"`
	}

	resp := respWithRawID{JSONRPC: "2.0", ID: rawID}
	if !ok {
		resp.Error = &jsonRPCError{Code: -32601, Message: "method not found: " + method}
	} else {
		var res any
		var rpcErr *jsonRPCError
		func() {
			defer func() {
				if r := recover(); r != nil {
					rpcErr = &jsonRPCError{Code: -32603, Message: "handler panicked"}
				}
			}()
			res, rpcErr = h(method, params)
		}()
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			// json.Marshal(nil) → "null"; any marshal failure also
			// falls back to a null success so the field is never empty.
			raw, err := json.Marshal(res)
			if err != nil || len(raw) == 0 {
				raw = json.RawMessage("null")
			}
			resp.Result = raw
		}
	}
	_ = c.send(resp)
}
