package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
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
	watcherStops  atomic.Int32
	producerStops atomic.Int32
	sharedCloses  atomic.Int32
	store         *store_sqlite.Store
	mu            sync.Mutex
	order         []string
}

func newTeardownProbe(t *testing.T) *teardownProbe {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "teardown.sqlite"))
	require.NoError(t, err)
	// A write so the close has a WAL to checkpoint rather than a fresh file.
	store.AddBatch([]*graph.Node{
		{ID: "pkg/a.go::Alpha", Kind: graph.KindFunction, Name: "Alpha", FilePath: "pkg/a.go"},
	}, nil)
	return &teardownProbe{store: store}
}

func (p *teardownProbe) record(step string) {
	p.mu.Lock()
	p.order = append(p.order, step)
	p.mu.Unlock()
}

func (p *teardownProbe) stopWatcher() {
	p.watcherStops.Add(1)
	p.record("watcher")
}

func (p *teardownProbe) stopProducers() {
	p.producerStops.Add(1)
	p.record("producers")
}

// closeShared stands in for the shared stack's Close: it runs the backend
// close that checkpoints the WAL. Closing an already-closed store is exactly
// the double-teardown this chain must not perform, so the count matters.
func (p *teardownProbe) closeShared() error {
	p.sharedCloses.Add(1)
	p.record("shared")
	return p.store.Close()
}

func (p *teardownProbe) shutdownOrder() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.order...)
}

// TestDaemonTeardownRunsOnASignalledExit drives the real signal path. The
// daemon server handles SIGINT/SIGTERM itself: it shuts the listener down
// directly and Serve returns, without the controller ever hearing about it.
// With the teardown installed only on the controller's hook, a signalled
// daemon skipped watcher shutdown, the savings flush, and the final WAL
// checkpoint outright — the store was left as the process found it.
func TestDaemonTeardownRunsOnASignalledExit(t *testing.T) {
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

	srv := daemon.New(daemon.SocketPath(), "test", zap.NewNop())
	require.NoError(t, srv.Listen())
	probe := newTeardownProbe(t)
	runTeardown := installDaemonTeardown(
		probe.stopWatcher, probe.stopProducers, probe.closeShared,
	)

	// Exactly the shape of runDaemonStart's tail: the teardown is deferred
	// around the serve loop, and nothing else runs it.
	stopped := make(chan error, 1)
	go func() {
		serveErr := srv.Serve()
		teardownErr := completeDaemonShutdown(nil, runTeardown, srv.ReleaseProcessState)
		stopped <- errors.Join(serveErr, teardownErr)
	}()

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
	assert.Equal(t, int32(1), probe.producerStops.Load())
	assert.Equal(t, []string{"producers", "watcher", "shared"}, probe.shutdownOrder())
	_, pidErr := os.Stat(daemon.PIDFilePath())
	require.ErrorIs(t, pidErr, os.ErrNotExist, "process ownership must be released after shared teardown")
}

// TestDaemonTeardownRunsOnceAcrossBothExitPaths pins the once guard. The
// control-socket stop and the signalled exit are independent, and a daemon
// that is signalled while a `gortex daemon stop` is in flight reaches both. A
// second run would close an already-closed store and flush a torn-down stack.
func TestDaemonTeardownRunsOnceAcrossBothExitPaths(t *testing.T) {
	probe := newTeardownProbe(t)
	runTeardown := installDaemonTeardown(
		probe.stopWatcher, probe.stopProducers, probe.closeShared,
	)

	// A signal and control shutdown can race before Serve returns. Whichever
	// deferred exit observes it, every phase still runs exactly once.
	var exits sync.WaitGroup
	teardownErrs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		exits.Add(1)
		go func() {
			defer exits.Done()
			teardownErrs <- runTeardown()
		}()
	}
	exits.Wait()
	close(teardownErrs)
	for err := range teardownErrs {
		require.NoError(t, err)
	}
	require.NoError(t, runTeardown())

	assert.Equal(t, int32(1), probe.watcherStops.Load(), "the watcher must be stopped exactly once")
	assert.Equal(t, int32(1), probe.producerStops.Load(), "startup producers must be stopped exactly once")
	assert.Equal(t, int32(1), probe.sharedCloses.Load(), "the shared stack must be closed exactly once")
	assert.Equal(t, []string{"producers", "watcher", "shared"}, probe.shutdownOrder())
}

func TestControllerShutdownDoesNotCloseStackInsideRequestHandler(t *testing.T) {
	probe := newTeardownProbe(t)
	runTeardown := installDaemonTeardown(
		probe.stopWatcher, probe.stopProducers, probe.closeShared,
	)
	controller := &realController{}
	require.NoError(t, controller.Shutdown(context.Background()))
	assert.Zero(t, probe.watcherStops.Load())
	assert.Zero(t, probe.producerStops.Load())
	assert.Zero(t, probe.sharedCloses.Load())

	require.NoError(t, runTeardown())
	assert.Equal(t, int32(1), probe.sharedCloses.Load())
}

