package mcp

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// A `view:{kind:"base"}` selector names one graph out of a corpus that holds
// every tracked repository. The fixture indexes two — "repo", which the graph
// under test owns, and "other", which it does not — so every assertion below
// is about that boundary: what the label promised, and what the request read.

const (
	// baseSelectorOwnNode lives in the graph the selector names.
	baseSelectorOwnNode = "repo/edit.go::Old"
	// baseSelectorForeignNode lives only in the sibling repository, which no
	// answer about this graph may contain.
	baseSelectorForeignNode = "other/other.go::Other"
	baseSelectorForeignFile = "other/other.go"
	baseSelectorForeignRepo = "other"
)

// countForeignNodes / countForeignEdges report how much of the sibling
// repository a lane leaked. Counting instead of failing fast keeps every lane
// below independently observable, so a revert shows which ones it reopened.
func countForeignNodes(nodes []*graph.Node) int {
	foreign := 0
	for _, node := range nodes {
		if node != nil && node.RepoPrefix == baseSelectorForeignRepo {
			foreign++
		}
	}
	return foreign
}

func countForeignEdges(edges []*graph.Edge) int {
	foreign := 0
	for _, edge := range edges {
		if edge != nil && strings.HasPrefix(edge.FilePath, baseSelectorForeignRepo+"/") {
			foreign++
		}
	}
	return foreign
}

func baseSelectorArgs(graphID string, extra map[string]any) map[string]any {
	args := map[string]any{"view": map[string]any{"kind": "base", "graph_id": graphID}}
	maps.Copy(args, extra)
	return args
}

// TestBaseSelectorNarrowsTheReaderToItsGraph is the production trace: one call
// through the whole tool middleware, asking what the leaf handler read.
//
// The control runs first. Without it a green narrowing assertion could mean
// the fixture never held the sibling repository at all, which would make the
// interesting half of the test vacuous.
func TestBaseSelectorNarrowsTheReaderToItsGraph(t *testing.T) {
	stack := newViewStack(t)

	var whole graph.Reader
	if _, err := stack.callWithView(t, stack.repoRoot, "get_symbol", nil, captureReader(stack.srv, &whole)); err != nil {
		t.Fatalf("call with no view named: %v", err)
	}
	if !hasNode(whole, baseSelectorForeignNode) {
		t.Fatalf("the corpus does not hold %s, so narrowing it proves nothing", baseSelectorForeignNode)
	}

	var scoped graph.Reader
	res, err := stack.callWithView(t, stack.repoRoot, "get_symbol",
		baseSelectorArgs(stack.graphID, nil), captureReader(stack.srv, &scoped))
	if err != nil {
		t.Fatalf("call under the base selector: %v", err)
	}
	if res.IsError {
		t.Fatalf("the base selector was refused: %s", viewResultText(t, res))
	}
	if scoped == nil {
		t.Fatal("the request read through no reader at all")
	}
	if hasNode(scoped, baseSelectorForeignNode) {
		t.Errorf("a base view of %s served %s, which lives in another graph", stack.graphID, baseSelectorForeignNode)
	}
	if !hasNode(scoped, baseSelectorOwnNode) {
		t.Errorf("the base view of %s did not serve its own %s", stack.graphID, baseSelectorOwnNode)
	}

	// The rider still names the graph, and still exactly — that claim is what
	// the narrowing above makes true rather than decorative.
	rider := resultFreshness(t, res)
	if rider["exact"] != true || rider["graph_id"] != stack.graphID {
		t.Errorf("rider = %v, want an exact answer about graph %s", rider, stack.graphID)
	}
	if rider["actual_view"] != "base:"+stack.graphID {
		t.Errorf("actual_view = %v, want base:%s", rider["actual_view"], stack.graphID)
	}
}

// TestBaseGraphReaderNarrowsEveryLane walks the Reader surface. One narrowed
// lane and a dozen open ones is not a narrowed view: a caller reaches nodes by
// name, by file, by repository, by id batch, by kind scan and by whole-corpus
// scan, and every one of those has to answer about this graph alone.
func TestBaseGraphReaderNarrowsEveryLane(t *testing.T) {
	stack := newViewStack(t)
	var scoped graph.Reader
	if _, err := stack.callWithView(t, stack.repoRoot, "get_symbol",
		baseSelectorArgs(stack.graphID, nil), captureReader(stack.srv, &scoped)); err != nil {
		t.Fatalf("call: %v", err)
	}
	if scoped == nil {
		t.Fatal("the base selector produced no reader")
	}

	// The kind-scan lane needs a kind the sibling repository actually has an
	// edge of, or the assertion below would hold on an empty iterator. It is
	// read off the unnarrowed corpus, so the control cannot drift from the
	// fixture.
	var whole graph.Reader
	if _, err := stack.callWithView(t, stack.repoRoot, "get_symbol", nil, captureReader(stack.srv, &whole)); err != nil {
		t.Fatalf("call with no view named: %v", err)
	}
	foreignKind := graph.EdgeKind("")
	for _, edge := range whole.AllEdges() {
		if edge != nil && strings.HasPrefix(edge.FilePath, baseSelectorForeignRepo+"/") {
			foreignKind = edge.Kind
			break
		}
	}
	if foreignKind == "" {
		t.Fatal("the corpus holds no edge of the sibling repository, so the edge lanes prove nothing")
	}

	if node := scoped.GetNode(baseSelectorForeignNode); node != nil {
		t.Errorf("GetNode served a foreign node: %+v", node)
	}
	if nodes := scoped.FindNodesByName("Other"); len(nodes) != 0 {
		t.Errorf("FindNodesByName served %d foreign nodes", len(nodes))
	}
	if nodes := scoped.FindNodesByNameContaining("Othe", 10); len(nodes) != 0 {
		t.Errorf("FindNodesByNameContaining served %d foreign nodes", len(nodes))
	}
	if nodes := scoped.GetFileNodes(baseSelectorForeignFile); len(nodes) != 0 {
		t.Errorf("GetFileNodes served %d nodes of a foreign file", len(nodes))
	}
	if nodes := scoped.GetRepoNodes(baseSelectorForeignRepo); len(nodes) != 0 {
		t.Errorf("GetRepoNodes served %d nodes of a foreign repository", len(nodes))
	}
	if found := scoped.GetNodesByIDs([]string{baseSelectorOwnNode, baseSelectorForeignNode}); found[baseSelectorForeignNode] != nil {
		t.Error("GetNodesByIDs hydrated a foreign node")
	}
	if foreign := countForeignNodes(scoped.AllNodes()); foreign > 0 {
		t.Errorf("AllNodes served %d foreign nodes", foreign)
	}
	byKind := make([]*graph.Node, 0, 8)
	for node := range scoped.NodesByKind(graph.KindFunction) {
		byKind = append(byKind, node)
	}
	if foreign := countForeignNodes(byKind); foreign > 0 {
		t.Errorf("NodesByKind served %d foreign nodes", foreign)
	}

	// Adjacency is anchored: a walk may not start at a node this view does not
	// hold, whichever direction it runs in and however it is batched.
	if edges := scoped.GetOutEdges(baseSelectorForeignFile); len(edges) != 0 {
		t.Errorf("GetOutEdges walked a foreign anchor: %d edges", len(edges))
	}
	if edges := scoped.GetInEdges(baseSelectorForeignNode); len(edges) != 0 {
		t.Errorf("GetInEdges walked a foreign anchor: %d edges", len(edges))
	}
	if walked := scoped.GetOutEdgesByNodeIDs([]string{baseSelectorForeignFile}); len(walked[baseSelectorForeignFile]) != 0 {
		t.Errorf("GetOutEdgesByNodeIDs walked a foreign anchor: %v", walked)
	}
	if walked := scoped.GetInEdgesByNodeIDs([]string{baseSelectorForeignNode}); len(walked[baseSelectorForeignNode]) != 0 {
		t.Errorf("GetInEdgesByNodeIDs walked a foreign anchor: %v", walked)
	}
	if foreign := countForeignEdges(scoped.AllEdges()); foreign > 0 {
		t.Errorf("AllEdges served %d foreign edges", foreign)
	}
	byEdgeKind := make([]*graph.Edge, 0, 8)
	for edge := range scoped.EdgesByKind(foreignKind) {
		byEdgeKind = append(byEdgeKind, edge)
	}
	if foreign := countForeignEdges(byEdgeKind); foreign > 0 {
		t.Errorf("EdgesByKind(%s) served %d foreign edges", foreignKind, foreign)
	}

	// And the view still holds its own graph: a filter that empties everything
	// would pass every assertion above.
	if scoped.GetNode(baseSelectorOwnNode) == nil {
		t.Errorf("the view lost its own %s", baseSelectorOwnNode)
	}
	if nodes := scoped.GetFileNodes("repo/edit.go"); len(nodes) == 0 {
		t.Error("the view lost its own file's nodes")
	}
	if nodes := scoped.GetRepoNodes("repo"); len(nodes) == 0 {
		t.Error("the view lost its own repository's nodes")
	}
	if edges := scoped.GetOutEdges("repo/edit.go"); len(edges) == 0 {
		t.Error("the view lost its own file's outgoing edges")
	}
	if stats := scoped.RepoStats(); len(stats) > 1 {
		t.Errorf("RepoStats reported %d repositories for a one-repository view", len(stats))
	}
}

