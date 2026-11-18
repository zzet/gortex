package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestRunTrackPrefixCollision(t *testing.T) {
	for _, wait := range []bool{false, true} {
		for _, preexisting := range []bool{false, true} {
			t.Run(fmt.Sprintf("wait=%v/preexisting=%v", wait, preexisting), func(t *testing.T) {
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
				first, second := pathkey.CanonicalExistingRoot(t.TempDir()), pathkey.CanonicalExistingRoot(t.TempDir())
				gc, err := config.LoadGlobal()
				require.NoError(t, err)
				require.NoError(t, gc.AddRepo(config.RepoEntry{Path: first, Name: "src"}))
				if preexisting {
					require.NoError(t, gc.AddRepo(config.RepoEntry{Path: second, Name: "src"}))
				}
				require.NoError(t, gc.Save())
				origWait, origName, origAsWorktree := trackWait, trackName, trackAsWorktree
				origEnsure, origNotify := trackEnsureDaemonReadyFn, trackNotifyDaemonTrackFn
				trackWait, trackName, trackAsWorktree = wait, "src", false
				trackEnsureDaemonReadyFn = func(bool) daemonDecision { return daemonReady }
				trackNotifyDaemonTrackFn = func(string, string, time.Time) error {
					return &trackPrefixCollisionError{message: (&indexer.PrefixCollisionError{
						Prefix: "src", ExistingPath: first, Path: second,
					}).Error()}
				}
				t.Cleanup(func() {
					trackWait, trackName, trackAsWorktree = origWait, origName, origAsWorktree
					trackEnsureDaemonReadyFn, trackNotifyDaemonTrackFn = origEnsure, origNotify
				})
				cmd := &cobra.Command{}
				output := &bytes.Buffer{}
				cmd.SetErr(output)
				err = runTrack(cmd, []string{second})
				require.Error(t, err, "a rejected track must return non-zero even without --wait")
				assert.Contains(t, err.Error(), first)
				assert.Contains(t, err.Error(), "gortex track --name <other>")
				assert.NotContains(t, output.String(), "Tracked repository")
				gc, err = config.LoadGlobal()
				require.NoError(t, err)
				want := 1
				if preexisting {
					want = 2
				}
				assert.Len(t, gc.Repos, want, "only a newly-added config entry may be removed")
				assert.Equal(t, first, gc.Repos[0].Path)
			})
		}
	}
}

func TestRunTrackIdentityPendingRemovesOnlyItsNewEntry(t *testing.T) {
	for _, wait := range []bool{false, true} {
		for _, preexisting := range []bool{false, true} {
			t.Run(fmt.Sprintf("wait=%v/preexisting=%v", wait, preexisting), func(t *testing.T) {
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
				root := pathkey.CanonicalExistingRoot(t.TempDir())
				gc, err := config.LoadGlobal()
				require.NoError(t, err)
				if preexisting {
					require.NoError(t, gc.AddRepo(config.RepoEntry{Path: root, Name: "chosen"}))
				}
				require.NoError(t, gc.Save())
				oldWait, oldName, oldAsWorktree := trackWait, trackName, trackAsWorktree
				oldEnsure, oldNotify := trackEnsureDaemonReadyFn, trackNotifyDaemonTrackFn
				t.Cleanup(func() {
					trackWait, trackName, trackAsWorktree = oldWait, oldName, oldAsWorktree
					trackEnsureDaemonReadyFn, trackNotifyDaemonTrackFn = oldEnsure, oldNotify
				})
				trackWait, trackName, trackAsWorktree = wait, "chosen", false
				trackEnsureDaemonReadyFn = func(bool) daemonDecision { return daemonReady }
				trackNotifyDaemonTrackFn = func(string, string, time.Time) error {
					return &repoLoadingError{admissionReason: "identity_pending", phase: "identity_resolution"}
				}
				cmd := &cobra.Command{}
				cmd.SetErr(&bytes.Buffer{})
				err = runTrack(cmd, []string{root})
				var pending *repoLoadingError
				require.ErrorAs(t, err, &pending)
				require.Equal(t, exitTempFail, pending.ExitCode())
				gc, err = config.LoadGlobal()
				require.NoError(t, err)
				if preexisting {
					require.Len(t, gc.Repos, 1)
				} else {
					require.Empty(t, gc.Repos, "uncertainty cannot persist an unproved new physical-root entry")
				}
			})
		}
	}
}

