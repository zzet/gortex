package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/cochange"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

// newCoChangeTestServer builds a server with a small graph and the
// co-change caches pre-populated. The cochangeOnce is consumed with a
// no-op so the handler's ensureCoChange() does not shell out to git.
func newCoChangeTestServer(t *testing.T) *Server {
	t.Helper()
	g := graph.New()
	for _, f := range []string{"a.go", "b.go", "c.go", "lonely.go"} {
		g.AddNode(&graph.Node{ID: f, Kind: graph.KindFile, Name: f, FilePath: f, Language: "go"})
	}
	g.AddNode(&graph.Node{ID: "a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "b.go::Bar", Kind: graph.KindFunction, Name: "Bar", FilePath: "b.go", Language: "go"})

	s := &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
	s.storeCoChange(
		map[string]map[string]float64{
			"a.go": {"b.go": 0.9, "c.go": 0.3},
		},
		map[string]map[string]int{
			"a.go": {"b.go": 5, "c.go": 2},
		},
	)
	// Consume the once-guard so ensureCoChange becomes a no-op.
	s.cochangeOnce.Do(func() {})
	return s
}

func callFindCoChanging(t *testing.T, s *Server, args map[string]any) (map[string]any, bool) {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := s.handleFindCoChangingSymbols(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	if res.IsError {
		return nil, true
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &m))
	return m, false
}

func TestFindCoChanging_ByFilePath(t *testing.T) {
	s := newCoChangeTestServer(t)
	out, isErr := callFindCoChanging(t, s, map[string]any{"file_path": "a.go"})
	require.False(t, isErr)

	rows, _ := out["co_changing"].([]any)
	require.Len(t, rows, 2)
	// Sorted by score descending: b.go (0.9) before c.go (0.3).
	first, _ := rows[0].(map[string]any)
	require.Equal(t, "b.go", first["file"])
	require.Equal(t, float64(5), first["count"])
	require.InDelta(t, 0.9, first["score"], 0.001)
	syms, _ := first["symbols"].([]any)
	require.Contains(t, syms, "Bar")
}

func TestFindCoChanging_BySymbolID(t *testing.T) {
	s := newCoChangeTestServer(t)
	out, isErr := callFindCoChanging(t, s, map[string]any{"symbol_id": "a.go::Foo"})
	require.False(t, isErr)
	require.Equal(t, "a.go", out["target_file"])
	require.Equal(t, "a.go::Foo", out["symbol_id"])
	rows, _ := out["co_changing"].([]any)
	require.Len(t, rows, 2)
}

func TestFindCoChanging_MinScoreFilter(t *testing.T) {
	s := newCoChangeTestServer(t)
	out, isErr := callFindCoChanging(t, s, map[string]any{"file_path": "a.go", "min_score": 0.5})
	require.False(t, isErr)
	rows, _ := out["co_changing"].([]any)
	require.Len(t, rows, 1)
	first, _ := rows[0].(map[string]any)
	require.Equal(t, "b.go", first["file"])
}

func TestFindCoChanging_NoData(t *testing.T) {
	s := newCoChangeTestServer(t)
	out, isErr := callFindCoChanging(t, s, map[string]any{"file_path": "lonely.go"})
	require.False(t, isErr)
	rows, _ := out["co_changing"].([]any)
	require.Empty(t, rows)
}

func TestFindCoChanging_MissingArgs(t *testing.T) {
	s := newCoChangeTestServer(t)
	_, isErr := callFindCoChanging(t, s, map[string]any{})
	require.True(t, isErr, "expected an error when neither symbol_id nor file_path is given")
}

func TestFindCoChanging_UnknownSymbol(t *testing.T) {
	s := newCoChangeTestServer(t)
	_, isErr := callFindCoChanging(t, s, map[string]any{"symbol_id": "does/not::Exist"})
	require.True(t, isErr)
}

// TestCoChange_PersistedEdgesTakeFastPath proves change B's mechanism:
// mineCoChange persists mined pairs as EdgeCoChange edges (via
// cochange.AddEdges), so a subsequent daemon start reads them back via
// coChangeFromEdges (the fast path) instead of re-mining git log.
func TestCoChange_PersistedEdgesTakeFastPath(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "b.go", Language: "go"})

	// What mineCoChange now does after a git mine: persist the pairs.
	n := cochange.AddEdges(g, []cochange.Pair{{FileA: "a.go", FileB: "b.go", Score: 0.9, Count: 5}}, "")
	require.Positive(t, n, "AddEdges must persist EdgeCoChange edges")

	// A fresh server over the same graph takes the coChangeFromEdges
	// fast path (no git mine) and surfaces the persisted co-change.
	s := &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
	scores := map[string]map[string]float64{}
	counts := map[string]map[string]int{}
	require.True(t, s.coChangeFromEdges(scores, counts), "persisted edges must take the fast path")
	require.InDelta(t, 0.9, scores["a.go"]["b.go"], 1e-9)
	require.Equal(t, 5, counts["a.go"]["b.go"])
}

