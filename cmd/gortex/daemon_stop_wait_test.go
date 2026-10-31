//go:build darwin || linux || windows

package main

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/platform"
)

func TestDaemonStopFreeStoreAndGoneRuntimeNeverKillsLiveFinalizer(t *testing.T) {
	target, exit, wait, unlock := ownedStopTarget(t)
	require.NoError(t, unlock()) // Physical helper lock released, child still live.
	for _, file := range target.files {
		require.NoError(t, os.Remove(file.path))
	}
	previous := daemonKillCapturedProcess
	var kills atomic.Int32
	daemonKillCapturedProcess = func(platform.ProcessIdentity) error { kills.Add(1); return errors.New("unexpected kill") }
	t.Cleanup(func() { daemonKillCapturedProcess = previous })
	var out bytes.Buffer
	started := time.Now()
	result, err := waitForDaemonTarget(&out, target, 0, false)
	require.NoError(t, err)
	require.False(t, result.forced)
	require.Less(t, time.Since(started), 500*time.Millisecond, "free store/runtime should complete in its first poll")
	require.Zero(t, kills.Load())
	require.Contains(t, out.String(), "still exiting")
	require.NotContains(t, out.String(), "reaped", "live helper is not a proven zombie")
	require.NotContains(t, out.String(), "timed out")
	running, err := target.process.Running()
	require.NoError(t, err)
	require.True(t, running, "completion must not terminate the actual live finalizer fixture")
	require.NoError(t, exit())
	require.NoError(t, wait())
}

func TestDaemonStopDumpDefaultAndNonOneKeepExistingKillPath(t *testing.T) {
	for _, value := range []string{"", "true"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("GORTEX_STOP_DUMP", value)
			target, _, wait, _ := ownedStopTarget(t)
			previous := daemonDiagnosticTerminate
			daemonDiagnosticTerminate = func(platform.ProcessIdentity) error { t.Error("diagnostic was not opted in"); return nil }
			t.Cleanup(func() { daemonDiagnosticTerminate = previous })
			var out bytes.Buffer
			result, err := waitForDaemonTarget(&out, target, 0, false)
			require.NoError(t, err)
			require.True(t, result.forced)
			require.False(t, result.diagnostic)
			require.Error(t, wait())
			require.NotContains(t, out.String(), "SIGQUIT")
			require.Contains(t, out.String(), "graceful shutdown timed out — sent force-kill")
		})
	}
}

func TestDaemonStopForceActuallyReleasesAndReapsOnlyOwnedChild(t *testing.T) {
	unrelated, exitUnrelated, waitUnrelated, _ := ownedStopTarget(t)
	target, _, wait, _ := ownedStopTarget(t)
	var out bytes.Buffer
	result, err := waitForDaemonTarget(&out, target, 0, false)
	require.NoError(t, err)
	require.True(t, result.forced)
	require.Error(t, wait(), "actual force signal must terminate this exact helper")
	free, err := target.storeFree()
	require.NoError(t, err)
	require.True(t, free)
	gone, err := target.runtimeGone()
	require.NoError(t, err)
	require.True(t, gone)
	running, err := unrelated.process.Running()
	require.NoError(t, err)
	require.True(t, running)
	require.NoError(t, exitUnrelated())
	require.NoError(t, waitUnrelated())
}

func TestDaemonStopFailedKillRetainsErrorAndActualRuntime(t *testing.T) {
	target, exit, wait, _ := ownedStopTarget(t)
	failure := errors.New("injected captured kill failure")
	previous := daemonKillCapturedProcess
	daemonKillCapturedProcess = func(platform.ProcessIdentity) error { return failure }
	t.Cleanup(func() { daemonKillCapturedProcess = previous })
	_, err := waitForDaemonTarget(&bytes.Buffer{}, target, 0, false)
	require.ErrorIs(t, err, failure)
	for _, file := range target.files {
		present, err := file.present()
		require.NoError(t, err)
		require.True(t, present)
	}
	running, err := target.process.Running()
	require.NoError(t, err)
	require.True(t, running)
	require.NoError(t, exit())
	require.NoError(t, wait())
}