// The rollback removes only the entry this run added, never another
// spelling of the same directory that was already in config.
func TestRunTrackPrefixCollisionKeepsAliasEntry(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	first, second := pathkey.CanonicalExistingRoot(t.TempDir()), pathkey.CanonicalExistingRoot(t.TempDir())
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(second, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	gc, err := config.LoadGlobal()
	require.NoError(t, err)
	require.NoError(t, gc.AddRepo(config.RepoEntry{Path: first, Name: "src"}))
	require.NoError(t, gc.AddRepo(config.RepoEntry{Path: alias, Name: "kept"}))
	require.NoError(t, gc.Save())
	origWait, origName, origAsWorktree := trackWait, trackName, trackAsWorktree
	origEnsure, origNotify := trackEnsureDaemonReadyFn, trackNotifyDaemonTrackFn
	trackWait, trackName, trackAsWorktree = false, "src", false
	trackEnsureDaemonReadyFn = func(bool) daemonDecision { return daemonReady }
	trackNotifyDaemonTrackFn = func(string, string, time.Time) error {
		return &trackPrefixCollisionError{message: "prefix collision"}
	}
	t.Cleanup(func() {
		trackWait, trackName, trackAsWorktree = origWait, origName, origAsWorktree
		trackEnsureDaemonReadyFn, trackNotifyDaemonTrackFn = origEnsure, origNotify
	})
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	require.Error(t, runTrack(cmd, []string{second}))
	gc, err = config.LoadGlobal()
	require.NoError(t, err)
	var paths []string
	for _, entry := range gc.Repos {
		paths = append(paths, entry.Path)
	}
	assert.Equal(t, []string{first, alias}, paths)
}

func TestNotifyDaemonTrackPrefixCollision(t *testing.T) {
	_, _, first, _ := spinUpDaemonWithConfig(t)
	second := filepath.Join(t.TempDir(), filepath.Base(first))
	require.NoError(t, os.MkdirAll(second, 0o755))
	origName := trackName
	trackName = ""
	t.Cleanup(func() { trackName = origName })
	err := notifyDaemonTrack(second, "", time.Time{})
	var collision *trackPrefixCollisionError
	require.ErrorAs(t, err, &collision)
	assert.Equal(t, "track rejected: "+(&indexer.PrefixCollisionError{
		Prefix: filepath.Base(first), ExistingPath: first, Path: second,
	}).Error(), err.Error())
}

func TestRunTrackOfflineRejectsPrefixBeforeSave(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
			first := filepath.Join(t.TempDir(), "src")
			second := filepath.Join(t.TempDir(), "src")
			require.NoError(t, os.MkdirAll(first, 0o755))
			require.NoError(t, os.MkdirAll(second, 0o755))
			gc, err := config.LoadGlobal()
			require.NoError(t, err)
			name := ""
			if explicit {
				name = "chosen"
			}
			require.NoError(t, gc.AddRepo(config.RepoEntry{Path: first, Name: name}))
			require.NoError(t, gc.Save())
			before, err := os.ReadFile(gc.ConfigPath())
			require.NoError(t, err)
			origWait, origName, origAs := trackWait, trackName, trackAsWorktree
			origEnsure := trackEnsureDaemonReadyFn
			trackWait, trackName, trackAsWorktree = false, name, false
			contacted := false
			trackEnsureDaemonReadyFn = func(bool) daemonDecision { contacted = true; return daemonUnavailable }
			t.Cleanup(func() {
				trackWait, trackName, trackAsWorktree = origWait, origName, origAs
				trackEnsureDaemonReadyFn = origEnsure
			})
			cmd := &cobra.Command{}
			output := &bytes.Buffer{}
			cmd.SetErr(output)
			err = runTrack(cmd, []string{second})
			var collision *indexer.PrefixCollisionError
			require.ErrorAs(t, err, &collision)
			assert.False(t, contacted, "reject before daemon contact")
			after, err := os.ReadFile(gc.ConfigPath())
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.NotContains(t, output.String(), "config only")
		})
	}
}

func TestRunTrackOfflineUsesWorktreeInstancePrefix(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	canonical, worktree := filepath.Join(t.TempDir(), "src"), filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.MkdirAll(canonical, 0o755))
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", canonical}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	git("init", "-q", "-b", "main")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "init")
	git("worktree", "add", "-q", "-b", "review", worktree)
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".gortex.yaml"), []byte("workspace: review\n"), 0o644))
	gc, err := config.LoadGlobal()
	require.NoError(t, err)
	require.NoError(t, gc.AddRepo(config.RepoEntry{Path: canonical}))
	require.NoError(t, gc.Save())
	origWait, origName, origAs := trackWait, trackName, trackAsWorktree
	origEnsure := trackEnsureDaemonReadyFn
	trackWait, trackName, trackAsWorktree = false, "", true // This test explicitly requests an independent graph.
	trackEnsureDaemonReadyFn = func(bool) daemonDecision { return daemonUnavailable }
	t.Cleanup(func() {
		trackWait, trackName, trackAsWorktree = origWait, origName, origAs
		trackEnsureDaemonReadyFn = origEnsure
	})
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	require.NoError(t, runTrack(cmd, []string{worktree}))
	gc, err = config.LoadGlobal()
	require.NoError(t, err)
	require.Len(t, gc.Repos, 2)
	assert.Equal(t, "src@src", gc.Repos[1].Name, "use Git admin name, not branch or workspace")
	// An unpinned worktree entry also reserves its instance, not the basename.
	gc.Repos = []config.RepoEntry{{Path: worktree}}
	require.NoError(t, gc.Save())
	require.NoError(t, runTrack(cmd, []string{canonical}))
}
