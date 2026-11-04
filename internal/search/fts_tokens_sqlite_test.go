package search_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/search"
)

func TestSharedASCIITokensRankLikeFTS5(t *testing.T) {
	ctx := context.Background()
	f := &sizedFTSTable{ftsTable: newFTSTable(t), matched: map[int64]int{}, whole: map[int64]int{}}
	for _, gen := range []int64{81, 82} {
		var rows [][3]string
		for i := 0; i < 60; i++ {
			text := []string{
				"checkout route checkout coordinator",
				"CHECKOUT.Route Checkout::Coordinator123",
				"check\x00checkout_123\tRoute\r\nroute",
				"ROUTE" + strings.Repeat(" :/_-.\t", 64) + "Checkout",
				"!!!",
			}[i%5]
			rows = append(rows, [3]string{fmt.Sprintf("g%d/row%d", gen, i), []string{"alpha", "beta", ""}[i%3], text})
		}
		f.addGeneration(t, gen, rows)
	}
	ranker := search.NewFTSRanker(f)
	if _, err := ranker.WarmGenerations(ctx, []int64{81, 82}); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ {
		for _, gen := range []int64{81, 82} {
			for _, q := range []string{"checkout", "route", "co", "123", "checkout route"} {
				for _, repos := range [][]string{nil, {"alpha"}, {"beta"}, {"unknown"}} {
					for _, limit := range []int{1, 7, 100} {
						got, ok, err := ranker.Rank(ctx, gen, true, q, repos, limit)
						if err != nil || !ok {
							t.Fatalf("gen %d query %q: ok=%v err=%v", gen, q, ok, err)
						}
						want := f.storeRank(t, gen, q, repos, limit)
						if renderHits(got) != renderHits(want) {
							t.Fatalf("gen %d query %q repos %v limit %d:\n memory %s\n fts5 %s", gen, q, repos, limit, renderHits(got), renderHits(want))
						}
					}
				}
			}
		}
		// Rebuild one generation's tokens while the other's documents stay
		// cached: no reusable buffer may corrupt its bytes or score ties.
		ranker.Forget(81)
	}
}
