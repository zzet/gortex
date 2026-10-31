package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/testenv"
	"go.uber.org/zap"
)

type proxyRestartDispatcher struct {
	mu                    sync.Mutex
	methods               []string
	blockEditingContext   bool
	editingContextStarted chan struct{}
	editingContextOnce    sync.Once
}

func (d *proxyRestartDispatcher) Dispatch(ctx context.Context, _ *daemon.Session, frame []byte) ([]byte, error) {
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string `json:"name"`
			Arguments struct {
				Operation string `json:"operation"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &request); err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.methods = append(d.methods, request.Method)
	d.mu.Unlock()
	if d.blockEditingContext && request.Method == "tools/call" && request.Params.Name == "read" && request.Params.Arguments.Operation == "editing_context" {
		d.editingContextOnce.Do(func() {
			if d.editingContextStarted != nil {
				close(d.editingContextStarted)
			}
		})
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if len(request.ID) == 0 {
		return nil, nil
	}
	result := any(map[string]any{"content": []any{}})
	if request.Method == "initialize" {
		result = map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
			"serverInfo":      map[string]any{"name": "gortex-test", "version": "test"},
		}
	}
	return json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result":  result,
	})
}

func (d *proxyRestartDispatcher) snapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.methods...)
}

func TestRelayProxySessionRestoresProtocolAgainstRealRestartedDaemon(t *testing.T) {
	oldDial := dialDaemon
	oldWindow := proxyDialRetryWindow
	oldInterval := proxyDialRetryInterval
	t.Cleanup(func() {
		dialDaemon = oldDial
		proxyDialRetryWindow = oldWindow
		proxyDialRetryInterval = oldInterval
	})
	proxyDialRetryWindow = 2 * time.Second
	proxyDialRetryInterval = time.Millisecond
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	dir := testenv.ShortTempDir(t)
	socket := filepath.Join(dir, "s")
	h := daemon.Handshake{
		Version:          daemon.ProtocolVersion,
		Mode:             daemon.ModeMCP,
		PID:              os.Getpid(),
		LogicalSessionID: testLogicalSessionID,
		CWD:              t.TempDir(),
	}

	firstDispatcher := &proxyRestartDispatcher{
		blockEditingContext:   true,
		editingContextStarted: make(chan struct{}),
	}
	firstServer, firstDone := startProxyRestartDaemon(t, socket, firstDispatcher)
	var stopFirstOnce sync.Once
	stopFirst := func() { stopFirstOnce.Do(func() { stopProxyRestartDaemon(t, firstServer, firstDone) }) }
	t.Cleanup(stopFirst)
	initial, err := daemon.DialTo(socket, h)
	if err != nil {
		t.Fatal(err)
	}
	firstInstance := initial.Ack.DaemonInstance
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reconnectReady := make(chan struct{})
	var reconnectOnce sync.Once
	allowReconnect := func() { reconnectOnce.Do(func() { close(reconnectReady) }) }
	dialDaemon = func(handshake daemon.Handshake) (*daemon.Client, error) {
		select {
		case <-reconnectReady:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return daemon.DialTo(socket, handshake)
	}

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stdout := bufio.NewReader(stdoutR) // one reader owns any read-ahead for this stream
	done := make(chan error, 1)
	go func() {
		done <- relayProxySession(ctx, h, initial, stdinR, stdoutW, io.Discard, nil, nil)
	}()
	defer func() {
		// Keep the host transport open until cancellation joins the relay.
		// Closing it first races a synthetic ErrClosedPipe against ctx.Done.
		// Still release every endpoint if the bounded join itself fails.
		defer func() {
			_ = stdinW.Close()
			_ = stdinR.Close()
			_ = stdoutR.Close()
			_ = stdoutW.Close()
			_ = initial.Close()
		}()
		cancel()
		allowReconnect()
		waitRelay(t, done)
	}()

	initialize := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"restart-test","version":"1"},"capabilities":{}}}` + "\n")
	mustWriteTestFrame(t, stdinW, initialize)
	assertTestResponse(t, mustReadTestFrame(t, stdout), 1, false)
	mustWriteTestFrame(t, stdinW, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"))
	waitForProxyMethods(t, firstDispatcher, 2)

	// Interrupt a real read(editing_context) while it is executing. Closing
	// the first daemon socket must not close the host's stdin/stdout transport.
	interrupted := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read","arguments":{"operation":"editing_context","target":{"file":"internal/mcp/tools_explore.go"}}}}` + "\n")
	mustWriteTestFrame(t, stdinW, interrupted)
	select {
	case <-firstDispatcher.editingContextStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("editing_context did not start before daemon restart")
	}

	// This case means transport loss before a terminal reply. Close our old
	// client transport first: a graceful server stop can otherwise race a
	// cancellation response that legitimately completes the pending request.
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	stopFirst()
	secondDispatcher := &proxyRestartDispatcher{}
	secondServer, secondDone := startProxyRestartDaemon(t, socket, secondDispatcher)
	t.Cleanup(func() { stopProxyRestartDaemon(t, secondServer, secondDone) })
	allowReconnect() // only the real replacement listener may receive redial

	warning := mustReadTestFrame(t, stdout)
	var resetNotification struct {
		Method string `json:"method"`
		Params struct {
			Data struct {
				Code             string `json:"code"`
				PreviousInstance string `json:"previous_daemon_instance"`
				CurrentInstance  string `json:"daemon_instance"`
			} `json:"data"`
		} `json:"params"`
	}
	if err := json.Unmarshal(warning, &resetNotification); err != nil ||
		resetNotification.Method != "notifications/message" ||
		resetNotification.Params.Data.Code != "gortex_session_reset" ||
		resetNotification.Params.Data.PreviousInstance != firstInstance ||
		resetNotification.Params.Data.CurrentInstance == "" ||
		resetNotification.Params.Data.CurrentInstance == firstInstance {
		t.Fatalf("machine-readable daemon instance reset missing: %s (%v)", warning, err)
	}
	listChanged := mustReadTestFrame(t, stdout)
	if !jsonFrameMethod(listChanged, "notifications/tools/list_changed") {
		t.Fatalf("restart tools/list_changed notification missing: %s", listChanged)
	}
	resetError := mustReadTestFrame(t, stdout)
	assertTestResponse(t, resetError, 2, true)
	if initial.Ack.DaemonInstance == "" || initial.Ack.DaemonInstance != firstInstance {
		t.Fatal("initial daemon instance was not stable")
	}
	methods := waitForProxyMethods(t, secondDispatcher, 2)
	if methods[0] != "initialize" || methods[1] != "notifications/initialized" {
		t.Fatalf("protocol state replay order = %v", methods)
	}
	for _, method := range methods {
		if method == "tools/call" {
			t.Fatalf("triggering request executed despite session reset: %v", methods)
		}
	}

	retry := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read","arguments":{"operation":"editing_context","target":{"file":"internal/mcp/tools_explore.go"}}}}` + "\n")
	mustWriteTestFrame(t, stdinW, retry)
	assertTestResponse(t, mustReadTestFrame(t, stdout), 3, false)
	methods = waitForProxyMethods(t, secondDispatcher, 3)
	if methods[2] != "tools/call" {
		t.Fatalf("explicit retry was not dispatched: %v", methods)
	}
}

func startProxyRestartDaemon(t *testing.T, socket string, dispatcher daemon.MCPDispatcher) (*daemon.Server, <-chan error) {
	t.Helper()
	server := daemon.New(socket, "test", zap.NewNop())
	server.MCPDispatcher = dispatcher
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	return server, done
}

func testProtocolRestoreClient(t *testing.T) (*daemon.Client, func()) {
	t.Helper()
	dir := testenv.ShortTempDir(t)
	socket := filepath.Join(dir, "s")
	server, done := startProxyRestartDaemon(t, socket, &proxyRestartDispatcher{})
	client, err := daemon.DialTo(socket, daemon.Handshake{
		Version:          daemon.ProtocolVersion,
		Mode:             daemon.ModeMCP,
		PID:              os.Getpid(),
		LogicalSessionID: testLogicalSessionID,
		CWD:              t.TempDir(),
	})
	if err != nil {
		stopProxyRestartDaemon(t, server, done)
		t.Fatal(err)
	}
	cleanup := func() {
		_ = client.Close()
		stopProxyRestartDaemon(t, server, done)
	}
	return client, cleanup
}

func stopProxyRestartDaemon(t *testing.T, server *daemon.Server, done <-chan error) {
	t.Helper()
	if server == nil {
		return
	}
	shutdownErr := server.Shutdown()
	select {
	case err := <-done:
		if shutdownErr != nil {
			t.Fatal(shutdownErr)
		}
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		if shutdownErr != nil {
			t.Errorf("daemon shutdown: %v", shutdownErr)
		}
		t.Fatal("daemon did not stop")
	}
}

func BenchmarkProxyRestartDaemonLifecycle(b *testing.B) {
	dir := testenv.ShortTempDir(b)
	b.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	b.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "p"))
	socket := filepath.Join(dir, "s")

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		server := daemon.New(socket, "benchmark", zap.NewNop())
		if err := server.Listen(); err != nil {
			b.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- server.Serve() }()
		if err := server.Shutdown(); err != nil {
			b.Fatal(err)
		}
		if err := <-done; err != nil {
			b.Fatal(err)
		}
	}
}

func waitForProxyMethods(t *testing.T, dispatcher *proxyRestartDispatcher, want int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		methods := dispatcher.snapshot()
		if len(methods) >= want {
			return methods
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("dispatcher methods = %v, want at least %d", dispatcher.snapshot(), want)
	return nil
}

func jsonFrameMethod(frame []byte, want string) bool {
	var envelope struct {
		Method string `json:"method"`
	}
	return json.Unmarshal(frame, &envelope) == nil && envelope.Method == want
}
