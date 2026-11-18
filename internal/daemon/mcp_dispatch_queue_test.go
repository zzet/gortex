package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

// mcpDispatchQueueNoopMarker cancels nothing. On net.Pipe its write completes
// only after the reader has passed the previous request's admission boundary.
const mcpDispatchQueueNoopMarker = `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"not-in-flight"}}`

const mcpDispatchQueueNotification = "notifications/roots/list_changed"

type mcpDispatchQueueFixture struct {
	pool          chan struct{}
	conn          net.Conn
	reader        *bufio.Reader
	holderStarted chan struct{}
	releaseHolder func()
	notified      chan struct{}

	mu    sync.Mutex
	calls map[string]int
}

// startMCPDispatchQueueFixture serves one connection over its own pool of one
// slot. Request id 1 holds that slot, ignoring its context, until released;
// every other handler returns at once.
func startMCPDispatchQueueFixture(t *testing.T, timeout, queueWait time.Duration) *mcpDispatchQueueFixture {
	t.Helper()
	f := &mcpDispatchQueueFixture{
		pool:          newMCPDispatchSlots(1),
		holderStarted: make(chan struct{}),
		notified:      make(chan struct{}),
		calls:         map[string]int{},
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	f.releaseHolder = func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(f.releaseHolder)
	var notifyOnce sync.Once
	dispatch := func(_ context.Context, frame []byte) ([]byte, error) {
		var envelope mcpRequestEnvelope
		if err := json.Unmarshal(frame, &envelope); err != nil {
			return nil, err
		}
		if len(envelope.ID) == 0 {
			f.record(envelope.Method)
			notifyOnce.Do(func() { close(f.notified) })
			return nil, nil
		}
		f.record(string(envelope.ID))
		if string(envelope.ID) == "1" {
			close(f.holderStarted)
			<-release
		}
		return []byte(`{"jsonrpc":"2.0","id":` + string(envelope.ID) + `,"result":{"ok":true}}`), nil
	}
	f.conn, f.reader, _ = startMCPConnectionWithHooksTest(t, timeout, dispatch, &mcpConnectionHooks{
		dispatchSlots:     f.pool,
		dispatchQueueWait: &queueWait,
	})
	return f
}

func (f *mcpDispatchQueueFixture) record(key string) {
	f.mu.Lock()
	f.calls[key]++
	f.mu.Unlock()
}

func (f *mcpDispatchQueueFixture) callCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

func (f *mcpDispatchQueueFixture) hold(t *testing.T) {
	t.Helper()
	writeMCPTestFrame(t, f.conn, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"edit"}}`)
	awaitMCPTestSignal(t, f.holderStarted, "holder did not take the only dispatch slot")
}

// queue sends a tool call that must wait for the held slot, then the marker so
// the call is counted as a waiter before the next step.
func (f *mcpDispatchQueueFixture) queue(t *testing.T, id string) {
	t.Helper()
	writeMCPTestFrame(t, f.conn, mcpDispatchQueueToolCall(id))
	writeMCPTestFrame(t, f.conn, mcpDispatchQueueNoopMarker)
}

func mcpDispatchQueueToolCall(id string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"read"}}`
}

func readMCPTestResponsesByID(t *testing.T, conn net.Conn, reader *bufio.Reader, count int) map[string]testMCPResponse {
	t.Helper()
	responses := make(map[string]testMCPResponse, count)
	for range count {
		response := readMCPTestResponse(t, conn, reader)
		if _, duplicate := responses[string(response.ID)]; duplicate {
			t.Fatalf("duplicate response for id %s: %#v", response.ID, response)
		}
		responses[string(response.ID)] = response
	}
	return responses
}

func awaitMCPDispatchQueueWaiters(t *testing.T, pool chan struct{}, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if mcpDispatchQueueWaiters(pool) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("dispatch queue waiters = %d, want %d", mcpDispatchQueueWaiters(pool), want)
}

// assertMCPDispatchQueueDrained runs after the last terminal reply was read.
// Waiters leave the queue before their reply is written; the slot release may
// trail the reply where it is deferred past the result send, so poll for it.
func assertMCPDispatchQueueDrained(t *testing.T, pool chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(pool) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(pool); got != 0 {
		t.Fatalf("dispatch pool still holds %d slots", got)
	}
	if got := mcpDispatchQueueWaiters(pool); got != 0 {
		t.Fatalf("dispatch queue still counts %d waiters", got)
	}
}

func assertMCPDispatchQueueSucceeded(t *testing.T, responses map[string]testMCPResponse, ids ...string) {
	t.Helper()
	for _, id := range ids {
		response, ok := responses[id]
		if !ok {
			t.Fatalf("no response for id %s in %#v", id, responses)
		}
		if response.Error != nil || response.Result["ok"] != true {
			t.Fatalf("response %s = %#v", id, response)
		}
	}
}

