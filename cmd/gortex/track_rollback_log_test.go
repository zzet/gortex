package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestRemoveAddedRepoEntryRollbackLogging(t *testing.T) {
	priorLevel := logLevel
	logLevel = "info"
	t.Cleanup(func() { logLevel = priorLevel })
	removalEvents := func(t *testing.T, raw string) []map[string]any {
		t.Helper()
		var events []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
			var entry map[string]any
			if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == "config: removed tracked repo" {
				require.Equal(t, 1, strings.Count(line, `"caller":`), "the caller field must not be duplicated")
				events = append(events, entry)
			}
		}
		return events
	}
	fixture := func(t *testing.T) (string, string, map[string]config.ProjectConfig) {
		t.Helper()
		keep, candidate := offlineUntrackFixture(t)
		gc, err := config.LoadGlobal()
		require.NoError(t, err)
		projects := map[string]config.ProjectConfig{
			"preserved": {Repos: []config.RepoEntry{{Path: keep, Name: "project-keep"}, {Path: candidate, Name: "project-candidate"}}},
		}
		gc.Projects = projects
		require.NoError(t, gc.Save())
		return keep, candidate, projects
	}

	t.Run("persisted_removal_and_repeat", func(t *testing.T) {
		keep, candidate, projects := fixture(t)
		var removedErr error
		stderr, _ := captureLegacyStderr(t, func() { removedErr = removeAddedRepoEntry(candidate) })
		require.NoError(t, removedErr)
		events := removalEvents(t, stderr)
		require.Len(t, events, 1, "successful durable rollback must emit one removal event: %s", stderr)
		require.Equal(t, "info", events[0]["level"])
		require.Equal(t, "track_rejected", events[0]["cause"])
		require.Equal(t, "removeAddedRepoEntry", events[0]["caller"])
		require.Equal(t, candidate, events[0]["root"])
		gc, err := config.LoadGlobal()
		require.NoError(t, err)
		require.Len(t, gc.Repos, 1)
		require.True(t, pathkey.EqualPaths(keep, gc.Repos[0].Path))
		require.Equal(t, projects, gc.Projects, "rollback must retain accepted project memberships")
		afterRemoval, err := os.ReadFile(config.DefaultGlobalConfigPath())
		require.NoError(t, err)
		stderr, _ = captureLegacyStderr(t, func() { removedErr = removeAddedRepoEntry(candidate) })
		require.NoError(t, removedErr)
		require.Empty(t, removalEvents(t, stderr), "a repeat that removed nothing must not log a removal")
		require.NotContains(t, stderr, "config: removed tracked repo")
		afterRepeat, err := os.ReadFile(config.DefaultGlobalConfigPath())
		require.NoError(t, err)
		require.Equal(t, afterRemoval, afterRepeat)
	})

	t.Run("absent_entry_does_not_save_or_log", func(t *testing.T) {
		fixture(t)
		before, err := os.ReadFile(config.DefaultGlobalConfigPath())
		require.NoError(t, err)
		priorSave := trackRollbackSaveFn
		t.Cleanup(func() { trackRollbackSaveFn = priorSave })
		saveCalls := 0
		trackRollbackSaveFn = func(*config.GlobalConfig) error {
			saveCalls++
			return os.ErrPermission
		}
		var removedErr error
		stderr, _ := captureLegacyStderr(t, func() { removedErr = removeAddedRepoEntry(t.TempDir()) })
		require.NoError(t, removedErr)
		require.Zero(t, saveCalls, "an absent entry must not attempt a config write")
		require.Empty(t, removalEvents(t, stderr))
		require.NotContains(t, stderr, "config: removed tracked repo")
		after, err := os.ReadFile(config.DefaultGlobalConfigPath())
		require.NoError(t, err)
		require.Equal(t, before, after)
	})

	t.Run("failed_persistence_does_not_log_or_change_config", func(t *testing.T) {
		keep, candidate, projects := fixture(t)
		before, err := os.ReadFile(config.DefaultGlobalConfigPath())
		require.NoError(t, err)
		blockedDestination := t.TempDir()
		priorSave := trackRollbackSaveFn
		t.Cleanup(func() { trackRollbackSaveFn = priorSave })
		saveCalls := 0
		trackRollbackSaveFn = func(gc *config.GlobalConfig) error {
			saveCalls++
			// The real atomic writer cannot replace a directory with a file.
			// This gives every platform a genuine persistence failure.
			gc.SetConfigPath(blockedDestination)
			return priorSave(gc)
		}
		var removedErr error
		stderr, _ := captureLegacyStderr(t, func() { removedErr = removeAddedRepoEntry(candidate) })
		require.ErrorContains(t, removedErr, "writing global config")
		require.Equal(t, 1, saveCalls)
		require.Empty(t, removalEvents(t, stderr), "failed persistence must not report a completed removal")
		require.NotContains(t, stderr, "config: removed tracked repo")
		after, err := os.ReadFile(config.DefaultGlobalConfigPath())
		require.NoError(t, err)
		require.Equal(t, before, after)
		gc, err := config.LoadGlobal()
		require.NoError(t, err)
		require.Len(t, gc.Repos, 2)
		require.True(t, pathkey.EqualPaths(keep, gc.Repos[0].Path))
		require.True(t, pathkey.EqualPaths(candidate, gc.Repos[1].Path))
		require.Equal(t, projects, gc.Projects)
		require.DirExists(t, blockedDestination)
	})
}