// TestDaemonTeardownReportsTheCloseFailureToEveryCaller: whoever exits second
// is often the one reporting, so a later caller must not see a clean nil for a
// teardown that failed.
func TestDaemonTeardownReportsTheCloseFailureToEveryCaller(t *testing.T) {
	closeErr := errors.New("backend close failed")
	stops := 0
	teardown := newDaemonTeardown(func() { stops++ }, nil, func() error { return closeErr })

	require.ErrorIs(t, teardown(), closeErr)
	require.ErrorIs(t, teardown(), closeErr, "a later exit path must see the same outcome, not a clean nil")
	assert.Equal(t, 1, stops)
}

func TestDaemonTeardownJoinsProducersBeforeSharedClose(t *testing.T) {
	producerEntered := make(chan struct{})
	releaseProducer := make(chan struct{})
	sharedEntered := make(chan struct{})
	teardown := newDaemonTeardown(nil, func() {
		close(producerEntered)
		<-releaseProducer
	}, func() error {
		close(sharedEntered)
		return nil
	})
	done := make(chan error, 1)
	go func() { done <- teardown() }()
	<-producerEntered
	select {
	case <-sharedEntered:
		t.Fatal("shared close started before startup producers joined")
	case <-time.After(10 * time.Millisecond):
	}
	close(releaseProducer)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("teardown did not finish after producer join")
	}
}

func TestDaemonTeardownRetainsPIDUntilSharedCloseCompletes(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("gx-pid-fence-%d.sock", os.Getpid()))
	t.Cleanup(func() { _ = os.Remove(socket) })
	t.Setenv("GORTEX_DAEMON_SOCKET", socket)
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "d.pid"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(dir, "d.state.json"))

	srv := daemon.New(daemon.SocketPath(), "test", zap.NewNop())
	require.NoError(t, srv.Listen())
	require.NoError(t, daemon.WriteRuntimeState(daemon.RuntimeState{PID: os.Getpid()}))
	require.NoError(t, srv.Shutdown(), "transport must close before graph teardown")

	sharedEntered := make(chan struct{})
	releaseShared := make(chan struct{})
	teardown := newDaemonTeardown(nil, nil, func() error {
		close(sharedEntered)
		<-releaseShared
		return nil
	})
	done := make(chan error, 1)
	go func() {
		done <- completeDaemonShutdown(nil, teardown, srv.ReleaseProcessState)
	}()

	select {
	case <-sharedEntered:
	case <-time.After(time.Second):
		t.Fatal("shared close did not start")
	}
	pid, running := daemon.RunningPID()
	require.True(t, running, "PID must remain visible while the shared store is closing")
	require.Equal(t, os.Getpid(), pid)
	_, runtimeVisible := daemon.ReadRuntimeState()
	require.True(t, runtimeVisible, "runtime state must share the PID ownership lifetime")
	select {
	case err := <-done:
		t.Fatalf("teardown returned before shared close was released: %v", err)
	case <-time.After(10 * time.Millisecond):
	}

	close(releaseShared)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("teardown did not finish after shared close")
	}
	_, pidErr := os.Stat(daemon.PIDFilePath())
	require.ErrorIs(t, pidErr, os.ErrNotExist)
	_, runtimeErr := os.Stat(daemon.RuntimeStatePath())
	require.ErrorIs(t, runtimeErr, os.ErrNotExist)
}

func TestCompleteDaemonShutdownStopsRuntimePublisherBeforeTeardownAndReleasesLast(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
	)
	record := func(step string) {
		mu.Lock()
		order = append(order, step)
		mu.Unlock()
	}
	closeErr := errors.New("close failed")
	runTeardown := newDaemonTeardown(
		func() { record("watcher") },
		func() { record("producers") },
		func() error {
			record("shared")
			return closeErr
		},
	)

	err := completeDaemonShutdown(
		func() { record("runtime-publisher") },
		runTeardown,
		func() { record("process-state") },
	)
	require.ErrorIs(t, err, closeErr)
	require.Equal(t,
		[]string{"runtime-publisher", "producers", "watcher", "shared", "process-state"},
		order,
	)
}

func TestJoinedReconcileLoopStopJoinsAdmittedTick(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var tickOnce sync.Once
	stop := startJoinedReconcileLoop(time.Millisecond, func(context.Context) {
		tickOnce.Do(func() {
			close(entered)
			<-release
		})
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("reconcile tick was not admitted")
	}

	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("janitor stop returned before its admitted tick joined")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("janitor stop did not finish after admitted tick returned")
	}
	stop()
}

func BenchmarkDaemonTeardownJoinedPhases(b *testing.B) {
	stop := func() {}
	closeShared := func() error { return nil }
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		teardown := newDaemonTeardown(stop, stop, closeShared)
		if err := teardown(); err != nil {
			b.Fatal(err)
		}
	}
}
