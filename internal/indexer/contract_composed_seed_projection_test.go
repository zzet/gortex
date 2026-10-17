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
	edge := &graph.Edge{From: source.ID, To: target.ID, Kind: graph.EdgeProvides, FilePath: source.FilePath, Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingUseValue, graph.MetaDIToken: "TOKEN"}}
	layer.AddEdge(edge)
	layer.AddEdge(&graph.Edge{From: foreign.ID, To: target.ID, Kind: graph.EdgeConsumes, FilePath: foreign.FilePath})
	selected := graph.NewOverlaidView(store, layer)
	// Duplicate physical handles exercise candidate deduplication; the selected
	// edge-only layer deliberately inherits its source from the lower store.
	handles := []*store_sqlite.Store{store, store}
	nodes, edges, supported, err := readContractFollowupSeeds(ctx, selected, handles, "repo")
	require.NoError(t, err)
	require.True(t, supported)
	require.Equal(t, []*graph.Node{&updated}, nodes)
	require.Equal(t, []*graph.Edge{edge}, edges)
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
	handles := []*store_sqlite.Store{store, store}
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
