package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

// Warmup-aware fast path for tool calls.
//
// The daemon serves the MCP socket as soon as it binds — long before
// warmup (re-index every tracked repo, run the cross-repo resolve,
// derive the graph-wide passes) has finished. A graph-querying tool
// invoked in that window would otherwise either answer against a
// half-built graph with no signal that it is incomplete, or — for
// tools that genuinely need a finished graph — fail with a bare
// error the caller cannot reason about.
//
// `subscribe_workspace_readiness` already pushes phase transitions
// out-of-band; this is the IN-BAND counterpart. Every graph-querying
// tool call routed through wrapToolHandler is checked against the
// readiness broadcaster's last-published phase. While the daemon is
// warming up the call still runs (so the caller gets a best-effort
// partial answer from the portion of the graph already indexed) and
// the result is decorated with a structured `warming` block carrying
// a flag, a real progress percentage, the current phase, and a
// human-readable message. A caller can act on the partial answer or
// retry once `warming` clears.
//
// When ready (or in a single-process server with no warmup pipeline)
// this path is a transparent pass-through.

// warmupPhaseProgress maps each daemon warmup phase to a real
// completion percentage. The pipeline runs the phases in this strict
// order:
//
//	snapshot_loaded → parallel_parse → parallel_parse_done → resolve →
//	resolve_done → ready (queryable) → deferred_passes_all →
//	deferred_passes_all_done → global_resolve → global_resolve_done →
//	end_batch → end_batch_done → watcher_started → enrichment_complete
//
// The weights are cost-proportional rather than evenly spaced:
// parallel_parse (parsing every tracked repo off disk) dominates total
// warmup time, so it spans 15→55. `ready` fires at the resolve point —
// once references are queryable — so the warming envelope clears there;
// the phases past it report background-enrichment progress and carry
// ready:true. The numbers are derived from the phase the daemon actually
// reports — never a placeholder.
var warmupPhaseProgress = map[string]int{
	"snapshot_loaded":          5,
	"parallel_parse":           15,
	"parallel_parse_done":      55,
	"resolve":                  57,
	"resolve_done":             59,
	"deferred_passes_all":      60,
	"deferred_passes_all_done": 75,
	"global_resolve":           78,
	"global_resolve_done":      88,
	"end_batch":                90,
	"end_batch_done":           95,
	"watcher_started":          98,
	"ready":                    100,
	"enrichment_complete":      100,
}

// warmupExemptTools is the set of MCP tools that do NOT depend on the
// graph being fully warmed up, so the fast path leaves them entirely
// alone — no warming block, no decoration.
//
// Three groups, exhaustive:
//
//   - Readiness / subscription tools: a caller polling warmup must
//     reach these regardless of warmup state, or it could never learn
//     the daemon is warming.
//   - Session-control tools: planning-mode and session lifecycle
//     never read the graph.
//   - Discovery / indexing tools: tools_search walks the static tool
//     catalog; index_repository / track_repository are the very
//     operations that POPULATE the graph and must run during warmup.
//
// graph_stats is deliberately NOT exempt: it already surfaces a
// workspace_readiness block, and decorating it with the in-band
// warming flag keeps a single canonical signal across both shapes.
var warmupExemptTools = map[string]bool{
	// Readiness / subscription channels.
	"subscribe_workspace_readiness":   true,
	"unsubscribe_workspace_readiness": true,
	"subscribe_diagnostics":           true,
	"unsubscribe_diagnostics":         true,
	"subscribe_daemon_health":         true,
	"unsubscribe_daemon_health":       true,
	"subscribe_stale_refs":            true,
	"unsubscribe_stale_refs":          true,
	"subscribe_graph_invalidated":     true,
	"unsubscribe_graph_invalidated":   true,
	// Session-control tools.
	"set_planning_mode": true,
	// Discovery / indexing tools — these run the graph build itself.
	"tools_search":     true,
	"index_repository": true,
	"track_repository": true,
}

// warmupState is the in-band snapshot of the daemon's warmup curve,
// derived from the readiness broadcaster's last-published phase.
type warmupState struct {
	// known is false when no readiness phase has ever been published
	// — a single-process `gortex server` (no warmup pipeline) or a
	// daemon that has not yet reached its first phase. Callers treat
	// !known as "not warming, proceed normally".
	known bool
	// ready is true once the daemon has made the graph queryable. Expensive
	// whole-graph work remains deferred until enriched is also true.
	ready bool
	// enriched is true only after the daemon publishes its terminal
	// enrichment_complete phase.
	enriched bool
	// phase is the last-published warmup phase name.
	phase string
	// percent is the cost-proportional completion percentage in
	// 0..100, derived from phase.
	percent int
}

// warming reports whether a graph-querying tool should be decorated
// with the warming envelope — true only when a phase is known and the
// daemon has not yet reached `ready`.
func (w warmupState) warming() bool { return w.known && !w.ready }

// enrichmentIncomplete is intentionally stricter than warming: queryable
// `ready` is published before deferred/global startup passes finish. Only
// whole-graph consumers use this predicate; ordinary graph lookups remain
// available as soon as ready is true.
func (w warmupState) enrichmentIncomplete() bool { return w.known && !w.enriched }

