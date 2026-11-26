package mcp

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

// While a repository's mutation lane is held, index_health lists that lane as
// active and a pending receipt of a file in the repository names the holder;
// the other repository's lane is idle, and nothing stays active once the lane
// is released. The lanes ride on the live part of index_health, which both the
// tool and the resource render.
func TestRepositoryLaneStatusNamesTheHolder(t *testing.T) {
	srv, paths := newAnalyzeServer(t, false,
		analyzeRepoSpec{name: "alpha", workspace: "ws", body: structuralBody("a")},
		analyzeRepoSpec{name: "beta", workspace: "ws", body: structuralBody("b")})
	alpha := srv.multiIndexer.RepoForFile(filepath.Join(paths["alpha"], "main.go"))
	require.NotEmpty(t, alpha)
	release, ok := srv.multiIndexer.TryRepositoryMutationLanesFor(indexer.OutputEntryContractRepair, []string{alpha})
	require.True(t, ok)
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	lanes, _ := srv.refreshIndexHealthFileFailures(t.Context(), map[string]any{})["repository_lanes"].(map[string]any)
	require.NotNil(t, lanes, "index_health carries no repository lanes")
	active, _ := lanes["active"].([]indexer.RepositoryLaneStatus)
	require.Len(t, active, 1, "active lanes: %+v", active)
	require.Equal(t, alpha, active[0].Repo)
	require.NotNil(t, active[0].Holder)
	require.Equal(t, string(indexer.OutputEntryContractRepair), active[0].Holder.Kind)
	require.Nil(t, active[0].ByKind, "per-kind totals belong to daemon status")

	pending := map[string]any{}
	srv.attachPendingRepositoryLane(pending, "", filepath.Join(paths["alpha"], "main.go"))
	lane, ok := pending["repository_lane"].(indexer.RepositoryLaneStatus)
	require.True(t, ok, "a pending receipt in alpha carries no lane: %+v", pending)
	require.Equal(t, alpha, lane.Repo)
	require.NotNil(t, lane.Holder)
	require.Equal(t, string(indexer.OutputEntryContractRepair), lane.Holder.Kind)
	require.NotNil(t, lane.Topology, "a pending receipt's lane omits the topology gates")
	require.Nil(t, lane.ByKind, "a polled receipt carries the lane's history")

	pending = map[string]any{}
	srv.attachPendingRepositoryLane(pending, "", filepath.Join(paths["beta"], "main.go"))
	if lane, ok := pending["repository_lane"].(indexer.RepositoryLaneStatus); ok {
		require.Nil(t, lane.Holder, "the idle lane of beta reports a holder")
	}

	release()
	released = true
	lanes, _ = srv.refreshIndexHealthFileFailures(t.Context(), map[string]any{})["repository_lanes"].(map[string]any)
	active, _ = lanes["active"].([]indexer.RepositoryLaneStatus)
	require.Empty(t, active, "a released lane stays active")
}
