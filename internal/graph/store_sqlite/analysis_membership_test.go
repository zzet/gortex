package store_sqlite

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestAnalysisMembershipsContextPreservesRepeatedProcessSteps(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	revision := store.AnalysisMutationRevision()
	id, accepted, err := store.BeginAnalysisGeneration(revision, graph.AnalysisGenerationHeader{
		FormatVersion: 77, NodeCount: 1, CommunityCount: 1, ProcessCount: 1,
	})
	requireAnalysisAccepted(t)(accepted, err)
	requireAnalysisAccepted(t)(store.AppendAnalysisCommunities(revision, id, []graph.AnalysisCommunitySummary{{ID: "c1", Size: 1}}))
	requireAnalysisAccepted(t)(store.AppendAnalysisNodes(revision, id, []graph.AnalysisNodeMetric{{NodeID: "n1", CommunityID: "c1"}}))
	requireAnalysisAccepted(t)(store.AppendAnalysisProcesses(revision, id, []graph.AnalysisProcessSummary{{ID: "p1", EntryPoint: "n1", StepCount: 2}}, []graph.AnalysisProcessStep{
		{ProcessID: "p1", NodeID: "n1", Ordinal: 0},
		{ProcessID: "p1", NodeID: "n1", Ordinal: 1, Depth: 1},
	}))
	requireAnalysisAccepted(t)(store.PutAnalysisBlob(revision, id, graph.AnalysisBlob{Component: graph.AnalysisBlobAdjacency, Payload: []byte("adj")}))
	requireAnalysisAccepted(t)(store.PutAnalysisBlob(revision, id, graph.AnalysisBlob{Component: graph.AnalysisBlobLeiden, Payload: []byte("leiden")}))
	for component, count := range map[graph.AnalysisComponent]int{
		graph.AnalysisComponentNodes: 1, graph.AnalysisComponentCommunities: 1,
		graph.AnalysisComponentProcesses: 1, graph.AnalysisComponentConcepts: 0,
		graph.AnalysisComponentAdjacency: 1, graph.AnalysisComponentLeiden: 1,
	} {
		requireAnalysisAccepted(t)(store.SealAnalysisComponent(revision, id, component, count))
	}
	requireAnalysisAccepted(t)(store.ActivateAnalysisGeneration(revision, id))
	rows, err := store.AnalysisMembershipsContext(t.Context(), id, []string{"n1", "missing"})
	require.NoError(t, err)
	require.Equal(t, []graph.AnalysisMembership{
		{NodeID: "n1", CommunityID: "c1", ProcessID: "p1"},
		{NodeID: "n1", CommunityID: "c1", ProcessID: "p1"},
	}, rows)
	rows, err = store.AnalysisMembershipsContext(t.Context(), id, nil)
	require.NoError(t, err)
	require.Empty(t, rows)
	_, err = store.AnalysisMembershipsContext(t.Context(), id, []string{"n1", "n1"})
	require.Error(t, err, "duplicate ID rejection must stay visible")
	_, err = store.AnalysisMembershipsContext(t.Context(), id, []string{""})
	require.Error(t, err, "empty ID rejection must stay visible")
}

func TestAnalysisMembershipsContextCancellationAndInactiveGeneration(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	id := buildMinimalAnalysisGeneration(t, store, "fixture", 0, true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rows, err := store.AnalysisMembershipsContext(ctx, id, []string{"fixture-node"})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
	_, err = store.AnalysisMembershipsContext(t.Context(), id+1, []string{"fixture-node"})
	require.ErrorIs(t, err, graph.ErrAnalysisGenerationInactive)
	rows, err = store.AnalysisMembershipsContext(t.Context(), id, []string{"fixture-node"})
	require.NoError(t, err)
	require.Equal(t, []graph.AnalysisMembership{{NodeID: "fixture-node", CommunityID: "fixture-community"}}, rows)
}
