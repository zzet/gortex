package mcp

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// SetOverlayManager wires the editor-overlay manager into the MCP
// server. After this call:
//
//   - Every `tools/call` whose session has overlay buffers attached is
//     wrapped with a per-request middleware that constructs a shadow-
//     graph view (`*graph.OverlaidView`) layering the parsed overlay
//     on top of the immutable base graph. The view is attached to the
//     request context; tool handlers read it via `s.readerFor(ctx)`
//     instead of touching `s.graph` directly. The base graph is never
//     mutated, so concurrent sessions — overlay-active or not — see
//     their own consistent view and the file watcher never races on
//     overlay state.
//
//   - The overlay management MCP tools (`overlay_register`,
//     `overlay_push`, `overlay_list`, `overlay_delete`, `overlay_drop`)
//     become live so MCP-native editor extensions can manage overlays
//     without reaching for the parallel `/v1/overlay/*` HTTP surface.
//
// Passing nil leaves the server in pre-overlay behaviour (reads always
// come from the base graph; overlay tools are not registered). Calling
// twice re-registers the overlay tools idempotently.
func (s *Server) SetOverlayManager(mgr *daemon.OverlayManager) {
	s.overlays = mgr
	if mgr == nil {
		return
	}
	s.registerOverlayToolsOnce.Do(func() {
		s.registerOverlayTools()
	})
}

// OverlayManager returns the wired editor-overlay manager, or nil
// when overlay support is disabled for this server instance.
func (s *Server) OverlayManager() *daemon.OverlayManager { return s.overlays }

// wrapToolHandler returns a tool handler decorated with the
// overlay-view middleware. Tool registration helpers (`s.addTool`)
// route every handler through this so the daemon-dispatched path
// (HandleMessage) and the HTTP `CallToolStrict` path get identical
// shadow-graph semantics — the latter bypasses mcp-go's hook surface,
// so handler-level wrapping is the only place that covers both
// transports.
//
// The middleware is non-mutating: it parses the calling session's
// overlay buffers once per request (cached by (sessID, contentHash) in
// s.overlayLayerCache) and attaches the resulting view to ctx via
// WithOverlayView. Tool handlers obtain the active reader via
// s.readerFor(ctx), which returns the view when present and the base
// graph otherwise. Concurrent sessions are isolated by construction
// because no shared state is touched.
//
// When the calling session has no overlay or no overlay manager is
// wired, this is a transparent pass-through (one map lookup, zero
// parsing) — non-overlay traffic pays no cost.
func (s *Server) wrapToolHandler(h mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return s.wrapToolHandlerMode(h, true)
}

// wrapControlToolHandler applies the normal safety, telemetry, logging, and
// response middleware without pre-building the caller's overlay view. Overlay
// lifecycle/simulation handlers compose their own views and must run through
// this path rather than bypassing gates with a bare MCP AddTool.
func (s *Server) wrapControlToolHandler(h mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return s.wrapToolHandlerMode(h, false)
}

