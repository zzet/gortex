package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

func TestRelayProxySessionReplaysSafeReadAfterDaemonRestart(t *testing.T) {
	oldDial := dialDaemon
	oldWindow := proxyDialRetryWindow
	oldInterval := proxyDialRetryInterval
	t.Cleanup(func() {
		dialDaemon = oldDial
		proxyDialRetryWindow = oldWindow
		proxyDialRetryInterval = oldInterval
	})
	proxyDialRetryWindow = 50 * time.Millisecond
	proxyDialRetryInterval = time.Millisecond

	initialClient, initialDaemon := testDaemonPipe()
	recoveredClient, recoveredDaemon := testDaemonPipe()
	var dialCalls atomic.Int32
	dialDaemon = func(daemon.Handshake) (*daemon.Client, error) {
		dialCalls.Add(1)
		return recoveredClient, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- relayProxySession(ctx, testProxyHandshake(), initialClient, stdinR, stdoutW, &stderr, nil, nil)
	}()

	request := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"operation":"source"}}}` + "\n")
	mustWriteTestFrame(t, stdinW, request)
	if got := mustReadTestFrame(t, initialDaemon); !bytes.Equal(got, request) {
		t.Fatalf("initial request mismatch:\n got %s\nwant %s", got, request)
	}
	if err := initialDaemon.Close(); err != nil {
		t.Fatal(err)
	}

	if got := mustReadTestFrame(t, recoveredDaemon); !bytes.Equal(got, request) {
		t.Fatalf("safe read was not replayed after reconnect:\n got %s\nwant %s", got, request)
	}
	mustWriteTestFrame(t, recoveredDaemon, []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`+"\n"))
	response := mustReadTestFrame(t, stdoutR)
	assertTestResponse(t, response, 1, false)
	if got := dialCalls.Load(); got != 1 {
		t.Fatalf("dial calls = %d, want 1", got)
	}
	assertRelayRunning(t, done)

	cancel()
	_ = stdinW.Close()
	_ = recoveredDaemon.Close()
	waitRelay(t, done)
}

