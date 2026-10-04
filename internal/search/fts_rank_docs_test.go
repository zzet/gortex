package search_test

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/search"
)

// sizedFTSTable serves the ranker's document counts and records the match of
// every generation read. A generation listed in large reports a count over any
// whole-read bound, so it keeps the MATCH read.
type sizedFTSTable struct {
	*ftsTable
	large   map[int64]bool
	matched map[int64]int // generation -> reads with a MATCH
	whole   map[int64]int // generation -> reads with no MATCH
}

func (s *sizedFTSTable) SymbolFTSGenerationRun(_ context.Context, generation int64) (search.FTSGenerationRun, error) {
	span := s.spans[generation]
	if s.large[generation] {
		return search.FTSGenerationRun{Docs: 1 << 40, Lo: span[0], Hi: span[1], Known: true, DocsKnown: true}, nil
	}
	return search.FTSGenerationRun{Docs: span[1] - span[0] + 1, Lo: span[0], Hi: span[1], Known: true, DocsKnown: true}, nil
}

func (s *sizedFTSTable) SymbolFTSGenerationRows(ctx context.Context, generation int64, match string, repoAllow []string) ([]search.FTSRankRow, error) {
	if match == "" {
		s.whole[generation]++
	} else {
		s.matched[generation]++
	}
	return s.ftsTable.SymbolFTSGenerationRows(ctx, generation, match, repoAllow)
}

// A small immutable generation is read whole once and every later query —
// one naming an identifier no kept match list holds included — is matched in
// memory with no further read, ranking exactly as FTS5 does. A large
// generation keeps the per-query MATCH read, and a generation holding a row
// the ranker cannot tokenize keeps it too.
func TestSmallGenerationsAreReadWholeOnceAndRankLikeFTS5(t *testing.T) {
	ctx := context.Background()
	f := &sizedFTSTable{ftsTable: newFTSTable(t), large: map[int64]bool{10: true}, matched: map[int64]int{}, whole: map[int64]int{}}
	f.addGeneration(t, 10, fixtureRows("root", 400, 1))
	f.addGeneration(t, 11, fixtureRows("commit", 60, 4))
	f.addGeneration(t, 12, fixtureRows("dirty", 25, 9))
	f.addGeneration(t, 14, append(fixtureRows("mixed", 20, 3), [3]string{"mixed/accent", "", "café config"}))
	ranker := search.NewFTSRanker(f)
	compare := func(queries []string) {
		for _, generation := range []int64{10, 11, 12} {
			for _, q := range queries {
				for _, repo := range [][]string{nil, {"alpha"}} {
					for _, limit := range []int{1, 7, 200} {
						want := f.storeRank(t, generation, q, repo, limit)
						got, ok, err := ranker.Rank(ctx, generation, true, q, repo, limit)
						if err != nil || !ok {
							t.Fatalf("gen %d %q: ok=%v err=%v", generation, q, ok, err)
						}
						if renderHits(got) != renderHits(want) {
							t.Fatalf("gen %d %q repos %v limit %d:\n memory %s\n fts5   %s", generation, q, repo, limit, renderHits(got), renderHits(want))
						}
					}
				}
			}
		}
	}
	compare([]string{"config", "checkout route", "load config loader", "co", "dirty sampler refresh"})
	for _, generation := range []int64{11, 12} {
		if f.whole[generation] != 1 || f.matched[generation] != 0 {
			t.Fatalf("gen %d: %d whole reads and %d MATCH reads, want 1 and 0", generation, f.whole[generation], f.matched[generation])
		}
	}
	if f.whole[10] != 0 || f.matched[10] == 0 {
		t.Fatalf("the large generation: %d whole reads and %d MATCH reads, want 0 and some", f.whole[10], f.matched[10])
	}

	// New identifiers, as after an edit: the small generations are not read
	// again.
	compare([]string{"matchesSkipRuleRen5", "flip coordinator", "rule"})
	for _, generation := range []int64{11, 12} {
		if f.whole[generation] != 1 || f.matched[generation] != 0 {
			t.Fatalf("gen %d after new queries: %d whole reads and %d MATCH reads, want 1 and 0", generation, f.whole[generation], f.matched[generation])
		}
	}

	// A row the ranker cannot tokenize: the generation keeps the MATCH read
	// (which declines here, the non-ASCII row matching), and the decline is
	// kept rather than read again.
	for i := 0; i < 2; i++ {
		if _, ok, err := ranker.Rank(ctx, 14, true, "config", nil, 10); ok || err != nil {
			t.Fatalf("a generation holding a non-ASCII row was ranked in memory (ok=%v err=%v)", ok, err)
		}
	}
	if f.whole[14] != 1 || f.matched[14] != 1 {
		t.Fatalf("gen 14: %d whole reads and %d MATCH reads, want 1 and 1", f.whole[14], f.matched[14])
	}
}
