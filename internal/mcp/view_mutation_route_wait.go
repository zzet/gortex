package mcp

import (
	"context"
	"time"

	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

// mutationRouteWaitCap bounds an otherwise unbounded source mutation. Bounded
// requests use their original deadline, reserving time for the mutation itself.
const mutationRouteWaitCap = 5 * time.Second

// mutationRouteWaitMargin is the part of the request's deadline the wait
// leaves for the mutation itself.
const mutationRouteWaitMargin = 2 * time.Second

// mutationRouteWaitPoll is how often the wait re-selects the view; a variable
// so tests can shorten it. The edit is on hold for the whole wait, so the poll
// is what an admitted edit loses to it after the route comes back: at 100 ms
// that was up to a tenth of the whole edit budget. A re-selection is a few
// catalog reads.
var mutationRouteWaitPoll = 25 * time.Millisecond

// awaitMutationRoute re-selects an explicit worktree view whose route is being
// rebuilt until it is routed again, for a source mutation only.
//
// A mutation on a checkout whose route is mid-rebuild used to be refused at
// once with view_building. The rebuild is exactly what an edit a moment ago
// started, so the refusal hit the second edit of every quick sequence and
// lost its receipt. Waiting (bounded by the request deadline and
// mutationRouteWaitCap) turns that into the admission the caller wanted; the
// refusal still happens — as an error result with its text — when the route
// does not come back in time, or when any other error arrives.
func (s *Server) awaitMutationRoute(
	ctx context.Context,
	selector graphview.Selector,
	policy requestViewPolicy,
	view *requestView,
	err error,
) (*requestView, error) {
	// The first selection is done: what follows (for a mutation whose route
	// is being rebuilt) is the route wait, timed separately.
	indexer.StampPublicationPhase(ctx, indexer.PublicationViewSelected)
	if !policy.awaitRoute || selector.Kind != graphview.SelectorWorktree ||
		err == nil || graphview.CodeOf(err) != graphview.CodeViewBuilding {
		return view, err
	}
	deadline := time.Now().Add(mutationRouteWaitCap)
	if ctxDeadline, ok := ctx.Deadline(); ok {
		deadline = ctxDeadline.Add(-mutationRouteWaitMargin)
	}
	if policy.freshness.requested() || policy.freshness.hasDeadline {
		if bounded := policy.freshness.effectiveDeadline(time.Now(), ctx); bounded.Before(deadline) {
			deadline = bounded
		}
	}
	resumeIntent := suspendSourceMutationWriteIntent(ctx)
	defer resumeIntent()
	if s.mutationRouteWaitEntered != nil {
		s.mutationRouteWaitEntered(ctx)
	}
	for time.Now().Before(deadline) {
		timer := time.NewTimer(mutationRouteWaitPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return view, err
		case <-timer.C:
		}
		view.close()
		view, err = s.selectRequestView(ctx, selector, policy)
		if err == nil || graphview.CodeOf(err) != graphview.CodeViewBuilding {
			return view, err
		}
	}
	return view, err
}

// withMutationPublicationStamps arms the request's publication stamp
// collector for a source mutation, so every step between the tool call
// arriving and its disk commit is timed onto the edit's publication record
// (openMutationPhases absorbs them). Other tools pay nothing.
func (s *Server) withMutationPublicationStamps(ctx context.Context, tool string) context.Context {
	if s == nil || s.facades == nil || !s.facades.mutatesSource(tool) {
		return ctx
	}
	return indexer.WithPublicationStamps(ctx)
}
