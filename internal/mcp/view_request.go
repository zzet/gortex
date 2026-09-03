package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// requestViewCtxKey carries the view decision made for one `tools/call`.
// Unexported so nothing outside this package can smuggle a reader onto an
// unrelated context.
type requestViewCtxKey struct{}

// requestView is what one request reads through, plus what the response says
// about it.
type requestView struct {
	// reader is the composed routed stack. Nil means the base corpus serves,
	// which is what every request read before routed views existed.
	reader graph.Reader
	// materialized is the leased view behind reader, released on request end.
	materialized *graphview.RepoView
	// candidates and content are the stack expressed as search corpora —
	// what reader cannot serve, since no composition carries an index. Both
	// are bound once at materialization; see bindSources.
	candidates []query.ViewLayerSource
	content    *viewContentSearcher
	// rider travels on the response whenever the caller named a view or
	// something other than the base answered.
	rider *graphview.ViewRider
	// files is the committed-tree file surface a view with no working copy
	// reads through. Nil for every view that has one.
	files *refViewFiles
	// viewRoot is the working copy this view's content is on disk at: the
	// routed checkout's root. Empty for a view of a committed tree, which is
	// the whole difference a filesystem-backed capability turns on.
	viewRoot string
	// suppressBufferOverlay is set only for an unavailable checkout's
	// primary-base fallback. Grace answers must exclude both persisted dirty
	// state and session buffers; a normal cold-build fallback may still compose
	// the caller's live editor buffers over its lower view.
	suppressBufferOverlay bool

	// mu guards the annotations the request collects while it runs. The
	// capability evaluation writes before the handler starts, but a handler
	// may annotate from a goroutine it fans out to.
	mu sync.Mutex
	// degraded lists capabilities this view does not serve completely that
	// the request did not require. They never fail it — they ride back on
	// the rider so a thin answer is legible as one.
	degraded []graphview.CapabilityStatus
	// baseScoped lists capabilities a base-scoped engine answered while
	// this view served the request.
	baseScoped []graphview.CapabilityID
}

// completeness is what the materialized view can answer, nil for a request
// the base corpus serves.
func (v *requestView) completeness() graphview.Completeness {
	if v == nil || v.materialized == nil {
		return nil
	}
	return v.materialized.Completeness
}

// noteDegraded records capabilities that shaped the answer without failing
// the request.
func (v *requestView) noteDegraded(statuses []graphview.CapabilityStatus) {
	if v == nil || len(statuses) == 0 {
		return
	}
	v.mu.Lock()
	v.degraded = append(v.degraded, statuses...)
	v.mu.Unlock()
}

// noteBaseScoped records capabilities a base-scoped engine answered.
func (v *requestView) noteBaseScoped(caps []graphview.CapabilityID) {
	if v == nil || len(caps) == 0 {
		return
	}
	v.mu.Lock()
	v.baseScoped = mergeCapabilities(v.baseScoped, caps)
	v.mu.Unlock()
}

// annotations reports what the request collected, for the rider.
func (v *requestView) annotations() ([]graphview.CapabilityStatus, []graphview.CapabilityID) {
	if v == nil {
		return nil, nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.degraded), slices.Clone(v.baseScoped)
}

// routed reports whether a composed checkout view — rather than the base
// corpus — answers this request.
func (v *requestView) routed() bool { return v != nil && v.reader != nil }

// acceptsBufferOverlay reports whether session-local editor buffers may layer
// over this answer. A grace fallback deliberately returns the stable primary
// graph only: composing the disappeared checkout's buffers would make the
// response look inexact while still leaking the unavailable working copy.
func (v *requestView) acceptsBufferOverlay() bool {
	return v == nil || !v.suppressBufferOverlay
}

// close releases the generations the view leased and the git child its file
// surface holds. Idempotent and nil-safe.
func (v *requestView) close() {
	if v == nil {
		return
	}
	v.files.close()
	v.materialized.Close()
}

func withRequestView(ctx context.Context, v *requestView) context.Context {
	if ctx == nil || v == nil {
		return ctx
	}
	return context.WithValue(ctx, requestViewCtxKey{}, v)
}

func requestViewFromContext(ctx context.Context) *requestView {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(requestViewCtxKey{}).(*requestView)
	return v
}

// SetMaterializer wires the routed-view materializer built over the shared
// store's catalog. Passing nil (or never calling this) leaves every request
// on the base corpus: the view argument then reports the capability as
// unavailable instead of quietly answering from somewhere else.
func (s *Server) SetMaterializer(m *graphview.Materializer) {
	if s == nil {
		return
	}
	s.materializer = m
}

