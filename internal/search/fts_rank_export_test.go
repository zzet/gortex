package search

// SearchViewGenerationsForTest exposes searchViewGenerations to the external
// ranker tests.
var SearchViewGenerationsForTest = searchViewGenerations

// ResetFTSRankersForTest drops the per-core rankers.
func ResetFTSRankersForTest() {
	ftsRankers.Range(func(k, _ any) bool { ftsRankers.Delete(k); return true })
}

// SetFTSViewMatchSelectiveHitsForTest sets the selective mode's bound until
// restore runs.
func SetFTSViewMatchSelectiveHitsForTest(n int64) (restore func()) {
	previous := ftsViewMatchSelectiveHits
	ftsViewMatchSelectiveHits = n
	return func() { ftsViewMatchSelectiveHits = previous }
}
