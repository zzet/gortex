package search

import (
	"fmt"
	"strings"
	"testing"
)

// BenchmarkMatchKeptBaseDocuments is one query's in-memory match over kept
// base generations the size of the live store's three (223,720 documents of
// about 20 tokens), with the edit bundle's query shape.
func BenchmarkMatchKeptBaseDocuments(b *testing.B) {
	// A 3,000-word vocabulary: each word is in about 0.7% of the documents,
	// so the query's four phrases match about 3% of them — in line with the
	// window-4 records (29,000-150,000 matching documents over the ~890,000-row
	// table for the bundle's names).
	words := make([]string, 3000)
	for i := range words {
		words[i] = fmt.Sprintf("w%dx", i)
	}
	words[1], words[2], words[3] = "new", "dirty", "sampler"
	const docs = 223_720
	rows := make([]FTSRankRow, docs)
	tokens := make([][]string, docs)
	for i := range rows {
		parts := make([]string, 20)
		for j := range parts {
			parts[j] = words[(i*7919+j*104729)%len(words)]
		}
		parts[0] = fmt.Sprintf("sym%d", i)
		rows[i] = FTSRankRow{Rowid: int64(i + 1), NodeID: fmt.Sprintf("repo/file%d.go::sym%d", i/40, i), Tokens: strings.Join(parts, " ")}
		tokens[i], _ = ftsTokensOf(rows[i].Tokens)
	}
	terms, _ := FTSRankTerms("newDirtySamplerRen9")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := ftsBuildMatchesTokenized(rows, tokens, terms, nil)
		if !m.exact {
			b.Fatal("the match list went over the cap")
		}
		b.ReportMetric(float64(len(m.rows)), "matches")
	}
	b.ReportMetric(float64(ftsDocsBytes(rows, tokens))/1e6, "MB-kept")
}
