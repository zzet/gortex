package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultMCPToolCallTimeout leaves time for a host to receive a terminal
	// response before its common 300-second transport timeout.
	DefaultMCPToolCallTimeout = 60 * time.Second

	mcpRequestCancelledCode = -32800
	mcpRequestTimeoutCode   = -32001
)

type mcpRequestEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		RequestID json.RawMessage `json:"requestId"`
	} `json:"params"`
}

func canonicalMCPRequestID(id json.RawMessage) (string, bool) {
	if len(id) == 0 {
		return "", false
	}
	decoder := json.NewDecoder(bytes.NewReader(id))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return "", false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "", false
	}
	switch value := decoded.(type) {
	case nil:
		return "z:null", true
	case string:
		return "s:" + value, true
	case json.Number:
		return "n:" + value.String(), true
	default:
		return "", false
	}
}

type mcpInFlightRequest struct {
	key    string
	cancel context.CancelFunc
}

type mcpRequestRegistry struct {
	mu       sync.Mutex
	requests map[string]*mcpInFlightRequest
}

func (r *mcpRequestRegistry) begin(parent context.Context, id json.RawMessage, timeout time.Duration) (context.Context, *mcpInFlightRequest, bool) {
	key, valid := canonicalMCPRequestID(id)
	if !valid {
		return nil, nil, false
	}
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, timeout)
	} else {
		ctx, cancel = context.WithCancel(parent)
	}
	entry := &mcpInFlightRequest{key: key, cancel: cancel}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.requests == nil {
		r.requests = make(map[string]*mcpInFlightRequest)
	}
	if _, exists := r.requests[key]; exists {
		cancel()
		return nil, nil, false
	}
	r.requests[key] = entry
	return ctx, entry, true
}

func (r *mcpRequestRegistry) finish(entry *mcpInFlightRequest) {
	if entry == nil {
		return
	}
	r.mu.Lock()
	if r.requests[entry.key] == entry {
		delete(r.requests, entry.key)
	}
	r.mu.Unlock()
	entry.cancel()
}

func (r *mcpRequestRegistry) cancel(id json.RawMessage) bool {
	key, valid := canonicalMCPRequestID(id)
	if !valid {
		return false
	}
	r.mu.Lock()
	entry := r.requests[key]
	r.mu.Unlock()
	if entry == nil {
		return false
	}
	entry.cancel()
	return true
}

func (r *mcpRequestRegistry) cancelAll() {
	r.mu.Lock()
	entries := make([]*mcpInFlightRequest, 0, len(r.requests))
	for key, entry := range r.requests {
		entries = append(entries, entry)
		delete(r.requests, key)
	}
	r.mu.Unlock()
	for _, entry := range entries {
		entry.cancel()
	}
}

type mcpResponseWriter struct {
	mu   sync.Mutex
	conn net.Conn
}

func (w *mcpResponseWriter) write(frame []byte) error {
	if len(frame) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	frame = append(append([]byte(nil), frame...), '\n')
	for len(frame) > 0 {
		n, err := w.conn.Write(frame)
		if err != nil {
			return err
		}
		frame = frame[n:]
	}
	return nil
}

type mcpFrameDispatch func(context.Context, []byte) ([]byte, error)

type mcpDispatchResult struct {
	reply []byte
	err   error
}

const (
	defaultMaxConcurrentMCPDispatches = 8
	maxConfiguredMCPDispatches        = 64
	mcpDispatchLimitEnv               = "GORTEX_MCP_MAX_CONCURRENT_DISPATCHES"
)

func mcpDispatchLimit(raw string) int {
	if strings.TrimSpace(raw) == "" {
		return defaultMaxConcurrentMCPDispatches
	}
	limit, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || limit < 1 {
		return defaultMaxConcurrentMCPDispatches
	}
	if limit > maxConfiguredMCPDispatches {
		return maxConfiguredMCPDispatches
	}
	return limit
}

