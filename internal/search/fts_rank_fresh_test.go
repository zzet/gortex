package search_test

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/search"
)

// freshTable hands over, once, the rows it was given per generation (the
// store's fresh-rows hand-off), with no token counts, as the write path has
// them.
type freshTable struct {
	*ownedFTSTable
	fresh map[int64][]search.FTSRankRow
}

func (f *freshTable) TakeFreshGenerationRows(generation int64) ([]search.FTSRankRow, bool) {
	rows, ok := f.fresh[generation]
	delete(f.fresh, generation)
	return rows, ok
}

// writtenRows are a generation's rows as its build wrote them: rowid, node,
// repository and tokens, no token count.
func (f *freshTable) writtenRows(t *testing.T, generation int64) []search.FTSRankRow {
	t.Helper()
	rows, err := f.SymbolFTSGenerationRows(context.Background(), generation, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.walked = f.walked[:len(f.walked)-1] // setting up the hand-off is not a read under test
	for i := range rows {
		rows[i].TokenCount = 0
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Rowid < rows[j].Rowid })
	return rows
}

// A new generation whose rows were handed over at write time is ranked from
// them with no read of its documents — rankings equal FTS5's, token counts
// counted by the ranker — and a hand-off that does not match the generation's
// statistics is refused, and the generation read.
func TestHandedOverRowsServeTheNewGenerationWithNoRead(t *testing.T) {
	ctx := context.Background()
	f := &freshTable{ownedFTSTable: newOwnedFTSTable(t), fresh: map[int64][]search.FTSRankRow{}}
	rows := fixtureRows("fresh", 2000, 7)
	next := 0
	take := func() [3]string { r := rows[next%len(rows)]; next++; r[0] = fmt.Sprintf("f%d", next); return r }
	for i := 0; i < 40; i++ {
		f.insert(t, 60, take())
		for j := 0; j < 20; j++ {
			f.insert(t, 62, take())
		}
		f.insert(t, 61, take())
	}
	f.fresh[60] = f.writtenRows(t, 60)
	short := f.writtenRows(t, 61)
	f.fresh[61] = short[:len(short)-1] // one row missing: not the generation's
	ranker := search.NewFTSRanker(f)
	if !ranker.AdoptFresh(60) || !ranker.AdoptFresh(61) {
		t.Fatal("the hand-off was not taken")
	}
	adopted, rejected := search.FTSFreshCounts()
	for _, q := range []string{"config", "checkout route", "newDirtySamplerRen9", "flip coordinator"} {
		for _, generation := range []int64{60, 61} {
			got, ok, err := ranker.Rank(ctx, generation, true, q, nil, 20)
			if err != nil || !ok {
				t.Fatalf("gen %d %q: ok=%v err=%v", generation, q, ok, err)
			}
			if want := f.storeRankOwned(t, generation, q, 20); renderHits(got) != renderHits(want) {
				t.Fatalf("gen %d %q:\n memory %s\n fts5   %s", generation, q, renderHits(got), renderHits(want))
			}
		}
	}
	nowAdopted, nowRejected := search.FTSFreshCounts()
	if nowAdopted-adopted != 1 || nowRejected-rejected != 1 {
		t.Fatalf("adopted %d, rejected %d; want 1 and 1", nowAdopted-adopted, nowRejected-rejected)
	}
	reads := wholeReads(f.ownedFTSTable)
	if reads[60] != 0 || matchReads(f.ownedFTSTable, 60) != 0 {
		t.Fatalf("the handed-over generation was read: %d whole, %d MATCH", reads[60], matchReads(f.ownedFTSTable, 60))
	}
	if reads[61] != 1 {
		t.Fatalf("the refused hand-off's generation was read whole %d time(s), want 1", reads[61])
	}
}
