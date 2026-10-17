package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type extractionCoreProjectionProbe struct {
	*graph.Graph
	fullCalls, selectedCalls int
	selectedError            error
}

func (p *extractionCoreProjectionProbe) GetOutEdgesByNodeIDsWithMetadataContext(ctx context.Context, ids []string, limit int) (map[string][]*graph.Edge, bool, error) {
	p.fullCalls++
	return p.Graph.GetOutEdgesByNodeIDsWithMetadataContext(ctx, ids, limit)
}
func (p *extractionCoreProjectionProbe) ContractExtractionEdgesContext(ctx context.Context, ids []string, limit int) (map[string][]*graph.Edge, bool, error) {
	p.selectedCalls++
	if p.selectedError != nil {
		return nil, false, p.selectedError
	}
	rows, truncated, err := p.Graph.GetOutEdgesByNodeIDsWithMetadataContext(ctx, ids, limit)
	out := make(map[string][]*graph.Edge)
	for id, edges := range rows {
		for _, edge := range edges {
			if edge.Kind == graph.EdgeCalls || edge.Kind == graph.EdgeProvides || edge.Kind == graph.EdgeConsumes {
				out[id] = append(out[id], edge)
			}
		}
	}
	return out, truncated, err
}

func TestContractExtractionUsesNarrowCoreAndLaterFullReadRemainsComplete(t *testing.T) {
	const path = "repo/dashboard.go"
	const id = path + "::render"
	g := graph.New()
	g.AddNode(&graph.Node{ID: path, Kind: graph.KindFile, FilePath: path, RepoPrefix: "repo"})
	g.AddNode(&graph.Node{ID: id, Name: "render", Kind: graph.KindFunction, FilePath: path, RepoPrefix: "repo", Meta: map[string]any{"signature": "func render()"}})
	ordinary := &graph.Edge{From: id, To: "repo/target.go::Other", Kind: graph.EdgeReferences, FilePath: path, Meta: map[string]any{"nested": map[string]any{"kept": true}}}
	spring := &graph.Edge{From: id, To: "repo/target.go::Bean", Kind: graph.EdgeCalls, FilePath: path, Confidence: .9, Meta: map[string]any{"via": "spring.Bean", "nested": map[string]any{"kept": true}}}
	g.AddEdge(ordinary)
	g.AddEdge(spring)
	probe := &extractionCoreProjectionProbe{Graph: g}
	e := extractionFileEvidence(t, nil, nil)
	e.core = probe
	e.readCore = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
		require.True(t, e.guard.recorded())
		return readContractFollowupCoreFile(ctx, probe, file)
	}
	nodes, seeds, err := e.loadExtractionFile(path)
	require.NoError(t, err)
	require.Len(t, nodes, 2)
	require.Equal(t, []*graph.Edge{spring}, seeds, "the complete consumed seed payload must match the full-read result")
	require.Len(t, e.ledger.reads, 1)
	require.Equal(t, contractReadExtractionFile, e.ledger.reads[0].kind)
	require.Zero(t, probe.fullCalls, "ordinary extraction must not request unrelated full outgoing metadata")
	require.Equal(t, 1, probe.selectedCalls)
	// A full enrichment read must not confuse the partial import with a
	// complete file, or overwrite mutations made to its private node rows.
	for _, node := range nodes {
		if node.ID == id {
			node.Meta["private_enrichment"] = "kept"
			e.AddNode(node)
		}
	}
	fullNodes, edges, err := e.loadFile(path)
	require.NoError(t, err)
	require.ElementsMatch(t, []*graph.Edge{ordinary, spring}, edges)
	require.Equal(t, 1, probe.fullCalls)
	var enriched bool
	for _, node := range fullNodes {
		if node.ID == id {
			enriched = node.Meta["private_enrichment"] == "kept"
		}
	}
	require.True(t, enriched, "extending edge evidence preserves private node enrichment")
	_, seeds, err = e.loadExtractionFile(path)
	require.NoError(t, err)
	require.Equal(t, []*graph.Edge{spring}, seeds)
	require.Equal(t, 1, probe.fullCalls)
	require.Equal(t, 1, probe.selectedCalls)
}

func TestContractExtractionNarrowCoreFailureDoesNotBecomeFullFallback(t *testing.T) {
	const path = "repo/dashboard.go"
	g := graph.New()
	g.AddNode(&graph.Node{ID: path, Kind: graph.KindFile, FilePath: path, RepoPrefix: "repo"})
	probe := &extractionCoreProjectionProbe{Graph: g, selectedError: graph.ErrContractProjectionStale}
	e := extractionFileEvidence(t, nil, nil)
	e.readCore = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
		return readContractFollowupCoreFile(ctx, probe, file)
	}
	nodes, edges, err := e.loadExtractionFile(path)
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	require.Nil(t, nodes)
	require.Nil(t, edges)
	require.Empty(t, e.ledger.reads)
	require.Zero(t, probe.fullCalls)
	require.Equal(t, 1, probe.selectedCalls)
}
