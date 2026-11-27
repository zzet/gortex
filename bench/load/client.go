package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"time"
)

const maxWireBytes = 64 << 20

// A frame write that has not finished by then is given up, unless the call's
// own context ends sooner.
const defaultWriteTimeout = 10 * time.Second

const loadWireTools, loadWireToolsMode = "facade-v1", "hide"

type rpcError struct {
	Code      int            `json:"code"`
	Message   string         `json:"message"`
	Data      map[string]any `json:"data,omitempty"`
	Retriable bool           `json:"-"` // typed tool-result refusal, not a JSON-RPC top-level field
}

func (e *rpcError) Error() string { return fmt.Sprintf("RPC %d: %s", e.Code, e.Message) }

func (e *rpcError) retryEditProbe() bool {
	if unknown, _ := e.Data["delivery_unknown"].(bool); unknown {
		return false
	}
	// These are actual front-door shapes: dispatcher -32002 in
	// mcp_request_lifetime.go, and repo_loading data in
	// cmd/gortex/daemon_mcp_repo_loading.go. Unknowns stay terminal.
	if e.Code == -32002 {
		return true
	}
	if e.Code == -32000 && editProbeTransientCheckoutRefusal(e.Message) != "" {
		return true // a typed checkout refusal before admission; see the classifier
	}
	return e.Retriable || e.Code == -32000 && e.Data["error_code"] == "repo_loading" && e.Data["retryable"] == true
}

type wireReply struct {
	ID       json.Number     `json:"id"`
	Result   json.RawMessage `json:"result"`
	Error    *rpcError       `json:"error"`
	Method   string          `json:"method"`
	Params   json.RawMessage `json:"params"`
	err      error
	rawFrame []byte
	rawBytes int
}

type notificationObservation struct {
	Received time.Time       `json:"received"`
	Payload  json.RawMessage `json:"payload"`
}

type socketClient struct {
	conn                      net.Conn
	reader                    *bufio.Reader
	writeGate                 chan struct{}
	mu                        sync.Mutex
	next                      int64
	pending                   map[int64]chan wireReply
	capturePending            map[int64]bool // allocated only for opt-in observed calls
	closed                    error
	done                      chan struct{}
	notifies                  []notificationObservation
	notifyDropped             int
	version                   string
	cwd                       string // immutable checkout supplied by the successful handshake
	offeredTools, offeredMode string
	socket                    string        // set by openMCP; a session without it never re-dials
	writeTimeout              time.Duration // zero means defaultWriteTimeout
	redial                    sessionRedial
}

// sessionRedial is guarded by socketClient.mu; gate serializes re-dials.
type sessionRedial struct {
	limit     int       // zero: a closed stream stays closed
	abandoned bool      // this client gave the current stream up mid-write
	shut      bool      // Close was called
	retryAt   time.Time // a failed re-dial delays the next one until then
	gate      chan struct{}
	counts    sessionRedials
}

// redialBackoff spaces re-dials after failures, so calls arriving together
// on a session whose re-dial failed cannot spend its whole limit at once.
func redialBackoff(failed int) time.Duration {
	return min(time.Second, 100*time.Millisecond<<max(0, failed-1))
}

// sessionRedials counts one session's re-dials.
type sessionRedials struct {
	Attempts     int  // re-dials started; each one counts against the limit
	Failed       int  // re-dials that did not open a new session
	LimitReached bool // a call found the stream closed with no re-dial left
}

// errStreamReplaced reports a frame that was not written because its stream
// failed and the session has since re-dialed. The failure itself reaches the
// call through its pending reply.
var errStreamReplaced = errors.New("session stream was replaced before this frame was written")

