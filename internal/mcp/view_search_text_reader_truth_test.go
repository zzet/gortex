package mcp

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/search/trigram"
)

// What the READER says about text search, which is the half D5 did not have.
//
// The producers were made truthful first (indexer/builder_generation.go,
// textSearchProducer), and it changed nothing a caller could see: the
// materializer seeded every capability at StateComplete and only ever worsted,
// so a generation that declares nothing contributed Complete and a directly
// selected committed identity still claimed whole text search. The reader now
// takes the TOP layer's declaration as authoritative for CapSearchText alone
// (graphview/materialize.go, Materializer.completeness), which is what these
// tests are about — plus the other direction: a base selector, whose narrowing
// has an exact expression, answers narrowed instead of withdrawing.

// searchTextRefusalText is the text of a refused search_text answer.
func searchTextRefusalText(t *testing.T, stack *viewStack, cwd string, args map[string]any) string {
	t.Helper()
	res, err := stack.callHandler(t, cwd, "search_text", args, stack.srv.handleSearchText)
	if err != nil {
		t.Fatalf("search_text: %v", err)
	}
	if !res.IsError {
		t.Fatalf("search_text answered where a refusal was expected: %s", viewResultText(t, res))
	}
	return viewResultText(t, res)
}

// routeCommitOnly re-routes the fixture's checkout to its commit layer alone,
// which is the shape a coordinator publishes for the window in which it is
// moving the commit slot — and the shape a dedicated base with nothing over it
// is served in permanently.
func routeCommitOnly(t *testing.T, stack *viewStack) {
	t.Helper()
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, 0, store_sqlite.RouteActive)
}

// TestCommittedTopViewRefusesTextSearchAsACapability pins the reader change at
// the seam graphview hands to this package.
//
// A checkout stack routed to its commit layer alone reads a committed tree
// while its root is free to hold edits that tree does not contain. Before the
// top-layer rule the materialized view said search.text was Complete — the
// commit layer declares nothing and silence was read as "inherited" — so
// searchTextInView got past its own capability evaluation and refused further
// down for an unrelated reason: that no searcher was registered for the
// checkout. The refusal a caller gets now names the actual cause, and it is the
// capability evaluation's own refusal rather than a second opinion.
//
// The view is materialized and handed to searchTextInView directly, because
// materializeRequestView gates the cwd/selector lane on RouteReady
// (view_request.go, "!found || !graphview.RouteReady(route)"), which requires a
// working-tree slot — so today's worktree lane never presents this shape and a
// middleware-driven request would exercise the base fallback instead. What is
// pinned here is the contract between the two packages, which is what the
// dedicated-base routes land on.
func TestCommittedTopViewRefusesTextSearchAsACapability(t *testing.T) {
	stack := newViewStack(t)
	// The working-tree layer claims the capability the way a real coordinator's
	// generation does. It is routed away below, which is the whole point: the
	// claim must not survive the layer leaving the top of the stack.
	stack.declareProducer(t, stack.dirty, graphview.CapSearchText, store_sqlite.ProducerStateComplete)
	routeCommitOnly(t, stack)

	materialized, err := stack.srv.materializer.MaterializeCheckout(context.Background(), viewTestWorktree)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	defer materialized.Close()
	if got := materialized.Completeness.State(graphview.CapSearchText); got != graphview.StateUnavailable {
		t.Fatalf("a commit-layer top reports %s = %q, want %q",
			graphview.CapSearchText, got, graphview.StateUnavailable)
	}

	view := &requestView{
		kind:         requestViewKindWorktree,
		reader:       materialized.Reader,
		materialized: materialized,
		viewRoot:     stack.worktreeRoot,
	}
	matches, refusal := stack.srv.searchTextInView(context.Background(), view, "func Keeper", false, 10)
	if refusal == nil {
		t.Fatalf("a committed top answered a text search: %v", matches)
	}
	text := viewResultText(t, refusal)
	if !strings.Contains(text, string(graphview.CodeCapabilityUnavailable)) ||
		!strings.Contains(text, string(graphview.CapSearchText)) {
		t.Fatalf("the refusal does not name the capability:\n%s", text)
	}
	if !strings.Contains(text, string(graphview.StateUnavailable)) {
		t.Errorf("the refusal does not report the state the view is in:\n%s", text)
	}
	// The reason is what separates this refusal from the one a routed view with
	// no searcher gets. Without the top-layer rule the request reaches that
	// second refusal instead, and this assertion is what fails.
	if !strings.Contains(text, "the top layer of its stack is a committed tree") {
		t.Fatalf("the refusal does not explain what about the view refuses it:\n%s", text)
	}
	if strings.Contains(text, "nothing indexes its working copy") {
		t.Fatalf("the request reached the searcher lookup, so the capability did not refuse it:\n%s", text)
	}
}

