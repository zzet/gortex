package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/platform"
)

// drainingController is a daemon whose store close takes closeFor: its
// Shutdown (the teardown the ack waits on) blocks that long, and the "process"
// counts as exited only once it returns. Status reports storage — the close
// checkpoint's estimate — when storage is non-nil, and nothing about the store
// otherwise (an older daemon, or one whose store is not SQLite).
type drainingController struct {
	*realController
	closeFor time.Duration
	storage  *daemon.StorageStatus
	exited   atomic.Bool
}

func (c *drainingController) Status(context.Context) (daemon.StatusResponse, error) {
	return daemon.StatusResponse{UptimeSeconds: 42, Storage: c.storage}, nil
}

func (c *drainingController) Shutdown(context.Context) error {
	time.Sleep(c.closeFor)
	c.exited.Store(true)
	return nil
}

// stopFixture stands up a fake daemon on a private socket and PID file and
// swaps the stop path's clocks and process control for ones driven by it. The
// PID file (written by Listen) names this test process, and the kill seam only records: nothing
// real is ever signalled.
type stopFixture struct {
	controller *drainingController
	kills      atomic.Int32
	stderr     bytes.Buffer
}

func newStopFixture(t *testing.T, controller *drainingController) *stopFixture {
	t.Helper()
	dir := t.TempDir()
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("gx-stop-drain-%d.sock", os.Getpid()))
	_ = os.Remove(socket)
	t.Cleanup(func() { _ = os.Remove(socket) })
	t.Setenv("XDG_CACHE_HOME", dir)
	t.Setenv("GORTEX_DAEMON_SOCKET", socket)
	pidFile := filepath.Join(dir, "d.pid")
	t.Setenv("GORTEX_DAEMON_PIDFILE", pidFile)
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(dir, "d.state.json"))

	f := &stopFixture{controller: controller}
	serviceActive = func() bool { return false }
	prevAck, prevNoAck, prevAcked, prevCap, prevProgress := daemonShutdownAckWait,
		daemonNoAckExitGrace, daemonAckedExitGrace, daemonExitGraceCap, daemonDrainProgressPeriod
	prevAlive, prevKill := daemonProcessAlive, daemonKillProcess
	t.Cleanup(func() {
		restoreServiceSeams()
		daemonShutdownAckWait, daemonNoAckExitGrace, daemonAckedExitGrace = prevAck, prevNoAck, prevAcked
		daemonExitGraceCap, daemonDrainProgressPeriod = prevCap, prevProgress
		daemonProcessAlive, daemonKillProcess = prevAlive, prevKill
	})
	// Scaled clocks: the ack is given up on at 100ms, a daemon with no
	// reported backlog is force-killed 300ms later, and the normal exit
	// grace (the margin added to an estimate) is 500ms.
	daemonShutdownAckWait = 100 * time.Millisecond
	daemonNoAckExitGrace = 300 * time.Millisecond
	daemonAckedExitGrace = 500 * time.Millisecond
	daemonExitGraceCap = 10 * time.Second
	daemonDrainProgressPeriod = 250 * time.Millisecond
	daemonProcessAlive = func(pid int) bool {
		if pid != os.Getpid() {
			return platform.ProcessAlive(pid)
		}
		return !controller.exited.Load() && f.kills.Load() == 0
	}
	daemonKillProcess = func(int) error { f.kills.Add(1); return nil }

	srv := daemon.New(socket, "test", zap.NewNop())
	srv.Controller = controller
	require.NoError(t, srv.Listen())
	// Listen writes this process's PID, which is what the stop path waits on.
	pid, ok := daemon.RunningPID()
	require.True(t, ok)
	require.Equal(t, os.Getpid(), pid)
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	t.Cleanup(func() {
		_ = srv.Shutdown()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Error("fake daemon did not stop")
		}
	})
	require.Eventually(t, func() bool { return daemon.IsRunningAt(socket) }, 2*time.Second, 10*time.Millisecond)
	return f
}

func (f *stopFixture) stop(t *testing.T) time.Duration {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetErr(&f.stderr)
	cmd.SetOut(&f.stderr)
	started := time.Now()
	require.NoError(t, runDaemonStop(cmd, nil))
	return time.Since(started)
}

