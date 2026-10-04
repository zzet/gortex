package store_sqlite

import (
	"context"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The split exact tier answers exactly what each generation's own search
// answers from its exact tier (the single-generation path, an independent
// implementation), leaves exactly the generations that search sends to
// full text, and the combined batch call is unchanged by the split.
func TestSymbolExactHitsViewGenerationsIsTheCombinedCallsExactTier(t *testing.T) {
	ctx := context.Background()
	store := viewBatchTestStore(t)
	add := func(gen int64, nodes []*graph.Node, docs []graph.SymbolFTSItem) {
		t.Helper()
		if err := viewBatchHandle(store, gen).AddBatchChecked(nodes, nil); err != nil {
			t.Fatal(err)
		}
		seedViewBatchFTS(t, store, gen, docs)
	}
	add(0, []*graph.Node{
		{ID: "b1", Name: "Needle", Kind: graph.KindFunction, RepoPrefix: "repo"},
		{ID: "b2", Name: "Needle", Kind: graph.KindMethod, RepoPrefix: "repo"},
		{ID: "b3", Name: "Needle", Kind: graph.KindType, RepoPrefix: "repo"},
		{ID: "b-fts", RepoPrefix: "repo"},
	}, []graph.SymbolFTSItem{{NodeID: "b-fts", Tokens: "needle"}})
	add(5, []*graph.Node{{ID: "g5", Name: "Needle", Kind: graph.KindFunction, RepoPrefix: "other"}, {ID: "g5-fts", RepoPrefix: "repo"}},
		[]graph.SymbolFTSItem{{NodeID: "g5-fts", Tokens: "needle haystack"}})
	add(7, []*graph.Node{{ID: "g7", Name: "Needle", Kind: graph.KindFunction, RepoPrefix: "repo"}}, nil)
	add(9, []*graph.Node{{ID: "g9-fts", RepoPrefix: "repo"}}, []graph.SymbolFTSItem{{NodeID: "g9-fts", Tokens: "needle"}})

	for _, tc := range []struct {
		query string
		repos []string
		limit int
	}{
		{"Needle", []string{"repo"}, 2},
		{"Needle", nil, 5},
		{"needle haystack", []string{"repo"}, 3},
		{"", nil, 3},
	} {
		gens := []int64{0, 5, 7, 9, 5}
		hits, remaining, err := store.SymbolExactHitsViewGenerations(ctx, tc.query, tc.repos, gens, tc.limit)
		if err != nil {
			t.Fatal(err)
		}
		combined, err := store.SearchSymbolsViewGenerationsRepoScopedContext(ctx, tc.query, tc.repos, gens, tc.limit)
		if err != nil {
			t.Fatal(err)
		}
		var wantRemaining []int64
		for _, gen := range []int64{0, 5, 7, 9} {
			if tc.query == "" {
				if hits[gen] != nil || combined[gen] != nil {
					t.Fatalf("empty query answered generation %d", gen)
				}
				continue
			}
			single, err := viewBatchHandle(store, gen).SearchSymbolsRepoScopedContext(ctx, tc.query, tc.repos, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			exact := len(single) > 0 && single[0].Score == 100.0 && isIdentifierQuery(tc.query)
			if exact {
				if !reflect.DeepEqual(hits[gen], single) {
					t.Fatalf("%q gen %d: split exact tier %v, generation's own %v", tc.query, gen, hits[gen], single)
				}
			} else {
				if len(hits[gen]) != 0 {
					t.Fatalf("%q gen %d: split exact tier answered %v, the generation's own search did not", tc.query, gen, hits[gen])
				}
				wantRemaining = append(wantRemaining, gen)
			}
			if !reflect.DeepEqual(combined[gen], single) {
				t.Fatalf("%q gen %d: combined %v, generation's own %v", tc.query, gen, combined[gen], single)
			}
		}
		if !reflect.DeepEqual(remaining, wantRemaining) {
			t.Fatalf("%q: remaining %v, want %v", tc.query, remaining, wantRemaining)
		}
	}
	if store.SymbolSearchCoreKey() == nil || store.SymbolSearchCoreKey() != viewBatchHandle(store, 7).SymbolSearchCoreKey() {
		t.Fatal("handles over one core must share the search core key")
	}
	other := viewBatchTestStore(t)
	if other.SymbolSearchCoreKey() == store.SymbolSearchCoreKey() {
		t.Fatal("two stores share a search core key")
	}
}
