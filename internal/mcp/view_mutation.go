package mcp

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graphview"
)

// prepareRoutedViewMutation establishes authority for the small set of tools
// whose disk commits and graph refreshes use the checkout-aware primitives.
// All other source operations continue to fail closed in the mutation guard.
func (s *Server) prepareRoutedViewMutation(ctx context.Context, req *mcp.CallToolRequest) (context.Context, func(), *mcp.CallToolResult) {
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

	mutation, err := s.lifecycle.BeginCheckoutMutation(ctx, view.rider.CheckoutID,
		view.viewRoot, view.materialized.CheckoutRouteEpoch)
	if err != nil {
		s.activateSelectedCheckout(view.rider.CheckoutID, "source mutation needs a fresh checkout view")
		return ctx, noop, mcp.NewToolResultError(fmt.Sprintf(
			"%s: checkout mutation was not admitted; no files were changed: %v", graphview.CodeViewReadOnly, err))
	}
	return withCheckoutMutation(ctx, mutation, view.viewRoot), mutation.Close, nil
}