func TestDaemonStopSuccessfulSignalWithoutExitIsNotCompletion(t *testing.T) {
	target, exit, wait, _ := ownedStopTarget(t)
	previous := daemonKillCapturedProcess
	daemonKillCapturedProcess = func(platform.ProcessIdentity) error { return nil } // Deliberately no OS signal.
	t.Cleanup(func() { daemonKillCapturedProcess = previous })
	result, err := waitForDaemonTarget(&bytes.Buffer{}, target, 0, false)
	require.ErrorContains(t, err, "did not release its store and runtime")
	require.False(t, result.forced, "a fake signal receipt cannot certify a successful force-stop")
	running, stateErr := target.process.Running()
	require.NoError(t, stateErr)
	require.True(t, running)
	for _, file := range target.files {
		present, err := file.present()
		require.NoError(t, err)
		require.True(t, present)
	}
	require.NoError(t, exit())
	require.NoError(t, wait())
}

func TestDaemonStopUnknownProcessStateCannotSignalOrCleanRuntime(t *testing.T) {
	target, exit, wait, _ := ownedStopTarget(t)
	previousRunning, previousKill := daemonCapturedProcessRunning, daemonKillCapturedProcess
	var kills atomic.Int32
	daemonCapturedProcessRunning = func(platform.ProcessIdentity) (bool, error) { return false, platform.ErrProcessIdentityChanged }
	daemonKillCapturedProcess = func(platform.ProcessIdentity) error { kills.Add(1); return nil }
	t.Cleanup(func() { daemonCapturedProcessRunning, daemonKillCapturedProcess = previousRunning, previousKill })
	_, err := waitForDaemonTarget(&bytes.Buffer{}, target, 0, false)
	require.ErrorIs(t, err, platform.ErrProcessIdentityChanged)
	require.Zero(t, kills.Load())
	for _, file := range target.files {
		present, err := file.present()
		require.NoError(t, err)
		require.True(t, present)
	}
	require.NoError(t, exit())
	require.NoError(t, wait())
}

func TestDaemonStopChangedRuntimeCannotSignalCapturedProcess(t *testing.T) {
	target, exit, wait, _ := ownedStopTarget(t)
	require.NoError(t, os.WriteFile(daemon.PIDFilePath(), []byte(strconv.Itoa(target.process.PID()+1)), 0o600))
	previous := daemonKillCapturedProcess
	var kills atomic.Int32
	daemonKillCapturedProcess = func(platform.ProcessIdentity) error { kills.Add(1); return nil }
	t.Cleanup(func() { daemonKillCapturedProcess = previous })
	_, err := waitForDaemonTarget(&bytes.Buffer{}, target, 0, false)
	require.ErrorContains(t, err, "changed runtime ownership")
	require.Zero(t, kills.Load())
	running, stateErr := target.process.Running()
	require.NoError(t, stateErr)
	require.True(t, running)
	require.NoError(t, exit())
	require.NoError(t, wait())
}

func TestDaemonStopCLIFallbackJoinsActualOwnedProcessAndStoreLock(t *testing.T) {
	target, _, wait, _ := ownedStopTarget(t)
	var out bytes.Buffer
	// The fixture endpoint is a regular owned file, not a listener: this
	// deliberately exercises the CLI's identity-checked TERM fallback.
	require.NoError(t, stopRunningDaemon(&out))
	require.Error(t, wait(), "the actual helper must have received its own termination signal")
	free, err := target.storeFree()
	require.NoError(t, err)
	require.True(t, free)
	gone, err := target.runtimeGone()
	require.NoError(t, err)
	require.True(t, gone)
	require.Contains(t, out.String(), "stopped")
}

