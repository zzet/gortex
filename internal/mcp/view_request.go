package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"path/filepath"
	"slices"
	"strings"
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
	// baseNarrowed marks a reader that is a filter over the shared corpus
	// rather than a route to a checkout: a labelled base selector reading the
	// one repository its graph owns. Such a view names no working copy, pins
	// no generation and changes no byte resolution, so every question that
	// really asks "does this request read a checkout of its own?" — the
	// source-mutation gate below is the one that matters — must answer the
	// same as it did when the base corpus was read unnarrowed. routed() alone
	// cannot tell the two apart, because it is reader-presence.
	baseNarrowed bool
	// kind is the shape of view for the routing counters, stated by the
	// producer rather than inferred from which fields happen to be set.
	// Reader-presence used to stand in for "a checkout answered", and the
	// base narrowing broke that inference: a base-scoped view has a reader
	// and still reads the shared corpus. A producer that names no kind is
	// classified by requestViewKind's fallback below.
	kind requestViewKindLabel

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

	// declared is what a view with no materialized generation stack can
	// answer: a labelled base selector, a non-strict fallback, or a grace
	// answer. Those views carry no producer rows to read a completeness off,
	// and leaving it nil is what let them skip the capability contract
	// entirely. Ignored whenever materialized is set — a leased stack states
	// its own completeness and nothing here may soften it.
	declared graphview.Completeness
}

// completeness is what the view can answer: the leased stack's own statement
// when one was materialized, otherwise whatever the reader-less view declared.
// Nil only for a request that named no view at all.
func (v *requestView) completeness() graphview.Completeness {
	switch {
	case v == nil:
		return nil
	case v.materialized != nil:
		return v.materialized.Completeness
	default:
		return v.declared
	}
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

// readsOwnCheckout reports whether this request reads through a working copy
// of its own rather than through the shared corpus. It is routed() minus the
// base narrowing: a base selector has a reader, but the bytes behind it are
// the same canonical checkouts an unrouted request resolved against.
func (v *requestView) readsOwnCheckout() bool { return v.routed() && !v.baseNarrowed }

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
	"kind": true, "graph_id": true, "checkout_id": true, "value": true, "path": true,
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
				"checkout_id": text("Registered checkout identifier, available from workspace.checkouts. For a filesystem path use view.path instead."),
			}, "kind", "checkout_id"),
			object(map[string]any{
				"kind": kind(string(graphview.SelectorWorktree)),
				"path": text("Absolute root path of an automatically discovered worktree. No explicit tracking is needed."),
			}, "kind", "path"),
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
	if _, hasPath := obj["path"]; hasPath {
		if _, hasID := obj["checkout_id"]; hasID {
			return graphview.Selector{}, graphview.NewViewError(graphview.CodeSelectorConflict,
				"worktree selector requires exactly one of checkout_id or path")
		}
		if fields["path"] == "" {
			return graphview.Selector{}, graphview.NewViewError(graphview.CodeInvalidViewSelector,
				"view.path must be a non-empty absolute worktree root")
		}
	}
	return graphview.ParseSelectorWithPath(fields["kind"], fields["graph_id"], fields["checkout_id"], fields["value"], fields["path"])
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

// requestViewKindLabel is one value of the routing counter's view-kind
// vocabulary, which viewmetrics declares as {base, worktree, ref} for
// RequestServedTotal (internal/viewmetrics/catalog.go). The catalog accepts
// whatever string it is handed, so the vocabulary is checked here instead:
// only a label a producer can name below is allowed to reach the counter, and
// anything else falls through to requestViewKind's inference rather than
// shipping a series nobody declared.
type requestViewKindLabel string

const (
	requestViewKindBase     requestViewKindLabel = viewmetrics.ViewBase
	requestViewKindWorktree requestViewKindLabel = viewmetrics.ViewWorktree
	requestViewKindRef      requestViewKindLabel = viewmetrics.ViewRef
)

// declared reports whether this label is in the counter's vocabulary.
func (k requestViewKindLabel) declared() bool {
	switch k {
	case requestViewKindBase, requestViewKindWorktree, requestViewKindRef:
		return true
	default:
		return false
	}
}