// warmupStateFromSnapshot derives a warmupState from a readiness
// broadcaster payload (the `{phase, ready, ...}` map the broadcaster
// stores as last-known state). A nil snapshot means no phase has been
// published yet.
func warmupStateFromSnapshot(snap map[string]any) warmupState {
	if snap == nil {
		return warmupState{}
	}
	st := warmupState{known: true}
	if phase, ok := snap["phase"].(string); ok {
		st.phase = phase
	}
	if ready, ok := snap["ready"].(bool); ok {
		st.ready = ready
	}
	if enriched, ok := snap["enriched"].(bool); ok {
		st.enriched = enriched
	}
	// The phase name is the compatibility source for publishers that omit the
	// explicit enriched flag from their terminal payload.
	if st.phase == "enrichment_complete" {
		st.enriched = true
	}
	// Percentage is derived from the published phase. An unrecognised
	// phase (forward-compatibility: the daemon added a phase this
	// build does not know) falls back to ready vs. mid-warmup
	// extremes so the number is never a placeholder.
	if pct, ok := warmupPhaseProgress[st.phase]; ok {
		st.percent = pct
	} else if st.ready {
		st.percent = 100
	} else {
		st.percent = 1
	}
	// A daemon that published `ready` is fully warm regardless of
	// which phase string carried the flag.
	if st.ready {
		st.percent = 100
	}
	return st
}

// warmupSnapshot returns the daemon's current warmup state as seen by
// the MCP server. nil broadcaster (single-process modes that never
// wire the publisher) yields a not-known state, so the fast path
// stays a pure pass-through there.
func (s *Server) warmupSnapshot() warmupState {
	if s == nil || s.readinessBroadcaster == nil {
		return warmupState{}
	}
	return warmupStateFromSnapshot(s.readinessBroadcaster.snapshot())
}

// warmupEnvelope is the structured `warming` block merged into a
// graph-querying tool's result while the daemon is still warming up.
// It is also the standalone body returned for tools that genuinely
// cannot produce a partial answer mid-warmup.
type warmupEnvelope struct {
	// Warming is always true on an emitted envelope — it is the flag
	// a caller branches on.
	Warming bool `json:"warming"`
	// Percent is the real warmup completion percentage (0..100).
	Percent int `json:"percent"`
	// Phase is the daemon's current warmup phase.
	Phase string `json:"phase"`
	// Retriable is always true: re-issuing the call once warmup
	// completes yields the full answer.
	Retriable bool `json:"retriable"`
	// PartialResults is true when the decorated tool result still
	// carries best-effort data from the portion of the graph already
	// indexed; false on a standalone warming-only envelope.
	PartialResults bool `json:"partial_results"`
	// Message is a human-readable explanation for a caller that does
	// not branch on the structured fields.
	Message string `json:"message"`
}

// newWarmupEnvelope builds the warming block for a warmup state.
// partial flags whether the accompanying tool result still carries
// best-effort data (decoration path) or none (standalone path).
func newWarmupEnvelope(w warmupState, partial bool) warmupEnvelope {
	var msg string
	if partial {
		msg = fmt.Sprintf(
			"daemon is still warming up (phase %q, %d%% complete) — these results are partial, "+
				"computed from the portion of the graph indexed so far; retry once warmup completes for the full answer",
			w.phase, w.percent)
	} else {
		msg = fmt.Sprintf(
			"daemon is still warming up (phase %q, %d%% complete) — retry this call once warmup completes",
			w.phase, w.percent)
	}
	return warmupEnvelope{
		Warming:        true,
		Percent:        w.percent,
		Phase:          w.phase,
		Retriable:      true,
		PartialResults: partial,
		Message:        msg,
	}
}

var enrichmentDeferredResources = map[string]struct{}{
	"gortex://report":    {},
	"gortex://god-nodes": {},
	"gortex://surprises": {},
	"gortex://audit":     {},
	"gortex://questions": {},
}

func newEnrichmentPendingEnvelope(w warmupState) warmupEnvelope {
	env := newWarmupEnvelope(w, false)
	env.Message = fmt.Sprintf(
		"daemon startup enrichment is still running (phase %q); this whole-graph operation is deferred to protect indexing; retry after workspace readiness reaches enrichment_complete",
		w.phase)
	return env
}

func enrichmentPendingToolResult(w warmupState) *mcp.CallToolResult {
	env := newEnrichmentPendingEnvelope(w)
	body, err := json.Marshal(map[string]any{"warming": env})
	if err != nil {
		return mcp.NewToolResultError(env.Message)
	}
	return mcp.NewToolResultText(string(body))
}

func (s *Server) deferAnalyzerResource(uri string) (warmupState, bool) {
	if _, wholeGraph := enrichmentDeferredResources[uri]; !wholeGraph {
		return warmupState{}, false
	}
	w := s.warmupSnapshot()
	return w, w.enrichmentIncomplete()
}