func TestRelayProxySessionKeepsTransportAfterEditingContextInterruptedByRestart(t *testing.T) {
	oldDial := dialDaemon
	oldWindow := proxyDialRetryWindow
	oldInterval := proxyDialRetryInterval
	t.Cleanup(func() {
		dialDaemon = oldDial
		proxyDialRetryWindow = oldWindow
		proxyDialRetryInterval = oldInterval
	})
	proxyDialRetryWindow = 10 * time.Millisecond
	proxyDialRetryInterval = time.Millisecond

	initialClient, initialDaemon := testDaemonPipe()
	recoveredClient, recoveredDaemon := testDaemonPipe()
	recoveredClient.Ack.DaemonInstance = "daemon-instance-b"
	var daemonRecovered atomic.Bool
	var recoveredClientTaken atomic.Bool
	dialDaemon = func(daemon.Handshake) (*daemon.Client, error) {
		if !daemonRecovered.Load() {
			return nil, daemon.ErrDaemonUnavailable
		}
		if recoveredClientTaken.Swap(true) {
			return nil, daemon.ErrDaemonUnavailable
		}
		return recoveredClient, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- relayProxySession(ctx, testProxyHandshake(), initialClient, stdinR, stdoutW, &stderr, nil, nil)
	}()

	interrupted := []byte(`{"jsonrpc":"2.0","id":41,"method":"tools/call","params":{"name":"read","arguments":{"operation":"editing_context","target":{"file":"internal/mcp/tools_explore.go"},"context":{"compress_bodies":true}}}}` + "\n")
	mustWriteTestFrame(t, stdinW, interrupted)
	if got := mustReadTestFrame(t, initialDaemon); !bytes.Equal(got, interrupted) {
		t.Fatalf("initial editing-context request mismatch:\n got %s\nwant %s", got, interrupted)
	}
	if err := initialDaemon.Close(); err != nil {
		t.Fatal(err)
	}
	assertTestResponse(t, mustReadTestFrame(t, stdoutR), 41, true)
	assertRelayRunning(t, done)

	daemonRecovered.Store(true)
	restartTrigger := []byte(`{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"read","arguments":{"operation":"editing_context","target":{"file":"internal/mcp/facade_tools.go"},"context":{"compress_bodies":true}}}}` + "\n")
	mustWriteTestFrame(t, stdinW, restartTrigger)
	warning := mustReadTestFrame(t, stdoutR)
	var notification struct {
		Method string `json:"method"`
		Params struct {
			Data struct {
				Code string `json:"code"`
			} `json:"data"`
		} `json:"params"`
	}
	if err := json.Unmarshal(warning, &notification); err != nil || notification.Method != "notifications/message" || notification.Params.Data.Code != "gortex_session_reset" {
		t.Fatalf("missing restart warning: %s (%v)", warning, err)
	}
	listChanged := mustReadTestFrame(t, stdoutR)
	var listNotification struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(listChanged, &listNotification); err != nil || listNotification.Method != "notifications/tools/list_changed" {
		t.Fatalf("missing tools/list_changed after restart: %s (%v)", listChanged, err)
	}
	resetError := mustReadTestFrame(t, stdoutR)
	assertTestResponse(t, resetError, 42, true)
	if !bytes.Contains(resetError, []byte(`"session_reset":true`)) {
		t.Fatalf("restart error is not machine-detectable: %s", resetError)
	}
	assertRelayRunning(t, done)

	// The request that discovers a new daemon instance is deliberately not
	// forwarded. An explicit retry succeeds on the same host stdio transport.
	retry := []byte(`{"jsonrpc":"2.0","id":43,"method":"tools/call","params":{"name":"read","arguments":{"operation":"editing_context","target":{"file":"internal/mcp/facade_tools.go"},"context":{"compress_bodies":true}}}}` + "\n")
	mustWriteTestFrame(t, stdinW, retry)
	if got := mustReadTestFrame(t, recoveredDaemon); !bytes.Equal(got, retry) {
		t.Fatalf("post-restart retry mismatch:\n got %s\nwant %s", got, retry)
	}
	mustWriteTestFrame(t, recoveredDaemon, []byte(`{"jsonrpc":"2.0","id":43,"result":{"content":[]}}`+"\n"))
	assertTestResponse(t, mustReadTestFrame(t, stdoutR), 43, false)
	assertRelayRunning(t, done)

	cancel()
	_ = stdinW.Close()
	_ = recoveredDaemon.Close()
	waitRelay(t, done)
}

