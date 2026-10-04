package search

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

// ContextBackend is the request-aware form of Backend search. Callers should
// prefer it whenever a request context is available and fall back to Backend
// only while that context is still live.
type ContextBackend interface {
	SearchContext(ctx context.Context, query string, limit int) []SearchResult
}

// ContextSymbolBundleSearcherBackend is the request-aware bundled-search path.
type ContextSymbolBundleSearcherBackend interface {
	SearchSymbolBundlesContext(ctx context.Context, query string, limit int) []SymbolBundle
}

// ScopedContextSymbolBundleSearcherBackend is the request-aware repository-
// scoped bundled-search path.
type ScopedContextSymbolBundleSearcherBackend interface {
	SearchSymbolBundlesScopedContext(ctx context.Context, query string, repoAllow []string, limit int) []SymbolBundle
}

// PathScopedContextSymbolBundleSearcherBackend applies normalized repo-relative
// path prefixes before the text limit and node/edge bundle hydration. handled=false means
// unsupported; handled=true preserves authoritative empty or failed answers.
type PathScopedContextSymbolBundleSearcherBackend interface {
	SearchSymbolBundlesPathScopedContext(context.Context, string, []string, []string, int) ([]SymbolBundle, bool)
}

// ContextChannelSearcher is the request-aware text/vector channel path.
type ContextChannelSearcher interface {
	SearchChannelsContext(ctx context.Context, query string, limit int) (textResults []SearchResult, vectorIDs []string)
}

// ContextTimedChannelSearcher adds the existing per-channel timings to a
// request-aware channel search.
type ContextTimedChannelSearcher interface {
	SearchChannelsTimedContext(ctx context.Context, query string, limit int) ([]SearchResult, []string, ChannelTimings)
}

// ContextVectorChannelOnly is the request-aware vector-only channel path.
type ContextVectorChannelOnly interface {
	VectorChannelOnlyContext(ctx context.Context, query string, limit int) ([]string, ChannelTimings)
}

// ContextViewBatchSearcherBackend searches several pinned view backends in one
// request. Results are position-aligned with peers. handled=false means the
// peer set is unsupported or spans backend families; cancellation and runtime
// failure are authoritative handled=true empty answers and must not trigger a
// sequential retry.
type ContextViewBatchSearcherBackend interface {
	SearchViewBatchContext(ctx context.Context, query string, peers []Backend, limit int) (results [][]SearchResult, handled bool)
}

type contextSymbolSearcher interface {
	SearchSymbolsContext(ctx context.Context, query string, limit int) ([]graph.SymbolHit, error)
}

type contextSymbolBundleSearcher interface {
	SearchSymbolBundlesContext(ctx context.Context, query string, limit int) ([]graph.SymbolBundle, error)
}

type scopedContextSymbolBundleSearcher interface {
	SearchSymbolBundlesRepoScopedContext(ctx context.Context, query string, repoAllow []string, limit int) ([]graph.SymbolBundle, error)
}

type pathScopedContextSymbolBundleSearcher interface {
	SearchSymbolBundlesPathScopedContext(context.Context, string, []string, []string, int) ([]graph.SymbolBundle, error)
}

// contextViewGenerationSymbolSearcher is the Store-side batch contract used by
// SymbolSearcherBackend. Identity checks stay inside the Store so derived
// handles can compare their shared storeCore pointer without exposing it or
// substituting a database path string.
type contextViewGenerationSymbolSearcher interface {
	SearchSymbolsViewGenerationsRepoScopedContext(ctx context.Context, query string, repoPrefixes []string, viewGens []int64, limit int) (map[int64][]graph.SymbolHit, error)
	SymbolSearchViewGeneration() int64
	SharesSymbolSearchCore(other any) bool
}

func liveSearchContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func symbolHitsToSearchResults(hits []graph.SymbolHit) []SearchResult {
	if len(hits) == 0 {
		return nil
	}
	out := make([]SearchResult, len(hits))
	for i, hit := range hits {
		out[i] = SearchResult{ID: hit.NodeID, Score: hit.Score}
	}
	return out
}

func emptyViewBatch(peers []Backend) [][]SearchResult {
	return make([][]SearchResult, len(peers))
}

