package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestServeMCPRegistryReleasedBeforeTerminalResponse(t *testing.T) {
	var calls atomic.Int32
	firstResponseReady := make(chan struct{})
	releaseFirstResponse := make(chan struct{})
	var blockFirst atomic.Bool
	hooks := &mcpConnectionHooks{beforeResponse: func(id json.RawMessage) {
		if string(id) == "41" && blockFirst.CompareAndSwap(false, true) {
			close(firstResponseReady)
			<-releaseFirstResponse
		}
	}}
	dispatch := func(ctx context.Context, _ []byte) ([]byte, error) {
		if calls.Add(1) == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return []byte(`{"jsonrpc":"2.0","id":41,"result":{"retried":true}}`), nil
	}
	conn, reader, done := startMCPConnectionWithHooksTest(t, 20*time.Millisecond, dispatch, hooks)
	request := `{"jsonrpc":"2.0","id":41,"method":"tools/call","params":{"name":"search"}}`

	writeMCPTestFrame(t, conn, request)
	awaitMCPTestSignal(t, firstResponseReady, "first terminal response did not reach publish barrier")
	writeMCPTestFrame(t, conn, request)
	response := readMCPTestResponse(t, conn, reader)
	if response.Error != nil || response.Result["retried"] != true {
		t.Fatalf("immediate retry response = %#v", response)
	}
	close(releaseFirstResponse)
	response = readMCPTestResponse(t, conn, reader)
	if response.Error == nil || response.Error.Code != mcpRequestTimeoutCode {
		t.Fatalf("original terminal response = %#v", response)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("dispatch calls = %d, want 2", got)
	}
	_ = conn.Close()
	awaitMCPTestSignal(t, done, "connection did not stop")
}

func awaitMCPDispatchSlotsReleased(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(mcpDispatchSlots) == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("MCP dispatch slots still occupied: %d", len(mcpDispatchSlots))
}

