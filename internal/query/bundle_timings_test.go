package query

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

type timingBundleBackend struct{ pathBundleBackend }

func (b *timingBundleBackend) SearchSymbolBundlesPathScopedContext(ctx context.Context, query string, repos, paths []string, limit int) ([]search.SymbolBundle, bool) {
	if observe := search.SymbolBundleTimingsObserver(ctx); observe != nil {
		observe(search.SymbolBundleTimings{Calls: 1, RankMS: 1.25, NodeMS: 2.5, OutMS: 3.75, InMS: 4.5, RankedHits: 1, UniqueIDs: 1, CacheMisses: 1, NodeRows: 1, OutRows: 2, InRows: 3})
	}
	return b.pathBundleBackend.SearchSymbolBundlesPathScopedContext(ctx, query, repos, paths, limit)
}

func TestBundleLegTimingsStayWithinSearchRequest(t *testing.T) {
	g := graph.New()
	n := &graph.Node{ID: "repo/a.go::Needle", Kind: graph.KindFunction, Name: "needle", RepoPrefix: "repo", FilePath: "repo/a.go"}
	g.AddNode(n)
	b := &timingBundleBackend{pathBundleBackend: pathBundleBackend{answer: []search.SymbolBundle{{Node: n}}, handled: true}}
	eng := NewEngine(g)
	eng.SetSearch(b)
	eng.SetRerank(nil)
	first, second := &SearchTimings{}, &SearchTimings{}
	opts := QueryOptions{RepoAllow: map[string]bool{"repo": true}, SearchPathPrefixes: []string{"a.go"}, SkipVectorChannel: true, SkipInnerRerank: true, SearchTimings: first}
	for i := 0; i < 2; i++ {
		got := eng.GatherSymbolCandidatesContext(context.Background(), "needle", 1, opts, nil)
		if len(got) != 1 || got[0].Node.ID != n.ID {
			t.Fatalf("results=%v", got)
		}
	}
	opts.SearchTimings = second
	eng.GatherSymbolCandidatesContext(context.Background(), "needle", 1, opts, nil)
	if first.BundleLegs.Calls != 2 || first.BundleLegs.RankMS != 2.5 || first.BundleLegs.NodeMS != 5 || first.BundleLegs.OutMS != 7.5 || first.BundleLegs.InMS != 9 || first.BundleLegs.CacheMisses != 2 || first.BundleLegs.NodeRows != 2 || first.BundleLegs.OutRows != 4 || first.BundleLegs.InRows != 6 {
		t.Fatalf("accumulated timings=%+v", first.BundleLegs)
	}
	if second.BundleLegs.Calls != 1 || second.BundleLegs.RankMS != 1.25 {
		t.Fatalf("cross-request timing leak=%+v", second.BundleLegs)
	}
}
