package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/testenv"
)

// Exercise the real CLI routing/probe/MCP/error path with piped stdout. Process
// startup is deliberately outside this bound; the runtime probes also measure
// the complete installed CLI invocation on the reference host.
func TestNonTTYCLI_LoadingSocketReturnsTempFailWithin100ms(t *testing.T) {
	dispatcher, mi, _, loading := loadingDispatcherSetup(t)
	controller := probeController(t, fmt.Sprintf("repos:\n  - path: %q\n", loading))
	controller.multiIndexer = mi
	controller.graph = mi.Graph()
	dir := testenv.ShortTempDir(t)
	socket := filepath.Join(dir, "s")
	t.Setenv("GORTEX_DAEMON_SOCKET", socket)
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "p"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(dir, "state.json"))
	t.Setenv("GORTEX_AUTOSTART", "0")
	srv := daemon.New(socket, "test", zap.NewNop())
	srv.Controller = controller
	srv.MCPDispatcher = dispatcher
	require.NoError(t, srv.Listen())
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()
	t.Cleanup(func() {
		require.NoError(t, srv.Shutdown())
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("owned CLI loading server did not stop")
		}
	})

	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	stdout := os.Stdout
	os.Stdout = writer
	flag := rootCmd.PersistentFlags().Lookup("loading-wait")
	changed := flag.Changed
	flag.Changed = false
	t.Cleanup(func() {
		os.Stdout = stdout
		flag.Changed = changed
		_ = writer.Close()
		_ = reader.Close()
	})
	require.Zero(t, effectiveLoadingWait(), "piped CLI stdout must not use the interactive wait")

	started := time.Now()
	_, err = requireDaemonTool(loading, "search_symbols", map[string]any{"query": "LoadProbe"})
	elapsed := time.Since(started)
	var coded interface{ ExitCode() int }
	require.ErrorAs(t, err, &coded)
	assert.Equal(t, exitTempFail, coded.ExitCode(), "execute must propagate the loading exit contract")
	assert.LessOrEqual(t, elapsed, 100*time.Millisecond)
	assert.Contains(t, err.Error(), "still loading")
	assert.NotContains(t, err.Error(), "gortex track")
	assert.NotContains(t, err.Error(), "repos reconcile")
}
