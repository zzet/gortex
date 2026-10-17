package graphview

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestContractFileProjectionEdgeOnlyGenerationUsesInheritedSourceAndTarget(t *testing.T) {
	store := openTestStore(t)
	source := &graph.Node{ID: "/repo/source.go::handler", Kind: graph.KindFunction, FilePath: "/repo/source.go", RepoPrefix: "repo"}
	canonical := &graph.Node{ID: "contract", Kind: graph.KindContract, FilePath: "/repo/canonical.go", Meta: map[string]any{"contract_meta": "base"}}
	store.AddBatch([]*graph.Node{source, canonical}, nil)
	_, handle := beginTestGeneration(t, store, "contract-edge-only")
	owner := &graph.Edge{From: source.ID, To: canonical.ID, Kind: graph.EdgeProvides, FilePath: "/repo/recorded.go", Meta: map[string]any{"contract_meta": map[string]any{"file": "/repo/recorded.go"}}}
	handle.AddBatch(nil, []*graph.Edge{owner})

	publishTestGeneration(t, store, handle.ViewGeneration())
	layer, err := NewGenerationLayer(handle)
	require.NoError(t, err)
	view := graph.NewOverlaidViewWithLayer(store, layer)
	p, err := view.LoadContractFileProjectionContext(context.Background(), "repo", []string{owner.FilePath})
	require.NoError(t, err)
	require.Len(t, p.OwnerRows, 1)
	require.Equal(t, "repo", p.OwnerRows[0].RepoPrefix)
	require.Equal(t, canonical.Meta, p.Targets[canonical.ID].Meta)
	require.Equal(t, owner.Meta, p.OwnerRows[0].Edge.Meta)
	p, err = view.LoadContractFileProjectionContext(context.Background(), "repo", []string{source.FilePath})
	require.NoError(t, err)
	require.Len(t, p.OffFileOwnerRows, 1)
	// A deletion above the edge-only layer removes its inherited source.
	removed := graph.NewOverlayLayer()
	removed.MarkFile(source.FilePath, true)
	p, err = graph.NewOverlaidView(view, removed).LoadContractFileProjectionContext(context.Background(), "repo", []string{owner.FilePath})
	require.NoError(t, err)
	require.Empty(t, p.OwnerRows)
}

func TestContractFileProjectionGenerationContextRowsDoNotReplaceBase(t *testing.T) {
	store := openTestStore(t)
	node := &graph.Node{ID: "/repo/context.go", Kind: graph.KindFile, FilePath: "/repo/context.go", Meta: map[string]any{"stamp": "base"}}
	store.AddBatch([]*graph.Node{node}, nil)
	_, handle := beginTestGeneration(t, store, "contract-context")

	require.NoError(t, handle.SetFileMasks([]store_sqlite.FileMask{{FilePath: node.FilePath, Mode: store_sqlite.OwnershipContext}}))
	publishTestGeneration(t, store, handle.ViewGeneration())
	layer, err := NewGenerationLayer(handle)
	require.NoError(t, err)
	p, err := graph.NewOverlaidViewWithLayer(store, layer).LoadContractFileProjectionContext(context.Background(), "repo", []string{node.FilePath})
	require.NoError(t, err)
	require.Len(t, p.FileNodes[node.FilePath], 1)
	require.Equal(t, node.Meta, p.FileNodes[node.FilePath][0].Meta)
	require.NoError(t, store.Close())
	p, err = graph.NewOverlaidViewWithLayer(store, layer).LoadContractFileProjectionContext(context.Background(), "repo", []string{node.FilePath})
	require.Error(t, err)
	require.Equal(t, graph.ContractFileProjection{}, p)
}
