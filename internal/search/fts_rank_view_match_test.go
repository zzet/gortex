package search_test

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/search"
)

// viewMatchTable serves one FTS5 MATCH over several generations: the MATCH runs
// once over the whole table and its rows are split by generation span. A
// generation in forceOver is reported over its per-generation bound. Every
// generation is sized over the whole-read bound, so each one needs a MATCH.
type viewMatchTable struct {
	*ftsTable
	forceOver map[int64]bool
	viewReads int
	genReads  map[int64]int // per-generation MATCH reads
	asked     [][]int64     // generations of each one-MATCH read
}

func (v *viewMatchTable) SymbolFTSGenerationRun(context.Context, int64) (search.FTSGenerationRun, error) {
	return search.FTSGenerationRun{}, nil // no run known: every generation needs a MATCH
}

func (v *viewMatchTable) SymbolFTSGenerationRows(ctx context.Context, generation int64, match string, repoAllow []string) ([]search.FTSRankRow, error) {
	v.genReads[generation]++
	return v.ftsTable.SymbolFTSGenerationRows(ctx, generation, match, repoAllow)
}

func (v *viewMatchTable) SymbolFTSViewGenerationRows(ctx context.Context, generations []int64, match string, perGeneration int) (map[int64][]search.FTSRankRow, map[int64]bool, error) {
	v.viewReads++
	v.asked = append(v.asked, append([]int64(nil), generations...))
	rows, err := v.db.QueryContext(ctx, `SELECT f.rowid, f.node_id, f.repo_prefix, f.tokens, d.sz FROM symbol_fts f JOIN symbol_fts_docsize d ON d.id = f.rowid WHERE symbol_fts MATCH ? ORDER BY f.rowid`, match)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := make(map[int64][]search.FTSRankRow, len(generations))
	over := make(map[int64]bool)
	for rows.Next() {
		var r search.FTSRankRow
		var sz []byte
		if err := rows.Scan(&r.Rowid, &r.NodeID, &r.RepoPrefix, &r.Tokens, &sz); err != nil {
			return nil, nil, err
		}
		for off := 0; off < len(sz); {
			n, m := varint(sz[off:])
			r.TokenCount += int(n)
			off += m
		}
		for _, generation := range generations {
			span := v.spans[generation]
			if r.Rowid < span[0] || r.Rowid > span[1] {
				continue
			}
			out[generation] = append(out[generation], r)
			if len(out[generation]) > perGeneration || v.forceOver[generation] {
				over[generation] = true
			}
		}
	}
	return out, over, rows.Err()
}