// TestBaseSelectorDeclaresWhatNarrowingCosts closes the other half of the
// defect: the selector used to be exempt from the capability contract
// entirely, so a caller could require anything of it and be told nothing.
//
// Narrowing has a price and the view states it: cross-repository resolution is
// incomplete because the repositories those references reach into are exactly
// what the view removed. Text search is NOT one of the prices — the trigram
// fan-out takes a repository allow-set, so the narrowing has an exact
// expression there (view_search_text.go, searchTextInNarrowedBase) and the
// capability is served rather than withdrawn.
func TestBaseSelectorDeclaresWhatNarrowingCosts(t *testing.T) {
	stack := newViewStack(t)

	t.Run("a required cross-repo resolution is refused as incomplete", func(t *testing.T) {
		res, err := stack.callWithView(t, stack.repoRoot, "get_symbol",
			baseSelectorArgs(stack.graphID, map[string]any{
				requiredCapabilitiesArgName: string(graphview.CapResolutionCrossRepo),
			}), captureReader(stack.srv, new(graph.Reader)))
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		assertToolError(t, res, graphview.CodeRequiredCapabilityIncomplete)
		if text := viewResultText(t, res); !strings.Contains(text, string(graphview.CapResolutionCrossRepo)) {
			t.Errorf("the refusal does not name the capability:\n%s", text)
		}
	})

	// Text search used to be declared unavailable here, which refused a search
	// the view can narrow exactly. The declaration and the handler now agree
	// that it is served; TestBaseSelectorTextSearchNarrowsToItsOwnRepository is
	// the proof that the answer really is narrowed.
	t.Run("a required text search is served", func(t *testing.T) {
		res, err := stack.callWithView(t, stack.repoRoot, "get_symbol",
			baseSelectorArgs(stack.graphID, map[string]any{
				requiredCapabilitiesArgName: string(graphview.CapSearchText),
			}), captureReader(stack.srv, new(graph.Reader)))
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if res.IsError {
			t.Fatalf("a base selector refused %s: %s", graphview.CapSearchText, viewResultText(t, res))
		}
	})

	t.Run("an optional cross-repo resolution rides back as degraded", func(t *testing.T) {
		res, err := stack.callWithView(t, stack.repoRoot, "get_symbol",
			baseSelectorArgs(stack.graphID, map[string]any{
				optionalCapabilitiesArgName: string(graphview.CapResolutionCrossRepo),
			}), captureReader(stack.srv, new(graph.Reader)))
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if res.IsError {
			t.Fatalf("an optional capability failed the request: %s", viewResultText(t, res))
		}
		text := viewResultText(t, res)
		if !strings.Contains(text, "degraded_capabilities") ||
			!strings.Contains(text, string(graphview.CapResolutionCrossRepo)) ||
			!strings.Contains(text, string(graphview.StateIncomplete)) {
			t.Errorf("the rider does not report the narrowing:\n%s", text)
		}
	})

	// A capability the narrowing does not touch is still served, so the two
	// refusals above are statements about this view and not a blanket denial.
	t.Run("what narrowing does not cost is still served", func(t *testing.T) {
		res, err := stack.callWithView(t, stack.repoRoot, "get_symbol",
			baseSelectorArgs(stack.graphID, map[string]any{
				requiredCapabilitiesArgName: string(graphview.CapSyntaxGraph) + "," + string(graphview.CapResolutionLocal),
			}), captureReader(stack.srv, new(graph.Reader)))
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if res.IsError {
			t.Fatalf("a capability the base graph serves was refused: %s", viewResultText(t, res))
		}
	})
}

// TestBaseSelectorTextSearchAnswersFromItsOwnRepositoryOnly takes the same
// claim through the real handler.
//
// Before the narrowing, search_text under a base selector fell through to the
// canonical searchers — every tracked repository at once — while the rider said
// exact, graph_id:X. The first fix withdrew the capability, which refused a
// search the selector can answer precisely; this one pins the allow-set to the
// selector's own repository instead, so the answer is the view's and nobody
// else's. The sibling repository holds the query too, which is what makes the
// absence below a narrowing rather than an empty corpus.
func TestBaseSelectorTextSearchAnswersFromItsOwnRepositoryOnly(t *testing.T) {
	stack := newViewStack(t)
	const query = "func "

	// The session narrows by workspace, and the two fixture repositories are in
	// different ones, so both calls run from the directory above both: there the
	// session narrows nothing and the view selector is the only difference.
	wide := filepath.Dir(stack.repoRoot)
	plain, err := stack.callHandler(t, wide, "search_text",
		map[string]any{"query": query}, stack.srv.handleSearchText)
	if err != nil {
		t.Fatalf("search the canonical checkouts: %v", err)
	}
	if plain.IsError {
		t.Fatalf("the canonical search failed: %s", viewResultText(t, plain))
	}
	whole := searchTextMatchPaths(t, plain)
	if !containsPathIn(whole, baseSelectorForeignRepo+"/") {
		t.Fatalf("the canonical search answered %v, which holds nothing of %s", whole, baseSelectorForeignRepo)
	}

	res, err := stack.callHandler(t, wide, "search_text",
		baseSelectorArgs(stack.graphID, map[string]any{"query": query}), stack.srv.handleSearchText)
	if err != nil {
		t.Fatalf("search under the base selector: %v", err)
	}
	if res.IsError {
		t.Fatalf("the base selector refused a search it can narrow: %s", viewResultText(t, res))
	}
	scoped := searchTextMatchPaths(t, res)
	if len(scoped) == 0 {
		t.Fatalf("the base selector answered nothing: %s", viewResultText(t, res))
	}
	if containsPathIn(scoped, baseSelectorForeignRepo+"/") {
		t.Errorf("a base view of %s answered out of %s: %v", stack.graphID, baseSelectorForeignRepo, scoped)
	}
}

// baseSelectorCrossRepoCaller is a symbol of the sibling repository that calls
// into the graph under test. It exists only in "other", so its id is exactly
// what a base view of "repo" must never put on the wire.
const (
	baseSelectorCrossRepoCaller = "other/other.go::Caller"
	baseSelectorCrossRepoTarget = "repo/keep.go::Keeper"
)

// seedCrossRepoCall records one call from the sibling repository into this
// graph, which is the shape a cross-repository reference actually has in the
// corpus: an edge whose site is the *other* repository's file.
func seedCrossRepoCall(t *testing.T, stack *viewStack) {
	t.Helper()
	stack.store.AddBatch([]*graph.Node{
		{
			ID:         baseSelectorCrossRepoCaller,
			Kind:       graph.KindFunction,
			Name:       "Caller",
			QualName:   "other.Caller",
			FilePath:   baseSelectorForeignFile,
			RepoPrefix: baseSelectorForeignRepo,
			Language:   "go",
			StartLine:  5,
			EndLine:    7,
		},
	}, []*graph.Edge{
		{
			From:     baseSelectorCrossRepoCaller,
			To:       baseSelectorCrossRepoTarget,
			Kind:     graph.EdgeCalls,
			FilePath: baseSelectorForeignFile,
			Line:     6,
		},
	})
}

