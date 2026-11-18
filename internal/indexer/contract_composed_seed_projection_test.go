package indexer

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestContractComposedSeedsReselectMetadataMasksAndEdgeOnlyOwners(t *testing.T) {
	ctx := t.Context()
	store := newFTSStore(t)
	method := &graph.Node{ID: "repo/A.java::run", Name: "run", Kind: graph.KindMethod, Language: "java", FilePath: "repo/A.java", RepoPrefix: "repo"}
	config := &graph.Node{ID: "repo/B.java::url", Name: "url", Kind: graph.KindField, Language: "java", FilePath: "repo/B.java", RepoPrefix: "repo", Meta: map[string]any{"spring_config_keys": []string{"old"}}}
	source := &graph.Node{ID: "repo/wire.go::Wire", Kind: graph.KindFunction, FilePath: "repo/wire.go", RepoPrefix: "repo"}
	target := &graph.Node{ID: "repo/target.go::Target", Kind: graph.KindFunction, FilePath: "repo/target.go", RepoPrefix: "repo"}
	foreign := &graph.Node{ID: "other/wire.go::Other", Kind: graph.KindFunction, FilePath: "other/wire.go", RepoPrefix: "other"}
	require.NoError(t, store.AddBatchChecked([]*graph.Node{method, config, source, target, foreign}, nil))
	layer := graph.NewOverlayLayer()
	layer.MarkFile(method.FilePath, true)
	updated := *config
	updated.Meta = map[string]any{"spring_config_keys": []string{"new"}}
	layer.MarkFile(config.FilePath, false)
	layer.AddNode(config.FilePath, &updated)
	edge := &graph.Edge{From: source.ID, To: target.ID, Kind: graph.EdgeProvides, FilePath: source.FilePath, Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingUseExisting, graph.MetaDIToken: "TOKEN", "existing_token": "TARGET"}}
	layer.AddEdge(edge)
	layer.AddEdge(&graph.Edge{From: foreign.ID, To: target.ID, Kind: graph.EdgeConsumes, FilePath: foreign.FilePath})
	selected := graph.NewOverlaidView(store, layer)
	// A single base handle does not make this composed reader primary. The
	// selected edge-only alias deliberately inherits its source from the base.
	handles := []*store_sqlite.Store{store}
	nodes, edges, supported, err := readContractFollowupSeeds(ctx, selected, handles, "repo")
	require.NoError(t, err)
	require.True(t, supported)
	require.Equal(t, []*graph.Node{&updated}, nodes)
	require.Equal(t, []*graph.Edge{edge}, edges)
	duplicateNodes, duplicateEdges, supported, err := readContractFollowupSeeds(ctx, selected, []*store_sqlite.Store{store, store}, "repo")
	require.NoError(t, err)
	require.True(t, supported)
	require.Equal(t, nodes, duplicateNodes)
	require.Equal(t, edges, duplicateEdges, "physical candidate deduplication does not change the selected alias")
	replacement := graph.NewOverlayLayer()
	replacement.MarkFile(config.FilePath, false)
	plain := updated
	plain.Meta = nil
	replacement.AddNode(plain.FilePath, &plain)
	nodes, edges, supported, err = readContractFollowupSeeds(ctx, graph.NewOverlaidView(selected, replacement), handles, "repo")
	require.NoError(t, err)
	require.True(t, supported)
	require.Empty(t, nodes, "a physical Spring candidate whose selected metadata no longer qualifies is absent")
	require.Equal(t, []*graph.Edge{edge}, edges)
	wrongKind := graph.NewOverlayLayer()
	wrongKind.MarkFile(config.FilePath, false)
	function := updated
	function.Kind = graph.KindFunction
	wrongKind.AddNode(function.FilePath, &function)
	nodes, _, supported, err = readContractFollowupSeeds(ctx, graph.NewOverlaidView(selected, wrongKind), handles, "repo")
	require.NoError(t, err)
	require.True(t, supported)
	require.Empty(t, nodes, "selected kind must still qualify for the physical seed query")
	removed := graph.NewOverlayLayer()
	removed.MarkFile(source.FilePath, true)
	_, edges, supported, err = readContractFollowupSeeds(ctx, graph.NewOverlaidView(selected, removed), handles, "repo")
	require.NoError(t, err)
	require.True(t, supported)
	require.Empty(t, edges, "an inherited DI edge cannot survive its selected source tombstone")
}

