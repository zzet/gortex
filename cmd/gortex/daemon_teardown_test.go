package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/platform"
)

// teardownProbe counts each step of the daemon's shutdown chain and closes a
// real sqlite store, so "the store was closed" is an observed fact rather than
// an incremented integer.
type teardownProbe struct {
	watcherStops atomic.Int32
	sharedCloses atomic.Int32
	store        *store_sqlite.Store
	closeOnce    sync.Once
	closeErr     error
}

func newTeardownProbe(t *testing.T) *teardownProbe {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "teardown.sqlite"))
	require.NoError(t, err)
	// A write so the close has a WAL to checkpoint rather than a fresh file.
	store.AddBatch([]*graph.Node{
		{ID: "pkg/a.go::Alpha", Kind: graph.KindFunction, Name: "Alpha", FilePath: "pkg/a.go"},
	}, nil)
	p := &teardownProbe{store: store}
	// A test that never reaches the teardown (an early skip, a failed
	// assertion) would otherwise leave the sqlite handle open, and an open
	// file cannot be deleted on Windows — t.TempDir's RemoveAll then fails
	// the test that was already done.
	t.Cleanup(func() { _ = p.closeStore() })
	return p
}

func (p *teardownProbe) stopWatcher() { p.watcherStops.Add(1) }

// closeStore closes the backing store at most once, so the cleanup safety net
// and the teardown chain cannot both check-point a closed database.
func (p *teardownProbe) closeStore() error {
	p.closeOnce.Do(func() { p.closeErr = p.store.Close() })
	return p.closeErr
}

// closeShared stands in for the shared stack's Close: it runs the backend
// close that checkpoints the WAL. Closing an already-closed store is exactly
// the double-teardown this chain must not perform, so the count matters.
func (p *teardownProbe) closeShared() error {
	p.sharedCloses.Add(1)
	return p.closeStore()
}

// TestDaemonTeardownRunsOnASignalledExit drives the real signal path. The
// daemon server handles SIGINT/SIGTERM itself: it shuts the listener down
// directly and Serve returns, without the controller ever hearing about it.
// With the teardown installed only on the controller's hook, a signalled
// daemon skipped watcher shutdown, the savings flush, and the final WAL
// checkpoint outright — the store was left as the process found it.
func TestDaemonTeardownRunsOnASignalledExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		// os/exec aside, a Windows process cannot deliver a signal to
		// itself: Process.Signal returns "not supported by windows" for
		// everything except Kill. Skipping before the listener is up
		// keeps the serve goroutine from outliving the test.
		t.Skip("a process cannot signal itself on Windows")
	}
	shutdownSignals := platform.ShutdownSignals()
	if len(shutdownSignals) == 0 {
		t.Skip("no shutdown signals on this platform")
	}
	// Registered before anything is signalled so the test binary can never
	// take the default terminate action, even if the daemon's own handler is
	// missing — a regression must fail this test, not kill the run.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, shutdownSignals...)
	defer signal.Stop(guard)

	dir := t.TempDir()
	// An AF_UNIX path has a ~104-byte limit and the temp-dir path a test name
	// produces blows straight past it, so the socket goes at the temp root.
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("gx-teardown-%d.sock", os.Getpid()))
	t.Cleanup(func() { _ = os.Remove(socket) })
	t.Setenv("GORTEX_DAEMON_SOCKET", socket)
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "d.pid"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(dir, "d.state.json"))

	probe := newTeardownProbe(t)
	controller := &realController{}
	runTeardown := installDaemonTeardown(controller, probe.stopWatcher, probe.closeShared)

	srv := daemon.New(daemon.SocketPath(), "test", zap.NewNop())
	require.NoError(t, srv.Listen())
	t.Cleanup(func() { require.NoError(t, srv.Shutdown()) })

	// Exactly the shape of runDaemonStart's tail: the teardown is deferred
	// around the serve loop, and nothing else runs it.
	stopped := make(chan error, 1)
	go func() {
		defer func() { runTeardown() }()
		stopped <- srv.Serve()
	}()
	// Listen installs the signal handler before Serve owns the accept loop.
	// A signal in that gap correctly closes admission before Serve starts;
	// this regression specifically exercises an already-serving daemon.
	// A real completed handshake proves the accept loop entered, without a
	// scheduling sleep or changing the shutdown contract.
	client, err := daemon.DialToWithDeadline(socket, daemon.Handshake{
		Mode: daemon.ModeControl, ClientName: "teardown-test",
	}, time.Now().Add(5*time.Second))
	require.NoError(t, err)
	require.NoError(t, client.Close())

	proc, err := os.FindProcess(os.Getpid())
	require.NoError(t, err)
	if err := proc.Signal(shutdownSignals[len(shutdownSignals)-1]); err != nil {
		t.Skipf("cannot signal this process on this platform: %v", err)
	}

	select {
	case serveErr := <-stopped:
		require.NoError(t, serveErr)
	case <-time.After(10 * time.Second):
		t.Fatal("the serve loop never returned after the shutdown signal")
	}

	require.Eventually(t, func() bool { return probe.sharedCloses.Load() == 1 },
		5*time.Second, 10*time.Millisecond,
		"a signalled exit must still run the shared teardown (watcher stop, savings flush, WAL checkpoint)")
	assert.Equal(t, int32(1), probe.watcherStops.Load(), "the watcher must be stopped before the store closes")
}

// TestDaemonTeardownRunsOnceAcrossBothExitPaths pins the once guard. The
// control-socket stop and the signalled exit are independent, and a daemon
// that is signalled while a `gortex daemon stop` is in flight reaches both. A
// second run would close an already-closed store and flush a torn-down stack.
func TestDaemonTeardownRunsOnceAcrossBothExitPaths(t *testing.T) {
	probe := newTeardownProbe(t)
	controller := &realController{}
	runTeardown := installDaemonTeardown(controller, probe.stopWatcher, probe.closeShared)

	// The control socket gets there first...
	require.NoError(t, controller.Shutdown(context.Background()))
	// ...and the deferred exit path follows.
	runTeardown()
	// ...and a repeat of either changes nothing.
	require.NoError(t, controller.Shutdown(context.Background()))
	runTeardown()

	assert.Equal(t, int32(1), probe.watcherStops.Load(), "the watcher must be stopped exactly once")
	assert.Equal(t, int32(1), probe.sharedCloses.Load(), "the shared stack must be closed exactly once")
}

// TestDaemonTeardownReportsTheCloseFailureToEveryCaller: whoever exits second
// is often the one reporting, so a later caller must not see a clean nil for a
// teardown that failed.
func TestDaemonTeardownReportsTheCloseFailureToEveryCaller(t *testing.T) {
	closeErr := errors.New("backend close failed")
	stops := 0
	teardown := newDaemonTeardown(func() { stops++ }, func() error { return closeErr })

	require.ErrorIs(t, teardown(), closeErr)
	require.ErrorIs(t, teardown(), closeErr, "a later exit path must see the same outcome, not a clean nil")
	assert.Equal(t, 1, stops)
}