// TestBaseSelectorEdgesNeverNameAnotherGraphsSymbol takes the acceptance
// criterion through the production entrypoint: a
// view:{kind:"base",graph_id:A} request must not return a symbol that lives
// only in graph B.
//
// Refusing to hydrate the far node is not enough on its own. find_usages
// renders the edge list, and query.Engine.FindUsagesScoped appends an edge
// whether or not its `from` hydrated — so a reader that filtered nodes but
// returned an in-scope anchor's edges whole put the foreign symbol id in the
// answer while omitting it from `nodes`. The control call proves the corpus
// really carries that usage, so the narrowed assertion is not vacuous.
func TestBaseSelectorEdgesNeverNameAnotherGraphsSymbol(t *testing.T) {
	stack := newViewStack(t)
	seedCrossRepoCall(t, stack)

	// No session cwd: a cwd inside "repo" resolves a workspace scope of its
	// own, and the sibling repository lives in a different workspace, so the
	// control would be filtered by scope resolution rather than by this view.
	plain, err := stack.callHandler(t, "", "find_usages",
		map[string]any{"id": baseSelectorCrossRepoTarget}, stack.srv.handleFindUsages)
	if err != nil {
		t.Fatalf("find_usages with no view named: %v", err)
	}
	if plain.IsError {
		t.Fatalf("the unnarrowed usage query failed: %s", viewResultText(t, plain))
	}
	if !strings.Contains(viewResultText(t, plain), baseSelectorCrossRepoCaller) {
		t.Fatalf("the corpus does not report %s as a caller, so narrowing it proves nothing:\n%s",
			baseSelectorCrossRepoCaller, viewResultText(t, plain))
	}

	res, err := stack.callHandler(t, "", "find_usages",
		baseSelectorArgs(stack.graphID, map[string]any{"id": baseSelectorCrossRepoTarget}),
		stack.srv.handleFindUsages)
	if err != nil {
		t.Fatalf("find_usages under the base selector: %v", err)
	}
	if res.IsError {
		t.Fatalf("the base selector was refused: %s", viewResultText(t, res))
	}
	if text := viewResultText(t, res); strings.Contains(text, baseSelectorCrossRepoCaller) {
		t.Errorf("a base view of %s returned %s, which lives only in the sibling repository:\n%s",
			stack.graphID, baseSelectorCrossRepoCaller, text)
	}

	// The reader lane the handler reads through says the same thing directly,
	// so a later renderer change cannot quietly reopen this.
	var scoped graph.Reader
	if _, err := stack.callWithView(t, "", "get_symbol",
		baseSelectorArgs(stack.graphID, nil), captureReader(stack.srv, &scoped)); err != nil {
		t.Fatalf("capture the narrowed reader: %v", err)
	}
	for _, edge := range scoped.GetInEdges(baseSelectorCrossRepoTarget) {
		if edge != nil && edge.From == baseSelectorCrossRepoCaller {
			t.Errorf("GetInEdges kept an edge whose far end is %s", edge.From)
		}
	}
	for _, edge := range scoped.GetInEdgesByNodeIDs([]string{baseSelectorCrossRepoTarget})[baseSelectorCrossRepoTarget] {
		if edge != nil && edge.From == baseSelectorCrossRepoCaller {
			t.Errorf("GetInEdgesByNodeIDs kept an edge whose far end is %s", edge.From)
		}
	}
	// And the local edges of the same anchor survive: an endpoint filter that
	// dropped everything would pass every assertion above.
	if edges := scoped.GetInEdges(baseSelectorCrossRepoTarget); len(edges) == 0 {
		t.Error("the endpoint filter dropped this repository's own incoming edges too")
	}
}

// TestBaseSelectorStillAdmitsSourceEdits pins the one thing narrowing the
// reader must NOT change.
//
// refuseRoutedViewMutation gates on routed(), which is reader-presence, so
// giving the base selector a reader would have silently converted every
// edit under view:{kind:"base"} into a view_read_only refusal — a mutation
// policy change nobody asked this item for. The gate exists because a routed
// reader reads some other checkout's bytes than the path resolvers anchor to;
// a base selector resolves paths exactly as an unrouted request does, so it
// writes where it always wrote.
func TestBaseSelectorStillAdmitsSourceEdits(t *testing.T) {
	stack := newViewStack(t)
	if !stack.srv.facades.mutatesSource("edit_file") {
		t.Fatal("edit_file is not classified as a source mutation, so this gate proves nothing")
	}

	reached := false
	res, err := stack.callWithView(t, stack.repoRoot, "edit_file",
		baseSelectorArgs(stack.graphID, nil),
		func(context.Context) (*mcplib.CallToolResult, error) {
			reached = true
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	if err != nil {
		t.Fatalf("edit_file under the base selector: %v", err)
	}
	if res.IsError {
		t.Fatalf("the base selector refused a source edit: %s", viewResultText(t, res))
	}
	if !reached {
		t.Error("the mutation gate stopped an edit_file the base corpus used to admit")
	}

	// The gate is not disarmed: an inexact answer is still read-only, which is
	// the case the same function refuses one branch earlier.
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, 0, store_sqlite.RouteActive)
	fallbackReached := false
	refused, err := stack.callWithView(t, stack.worktreeRoot, "edit_file", nil,
		func(context.Context) (*mcplib.CallToolResult, error) {
			fallbackReached = true
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	if err != nil {
		t.Fatalf("edit_file on a fallback: %v", err)
	}
	assertToolError(t, refused, graphview.CodeViewReadOnly)
	if fallbackReached {
		t.Error("a read-only fallback admitted a source edit")
	}
}

// TestBaseSelectorDeclinesToClaimLanguageServerCompleteness covers the other
// direction of truthfulness: not refusing what the view serves, but refusing
// to *assert* what nothing evidences.
//
// A base graph carries no producer rows — that is why baseCorpusCompleteness
// has to assume at all — and binds no checkout of its own, so nothing behind
// this view says a language server ran over this repository. Declaring the
// five LSP capabilities complete would put a positive claim with nothing
// behind it on the one view whose rider says exact.
func TestBaseSelectorDeclinesToClaimLanguageServerCompleteness(t *testing.T) {
	for _, capability := range lspCapabilities() {
		t.Run(string(capability), func(t *testing.T) {
			stack := newViewStack(t)
			res, err := stack.callWithView(t, stack.repoRoot, "get_symbol",
				baseSelectorArgs(stack.graphID, map[string]any{
					requiredCapabilitiesArgName: string(capability),
				}), captureReader(stack.srv, new(graph.Reader)))
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			assertToolError(t, res, graphview.CodeRequiredCapabilityIncomplete)
			if text := viewResultText(t, res); !strings.Contains(text, string(capability)) {
				t.Errorf("the refusal does not name the capability:\n%s", text)
			}
		})
	}

	// Optional, it rides back instead of failing the request — the same
	// treatment cross-repository resolution gets, and the proof that this is a
	// withdrawn claim rather than a blanket denial of the LSP family.
	t.Run("optional rides back as degraded", func(t *testing.T) {
		stack := newViewStack(t)
		res, err := stack.callWithView(t, stack.repoRoot, "get_symbol",
			baseSelectorArgs(stack.graphID, map[string]any{
				optionalCapabilitiesArgName: string(graphview.CapLSPHover),
			}), captureReader(stack.srv, new(graph.Reader)))
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if res.IsError {
			t.Fatalf("an optional capability failed the request: %s", viewResultText(t, res))
		}
		text := viewResultText(t, res)
		if !strings.Contains(text, "degraded_capabilities") ||
			!strings.Contains(text, string(graphview.CapLSPHover)) {
			t.Errorf("the rider does not report the withdrawn claim:\n%s", text)
		}
	})
}

// TestNonStrictFallbackEvaluatesItsCapabilityContract is the second half of
// the item: evaluateRequestCapabilities used to return early for every
// reader-less view, which exempted fallbacks as well as base selectors.
//
// Replacing the exemption with a declaration is only worth something if the
// declaration says something. A fallback answered this request from the base
// corpus while the caller named a checkout it did not get, and text search
// runs over the canonical checkouts on disk — which are precisely not that
// checkout's bytes. That one capability is declared incomplete, so a restored
// exemption is observable rather than byte-identical.
func TestNonStrictFallbackEvaluatesItsCapabilityContract(t *testing.T) {
	stack := newViewStack(t)
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, 0, store_sqlite.RouteActive)

	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol",
		map[string]any{optionalCapabilitiesArgName: string(graphview.CapSearchText)},
		captureReader(stack.srv, new(graph.Reader)))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("an optional capability failed a fallback: %s", viewResultText(t, res))
	}
	rider := resultFreshness(t, res)
	if rider == nil || rider["exact"] != false {
		t.Fatalf("this is not the fallback shape the test needs: %v", rider)
	}
	text := viewResultText(t, res)
	if !strings.Contains(text, "degraded_capabilities") ||
		!strings.Contains(text, string(graphview.CapSearchText)) ||
		!strings.Contains(text, string(graphview.StateIncomplete)) {
		t.Errorf("a fallback's capability contract was not evaluated:\n%s", text)
	}
}