// TestCommittedTreeRefusalExplainsItself is the "and the rider explains" half
// of D5 on the one committed identity the MCP surface reaches today.
//
// A ref view's generation withdraws the capability outright, so the refusal was
// already the capability evaluation's — but the evaluation's message is a
// capability name and a state, which says a view cannot be searched without
// saying what about the view makes it unsearchable. The reason is what a caller
// now has that it did not have before.
func TestCommittedTreeRefusalExplainsItself(t *testing.T) {
	stack := newRefStack(t)

	res, err := stack.call(t, "search_text", refSelector("git_ref", "refs/heads/feature"),
		map[string]any{"query": "func Keeper"}, stack.srv.handleSearchText)
	if err != nil {
		t.Fatalf("search through the ref view: %v", err)
	}
	assertNamesTextCapability(t, res)
	if text := viewResultText(t, res); !strings.Contains(text, "it reads a committed tree no working copy holds") {
		t.Fatalf("the refusal names the capability but not the reason:\n%s", text)
	}
}

// TestRoutedWorkingCopyStillServesTextSearchCompletely is the false-negative
// guard. The top-layer rule reads a denial out of silence, and the layer whose
// silence used to be harmless — the commit layer — still sits in every routed
// checkout's stack. A working-tree layer over it must keep the capability
// whole, or the rule would refuse every live routed search.
func TestRoutedWorkingCopyStillServesTextSearchCompletely(t *testing.T) {
	stack := newViewStack(t)
	stack.declareProducer(t, stack.dirty, graphview.CapSearchText, store_sqlite.ProducerStateComplete)
	// A withdrawal on the layer BELOW the working copy, which is what the
	// producers deliberately do not write and what the union would have
	// worst-cased into a refusal.
	stack.declareProducer(t, stack.commit, graphview.CapSearchText, store_sqlite.ProducerStateUnavailable)

	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol",
		map[string]any{requiredCapabilitiesArgName: string(graphview.CapSearchText)},
		captureReader(stack.srv, new(graph.Reader)))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("a routed working copy was refused %s: %s", graphview.CapSearchText, viewResultText(t, res))
	}

	// And the search itself gets past the capability gate: what stops it here
	// is that this fixture registers no checkout searcher, which is a different
	// refusal with a different reason.
	text := searchTextRefusalText(t, stack, stack.worktreeRoot, map[string]any{"query": "func Keeper"})
	if !strings.Contains(text, "nothing indexes its working copy") {
		t.Fatalf("the capability gate refused a routed working copy:\n%s", text)
	}
}

// TestBaseSelectorTextSearchNarrowsToItsOwnRepository is the second half of the
// item: where an exact narrowing exists, take it instead of withdrawing the
// capability.
//
// A base selector is the indexed corpus with a repository predicate on it, and
// the per-repository trigram searchers take a repository allow-set. Pinning the
// allow-set to the selector's own graph answers out of exactly the repository
// the view names — not out of every tracked checkout, which is what the
// narrowing exists to stop, and not with a refusal, which is what it used to
// do.
//
// What this proves is the lane: a base selector reaches searchTextInView
// through the real handler and comes back with an answer. It does NOT prove
// the allow-set, because handleSearchText masks a foreign match afterwards
// (filterTextMatchesByResolvedScope) whenever the session resolved any scope,
// which this fixture always does. The allow-set is pinned separately, on the
// unscoped shape where nothing masks it — see
// TestBaseSelectorTextSearchPinsTheAllowSetToItsOwnRepository.
func TestBaseSelectorTextSearchNarrowsToItsOwnRepository(t *testing.T) {
	stack := newViewStack(t)
	const query = "func "

	// The control: unnarrowed, this query answers about both repositories, so
	// the narrowed answer below is a narrowing and not an empty corpus.
	// The session's own scope narrows by workspace, and the two fixture
	// repositories are in different workspaces — so the calls below run from
	// the directory ABOVE both, where the session narrows nothing and the only
	// difference between the two answers is the view selector.
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
	if !containsPathIn(whole, "repo/") || !containsPathIn(whole, baseSelectorForeignRepo+"/") {
		t.Fatalf("the unnarrowed search answered %v, which does not span both repositories", whole)
	}

	res, err := stack.callHandler(t, wide, "search_text",
		baseSelectorArgs(stack.graphID, map[string]any{"query": query}), stack.srv.handleSearchText)
	if err != nil {
		t.Fatalf("search under the base selector: %v", err)
	}
	if res.IsError {
		t.Fatalf("the base selector refused a search it can narrow exactly: %s", viewResultText(t, res))
	}
	scoped := searchTextMatchPaths(t, res)
	if len(scoped) == 0 {
		t.Fatalf("the base selector answered nothing for a query its own repository holds")
	}
	for _, path := range scoped {
		if !strings.HasPrefix(path, "repo/") {
			t.Errorf("a base view of %s answered %q, which is not its repository", stack.graphID, path)
		}
	}
}

