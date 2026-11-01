package daemon

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
	"strings"
	"sync"
	"sync/atomic"
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

const (
	// A request that finds every dispatch slot busy waits up to the queue
	// budget for one. At most mcpDispatchQueueFactor times the pool capacity
	// may wait; past that the request is refused at once.
	defaultMCPDispatchQueueWait = 5 * time.Second
	maxMCPDispatchQueueWait     = 30 * time.Second
	mcpDispatchQueueFactor      = 16
	mcpDispatchQueueWaitEnv     = "GORTEX_MCP_DISPATCH_QUEUE_WAIT"
	mcpDispatchBusyRetryAfter   = time.Second
)

// mcpDispatchQueueWait parses the queue budget. "0" or "off" disables the
// queue and restores the instant busy refusal.
func mcpDispatchQueueWait(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultMCPDispatchQueueWait
	}
	if strings.EqualFold(raw, "off") {
		return 0
	}
	wait, err := time.ParseDuration(raw)
	if err != nil || wait < 0 {
		return defaultMCPDispatchQueueWait
	}
	if wait > maxMCPDispatchQueueWait {
		return maxMCPDispatchQueueWait
	}
	return wait
}

var mcpDispatchQueueWaitDefault = mcpDispatchQueueWait(os.Getenv(mcpDispatchQueueWaitEnv))

// mcpDispatchQueueWaiterCounts maps each dispatch pool to the number of
// requests waiting for one of its slots. Pools are few and never removed.
var mcpDispatchQueueWaiterCounts sync.Map // chan struct{} -> *atomic.Int32

func mcpDispatchQueueWaiterCount(slots chan struct{}) *atomic.Int32 {
	waiters, _ := mcpDispatchQueueWaiterCounts.LoadOrStore(slots, new(atomic.Int32))
	return waiters.(*atomic.Int32)
}

func mcpDispatchQueueWaiters(slots chan struct{}) int {
	waiters, ok := mcpDispatchQueueWaiterCounts.Load(slots)
	if !ok {
		return 0
	}
	return int(waiters.(*atomic.Int32).Load())
}

var errMCPDispatchSaturated = errors.New("MCP dispatcher capacity is saturated")

// mcpDispatchBusyError is a busy refusal from the dispatch queue: the budget
// ran out, or the queue was full and the request did not wait.
type mcpDispatchBusyError struct{ waited time.Duration }

func (e *mcpDispatchBusyError) Error() string { return errMCPDispatchSaturated.Error() }
func (e *mcpDispatchBusyError) Unwrap() error { return errMCPDispatchSaturated }

var errMCPDispatchNotStarted = errors.New("MCP dispatch did not start")

// mcpDispatchNotStarted reports whether err proves the handler never ran.
func mcpDispatchNotStarted(err error) bool {
	return errors.Is(err, errMCPDispatchNotStarted) || errors.Is(err, errMCPDispatchSaturated)
}

type mcpDispatchAdmission struct {
	once sync.Once
	done chan struct{}
}

type mcpDispatchAdmissionContextKey struct{}
type mcpDispatchWaitContextKey struct{}
type mcpDispatchSlotsContextKey struct{}
type mcpDispatchQueueWaitContextKey struct{}
type mcpDispatchUnboundedContextKey struct{}
type mcpDispatchSlotLeaseContextKey struct{}

// mcpDispatchSlotLease lets the handler running a dispatch give its slot back
// before it returns.
type mcpDispatchSlotLease struct {
	mu           sync.Mutex
	grant        *mcpDispatchSlotGrant // the attached dispatch's release; nil when none
	timing       MCPDispatchTiming     // immutable original admission, guarded by mu
	requestIDKey string                // immutable validated wire key, assigned before dispatch
}

// mcpDispatchSlotGrant carries one dispatch's slot release. Its address
// identifies that dispatch, so a stale detach cannot clear a later attach.
type mcpDispatchSlotGrant struct {
	release func()
}

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

// withMCPDispatchSlotLease gives one request an empty lease. A dispatch that
// takes a slot attaches its release to the lease while its handler runs.
func withMCPDispatchSlotLease(ctx context.Context) context.Context {
	return withMCPDispatchSlotLeaseForRequest(ctx, "")
}

