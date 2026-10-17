package search

import (
	"context"
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type contextGraphSearcherStub struct {
	graph.SymbolSearcher

	legacyCalls       int
	contextCalls      int
	bundleCalls       int
	scopedBundleCalls int
	contextErr        error
	hits              []graph.SymbolHit
	bundles           []graph.SymbolBundle
}

func (s *contextGraphSearcherStub) SearchSymbols(string, int) ([]graph.SymbolHit, error) {
	s.legacyCalls++
	return s.hits, nil
}

func (s *contextGraphSearcherStub) SearchSymbolsContext(context.Context, string, int) ([]graph.SymbolHit, error) {
	s.contextCalls++
	return s.hits, s.contextErr
}

func (s *contextGraphSearcherStub) SearchSymbolBundlesContext(context.Context, string, int) ([]graph.SymbolBundle, error) {
	s.bundleCalls++
	return s.bundles, s.contextErr
}

func (s *contextGraphSearcherStub) SearchSymbolBundlesRepoScopedContext(context.Context, string, []string, int) ([]graph.SymbolBundle, error) {
	s.scopedBundleCalls++
	return s.bundles, s.contextErr
}

type legacyGraphSearcherStub struct {
	graph.SymbolSearcher
	calls int
	hits  []graph.SymbolHit
}

func (s *legacyGraphSearcherStub) SearchSymbols(string, int) ([]graph.SymbolHit, error) {
	s.calls++
	return s.hits, nil
}

type contextBackendStub struct {
	contextCalls int
	legacyCalls  int
	timedCalls   int
	results      []SearchResult
}

func (*contextBackendStub) Add(string, ...string) {}
func (*contextBackendStub) Remove(string)         {}
func (s *contextBackendStub) Search(string, int) []SearchResult {
	s.legacyCalls++
	return s.results
}
func (*contextBackendStub) Count() int { return 1 }
func (*contextBackendStub) Close()     {}
func (s *contextBackendStub) SearchContext(context.Context, string, int) []SearchResult {
	s.contextCalls++
	return s.results
}
func (s *contextBackendStub) SearchChannelsTimedContext(context.Context, string, int) ([]SearchResult, []string, ChannelTimings) {
	s.timedCalls++
	return s.results, nil, ChannelTimings{TextMS: 1}
}

func TestSymbolSearcherBackendSearchContextIsAuthoritative(t *testing.T) {
	source := &contextGraphSearcherStub{
		hits: []graph.SymbolHit{{NodeID: "symbol", Score: 7}},
	}
	backend := NewSymbolSearcherBackend(source)

	got := backend.SearchContext(context.Background(), "symbol query", 5)
	if len(got) != 1 || got[0].ID != "symbol" || got[0].Score != 7 {
		t.Fatalf("context results = %#v", got)
	}
	if source.contextCalls != 1 || source.legacyCalls != 0 {
		t.Fatalf("calls after context search = context:%d legacy:%d", source.contextCalls, source.legacyCalls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := backend.SearchContext(ctx, "symbol query", 5); got != nil {
		t.Fatalf("pre-canceled results = %#v", got)
	}
	if source.contextCalls != 1 || source.legacyCalls != 0 {
		t.Fatalf("pre-canceled search invoked source: context:%d legacy:%d", source.contextCalls, source.legacyCalls)
	}

	source.contextErr = errors.New("context search failed")
	if got := backend.SearchContext(context.Background(), "symbol query", 5); got != nil {
		t.Fatalf("failed context results = %#v", got)
	}
	if source.contextCalls != 2 || source.legacyCalls != 0 {
		t.Fatalf("context error fell back: context:%d legacy:%d", source.contextCalls, source.legacyCalls)
	}
}

func TestSymbolSearcherBackendSearchContextUsesLiveLegacyFallbackOnly(t *testing.T) {
	source := &legacyGraphSearcherStub{hits: []graph.SymbolHit{{NodeID: "legacy", Score: 3}}}
	backend := NewSymbolSearcherBackend(source)

	got := backend.SearchContext(context.Background(), "legacy query", 5)
	if len(got) != 1 || got[0].ID != "legacy" {
		t.Fatalf("legacy fallback results = %#v", got)
	}
	if source.calls != 1 {
		t.Fatalf("legacy calls = %d, want 1", source.calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := backend.SearchContext(ctx, "legacy query", 5); got != nil {
		t.Fatalf("pre-canceled legacy results = %#v", got)
	}
	if source.calls != 1 {
		t.Fatalf("pre-canceled request invoked legacy source; calls = %d", source.calls)
	}
}

func TestSymbolSearcherBackendContextBundlePathsDoNotFallback(t *testing.T) {
	source := &contextGraphSearcherStub{}
	backend := NewSymbolSearcherBackend(source)

	if got := backend.SearchSymbolBundlesScopedContext(context.Background(), "query", []string{"repo"}, 5); got == nil || len(got) != 0 {
		t.Fatalf("scoped empty result = %#v, want non-nil empty", got)
	}
	if source.scopedBundleCalls != 1 {
		t.Fatalf("scoped context bundle calls = %d, want 1", source.scopedBundleCalls)
	}

	source.contextErr = context.Canceled
	if got := backend.SearchSymbolBundlesContext(context.Background(), "query", 5); got != nil {
		t.Fatalf("canceled bundle results = %#v", got)
	}
	if source.bundleCalls != 1 || source.legacyCalls != 0 {
		t.Fatalf("bundle error fell back: bundle:%d legacy:%d", source.bundleCalls, source.legacyCalls)
	}
}

func TestSwappableContextForwardersPreferContextCapabilities(t *testing.T) {
	inner := &contextBackendStub{results: []SearchResult{{ID: "context", Score: 1}}}
	swappable := NewSwappable(inner)

	got := swappable.SearchContext(context.Background(), "query", 5)
	if len(got) != 1 || got[0].ID != "context" {
		t.Fatalf("context results = %#v", got)
	}
	if inner.contextCalls != 1 || inner.legacyCalls != 0 {
		t.Fatalf("search calls = context:%d legacy:%d", inner.contextCalls, inner.legacyCalls)
	}

	text, vector, timings := swappable.SearchChannelsTimedContext(context.Background(), "query", 5)
	if len(text) != 1 || len(vector) != 0 || timings.TextMS != 1 {
		t.Fatalf("timed channels = text:%#v vector:%#v timings:%+v", text, vector, timings)
	}
	if inner.timedCalls != 1 {
		t.Fatalf("timed context calls = %d, want 1", inner.timedCalls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := swappable.SearchContext(ctx, "query", 5); got != nil {
		t.Fatalf("pre-canceled swappable results = %#v", got)
	}
	if inner.contextCalls != 1 || inner.legacyCalls != 0 {
		t.Fatalf("pre-canceled swappable invoked inner: context:%d legacy:%d", inner.contextCalls, inner.legacyCalls)
	}
}
