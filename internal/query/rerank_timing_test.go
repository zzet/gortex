package query

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
)

func TestEngineRerankTimingLabelsAndRestoresObserver(t *testing.T) {
	for _, exit := range []string{"success", "cancel", "panic"} {
		t.Run(exit, func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{ID: "a", Name: "Alpha", Kind: graph.KindFunction})
			g.AddNode(&graph.Node{ID: "b", Name: "AlphaHelper", Kind: graph.KindFunction})
			eng := NewEngine(g)
			eng.SetSearch(&contextPlainBackend{results: []search.SearchResult{{ID: "a"}, {ID: "b"}}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var seen []rerank.TimingStage
			abort := true
			rctx := &rerank.Context{ObserveTiming: func(timing rerank.Timing) {
				seen = append(seen, timing.Stage)
				if abort && exit == "cancel" {
					cancel()
				}
				if abort && exit == "panic" {
					panic("observer test")
				}
			}}
			run := func() {
				got := eng.SearchSymbolsRankedContext(ctx, "Alpha", 2, QueryOptions{}, rctx)
				if exit == "success" {
					require.Len(t, got, 2)
				} else {
					require.Empty(t, got)
				}
			}
			if exit == "panic" {
				require.Panics(t, run)
			} else {
				run()
			}
			require.NotEmpty(t, seen)
			for _, stage := range seen {
				require.Equal(t, rerank.TimingInner, stage)
			}
			abort = false
			seen = nil
			rctx.Prepare([]*rerank.Candidate{{Node: g.GetNode("a")}})
			require.Equal(t, []rerank.TimingStage{rerank.TimingOuter}, seen, "engine must restore the original observer on every exit")
		})
	}
}
