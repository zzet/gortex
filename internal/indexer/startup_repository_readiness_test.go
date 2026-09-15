package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Seed must register the state written by the real lifecycle, both when the
// catalog is first populated and when persisted owners are restored at boot.
// A resumed coordinator needs that read admission even while warmup holds the
// build gate closed. All stores, config, and Git repositories are test-owned.
func TestCheckoutLifecycleSeedRestoresReadyOwnersAndCoordinators(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	main := f.gitRepo("startup-main")
	worktree := f.worktreeOf(main, "startup-worktree")
	entries := []config.RepoEntry{
		{Path: main, Name: "startup-main"},
		{Path: f.gitRepo("startup-other"), Name: "startup-other"},
	}
	for _, entry := range entries {
		require.NoError(t, f.cm.Global().AddRepo(entry))
	}
	require.NoError(t, f.cm.Global().Save())

	var automaticID string
	var owners []store_sqlite.DedicatedGraph
	for _, phase := range []string{"initial", "restart"} {
		runtime := &dedicatedBaseRuntime{store: f.store}
		require.NoError(t, f.lc.SetDedicatedBaseCleanupRuntime(runtime))
		gate := NewViewBuildGate()
		f.lc.SetBuildGate(gate)
		for _, entry := range entries {
			_, err := f.mi.TrackRepoCtx(ctx, entry)
			require.NoError(t, err)
		}
		require.NoError(t, f.lc.Seed(ctx), phase)
		for i, entry := range entries {
			graph := f.familyOf(entry.Name)
			require.Equal(t, "graph_ready", graph.State, phase)
			checkout := f.checkoutOf(entry.Name)
			read, err := f.lc.AcquireRepositoryRead(graph.GraphID)
			require.NoError(t, err, phase)
			read.Release()
			owner := store_sqlite.DedicatedBaseOwner{CheckoutID: checkout.CheckoutID, Incarnation: checkout.Incarnation}
			// Check Seed's publisher registration before installation can create
			// a slot, then exercise the SQL owner guard with the same catalog row.
			runtime.mu.Lock()
			registered := runtime.ownerAdmissions[graph.GraphID]
			runtime.mu.Unlock()
			require.NotNil(t, registered, phase)
			_, err = runtime.install(ctx, store_sqlite.AcquireDedicatedBaseAuthorityRequest{
				GraphID: graph.GraphID, Owner: owner, Token: "startup-authority",
			})
			require.NoError(t, err, phase)
			if phase == "initial" {
				owners = append(owners, graph)
			} else {
				require.Equal(t, owners[i], graph, "restart preserves the durable binding")
			}
		}
		if phase == "restart" {
			require.True(t, f.lc.coordinatorRegistered(automaticID), "Seed resumes the routed checkout before the build gate opens")
			require.Equal(t, 1, f.lc.LiveCoordinators(""))
			gate.Open()
			refresh, err := f.lc.RequestCheckoutRefresh(ctx, automaticID, worktree)
			require.NoError(t, err)
			require.NoError(t, awaitCheckoutRefresh(t, refresh).Err)
			route, found := f.routeOf(automaticID)
			require.True(t, found)
			mutation, err := f.lc.BeginCheckoutMutation(ctx, automaticID, worktree, route.RouteEpoch)
			require.NoError(t, err, "restored coordinator admits an exact checkout edit")
			mutation.Close()
			after, found := f.routeOf(automaticID)
			require.True(t, found)
			require.Equal(t, route, after, "mutation admission alone leaves the route intact")
			break
		}
		require.Zero(t, f.lc.LiveCoordinators(""), "an unselected checkout stays dormant")
		gate.Open()
		automaticID = f.automaticCheckoutID(owners[0].FamilyID, "startup-worktree")
		f.activateAndWait(automaticID)
		f.runCoordinator(automaticID)
		_, routed := f.routeOf(automaticID)
		require.True(t, routed)
		f.restart()
	}
}
