package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
)

func TestRunTrackOfflineSymlinkAlreadyTracked(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	root := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.Mkdir(root, 0o755))
	alias := filepath.Join(t.TempDir(), "src")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	gc, err := config.LoadGlobal()
	require.NoError(t, err)
	require.NoError(t, gc.AddRepo(config.RepoEntry{Path: alias, Name: "src"}))
	require.NoError(t, gc.Save())
	before, err := os.ReadFile(gc.ConfigPath())
	require.NoError(t, err)
	origWait, origName, origAs := trackWait, trackName, trackAsWorktree
	origEnsure := trackEnsureDaemonReadyFn
	trackWait, trackName, trackAsWorktree = false, "", false
	trackEnsureDaemonReadyFn = func(bool) daemonDecision { return daemonUnavailable }
	t.Cleanup(func() {
		trackWait, trackName, trackAsWorktree = origWait, origName, origAs
		trackEnsureDaemonReadyFn = origEnsure
	})
	cmd := &cobra.Command{}
	output := &bytes.Buffer{}
	cmd.SetErr(output)
	require.NoError(t, runTrack(cmd, []string{root}))
	after, err := os.ReadFile(gc.ConfigPath())
	require.NoError(t, err)
	assert.Equal(t, before, after, "re-tracking an alias must not add a second entry")
	assert.Contains(t, output.String(), "already tracked:")
}

func TestRunConfigExcludeListUsesRootOwner(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	owner, loser := filepath.Join(t.TempDir(), "src"), filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.MkdirAll(filepath.Join(owner, ".git"), 0o755))
	require.NoError(t, os.Mkdir(loser, 0o755))
	t.Chdir(owner)
	gc, err := config.LoadGlobal()
	require.NoError(t, err)
	gc.Repos = []config.RepoEntry{
		{Path: loser, Name: "src", Exclude: []string{"loser-only"}},
		{Path: owner, Name: "src", Exclude: []string{"owner-only"}},
	}
	require.NoError(t, gc.Save())
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	out := captureStdout(t, func() { require.NoError(t, runConfigExcludeList(cmd, nil)) })
	assert.Contains(t, out, "owner-only")
	assert.NotContains(t, out, "loser-only")
}
