package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/serverstack"
	"github.com/zzet/gortex/internal/testenv"
	"go.uber.org/zap"
)

func TestDaemonExitGuardRequiresBothSuccessfulOwnersAndPreservesFailure(t *testing.T) {
	for _, runtimeFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(runtimeFirst), func(t *testing.T) {
			g := newDaemonExitGuard(nil, io.Discard, nil, func(int) { t.Error("unexpected exit") })
			t.Cleanup(g.disarm)
			first, second := g.sharedClosed, g.runtimeClosed
			if runtimeFirst {
				first, second = second, first
			}
			first(nil)
			require.Equal(t, daemonExitPending, g.state.Load(), "one close cannot arm")
			second(nil)
			require.Equal(t, daemonExitArmed, g.state.Load())
			second(errors.New("actual physical close failed"))
			second(nil)
			first(nil)
			require.Equal(t, daemonExitFailed, g.state.Load(), "later nil erased actual error")
		})
	}
	for _, failedShared := range []bool{false, true} {
		g := newDaemonExitGuard(nil, io.Discard, nil, func(int) { t.Error("unexpected exit") })
		first, second := g.sharedClosed, g.runtimeClosed
		if !failedShared {
			first, second = second, first
		}
		first(errors.New("first close failed"))
		first(nil)
		second(nil)
		require.Equal(t, daemonExitFailed, g.state.Load(), "failed first receipt became success")
		g.disarm()
	}
}

func TestDaemonExitGuardConcurrentDisarmCannotArmOrClaimExit(t *testing.T) {
	for range 50 {
		g := newDaemonExitGuard(nil, io.Discard, nil, func(int) { t.Error("disarmed exit") })
		var wg sync.WaitGroup
		for _, f := range []func(){func() { g.sharedClosed(nil) }, func() { g.runtimeClosed(nil) }, g.disarm} {
			wg.Add(1)
			go func() { defer wg.Done(); f() }()
		}
		wg.Wait()
		require.Equal(t, daemonExitDisarmed, g.state.Load())
		require.False(t, g.state.CompareAndSwap(daemonExitArmed, daemonExitClaimed), "exit claimed after normal main disarm")
	}
}

type blockedDaemonDump struct {
	entered, release chan struct{}
	once             sync.Once
}

func (w *blockedDaemonDump) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

func TestDaemonExitGuardDisarmDoesNotJoinBlockedDiagnostic(t *testing.T) {
	log, err := os.CreateTemp(t.TempDir(), "daemon-log")
	require.NoError(t, err)
	t.Cleanup(func() { _ = log.Close() })
	w := &blockedDaemonDump{entered: make(chan struct{}), release: make(chan struct{})}
	g := newDaemonExitGuard(log, w, nil, func(int) { t.Error("unexpected exit") })
	dumped := make(chan struct{})
	go func() { defer close(dumped); g.diagnose() }()
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(w.release) }); <-dumped })
	select {
	case <-w.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("diagnostic did not reach the held sink")
	}
	// diagnose is actually blocked, not a synthetic unfinished flag.
	finished := make(chan struct{})
	go func() { g.disarm(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("normal disarm joined blocked diagnostic I/O")
	}
	release.Do(func() { close(w.release) })
}

func TestDaemonExitDiagnosticDoesNotClaimIncompleteDump(t *testing.T) {
	var stderr bytes.Buffer
	dumpDaemonExit(nil, &stderr, errors.New("log open failed"))
	require.Contains(t, stderr.String(), "goroutine")
	require.NotContains(t, stderr.String(), "dump complete")
}

func TestDaemonExitDiagnosticHandleSurvivesRotationWithoutReopening(t *testing.T) {
	dir := t.TempDir()
	path, rotated := filepath.Join(dir, "daemon.log"), filepath.Join(dir, "rotated.log")
	log, err := openDaemonExitLog(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = log.Close() })
	_, err = io.WriteString(log, "original log\n")
	require.NoError(t, err)
	require.NoError(t, os.Rename(path, rotated), "retained descriptor blocked rotation")
	require.NoError(t, os.WriteFile(path, []byte("successor log\n"), 0o600))
	dumpDaemonExit(log, io.Discard, nil)
	require.NoError(t, log.Close())
	old, err := os.ReadFile(rotated)
	require.NoError(t, err)
	require.Contains(t, string(old), "original log")
	require.Contains(t, string(old), "dump complete")
	newLog, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "successor log\n", string(newLog))
	bad, err := openDaemonExitLog(dir)
	require.ErrorContains(t, err, "not a regular file")
	require.Nil(t, bad)
}