// TestUndeclaredReaderlessViewIsServedAndAnnotated exercises the defensive
// arm of the closed exemption directly.
//
// No production producer builds a reader-less view with no declaration today,
// so this is the only way to reach it — and it must stay reachable in test,
// because the alternative for a future producer that forgets a declaration is
// silent: a nil Completeness denies every capability it is asked about, so the
// omission would surface as a blanket refusal instead of as this annotation.
func TestUndeclaredReaderlessViewIsServedAndAnnotated(t *testing.T) {
	stack := newViewStack(t)
	view := &requestView{rider: graphview.NewViewRider(graphview.Selector{Kind: graphview.SelectorBase})}
	if view.completeness() != nil || view.routed() {
		t.Fatal("the fixture is not the undeclared reader-less shape this arm is for")
	}
	ctx := withRequestView(context.Background(), view)
	req := &mcplib.CallToolRequest{}
	req.Params.Name = "get_symbol"

	if res := stack.srv.evaluateRequestCapabilities(ctx, req, capabilityRequest{
		required: []graphview.CapabilityID{graphview.CapSyntaxGraph},
	}); res != nil {
		t.Fatalf("an undeclared reader-less view refused a requirement: %s", viewResultText(t, res))
	}
	_, baseScoped := view.annotations()
	if !slices.Contains(baseScoped, graphview.CapSyntaxGraph) {
		t.Errorf("base_scoped = %v, want it to name the capability that was answered unchecked", baseScoped)
	}
}

// corpusStub overrides one lane of a real reader. Embedding the interface
// keeps every other lane exactly what the corpus does, so a test can pose one
// awkward backend shape without hand-writing graph.Reader.
type corpusStub struct {
	graph.Reader
	noRepoStats bool
	repoStats   map[string]graph.GraphStats
	byName      []*graph.Node
	asked       []int
}

func (c *corpusStub) RepoStats() map[string]graph.GraphStats {
	switch {
	case c.noRepoStats:
		return nil
	case c.repoStats != nil:
		return c.repoStats
	default:
		return c.Reader.RepoStats()
	}
}

func (c *corpusStub) FindNodesByNameContaining(substr string, limit int) []*graph.Node {
	if c.byName == nil {
		return c.Reader.FindNodesByNameContaining(substr, limit)
	}
	c.asked = append(c.asked, limit)
	if limit <= 0 || limit > len(c.byName) {
		return c.byName
	}
	return c.byName[:limit]
}

// TestBaseGraphReaderCountersNeverReportTheCorpus covers the counters lane.
//
// graph.Graph.RepoStats skips the empty prefix, so a corpus whose nodes carry
// no repository at all reports no per-repo rollup. Borrowing the corpus totals
// in that shape would put every repository's counters behind a rider that says
// exact:true, graph_id:X — a whole-corpus answer wearing this view's label.
func TestBaseGraphReaderCountersNeverReportTheCorpus(t *testing.T) {
	corpus := graph.New()
	corpus.AddBatch([]*graph.Node{
		{ID: "repo/a.go::One", Kind: graph.KindFunction, Name: "One", FilePath: "repo/a.go", Language: "go"},
		{ID: "repo/a.go::Two", Kind: graph.KindFunction, Name: "Two", FilePath: "repo/a.go", Language: "go"},
		{ID: "other/b.go::Three", Kind: graph.KindFunction, Name: "Three", FilePath: "other/b.go", Language: "go"},
	}, []*graph.Edge{
		{From: "repo/a.go::One", To: "repo/a.go::Two", Kind: graph.EdgeCalls, FilePath: "repo/a.go", Line: 2},
		{From: "other/b.go::Three", To: "other/b.go::Three", Kind: graph.EdgeCalls, FilePath: "other/b.go", Line: 2},
	})
	stub := &corpusStub{Reader: corpus, noRepoStats: true}
	if len(stub.RepoStats()) != 0 {
		t.Fatal("the stub still reports a per-repo rollup, so the derived path is unreachable")
	}
	corpusTotal := corpus.Stats().TotalNodes
	if corpusTotal != 3 {
		t.Fatalf("the corpus holds %d nodes, want the 3 the fixture wrote", corpusTotal)
	}

	scoped := newBaseGraphReader(stub, "repo")
	if scoped == nil {
		t.Fatal("the reader refused to narrow a prefix it was given")
	}
	stats := scoped.Stats()
	if stats.TotalNodes != 2 {
		t.Errorf("Stats().TotalNodes = %d, want the view's own 2 (corpus holds %d)", stats.TotalNodes, corpusTotal)
	}
	if stats.TotalEdges != 1 {
		t.Errorf("Stats().TotalEdges = %d, want the view's own 1", stats.TotalEdges)
	}
	if scoped.NodeCount() != 2 || scoped.EdgeCount() != 1 {
		t.Errorf("NodeCount/EdgeCount = %d/%d, want 2/1", scoped.NodeCount(), scoped.EdgeCount())
	}
}

// TestBaseGraphReaderFillsTheNameLimitPastForeignMatches covers the
// name-substring lane.
//
// The corpus ranks every repository's matches together, so a fixed over-fetch
// can be spent entirely on rows this view drops. Answering short there is
// never wrong, but it is silently missing rows the view does hold, with
// nothing on the response to say which limit truncated it.
func TestBaseGraphReaderFillsTheNameLimitPastForeignMatches(t *testing.T) {
	corpus := graph.New()
	ranked := make([]*graph.Node, 0, 600)
	for i := range 500 {
		ranked = append(ranked, &graph.Node{
			ID:         fmt.Sprintf("other/b.go::Match%d", i),
			Kind:       graph.KindFunction,
			Name:       fmt.Sprintf("Match%d", i),
			FilePath:   "other/b.go",
			RepoPrefix: "other",
		})
	}
	for i := range 5 {
		ranked = append(ranked, &graph.Node{
			ID:         fmt.Sprintf("repo/a.go::Match%d", i),
			Kind:       graph.KindFunction,
			Name:       fmt.Sprintf("Match%d", i),
			FilePath:   "repo/a.go",
			RepoPrefix: "repo",
		})
	}
	stub := &corpusStub{Reader: corpus, byName: ranked}
	scoped := newBaseGraphReader(stub, "repo")
	if scoped == nil {
		t.Fatal("the reader refused to narrow a prefix it was given")
	}

	got := scoped.FindNodesByNameContaining("Match", 3)
	if len(got) != 3 {
		t.Errorf("FindNodesByNameContaining answered %d rows for a limit of 3, though the view holds 5", len(got))
	}
	if foreign := countForeignNodes(got); foreign > 0 {
		t.Errorf("the widened scan let %d foreign rows through", foreign)
	}
	if len(stub.asked) < 2 {
		t.Errorf("the scan asked the corpus %v and never widened", stub.asked)
	}
}

