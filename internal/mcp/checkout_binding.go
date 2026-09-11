package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

// The three request-level freshness knobs. They are arguments of the request
// rather than of any one tool — the middleware reads them for every tool, no
// handler ever does — so they are named once here.
const (
	requireExactArgName = "require_exact"
	requireFreshArgName = "require_fresh"
	waitDeadlineArgName = "wait_deadline"
)

// requestFreshnessArgNames is the accepted set, in the order the guide states
// them.
var requestFreshnessArgNames = []string{requireExactArgName, requireFreshArgName, waitDeadlineArgName}

// requestFreshnessArgKeys is THE allowlist that makes the three knobs callable
// on every tool. It is the single place the set is enumerated for dispatch:
// wrapToolArgGuard consults it (arg_schema_guard.go) so a guarded tool's closed
// schema cannot refuse a knob the request middleware honours, and the tests
// that pin "callable on every tool" read it too.
//
// They are deliberately NOT published into any tool's InputSchema.Properties.
// Declaring three properties on every registered tool grows every tools/list —
// the compact facade surface and the agent / localization presets are byte
// budgeted (tools_list_budget_test.go, facade_tools_test.go) and those budgets
// do not move for a request-level contract. Discoverability lives once, in the
// shipped instructions: the views guide (guide.go) and the routing policy the
// agent profiles render (internal/profiles/bodies.go).
var requestFreshnessArgKeys = func() map[string]struct{} {
	keys := make(map[string]struct{}, len(requestFreshnessArgNames))
	for _, name := range requestFreshnessArgNames {
		keys[name] = struct{}{}
	}
	return keys
}()

// freshnessArgContainers are the envelopes a facade caller may nest a
// request-level knob inside. The top level is read first; these mirror the
// shape require_exact has always accepted.
var freshnessArgContainers = []string{"options", "arguments"}

// requestBoolArg reads a request-level boolean from the top level or from one
// of the facade envelopes.
func requestBoolArg(req *mcp.CallToolRequest, name string) bool {
	if req == nil {
		return false
	}
	if req.GetBool(name, false) {
		return true
	}
	for _, container := range freshnessArgContainers {
		fields, _ := req.GetArguments()[container].(map[string]any)
		if value, _ := fields[name].(bool); value {
			return true
		}
	}
	return false
}

// requestRawArg reports the first value a request-level knob was sent with,
// whatever its type, so a malformed value is refused rather than ignored.
func requestRawArg(req *mcp.CallToolRequest, name string) (any, bool) {
	if req == nil {
		return nil, false
	}
	args := req.GetArguments()
	if value, present := args[name]; present {
		return value, true
	}
	for _, container := range freshnessArgContainers {
		fields, _ := args[container].(map[string]any)
		if value, present := fields[name]; present {
			return value, true
		}
	}
	return nil, false
}

func requestRequiresExactCheckoutView(req *mcp.CallToolRequest) bool {
	return requestBoolArg(req, requireExactArgName)
}

// requestFreshness is what the caller asked about the freshness of the view
// that answers, parsed once per request.
type requestFreshness struct {
	// requireExact refuses a substituted view. It is enforced by the request
	// middleware for the route, and here for an expired freshness wait.
	requireExact bool
	// requireFresh asks for a bounded wait until the selected checkout route
	// reflects the working copy as it is now.
	requireFresh bool
	// deadline bounds that wait. It is absolute (RFC3339) so a retry carries
	// the same ceiling rather than restarting the clock.
	deadline    time.Time
	hasDeadline bool
	// err is a malformed knob. It is carried rather than returned so the one
	// parse site stays a pure derivation of the request.
	err error
}

// requested reports whether this request asked for anything the freshness
// machinery has to answer for.
func (f requestFreshness) requested() bool { return f.requireFresh }

