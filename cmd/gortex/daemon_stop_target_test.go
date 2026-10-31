//go:build darwin || linux || windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/testenv"
	"go.uber.org/zap"
)

type legacyStopEndpointController struct {
	*realController
	blocked     <-chan struct{}
	enteredPath string
}

func (*legacyStopEndpointController) Status(context.Context) (daemon.StatusResponse, error) {
	return daemon.StatusResponse{UptimeSeconds: 1}, nil
}

func (c *legacyStopEndpointController) Shutdown(context.Context) error {
	if err := os.WriteFile(c.enteredPath, []byte("actual shutdown request entered"), 0o600); err != nil {
		return err
	}
	<-c.blocked // Deliberate uncooperative helper: prove authorized PID fallback.
	return nil
}

func TestDaemonStopTargetChild(t *testing.T) {
	backend := os.Getenv("GORTEX_TEST_STOP_TARGET_BACKEND")
	if backend == "" {
		return
	}
	if os.Getenv("GORTEX_TEST_STOP_IGNORE_QUIT") == "1" {
		signal.Ignore(syscall.SIGQUIT) // Explicit Unix diagnostic-escalation fixture.
	}
	// TestMain intentionally replaces inherited production daemon overrides.
	// Reapply only these explicit test-owned paths after that sandbox exists;
	// never exempt this child from the package's HOME/XDG isolation.
	root := os.Getenv("GORTEX_TEST_STOP_TARGET_ROOT")
	if root == "" {
		os.Exit(11)
	}
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(root, "daemon.pid"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(root, "daemon.json"))
	t.Setenv("GORTEX_DAEMON_SOCKET", os.Getenv("GORTEX_TEST_STOP_TARGET_SOCKET"))
	lock := flock.New(backend + ".lock")
	held, err := lock.TryLock()
	if err != nil || !held {
		fmt.Fprintln(os.Stderr, "cannot acquire owned helper store lock", err)
		os.Exit(2)
	}
	if os.Getenv("GORTEX_TEST_STOP_LEGACY_ENDPOINT") == "1" {
		// Exact old-schema JSON, bypassing the new writer's native stamp.
		content, err := json.Marshal(map[string]any{"pid": os.Getpid(), "backend_path": backend})
		if err != nil {
			os.Exit(7)
		}
		if err := os.WriteFile(daemon.RuntimeStatePath(), content, 0o600); err != nil {
			os.Exit(8)
		}
		srv := daemon.New(daemon.SocketPath(), "legacy-schema-fixture", zap.NewNop())
		srv.Controller = &legacyStopEndpointController{realController: &realController{}, blocked: make(chan struct{}), enteredPath: backend + ".shutdown-entered"}
		if err := srv.Listen(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(9)
		}
		fmt.Fprintln(os.Stdout, "ready")
		err = srv.Serve()
		runtime.KeepAlive(lock)
		if err != nil {
			os.Exit(10)
		}
		os.Exit(0)
	}
	if err := os.WriteFile(daemon.PIDFilePath(), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(3)
	}
	if err := daemon.WriteRuntimeState(daemon.RuntimeState{BackendPath: backend}); err != nil {
		os.Exit(4)
	}
	if err := os.WriteFile(daemon.SocketPath(), []byte("owned helper runtime endpoint"), 0o600); err != nil {
		os.Exit(5)
	}
	fmt.Fprintln(os.Stdout, "ready")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		if scanner.Text() == "unlock" {
			if err := lock.Unlock(); err != nil {
				os.Exit(6)
			}
			fmt.Fprintln(os.Stdout, "unlocked")
		}
	}
	runtime.KeepAlive(lock)
	// Intentionally leave runtime files and the lock descriptor: actual OS
	// exit must release the lock; the parent tests safe stale-file cleanup.
	os.Exit(0)
}

func ownedStopTarget(t *testing.T) (*daemonStopTarget, func() error, func() error, func() error) {
	return ownedStopTargetMode(t, false)
}

func ownedStopTargetMode(t *testing.T, legacyEndpoint bool) (*daemonStopTarget, func() error, func() error, func() error) {
	return ownedStopTargetWithStderr(t, legacyEndpoint, nil)
}

