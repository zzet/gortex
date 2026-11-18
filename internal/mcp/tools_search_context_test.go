package mcp

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
)

type searchSymbolsContextBackend struct {
	block             bool
	started           chan struct{}
	startOnce         sync.Once
	bundles           []search.SymbolBundle
	contextCalls      atomic.Int32
	legacyBundleCalls atomic.Int32
	legacySearchCalls atomic.Int32
}

func (*searchSymbolsContextBackend) Add(string, ...string) {}
func (*searchSymbolsContextBackend) Remove(string)         {}
func (b *searchSymbolsContextBackend) Search(string, int) []search.SearchResult {
	b.legacySearchCalls.Add(1)
	return nil
}
func (*searchSymbolsContextBackend) Count() int { return 1 }
func (*searchSymbolsContextBackend) Close()     {}

func (b *searchSymbolsContextBackend) SearchSymbolBundles(_ string, _ int) []search.SymbolBundle {
	b.legacyBundleCalls.Add(1)
	return b.bundles
}

func (b *searchSymbolsContextBackend) SearchSymbolBundlesContext(ctx context.Context, _ string, _ int) []search.SymbolBundle {
	b.contextCalls.Add(1)
	b.startOnce.Do(func() {
		if b.started != nil {
			close(b.started)
		}
	})
	if b.block {
		<-ctx.Done()
		return nil
	}
	return b.bundles
}

func newSearchSymbolsContextServer(backend search.Backend) *Server {
	g := graph.New()
	eng := query.NewEngine(g)
	eng.SetSearch(backend)
	eng.SetRerank(nil)
	return NewServer(eng, g, nil, nil, zap.NewNop(), nil)
}

func searchSymbolsContextRequest(queryText string) mcpgo.CallToolRequest {
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"query":  queryText,
		"limit":  5,
		"assist": "off",
	}
	return req
}

type searchSymbolsContextResponse struct {
	result *mcpgo.CallToolResult
	err    error
}

func callSearchSymbolsContext(srv *Server, ctx context.Context, req mcpgo.CallToolRequest) <-chan searchSymbolsContextResponse {
	done := make(chan searchSymbolsContextResponse, 1)
	go func() {
		result, err := srv.handleSearchSymbols(ctx, req)
		done <- searchSymbolsContextResponse{result: result, err: err}
	}()
	return done
}

func TestSearchSymbolsCancellationStopsAdmittedBackend(t *testing.T) {
	backend := &searchSymbolsContextBackend{
		block:   true,
		started: make(chan struct{}),
	}
	srv := newSearchSymbolsContextServer(backend)
	ctx, cancel := context.WithCancel(context.Background())
	done := callSearchSymbolsContext(srv, ctx, searchSymbolsContextRequest("blockingLookup"))

	select {
	case <-backend.started:
	case <-time.After(5 * time.Second):
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatal("search_symbols never entered the context-aware backend")
	}
	cancel()

	var got searchSymbolsContextResponse
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("search_symbols worker did not exit after cancellation")
	}
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("handleSearchSymbols error = %v, want context.Canceled (result=%#v)", got.err, got.result)
	}
	if got.result != nil {
		t.Fatalf("canceled search_symbols returned authoritative result: %#v", got.result)
	}
	if calls := backend.contextCalls.Load(); calls != 1 {
		t.Fatalf("context calls = %d, want one admitted primary call and no fallback", calls)
	}
	if calls := backend.legacyBundleCalls.Load() + backend.legacySearchCalls.Load(); calls != 0 {
		t.Fatalf("canceled search_symbols used legacy backend %d times", calls)
	}
}

func TestSearchSymbolsPreCanceledAndLiveControls(t *testing.T) {
	req := searchSymbolsContextRequest("blockingLookup")

	preCanceled := &searchSymbolsContextBackend{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := newSearchSymbolsContextServer(preCanceled).handleSearchSymbols(ctx, req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled error = %v, want context.Canceled (result=%#v)", err, result)
	}
	if result != nil || preCanceled.contextCalls.Load() != 0 || preCanceled.legacyBundleCalls.Load() != 0 || preCanceled.legacySearchCalls.Load() != 0 {
		t.Fatalf("pre-canceled request did work: result=%#v context=%d bundle=%d search=%d",
			result, preCanceled.contextCalls.Load(), preCanceled.legacyBundleCalls.Load(), preCanceled.legacySearchCalls.Load())
	}

	for _, tc := range []struct {
		name    string
		bundles []search.SymbolBundle
	}{
		{name: "empty", bundles: []search.SymbolBundle{}},
		{name: "nonempty", bundles: []search.SymbolBundle{{Node: &graph.Node{ID: "node-1", Name: "blockingLookup", Kind: graph.KindFunction}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &searchSymbolsContextBackend{bundles: tc.bundles}
			liveCtx, liveCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer liveCancel()
			got := <-callSearchSymbolsContext(newSearchSymbolsContextServer(backend), liveCtx, req)
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.result == nil || got.result.IsError || len(got.result.Content) == 0 {
				t.Fatalf("uncanceled %s control lost result contract: %#v", tc.name, got.result)
			}
			if calls := backend.contextCalls.Load(); calls == 0 {
				t.Fatal("live control did not exercise the context-aware backend")
			}
			if calls := backend.legacyBundleCalls.Load() + backend.legacySearchCalls.Load(); calls != 0 {
				t.Fatalf("live context-aware control used legacy backend %d times", calls)
			}
		})
	}
}

func TestSearchSymbolsScopedContextMatchesLegacyHelper(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "a", Name: "Alpha", Kind: graph.KindFunction, WorkspaceID: "workspace"},
		{ID: "b", Name: "Beta", Kind: graph.KindMethod, WorkspaceID: "workspace"},
	}
	backend := &searchSymbolsContextBackend{bundles: []search.SymbolBundle{{Node: nodes[0]}, {Node: nodes[1]}}}
	eng := query.NewEngine(graph.New())
	eng.SetSearch(backend)
	eng.SetRerank(nil)
	scope := query.QueryOptions{WorkspaceID: "workspace", RerankContext: &rerank.Context{}}

	legacy, legacyPrimary := fetchAndMergeBM25Timed(eng, "alpha", []string{"beta"}, 5, scope, nil)
	legacyContextCalls := backend.contextCalls.Load()
	contextual, contextualPrimary := fetchAndMergeBM25TimedContext(context.Background(), eng, "alpha", []string{"beta"}, 5, scope, nil)
	ids := func(in []*graph.Node) []string {
		out := make([]string, 0, len(in))
		for _, node := range in {
			out = append(out, node.ID)
		}
		return out
	}
	if !reflect.DeepEqual(ids(contextual), ids(legacy)) || contextualPrimary != legacyPrimary {
		t.Fatalf("context helper ids/primary = %v/%d, legacy = %v/%d", ids(contextual), contextualPrimary, ids(legacy), legacyPrimary)
	}
	if legacyContextCalls == 0 || backend.contextCalls.Load() <= legacyContextCalls {
		t.Fatalf("parity test missed a helper path: after legacy=%d after context=%d", legacyContextCalls, backend.contextCalls.Load())
	}
	if calls := backend.legacyBundleCalls.Load() + backend.legacySearchCalls.Load(); calls != 0 {
		t.Fatalf("scoped helpers fell back to contextless backend %d times", calls)
	}
}
