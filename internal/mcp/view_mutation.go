package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

// prepareRoutedViewMutation establishes authority for the small set of tools
// whose disk commits and graph refreshes use the checkout-aware primitives.
// All other source operations continue to fail closed in the mutation guard.
func (s *Server) prepareRoutedViewMutation(
	ctx context.Context,
	req *mcp.CallToolRequest,
	selector graphview.Selector,
	policy requestViewPolicy,
) (context.Context, func(), *mcp.CallToolResult) {
	noop := func() {}
	view := requestViewFromContext(ctx)
	if !s.facades.mutatesSource(req.Params.Name) || !view.routed() ||
		view.rider == nil || !view.rider.Exact || view.viewRoot == "" ||
		view.files != nil || view.materialized == nil || s.lifecycle == nil {
		return ctx, noop, nil
	}
	// Use the same inference/defaults as facade dispatch. Looking only at an
	// explicit operation would refuse valid edit calls that infer it from target.
	legacy := req.Params.Name
	if isFacadeToolName(legacy) {
		spec, ok := s.viewFacadeOperation(req)
		if !ok {
			return ctx, noop, nil
		}
		legacy = spec.Legacy
	}
	switch legacy {
	case "edit_file", "write_file", "edit_symbol":
	default:
		return ctx, noop, nil
	}

	// Symbol ranges must come from the persisted checkout view, not unsaved
	// editor buffers. Pin even an empty cohort so a later facade preparation
	// cannot pick up buffers pushed while this request waits for admission.
	snapshot, present := overlayRequestSnapshotFromContext(ctx)
	if !present {
		var err error
		snapshot, err = s.snapshotOverlayRequestForCtx(ctx)
		if err != nil {
			return ctx, noop, mcp.NewToolResultError(fmt.Sprintf("%s: inspect editor buffers: %v", graphview.CodeViewReadOnly, err))
		}
	}
	if OverlayViewFromContext(ctx) != nil || (snapshot != nil && len(snapshot.files) != 0) {
		return ctx, noop, mcp.NewToolResultError(fmt.Sprintf(
			"%s: save or discard the session's editor buffers before editing the on-disk worktree.", graphview.CodeViewReadOnly))
	}
	if snapshot != nil {
		ctx = withOverlayRequestSnapshot(ctx, snapshot)
	}

	if mutationBeforeAdmission != nil {
		mutationBeforeAdmission(ctx)
	}
	mutation, err := s.lifecycle.BeginCheckoutMutation(ctx, view.rider.CheckoutID,
		view.viewRoot, view.materialized.CheckoutRouteEpoch)
	release := noop
	if errors.Is(err, indexer.ErrCheckoutMutationRouteMoved) {
		// The route (or the base under it) moved between this request's view
		// selection and its admission — a base advance or a rebuild published
		// in between. Nothing is written yet and nothing the edit reads came
		// from the old route, so select the view once more and admit against
		// that. Once only: a route that keeps moving is refused as before, and
		// a HEAD change is refused by the lease's own pre-write sample.
		if reselected, retryErr := s.reselectMutationView(ctx, selector, policy, view); retryErr == nil {
			var retried *indexer.CheckoutMutation
			retried, err = s.lifecycle.BeginCheckoutMutation(ctx, reselected.rider.CheckoutID,
				reselected.viewRoot, reselected.materialized.CheckoutRouteEpoch)
			if err == nil {
				mutation, view = retried, reselected
				ctx = withRequestView(ctx, reselected)
				release = reselected.close
			} else {
				reselected.close()
			}
		}
	}
	if err != nil {
		s.activateSelectedCheckout(view.rider.CheckoutID, "source mutation needs a fresh checkout view")
		return ctx, noop, mcp.NewToolResultError(fmt.Sprintf(
			"%s: checkout mutation was not admitted; no files were changed: %v", graphview.CodeViewReadOnly, err))
	}
	return withCheckoutMutation(ctx, mutation, view.viewRoot), func() {
		mutation.Close()
		release()
	}, nil
}

// mutationBeforeAdmission, when set, runs between a mutation's view selection
// and its admission. It is a test seam: it lets a test move the route inside
// the window admission has to tolerate.
var mutationBeforeAdmission func(context.Context)

// reselectMutationView selects the request's view again for an admission the
// moved route refused. The new selection must name the same checkout, be
// routed and exact, and carry a materialized stack like the first.
func (s *Server) reselectMutationView(
	ctx context.Context,
	selector graphview.Selector,
	policy requestViewPolicy,
	previous *requestView,
) (*requestView, error) {
	view, err := s.resolveRequestView(ctx, selector, policy)
	if err != nil {
		return nil, err
	}
	if !view.routed() || view.rider == nil || !view.rider.Exact || view.viewRoot == "" ||
		view.files != nil || view.materialized == nil ||
		view.rider.CheckoutID != previous.rider.CheckoutID {
		view.close()
		return nil, fmt.Errorf("the reselected view is not the same exact checkout")
	}
	return view, nil
}