// W3.2 — the co-change mine was the last enrichment write in this package that
// named no output generation at all.
//
// `mineCoChange` is raised lazily from a READ path (find_co_changing_symbols
// with refresh, and the impact handler), sweeps `git log` in every tracked
// repository's own live worktree, and persists the result as EdgeCoChange rows
// in generation zero. Nothing resolved a view, nothing named an owner, and
// nothing ordered it against the index mutation that might be rewriting the
// same corpus. These tests pin the two halves of the repair: the write is named
// through the authority as a BASE output, and a request that reads a checkout
// of its own does not cause one.

// TestCoChangeMineNamesTheCorpusOutputItWrites is the naming half.
//
// A receipt standing for the same (producer, corpus) owner must be superseded
// by the mine's own admission — which can only happen if the mine admitted one.
// The fixture repositories carry no git history, so no pair is mined and no row
// is written: the admission is resolved BEFORE the history is read (the same
// ordering the coverage path uses for its profile), which is exactly what makes
// this checkable without a git corpus.
//
// Revert-red: with `cochange.AddEdges(s.graph, …)` and the bare
// `s.collectRepoRoots("")` sweep back, nothing is admitted and the standing
// receipt settles cleanly.
func TestCoChangeMineNamesTheCorpusOutputItWrites(t *testing.T) {
	stack := newViewStack(t)
	standing, err := BeginBaseEnrichment(context.Background(), stack.srv.outputGenerationAuthority(),
		stack.srv.graph, EnrichProducerCochange, "repo", stack.repoRoot)
	require.NoError(t, err)

	stack.srv.mineCoChange()

	err = standing.Complete()
	require.Error(t, err, "the co-change mine admitted no output generation of its own")
	require.True(t, errors.Is(err, indexer.ErrOutputMutationReceiptSuperseded),
		"supersession identity is %v", err)
}

// TestARoutedCoChangeRequestStartsNoMine is the view half.
//
// The mine is a corpus-wide sweep of every tracked live worktree plus a
// generation-zero write. A request that reads a checkout of its own describes
// none of that, and must not be the reason it happens.
//
// The second assertion is the part that is easy to get wrong: the refusal has
// to happen BEFORE sync.Once is consumed, or one routed request would
// permanently prevent the mine every later base request is entitled to.
//
// Revert-red: call s.ensureCoChange() unconditionally from the handler and the
// first assertion fails; consume the once before the view check and the second
// does.
func TestARoutedCoChangeRequestStartsNoMine(t *testing.T) {
	stack := newViewStack(t)
	ctx := routedEnrichmentCtx(t, stack)

	require.False(t, stack.srv.ensureCoChangeForRequest(ctx),
		"a request reading its own checkout started a corpus-wide co-change mine")

	oneShotIntact := false
	stack.srv.cochangeOnce.Do(func() { oneShotIntact = true })
	require.True(t, oneShotIntact,
		"the routed refusal burned the one-shot; no later base request can ever mine")
}

