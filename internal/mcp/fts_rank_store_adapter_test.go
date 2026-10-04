package mcp

import (
	"context"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/search"
)

// The whole search_symbols answer through the in-memory FTS ranker equals the
// answer through the store's FTS5 query, on a real routed checkout, before and
// after an edit publishes a new top generation.
func TestSymbolSearchThroughTheInMemoryFTSRankerEqualsTheFTS5Path(t *testing.T) {
	checkInMemoryFTSRankerEqualsFTS5(t, search.FTSViewMatchOff)
}

// The same with every view's generations read by the store's one MATCH.
func TestSymbolSearchThroughTheOneMatchReadEqualsTheFTS5Path(t *testing.T) {
	// No generation is read whole, so each one needs a MATCH.
	t.Cleanup(search.SetFTSWholeReadMaxRowsForTest(0))
	before := search.FTSViewMatchReads()
	checkInMemoryFTSRankerEqualsFTS5(t, search.FTSViewMatchOn)
	require.Positive(t, search.FTSViewMatchReads()-before, "no search read its generations with one MATCH")
}

func checkInMemoryFTSRankerEqualsFTS5(t *testing.T, mode search.FTSViewMatchMode) {
	t.Cleanup(search.SetFTSViewMatchModeForTest(mode))
	// Every generation read spans several pages.
	previousPage := storeFTSRowsPage
	storeFTSRowsPage = 2
	t.Cleanup(func() { storeFTSRowsPage = previousPage })

	f, _ := newAdjacencyMemoFixture(t)
	f.srv.pprCache.enabled = false
	// The daemon's backend: the store's own full-text search behind the
	// indexer's swappable wrapper (indexer.initialSearchBackend).
	f.srv.engine.SetSearch(search.NewSwappable(search.NewSymbolSearcherBackend(f.store)))
	searchOnce := func(query string) string {
		req := mcplib.CallToolRequest{}
		req.Params.Name = "search_symbols"
		req.Params.Arguments = map[string]any{
			"query": query, "limit": 20,
			"view": map[string]any{"kind": "worktree", "checkout_id": f.checkoutID},
		}
		ctx := WithSessionCWD(WithSessionID(context.Background(), "fts-rank-equality"), f.primary)
		result, err := f.srv.wrapToolHandler(f.srv.handleSearchSymbols)(ctx, req)
		require.NoError(t, err)
		require.False(t, result.IsError, viewResultText(t, result))
		return viewResultText(t, result)
	}
	compare := func(label string) {
		for _, q := range []string{"Widget", "RenderWidget3", "BuildWidget", "render widget", "widget build", "New", "Extra", "RenderWidgetRenamed5"} {
			before := search.FTSRankedGenerations()
			inMemory := searchOnce(q)
			ranked := search.FTSRankedGenerations() - before
			restore := search.DisableFTSRankForTest()
			viaFTS5 := searchOnce(q)
			restore()
			require.Equal(t, viaFTS5, inMemory, "%s: %q answered differently through the in-memory ranker", label, q)
			require.NotContains(t, inMemory, `"results":null`, "%s: %q found nothing; the comparison would be vacuous", label, q)
			t.Logf("%s %q: %d generation ranking(s) in memory", label, q, ranked)
		}
	}
	engaged := search.FTSRankedGenerations()
	compare("before the edit")
	require.Positive(t, search.FTSRankedGenerations()-engaged, "the in-memory ranker never served a generation")

	written := f.edit(t, f.worktree, map[string]any{
		"path": "repo/widget5.go", "old_string": "func RenderWidget5(", "new_string": "func RenderWidgetRenamed5(",
	})
	require.False(t, written.IsError, viewResultText(t, written))
	f.awaitMutation(t, f.worktree, written)
	engaged = search.FTSRankedGenerations()
	compare("after the edit")
	require.Positive(t, search.FTSRankedGenerations()-engaged, "the in-memory ranker served nothing after the publication")
}

// The store does not serve the one-MATCH read yet: the adapter over it must
// not claim it, so every generation keeps its own MATCH read until it does.
func TestStoreAdapterClaimsTheOneMatchReadOnlyWhenTheStoreServesIt(t *testing.T) {
	f, _ := newAdjacencyMemoFixture(t)
	src, ok := adaptStoreFTSRankSource(f.store)
	require.True(t, ok)
	_, claims := src.(search.FTSViewMatchSource)
	_, served := any(f.store).(storeViewMatch)
	require.Equal(t, served, claims)
}