// effectiveDeadline is the absolute bound the wait runs under: the caller's
// wait_deadline when it named one, otherwise a short default, and never past
// the deadline the request itself is already running under.
//
// The clamp keeps transportDeadlineMargin of room ahead of the request's own
// deadline. Without the margin a wait_deadline at or past the hang firewall's
// budget (boundToolHandler, DefaultToolCallTimeout = 60s) races the firewall:
// both fire at the same instant, the firewall usually wins, and the call is
// abandoned — counted into the process-wide abandonedToolCalls, past whose cap
// every tool call fails fast — instead of answering fresh:false /
// fresh_reason:"deadline_exceeded". A wait that cannot be reported truthfully
// is not a wait worth running, so the bound is pulled in far enough for the
// answer to get out. The margin is the same one boundToolHandler uses against
// an inherited transport deadline, and for the same reason.
func (f requestFreshness) effectiveDeadline(now time.Time, ctx context.Context) time.Time {
	deadline := now.Add(freshnessDefaultWait)
	if f.hasDeadline {
		deadline = f.deadline
	}
	if ctx != nil {
		if bound, ok := ctx.Deadline(); ok {
			bound = bound.Add(-transportDeadlineMargin)
			if bound.Before(deadline) {
				return bound
			}
		}
	}
	return deadline
}

// takeRequestFreshness parses the three request-level knobs.
//
// It deliberately does not strip them from the argument map: require_exact has
// always travelled through to the handler and every tool ignores an argument
// it does not declare. What keeps them callable is that the argument guard
// admits requestFreshnessArgKeys on every tool, and that the middleware parses
// them BEFORE reconcileToolParams runs (overlay.go), so the alias matcher —
// which rewrites exactly the keys a tool does not declare — cannot take a knob
// away from the request before the request has read it. That ordering is
// pinned by TestFreshnessKnobsAreReadBeforeParameterReconciliation, which
// registers a tool whose own parameter sits inside the matcher's edit-distance
// budget of require_fresh; no shipped tool does today
// (TestNoToolAliasesTheFreshnessKnobs), which is why the ordering has to be
// pinned deliberately rather than by any live tool's schema.
func takeRequestFreshness(req *mcp.CallToolRequest) requestFreshness {
	freshness := requestFreshness{
		requireExact: requestRequiresExactCheckoutView(req),
		requireFresh: requestBoolArg(req, requireFreshArgName),
	}
	raw, present := requestRawArg(req, waitDeadlineArgName)
	if !present || raw == nil {
		return freshness
	}
	text, ok := raw.(string)
	if !ok {
		freshness.err = graphview.NewViewError(graphview.CodeInvalidViewSelector,
			fmt.Sprintf("%s must be an absolute RFC3339 timestamp string", waitDeadlineArgName))
		return freshness
	}
	text = strings.TrimSpace(text)
	if text == "" {
		freshness.err = graphview.NewViewError(graphview.CodeInvalidViewSelector,
			fmt.Sprintf("%s must be an absolute RFC3339 timestamp, not an empty string", waitDeadlineArgName))
		return freshness
	}
	when, err := time.Parse(time.RFC3339, text)
	if err != nil {
		freshness.err = graphview.WrapViewError(graphview.CodeInvalidViewSelector,
			fmt.Sprintf("%s %q is not an absolute RFC3339 timestamp", waitDeadlineArgName, text), err)
		return freshness
	}
	if !when.After(time.Now()) {
		freshness.err = graphview.NewViewError(graphview.CodeInvalidViewSelector,
			fmt.Sprintf("%s %s is not in the future; an absolute deadline in the past bounds nothing",
				waitDeadlineArgName, when.Format(time.RFC3339)))
		return freshness
	}
	freshness.deadline, freshness.hasDeadline = when, true
	return freshness
}

// freshnessDefaultWait bounds a require_fresh call that named no deadline. It
// is short on purpose: a caller that wants to wait for a real build says so
// with wait_deadline, and one that only wants to know whether the route is
// already current pays a poll, not a build.
const freshnessDefaultWait = 5 * time.Second

// freshnessRetryBackoff spaces the retries of a wait whose ticket was
// superseded — the working copy moved while the coordinator was sampling it —
// so a tree being edited continuously cannot turn the wait into a spin.
const freshnessRetryBackoff = 25 * time.Millisecond