// Per-call values returned to its caller, never a shared last-ID cache. Times
// retain monotonic readings until the scenario renders relative nanoseconds.
type wireCallIdentity struct {
	ID                                                             int64
	onWriteStart                                                   func(time.Time)
	failure                                                        *failureWireCapture
	Started, WriteStarted, WriteFinished, ReplyDelivered, Finished time.Time
	// Redialed reports that this call found its stream given up and went
	// through a re-dial before it could write: it re-dialed the session, or
	// waited for another call's re-dial, whether or not a stream opened. A
	// call refused without one (the daemon closed the stream, the session may
	// not re-dial, or its limit is spent) is not redialed. Unlike Redial it
	// does not depend on the clock: a re-dial quicker than the clock's
	// resolution is redialed with Redial zero.
	Redialed bool
	// Redial is the time this call spent re-opening its session before it
	// could write: waiting for another call's re-dial, the old reader's exit,
	// the dial, handshake and initialize, and any back-off after a failed
	// re-dial. It lies between Started and WriteStarted, so it is inside the
	// call's latency. Zero unless Redialed.
	Redial time.Duration
}

func readWireLine(reader *bufio.Reader) ([]byte, error) {
	var data []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(data)+len(part) > maxWireBytes {
			return nil, errors.New("daemon response exceeds 64 MiB")
		}
		data = append(data, part...)
		if err != bufio.ErrBufferFull {
			return data, err
		}
	}
}

func dialSocket(ctx context.Context, socket, cwd, mode string) (net.Conn, *bufio.Reader, string, error) {
	return dialSocketSurface(ctx, socket, cwd, mode, loadWireTools, loadWireToolsMode)
}

// socketDialers replaces the unix-socket dial for the socket paths it holds
// (string to func(context.Context) (net.Conn, error)). Only tests register
// one, to serve a stub daemon over net.Pipe: a pipe's write deadline fires
// the same way on every platform, while a real socket's depends on how the
// OS buffers an unread stream and interrupts a blocked send.
var socketDialers sync.Map

func dialUnixSocket(ctx context.Context, socket string) (net.Conn, error) {
	if dial, ok := socketDialers.Load(socket); ok {
		return dial.(func(context.Context) (net.Conn, error))(ctx)
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", socket)
}

// dialSocketSurface negotiates the session's tool surface in the handshake,
// as the CLI does per connection; the load workload always uses the facade.
func dialSocketSurface(ctx context.Context, socket, cwd, mode, tools, toolsMode string) (net.Conn, *bufio.Reader, string, error) {
	conn, err := dialUnixSocket(ctx, socket)
	if err != nil {
		return nil, nil, "", err
	}
	deadline := time.Now().Add(10 * time.Second)
	if parent, ok := ctx.Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, nil, "", err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	handshake := map[string]any{"version": 1, "mode": mode, "cwd": cwd, "client": "gortex-load", "tools": tools, "tools_mode": toolsMode}
	if err := json.NewEncoder(conn).Encode(handshake); err != nil {
		_ = conn.Close()
		return nil, nil, "", err
	}
	reader := bufio.NewReader(conn)
	line, err := readWireLine(reader)
	var ack struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error_msg"`
		Version string `json:"daemon_version"`
	}
	if err == nil {
		err = json.Unmarshal(line, &ack)
	}
	if err == nil && !ack.OK {
		err = fmt.Errorf("handshake refused: %s", ack.Error)
	}
	if err == nil {
		err = conn.SetDeadline(time.Time{})
	}
	if err != nil {
		_ = conn.Close()
		return nil, nil, "", err
	}
	return conn, reader, ack.Version, nil
}

func openMCP(ctx context.Context, socket, cwd string) (*socketClient, error) {
	conn, reader, version, err := dialSocket(ctx, socket, cwd, "mcp")
	if err != nil {
		return nil, err
	}
	c := &socketClient{conn: conn, reader: reader, pending: make(map[int64]chan wireReply), done: make(chan struct{}), version: version, cwd: cwd, writeGate: make(chan struct{}, 1), socket: socket}
	c.offeredTools, c.offeredMode = loadWireTools, loadWireToolsMode
	go c.readLoop()
	if _, err := c.call(ctx, "initialize", mcpInitializeParams()); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := c.send(ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, false); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func mcpInitializeParams() map[string]any {
	return map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "gortex-load", "version": "1"}}
}