func newMCPDispatchSlots(limit int) chan struct{} {
	if limit < 1 {
		limit = defaultMaxConcurrentMCPDispatches
	}
	return make(chan struct{}, limit)
}

var mcpDispatchSlots = newMCPDispatchSlots(mcpDispatchLimit(os.Getenv(mcpDispatchLimitEnv)))

var errMCPDispatchSaturated = errors.New("MCP dispatcher capacity is saturated")

type mcpDispatchAdmission struct {
	once sync.Once
	done chan struct{}
}

type mcpDispatchAdmissionContextKey struct{}
type mcpDispatchWaitContextKey struct{}
type mcpDispatchSlotsContextKey struct{}
type mcpDispatchUnboundedContextKey struct{}

func isLongLivedMCPRequest(method string) bool {
	return method == "subscriptions/listen"
}

func newMCPDispatchAdmission() *mcpDispatchAdmission {
	return &mcpDispatchAdmission{done: make(chan struct{})}
}

func withMCPDispatchAdmission(ctx context.Context, admission *mcpDispatchAdmission) context.Context {
	return context.WithValue(ctx, mcpDispatchAdmissionContextKey{}, admission)
}

func withMCPDispatchWait(ctx context.Context) context.Context {
	return context.WithValue(ctx, mcpDispatchWaitContextKey{}, true)
}

func withMCPDispatchUnbounded(ctx context.Context) context.Context {
	return context.WithValue(ctx, mcpDispatchUnboundedContextKey{}, true)
}

func isMCPDispatchUnbounded(ctx context.Context) bool {
	unbounded, _ := ctx.Value(mcpDispatchUnboundedContextKey{}).(bool)
	return unbounded
}

func withMCPDispatchSlots(ctx context.Context, slots chan struct{}) context.Context {
	return context.WithValue(ctx, mcpDispatchSlotsContextKey{}, slots)
}

func mcpDispatchSlotsForContext(ctx context.Context) chan struct{} {
	if slots, _ := ctx.Value(mcpDispatchSlotsContextKey{}).(chan struct{}); slots != nil {
		return slots
	}
	return mcpDispatchSlots
}

func signalMCPDispatchAdmission(ctx context.Context) {
	admission, _ := ctx.Value(mcpDispatchAdmissionContextKey{}).(*mcpDispatchAdmission)
	if admission != nil {
		admission.once.Do(func() { close(admission.done) })
	}
}

// dispatchMCPWithContext bounds detached dispatcher work process-wide. A
// cancelled caller returns without waiting for an uncooperative dispatcher;
// the buffered result lets that dispatcher finish without retaining a waiter.
func dispatchMCPWithContext(ctx context.Context, dispatch func() ([]byte, error)) ([]byte, error) {
	if isMCPDispatchUnbounded(ctx) {
		signalMCPDispatchAdmission(ctx)
		return dispatch()
	}
	slots := mcpDispatchSlotsForContext(ctx)
	if wait, _ := ctx.Value(mcpDispatchWaitContextKey{}).(bool); wait {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			signalMCPDispatchAdmission(ctx)
			return nil, ctx.Err()
		}
	} else {
		select {
		case slots <- struct{}{}:
		default:
			signalMCPDispatchAdmission(ctx)
			return nil, errMCPDispatchSaturated
		}
	}

	result := make(chan mcpDispatchResult, 1)
	started := make(chan struct{})
	go func() {
		defer func() { <-slots }()
		close(started)
		reply, err := dispatch()
		result <- mcpDispatchResult{reply: reply, err: err}
	}()
	<-started
	signalMCPDispatchAdmission(ctx)

	select {
	case completed := <-result:
		return completed.reply, completed.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) mcpToolCallTimeout() time.Duration {
	if s != nil && s.MCPToolCallTimeout > 0 {
		return s.MCPToolCallTimeout
	}
	return DefaultMCPToolCallTimeout
}

func serveMCPConnection(conn net.Conn, reader *bufio.Reader, timeout time.Duration, dispatch mcpFrameDispatch) {
	serveMCPConnectionWithHooks(conn, reader, timeout, dispatch, nil)
}

