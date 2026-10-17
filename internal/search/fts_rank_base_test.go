package search_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/search"
)

// newBaseFixture builds a table with a sparse base generation (40 documents
// interleaved with another generation's rows, so the store would walk the
// whole table to read it with no MATCH), a dense small generation, a small
// generation whose run the store does not report, and a generation id that
// owns no documents at all.
func newBaseFixture(t *testing.T) *ownedFTSTable {
	o := newOwnedFTSTable(t)
	o.unknownRun = map[int64]bool{35: true}
	rows := fixtureRows("row", 7000, 5)
	next := 0
	take := func() [3]string { r := rows[next%len(rows)]; next++; r[0] = fmt.Sprintf("b%d", next); return r }
	for i := 0; i < 40; i++ {
		o.insert(t, 30, take())
		for j := 0; j < 150; j++ {
			o.insert(t, 31, take())
		}
	}
	for i := 0; i < 50; i++ {
		o.insert(t, 33, take())
	}
	for i := 0; i < 20; i++ {
		o.insert(t, 35, take())
	}
	return o
}

func wholeReads(o *ownedFTSTable) map[int64]int {
	out := map[int64]int{}
	for _, w := range o.walked {
		if w.match == "" {
			out[w.generation]++
		}
	}
	return out
}

func matchReads(o *ownedFTSTable, generation int64) int {
	n := 0
	for _, w := range o.walked {
		if w.match != "" && w.generation == generation {
			n++
		}
	}
	return n
}

// A sparse generation is read whole once, and every later query — new names
// included — is answered from its kept documents with no read; a generation
// that owns no documents is answered empty with no read at all; a generation
// whose run the store does not report is read whole once too. Every ranking
// equals FTS5's.
func TestBaseGenerationsAreReadWholeOnceAndEmptyOnesNotAtAll(t *testing.T) {
	ctx := context.Background()
	o := newBaseFixture(t)
	ranker := search.NewFTSRanker(o)
	queries := []string{"config", "checkout route", "matchesSkipRuleRen5", "flip coordinator", "newDirtySamplerRen9"}
	for _, q := range queries {
		for _, generation := range []int64{30, 33, 35, 40} {
			got, ok, err := ranker.Rank(ctx, generation, true, q, nil, 20)
			if err != nil || !ok {
				t.Fatalf("gen %d %q: ok=%v err=%v", generation, q, ok, err)
			}
			if generation == 40 {
				if len(got) != 0 {
					t.Fatalf("the empty generation answered %v", renderHits(got))
				}
				continue
			}
			if want := o.storeRankOwned(t, generation, q, 20); renderHits(got) != renderHits(want) {
				t.Fatalf("gen %d %q:\n memory %s\n fts5   %s", generation, q, renderHits(got), renderHits(want))
			}
		}
	}
	whole := wholeReads(o)
	for _, generation := range []int64{30, 33, 35} {
		if whole[generation] != 1 || matchReads(o, generation) != 0 {
			t.Fatalf("gen %d: %d whole reads and %d MATCH reads, want 1 and 0", generation, whole[generation], matchReads(o, generation))
		}
	}
	if whole[40] != 0 || matchReads(o, 40) != 0 {
		t.Fatalf("the empty generation was read: %d whole, %d MATCH", whole[40], matchReads(o, 40))
	}
}

// The route pre-warm reads the base generations whole before any query, so the
// first search after an edit reads none of them.
func TestWarmGenerationsReadsTheBaseBeforeAnyQuery(t *testing.T) {
	ctx := context.Background()
	o := newBaseFixture(t)
	ranker := search.NewFTSRanker(o)
	read, err := ranker.WarmGenerations(ctx, []int64{0, 30, 40})
	if err != nil || read != 1 {
		t.Fatalf("warm read %d generation(s) (err %v), want 1: the base one", read, err)
	}
	warmed := len(o.walked)
	for _, q := range []string{"newDirtySamplerRen9", "retryableCheckoutRefreshErrorRen10"} {
		got, ok, err := ranker.Rank(ctx, 30, true, q, nil, 20)
		if err != nil || !ok {
			t.Fatalf("%q: ok=%v err=%v", q, ok, err)
		}
		if want := o.storeRankOwned(t, 30, q, 20); renderHits(got) != renderHits(want) {
			t.Fatalf("%q:\n memory %s\n fts5   %s", q, renderHits(got), renderHits(want))
		}
	}
	if len(o.walked) != warmed {
		t.Fatalf("the first searches read the warmed base generation again: %v", o.walked[warmed:])
	}
}
