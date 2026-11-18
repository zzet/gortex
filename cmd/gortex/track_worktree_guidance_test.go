package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/pathkey"
)

func resetTrackGuidanceSeams(t *testing.T) {
	t.Helper()
	oldName, oldAs, oldWait := trackName, trackAsWorktree, trackWait
	oldRunning, oldProbe, oldExplain := trackDaemonRunningFn, trackFamilyProbeFn, trackExplainViewFn
	oldEnsure, oldNotify := trackEnsureDaemonReadyFn, trackNotifyDaemonTrackFn
	t.Cleanup(func() {
		trackName, trackAsWorktree, trackWait = oldName, oldAs, oldWait
		trackDaemonRunningFn, trackFamilyProbeFn, trackExplainViewFn = oldRunning, oldProbe, oldExplain
		trackEnsureDaemonReadyFn, trackNotifyDaemonTrackFn = oldEnsure, oldNotify
	})
	trackName, trackAsWorktree, trackWait = "", false, false
	trackDaemonRunningFn = func() bool { return false }
	trackFamilyProbeFn = func() ([]string, error) { t.Fatal("unexpected read-only family probe"); return nil, nil }
	trackExplainViewFn = func(string) (json.RawMessage, error) { t.Fatal("unexpected explain-view"); return nil, nil }
	trackEnsureDaemonReadyFn = func(bool) daemonDecision {
		t.Fatal("refused worktree attempted daemon startup")
		return daemonUnavailable
	}
	trackNotifyDaemonTrackFn = func(string, string, time.Time) error { t.Fatal("refused worktree sent track RPC"); return nil }
}

func TestTrackLinkedWorktreeRequiresExplicitSeparateGraphOptIn(t *testing.T) {
	for _, online := range []bool{false, true} {
		t.Run(map[bool]string{false: "offline", true: "running"}[online], func(t *testing.T) {
			resetTrackGuidanceSeams(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
			primary, linked := guidanceGitFamily(t)
			primary, linked = pathkey.CanonicalExistingRoot(primary), pathkey.CanonicalExistingRoot(linked)
			gc, err := config.LoadGlobal()
			require.NoError(t, err)
			gc.Repos = []config.RepoEntry{{Path: primary}}
			require.NoError(t, gc.Save())
			before, err := os.ReadFile(gc.ConfigPath())
			require.NoError(t, err)
			var output bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetErr(&output)
			trackDaemonRunningFn = func() bool { return online }
			if online {
				trackExplainViewFn = func(path string) (json.RawMessage, error) {
					require.Equal(t, linked, path)
					require.Contains(t, output.String(), "needs no track", "warning must precede diagnostic RPC")
					return json.RawMessage(`{"checkout_state":"reconciling","effective_mode":"automatic","reason":"discovery_pending"}`), nil
				}
			}
			require.NoError(t, runTrack(cmd, []string{linked}))
			after, err := os.ReadFile(gc.ConfigPath())
			require.NoError(t, err)
			require.Equal(t, before, after, "refusal must preserve config bytes")
			require.Contains(t, output.String(), linked+" is a linked worktree of "+primary)
			require.Contains(t, output.String(), "Pass --as-worktree")
			if online {
				require.Contains(t, output.String(), "checkout_state=reconciling")
			}
			trackAsWorktree = true
			ensured, notified := 0, 0
			trackEnsureDaemonReadyFn = func(bool) daemonDecision { ensured++; return daemonReady }
			trackNotifyDaemonTrackFn = func(path, _ string, _ time.Time) error { notified++; require.Equal(t, linked, path); return nil }
			require.NoError(t, runTrack(cmd, []string{linked}))
			require.Equal(t, 1, ensured)
			require.Equal(t, 1, notified)
			updated, err := config.LoadGlobal()
			require.NoError(t, err)
			require.NotNil(t, updated.FindRepoByPath(linked))
		})
	}
}

func TestTrackRegisteredOnlyFamilyIsStillGuarded(t *testing.T) {
	resetTrackGuidanceSeams(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	primary, linked := guidanceGitFamily(t)
	trackDaemonRunningFn = func() bool { return true }
	trackFamilyProbeFn = func() ([]string, error) { return []string{primary}, nil }
	trackExplainViewFn = func(string) (json.RawMessage, error) {
		return json.RawMessage(`{"checkout_state":"checkout_ready"}`), nil
	}
	gc, err := config.LoadGlobal()
	require.NoError(t, err)
	var output bytes.Buffer
	require.True(t, refuseAutomaticWorktree(&output, pathkey.CanonicalExistingRoot(linked), gc))
	require.Contains(t, output.String(), "needs no track")
	require.Empty(t, gc.ReposSnapshot())
}

func TestFirstTrackedFamilyWorktreeKeepsChoiceButExplainsPrimary(t *testing.T) {
	resetTrackGuidanceSeams(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	_, linked := guidanceGitFamily(t)
	trackEnsureDaemonReadyFn = func(bool) daemonDecision { return daemonUnavailable }
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&output)
	require.NoError(t, runTrack(cmd, []string{linked}))
	gc, err := config.LoadGlobal()
	require.NoError(t, err)
	require.NotNil(t, gc.FindRepoByPath(linked))
	require.Contains(t, output.String(), "will become this Git family's tracked primary")
	require.Contains(t, output.String(), "default-branch checkout")
}