func (s *Server) wrapToolHandlerMode(h mcpserver.ToolHandlerFunc, injectOverlay bool) mcpserver.ToolHandlerFunc {
	// Prompt-injection screening sits closest to the handler so it
	// sees the real arguments and the real result (see sanitize.go).
	h = s.sanitizeToolHandler(h)
	// The hang firewall is the OUTERMOST layer (applied last, at the bottom
	// of this function) so it bounds the whole middleware chain, not just the
	// leaf handler — the overlay build, the freshness sweep, and the response
	// capture below all touch the graph and can block on the same locks.
	return s.boundToolHandler(func(ctx context.Context, req mcp.CallToolRequest) (res *mcp.CallToolResult, retErr error) {
		beginMCPToolCall()
		defer func() {
			endMCPToolCall(s.logger, req.Params.Name)
			s.releaseTransientAnalysisIfIdle()
		}()

		// Last-resort panic firewall around EVERY tool handler. A Go
		// panic in any handler (e.g. when the store surfaces a fatal
		// engine error) would otherwise unwind past the mcp-go server
		// loop and crash the whole daemon — dropping every session's
		// MCP transport, not just the offending call. Convert it to a
		// structured tool error so the panicking tool fails in
		// isolation and the daemon survives. This supersedes the
		// per-handler recover that get_file_summary carried; every
		// tool now gets the same protection.
		defer func() {
			if r := recover(); r != nil {
				if s.logger != nil {
					s.logger.Error("tool handler panic recovered",
						zap.String("tool", req.Params.Name),
						zap.Any("panic", r),
						zap.Stack("stack"))
				}
				res = mcp.NewToolResultError(fmt.Sprintf("tool %q internal error: %v", req.Params.Name, r))
				retErr = nil
			}
		}()
		// The view argument is request context, not a tool parameter: it is
		// read and stripped here so every tool honours it and no handler or
		// schema has to know about it. Stripping precedes reconciliation so
		// the alias matcher cannot rewrite it into a tool's own parameter.
		// The three request-level freshness knobs ride the same seam, for the
		// same reason, and are parsed HERE rather than beside the view policy
		// below for two reasons that both have to hold:
		//   - before reconcileToolParams: no tool declares these names, and
		//     the alias matcher rewrites exactly the keys a tool does not
		//     declare, so reading them later risks reading a request a
		//     spelling heuristic already edited. No SHIPPED tool has a
		//     parameter inside the matcher's edit-distance budget of a knob
		//     today (TestNoToolAliasesTheFreshnessKnobs), so this ordering
		//     buys nothing against the current surface and everything against
		//     the next tool that grows such a parameter;
		//     TestFreshnessKnobsAreReadBeforeParameterReconciliation registers
		//     exactly that collision and fails if the parse moves below.
		//   - before every branch below: a malformed wait_deadline must refuse
		//     for EVERY call shape, including the catalog-only checkout
		//     controls that resolve no view at all and the detect_changes
		//     carve-out that converts a view error into a degraded answer. A
		//     knob the server cannot parse is a request the server does not
		//     understand; answering it anyway is how a caller ends up reading
		//     a stale route it explicitly bounded.
		freshness := takeRequestFreshness(&req)
		if freshness.err != nil {
			return mcp.NewToolResultError(freshness.err.Error()), nil
		}
		requireExactView := freshness.requireExact
		selector, selectorErr := takeViewSelector(&req)
		if selectorErr != nil {
			return mcp.NewToolResultError(selectorErr.Error()), nil
		}
		// The capability contract travels on the same seam and for the same
		// reason: what a caller needs the view to be able to answer is a
		// property of the request, not a parameter of any one tool.
		capabilities, capabilitiesErr := takeCapabilityRequest(&req)
		if capabilitiesErr != nil {
			return mcp.NewToolResultError(capabilitiesErr.Error()), nil
		}
		// Tolerate hallucinated / mistyped parameter names before the
		// handler reads arguments (e.g. "symbol" accepted as "id").
		s.reconcileToolParams(&req)
		// Enforce the session's runtime mode / workflow phase — a hard
		// gate even if the client never re-read tools/list.
		if blocked := s.checkToolGate(ctx, req.Params.Name); blocked != nil {
			return blocked, nil
		}
		// Opt-in zero-config: background-index an untracked cwd on the first
		// tool call (GORTEX_AUTOINDEX=1). Cheap getenv + sync.Once on the
		// request path; all real work runs on a background goroutine.
		s.maybeAutoIndexCWD()
		// Every bounded file-summary lookup descended from this tools/call
		// shares one request-local allowance. Overlay preparation and facade
		// forwarding derive child contexts, so the pointer survives both paths;
		// idempotence prevents nested preparation from resetting the budget.
		ctx = withLocalizationFileRequestBudget(ctx)
		// Arm the arg guard's deferred-rider slot: the guard runs inside the
		// handler chain, but its warn rider must attach AFTER the decorators
		// below (see attachPendingArgGuardRider).
		ctx = withArgGuardRiderSlot(ctx)
		// Repository lifetime for this request, taken BEFORE anything resolves
		// a view so it also covers selection and materialization.
		//
		// The generation lease a materialized view holds is a different
		// guarantee: it stops a payload generation from being RETIRED. It says
		// nothing about the owner of the repository underneath, whose
		// finalization is followed by a physical purge of that repository's
		// payload — which is why an unrouted request (the base corpus answers,
		// and no view is materialized at all) held no repository lifetime
		// whatsoever, and that is the shape most tool calls have.
		//
		// Released on handler return, and joined by every worker the handler
		// leaves running behind it (handoffRequestView below), so the owner
		// cannot finalize under a detached reader either.
		scope := s.acquireServingRepositoryScope()
		ctx = withRequestRepositoryScope(ctx, scope)
		defer scope.Release()
		// Which view answers this request: the selector the caller named, the
		// checkout its cwd sits in, or the base corpus. Resolved before the
		// overlay so a session's editor buffers layer on top of whatever
		// answers here. The lease the materialized view holds is released
		// with the request, on the same lifecycle that discards the overlay.
		controlOperation := s.checkoutControlOperation(&req)
		if controlOperation != "" {
			control, controlErr := s.resolveCheckoutControlScope(ctx, selector, &req)
			if controlErr != nil {
				return mcp.NewToolResultError(controlErr.Error()), nil
			}
			ctx = withCheckoutControl(ctx, control)
		}
		var view *requestView
		if !catalogOnlyCheckoutControl(controlOperation) {
			var viewErr error
			view, viewErr = s.resolveRequestView(ctx, selector, s.requestViewPolicy(&req, freshness))
			if viewErr != nil {
				control := checkoutControlFromContext(ctx)
				if controlOperation != "detect_changes" || control == nil || !control.CheckoutScoped {
					return mcp.NewToolResultError(viewErr.Error()), nil
				}
				// A pending graph cannot certify symbol impact. The detect handler
				// can still report this checkout's Git file changes, explicitly
				// incomplete, without substituting the primary's working tree.
				view, _ = viewFallback(false, graphview.NewViewRider(control.Selector), viewErr)
				view.rider.CheckoutID = control.Checkout.CheckoutID
				view.rider.GraphID = control.GraphID
			}
		}
		if view != nil {
			ctx = withRequestView(ctx, view)
			defer view.close()
		}
		// Tell the deadline firewall what this call is reading and what
		// repositories it is admitted to. If it stops waiting for this handler
		// — its deadline fired, or the client hung up — the handler keeps
		// running and keeps reading through both, so the firewall joins them
		// on the abandoned goroutine's behalf and says so in its answer
		// instead of leaving a silent pin. It is published even for an
		// unrouted call, which has no view but does hold a repository scope.
		// Publishing is a no-op when no firewall frame is above us (an
		// unbounded call).
		noteRetainedRequest(ctx, view, scope)
		if requireExactView && view != nil && view.rider != nil && !view.rider.Exact {
			return mcp.NewToolResultError(graphview.NewViewError(graphview.CodeViewBuilding,
				"the requested exact checkout view is unavailable; retry after publication; no fallback was served").Error()), nil
		}
		// Approved source tools serialize with the selected checkout's index
		// coordinator. The lease does not invalidate or rebuild for dry runs;
		// the shared disk-commit and reindex helpers perform those steps.
		mutationCtx, releaseMutation, mutationErr := s.prepareRoutedViewMutation(ctx, &req)
		if mutationErr != nil {
			return mutationErr, nil
		}
		ctx = mutationCtx
		defer releaseMutation()
		if refused := s.refuseRoutedViewMutation(ctx, req.Params.Name); refused != nil {
			return refused, nil
		}
		// What the view can answer, checked against what this operation
		// needs, before the handler runs — a thin view must refuse rather
		// than answer thinly and look complete doing it.
		if !catalogOnlyCheckoutControl(controlOperation) {
			if refused := s.evaluateRequestCapabilities(ctx, &req, capabilities); refused != nil {
				return refused, nil
			}
		}
		if injectOverlay && !catalogOnlyCheckoutControl(controlOperation) && view.acceptsBufferOverlay() {
			var err error
			ctx, _, err = s.prepareOverlayRequest(ctx)
			if err != nil {
				if ctxErr := requestContextError(ctx, err); ctxErr != nil {
					return nil, ctxErr
				}
				// Drift and ownership failures surface as structured tool
				// errors so the client can refresh and resubmit without a
				// transport-level failure.
				return mcp.NewToolResultError(err.Error()), nil
			}
		}
		// Warmup fast path: when the daemon is still warming up and
		// this is a graph-querying tool, the handler still runs (so
		// the caller gets a best-effort partial answer from the part
		// of the graph indexed so far) and the result is decorated
		// with a structured `warming` block — flag + real progress
		// percentage + phase + message. Graph-independent tools are
		// untouched; a ready daemon is a transparent pass-through.
		// See warmup_fastpath.go.
		env, warming := s.checkWarmupFastPath(req.Params.Name)
		// Retrieval query logging: time the call and install a
		// result-count holder so handlers can report an exact count
		// (the logger falls back to parsing the response otherwise).
		logQuery := s.queryLog.shouldLog(req.Params.Name)
		var qStart time.Time
		if logQuery {
			ctx, _ = withResultCount(ctx)
			qStart = time.Now()
		}
		res, hErr := h(ctx, req)
		// Book the retrieval half of the savings ledger for a DIRECT legacy
		// call. Facade calls do not reach here under their legacy name — the
		// facade holds the unwrapped handler (prepareTool) and books in
		// invokeFacadeSpec — and facade names are not in the allow-list, so
		// the two paths cannot double-count.
		if hErr == nil {
			s.recordRetrievalSavings(ctx, req.Params.Name, res)
		}
		// Opt-in usage telemetry: count this tool invocation by name only —
		// never arguments or results. nil-safe, consent-gated, and fail-silent,
		// so a disabled or absent recorder adds nothing to the dispatch path.
		s.recorder.Record("mcp_tool_call", req.Params.Name)
		if logQuery {
			s.queryLog.record(s, ctx, req, res, hErr, qStart)
		}
		if warming && hErr == nil {
			res = decorateResultWithWarming(res, env)
		}
		// Inline freshness: when a file-reading tool returns content for a
		// file that has changed on disk since it was indexed, attach a
		// small `freshness` block so the agent knows the graph view may lag
		// the working tree. Omitted (zero cost) for the common fresh case.
		if hErr == nil {
			if rider := s.freshnessRiderFor(req.Params.Name, req); rider != nil {
				res = decorateResultWithFreshness(res, rider)
			} else if isFreshnessListTool(req.Params.Name) {
				// List tools get a per-file sweep: any hit whose file drifted
				// or vanished on disk is flagged with per-repo provenance.
				res = s.decorateListResultWithFreshness(res)
			}
			// Which view answered rides in that same block: a response that
			// came from somewhere other than the base — or fell back to it —
			// must say so where the caller already looks for provenance.
			res = s.attachViewRider(ctx, res)
			res = s.attachCheckoutControlScope(ctx, res)
		}
		// The arg guard's warn rider lands here — after the warming and
		// freshness decorators, both of which rebuild the text result from
		// Content[0] and would drop a rider block attached any earlier. This
		// is what keeps the unknown-option signal alive on the case it
		// exists for: a drifted file mid-edit carrying both riders.
		if hErr == nil {
			res = s.attachPendingArgGuardRider(ctx, res)
		}
		// Capture large successful responses into the session ring so
		// the post-filter tools can re-cut them without re-querying.
		if hErr == nil {
			s.captureResponse(ctx, req.Params.Name, res)
		}
		// One-shot momentum note: after many read calls in one session,
		// remind the agent that what it already holds is citeable
		// (momentum.go). No-op for non-read tools and error results.
		if hErr == nil {
			res = s.maybeAttachMomentumNote(ctx, req.Params.Name, res)
		}
		return res, hErr
	})
}