func assertMCPDispatchQueueBusy(t *testing.T, response testMCPResponse, id string) map[string]any {
	t.Helper()
	if string(response.ID) != id || response.Error == nil || response.Error.Code != -32002 {
		t.Fatalf("busy response %s = %#v", id, response)
	}
	data := response.Error.Data
	if data["outcome"] != "server_busy" || data["retryable"] != true || data["delivery_unknown"] != false {
		t.Fatalf("busy response %s data = %#v", id, data)
	}
	return data
}

func TestServeMCPDispatchQueueWaitsForSlotThenSucceeds(t *testing.T) {
	f := startMCPDispatchQueueFixture(t, 5*time.Second, 2*time.Second)
	f.hold(t)
	f.queue(t, "2")
	awaitMCPDispatchQueueWaiters(t, f.pool, 1)
	if got := f.callCount("2"); got != 0 {
		t.Fatalf("queued request ran %d times before the slot freed", got)
	}

	f.releaseHolder()
	responses := readMCPTestResponsesByID(t, f.conn, f.reader, 2)
	for id, response := range responses {
		if response.Error != nil && response.Error.Code == -32002 {
			t.Fatalf("response %s refused as busy: %#v", id, response)
		}
	}
	assertMCPDispatchQueueSucceeded(t, responses, "1", "2")
	if got := f.callCount("2"); got != 1 {
		t.Fatalf("queued request ran %d times, want 1", got)
	}
	assertMCPDispatchQueueDrained(t, f.pool)
}

func TestServeMCPDispatchQueueBudgetExpiresAsBusy(t *testing.T) {
	f := startMCPDispatchQueueFixture(t, 5*time.Second, 200*time.Millisecond)
	f.hold(t)
	writeMCPTestFrame(t, f.conn, mcpDispatchQueueToolCall("2"))

	data := assertMCPDispatchQueueBusy(t, readMCPTestResponse(t, f.conn, f.reader), "2")
	if waited, ok := data["queue_wait_ms"].(float64); !ok || waited < 200 {
		t.Fatalf("queue_wait_ms = %#v, want at least 200", data["queue_wait_ms"])
	}
	if data["retry_after_ms"] != float64(1000) {
		t.Fatalf("retry_after_ms = %#v, want 1000", data["retry_after_ms"])
	}
	if got := f.callCount("2"); got != 0 {
		t.Fatalf("busy request ran %d times", got)
	}

	f.releaseHolder()
	assertMCPDispatchQueueSucceeded(t, readMCPTestResponsesByID(t, f.conn, f.reader, 1), "1")
	assertMCPDispatchQueueDrained(t, f.pool)
}

func TestServeMCPDispatchQueueFullRefusesRequestsButNotNotifications(t *testing.T) {
	f := startMCPDispatchQueueFixture(t, 5*time.Second, 5*time.Second)
	f.hold(t)
	n := mcpDispatchQueueFactor
	waiters := make([]string, 0, n)
	for i := range n {
		id := strconv.Itoa(i + 2)
		waiters = append(waiters, id)
		f.queue(t, id)
	}
	awaitMCPDispatchQueueWaiters(t, f.pool, n)

	overflow := strconv.Itoa(n + 2)
	writeMCPTestFrame(t, f.conn, mcpDispatchQueueToolCall(overflow))
	data := assertMCPDispatchQueueBusy(t, readMCPTestResponse(t, f.conn, f.reader), overflow)
	if data["queue_wait_ms"] != float64(0) || data["retry_after_ms"] != float64(1000) {
		t.Fatalf("queue-full busy data = %#v", data)
	}
	if got := f.callCount(overflow); got != 0 {
		t.Fatalf("refused request ran %d times", got)
	}
	for _, id := range waiters {
		if got := f.callCount(id); got != 0 {
			t.Fatalf("waiter %s ran %d times while the slot was held", id, got)
		}
	}
	// A notification is never refused, even with the queue full.
	writeMCPTestFrame(t, f.conn, `{"jsonrpc":"2.0","method":"`+mcpDispatchQueueNotification+`"}`)

	f.releaseHolder()
	awaitMCPTestSignal(t, f.notified, "notification was not dispatched once capacity freed")
	responses := readMCPTestResponsesByID(t, f.conn, f.reader, n+1)
	assertMCPDispatchQueueSucceeded(t, responses, append([]string{"1"}, waiters...)...)
	for _, id := range waiters {
		if got := f.callCount(id); got != 1 {
			t.Fatalf("waiter %s ran %d times, want 1", id, got)
		}
	}
	// No other frame precedes the sentinel's reply.
	writeMCPTestFrame(t, f.conn, `{"jsonrpc":"2.0","id":"after","method":"ping"}`)
	if response := readMCPTestResponse(t, f.conn, f.reader); string(response.ID) != `"after"` || response.Error != nil {
		t.Fatalf("frame after the drained replies = %#v", response)
	}
	if got := f.callCount(mcpDispatchQueueNotification); got != 1 {
		t.Fatalf("notification ran %d times, want 1", got)
	}
	assertMCPDispatchQueueDrained(t, f.pool)
}