func withMCPDispatchSlotLeaseForRequest(ctx context.Context, requestIDKey string) context.Context {
	return context.WithValue(ctx, mcpDispatchSlotLeaseContextKey{}, &mcpDispatchSlotLease{requestIDKey: requestIDKey})
}

func mcpDispatchSlotLeaseForContext(ctx context.Context) *mcpDispatchSlotLease {
	lease, _ := ctx.Value(mcpDispatchSlotLeaseContextKey{}).(*mcpDispatchSlotLease)
	return lease
}

// attach hands release to the handler and returns the grant that detaches it.
// A nil lease attaches nothing.
func (l *mcpDispatchSlotLease) attach(release func()) *mcpDispatchSlotGrant {
	if l == nil {
		return nil
	}
	grant := &mcpDispatchSlotGrant{release: release}
	l.mu.Lock()
	l.grant = grant
	l.mu.Unlock()
	return grant
}

// detach takes back grant if it is still attached. It is idempotent.
func (l *mcpDispatchSlotLease) detach(grant *mcpDispatchSlotGrant) {
	if l == nil || grant == nil {
		return
	}
	l.mu.Lock()
	if l.grant == grant {
		l.grant = nil
	}
	l.mu.Unlock()
}

// ReleaseMCPDispatchSlot returns the calling request's dispatch slot to the
// pool before its handler returns. A handler calls it when it starts a
// passive wait and has no further heavy work: the rest of the handler runs
// outside the dispatch cap. It reports whether a slot was returned; it is a
// no-op without a daemon dispatch in progress and on every call after the
// first.
func ReleaseMCPDispatchSlot(ctx context.Context) bool {
	lease := mcpDispatchSlotLeaseForContext(ctx)
	if lease == nil {
		return false
	}
	lease.mu.Lock()
	grant := lease.grant
	lease.grant = nil
	lease.mu.Unlock()
	if grant == nil {
		return false
	}
	grant.release()
	return true
}

func withMCPDispatchQueueWait(ctx context.Context, wait time.Duration) context.Context {
	return context.WithValue(ctx, mcpDispatchQueueWaitContextKey{}, wait)
}

func mcpDispatchQueueWaitForContext(ctx context.Context) time.Duration {
	if wait, ok := ctx.Value(mcpDispatchQueueWaitContextKey{}).(time.Duration); ok {
		return wait
	}
	return mcpDispatchQueueWaitDefault
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
	lease := mcpDispatchSlotLeaseForContext(ctx)
	timing := lease.beginTiming()
	if wait, _ := ctx.Value(mcpDispatchWaitContextKey{}).(bool); wait {
		select {
		case slots <- struct{}{}:
			timing.granted()
		case <-ctx.Done():
			signalMCPDispatchAdmission(ctx)
			return nil, ctx.Err()
		}
	} else {
		select {
		case slots <- struct{}{}:
			timing.granted()
		default:
			if err := awaitMCPDispatchSlot(ctx, slots, timing); err != nil {
				return nil, err
			}
		}
	}

	result := make(chan mcpDispatchResult, 1)
	started := make(chan struct{})
	go func() {
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { <-slots }) }
		defer release() // Retain the slot until an uncooperative handler returns, including panic unwinding, unless the handler hands it back early through ReleaseMCPDispatchSlot.
		grant := lease.attach(release)
		defer lease.detach(grant) // Registered after release, so it runs first while unwinding.
		close(started)
		reply, err := dispatch()
		lease.detach(grant) // No release handle outlives the response.
		release()           // A terminal response must not overtake its dispatch capacity release.
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

