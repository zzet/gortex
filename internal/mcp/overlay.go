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
		selector, selectorErr := takeViewSelector(&req)
		if selectorErr != nil {
			return mcp.NewToolResultError(selectorErr.Error()), nil
		}
		consistency, consistencyErr := takeViewConsistencyRequest(&req)
		if consistencyErr != nil {
			return mcp.NewToolResultError(consistencyErr.Error()), nil
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
		// Reject bounded-input violations before resolving a view. Resolving a
		// cold ref can start an extraction, so leaving these checks only in the
		// leaf file handlers lets an invalid request consume indexing work.
		if admissionErr := s.validateRequestBeforeView(&req); admissionErr != nil {
			return mcp.NewToolResultError(admissionErr.Error()), nil
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
		// Which view answers this request: the selector the caller named, the
		// checkout its cwd sits in, or the base corpus. Resolved before the
		// overlay so a session's editor buffers layer on top of whatever
		// answers here. The lease the materialized view holds is released
		// with the request, on the same lifecycle that discards the overlay.
		view, viewErr := s.resolveRequestViewConsistently(ctx, selector, s.requestViewPolicy(&req), consistency)
		if viewErr != nil {
			if view != nil {
				defer view.close()
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			result := mcp.NewToolResultError(viewErr.Error())
			if view != nil {
				result = s.attachViewRider(withRequestView(ctx, view), result)
			}
			return result, nil
		}
		if view != nil {
			ctx = withRequestView(ctx, view)
			defer view.close()
		}
		// Routed source writes are admitted only for handlers that confine bytes
		// and publish freshness through this exact checkout's coordinator.
		if refused := s.refuseRoutedViewMutation(ctx, &req); refused != nil {
			return refused, nil
		}
		// What the view can answer, checked against what this operation
		// needs, before the handler runs — a thin view must refuse rather
		// than answer thinly and look complete doing it.
		if refused := s.evaluateRequestCapabilities(ctx, &req, capabilities); refused != nil {
			return refused, nil
		}
		if injectOverlay && view.acceptsBufferOverlay() {
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
			if rider := s.freshnessRiderFor(ctx, req.Params.Name, req); rider != nil {
				res = decorateResultWithFreshness(res, rider)
			} else if isFreshnessListTool(req.Params.Name) {
				// List tools get a per-file sweep: any hit whose file drifted
				// or vanished on disk is flagged with per-repo provenance.
				res = s.decorateListResultWithFreshness(ctx, res)
			}
			// Which view answered rides in that same block: a response that
			// came from somewhere other than the base — or fell back to it —
			// must say so where the caller already looks for provenance.
			res = s.attachViewRider(ctx, res)
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