func (c *socketClient) readLoop() {
	c.mu.Lock()
	conn, reader, done := c.conn, c.reader, c.done
	c.mu.Unlock()
	c.readStream(conn, reader, done)
}

// readStream delivers one stream's replies. A failure it reads is the
// daemon's doing, so it never makes the session re-dial.
func (c *socketClient) readStream(conn net.Conn, reader *bufio.Reader, done chan struct{}) {
	defer close(done)
	for {
		line, err := readWireLine(reader)
		if err != nil {
			c.failStream(conn, err, false)
			return
		}
		var reply wireReply
		if err := json.Unmarshal(line, &reply); err != nil {
			c.failStream(conn, fmt.Errorf("invalid daemon JSON: %w", err), false)
			return
		}
		if reply.Method != "" {
			c.noteNotification(line)
			continue
		}
		id, err := strconv.ParseInt(string(reply.ID), 10, 64)
		if err != nil {
			c.failStream(conn, errors.New("daemon returned non-integer response ID"), false)
			return
		}
		if reply.Result == nil && reply.Error == nil {
			c.failStream(conn, errors.New("daemon response has neither result nor error"), false)
			return
		}
		c.mu.Lock()
		ch := c.pending[id]
		capture := c.capturePending[id]
		delete(c.pending, id)
		delete(c.capturePending, id)
		c.mu.Unlock()
		if capture {
			reply.rawBytes = len(line)
			if len(line) <= readSourceFailureBytes {
				reply.rawFrame = append([]byte(nil), line...)
			}
		}
		if ch != nil {
			ch <- reply
		}
	}
}

func (c *socketClient) noteNotification(line []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Keep invalidation/publication evidence without retaining unbounded
	// diagnostic notifications throughout a saturation run.
	if len(c.notifies) < 1000 {
		c.notifies = append(c.notifies, notificationObservation{Received: time.Now(), Payload: append(json.RawMessage(nil), line...)})
	} else {
		c.notifyDropped++
	}
}

// failStream closes the session's current stream once and fails its pending
// calls with err. abandoned means this client gave the stream up mid-write
// (its write deadline or the call's context ended), not that the daemon
// closed or corrupted it. A failure of an earlier, replaced stream is ignored.
func (c *socketClient) failStream(conn net.Conn, err error, abandoned bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed != nil || c.conn != conn {
		return
	}
	c.closed, c.redial.abandoned = err, abandoned
	for id, ch := range c.pending {
		ch <- wireReply{err: err}
		delete(c.pending, id)
		delete(c.capturePending, id)
	}
}

func (c *socketClient) Close() error {
	c.mu.Lock()
	c.redial.shut = true
	conn, done := c.conn, c.done
	c.mu.Unlock()
	err := conn.Close()
	<-done
	return err
}

// allowRedials lets a session opened by openMCP replace a stream it gave up
// on mid-write with a new session on the same socket and checkout, at most
// limit times. Without it, that stream stays closed and every later call
// fails at once with the original error.
func (c *socketClient) allowRedials(limit int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.socket == "" {
		return
	}
	c.redial.limit = limit
	if c.redial.gate == nil {
		c.redial.gate = make(chan struct{}, 1)
	}
}

func (c *socketClient) redialCounts() sessionRedials {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.redial.counts
}

// redialRefusalLocked says why a closed session cannot re-dial now, or nil.
// The original failure stays first in every message, so its class survives.
// The caller holds c.mu and has seen c.closed set.
func (c *socketClient) redialRefusalLocked() error {
	switch {
	case c.redial.limit == 0 || c.redial.shut || !c.redial.abandoned:
		return c.closed
	case c.redial.counts.Attempts >= c.redial.limit:
		c.redial.counts.LimitReached = true
		return fmt.Errorf("%w (session not re-opened: re-dial limit %d reached)", c.closed, c.redial.limit)
	}
	return nil
}

