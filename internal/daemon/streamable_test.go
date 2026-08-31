package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// newDaemonNoServe is newDaemon's twin that stops short of calling
// Listen / Serve so the test can mutate fields like HTTPAddr /
// HTTPHandler before the listener comes up.
func newDaemonNoServe(t *testing.T, ctrl Controller) (*Server, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gx")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := filepath.Join(dir, "s")
	t.Setenv("GORTEX_DAEMON_SOCKET", socket)
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "p"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(dir, "state.json"))

	srv := New(socket, "test-0.0.0", zap.NewNop())
	srv.Controller = ctrl
	t.Cleanup(func() {
		_ = srv.Shutdown()
		srv.ReleaseProcessState()
	})
	return srv, socket
}

// TestDaemon_HTTPListenerServesAttachedHandler proves the daemon
// Server brings up an HTTP listener on HTTPAddr when HTTPHandler is
// set, that the handler reaches inbound requests, and that Shutdown
// tears the listener down cleanly. The streamable transport itself
// is exercised in internal/mcp/streamable; this test only proves the
// daemon-side plumbing.
func TestDaemon_HTTPListenerServesAttachedHandler(t *testing.T) {
	ctrl := &fakeController{}
	srv, _ := newDaemonNoServe(t, ctrl)

	srv.HTTPHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Method", r.Method)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	srv.HTTPAddr = "127.0.0.1:0"

	require.NoError(t, srv.Listen())
	go func() { _ = srv.Serve() }()

	require.Eventually(t, func() bool {
		return srv.httpListener != nil
	}, 2*time.Second, 10*time.Millisecond, "http listener never came up")

	addr := srv.httpListener.Addr().String()
	resp, err := http.Get("http://" + addr + "/anything")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "GET", resp.Header.Get("X-Method"))
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, "ok", string(body))

	require.NoError(t, srv.Shutdown())
	require.Eventually(t, func() bool {
		_, err := http.Get("http://" + addr + "/anything")
		return err != nil
	}, 2*time.Second, 10*time.Millisecond, "http listener stayed up after Shutdown")
}

func TestServeJoinsActiveHTTPHandlersBeforeReturning(t *testing.T) {
	srv, _ := newDaemonNoServe(t, &fakeController{})
	entered := make(chan struct{})
	release := make(chan struct{})
	srv.HTTPHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	srv.HTTPAddr = "127.0.0.1:0"
	require.NoError(t, srv.Listen())
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve() }()

	requestDone := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + srv.httpListener.Addr().String() + "/blocking")
		if resp != nil {
			_ = resp.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("blocking HTTP handler was not admitted")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- srv.Shutdown() }()
	select {
	case err := <-serveDone:
		t.Fatalf("Serve returned while an HTTP handler still owned graph work: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-shutdownDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not finish after HTTP handler release")
	}
	select {
	case err := <-serveDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after HTTP handler release")
	}
	select {
	case err := <-requestDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("HTTP request did not finish after handler release")
	}
}

func BenchmarkServerHandlerAdmission(b *testing.B) {
	srv := New("benchmark.sock", "benchmark", zap.NewNop())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !srv.beginHandler() {
			b.Fatal("handler admission unexpectedly closed")
		}
		srv.handlers.Done()
	}
}

// TestSessionRegistry_RegisterDetached covers the detached-session
// helper the streamable HTTP path relies on.
func TestSessionRegistry_RegisterDetached(t *testing.T) {
	r := NewSessionRegistry()
	sess := r.RegisterDetached("custom-id-42", Handshake{
		Mode:       ModeMCP,
		CWD:        "/tmp/x",
		ClientName: "http",
	})
	require.NotNil(t, sess)
	require.Equal(t, "custom-id-42", sess.ID)
	require.Equal(t, "http", sess.ClientName)
	require.Equal(t, "/tmp/x", sess.CWD)
	require.Nil(t, sess.Conn, "detached session must not carry a Conn")

	got := r.GetByID("custom-id-42")
	require.NotNil(t, got)
	require.Equal(t, sess, got)

	removed := r.RemoveByID("custom-id-42")
	require.NotNil(t, removed)
	require.Nil(t, r.GetByID("custom-id-42"))

	require.Nil(t, r.RemoveByID("custom-id-42"))
	require.Nil(t, r.RemoveByID(""))
}

