package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// runShutdownHelper is executed only by the actual portable child fixture.
// Modes differ in protocol cooperation, not parent-side synthetic exit state.
func runShutdownHelper(t *testing.T, mode string) {
	if ready := os.Getenv(helperReadyEnv); ready != "" {
		if err := os.WriteFile(ready, []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	if mode == helperLSPNoRead {
		for {
			time.Sleep(time.Hour)
		}
	}
	trace, err := os.OpenFile(os.Getenv(helperTranscriptEnv), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer func() { _ = trace.Close() }()
	in := bufio.NewReader(os.Stdin)
	for {
		body, ok := readFramed(in)
		if !ok {
			if mode == helperLSPIgnore {
				for {
					time.Sleep(time.Hour)
				}
			}
			return
		}
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			os.Exit(2)
		}
		_, _ = fmt.Fprintln(trace, req.Method)
		_ = trace.Sync()
		if req.Method == "initialize" && mode == helperLSPInitBarrier {
			if err := os.WriteFile(os.Getenv(helperInitEnteredEnv), []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
				os.Exit(2)
			}
			for {
				if _, err := os.Stat(os.Getenv(helperInitReleaseEnv)); err == nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
		}
		if req.Method == "shutdown" && mode == helperLSPIgnore {
			continue
		}
		if req.Method == "exit" {
			if mode == helperLSPIgnore {
				continue
			}
			return
		}
		if req.ID != 0 {
			writeFramed(t, os.Stdout, jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`null`)})
		}
	}
}

func shutdownClientFixture(t *testing.T, mode string) (*Client, *SpawnTransport, string) {
	t.Helper()
	trace := filepath.Join(t.TempDir(), "methods.txt")
	t.Setenv(helperTranscriptEnv, trace)
	tr := helperTransport(t, mode, 0, zap.NewNop())
	c, err := NewClientWithTransport(tr, zap.NewNop())
	require.NoError(t, err)
	t.Logf("owned LSP child pid=%d mode=%s", tr.cmd.Process.Pid, mode)
	t.Cleanup(func() {
		err := c.Shutdown()
		if state := tr.cmd.ProcessState; state != nil {
			t.Logf("reaped LSP child pid=%d exit=%d success=%t close_error=%v", state.Pid(), state.ExitCode(), state.Success(), err)
		} else {
			t.Errorf("owned LSP child pid=%d has no actual Wait receipt: %v", tr.cmd.Process.Pid, err)
		}
	})
	return c, tr, trace
}

func awaitClientShutdown(t *testing.T, c *Client) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Shutdown() }()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("owned LSP shutdown/reap did not complete within three seconds")
		return nil
	}
}

func TestClientShutdownSendsGracefulFramesAndReapsOnceForConcurrentCallers(t *testing.T) {
	c, tr, trace := shutdownClientFixture(t, helperLSPGraceful)
	require.NoError(t, c.Call("test/ping", nil, nil)) // Real child has entered its protocol loop.
	results := make(chan error, 8)
	var calls sync.WaitGroup
	for range 8 {
		calls.Go(func() { results <- c.Shutdown() })
	}
	joined := make(chan struct{})
	go func() { calls.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent callers did not join the actual child shutdown")
	}
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.NoError(t, c.Shutdown())
	require.NoError(t, tr.Stop(), "transport must reuse its actual Wait result")
	require.NotNil(t, tr.cmd.ProcessState)
	require.Equal(t, tr.cmd.Process.Pid, tr.cmd.ProcessState.Pid())
	require.True(t, tr.cmd.ProcessState.Success())
	methods, err := os.ReadFile(trace)
	require.NoError(t, err)
	require.Equal(t, []string{"test/ping", "shutdown", "exit"}, strings.Fields(string(methods)))
	require.Error(t, c.Notify("test/after-close", nil))
	select {
	case <-c.readerDone:
	default:
		t.Fatal("shutdown returned before response reader joined")
	}
}

func TestClientShutdownIgnoredProtocolReapsOnlyItsExactChild(t *testing.T) {
	c, tr, _ := shutdownClientFixture(t, helperLSPIgnore)
	require.NoError(t, c.Call("test/ping", nil, nil))
	// A separate owned child must still service real traffic after escalation.
	other := helperTransport(t, helperCat, 0, zap.NewNop())
	in, out, err := other.Start()
	require.NoError(t, err)
	t.Logf("independent fixture child pid=%d", other.cmd.Process.Pid)
	t.Cleanup(func() { _ = other.Stop() })
	first := awaitClientShutdown(t, c)
	require.ErrorIs(t, first, errLSPStopBudget)
	require.Same(t, first, c.Shutdown(), "later caller must retain the actual cached outcome")
	require.NotNil(t, tr.cmd.ProcessState)
	require.False(t, tr.cmd.ProcessState.Success(), "ignored shutdown should require actual child escalation")
	reply := make(chan error, 1)
	go func() {
		if _, err := io.WriteString(in, "unrelated-still-live\n"); err != nil {
			reply <- err
			return
		}
		line, err := bufio.NewReader(out).ReadString('\n')
		if err == nil && line != "unrelated-still-live\n" {
			err = fmt.Errorf("unexpected reply %q", line)
		}
		reply <- err
	}()
	select {
	case err := <-reply:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("unrelated child was affected")
	}
	require.NoError(t, other.Stop())
	require.True(t, other.cmd.ProcessState.Success())
	t.Logf("reaped independent child pid=%d success=true", other.cmd.ProcessState.Pid())
}

type observedShutdownWriter struct {
	io.WriteCloser
	entered  chan struct{}
	returned chan struct{}
	once     sync.Once
}

