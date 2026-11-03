package search

import (
	"fmt"
	"strings"
	"testing"
)

// Keep both the normalized text written by the store and mixed-case input:
// ftsTokensOf also serves uncached MATCH rows and cannot assume normalization.
func BenchmarkFTSTokensOf(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"Normalized", "internal indexer checkout17 coordinator go flip738 checkout coordinator flip route generations context error func ctx store sqlite view generation publication applied ownership masks commit dirty"},
		{"MixedCase", "internal/Indexer/Checkout17.go::Coordinator.Flip738 func(ctx Context) Error; Store SQLite View Generation Publication Applied Ownership Masks Commit Dirty"},
		{"LongToken", strings.Repeat("a", 1024)},
		{"Punctuation", strings.Repeat(" :/_-.\t", 64)},
		{"NonASCII", "internal indexer coordinator café"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(tc.text)))
			for b.Loop() {
				ftsTokensOf(tc.text)
			}
		})
	}
}

// Tokenize every document once, as generationDocsFor does when it warms a
// workspace. This includes slice storage and all tokens, not just a query.
func BenchmarkFTSTokenizeWorkspace(b *testing.B) {
	for _, repos := range []int{1, 8} {
		b.Run(fmt.Sprintf("repos%d", repos), func(b *testing.B) {
			const perRepo = 10_000
			rows := make([]FTSRankRow, repos*perRepo)
			for i := range rows {
				rows[i] = FTSRankRow{
					Rowid:      int64(i + 1),
					NodeID:     fmt.Sprintf("repo%d/internal/service/file%d.go::Process%d", i/perRepo, i/40, i),
					RepoPrefix: fmt.Sprintf("repo%d", i/perRepo),
					Tokens:     fmt.Sprintf("internal service file%d go process%d func ctx context request response error store sqlite view generation publication applied ownership masks commit dirty", i/40, i),
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				tokens := make([][]string, len(rows))
				for i := range rows {
					tokens[i], _ = ftsTokensOf(rows[i].Tokens)
				}
				if len(tokens[len(tokens)-1]) != 21 {
					b.Fatal("wrong token count")
				}
			}
			b.ReportMetric(float64(len(rows)), "docs/op")
		})
	}
}