// awaitMCPDispatchSlot waits for a slot of a saturated pool for at most the
// queue budget. Blocked senders are served in arrival order. The slot, the
// budget and cancellation share one select, so a request that takes a slot
// never also reports busy or not started.
func awaitMCPDispatchSlot(ctx context.Context, slots chan struct{}, timing mcpDispatchTimingAttempt) error {
	wait := mcpDispatchQueueWaitForContext(ctx)
	if wait <= 0 {
		signalMCPDispatchAdmission(ctx)
		return errMCPDispatchSaturated
	}
	waiters := mcpDispatchQueueWaiterCount(slots)
	waiterCap := int32(mcpDispatchQueueFactor * cap(slots))
	for {
		queued := waiters.Load()
		if queued >= waiterCap {
			signalMCPDispatchAdmission(ctx)
			return &mcpDispatchBusyError{}
		}
		if waiters.CompareAndSwap(queued, queued+1) {
			break
		}
	}
	defer waiters.Add(-1)
	// Count the waiter before freeing the reader so the next frame observes it.
	signalMCPDispatchAdmission(ctx)

	queuedAt := time.Now()
	budget := time.NewTimer(wait)
	defer budget.Stop()
	select {
	case slots <- struct{}{}:
		timing.granted()
		return nil
	case <-budget.C:
		return &mcpDispatchBusyError{waited: time.Since(queuedAt)}
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", errMCPDispatchNotStarted, ctx.Err())
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
	// dispatchQueueWait, if set, overrides the dispatch queue budget for
	// this connection; zero disables the queue.
	dispatchQueueWait *time.Duration
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
	if hooks != nil && hooks.dispatchQueueWait != nil {
		connectionCtx = withMCPDispatchQueueWait(connectionCtx, *hooks.dispatchQueueWait)
	}
	registry := &mcpRequestRegistry{}
	writer := &mcpResponseWriter{conn: conn}
	if hooks != nil && hooks.onReady != nil {
		hooks.onReady(writer.write)
	}
	var requests sync.WaitGroup
	var initializeDone <-chan struct{}
	var initializedDone <-chan struct{}
	// At most one request per connection waits for the initialized notification.
	// Keep the reader free for cancellation without an unbounded pending queue.
	initializedWaiter := make(chan struct{}, 1)
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

	launchNotification := func(line []byte, waitFor <-chan struct{}) <-chan struct{} {
		done := make(chan struct{})
		requests.Add(1)
		go func() {
			defer requests.Done()
			defer close(done)
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
		return done
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
		var waitForInitialized <-chan struct{}
		var admissionErr error
		if initializedDone != nil {
			select {
			case <-initializedDone:
			default:
				select {
				case initializedWaiter <- struct{}{}:
					waitForInitialized = initializedDone
				default:
					admissionErr = errMCPDispatchSaturated
				}
			}
		}
		requestLine := append([]byte(nil), line...)
		requestID := append(json.RawMessage(nil), responseID...)
		admission := newMCPDispatchAdmission()
		dispatchCtx := withMCPDispatchSlotLeaseForRequest(withMCPDispatchAdmission(requestCtx, admission), entry.key)
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

			if waitForInitialized != nil {
				// The request is registered but has not entered the dispatcher.
				// Let the reader process cancellation while this bounded waiter
				// preserves notification-before-tool protocol order.
				signalMCPDispatchAdmission(dispatchCtx)
				select {
				case <-waitForInitialized:
				case <-requestCtx.Done():
				}
				<-initializedWaiter
			}
			var reply []byte
			dispatchErr := admissionErr
			dispatchInvoked := false
			if requestErr := requestCtx.Err(); requestErr != nil {
				dispatchErr = requestErr
				signalMCPDispatchAdmission(dispatchCtx)
			} else if dispatchErr != nil {
				signalMCPDispatchAdmission(dispatchCtx)
			} else {
				dispatchInvoked = true
				reply, dispatchErr = dispatch(dispatchCtx, requestLine)
			}
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
					data["delivery_unknown"] = dispatchInvoked && !mcpDispatchNotStarted(dispatchErr)
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
				var busy *mcpDispatchBusyError
				if errors.As(dispatchErr, &busy) {
					data["queue_wait_ms"] = busy.waited.Milliseconds()
					data["retry_after_ms"] = mcpDispatchBusyRetryAfter.Milliseconds()
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
			done := launchNotification(append([]byte(nil), line...), waitFor)
			if envelope.Method == "notifications/initialized" {
				initializedDone = done
			}
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