type contractSeedProjectionFailure struct {
	graph.Reader
	failure error
}

func (r contractSeedProjectionFailure) Unwrap() graph.Reader { return r.Reader }
func (r contractSeedProjectionFailure) LayerContractRepoProjectionContext(context.Context, string) (graph.ContractFileProjection, error) {
	return graph.ContractFileProjection{ScalarNodes: []*graph.Node{{ID: "partial", Kind: graph.KindContract}}}, r.failure
}

func TestContractComposedSeedsRejectCheckedErrorsAndCancellation(t *testing.T) {
	store := newFTSStore(t)
	handles := []*store_sqlite.Store{store}
	for _, failure := range []error{errors.New("checked selected projection failed"), graph.ErrContractProjectionLimit, graph.ErrContractProjectionStale} {
		nodes, edges, supported, err := readContractFollowupSeeds(t.Context(), contractSeedProjectionFailure{Reader: store, failure: failure}, handles, "repo")
		require.ErrorIs(t, err, failure)
		require.False(t, supported)
		require.Nil(t, nodes)
		require.Nil(t, edges)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	nodes, edges, supported, err := readContractFollowupSeeds(ctx, store, handles, "repo")
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, supported)
	require.Nil(t, nodes)
	require.Nil(t, edges)
}

func TestContractSeedsUseSelectedNativeReaderInsteadOfHandleProxy(t *testing.T) {
	ctx := t.Context()
	selected := newFTSStore(t)
	proxy := newFTSStore(t)
	method := &graph.Node{ID: "repo/A.java::run", Name: "run", Kind: graph.KindMethod, Language: "java", FilePath: "repo/A.java", RepoPrefix: "repo", StartLine: 9, Meta: map[string]any{"signature": "rich selected row"}}
	foreign := &graph.Node{ID: "other/Wire::bind", Kind: graph.KindFunction, FilePath: "other/Wire", RepoPrefix: "other"}
	edge := &graph.Edge{From: method.ID, To: foreign.ID, Kind: graph.EdgeProvides, FilePath: method.FilePath, Line: 11, Confidence: .9, Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingUseExisting, graph.MetaDIToken: "ALIAS"}}
	require.NoError(t, selected.AddBatchChecked([]*graph.Node{method, foreign}, []*graph.Edge{edge, {From: foreign.ID, To: method.ID, Kind: graph.EdgeConsumes, FilePath: foreign.FilePath}}))
	require.NoError(t, proxy.AddBatchChecked([]*graph.Node{{ID: "repo/proxy.java::wrong", Kind: graph.KindMethod, Language: "java", RepoPrefix: "repo"}}, nil))
	wantNodes, err := selected.ContractSeedNodesContext(ctx, "repo")
	require.NoError(t, err)
	wantEdges, err := selected.ContractSeedEdgesContext(ctx, "repo")
	require.NoError(t, err)
	require.Len(t, wantNodes, 1)
	require.Len(t, wantEdges, 1, "edge ownership follows the source repository, not its foreign target")
	nodes, edges, supported, err := readContractFollowupSeeds(ctx, selected, []*store_sqlite.Store{proxy}, "repo")
	require.NoError(t, err)
	require.True(t, supported)
	require.Equal(t, wantNodes, nodes)
	require.Equal(t, wantEdges, edges)

	// A positive reader must reselect candidates even when its only physical
	// candidate handle is primary; this empty generation hides the base rows.
	nodes, edges, supported, err = readContractFollowupSeeds(ctx, selected.AtGeneration(8), []*store_sqlite.Store{selected}, "repo")
	require.NoError(t, err)
	require.True(t, supported)
	require.Empty(t, nodes)
	require.Empty(t, edges)

	nodes, edges, supported, err = readContractFollowupSeeds(ctx, selected, nil, "repo")
	require.NoError(t, err)
	require.False(t, supported)
	require.Nil(t, nodes)
	require.Nil(t, edges)
	for _, reader := range []graph.Reader{nil, (*store_sqlite.Store)(nil)} {
		nodes, edges, supported, err = readContractFollowupSeeds(ctx, reader, []*store_sqlite.Store{selected}, "repo")
		require.ErrorIs(t, err, graph.ErrContractProjectionUnsupported)
		require.False(t, supported)
		require.Nil(t, nodes)
		require.Nil(t, edges)
	}
}
