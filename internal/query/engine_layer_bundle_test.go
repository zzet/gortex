package query

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
)

type bundleHydrationReader struct {
	graph.Reader
	layer    *graph.OverlayLayer
	hydrated []string
}

func (r *bundleHydrationReader) Layer() *graph.OverlayLayer { return r.layer }
func (r *bundleHydrationReader) GetNodesByIDs(ids []string) map[string]*graph.Node {
	r.hydrated = append(r.hydrated, ids...)
	return r.Reader.GetNodesByIDs(ids)
}

func TestLayerBundleHydrationKeepsUntouchedPayloadAndEdges(t *testing.T) {
	base, backend, layer, replacement := overlayBundleFixture(t)
	untouched := &graph.Node{ID: "repo/untouched.go::WidgetOther", Name: "WidgetOther", Kind: graph.KindFunction, FilePath: "repo/untouched.go"}
	base.AddNode(untouched)
	backend.bundles = append(backend.bundles, search.SymbolBundle{Node: untouched, Score: 8})
	reader := &bundleHydrationReader{Reader: graph.NewOverlaidView(base, layer), layer: layer}
	engine := NewEngine(base)
	engine.SetSearch(backend)
	rctx := &rerank.Context{Graph: reader}
	got := engine.WithReader(reader).GatherSymbolCandidates("Widget", 5, QueryOptions{SkipInnerRerank: true, SkipVectorChannel: true}, rctx)
	require.Same(t, replacement, candidateByID(got, overlayBundleKeptID).Node)
	require.Nil(t, candidateByID(got, overlayBundleGoneID))
	require.Same(t, untouched, candidateByID(got, untouched.ID).Node)
	require.False(t, slices.Contains(reader.hydrated, untouched.ID), "an unrelated pending file must not discard this base bundle payload")
	require.Equal(t, 1.0, rctx.EdgeCacheHitRate([]string{untouched.ID}), "safe base adjacency must remain seeded even when another hit needs rehydration")
	require.Equal(t, 0.0, rctx.EdgeCacheHitRate([]string{overlayBundleKeptID}), "covered adjacency cannot be seeded from a stale base bundle")
	require.False(t, rctx.CachePreSeeded(), "partial seeds cannot claim complete coverage")
}

// The runtime contract wrapper deliberately hides the concrete Layer method.
// The explicit layer input must survive that wrapper, without type guessing.
type bundleContractReader struct{ graph.Reader }

func TestLayerBundleContractWrapperKeepsPendingReplacement(t *testing.T) {
	base, backend, layer, replacement := overlayBundleFixture(t)
	reader := bundleContractReader{Reader: graph.NewOverlaidView(base, layer)}
	engine := NewEngine(base)
	engine.SetSearch(backend)
	bound := engine.WithReader(reader)
	if explicit, ok := any(bound).(interface {
		WithLayers([]graph.OverlayLayerReader) *Engine
	}); ok {
		layers := []graph.OverlayLayerReader{layer}
		bound = explicit.WithLayers(layers)
		layers[0] = graph.NewOverlayLayer() // Caller slice mutation cannot rewrite an already bound request.
	}
	got := bound.GatherSymbolCandidates("Widget", 5, QueryOptions{SkipInnerRerank: true, SkipVectorChannel: true}, &rerank.Context{Graph: reader})
	require.Nil(t, candidateByID(got, overlayBundleGoneID), "contract wrapping must not revive a deleted base hit")
	kept := candidateByID(got, overlayBundleKeptID)
	require.NotNil(t, kept)
	require.Same(t, replacement, kept.Node, "contract wrapping must not serve the pre-edit bundle row")
}

func TestLayerBundleDoesNotSeedAdjacencyAffectedThroughAnotherFile(t *testing.T) {
	for _, kind := range []string{"new_edge", "deleted_endpoint", "changed_edge_owner"} {
		t.Run(kind, func(t *testing.T) {
			base, backend, layer, _ := overlayBundleFixture(t)
			node := &graph.Node{ID: "repo/untouched.go::WidgetOther", Name: "WidgetOther", Kind: graph.KindFunction, FilePath: "repo/untouched.go"}
			base.AddNode(node)
			bundle := search.SymbolBundle{Node: node, Score: 8}
			if kind == "new_edge" {
				layer.AddEdge(&graph.Edge{From: overlayBundleKeptID, To: node.ID, Kind: graph.EdgeCalls, FilePath: overlayBundleFile})
			} else {
				target, file := overlayBundleGoneID, node.FilePath
				if kind == "changed_edge_owner" {
					target = overlayBundleCaller
					file = overlayBundleFile
				}
				edge := &graph.Edge{From: node.ID, To: target, Kind: graph.EdgeCalls, FilePath: file}
				base.AddEdge(edge)
				bundle.OutEdges = []*graph.Edge{edge}
			}
			backend.bundles = append(backend.bundles, bundle)
			view := graph.NewOverlaidView(base, layer)
			reader := &bundleHydrationReader{Reader: view, layer: layer}
			engine := NewEngine(base)
			engine.SetSearch(backend)
			rctx := &rerank.Context{Graph: reader}
			got := engine.WithReader(reader).GatherSymbolCandidates("Widget", 5, QueryOptions{SkipInnerRerank: true, SkipVectorChannel: true}, rctx)
			kept := candidateByID(got, node.ID)
			require.NotNil(t, kept)
			require.Same(t, node, kept.Node, "adjacency changes do not require refetching an unchanged node")
			require.False(t, slices.Contains(reader.hydrated, node.ID))
			require.Zero(t, rctx.EdgeCacheHitRate([]string{node.ID}), "new/deleted/re-owned edges cannot be seeded from base bundles")
			if kind == "new_edge" {
				require.Len(t, view.GetInEdges(node.ID), 1)
			} else {
				require.Empty(t, view.GetOutEdges(node.ID))
			}
		})
	}
}