// Materializer returns the routed-view materializer the server reads through,
// nil when the backend carries no view catalog. It is how a caller that also
// owns retirement can check that both sides pin generations with the same
// lease manager.
func (s *Server) Materializer() *graphview.Materializer {
	if s == nil {
		return nil
	}
	return s.materializer
}

// viewArgName is the request-level argument every tool honours. It is read
// and stripped by the request middleware, so no handler sees it. The shared
// schema below publishes that middleware contract on every tool surface.
const viewArgName = "view"

// viewSelectorFields are the object keys a view argument may carry. Anything
// else is a typo the caller must see, not a field to ignore — an ignored
// selector field would silently answer about a different view.
var viewSelectorFields = map[string]bool{
	"kind": true, "graph_id": true, "checkout_id": true, "value": true,
}

// viewSelectorSchema is the single public schema for the request-level view
// selector. Each branch mirrors ParseSelector's field ownership, so clients
// can construct a valid selector without guessing which identifiers combine.
func viewSelectorSchema() map[string]any {
	kind := func(value string) map[string]any {
		return map[string]any{"type": "string", "const": value}
	}
	text := func(description string) map[string]any {
		return map[string]any{"type": "string", "minLength": 1, "description": description}
	}
	object := func(properties map[string]any, required ...string) map[string]any {
		schema := map[string]any{
			"type":                 "object",
			"properties":           properties,
			"additionalProperties": false,
		}
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	}
	return map[string]any{
		"type":        "object",
		"description": "Select the graph view for this request; omit it (or use auto) to resolve from the session workspace.",
		"oneOf": []any{
			object(map[string]any{"kind": kind(string(graphview.SelectorAuto))}),
			object(map[string]any{
				"kind":     kind(string(graphview.SelectorBase)),
				"graph_id": text("Dedicated graph identifier."),
			}, "kind", "graph_id"),
			object(map[string]any{
				"kind":        kind(string(graphview.SelectorWorktree)),
				"checkout_id": text("Registered checkout identifier."),
			}, "kind", "checkout_id"),
			object(map[string]any{
				"kind":     kind(string(graphview.SelectorGitRef)),
				"graph_id": text("Optional dedicated graph identifier."),
				"value":    text("Full ref name, for example refs/heads/main."),
			}, "kind", "value"),
			object(map[string]any{
				"kind":     kind(string(graphview.SelectorCommit)),
				"graph_id": text("Optional dedicated graph identifier."),
				"value":    text("Full lowercase Git object identifier."),
			}, "kind", "value"),
		},
	}
}

// compactViewSelectorSchema keeps tools/list bounded. The exact conditional
// selector contract is available per operation through capabilities.
func compactViewSelectorSchema() map[string]any {
	return map[string]any{"type": "object"}
}

func publishViewSelectorSchema(tool *mcp.Tool) {
	if tool == nil {
		return
	}
	if tool.InputSchema.Properties == nil {
		tool.InputSchema.Properties = make(map[string]any)
	}
	tool.InputSchema.Properties[viewArgName] = compactViewSelectorSchema()
}

// takeViewSelector pulls the structured view argument off the request and
// removes it from the argument map.
//
// It runs before parameter reconciliation so the alias matcher cannot rewrite
// `view` into some tool's own similarly-named parameter, and before any
// handler runs so every read tool honours the selector without per-tool
// plumbing.
func takeViewSelector(req *mcp.CallToolRequest) (graphview.Selector, error) {
	auto := graphview.Selector{Kind: graphview.SelectorAuto}
	if req == nil {
		return auto, nil
	}
	args, ok := req.Params.Arguments.(map[string]any)
	if !ok {
		return auto, nil
	}
	raw, present := args[viewArgName]
	if !present {
		return auto, nil
	}
	delete(args, viewArgName)
	if raw == nil {
		return auto, nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return graphview.Selector{}, graphview.NewViewError(graphview.CodeInvalidViewSelector,
			"the view argument must be an object naming a kind")
	}
	fields := make(map[string]string, len(obj))
	for name, value := range obj {
		if !viewSelectorFields[name] {
			return graphview.Selector{}, graphview.NewViewError(graphview.CodeInvalidViewSelector,
				fmt.Sprintf("the view argument has no field %q", name))
		}
		text, ok := value.(string)
		if !ok {
			return graphview.Selector{}, graphview.NewViewError(graphview.CodeInvalidViewSelector,
				fmt.Sprintf("view field %q must be a string", name))
		}
		fields[name] = text
	}
	return graphview.ParseSelector(fields["kind"], fields["graph_id"], fields["checkout_id"], fields["value"])
}