// requestViewKind names the shape of view that answered: the indexed corpus,
// a routed working copy, or a committed tree.
//
// The producer's own label decides it. Reading it off the reader was right
// only while the base corpus was the one view with no reader: a labelled base
// selector now narrows the corpus through a reader of its own, and counting
// that as a routed worktree would report base traffic as checkout traffic on
// views_request_served_total — a base answer wearing another view's label in
// the measurement surface, which is the very thing the narrowing exists to
// stop on the wire.
//
// The inference below is the fallback for a producer that states no kind, and
// for one that states a label the counter never declared. The committed-tree
// view (view_ref.go) is the only unlabelled producer left, and the file
// surface is what only it has: a view with no working copy reads its bytes out
// of the object store. The reader arm after it keeps a future routed producer
// that forgets its label counted as routed rather than as base.
func requestViewKind(view *requestView) string {
	switch {
	case view == nil:
		return viewmetrics.ViewBase
	case view.kind.declared():
		return string(view.kind)
	case view.files != nil:
		return viewmetrics.ViewRef
	case view.reader != nil:
		return viewmetrics.ViewWorktree
	default:
		return viewmetrics.ViewBase
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
	checkout, found, err := s.checkoutForRequestPath(ctx, cwd)
	if err != nil {
		return s.viewForCWDLookupError(cwd, err)
	}
	if !found || !graphview.ServesAutomaticView(checkout) {
		return nil, nil
	}
	if err := s.checkoutInSessionScope(ctx, checkout); err != nil {
		return nil, err
	}
	requested := graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: checkout.CheckoutID}
	return s.materializeRequestView(ctx, requested, checkout, false)
}

