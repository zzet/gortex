package store_sqlite

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// FTS5's own terms: case folded, diacritics removed, CJK kept, punctuation
// dropped, in token order.
func TestSymbolFTSTokenizeIsFTS5sOwnTokenizer(t *testing.T) {
	got, err := SymbolFTSTokenize(context.Background(), []string{"Überprüfung café NAÏVE", "日本語 — “quoted”", "plain ascii"})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"uberprufung", "cafe", "naive"}, {"日本語", "quoted"}, {"plain", "ascii"}}, got)
}

// The tokenizer's cost per non-ASCII row.
func BenchmarkSymbolFTSTokenize(b *testing.B) {
	texts := make([]string, 1000)
	for i := range texts {
		texts[i] = fmt.Sprintf("Überprüfung%d café naïve résumé — “quoted” config loader handler route%d", i, i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := SymbolFTSTokenize(context.Background(), texts); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N*len(texts)), "us/row")
}