// requestViewPolicy carries the operation-level guarantees that affect view
// selection. It is derived after parameter reconciliation, from the same
// facade operation/effect registry that dispatch and mutation authorization
// use, so a spelling alias cannot accidentally widen grace access.
type requestViewPolicy struct {
	allowGraceBaseFallback bool
}

func (s *Server) requestViewPolicy(req *mcp.CallToolRequest) requestViewPolicy {
	return requestViewPolicy{allowGraceBaseFallback: s.requestAllowsGraceBaseFallback(req)}
}

// requestAllowsGraceBaseFallback admits only read-effect graph/search
// operations. Exact source/file reads, filesystem-backed search, LSP work, and
// every write effect stay strict while the checkout is unavailable.
func (s *Server) requestAllowsGraceBaseFallback(req *mcp.CallToolRequest) bool {
	if s == nil || s.facades == nil || req == nil || requestTargetsFile(req) {
		return false
	}

	name := req.Params.Name
	if isFacadeToolName(name) {
		spec, ok := s.viewFacadeOperation(req)
		return ok && graceFallbackSpecEligible(spec)
	}

	specs := s.facades.byLegacy[name]
	if len(specs) == 0 {
		return false
	}
	for _, spec := range specs {
		// A legacy handler may be reachable through operations with different
		// effects (change_contract is the canonical example). Ambiguity must
		// fail closed rather than choosing whichever mapping was registered
		// first.
		if !graceFallbackSpecEligible(spec) {
			return false
		}
	}
	return true
}

// viewFacadeOperation resolves the compact request exactly as handleFacade
// does up to dispatch. Keeping grace authorization on the selected operation
// is what lets analyze reads fall back without admitting its administrative
// kinds.
func (s *Server) viewFacadeOperation(req *mcp.CallToolRequest) (facadeOperationSpec, bool) {
	name := req.Params.Name
	args := req.GetArguments()
	operation := resolveFacadeOperationAlias(name, normalizeFacadeOperation(req.GetString("operation", "")))
	if name == "analyze" {
		operation = requestedAnalyzeKind(args)
		if operation == "" {
			operation = "help"
		}
	}
	if operation == "" {
		operation = inferFacadeOperation(name, args)
	}
	if operation == "" {
		operation = defaultFacadeOperation(name)
	}
	if name == "read" {
		operation = normalizeFacadeReadOperation(operation, args)
	}
	return s.capabilityOperation(name, operation)
}

func graceFallbackSpecEligible(spec facadeOperationSpec) bool {
	if spec.Effect != facadeEffectRead || spec.Facade == "read" {
		return false
	}
	caps := capabilityDefaultsFor(spec.Legacy)
	if len(caps) == 0 {
		return false
	}
	for _, capability := range caps {
		switch capability {
		case graphview.CapSourceSnapshot,
			graphview.CapSourceConfig,
			graphview.CapSearchText,
			graphview.CapLSPReferences,
			graphview.CapLSPDiagnostics,
			graphview.CapLSPHover,
			graphview.CapLSPRename,
			graphview.CapLSPCodeActions:
			return false
		}
	}
	return true
}

// requestTargetsFile catches facade selectors and legacy file arguments before
// they are lowered. The capability filter above catches known file engines;
// this guard covers graph/analysis operations whose ordinary family default is
// broader than one request's concrete target.
func requestTargetsFile(req *mcp.CallToolRequest) bool {
	if req == nil {
		return false
	}
	args, ok := req.Params.Arguments.(map[string]any)
	if !ok {
		return false
	}
	if _, present := args["file"]; present {
		return true
	}
	for _, container := range []string{"target", "to", "source", "context"} {
		fields, _ := args[container].(map[string]any)
		if _, present := fields["file"]; present {
			return true
		}
	}
	return false
}

// resolveRequestView decides what this request reads through.
//
// Precedence is explicit selector, then the session's cwd binding, then the
// base corpus. An explicit selector fails loudly; a cwd binding that cannot
// be served falls back to the base and says so on the response.
//
// Materialization is per request. Caching it across requests needs the route
// epoch as the key — a route flip has to invalidate the cached stack — and
// that is the optimization this deliberately leaves for later.
func (s *Server) resolveRequestView(
	ctx context.Context,
	selector graphview.Selector,
	policy requestViewPolicy,
) (*requestView, error) {
	view, err := s.selectRequestView(ctx, selector, policy)
	s.recordRequestView(view, err)
	return view, err
}