func TestServeMCPDispatchQueueCancellationWhileQueued(t *testing.T) {
	f := startMCPDispatchQueueFixture(t, 5*time.Second, 5*time.Second)
	f.hold(t)
	for _, id := range []string{"2", "3", "4"} {
		f.queue(t, id)
	}
	awaitMCPDispatchQueueWaiters(t, f.pool, 3)
	writeMCPTestFrame(t, f.conn, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":3}}`)

	response := readMCPTestResponse(t, f.conn, f.reader)
	if string(response.ID) != "3" || response.Error == nil || response.Error.Code != mcpRequestCancelledCode {
		t.Fatalf("queued cancellation response = %#v", response)
	}
	if data := response.Error.Data; data["outcome"] != "cancelled" || data["retryable"] != false || data["delivery_unknown"] != false {
		t.Fatalf("queued cancellation data = %#v", data)
	}
	if got := mcpDispatchQueueWaiters(f.pool); got != 2 {
		t.Fatalf("dispatch queue waiters after cancellation = %d, want 2", got)
	}
	for _, id := range []string{"2", "3", "4"} {
		if got := f.callCount(id); got != 0 {
			t.Fatalf("request %s ran %d times while the slot was held", id, got)
		}
	}

	// The reader still accepts frames while requests wait.
	writeMCPTestFrame(t, f.conn, `{"jsonrpc":"2.0","id":5,"method":"ping"}`)
	f.releaseHolder()
	assertMCPDispatchQueueSucceeded(t, readMCPTestResponsesByID(t, f.conn, f.reader, 4), "1", "2", "4", "5")
	if got := f.callCount("3"); got != 0 {
		t.Fatalf("cancelled queued request ran %d times after the slot freed", got)
	}
	assertMCPDispatchQueueDrained(t, f.pool)
}

func TestServeMCPDispatchQueueDeadlineWhileQueued(t *testing.T) {
	f := startMCPDispatchQueueFixture(t, 300*time.Millisecond, 2*time.Second)
	f.hold(t)
	queuedAt := time.Now()
	writeMCPTestFrame(t, f.conn, mcpDispatchQueueToolCall("2"))

	responses := readMCPTestResponsesByID(t, f.conn, f.reader, 2)
	holder := responses["1"]
	if holder.Error == nil || holder.Error.Code != mcpRequestTimeoutCode || holder.Error.Data["delivery_unknown"] != true {
		t.Fatalf("started holder deadline response = %#v", holder)
	}
	queued := responses["2"]
	if queued.Error == nil || queued.Error.Code != -32002 {
		t.Fatalf("queued deadline response = %#v", queued)
	}
	// The lane budget deliberately reserves 100ms of the request deadline
	// for its not-started busy reply. The exact clamp is separately pinned
	// by TestDispatchSchedulerDeadlineBudgetAndSmallOverrides; allow broad
	// scheduling slack here instead of a tight wall-clock tolerance.
	data := assertMCPDispatchQueueBusy(t, queued, "2")
	if waited, ok := data["queue_wait_ms"].(float64); !ok || waited <= 0 || waited >= 300 {
		t.Fatalf("queued budget did not leave response time: %#v", data)
	}
	if retry, ok := data["retry_after_ms"].(float64); !ok || retry < 100 || retry > 5000 {
		t.Fatalf("queued retry guidance = %#v", data)
	}
	if elapsed := time.Since(queuedAt); elapsed >= 2*time.Second {
		t.Fatalf("deadline-clamped calls consumed configured 2s queue budget: %s", elapsed)
	}
	if got := f.callCount("2"); got != 0 {
		t.Fatalf("queued request ran %d times", got)
	}
}

func TestMCPDispatchQueueWaitConfiguration(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want time.Duration
	}{
		{raw: "", want: 5 * time.Second},
		{raw: "  ", want: 5 * time.Second},
		{raw: "0", want: 0},
		{raw: "0s", want: 0},
		{raw: "off", want: 0},
		{raw: "OFF", want: 0},
		{raw: " 500ms ", want: 500 * time.Millisecond},
		{raw: "500ms", want: 500 * time.Millisecond},
		{raw: "10s", want: 10 * time.Second},
		{raw: "90s", want: 30 * time.Second},
		{raw: "-1s", want: 5 * time.Second},
		{raw: "many", want: 5 * time.Second},
		{raw: "10", want: 5 * time.Second},
	} {
		if got := mcpDispatchQueueWait(test.raw); got != test.want {
			t.Errorf("mcpDispatchQueueWait(%q) = %v, want %v", test.raw, got, test.want)
		}
	}
}