// A lookup error is not proof that the canonical corpus owns this CWD. Only
// independent tracked-root metadata may authorize the legacy catalog fallback;
// an unknown, nested, or automatic sibling checkout must fail closed instead.
func (s *Server) viewForCWDLookupError(cwd string, err error) (*requestView, error) {
	if errors.Is(err, indexer.ErrCheckoutMutationStale) || errors.Is(err, indexer.ErrCheckoutRefreshStopped) {
		return nil, graphview.WrapViewError(graphview.CodeCheckoutInaccessible,
			"automatic checkout discovery no longer has a valid checkout identity", err)
	}
	if errors.Is(err, indexer.ErrCheckoutMutationBusy) {
		return nil, graphview.WrapViewError(graphview.CodeViewBuilding,
			"automatic checkout discovery is still pending; retry this request", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, graphview.WrapViewError(graphview.CodeCheckoutInaccessible,
			"automatic checkout discovery was interrupted", err)
	}
	if checkoutLookupErrorRequiresRefusal(err) {
		return nil, err
	}
	if s.logger != nil {
		s.logger.Debug("view routing: could not bind the session cwd to a checkout", zap.Error(err))
	}
	if s.multiIndexer != nil {
		_, _, prefix, bound := s.multiIndexer.ScopeForCWD(cwd)
		if root, found := s.multiIndexer.RepoRoot(prefix); bound && found && checkoutControlRootOwnsPath(root, cwd) == nil {
			return viewFallback(false, graphview.NewViewRider(graphview.Selector{Kind: graphview.SelectorAuto}), err)
		}
	}
	return nil, graphview.WrapViewError(graphview.CodeCheckoutInaccessible,
		"checkout lookup failed before a canonical root could be established", err)
}

// Catalog reads use checkout_inaccessible too; that code may reach the
// independent canonical-ownership gate. No wrapper or joined cause may hide
// a more specific refusal such as a denied scope or invalid selector.
func checkoutLookupErrorRequiresRefusal(err error) bool {
	if code := graphview.CodeOf(err); code != "" && code != graphview.CodeCheckoutInaccessible {
		return true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, cause := range wrapped.Unwrap() {
			if checkoutLookupErrorRequiresRefusal(cause) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return checkoutLookupErrorRequiresRefusal(wrapped.Unwrap())
	}
	return false
}

// viewForWorktreeSelector serves an explicitly named checkout. Every refusal
// is reported with its own code: the caller asked for one specific view and
// must never be handed a different one.
func (s *Server) viewForWorktreeSelector(
	ctx context.Context,
	selector graphview.Selector,
	policy requestViewPolicy,
) (*requestView, error) {
	checkout, err := s.registeredWorktreeSelector(ctx, selector)
	if err != nil {
		return nil, err
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

// Resolve aliases before containment lookup, so a symlink inside the primary
// pointing to a sibling checkout selects the sibling. Keep the lexical root
// when unavailable so removal-grace requests still reach the existing policy.
func canonicalWorktreeSelectorRoot(root string) string {
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		return resolved
	}
	return filepath.Clean(root)
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
	return &requestView{
		kind:                  requestViewKindBase,
		rider:                 rider,
		declared:              baseFallbackCompleteness(),
		suppressBufferOverlay: true,
	}, nil
}

// viewForBaseSelector pins the request to a named base graph. A dedicated
// graph is read from the indexed corpus, so the selector's work is proving
// the graph exists and is ready — and naming it on the response.
//
// Naming it is not enough on its own. The indexed corpus holds every
// repository the store tracks, so a request that asked for graph X and read
// the corpus whole was answered about every repository at once while its
// rider said exact:true, graph_id:X. The reader below narrows that corpus to
// the graph's own repository, which is what makes the label a served view
// rather than a decoration.
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
	// The kind is stated here and not derived: whichever arm below answers,
	// this request read the indexed corpus, and the counters must say so even
	// when the narrowed arm hands it a reader.
	view := &requestView{kind: requestViewKindBase, rider: rider}
	if scoped := newBaseGraphReader(s.graph, dedicated.RepoPrefix); scoped != nil {
		view.reader = scoped
		view.baseNarrowed = true
		view.declared = baseGraphCompleteness()
		return view, nil
	}
	// Nothing to narrow with: this store spells the graph's nodes with no
	// repository prefix, so the corpus either is the graph or is not separable
	// from it here. Serve it — that is what the label meant before — but say
	// on the response that a base-scoped corpus answered, rather than let the
	// exact rider imply a reader the request never got.
	view.declared = baseCorpusCompleteness()
	view.noteBaseScoped(baseGraphUnnarrowedCapabilities())
	return view, nil
}

// baseGraphReader narrows the indexed corpus to the one repository a named
// base graph owns.
//
// It is a filter, not a composition: there is no generation stack under a base
// selector, only the corpus every tracked repository shares. Narrowing it is
// therefore a predicate on what the corpus already holds — a node belongs to
// this view when the repository it was indexed under is this graph's.
//
// It deliberately forwards none of the optional store capabilities (the
// bounded adjacency projections, the name and file readers, the degree
// aggregators). Forwarding one without re-applying the predicate would hand a
// caller rows from every repository through a reader that promised one, so the
// type assertions fail and each call site takes its plain-Reader path instead.
// That costs work on those paths; it cannot cost correctness. The one
// capability that is forwarded is content search, which takes the repository
// to search as an argument and so can be narrowed exactly — see
// baseGraphContentReader.
type baseGraphReader struct {
	base       graph.Reader
	repoPrefix string
	// sites memoizes siteInScope for a path this view's prefix does not
	// spell. Deciding one asks the corpus what it holds at that path, and an
	// edge scan asks about the same few thousand paths over and over. The
	// reader is built per request, so the cache dies with the request; a
	// sync.Map because a handler may fan its reads out across goroutines.
	sites sync.Map
}

// baseGraphNameOverfetch is how much wider than the caller's limit a
// name-substring scan asks the corpus for on its first try, so the filter
// below has something to keep. When that is not enough the ask widens (see
// FindNodesByNameContaining) rather than answering short.
const baseGraphNameOverfetch = 8

// baseGraphNameOverfetchFloor keeps a one-row request from asking for one row
// and filtering it away.
const baseGraphNameOverfetchFloor = 64

// baseGraphNameOverfetchCeiling bounds the widening loop. Reaching it means
// the corpus holds more than this many matches ranked ahead of the first
// in-scope one, which is a scan of the whole name index in all but name; the
// answer is then short rather than unbounded work.
const baseGraphNameOverfetchCeiling = 1 << 16

// newBaseGraphReader narrows reader to repoPrefix. It returns nil when there
// is nothing to narrow — no reader at all, or a corpus that spells this
// graph's nodes with no prefix — so the caller can tell a served narrowing
// from an unnarrowed corpus instead of shipping a reader that filters nothing.
func newBaseGraphReader(reader graph.Reader, repoPrefix string) graph.Reader {
	if reader == nil || repoPrefix == "" {
		return nil
	}
	scoped := &baseGraphReader{base: reader, repoPrefix: repoPrefix}
	if searcher, ok := reader.(graph.ContentSearcher); ok {
		return &baseGraphContentReader{baseGraphReader: scoped, content: searcher}
	}
	return scoped
}

// inScope is the view's membership predicate.
//
// The repository a node was indexed under decides it. The file fallback covers
// the one legacy shape that predates prefixed identities: a node the corpus
// stamped with no repository at all, which is this view's exactly when its
// path is. A node stamped with another repository is never in scope, whatever
// its path says.
func (r *baseGraphReader) inScope(node *graph.Node) bool {
	switch {
	case node == nil:
		return false
	case node.RepoPrefix == r.repoPrefix:
		return true
	case node.RepoPrefix == "":
		return r.pathInScope(node.FilePath)
	default:
		return false
	}
}

// pathInScope reports whether a repo-prefixed path names a file of this
// repository.
func (r *baseGraphReader) pathInScope(path string) bool {
	return path == r.repoPrefix || strings.HasPrefix(path, r.repoPrefix+"/")
}

// edgeInScope decides a whole-corpus edge scan.
//
// Both halves have to hold. Both endpoints are checked with endpointInScope:
// an edge whose far end names a symbol of another repository would put that
// repository's node id in the answer, which is the leak this reader exists to
// close. Then the edge's file path is checked — the reference site, which is
// the repository that owns the edge — because a site outside this repository
// is not this view's edge at all even when both of its ends are.
//
// The endpoints are asked first on purpose, and this is an ordering with a
// cost attached rather than a style choice. An endpoint decision is a map hit
// in the batch endpointScope already hydrated; a site decision that the
// prefix cannot spell has to ask the corpus what it holds at that path, which
// is a store round-trip. Asking the site half first made an edge the endpoint
// filter was going to drop anyway pay for that round-trip: on a corpus with a
// sibling repository of N files, a whole-corpus scan issued N lookups and
// changed its answer by nothing. && is commutative, so the answer is the same
// either way and only the work moves.
//
// scope memoizes the endpoint decisions for one scan. A whole-corpus scan asks
// about the same few thousand nodes over and over, and a nil map is a valid
// unmemoized call for the one-off paths.
func (r *baseGraphReader) edgeInScope(edge *graph.Edge, scope map[string]bool) bool {
	switch {
	case edge == nil:
		return false
	case !r.endpointsInScope(edge, scope):
		return false
	default:
		return r.siteInScope(edge.FilePath)
	}
}

// endpointsInScope is the cheap half of edgeInScope: both ends, decided out of
// the batch hydration when there is one.
func (r *baseGraphReader) endpointsInScope(edge *graph.Edge, scope map[string]bool) bool {
	if edge == nil {
		return false
	}
	return r.endpointInScope(edge.From, scope) && r.endpointInScope(edge.To, scope)
}

// siteInScope decides an edge's reference site: the file the reference was
// written in, which is the repository that owns the edge.
//
// A path this repository's prefix spells is this view's, and a synthesized
// edge carries no site at all, so nothing places it anywhere. What is left is
// a path spelled some other way — and "not spelled with my prefix" is not the
// same fact as "another repository's". The node lane already knows that:
// inScope keeps a node the corpus stamped with no repository when its path is
// this view's. Its symmetric question for a site is what the corpus itself
// holds at that path, because a corpus that predates prefixed identities
// records the site relative to the repository root while still stamping the
// nodes there with the repository. The nodes at the path are then what says
// whose file it is, and they are decided by the very same predicate — so the
// node lane and the edge lane can never disagree about one file.
//
// A path the corpus holds nothing at stays out. That is not the same shape as
// "a file of mine spelled oddly": a node at a path is exactly what attributes
// that path, so a site the corpus holds nothing at is never a path this view
// holds a node at — neither endpoint of the edge lives there. Dropping it
// under-reports a reference the corpus recorded against a file it indexed no
// symbol in; keeping it would put a path this view cannot attribute to itself
// on the wire, which is the leak the site half exists to close.
//
// Deciding a path costs a store round-trip, so callers with a batch of edges
// in hand run prefetchSites first and this function then answers from the
// memo. The per-path door below is the fallback for a backend with no batch
// primitive and for the one-off paths.
func (r *baseGraphReader) siteInScope(path string) bool {
	if path == "" || r.pathInScope(path) {
		return true
	}
	if cached, seen := r.sites.Load(path); seen {
		decided, _ := cached.(bool)
		return decided
	}
	decided := r.decideSite(r.base.GetFileNodes(path))
	r.sites.Store(path, decided)
	return decided
}

// decideSite turns what the corpus holds at a path into the site decision.
// The nodes there are judged by the very same predicate the node lane uses, so
// the node lane and the edge lane can never disagree about one file.
func (r *baseGraphReader) decideSite(nodes []*graph.Node) bool {
	if len(nodes) == 0 {
		return false
	}
	for _, node := range nodes {
		if !r.inScope(node) {
			return false
		}
	}
	return true
}

// fileNodesBatchReader is the batched door to the corpus's file index.
// graph.Store declares GetFileNodesByPaths as the batched sibling of
// GetFileNodes precisely so a frontier of paths costs bounded IN-queries
// instead of one round-trip per path (internal/graph/store.go), and both
// production backends implement it. A reader that does not is answered
// path by path.
type fileNodesBatchReader interface {
	GetFileNodesByPaths(filePaths []string) map[string][]*graph.Node
}

// prefetchSites decides every still-undecided out-of-prefix site of edges in
// one round-trip, so the site half of the filters below costs one batched
// lookup per lane call rather than one point lookup per distinct foreign path.
//
// This is the same shape the endpoint half already has: keepEdges hydrates
// every far id through one GetNodesByIDs, and AllEdges pre-decides both ends
// of every edge through endpointScope. Leaving the site half un-batched made
// a single find_usages or whole-corpus scan issue one serial store query per
// distinct file path of every other tracked repository.
//
// Paths already in the memo are skipped, so a second lane over the same edges
// costs nothing, and a backend with no batch primitive is left to the per-path
// door in siteInScope.
func (r *baseGraphReader) prefetchSites(edges []*graph.Edge) {
	batch, ok := r.base.(fileNodesBatchReader)
	if !ok {
		return
	}
	var paths []string
	seen := make(map[string]struct{}, len(edges))
	for _, edge := range edges {
		if edge == nil || edge.FilePath == "" || r.pathInScope(edge.FilePath) {
			continue
		}
		if _, duplicate := seen[edge.FilePath]; duplicate {
			continue
		}
		seen[edge.FilePath] = struct{}{}
		if _, decided := r.sites.Load(edge.FilePath); decided {
			continue
		}
		paths = append(paths, edge.FilePath)
	}
	if len(paths) == 0 {
		return
	}
	byPath := batch.GetFileNodesByPaths(paths)
	for _, path := range paths {
		r.sites.Store(path, r.decideSite(byPath[path]))
	}
}

// endpointInScope decides one end of an edge.
//
// An id the corpus does not hydrate is kept: an unresolved reference names no
// symbol at all, in this repository or any other, so dropping it would delete
// a fact about this repository without hiding anything foreign. An id that
// hydrates to another repository's node is the one case that must go — that
// node id is exactly what a caller under a base selector must never receive.
func (r *baseGraphReader) endpointInScope(id string, scope map[string]bool) bool {
	if id == "" {
		return true
	}
	if scope != nil {
		if decided, seen := scope[id]; seen {
			return decided
		}
	}
	node := r.base.GetNode(id)
	decided := node == nil || r.inScope(node)
	if scope != nil {
		scope[id] = decided
	}
	return decided
}

// keepEdges drops the edges of an in-scope anchor whose site or far end
// belongs to another repository, hydrating every far id in one round-trip.
//
// The anchor itself is already known to be in scope, so what is decided here
// is the other end and the site. Both, because they are separate facts: an
// incoming edge written in another repository's file whose `from` the corpus
// does not hydrate has no foreign node id to catch, and dropping only on the
// far end would put that repository's file path in the answer through
// find_usages. This is the same pair edgeInScope applies to a whole-corpus
// scan; the two lanes must not disagree about one edge. See endpointInScope
// for why an id that hydrates to nothing is kept.
//
// The far end is decided first and the site second, for the reason
// edgeInScope spells out: the far end is already hydrated in the batch above,
// while a site the prefix cannot spell costs a store lookup, so an edge the
// far end drops must not pay for one. The sites the survivors need are then
// resolved in a single batched round-trip beside the hydration.
func (r *baseGraphReader) keepEdges(edges []*graph.Edge, far func(*graph.Edge) string) []*graph.Edge {
	if len(edges) == 0 {
		return edges
	}
	ids := make([]string, 0, len(edges))
	for _, edge := range edges {
		if edge != nil && far(edge) != "" {
			ids = append(ids, far(edge))
		}
	}
	hydrated := r.base.GetNodesByIDs(ids)
	local := make([]*graph.Edge, 0, len(edges))
	for _, edge := range edges {
		if edge == nil {
			continue
		}
		id := far(edge)
		node, found := hydrated[id]
		if id == "" || !found || node == nil || r.inScope(node) {
			local = append(local, edge)
		}
	}
	r.prefetchSites(local)
	out := make([]*graph.Edge, 0, len(local))
	for _, edge := range local {
		if r.siteInScope(edge.FilePath) {
			out = append(out, edge)
		}
	}
	return out
}

func edgeFrom(edge *graph.Edge) string { return edge.From }

func edgeTo(edge *graph.Edge) string { return edge.To }

// keep filters a node slice down to this view.
func (r *baseGraphReader) keep(nodes []*graph.Node) []*graph.Node {
	out := make([]*graph.Node, 0, len(nodes))
	for _, node := range nodes {
		if r.inScope(node) {
			out = append(out, node)
		}
	}
	return out
}

// anchored reports whether an adjacency walk may start at id — that is,
// whether the node the walk is about belongs to this view.
//
// Starting there is necessary but not sufficient. The walk's own edges are
// filtered again by keepEdges, because a caller does not only read the nodes
// this reader hydrates: query.Engine.FindUsagesScoped appends an edge to the
// answer whether or not its far end hydrated (internal/query/engine.go, the
// `from == nil` skip is gated on opts.hasScopeFilter()), so an unfiltered edge
// list puts a foreign symbol id on the wire while the node list omits it.
// Refusing the far *node* is not containment on its own; refusing the far
// *edge* is. What that costs is declared: CapResolutionCrossRepo is incomplete
// for this view precisely because those references were removed.
func (r *baseGraphReader) anchored(id string) bool {
	return r.inScope(r.base.GetNode(id))
}

func (r *baseGraphReader) GetNode(id string) *graph.Node {
	node := r.base.GetNode(id)
	if !r.inScope(node) {
		return nil
	}
	return node
}

func (r *baseGraphReader) GetNodeByQualName(qualName string) *graph.Node {
	node := r.base.GetNodeByQualName(qualName)
	if !r.inScope(node) {
		return nil
	}
	return node
}

func (r *baseGraphReader) FindNodesByName(name string) []*graph.Node {
	return r.keep(r.base.FindNodesByName(name))
}

// FindNodesByNameContaining fills the caller's limit from this view.
//
// The corpus ranks every repository's matches together, so a fixed over-fetch
// could be spent entirely on rows this view drops and answer short — an answer
// that is never wrong but silently missing rows the view does hold, with
// nothing on the response to say which of the two limits truncated it. The
// widening loop removes that case instead of annotating it: it asks for more
// until the filter has the caller's limit, or until the corpus returns fewer
// rows than it was asked for, which is the corpus saying it has no more.
func (r *baseGraphReader) FindNodesByNameContaining(substr string, limit int) []*graph.Node {
	if limit <= 0 {
		return r.keep(r.base.FindNodesByNameContaining(substr, 0))
	}
	ask := limit * baseGraphNameOverfetch
	if ask < baseGraphNameOverfetchFloor {
		ask = baseGraphNameOverfetchFloor
	}
	for {
		raw := r.base.FindNodesByNameContaining(substr, ask)
		out := r.keep(raw)
		switch {
		case len(out) >= limit:
			return out[:limit]
		case len(raw) < ask:
			// The corpus is exhausted: this is every match there is.
			return out
		case ask >= baseGraphNameOverfetchCeiling:
			return out
		}
		ask *= 4
	}
}

func (r *baseGraphReader) GetNodesByIDs(ids []string) map[string]*graph.Node {
	found := r.base.GetNodesByIDs(ids)
	out := make(map[string]*graph.Node, len(found))
	for id, node := range found {
		if r.inScope(node) {
			out[id] = node
		}
	}
	return out
}

func (r *baseGraphReader) GetFileNodes(filePath string) []*graph.Node {
	return r.keep(r.base.GetFileNodes(filePath))
}

// GetRepoNodes answers only for this view's repository. A wildcard ("" means
// every repository on the base graph) is answered as this repository, which is
// every repository this view has.
func (r *baseGraphReader) GetRepoNodes(repoPrefix string) []*graph.Node {
	if repoPrefix != "" && repoPrefix != r.repoPrefix {
		return nil
	}
	return r.keep(r.base.GetRepoNodes(r.repoPrefix))
}

func (r *baseGraphReader) GetOutEdges(nodeID string) []*graph.Edge {
	if !r.anchored(nodeID) {
		return nil
	}
	return r.keepEdges(r.base.GetOutEdges(nodeID), edgeTo)
}

func (r *baseGraphReader) GetInEdges(nodeID string) []*graph.Edge {
	if !r.anchored(nodeID) {
		return nil
	}
	return r.keepEdges(r.base.GetInEdges(nodeID), edgeFrom)
}

func (r *baseGraphReader) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return r.adjacencyByNodeIDs(ids, r.base.GetInEdgesByNodeIDs, edgeFrom)
}

func (r *baseGraphReader) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return r.adjacencyByNodeIDs(ids, r.base.GetOutEdgesByNodeIDs, edgeTo)
}

