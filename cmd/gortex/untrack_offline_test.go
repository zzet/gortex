package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/testenv"
)

func offlineUntrackFixture(t *testing.T) (string, string) {
	t.Helper()
	runtimeDir := testenv.ShortTempDir(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GORTEX_AUTOSTART", "off")
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(runtimeDir, "absent.sock"))
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(runtimeDir, "absent.pid"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(runtimeDir, "absent.state"))
	keep, remove := t.TempDir(), t.TempDir()
	gc, err := config.LoadGlobal()
	require.NoError(t, err)
	gc.Repos = []config.RepoEntry{{Path: keep, Name: "keep"}, {Path: remove, Name: "remove"}}
	require.NoError(t, gc.Save())
	return keep, remove
}

func TestUntrackOfflineRemovesOnlyTrackedAbsolutePath(t *testing.T) {
	keep, remove := offlineUntrackFixture(t)
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&output)
	require.NoError(t, runUntrack(cmd, []string{remove}))
	require.Contains(t, output.String(), "(config only)")
	gc, err := config.LoadGlobal()
	require.NoError(t, err)
	require.Len(t, gc.Repos, 1)
	require.True(t, pathkey.EqualPaths(gc.Repos[0].Path, keep))
	require.Equal(t, "keep", gc.Repos[0].Name)
}

func TestUntrackOfflineUnknownPathLeavesConfigUnchanged(t *testing.T) {
	offlineUntrackFixture(t)
	before, err := os.ReadFile(config.DefaultGlobalConfigPath())
	require.NoError(t, err)
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	err = runUntrack(cmd, []string{t.TempDir()})
	require.ErrorContains(t, err, "repository not found")
	after, err := os.ReadFile(config.DefaultGlobalConfigPath())
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestUntrackOfflineUnresolvablePrefixLeavesConfigUnchanged(t *testing.T) {
	_, remove := offlineUntrackFixture(t)
	// The tracked Name is deliberately unrelated to the working directory.
	// Offline untrack resolves this as a path, not as a configured prefix.
	t.Chdir(t.TempDir())
	require.False(t, strings.HasSuffix(remove, string(filepath.Separator)+"remove"))
	before, err := os.ReadFile(config.DefaultGlobalConfigPath())
	require.NoError(t, err)
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	require.ErrorContains(t, runUntrack(cmd, []string{"remove"}), "repository not found")
	after, err := os.ReadFile(config.DefaultGlobalConfigPath())
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestUntrackOfflineConfigRemovalLogIsExactlyOnce(t *testing.T) {
	keep, remove := offlineUntrackFixture(t)
	priorLevel := logLevel
	logLevel = "info"
	t.Cleanup(func() { logLevel = priorLevel })
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&output)
	var removedErr error
	stderr, _ := captureLegacyStderr(t, func() { removedErr = runUntrack(cmd, []string{remove}) })
	require.NoError(t, removedErr)
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == "config: removed tracked repo" {
			require.Equal(t, 1, strings.Count(line, `"caller":`), "the stable caller must not duplicate zap's automatic caller key")
			entries = append(entries, entry)
		}
	}
	require.Len(t, entries, 1, "actual offline removal must emit exactly one structured event: %s", stderr)
	require.Equal(t, "info", entries[0]["level"])
	require.Equal(t, "untrack", entries[0]["cause"])
	require.Equal(t, "runUntrack", entries[0]["caller"])
	require.Equal(t, remove, entries[0]["root"])
	require.Contains(t, output.String(), "(config only)")
	persisted, err := config.LoadGlobal()
	require.NoError(t, err)
	require.Len(t, persisted.Repos, 1)
	require.True(t, pathkey.EqualPaths(keep, persisted.Repos[0].Path), "the event must describe an actual persisted removal")
	before, err := os.ReadFile(config.DefaultGlobalConfigPath())
	require.NoError(t, err)
	for _, target := range []string{remove, t.TempDir()} {
		var notFound error
		stderr, _ := captureLegacyStderr(t, func() { notFound = runUntrack(cmd, []string{target}) })
		require.ErrorContains(t, notFound, "repository not found")
		require.NotContains(t, stderr, "config: removed tracked repo", "repeat/unknown target did not remove any config entry")
		after, err := os.ReadFile(config.DefaultGlobalConfigPath())
		require.NoError(t, err)
		require.Equal(t, before, after)
	}
}
