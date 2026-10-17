package store_sqlite

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// publishFTSGeneration publishes one derived generation carrying n symbol
// documents whose tokens vary so bm25 has real differences to rank.
func publishFTSGeneration(t *testing.T, store *Store, layer string, n int) int64 {
	t.Helper()
	ctx := context.Background()
	id, handle, err := store.BeginPayloadGeneration(ctx, PayloadGenerationRequest{
		OwnerKind: "ref_view", GraphID: "graph-" + layer, LayerID: layer,
		GenerationKind: "commit", TreeOID: "tree-" + layer, CreatedAt: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	var nodes []*graph.Node
	var items []graph.SymbolFTSItem
	for i := range n {
		nodeID := fmt.Sprintf("repo/%s.go::S%03d", layer, i)
		nodes = append(nodes, &graph.Node{ID: nodeID, Name: fmt.Sprintf("S%03d", i), Kind: graph.KindFunction, RepoPrefix: "repo"})
		tokens := "match skip rule"
		switch i % 4 {
		case 1:
			tokens = "match skip rule ren extra words here"
		case 2:
			tokens = fmt.Sprintf("matches other%d", i%9)
		case 3:
			tokens = "rule"
		}
		items = append(items, graph.SymbolFTSItem{NodeID: nodeID, Tokens: tokens})
	}
	handle.AddBatch(nodes, nil)
	if err := handle.BatchUpsertSymbolFTS(items); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishPayloadGeneration(ctx, id, 20); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestPublishedGenerationFTSPagesAreMemoizedAndFollowTheCorpus pins both halves
// of the page memo: a repeated or deeper page of a published generation runs
// no query, and any write to the shared FTS table — which changes bm25's corpus
// statistics — retires every memoized ranking.
func TestPublishedGenerationFTSPagesAreMemoizedAndFollowTheCorpus(t *testing.T) {
	store := openCatalogStore(t)
	gen := publishFTSGeneration(t, store, "memo", 60)
	queries := map[int64]int{}
	symbolFTSSpanQueryObserver = func(generation int64) { queries[generation]++ }
	t.Cleanup(func() { symbolFTSSpanQueryObserver = nil })

	search := func(limit int) map[int64][]graph.SymbolHit {
		t.Helper()
		got, err := store.SearchSymbolsViewGenerationsRepoScopedContext(context.Background(), "matchSkipRuleRen", nil, []int64{gen}, limit)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for _, limit := range []int{5, 10, 40, 5} {
		got := search(limit)
		if want := unboundedSymbolPages(t, store, "matchSkipRuleRen", nil, []int64{gen}, limit); !reflect.DeepEqual(got, want) {
			t.Fatalf("limit %d: memoized page differs\n got=%v\nwant=%v", limit, got, want)
		}
		single, err := store.AtGeneration(gen).SearchSymbolsRepoScopedContext(context.Background(), "matchSkipRuleRen", nil, limit)
		if err != nil {
			t.Fatal(err)
		}
		if want := unboundedSingleGeneration(t, store, gen, "matchSkipRuleRen", nil, limit); !reflect.DeepEqual(single, want) {
			t.Fatalf("limit %d: memoized single page differs\n got=%v\nwant=%v", limit, single, want)
		}
	}
	if queries[gen] != 1 {
		t.Fatalf("published generation ran %d FTS queries for one MATCH, want 1", queries[gen])
	}

	// Another generation's documents change bm25's statistics for every row.
	before := search(20)[gen]
	publishFTSGeneration(t, store, "shift", 200)
	after := search(20)
	if want := unboundedSymbolPages(t, store, "matchSkipRuleRen", nil, []int64{gen}, 20); !reflect.DeepEqual(after, want) {
		t.Fatalf("a stale ranking was served after the corpus changed\n got=%v\nwant=%v", after, want)
	}
	if reflect.DeepEqual(before, after[gen]) {
		t.Fatal("fixture: the corpus change did not move any score, so staleness is not exercised")
	}
	if queries[gen] != 2 {
		t.Fatalf("the corpus change did not force a re-rank (queries=%d)", queries[gen])
	}
}
