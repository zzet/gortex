package mcp

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"go.uber.org/zap"
)

func TestSearchSymbolsPathCandidateHorizonRescuesNaturalLanguageAndCursor(t *testing.T) {
	for _, weak := range []bool{false, true} {
		t.Run(fmt.Sprintf("weak_survivor_%v", weak), func(t *testing.T) {
			g := graph.New()
			backend := newOrderedBackend()
			// No node NAME contains the phrase. Only deeper text-channel retrieval
			// can find the path-scoped targets, even with a nonempty shallow page.
			if weak {
				id := "repo/src/weak.go::Weak"
				g.AddNode(&graph.Node{ID: id, Name: "Weak", Kind: graph.KindFunction, FilePath: "repo/src/weak.go", RepoPrefix: "repo"})
				backend.put("output", id)
			}
			for i := 0; i < 1100; i++ {
				id := fmt.Sprintf("repo/other/%04d.go::Noise", i)
				g.AddNode(&graph.Node{ID: id, Name: "Noise", Kind: graph.KindFunction, FilePath: fmt.Sprintf("repo/other/%04d.go", i), RepoPrefix: "repo"})
				backend.put("output", id)
			}
			for i := 0; i < 20; i++ {
				id := fmt.Sprintf("repo/src/%02d.go::Format", i)
				g.AddNode(&graph.Node{ID: id, Name: "Format", Kind: graph.KindFunction, FilePath: fmt.Sprintf("repo/src/%02d.go", i), RepoPrefix: "repo"})
				backend.put("output", id)
			}
			eng := query.NewEngine(g)
			eng.SetSearch(backend)
			eng.SetRerank(nil)
			srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
			off := false
			srv.SetSearchConfig(config.SearchConfig{RerankEmbedder: &off})
			// These fixtures exercise text retrieval with no vector backend.
			srv.RunAnalysis()
			resp := searchResp(t, srv, map[string]any{"query": "output encoding", "path": "./src/", "limit": 5, "cursor": encodeCursor(10), "expand": "none", "assist": "never"})
			require.Len(t, respIDs(resp), 5)
			for id := range respIDs(resp) {
				require.Contains(t, id, "repo/src/")
			}
			require.Equal(t, true, resp["fetch_escalated"])
			require.LessOrEqual(t, len(backend.searchLimits()), 3, "bounded initial plus two deeper fetches")
		})
	}
}

func TestSearchSymbolsPathRareExactDoesNotRefetchUnsaturatedText(t *testing.T) {
	g := graph.New()
	backend := newOrderedBackend()
	id := "repo/Makefile::BENCH_BASELINE"
	g.AddNode(&graph.Node{ID: id, Name: "BENCH_BASELINE", Kind: graph.KindVariable, FilePath: "repo/Makefile", RepoPrefix: "repo"})
	backend.put("bench", id)
	eng := query.NewEngine(g)
	eng.SetSearch(backend)
	eng.SetRerank(nil)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
	off := false
	srv.SetSearchConfig(config.SearchConfig{RerankEmbedder: &off})
	resp := searchResp(t, srv, map[string]any{"query": "BENCH_BASELINE", "path": "Makefile", "limit": 100, "expand": "none", "assist": "never"})
	require.True(t, respIDs(resp)[id])
	require.Nil(t, resp["fetch_escalated"])
	require.Len(t, backend.searchLimits(), 1)
}

func TestSearchSymbolsPathNameRecallWithNonemptyFilteredHead(t *testing.T) {
	g := graph.New()
	backend := newOrderedBackend()
	for i := 0; i < 300; i++ {
		id := fmt.Sprintf("gortex/elsewhere/%03d.go::BENCH_BASELINE", i)
		g.AddNode(&graph.Node{ID: id, Name: "BENCH_BASELINE", Kind: graph.KindVariable, FilePath: fmt.Sprintf("gortex/elsewhere/%03d.go", i), RepoPrefix: "gortex"})
		backend.put("bench", id)
		backend.put("baseline", id)
	}
	id := "gortex/Makefile::BENCH_BASELINE"
	g.AddNode(&graph.Node{ID: id, Name: "BENCH_BASELINE", Kind: graph.KindVariable, FilePath: "gortex/Makefile", RepoPrefix: "gortex"})
	weak := "gortex/Makefile::BENCH_BASE_WEAK"
	g.AddNode(&graph.Node{ID: weak, Name: "BENCH_BASE_WEAK", Kind: graph.KindVariable, FilePath: "gortex/Makefile", RepoPrefix: "gortex"})
	backend.put("bench", weak)
	eng := query.NewEngine(g)
	eng.SetSearch(backend)
	eng.SetRerank(nil)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
	off := false
	srv.SetSearchConfig(config.SearchConfig{RerankEmbedder: &off})
	srv.RunAnalysis()
	for _, q := range []string{"BENCH_BASE", "BASELINE"} {
		resp := searchResp(t, srv, map[string]any{"query": q, "repo": "gortex", "path": "Makefile", "limit": 100, "expand": "none", "assist": "never"})
		require.True(t, respIDs(resp)[id], "partial name must find Makefile target")
		for got := range respIDs(resp) {
			require.Contains(t, got, "gortex/Makefile::")
		}
	}
}