// SearchViewBatchContext unwraps a same-family set of pinned Store adapters,
// issues one Store batch, and maps each generation back to its peer position.
// Duplicate generations receive independent slices so later composition or
// refill cannot mutate another position through shared storage.
func (b *SymbolSearcherBackend) SearchViewBatchContext(ctx context.Context, query string, peers []Backend, limit int) ([][]SearchResult, bool) {
	ctx = liveSearchContext(ctx)
	if b == nil || b.s == nil || len(peers) == 0 {
		return nil, false
	}
	first, ok := peers[0].(*SymbolSearcherBackend)
	if !ok || first != b {
		return nil, false
	}
	batcher, ok := b.s.(contextViewGenerationSymbolSearcher)
	if !ok {
		return nil, false
	}
	viewGens := make([]int64, len(peers))
	for i, peer := range peers {
		adapter, ok := peer.(*SymbolSearcherBackend)
		if !ok || adapter == nil || adapter.s == nil {
			return nil, false
		}
		view, ok := adapter.s.(contextViewGenerationSymbolSearcher)
		if !ok || !batcher.SharesSymbolSearchCore(adapter.s) {
			return nil, false
		}
		viewGens[i] = view.SymbolSearchViewGeneration()
	}
	empty := emptyViewBatch(peers)
	if strings.TrimSpace(query) == "" || limit <= 0 || ctx.Err() != nil {
		return empty, true
	}
	hitsByGeneration, err := searchViewGenerations(ctx, batcher, query, viewGens, limit)
	if err != nil || ctx.Err() != nil {
		return empty, true
	}
	out := make([][]SearchResult, len(peers))
	for i, viewGen := range viewGens {
		out[i] = append([]SearchResult(nil), symbolHitsToSearchResults(hitsByGeneration[viewGen])...)
	}
	return out, true
}

// SearchContext forwards a live request context to a capable graph searcher.
// A canceled request never enters the legacy background-context fallback.
func (b *SymbolSearcherBackend) SearchContext(ctx context.Context, query string, limit int) []SearchResult {
	ctx = liveSearchContext(ctx)
	if b == nil || b.s == nil || strings.TrimSpace(query) == "" || ctx.Err() != nil {
		return nil
	}
	var (
		hits []graph.SymbolHit
		err  error
	)
	if searcher, ok := b.s.(contextSymbolSearcher); ok {
		hits, err = searcher.SearchSymbolsContext(ctx, query, limit)
	} else {
		hits, err = b.s.SearchSymbols(query, limit)
	}
	if err != nil || ctx.Err() != nil {
		return nil
	}
	return symbolHitsToSearchResults(hits)
}

// SearchSymbolBundlesContext forwards a live request context through the
// bundled graph-search capability. Context-aware errors are authoritative and
// never trigger a second legacy query here.
func (b *SymbolSearcherBackend) SearchSymbolBundlesContext(ctx context.Context, query string, limit int) []SymbolBundle {
	ctx = liveSearchContext(ctx)
	if b == nil || b.s == nil || strings.TrimSpace(query) == "" || ctx.Err() != nil {
		return nil
	}
	if searcher, ok := b.s.(contextSymbolBundleSearcher); ok {
		bundles, err := searcher.SearchSymbolBundlesContext(ctx, query, limit)
		if err != nil || ctx.Err() != nil {
			return nil
		}
		return bundles
	}
	return b.SearchSymbolBundles(query, limit)
}

// SearchSymbolBundlesScopedContext forwards the repository scope inside the
// context-aware store query. A non-nil empty slice means the scoped path
// answered successfully with no matches, matching the legacy adapter.
func (b *SymbolSearcherBackend) SearchSymbolBundlesScopedContext(ctx context.Context, query string, repoAllow []string, limit int) []SymbolBundle {
	ctx = liveSearchContext(ctx)
	if b == nil || b.s == nil || strings.TrimSpace(query) == "" || ctx.Err() != nil {
		return nil
	}
	if searcher, ok := b.s.(scopedContextSymbolBundleSearcher); ok {
		bundles, err := searcher.SearchSymbolBundlesRepoScopedContext(ctx, query, repoAllow, limit)
		if err != nil || ctx.Err() != nil {
			return nil
		}
		if bundles == nil {
			return []SymbolBundle{}
		}
		return bundles
	}
	return b.SearchSymbolBundlesScoped(query, repoAllow, limit)
}