// One MATCH over the view's generations ranks every generation exactly as its
// own FTS5 query does — the split by generation happens after the read — and
// a query pays one read, not one per generation. A generation reported over
// its bound is declined (and the decline kept), the mutable base is read on
// its own every time, and a repeated query reads no immutable generation
// again.
func TestOneMatchOverTheViewRanksEachGenerationLikeFTS5(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(search.SetFTSViewMatchModeForTest(search.FTSViewMatchOn))
	v := &viewMatchTable{ftsTable: newFTSTable(t), forceOver: map[int64]bool{}, genReads: map[int64]int{}}
	v.addGeneration(t, 0, fixtureRows("base", 120, 7))
	v.addGeneration(t, 10, fixtureRows("root", 400, 1))
	v.addGeneration(t, 11, fixtureRows("commit", 60, 4))
	v.addGeneration(t, 12, fixtureRows("dirty", 25, 9))
	v.addGeneration(t, 15, fixtureRows("big", 30, 2))
	v.forceOver[15] = true
	ranker := search.NewFTSRanker(v)
	view := []int64{12, 11, 10, 15}
	queries := []string{"config", "checkout route", "load config loader", "co", "dirty sampler refresh", "matchesSkipRuleRen5", "rule"}
	for _, q := range queries {
		for _, limit := range []int{1, 7, 200} {
			before := v.viewReads
			ranked, declined, err := ranker.RankGenerations(ctx, view, q, nil, limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(declined) != 1 || declined[0] != 15 {
				t.Fatalf("%q: declined %v, want [15]", q, declined)
			}
			for _, generation := range []int64{10, 11, 12} {
				got, ok := ranked[generation]
				if !ok {
					t.Fatalf("%q: generation %d was not ranked", q, generation)
				}
				if want := v.storeRank(t, generation, q, nil, limit); renderHits(got) != renderHits(want) {
					t.Fatalf("%q gen %d limit %d:\n one MATCH %s\n fts5      %s", q, generation, limit, renderHits(got), renderHits(want))
				}
			}
			reads := v.viewReads - before
			if limit == 1 && reads != 1 {
				t.Fatalf("%q: %d one-MATCH reads for a new query, want 1", q, reads)
			}
			if limit != 1 && reads != 0 {
				t.Fatalf("%q: a repeated query read the immutable generations again (%d reads)", q, reads)
			}
		}
	}
	for _, generation := range view {
		if v.genReads[generation] != 0 {
			t.Fatalf("generation %d was read with its own MATCH %d time(s)", generation, v.genReads[generation])
		}
	}

	// The mutable base with the view: it is read with the view's generations
	// that still need a MATCH, and on its own once they are kept.
	for i, q := range []string{"flip coordinator", "flip coordinator"} {
		viewBefore, baseBefore := v.viewReads, v.genReads[0]
		ranked, _, err := ranker.RankGenerations(ctx, []int64{0, 12, 11}, q, nil, 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, generation := range []int64{0, 11, 12} {
			if want := v.storeRank(t, generation, q, nil, 20); renderHits(ranked[generation]) != renderHits(want) {
				t.Fatalf("%q gen %d:\n memory %s\n fts5   %s", q, generation, renderHits(ranked[generation]), renderHits(want))
			}
		}
		switch i {
		case 0:
			if v.viewReads-viewBefore != 1 || v.genReads[0] != baseBefore {
				t.Fatalf("first query: %d one-MATCH reads and %d base reads, want 1 and 0", v.viewReads-viewBefore, v.genReads[0]-baseBefore)
			}
			if last := v.asked[len(v.asked)-1]; len(last) != 3 {
				t.Fatalf("the one-MATCH read asked for %v, want the base and both generations", last)
			}
		case 1:
			if v.viewReads-viewBefore != 0 || v.genReads[0] != baseBefore+1 {
				t.Fatalf("repeat: %d one-MATCH reads and %d base reads, want 0 and 1", v.viewReads-viewBefore, v.genReads[0]-baseBefore)
			}
		}
	}
}

// The one-MATCH read is off unless switched on, and in the selective mode it
// is used only for a query whose phrases match few documents over the whole
// table; every other query keeps the per-generation reads. The ranking is the
// same either way.
func TestOneMatchModesDecideOnlyTheRead(t *testing.T) {
	ctx := context.Background()
	v := &viewMatchTable{ftsTable: newFTSTable(t), forceOver: map[int64]bool{}, genReads: map[int64]int{}}
	v.addGeneration(t, 10, fixtureRows("root", 400, 1))
	v.addGeneration(t, 11, fixtureRows("commit", 60, 4))
	v.addGeneration(t, 12, fixtureRows("dirty", 25, 9))
	view := []int64{12, 11, 10}
	run := func(mode search.FTSViewMatchMode, q string) (viewReads, genReads int) {
		restore := search.SetFTSViewMatchModeForTest(mode)
		defer restore()
		ranker := search.NewFTSRanker(v)
		vb, gb := v.viewReads, v.genReads[10]+v.genReads[11]+v.genReads[12]
		ranked, declined, err := ranker.RankGenerations(ctx, view, q, nil, 20)
		if err != nil || len(declined) != 0 {
			t.Fatalf("%q: declined %v err %v", q, declined, err)
		}
		for _, generation := range view {
			if want := v.storeRank(t, generation, q, nil, 20); renderHits(ranked[generation]) != renderHits(want) {
				t.Fatalf("mode %d %q gen %d:\n memory %s\n fts5   %s", mode, q, generation, renderHits(ranked[generation]), renderHits(want))
			}
		}
		return v.viewReads - vb, v.genReads[10] + v.genReads[11] + v.genReads[12] - gb
	}
	if vr, gr := run(search.FTSViewMatchOff, "config"); vr != 0 || gr != 3 {
		t.Fatalf("off: %d one-MATCH and %d per-generation reads, want 0 and 3", vr, gr)
	}
	if vr, gr := run(search.FTSViewMatchOn, "config"); vr != 1 || gr != 0 {
		t.Fatalf("on: %d one-MATCH and %d per-generation reads, want 1 and 0", vr, gr)
	}
	// "flip" matches few documents, "co" (a prefix of most words) matches most
	// of the table.
	hits, _, err := v.SymbolFTSPrefixHits(ctx, []string{"flip", "co"})
	if err != nil || hits["flip"] >= hits["co"] {
		t.Fatalf("fixture: flip %d, co %d documents (err %v)", hits["flip"], hits["co"], err)
	}
	restoreBound := search.SetFTSViewMatchSelectiveHitsForTest(hits["flip"])
	defer restoreBound()
	if vr, gr := run(search.FTSViewMatchSelective, "flip"); vr != 1 || gr != 0 {
		t.Fatalf("selective, a rare term: %d one-MATCH and %d per-generation reads, want 1 and 0", vr, gr)
	}
	if vr, gr := run(search.FTSViewMatchSelective, "co"); vr != 0 || gr != 3 {
		t.Fatalf("selective, a common prefix: %d one-MATCH and %d per-generation reads, want 0 and 3", vr, gr)
	}
}