// The rider vocabulary for a wait that did not end in a fresh route. Every
// value is a fact about this request, never a claim about the view.
const (
	// freshReasonDeadlineExceeded: the bound expired before the route caught
	// up with the working copy.
	freshReasonDeadlineExceeded = "deadline_exceeded"
	// freshReasonInterrupted: the request itself ended first.
	freshReasonInterrupted = "interrupted"
	// freshReasonCoordinatorUnavailable: nothing on this server can publish a
	// checkout generation, so there is no settle signal to wait on.
	//
	// It is a statement ABOUT THE SERVER, and therefore the narrowest of these
	// reasons: only a missing waiter reaches it. A refusal that is about this
	// CHECKOUT — its root moved, its coordinator stopped — gets its own reason
	// below, because telling a caller "no checkout on this server can be
	// refreshed" when one checkout moved is the same class of false claim as
	// stamping a committed base on a lookup failure.
	freshReasonCoordinatorUnavailable = "coordinator_unavailable"
	// freshReasonPublicationFailed: the coordinator answered, and what it
	// answered was a failure.
	freshReasonPublicationFailed = "publication_failed"
	// freshReasonCheckoutRootChanged: the checkout this request waited on is
	// not the checkout the catalog names at that root any more. It is what
	// indexer.ErrCheckoutMutationStale says — "checkout root changed",
	// "checkout root is unavailable", a HEAD or identity that moved under the
	// sample — and it is a fact about THIS checkout: every other checkout on
	// the server can still be refreshed, and re-selecting this one may resolve
	// a different route to wait on.
	freshReasonCheckoutRootChanged = "checkout_root_changed"
	// freshReasonCheckoutRefreshStopped: this checkout's coordinator stopped
	// before the refresh completed (indexer.ErrCheckoutRefreshStopped) — it is
	// closing, or the root it was started for is no longer the root this
	// request named. Also about THIS checkout, not about the server.
	//
	// The two sentinels are kept apart rather than folded into one "checkout
	// unavailable" reason because they are reached for opposite causes and only
	// one of them is worth retrying immediately. They are NOT split further by
	// the message each error wraps: the sentinel is the contract, the sentence
	// after it is not, and matching on it would make the rider a hostage to
	// someone else's wording.
	freshReasonCheckoutRefreshStopped = "checkout_refresh_stopped"
	// freshReasonCommittedBaseAdvance: the view that answered reads a
	// committed base — the shared corpus, a labelled base graph, a ref view,
	// or a dedicated/primary checkout. Advancing a committed base on demand is
	// not implemented; require_fresh waits for a routed working copy only.
	//
	// It is a statement ABOUT THE VIEW, so it is emitted only when the request
	// really did resolve to a committed base. A wait that could not start for
	// a reason that says nothing about the view — a catalog read that failed,
	// a checkout row that vanished between selection and the wait — gets
	// freshReasonWaitTargetUnavailable instead. Labelling those
	// "committed_base_advance_unimplemented" would put a false claim about the
	// view on the response of the one item whose thesis is a truthful rider.
	freshReasonCommittedBaseAdvance = "committed_base_advance_unimplemented"
	// freshReasonWaitTargetUnavailable: the request named a route, and the
	// server could not resolve it — to wait on before the wait, or to read
	// back the publication after it. No catalog wired, a catalog read that
	// failed, a checkout row or route row that is gone. Nothing is claimed
	// about the view: this is a fact about the lookup, and a retry may well
	// succeed.
	freshReasonWaitTargetUnavailable = "wait_target_unavailable"
	// freshReasonRouteWithdrawn: the coordinator published, and the view that
	// answers is not that publication — the route stopped serving a view and a
	// labelled base fallback or a routeless carrier took its place, another
	// checkout answered, or the route moved again between the publication and
	// the re-selection. Whatever answers now is not the thing the wait was
	// about, so the wait's success is not this answer's freshness.
	freshReasonRouteWithdrawn = "route_withdrawn"
)