func TestRelayProxySessionKeepsTransportAcrossUnavailableDaemon(t *testing.T) {
	oldDial := dialDaemon
	oldWindow := proxyDialRetryWindow
	oldInterval := proxyDialRetryInterval
	t.Cleanup(func() {
		dialDaemon = oldDial
		proxyDialRetryWindow = oldWindow
		proxyDialRetryInterval = oldInterval
	})
	proxyDialRetryWindow = 10 * time.Millisecond
	proxyDialRetryInterval = time.Millisecond

	initialClient, initialDaemon := testDaemonPipe()
	recoveredClient, recoveredDaemon := testDaemonPipe()
	var daemonRecovered atomic.Bool
	var dialCalls atomic.Int32
	var recoveredClientTaken atomic.Bool
	dialDaemon = func(daemon.Handshake) (*daemon.Client, error) {
		dialCalls.Add(1)
		if !daemonRecovered.Load() {
			return nil, daemon.ErrDaemonUnavailable
		}
		if recoveredClientTaken.Swap(true) {
			return nil, daemon.ErrDaemonUnavailable
		}
		return recoveredClient, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- relayProxySession(ctx, testProxyHandshake(), initialClient, stdinR, stdoutW, &stderr, nil, nil)
	}()

	mutation := []byte(`{"jsonrpc":"2.0","id":null,"method":"tools/call","params":{"name":"edit","arguments":{"operation":"file"}}}` + "\n")
	mustWriteTestFrame(t, stdinW, mutation)
	if got := mustReadTestFrame(t, initialDaemon); !bytes.Equal(got, mutation) {
		t.Fatalf("initial mutation mismatch:\n got %s\nwant %s", got, mutation)
	}
	if err := initialDaemon.Close(); err != nil {
		t.Fatal(err)
	}
	mutationError := mustReadTestFrame(t, stdoutR)
	assertNullIDError(t, mutationError, false, true)
	if got := dialCalls.Load(); got != 0 {
		t.Fatalf("mutation was replayed: dial calls = %d, want 0", got)
	}
	assertRelayRunning(t, done)

	// Notifications never receive synthetic JSON-RPC responses. While the
	// daemon is down this one is dropped, then the following request receives
	// the first output frame and proves stdout was not polluted.
	notification := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	mustWriteTestFrame(t, stdinW, notification)
	unavailable := []byte(`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"search","arguments":{"operation":"symbols","query":"x"}}}` + "\n")
	mustWriteTestFrame(t, stdinW, unavailable)
	unavailableError := mustReadTestFrame(t, stdoutR)
	assertTestResponse(t, unavailableError, 11, true)
	assertRelayRunning(t, done)

	daemonRecovered.Store(true)
	recovered := []byte(`{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"search","arguments":{"operation":"symbols","query":"y"}}}` + "\n")
	mustWriteTestFrame(t, stdinW, recovered)
	if got := mustReadTestFrame(t, recoveredDaemon); !bytes.Equal(got, recovered) {
		t.Fatalf("post-restart request mismatch or earlier unsafe frame replayed:\n got %s\nwant %s", got, recovered)
	}
	mustWriteTestFrame(t, recoveredDaemon, []byte(`{"jsonrpc":"2.0","id":12,"result":{"content":[]}}`+"\n"))
	response := mustReadTestFrame(t, stdoutR)
	assertTestResponse(t, response, 12, false)
	assertRelayRunning(t, done)

	cancel()
	_ = stdinW.Close()
	_ = recoveredDaemon.Close()
	waitRelay(t, done)
}

