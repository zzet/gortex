package main

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/testenv"
)

func TestCLIStartupRefusalKeepsKnownScopeBeforeControllerHandoff(t *testing.T) {
	dir, err := filepath.EvalSymlinks(testenv.ShortTempDir(t))
	require.NoError(t, err)
	primary, linked := fakeLinkedWorktree(t, filepath.Join(dir, "configured"))
	unrelated, foreignLinked := fakeLinkedWorktree(t, filepath.Join(dir, "unconfigured"))
	family, ok := linkedWorktreeAt(linked)
	require.True(t, ok)
	require.Equal(t, primary, family.mainRepo)

	socket := filepath.Join(dir, "s")
	t.Setenv("GORTEX_DAEMON_SOCKET", socket)
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "p"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(dir, "state"))
	t.Setenv("GORTEX_AUTOSTART", "0")
	flag := rootCmd.PersistentFlags().Lookup("loading-wait")
	oldWait, oldChanged := loadingWait, flag.Changed
	loadingWait, flag.Changed = 0, true
	t.Cleanup(func() { loadingWait, flag.Changed = oldWait, oldChanged })

	logger := zap.NewNop()
	reporter := newDaemonStartupReporterWithWriter(logger, func(daemon.RuntimeState) error { return nil })
	reporter.ObserveMigration(store_sqlite.MigrationProgress{Version: 40, Phase: store_sqlite.MigrationStarted})
	t.Cleanup(reporter.Stop)
	metadata := newMigrationMetadataDispatcher(config.Default(), []config.RepoEntry{{Path: primary, Name: "configured"}}, logger, reporter)
	srv := daemon.New(socket, "startup-scope-test", logger)
	srv.MCPDispatcher = metadata
	srv.EnableStartupMetadata()
	require.NoError(t, srv.Listen())
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()
	t.Cleanup(func() {
		require.NoError(t, srv.Shutdown())
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("owned startup listener did not stop")
		}
	})

	control, err := daemon.DialToWithDeadline(socket, daemon.Handshake{Mode: daemon.ModeControl}, time.Now().Add(3*time.Second))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, control.Close()) })
	pending, err := control.ControlWithTimeout(daemon.ControlProbe, nil, daemonRoutingProbeTimeout)
	require.NoError(t, err)
	require.False(t, pending.OK)
	require.Equal(t, daemon.ErrRepoLoading, pending.ErrorCode)
	require.True(t, pending.Retryable)

	client := openStartupIntrospectionSession(t, socket, primary, "core", "defer", time.Now().Add(3*time.Second))
	_, reply := startupIntrospectionRoundTrip(t, client, 2, "tools/call", map[string]any{"name": "index_health"})
	health := startupIntrospectionHealth(t, reply, "migrating")
	require.Len(t, health.Repos, 1)
	require.Equal(t, primary, health.Repos[0].Path)
	require.NotEmpty(t, control.Ack.DaemonInstance)
	require.False(t, startupConfiguresCWD(control.Ack.DaemonInstance+"-stale", primary),
		"the same configured root cannot establish ownership for a different daemon instance")

	for _, test := range []struct {
		name, cwd string
		known     bool
	}{
		{"configured_primary", primary, true},
		{"configured_linked", linked, true},
		{"unrelated_primary", unrelated, false},
		{"unrelated_linked", foreignLinked, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var diagnostics bytes.Buffer
			_, err := requireDaemonToolWithSurface(test.cwd, "search_symbols", map[string]any{"query": "LoadProbe"},
				"core", "defer", &diagnostics)
			require.Error(t, err)
			var loading *repoLoadingError
			if !test.known {
				require.NotErrorAs(t, err, &loading, "global startup does not establish this checkout's ownership")
				require.ErrorContains(t, err, "could not verify repository ownership")
				require.NotContains(t, err.Error(), "gortex track")
				return
			}
			require.ErrorAs(t, err, &loading, "configured scope got %v (%s)", err, diagnostics.String())
			assert.Equal(t, exitTempFail, loading.ExitCode())
			assert.Equal(t, test.cwd, loading.repo)
			assert.Equal(t, "repo_loading", loading.details.ErrorCode)
			require.NotNil(t, loading.details.Retryable)
			assert.True(t, *loading.details.Retryable)
			assert.Equal(t, "migrating", loading.details.Phase)
			require.NotNil(t, loading.details.Percent)
			assert.Zero(t, *loading.details.Percent)
			assert.NotContains(t, loading.Error(), "gortex track")
			assert.NotContains(t, loading.Error(), "repos reconcile")
		})
	}
}