// The adapter reads a generation's rowid run from the store's own generation
// statistics: on the real routed fixture every generation of the view with
// documents reports a run whose bounds and document count are the store's.
func TestStoreAdapterReadsTheGenerationRunFromTheStore(t *testing.T) {
	f, _ := newAdjacencyMemoFixture(t)
	src, ok := adaptStoreFTSRankSource(f.store)
	require.True(t, ok)
	runner, ok := src.(search.FTSGenerationDocsSource)
	require.True(t, ok)
	ctx := context.Background()
	view, err := f.srv.selectRequestView(ctx, graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: f.checkoutID}, requestViewPolicy{})
	require.NoError(t, err)
	t.Cleanup(view.close)
	sources := view.materialized.GenerationSources()
	require.NotEmpty(t, sources)
	known := 0
	for _, source := range sources {
		generation := source.Handle.SymbolSearchViewGeneration()
		run, err := runner.SymbolFTSGenerationRun(ctx, generation)
		require.NoError(t, err)
		st, err := f.store.SymbolFTSGenerationStats(ctx, generation)
		require.NoError(t, err)
		if !run.Known {
			require.Zero(t, st.Rows, "generation %d has documents but reported no run", generation)
			continue
		}
		known++
		require.Equal(t, st.Rows, run.Docs, "generation %d", generation)
		require.LessOrEqual(t, run.Lo, run.Hi, "generation %d", generation)
		rows, _, err := f.store.SymbolFTSGenerationRows(ctx, generation, "", 0, 100000)
		require.NoError(t, err)
		require.Len(t, rows, int(run.Docs), "generation %d", generation)
		require.Equal(t, run.Lo, rows[0].RowID, "generation %d", generation)
		require.Equal(t, run.Hi, rows[len(rows)-1].RowID, "generation %d", generation)
	}
	require.Positive(t, known, "no generation of the view reported a run")
}

// waitRouteFTSWarmIdle waits until the background route warm has no route
// queued and its worker has stopped.
func waitRouteFTSWarmIdle(t *testing.T, srv *Server) {
	t.Helper()
	require.Eventually(t, func() bool {
		q := &srv.routeFTSWarm
		q.mu.Lock()
		defer q.mu.Unlock()
		return !q.running && len(q.pending) == 0
	}, 60*time.Second, 10*time.Millisecond, "the route warm did not settle")
}

func searchSymbolsOnce(t *testing.T, f *realCheckoutMutationFixture, session, query string) string {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "search_symbols"
	req.Params.Arguments = map[string]any{
		"query": query, "limit": 20,
		"view": map[string]any{"kind": "worktree", "checkout_id": f.checkoutID},
	}
	ctx := WithSessionCWD(WithSessionID(context.Background(), session), f.primary)
	result, err := f.srv.wrapToolHandler(f.srv.handleSearchSymbols)(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError, viewResultText(t, result))
	return viewResultText(t, result)
}

// The route pre-warm queues the route's base generations for the full-text
// ranker, and the background worker reads them, so the first search after a
// publication reads at most the new working-tree generation — and its answer
// still equals the FTS5 path's.
func TestRoutePrewarmReadsTheBaseGenerationsForTheRanker(t *testing.T) {
	f, _ := newAdjacencyMemoFixture(t)
	f.srv.pprCache.enabled = false
	f.srv.engine.SetSearch(search.NewSwappable(search.NewSymbolSearcherBackend(f.store)))
	written := f.edit(t, f.worktree, map[string]any{
		"path": "repo/widget5.go", "old_string": "func RenderWidget5(", "new_string": "func RenderWidgetWarm5(",
	})
	require.False(t, written.IsError, viewResultText(t, written))
	f.awaitMutation(t, f.worktree, written)
	waitRouteFTSWarmIdle(t, f.srv)

	before, _ := search.FTSWholeReadCounts()
	inMemory := searchSymbolsOnce(t, f, "fts-route-warm", "RenderWidgetWarm5")
	after, _ := search.FTSWholeReadCounts()
	require.LessOrEqual(t, after-before, int64(1), "the first search after the publication read %d generations whole; the warm should have read all but the new one", after-before)

	restore := search.DisableFTSRankForTest()
	viaFTS5 := searchSymbolsOnce(t, f, "fts-route-warm", "RenderWidgetWarm5")
	restore()
	require.Equal(t, viaFTS5, inMemory)
	require.Contains(t, inMemory, "RenderWidgetWarm5")
}

