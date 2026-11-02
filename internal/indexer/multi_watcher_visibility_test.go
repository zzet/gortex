package indexer

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMultiWatcherReportsRepoThatNeverStarted is the health half of the macOS
// watcher outage. A per-repo watcher that fails to start was logged once and
// then forgotten: it was absent from `started`, had no DegradedReason of its
// own, and MultiWatcher.Start still returned nil — so the daemon announced
// every configured repo as watched and the graph aged behind a green status.
func TestMultiWatcherReportsRepoThatNeverStarted(t *testing.T) {
	mw, _, repoADir, _ := setupMultiWatcherTest(t)
	require.False(t, mw.TierAuthority("repo-a"), "configured is not yet watching")

	// Repo A's root disappears before the watchers come up — a checkout
	// removed under a running daemon.
	require.NoError(t, os.RemoveAll(repoADir))
	require.NoError(t, mw.Start())
	t.Cleanup(func() { _ = mw.Stop() })

	live, configured := mw.WatchedRepos()
	require.Equal(t, 2, configured)
	require.Equal(t, 1, live, "only repo-b can be watched once repo-a's root is gone")
	require.False(t, mw.TierAuthority("repo-a"), "a failed startup cannot certify an empty tier set")
	require.True(t, mw.TierAuthority("repo-b"))
	require.False(t, mw.TierAuthority("unknown-repo"))

	reason := mw.DegradedReason()
	require.NotEmpty(t, reason, "an unwatched repository must not read as healthy")
	require.Contains(t, reason, "repo-a", "the reason must name the repository that is not being watched")
}

// TestMultiWatcherHealthyWhenAllReposStart guards against the inverse: a fully
// started fleet must report no degradation and a live count equal to the
// configured one.
func TestMultiWatcherHealthyWhenAllReposStart(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)

	require.NoError(t, mw.Start())
	t.Cleanup(func() {
		_ = mw.Stop()
		require.False(t, mw.TierAuthority("repo-a"), "a stopped watcher cannot certify a tier")
	})

	live, configured := mw.WatchedRepos()
	require.Equal(t, configured, live)
	require.Equal(t, 2, live)
	require.Empty(t, mw.DegradedReason())
	require.True(t, mw.TierAuthority("repo-a"))
	require.True(t, mw.TierAuthority("repo-b"))
}