func ownedStopTargetWithStderr(t *testing.T, legacyEndpoint bool, stderr io.Writer) (*daemonStopTarget, func() error, func() error, func() error) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "daemon.pid"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(dir, "daemon.json"))
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(dir, "daemon.sock"))
	if legacyEndpoint {
		t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(testenv.ShortTempDir(t), "s"))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDaemonStopTargetChild$")
	cmd.Stderr = stderr
	cmd.Env = append(os.Environ(), "GORTEX_TEST_STOP_TARGET_BACKEND="+filepath.Join(dir, "store.sqlite"),
		"GORTEX_TEST_STOP_TARGET_ROOT="+dir, "GORTEX_TEST_STOP_TARGET_SOCKET="+daemon.SocketPath(), "GORACE=atexit_sleep_ms=0", "GOTRACEBACK=all")
	if legacyEndpoint {
		cmd.Env = append(cmd.Env, "GORTEX_TEST_STOP_LEGACY_ENDPOINT=1")
	}
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Logf("owned stop helper started: pid=%d argv=%q", cmd.Process.Pid, cmd.Args)
	var once sync.Once
	var waitErr error
	wait := func() error {
		once.Do(func() {
			waitErr = cmd.Wait()
			t.Logf("owned stop helper reaped: pid=%d state=%v error=%v", cmd.Process.Pid, cmd.ProcessState, waitErr)
		})
		return waitErr
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = wait() })
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", line)
	target, err := captureDaemonStopTarget(cmd.Process.Pid)
	require.NoError(t, err)
	unlock := func() error {
		if _, err := fmt.Fprintln(stdin, "unlock"); err != nil {
			return err
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if line != "unlocked\n" {
			return fmt.Errorf("unexpected child unlock receipt: %q", line)
		}
		return nil
	}
	return target, stdin.Close, wait, unlock
}

func TestDaemonStopTargetRetainsDeclaredStoreAfterRuntimeRemoval(t *testing.T) {
	target, exit, wait, _ := ownedStopTarget(t)
	free, err := target.storeFree()
	require.NoError(t, err)
	require.False(t, free)
	require.ErrorContains(t, target.removeExitedRuntime(), "still running")
	require.NoError(t, exit())
	require.NoError(t, wait()) // Actual helper process reaped before cleanup.
	free, err = target.storeFree()
	require.NoError(t, err)
	require.True(t, free, "actual OS exit did not release the captured store lock")
	require.NoError(t, target.removeExitedRuntime())
	gone, err := target.runtimeGone()
	require.NoError(t, err)
	require.True(t, gone)
	// The pre-stop store identity remains usable after ReadRuntimeState can
	// no longer discover a live daemon or its removed state file.
	_, live := daemon.ReadRuntimeState()
	require.False(t, live)
	free, err = target.storeFree()
	require.NoError(t, err)
	require.True(t, free)
}