func TestDaemonExitGuardInProcessInvocationCannotInstallProcessExit(t *testing.T) {
	require.Nil(t, mainDaemonLifetime)
	path := filepath.Join(t.TempDir(), "not-opened.log")
	g, err := installDaemonExitGuard(path, io.Discard)
	require.NoError(t, err)
	require.Nil(t, g)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

// A warning-only diagnostic wait has no outstanding owner in this fixture.
// Existing contract-drain tests prove the separate actual completion barrier.
type exitGuardWarningDrain struct{}

func (exitGuardWarningDrain) Close(context.Context) error { return context.DeadlineExceeded }
func (exitGuardWarningDrain) waitClosed() error           { return nil }

// This runs real main/Cobra finalizers, a real SharedServer/SQLite close and a
// real daemon runtime shutdown. Fixture startup is outside the 3-second clock.
// It is a local ownership regression, not native full-corpus stop acceptance.
func TestDaemonExitGuardActualMainFinalizerAndIndependentDeadline(t *testing.T) {
	unrelated, exitUnrelated, waitUnrelated, _ := ownedStopTarget(t)
	for _, mode := range []string{"outer-finalizer", "blocked-diagnostic", "error-exit", "contract-warning", "normal"} {
		t.Run(mode, func(t *testing.T) {
			root := testenv.ShortTempDir(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDaemonExitGuardProcessHelper$", "-test.count=1")
			cmd.Env = append(os.Environ(), "GORTEX_TEST_EXIT_GUARD_ROOT="+root, "GORTEX_TEST_EXIT_GUARD_MODE="+mode,
				"GORACE=atexit_sleep_ms=0") // Keep helper race instrumentation, not its exit sleep.
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			require.NoError(t, cmd.Start())
			t.Logf("owned exit-guard helper started: pid=%d argv=%q", cmd.Process.Pid, cmd.Args)
			identity, identityErr := platform.CaptureProcessIdentity(cmd.Process.Pid)
			if identityErr != nil {
				_ = cmd.Process.Kill() // The actual unreaped child, never a PID lookup.
				_ = cmd.Wait()
				t.Fatal(identityErr)
			}
			t.Logf("owned exit-guard helper native identity: %+v", identity)
			waitErr := cmd.Wait()
			reapedAt := time.Now()
			t.Logf("owned exit-guard helper reaped: pid=%d state=%s", cmd.Process.Pid, cmd.ProcessState)
			require.NoError(t, ctx.Err(), "helper exceeded outer bound: %s", stderr.String())
			if mode == "error-exit" {
				var exitErr *exec.ExitError
				require.ErrorAs(t, waitErr, &exitErr, "stdout=%s stderr=%s", stdout.String(), stderr.String())
				require.Equal(t, 7, exitErr.ExitCode())
			} else {
				require.NoError(t, waitErr, "stdout=%s stderr=%s", stdout.String(), stderr.String())
			}
			require.Contains(t, stdout.String(), "actual shared and runtime owners completed")
			require.Contains(t, stdout.String(), "command logger closed")
			for line := range strings.SplitSeq(stdout.String(), "\n") {
				if raw, ok := strings.CutPrefix(line, "owners_completed_unix_nano="); ok {
					ns, err := strconv.ParseInt(raw, 10, 64)
					require.NoError(t, err)
					// Excludes fixture construction; generous scheduler margin is
					// not a product shutdown-latency acceptance measurement.
					require.Less(t, reapedAt.Sub(time.Unix(0, ns)), 8*time.Second)
				}
			}
			require.Contains(t, stdout.String(), "owners_completed_unix_nano=")
			require.NotContains(t, stderr.String(), "DATA RACE")
			for _, name := range []string{"pid", "state.json", "sock"} {
				_, err := os.Lstat(filepath.Join(root, name))
				require.ErrorIs(t, err, os.ErrNotExist, "owned runtime survived: %s", name)
			}
			log, err := os.ReadFile(filepath.Join(root, "daemon.log"))
			require.NoError(t, err)
			if mode == "normal" {
				require.Contains(t, stdout.String(), "main final disarm completed")
				require.NotContains(t, string(log), "shutdown still pending")
			} else {
				require.Contains(t, string(log), "outer shutdown still pending")
				require.Contains(t, string(log), "TestDaemonExitGuardProcessHelper")
				if mode == "blocked-diagnostic" {
					require.NotContains(t, string(log), "dump complete")
				} else {
					require.Contains(t, string(log), "dump complete", "closed command logger must not close the watchdog sink")
				}
			}
			// The child must release its real lock and leave a durable store that
			// is immediately reopenable; exit0 alone does not certify either.
			lock, err := captureDaemonStoreLock(filepath.Join(root, "store.sqlite"))
			require.NoError(t, err)
			free, err := lock.probe()
			require.NoError(t, err)
			require.True(t, free)
			st, err := store_sqlite.Open(filepath.Join(root, "store.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = st.Close() })
			node := st.GetNode("exit-guard-fixture")
			require.NotNil(t, node)
			require.Equal(t, "ExitGuardFixture", node.Name)
			require.NoError(t, st.Close())
			running, err := unrelated.process.Running()
			require.NoError(t, err)
			require.True(t, running, "watchdog exit affected an unrelated owned child")
		})
	}
	require.NoError(t, exitUnrelated())
	require.NoError(t, waitUnrelated())
}

func TestDaemonExitGuardProcessHelper(t *testing.T) {
	root := os.Getenv("GORTEX_TEST_EXIT_GUARD_ROOT")
	if root == "" {
		t.Skip("owned child entrypoint")
	}
	mode := os.Getenv("GORTEX_TEST_EXIT_GUARD_MODE")
	// TestMain has installed its normal sandbox. Apply only explicit fixture
	// paths afterwards, as production path env vars were intentionally reset.
	for key, name := range map[string]string{"GORTEX_DAEMON_PIDFILE": "pid", "GORTEX_DAEMON_STATEFILE": "state.json"} {
		require.NoError(t, os.Setenv(key, filepath.Join(root, name)))
	}
	for _, key := range []string{"GORTEX_GO_TYPES", "GORTEX_TELEMETRY"} {
		require.NoError(t, os.Setenv(key, "0"))
	}
	rootCmd = &cobra.Command{Use: "gortex", SilenceUsage: true}
	rootCmd.PersistentPostRun = func(_ *cobra.Command, _ []string) {
		if mode != "normal" {
			<-make(chan struct{}) // Real outer finalizer after command defers.
		}
	}
	fixture := &cobra.Command{Use: "owned-exit-guard", RunE: func(_ *cobra.Command, _ []string) error {
		var commandErr error
		if mode == "error-exit" {
			commandErr = &exitCodeError{code: 7}
			// Like a failed Serve followed by a blocked deferred finalizer:
			// preserve its actual return error before the defer can stall.
			defer func() { <-make(chan struct{}) }()
		}
		logPath := filepath.Join(root, "daemon.log")
		inheritedLog, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		defer func() { _ = inheritedLog.Close() }()
		// A real inherited daemon-log handle selects the rotating logger path,
		// whose closeLog physically closes its writer, not merely stderr Sync.
		logger, closeLog := configureDaemonLogger(newLogger(), config.DaemonConfig{}, logPath, &inheritedLog, &inheritedLog)
		defer func() { closeLog(); fmt.Println("command logger closed") }()
		logger.Info("owned exit-guard command logger active")
		var stderr io.Writer = os.Stderr
		if mode == "blocked-diagnostic" {
			stderr = &blockedDaemonDump{entered: make(chan struct{}), release: make(chan struct{})}
		}
		g, err := installDaemonExitGuard(logPath, stderr)
		require.NoError(t, err)
		require.NotNil(t, g)
		cfg := config.Default()
		cfg.Semantic.Enabled = false
		shared, err := serverstack.NewSharedServer(serverstack.SharedServerConfig{
			Lifecycle: serverstack.LifecycleDaemon, Index: root, BackendPath: filepath.Join(root, "store.sqlite"),
			Config: cfg, Global: &config.GlobalConfig{}, Logger: zap.NewNop(), Version: "owned-exit-guard",
			Embedder:    serverstack.EmbedderRequest{FlagChanged: true, FlagEnabled: false},
			SavingsPath: filepath.Join(root, "savings.sqlite"), SavingsLegacyJSON: filepath.Join(root, "savings.json"),
		})
		require.NoError(t, err)
		shared.Graph.AddNode(&graph.Node{ID: "exit-guard-fixture", Kind: graph.KindFunction, Name: "ExitGuardFixture"})
		reporter := newDaemonStartupReporter(logger)
		srv := daemon.New(filepath.Join(root, "sock"), "owned-exit-guard", logger)
		srv.RuntimeStateFile = filepath.Join(root, "state.json")
		srv.CancelShutdownOwners, srv.JoinRuntimeWriter = reporter.Cancel, reporter.Stop
		srv.OnShutdownComplete = g.runtimeClosed
		require.NoError(t, srv.Listen())
		reporter.Serving(shared.StorePath)
		if mode == "error-exit" {
			g.setExitCode(commandErr)
		}
		// Vary actual completion order, not just synthetic receipt order.
		if mode == "outer-finalizer" {
			require.NoError(t, srv.Shutdown())
		}
		closeShared := func() error {
			closeErr := shared.Close()
			g.sharedClosed(closeErr)
			return closeErr
		}
		if mode == "contract-warning" {
			err = closeDaemonOwners(context.Background(), exitGuardWarningDrain{}, closeShared, zap.NewNop())
			require.ErrorIs(t, err, context.DeadlineExceeded)
		} else {
			require.NoError(t, closeShared())
		}
		require.NoError(t, srv.Shutdown())
		fmt.Printf("owners_completed_unix_nano=%d\n", time.Now().UnixNano())
		fmt.Println("actual shared and runtime owners completed")
		return commandErr
	}}
	rootCmd.AddCommand(fixture)
	os.Args = []string{"gortex", "owned-exit-guard"}
	main() // Actual process-only install and outermost disarm scope.
	require.Equal(t, daemonExitDisarmed, mainDaemonLifetime.guard.state.Load())
	fmt.Println("main final disarm completed")
}
