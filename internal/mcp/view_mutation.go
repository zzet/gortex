package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
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
	case "edit_file", "write_file", "edit_symbol", "batch_edit":
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
	mutation, err := s.beginViewMutationWithRefusalProof(ctx, view)
	release := noop
	// The route (or the base under it) moved between this request's view
	// selection and its admission — a base advance or a rebuild published in
	// between. Nothing is written yet and nothing the edit reads came from
	// the old route, so select the view again and admit against that.
	//
	// The first retry reselects at once, which is enough when a rebuild has
	// already published the new route. When only the primary base moved (the
	// committed base was just published or advanced), the route still names
	// a commit layer over the old base until the checkout's coordinator
	// recomposes it, and an immediate reselection returns that same route:
	// the second retry asks the coordinator for a refresh, which recomposes
	// the route over the new base, waits for it (bounded), and reselects.
	// Concurrent disk motion can leave the same coherent route awaiting its
	// publication; it needs the same pre-read recomposition. Target-content
	// guards are evaluated only after admission against the reselected view.
	// A route or disk that keeps moving is refused as before, and a HEAD
	// change is refused by the lease's own pre-write sample.
	for attempt := 0; attempt < 2 && (errors.Is(err, indexer.ErrCheckoutMutationRouteMoved) || errors.Is(err, indexer.ErrCheckoutMutationDiskMoved)); attempt++ {
		if attempt == 1 && !s.awaitRouteRecomposition(ctx, view) {
			break
		}
		reselected, retryErr := s.reselectMutationView(ctx, selector, policy, view)
		if retryErr != nil {
			break
		}
		var retried *indexer.CheckoutMutation
		retried, err = s.beginViewMutationWithRefusalProof(ctx, reselected)
		if err != nil {
			reselected.close()
			continue
		}
		release()
		mutation, view = retried, reselected
		ctx = withRequestView(ctx, reselected)
		release = reselected.close
	}
	if err != nil {
		nativeAllowed := s.lifecycle.RecordCheckoutMutationRefusal(ctx, err)
		s.activateSelectedCheckout(view.rider.CheckoutID, "source mutation needs a fresh checkout view")
		message := fmt.Sprintf("%s: checkout mutation was not admitted; no files were changed: %v", graphview.CodeViewReadOnly, err)
		message = checkoutMutationRecoveryMessage(message, err, nativeAllowed)
		return ctx, noop, mcp.NewToolResultError(message)
	}
	ctx = withCheckoutMutation(ctx, mutation, view.viewRoot)
	checkoutMutationFromContext(ctx).singleFile = singleFileCheckoutMutationTool(legacy)
	return ctx, func() {
		mutation.Close()
		release()
	}, nil
}

func singleFileCheckoutMutationTool(tool string) bool {
	switch tool {
	case "edit_file", "write_file", "edit_symbol":
		return true
	default:
		return false
	}
}

// routeRecompositionWait bounds how long an edit waits for its checkout's
// route to be recomposed over a moved base before its last admission retry.
var routeRecompositionWait = 30 * time.Second

// awaitRouteRecomposition asks the checkout's coordinator for a refresh and
// waits (bounded by the request and routeRecompositionWait) until the refresh
// has published: a cycle over a moved primary base recomposes the route over
// it. It reports whether the refresh completed.
func (s *Server) awaitRouteRecomposition(ctx context.Context, view *requestView) bool {
	if s == nil || s.lifecycle == nil || view == nil || view.rider == nil {
		return false
	}
	resumeIntent := suspendSourceMutationWriteIntent(ctx)
	defer resumeIntent()
	return awaitRouteRecompositionTickets(ctx, func(ctx context.Context) (*indexer.CheckoutRefreshTicket, error) {
		return s.lifecycle.RequestCheckoutRefresh(ctx, view.rider.CheckoutID, view.viewRoot)
	})
}

// Bulk checkpoint contention aborts one physical generation attempt so its
// coordinator can retry after PASSIVE settles. Keep that retry inside the
// original admission budget, before any edit reads or writes source bytes.
// Semantic refusals and caller cancellation remain terminal.
func awaitRouteRecompositionTickets(ctx context.Context, request func(context.Context) (*indexer.CheckoutRefreshTicket, error)) bool {
	ctx, cancel := context.WithTimeout(ctx, routeRecompositionWait)
	defer cancel()
	for {
		if ctx.Err() != nil {
			return false
		}
		ticket, err := request(ctx)
		if err != nil || ticket == nil || ticket.Ticket == nil {
			return false
		}
		select {
		case result, open := <-ticket.Ticket.Done:
			if !open {
				return false
			}
			if !errors.Is(result.Err, store_sqlite.ErrGenerationBulkCheckpointBusy) {
				return result.Err == nil
			}
		case <-ctx.Done():
			return false
		}
	}
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

// Busy/Pending retain their retry semantics; only stale refusal opens the
// checkout's native recovery path. Reconnecting is a user action, not a hook
// daemon restart or an instruction to change configuration.
func checkoutMutationRecoveryMessage(message string, err error, nativeAllowed bool) string {
	if errors.Is(err, indexer.ErrCheckoutMutationStale) && !errors.Is(err, indexer.ErrCheckoutMutationBusy) && !errors.Is(err, indexer.ErrCheckoutMutationPending) {
		if nativeAllowed {
			message += ". Native read/edit is allowed for this checkout until a fresh view is ready."
		} else {
			message += ". Native fallback status was not established for this checkout."
		}
		message += " Wait for a fresh view and retry the Gortex edit, or reconnect the MCP session."
	}
	return message
}

func (s *Server) beginViewMutationWithRefusalProof(ctx context.Context, view *requestView) (*indexer.CheckoutMutation, error) {
	if route, known := view.materialized.CheckoutMutationRoute(); known && view.answerIdentity != nil && view.answerIdentity.incarnation != "" {
		return s.lifecycle.BeginCheckoutMutationAtView(ctx, view.rider.CheckoutID, view.viewRoot, view.answerIdentity.incarnation, route)
	}
	return s.lifecycle.BeginCheckoutMutation(ctx, view.rider.CheckoutID, view.viewRoot, view.materialized.CheckoutRouteEpoch)
}