// adjacencyByNodeIDs runs the batched walk over the in-scope anchors only, in
// one hydration round-trip rather than one per id, and filters every returned
// list the same way the single-anchor lanes do.
func (r *baseGraphReader) adjacencyByNodeIDs(
	ids []string,
	walk func([]string) map[string][]*graph.Edge,
	far func(*graph.Edge) string,
) map[string][]*graph.Edge {
	anchors := r.base.GetNodesByIDs(ids)
	scoped := make([]string, 0, len(ids))
	for _, id := range ids {
		if r.inScope(anchors[id]) {
			scoped = append(scoped, id)
		}
	}
	if len(scoped) == 0 {
		return map[string][]*graph.Edge{}
	}
	walked := walk(scoped)
	// One batched site lookup for the whole walk, not one per anchor: the
	// per-list filter below then finds every decision already memoized. The
	// union is what keeps a fan-out over anchors from reappearing as a fan-out
	// over batches.
	union := make([]*graph.Edge, 0, len(walked))
	for _, edges := range walked {
		union = append(union, edges...)
	}
	r.prefetchSites(union)
	out := make(map[string][]*graph.Edge, len(walked))
	for id, edges := range walked {
		out[id] = r.keepEdges(edges, far)
	}
	return out
}

func (r *baseGraphReader) AllNodes() []*graph.Node {
	return r.keep(r.base.AllNodes())
}

