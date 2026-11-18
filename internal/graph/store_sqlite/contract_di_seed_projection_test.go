package store_sqlite

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestContractDISeedProjectionKeepsScalarIdentityAndChecksRelevantPayload(t *testing.T) {
	s := newTestProjectionStore(t)
	local := &graph.Node{ID: "repo/wire.go::Wire", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/wire.go"}
	foreign := &graph.Node{ID: "other/wire.go::Wire", Kind: graph.KindFunction, RepoPrefix: "other", FilePath: "other/wire.go"}
	target := &graph.Node{ID: "di::TOKEN", Kind: graph.KindContract, RepoPrefix: "other", Meta: map[string]any{"contract_meta": "shared"}}
	unused := &graph.Node{ID: "http::GET::/unrelated", Kind: graph.KindContract, RepoPrefix: "repo"}
	own := &graph.Edge{From: local.ID, To: target.ID, Kind: graph.EdgeProvides, FilePath: local.FilePath, Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingUseExisting, graph.MetaDIToken: "ALIAS"}}
	other := &graph.Edge{From: foreign.ID, To: target.ID, Kind: graph.EdgeConsumes, FilePath: foreign.FilePath}
	require.NoError(t, s.AddBatchChecked([]*graph.Node{local, foreign, target, unused}, []*graph.Edge{own, other}))
	full, err := graph.CompleteContractRepoProjection(t.Context(), s, "repo")
	require.NoError(t, err)
	_, err = s.writerDB.Exec(`UPDATE nodes SET meta=? WHERE id=? AND view_gen=0`, []byte("invalid unrelated scalar metadata"), unused.ID)
	require.NoError(t, err)
	_, err = graph.CompleteContractRepoProjection(t.Context(), s, "repo")
	require.Error(t, err, "the generic full projection retains scalar validation")
	raw, err := s.LayerContractRepoOwnerProjectionContext(t.Context(), "repo")
	require.NoError(t, err, "the initial identity projection does not decode opaque scalar metadata")
	require.Equal(t, []*graph.Node{{ID: unused.ID, Kind: graph.KindContract, RepoPrefix: "repo"}}, raw.ScalarNodes)
	seeds, err := graph.CompleteContractDISeedProjection(t.Context(), s, "repo")
	require.Error(t, err, "retained scalar keys still receive complete canonical validation")
	require.Equal(t, graph.ContractFileProjection{}, seeds)
	require.NoError(t, s.AddBatchChecked([]*graph.Node{unused}, nil))
	seeds, err = graph.CompleteContractDISeedProjection(t.Context(), s, "repo")
	require.NoError(t, err)
	require.Empty(t, seeds.ScalarNodes)
	require.ElementsMatch(t, full.OwnerRows, seeds.OwnerRows)
	require.Len(t, seeds.OwnerRows, 2)
	require.Equal(t, target, seeds.Targets[target.ID])
	_, err = s.writerDB.Exec(`UPDATE edges SET meta=? WHERE from_id=? AND view_gen=0`, []byte("invalid relevant edge metadata"), local.ID)
	require.NoError(t, err)
	seeds, err = graph.CompleteContractDISeedProjection(t.Context(), s, "repo")
	require.Error(t, err)
	require.Equal(t, graph.ContractFileProjection{}, seeds)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	seeds, err = graph.CompleteContractDISeedProjection(canceled, s, "repo")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, graph.ContractFileProjection{}, seeds)
}

func TestContractDISeedProjectionKeepsOffFileReattributedOwner(t *testing.T) {
	s := newTestProjectionStore(t)
	source := &graph.Node{ID: "other/source.go::Wire", Kind: graph.KindFunction, RepoPrefix: "other", FilePath: "other/source.go"}
	target := &graph.Node{ID: "di::TOKEN", Kind: graph.KindContract, RepoPrefix: "repo", FilePath: "repo/di.go", Meta: map[string]any{"contract_meta": "complete"}}
	edge := &graph.Edge{From: source.ID, To: target.ID, Kind: graph.EdgeProvides, FilePath: "repo/recorded.go", Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingUseExisting, graph.MetaDIToken: "ALIAS"}}
	require.NoError(t, s.AddBatchChecked([]*graph.Node{source, target}, []*graph.Edge{edge}))
	layer := graph.NewOverlayLayer()
	selectedSource := *source
	selectedSource.RepoPrefix = "repo"
	layer.AddNode(source.FilePath, &selectedSource)
	view := graph.NewOverlaidView(s, layer)
	full, err := graph.CompleteContractRepoProjection(t.Context(), view, "repo")
	require.NoError(t, err)
	require.Len(t, full.OwnerRows, 1, "the off-file owner row survives the source-only replacement")
	require.Equal(t, &selectedSource, full.SourceNodes[source.ID])
	seeds, err := graph.CompleteContractDISeedProjection(t.Context(), view, "repo")
	require.NoError(t, err)
	require.ElementsMatch(t, full.OwnerRows, seeds.OwnerRows)
	require.Equal(t, full.SourceNodes, seeds.SourceNodes)
	require.Equal(t, full.Targets, seeds.Targets)
	removed := graph.NewOverlayLayer()
	removed.MarkFile(edge.FilePath, true)
	masked := graph.NewOverlaidView(view, removed)
	full, err = graph.CompleteContractRepoProjection(t.Context(), masked, "repo")
	require.NoError(t, err)
	seeds, err = graph.CompleteContractDISeedProjection(t.Context(), masked, "repo")
	require.NoError(t, err)
	require.Empty(t, seeds.OwnerRows)
	require.ElementsMatch(t, full.OwnerRows, seeds.OwnerRows)
}
