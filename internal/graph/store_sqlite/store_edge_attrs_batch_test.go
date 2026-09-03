package store_sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestPersistEdgeAttributesBatchUsesSetOrientedChunks(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	const fromID = "repo/caller.go::Caller"
	nodes := []*graph.Node{{
		ID: fromID, Kind: graph.KindFunction, Name: "Caller",
		FilePath: "repo/caller.go", RepoPrefix: "repo",
	}}
	edges := make([]*graph.Edge, 0, edgeAttributeUpdateChunkSize+1)
	for i := 0; i < edgeAttributeUpdateChunkSize+1; i++ {
		toID := fmt.Sprintf("repo/target-%03d.go::Target", i)
		nodes = append(nodes, &graph.Node{
			ID: toID, Kind: graph.KindFunction, Name: "Target",
			FilePath: fmt.Sprintf("repo/target-%03d.go", i), RepoPrefix: "repo",
		})
		edges = append(edges, &graph.Edge{
			From: fromID, To: toID, Kind: graph.EdgeCalls,
			FilePath: "repo/caller.go", Line: i + 1,
		})
	}
	store.AddBatch(nodes, edges)

	updates := make([]*graph.Edge, 0, len(edges)+3)
	for _, edge := range edges {
		updates = append(updates, &graph.Edge{
			From: edge.From, To: edge.To, Kind: edge.Kind,
			FilePath: edge.FilePath, Line: edge.Line,
			Confidence: 0.25, ConfidenceLabel: "confirmed",
			Origin: "go-types", Tier: "semantic",
			Meta: map[string]any{
				"resolve_terminal":        true,
				"resolve_terminal_reason": "confirmed",
				"marker":                  "batch",
			},
		})
	}

	query, args, err := edgeAttributeUpdateStatement(store.viewGen, updates[:edgeAttributeUpdateChunkSize])
	require.NoError(t, err)
	assert.Len(t, args, edgeAttributeUpdateChunkSize*edgeAttributeUpdateParamsPerRow+1, "the chunk plus its trailing generation binding must stay below SQLite's conservative 999-variable bound")
	assert.Equal(t, edgeAttributeUpdateChunkSize, strings.Count(query, "(?,?,?,?,?,?,?,?,?,?,?,?,?)"), "each logical edge must contribute exactly one VALUES row")

	updates = append(updates, nil)
	updates = append(updates, &graph.Edge{
		From: fromID, To: "repo/missing.go::Missing", Kind: graph.EdgeCalls,
		FilePath: "repo/caller.go", Line: 999, Origin: "missing",
	})
	winner := *updates[0]
	winner.Confidence = 0.99
	winner.Origin = "last-write"
	winner.Meta = map[string]any{
		"resolve_terminal":        false,
		"resolve_terminal_reason": "winner",
		"marker":                  "winner",
	}
	updates = append(updates, &winner)

	statements, err := store.persistEdgeAttributesBatch(updates)
	require.NoError(t, err)
	assert.Equal(t, 2, statements, "84 inputs must use two set-oriented UPDATE statements, not one UPDATE per edge")

	persisted := store.GetOutEdges(fromID)
	require.Len(t, persisted, len(edges))
	byLine := make(map[int]*graph.Edge, len(persisted))
	for _, edge := range persisted {
		byLine[edge.Line] = edge
	}
	for i := 0; i < len(edges); i++ {
		edge := byLine[i+1]
		require.NotNil(t, edge)
		assert.Equal(t, "confirmed", edge.ConfidenceLabel)
		assert.Equal(t, "semantic", edge.Tier)
		if i == 0 {
			assert.InDelta(t, 0.99, edge.Confidence, 0.0001)
			assert.Equal(t, "last-write", edge.Origin)
			assert.Equal(t, false, edge.Meta["resolve_terminal"])
			assert.Equal(t, "winner", edge.Meta["resolve_terminal_reason"])
			assert.Equal(t, "winner", edge.Meta["marker"])
			continue
		}
		assert.InDelta(t, 0.25, edge.Confidence, 0.0001)
		assert.Equal(t, "go-types", edge.Origin)
		assert.Equal(t, true, edge.Meta["resolve_terminal"])
		assert.Equal(t, "confirmed", edge.Meta["resolve_terminal_reason"])
		assert.Equal(t, "batch", edge.Meta["marker"])
	}

	idempotent := append([]*graph.Edge(nil), updates[:len(edges)]...)
	idempotent[0] = &winner // retain the last-write winner without replaying an earlier duplicate
	buildMinimalAnalysisGeneration(t, store, "edge-attrs-noop", 0, true)
	beforeRevision := store.AnalysisMutationRevision()
	statements, err = store.persistEdgeAttributesBatch(idempotent)
	require.NoError(t, err)
	assert.Equal(t, 2, statements)
	assert.Equal(t, beforeRevision, store.AnalysisMutationRevision(), "idempotent attributes must not advance the graph revision")
	_, found, err := store.LoadActiveAnalysisHeader(77)
	require.NoError(t, err)
	assert.True(t, found, "idempotent attributes must preserve active warm analysis")

	changed := *updates[1]
	changed.Origin = "changed"
	_, err = store.persistEdgeAttributesBatch([]*graph.Edge{&changed})
	require.NoError(t, err)
	assert.Greater(t, store.AnalysisMutationRevision(), beforeRevision)
	_, found, err = store.LoadActiveAnalysisHeader(77)
	require.NoError(t, err)
	assert.False(t, found, "a real attribute change must invalidate active analysis atomically")
}