func (r *baseGraphReader) AllEdges() []*graph.Edge {
	edges := r.base.AllEdges()
	// Hydrate every endpoint the scan will ask about in one round-trip. Asking
	// per id would turn a whole-corpus scan into one point lookup per endpoint
	// against a disk-backed store, which is the cost GetNodesByIDs exists to
	// collapse.
	scope := r.endpointScope(edges)
	// Then the same collapse for the other half. Only the edges both
	// endpoints kept can still be decided by their site, so only their paths
	// are worth a lookup — and they are worth exactly one, batched, rather
	// than one apiece.
	local := make([]*graph.Edge, 0, len(edges))
	for _, edge := range edges {
		if r.endpointsInScope(edge, scope) {
			local = append(local, edge)
		}
	}
	r.prefetchSites(local)
	out := make([]*graph.Edge, 0, len(local))
	for _, edge := range local {
		if r.edgeInScope(edge, scope) {
			out = append(out, edge)
		}
	}
	return out
}

// endpointScope pre-decides both ends of every edge in one batched hydration.
//
// An id the batch returns no row for is decided here too, and decided the way
// the slow path decides it: an unresolved reference names no symbol at all, so
// it is kept. Leaving it absent from the map instead sent endpointInScope back
// to a point lookup per unresolved id — and an unresolved caller is exactly
// the shape a cross-repository reference has before resolution, so the absent
// arm was the fan-out, not the rare case.
func (r *baseGraphReader) endpointScope(edges []*graph.Edge) map[string]bool {
	ids := make([]string, 0, 2*len(edges))
	seen := make(map[string]struct{}, 2*len(edges))
	for _, edge := range edges {
		if edge == nil {
			continue
		}
		for _, id := range [...]string{edge.From, edge.To} {
			if id == "" {
				continue
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	hydrated := r.base.GetNodesByIDs(ids)
	scope := make(map[string]bool, len(ids))
	for _, id := range ids {
		node, found := hydrated[id]
		scope[id] = !found || node == nil || r.inScope(node)
	}
	return scope
}

// baseGraphEdgeWindow is how many edges the streaming scan buffers before it
// decides them. The buffer is what lets the endpoint hydration and the site
// lookups be batched at all on a lane that has no slice to start from; it
// bounds the read-ahead a consumer that stops early pays for.
const baseGraphEdgeWindow = 512

func (r *baseGraphReader) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		scope := make(map[string]bool)
		window := make([]*graph.Edge, 0, baseGraphEdgeWindow)
		// flush decides one window in two batched round-trips and yields what
		// survives, in the order the corpus produced it.
		flush := func() bool {
			if len(window) == 0 {
				return true
			}
			maps.Copy(scope, r.endpointScope(window))
			local := make([]*graph.Edge, 0, len(window))
			for _, edge := range window {
				if r.endpointsInScope(edge, scope) {
					local = append(local, edge)
				}
			}
			r.prefetchSites(local)
			window = window[:0]
			for _, edge := range local {
				if r.siteInScope(edge.FilePath) && !yield(edge) {
					return false
				}
			}
			return true
		}
		for edge := range r.base.EdgesByKind(kind) {
			if edge == nil {
				continue
			}
			window = append(window, edge)
			if len(window) >= baseGraphEdgeWindow && !flush() {
				return
			}
		}
		flush()
	}
}

func (r *baseGraphReader) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		for node := range r.base.NodesByKind(kind) {
			if r.inScope(node) && !yield(node) {
				return
			}
		}
	}
}

