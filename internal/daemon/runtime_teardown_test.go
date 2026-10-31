package daemon

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/testenv"
)

func runtimeCleanupServer(t *testing.T, configure func(*Server)) *Server {
	t.Helper()
	dir := testenv.ShortTempDir(t)
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "pid"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(dir, "state"))
	s := New(filepath.Join(dir, "sock"), "runtime-fixture", zap.NewNop())
	s.RuntimeStateFile = RuntimeStatePath()
	s.Controller = &fakeController{}
	if configure != nil {
		configure(s)
	}
	require.NoError(t, WriteRuntimeState(RuntimeState{BackendPath: filepath.Join(dir, "fixture.sqlite")}))
	require.NoError(t, s.Listen())
	t.Cleanup(func() { _ = s.Shutdown() })
	return s
}

type runtimeFaultListener struct {
	net.Listener
	failure error
	calls   atomic.Int32
}

func (l *runtimeFaultListener) Close() error {
	l.calls.Add(1)
	return errors.Join(l.Listener.Close(), net.ErrClosed, l.failure)
}

type runtimeFaultConn struct {
	net.Conn
	failure error
	calls   atomic.Int32
}

func (c *runtimeFaultConn) Close() error {
	c.calls.Add(1)
	return errors.Join(c.Conn.Close(), net.ErrClosed, c.failure)
}

func TestRuntimeTeardownCachesJoinedErrorsAndObservesOutsideOnce(t *testing.T) {
	listenerFailure, connFailure := errors.New("actual listener close fault"), errors.New("actual connection close fault")
	observed, reentryDone := make(chan error, 1), make(chan struct{})
	var observations atomic.Int32
	s := runtimeCleanupServer(t, func(s *Server) {
		s.OnShutdownComplete = func(result error) {
			observations.Add(1)
			reentered := make(chan error, 1)
			go func() {
				defer close(reentryDone)
				s.doneOnce.Do(func() {}) // Must not wait behind the callback itself.
				reentered <- s.Shutdown()
			}()
			select {
			case err := <-reentered:
				if !errors.Is(err, listenerFailure) || !errors.Is(err, connFailure) {
					observed <- fmt.Errorf("reentry lost actual cached errors: %w", err)
					return
				}
			case <-time.After(2 * time.Second):
				observed <- errors.New("completion callback was invoked under the close-once gate")
				return
			}
			observed <- result
		}
	})
	listener := &runtimeFaultListener{Listener: s.listener, failure: listenerFailure}
	s.listener = listener // No Serve or concurrent shutdown has been admitted.
	left, right := net.Pipe()
	t.Cleanup(func() { _ = right.Close() })
	conn := &runtimeFaultConn{Conn: left, failure: connFailure}
	require.True(t, s.trackConn(conn))
	var wg sync.WaitGroup
	results := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.Shutdown() }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		require.ErrorIs(t, err, listenerFailure)
		require.ErrorIs(t, err, connFailure)
	}
	require.ErrorIs(t, <-observed, listenerFailure)
	select {
	case <-reentryDone:
	case <-time.After(3 * time.Second):
		t.Fatal("owned callback reentry did not actually join")
	}
	require.Equal(t, int32(1), observations.Load())
	require.Equal(t, int32(1), listener.calls.Load())
	require.Equal(t, int32(1), conn.calls.Load())
	for _, path := range []string{s.ownedSocket.path, s.pidPath, s.statePath} {
		_, err := os.Lstat(path)
		require.ErrorIs(t, err, os.ErrNotExist, "close failures must not skip independent cleanup")
	}
	late, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(time.Second)))
	require.False(t, s.trackConn(late))
	_, err := peer.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF)
	require.ErrorContains(t, s.Serve(), "already started or closed")
}

func TestRuntimeTeardownDoesNotUnlinkReplacementSocketOnListenerClose(t *testing.T) {
	s := runtimeCleanupServer(t, nil)
	old := s.SocketPath + ".old"
	require.NoError(t, os.Rename(s.SocketPath, old))
	t.Cleanup(func() { _ = os.Remove(old) })
	replacement, err := net.Listen("unix", s.SocketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = replacement.Close() })
	err = s.Shutdown()
	require.ErrorContains(t, err, "changed runtime file")
	require.Equal(t, err, s.Shutdown(), "later calls must retain first actual cleanup failure")
	conn, err := net.DialTimeout("unix", s.SocketPath, time.Second)
	require.NoError(t, err, "old UnixListener auto-unlink destroyed the new endpoint")
	require.NoError(t, conn.Close())
}

