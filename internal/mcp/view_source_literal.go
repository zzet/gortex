package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

// searchSourceLiteralInView is the literal-recall lane for a request that reads
// through a view of its own: a routed worktree, or a committed ref view.
//
// Both callers of the literal recall — search_symbols' content section for an
// empty symbol page, and explore's source-literal grounding — used to grep the
// per-repository searcher, which indexes the CANONICAL checkout. A worktree
// that renamed or deleted a name the canonical checkout still holds then got
// the canonical line back, attributed to the worktree's view and labelled
// exact and fresh, while search_text through the same view correctly found
// nothing. The answer here is the one search_text gives (view_search_text.go):
// the view's own working copy, or nothing when the view has no working copy to
// search. It never falls through to the canonical searchers.
//
// Buffer overlays are not composed in: search_text through a view does not
// compose them either, and the two lanes must report the same bytes.
func (s *Server) searchSourceLiteralInView(
	ctx context.Context,
	view *requestView,
	term string,
	maxHits int,
) exploreSourceLiteralSearch {
	repoPrefix := viewRepoPrefix(view)
	result := exploreSourceLiteralSearch{backend: "view", lookupRepoPrefix: repoPrefix, owned: true}
	if s == nil || s.lifecycle == nil ||
		view.completeness().Evaluate([]graphview.CapabilityID{graphview.CapSearchText}, nil) != nil ||
		viewReadsCommittedTree(view) {
		// Nothing indexes this view's bytes for text search. "No evidence"
		// is the honest answer; the canonical checkout's lines are not this
		// view's evidence.
		result.backend = "view-unavailable"
		result.incomplete = true
		return result
	}
	matches, served, err := s.lifecycle.GrepCheckout(ctx, indexer.CheckoutTextQuery{
		CheckoutID: viewCheckoutID(view),
		Query:      term,
		Limit:      maxHits + 1,
	})
	switch {
	case err != nil:
		result.err = err
		result.incomplete = true
		return result
	case !served:
		result.backend = "view-unavailable"
		result.incomplete = true
		return result
	}
	noteWorktreeRouteDrift(ctx, view, graphview.CapSearchText)
	if len(matches) > maxHits {
		matches = matches[:maxHits]
		result.incomplete = true
	}
	result.matches = stampRepoPrefix(matches, repoPrefix)
	return result
}