// recordRequestView counts what answered this request, and logs the ones that
// did not answer what was asked for.
//
// The counter is by view kind and — for an inexact answer — by the code that
// explains the substitution; neither carries a checkout, a ref or a
// fingerprint. The log line beside a fallback carries all three, because "the
// worktree lane fell back to base 40 times" is only actionable once you know
// which worktree and which generation stack it was trying to reach.
func (s *Server) recordRequestView(view *requestView, err error) {
	if err != nil {
		return
	}
	viewmetrics.Count(viewmetrics.RequestServedTotal, requestViewKind(view))
	if view == nil || view.rider == nil || view.rider.Exact {
		return
	}
	reason := viewmetrics.FallbackReasonCode(view.rider.FallbackReason)
	viewmetrics.Count(viewmetrics.RequestFallbackTotal, reason)
	if s.logger == nil {
		return
	}
	fields := []zap.Field{
		zap.String("requested_view", view.rider.RequestedView),
		zap.String("actual_view", view.rider.ActualView),
		zap.String("reason", reason),
		zap.String("detail", view.rider.FallbackReason),
	}
	if view.rider.GraphID != "" {
		fields = append(fields, zap.String("graph", view.rider.GraphID))
	}
	if view.rider.CheckoutID != "" {
		fields = append(fields, zap.String("checkout", view.rider.CheckoutID))
	}
	if view.rider.ViewFingerprint != "" {
		fields = append(fields, zap.String("view_fingerprint", view.rider.ViewFingerprint))
	}
	if view.materialized != nil {
		fields = append(fields, zap.Int64s("generations", view.materialized.Generations()))
	}
	if view.rider.BuildToken != "" {
		fields = append(fields, zap.String("build_token", view.rider.BuildToken))
	}
	s.logger.Debug("view routing: served a fallback view", fields...)
}

// requestViewKind names the shape of view that answered: the indexed corpus,
// a routed working copy, or a committed tree. The file surface is what tells
// the last two apart — only a view with no working copy reads its bytes out of
// the object store.
func requestViewKind(view *requestView) string {
	switch {
	case view == nil || view.reader == nil:
		return viewmetrics.ViewBase
	case view.files != nil:
		return viewmetrics.ViewRef
	default:
		return viewmetrics.ViewWorktree
	}
}

// selectRequestView is resolveRequestView's decision, split out so the
// recording above wraps every path through it exactly once.
func (s *Server) selectRequestView(
	ctx context.Context,
	selector graphview.Selector,
	policy requestViewPolicy,
) (*requestView, error) {
	if s == nil || s.materializer == nil {
		if selector.Kind == graphview.SelectorAuto {
			return nil, nil
		}
		return nil, graphview.NewViewError(graphview.CodeCapabilityUnavailable,
			"this store carries no view catalog, so only the automatic view can be served")
	}
	switch selector.Kind {
	case graphview.SelectorAuto:
		return s.viewForSessionCWD(ctx)
	case graphview.SelectorWorktree:
		return s.viewForWorktreeSelector(ctx, selector, policy)
	case graphview.SelectorBase:
		return s.viewForBaseSelector(ctx, selector)
	case graphview.SelectorGitRef, graphview.SelectorCommit:
		return s.viewForRefSelector(ctx, selector)
	default:
		return nil, graphview.NewViewError(graphview.CodeCapabilityUnavailable,
			fmt.Sprintf("a %s selector names a view no builder produces yet", string(selector.Kind)))
	}
}

// viewForSessionCWD binds the session's working directory to a registered
// checkout and routes the request to that checkout's composed view.
//
// Only an automatic checkout is routed here. A dedicated checkout and the
// family's primary are served from the indexed corpus, which is exactly what
// the base path already does for them.
func (s *Server) viewForSessionCWD(ctx context.Context) (*requestView, error) {
	cwd := SessionCWDFromContext(ctx)
	if cwd == "" {
		return nil, nil
	}
	checkout, found, err := graphview.CheckoutForPath(ctx, s.materializer.Catalog, s.viewFamilies(ctx), cwd)
	if err != nil {
		// The binding is an optimization over the base corpus, not a
		// precondition for answering: a catalog that cannot be read still
		// answers from the base. The cwd may well sit inside a routed
		// checkout, so the degradation rides on the response rather than
		// passing for an exact answer.
		if s.logger != nil {
			s.logger.Debug("view routing: could not bind the session cwd to a checkout", zap.Error(err))
		}
		return viewFallback(false, graphview.NewViewRider(graphview.Selector{Kind: graphview.SelectorAuto}), err)
	}
	if !found || !graphview.ServesAutomaticView(checkout) {
		return nil, nil
	}
	requested := graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: checkout.CheckoutID}
	return s.materializeRequestView(ctx, requested, checkout, false)
}

