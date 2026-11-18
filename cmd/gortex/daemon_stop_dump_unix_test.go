//go:build darwin || linux

package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/platform"
)

func TestDaemonStopDumpActuallyTerminatesOnlyCapturedGoChild(t *testing.T) {
	unrelated, exitUnrelated, waitUnrelated, _ := ownedStopTarget(t)
	t.Setenv("GORTEX_STOP_DUMP", "1")
	t.Setenv("GORTEX_TEST_STOP_IGNORE_QUIT", "0")
	var stderr bytes.Buffer
	target, _, wait, _ := ownedStopTargetWithStderr(t, false, &stderr)
	var out bytes.Buffer
	result, err := waitForDaemonTarget(&out, target, 0, false)
	require.NoError(t, err)
	require.True(t, result.diagnostic)
	require.False(t, result.forced, "default Go SIGQUIT should terminate without a KILL")
	require.Error(t, wait(), "actual default SIGQUIT must terminate the child")
	// Wait joins stderr collection before reading it; signal receipt alone is
	// not evidence that Go actually emitted its diagnostic stack.
	require.Contains(t, stderr.String(), "SIGQUIT")
	require.Contains(t, stderr.String(), "goroutine")
	require.NoError(t, reportCapturedDaemonStop(&out, result, nil, "owned fixture", 0))
	require.Contains(t, out.String(), "SIGQUIT diagnostic termination requested; store free")
	require.NotContains(t, out.String(), "then force-killed")
	running, err := unrelated.process.Running()
	require.NoError(t, err)
	require.True(t, running)
	require.NoError(t, exitUnrelated())
	require.NoError(t, waitUnrelated())
}

func TestDaemonStopDumpIgnoredQuitIsRefencedBeforeActualKill(t *testing.T) {
	t.Setenv("GORTEX_STOP_DUMP", "1")
	t.Setenv("GORTEX_TEST_STOP_IGNORE_QUIT", "1")
	target, _, wait, _ := ownedStopTarget(t)
	previousQuit, previousKill := daemonDiagnosticTerminate, daemonKillCapturedProcess
	var order []string
	daemonDiagnosticTerminate = func(p platform.ProcessIdentity) error {
		order = append(order, "QUIT")
		return previousQuit(p) // Actual OS signal to the explicitly ignoring child.
	}
	daemonKillCapturedProcess = func(p platform.ProcessIdentity) error {
		order = append(order, "KILL")
		free, err := target.storeFree()
		require.NoError(t, err)
		require.False(t, free, "KILL admitted after a free-store proof")
		same, err := p.Matches()
		require.NoError(t, err)
		require.True(t, same)
		return previousKill(p)
	}
	t.Cleanup(func() { daemonDiagnosticTerminate, daemonKillCapturedProcess = previousQuit, previousKill })
	var out bytes.Buffer
	result, err := waitForDaemonTarget(&out, target, 0, false)
	require.NoError(t, err)
	require.True(t, result.diagnostic)
	require.True(t, result.forced)
	require.Equal(t, []string{"QUIT", "KILL"}, order)
	require.Error(t, wait())
	require.NoError(t, reportCapturedDaemonStop(&out, result, nil, "owned fixture", 0))
	require.Contains(t, out.String(), "SIGQUIT diagnostic termination attempted, then force-killed")
}

// Simulate store release during intent output, after the normal grace loop.
// The final signal fences must run after output, not trust an earlier poll.
type releaseOnDiagnosticIntent struct {
	bytes.Buffer
	match   string
	release func() error
	err     error
}

func (w *releaseOnDiagnosticIntent) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.match) && w.release != nil {
		w.err = w.release()
		w.release = nil
	}
	return w.Buffer.Write(p)
}