func TestRelayProxySessionZeroByteMutationIsRetryableButNotReplayed(t *testing.T) {
	oldDial := dialDaemon
	t.Cleanup(func() { dialDaemon = oldDial })
	var dialCalls atomic.Int32
	dialDaemon = func(daemon.Handshake) (*daemon.Client, error) {
		dialCalls.Add(1)
		return nil, daemon.ErrDaemonUnavailable
	}

	conn := &zeroWriteConn{closed: make(chan struct{})}
	initial := &daemon.Client{
		Conn: conn,
		Ack: daemon.HandshakeAck{
			SessionID:      testLogicalSessionID,
			DaemonInstance: testDaemonInstance,
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- relayProxySession(ctx, testProxyHandshake(), initial, stdinR, stdoutW, io.Discard, nil, nil)
	}()

	request := []byte(`{"jsonrpc":"2.0","id":30,"method":"tools/call","params":{"name":"edit"}}` + "\n")
	mustWriteTestFrame(t, stdinW, request)
	response := mustReadTestFrame(t, stdoutR)
	var envelope struct {
		ID    int `json:"id"`
		Error struct {
			Data struct {
				Retryable       bool `json:"retryable"`
				DeliveryUnknown bool `json:"delivery_unknown"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.ID != 30 || !envelope.Error.Data.Retryable || envelope.Error.Data.DeliveryUnknown {
		t.Fatalf("zero-byte mutation response = %s", response)
	}
	if dialCalls.Load() != 0 {
		t.Fatalf("zero-byte mutation was auto-replayed: dial calls=%d", dialCalls.Load())
	}
	assertRelayRunning(t, done)

	cancel()
	_ = stdinW.Close()
	waitRelay(t, done)
}

type zeroWriteConn struct {
	closed chan struct{}
	once   sync.Once
}

func (c *zeroWriteConn) Read([]byte) (int, error) {
	<-c.closed
	return 0, io.EOF
}
func (c *zeroWriteConn) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (c *zeroWriteConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
func (*zeroWriteConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (*zeroWriteConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (*zeroWriteConn) SetDeadline(time.Time) error      { return nil }
func (*zeroWriteConn) SetReadDeadline(time.Time) error  { return nil }
func (*zeroWriteConn) SetWriteDeadline(time.Time) error { return nil }

type deadlineErrorConn struct {
	net.Conn
	setErr   error
	clearErr error
}

func (c *deadlineErrorConn) SetDeadline(deadline time.Time) error {
	if deadline.IsZero() {
		return c.clearErr
	}
	return c.setErr
}

func TestRestoreProtocolStateRejectsDeadlineFailures(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	initialize := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	t.Run("set", func(t *testing.T) {
		sentinel := errors.New("set deadline")
		proxyConn, daemonConn := net.Pipe()
		defer proxyConn.Close()
		defer daemonConn.Close()
		client := &daemon.Client{Conn: &deadlineErrorConn{Conn: proxyConn, setErr: sentinel}}
		state := &proxyRelayState{initialize: initialize}
		if err := state.restoreProtocolState(client); !errors.Is(err, sentinel) {
			t.Fatalf("restore error = %v, want wrapped set-deadline error", err)
		}
	})

	t.Run("clear", func(t *testing.T) {
		sentinel := errors.New("clear deadline")
		client, cleanup := testProtocolRestoreClient(t)
		defer cleanup()
		client.Conn = &deadlineErrorConn{Conn: client.Conn, clearErr: sentinel}
		state := &proxyRelayState{initialize: initialize}
		if err := state.restoreProtocolState(client); !errors.Is(err, sentinel) {
			t.Fatalf("restore error = %v, want wrapped clear-deadline error", err)
		}
	})
}

func TestRestoreProtocolStateForwardsInterleavedNotification(t *testing.T) {
	proxyConn, daemonConn := net.Pipe()
	defer proxyConn.Close()
	defer daemonConn.Close()

	client := &daemon.Client{Conn: proxyConn}
	daemonSide := &daemon.Client{Conn: daemonConn}
	initialize := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	notification := []byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
	response := []byte(`{"jsonrpc":"2.0","id":"gortex-reconnect-initialize","result":{"protocolVersion":"2025-06-18"}}`)
	var stdout bytes.Buffer

	done := make(chan error, 1)
	go func() {
		if _, err := daemonSide.ReadMCPFrame(); err != nil {
			done <- err
			return
		}
		if err := daemonSide.WriteMCPFrame(notification); err != nil {
			done <- err
			return
		}
		if err := daemonSide.WriteMCPFrame(response); err != nil {
			done <- err
			return
		}
		_, err := daemonSide.ReadMCPFrame() // notifications/initialized
		done <- err
	}()

	state := &proxyRelayState{initialize: initialize, stdout: &stdout}
	if err := state.restoreProtocolState(client); err != nil {
		t.Fatalf("restore protocol state: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("daemon side: %v", err)
	}
	if got := bytes.TrimSpace(stdout.Bytes()); !bytes.Equal(got, notification) {
		t.Fatalf("forwarded frame = %s, want %s", got, notification)
	}
}

func TestRelayProxySessionReportsDaemonProcessReset(t *testing.T) {
	oldDial := dialDaemon
	oldWindow := proxyDialRetryWindow
	oldInterval := proxyDialRetryInterval
	t.Cleanup(func() {
		dialDaemon = oldDial
		proxyDialRetryWindow = oldWindow
		proxyDialRetryInterval = oldInterval
	})
	proxyDialRetryWindow = 20 * time.Millisecond
	proxyDialRetryInterval = time.Millisecond

	initialClient, initialDaemon := testDaemonPipe()
	recoveredClient, recoveredDaemon := testDaemonPipe()
	recoveredClient.Ack.DaemonInstance = "daemon-instance-b"
	var taken atomic.Bool
	dialDaemon = func(daemon.Handshake) (*daemon.Client, error) {
		if taken.Swap(true) {
			return nil, daemon.ErrDaemonUnavailable
		}
		return recoveredClient, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- relayProxySession(ctx, testProxyHandshake(), initialClient, stdinR, stdoutW, &stderr, nil, nil)
	}()

	// Disconnect without an in-flight request, then let the next call trigger
	// a reconnect to a different daemon process.
	if err := initialDaemon.Close(); err != nil {
		t.Fatal(err)
	}
	request := []byte(`{"jsonrpc":"2.0","id":null,"method":"tools/call","params":{"name":"search"}}` + "\n")
	mustWriteTestFrame(t, stdinW, request)

	warning := mustReadTestFrame(t, stdoutR)
	var notification struct {
		Method string `json:"method"`
		Params struct {
			Data struct {
				Code string `json:"code"`
			} `json:"data"`
		} `json:"params"`
	}
	if err := json.Unmarshal(warning, &notification); err != nil || notification.Method != "notifications/message" || notification.Params.Data.Code != "gortex_session_reset" {
		t.Fatalf("missing machine-detectable reset warning: %s (%v)", warning, err)
	}
	listChanged := mustReadTestFrame(t, stdoutR)
	var listNotification struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(listChanged, &listNotification); err != nil || listNotification.Method != "notifications/tools/list_changed" {
		t.Fatalf("missing tools/list_changed after reset: %s (%v)", listChanged, err)
	}
	resetError := mustReadTestFrame(t, stdoutR)
	assertNullIDError(t, resetError, true, false)
	if bytes.Contains(resetError, []byte(`"session_reset":true`)) == false {
		t.Fatalf("reset error is not machine-detectable: %s", resetError)
	}
	assertRelayRunning(t, done)

	// The triggering request was not executed. A later, explicit retry uses
	// the fresh daemon session normally.
	retry := []byte(`{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"search"}}` + "\n")
	mustWriteTestFrame(t, stdinW, retry)
	if got := mustReadTestFrame(t, recoveredDaemon); !bytes.Equal(got, retry) {
		t.Fatalf("fresh-session retry mismatch:\n got %s\nwant %s", got, retry)
	}
	mustWriteTestFrame(t, recoveredDaemon, []byte(`{"jsonrpc":"2.0","id":22,"result":{}}`+"\n"))
	assertTestResponse(t, mustReadTestFrame(t, stdoutR), 22, false)

	cancel()
	_ = stdinW.Close()
	_ = recoveredDaemon.Close()
	waitRelay(t, done)
}

func TestRelayProxySessionDoesNotReplayCompletedRequestAfterDisconnect(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
	}{
		{name: "success", response: `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}` + "\n"},
		{name: "canceled", response: `{"jsonrpc":"2.0","id":1,"error":{"code":-32800,"message":"context canceled"}}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldDial := dialDaemon
			t.Cleanup(func() { dialDaemon = oldDial })
			initialClient, initialDaemon := testDaemonPipe()
			recoveredClient, recoveredDaemon := testDaemonPipe()
			recoveredClient.Ack.DaemonInstance = "daemon-instance-b"
			var dialCalls atomic.Int32
			dialDaemon = func(daemon.Handshake) (*daemon.Client, error) {
				dialCalls.Add(1)
				return recoveredClient, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			stdinR, stdinW := io.Pipe()
			stdoutR, stdoutW := io.Pipe()
			stdout := bufio.NewReader(stdoutR)
			done := make(chan error, 1)
			go func() {
				done <- relayProxySession(ctx, testProxyHandshake(), initialClient, stdinR, stdoutW, io.Discard, nil, nil)
			}()
			defer func() {
				// Keep relay I/O valid until genuine stdin EOF has stopped and
				// joined the relay. Closing its read/output pipes first races
				// teardown into an unrelated io.ErrClosedPipe failure.
				defer func() {
					cancel()
					_ = stdinR.Close()
					_ = stdoutR.Close()
					_ = stdoutW.Close()
					_ = initialDaemon.Close()
					_ = recoveredDaemon.Close()
					_ = initialClient.Close()
					_ = recoveredClient.Close()
				}()
				_ = stdinW.Close()
				waitRelay(t, done)
			}()

			request := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read"}}` + "\n")
			mustWriteTestFrame(t, stdinW, request)
			if got := mustReadTestFrame(t, initialDaemon); !bytes.Equal(got, request) {
				t.Fatalf("initial request mismatch: %s", got)
			}
			mustWriteTestFrame(t, initialDaemon, []byte(tc.response))
			if got := mustReadTestFrame(t, stdout); !bytes.Equal(got, []byte(tc.response)) {
				t.Fatalf("terminal response changed: %s", got)
			}
			// The host has observed the terminal response before transport loss.
			// Neither success nor cancellation leaves a request eligible for replay.
			if err := initialDaemon.Close(); err != nil {
				t.Fatal(err)
			}
			trigger := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read"}}` + "\n")
			mustWriteTestFrame(t, stdinW, trigger)
			warning := mustReadTestFrame(t, stdout)
			if !jsonFrameMethod(warning, "notifications/message") || !bytes.Contains(warning, []byte(`"gortex_session_reset"`)) {
				t.Fatalf("expected reset warning before any replay: %s", warning)
			}
			if got := mustReadTestFrame(t, stdout); !jsonFrameMethod(got, "notifications/tools/list_changed") {
				t.Fatalf("expected tools/list_changed: %s", got)
			}
			reset := mustReadTestFrame(t, stdout)
			assertTestResponse(t, reset, 2, true)
			if !bytes.Contains(reset, []byte(`"session_reset":true`)) {
				t.Fatalf("expected reset error for the new request: %s", reset)
			}
			retry := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read"}}` + "\n")
			mustWriteTestFrame(t, stdinW, retry)
			if got := mustReadTestFrame(t, recoveredDaemon); !bytes.Equal(got, retry) {
				t.Fatalf("completed request replayed instead of explicit retry: %s", got)
			}
			mustWriteTestFrame(t, recoveredDaemon, []byte(`{"jsonrpc":"2.0","id":3,"result":{}}`+"\n"))
			assertTestResponse(t, mustReadTestFrame(t, stdout), 3, false)
			if got := dialCalls.Load(); got != 1 {
				t.Fatalf("dial calls = %d, want 1", got)
			}
			assertRelayRunning(t, done)
		})
	}
}