// viewForWorktreeSelector serves an explicitly named checkout. Every refusal
// is reported with its own code: the caller asked for one specific view and
// must never be handed a different one.
func (s *Server) viewForWorktreeSelector(
	ctx context.Context,
	selector graphview.Selector,
	policy requestViewPolicy,
) (*requestView, error) {
	catalog := s.materializer.Catalog
	checkout, found, err := catalog.GetCheckout(ctx, selector.CheckoutID)
	switch {
	case err != nil:
		return nil, graphview.WrapViewError(graphview.CodeCheckoutInaccessible,
			fmt.Sprintf("read checkout %q", selector.CheckoutID), err)
	case !found:
		return nil, graphview.NewViewError(graphview.CodeCheckoutInaccessible,
			fmt.Sprintf("checkout %q is not registered", selector.CheckoutID))
	}
	if checkout.State != store_sqlite.CheckoutStateReady {
		stateErr := graphview.NewViewError(graphview.CodeCheckoutInaccessible,
			fmt.Sprintf("checkout %q is %s", checkout.CheckoutID, string(checkout.State)))
		if checkoutStateAllowsBaseFallback(checkout.State) {
			// The disappeared checkout may still own a dedicated graph row whose
			// repository prefix is no longer in the live workspace. Grace serves
			// the surviving family primary, so scope that answer by the primary
			// rather than by stale checkout ownership before applying policy.
			primary, primaryErr := s.familyPrimaryRegistration(ctx, checkout.FamilyID)
			if primaryErr != nil {
				return nil, primaryErr
			}
			if err := s.repoPrefixInSessionScope(ctx, primary.RepoPrefix, checkout.CheckoutID); err != nil {
				return nil, err
			}
			if primary.State != reconcile.GraphStateReady {
				return nil, graphview.NewViewError(graphview.CodePrimaryNotReady,
					fmt.Sprintf("primary graph %q is %s", primary.GraphID, primary.State))
			}
			if !policy.allowGraceBaseFallback {
				return nil, stateErr
			}
			return graceBaseFallback(selector, checkout, primary)
		}
		if err := s.checkoutInSessionScope(ctx, checkout); err != nil {
			return nil, err
		}
		return nil, stateErr
	}
	if err := s.checkoutInSessionScope(ctx, checkout); err != nil {
		return nil, err
	}
	if _, err := s.familyPrimary(ctx, checkout.FamilyID); err != nil {
		return nil, err
	}
	return s.materializeRequestView(ctx, selector, checkout, true)
}

func checkoutStateAllowsBaseFallback(state store_sqlite.CheckoutState) bool {
	switch state {
	case store_sqlite.CheckoutStateAvailabilityGrace,
		store_sqlite.CheckoutStateRemovalGrace,
		store_sqlite.CheckoutStateUnavailable:
		return true
	default:
		return false
	}
}

// graceBaseFallback serves only the sealed primary corpus. It does not carry a
// routed reader or a filesystem root, and suppressBufferOverlay keeps session
// buffers from reintroducing the unavailable checkout above the base.
func graceBaseFallback(
	selector graphview.Selector,
	checkout store_sqlite.Checkout,
	primary store_sqlite.DedicatedGraph,
) (*requestView, error) {
	rider := graphview.NewViewRider(selector)
	actual := graphview.Selector{Kind: graphview.SelectorBase, GraphID: primary.GraphID}
	if err := rider.MarkFallback(actual.String(), string(checkout.State)); err != nil {
		return nil, err
	}
	rider.GraphID = primary.GraphID
	rider.CheckoutID = checkout.CheckoutID
	rider.RequestedState = string(store_sqlite.CheckoutStateReady)
	rider.ActualState = string(checkout.State)
	return &requestView{rider: rider, suppressBufferOverlay: true}, nil
}

// viewForBaseSelector pins the request to a named base graph. A dedicated
// graph is read from the indexed corpus, so the selector's work is proving
// the graph exists and is ready — and naming it on the response.
func (s *Server) viewForBaseSelector(ctx context.Context, selector graphview.Selector) (*requestView, error) {
	dedicated, found, err := s.materializer.Catalog.GetDedicatedGraph(ctx, selector.GraphID)
	switch {
	case err != nil:
		return nil, graphview.WrapViewError(graphview.CodeCheckoutInaccessible,
			fmt.Sprintf("read graph %q", selector.GraphID), err)
	case !found:
		return nil, graphview.NewViewError(graphview.CodeInvalidViewSelector,
			fmt.Sprintf("graph %q is not registered", selector.GraphID))
	}
	// The scope ceiling is checked before the state, so a session outside the
	// workspace cannot tell a building graph from a ready one in a sibling
	// workspace. This is the order the worktree selector holds to.
	if err := s.repoPrefixInSessionScope(ctx, dedicated.RepoPrefix, selector.GraphID); err != nil {
		return nil, err
	}
	if dedicated.State != reconcile.GraphStateReady {
		return nil, graphview.NewViewError(graphview.CodeViewBuilding,
			fmt.Sprintf("graph %q is %s", selector.GraphID, dedicated.State))
	}
	rider := graphview.NewViewRider(selector)
	rider.MarkExact(selector.String())
	rider.GraphID = dedicated.GraphID
	return &requestView{rider: rider}, nil
}