type mcpConnectionHooks struct {
	beforeResponse func(json.RawMessage)
	dispatchSlots  chan struct{}
	// onReady, if set, is invoked once — before the read loop starts —
	// with a function that pushes one server-initiated JSON-RPC frame
	// through the same serialised writer the request/notification
	// replies use. This is how a SessionStartedHook implementation gets
	// a write path that is safe for concurrent use with reply delivery.
	onReady func(write func([]byte) error)
}

func serveMCPConnectionWithHooks(conn net.Conn, reader *bufio.Reader, timeout time.Duration, dispatch mcpFrameDispatch, hooks *mcpConnectionHooks) {
	if timeout <= 0 {
		timeout = DefaultMCPToolCallTimeout
	}
	connectionCtx, connectionCancel := context.WithCancel(context.Background())
	if hooks != nil && hooks.dispatchSlots != nil {
		connectionCtx = withMCPDispatchSlots(connectionCtx, hooks.dispatchSlots)
	}
	registry := &mcpRequestRegistry{}
	writer := &mcpResponseWriter{conn: conn}
	if hooks != nil && hooks.onReady != nil {
		hooks.onReady(writer.write)
	}
	var requests sync.WaitGroup
	var initializeDone <-chan struct{}
	defer func() {
		connectionCancel()
		registry.cancelAll()
		requests.Wait()
	}()

	write := func(reply []byte) bool {
		if err := writer.write(reply); err != nil {
			connectionCancel()
			registry.cancelAll()
			_ = conn.Close()
			return false
		}
		return true
	}

	launchNotification := func(line []byte, waitFor <-chan struct{}) {
		requests.Add(1)
		go func() {
			defer requests.Done()
			if waitFor != nil {
				select {
				case <-waitFor:
				case <-connectionCtx.Done():
					return
				}
			}
			// The supplied dispatcher owns the single process-wide admission.
			// Notifications wait for that slot under the connection context so
			// state transitions are not silently dropped under load.
			notificationCtx := withMCPDispatchWait(connectionCtx)
			_, _ = dispatch(notificationCtx, line)
		}()
	}

	launchRequest := func(line []byte, envelope mcpRequestEnvelope, responseID json.RawMessage, tracked bool) {
		longLived := isLongLivedMCPRequest(envelope.Method)
		requestTimeout := timeout
		if longLived {
			requestTimeout = 0
		}
		var requestCtx context.Context
		var entry *mcpInFlightRequest
		if tracked {
			var admitted bool
			requestCtx, entry, admitted = registry.begin(connectionCtx, responseID, requestTimeout)
			if !admitted {
				write(mcpErrorResponse(responseID, -32600, "duplicate in-flight request id", map[string]any{
					"outcome":   "duplicate_id",
					"retryable": false,
				}))
				return
			}
		} else {
			var cancel context.CancelFunc
			if requestTimeout > 0 {
				requestCtx, cancel = context.WithTimeout(connectionCtx, requestTimeout)
			} else {
				requestCtx, cancel = context.WithCancel(connectionCtx)
			}
			entry = &mcpInFlightRequest{cancel: cancel}
		}
		if longLived {
			requestCtx = withMCPDispatchUnbounded(requestCtx)
		}

		var initialized chan struct{}
		if envelope.Method == "initialize" {
			initialized = make(chan struct{})
			initializeDone = initialized
		}
		requestLine := append([]byte(nil), line...)
		requestID := append(json.RawMessage(nil), responseID...)
		admission := newMCPDispatchAdmission()
		dispatchCtx := withMCPDispatchAdmission(requestCtx, admission)
		requests.Add(1)
		go func() {
			defer requests.Done()
			if initialized != nil {
				defer close(initialized)
			}
			finished := false
			finish := func() {
				if finished {
					return
				}
				finished = true
				if tracked {
					registry.finish(entry)
				} else {
					entry.cancel()
				}
			}
			defer finish()

			reply, dispatchErr := dispatch(dispatchCtx, requestLine)
			if requestErr := requestCtx.Err(); requestErr != nil && (!longLived || dispatchErr != nil || len(reply) == 0) {
				code := mcpRequestCancelledCode
				message := "request cancelled"
				data := map[string]any{"outcome": "cancelled"}
				if errors.Is(requestErr, context.DeadlineExceeded) {
					code = mcpRequestTimeoutCode
					message = "request deadline exceeded"
					data = map[string]any{
						"outcome":     "deadline",
						"deadline_ms": timeout.Milliseconds(),
					}
				}
				if envelope.Method == "tools/call" {
					data["delivery_unknown"] = true
					data["retryable"] = false
				}
				reply = mcpErrorResponse(requestID, code, message, data)
			} else if errors.Is(dispatchErr, errMCPDispatchSaturated) {
				data := map[string]any{
					"outcome":   "server_busy",
					"retryable": true,
				}
				if envelope.Method == "tools/call" {
					data["delivery_unknown"] = false
				}
				reply = mcpErrorResponse(requestID, -32002, "MCP dispatcher is busy", data)
			} else if dispatchErr != nil {
				reply = mcpErrorResponse(requestID, -32603, "request dispatch failed", map[string]any{
					"outcome":   "dispatch_error",
					"retryable": false,
				})
			} else if len(reply) == 0 {
				reply = mcpErrorResponse(requestID, -32603, "request completed without a response", map[string]any{
					"outcome":   "empty_response",
					"retryable": false,
				})
			}

			// Release the request ID before publishing any terminal response so
			// an immediate same-ID retry can be admitted deterministically.
			finish()
			if hooks != nil && hooks.beforeResponse != nil {
				hooks.beforeResponse(requestID)
			}
			write(reply)
		}()
		select {
		case <-admission.done:
		case <-requestCtx.Done():
		}
	}

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				connectionCancel()
			}
			return
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) == 0 {
			continue
		}

		var envelope mcpRequestEnvelope
		if err := json.Unmarshal(line, &envelope); err != nil {
			launchRequest(line, envelope, json.RawMessage("null"), false)
			continue
		}
		if envelope.Method == "notifications/cancelled" {
			if len(envelope.ID) != 0 {
				responseID := envelope.ID
				if _, valid := canonicalMCPRequestID(responseID); !valid {
					responseID = json.RawMessage("null")
				}
				write(mcpErrorResponse(responseID, -32600, "notifications/cancelled must not contain an id", nil))
				continue
			}
			if _, valid := canonicalMCPRequestID(envelope.Params.RequestID); valid {
				registry.cancel(envelope.Params.RequestID)
			}
			continue
		}
		if len(envelope.ID) == 0 {
			var waitFor <-chan struct{}
			if envelope.Method == "notifications/initialized" {
				waitFor = initializeDone
			}
			launchNotification(append([]byte(nil), line...), waitFor)
			continue
		}
		if _, valid := canonicalMCPRequestID(envelope.ID); !valid {
			write(mcpErrorResponse(json.RawMessage("null"), -32600, "invalid JSON-RPC request id", nil))
			continue
		}
		launchRequest(line, envelope, envelope.ID, true)
	}
}

func mcpErrorResponse(id json.RawMessage, code int, message string, data map[string]any) []byte {
	if len(id) == 0 {
		return nil
	}
	type rpcError struct {
		Code    int            `json:"code"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data,omitempty"`
	}
	errorJSON, _ := json.Marshal(rpcError{Code: code, Message: message, Data: data})
	encoded := make([]byte, 0, len(id)+len(errorJSON)+40)
	encoded = append(encoded, `{"jsonrpc":"2.0","id":`...)
	encoded = append(encoded, id...)
	encoded = append(encoded, `,"error":`...)
	encoded = append(encoded, errorJSON...)
	encoded = append(encoded, '}')
	return encoded
}