// SearchContext preserves HybridBackend's adaptive text/vector fusion while
// threading the caller's context through both channels.
func (h *HybridBackend) SearchContext(ctx context.Context, query string, limit int) []SearchResult {
	ctx = liveSearchContext(ctx)
	if ctx.Err() != nil {
		return nil
	}
	textResults, vectorIDs, _ := h.searchChannelsContext(ctx, query, limit)
	if ctx.Err() != nil {
		return nil
	}
	if len(vectorIDs) == 0 {
		if len(textResults) > limit {
			return textResults[:limit]
		}
		return textResults
	}
	return alphaFuse(textResults, vectorIDs, rerank.AlphaFor(query), h.k, limit)
}

// SearchChannelsContext is the context-aware raw channel path.
func (h *HybridBackend) SearchChannelsContext(ctx context.Context, query string, limit int) ([]SearchResult, []string) {
	text, vector, _ := h.searchChannelsContext(ctx, query, limit)
	return text, vector
}

// SearchChannelsTimedContext is SearchChannelsContext with phase timings.
func (h *HybridBackend) SearchChannelsTimedContext(ctx context.Context, query string, limit int) ([]SearchResult, []string, ChannelTimings) {
	return h.searchChannelsContext(ctx, query, limit)
}

func (h *HybridBackend) searchChannelsContext(ctx context.Context, query string, limit int) ([]SearchResult, []string, ChannelTimings) {
	var stats ChannelTimings
	ctx = liveSearchContext(ctx)
	if h == nil || h.text == nil || ctx.Err() != nil {
		return nil, nil, stats
	}
	textStart := time.Now()
	var textResults []SearchResult
	if searcher, ok := h.text.(ContextBackend); ok {
		textResults = searcher.SearchContext(ctx, query, limit*2)
	} else {
		textResults = h.text.Search(query, limit*2)
	}
	stats.TextMS = time.Since(textStart).Milliseconds()
	if ctx.Err() != nil {
		return nil, nil, stats
	}
	vectorIDs, vectorStats := h.VectorChannelOnlyContext(ctx, query, limit)
	stats.EmbedMS = vectorStats.EmbedMS
	stats.VectorSearchMS = vectorStats.VectorSearchMS
	if ctx.Err() != nil {
		return nil, nil, stats
	}
	return textResults, vectorIDs, stats
}

// VectorChannelOnlyContext derives the existing five-second embedding budget
// from the request context. The ANN implementation itself has no context API,
// so cancellation is checked immediately before and after that bounded call.
func (h *HybridBackend) VectorChannelOnlyContext(ctx context.Context, query string, limit int) ([]string, ChannelTimings) {
	var stats ChannelTimings
	ctx = liveSearchContext(ctx)
	if h == nil || h.vector == nil || h.vector.Count() == 0 || ctx.Err() != nil {
		return nil, stats
	}
	embedCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	embedStart := time.Now()
	queryVector, err := h.embedder.Embed(embedCtx, query)
	stats.EmbedMS = time.Since(embedStart).Milliseconds()
	if err != nil || queryVector == nil || ctx.Err() != nil {
		return nil, stats
	}
	fetch := limit * 2
	if h.vector.HasChunks() {
		fetch = limit * 8
	}
	vectorStart := time.Now()
	rawIDs := h.vector.Search(queryVector, fetch)
	stats.VectorSearchMS = time.Since(vectorStart).Milliseconds()
	if ctx.Err() != nil {
		return nil, stats
	}
	return h.dechunkVectorIDs(rawIDs, limit*2), stats
}

// SearchSymbolBundlesContext forwards the context-aware text bundle path; the
// vector channel remains independently available through VectorChannelOnlyContext.
func (h *HybridBackend) SearchSymbolBundlesContext(ctx context.Context, query string, limit int) []SymbolBundle {
	ctx = liveSearchContext(ctx)
	if h == nil || h.text == nil || ctx.Err() != nil {
		return nil
	}
	if searcher, ok := h.text.(ContextSymbolBundleSearcherBackend); ok {
		return searcher.SearchSymbolBundlesContext(ctx, query, limit)
	}
	return h.SearchSymbolBundles(query, limit)
}

// SearchSymbolBundlesScopedContext forwards the context-aware scoped text
// bundle path without involving the vector channel.
func (h *HybridBackend) SearchSymbolBundlesScopedContext(ctx context.Context, query string, repoAllow []string, limit int) []SymbolBundle {
	ctx = liveSearchContext(ctx)
	if h == nil || h.text == nil || ctx.Err() != nil {
		return nil
	}
	if searcher, ok := h.text.(ScopedContextSymbolBundleSearcherBackend); ok {
		return searcher.SearchSymbolBundlesScopedContext(ctx, query, repoAllow, limit)
	}
	return h.SearchSymbolBundlesScoped(query, repoAllow, limit)
}

