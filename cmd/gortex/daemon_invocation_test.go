package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/telemetry"
)

func daemonInvocationFixture(t *testing.T) *cobra.Command {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv(telemetry.EnvTelemetry, "1")
	t.Setenv("GORTEX_SESSION_ID", "foreground-invocation")
	t.Setenv("GORTEX_DAEMON_CHILD", "")
	root := &cobra.Command{Use: "gortex"}
	d := &cobra.Command{Use: "daemon"}
	s := &cobra.Command{Use: "start"}
	root.AddCommand(d)
	d.AddCommand(s)
	return s
}

func TestForegroundDaemonRecordsBeforeTeardownAndNeverReopensPostRunSidecar(t *testing.T) {
	cmd := daemonInvocationFixture(t)
	path := redirectCLIEventSidecar(t)
	recordForegroundDaemonInvocation(cmd)
	store := telemetry.NewStore(platform.TelemetryDir())
	day := telemetry.DayKey(time.Now())
	roll, err := store.Load(day)
	require.NoError(t, err)
	require.EqualValues(t, 1, roll.Counts["cli_command:daemon.start"])
	sc, err := persistence.OpenSidecar(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sc.Close() })
	rows, err := sc.CLIEventsBySession("foreground-invocation")
	require.NoError(t, err)
	require.Len(t, rows, 1, "invocation was not durable before teardown")
	require.Equal(t, "daemon.start", rows[0].Verb)
	require.NoError(t, sc.Close())

	// A different target makes even a post-close open (not merely duplicate
	// insertion into the old DB) observable. This is the actual post-run path.
	late := filepath.Join(t.TempDir(), "must-not-open.sqlite")
	cliEventSidecarPath = func() string { return late }
	recordCLIInvocationAfterRun(cmd)
	_, err = os.Stat(late)
	require.ErrorIs(t, err, os.ErrNotExist)
	roll, err = store.Load(day)
	require.NoError(t, err)
	require.EqualValues(t, 1, roll.Counts["cli_command:daemon.start"])
	require.NotContains(t, cmd.Annotations, foregroundDaemonRecorded)
}

func TestDetachedDaemonParentAccountingAndChildExclusionStayUnchanged(t *testing.T) {
	cmd := daemonInvocationFixture(t)
	path := redirectCLIEventSidecar(t)
	// The detach parent never enters foreground and still records in post-run.
	recordCLIInvocationAfterRun(cmd)
	t.Setenv("GORTEX_DAEMON_CHILD", "1")
	recordForegroundDaemonInvocation(cmd)
	recordCLIInvocationAfterRun(cmd)
	sc, err := persistence.OpenSidecar(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sc.Close() })
	rows, err := sc.CLIEventsBySession("foreground-invocation")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	roll, err := telemetry.NewStore(platform.TelemetryDir()).Load(telemetry.DayKey(time.Now()))
	require.NoError(t, err)
	require.EqualValues(t, 1, roll.Counts["cli_command:daemon.start"])
}