// TestBaseSelectorRegexpTextSearchNarrowsToo pins the regexp arm of the same
// lane, which reaches a different MultiIndexer entry point.
func TestBaseSelectorRegexpTextSearchNarrowsToo(t *testing.T) {
	stack := newViewStack(t)

	wide := filepath.Dir(stack.repoRoot)
	res, err := stack.callHandler(t, wide, "search_text",
		baseSelectorArgs(stack.graphID, map[string]any{"query": "func [A-Z][a-z]+", "regexp": true}),
		stack.srv.handleSearchText)
	if err != nil {
		t.Fatalf("regexp search under the base selector: %v", err)
	}
	if res.IsError {
		t.Fatalf("the base selector refused a regexp search it can narrow: %s", viewResultText(t, res))
	}
	scoped := searchTextMatchPaths(t, res)
	if len(scoped) == 0 {
		t.Fatalf("the base selector answered nothing for a pattern its own repository matches")
	}
	for _, path := range scoped {
		if !strings.HasPrefix(path, "repo/") {
			t.Errorf("a base view of %s answered %q, which is not its repository", stack.graphID, path)
		}
	}

	// A pattern that does not compile is still the caller's error rather than
	// an empty answer.
	bad, err := stack.callHandler(t, wide, "search_text",
		baseSelectorArgs(stack.graphID, map[string]any{"query": "func [", "regexp": true}),
		stack.srv.handleSearchText)
	if err != nil {
		t.Fatalf("invalid regexp under the base selector: %v", err)
	}
	if !bad.IsError || !strings.Contains(viewResultText(t, bad), "invalid regexp") {
		t.Fatalf("an uncompilable pattern was not reported as one: %s", viewResultText(t, bad))
	}
}

// containsPathIn reports whether any path carries the prefix.
func containsPathIn(paths []string, prefix string) bool {
	for _, path := range paths {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// narrowedMatchPaths reads the paths off a searcher answer taken directly,
// i.e. without the tool response the end-to-end helpers parse.
func narrowedMatchPaths(matches []trigram.Match) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.Path)
	}
	sort.Strings(out)
	return out
}

// baseNarrowedView is a base selector's view as materializeRequestView builds
// it: the corpus reader narrowed to one repository, flagged baseNarrowed, and
// labelled base (view_request.go, "view.reader = scoped; view.baseNarrowed =
// true").
func baseNarrowedView(t *testing.T, stack *viewStack, repoPrefix string) *requestView {
	t.Helper()
	scoped := newBaseGraphReader(stack.srv.graph, repoPrefix)
	if scoped == nil {
		t.Fatalf("the fixture corpus would not narrow to %q", repoPrefix)
	}
	return &requestView{kind: requestViewKindBase, reader: scoped, baseNarrowed: true}
}

// TestBaseSelectorTextSearchPinsTheAllowSetToItsOwnRepository pins the
// narrowing itself, at the primitive rather than through the tool.
//
// The end-to-end base-selector tests above drive handleSearchText, and that
// handler runs filterTextMatchesByResolvedScope (tools_search_text.go) over
// whatever the search returned. In the fixture the session always resolves a
// non-empty RepoAllow, so that filter drops a foreign match on its own: the
// end-to-end assertions stay green even with this arm's allow-set deleted,
// which means they observe the mask and not the narrowing.
//
// The mask is not a substitute. Its first statement returns the matches
// untouched when the resolved workspace, project and repo-allow are all empty
// (tools_search_text.go, filterTextMatchesByResolvedScope), which is an
// ordinary unscoped session — and on that session the allow-set built here is
// the only thing between a view whose rider says exact, graph_id:X and every
// tracked repository's bytes. So the arm is driven directly, where nothing
// downstream can stand in for it.
func TestBaseSelectorTextSearchPinsTheAllowSetToItsOwnRepository(t *testing.T) {
	stack := newViewStack(t)
	const query = "func "
	view := baseNarrowedView(t, stack, "repo")

	// The control is the same searcher this arm calls, asked unnarrowed: it
	// must span both repositories, or the narrowed answer below would be a
	// narrowing of nothing.
	unnarrowed := narrowedMatchPaths(stack.srv.multiIndexer.GrepTextForRepos(query, nil, 100))
	if !containsPathIn(unnarrowed, "repo/") || !containsPathIn(unnarrowed, baseSelectorForeignRepo+"/") {
		t.Fatalf("the unnarrowed searchers answered %v, which does not span both repositories", unnarrowed)
	}

	matches, refusal := stack.srv.searchTextInNarrowedBase(view, query, false, 100)
	if refusal != nil {
		t.Fatalf("the base narrowing refused a search it can answer exactly: %s", viewResultText(t, refusal))
	}
	got := narrowedMatchPaths(matches)
	if len(got) == 0 {
		t.Fatal("the base narrowing answered nothing for a query its own repository holds")
	}
	for _, path := range got {
		if !strings.HasPrefix(path, "repo/") {
			t.Errorf("a base view of %s answered %q out of %v, which is not its repository",
				stack.graphID, path, got)
		}
	}
}