// reopen replaces the closed stream of a session that may re-dial. It
// returns nil once a stream is open, or the reason the session stays closed.
// redialed reports that the call went through a re-dial, its own or one it
// waited for, rather than finding the stream open or being refused at once.
func (c *socketClient) reopen(ctx context.Context) (redialed bool, err error) {
	c.mu.Lock()
	if c.closed == nil { // another call re-opened it meanwhile
		c.mu.Unlock()
		return false, nil
	}
	if err := c.redialRefusalLocked(); err != nil {
		c.mu.Unlock()
		return false, err
	}
	gate := c.redial.gate
	c.mu.Unlock()
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return true, ctx.Err()
	}
	defer func() { <-gate }()
	c.mu.Lock()
	if c.closed == nil {
		c.mu.Unlock()
		return true, nil
	}
	if err := c.redialRefusalLocked(); err != nil {
		c.mu.Unlock()
		return true, err
	}
	if wait := time.Until(c.redial.retryAt); wait > 0 {
		// Holding the gate, only Close can change the session meanwhile.
		c.mu.Unlock()
		if err := waitContext(ctx, wait); err != nil {
			return true, err
		}
		c.mu.Lock()
		if c.redial.shut {
			err := c.closed
			c.mu.Unlock()
			return true, err
		}
	}
	c.redial.counts.Attempts++
	attempt, cause, old, oldDone := c.redial.counts.Attempts, c.closed, c.conn, c.done
	c.next++
	initialize := c.next
	c.mu.Unlock()
	conn, reader, err := c.dialStream(ctx, old, oldDone, initialize)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil && c.redial.shut {
		err = net.ErrClosed
		_ = conn.Close()
	}
	if err != nil {
		c.redial.counts.Failed++
		c.redial.retryAt = time.Now().Add(redialBackoff(c.redial.counts.Failed))
		return true, fmt.Errorf("%w (re-dial %d of %d failed: %v)", cause, attempt, c.redial.limit, err)
	}
	done := make(chan struct{})
	c.conn, c.reader, c.done = conn, reader, done
	c.pending, c.capturePending = make(map[int64]chan wireReply), nil
	c.closed, c.redial.abandoned = nil, false
	go c.readStream(conn, reader, done)
	return true, nil
}

// dialStream opens and initializes a new session before any call can see it.
// The old stream's reader stops first, so it cannot fail the new stream.
func (c *socketClient) dialStream(ctx context.Context, old net.Conn, oldDone chan struct{}, initialize int64) (net.Conn, *bufio.Reader, error) {
	_ = old.Close() // The failed writer closed it already.
	select {
	case <-oldDone:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	conn, reader, version, err := dialSocketSurface(ctx, c.socket, c.cwd, "mcp", c.offeredTools, c.offeredMode)
	if err != nil {
		return nil, nil, err
	}
	if version != c.version {
		err = fmt.Errorf("daemon version changed from %q to %q", c.version, version)
	} else {
		err = c.initializeStream(ctx, conn, reader, initialize)
	}
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, reader, nil
}

// initializeStream runs the MCP initialize exchange that openMCP runs, on a
// stream that no call uses yet.
func (c *socketClient) initializeStream(ctx context.Context, conn net.Conn, reader *bufio.Reader, id int64) error {
	deadline := time.Now().Add(c.writeBudget())
	if parent, ok := ctx.Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	err := func() error {
		encoder := json.NewEncoder(conn)
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": "initialize", "params": mcpInitializeParams()}); err != nil {
			return err
		}
		for {
			line, err := readWireLine(reader)
			if err != nil {
				return err
			}
			var reply wireReply
			if err := json.Unmarshal(line, &reply); err != nil {
				return fmt.Errorf("invalid daemon JSON: %w", err)
			}
			if reply.Method != "" {
				c.noteNotification(line)
				continue
			}
			if got, err := strconv.ParseInt(string(reply.ID), 10, 64); err != nil || got != id {
				return errors.New("daemon answered another request during initialize")
			}
			if reply.Error != nil {
				return reply.Error
			}
			return encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
		}
	}()
	if !stop() {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	return conn.SetDeadline(time.Time{})
}

func (c *socketClient) send(ctx context.Context, value any, advisory bool) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	return c.sendOn(ctx, conn, value, advisory, nil)
}

