package search_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

// tokenizingTable is the store double with the store's FTS5 tokenizer.
type tokenizingTable struct{ *ownedFTSTable }

func (tt tokenizingTable) SymbolFTSTokenize(ctx context.Context, texts []string) ([][]string, error) {
	return store_sqlite.SymbolFTSTokenize(ctx, texts)
}

// nonASCIIRows mixes non-ASCII identifiers, comments and strings with ASCII
// rows, as a Go corpus has them.
func nonASCIIRows() [][3]string {
	special := []string{
		"Überprüfung ueberpruefung check config",
		"café latte route handler",
		"naïve résumé flip coordinator",
		"日本語 テスト config loader",
		"straße strasse load config",
		"— “quoted” checkout route",
		"Ωmega omega dirty sampler",
		"ÉCOLE école sampler refresh",
		"Grüße grusse matches rule",
		"año nino niño config",
	}
	var out [][3]string
	ascii := fixtureRows("plain", 60, 4)
	for i := range ascii {
		out = append(out, ascii[i])
		if i < len(special) {
			out = append(out, [3]string{fmt.Sprintf("u%d", i), []string{"alpha", "beta", ""}[i%3], special[i]})
		}
	}
	return out
}

// A generation holding non-ASCII rows is kept: those rows get FTS5's own
// terms (the store's tokenizer), so rankings and score bits equal FTS5's for
// queries that reach them through case folding and diacritic removal. Without
// the tokenizer the generation is refused, and the record says why.
func TestGenerationsWithNonASCIIRowsAreKeptAndRankLikeFTS5(t *testing.T) {
	ctx := context.Background()
	o := newOwnedFTSTable(t)
	for _, row := range nonASCIIRows() {
		o.insert(t, 70, row)
	}
	queries := []string{"cafe", "naive", "resume", "config", "strasse", "ecole", "checkout route", "omega", "uberprufung", "quoted", "grusse", "nino", "sampler refresh", "latte"}
	ranker := search.NewFTSRanker(tokenizingTable{o})
	before, _ := search.FTSNonASCIIRows()
	for _, q := range queries {
		for _, limit := range []int{1, 5, 50} {
			got, ok, err := ranker.Rank(ctx, 70, true, q, nil, limit)
			if err != nil || !ok {
				t.Fatalf("%q: ok=%v err=%v", q, ok, err)
			}
			if want := o.storeRankOwned(t, 70, q, limit); renderHits(got) != renderHits(want) {
				t.Fatalf("%q limit %d:\n memory %s\n fts5   %s", q, limit, renderHits(got), renderHits(want))
			}
		}
	}
	if after, _ := search.FTSNonASCIIRows(); after-before != 10 {
		t.Fatalf("%d rows tokenized by FTS5, want the 10 non-ASCII ones", after-before)
	}
	if whole := wholeReads(o); whole[70] != 1 || matchReads(o, 70) != 0 {
		t.Fatalf("gen 70: %d whole reads and %d MATCH reads, want 1 and 0 (kept)", whole[70], matchReads(o, 70))
	}

	// No tokenizer: refused, with the reason on the record.
	plain := search.NewFTSRanker(o)
	if _, err := plain.WarmGenerations(ctx, []int64{70}); err != nil {
		t.Fatal(err)
	}
	var reason string
	var nonASCII int
	for _, d := range search.FTSWholeReadDecisions() {
		if d.Generation == 70 && d.Refused != "" {
			reason, nonASCII = d.Refused, d.NonASCII
		}
	}
	if reason != "non_ascii" || nonASCII != 10 {
		t.Fatalf("refusal record: reason %q, non-ASCII rows %d; want non_ascii and 10", reason, nonASCII)
	}
}
