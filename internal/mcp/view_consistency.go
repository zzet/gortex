package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

const (
	requireExactArgName = "require_exact"
	requireFreshArgName = "require_fresh"
	waitDeadlineArgName = "wait_deadline"

	viewConsistencyPollInterval = 300 * time.Millisecond
	viewWaitResponseMargin      = 100 * time.Millisecond
)

// requestViewConsistency is the request-level consistency contract after its
// wire values have been validated and stripped from handler arguments.
type requestViewConsistency struct {
	requireExact bool
	requireFresh bool
	waitDeadline time.Time
	hasDeadline  bool

	// freshnessProbe is a focused test seam. Production requests leave it nil
	// and use the checkout lifecycle below.
	freshnessProbe func(context.Context, string) (indexer.CheckoutFreshness, error)
}

func (c requestViewConsistency) forbidsFallback() bool {
	return c.requireExact || c.requireFresh
}

// takeViewConsistencyRequest accepts native booleans and their flattened text
// form, matching the capability controls. wait_deadline is deliberately an
// absolute timestamp: retries across processes retain the same upper bound.
func takeViewConsistencyRequest(req *mcp.CallToolRequest) (requestViewConsistency, error) {
	var want requestViewConsistency
	if req == nil {
		return want, nil
	}
	args, ok := req.Params.Arguments.(map[string]any)
	if !ok {
		return want, nil
	}
	var err error
	if want.requireExact, err = takeViewConsistencyFlag(args, requireExactArgName); err != nil {
		return requestViewConsistency{}, err
	}
	if want.requireFresh, err = takeViewConsistencyFlag(args, requireFreshArgName); err != nil {
		return requestViewConsistency{}, err
	}
	raw, present := args[waitDeadlineArgName]
	if !present {
		return want, nil
	}
	delete(args, waitDeadlineArgName)
	text, ok := raw.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return requestViewConsistency{}, graphview.NewViewError(graphview.CodeInvalidViewSelector,
			waitDeadlineArgName+" must be an absolute RFC3339 timestamp")
	}
	want.waitDeadline, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(text))
	if err != nil {
		return requestViewConsistency{}, graphview.WrapViewError(graphview.CodeInvalidViewSelector,
			waitDeadlineArgName+" must be an absolute RFC3339 timestamp", err)
	}
	want.hasDeadline = true
	return want, nil
}

func takeViewConsistencyFlag(args map[string]any, name string) (bool, error) {
	raw, present := args[name]
	if !present {
		return false, nil
	}
	delete(args, name)
	switch value := raw.(type) {
	case nil:
		return false, nil
	case bool:
		return value, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true":
			return true, nil
		case "false", "":
			return false, nil
		}
	}
	return false, graphview.NewViewError(graphview.CodeInvalidViewSelector,
		fmt.Sprintf("%s must be a boolean", name))
}

func viewConsistencyProperties() map[string]any {
	return map[string]any{
		requireExactArgName: map[string]any{
			"type": "boolean", "default": false,
			"description": "Refuse a fallback whose actual view differs from the requested view.",
		},
		requireFreshArgName: map[string]any{
			"type": "boolean", "default": false,
			"description": "Wait for a current exact view; stale and building fallbacks are forbidden.",
		},
		waitDeadlineArgName: map[string]any{
			"type": "string", "format": "date-time",
			"description": "Absolute RFC3339 deadline for waiting on a transient view build; bounded by the request deadline.",
		},
	}
}

func publishViewRequestProperties(properties map[string]any, selectorSchema map[string]any) {
	if properties == nil {
		return
	}
	properties[viewArgName] = selectorSchema
	for name, schema := range viewConsistencyProperties() {
		properties[name] = schema
	}
}

// resolveRequestViewConsistently selects once for ordinary requests and polls
// only when the caller's contract requires a transient view to become ready.
// It records the terminal selection exactly once; rejected intermediate
// fallbacks release their generation leases before the wait.
func (s *Server) resolveRequestViewConsistently(
	ctx context.Context,
	selector graphview.Selector,
	policy requestViewPolicy,
	want requestViewConsistency,
) (*requestView, error) {
	deadline, canWait := effectiveViewWaitDeadline(ctx, want)
	retrySelector := selector
	requestedView := ""
	var last *requestView
	defer func() {
		if last != nil {
			last.close()
		}
	}()

	for {
		view, err := s.selectRequestView(ctx, retrySelector, policy)
		if retrySelector.Kind == graphview.SelectorAuto && view != nil && view.rider != nil && view.rider.CheckoutID != "" {
			// CWD binding is the only broad family scan in the wait loop. Once
			// it names a concrete checkout, retries address that identity
			// directly. The rider was already created from the resolved
			// worktree selector, so this does not rewrite requested_view.
			requestedView = view.rider.RequestedView
			retrySelector = graphview.Selector{
				Kind: graphview.SelectorWorktree, CheckoutID: view.rider.CheckoutID,
			}
		}
		if err == nil && want.forbidsFallback() && !requestViewIsExact(selector, view) {
			err = fallbackRefusalError(view)
		}
		if err == nil && want.requireFresh && view != nil && view.checkoutID != "" {
			probe := want.freshnessProbe
			if probe == nil && s.lifecycle != nil {
				probe = s.lifecycle.EnsureCheckoutFresh
			}
			if probe == nil {
				err = graphview.NewViewError(graphview.CodeCapabilityUnavailable,
					"the selected checkout has no lifecycle capable of proving freshness")
			} else {
				freshness, freshErr := probe(ctx, view.checkoutID)
				switch {
				case ctx.Err() != nil:
					err = ctx.Err()
				case freshErr != nil:
					err = graphview.WrapViewError(graphview.CodeCheckoutInaccessible,
						fmt.Sprintf("prove checkout %q fresh", view.checkoutID), freshErr)
				case !freshness.Fresh:
					err = graphview.NewViewError(graphview.CodeViewBuilding,
						fmt.Sprintf("checkout %q is reconciling its current working tree", view.checkoutID))
				}
			}
		}

		if err == nil {
			if requestedView != "" && view != nil && view.rider != nil {
				view.rider.RequestedView = requestedView
			}
			if last != nil {
				last.close()
				last = nil
			}
			s.recordRequestView(view, nil)
			return view, nil
		}

		if view == nil {
			view = requestViewRefusal(selector, err)
		} else {
			markRequestViewRefusal(view, err)
		}
		if last != nil {
			last.close()
		}
		last = cloneRequestViewMetadata(view)
		view.close()

		if ctxErr := ctx.Err(); ctxErr != nil {
			return last, ctxErr
		}
		if !viewErrorIsTransient(err) || !requestShouldWait(want, canWait) {
			return takeRequestView(&last), err
		}
		if !canWait || !time.Now().Before(deadline) {
			return takeRequestView(&last), viewWaitExpiredError(err, deadline)
		}

		interval := requestViewRetryInterval(last, retrySelector)
		remaining := time.Until(deadline)
		if interval > remaining {
			interval = remaining
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return takeRequestView(&last), ctx.Err()
		case <-timer.C:
		}
	}
}

