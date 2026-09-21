package mcp

import (
	"context"
	"fmt"
	"regexp"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/search/trigram"
)

// Text search is the one search lane a composed view cannot answer by stacking
// corpora the way view_search.go stacks the symbol and content ones. A trigram
// index is built from bytes on disk, not from rows a generation carries, so
// what serves a view is a searcher over the view's own working copy — and a
// view that has no working copy has nothing to search.

// searchTextInView answers a literal or regexp search over the working copy the
// request's view reads, and refuses when the view has none.
//
// The refusal is the point. Falling through to the per-repository searchers
// would answer out of the canonical checkout: a different branch, a different
// working tree, and lines the caller's view does not contain. Naming the
// capability the view cannot serve is the honest answer, and it is the same
// answer the capability evaluation gives a caller that required search.text —
// this asks that evaluation rather than re-deciding it, so a request that did
// not require the capability cannot be served content a request that did would
// have been refused.
func (s *Server) searchTextInView(
	ctx context.Context,
	view *requestView,
	query string,
	useRegexp bool,
	limit int,
) ([]trigram.Match, *mcp.CallToolResult) {
	if err := view.completeness().Evaluate([]graphview.CapabilityID{graphview.CapSearchText}, nil); err != nil {
		// The evaluation names the capability and the state it is in; on its
		// own that says a view cannot be searched without saying what about
		// the view makes it unsearchable. The rider carries the same state,
		// so the reason is what a caller has that it did not have before.
		return nil, mcp.NewToolResultError(fmt.Sprintf(
			"search_text: %s: %s", err.Error(), textSearchRefusalReason(view)))
	}
	if view.baseNarrowed {
		return s.searchTextInNarrowedBase(view, query, useRegexp, limit)
	}
	// The same predicate the byte lane classifies a view by (view_paths.go),
	// so the two lanes cannot disagree about whether this view's content is on
	// a working copy.
	//
	// It is a backstop rather than the lane's production refusal, and saying so
	// is the honest claim: for the shape it names — a routed checkout whose
	// route withdrew its working-tree layer — the evaluation above refuses
	// first, because the commit generation on top declares nothing for
	// search.text (indexer/builder_generation.go textSearchProducer) and the
	// reader reads that silence as StateUnavailable (graphview/materialize.go
	// completeness). No production shape reaches here declaring the capability
	// complete over a committed tree today. What this arm buys is that the two
	// lanes classify by one predicate: if a future producer or ordering change
	// lets a committed-tree view declare text search, the searcher still does
	// not get pointed at a root the view does not read. The test that covers it
	// sets the completeness by hand and says so.
	if viewReadsCommittedTree(view) {
		return nil, viewTextUnavailable(view, textSearchRefusalReason(view))
	}

	var compiled *regexp.Regexp
	if useRegexp {
		re, err := regexp.Compile(query)
		if err != nil {
			return nil, mcp.NewToolResultError("search_text: invalid regexp: " + err.Error())
		}
		compiled = re
	}

	matches, served, err := s.lifecycle.GrepCheckout(ctx, indexer.CheckoutTextQuery{
		CheckoutID: viewCheckoutID(view),
		Query:      query,
		Regexp:     compiled,
		Limit:      limit,
	})
	switch {
	case err != nil:
		return nil, mcp.NewToolResultError("search_text: " + err.Error())
	case !served:
		return nil, viewTextUnavailable(view, "nothing indexes its working copy for text search")
	}
	// The answer came off a live root, which nothing freezes for the length of
	// a request. If the route this view pinned advanced while the search ran,
	// the caller is told the answer is thin rather than left to assume it is
	// the snapshot the rider names.
	noteWorktreeRouteDrift(ctx, view, graphview.CapSearchText)
	return stampRepoPrefix(matches, viewRepoPrefix(view)), nil
}

