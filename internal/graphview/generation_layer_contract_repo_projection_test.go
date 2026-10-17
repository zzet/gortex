package graphview

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestContractRepoProjectionSelectedLayerKeepsInheritedOwnerEndpoints(t *testing.T) {
	s := openTestStore(t)
	source := &graph.Node{ID: "/repo/source.go::handler", Kind: graph.KindFunction, FilePath: "/repo/source.go", RepoPrefix: "repo"}
	target := &graph.Node{ID: "http-contract", Kind: graph.KindContract, FilePath: "/repo/source.go", RepoPrefix: "repo"}
	s.AddBatch([]*graph.Node{source, target}, nil)
	_, handle := beginTestGeneration(t, s, "repo-edge-only")
	owner := &graph.Edge{From: source.ID, To: target.ID, Kind: graph.EdgeProvides, FilePath: source.FilePath}
	handle.AddBatch(nil, []*graph.Edge{owner})
	publishTestGeneration(t, s, handle.ViewGeneration())
	layer, err := NewGenerationLayer(handle)
	require.NoError(t, err)
	view := graph.NewOverlaidViewWithLayer(s, layer)
	p, err := view.LoadContractRepoProjectionContext(context.Background(), "repo")
	require.NoError(t, err)
	require.Len(t, p.OwnerRows, 1)
	require.Equal(t, owner, p.OwnerRows[0].Edge)
	require.Equal(t, source, p.SourceNodes[source.ID])
	require.Equal(t, target, p.Targets[target.ID])
	removed := graph.NewOverlayLayer()
	removed.MarkFile(source.FilePath, true)
	p, err = graph.NewOverlaidView(view, removed).LoadContractRepoProjectionContext(context.Background(), "repo")
	require.NoError(t, err)
	require.Empty(t, p.OwnerRows)
}

func TestContractRepoProjectionCheckedLoaderRejectsStaleMaterializedLayer(t *testing.T) {
	s, nodes := constantLayerFixture(t)
	_, handle := beginTestGeneration(t, s, "repo-stale-inputs")
	handle.AddBatch(nodes, nil)
	require.NoError(t, handle.SetNodeIdentityReplacements([]string{nodes[0].ID, nodes[1].ID}))
	publishTestGeneration(t, s, handle.ViewGeneration())
	layer, err := NewGenerationLayer(handle)
	require.NoError(t, err)
	correction, err := s.BeginDerivedCorrection(context.Background(), store_sqlite.DerivedCorrectionRequest{
		GenerationID: handle.ViewGeneration(), Pass: "capability", FromVersion: 0, ToVersion: 1, EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField},
	})
	require.NoError(t, err)
	updated := *nodes[0]
	updated.Meta = map[string]any{"source_derived_decl_fingerprint": "changed"}
	require.NoError(t, correction.ReplaceSourceEdges(context.Background(), []string{nodes[1].ID}, nil, []*graph.Node{&updated}))
	view := graph.NewOverlaidViewWithLayer(s, layer)
	p, err := view.LoadContractRepoProjectionContext(context.Background(), "repo")
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	require.Equal(t, graph.ContractFileProjection{}, p)
	// The loader must retain the materialized layer's checked capability rather
	// than unwrapping it to the now-current physical handle.
	reg, err := contracts.LoadRegistryFromGraphChecked(context.Background(), view, contracts.RegistryLoadOptions{RepoPrefix: "repo"})
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	require.Nil(t, reg)
	_, err = correction.Finish(context.Background())
	require.NoError(t, err)
}

func TestContractRepoProjectionCanceledOrClosedLayerHasNoPartialResult(t *testing.T) {
	s := openTestStore(t)
	_, handle := beginTestGeneration(t, s, "repo-errors")
	publishTestGeneration(t, s, handle.ViewGeneration())
	layer, err := NewGenerationLayer(handle)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p, err := layer.LayerContractRepoProjectionContext(ctx, "repo")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, graph.ContractFileProjection{}, p)
	require.NoError(t, s.Close())
	p, err = layer.LayerContractRepoProjectionContext(context.Background(), "repo")
	require.Error(t, err)
	require.Equal(t, graph.ContractFileProjection{}, p)
}