func TestRuntimeTeardownRefusesChangedPIDAndForeignState(t *testing.T) {
	for _, kind := range []string{"pid", "state", "endpoint_directory"} {
		t.Run(kind, func(t *testing.T) {
			s := runtimeCleanupServer(t, nil)
			var preserved string
			switch kind {
			case "pid":
				preserved = s.pidPath
				require.NoError(t, os.WriteFile(preserved, []byte(strconv.Itoa(os.Getpid()+1)), 0o600))
			case "state":
				preserved = s.statePath
				require.NoError(t, os.WriteFile(preserved, []byte(`{"pid":1,"process_birth_token":"foreign"}`), 0o600))
			case "endpoint_directory":
				preserved = s.SocketPath
				require.NoError(t, os.Remove(preserved))
				require.NoError(t, os.Mkdir(preserved, 0o700))
			}
			err := s.Shutdown()
			require.Error(t, err)
			require.Equal(t, err, s.Shutdown())
			_, err = os.Lstat(preserved)
			require.NoError(t, err, "foreign or unexpected path was removed")
		})
	}
}

func TestRuntimeTeardownKeepsStartupPathsWhenAmbientOverridesChange(t *testing.T) {
	s := runtimeCleanupServer(t, nil)
	other := testenv.ShortTempDir(t)
	pid, state := filepath.Join(other, "pid"), filepath.Join(other, "state")
	require.NoError(t, os.WriteFile(pid, []byte("other PID record"), 0o600))
	require.NoError(t, os.WriteFile(state, []byte("other state record"), 0o600))
	t.Setenv("GORTEX_DAEMON_PIDFILE", pid)
	t.Setenv("GORTEX_DAEMON_STATEFILE", state)
	require.NoError(t, s.Shutdown())
	for _, path := range []string{pid, state} {
		_, err := os.Stat(path)
		require.NoError(t, err)
	}
	for _, path := range []string{s.pidPath, s.statePath, s.ownedSocket.path} {
		_, err := os.Lstat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

type runtimeHeldAcceptListener struct {
	net.Listener
	entered chan struct{}
	release <-chan struct{}
	failure error
}

func (l *runtimeHeldAcceptListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	close(l.entered)
	<-l.release // Descriptor is accepted and owned, but not tracked yet.
	return &runtimeFaultConn{Conn: conn, failure: l.failure}, nil
}

func TestRuntimeTeardownRetainsAcceptedBeforeTrackingCloseFailure(t *testing.T) {
	observed := make(chan error, 1)
	s := runtimeCleanupServer(t, func(s *Server) { s.OnShutdownComplete = func(err error) { observed <- err } })
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	failure := errors.New("late accepted physical close fault")
	s.listener = &runtimeHeldAcceptListener{Listener: s.listener, entered: entered, release: release, failure: failure}
	served := make(chan error, 1)
	t.Cleanup(func() { unlock(); _ = s.Shutdown(); <-served })
	go func() { served <- s.Serve() }()
	conn, err := net.DialTimeout("unix", s.SocketPath, time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("actual accept did not reach its pre-track barrier")
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Shutdown() }()
	require.Eventually(t, func() bool { s.connsMu.Lock(); defer s.connsMu.Unlock(); return s.connsClosing }, 3*time.Second, time.Millisecond)
	select {
	case <-observed:
		t.Fatal("runtime completion dropped its accepted descriptor owner")
	default:
	}
	unlock()
	select {
	case err := <-closed:
		require.ErrorIs(t, err, failure)
	case <-time.After(3 * time.Second):
		t.Fatal("actual late descriptor close did not join")
	}
	require.ErrorIs(t, <-observed, failure)
	require.ErrorIs(t, s.Shutdown(), failure, "late close error was not cached")
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = conn.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF, "the injected error must still close its real descriptor")
}

func TestRuntimeListenHTTPFailureRemovesOwnedStartupFiles(t *testing.T) {
	dir := testenv.ShortTempDir(t)
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "pid"))
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = busy.Close() })
	s := New(filepath.Join(dir, "sock"), "runtime-fixture", zap.NewNop())
	s.HTTPHandler, s.HTTPAddr = http.NewServeMux(), busy.Addr().String()
	t.Cleanup(func() { _ = s.Shutdown() })
	require.ErrorContains(t, s.Listen(), "listen http")
	for _, path := range []string{s.SocketPath, s.pidPath} {
		_, err := os.Lstat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}