func (r *baseGraphReader) NodeCount() int { return r.Stats().TotalNodes }

func (r *baseGraphReader) EdgeCount() int { return r.Stats().TotalEdges }

func (r *baseGraphReader) EdgeIdentityRevisions() int { return r.base.EdgeIdentityRevisions() }

// Stats reports this repository's counters.
//
// The per-repo rollup answers it whenever the backend keeps one. When it does
// not — graph.Graph.RepoStats skips the empty prefix, so a corpus whose nodes
// carry no repository at all reports nothing — the counters are derived from
// this reader's own filtered surface instead of borrowed from the corpus.
// Reporting the corpus here would put every repository's totals behind a rider
// that says exact:true, graph_id:X: a whole-corpus answer wearing this view's
// label, which is the exact defect this reader exists to close. Deriving costs
// one filtered pass and is reached only in that degenerate shape.
// A rollup that carries no row for this prefix answers zero — the corpus
// attributes nothing to this repository — and it answers it with allocated
// counter maps, because a rollup miss is the one path that used to return the
// zero GraphStats whole: nil ByKind and nil ByLanguage, which every other
// producer of this type fills in and which a caller that counts into the map
// it was handed panics on.
func (r *baseGraphReader) Stats() graph.GraphStats {
	if perRepo := r.base.RepoStats(); len(perRepo) > 0 {
		return withCounterMaps(perRepo[r.repoPrefix])
	}
	return r.derivedStats()
}