// TestDaemonStopWaitsOutTheReportedCloseCheckpoint pins the stop path against
// the store's close-checkpoint estimate. The daemon reports a backlog whose
// estimate (1.2s) outlasts the no-ack grace (300ms here), and its close takes
// 1s — so the ack is late. The old schedule force-killed the daemon 300ms into
// the wait, in the middle of the checkpoint; an interrupted checkpoint records
// no progress, and the WAL survived the restart whole. The wait must now cover
// the estimate, say what it is waiting on, and end on the daemon's own exit.
func TestDaemonStopWaitsOutTheReportedCloseCheckpoint(t *testing.T) {
	f := newStopFixture(t, &drainingController{
		realController: &realController{},
		closeFor:       time.Second,
		storage: &daemon.StorageStatus{
			WALBytes:                  512 << 20,
			WALPendingFrames:          123456,
			CloseCheckpointEstimateMS: 1200,
		},
	})

	took := f.stop(t)
	out := f.stderr.String()

	assert.Zero(t, f.kills.Load(), "the stop force-killed a daemon inside its reported close checkpoint:\n%s", out)
	assert.True(t, f.controller.exited.Load(), "the stop returned before the daemon's close finished")
	assert.Contains(t, out, "no shutdown ack yet")
	assert.Contains(t, out, "draining 123456 WAL frames, up to 1s before force-killing",
		"the wait must say what it is waiting on: estimate 1.2s + 500ms exit grace, truncated")
	assert.Contains(t, out, "still draining the WAL", "a long drain wait must report progress")
	assert.NotContains(t, out, "graceful shutdown timed out")
	assert.Less(t, took, 5*time.Second, "the wait must end on the exit, not run out the grace")
}

// TestDaemonStopKeepsItsScheduleWithoutAnEstimate pins the unchanged half: a
// daemon that reports no store (an older daemon, or a status that failed)
// keeps the no-ack grace, and a close that overruns it is force-killed as
// before, with no drain line.
func TestDaemonStopKeepsItsScheduleWithoutAnEstimate(t *testing.T) {
	f := newStopFixture(t, &drainingController{
		realController: &realController{},
		closeFor:       time.Second,
	})

	f.stop(t)
	out := f.stderr.String()

	assert.Equal(t, int32(1), f.kills.Load(), "a daemon with no reported backlog keeps the old no-ack schedule")
	assert.Contains(t, out, "graceful shutdown timed out — force-killing")
	assert.NotContains(t, out, "draining")
}

// TestDaemonStopAfterAnAckKeepsTheShortGrace: an ack means the store is
// already checkpointed and closed (the daemon closes it before answering), so
// the estimate does not stretch the exit wait and no drain line is printed.
func TestDaemonStopAfterAnAckKeepsTheShortGrace(t *testing.T) {
	f := newStopFixture(t, &drainingController{
		realController: &realController{},
		closeFor:       20 * time.Millisecond,
		storage:        &daemon.StorageStatus{WALPendingFrames: 99, CloseCheckpointEstimateMS: 60_000},
	})

	took := f.stop(t)
	out := f.stderr.String()

	assert.Zero(t, f.kills.Load())
	assert.NotContains(t, out, "draining")
	assert.NotContains(t, out, "no shutdown ack yet")
	assert.Less(t, took, 5*time.Second)
}

// TestDaemonStopExitGraceIsBoundedByTheCap pins the grace arithmetic,
// including the hard cap that keeps a wedged daemon from holding `daemon stop`
// hostage to a huge estimate.
func TestDaemonStopExitGraceIsBoundedByTheCap(t *testing.T) {
	cases := []struct {
		name  string
		acked bool
		pre   daemonStopPreflight
		want  time.Duration
	}{
		{"acked ignores the estimate", true, daemonStopPreflight{closeEstimate: 2 * time.Minute, estimateKnown: true}, daemonExitGrace},
		{"no ack, estimate unknown", false, daemonStopPreflight{}, daemonBusyExitGrace},
		{"no ack, small estimate keeps the busy grace", false, daemonStopPreflight{closeEstimate: 30 * time.Second, estimateKnown: true}, daemonBusyExitGrace},
		{"no ack, estimate plus exit grace", false, daemonStopPreflight{closeEstimate: 2 * time.Minute, estimateKnown: true}, 2*time.Minute + daemonExitGrace},
		{"no ack, capped", false, daemonStopPreflight{closeEstimate: time.Hour, estimateKnown: true}, daemonMaxExitGrace},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, daemonStopExitGrace(tc.acked, tc.pre))
		})
	}
}