// materializeRequestView turns a routed checkout into the reader that answers
// the request.
//
// strict separates the two callers. An explicit selector must fail rather than
// answer about something else; a cwd binding falls back to the base corpus and
// records why, so a half-built route degrades to today's answer instead of an
// error — and never silently.
func (s *Server) materializeRequestView(
	ctx context.Context,
	requested graphview.Selector,
	checkout store_sqlite.Checkout,
	strict bool,
) (*requestView, error) {
	rider := graphview.NewViewRider(requested)
	route, found, err := s.materializer.Catalog.GetCheckoutRoute(ctx, checkout.CheckoutID)
	switch {
	case err != nil:
		return viewFallback(strict, rider, graphview.WrapViewError(graphview.CodeCheckoutInaccessible,
			fmt.Sprintf("read the route of checkout %q", checkout.CheckoutID), err))
	case !found || !graphview.RouteReady(route):
		// The selected worktree has no ready route — it may be dormant, never
		// built since startup. Kick its coordinator so the awaited or retried
		// build actually runs, then return the labelled base fallback now. The
		// activation is fire-and-forget; nothing here waits on it.
		s.activateSelectedCheckout(checkout.CheckoutID, "view requested but not routed")
		return viewFallback(strict, rider, graphview.NewViewError(graphview.CodeViewBuilding,
			fmt.Sprintf("checkout %q is not fully routed yet", checkout.CheckoutID)))
	}
	view, err := s.materializer.MaterializeCheckout(ctx, checkout.CheckoutID)
	if err != nil {
		// A route that will not materialize is a stale-HEAD or half-built
		// generation; kick a rebuild the same way before falling back.
		s.activateSelectedCheckout(checkout.CheckoutID, "view requested but materialization failed")
		return viewFallback(strict, rider, err)
	}
	rider.MarkExact(requested.String())
	rider.GraphID = view.ID.BaseGraphID
	rider.CheckoutID = checkout.CheckoutID
	routed := &requestView{
		reader:       view.Reader,
		materialized: view,
		rider:        rider,
		viewRoot:     checkout.RootPath,
	}
	routed.bindSources(view.GenerationSources(), s.graph)
	return routed, nil
}

// viewFallback either propagates the failure (an explicit selector) or serves
// the base corpus with the reason recorded on the rider (a cwd binding).
// activateSelectedCheckout kicks a dormant checkout's coordinator when a
// request selects a view that is not routed yet. It is fire-and-forget: the
// request returns its labelled base fallback now and the build runs behind it,
// so a later retry finds the composed view ready. Nil-safe for the surfaces
// wired without a lifecycle.
func (s *Server) activateSelectedCheckout(checkoutID, reason string) {
	if s == nil || s.lifecycle == nil || checkoutID == "" {
		return
	}
	s.lifecycle.ActivateCheckout(checkoutID, reason)
}

func viewFallback(strict bool, rider *graphview.ViewRider, err error) (*requestView, error) {
	if strict {
		return nil, err
	}
	reason := graphview.CodeOf(err)
	if reason == "" {
		reason = graphview.CodeViewBuilding
	}
	if markErr := rider.MarkFallback(string(graphview.SelectorBase), reason); markErr != nil {
		return nil, markErr
	}
	return &requestView{rider: rider}, nil
}

// viewFamilies lists the checkout families the indexed corpus reaches, one
// per repository prefix it carries. The catalog indexes checkouts by family,
// so this is what turns a working directory into a checkout row.
func (s *Server) viewFamilies(ctx context.Context) []string {
	if s.graph == nil {
		return nil
	}
	prefixes := s.graph.RepoPrefixes()
	seen := make(map[string]bool, len(prefixes))
	out := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		if prefix == "" {
			continue
		}
		dedicated, found, err := s.materializer.Catalog.GetDedicatedGraph(ctx, indexer.GraphIDFor(prefix))
		if err != nil || !found || dedicated.FamilyID == "" || seen[dedicated.FamilyID] {
			continue
		}
		seen[dedicated.FamilyID] = true
		out = append(out, dedicated.FamilyID)
	}
	return out
}