// withCounterMaps fills in the counter maps a GraphStats value may be missing.
// It never replaces one the producer built, so a real rollup row is passed
// through as it was written.
func withCounterMaps(stats graph.GraphStats) graph.GraphStats {
	if stats.ByKind == nil {
		stats.ByKind = map[string]int{}
	}
	if stats.ByLanguage == nil {
		stats.ByLanguage = map[string]int{}
	}
	return stats
}

// derivedStats counts what this view actually holds. It is the fallback for a
// backend with no per-repo rollup, and it can only ever report a subset of the
// corpus, never the corpus.
func (r *baseGraphReader) derivedStats() graph.GraphStats {
	nodes := r.AllNodes()
	stats := graph.GraphStats{
		TotalNodes: len(nodes),
		ByKind:     make(map[string]int, len(nodes)),
		ByLanguage: make(map[string]int),
	}
	for _, node := range nodes {
		stats.ByKind[string(node.Kind)]++
		if node.Language != "" {
			stats.ByLanguage[node.Language]++
		}
	}
	stats.TotalEdges = len(r.AllEdges())
	return stats
}

func (r *baseGraphReader) RepoStats() map[string]graph.GraphStats {
	perRepo := r.base.RepoStats()
	if len(perRepo) == 0 {
		return nil
	}
	stats, found := perRepo[r.repoPrefix]
	if !found {
		return map[string]graph.GraphStats{}
	}
	return map[string]graph.GraphStats{r.repoPrefix: withCounterMaps(stats)}
}