// writeBudget bounds one frame write, and the initialize exchange of a
// re-dialed stream: the client's own write timeout, or defaultWriteTimeout.
func (c *socketClient) writeBudget() time.Duration {
	if c.writeTimeout > 0 {
		return c.writeTimeout
	}
	return defaultWriteTimeout
}

// sendOn writes one frame on conn, the stream its caller registered on. It
// never writes on a stream that has failed or been replaced.
func (c *socketClient) sendOn(ctx context.Context, conn net.Conn, value any, advisory bool, observation *wireCallIdentity) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if advisory {
		select {
		case c.writeGate <- struct{}{}:
		default:
			return nil // cancellation is advisory; never queue behind a writer
		}
	} else {
		select {
		case c.writeGate <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer func() { <-c.writeGate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	closed, replaced := c.closed, c.conn != conn
	c.mu.Unlock()
	if replaced {
		return errStreamReplaced
	}
	if closed != nil {
		return closed
	}
	deadline := time.Now().Add(c.writeBudget())
	if parent, ok := ctx.Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		c.failStream(conn, err, false)
		_ = conn.Close()
		return err
	}
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		// A canceled partial frame cannot be reused by another request.
		c.failStream(conn, ctx.Err(), true)
		_ = conn.Close()
		close(interrupted)
	})
	if observation != nil {
		observation.WriteStarted = time.Now()
		if observation.onWriteStart != nil {
			observation.onWriteStart(observation.WriteStarted)
		}
	}
	frame := append(data, '\n')
	written, writeErr := io.Copy(conn, bytes.NewReader(frame))
	err = writeErr
	if observation != nil && observation.failure != nil {
		capture := observation.failure
		capture.requestBytes = int(written)
		capture.writeCompleted = writeErr == nil && written == int64(len(frame))
		switch {
		case written > readSourceFailureBytes:
			capture.requestStatus = "oversize"
		case written > 0:
			capture.request = append([]byte(nil), frame[:int(written)]...)
			capture.requestStatus = "partial"
			if capture.writeCompleted {
				capture.requestStatus = "complete"
			}
		}
	}
	if observation != nil {
		observation.WriteFinished = time.Now()
	}
	if !stop() {
		<-interrupted
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		// The frame may be partial, so the stream is closed either way. Only
		// a write this client gave up on lets the session re-dial; a write the
		// daemon refused (a reset or broken pipe) does not.
		c.failStream(conn, err, ctx.Err() != nil || errors.Is(writeErr, os.ErrDeadlineExceeded))
		_ = conn.Close()
	}
	return err
}

func (c *socketClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return c.callObserved(ctx, method, params, nil)
}

func (c *socketClient) callWithFailureCapture(ctx context.Context, method string, params any, capture *failureWireCapture) (json.RawMessage, error) {
	observation := wireCallIdentity{failure: capture}
	capture.offeredTools, capture.offeredMode, capture.daemonVersion = c.offeredTools, c.offeredMode, c.version
	data, err := c.callObserved(ctx, method, params, &observation)
	capture.id = observation.ID
	return data, err
}

func (c *socketClient) callWithIdentity(ctx context.Context, method string, params any) (json.RawMessage, wireCallIdentity, error) {
	return c.callWithWriteObserver(ctx, method, params, nil)
}

