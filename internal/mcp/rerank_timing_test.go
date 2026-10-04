package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
)

func TestSearchSymbolsRerankTimingLabelsPreserveResults(t *testing.T) {
	run := func(debug bool) ([]string, map[string]interface{}, int32) {
		g := graph.New()
		a := &graph.Node{ID: "r/a.go::Alpha", Name: "Alpha", Kind: graph.KindFunction, FilePath: "r/a.go", RepoPrefix: "r"}
		b := &graph.Node{ID: "r/b.go::AlphaHelper", Name: "AlphaHelper", Kind: graph.KindFunction, FilePath: "r/b.go", RepoPrefix: "r"}
		g.AddNode(a)
		g.AddNode(b)
		backend := &searchSymbolsContextBackend{bundles: []search.SymbolBundle{{Node: a}, {Node: b}}}
		eng := query.NewEngine(g)
		eng.SetSearch(backend)
		level := zap.InfoLevel
		if debug {
			level = zap.DebugLevel
		}
		core, logs := observer.New(level)
		srv := NewServer(eng, g, nil, nil, zap.New(core), nil)
		res, err := srv.handleSearchSymbols(context.Background(), makeReq("search_symbols", map[string]any{"query": "Alpha", "limit": 5, "expand": "none", "assist": "never"}))
		require.NoError(t, err)
		require.False(t, res.IsError)
		var body map[string]any
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcpgo.TextContent).Text), &body))
		var fields map[string]interface{}
		entries := logs.FilterMessage("search_symbols phases").All()
		if debug {
			require.Len(t, entries, 1)
			fields = entries[0].ContextMap()
		} else {
			require.Empty(t, entries)
		}
		return resultIDs(body), fields, backend.contextCalls.Load()
	}
	plain, _, reads := run(false)
	observed, fields, observedReads := run(true)
	require.NotEmpty(t, plain)
	require.Equal(t, plain, observed)
	require.Equal(t, reads, observedReads)
	for _, label := range []string{"rerank_inner", "rerank_outer"} {
		legs := fields[label].(map[string]any)
		require.Equal(t, 1, legs["prepare_calls"])
		require.Equal(t, 1, legs["scoring_calls"])
		for _, key := range []string{"prepare_ms", "metrics_ms", "centrality_ms", "scoring_ms"} {
			require.IsType(t, float64(0), legs[key])
		}
	}
	fieldsMS := symbolRerankTimingFields(rerank.Timing{Metrics: 2500 * time.Microsecond})
	require.Equal(t, 2.5, fieldsMS["metrics_ms"])
}