// TestRoutedCoChangeAnswersSayTheyCameFromTheBase is the annotation: the
// caches this tool answers from are process-wide and built over the indexed
// corpus, so under a routed view the answer is about the base and the rider
// has to say so.
//
// Revert-red: drop the annotateBaseScoped call and the routed answer carries no
// base_scoped entry.
func TestRoutedCoChangeAnswersSayTheyCameFromTheBase(t *testing.T) {
	stack := newViewStack(t)
	// refresh:false keeps this about the ANSWER — the mine's own gate is
	// pinned by the test above.
	args := map[string]any{"file_path": "repo/keep.go", "refresh": false}

	res, err := stack.callHandler(t, stack.worktreeRoot, "find_co_changing_symbols", args,
		stack.srv.handleFindCoChangingSymbols)
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	rider := resultFreshness(t, res)
	require.NotNil(t, rider, "a routed answer carries no rider: %s", viewResultText(t, res))
	named := map[string]bool{}
	entries, _ := rider["base_scoped"].([]any)
	for _, entry := range entries {
		name, _ := entry.(string)
		named[name] = true
	}
	require.True(t, named[string(graphview.CapSyntaxGraph)],
		"base_scoped = %v, want it to name %s", rider["base_scoped"], graphview.CapSyntaxGraph)

	// The control: on a base request the corpus IS the answer.
	plain, err := stack.callHandler(t, stack.repoRoot, "find_co_changing_symbols", args,
		stack.srv.handleFindCoChangingSymbols)
	require.NoError(t, err)
	require.NotContains(t, viewResultText(t, plain), "base_scoped",
		"a base answer was annotated as base-scoped")
}

// TestTheCoChangeHandlerAsksThroughTheViewGate is the production-entrypoint
// trace for the refusal above: `ensureCoChangeForRequest` is not a primitive
// that happens to exist — it is the only door the request path has.
//
// The test above drives the primitive directly, which says nothing about which
// function the handler calls. Here the whole middleware runs: a
// find_co_changing_symbols with `refresh:true` arrives with the worktree as its
// cwd, exactly as an agent working in a routed checkout sends it, and the
// corpus-wide git sweep it would otherwise start must not happen. The base
// control is the other half — the gate is a view test, not an off switch.
//
// Revert-red: call `s.ensureCoChange()` unconditionally at
// tools_cochange.go's refresh branch and the routed half fails, because a
// routed request again sweeps every tracked live worktree and writes
// generation zero.
func TestTheCoChangeHandlerAsksThroughTheViewGate(t *testing.T) {
	t.Run("routed", func(t *testing.T) {
		stack := newViewStack(t)
		res, err := stack.callHandler(t, stack.worktreeRoot, "find_co_changing_symbols",
			map[string]any{"file_path": "repo/keep.go", "refresh": true},
			stack.srv.handleFindCoChangingSymbols)
		require.NoError(t, err)
		require.False(t, res.IsError, viewResultText(t, res))

		// Consuming the one-shot here is the measurement: it runs the body
		// only if the handler left it unconsumed.
		intact := false
		stack.srv.cochangeOnce.Do(func() { intact = true })
		require.True(t, intact,
			"a routed find_co_changing_symbols started the corpus-wide co-change mine")
	})

	t.Run("base", func(t *testing.T) {
		stack := newViewStack(t)
		res, err := stack.callHandler(t, stack.repoRoot, "find_co_changing_symbols",
			map[string]any{"file_path": "repo/keep.go", "refresh": true},
			stack.srv.handleFindCoChangingSymbols)
		require.NoError(t, err)
		require.False(t, res.IsError, viewResultText(t, res))

		// The mine is fire-and-forget; wait for it before the fixture store
		// closes under it. Completion is what publishes the caches.
		require.Eventually(t, stack.srv.coChangeReady, 30*time.Second, 10*time.Millisecond,
			"the base request started no co-change mine at all; the gate is an off switch")

		consumed := true
		stack.srv.cochangeOnce.Do(func() { consumed = false })
		require.True(t, consumed,
			"the base request left the one-shot unconsumed")
	})
}
