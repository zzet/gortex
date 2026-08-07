package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
)

// SearchSymbols is documented on the Controller interface as "the cheap probe
// path used by external clients (Claude Code's Grep-redirect hook) that need a
// single short answer without setting up a full MCP session".
//
// It was not cheap. It took c.mu to read a handle that is assigned once at
// construction and never reassigned, and c.mu is held for the entire duration
// of a track / reload / enrichment. So the cheapest call in the tree waited
// out the most expensive operation in the daemon: measured, the
// UserPromptSubmit probe that depends on it exceeded its 800ms budget on
// 82.6% of turns, which reads downstream as "no hits" rather than "ask later".
func TestSearchSymbolsAnswersDuringLongMutation(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "pkg/a.go::Widget", Kind: graph.KindType, Name: "Widget",
		FilePath: "pkg/a.go", Language: "go",
	})
	c := &realController{graph: g}

	c.mu.Lock() // stand in for a track / reload / enrichment in flight
	defer c.mu.Unlock()

	type searchResult struct {
		res daemon.SearchSymbolsResult
		err error
	}
	done := make(chan searchResult, 1)
	go func() {
		res, err := c.SearchSymbols(context.Background(), daemon.SearchSymbolsParams{Query: "Widget"})
		done <- searchResult{res: res, err: err}
	}()

	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.NotEmpty(t, got.res.Hits,
			"the probe must still answer from the graph while the indexer is busy")
	case <-time.After(2 * time.Second):
		t.Fatal("SearchSymbols blocked behind the controller mutex — the hook probe path " +
			"must not take c.mu, or it waits out every reindex")
	}
}