// familyPrimary resolves the one primary base and requires it to be ready.
// Automatic routes and grace fallbacks share this invariant: naming a primary
// row is not enough when its graph cannot truthfully answer yet.
func (s *Server) familyPrimary(ctx context.Context, familyID string) (store_sqlite.DedicatedGraph, error) {
	primary, err := s.familyPrimaryRegistration(ctx, familyID)
	if err != nil {
		return store_sqlite.DedicatedGraph{}, err
	}
	if primary.State != reconcile.GraphStateReady {
		return store_sqlite.DedicatedGraph{}, graphview.NewViewError(graphview.CodePrimaryNotReady,
			fmt.Sprintf("primary graph %q is %s", primary.GraphID, primary.State))
	}
	return primary, nil
}

// familyPrimaryRegistration resolves primary identity without revealing its
// readiness. Grace selectors first scope this registration's repository, then
// report readiness only to sessions allowed to observe that primary.
func (s *Server) familyPrimaryRegistration(ctx context.Context, familyID string) (store_sqlite.DedicatedGraph, error) {
	graphs, err := s.materializer.Catalog.ListDedicatedGraphs(ctx, familyID)
	if err != nil {
		return store_sqlite.DedicatedGraph{}, graphview.WrapViewError(graphview.CodeCheckoutInaccessible,
			fmt.Sprintf("list the graphs of family %q", familyID), err)
	}
	for _, dedicated := range graphs {
		if dedicated.IsPrimaryBase {
			return dedicated, nil
		}
	}
	return store_sqlite.DedicatedGraph{}, graphview.NewViewError(graphview.CodeNoPrimary,
		fmt.Sprintf("family %q has no primary base graph", familyID))
}

// checkoutInSessionScope clamps an explicit selector to the repositories the
// calling session may see. Without it, naming a checkout id would reach
// across the workspace boundary every other query is held to.
func (s *Server) checkoutInSessionScope(ctx context.Context, checkout store_sqlite.Checkout) error {
	if prefix := s.repoPrefixForCheckout(ctx, checkout); prefix != "" {
		return s.repoPrefixInSessionScope(ctx, prefix, checkout.CheckoutID)
	}

	// A family can be structurally invalid (for example, temporarily missing
	// its primary) while its surviving graph row still proves which repository
	// owns the checkout. Use that evidence only for the scope ceiling; the
	// selector resolver must still return the more precise no_primary error.
	repos, bound := s.sessionWorkspaceRepoSet(ctx)
	if !bound {
		return nil
	}
	if len(repos) == 0 {
		// A broken family can make ordinary prefix resolution impossible, but
		// the catalog still proves that the session CWD is one of this family's
		// checkouts. Preserve the structural no_primary/inaccessible error only
		// for that same-family session; an unrelated unresolved CWD still fails
		// closed below.
		if cwd := SessionCWDFromContext(ctx); cwd != "" {
			_, found, err := graphview.CheckoutForPath(ctx, s.materializer.Catalog,
				[]string{checkout.FamilyID}, cwd)
			if err == nil && found {
				return nil
			}
		}
		return s.repoPrefixInSessionScope(ctx, "", checkout.CheckoutID)
	}
	graphs, err := s.materializer.Catalog.ListDedicatedGraphs(ctx, checkout.FamilyID)
	if err == nil {
		for _, dedicated := range graphs {
			if dedicated.RepoPrefix != "" && repos[dedicated.RepoPrefix] {
				return nil
			}
		}
	}
	return s.repoPrefixInSessionScope(ctx, "", checkout.CheckoutID)
}

// repoPrefixInSessionScope reports whether the session may read a repository.
// An unbound session (no cwd, no multi-repo indexer) has no ceiling, which is
// the same posture every other scope consumer takes.
func (s *Server) repoPrefixInSessionScope(ctx context.Context, repoPrefix, subject string) error {
	repos, bound := s.sessionWorkspaceRepoSet(ctx)
	if !bound {
		return nil
	}
	if repoPrefix != "" && repos[repoPrefix] {
		return nil
	}
	return graphview.NewViewError(graphview.CodeSelectorOutOfScope,
		fmt.Sprintf("%q is outside this session's workspace", subject))
}

// repoPrefixForCheckout resolves the repository a checkout is served under:
// its own dedicated graph when it has one, and otherwise the family's primary
// base graph, which is the lane an automatic checkout reads through.
func (s *Server) repoPrefixForCheckout(ctx context.Context, checkout store_sqlite.Checkout) string {
	graphs, err := s.materializer.Catalog.ListDedicatedGraphs(ctx, checkout.FamilyID)
	if err != nil {
		return ""
	}
	primary := ""
	for _, dedicated := range graphs {
		if dedicated.OwnerCheckoutID == checkout.CheckoutID && dedicated.RepoPrefix != "" {
			return dedicated.RepoPrefix
		}
		if dedicated.IsPrimaryBase && dedicated.RepoPrefix != "" {
			primary = dedicated.RepoPrefix
		}
	}
	return primary
}