func TestSessionDispatchMCPOnceContextRetriesCancelledForeignFlight(t *testing.T) {
	sess := &Session{LogicalSessionID: "reconnect-cancel"}
	request := []byte(`{"jsonrpc":"2.0","id":77,"method":"tools/call","params":{"name":"search"}}`)
	oldCtx, cancelOld := context.WithCancel(context.Background())
	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	oldHandlerReturned := make(chan struct{})
	oldDone := make(chan error, 1)
	go func() {
		_, _, err := sess.dispatchMCPOnceContext(oldCtx, request, func() ([]byte, error) {
			close(oldStarted)
			<-releaseOld
			close(oldHandlerReturned)
			return []byte(`{"jsonrpc":"2.0","id":77,"result":{"old":true}}`), nil
		})
		oldDone <- err
	}()
	awaitMCPTestSignal(t, oldStarted, "old connection flight did not start")

	newStarted := make(chan struct{})
	type dispatchResult struct {
		reply    []byte
		replayed bool
		err      error
	}
	newDone := make(chan dispatchResult, 1)
	go func() {
		reply, replayed, err := sess.dispatchMCPOnceContext(context.Background(), request, func() ([]byte, error) {
			close(newStarted)
			return []byte(`{"jsonrpc":"2.0","id":77,"result":{"new":true}}`), nil
		})
		newDone <- dispatchResult{reply: reply, replayed: replayed, err: err}
	}()
	select {
	case <-newStarted:
		t.Fatal("new connection bypassed the live old flight")
	case <-time.After(10 * time.Millisecond):
	}

	cancelOld()
	select {
	case err := <-oldDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("old flight error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("old cancelled flight did not detach")
	}
	awaitMCPTestSignal(t, newStarted, "new connection did not retry cancelled flight")
	result := <-newDone
	if result.err != nil || result.replayed || !json.Valid(result.reply) {
		t.Fatalf("new flight result = %#v", result)
	}
	close(releaseOld)
	awaitMCPTestSignal(t, oldHandlerReturned, "old orphan handler did not return")
	awaitMCPDispatchSlotsReleased(t)

	called := false
	reply, replayed, err := sess.dispatchMCPOnceContext(context.Background(), request, func() ([]byte, error) {
		called = true
		return nil, errors.New("cached response was lost")
	})
	if err != nil || !replayed || called || string(reply) != string(result.reply) {
		t.Fatalf("cached replacement = reply:%s replayed:%v called:%v err:%v", reply, replayed, called, err)
	}
}

func TestServeMCPUncooperativeNonToolDoesNotBlockNextRequestOrTeardown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handlerReturned := make(chan struct{})
	dispatcher := testMCPDispatcherFunc(func(_ context.Context, _ *Session, frame []byte) ([]byte, error) {
		var envelope mcpRequestEnvelope
		if err := json.Unmarshal(frame, &envelope); err != nil {
			return nil, err
		}
		if string(envelope.ID) == "1" {
			close(started)
			<-release
			close(handlerReturned)
			return []byte(`{"jsonrpc":"2.0","id":1,"result":{"late":true}}`), nil
		}
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"ok":true}}`), nil
	})
	server := &Server{MCPDispatcher: dispatcher, MCPToolCallTimeout: 40 * time.Millisecond}
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.serveMCP(serverConn, bufio.NewReader(serverConn), &Session{})
	}()

	writeMCPTestFrame(t, clientConn, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	awaitMCPTestSignal(t, started, "uncooperative non-tool request did not start")
	writeMCPTestFrame(t, clientConn, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	reader := bufio.NewReader(clientConn)
	response := readMCPTestResponse(t, clientConn, reader)
	if string(response.ID) != "2" || response.Error != nil || response.Result["ok"] != true {
		t.Fatalf("next request response = %#v", response)
	}
	response = readMCPTestResponse(t, clientConn, reader)
	if string(response.ID) != "1" || response.Error == nil || response.Error.Code != mcpRequestTimeoutCode {
		t.Fatalf("uncooperative timeout response = %#v", response)
	}
	_ = clientConn.Close()
	awaitMCPTestSignal(t, done, "connection teardown waited for uncooperative dispatcher")
	close(release)
	select {
	case <-handlerReturned:
	case <-time.After(time.Second):
		t.Fatal("uncooperative dispatcher did not return after release")
	}
	awaitMCPDispatchSlotsReleased(t)
}

func TestServeMCPCancellationCanonicalizesEquivalentIDs(t *testing.T) {
	tests := []struct {
		name      string
		requestID string
		cancelID  string
	}{
		{name: "escaped string", requestID: `"<"`, cancelID: `"\u003c"`},
		{name: "large number", requestID: `9007199254740993`, cancelID: `9007199254740993`},
		{name: "null compatibility", requestID: `null`, cancelID: `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			started := make(chan struct{})
			conn, reader, _ := startMCPConnectionTest(t, time.Second, func(ctx context.Context, _ []byte) ([]byte, error) {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			})
			writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":`+tt.requestID+`,"method":"tools/call","params":{"name":"search"}}`)
			awaitMCPTestSignal(t, started, "request did not start")
			writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":`+tt.cancelID+`}}`)
			response := readMCPTestResponse(t, conn, reader)
			if string(response.ID) != tt.requestID || response.Error == nil || response.Error.Code != mcpRequestCancelledCode {
				t.Fatalf("canonical cancellation response = %#v", response)
			}
		})
	}
}

