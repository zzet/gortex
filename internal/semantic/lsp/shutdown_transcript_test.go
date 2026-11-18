package lsp

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const helperExitSyncStallEnv = "GORTEX_LSP_HELPER_EXIT_SYNC_STALL"

// A transcript backend whose durability operation stalls only after the exit
// frame. Ordinary writes remain visible, and ping/shutdown responses complete.
// This isolates storage durability from protocol readiness or process startup.
type exitSyncStallTranscript struct {
	*os.File
	entered string
	exit    bool
}

func (w *exitSyncStallTranscript) Write(p []byte) (int, error) {
	w.exit = bytes.Equal(bytes.TrimSpace(p), []byte("exit"))
	return w.File.Write(p)
}
func (w *exitSyncStallTranscript) Sync() error {
	if !w.exit {
		return w.File.Sync()
	}
	if err := os.WriteFile(w.entered, []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
		panic(err) // A broken fault fixture must fail, never silently disarm.
	}
	for {
		time.Sleep(time.Hour)
	} // An actual blocked fixture operation; owner must reap it.
}

var _ interface{ Sync() error } = (*exitSyncStallTranscript)(nil)

func TestShutdownTranscriptStorageStallCannotPreventOwnedChildExit(t *testing.T) {
	entered := filepath.Join(t.TempDir(), "exit-sync-entered")
	t.Setenv(helperExitSyncStallEnv, entered)
	c, tr, trace := shutdownClientFixture(t, helperLSPGraceful)
	original := tr.cmd
	stdin, stdout, err := tr.Start()
	require.ErrorContains(t, err, "single-use")
	require.Nil(t, stdin)
	require.Nil(t, stdout)
	require.Same(t, original, tr.cmd)
	require.NoError(t, c.Call("test/after-rejected-start", nil, nil))
	err = awaitClientShutdown(t, c)
	if pid, readErr := os.ReadFile(entered); readErr == nil {
		require.Equal(t, fmt.Sprint(original.Process.Pid), string(pid))
		t.Logf("exact owned child pid=%s reached the injected exit transcript durability stall", pid)
	}
	require.NoError(t, err, "transcript durability must not hold a cooperative child in the graceful-exit path")
	require.NotNil(t, original.ProcessState)
	require.Equal(t, original.Process.Pid, original.ProcessState.Pid())
	require.True(t, original.ProcessState.Success())
	require.Same(t, original, tr.cmd)
	require.NoError(t, tr.Stop())
	methods, err := os.ReadFile(trace)
	require.NoError(t, err)
	require.Equal(t, []string{"test/after-rejected-start", "shutdown", "exit"}, strings.Fields(string(methods)))
}