func effectiveViewWaitDeadline(ctx context.Context, want requestViewConsistency) (time.Time, bool) {
	var deadline time.Time
	if want.hasDeadline {
		deadline = want.waitDeadline
	}
	if inherited, ok := ctx.Deadline(); ok {
		inherited = inherited.Add(-viewWaitResponseMargin)
		if deadline.IsZero() || inherited.Before(deadline) {
			deadline = inherited
		}
	}
	return deadline, !deadline.IsZero()
}

func requestShouldWait(want requestViewConsistency, canWait bool) bool {
	if !canWait {
		return false
	}
	return want.requireFresh || want.hasDeadline
}

func requestViewIsExact(selector graphview.Selector, view *requestView) bool {
	if view == nil {
		return selector.Kind == graphview.SelectorAuto
	}
	if view.rider == nil {
		return selector.Kind == graphview.SelectorAuto
	}
	return view.rider.Exact
}

func fallbackRefusalError(view *requestView) error {
	if view != nil && view.rider != nil {
		reason := view.rider.FallbackReason
		if strings.HasPrefix(reason, graphview.CodeViewBuilding) {
			return graphview.NewViewError(graphview.CodeViewBuilding, reason)
		}
		if reason != "" {
			return graphview.NewViewError(graphview.CodeCheckoutInaccessible, reason)
		}
	}
	return graphview.NewViewError(graphview.CodeViewBuilding, "the requested exact view is not ready")
}

func viewErrorIsTransient(err error) bool {
	switch graphview.CodeOf(err) {
	case graphview.CodeViewBuilding:
		return true
	case graphview.CodePrimaryNotReady:
		text := strings.ToLower(err.Error())
		return strings.Contains(text, "building") || strings.Contains(text, "refreshing")
	default:
		return false
	}
}

func viewWaitExpiredError(last error, deadline time.Time) error {
	message := "the requested view is still building"
	if !deadline.IsZero() {
		message = fmt.Sprintf("the requested view was not ready by %s", deadline.Format(time.RFC3339Nano))
	}
	if last != nil {
		return graphview.WrapViewError(graphview.CodeViewBuilding, message, last)
	}
	return graphview.NewViewError(graphview.CodeViewBuilding, message)
}

func requestViewRetryInterval(view *requestView, selector graphview.Selector) time.Duration {
	if view != nil && view.rider != nil && view.rider.RetryAfter > 0 {
		return time.Duration(view.rider.RetryAfter) * time.Second
	}
	if selector.Kind == graphview.SelectorGitRef || selector.Kind == graphview.SelectorCommit {
		return refViewRetryAfterSeconds * time.Second
	}
	return viewConsistencyPollInterval
}

func requestViewRefusal(selector graphview.Selector, err error) *requestView {
	view := &requestView{rider: graphview.NewViewRider(selector)}
	markRequestViewRefusal(view, err)
	return view
}

func markRequestViewRefusal(view *requestView, err error) {
	if view == nil {
		return
	}
	if view.rider == nil {
		view.rider = graphview.NewViewRider(graphview.Selector{Kind: graphview.SelectorAuto})
	}
	code := graphview.CodeOf(err)
	if code == "" {
		code = graphview.CodeViewBuilding
	}
	view.rider.Exact = false
	if view.rider.FallbackReason == "" {
		view.rider.FallbackReason = code
	}
	if view.rider.RequestedState == "" && code == graphview.CodeViewBuilding {
		view.rider.RequestedState = "building"
	}
	if view.rider.ActualState == "" {
		if view.rider.ActualView == "" {
			view.rider.ActualState = "none"
		} else {
			view.rider.ActualState = "stale"
		}
	}
	view.rider.Error = err.Error()
}

func cloneRequestViewMetadata(view *requestView) *requestView {
	if view == nil || view.rider == nil {
		return nil
	}
	rider := *view.rider
	degraded, baseScoped := view.annotations()
	return &requestView{
		kind: view.kind, rider: &rider, baseFallback: view.baseFallback,
		suppressBufferOverlay: true, degraded: degraded, baseScoped: baseScoped,
	}
}

func takeRequestView(view **requestView) *requestView {
	if view == nil {
		return nil
	}
	out := *view
	*view = nil
	return out
}