func TestSessionRegistry_RegisterDetachedAutoGenID(t *testing.T) {
	r := NewSessionRegistry()
	a := r.RegisterDetached("", Handshake{Mode: ModeMCP})
	b := r.RegisterDetached("", Handshake{Mode: ModeMCP})
	require.NotEmpty(t, a.ID)
	require.NotEmpty(t, b.ID)
	require.NotEqual(t, a.ID, b.ID)
	require.Equal(t, 2, r.Count())
}

// TestDaemon_HTTPAndUnixCoexist confirms the unix-socket transport
// still works when an HTTP listener is also attached.
func TestDaemon_HTTPAndUnixCoexist(t *testing.T) {
	ctrl := &fakeController{}
	srv, socket := newDaemonNoServe(t, ctrl)
	srv.HTTPHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	srv.HTTPAddr = "127.0.0.1:0"
	require.NoError(t, srv.Listen())
	go func() { _ = srv.Serve() }()

	require.Eventually(t, func() bool {
		return srv.httpListener != nil && IsRunningAt(socket)
	}, 2*time.Second, 10*time.Millisecond)

	httpAddr := srv.httpListener.Addr().String()
	resp, err := http.Get("http://" + httpAddr)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusTeapot, resp.StatusCode)

	c, err := DialTo(socket, Handshake{Mode: ModeControl, ClientName: "cli"})
	require.NoError(t, err)
	require.NoError(t, c.Close())
}

// TestDaemon_HTTPListenerFailureAbortsListen covers the operator
// misconfig case: pointing at a port that's already in use surfaces
// as a fatal Listen error rather than a silent half-up daemon.
func TestDaemon_HTTPListenerFailureAbortsListen(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = occupied.Close() })

	ctrl := &fakeController{}
	srv, _ := newDaemonNoServe(t, ctrl)
	srv.HTTPHandler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	srv.HTTPAddr = occupied.Addr().String()
	err = srv.Listen()
	require.Error(t, err, "Listen should fail when http addr is in use")
	require.Contains(t, err.Error(), "listen http")
}

// TestDaemon_NoHTTPAddrKeepsHTTPListenerNil — default-off path: no
// HTTPAddr means no port opens.
func TestDaemon_NoHTTPAddrKeepsHTTPListenerNil(t *testing.T) {
	ctrl := &fakeController{}
	srv, _ := newDaemonNoServe(t, ctrl)
	require.NoError(t, srv.Listen())
	require.Nil(t, srv.httpListener)
	require.NoError(t, srv.Shutdown())
}

// TestDaemon_StreamableHTTPEndToEnd wires a streamable-shaped
// handler onto the daemon's HTTP listener and confirms a JSON-RPC
// roundtrip works over the wire. The real streamable.Transport lives
// in internal/mcp/streamable with its own dedicated tests; this one
// proves the daemon's HTTPHandler / HTTPAddr / SessionRegistry
// integration in one place.
func TestDaemon_StreamableHTTPEndToEnd(t *testing.T) {
	ctrl := &fakeController{}
	srv, _ := newDaemonNoServe(t, ctrl)
	srv.MCPDispatcher = httpEchoDispatcher{}
	srv.HTTPHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		sess := srv.sessions.RegisterDetached("", Handshake{Mode: ModeMCP})
		w.Header().Set("Mcp-Session-Id", sess.ID)
		w.Header().Set("Content-Type", "application/json")
		reply, _ := srv.MCPDispatcher.Dispatch(r.Context(), sess, body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(reply)
	})
	srv.HTTPAddr = "127.0.0.1:0"
	require.NoError(t, srv.Listen())
	go func() { _ = srv.Serve() }()
	require.Eventually(t, func() bool { return srv.httpListener != nil },
		2*time.Second, 10*time.Millisecond)

	addr := srv.httpListener.Addr().String()
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"echo"}`)
	resp, err := http.Post("http://"+addr, "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	sid := resp.Header.Get("Mcp-Session-Id")
	require.NotEmpty(t, sid)
	out, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(out), `"jsonrpc":"2.0"`)
	require.NotNil(t, srv.sessions.GetByID(sid))
}

// httpEchoDispatcher is the minimal MCPDispatcher used by
// TestDaemon_StreamableHTTPEndToEnd — returns a canned envelope
// naming the session id so the test can prove the dispatch path
// threaded both inputs through.
type httpEchoDispatcher struct{}

func (httpEchoDispatcher) Dispatch(_ context.Context, sess *Session, _ []byte) ([]byte, error) {
	out, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"result": map[string]any{
			"session_id": sess.ID,
			"echoed":     "yes",
		},
	})
	return out, nil
}