func TestDaemonStopDumpRefusesLateFreeAndUnknownOrReusedOwnership(t *testing.T) {
	for _, mode := range []string{"late-free", "unknown", "reused-runtime"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("GORTEX_STOP_DUMP", "1")
			target, exit, wait, unlock := ownedStopTarget(t)
			previousQuit, previousKill, previousRunning := daemonDiagnosticTerminate, daemonKillCapturedProcess, daemonCapturedProcessRunning
			calls := 0
			refuse := func(platform.ProcessIdentity) error { calls++; return errors.New("unexpected diagnostic/kill") }
			daemonDiagnosticTerminate, daemonKillCapturedProcess = refuse, refuse
			t.Cleanup(func() {
				daemonDiagnosticTerminate, daemonKillCapturedProcess, daemonCapturedProcessRunning = previousQuit, previousKill, previousRunning
			})
			out := &releaseOnDiagnosticIntent{match: "before SIGQUIT"}
			switch mode {
			case "late-free":
				out.release = func() error {
					if err := unlock(); err != nil {
						return err
					}
					for _, file := range target.files {
						if err := os.Remove(file.path); err != nil {
							return err
						}
					}
					return nil
				}
			case "unknown":
				daemonCapturedProcessRunning = func(platform.ProcessIdentity) (bool, error) { return false, platform.ErrProcessIdentityChanged }
			case "reused-runtime":
				require.NoError(t, os.WriteFile(target.files[0].path, []byte("foreign incarnation"), 0o600))
			}
			result, err := waitForDaemonTarget(out, target, 0, false)
			require.Zero(t, calls)
			require.False(t, result.diagnostic)
			if mode == "late-free" {
				require.NoError(t, out.err)
				require.NoError(t, err)
				require.NotContains(t, out.String(), "timed out", "late free-store success was described as a timeout")
			} else {
				require.Error(t, err)
			}
			running, err := target.process.Running()
			require.NoError(t, err)
			require.True(t, running)
			require.NoError(t, exit())
			require.NoError(t, wait())
		})
	}
}

func TestDaemonStopDumpIgnoredQuitCannotAuthorizeKillAfterLateStoreRelease(t *testing.T) {
	t.Setenv("GORTEX_STOP_DUMP", "1")
	t.Setenv("GORTEX_TEST_STOP_IGNORE_QUIT", "1")
	target, exit, wait, unlock := ownedStopTarget(t)
	previousKill := daemonKillCapturedProcess
	daemonKillCapturedProcess = func(platform.ProcessIdentity) error { t.Error("KILL used a stale pre-QUIT proof"); return nil }
	t.Cleanup(func() { daemonKillCapturedProcess = previousKill })
	out := &releaseOnDiagnosticIntent{match: "before force-killing", release: func() error {
		if err := unlock(); err != nil {
			return err
		}
		for _, file := range target.files {
			if err := os.Remove(file.path); err != nil {
				return err
			}
		}
		return nil
	}}
	result, err := waitForDaemonTarget(out, target, 0, false)
	require.NoError(t, err)
	require.NoError(t, out.err)
	require.True(t, result.diagnostic)
	require.False(t, result.forced)
	require.NotContains(t, out.String(), "timed out", "late release after QUIT was described as a KILL timeout")
	running, err := target.process.Running()
	require.NoError(t, err)
	require.True(t, running, "ignored-QUIT child was killed after its store became free")
	require.NoError(t, exit())
	require.NoError(t, wait())
}

func TestDaemonStopDumpSignalErrorIsRetainedWithoutKill(t *testing.T) {
	t.Setenv("GORTEX_STOP_DUMP", "1")
	target, exit, wait, _ := ownedStopTarget(t)
	previousQuit, previousKill := daemonDiagnosticTerminate, daemonKillCapturedProcess
	failure := errors.New("actual diagnostic signal admission failed")
	daemonDiagnosticTerminate = func(platform.ProcessIdentity) error { return failure }
	daemonKillCapturedProcess = func(platform.ProcessIdentity) error { t.Error("KILL after failed QUIT"); return nil }
	t.Cleanup(func() { daemonDiagnosticTerminate, daemonKillCapturedProcess = previousQuit, previousKill })
	result, err := waitForDaemonTarget(&bytes.Buffer{}, target, 0, false)
	require.ErrorIs(t, err, failure)
	require.False(t, result.diagnostic)
	running, err := target.process.Running()
	require.NoError(t, err)
	require.True(t, running)
	require.NoError(t, exit())
	require.NoError(t, wait())
}
