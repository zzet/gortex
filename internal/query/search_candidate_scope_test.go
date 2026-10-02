package query

import (
	"context"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

func TestBackendPathScopePrecedesSaturationAndSupplementaryLimit(t *testing.T) {
	g := graph.New()
	backend := &contextPlainBackend{}
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("other/%03d.go::BENCH_BASELINE", i)
		g.AddNode(&graph.Node{ID: id, Name: "BENCH_BASELINE", Kind: graph.KindFunction, FilePath: fmt.Sprintf("other/%03d.go", i), RepoPrefix: "other"})
		backend.results = append(backend.results, search.SearchResult{ID: id})
	}
	target := &graph.Node{ID: "gortex/Makefile::BENCH_BASELINE", Name: "BENCH_BASELINE", Kind: graph.KindVariable, FilePath: "gortex/Makefile", RepoPrefix: "gortex"}
	g.AddNode(target)
	eng := NewEngine(g)
	eng.SetSearch(backend)
	eng.SetRerank(nil)
	opts := QueryOptions{RepoAllow: map[string]bool{"gortex": true}, SearchNodeFilter: func(n *graph.Node) bool { return n.FilePath == "gortex/Makefile" }}
	for _, q := range []string{"BENCH_BASELINE", "BENCH_BASE", "BASELINE"} {
		got := eng.SearchSymbolsRankedContext(context.Background(), q, 1, opts, nil)
		if len(got) != 1 || got[0].Node.ID != target.ID {
			t.Fatalf("%s scoped result=%v", q, got)
		}
	}
}

func TestNoBackendScopePrecedesHeapAndOnlyWinnersHydrate(t *testing.T) {
	g := graph.New()
	for i := 0; i < 400; i++ {
		g.AddNode(&graph.Node{ID: fmt.Sprintf("other/a.go::n%03d", i), Name: "needle", Kind: graph.KindFunction, FilePath: "other/a.go", RepoPrefix: "other"})
	}
	g.AddNode(&graph.Node{ID: "repo/a.go::NeedleLonger", Name: "NeedleLonger", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo", WorkspaceID: "ws", ProjectID: "proj"})
	probe := &substringProbeStore{Graph: g}
	opts := QueryOptions{WorkspaceID: "ws", ProjectID: "proj", RepoAllow: map[string]bool{"repo": true}, SearchNodeFilter: func(n *graph.Node) bool { return n.FilePath == "repo/a.go" }}
	got := NewEngine(probe).searchSubstringScoped(context.Background(), "needle", 1, opts)
	if len(got) != 1 || got[0].ID != "repo/a.go::NeedleLonger" {
		t.Fatalf("scope after heap truncated winner: %v", substringNodeIDs(got))
	}
	if len(probe.hydrationBatches) != 1 || len(probe.hydrationBatches[0]) != 1 || probe.hydrationBatches[0][0] != got[0].ID {
		t.Fatalf("hydrated rejected keys: %v", probe.hydrationBatches)
	}
}

func TestNameSupplementKeepsGlobalExternalWorkspacePolicy(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "external::go:fmt::needle", Name: "needle", Kind: graph.KindFunction, FilePath: "external::go:fmt"})
	g.AddNode(&graph.Node{ID: "unowned/a.go::needle", Name: "needle", Kind: graph.KindFunction, FilePath: "unowned/a.go"})
	eng := NewEngine(g)
	eng.SetSearch(&contextPlainBackend{})
	eng.SetRerank(nil)
	got := eng.SearchSymbolsRankedContext(context.Background(), "needle", 1, QueryOptions{WorkspaceID: "ws", RepoAllow: map[string]bool{"repo": true}}, nil)
	if len(got) != 1 || got[0].Node.ID != "external::go:fmt::needle" {
		t.Fatalf("global external scope policy changed: %v", got)
	}
}