// TestBaseSelectorRegexpTextSearchPinsTheAllowSetToo is the same pin on the
// regexp arm, which reaches a different MultiIndexer entry point.
func TestBaseSelectorRegexpTextSearchPinsTheAllowSetToo(t *testing.T) {
	stack := newViewStack(t)
	const pattern = "func [A-Z][a-z]+"
	view := baseNarrowedView(t, stack, "repo")

	wide, err := stack.srv.multiIndexer.GrepRegexpForRepos(pattern, "", nil, 100)
	if err != nil {
		t.Fatalf("unnarrowed regexp search: %v", err)
	}
	unnarrowed := narrowedMatchPaths(wide)
	if !containsPathIn(unnarrowed, "repo/") || !containsPathIn(unnarrowed, baseSelectorForeignRepo+"/") {
		t.Fatalf("the unnarrowed searchers answered %v, which does not span both repositories", unnarrowed)
	}

	matches, refusal := stack.srv.searchTextInNarrowedBase(view, pattern, true, 100)
	if refusal != nil {
		t.Fatalf("the base narrowing refused a regexp search it can answer: %s", viewResultText(t, refusal))
	}
	got := narrowedMatchPaths(matches)
	if len(got) == 0 {
		t.Fatal("the base narrowing answered nothing for a pattern its own repository matches")
	}
	for _, path := range got {
		if !strings.HasPrefix(path, "repo/") {
			t.Errorf("a base view of %s answered %q out of %v, which is not its repository",
				stack.graphID, path, got)
		}
	}
}

// TestBaseNarrowedTextSearchRefusesWhenItCannotNarrow pins the arm's own
// fail-closed guard.
//
// The narrowing needs two things: the repository prefix the view names, and a
// MultiIndexer holding that repository's searcher. Without either, the only
// searchers in reach are the unnarrowed ones — the cross-repository answer
// this whole lane exists to stop — so the arm refuses rather than falling
// through to them. Both shapes are unreachable today (view.baseNarrowed is set
// only where newBaseGraphReader returned non-nil, which requires a non-empty
// prefix), which is exactly why the guard needs a test: nothing else would
// notice it going away.
func TestBaseNarrowedTextSearchRefusesWhenItCannotNarrow(t *testing.T) {
	stack := newViewStack(t)
	narrowed := baseNarrowedView(t, stack, "repo")

	cases := []struct {
		name string
		srv  *Server
		view *requestView
	}{
		{
			"the view names no repository to narrow to",
			stack.srv,
			&requestView{kind: requestViewKindBase, baseNarrowed: true},
		},
		{
			"no per-repository searcher exists at all",
			&Server{},
			narrowed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matches, refusal := tc.srv.searchTextInNarrowedBase(tc.view, "func ", false, 100)
			if refusal == nil {
				t.Fatalf("an unnarrowable base view answered %v", narrowedMatchPaths(matches))
			}
			text := viewResultText(t, refusal)
			if !strings.Contains(text, string(graphview.CodeCapabilityUnavailable)) ||
				!strings.Contains(text, string(graphview.CapSearchText)) {
				t.Errorf("the refusal does not name the capability:\n%s", text)
			}
			if !strings.Contains(text, "no per-repository searcher is narrowed to the graph it names") {
				t.Errorf("the refusal does not say what it could not narrow with:\n%s", text)
			}

			// The regexp arm refuses on the same shape, and refuses BEFORE it
			// compiles: an uncompilable pattern coming back as "invalid
			// regexp" would mean the guard was passed and the searcher call
			// was what stopped the request.
			_, reRefusal := tc.srv.searchTextInNarrowedBase(tc.view, "func [", true, 100)
			if reRefusal == nil {
				t.Fatal("an unnarrowable base view answered a regexp search")
			}
			if reText := viewResultText(t, reRefusal); strings.Contains(reText, "invalid regexp") {
				t.Errorf("the regexp arm got past the guard before refusing:\n%s", reText)
			}
		})
	}
}