func TestDaemonStopTargetDoesNotFallbackWhenExpectedLockIsMissing(t *testing.T) {
	target, exit, wait, _ := ownedStopTarget(t)
	path := target.store.path
	require.NoError(t, exit())
	require.NoError(t, wait())
	require.NoError(t, os.Remove(path))
	free, err := target.storeFree()
	require.ErrorIs(t, err, os.ErrNotExist)
	require.False(t, free, "a known store with missing lock became a no-store fallback")
	_, err = os.Lstat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestDaemonStopTargetRefusesChangedRuntimeWithoutRemovingOtherFiles(t *testing.T) {
	target, exit, wait, _ := ownedStopTarget(t)
	require.NoError(t, exit())
	require.NoError(t, wait())
	// Rewrite in place: unchanged inode alone is not sufficient provenance.
	require.NoError(t, os.WriteFile(daemon.PIDFilePath(), []byte(strconv.Itoa(target.process.PID()+1)), 0o600))
	gone, err := target.runtimeGone()
	require.ErrorContains(t, err, "changed content")
	require.False(t, gone)
	require.ErrorContains(t, target.removeExitedRuntime(), "changed content")
	for _, file := range target.files {
		_, err := os.Lstat(file.path)
		require.NoError(t, err, "changed provenance must refuse the whole cleanup")
	}
}

func TestDaemonStopTargetLegacyNoStoreFallbackRequiresActualExit(t *testing.T) {
	known, exit, wait, _ := ownedStopTarget(t)
	require.NoError(t, os.Remove(daemon.RuntimeStatePath()))
	legacy, err := captureDaemonStopTarget(known.process.PID())
	require.NoError(t, err)
	require.True(t, legacy.noStore)
	require.Nil(t, legacy.store, "legacy fallback must not guess the default store")
	free, err := legacy.storeFree()
	require.NoError(t, err)
	require.False(t, free, "live process with undeclared store was called free")
	require.NoError(t, exit())
	require.NoError(t, wait())
	free, err = legacy.storeFree()
	require.NoError(t, err)
	require.True(t, free)
	require.NoError(t, legacy.removeExitedRuntime())
}

func TestDaemonStopRuntimeFileRefusesNewAndReplacedPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.pid")
	missing, err := captureDaemonStopFile(path, true)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("123"), 0o600))
	_, err = missing.present()
	require.ErrorContains(t, err, "appeared or changed identity")
	captured, err := captureDaemonStopFile(path, true)
	require.NoError(t, err)
	require.NoError(t, os.Rename(path, path+".old"))
	require.NoError(t, os.WriteFile(path, []byte("123"), 0o600))
	_, err = captured.present()
	require.ErrorContains(t, err, "changed identity", "identical content cannot certify a replacement inode")
	_, err = captureDaemonStopFile(t.TempDir(), false)
	require.ErrorContains(t, err, "unexpected daemon endpoint file type")
}

func TestDaemonStopTargetRefusesPresentMismatchedBirthToken(t *testing.T) {
	target, exit, wait, _ := ownedStopTarget(t)
	require.True(t, target.signalOwnerVerified)
	content, err := os.ReadFile(daemon.RuntimeStatePath())
	require.NoError(t, err)
	var state daemon.RuntimeState
	require.NoError(t, json.Unmarshal(content, &state))
	state.ProcessBirthToken = "different-native-incarnation"
	content, err = json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(daemon.RuntimeStatePath(), content, 0o600))
	_, err = captureDaemonStopTarget(target.process.PID())
	require.ErrorIs(t, err, platform.ErrProcessIdentityChanged)
	require.ErrorIs(t, stopRunningDaemon(&bytes.Buffer{}), platform.ErrProcessIdentityChanged)
	running, err := target.process.Running()
	require.NoError(t, err)
	require.True(t, running)
	require.NoError(t, exit())
	require.NoError(t, wait(), "mismatched owner must not receive a signal")
}

func TestDaemonStopLegacySignalNeedsMatchingSuccessfulProbe(t *testing.T) {
	target, exit, wait, _ := ownedStopTarget(t)
	content, err := os.ReadFile(daemon.RuntimeStatePath())
	require.NoError(t, err)
	var state daemon.RuntimeState
	require.NoError(t, json.Unmarshal(content, &state))
	state.ProcessBirthToken = "" // Exact legacy JSON omits this optional field.
	content, err = json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(daemon.RuntimeStatePath(), content, 0o600))
	legacy, err := captureDaemonStopTarget(target.process.PID())
	require.NoError(t, err)
	require.False(t, legacy.signalOwnerVerified)
	require.NoError(t, legacy.verifyBeforeControl(), "legacy graceful control remains allowed")
	require.Error(t, legacy.acceptProbeOwner(legacy.pid()+1))
	require.False(t, legacy.signalOwnerVerified)
	_, err = terminateCapturedDaemon(&bytes.Buffer{}, legacy)
	require.ErrorContains(t, err, "cannot verify daemon incarnation")
	_, err = waitForDaemonTarget(&bytes.Buffer{}, legacy, 0, false)
	require.ErrorContains(t, err, "cannot verify daemon incarnation")
	running, err := legacy.process.Running()
	require.NoError(t, err)
	require.True(t, running)
	require.NoError(t, exit())
	require.NoError(t, wait(), "unverified legacy fallback must not signal this actual process")
}