func (w *observedShutdownWriter) Write(p []byte) (int, error) {
	if len(p) > 1<<20 {
		w.once.Do(func() { close(w.entered) })
		defer close(w.returned)
	}
	return w.WriteCloser.Write(p)
}

func TestClientShutdownInterruptsAnActualBlockedPipeWriter(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "child-ready")
	t.Setenv(helperReadyEnv, ready)
	c, tr, _ := shutdownClientFixture(t, helperLSPNoRead)
	require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 3*time.Second, time.Millisecond)
	readyPID, err := os.ReadFile(ready)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprint(tr.cmd.Process.Pid), string(readyPID), "ready receipt must belong to this exact child")
	writer := &observedShutdownWriter{WriteCloser: c.stdin, entered: make(chan struct{}), returned: make(chan struct{})}
	c.stdin = writer
	request := make(chan error, 1)
	go func() { request <- c.Call("test/large", strings.Repeat("x", 4<<20), nil) }()
	select {
	case <-writer.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("large pipe write never entered")
	}
	// The child never reads; four MiB cannot fit in the OS pipe, independently
	// of clock granularity. Its actual writer must unblock when stdin closes.
	shutdown := make(chan error, 1)
	go func() { shutdown <- c.Shutdown() }()
	require.Eventually(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.closing }, 3*time.Second, time.Millisecond)
	require.Error(t, c.Notify("test/new-work", nil), "closing admission must not queue behind old pipe write")
	select {
	case err := <-shutdown:
		require.ErrorIs(t, err, errLSPStopBudget)
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown remained behind a blocked pipe writer")
	}
	select {
	case err := <-request:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("actual blocked writer outlived shutdown")
	}
	require.NotNil(t, tr.cmd.ProcessState)
	select {
	case <-writer.returned:
	default:
		t.Fatal("shutdown returned before the actual ordinary pipe writer")
	}
	select {
	case <-c.readerDone:
	default:
		t.Fatal("response reader outlived child reap")
	}
}

func TestClientShutdownAlreadyExitedChildReturnsCachedActualSuccess(t *testing.T) {
	c, tr, _ := shutdownClientFixture(t, helperExitClean)
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("child EOF not observed")
	}
	require.NoError(t, awaitClientShutdown(t, c))
	require.NoError(t, c.Shutdown())
	require.NoError(t, tr.Stop())
	require.NotNil(t, tr.cmd.ProcessState)
	require.True(t, tr.cmd.ProcessState.Success())
}

func TestClientShutdownDialOnlyDisconnectsAndLeavesIDEPeerAvailable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var connMu sync.Mutex
	var active net.Conn
	peerDone := make(chan error, 1)
	peerExited := make(chan struct{})
	methods := make(chan string, 8)
	// This peer explicitly serves TWO sequential connections. The existing
	// passive fixture accepts only one and cannot prove continued availability.
	go func() {
		defer close(peerExited)
		serve := func() error {
			for range 2 {
				conn, err := listener.Accept()
				if err != nil {
					return err
				}
				connMu.Lock()
				active = conn
				connMu.Unlock()
				if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
					return err
				}
				in := bufio.NewReader(conn)
				for {
					body, ok := readFramed(in)
					if !ok {
						break
					}
					var req jsonRPCRequest
					if err := json.Unmarshal(body, &req); err != nil {
						return err
					}
					methods <- req.Method
					if req.ID != 0 {
						payload, err := json.Marshal(jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`null`)})
						if err != nil {
							return err
						}
						if _, err := fmt.Fprintf(conn, "Content-Length: %d\r\n\r\n%s", len(payload), payload); err != nil {
							return err
						}
					}
				}
				_ = conn.Close()
			}
			return nil
		}
		peerDone <- serve()
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		connMu.Lock()
		if active != nil {
			_ = active.Close()
		}
		connMu.Unlock()
		select {
		case <-peerExited:
		case <-time.After(3 * time.Second):
			t.Error("owned two-connection IDE fixture did not join during cleanup")
		}
	})
	for range 2 {
		c, err := NewClientWithTransport(&DialTransport{Network: "tcp", Address: listener.Addr().String()}, zap.NewNop())
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Shutdown() })
		c.SetCallTimeout(time.Second)
		require.NoError(t, c.Call("initialize", InitializeParams{}, nil))
		require.NoError(t, awaitClientShutdown(t, c))
	}
	select {
	case err := <-peerDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("two-session IDE peer did not finish")
	}
	close(methods)
	var observed []string
	for method := range methods {
		observed = append(observed, method)
	}
	require.Equal(t, []string{"initialize", "initialize"}, observed, "IDE server must receive neither shutdown nor exit")
}

func TestSpawnTransportRepeatedStartRefusesWithoutReplacingOwnedChild(t *testing.T) {
	c, tr, _ := shutdownClientFixture(t, helperLSPGraceful)
	original := tr.cmd
	stdin, stdout, err := tr.Start()
	require.ErrorContains(t, err, "single-use")
	require.Nil(t, stdin)
	require.Nil(t, stdout)
	require.Same(t, original, tr.cmd)
	// The first child actually answers after the rejected second Start, and
	// the cached Stop must still reap that same process rather than a new one.
	require.NoError(t, c.Call("test/after-rejected-start", nil, nil))
	require.NoError(t, awaitClientShutdown(t, c))
	require.NotNil(t, original.ProcessState)
	require.Equal(t, original.Process.Pid, original.ProcessState.Pid())
	require.True(t, original.ProcessState.Success())
	_, _, err = tr.Start()
	require.ErrorContains(t, err, "single-use", "a completed Stop cannot be bypassed by reusing its transport")
	require.Same(t, original, tr.cmd)
	require.NoError(t, tr.Stop())
}