func TestServeMCPRejectsInvalidRequestAndCancellationIDs(t *testing.T) {
	var calls atomic.Int32
	conn, reader, _ := startMCPConnectionTest(t, time.Second, func(context.Context, []byte) ([]byte, error) {
		calls.Add(1)
		return nil, nil
	})
	for _, invalidID := range []string{`{}`, `[]`, `true`} {
		writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":`+invalidID+`,"method":"tools/call","params":{"name":"search"}}`)
		response := readMCPTestResponse(t, conn, reader)
		if string(response.ID) != "null" || response.Error == nil || response.Error.Code != -32600 {
			t.Fatalf("invalid id %s response = %#v", invalidID, response)
		}
	}
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":5,"method":"notifications/cancelled","params":{"requestId":1}}`)
	response := readMCPTestResponse(t, conn, reader)
	if string(response.ID) != "5" || response.Error == nil || response.Error.Code != -32600 {
		t.Fatalf("request-shaped cancellation response = %#v", response)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("invalid requests reached dispatcher %d times", got)
	}
}

func TestServeMCPInvalidCancellationTargetDoesNotCancelRequest(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan error, 1)
	conn, reader, _ := startMCPConnectionTest(t, time.Second, func(ctx context.Context, _ []byte) ([]byte, error) {
		close(started)
		<-ctx.Done()
		cancelled <- ctx.Err()
		return nil, ctx.Err()
	})
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":"keep","method":"tools/call","params":{"name":"search"}}`)
	awaitMCPTestSignal(t, started, "request did not start")
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":{}}}`)
	select {
	case err := <-cancelled:
		t.Fatalf("invalid cancellation target cancelled request: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"keep"}}`)
	response := readMCPTestResponse(t, conn, reader)
	if response.Error == nil || response.Error.Code != mcpRequestCancelledCode {
		t.Fatalf("valid cancellation response = %#v", response)
	}
}

func TestServeMCPRequestsAlwaysReceiveDispatchTerminalResponse(t *testing.T) {
	conn, reader, _ := startMCPConnectionTest(t, time.Second, func(_ context.Context, frame []byte) ([]byte, error) {
		var envelope mcpRequestEnvelope
		if err := json.Unmarshal(frame, &envelope); err != nil {
			return nil, err
		}
		if string(envelope.ID) == "1" {
			return nil, errors.New("boom")
		}
		return nil, nil
	})
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	seen := map[string]string{}
	for range 2 {
		response := readMCPTestResponse(t, conn, reader)
		if response.Error == nil || response.Error.Code != -32603 {
			t.Fatalf("terminal error response = %#v", response)
		}
		seen[string(response.ID)] = response.Error.Data["outcome"].(string)
	}
	if seen["1"] != "dispatch_error" || seen["2"] != "empty_response" {
		t.Fatalf("terminal outcomes = %#v", seen)
	}
}

func TestServeMCPMutationTimeoutMarksDeliveryUnknown(t *testing.T) {
	conn, reader, _ := startMCPConnectionTest(t, 20*time.Millisecond, func(ctx context.Context, _ []byte) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":"mutation","method":"tools/call","params":{"name":"edit"}}`)
	response := readMCPTestResponse(t, conn, reader)
	if response.Error == nil || response.Error.Code != mcpRequestTimeoutCode {
		t.Fatalf("mutation timeout = %#v", response)
	}
	if response.Error.Data["delivery_unknown"] != true || response.Error.Data["retryable"] != false {
		t.Fatalf("mutation timeout safety data = %#v", response.Error.Data)
	}
}

func TestServeMCPInitializedNotificationWaitsForInitializeResponse(t *testing.T) {
	testSlots := newMCPDispatchSlots(1)
	initializeStarted := make(chan struct{})
	releaseInitialize := make(chan struct{})
	initializedDispatched := make(chan struct{})
	slotHeld := make(chan struct{})
	var heldSlotReleased atomic.Bool
	t.Cleanup(func() {
		if heldSlotReleased.Load() {
			return
		}
		select {
		case <-slotHeld:
			select {
			case <-testSlots:
			default:
			}
		default:
		}
	})
	hooks := &mcpConnectionHooks{dispatchSlots: testSlots, beforeResponse: func(id json.RawMessage) {
		if string(id) != "1" {
			return
		}
		testSlots <- struct{}{}
		close(slotHeld)
	}}
	conn, reader, _ := startMCPConnectionWithHooksTest(t, time.Second, func(_ context.Context, frame []byte) ([]byte, error) {
		var envelope mcpRequestEnvelope
		if err := json.Unmarshal(frame, &envelope); err != nil {
			return nil, err
		}
		switch envelope.Method {
		case "initialize":
			close(initializeStarted)
			<-releaseInitialize
			return []byte(`{"jsonrpc":"2.0","id":1,"result":{"ready":true}}`), nil
		case "notifications/initialized":
			close(initializedDispatched)
		}
		return nil, nil
	}, hooks)
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	awaitMCPTestSignal(t, initializeStarted, "initialize did not start")
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	select {
	case <-initializedDispatched:
		t.Fatal("initialized notification overtook initialize response")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseInitialize)
	awaitMCPTestSignal(t, slotHeld, "test did not occupy the only dispatch slot")
	response := readMCPTestResponse(t, conn, reader)
	if response.Error != nil || response.Result["ready"] != true {
		t.Fatalf("initialize response = %#v", response)
	}
	select {
	case <-initializedDispatched:
		t.Fatal("initialized notification bypassed occupied dispatch capacity")
	case <-time.After(20 * time.Millisecond):
	}
	<-testSlots
	heldSlotReleased.Store(true)
	awaitMCPTestSignal(t, initializedDispatched, "initialized notification was dropped after capacity became available")
}

func TestMCPDispatchLimitConfiguration(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want int
	}{
		{name: "default", want: defaultMaxConcurrentMCPDispatches},
		{name: "whitespace", raw: "  ", want: defaultMaxConcurrentMCPDispatches},
		{name: "small override", raw: "2", want: 2},
		{name: "trimmed override", raw: " 16 ", want: 16},
		{name: "invalid", raw: "many", want: defaultMaxConcurrentMCPDispatches},
		{name: "zero", raw: "0", want: defaultMaxConcurrentMCPDispatches},
		{name: "negative", raw: "-1", want: defaultMaxConcurrentMCPDispatches},
		{name: "bounded override", raw: "65", want: maxConfiguredMCPDispatches},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := mcpDispatchLimit(test.raw); got != test.want {
				t.Fatalf("mcpDispatchLimit(%q) = %d, want %d", test.raw, got, test.want)
			}
		})
	}
	if got := cap(newMCPDispatchSlots(0)); got != defaultMaxConcurrentMCPDispatches {
		t.Fatalf("zero-capacity test override = %d, want %d", got, defaultMaxConcurrentMCPDispatches)
	}
}

