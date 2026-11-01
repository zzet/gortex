package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
)

func TestWarmupCollisionTypedSkip(t *testing.T) {
	for _, mode := range []string{"warm", "cold"} {
		t.Run(mode, func(t *testing.T) {
			base := startupPublicationEnv(t)
			owner := startupPublicationRepo(t, base, "a/src")
			rejected := startupPublicationRepo(t, base, "b/src")
			require.NoError(t, os.WriteFile(filepath.Join(owner, ".gortex.yaml"),
				[]byte("workspace: owner\nexclude:\n  - owner-only\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(rejected, ".gortex.yaml"),
				[]byte("workspace: rejected\nexclude:\n  - rejected-only\n"), 0o600))
			state, err := buildDaemonState(zap.NewNop())
			require.NoError(t, err)
			t.Cleanup(func() {
				state.basePublisher.Close()
				require.NoError(t, state.shared.Close())
			})
			require.NotNil(t, state.basePublisher, "a real publication queue is required to prove no scheduling")
			registered, err := state.lifecycle.Register(t.Context(), config.RepoEntry{Path: owner, Name: "src"}, indexer.TrackSourceCLI)
			require.NoError(t, err)
			require.NoError(t, registered.CatalogErr)
			ownerConfig, revision := state.configManager.GetRepoConfig("src"), state.configManager.Revision()
			nodes, edges := state.graph.GetRepoNodes("src"), state.graph.AllEdges()
			entry := config.RepoEntry{Path: rejected, Name: "src"}
			state.multiIndexer.ExpectRepos([]config.RepoEntry{entry})
			state.multiIndexer.StartRepoLoad(rejected)
			var prior map[string]int64
			if mode == "warm" {
				prior = map[string]int64{"main.go": 1}
			}
			var changed, files atomic.Int64
			var unknown atomic.Bool
			var prefixes sync.Map
			frontier := newWarmupDeltaFrontier()
			accounting := warmupRepoAccounting{changed: &changed, files: &files, unknown: &unknown, prefixes: &prefixes, frontier: frontier}
			var pathFn string
			var jobErr error
			require.NoError(t, state.multiIndexer.RunRepositoryTopologyBatch(t.Context(), func(ctx context.Context) error {
				pathFn, jobErr = accounting.load(ctx, state, entry, prior, zap.NewNop())
				return nil
			}))
			var skip *indexer.RepositoryCollisionSkip
			require.ErrorAs(t, jobErr, &skip)
			require.Equal(t, "src", skip.Collision.Prefix)
			require.Equal(t, owner, skip.Collision.ExistingPath)
			if mode == "warm" {
				require.Equal(t, "reconcile", pathFn)
			} else {
				require.Equal(t, "track", pathFn)
			}
			require.Zero(t, changed.Load())
			require.Zero(t, files.Load())
			require.False(t, unknown.Load())
			prefixes.Range(func(key, value any) bool {
				t.Errorf("collision recorded changed prefix %v", key)
				return true
			})
			frontier.mu.Lock()
			exact, plans, crossFiles := frontier.exact, len(frontier.plans), len(frontier.crossRepoFiles)
			frontier.mu.Unlock()
			require.True(t, exact, "a rejected root must not invalidate the existing delta frontier")
			require.Zero(t, plans)
			require.Zero(t, crossFiles)
			require.Zero(t, state.basePublisher.Pending())
			require.Empty(t, state.basePublisher.Outcomes())
			require.Equal(t, nodes, state.graph.GetRepoNodes("src"))
			require.Equal(t, edges, state.graph.AllEdges())
			require.Equal(t, ownerConfig, state.configManager.GetRepoConfig("src"))
			require.Equal(t, revision, state.configManager.Revision())
			state.multiIndexer.FinishRepoLoad(rejected, jobErr)
			load, found := state.multiIndexer.RepoLoadFor(rejected)
			require.True(t, found)
			require.Equal(t, indexer.RepoLoadFailed, load.State)
			require.Equal(t, "prefix_collision", load.ErrorCode)
			require.Equal(t, owner, load.OwnerPath)
			require.Equal(t, "present", load.OwnerState)
			// Positive control uses the same accounting and real queue. A
			// registered owner's warm no-op still schedules its own base.
			_, err = accounting.load(t.Context(), state, config.RepoEntry{Path: owner, Name: "src"}, map[string]int64{"main.go": 1}, zap.NewNop())
			require.NoError(t, err)
			require.Equal(t, 1, state.basePublisher.Pending())
			require.Zero(t, changed.Load())
		})
	}
}

func TestWarmupNonCollisionFailureStillInvalidates(t *testing.T) {
	base := startupPublicationEnv(t)
	state, err := buildDaemonState(zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() {
		state.basePublisher.Close()
		require.NoError(t, state.shared.Close())
	})
	var changed, files atomic.Int64
	var unknown atomic.Bool
	var prefixes sync.Map
	frontier := newWarmupDeltaFrontier()
	accounting := warmupRepoAccounting{changed: &changed, files: &files, unknown: &unknown, prefixes: &prefixes, frontier: frontier}
	_, err = accounting.load(t.Context(), state, config.RepoEntry{Path: filepath.Join(base, "missing"), Name: "missing"}, map[string]int64{"old.go": 1}, zap.NewNop())
	require.Error(t, err)
	var skip *indexer.RepositoryCollisionSkip
	require.NotErrorAs(t, err, &skip)
	require.Equal(t, int64(1), changed.Load())
	require.True(t, unknown.Load())
	frontier.mu.Lock()
	exact := frontier.exact
	frontier.mu.Unlock()
	require.False(t, exact, "unrelated failures must retain conservative invalidation")
}
