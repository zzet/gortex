//go:build windows

package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/platform"
)

func TestDaemonStopDumpWindowsReportsUnavailableWithoutSubstituteSignal(t *testing.T) {
	t.Setenv("GORTEX_STOP_DUMP", "1")
	target, _, wait, _ := ownedStopTarget(t)
	require.ErrorContains(t, target.process.DiagnosticTerminate(), "unavailable on Windows")
	running, err := target.process.Running()
	require.NoError(t, err)
	require.True(t, running)
	previousQuit := daemonDiagnosticTerminate
	daemonDiagnosticTerminate = func(platform.ProcessIdentity) error { t.Error("synthetic Windows diagnostic signal"); return nil }
	t.Cleanup(func() { daemonDiagnosticTerminate = previousQuit })
	var out bytes.Buffer
	result, err := waitForDaemonTarget(&out, target, 0, false)
	require.NoError(t, err)
	require.True(t, result.forced)
	require.False(t, result.diagnostic)
	require.Error(t, wait())
	require.Contains(t, out.String(), "SIGQUIT diagnostic termination is unavailable on Windows")
}
