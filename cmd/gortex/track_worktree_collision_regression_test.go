package main

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestRunTrackSameWorkspaceWorktrees(t *testing.T) {
	for _, mode := range []string{"offline", "online", "online-config-only-owner"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
			online := mode != "offline"
			if online {
				spinUpDaemonWithConfig(t)
			}
			canonical := filepath.Join(t.TempDir(), "canonical")
			require.NoError(t, os.MkdirAll(canonical, 0o755))
			git := func(args ...string) {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-C", canonical}, args...)...)
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, "%s", out)
			}
			git("init", "-q", "-b", "main")
			git("-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "init")
			worktrees := []string{filepath.Join(t.TempDir(), "src"), filepath.Join(t.TempDir(), "src")}
			for i, path := range worktrees {
				git("worktree", "add", "-q", "-b", fmt.Sprintf("task-%d", i), path)
				require.NoError(t, os.WriteFile(filepath.Join(path, ".gortex.yaml"), []byte("workspace: team\n"), 0o644))
				worktrees[i] = pathkey.CanonicalExistingRoot(path)
			}
			origWait, origName, origAs := trackWait, trackName, trackAsWorktree
			origEnsure := trackEnsureDaemonReadyFn
			trackWait, trackName, trackAsWorktree = false, "", false
			trackEnsureDaemonReadyFn = func(bool) daemonDecision {
				if online {
					return daemonReady
				}
				return daemonUnavailable
			}
			t.Cleanup(func() {
				trackWait, trackName, trackAsWorktree = origWait, origName, origAs
				trackEnsureDaemonReadyFn = origEnsure
			})
			cmd := &cobra.Command{}
			cmd.SetErr(&bytes.Buffer{})
			if mode == "online-config-only-owner" {
				gc, err := config.LoadGlobal()
				require.NoError(t, err)
				gc.Repos = []config.RepoEntry{{Path: worktrees[0], Name: "src@team"}}
				require.NoError(t, gc.Save())
			} else {
				require.NoError(t, runTrack(cmd, worktrees[:1]))
			}
			require.NoError(t, runTrack(cmd, worktrees[1:]))
			gc, err := config.LoadGlobal()
			require.NoError(t, err)
			hash := fnv.New32a()
			_, err = hash.Write([]byte(filepath.Clean(worktrees[1])))
			require.NoError(t, err)
			want := fmt.Sprintf("src@team-%08x", hash.Sum32())
			require.Len(t, gc.Repos, 2)
			assert.Equal(t, "src@team", gc.Repos[0].Name)
			assert.Equal(t, want, gc.Repos[1].Name)
			if online {
				status, err := fetchDaemonStatusForCLI()
				require.NoError(t, err)
				found := false
				for _, repo := range status.TrackedRepos {
					if pathkey.SamePathIdentity(repo.Path, worktrees[1]) {
						assert.Equal(t, want, repo.Prefix, "config and daemon must agree")
						found = true
					}
				}
				assert.True(t, found, "daemon must register the second worktree")
			}
		})
	}
}

func TestRunTrackPlainLinkedWorktreeSameBasename(t *testing.T) {
	for _, online := range []bool{false, true} {
		t.Run(fmt.Sprint(online), func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
			main := filepath.Join(t.TempDir(), "src")
			linked := filepath.Join(t.TempDir(), "src")
			require.NoError(t, os.MkdirAll(main, 0o755))
			git := func(args ...string) {
				t.Helper()
				out, err := exec.Command("git", append([]string{"-C", main}, args...)...).CombinedOutput()
				require.NoError(t, err, "%s", out)
			}
			git("init", "-q", "-b", "main")
			git("-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "init")
			git("worktree", "add", "-q", "-b", "linked", linked)
			main, linked = pathkey.CanonicalExistingRoot(main), pathkey.CanonicalExistingRoot(linked)
			gc, err := config.LoadGlobal()
			require.NoError(t, err)
			gc.Repos = []config.RepoEntry{{Path: main}}
			require.NoError(t, gc.Save())
			origWait, origName, origAs := trackWait, trackName, trackAsWorktree
			origEnsure, origNotify := trackEnsureDaemonReadyFn, trackNotifyDaemonTrackFn
			t.Cleanup(func() {
				trackWait, trackName, trackAsWorktree = origWait, origName, origAs
				trackEnsureDaemonReadyFn, trackNotifyDaemonTrackFn = origEnsure, origNotify
			})
			trackWait, trackName, trackAsWorktree = false, "", false
			trackEnsureDaemonReadyFn = func(bool) daemonDecision {
				if online {
					return daemonReady
				}
				return daemonUnavailable
			}
			notified := false
			trackNotifyDaemonTrackFn = func(path, name string, _ time.Time) error {
				notified = true
				assert.Equal(t, linked, path)
				assert.Equal(t, "src@src", name)
				return nil
			}
			cmd := &cobra.Command{}
			cmd.SetErr(&bytes.Buffer{})
			require.NoError(t, runTrack(cmd, []string{linked}))
			gc, err = config.LoadGlobal()
			require.NoError(t, err)
			require.Len(t, gc.Repos, 2)
			assert.Equal(t, "src@src", gc.Repos[1].Name)
			assert.Equal(t, online, notified)
		})
	}
}