// errBaseSHADrift is the structured drift error returned by the
// disk-write edit tools (edit_file / edit_symbol / write_file) when
// the caller-supplied base_sha does not match the current on-disk
// blob SHA. The message mirrors daemon.ErrOverlayDrift so callers
// can pattern-match on a single substring across overlay-push and
// plain-write paths: "re-read and resubmit".
const errBaseSHADrift = "base_sha mismatch — re-read and resubmit"

// gitBlobSHA computes the git blob SHA-1 of the given content. The
// hash matches `git ls-files -s` / `git hash-object` output (i.e.
// sha1 of "blob <len>\0<content>"), so editors can pass the SHA they
// already have without any client-side reformatting. The returned
// string is lowercase hex. This is the canonical drift-anchor helper
// shared by overlay_push and the disk-write edit tools.
func gitBlobSHA(data []byte) string {
	h := sha1.New()
	// hash.Hash.Write never errors; fmt.Fprintf returns (n, err)
	// because it's the io.Writer interface, but the underlying
	// hash.Hash's Write contract forbids non-nil errors. Discard
	// both to keep the linter happy without inventing fake error
	// handling.
	fmt.Fprintf(h, "blob %d\x00", len(data))
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// normalizeExpectedSHA lowercases and trims a caller-supplied
// base_sha so comparisons are case- and whitespace-insensitive.
func normalizeExpectedSHA(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// overlaySHAMatches re-computes the git blob SHA of an on-disk file
// and compares it to the SHA the editor recorded at didOpen time.
// Returns false on any read error: the safer default is "drift" —
// the client re-reads and resubmits.
func overlaySHAMatches(absPath, expected string) bool {
	expected = normalizeExpectedSHA(expected)
	if expected == "" {
		return true
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return false
	}
	return gitBlobSHA(data) == expected
}

// _ keeps sync.Mutex referenced by the package even after future
// refactors strip a field — the import lints flagged a phantom
// dependency in the prior iteration; harmless guard.
var _ sync.Mutex

// requestViewPin is one piece of detached work's own hold on the view its
// request read.
//
// The request's view is released by `defer view.close()` above, on handler
// return. Work that deliberately outlives the handler — an admitted
// publication, a detached first index, a handler the deadline firewall
// stopped waiting for — therefore used to run over a payload the retirement
// sweep was free to collect the moment the response went out. A pin is the
// joined-consumer half of that lifetime (graphview.RepoView.Handoff): the
// generations stay pinned, retirement keeps refusing them and
// LeaseManager.WaitDrain keeps blocking, until the detached worker releases
// its own handle.
//
// release is mandatory and idempotent, and every method is nil-safe: a
// request that materialized no view hands out a nil pin and the call sites
// stay branch-free.
type requestViewPin struct {
	handoff *graphview.ViewHandoff
	// base carries the two halves a routed request's base pin holds:
	// generation zero, and the registered owner that speaks for it. Both used
	// to die at `defer view.close()` — requestView.close releases the base pin
	// unconditionally — so a detached worker kept the derived stack pinned and
	// lost the corpus underneath it.
	base *graphview.BasePinHandoff
	// owner is the request's own repository admission, which exists even when
	// no view was materialized at all.
	owner    *graphview.RepositoryReadHandoff
	consumer string
	once     sync.Once
}

// requestRepositoryScopeKey carries the repository admission one `tools/call`
// holds. Unexported for the same reason the view key is: nothing outside this
// package may smuggle a lifetime onto an unrelated context.
type requestRepositoryScopeKey struct{}

// withRequestRepositoryScope publishes the request's repository admission so
// detached work started inside the handler can join it.
func withRequestRepositoryScope(ctx context.Context, scope *graphview.RepositoryReadLease) context.Context {
	if scope == nil {
		return ctx
	}
	return context.WithValue(ctx, requestRepositoryScopeKey{}, scope)
}

// requestRepositoryScopeFromContext returns the admission this request holds,
// nil when it holds none.
func requestRepositoryScopeFromContext(ctx context.Context) *graphview.RepositoryReadLease {
	if ctx == nil {
		return nil
	}
	scope, _ := ctx.Value(requestRepositoryScopeKey{}).(*graphview.RepositoryReadLease)
	return scope
}

// acquireServingRepositoryScope admits this request to every repository owner
// it may read, for the request's lifetime.
//
// Nil for a server with no view catalog wired (the embedded and test surfaces)
// and for a process where nothing has registered an owner yet: there is no
// repository lifetime to hold, which is exactly the state every request was in
// before this existed. Release and Handoff are nil-safe, so no call site
// branches on it.
func (s *Server) acquireServingRepositoryScope() *graphview.RepositoryReadLease {
	if s == nil || s.materializer == nil || s.materializer.Leases == nil {
		return nil
	}
	return s.materializer.Leases.AcquireServingRepositoryRead()
}

// payloadRepositories names the repositories whose payload a worker detached
// from this request keeps reading: the repository the materialized stack
// belongs to.
//
// It is deliberately the stack's repository and not the request's whole
// admitted scope — see handoffRequest — and it deliberately omits the base
// corpus's owner, which the base pin carries its own half of and hands off
// with it (BasePin.Handoff). A view that materialized no stack names nothing,
// so a detached worker is admitted to nothing rather than to everything.
func payloadRepositories(view *requestView) []string {
	if view == nil || view.materialized == nil {
		return nil
	}
	prefix := view.materialized.ID.RepoPrefix
	if prefix == "" {
		return nil
	}
	return []string{prefix}
}

// handoffRequestView joins consumer to what the request answering ctx reads:
// the view's generation stack, the base corpus beneath it, and the repository
// owners that speak for them.
//
// It returns nil in two different situations, which the counters separate:
//
//   - there is nothing to pin, because this request materialized no payload
//     at all — neither a generation stack nor a base pin. That is the
//     unrouted base-corpus request, the shape most tool calls have: a worker
//     it leaves behind inherits no payload, so there is no lifetime to
//     extend and nothing is recorded.
//   - the view exists but every holder of its lease has already released, so
//     graphview refuses the join. That is recorded as a refusal, because the
//     payload underneath may already be gone and the caller is now running
//     unpinned. It is not reachable from inside a live handler — close() is
//     deferred to handler return — so a non-zero refusal count means a
//     detached worker asked for a pin after its request had already ended,
//     which is a wiring bug rather than a routine outcome.
//
// A caller must never read a nil pin as a successful handoff.
func handoffRequestView(ctx context.Context, consumer string) *requestViewPin {
	return handoffRequest(requestViewFromContext(ctx), requestRepositoryScopeFromContext(ctx), consumer)
}

// handoffRequest is handoffRequestView for a caller that already holds what
// the request read rather than a context carrying it — the deadline firewall,
// which resolved nothing itself and was handed both by the middleware it
// bounds.
//
// Three holds travel together, because they protect three different ways the
// payload a worker is reading can disappear underneath it: the derived
// generation stack (retirement), the base corpus generation (retirement of
// generation zero), and the repository owner (finalization, then a physical
// purge of that repository's rows).
//
// The owner hold is taken for the repositories this request's payload belongs
// to (payloadRepositories below), NOT for the whole scope the request itself
// is admitted to. A serving request cannot know which repositories its handler
// will read, so it is admitted to all of them — safe for one request lifetime,
// and NOT safe to hand to work of unbounded duration, which would make one
// repository's background job block every other repository's drain and the
// physical purge behind it. A worker names what it reads.
//
// "Nothing to pin" and "refused" stay distinct: a request that materialized no
// payload at all offers nothing and records nothing, while one whose every
// hold had already been released records a refusal — only reachable from a
// worker asking after its request ended, which is a wiring bug rather than a
// routine outcome.
func handoffRequest(view *requestView, scope *graphview.RepositoryReadLease, consumer string) *requestViewPin {
	if view == nil || (view.materialized == nil && view.basePin == nil) {
		// The unrouted base-corpus request: it materialized nothing, so a
		// worker it leaves behind inherits no payload to keep alive. Its own
		// repository admission covers it for as long as it is being served
		// and is deliberately not extended past that.
		return nil
	}
	pin := &requestViewPin{consumer: consumer}
	if view.materialized != nil {
		pin.handoff = view.materialized.Handoff()
	}
	if view.basePin != nil {
		pin.base = view.basePin.Handoff()
	}
	pin.owner = scope.HandoffFor(payloadRepositories(view)...)
	if pin.handoff == nil && pin.base == nil && pin.owner == nil {
		viewmetrics.Count(viewmetrics.HandoffTotal, consumer, viewmetrics.HandoffRefused)
		return nil
	}
	viewmetrics.Count(viewmetrics.HandoffTotal, consumer, viewmetrics.HandoffJoined)
	viewmetrics.AddGauge(viewmetrics.HandoffsOutstanding, 1, consumer)
	return pin
}

// release drops this worker's hold. Idempotent and nil-safe; the generations
// are unpinned once the request and every other joined consumer have released
// too.
func (p *requestViewPin) release() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		p.handoff.Close()
		p.base.Release()
		p.owner.Release()
		viewmetrics.AddGauge(viewmetrics.HandoffsOutstanding, -1, p.consumer)
	})
}

// generations lists the payload generations this pin keeps alive, bottom
// first. Nil for a pin that was never taken, which is what lets a diagnosis
// say "nothing is retained" without a second flag.
//
// It is the derived stack only, exactly as RepoView.Generations reports it:
// the base corpus generation the pin may also hold is shared, unretirable and
// named by nothing, so listing it in an operator-facing "retirement is refused
// for them" sentence would be noise rather than information.
func (p *requestViewPin) generations() []int64 {
	if p == nil {
		return nil
	}
	return p.handoff.Generations()
}

// reader is the composed stack the pinning worker may keep reading through
// after the request that materialized it has returned.
func (p *requestViewPin) reader() graph.Reader {
	if p == nil || p.handoff == nil {
		return nil
	}
	return p.handoff.Reader
}