func TestDaemonStopReapedBeforeCaptureCleansOnlyPinnedStaleRuntime(t *testing.T) {
	original, exit, wait, _ := ownedStopTarget(t)
	require.NoError(t, exit())
	require.NoError(t, wait()) // Deliberately reap BEFORE the CLI/native capture.
	stale, err := captureDaemonStopTarget(original.process.PID())
	require.NoError(t, err)
	require.Equal(t, original.process.PID(), stale.absentPID)
	require.Zero(t, stale.process.PID(), "absence must not fabricate a reusable birth identity")
	require.NotNil(t, stale.store, "stale path must use its declared pinned store, never a default")
	previousKill, previousTerminate := daemonKillCapturedProcess, daemonTerminateCapturedProcess
	var signals atomic.Int32
	refuse := func(platform.ProcessIdentity) error {
		signals.Add(1)
		return errors.New("stale PID must never be signaled")
	}
	daemonKillCapturedProcess, daemonTerminateCapturedProcess = refuse, refuse
	t.Cleanup(func() { daemonKillCapturedProcess, daemonTerminateCapturedProcess = previousKill, previousTerminate })
	var out bytes.Buffer
	require.NoError(t, stopRunningDaemon(&out))
	require.Zero(t, signals.Load())
	free, err := stale.storeFree()
	require.NoError(t, err)
	require.True(t, free)
	gone, err := stale.runtimeGone()
	require.NoError(t, err)
	require.True(t, gone)
}

func TestDaemonStopTermRechecksStoreAfterNativeAndRuntimeReads(t *testing.T) {
	target, exit, wait, unlock := ownedStopTarget(t)
	previousRunning, previousTerminate := daemonCapturedProcessRunning, daemonTerminateCapturedProcess
	var once sync.Once
	var actionErr error
	var signals atomic.Int32
	daemonCapturedProcessRunning = func(identity platform.ProcessIdentity) (bool, error) {
		once.Do(func() {
			actionErr = unlock()
			if actionErr == nil {
				for _, file := range target.files {
					actionErr = errors.Join(actionErr, os.Remove(file.path))
				}
			}
		})
		if actionErr != nil {
			return false, actionErr
		}
		return identity.Running()
	}
	daemonTerminateCapturedProcess = func(platform.ProcessIdentity) error {
		signals.Add(1)
		return errors.New("unexpected TERM after free store")
	}
	t.Cleanup(func() {
		daemonCapturedProcessRunning, daemonTerminateCapturedProcess = previousRunning, previousTerminate
	})
	result, err := terminateCapturedDaemon(&bytes.Buffer{}, target)
	require.NoError(t, err)
	require.False(t, result.forced)
	require.Zero(t, signals.Load())
	running, err := target.process.Running()
	require.NoError(t, err)
	require.True(t, running, "observed store-free tail must remain unsignaled")
	require.NoError(t, exit())
	require.NoError(t, wait())
}

func TestDaemonStopLegacyProbeAuthorizesActualOwnedFallbackAndPreservesOtherChild(t *testing.T) {
	unrelated, exitUnrelated, waitUnrelated, _ := ownedStopTarget(t)
	legacy, _, wait, _ := ownedStopTargetMode(t, true)
	require.False(t, legacy.signalOwnerVerified, "old JSON must not pretend to have a native stamp")
	previousAck, previousGrace := daemonShutdownAckWait, daemonNoAckExitGrace
	daemonShutdownAckWait, daemonNoAckExitGrace = 500*time.Millisecond, 150*time.Millisecond
	t.Cleanup(func() { daemonShutdownAckWait, daemonNoAckExitGrace = previousAck, previousGrace })
	// The actual CLI receives a successful real ControlProbe from this
	// endpoint. Its PID must authorize fallback despite the exact legacy
	// record lacking a stamp. Shutdown itself deliberately does not ACK.
	var out bytes.Buffer
	require.NoError(t, stopRunningDaemon(&out))
	require.Error(t, wait(), "the actual legacy-schema helper must receive the intended fallback signal")
	require.Contains(t, out.String(), "force-killed")
	entered, err := os.ReadFile(strings.TrimSuffix(legacy.store.path, ".lock") + ".shutdown-entered")
	require.NoError(t, err)
	require.Equal(t, "actual shutdown request entered", string(entered))
	free, err := legacy.storeFree()
	require.NoError(t, err)
	require.True(t, free)
	gone, err := legacy.runtimeGone()
	require.NoError(t, err)
	require.True(t, gone)
	running, err := unrelated.process.Running()
	require.NoError(t, err)
	require.True(t, running, "matching legacy endpoint must not authorize signaling another child")
	require.NoError(t, exitUnrelated())
	require.NoError(t, waitUnrelated())
}