func (c *socketClient) callWithWriteObserver(ctx context.Context, method string, params any, onWriteStart func(time.Time)) (json.RawMessage, wireCallIdentity, error) {
	var observation wireCallIdentity
	observation.onWriteStart = onWriteStart
	data, err := c.callObserved(ctx, method, params, &observation)
	observation.onWriteStart = nil
	return data, observation, err
}

func (c *socketClient) callObserved(ctx context.Context, method string, params any, observation *wireCallIdentity) (json.RawMessage, error) {
	if observation != nil {
		observation.Started = time.Now()
		defer func() { observation.Finished = time.Now() }()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed != nil {
		c.mu.Unlock()
		redialStarted := time.Now()
		redialed, err := c.reopen(ctx)
		if observation != nil && redialed {
			observation.Redialed, observation.Redial = true, time.Since(redialStarted)
		}
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		if c.closed != nil { // the new stream has failed already
			err := c.closed
			c.mu.Unlock()
			return nil, err
		}
	}
	conn := c.conn
	c.next++
	id := c.next
	if observation != nil {
		observation.ID = id
	}
	ch := make(chan wireReply, 1)
	c.pending[id] = ch
	if observation != nil && observation.failure != nil {
		if c.capturePending == nil {
			c.capturePending = make(map[int64]bool)
		}
		c.capturePending[id] = true
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		delete(c.capturePending, id)
		c.mu.Unlock()
	}()
	if err := c.sendOn(ctx, conn, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}, false, observation); err != nil {
		if errors.Is(err, errStreamReplaced) {
			select {
			case reply := <-ch:
				if reply.err != nil {
					return nil, reply.err
				}
			default:
			}
		}
		return nil, err
	}
	select {
	case <-ctx.Done():
		// This is advisory; closing a shared connection here would censor
		// unrelated burst calls and distort their latency/error samples.
		cancelCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		_ = c.sendOn(cancelCtx, conn, map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": id, "reason": ctx.Err().Error()}}, true, nil)
		cancel()
		return nil, ctx.Err()
	case reply := <-ch:
		if observation != nil {
			observation.ReplyDelivered = time.Now()
			if capture := observation.failure; capture != nil && reply.rawBytes > 0 {
				capture.responseBytes = reply.rawBytes
				capture.response = reply.rawFrame
				capture.responseStatus = "complete"
				if len(reply.rawFrame) == 0 {
					capture.responseStatus = "oversize"
				}
			}
		}
		if ctx.Err() != nil {
			return reply.Result, ctx.Err()
		}
		if reply.err != nil {
			return nil, reply.err
		}
		if reply.Error != nil {
			return reply.Result, reply.Error
		}
		return reply.Result, nil
	}
}

func control(ctx context.Context, socket, kind string, params any) (json.RawMessage, error) {
	conn, reader, _, err := dialSocket(ctx, socket, "", "control")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	if err := json.NewEncoder(conn).Encode(map[string]any{"kind": kind, "params": params}); err != nil {
		return nil, err
	}
	line, err := readWireLine(reader)
	if err != nil {
		return nil, err
	}
	var response struct {
		OK     bool            `json:"ok"`
		Error  string          `json:"error_msg"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(line, &response); err != nil {
		return nil, err
	}
	if !response.OK {
		return response.Result, fmt.Errorf("control %s: %s", kind, response.Error)
	}
	return response.Result, nil
}

// textPayload handles the public MCP content envelope, never daemon internals.
func textPayload(result json.RawMessage, field string) (json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(result, &envelope); err != nil {
		return nil, err
	}
	var rows []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(envelope[field], &rows); err != nil {
		return nil, err
	}
	for _, row := range rows {
		if json.Valid([]byte(row.Text)) {
			return json.RawMessage(row.Text), nil
		}
	}
	return nil, io.ErrUnexpectedEOF
}