// TestBaseSelectorIsCountedAsABaseViewServed pins the routing telemetry
// against the narrowing.
//
// views_request_served_total is how an operator reads what the seam is doing,
// and its kind label used to be inferred from reader-presence: no reader meant
// the indexed corpus answered. Giving the base selector a reader of its own
// broke that inference — every base-labelled request would be counted as a
// routed worktree, which is a base answer wearing another view's label in the
// measurement surface. The capture below is what makes this test about that:
// the request really did read through the narrowed reader, and it is still
// counted as base.
func TestBaseSelectorIsCountedAsABaseViewServed(t *testing.T) {
	stack := newViewStack(t)

	var scoped graph.Reader
	before := viewmetrics.Read()
	res, err := stack.callWithView(t, stack.repoRoot, "get_symbol",
		baseSelectorArgs(stack.graphID, nil), captureReader(stack.srv, &scoped))
	if err != nil {
		t.Fatalf("call under the base selector: %v", err)
	}
	after := viewmetrics.Read()
	if res.IsError {
		t.Fatalf("the base selector was refused: %s", viewResultText(t, res))
	}
	if scoped == nil {
		t.Fatal("the request read through no reader, so this says nothing about the narrowed shape")
	}
	if hasNode(scoped, baseSelectorForeignNode) {
		t.Fatal("the reader was not narrowed, so this says nothing about the narrowed shape")
	}

	if got := servedDelta(before, after, viewmetrics.ViewBase); got != 1 {
		t.Errorf("base views served = %d, want 1", got)
	}
	if got := servedDelta(before, after, viewmetrics.ViewWorktree); got != 0 {
		t.Errorf("a base selector was counted as a routed worktree (%d)", got)
	}
	if got := servedDelta(before, after, viewmetrics.ViewRef); got != 0 {
		t.Errorf("a base selector was counted as a committed tree (%d)", got)
	}
	for _, reason := range viewmetrics.ViewErrorCodes {
		if got := fallbackDelta(before, after, reason); got != 0 {
			t.Errorf("an exact base answer counted a %s fallback (%d)", reason, got)
		}
	}
}