// requestFreshnessOutcome is what the wait did, rendered onto the response by
// viewRiderFields. It exists so a caller can tell "the route was current" from
// "nobody waited" without inspecting timings.
type requestFreshnessOutcome struct {
	fresh    bool
	reason   string
	waited   time.Duration
	deadline time.Time
}

// checkoutFreshnessWaiter is the coordinator-backed settle signal require_fresh
// waits on.
//
// RequestCheckoutRefresh samples the working copy through the coordinator's own
// sampler and admits a ticket against that sample; the coordinator completes it
// successfully only once the active route names a servable dirty generation
// whose LowerViewFingerprint equals both the admitted sample and a sample taken
// at completion time (internal/indexer/checkout_refresh.go:304-380). That is
// exactly "route ready AND the dirty generation's fingerprint equals the
// coordinator's current sample", asked of the component that owns the answer,
// and a tree that is already current settles through the coordinator's
// settledWithoutBuild preflight without allocating a generation.
//
// It is an interface rather than the concrete *indexer.CheckoutLifecycle so a
// test can drive the wait's outcomes without a live coordinator.
type checkoutFreshnessWaiter interface {
	RequestCheckoutRefresh(ctx context.Context, checkoutID, expectedRoot string) (*indexer.CheckoutRefreshTicket, error)
}

// checkoutFreshness is the waiter this server uses: the test seam when one is
// installed, otherwise the checkout lifecycle.
func (s *Server) checkoutFreshness() checkoutFreshnessWaiter {
	switch {
	case s == nil:
		return nil
	case s.freshnessWaiter != nil:
		return s.freshnessWaiter
	case s.lifecycle != nil:
		return s.lifecycle
	default:
		return nil
	}
}

// awaitCheckoutFreshness waits until the checkout's route reflects the working
// copy, or until the deadline.
//
// It never fabricates freshness: the only true return is a ticket the
// coordinator completed successfully, which is its own statement that the route
// it published describes the tree it sampled.
func (s *Server) awaitCheckoutFreshness(
	ctx context.Context,
	checkout store_sqlite.Checkout,
	deadline time.Time,
) (bool, string) {
	// One nil check for the whole wait. The helpers below and
	// context.WithDeadline all take this context; guarding some of them and
	// not others would advertise a tolerance the next line breaks.
	if ctx == nil {
		ctx = context.Background()
	}
	waiter := s.checkoutFreshness()
	switch {
	case waiter == nil:
		return false, freshReasonCoordinatorUnavailable
	case checkout.CheckoutID == "" || checkout.RootPath == "":
		// A wait target that names no checkout or no root cannot be admitted.
		// That is the lookup, not the server: the coordinator is there and can
		// publish for every checkout that does have an id and a root.
		return false, freshReasonWaitTargetUnavailable
	case !graphview.ServesAutomaticView(checkout):
		// A dedicated or primary checkout is read from the indexed corpus;
		// there is no checkout route to advance and the committed base's own
		// advancement is not on demand.
		return false, freshReasonCommittedBaseAdvance
	}
	for {
		if ctx.Err() != nil {
			return false, freshReasonInterrupted
		}
		if !time.Now().Before(deadline) {
			return false, freshReasonDeadlineExceeded
		}
		waitCtx, cancel := context.WithDeadline(ctx, deadline)
		ticket, err := waiter.RequestCheckoutRefresh(waitCtx, checkout.CheckoutID, checkout.RootPath)
		cancel()
		if err != nil {
			if freshnessWaitRetryable(err) {
				if !freshnessBackoff(ctx, deadline) {
					return false, freshnessExpiryReason(ctx)
				}
				continue
			}
			if freshnessContextExpiry(err) {
				// waitCtx carries exactly two bounds — this request's own
				// context and this wait's deadline — so a context error out
				// of the admission is one of those two expiring. It is a
				// fact about the bound, and reporting it as
				// coordinator_unavailable states something false about the
				// server: that nothing here can publish a generation. The
				// retryable check runs first so a busy/superseded refusal
				// that merely wraps a context error keeps its own meaning.
				return false, freshnessExpiryReason(ctx)
			}
			if reason, mapped := freshnessCheckoutErrorReason(err); mapped {
				return false, reason
			}
			return false, freshReasonCoordinatorUnavailable
		}
		if ticket == nil || ticket.Ticket == nil || ticket.Ticket.Done == nil {
			return false, freshReasonCoordinatorUnavailable
		}
		fresh, reason, retry := awaitFreshnessTicket(ctx, ticket, deadline)
		if !retry {
			return fresh, reason
		}
		// The working copy moved while the coordinator was sampling it. Ask
		// again against the newer tree rather than reporting the older one's
		// publication as this request's freshness.
		if !freshnessBackoff(ctx, deadline) {
			return false, freshnessExpiryReason(ctx)
		}
	}
}