// Two publications in a row with no search between them: each publication's
// route pre-warm makes no store read for the ranker (it only queues the
// route); the reads happen afterwards, on the background worker, and the next
// search then reads at most the newest generation.
func TestRoutePrewarmMakesNoRankerReadInsideAPublication(t *testing.T) {
	gate := make(chan struct{})
	previous := routeFTSWarmGateForTest
	routeFTSWarmGateForTest = func() { <-gate }
	released := false
	release := func() {
		if !released {
			released = true
			close(gate)
		}
	}
	t.Cleanup(func() { release(); routeFTSWarmGateForTest = previous })

	f, _ := newAdjacencyMemoFixture(t)
	f.srv.pprCache.enabled = false
	f.srv.engine.SetSearch(search.NewSwappable(search.NewSymbolSearcherBackend(f.store)))

	reads := search.FTSStoreReads()
	for i, edit := range []map[string]any{
		{"path": "repo/widget5.go", "old_string": "func RenderWidget5(", "new_string": "func RenderWidgetFirst5("},
		{"path": "repo/widget3.go", "old_string": "func RenderWidget3(", "new_string": "func RenderWidgetSecond3("},
	} {
		written := f.edit(t, f.worktree, edit)
		require.False(t, written.IsError, viewResultText(t, written))
		f.awaitMutation(t, f.worktree, written)
		require.Equal(t, reads, search.FTSStoreReads(), "publication %d made %d store read(s) for the ranker", i+1, search.FTSStoreReads()-reads)
	}

	release()
	waitRouteFTSWarmIdle(t, f.srv)
	warmed := search.FTSStoreReads() - reads
	require.Positive(t, warmed, "the background worker read nothing")

	before, _ := search.FTSWholeReadCounts()
	answer := searchSymbolsOnce(t, f, "fts-route-warm-two", "RenderWidgetSecond3")
	after, _ := search.FTSWholeReadCounts()
	require.LessOrEqual(t, after-before, int64(1), "the search after two publications read %d generations whole", after-before)
	require.Contains(t, answer, "RenderWidgetSecond3")
}

// A generation that stops being servable is dropped from the ranker with the
// materializer's masks.
func TestForgottenGenerationsLeaveTheRanker(t *testing.T) {
	f, _ := newAdjacencyMemoFixture(t)
	f.srv.pprCache.enabled = false
	f.srv.engine.SetSearch(search.NewSwappable(search.NewSymbolSearcherBackend(f.store)))
	waitRouteFTSWarmIdle(t, f.srv)
	_ = searchSymbolsOnce(t, f, "fts-forget", "Widget")
	docs, bytes := search.FTSKeptDocs()
	require.Positive(t, docs)
	var kept []int64
	for _, d := range search.FTSWholeReadDecisions() {
		if d.Dense {
			kept = append(kept, d.Generation)
		}
	}
	require.NotEmpty(t, kept)
	for _, generation := range kept {
		f.srv.materializer.ForgetGeneration(generation)
	}
	afterDocs, afterBytes := search.FTSKeptDocs()
	require.Less(t, afterDocs, docs, "forgetting the generations dropped no kept document")
	require.Less(t, afterBytes, bytes)
}

// With the route pre-warm taking the new generation's rows from the store's
// fresh-rows hand-off, the first search after an edit reads no generation
// whole — for the old name, which the new generation does not hold and so must
// rank (the new name is answered by the exact-name tier) — and its whole output
// equals the FTS5 path's.
func TestFirstSearchAfterAnEditReadsNoGenerationWhole(t *testing.T) {
	f, _ := newAdjacencyMemoFixture(t)
	f.srv.pprCache.enabled = false
	f.srv.engine.SetSearch(search.NewSwappable(search.NewSymbolSearcherBackend(f.store)))
	waitRouteFTSWarmIdle(t, f.srv)
	_ = searchSymbolsOnce(t, f, "fts-fresh", "Widget") // the stack below is kept
	adopted, _ := search.FTSFreshCounts()

	written := f.edit(t, f.worktree, map[string]any{
		"path": "repo/widget3.go", "old_string": "func RenderWidget3(", "new_string": "func RenderWidgetFresh3(",
	})
	require.False(t, written.IsError, viewResultText(t, written))
	f.awaitMutation(t, f.worktree, written)
	waitRouteFTSWarmIdle(t, f.srv)

	before, _ := search.FTSWholeReadCounts()
	inMemory := searchSymbolsOnce(t, f, "fts-fresh", "RenderWidget3")
	after, _ := search.FTSWholeReadCounts()
	nowAdopted, _ := search.FTSFreshCounts()
	require.Equal(t, before, after, "the first search after the edit read %d generation(s) whole", after-before)
	require.Equal(t, adopted+1, nowAdopted, "the new generation's handed-over rows were not used")

	restore := search.DisableFTSRankForTest()
	viaFTS5 := searchSymbolsOnce(t, f, "fts-fresh", "RenderWidget3")
	restore()
	require.Equal(t, viaFTS5, inMemory)
}