// TestRequestViewKindReadsTheProducersLabel covers the classifier directly,
// including the two shapes no production producer builds today.
//
// The kind is stated by the producer because no field of the view implies it
// any more. What is left of the inference is the fallback for a producer that
// states nothing — the committed-tree view is the only one — and it must not
// answer "base" for a routed view that simply forgot its label, which would
// under-count routed traffic instead of over-counting it.
func TestRequestViewKindReadsTheProducersLabel(t *testing.T) {
	narrowed := newBaseGraphReader(graph.New(), "repo")
	if narrowed == nil {
		t.Fatal("the reader refused to narrow a prefix it was given")
	}
	cases := []struct {
		name string
		view *requestView
		want string
	}{
		{"no view at all", nil, viewmetrics.ViewBase},
		{"unrouted base", &requestView{kind: viewmetrics.ViewBase}, viewmetrics.ViewBase},
		{
			"narrowed base selector",
			&requestView{kind: viewmetrics.ViewBase, reader: narrowed, baseNarrowed: true},
			viewmetrics.ViewBase,
		},
		{
			"routed worktree",
			&requestView{kind: viewmetrics.ViewWorktree, reader: narrowed, viewRoot: "/tmp/wt"},
			viewmetrics.ViewWorktree,
		},
		{"unlabelled committed tree", &requestView{reader: narrowed, files: &refViewFiles{}}, viewmetrics.ViewRef},
		{"unlabelled routed view", &requestView{reader: narrowed}, viewmetrics.ViewWorktree},
		{"unlabelled readerless view", &requestView{}, viewmetrics.ViewBase},
		// An off-vocabulary label must never reach the counter. viewmetrics
		// declares {base, worktree, ref} for RequestServedTotal and accepts
		// whatever string it is handed, so a typo at a producer would ship a
		// series nobody declared. It is classified by the inference instead,
		// which is the same answer that producer got before it was labelled.
		{"off-vocabulary label on a readerless view", &requestView{kind: "checkout"}, viewmetrics.ViewBase},
		{
			"off-vocabulary label on a routed view",
			&requestView{kind: "Base", reader: narrowed, viewRoot: "/tmp/wt"},
			viewmetrics.ViewWorktree,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestViewKind(tc.view); got != tc.want {
				t.Errorf("requestViewKind = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBaseGraphReaderCountersAlwaysCarryTheirMaps covers the rollup miss.
//
// A per-repo rollup that carries no row for this graph's prefix used to be
// returned as the zero GraphStats — nil ByKind, nil ByLanguage — while every
// other producer of this type fills both in. A reader is entitled to hand back
// zero counters; it is not entitled to hand back a value shaped differently
// from the one the same method returns on every other path, because a caller
// that counts into the map it was given panics on the nil.
func TestBaseGraphReaderCountersAlwaysCarryTheirMaps(t *testing.T) {
	corpus := graph.New()
	corpus.AddBatch([]*graph.Node{
		{ID: "other/b.go::Three", Kind: graph.KindFunction, Name: "Three", FilePath: "other/b.go", RepoPrefix: "other", Language: "go"},
	}, nil)
	stub := &corpusStub{Reader: corpus, repoStats: map[string]graph.GraphStats{
		"other": {TotalNodes: 1, ByKind: map[string]int{"function": 1}, ByLanguage: map[string]int{"go": 1}},
	}}
	scoped := newBaseGraphReader(stub, "repo")
	if scoped == nil {
		t.Fatal("the reader refused to narrow a prefix it was given")
	}
	if _, found := stub.RepoStats()["repo"]; found {
		t.Fatal("the rollup carries this view's prefix, so the missing-row path is unreachable")
	}

	stats := scoped.Stats()
	if stats.TotalNodes != 0 || stats.TotalEdges != 0 {
		t.Errorf("Stats() = %d nodes / %d edges for a prefix the rollup does not carry, want 0/0",
			stats.TotalNodes, stats.TotalEdges)
	}
	if stats.ByKind == nil {
		t.Error("Stats().ByKind is nil; derivedStats always allocates it")
	}
	if stats.ByLanguage == nil {
		t.Error("Stats().ByLanguage is nil; derivedStats always allocates it")
	}
	if stats.ByKind == nil || stats.ByLanguage == nil {
		return
	}
	// What a nil map costs is a panic in the caller, not a wrong number, so
	// the writes are the assertion.
	stats.ByKind["function"]++
	stats.ByLanguage["go"]++

	// The same guarantee on the rollup lane, for a prefix the rollup does
	// carry but with counters the backend left unbuilt.
	stub.repoStats = map[string]graph.GraphStats{"repo": {TotalNodes: 2}}
	rolled := scoped.Stats()
	if rolled.TotalNodes != 2 {
		t.Errorf("Stats().TotalNodes = %d, want the rollup's own 2", rolled.TotalNodes)
	}
	if rolled.ByKind == nil || rolled.ByLanguage == nil {
		t.Fatal("a rollup row with unbuilt counter maps was passed through with nil maps")
	}
	rolled.ByKind["function"]++
	perRepo := scoped.RepoStats()
	row, found := perRepo["repo"]
	if !found {
		t.Fatal("RepoStats dropped this view's own row")
	}
	if row.ByKind == nil || row.ByLanguage == nil {
		t.Fatal("RepoStats returned a row with nil counter maps")
	}
	row.ByLanguage["go"]++
}

// legacyUnprefixedCorpus poses the corpus shape that predates prefixed
// identities: the nodes are stamped with the repository they were indexed
// under, but their file paths — and the reference sites of the edges between
// them — are spelled relative to the repository root, with no prefix. The
// sibling repository beside them is spelled the modern way, so one corpus
// holds both and every assertion below is about which of the two an edge is.
func legacyUnprefixedCorpus() *graph.Graph {
	corpus := graph.New()
	corpus.AddBatch([]*graph.Node{
		{ID: "edit.go", Kind: graph.KindFile, Name: "edit.go", FilePath: "edit.go", RepoPrefix: "repo", Language: "go"},
		{ID: "edit.go::Old", Kind: graph.KindFunction, Name: "Old", FilePath: "edit.go", RepoPrefix: "repo", Language: "go"},
		{ID: "edit.go::New", Kind: graph.KindFunction, Name: "New", FilePath: "edit.go", RepoPrefix: "repo", Language: "go"},
		{
			ID: baseSelectorForeignNode, Kind: graph.KindFunction, Name: "Other",
			FilePath: baseSelectorForeignFile, RepoPrefix: baseSelectorForeignRepo, Language: "go",
		},
	}, []*graph.Edge{
		// This view's own call, sited the legacy way.
		{From: "edit.go::Old", To: "edit.go::New", Kind: graph.EdgeCalls, FilePath: "edit.go", Line: 3},
		// The sibling repository's own call, sited the modern way.
		{
			From: baseSelectorForeignNode, To: baseSelectorForeignNode, Kind: graph.EdgeCalls,
			FilePath: baseSelectorForeignFile, Line: 3,
		},
		// A reference written in the sibling repository at a symbol this view
		// holds, whose `from` the corpus never resolved. Nothing about its
		// endpoints says "foreign" — only its site does.
		{
			From: "other/other.go::Ghost", To: "edit.go::Old", Kind: graph.EdgeCalls,
			FilePath: baseSelectorForeignFile, Line: 9,
		},
	})
	return corpus
}

func edgeBetween(edges []*graph.Edge, from, to string) bool {
	for _, edge := range edges {
		if edge != nil && edge.From == from && edge.To == to {
			return true
		}
	}
	return false
}

// TestBaseGraphReaderKeepsALegacyUnprefixedEdgeSite closes the asymmetry
// between the two membership predicates.
//
// inScope keeps a node the corpus spells without this repository's prefix when
// what the corpus holds says it is this view's; edgeInScope had no matching
// arm, so on the very same corpus the node lane kept a symbol and the edge
// lane discarded every reference between two such symbols. Under-reporting
// only — but a view that holds both ends of a call and denies the call is not
// the repository the label named.
func TestBaseGraphReaderKeepsALegacyUnprefixedEdgeSite(t *testing.T) {
	scoped := newBaseGraphReader(legacyUnprefixedCorpus(), "repo")
	if scoped == nil {
		t.Fatal("the reader refused to narrow a prefix it was given")
	}
	if scoped.GetNode("edit.go::Old") == nil || scoped.GetNode("edit.go::New") == nil {
		t.Fatal("the node lane does not hold both ends, so the edge lanes prove nothing")
	}

	if !edgeBetween(scoped.AllEdges(), "edit.go::Old", "edit.go::New") {
		t.Error("AllEdges dropped a call between two nodes this view holds, because its site carries no prefix")
	}
	byKind := make([]*graph.Edge, 0, 4)
	for edge := range scoped.EdgesByKind(graph.EdgeCalls) {
		byKind = append(byKind, edge)
	}
	if !edgeBetween(byKind, "edit.go::Old", "edit.go::New") {
		t.Error("EdgesByKind dropped the same call")
	}
	if !edgeBetween(scoped.GetOutEdges("edit.go::Old"), "edit.go::Old", "edit.go::New") {
		t.Error("GetOutEdges dropped the same call")
	}
	if !edgeBetween(scoped.GetInEdges("edit.go::New"), "edit.go::Old", "edit.go::New") {
		t.Error("GetInEdges dropped the same call")
	}
	if !edgeBetween(scoped.GetOutEdgesByNodeIDs([]string{"edit.go::Old"})["edit.go::Old"], "edit.go::Old", "edit.go::New") {
		t.Error("GetOutEdgesByNodeIDs dropped the same call")
	}

	// Keeping an unprefixed site is not keeping every site: the sibling
	// repository is spelled with its prefix and stays out of every lane.
	if foreign := countForeignEdges(scoped.AllEdges()); foreign > 0 {
		t.Errorf("AllEdges served %d edges sited in the sibling repository", foreign)
	}
	if foreign := countForeignEdges(byKind); foreign > 0 {
		t.Errorf("EdgesByKind served %d edges sited in the sibling repository", foreign)
	}
}

// TestBaseGraphReaderDropsAForeignSiteFromTheAnchoredLanes covers the other
// half of the same disagreement, in the other direction.
//
// The whole-corpus scan tests an edge's site and its endpoints; the anchored
// lanes — the ones find_usages actually reads through — tested only the far
// endpoint. An incoming reference written in another repository's file whose
// `from` the corpus never resolved has no foreign node id to catch, so it was
// kept, and the answer carried that repository's file path. The two lanes must
// decide one edge the same way.
func TestBaseGraphReaderDropsAForeignSiteFromTheAnchoredLanes(t *testing.T) {
	corpus := legacyUnprefixedCorpus()
	scoped := newBaseGraphReader(corpus, "repo")
	if scoped == nil {
		t.Fatal("the reader refused to narrow a prefix it was given")
	}
	if corpus.GetNode("other/other.go::Ghost") != nil {
		t.Fatal("the unresolved caller hydrates, so the far-endpoint filter would catch it on its own")
	}
	if !edgeBetween(corpus.GetInEdges("edit.go::Old"), "other/other.go::Ghost", "edit.go::Old") {
		t.Fatal("the corpus does not hold the foreign-sited reference, so narrowing it proves nothing")
	}

	if edgeBetween(scoped.GetInEdges("edit.go::Old"), "other/other.go::Ghost", "edit.go::Old") {
		t.Error("GetInEdges served an edge written in the sibling repository's file")
	}
	if edgeBetween(scoped.GetInEdgesByNodeIDs([]string{"edit.go::Old"})["edit.go::Old"], "other/other.go::Ghost", "edit.go::Old") {
		t.Error("GetInEdgesByNodeIDs served the same edge")
	}
	if edgeBetween(scoped.AllEdges(), "other/other.go::Ghost", "edit.go::Old") {
		t.Error("AllEdges served the same edge")
	}
	// And the anchor keeps its own edges: a lane that dropped everything would
	// pass every assertion above.
	if !edgeBetween(scoped.GetOutEdges("edit.go::Old"), "edit.go::Old", "edit.go::New") {
		t.Error("the site filter dropped this repository's own outgoing edge too")
	}
}

// countingCorpus counts the store round-trips a narrowed lane makes.
//
// The narrowing is a filter, so every question it asks the corpus is a query
// against a disk-backed store in production (the only backend is SQLite). A
// filter that asks one question per row of another repository is not a filter
// a request can afford, and nothing else in the package observes that — so
// this stub makes the round-trips assertable, per door, and the tests below
// name the number.
type countingCorpus struct {
	graph.Reader
	inner *graph.Graph
	// fileNodes counts the per-path door; fileNodeBatches counts the batched
	// one and batchedPaths what it was asked for.
	fileNodes       int
	fileNodeBatches int
	batchedPaths    int
	// nodeLookups counts the per-id hydration door, nodeBatches the batched
	// one.
	nodeLookups int
	nodeBatches int
}

func newCountingCorpus(inner *graph.Graph) *countingCorpus {
	return &countingCorpus{Reader: inner, inner: inner}
}

func (c *countingCorpus) GetFileNodes(filePath string) []*graph.Node {
	c.fileNodes++
	return c.inner.GetFileNodes(filePath)
}

func (c *countingCorpus) GetFileNodesByPaths(filePaths []string) map[string][]*graph.Node {
	c.fileNodeBatches++
	c.batchedPaths += len(filePaths)
	return c.inner.GetFileNodesByPaths(filePaths)
}

func (c *countingCorpus) GetNode(id string) *graph.Node {
	c.nodeLookups++
	return c.inner.GetNode(id)
}

func (c *countingCorpus) GetNodesByIDs(ids []string) map[string]*graph.Node {
	c.nodeBatches++
	return c.inner.GetNodesByIDs(ids)
}

// baseSiteFanoutFiles is how wide the sibling repository is in the fan-out
// fixtures. Large enough that one-lookup-per-path is unmistakable in the
// counters, small enough to stay a unit test.
const baseSiteFanoutFiles = 64

// unresolvedSiteFanoutCorpus is the shape that makes the site half expensive:
// a sibling repository of many files, each holding one reference into this
// view whose caller the corpus never resolved.
//
// Nothing about such an edge's endpoints says "foreign" — the caller hydrates
// to nothing and the callee is this view's — so the endpoint filter keeps every
// one of them and the site is what decides them. That is the population the
// site lookups are paid for.
func unresolvedSiteFanoutCorpus() *graph.Graph {
	corpus := graph.New()
	nodes := []*graph.Node{
		{ID: "repo/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/a.go::Local", Kind: graph.KindFunction, Name: "Local", FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go", StartLine: 3},
		{ID: "repo/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go", StartLine: 7},
	}
	edges := []*graph.Edge{
		{From: "repo/a.go::Caller", To: "repo/a.go::Local", Kind: graph.EdgeCalls, FilePath: "repo/a.go", Line: 8},
	}
	for i := range baseSiteFanoutFiles {
		path := fmt.Sprintf("%s/f%d.go", baseSelectorForeignRepo, i)
		nodes = append(nodes, &graph.Node{
			ID: path, Kind: graph.KindFile, Name: fmt.Sprintf("f%d.go", i),
			FilePath: path, RepoPrefix: baseSelectorForeignRepo, Language: "go",
		})
		edges = append(edges, &graph.Edge{
			From: path + "::Ghost", To: "repo/a.go::Local", Kind: graph.EdgeCalls, FilePath: path, Line: 2,
		})
	}
	corpus.AddBatch(nodes, edges)
	return corpus
}

// resolvedSiblingCorpus is the same width, but every sibling edge is the
// sibling repository's own call: both of its endpoints hydrate there.
func resolvedSiblingCorpus() *graph.Graph {
	corpus := graph.New()
	nodes := []*graph.Node{
		{ID: "repo/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/a.go::Local", Kind: graph.KindFunction, Name: "Local", FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go", StartLine: 3},
		{ID: "repo/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go", StartLine: 7},
	}
	edges := []*graph.Edge{
		{From: "repo/a.go::Caller", To: "repo/a.go::Local", Kind: graph.EdgeCalls, FilePath: "repo/a.go", Line: 8},
	}
	for i := range baseSiteFanoutFiles {
		path := fmt.Sprintf("%s/f%d.go", baseSelectorForeignRepo, i)
		id := fmt.Sprintf("%s::Sibling%d", path, i)
		nodes = append(nodes, &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: fmt.Sprintf("Sibling%d", i),
			FilePath: path, RepoPrefix: baseSelectorForeignRepo, Language: "go", StartLine: 2,
		})
		edges = append(edges, &graph.Edge{From: id, To: id, Kind: graph.EdgeCalls, FilePath: path, Line: 3})
	}
	corpus.AddBatch(nodes, edges)
	return corpus
}

// TestBaseSelectorDecidesForeignSitesInOneBatchedLookup is the cost half of
// the site filter, which is as load-bearing as its answer.
//
// Deciding a site the prefix cannot spell asks the corpus what it holds at
// that path. Asking it one path at a time turns a single find_usages into one
// serial store query per distinct file path of every other tracked repository
// — the per-path amplification this test exists to refuse, introduced by the
// fix for the leak. graph.Store declares GetFileNodesByPaths as the batched sibling of
// GetFileNodes for exactly this, and the endpoint half of the same filters
// already collapses its round-trips that way.
//
// The answer is asserted first in both lanes, so a reader that stopped asking
// by dropping (or keeping) everything cannot pass on the counters alone.
func TestBaseSelectorDecidesForeignSitesInOneBatchedLookup(t *testing.T) {
	t.Run("anchored lane", func(t *testing.T) {
		corpus := newCountingCorpus(unresolvedSiteFanoutCorpus())
		scoped := newBaseGraphReader(corpus, "repo")
		if scoped == nil {
			t.Fatal("the reader refused to narrow a prefix it was given")
		}
		edges := scoped.GetInEdges("repo/a.go::Local")
		if !edgeBetween(edges, "repo/a.go::Caller", "repo/a.go::Local") {
			t.Fatal("GetInEdges dropped this repository's own incoming call")
		}
		if foreign := countForeignEdges(edges); foreign > 0 {
			t.Fatalf("GetInEdges served %d edges sited in the sibling repository", foreign)
		}
		if corpus.fileNodes != 0 {
			t.Errorf("the site filter took the per-path door %d times; %d foreign sites must cost one batched lookup",
				corpus.fileNodes, baseSiteFanoutFiles)
		}
		if corpus.fileNodeBatches != 1 {
			t.Errorf("site lookups = %d batches, want exactly 1 for one lane call", corpus.fileNodeBatches)
		}
		if corpus.batchedPaths != baseSiteFanoutFiles {
			t.Errorf("the batch asked for %d paths, want the %d distinct foreign sites",
				corpus.batchedPaths, baseSiteFanoutFiles)
		}
	})

	t.Run("whole-corpus lane", func(t *testing.T) {
		corpus := newCountingCorpus(unresolvedSiteFanoutCorpus())
		scoped := newBaseGraphReader(corpus, "repo")
		if scoped == nil {
			t.Fatal("the reader refused to narrow a prefix it was given")
		}
		edges := scoped.AllEdges()
		if !edgeBetween(edges, "repo/a.go::Caller", "repo/a.go::Local") {
			t.Fatal("AllEdges dropped this repository's own call")
		}
		if foreign := countForeignEdges(edges); foreign > 0 {
			t.Fatalf("AllEdges served %d edges sited in the sibling repository", foreign)
		}
		if corpus.fileNodes != 0 {
			t.Errorf("the site filter took the per-path door %d times in a whole-corpus scan", corpus.fileNodes)
		}
		if corpus.fileNodeBatches != 1 {
			t.Errorf("site lookups = %d batches, want exactly 1 for one scan", corpus.fileNodeBatches)
		}
		// The endpoint half must batch the same way. An unresolved caller is
		// absent from the batched hydration's result, and reading that as
		// "ask again, one id at a time" made the cross-repository shape — the
		// one that is unresolved by definition — the fan-out case.
		if corpus.nodeLookups != 0 {
			t.Errorf("the endpoint filter hydrated %d ids one at a time; an id the batch returned no row for is already decided",
				corpus.nodeLookups)
		}
	})
}

// TestBaseSelectorNeverProbesASiteTheEndpointsAlreadyDropped pins the ordering
// inside the predicate.
//
// An edge whose endpoints are another repository's is dropped by the endpoint
// half, which reads out of a batch already in memory. Asking its site first
// bought a store round-trip per sibling file and changed the answer by
// nothing. The two halves are an &&, so only the work moves.
func TestBaseSelectorNeverProbesASiteTheEndpointsAlreadyDropped(t *testing.T) {
	for _, lane := range []struct {
		name string
		run  func(*baseGraphReader) []*graph.Edge
	}{
		{"AllEdges", func(r *baseGraphReader) []*graph.Edge { return r.AllEdges() }},
		{"EdgesByKind", func(r *baseGraphReader) []*graph.Edge {
			out := make([]*graph.Edge, 0, 4)
			for edge := range r.EdgesByKind(graph.EdgeCalls) {
				out = append(out, edge)
			}
			return out
		}},
	} {
		t.Run(lane.name, func(t *testing.T) {
			corpus := newCountingCorpus(resolvedSiblingCorpus())
			scoped, ok := newBaseGraphReader(corpus, "repo").(*baseGraphReader)
			if !ok {
				t.Fatal("the reader refused to narrow a prefix it was given")
			}
			edges := lane.run(scoped)
			if !edgeBetween(edges, "repo/a.go::Caller", "repo/a.go::Local") {
				t.Fatal("the lane dropped this repository's own call")
			}
			if foreign := countForeignEdges(edges); foreign > 0 {
				t.Fatalf("the lane served %d edges sited in the sibling repository", foreign)
			}
			if corpus.fileNodes != 0 || corpus.fileNodeBatches != 0 {
				t.Errorf("the lane asked the corpus about %d sites (%d batched) that its endpoints had already decided",
					corpus.fileNodes, corpus.fileNodeBatches)
			}
		})
	}
}

// TestBaseGraphReaderDropsASiteTheCorpusAttributesToNobody covers the arm the
// two lanes share and nothing pinned.
//
// A site the corpus holds no node at is not "one of my files spelled oddly":
// a node at a path is exactly what attributes that path, so neither endpoint
// of such an edge lives there. The view cannot say whose file it is, so it
// does not put it on the wire — under-reporting a reference rather than
// serving a path it cannot attribute to itself. The point of asserting it is
// that both lanes do it, because a whole-corpus scan and an anchored walk that
// disagree about one edge are worse than either answer.
func TestBaseGraphReaderDropsASiteTheCorpusAttributesToNobody(t *testing.T) {
	const unattributed = "/generated/zz_bindings.go"
	corpus := graph.New()
	corpus.AddBatch([]*graph.Node{
		{ID: "repo/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/a.go::One", Kind: graph.KindFunction, Name: "One", FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go", StartLine: 3},
		{ID: "repo/a.go::Two", Kind: graph.KindFunction, Name: "Two", FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go", StartLine: 7},
		{ID: "repo/a.go::Three", Kind: graph.KindFunction, Name: "Three", FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go", StartLine: 11},
	}, []*graph.Edge{
		{From: "repo/a.go::One", To: "repo/a.go::Two", Kind: graph.EdgeCalls, FilePath: "repo/a.go", Line: 4},
		{From: "repo/a.go::One", To: "repo/a.go::Three", Kind: graph.EdgeCalls, FilePath: unattributed, Line: 5},
	})
	if len(corpus.GetFileNodes(unattributed)) != 0 {
		t.Fatal("the fixture attributes the site after all, so it poses nothing")
	}
	scoped := newBaseGraphReader(corpus, "repo")
	if scoped == nil {
		t.Fatal("the reader refused to narrow a prefix it was given")
	}

	all := scoped.AllEdges()
	out := scoped.GetOutEdges("repo/a.go::One")
	// The control: an edge of the same pair, sited at a path the corpus does
	// attribute to this view, survives both lanes.
	if !edgeBetween(all, "repo/a.go::One", "repo/a.go::Two") {
		t.Error("AllEdges dropped an edge sited in this repository's own file")
	}
	if !edgeBetween(out, "repo/a.go::One", "repo/a.go::Two") {
		t.Error("GetOutEdges dropped an edge sited in this repository's own file")
	}
	scanned := edgeBetween(all, "repo/a.go::One", "repo/a.go::Three")
	anchored := edgeBetween(out, "repo/a.go::One", "repo/a.go::Three")
	if scanned != anchored {
		t.Errorf("the two lanes disagree about one edge: AllEdges kept = %v, GetOutEdges kept = %v", scanned, anchored)
	}
	if scanned || anchored {
		t.Errorf("a site the corpus attributes to no repository was served (AllEdges %v, GetOutEdges %v)", scanned, anchored)
	}
}

// seedEdgeSiteShapes records, in the real store the handlers read, the two
// site shapes this round decides: a reference this view owns whose site is
// spelled the legacy way, and a reference written in the sibling repository
// whose caller the corpus never resolved.
func seedEdgeSiteShapes(t *testing.T, stack *viewStack) {
	t.Helper()
	stack.store.AddBatch([]*graph.Node{
		{
			ID: "legacy.go", Kind: graph.KindFile, Name: "legacy.go",
			FilePath: "legacy.go", RepoPrefix: "repo", Language: "go",
		},
		{
			ID: "legacy.go::LegacyCaller", Kind: graph.KindFunction, Name: "LegacyCaller",
			QualName: "repo.LegacyCaller", FilePath: "legacy.go", RepoPrefix: "repo",
			Language: "go", StartLine: 3, EndLine: 5,
		},
		{
			ID: "legacy.go::LegacyTarget", Kind: graph.KindFunction, Name: "LegacyTarget",
			QualName: "repo.LegacyTarget", FilePath: "legacy.go", RepoPrefix: "repo",
			Language: "go", StartLine: 7, EndLine: 9,
		},
	}, []*graph.Edge{
		{
			From: "legacy.go::LegacyCaller", To: "legacy.go::LegacyTarget",
			Kind: graph.EdgeCalls, FilePath: "legacy.go", Line: 4,
		},
		{
			From: baseSelectorGhostCaller, To: baseSelectorCrossRepoTarget,
			Kind: graph.EdgeCalls, FilePath: baseSelectorForeignFile, Line: 11,
		},
	})
}

// baseSelectorGhostCaller is a caller id the corpus hydrates nothing for: the
// shape a cross-repository reference has before resolution. Only its site says
// which repository wrote it.
const baseSelectorGhostCaller = "other/other.go::Ghost"

// TestBaseSelectorEdgeSitesDecidedThroughTheHandler takes both site shapes
// through the production entrypoint.
//
// The reader-level tests fix the predicate; this one fixes that find_usages
// actually reads through it, in both directions, on the store the server runs
// against. Each half runs its control first — without the view — so a green
// assertion cannot mean the corpus never held the usage.
func TestBaseSelectorEdgeSitesDecidedThroughTheHandler(t *testing.T) {
	stack := newViewStack(t)
	seedEdgeSiteShapes(t, stack)

	usages := func(t *testing.T, what string, args map[string]any) string {
		t.Helper()
		res, err := stack.callHandler(t, "", "find_usages", args, stack.srv.handleFindUsages)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if res.IsError {
			t.Fatalf("%s was refused: %s", what, viewResultText(t, res))
		}
		return viewResultText(t, res)
	}

	t.Run("a legacy-sited reference of this view survives", func(t *testing.T) {
		args := map[string]any{"id": "legacy.go::LegacyTarget"}
		if text := usages(t, "the unnarrowed usage query", args); !strings.Contains(text, "legacy.go::LegacyCaller") {
			t.Fatalf("the corpus does not report the legacy caller, so narrowing it proves nothing:\n%s", text)
		}
		text := usages(t, "find_usages under the base selector", baseSelectorArgs(stack.graphID, args))
		if !strings.Contains(text, "legacy.go::LegacyCaller") {
			t.Errorf("a base view of %s dropped a call between two symbols it holds, because the site carries no prefix:\n%s",
				stack.graphID, text)
		}
	})

	t.Run("a sibling-sited reference with an unresolved caller does not", func(t *testing.T) {
		args := map[string]any{"id": baseSelectorCrossRepoTarget}
		control := usages(t, "the unnarrowed usage query", args)
		if !strings.Contains(control, baseSelectorGhostCaller) {
			t.Fatalf("the corpus does not report the unresolved caller, so narrowing it proves nothing:\n%s", control)
		}
		text := usages(t, "find_usages under the base selector", baseSelectorArgs(stack.graphID, args))
		if strings.Contains(text, baseSelectorGhostCaller) {
			t.Errorf("a base view of %s returned a reference written in the sibling repository:\n%s", stack.graphID, text)
		}
		if strings.Contains(text, baseSelectorForeignFile) {
			t.Errorf("a base view of %s put the sibling repository's file path on the wire:\n%s", stack.graphID, text)
		}
	})
}

// graph.Store is the type Server.graph holds (server.go), and it declares the
// batched file-node door. This assignment fails to compile if that stops
// being true, which is what keeps the batch the production path rather than a
// lucky type assertion.
var _ fileNodesBatchReader = (graph.Store)(nil)

// TestBaseSelectorTakesTheBatchDoorOnTheProductionStore closes the gap between
// "the reader batches when it can" and "the server's corpus lets it".
//
// prefetchSites is a type assertion, and a type assertion that misses is
// silent: every site decision falls back to the per-path door and the only
// symptom is a slow request. The narrowed reader the server builds is
// therefore asked here, on the backend the server actually runs against,
// whether the batched door is reachable at all.
func TestBaseSelectorTakesTheBatchDoorOnTheProductionStore(t *testing.T) {
	stack := newViewStack(t)
	if _, ok := any(stack.srv.graph).(fileNodesBatchReader); !ok {
		t.Fatalf("the server's corpus (%T) offers no batched file-node door, so every site decision is a point lookup",
			stack.srv.graph)
	}
	narrowed := newBaseGraphReader(stack.srv.graph, "repo")
	if narrowed == nil {
		t.Fatal("the reader refused to narrow a prefix it was given")
	}
	// And the corpus the narrowed reader kept a handle on is that same store,
	// not something the narrowing wrapped along the way.
	var corpus graph.Reader
	switch reader := narrowed.(type) {
	case *baseGraphContentReader:
		corpus = reader.base
	case *baseGraphReader:
		corpus = reader.base
	default:
		t.Fatalf("unexpected narrowed reader type %T", narrowed)
	}
	if _, batched := corpus.(fileNodesBatchReader); !batched {
		t.Errorf("the narrowed reader's corpus (%T) offers no batched file-node door", corpus)
	}
}
