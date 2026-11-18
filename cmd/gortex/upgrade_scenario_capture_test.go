//go:build acceptance && (linux || darwin)

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The existing runner must preserve a failing child's two actual streams,
// even when startup fails before it can return a completed run receipt.
// This child is a tiny shell script, not a daemon or an upgrade campaign.
func TestUpgradeScenarioRetainsFailedChildOutput(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "failed-child")
	require.NoError(t, os.WriteFile(binary, []byte("#!/bin/sh\nprintf 'raw stdout before failure\\n'\nprintf 'raw stderr before failure\\n' >&2\nexit 7\n"), 0o700))
	receipt := filepath.Join(root, "receipt.json")
	self, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(self, "-test.run=^TestHelperUpgradeScenarioFailedOutput$", "-test.timeout=10s")
	cmd.Env = append(os.Environ(), "GORTEX_UPGRADE_CAPTURE_HELPER=1", "GORTEX_UPGRADE_CAPTURE_CHILD="+binary, upgradeScenarioOutEnv+"="+receipt)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "the child failure must not become a successful run")
	require.Contains(t, string(out), "exit status 7", "retain the original wait error")
	stdout, err := os.ReadFile(receipt + ".interrupted.stdout.log")
	require.NoError(t, err, "raw stdout must survive an early startup failure")
	require.Equal(t, "raw stdout before failure\n", string(stdout))
	stderr, err := os.ReadFile(receipt + ".interrupted.stderr.log")
	require.NoError(t, err, "raw stderr must remain separate")
	require.Equal(t, "raw stderr before failure\n", string(stderr))
	partial, err := os.ReadFile(receipt + ".interrupted.run.json")
	require.NoError(t, err, "a failed runner must retain its partial postmortem")
	var run upgradeScenarioRun
	require.NoError(t, json.Unmarshal(partial, &run))
	require.NotNil(t, run.ExitCode)
	require.Equal(t, 7, *run.ExitCode)
	require.Contains(t, run.WaitError, "exit status 7")
	require.Equal(t, receipt+".interrupted.stdout.log", run.StdoutPath)
	require.Equal(t, receipt+".interrupted.stderr.log", run.StderrPath)
}

func TestHelperUpgradeScenarioFailedOutput(t *testing.T) {
	if os.Getenv("GORTEX_UPGRADE_CAPTURE_HELPER") != "1" {
		t.Skip("owned failing-child output control only")
	}
	root := t.TempDir()
	runScenarioDaemon(t, os.Getenv("GORTEX_UPGRADE_CAPTURE_CHILD"), os.Environ(), filepath.Join(root, "unused.sqlite"), filepath.Join(root, "absent.sock"), root, time.Now(), true)
}
