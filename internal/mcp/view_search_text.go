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
		return nil, mcp.NewToolResultError("search_text: " + err.Error())
	}
	if view.viewRoot == "" {
		return nil, viewTextUnavailable(view, "it reads a committed tree no working copy holds")
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
	return stampRepoPrefix(matches, viewRepoPrefix(view)), nil
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
	if view == nil {
		return ""
	}
	if view.rider != nil && view.rider.CheckoutID != "" {
		return view.rider.CheckoutID
	}
	return view.checkoutID
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
