package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
)

func resetRerankProviderForTest(t *testing.T) {
	t.Helper()
	// Pin the baked offline provider: these policy tests must not load or
	// download user model assets. Reset publication without changing HOME.
	t.Setenv("GORTEX_POTION", "0")
	embedding.SetCodeEmbedderEnabled(false)
	embedding.SetCodeEmbedderEnabled(true)
	t.Cleanup(func() {
		embedding.SetCodeEmbedderEnabled(false)
		embedding.SetCodeEmbedderEnabled(true)
	})
}

func TestSearchSymbolsSemanticLoadingUsesEffectiveClass(t *testing.T) {
	for _, tc := range []struct {
		name, text, pin string
		loads           bool
		invalid         bool
	}{
		{name: "symbol", text: "HTTPServer"},
		{name: "path", text: "pkg/server.go"},
		{name: "signature", text: "func(ctx) error"},
		{name: "concept", text: "output encoding", loads: true},
		{name: "pinned_symbol", text: "output encoding", pin: "symbol"},
		{name: "pinned_path", text: "output encoding", pin: "path"},
		{name: "pinned_signature", text: "output encoding", pin: "signature"},
		{name: "pinned_concept", text: "HTTPServer", pin: "concept", loads: true},
		{name: "pinned_soup", text: "HTTPServer", pin: "keyword_soup", loads: true},
		{name: "soup_overrides_symbol_pin", text: "auth OR login OR credential", pin: "symbol", loads: true},
		{name: "invalid_pin", text: "output encoding", pin: "invalid", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetRerankProviderForTest(t)
			backend := &searchSymbolsContextBackend{bundles: []search.SymbolBundle{{Node: &graph.Node{ID: "Format", Name: "Format", Kind: graph.KindFunction}}}}
			srv := newSearchSymbolsContextServer(backend)
			args := map[string]any{"query": tc.text, "expand": "none", "assist": "never"}
			if tc.pin != "" {
				args["query_class"] = tc.pin
			}
			res, err := srv.handleSearchSymbols(context.Background(), makeReq("search_symbols", args))
			require.NoError(t, err)
			require.Equal(t, tc.invalid, res.IsError)
			require.Equal(t, tc.loads, embedding.LoadedSharedCodeEmbedder() != nil)
			if tc.invalid {
				require.Zero(t, backend.contextCalls.Load(), "invalid class must fail before retrieval or initialization")
			}
		})
	}
}

func TestSymbolRerankWarmProviderPreservesVectorsAndRanking(t *testing.T) {
	resetRerankProviderForTest(t)
	g := graph.New()
	nodes := []*graph.Node{
		{ID: "repo/output.go::Format", Name: "Format", Kind: graph.KindFunction, FilePath: "repo/output.go", Meta: map[string]any{"doc": "output encoding"}},
		{ID: "repo/db.go::Close", Name: "Close", Kind: graph.KindFunction, FilePath: "repo/db.go", Meta: map[string]any{"doc": "close database connection"}},
	}
	for _, node := range nodes {
		g.AddNode(node)
	}
	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	text := "output encoding"
	// Natural-language construction still loads eagerly. A later literal
	// search reuses that exact provider and all its scoring inputs.
	eager := srv.buildRerankContext(context.Background(), text)
	require.NotNil(t, eager.EmbedText)
	require.NotEmpty(t, eager.QueryVec)
	warm := srv.buildSymbolRerankContext(context.Background(), text, rerank.QueryClassSymbol)
	require.NotNil(t, warm.EmbedText)
	require.Equal(t, eager.QueryVec, warm.QueryVec)
	require.Equal(t, eager.EmbedText("format output encoding"), warm.EmbedText("format output encoding"))
	require.Equal(t, rerank.QueryClassUnknown, warm.QueryClass, "provider policy must not move ranking-class assignment earlier")
	candidates := func() []*rerank.Candidate {
		return []*rerank.Candidate{{Node: nodes[0], TextRank: 1, VectorRank: -1}, {Node: nodes[1], TextRank: 0, VectorRank: -1}}
	}
	rank := rerank.NewDefault()
	require.Equal(t, rank.Rerank(text, candidates(), eager), rank.Rerank(text, candidates(), warm))
	require.Greater(t, (rerank.SemanticCosineSignal{}).Contribute(text, &rerank.Candidate{Node: nodes[0]}, warm), 0.0, "natural-language semantic scoring must remain active")

	embedding.SetCodeEmbedderEnabled(false)
	disabled := srv.buildSymbolRerankContext(context.Background(), text, rerank.QueryClassSymbol)
	require.Nil(t, disabled.EmbedText)
	require.Empty(t, disabled.QueryVec)
}

func TestRerankContextPreCanceledDoesNotInitializeProvider(t *testing.T) {
	resetRerankProviderForTest(t)
	srv := newSearchSymbolsContextServer(&searchSymbolsContextBackend{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, build := range []func() *rerank.Context{
		func() *rerank.Context { return srv.buildRerankContext(ctx, "output encoding") },
		func() *rerank.Context {
			return srv.buildSymbolRerankContext(ctx, "output encoding", rerank.QueryClassConcept)
		},
		func() *rerank.Context {
			return srv.buildSymbolRerankContext(ctx, "HTTPServer", rerank.QueryClassSymbol)
		},
	} {
		rctx := build()
		require.Nil(t, rctx.EmbedText)
		require.Empty(t, rctx.QueryVec)
		require.Nil(t, embedding.LoadedSharedCodeEmbedder())
	}
}