// searchTextInNarrowedBase answers a labelled base selector's text search out
// of the one repository its graph owns.
//
// A base selector is not a route to a checkout: it is the indexed corpus with
// a predicate on it, and the corpus for that repository was built from the
// repository's own canonical checkout — which is exactly the root the
// per-repository trigram searcher indexes. So the narrowing has an exact
// expression here, `RepoAllow = {the graph's prefix}`, and taking it is what
// stops this lane from being the one place a base selector answers about
// every tracked repository at once. Withdrawing the capability instead would
// refuse a search the view can answer precisely; fanning out unnarrowed is
// the cross-repository answer the narrowing exists to stop. This is neither.
//
// The match paths come back already stamped with the repository prefix
// (MultiIndexer.GrepTextForRepos does it), which is the spelling every other
// search_text lane and every graph node uses, so nothing restamps them.
func (s *Server) searchTextInNarrowedBase(
	view *requestView,
	query string,
	useRegexp bool,
	limit int,
) ([]trigram.Match, *mcp.CallToolResult) {
	prefix := baseNarrowedRepoPrefix(view)
	if prefix == "" || s.multiIndexer == nil {
		// Nothing to narrow with. Falling through to the unnarrowed searchers
		// is the one answer this lane must never give, so it refuses instead.
		return nil, viewTextUnavailable(view,
			"no per-repository searcher is narrowed to the graph it names")
	}
	allow := map[string]bool{prefix: true}
	if useRegexp {
		matches, err := s.multiIndexer.GrepRegexpForRepos(query, "", allow, limit)
		if err != nil {
			return nil, mcp.NewToolResultError("search_text: invalid regexp: " + err.Error())
		}
		return matches, nil
	}
	return s.multiIndexer.GrepTextForRepos(query, allow, limit), nil
}

// scopedRepoPrefix names the repository a base-narrowed reader filters the
// corpus down to. baseGraphContentReader embeds this type, so both shapes
// newBaseGraphReader returns answer it.
func (r *baseGraphReader) scopedRepoPrefix() string {
	if r == nil {
		return ""
	}
	return r.repoPrefix
}

// baseNarrowedRepoPrefix reads that prefix off the view, and answers "" for
// every view that is not a base narrowing — which is what keeps the routed
// checkout lane out of the arm above.
func baseNarrowedRepoPrefix(view *requestView) string {
	if view == nil || !view.baseNarrowed || view.reader == nil {
		return ""
	}
	scoped, ok := view.reader.(interface{ scopedRepoPrefix() string })
	if !ok {
		return ""
	}
	return scoped.scopedRepoPrefix()
}

// textSearchRefusalReason says what about this view's shape stops a text
// search, in the vocabulary the view itself is described in.
func textSearchRefusalReason(view *requestView) string {
	switch {
	case view == nil:
		return "no view answers this request"
	case view.baseNarrowed:
		return "a base graph binds no working copy to search"
	case view.viewRoot == "":
		return "it reads a committed tree no working copy holds"
	default:
		// The remaining shape this is asked about is a routed checkout whose
		// route withdrew its working-tree layer (viewReadsCommittedTree): it
		// has a root, and the root is not what it reads.
		return "the top layer of its stack is a committed tree, so the working copy " +
			"at its root is not the snapshot it reads"
	}
}

// viewTextUnavailable is the typed refusal a view that cannot be text-searched
// answers with. It carries the same code and capability name the capability
// evaluation would have refused a required search.text with, so a caller reads
// one vocabulary whether it declared the requirement or not.
func viewTextUnavailable(view *requestView, because string) *mcp.CallToolResult {
	actual := string(graphview.SelectorBase)
	if view.rider != nil && view.rider.ActualView != "" {
		actual = view.rider.ActualView
	}
	return mcp.NewToolResultError(fmt.Sprintf(
		"search_text: %s: %s cannot serve %s because %s",
		graphview.CodeCapabilityUnavailable, actual, graphview.CapSearchText, because))
}

// viewCheckoutID and viewRepoPrefix read the two identities the checkout
// searcher is addressed by, tolerating a view that carries neither.
func viewCheckoutID(view *requestView) string {
	if view == nil || view.rider == nil {
		return ""
	}
	return view.rider.CheckoutID
}

func viewRepoPrefix(view *requestView) string {
	if view == nil || view.materialized == nil {
		return ""
	}
	return view.materialized.ID.RepoPrefix
}

// stampRepoPrefix spells a checkout searcher's repo-relative match paths the
// way every other search_text path is spelled: under the repository the view's
// layers live in, which is also how the graph keys the file nodes each match is
// attributed and enriched through.
func stampRepoPrefix(matches []trigram.Match, repoPrefix string) []trigram.Match {
	if repoPrefix == "" {
		return matches
	}
	for i := range matches {
		matches[i].Path = repoPrefix + "/" + matches[i].Path
	}
	return matches
}