// SearchContext pins the active backend for the complete request-aware call.
func (s *Swappable) SearchContext(ctx context.Context, query string, limit int) []SearchResult {
	ctx = liveSearchContext(ctx)
	if ctx.Err() != nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if searcher, ok := s.inner.(ContextBackend); ok {
		return searcher.SearchContext(ctx, query, limit)
	}
	if ctx.Err() != nil {
		return nil
	}
	return s.inner.Search(query, limit)
}

// SearchChannelsContext pins and forwards the context-aware channel path.
func (s *Swappable) SearchChannelsContext(ctx context.Context, query string, limit int) ([]SearchResult, []string) {
	ctx = liveSearchContext(ctx)
	if ctx.Err() != nil {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if searcher, ok := s.inner.(ContextChannelSearcher); ok {
		return searcher.SearchChannelsContext(ctx, query, limit)
	}
	if ctx.Err() != nil {
		return nil, nil
	}
	if searcher, ok := s.inner.(ChannelSearcher); ok {
		return searcher.SearchChannels(query, limit)
	}
	return s.inner.Search(query, limit), nil
}

// SearchChannelsTimedContext pins and forwards the timed context-aware path.
func (s *Swappable) SearchChannelsTimedContext(ctx context.Context, query string, limit int) ([]SearchResult, []string, ChannelTimings) {
	ctx = liveSearchContext(ctx)
	if ctx.Err() != nil {
		return nil, nil, ChannelTimings{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if searcher, ok := s.inner.(ContextTimedChannelSearcher); ok {
		return searcher.SearchChannelsTimedContext(ctx, query, limit)
	}
	if ctx.Err() != nil {
		return nil, nil, ChannelTimings{}
	}
	if searcher, ok := s.inner.(ContextChannelSearcher); ok {
		text, vector := searcher.SearchChannelsContext(ctx, query, limit)
		return text, vector, ChannelTimings{}
	}
	type timer interface {
		SearchChannelsTimed(query string, limit int) ([]SearchResult, []string, ChannelTimings)
	}
	if searcher, ok := s.inner.(timer); ok {
		return searcher.SearchChannelsTimed(query, limit)
	}
	if searcher, ok := s.inner.(ChannelSearcher); ok {
		text, vector := searcher.SearchChannels(query, limit)
		return text, vector, ChannelTimings{}
	}
	return s.inner.Search(query, limit), nil, ChannelTimings{}
}

// SearchSymbolBundlesContext pins and forwards the context-aware bundle path.
func (s *Swappable) SearchSymbolBundlesContext(ctx context.Context, query string, limit int) []SymbolBundle {
	ctx = liveSearchContext(ctx)
	if ctx.Err() != nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if ctx.Err() != nil {
		return nil
	}
	if searcher, ok := s.inner.(ContextSymbolBundleSearcherBackend); ok {
		return searcher.SearchSymbolBundlesContext(ctx, query, limit)
	}
	if searcher, ok := s.inner.(SymbolBundleSearcherBackend); ok {
		return searcher.SearchSymbolBundles(query, limit)
	}
	return nil
}

// SearchSymbolBundlesScopedContext pins and forwards the scoped context-aware
// bundle path.
func (s *Swappable) SearchSymbolBundlesScopedContext(ctx context.Context, query string, repoAllow []string, limit int) []SymbolBundle {
	ctx = liveSearchContext(ctx)
	if ctx.Err() != nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if ctx.Err() != nil {
		return nil
	}
	if searcher, ok := s.inner.(ScopedContextSymbolBundleSearcherBackend); ok {
		return searcher.SearchSymbolBundlesScopedContext(ctx, query, repoAllow, limit)
	}
	if searcher, ok := s.inner.(ScopedSymbolBundleSearcherBackend); ok {
		return searcher.SearchSymbolBundlesScoped(query, repoAllow, limit)
	}
	return nil
}

// VectorChannelOnlyContext pins and forwards the request-aware vector path.
func (s *Swappable) VectorChannelOnlyContext(ctx context.Context, query string, limit int) ([]string, ChannelTimings) {
	ctx = liveSearchContext(ctx)
	if ctx.Err() != nil {
		return nil, ChannelTimings{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if ctx.Err() != nil {
		return nil, ChannelTimings{}
	}
	if searcher, ok := s.inner.(ContextVectorChannelOnly); ok {
		return searcher.VectorChannelOnlyContext(ctx, query, limit)
	}
	if searcher, ok := s.inner.(interface {
		VectorChannelOnly(query string, limit int) ([]string, ChannelTimings)
	}); ok {
		return searcher.VectorChannelOnly(query, limit)
	}
	return nil, ChannelTimings{}
}

var (
	_ ContextBackend                           = (*SymbolSearcherBackend)(nil)
	_ ContextSymbolBundleSearcherBackend       = (*SymbolSearcherBackend)(nil)
	_ ScopedContextSymbolBundleSearcherBackend = (*SymbolSearcherBackend)(nil)
	_ ContextViewBatchSearcherBackend          = (*SymbolSearcherBackend)(nil)
	_ ContextBackend                           = (*HybridBackend)(nil)
	_ ContextChannelSearcher                   = (*HybridBackend)(nil)
	_ ContextTimedChannelSearcher              = (*HybridBackend)(nil)
	_ ContextVectorChannelOnly                 = (*HybridBackend)(nil)
	_ ContextSymbolBundleSearcherBackend       = (*HybridBackend)(nil)
	_ ScopedContextSymbolBundleSearcherBackend = (*HybridBackend)(nil)
	_ ContextBackend                           = (*Swappable)(nil)
	_ ContextChannelSearcher                   = (*Swappable)(nil)
	_ ContextTimedChannelSearcher              = (*Swappable)(nil)
	_ ContextVectorChannelOnly                 = (*Swappable)(nil)
	_ ContextSymbolBundleSearcherBackend       = (*Swappable)(nil)
	_ ScopedContextSymbolBundleSearcherBackend = (*Swappable)(nil)
)

// ViewFTSRankSource is a store that splits its per-generation symbol search
// into the exact-name tier and the full-text tier and serves the reads the
// in-memory full-text ranker needs (fts_rank.go). Stores that do not are
// searched as before.
type ViewFTSRankSource interface {
	FTSRankSource
	// SymbolExactHitsViewGenerations answers the exact-name tier for each
	// generation and returns the generations it left for full-text ranking,
	// in request order.
	SymbolExactHitsViewGenerations(ctx context.Context, query string, repoPrefixes []string, viewGens []int64, limit int) (map[int64][]graph.SymbolHit, []int64, error)
}

// ftsRankers keeps one ranker per store core, so an immutable generation's
// match lists outlive a request.
var ftsRankers sync.Map // store core identity (any comparable) -> *FTSRanker

type symbolSearchCoreKeyer interface {
	SymbolSearchCoreKey() any
}

// ftsRankAdapter, when installed (SetFTSRankSourceAdapter), turns a store the
// search package cannot name into a ViewFTSRankSource: the store's package
// imports this one, so the adapter lives with a package that imports both.
var ftsRankAdapter atomic.Pointer[func(searcher any) (ViewFTSRankSource, bool)]

// ftsRankedGenerations counts the generations ranked in memory
// (FTSRankedGenerations).
var ftsRankedGenerations atomic.Int64

// FTSRankedGenerations reports how many generation rankings the in-memory
// ranker has served (diagnostics and tests).
func FTSRankedGenerations() int64 { return ftsRankedGenerations.Load() }

// ftsRankOff routes every view search through the store's own FTS5 query.
// Tests only (DisableFTSRankForTest).
var ftsRankOff atomic.Bool

// SetFTSRankSourceAdapter installs the adapter that lets a store serve the
// in-memory ranker. A nil adapter removes it.
func SetFTSRankSourceAdapter(adapt func(searcher any) (ViewFTSRankSource, bool)) {
	if adapt == nil {
		ftsRankAdapter.Store(nil)
		return
	}
	ftsRankAdapter.Store(&adapt)
}

// DisableFTSRankForTest makes every view search use the store's FTS5 query
// until restore runs, so a test can compare the two paths. Tests only.
func DisableFTSRankForTest() (restore func()) {
	ftsRankOff.Store(true)
	return func() { ftsRankOff.Store(false) }
}

// ftsRankSourceOf returns searcher as a ranker source: directly, or through
// the installed adapter.
func ftsRankSourceOf(searcher any) (ViewFTSRankSource, bool) {
	if ftsRankOff.Load() {
		return nil, false
	}
	if src, ok := searcher.(ViewFTSRankSource); ok {
		return src, true
	}
	if adapt := ftsRankAdapter.Load(); adapt != nil {
		return (*adapt)(searcher)
	}
	return nil, false
}

func ftsRankerFor(src ViewFTSRankSource) *FTSRanker {
	keyer, ok := src.(symbolSearchCoreKeyer)
	if !ok {
		return nil
	}
	key := keyer.SymbolSearchCoreKey()
	if r, ok := ftsRankers.Load(key); ok {
		return r.(*FTSRanker)
	}
	r, _ := ftsRankers.LoadOrStore(key, NewFTSRanker(src))
	return r.(*FTSRanker)
}

// searchViewGenerations ranks the view's generations. On a store that serves
// the ranker's reads, the exact-name tier is the store's; each remaining
// generation is ranked in memory — its match list kept across publications,
// only the table statistics re-read — and the store's FTS5 query answers only
// the generations the ranker declines. The per-generation hits are the
// store's, score bits and order included (TestFTSRankerRanksLikeFTS5), so the
// composition downstream is unchanged.
func searchViewGenerations(ctx context.Context, batcher contextViewGenerationSymbolSearcher, query string, viewGens []int64, limit int) (map[int64][]graph.SymbolHit, error) {
	src, ok := ftsRankSourceOf(batcher)
	if !ok {
		return batcher.SearchSymbolsViewGenerationsRepoScopedContext(ctx, query, nil, viewGens, limit)
	}
	ranker := ftsRankerFor(src)
	if ranker == nil {
		return batcher.SearchSymbolsViewGenerationsRepoScopedContext(ctx, query, nil, viewGens, limit)
	}
	hits, rest, err := src.SymbolExactHitsViewGenerations(ctx, query, nil, viewGens, limit)
	if err != nil {
		return nil, err
	}
	if hits == nil {
		hits = make(map[int64][]graph.SymbolHit, len(viewGens))
	}
	ranked, declined, err := ranker.RankGenerations(ctx, rest, query, nil, limit)
	if err != nil {
		return nil, err
	}
	for generation, generationHits := range ranked {
		ftsRankedGenerations.Add(1)
		hits[generation] = generationHits
	}
	if len(declined) > 0 {
		fallback, err := batcher.SearchSymbolsViewGenerationsRepoScopedContext(ctx, query, nil, declined, limit)
		if err != nil {
			return nil, err
		}
		for _, generation := range declined {
			hits[generation] = fallback[generation]
		}
	}
	for _, generation := range viewGens {
		if _, ok := hits[generation]; !ok {
			hits[generation] = nil
		}
	}
	return hits, nil
}

func (b *SymbolSearcherBackend) SearchSymbolBundlesPathScopedContext(ctx context.Context, query string, repos, paths []string, limit int) ([]SymbolBundle, bool) {
	ctx = liveSearchContext(ctx)
	if b == nil || b.s == nil || ctx.Err() != nil {
		return nil, true
	}
	source, ok := b.s.(pathScopedContextSymbolBundleSearcher)
	if !ok {
		return nil, false
	}
	bundles, err := source.SearchSymbolBundlesPathScopedContext(ctx, query, repos, paths, limit)
	if err != nil || ctx.Err() != nil {
		return nil, true
	}
	if bundles == nil {
		return []SymbolBundle{}, true
	}
	return bundles, true
}

func (h *HybridBackend) SearchSymbolBundlesPathScopedContext(ctx context.Context, query string, repos, paths []string, limit int) ([]SymbolBundle, bool) {
	ctx = liveSearchContext(ctx)
	if h == nil || h.text == nil || ctx.Err() != nil {
		return nil, true
	}
	if source, ok := h.text.(PathScopedContextSymbolBundleSearcherBackend); ok {
		return source.SearchSymbolBundlesPathScopedContext(ctx, query, repos, paths, limit)
	}
	return nil, false
}

func (s *Swappable) SearchSymbolBundlesPathScopedContext(ctx context.Context, query string, repos, paths []string, limit int) ([]SymbolBundle, bool) {
	ctx = liveSearchContext(ctx)
	if ctx.Err() != nil {
		return nil, true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if ctx.Err() != nil {
		return nil, true
	}
	if source, ok := s.inner.(PathScopedContextSymbolBundleSearcherBackend); ok {
		return source.SearchSymbolBundlesPathScopedContext(ctx, query, repos, paths, limit)
	}
	return nil, false
}
