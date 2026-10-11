package store_sqlite

import (
	"context"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

func TestSearchSymbolBundleTimingsPreserveColdAndCachedResults(t *testing.T) {
	s := newBundleTestStore(t)
	seedBundleStore(t, s)
	s.SetBundleFingerprints(map[string]uint64{"pkg": 1})
	var observed []search.SymbolBundleTimings
	ctx := search.WithSymbolBundleTimingsObserver(context.Background(), func(stats search.SymbolBundleTimings) {
		observed = append(observed, stats)
	})
	first, err := s.SearchSymbolBundlesContext(ctx, "widget", 10)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.SearchSymbolBundlesContext(ctx, "widget", 10)
	if err != nil {
		t.Fatal(err)
	}
	unobserved, err := s.SearchSymbolBundles("widget", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(second, unobserved) {
		t.Fatalf("timing changed ranked bundles: cold=%v cached=%v unobserved=%v", first, second, unobserved)
	}
	if len(observed) != 2 {
		t.Fatalf("observations=%v", observed)
	}
	cold, cached := observed[0], observed[1]
	if cold.Calls != 1 || cold.RankedHits != 2 || cold.UniqueIDs != 2 || cold.CacheHits != 0 || cold.CacheMisses != 2 || cold.NodeRows != 2 || cold.OutRows != 1 || cold.InRows != 1 {
		t.Fatalf("cold counts=%+v", cold)
	}
	if cached.Calls != 1 || cached.RankedHits != 2 || cached.UniqueIDs != 2 || cached.CacheHits != 2 || cached.CacheMisses != 0 || cached.NodeRows != 0 || cached.OutRows != 0 || cached.InRows != 0 || cached.NodeMS != 0 || cached.OutMS != 0 || cached.InMS != 0 {
		t.Fatalf("cached legs=%+v", cached)
	}
	for _, stats := range observed {
		if stats.RankMS < 0 || stats.NodeMS < 0 || stats.OutMS < 0 || stats.InMS < 0 {
			t.Fatalf("negative milliseconds=%+v", stats)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.SearchSymbolBundlesContext(canceled, "widget", 10)
	if err != context.Canceled {
		t.Fatalf("cancellation=%v", err)
	}
	if len(observed) != 3 || observed[2].CacheHits != 0 || observed[2].CacheMisses != 0 || observed[2].NodeRows != 0 || observed[2].OutRows != 0 || observed[2].InRows != 0 {
		t.Fatalf("canceled call hydrated: %+v", observed)
	}
	// The graph facts remain unchanged after observed, cached and canceled calls.
	if len(s.GetOutEdgesByNodeIDs([]string{"pkg/x.go::A"})["pkg/x.go::A"]) != 1 || first[0].Node.Kind != graph.KindFunction {
		t.Fatal("graph facts changed")
	}
}