func TestProxyTestFrameReaderRetainsCoalescedFrames(t *testing.T) {
	frames := [][]byte{
		[]byte(`{"jsonrpc":"2.0","method":"notifications/message"}` + "\n"),
		[]byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}` + "\n"),
		[]byte(`{"jsonrpc":"2.0","id":2,"error":{"code":-32000}}` + "\n"),
	}
	reader := bufio.NewReader(bytes.NewReader(bytes.Join(frames, nil)))
	for i, want := range frames {
		if got := mustReadTestFrame(t, reader); !bytes.Equal(got, want) {
			t.Fatalf("coalesced frame %d = %s, want %s", i, got, want)
		}
	}
}

func TestRetryableProxyRequestIsConservative(t *testing.T) {
	tests := []struct {
		name  string
		frame string
		want  bool
	}{
		{name: "read facade", frame: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read"}}`, want: true},
		{name: "explore task", frame: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"explore","arguments":{"operation":"task"}}}`, want: true},
		{name: "explore localize uses daemon response dedup", frame: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"explore","arguments":{"operation":" Localize "}}}`, want: true},
		{name: "capabilities", frame: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"capabilities"}}`, want: true},
		{name: "edit mutation", frame: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"edit"}}`, want: false},
		{name: "change deliberately not replayed", frame: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"change"}}`, want: false},
		{name: "ask deliberately not replayed", frame: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask"}}`, want: false},
		{name: "unknown tool", frame: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"custom_mutation"}}`, want: false},
		{name: "notification", frame: `{"jsonrpc":"2.0","method":"notifications/initialized"}`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retryableProxyRequest([]byte(tt.frame)); got != tt.want {
				t.Fatalf("retryableProxyRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPendingRequestIDsPreserveLargeIntegersAndTypes(t *testing.T) {
	requests := [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":9007199254740992,"method":"tools/call","params":{"name":"read"}}`),
		[]byte(`{"jsonrpc":"2.0","id":9007199254740993,"method":"tools/call","params":{"name":"read"}}`),
		[]byte(`{"jsonrpc":"2.0","id":"9007199254740992","method":"tools/call","params":{"name":"read"}}`),
		[]byte(`{"jsonrpc":"2.0","id":null,"method":"tools/call","params":{"name":"read"}}`),
	}
	responses := [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":9007199254740992,"result":{}}`),
		[]byte(`{"jsonrpc":"2.0","id":9007199254740993,"result":{}}`),
		[]byte(`{"jsonrpc":"2.0","id":"9007199254740992","result":{}}`),
		[]byte(`{"jsonrpc":"2.0","id":null,"result":{}}`),
	}

	pending := make(map[string]*proxyPendingRequest)
	for i, frame := range requests {
		request, ok := pendingRequest(frame)
		if !ok {
			t.Fatalf("request %d did not produce a pending ID", i)
		}
		if _, collision := pending[request.key]; collision {
			t.Fatalf("request %d collided on key %q", i, request.key)
		}
		pending[request.key] = request
		responseKey, ok := responseIDKey(responses[i])
		if !ok || responseKey != request.key {
			t.Fatalf("response %d key = %q, want %q", i, responseKey, request.key)
		}
	}
	if len(pending) != 4 {
		t.Fatalf("pending IDs = %d, want 4 distinct entries", len(pending))
	}
}