// refuseRoutedViewMutation blocks a source-mutating tool whose request reads
// through a routed view.
//
// Path resolution follows the view, but nothing else on the write path does:
// the view is a leased snapshot of generations, and a write beneath it leaves
// the stack this request read describing content that is no longer there.
// Refusing is the honest answer until the write path can invalidate the route
// it wrote through; editing an automatic worktree comes with that.
func (s *Server) refuseRoutedViewMutation(ctx context.Context, tool string) *mcp.CallToolResult {
	view := requestViewFromContext(ctx)
	if !view.routed() || !s.facades.mutatesSource(tool) {
		return nil
	}
	return mcp.NewToolResultError(fmt.Sprintf(
		"%s: this request reads through %s, and %s would write the canonical checkout instead of that one. "+
			"Read through the view; edit from the checkout's own working copy.",
		graphview.CodeViewReadOnly, view.rider.ActualView, tool))
}

// attachViewRider puts the view fields on the response, inside the freshness
// block every view-relevant answer already carries. It is the same rider
// channel, extended — a second block would let a client read one and miss the
// other.
//
// The payload's wire format decides where the block can land, and every format
// has to get one: a routed answer that says nothing about its view is
// indistinguishable from a base answer. A JSON object gets the fields merged
// in. A GCX payload gets them in the header's own meta channel — gcx is the
// session default for every known agent client, so merging into JSON alone
// left the tools those clients call carrying no provenance at all. The formats
// with no structural home for a rider — TOON, the one-line text shape, a
// diagram — carry it on the response envelope, which every shape has.
func (s *Server) attachViewRider(ctx context.Context, res *mcp.CallToolResult) *mcp.CallToolResult {
	view := requestViewFromContext(ctx)
	if view == nil || view.rider == nil {
		return res
	}
	fields := viewRiderFields(view)
	res = mergeResultMeta(res, map[string]any{"freshness": fields})
	text, ok := singleTextContent(res)
	if !ok || text == "" {
		return res
	}
	if body, isGCX := injectGCXHeaderMeta(text, fields); isGCX {
		return rebuildTextResult(res, body)
	}
	var asObj map[string]any
	if json.Unmarshal([]byte(text), &asObj) != nil {
		return res
	}
	rider, _ := asObj["freshness"].(map[string]any)
	if rider == nil {
		rider = make(map[string]any, len(fields))
	}
	for name, value := range fields {
		rider[name] = value
	}
	asObj["freshness"] = rider
	body, err := json.Marshal(asObj)
	if err != nil {
		return res
	}
	return rebuildTextResult(res, string(body))
}

// viewRiderFields renders the rider as response fields. An empty value is
// omitted so a request carries exactly what it has something to say about.
func viewRiderFields(view *requestView) map[string]any {
	fields := map[string]any{
		"requested_view": view.rider.RequestedView,
		"actual_view":    view.rider.ActualView,
		"exact":          view.rider.Exact,
	}
	if view.rider.FallbackReason != "" {
		fields["fallback_reason"] = view.rider.FallbackReason
	}
	for name, value := range map[string]string{
		"graph_id":         view.rider.GraphID,
		"checkout_id":      view.rider.CheckoutID,
		"requested_state":  view.rider.RequestedState,
		"actual_state":     view.rider.ActualState,
		"view_fingerprint": view.rider.ViewFingerprint,
		"requested_ref":    view.rider.RequestedRef,
		"resolved_ref":     view.rider.ResolvedRef,
		"resolved_commit":  view.rider.ResolvedCommit,
		"resolved_tree":    view.rider.ResolvedTree,
		"build_token":      view.rider.BuildToken,
	} {
		if value != "" {
			fields[name] = value
		}
	}
	if view.rider.RetryAfter > 0 {
		fields["retry_after"] = view.rider.RetryAfter
	}
	// The capability annotations: what the view served thinly, and what a
	// base-scoped engine answered instead of the view. Both are omitted when
	// empty, so a request that hit neither carries exactly the fields it did
	// before either existed.
	if degraded, baseScoped := view.annotations(); len(degraded) > 0 || len(baseScoped) > 0 {
		if len(degraded) > 0 {
			fields["degraded_capabilities"] = degraded
		}
		if len(baseScoped) > 0 {
			fields["base_scoped"] = sortedCapabilityNames(baseScoped)
		}
	}
	return fields
}