// awaitFreshnessTicket blocks on one admitted ticket. The third return says the
// wait should be re-admitted against a newer sample.
//
// ctx is non-nil: awaitCheckoutFreshness, the only caller, normalises it once
// for the whole wait.
func awaitFreshnessTicket(
	ctx context.Context,
	ticket *indexer.CheckoutRefreshTicket,
	deadline time.Time,
) (fresh bool, reason string, retry bool) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case result, open := <-ticket.Ticket.Done:
		if !open {
			return false, freshReasonCoordinatorUnavailable, false
		}
		if result.Err == nil {
			return true, "", false
		}
		if freshnessWaitRetryable(result.Err) {
			return false, "", true
		}
		if freshnessContextExpiry(result.Err) {
			// The coordinator abandoned the ticket because the context this
			// wait handed it ended. That is this request's bound expiring,
			// not a publication that failed.
			return false, freshnessExpiryReason(ctx), false
		}
		if reason, mapped := freshnessCheckoutErrorReason(result.Err); mapped {
			// The coordinator failed the ticket because THIS checkout stopped
			// being refreshable — it is closing, or its root moved. Reporting
			// that as publication_failed would say a build was attempted and
			// broke, which is a different thing to retry.
			return false, reason, false
		}
		return false, freshReasonPublicationFailed, false
	case <-timer.C:
		return false, freshReasonDeadlineExceeded, false
	case <-ctx.Done():
		return false, freshReasonInterrupted, false
	}
}

// freshnessBackoff pauses between admissions. It reports false when the pause
// would cross the deadline or the request ended.
func freshnessBackoff(ctx context.Context, deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	pause := freshnessRetryBackoff
	if remaining < pause {
		pause = remaining
	}
	timer := time.NewTimer(pause)
	defer timer.Stop()
	select {
	case <-timer.C:
		return time.Now().Before(deadline)
	case <-ctx.Done():
		return false
	}
}

func freshnessExpiryReason(ctx context.Context) string {
	if ctx.Err() != nil {
		return freshReasonInterrupted
	}
	return freshReasonDeadlineExceeded
}

// freshnessContextExpiry reports whether an error is one of the two contexts
// the wait itself imposes ending, rather than anything the coordinator has to
// say about this checkout. The wait hands the coordinator ctx + deadline and
// nothing else, so a context error coming back is always one of those.
func freshnessContextExpiry(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// freshnessCheckoutErrorReason maps the coordinator's two "this checkout
// cannot be refreshed" sentinels onto their own rider reasons.
//
// Both used to fall through to coordinator_unavailable, whose own doc claims
// that NOTHING on this server can publish a checkout generation. That is false
// for a checkout whose root changed or whose coordinator closed: every other
// checkout still refreshes, and the caller's next move is different in each
// case. The check runs AFTER freshnessWaitRetryable and AFTER
// freshnessContextExpiry, so a busy/superseded refusal and this wait's own
// bound both keep their meanings even when they wrap one of these sentinels.
func freshnessCheckoutErrorReason(err error) (string, bool) {
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, indexer.ErrCheckoutMutationStale):
		return freshReasonCheckoutRootChanged, true
	case errors.Is(err, indexer.ErrCheckoutRefreshStopped):
		return freshReasonCheckoutRefreshStopped, true
	default:
		return "", false
	}
}

