package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
)

// The private in-process control server runs the real ownership lifecycle.
// No child daemon, timing measurement, or application store is involved.
func TestRunTrackConfiguredCollisionReachesDaemonAuthority(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	owner, loser := filepath.Join(t.TempDir(), "src"), filepath.Join(t.TempDir(), "src")
	for _, root := range []string{owner, loser} {
		require.NoError(t, os.MkdirAll(root, 0o755))
		wtGit(t, root, "init", "-q", "-b", "main")
		require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package sample\nfunc Alpha() {}\n"), 0o644))
		wtGit(t, root, "add", ".")
		wtGit(t, root, "commit", "-q", "-m", "isolated ownership fixture")
	}
	core, logs := observer.New(zap.WarnLevel)
	cm, mi := startCatalogTrackDaemonWithLogger(t, []config.RepoEntry{{Path: owner, Name: "src"}}, []config.RepoEntry{{Path: owner, Name: "src"}}, zap.New(core))
	before, err := os.ReadFile(cm.Global().ConfigPath())
	require.NoError(t, err)
	ownerMetadata := mi.GetMetadata("src")
	ownerConfig := cm.GetRepoConfig("src")
	oldWait, oldName, oldAsWorktree := trackWait, trackName, trackAsWorktree
	t.Cleanup(func() { trackWait, trackName, trackAsWorktree = oldWait, oldName, oldAsWorktree })
	trackName, trackAsWorktree = "", false
	for _, wait := range []bool{false, true, false} {
		trackWait = wait
		cmd := &cobra.Command{}
		cmd.SetErr(&bytes.Buffer{})
		err := runTrack(cmd, []string{loser})
		require.ErrorContains(t, err, owner)
		var authoritative *trackPrefixCollisionError
		require.ErrorAs(t, err, &authoritative)
		after, err := os.ReadFile(cm.Global().ConfigPath())
		require.NoError(t, err)
		require.Equal(t, before, after, "collision must never save the loser")
		require.Same(t, ownerMetadata, mi.GetMetadata("src"))
		require.Equal(t, ownerConfig, cm.GetRepoConfig("src"))
		warnings := logs.FilterMessage("repository not loaded").All()
		require.Len(t, warnings, 1, "CLI refusal must reach the daemon ownership decision; retries must deduplicate")
		require.Contains(t, warnings[0].ContextMap()["error"], owner)
		require.Contains(t, warnings[0].ContextMap()["error"], loser)
		require.Contains(t, warnings[0].ContextMap()["error"], "--name")
	}
	trackWait = false
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	require.NoError(t, runTrack(cmd, []string{owner}))
	require.Len(t, logs.FilterMessage("repository not loaded").All(), 1, "same-root no-op adds no collision warning")
}

func TestRunTrackLocalCollisionCannotRegisterWhenDaemonHasNoOwner(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	owner, loser := filepath.Join(t.TempDir(), "src"), filepath.Join(t.TempDir(), "src")
	for _, root := range []string{owner, loser} {
		require.NoError(t, os.MkdirAll(root, 0o755))
	}
	cm, mi := startCatalogTrackDaemon(t, []config.RepoEntry{{Path: owner, Name: "src"}}, nil)
	before, err := os.ReadFile(cm.Global().ConfigPath())
	require.NoError(t, err)
	oldWait, oldName, oldAsWorktree := trackWait, trackName, trackAsWorktree
	t.Cleanup(func() { trackWait, trackName, trackAsWorktree = oldWait, oldName, oldAsWorktree })
	trackWait, trackName, trackAsWorktree = false, "", false
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	require.ErrorContains(t, runTrack(cmd, []string{loser}), owner)
	after, err := os.ReadFile(cm.Global().ConfigPath())
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Nil(t, mi.GetMetadata("src"), "read-only preflight must never register the loser when daemon ownership is unresolved")
}