const (
	testLogicalSessionID = "0123456789abcdef0123456789abcdef"
	testDaemonInstance   = "daemon-instance-a"
)

func testProxyHandshake() daemon.Handshake {
	return daemon.Handshake{
		Mode:             daemon.ModeMCP,
		PID:              42,
		LogicalSessionID: testLogicalSessionID,
	}
}

func testDaemonPipe() (*daemon.Client, net.Conn) {
	proxyConn, daemonConn := net.Pipe()
	return &daemon.Client{
		Conn: proxyConn,
		Ack: daemon.HandshakeAck{
			SessionID:      testLogicalSessionID,
			DaemonInstance: testDaemonInstance,
		},
	}, daemonConn
}

func mustWriteTestFrame(t *testing.T, dst io.Writer, frame []byte) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := dst.Write(frame)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out writing frame: %s", frame)
	}
}

func mustReadTestFrame(t *testing.T, src io.Reader) []byte {
	t.Helper()
	type result struct {
		frame []byte
		err   error
	}
	done := make(chan result, 1)
	go func() {
		frame, err := bufio.NewReader(src).ReadBytes('\n')
		done <- result{frame: frame, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		return got.frame
	case <-time.After(2 * time.Second):
		t.Fatal("timed out reading frame")
		return nil
	}
}

func assertTestResponse(t *testing.T, frame []byte, wantID int, wantError bool) {
	t.Helper()
	var response struct {
		ID    int             `json:"id"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(frame, &response); err != nil {
		t.Fatalf("invalid JSON-RPC response %q: %v", frame, err)
	}
	if response.ID != wantID {
		t.Fatalf("response id = %d, want %d; frame=%s", response.ID, wantID, frame)
	}
	if gotError := len(response.Error) != 0 && string(response.Error) != "null"; gotError != wantError {
		t.Fatalf("response error presence = %v, want %v; frame=%s", gotError, wantError, frame)
	}
}

func assertNullIDError(t *testing.T, frame []byte, wantRetryable, wantDeliveryUnknown bool) {
	t.Helper()
	var response struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code int `json:"code"`
			Data struct {
				Retryable       bool `json:"retryable"`
				DeliveryUnknown bool `json:"delivery_unknown"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(frame, &response); err != nil {
		t.Fatalf("invalid JSON-RPC response %q: %v", frame, err)
	}
	if string(bytes.TrimSpace(response.ID)) != "null" {
		t.Fatalf("response id = %s, want explicit null; frame=%s", response.ID, frame)
	}
	if response.Error.Code == 0 {
		t.Fatalf("missing error response: %s", frame)
	}
	if response.Error.Data.Retryable != wantRetryable || response.Error.Data.DeliveryUnknown != wantDeliveryUnknown {
		t.Fatalf("error delivery flags = retryable:%v delivery_unknown:%v, want %v/%v; frame=%s",
			response.Error.Data.Retryable, response.Error.Data.DeliveryUnknown, wantRetryable, wantDeliveryUnknown, frame)
	}
}

func assertRelayRunning(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("relay exited while stdio remained open: %v", err)
	default:
	}
}

func waitRelay(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not stop after cancellation")
	}
}