// freshnessWaitRetryable separates "ask again, the world moved" from "this
// checkout cannot answer". A superseded ticket means the tree changed under the
// sample; a busy coordinator is still activating; a full queue is transient.
func freshnessWaitRetryable(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, indexer.ErrCheckoutRefreshSuperseded),
		errors.Is(err, indexer.ErrCheckoutMutationBusy),
		errors.Is(err, indexer.ErrCheckoutRefreshQueueFull):
		return true
	default:
		return false
	}
}

// freshnessDeadlineRefusal is the typed refusal a require_exact + require_fresh
// caller gets when the bound expired. It names the deadline, because the one
// thing the caller must decide is whether to extend it.
func freshnessDeadlineRefusal(deadline time.Time, waited time.Duration) error {
	return graphview.NewViewError(graphview.CodeViewBuilding, fmt.Sprintf(
		"the selected checkout view did not reach the current working copy before wait_deadline %s "+
			"(waited %dms); require_exact refused the stale route and no fallback was served",
		deadline.UTC().Format(time.RFC3339), waited.Milliseconds()))
}

// freshnessExactRefusal is the typed refusal for every OTHER way a
// require_exact + require_fresh call can end without a fresh route.
//
// require_exact means "do not hand me something I did not ask for". A caller
// that also said require_fresh asked for a route that reflects the working
// copy; answering out of a route that demonstrably does not — because the
// coordinator is gone, because this checkout's root changed or its coordinator
// stopped, because publication failed, because the route was withdrawn under
// the wait, because the view reads a committed base whose advancement is not
// implemented, or because the wait target could not be resolved — is exactly
// the substitution require_exact exists to refuse. The
// expired-deadline case keeps its own refusal (freshnessDeadlineRefusal): it is
// the one outcome where the actionable next step is a bigger wait_deadline, so
// the bound is named.
//
// The reason is carried verbatim so the refusal text and the fresh_reason a
// non-exact caller would have received are the same vocabulary.
func freshnessExactRefusal(reason string, waited time.Duration) error {
	if reason == "" {
		reason = freshReasonRouteWithdrawn
	}
	return graphview.NewViewError(graphview.CodeViewBuilding, fmt.Sprintf(
		"the selected checkout view is not current (fresh_reason %q after %dms); "+
			"require_fresh asked for the working copy and require_exact refused the stale route, "+
			"so no fallback was served",
		reason, waited.Milliseconds()))
}

// checkoutForRequestPath is the shared catalog boundary for daemon admission,
// session scope and request view selection. A cache miss observes only this
// checkout of an already known Git family; it never tracks a new repository or
// waits for parsing. Existing identities take the catalog-only fast path.
func (s *Server) checkoutForRequestPath(ctx context.Context, path string) (store_sqlite.Checkout, bool, error) {
	checkout, found, err := s.registeredCheckoutForPath(ctx, path)
	if err != nil {
		return checkout, false, err
	}
	var parentErr error
	if found {
		parentErr = checkoutControlRootOwnsPath(checkout.RootPath, path)
		if parentErr == nil {
			return checkout, true, nil
		}
	}
	if s.lifecycle == nil {
		return store_sqlite.Checkout{}, false, parentErr
	}
	// CWD observation establishes a session's scope. A different explicit
	// path must pass that scope before allocation or coordinator activation,
	// not merely before its graph data is returned. The authorizer is called
	// outside lifecycle locks; deriving the scope may bind the own CWD once.
	var authorize []func(string) error
	if cwd := SessionCWDFromContext(ctx); cwd != "" && !pathkey.EqualPaths(canonicalWorktreeSelectorRoot(cwd), canonicalWorktreeSelectorRoot(path)) {
		authorize = append(authorize, func(prefix string) error {
			return s.repoPrefixInSessionScope(ctx, prefix, prefix)
		})
	}
	observed, present, observeErr := s.lifecycle.ObserveCheckoutPath(ctx, path, authorize...)
	if observeErr != nil {
		return store_sqlite.Checkout{}, false, observeErr
	}
	if !present {
		return store_sqlite.Checkout{}, false, parentErr
	}
	return observed, true, nil
}
