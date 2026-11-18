package search

import "context"

// SymbolBundleTimings describes one persisted bundle call. Milliseconds retain
// sub-millisecond precision; row counts cover hydration misses, not cached rows.
type SymbolBundleTimings struct {
	Calls                                         int
	RankMS, NodeMS, OutMS, InMS                   float64
	RankedHits, UniqueIDs, CacheHits, CacheMisses int
	NodeRows, OutRows, InRows                     int
}

func (t *SymbolBundleTimings) Add(other SymbolBundleTimings) {
	t.Calls += other.Calls
	t.RankMS += other.RankMS
	t.NodeMS += other.NodeMS
	t.OutMS += other.OutMS
	t.InMS += other.InMS
	t.RankedHits += other.RankedHits
	t.UniqueIDs += other.UniqueIDs
	t.CacheHits += other.CacheHits
	t.CacheMisses += other.CacheMisses
	t.NodeRows += other.NodeRows
	t.OutRows += other.OutRows
	t.InRows += other.InRows
}

type symbolBundleTimingsKey struct{}

// WithSymbolBundleTimingsObserver binds diagnostics to this synchronous call's
// existing context chain, without changing any backend interface or query.
func WithSymbolBundleTimingsObserver(ctx context.Context, observe func(SymbolBundleTimings)) context.Context {
	return context.WithValue(ctx, symbolBundleTimingsKey{}, observe)
}

func SymbolBundleTimingsObserver(ctx context.Context) func(SymbolBundleTimings) {
	if ctx == nil {
		return nil
	}
	observe, _ := ctx.Value(symbolBundleTimingsKey{}).(func(SymbolBundleTimings))
	return observe
}
