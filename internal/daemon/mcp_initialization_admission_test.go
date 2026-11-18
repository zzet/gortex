package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestServeMCPEightClientsInitializeThenFirstToolAtDefaultCapacity(t *testing.T) {
	slots := newMCPDispatchSlots(defaultMaxConcurrentMCPDispatches)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unpark := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unpark)
	started := make(chan struct{}, defaultMaxConcurrentMCPDispatches)
	var tools atomic.Int32
	dispatch := func(ctx context.Context, frame []byte) ([]byte, error) {
		var envelope mcpRequestEnvelope
		if err := json.Unmarshal(frame, &envelope); err != nil {
			return nil, err
		}
		switch envelope.Method {
		case "notifications/initialized":
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil, nil
		case "tools/call":
			tools.Add(1)
		}
		return []byte(`{"jsonrpc":"2.0","id":` + string(envelope.ID) + `,"result":{"ok":true}}`), nil
	}
	var conns []net.Conn
	var readers []*bufio.Reader
	for range defaultMaxConcurrentMCPDispatches {
		conn, reader, _ := startMCPConnectionWithHooksTest(t, time.Second, dispatch, &mcpConnectionHooks{dispatchSlots: slots})
		conns = append(conns, conn)
		readers = append(readers, reader)
		writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
		response := readMCPTestResponse(t, conn, reader)
		if response.Error != nil || response.Result["ok"] != true {
			t.Fatalf("initialize response = %#v", response)
		}
		writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		awaitMCPTestSignal(t, started, "initialized handler did not acquire its slot")
	}
	if len(slots) != defaultMaxConcurrentMCPDispatches {
		t.Fatalf("held initialized slots = %d, want %d", len(slots), defaultMaxConcurrentMCPDispatches)
	}
	for _, conn := range conns {
		writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read"}}`)
		// net.Pipe's next write completes only when the reader has advanced
		// past the tool frame's admission boundary. The nonexistent cancel is
		// a no-op marker, not a retry, sleep, or a dispatcher-capacity bypass.
		writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"not-in-flight"}}`)
	}
	if got := tools.Load(); got != 0 {
		t.Fatalf("%d tools overtook their held initialized notification", got)
	}
	unpark()
	for i, conn := range conns {
		response := readMCPTestResponse(t, conn, readers[i])
		if string(response.ID) != "2" || response.Error != nil || response.Result["ok"] != true {
			t.Fatalf("client %d first tool response = %#v", i, response)
		}
	}
	if got := tools.Load(); got != defaultMaxConcurrentMCPDispatches {
		t.Fatalf("tool dispatches = %d, want %d", got, defaultMaxConcurrentMCPDispatches)
	}
}

func TestServeMCPInitializedWaitKeepsCancellationResponsive(t *testing.T) {
	slots := newMCPDispatchSlots(1)
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unpark := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unpark)
	var tools atomic.Int32
	conn, reader, _ := startMCPConnectionWithHooksTest(t, time.Second, func(ctx context.Context, frame []byte) ([]byte, error) {
		var envelope mcpRequestEnvelope
		if err := json.Unmarshal(frame, &envelope); err != nil {
			return nil, err
		}
		if envelope.Method == "notifications/initialized" {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil, nil
		}
		tools.Add(1)
		return []byte(`{"jsonrpc":"2.0","id":` + string(envelope.ID) + `,"result":{"ok":true}}`), nil
	}, &mcpConnectionHooks{dispatchSlots: slots})
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	awaitMCPTestSignal(t, started, "initialized handler did not start")
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"edit"}}`)
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`)
	response := readMCPTestResponse(t, conn, reader)
	if response.Error == nil || response.Error.Code != mcpRequestCancelledCode || response.Error.Data["delivery_unknown"] != false || response.Error.Data["retryable"] != false {
		t.Fatalf("not-started cancellation response = %#v", response)
	}
	if got := tools.Load(); got != 0 {
		t.Fatalf("cancelled queued mutation entered dispatcher %d times", got)
	}
	unpark()
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read"}}`)
	response = readMCPTestResponse(t, conn, reader)
	if string(response.ID) != "3" || response.Error != nil || response.Result["ok"] != true {
		t.Fatalf("post-cancellation tool response = %#v", response)
	}
}

func TestServeMCPInitializedWaitRefusesExtraPipelinedRequest(t *testing.T) {
	slots := newMCPDispatchSlots(1)
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unpark := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unpark)
	var tools atomic.Int32
	conn, reader, _ := startMCPConnectionWithHooksTest(t, time.Second, func(ctx context.Context, frame []byte) ([]byte, error) {
		var envelope mcpRequestEnvelope
		if err := json.Unmarshal(frame, &envelope); err != nil {
			return nil, err
		}
		if envelope.Method == "notifications/initialized" {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil, nil
		}
		tools.Add(1)
		return []byte(`{"jsonrpc":"2.0","id":` + string(envelope.ID) + `,"result":{"ok":true}}`), nil
	}, &mcpConnectionHooks{dispatchSlots: slots})
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	awaitMCPTestSignal(t, started, "initialized handler did not start")
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read"}}`)
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"edit"}}`)
	response := readMCPTestResponse(t, conn, reader)
	if string(response.ID) != "3" || response.Error == nil || response.Error.Code != -32002 || response.Error.Data["outcome"] != "server_busy" || response.Error.Data["delivery_unknown"] != false || response.Error.Data["retryable"] != true {
		t.Fatalf("extra queued request response = %#v", response)
	}
	if got := tools.Load(); got != 0 {
		t.Fatalf("extra queued request bypassed initialization/capacity: %d calls", got)
	}
	unpark()
	response = readMCPTestResponse(t, conn, reader)
	if string(response.ID) != "2" || response.Error != nil || response.Result["ok"] != true {
		t.Fatalf("original queued request response = %#v", response)
	}
	if got := tools.Load(); got != 1 {
		t.Fatalf("tool dispatches = %d, want one", got)
	}
}

func TestMCPDispatchReleasesCapacityBeforeReturningResult(t *testing.T) {
	slots := newMCPDispatchSlots(1)
	ctx := withMCPDispatchSlots(t.Context(), slots)
	_, err := dispatchMCPWithContext(ctx, func() ([]byte, error) { return []byte(`{"ok":true}`), nil })
	if err != nil {
		t.Fatal(err)
	}
	if got := len(slots); got != 0 {
		t.Fatalf("completed response retained %d dispatch slots", got)
	}
}