// baseGraphContentReader adds the one optional capability a narrowed corpus
// can keep exactly: content search already takes the repository to search, so
// scoping it is pinning that argument rather than filtering an answer. The
// write half of graph.ContentSearcher is refused — a request view is a reader,
// and nothing may append to a corpus through one.
type baseGraphContentReader struct {
	*baseGraphReader
	content graph.ContentSearcher
}

// errBaseGraphReadOnly is what the content writers answer. A view is a read of
// the corpus; the corpus is written by whoever indexed it.
var errBaseGraphReadOnly = errors.New("a base-scoped view reads the indexed corpus and cannot write to it")

func (r *baseGraphContentReader) SearchContent(text, repoPrefix string, limit int) ([]graph.ContentHit, error) {
	if repoPrefix != "" && repoPrefix != r.repoPrefix {
		return nil, nil
	}
	return r.content.SearchContent(text, r.repoPrefix, limit)
}

func (r *baseGraphContentReader) ScanContent(repoPrefix string, fn func(nodeID, filePath, body string) bool) error {
	if repoPrefix != "" && repoPrefix != r.repoPrefix {
		return nil
	}
	return r.content.ScanContent(r.repoPrefix, fn)
}

func (r *baseGraphContentReader) WipeContent(string) error { return errBaseGraphReadOnly }

func (r *baseGraphContentReader) WipeContentFile(string) error { return errBaseGraphReadOnly }

func (r *baseGraphContentReader) AppendContent(string, []graph.ContentFTSItem) error {
	return errBaseGraphReadOnly
}

func (r *baseGraphContentReader) BuildContentIndex() error { return errBaseGraphReadOnly }

var (
	_ graph.Reader          = (*baseGraphReader)(nil)
	_ graph.Reader          = (*baseGraphContentReader)(nil)
	_ graph.ContentSearcher = (*baseGraphContentReader)(nil)
)

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
	rider.CheckoutID = checkout.CheckoutID
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
		kind:         requestViewKindWorktree,
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
	return &requestView{
		kind:     requestViewKindBase,
		rider:    rider,
		declared: baseFallbackCompleteness(),
	}, nil
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
	prefix, _ := s.repoPrefixForCheckoutChecked(ctx, checkout)
	return prefix
}

func (s *Server) repoPrefixForCheckoutChecked(ctx context.Context, checkout store_sqlite.Checkout) (string, error) {
	graphs, err := s.materializer.Catalog.ListDedicatedGraphs(ctx, checkout.FamilyID)
	if err != nil {
		return "", err
	}
	primary := ""
	for _, dedicated := range graphs {
		if dedicated.OwnerCheckoutID == checkout.CheckoutID && dedicated.RepoPrefix != "" {
			return dedicated.RepoPrefix, nil
		}
		if dedicated.IsPrimaryBase && dedicated.RepoPrefix != "" {
			primary = dedicated.RepoPrefix
		}
	}
	return primary, nil
}

// refuseRoutedViewMutation admits only mutations that obtained checkout-local
// coordination. An inexact fallback is read-only even when it has no routed
// reader: allowing that case would silently write the primary checkout.
//
// A base-narrowed view is admitted for the same reason an unnarrowed base
// request always was. The gate exists because a routed reader reads bytes at
// some *other* checkout than the one every path resolver anchors to, so a
// write through it would land in the wrong working copy unless the checkout
// coordinator approved it. A base selector re-reads the corpus the request
// would have read anyway, resolves paths against the repository's canonical
// root exactly as before (view_paths.go: viewRoot is empty, so
// requestViewPathRoot is the zero value), and therefore writes where it
// always wrote. Narrowing what a base selector *reads* must not silently
// convert every base-labelled edit into a refusal.
func (s *Server) refuseRoutedViewMutation(ctx context.Context, tool string) *mcp.CallToolResult {
	view := requestViewFromContext(ctx)
	if !s.facades.mutatesSource(tool) || view == nil {
		return nil
	}
	if view.rider != nil && !view.rider.Exact {
		return mcp.NewToolResultError(fmt.Sprintf(
			"%s: source edits require an exact live checkout; this request received a read-only fallback. Retry when the selected checkout is ready.",
			graphview.CodeViewReadOnly))
	}
	if !view.readsOwnCheckout() || checkoutMutationFromContext(ctx) != nil {
		return nil
	}
	return mcp.NewToolResultError(fmt.Sprintf(
		"%s: %s has no approved write path for this view. "+
			"Only file edits, file writes, and symbol edits are supported on an exact live worktree with an active checkout coordinator. "+
			"Batch operations, refactors, and immutable ref views remain read-only through routed views.",
		graphview.CodeViewReadOnly, tool))
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