// checkWarmupFastPath is the pre-handler hook wired into
// wrapToolHandler. It returns:
//
//   - (envelope, true) when the caller should run the handler and
//     then decorate its result with the warming block — the daemon is
//     warming and the tool depends on the graph.
//   - (zero, false) when the call should proceed untouched — the
//     daemon is ready, no phase is known, or the tool is graph-
//     independent.
//
// Unlike a hard gate, the warming case never blocks the handler: the
// handler runs against whatever the graph holds right now (a
// best-effort partial answer) and the decoration marks it partial.
func (s *Server) checkWarmupFastPath(toolName string) (warmupEnvelope, bool) {
	if warmupExemptTools[toolName] {
		return warmupEnvelope{}, false
	}
	w := s.warmupSnapshot()
	if !w.warming() {
		return warmupEnvelope{}, false
	}
	return newWarmupEnvelope(w, true), true
}

// decorateResultWithWarming merges the warming envelope into a tool
// result. The handler's result is a single JSON TextContent block (the
// shape every Gortex tool returns via respondJSONOrTOON / GCX
// encoders); decoration parses that JSON, attaches a top-level
// `warming` key, and re-marshals.
//
// Three result shapes are handled so no tool slips through unmarked:
//
//   - JSON object → the `warming` key is added alongside existing keys.
//   - JSON array / scalar → wrapped as {"warming": …, "results": <orig>}
//     so the partial payload is preserved under a stable key.
//   - empty / non-JSON / nil → replaced with a standalone warming-only
//     envelope (the caller genuinely got nothing usable).
//
// An error result is decorated too: a structured tool error raised
// mid-warmup is very likely a consequence of the half-built graph, and
// the caller should see the retry signal rather than a bare failure.
func decorateResultWithWarming(res *mcp.CallToolResult, env warmupEnvelope) *mcp.CallToolResult {
	text, ok := singleTextContent(res)
	if !ok || text == "" {
		// Nothing usable came back — emit a standalone warming-only
		// envelope so the caller still gets flag + percent + phase.
		empty := env
		empty.PartialResults = false
		empty.Message = fmt.Sprintf(
			"daemon is still warming up (phase %q, %d%% complete) — no result could be produced yet; "+
				"retry this call once warmup completes",
			env.Phase, env.Percent)
		body, mErr := json.Marshal(map[string]any{"warming": empty})
		if mErr != nil {
			return res
		}
		return rebuildTextResult(res, string(body))
	}

	var asObj map[string]any
	if json.Unmarshal([]byte(text), &asObj) == nil {
		// Don't clobber a `warming` key a handler set itself.
		if _, exists := asObj["warming"]; !exists {
			asObj["warming"] = env
		}
		body, mErr := json.Marshal(asObj)
		if mErr != nil {
			return res
		}
		return rebuildTextResult(res, string(body))
	}

	// Array / scalar / non-JSON (e.g. TOON, GCX) payload: wrap it so
	// the original partial answer is preserved verbatim under a
	// stable key while the warming flag rides alongside it.
	var asAny any
	if json.Unmarshal([]byte(text), &asAny) == nil {
		body, mErr := json.Marshal(map[string]any{"warming": env, "results": asAny})
		if mErr != nil {
			return res
		}
		return rebuildTextResult(res, string(body))
	}
	body, mErr := json.Marshal(map[string]any{"warming": env, "results": text})
	if mErr != nil {
		return res
	}
	return rebuildTextResult(res, string(body))
}

// singleTextContent extracts the text of a result's single
// TextContent block. Returns ("", false) when the result is nil, has
// no content, or its first block is not text.
func singleTextContent(res *mcp.CallToolResult) (string, bool) {
	if res == nil || len(res.Content) == 0 {
		return "", false
	}
	tc, ok := mcp.AsTextContent(res.Content[0])
	if !ok {
		return "", false
	}
	return tc.Text, true
}

// rebuildTextResult returns a copy of res with its text content
// replaced by body, preserving the IsError flag and the _meta
// envelope. StructuredContent is dropped because it would otherwise
// disagree with the decorated text block — the text block is the
// canonical payload every Gortex tool ships. _meta is not payload:
// the scope disclosure and the view rider both ride there, and a
// decorator that rewrites the text block must not take them with it.
func rebuildTextResult(res *mcp.CallToolResult, body string) *mcp.CallToolResult {
	out := mcp.NewToolResultText(body)
	if res != nil {
		out.IsError = res.IsError
		out.Meta = res.Meta
	}
	return out
}

// mergeResultMeta writes fields into the result's _meta envelope — the
// format-independent channel a payload's wire encoding cannot reach.
// Creates the block when the result carries none.
func mergeResultMeta(res *mcp.CallToolResult, fields map[string]any) *mcp.CallToolResult {
	if res == nil || len(fields) == 0 {
		return res
	}
	if res.Meta == nil {
		res.Meta = mcp.NewMetaFromMap(fields)
		return res
	}
	if res.Meta.AdditionalFields == nil {
		res.Meta.AdditionalFields = map[string]any{}
	}
	for name, value := range fields {
		res.Meta.AdditionalFields[name] = value
	}
	return res
}
