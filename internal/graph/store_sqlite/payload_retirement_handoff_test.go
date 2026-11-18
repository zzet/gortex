package store_sqlite

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestPayloadRetirementFenceLeavesNonemptyRowsWithoutWALWait(t *testing.T) {
	s, id, payload := emptyRetirementWALFixture(t)
	node := &graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/a.go", Meta: map[string]any{"nested": map[string]any{"kept": true}}}
	require.NoError(t, payload.AddBatchChecked([]*graph.Node{node}, nil))
	before := s.WALReclaimStats()
	require.NoError(t, s.FencePayloadGenerationRetirement(t.Context(), id, nil))
	after := s.WALReclaimStats()
	require.Equal(t, before.RetirementWaits, after.RetirementWaits, "a durable handoff performs no physical retirement WAL wait")
	require.Equal(t, before.RetirementWaitTimeouts, after.RetirementWaitTimeouts)
	row, found, err := s.Catalog().GetViewGeneration(t.Context(), id)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ViewGenerationRetiring, row.State)
	require.Equal(t, 1, countAtGeneration(t, s, "nodes", id), "fencing is not physical collection")
	require.ErrorIs(t, payload.AddBatchChecked([]*graph.Node{{ID: "repo/new.go::New", Kind: graph.KindFunction}}, nil), ErrPayloadGenerationSealed)
	require.Error(t, s.PublishPayloadGeneration(t.Context(), id, 1), "retiring work cannot be published")
	// The existing collector resumes this exact durable state, retaining its
	// ordinary WAL policy. This controlled fixture now permits reclamation.
	s.walReclaim.update(func(st *WALReclaimStats) { st.ThresholdBytes, st.CeilingBytes = 1<<30, 1<<30 })
	for attempt := 0; attempt < 16; attempt++ {
		err := s.RetirePayloadGeneration(t.Context(), id, nil)
		if errors.Is(err, ErrPayloadSweepBudgetExhausted) {
			continue
		}
		require.NoError(t, err)
		_, found, err = s.Catalog().GetViewGeneration(t.Context(), id)
		require.NoError(t, err)
		require.False(t, found)
		require.Zero(t, countAtGeneration(t, s, "nodes", id))
		return
	}
	t.Fatal("ordinary collection did not finish the fenced reservation")
}

func TestPayloadRetirementFencePreservesLeaseAndRouteRefusals(t *testing.T) {
	t.Run("lease", func(t *testing.T) {
		s, id, payload := emptyRetirementWALFixture(t)
		require.NoError(t, payload.AddBatchChecked([]*graph.Node{{ID: "repo/a.go::A", Kind: graph.KindFunction}}, nil))
		require.ErrorIs(t, s.FencePayloadGenerationRetirement(t.Context(), id, func(candidate int64) bool { return candidate == id }), ErrPayloadGenerationInUse)
		row, found, err := s.Catalog().GetViewGeneration(t.Context(), id)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, ViewGenerationBuilding, row.State)
		require.Equal(t, 1, countAtGeneration(t, s, "nodes", id))
	})
	t.Run("route", func(t *testing.T) {
		s := openPayloadStore(t)
		seedPayloadBase(t, s)
		seedPayloadControlPlane(t, s)
		id, payload, err := s.BeginPayloadGeneration(t.Context(), payloadRequest())
		require.NoError(t, err)
		writePayloadOverlay(t, payload)
		require.NoError(t, s.PublishAndRoute(t.Context(), id, payloadCheckoutID, 0, RouteSlotDirty))
		require.ErrorIs(t, s.FencePayloadGenerationRetirement(t.Context(), id, nil), ErrCatalogGenerationReferenced)
		row, found, err := s.Catalog().GetViewGeneration(t.Context(), id)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, ViewGenerationReady, row.State)
		require.Equal(t, 2, countAtGeneration(t, s, "nodes", id))
	})
}
