package search

import (
	"context"
	"strings"
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

type contextSymbolSearcher interface {
	SearchSymbolsContext(ctx context.Context, query string, limit int) ([]graph.SymbolHit, error)
}

type contextSymbolBundleSearcher interface {
	SearchSymbolBundlesContext(ctx context.Context, query string, limit int) ([]graph.SymbolBundle, error)
}

type scopedContextSymbolBundleSearcher interface {
	SearchSymbolBundlesRepoScopedContext(ctx context.Context, query string, repoAllow []string, limit int) ([]graph.SymbolBundle, error)
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
