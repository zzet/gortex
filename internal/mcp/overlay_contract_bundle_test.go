package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

func TestContractRuntimeSearchPreservesSessionOverlayBundles(t *testing.T) {
	for _, contract := range []bool{false, true} {
		name := "plain_overlay"
		if contract {
			name = "contract_runtime"
		}
		t.Run(name, func(t *testing.T) {
			old := &graph.Node{ID: "repo/edit.go::Widget", Name: "Widget", Kind: graph.KindFunction, FilePath: "repo/edit.go", StartLine: 3}
			gone := &graph.Node{ID: "repo/edit.go::WidgetGone", Name: "WidgetGone", Kind: graph.KindFunction, FilePath: "repo/edit.go", StartLine: 8}
			backend := &searchSymbolsContextBackend{bundles: []search.SymbolBundle{{Node: old, Score: 10}, {Node: gone, Score: 9}}}
			server := newSearchSymbolsContextServer(backend)
			base := server.graph.(*graph.Graph)
			base.AddNode(old)
			base.AddNode(gone)
			layer := graph.NewOverlayLayer()
			layer.MarkFile(old.FilePath, false)
			replacement := *old
			replacement.StartLine = 44
			layer.AddNode(old.FilePath, &replacement)
			layer.MarkRemoved(gone.Name, gone.ID)
			if contract {
				server.contractAnalysisRuntime = &ContractAnalysisRuntime{}
			}
			ctx := WithOverlayView(t.Context(), graph.NewOverlaidView(base, layer))
			got := server.engineFor(ctx).GatherSymbolCandidates("Widget", 5, query.QueryOptions{SkipInnerRerank: true, SkipVectorChannel: true}, nil)
			require.Len(t, got, 1, "the actual request engine cannot revive an overlay-deleted bundle through its runtime reader wrapper")
			require.Same(t, &replacement, got[0].Node, "the actual request engine must serve the session replacement")
		})
	}
}
