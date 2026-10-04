package query

import (
	"context"

	"github.com/zzet/gortex/internal/search"
)

func liveRequestContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func requestTextSearch(ctx context.Context, backend search.Backend, query string, limit int) []search.SearchResult {
	ctx = liveRequestContext(ctx)
	if backend == nil || ctx.Err() != nil {
		return nil
	}
	if contextual, ok := backend.(search.ContextBackend); ok {
		return contextual.SearchContext(ctx, query, limit)
	}
	results := backend.Search(query, limit)
	if ctx.Err() != nil {
		return nil
	}
	return results
}

type requestBundleAnswer struct {
	bundles       []search.SymbolBundle
	supported     bool
	authoritative bool
	failed        bool
}

func requestSymbolBundles(ctx context.Context, backend search.Backend, query string, limit int) requestBundleAnswer {
	ctx = liveRequestContext(ctx)
	if backend == nil || ctx.Err() != nil {
		return requestBundleAnswer{supported: true, authoritative: true, failed: true}
	}
	if contextual, ok := backend.(search.ContextSymbolBundleSearcherBackend); ok {
		bundles := contextual.SearchSymbolBundlesContext(ctx, query, limit)
		if ctx.Err() != nil {
			return requestBundleAnswer{supported: true, authoritative: true, failed: true}
		}
		return requestBundleAnswer{bundles: bundles, supported: true, authoritative: bundles != nil}
	}
	legacy, ok := backend.(search.SymbolBundleSearcherBackend)
	if !ok {
		return requestBundleAnswer{}
	}
	bundles := legacy.SearchSymbolBundles(query, limit)
	if ctx.Err() != nil {
		return requestBundleAnswer{supported: true, authoritative: true, failed: true}
	}
	return requestBundleAnswer{bundles: bundles, supported: true, authoritative: bundles != nil}
}

func requestScopedSymbolBundles(ctx context.Context, backend search.Backend, query string, repoAllow []string, limit int) requestBundleAnswer {
	ctx = liveRequestContext(ctx)
	if backend == nil || ctx.Err() != nil {
		return requestBundleAnswer{supported: true, authoritative: true, failed: true}
	}
	if contextual, ok := backend.(search.ScopedContextSymbolBundleSearcherBackend); ok {
		bundles := contextual.SearchSymbolBundlesScopedContext(ctx, query, repoAllow, limit)
		if ctx.Err() != nil {
			return requestBundleAnswer{supported: true, authoritative: true, failed: true}
		}
		return requestBundleAnswer{bundles: bundles, supported: true, authoritative: bundles != nil}
	}
	legacy, ok := backend.(search.ScopedSymbolBundleSearcherBackend)
	if !ok {
		return requestBundleAnswer{}
	}
	bundles := legacy.SearchSymbolBundlesScoped(query, repoAllow, limit)
	if ctx.Err() != nil {
		return requestBundleAnswer{supported: true, authoritative: true, failed: true}
	}
	return requestBundleAnswer{bundles: bundles, supported: true, authoritative: bundles != nil}
}

func requestVectorChannel(ctx context.Context, backend search.Backend, query string, limit int) ([]string, search.ChannelTimings) {
	ctx = liveRequestContext(ctx)
	if backend == nil || ctx.Err() != nil {
		return nil, search.ChannelTimings{}
	}
	if contextual, ok := backend.(search.ContextVectorChannelOnly); ok {
		ids, timings := contextual.VectorChannelOnlyContext(ctx, query, limit)
		if ctx.Err() != nil {
			return nil, search.ChannelTimings{}
		}
		return ids, timings
	}
	legacy, ok := backend.(interface {
		VectorChannelOnly(string, int) ([]string, search.ChannelTimings)
	})
	if !ok {
		return nil, search.ChannelTimings{}
	}
	ids, timings := legacy.VectorChannelOnly(query, limit)
	if ctx.Err() != nil {
		return nil, search.ChannelTimings{}
	}
	return ids, timings
}

func requestSearchChannels(ctx context.Context, backend search.Backend, query string, limit int) ([]search.SearchResult, []string, search.ChannelTimings) {
	ctx = liveRequestContext(ctx)
	if backend == nil || ctx.Err() != nil {
		return nil, nil, search.ChannelTimings{}
	}
	if contextual, ok := backend.(search.ContextTimedChannelSearcher); ok {
		text, vector, timings := contextual.SearchChannelsTimedContext(ctx, query, limit)
		if ctx.Err() != nil {
			return nil, nil, search.ChannelTimings{}
		}
		return text, vector, timings
	}
	if contextual, ok := backend.(search.ContextChannelSearcher); ok {
		text, vector := contextual.SearchChannelsContext(ctx, query, limit)
		if ctx.Err() != nil {
			return nil, nil, search.ChannelTimings{}
		}
		return text, vector, search.ChannelTimings{}
	}
	if contextual, ok := backend.(search.ContextBackend); ok {
		text := contextual.SearchContext(ctx, query, limit)
		if ctx.Err() != nil {
			return nil, nil, search.ChannelTimings{}
		}
		return text, nil, search.ChannelTimings{}
	}
	if timed, ok := backend.(interface {
		SearchChannelsTimed(string, int) ([]search.SearchResult, []string, search.ChannelTimings)
	}); ok {
		text, vector, timings := timed.SearchChannelsTimed(query, limit)
		if ctx.Err() != nil {
			return nil, nil, search.ChannelTimings{}
		}
		return text, vector, timings
	}
	if channels, ok := backend.(search.ChannelSearcher); ok {
		text, vector := channels.SearchChannels(query, limit)
		if ctx.Err() != nil {
			return nil, nil, search.ChannelTimings{}
		}
		return text, vector, search.ChannelTimings{}
	}
	return requestTextSearch(ctx, backend, query, limit), nil, search.ChannelTimings{}
}

func requestPathScopedSymbolBundles(ctx context.Context, backend search.Backend, query string, repos, paths []string, limit int) requestBundleAnswer {
	ctx = liveRequestContext(ctx)
	if ctx.Err() != nil {
		return requestBundleAnswer{supported: true, authoritative: true, failed: true}
	}
	source, ok := backend.(search.PathScopedContextSymbolBundleSearcherBackend)
	if !ok {
		return requestBundleAnswer{}
	}
	bundles, handled := source.SearchSymbolBundlesPathScopedContext(ctx, query, repos, paths, limit)
	if ctx.Err() != nil {
		return requestBundleAnswer{supported: true, authoritative: true, failed: true}
	}
	return requestBundleAnswer{bundles: bundles, supported: true, authoritative: handled, failed: handled && bundles == nil}
}