func TestServeMCPDispatcherSaturationWithQueueDisabledFailsFastAndKeepsReaderResponsive(t *testing.T) {
	filled := 0
	for {
		select {
		case mcpDispatchSlots <- struct{}{}:
			filled++
		default:
			goto saturated
		}
	}

saturated:
	defer func() {
		for range filled {
			<-mcpDispatchSlots
		}
	}()
	var dispatches atomic.Int32
	var zero time.Duration
	conn, reader, done := startMCPConnectionWithHooksTest(t, time.Second, func(context.Context, []byte) ([]byte, error) {
		dispatches.Add(1)
		return nil, errors.New("saturated dispatcher unexpectedly ran")
	}, &mcpConnectionHooks{dispatchQueueWait: &zero})
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"edit"}}`)
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	responses := map[string]testMCPResponse{}
	for range 2 {
		response := readMCPTestResponse(t, conn, reader)
		responses[string(response.ID)] = response
	}
	for _, id := range []string{"1", "2"} {
		response := responses[id]
		if response.Error == nil || response.Error.Code != -32002 || response.Error.Data["outcome"] != "server_busy" {
			t.Fatalf("busy response %s = %#v", id, response)
		}
		// Lane admission supplies bounded retry guidance even with queueing off;
		// this remains a not-started refusal and does not relax lifetime safety.
		retry, ok := response.Error.Data["retry_after_ms"].(float64)
		if !ok || retry < 100 || retry > 5000 {
			t.Fatalf("busy response %s retry=%#v", id, response.Error.Data["retry_after_ms"])
		}
		if waited, ok := response.Error.Data["queue_wait_ms"].(float64); !ok || waited != 0 {
			t.Fatalf("queue-off response %s wait=%#v", id, response.Error.Data["queue_wait_ms"])
		}
	}
	if responses["1"].Error.Data["delivery_unknown"] != false || responses["1"].Error.Data["retryable"] != true {
		t.Fatalf("not-started mutation busy data = %#v", responses["1"].Error.Data)
	}
	if got := dispatches.Load(); got != 0 {
		t.Fatalf("saturated dispatcher ran %d times", got)
	}
	_ = conn.Close()
	awaitMCPTestSignal(t, done, "saturated connection did not stop")
}

func startMCPConnectionWithHooksTest(t *testing.T, timeout time.Duration, dispatch mcpFrameDispatch, hooks *mcpConnectionHooks) (net.Conn, *bufio.Reader, <-chan struct{}) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		serveMCPConnectionWithHooks(serverConn, bufio.NewReader(serverConn), timeout, func(ctx context.Context, frame []byte) ([]byte, error) {
			return dispatchMCPWithContext(ctx, func() ([]byte, error) { return dispatch(ctx, frame) })
		}, hooks)
	}()
	var closeOnce sync.Once
	t.Cleanup(func() {
		closeOnce.Do(func() { _ = clientConn.Close() })
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("MCP connection with hooks did not stop")
		}
	})
	return clientConn, bufio.NewReader(clientConn), done
}

func TestServeMCPSubscriptionsListenStaysOpenWithoutTimeoutOrSlotConsumption(t *testing.T) {
	listenStarted := make(chan struct{})
	slots := make(chan struct{}, 1)
	hooks := &mcpConnectionHooks{dispatchSlots: slots}
	conn, reader, done := startMCPConnectionWithHooksTest(t, 20*time.Millisecond, func(ctx context.Context, frame []byte) ([]byte, error) {
		var envelope mcpRequestEnvelope
		if err := json.Unmarshal(frame, &envelope); err != nil {
			return nil, err
		}
		if envelope.Method == "subscriptions/listen" {
			if _, hasDeadline := ctx.Deadline(); hasDeadline {
				return nil, errors.New("subscriptions/listen must not carry the per-request tool-call deadline")
			}
			close(listenStarted)
			<-ctx.Done()
			return []byte(`{"jsonrpc":"2.0","id":2,"result":{}}`), nil
		}
		return []byte(`{"jsonrpc":"2.0","id":3,"result":{"ok":true}}`), nil
	}, hooks)

	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":2,"method":"subscriptions/listen","params":{}}`)
	awaitMCPTestSignal(t, listenStarted, "subscriptions/listen did not start")

	// Wait longer than the 20ms per-request timeout and confirm the stream does not
	// time out or occupy the single bounded dispatch slot.
	time.Sleep(50 * time.Millisecond)
	if len(slots) != 0 {
		t.Fatalf("subscriptions/listen occupied %d bounded dispatch slots, want 0", len(slots))
	}

	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`)
	response := readMCPTestResponse(t, conn, reader)
	if string(response.ID) != "3" || response.Error != nil || response.Result["ok"] != true {
		t.Fatalf("concurrent request while subscriptions/listen active = %#v", response)
	}

	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`)
	cancelResp := readMCPTestResponse(t, conn, reader)
	if string(cancelResp.ID) != "2" || cancelResp.Error != nil {
		t.Fatalf("subscriptions/listen cancellation response = %#v", cancelResp)
	}
	_ = conn.Close()
	awaitMCPTestSignal(t, done, "connection did not stop")
}
