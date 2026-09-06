package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	toon "github.com/toon-format/toon-go"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
	"github.com/zzet/gortex/internal/telemetry"
)

// minTierParamDescription is the `min_tier` parameter description shared by
// every edge-returning tool. Mentioning the tier vocabulary inline lets agents
// pick an appropriate filter without consulting external docs.
const minTierParamDescription = "Filter edges by minimum confidence tier. " +
	"Values (highest to lowest): lsp_resolved (compiler-verified), " +
	"lsp_dispatch (interface→impl via semantic provider), " +
	"ast_resolved (tree-sitter direct match), " +
	"ast_inferred (type heuristic), " +
	"text_matched (name-only). When omitted, text_matched edges are " +
	"auto-suppressed whenever resolver-verified evidence exists — the " +
	"response then carries text_matched_suppressed with the hidden count; " +
	"pass min_tier:\"text_matched\" to include them. If the target's file was " +
	"re-parsed after the last enrichment pass, the response also carries " +
	"suppression_caveat warning the hidden rows may be real. " +
	"find_usages / get_callers additionally report name_only_candidates: call " +
	"sites that name the symbol but bound to nothing, so a thin edge list is " +
	"legible as \"N verified, M unverified\" — min_tier:\"text_matched\" returns " +
	"those rows too. " +
	"Use lsp_resolved for high-stakes refactors where false positives are expensive."

const includeSpeculativeParamDescription = "Include best-guess speculative " +
	"dynamic-dispatch edges (computed-member calls like obj[name](), getattr, " +
	"decorator registries). Default false — these are hidden low-confidence " +
	"fan-outs minted only when synthesize_speculative_dispatch is enabled; " +
	"set true to surface them (each carries candidate_count + a speculative tier)."

// isCompact checks if the compact flag is set in the request.
func isCompact(req mcp.CallToolRequest) bool {
	if v, ok := req.GetArguments()["compact"].(bool); ok {
		return v
	}
	return false
}

// isTOON reports whether the caller requested the TOON wire format
// for this tool call. Selection mirrors `Server.isGCX`:
//
//  1. Explicit `format` arg wins.
//  2. Otherwise the per-session default (driven by MCP `clientInfo`)
//     decides — TOON is the second-tier compact format used when a
//     client decodes TOON but not GCX. Today no shipping client is
//     known to be in this bucket; the helper exists for forward
//     compat as plugins evolve.
//  3. Default false — JSON wins.
func (s *Server) isTOON(ctx context.Context, req mcp.CallToolRequest) bool {
	if v, ok := req.GetArguments()["format"].(string); ok && v != "" {
		return v == "toon"
	}
	if s == nil {
		return false
	}
	return s.resolveSessionFormat(ctx) == "toon"
}

// toonNodeRow is a TOON-optimized flat representation of a graph node.
type toonNodeRow struct {
	ID          string `toon:"id"`
	Kind        string `toon:"kind"`
	Name        string `toon:"name"`
	FilePath    string `toon:"file_path"`
	StartLine   int    `toon:"start_line"`
	Enclosing   string `toon:"enclosing,omitempty"`
	EnclosingID string `toon:"enclosing_id,omitempty"`
	IsTest      bool   `toon:"is_test"`
	TestRole    string `toon:"test_role"`
	TestRunner  string `toon:"test_runner,omitempty"`
	TypeFlavor  string `toon:"type_flavor,omitempty"`
	UIComponent string `toon:"ui_component,omitempty"`
}

// toonEdgeRow is a TOON-optimized flat representation of a graph edge.
type toonEdgeRow struct {
	From       string  `toon:"from"`
	To         string  `toon:"to"`
	Kind       string  `toon:"kind"`
	Origin     string  `toon:"origin,omitempty"`
	Tier       string  `toon:"tier,omitempty"`
	Confidence float64 `toon:"confidence"`
	Label      string  `toon:"label"`
}

// toonCallerNoteRow is a TOON-optimized flat representation of one
// caller's concurrency annotation (get_callers only).
type toonCallerNoteRow struct {
	ID                 string `toon:"id"`
	SyncGuarded        bool   `toon:"sync_guarded"`
	SyncGuardedWhy     string `toon:"sync_guarded_why,omitempty"`
	CrossConcurrent    bool   `toon:"cross_concurrent"`
	CrossConcurrentWhy string `toon:"cross_concurrent_why,omitempty"`
}

// toonSubGraphResult wraps nodes and edges for TOON tabular output.
// toonZeroEdgeCaveat mirrors graph.ZeroEdgeCaveat with a plain string
// class: toon-go rejects the named ZeroEdgeClass string type, and the
// silent JSON fallback that error triggered meant a caveat-bearing
// subgraph never actually rendered as TOON.
type toonZeroEdgeCaveat struct {
	Class   string `toon:"class"`
	Message string `toon:"message"`
}

func toonCaveat(c *graph.ZeroEdgeCaveat) *toonZeroEdgeCaveat {
	if c == nil {
		return nil
	}
	return &toonZeroEdgeCaveat{Class: string(c.Class), Message: c.Message}
}

type toonSubGraphResult struct {
	Nodes     []toonNodeRow `toon:"nodes"`
	Edges     []toonEdgeRow `toon:"edges"`
	Total     int           `toon:"total"`
	Truncated bool          `toon:"truncated"`
	// TotalEdges carries the full row count when a row cap cut the edge
	// list; zero (and omitted) otherwise, so untrimmed responses and
	// node-truncated BFS results keep their wire shape.
	TotalEdges            int                 `toon:"total_edges,omitempty"`
	Caveat                *toonZeroEdgeCaveat `toon:"caveat,omitempty"`
	TextMatchedSuppressed int                 `toon:"text_matched_suppressed,omitempty"`
	SuppressionCaveat     string              `toon:"suppression_caveat,omitempty"`
	RelatedTools          string              `toon:"related_tools,omitempty"`
	CallerNotes           []toonCallerNoteRow `toon:"caller_notes,omitempty"`
	UsageSummary          *query.UsageSummary `toon:"usage_summary,omitempty"`
}

// toonSearchResult wraps search results for TOON tabular output.
type toonSearchResult struct {
	Results   []toonNodeRow `toon:"results"`
	Total     int           `toon:"total"`
	Truncated bool          `toon:"truncated"`
}

// nodesToTOONRows converts graph nodes to flat TOON rows.
func nodesToTOONRows(nodes []*graph.Node) []toonNodeRow {
	rows := make([]toonNodeRow, 0, len(nodes))
	for _, n := range nodes {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		isTest, _ := n.Meta["is_test"].(bool)
		testRole, _ := n.Meta["test_role"].(string)
		testRunner, _ := n.Meta["test_runner"].(string)
		typeFlavor, _ := n.Meta["type_flavor"].(string)
		uiComponent, _ := n.Meta["ui_component"].(string)
		encID, encName := graph.EnclosingFromID(n.ID, n.Kind)
		rows = append(rows, toonNodeRow{
			ID:          n.ID,
			Kind:        string(n.Kind),
			Name:        n.Name,
			FilePath:    n.FilePath,
			StartLine:   n.StartLine,
			Enclosing:   encName,
			EnclosingID: encID,
			IsTest:      isTest,
			TestRole:    testRole,
			TestRunner:  testRunner,
			TypeFlavor:  typeFlavor,
			UIComponent: uiComponent,
		})
	}
	return rows
}

// callerNotesToTOONRows flattens the get_callers concurrency-annotation
// map into TOON rows, sorted by node ID for deterministic output.
// Returns nil for an empty map so the `caller_notes` field is omitted.
func callerNotesToTOONRows(notes map[string]*graph.ConcurrencyAnnotation) []toonCallerNoteRow {
	if len(notes) == 0 {
		return nil
	}
	ids := make([]string, 0, len(notes))
	for id := range notes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows := make([]toonCallerNoteRow, 0, len(ids))
	for _, id := range ids {
		a := notes[id]
		rows = append(rows, toonCallerNoteRow{
			ID:                 id,
			SyncGuarded:        a.SyncGuarded,
			SyncGuardedWhy:     a.SyncGuardedWhy,
			CrossConcurrent:    a.CrossConcurrent,
			CrossConcurrentWhy: a.CrossConcurrentWhy,
		})
	}
	return rows
}

// returnSubGraph returns a SubGraph in the requested format (JSON, compact, GCX, or TOON).
// Method on Server so the format negotiation can consult per-session
// client identity (claude-code → gcx, etc.).
func (s *Server) returnSubGraph(ctx context.Context, req mcp.CallToolRequest, sg *query.SubGraph) (*mcp.CallToolResult, error) {
	// Decorate nodes with absolute paths once, up front, so every output
	// format below surfaces an openable path. The canonical graph nodes
	// are copied, never mutated.
	sg.Nodes = s.withAbsPaths(ctx, sg.Nodes)
	// Diagram formats render the subgraph directly — one place serves
	// every traversal tool (callers/dependencies/usages/...), so a
	// `gortex query ... --format mermaid|dot` gets a real diagram.
	// Every text renderer below keeps the same byte/token budget the
	// JSON and GCX paths enforce: the budget is a contract on the
	// response, not on one format. Structured grammars re-render over
	// fewer rows instead of taking a byte-level cut — a sliced diagram
	// or TOON table fails downstream parsers while looking successful.
	switch req.GetString("format", "") {
	case "mermaid":
		return mcp.NewToolResultText(renderSubGraphWithinBudget(req, sg, func(t *query.SubGraph) string {
			text := t.ToMermaid()
			if note := subGraphBudgetNote(t, sg); note != "" {
				text += "%% " + note + "\n"
			}
			return text
		})), nil
	case "dot":
		return mcp.NewToolResultText(renderSubGraphWithinBudget(req, sg, func(t *query.SubGraph) string {
			text := t.ToDot()
			if note := subGraphBudgetNote(t, sg); note != "" {
				text += "// " + note + "\n"
			}
			return text
		})), nil
	}
	if isCompact(req) {
		// Compact is a caller-facing renderer, not an escape hatch from
		// the byte/token budget: a request routed here (including
		// compact over a gcx session default) keeps the same
		// effectiveBudget cap every other format path enforces.
		return mcp.NewToolResultText(textWithinBudget(req, compactSubGraph(sg))), nil
	}
	if s.isGCX(ctx, req) {
		tool := requestToolName(req)
		if tool == "" {
			tool = "subgraph"
		}
		return s.gcxResponseWithBudget(req)(encodeSubGraph(tool, sg, s.nodeGetterFor(ctx)))
	}
	if s.isTOON(ctx, req) {
		getter := s.nodeGetterFor(ctx)
		return mcp.NewToolResultText(renderSubGraphWithinBudget(req, sg, func(t *query.SubGraph) string {
			return subGraphToTOON(t, getter)
		})), nil
	}
	return s.respondJSONOrTOON(ctx, req, sg)
}

// trimBudgetMarker closes a budget-trimmed compact payload; package
// level so the boundary tests exercise the exact marker the trimmer
// emits.
const trimBudgetMarker = "... trimmed to byte budget; pass max_bytes:0 for the full result\n"

// trimTextToBudget cuts a line-oriented text payload to at most budget
// bytes, breaking at a line boundary and closing with a marker line so
// the cut is legible. The marker's own bytes count against the budget.
func trimTextToBudget(text string, budget int) string {
	// The budget is a hard ceiling at every size: a non-positive budget
	// yields nothing, a budget below the marker's own size gets a
	// truncated marker, never a payload above it.
	if budget <= 0 {
		return ""
	}
	if budget <= len(trimBudgetMarker) {
		return trimBudgetMarker[:budget]
	}
	keep := budget - len(trimBudgetMarker)
	if keep > len(text) {
		keep = len(text)
	}
	cut := strings.LastIndexByte(text[:keep], '\n')
	if cut < 0 {
		cut = 0
	} else {
		cut++
	}
	return text[:cut] + trimBudgetMarker
}

// textWithinBudget applies the request's effective byte/token budget
// to a rendered text payload, trimming only on overflow — the fitting
// case passes through marker-free.
func textWithinBudget(req mcp.CallToolRequest, text string) string {
	if budget := effectiveBudget(req); budget > 0 && len(text) > budget {
		return trimTextToBudget(text, budget)
	}
	return text
}

// renderSubGraphWithinBudget renders sg through render and, when the
// full rendering overflows the caller's budget, re-renders over the
// largest structural subset that fits, so the output stays
// syntactically valid in grammars a byte-level cut would corrupt.
// Two stages, both stamping Truncated plus the TotalEdges/TotalNodes
// floors on the trial: first the largest edge prefix (nodes pruned to
// the surviving rows), then — when no edge prefix fits, the shape a
// node-heavy result with few or zero edges produces — the largest
// node prefix over the rowless base. The page order each prefix
// consumes is whatever the handler already established. Only the
// degenerate case where even an empty rendering overflows falls back
// to the plain-text trim.
func renderSubGraphWithinBudget(req mcp.CallToolRequest, sg *query.SubGraph, render func(*query.SubGraph) string) string {
	budget := effectiveBudget(req)
	full := render(sg)
	if budget <= 0 || len(full) <= budget {
		return full
	}
	trial := *sg
	trial.Truncated = true
	trial.TotalEdges = max(sg.TotalEdges, len(sg.Edges))
	trial.TotalNodes = max(sg.TotalNodes, len(sg.Nodes))
	// One fit predicate for both stages: the largest prefix count whose
	// rendering fits, with set() mutating the trial to the candidate.
	largestFit := func(n int, set func(int)) string {
		fit := ""
		lo, hi := 0, n
		for lo < hi {
			mid := (lo + hi + 1) / 2
			set(mid)
			if text := render(&trial); len(text) <= budget {
				lo, fit = mid, text
			} else {
				hi = mid - 1
			}
		}
		return fit
	}
	if fit := largestFit(len(sg.Edges), func(m int) {
		trial.Edges = sg.Edges[:m]
		trial.Nodes = nodesForEdgePage(sg, trial.Edges)
	}); fit != "" {
		return fit
	}
	// No edge prefix fits — the shape a node-heavy result with few or
	// zero edges produces. Cut a node prefix over the edge-touched
	// nodes first (the queried subject is incident to its own rows),
	// then the isolated remainder, so the subject survives any cut
	// that keeps at least one node.
	trial.Edges = nil
	baseNodes := edgeIncidentFirst(sg)
	if fit := largestFit(len(baseNodes), func(m int) {
		trial.Nodes = baseNodes[:m]
	}); fit != "" {
		return fit
	}
	trial.Nodes = nil
	if text := render(&trial); len(text) <= budget {
		return text
	}
	return trimTextToBudget(full, budget)
}

// edgeIncidentFirst orders the subgraph's nodes with the ones its
// edges touch ahead of the isolated remainder, keeping the handler's
// relative order within each class. The node-prefix budget cut
// consumes this order, so the result's structurally-connected nodes —
// the queried subject among them — outlive the isolated tail.
func edgeIncidentFirst(sg *query.SubGraph) []*graph.Node {
	incident := make(map[string]bool, len(sg.Edges)*2)
	for _, e := range sg.Edges {
		if e != nil {
			incident[e.From] = true
			incident[e.To] = true
		}
	}
	out := make([]*graph.Node, 0, len(sg.Nodes))
	for _, n := range sg.Nodes {
		if n != nil && incident[n.ID] {
			out = append(out, n)
		}
	}
	for _, n := range sg.Nodes {
		if n != nil && !incident[n.ID] {
			out = append(out, n)
		}
	}
	return out
}

// subGraphBudgetNote names what the BYTE-BUDGET cut left out — edges,
// nodes, or both — in one grammar-neutral sentence the diagram
// renderers wrap in their own comment syntax. The cut is measured
// against base, the row set the handler returned: a handler-side
// `limit` cap also stamps Truncated and pre-cut totals, and blaming
// the byte budget for those rows would send the caller on a
// `max_bytes:0` retry that cannot restore them. The denominators are
// base's row counts for the same reason — they are what max_bytes:0
// actually returns. Empty when the rendering was not byte-cut.
func subGraphBudgetNote(t, base *query.SubGraph) string {
	var parts []string
	if len(t.Edges) < len(base.Edges) {
		parts = append(parts, fmt.Sprintf("%d of %d edges", len(t.Edges), len(base.Edges)))
	}
	if len(t.Nodes) < len(base.Nodes) {
		parts = append(parts, fmt.Sprintf("%d of %d nodes", len(t.Nodes), len(base.Nodes)))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ") + " shown (byte budget); pass max_bytes:0 for the full graph"
}

// nodesForEdgePage keeps the nodes a trimmed edge page references,
// plus every node with no incident edge in the full result (the
// queried seed among them), so the page cut never drops the subject.
func nodesForEdgePage(sg *query.SubGraph, page []*graph.Edge) []*graph.Node {
	incident := make(map[string]bool, len(sg.Edges)*2)
	for _, e := range sg.Edges {
		if e != nil {
			incident[e.From] = true
			incident[e.To] = true
		}
	}
	keep := make(map[string]bool, len(page)*2)
	for _, e := range page {
		if e != nil {
			keep[e.From] = true
			keep[e.To] = true
		}
	}
	out := make([]*graph.Node, 0, len(page)*2)
	for _, n := range sg.Nodes {
		if n == nil {
			continue
		}
		if keep[n.ID] || !incident[n.ID] {
			out = append(out, n)
		}
	}
	return out
}

// requestToolName extracts the MCP tool name from a CallToolRequest.
// mcp-go surfaces the name on req.Params.Name so we can route multiple
// subgraph-returning tools through the same encoder with distinct
// header tags.
func requestToolName(req mcp.CallToolRequest) string {
	return req.Params.Name
}

// returnTOON marshals payload as TOON and returns a text result. It
// goes JSON-first so the on-wire field names match the JSON schema
// every tool already advertises: toon-go honours only `toon:` tags
// and rejects map[int]X / non-string keys outright, but every Gortex
// payload tags its fields with `json:` (we don't double-tag with
// `toon:`). Round-tripping through JSON gives us tag-driven naming
// and string-key normalisation (Go's encoding/json stringifies int
// keys) for free, with a single allocation we can amortise across
// the tool surface.
//
// Falls back to JSON on encoder error so a malformed payload can
// never take down the response — the caller never sees a half-
// written document.
func returnTOON(payload any) (*mcp.CallToolResult, error) {
	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return mcp.NewToolResultJSON(payload)
	}
	var generic any
	if err := json.Unmarshal(jsonBytes, &generic); err != nil {
		return mcp.NewToolResultJSON(payload)
	}
	data, err := toon.Marshal(generic)
	if err != nil {
		return mcp.NewToolResultJSON(payload)
	}
	return mcp.NewToolResultText(string(data)), nil
}

// respondJSONOrTOON is the bottom-of-the-handler decision shared by
// every tool that advertises `format` in its schema and lets a
// per-tool GCX encoder run ahead of it. It returns TOON when the
// caller (or the per-session default) asks for it and JSON otherwise.
// GCX is handled inline at the call site because GCX uses hand-tuned
// per-tool encoders rather than reusing the JSON shape.
//
// Three pipeline stages run before the format encoder:
//
//  1. Sparse-fieldsets filter: when the caller passes
//     `fields: "id,line"`, list rows are projected down to those keys.
//     Trims response size at the row level.
//  2. Graceful degradation: tools that registered a per-shape policy
//     (`get_file_summary`, `get_editing_context`, `find_usages`, …)
//     run a cascade — strip verbose meta, drop low-priority kinds,
//     last-resort tail-trim. Quality stays high under pressure.
//  3. Generic budget: tools without a registered shape fall back to
//     a "trim the longest list" heuristic. Always emits inline data
//     with `_truncated_by_budget` metadata; never falls through to
//     a transport spill that the agent has to re-read from disk.
//
// effectiveBudget defaults to defaultMaxBytes when the caller does
// not specify; pass `max_bytes: 0` to opt out of budgeting and get
// the full result in one shot (transport spill if oversized).
func (s *Server) respondJSONOrTOON(ctx context.Context, req mcp.CallToolRequest, payload any) (*mcp.CallToolResult, error) {
	payload = applyFieldsFilter(payload, parseFields(req.GetString("fields", "")))
	if budget := effectiveBudget(req); budget > 0 {
		// The token-budget decoration lands after the fit; reserve its
		// bytes so the decorated payload stays in cap. A budget the
		// reserve itself cannot fit inside drops the decoration instead
		// of letting it become the bytes that break the contract.
		reserve := tokenBudgetDecorationReserve(req)
		decorate := reserve == 0 || reserve < budget
		if reserve > 0 && decorate {
			budget -= reserve
		}
		var trimmed bool
		if shape, ok := degradeShapes[req.Params.Name]; ok {
			payload, trimmed = applyDegradation(payload, shape, budget)
		} else {
			payload, trimmed = applyBudget(payload, budget)
		}
		if trimmed && decorate {
			payload = decorateTokenBudgetJSON(payload, req)
		}
	}
	// TOON is the right fallback whenever the caller (or the
	// per-session default) asked for a compact format. That covers
	// two cases:
	//
	//  1. Explicit `format=toon` — return TOON.
	//  2. Session default is gcx but this tool does not have a
	//     hand-tuned GCX encoder (status-shape tools like graph_stats
	//     / index_health / list_repos go through this path). Falling
	//     back to TOON instead of JSON keeps the response compact —
	//     ~10–15% smaller than JSON for typical payloads — without
	//     forcing every status tool to ship a bespoke GCX encoder.
	//
	// Plain JSON is still the answer when neither toon nor gcx was
	// requested (unknown clients, explicit `format=json`).
	if s.isTOON(ctx, req) || s.isGCX(ctx, req) {
		return returnTOON(payload)
	}
	return mcp.NewToolResultJSON(payload)
}

// subGraphToTOON renders a SubGraph as TOON text; a marshal failure
// falls back to the JSON rendering so the caller always holds one
// parseable document.
func subGraphToTOON(sg *query.SubGraph, g graph.NodeGetter) string {
	var edgeRows []toonEdgeRow
	for _, e := range sg.Edges {
		label := e.ConfidenceLabel
		if label == "" {
			label = graph.ConfidenceLabelFor(e.Kind, e.Confidence)
		}
		tier := e.Tier
		if tier == "" {
			tier = graph.ResolvedBy(e.Origin)
		}
		edgeRows = append(edgeRows, toonEdgeRow{
			From:       e.From,
			To:         e.To,
			Kind:       string(e.Kind),
			Origin:     e.Origin,
			Tier:       tier,
			Confidence: e.Confidence,
			Label:      label,
		})
	}
	nodeRows := nodesToTOONRows(sg.Nodes)
	// Subgraph rows ride next to the exclude_tests filter, so their
	// is_test column uses the same shared classifier (stamp → owner →
	// path); pure listing tools keep the raw stamp. Joined by node ID —
	// the row builder skips File/Import nodes, so row positions do not
	// line up with sg.Nodes positions.
	nodeByID := indexNodes(sg.Nodes)
	for i := range nodeRows {
		if n := nodeByID[nodeRows[i].ID]; n != nil {
			nodeRows[i].IsTest = graph.NodeIsTest(g, n)
		}
	}
	result := toonSubGraphResult{
		Nodes:                 nodeRows,
		Edges:                 edgeRows,
		Total:                 sg.TotalNodes,
		Truncated:             sg.Truncated,
		Caveat:                toonCaveat(sg.Caveat),
		TextMatchedSuppressed: sg.TextMatchedSuppressed,
		SuppressionCaveat:     sg.SuppressionCaveat,
		RelatedTools:          sg.RelatedTools,
		CallerNotes:           callerNotesToTOONRows(sg.CallerNotes),
		UsageSummary:          sg.UsageSummary,
	}
	if sg.TotalEdges > len(sg.Edges) {
		result.TotalEdges = sg.TotalEdges
	}
	data, err := toon.Marshal(result)
	if err != nil {
		if j, jerr := json.Marshal(sg); jerr == nil {
			return string(j)
		}
		return ""
	}
	return string(data)
}

// resolveRepoFilter resolves the optional repo/project/ref params into
// a set of allowed repo prefixes, enforced against the session's
// workspace boundary.
//
// For a workspace-bound session (the daemon socket path) the boundary
// is mandatory and cannot be widened by args: a `workspace` arg may
// only name the session's own workspace, and `repo`/`project`/`ref`
// args are intersected with the workspace so they can only ever
// narrow. With no explicit narrowing the allow-set is every repo in
// the session's workspace — not "all repos".
//
// gitDiffScopes are the diff-selection values the review-family tools
// accept in their `scope` argument; they share the argument name with the
// saved-scope filter and are reserved (never saved-scope names).
var gitDiffScopes = map[string]bool{"unstaged": true, "staged": true, "all": true, "compare": true}

// For an unbound session (embedded stdio / `gortex server
// --workspace` / legacy) it falls back to resolveRepoFilterArgs with
// the active-project default applied. A nil result there still means
// "no filter — all repos".
func (s *Server) resolveRepoFilter(ctx context.Context, req mcp.CallToolRequest) (map[string]bool, error) {
	repo := strings.TrimSpace(req.GetString("repo", ""))
	if repo != "*" {
		if p := s.resolveRepoPrefix(repo); p != "" {
			repo = p
		}
	}
	project := strings.TrimSpace(req.GetString("project", ""))
	ref := strings.TrimSpace(req.GetString("ref", ""))
	workspaceArg := strings.TrimSpace(req.GetString("workspace", ""))

	scopeArg := strings.TrimSpace(req.GetString("scope", ""))
	if gitDiffScopes[scopeArg] {
		scopeArg = ""
	}
	var scopeRepos map[string]bool
	if scopeArg != "" && repo == "" && project == "" && ref == "" {
		sc, ok := s.lookupScope(scopeArg)
		if !ok {
			return nil, fmt.Errorf("unknown scope %q — run list_scopes to see saved scopes, or create one with save_scope", scopeArg)
		}
		scopeRepos = s.scopeRepoSet(sc)
		if len(scopeRepos) == 0 {
			return nil, fmt.Errorf("saved scope %q names no repositories", scopeArg)
		}
	}

	if sessWS, _, bound := s.sessionScope(ctx); bound {
		if workspaceArg != "" && workspaceArg != sessWS {
			return nil, fmt.Errorf(
				"workspace %q is outside the active workspace %q; cross-workspace queries are not permitted",
				workspaceArg, sessWS)
		}
		wsRepos := map[string]bool{}
		if s.multiIndexer != nil {
			wsRepos = s.multiIndexer.ReposInWorkspace(sessWS)
		}
		if scopeRepos != nil {
			intersected := intersectRepoSets(scopeRepos, wsRepos)
			if len(intersected) == 0 {
				return nil, fmt.Errorf(
					"saved scope %q resolves to nothing inside the active workspace %q",
					scopeArg, sessWS)
			}
			return intersected, nil
		}
		if repo == "*" {
			repo = ""
		}
		if repo == "" && project == "" && ref == "" {
			return wsRepos, nil
		}
		narrowed, err := s.resolveRepoFilterArgs(repo, project, ref, false)
		if err != nil {
			return nil, err
		}
		if narrowed == nil {
			return wsRepos, nil
		}
		intersected := intersectRepoSets(narrowed, wsRepos)
		if len(intersected) == 0 {
			return nil, fmt.Errorf(
				"repo/project/ref filter resolves to nothing inside the active workspace %q; cross-workspace queries are not permitted",
				sessWS)
		}
		return intersected, nil
	}

	if scopeRepos != nil {
		return scopeRepos, nil
	}
	if repo == "*" {
		repo = ""
	}
	return s.resolveRepoFilterArgs(repo, project, ref, true)
}

// resolveRepoFilterArgs folds explicit repo/project/ref args into a
// single allow-set of repo prefixes. A nil map means "no filter — all
// repos". When useActiveProjectDefault is true and no axis is given,
// the server's active project is applied as the default scope (the
// legacy single-tenant behaviour); workspace-bound callers pass false
// because their boundary is supplied separately.
//
// An explicit unknown project is a hard error (the caller asked for X
// by name, deserves to know X is wrong); a stale active-project
// default degrades to "no filter" with a warning log instead, so a
// single misconfigured config line does not break every MCP call.
func (s *Server) resolveRepoFilterArgs(repo, project, ref string, useActiveProjectDefault bool) (map[string]bool, error) {
	projectFromActive := false
	if useActiveProjectDefault && repo == "" && project == "" && ref == "" && s.activeProject != "" {
		project = s.activeProject
		projectFromActive = true
	}

	if repo == "" && project == "" && ref == "" {
		return nil, nil // no filter — search all repos
	}

	// Direct repo filter — just that one prefix.
	if repo != "" {
		return map[string]bool{repo: true}, nil
	}

	// Resolve project/ref via ConfigManager.
	if s.configManager == nil {
		return nil, fmt.Errorf("configuration manager is not available")
	}

	gc := s.configManager.Global()

	var entries []config.RepoEntry
	if project != "" {
		repos, err := gc.ResolveRepos(project)
		if err != nil {
			if projectFromActive {
				// Stale active-project default. Log and degrade to no
				// filter (all repos) so the call still succeeds. This
				// mirrors ConfigManager.ActiveRepos behavior.
				if s.logger != nil {
					s.logger.Warn("active project not resolvable, falling back to all repos",
						zap.String("active_project", project),
						zap.Error(err))
				}
				return nil, nil
			}
			return nil, err
		}
		entries = repos
	} else {
		// ref without project — collect all repos from all projects.
		for _, proj := range gc.Projects {
			entries = append(entries, proj.Repos...)
		}
		// Also include top-level repos.
		entries = append(entries, gc.Repos...)
	}

	// Apply ref filter if set.
	allowed := make(map[string]bool)
	for _, e := range entries {
		if ref != "" && e.Ref != ref {
			continue
		}
		allowed[config.ResolvePrefix(e)] = true
	}

	return allowed, nil
}

// repoNarrowAdmits is the single definition of what a repo narrow does to a
// node that carries no repo prefix. Every repo-scoping predicate in the
// server must route through it.
//
// A prefix-less node is owned by no repository, so a narrow keyed on
// registry prefixes can only ever exclude it vacuously. Admitting it is the
// correct reading: synthetic global externals (dep::, external::, non-Go
// module::) model third-party symbols visible from every repo by
// construction, and workspace / project scoping still bounds them.
//
// The alternative — rejecting — does not narrow a result, it empties one.
// That is exactly what the explore resilience ladder used to do: its
// post-filter rejected prefix-less nodes while query/subgraph.go and
// filterNodes admitted them, so a scoped session could relax into the
// unscoped rungs and still be handed nothing.
//
// An empty or nil allow-set means "no narrow" and admits everything.
func repoNarrowAdmits(allowed map[string]bool, repoPrefix string) bool {
	if len(allowed) == 0 {
		return true
	}
	return repoPrefix == "" || allowed[repoPrefix]
}

// filterNodes returns only nodes whose RepoPrefix is in the allowed set.
// If allowed is nil, returns the original slice unchanged.
func filterNodes(nodes []*graph.Node, allowed map[string]bool) []*graph.Node {
	if allowed == nil {
		return nodes
	}
	var out []*graph.Node
	for _, n := range nodes {
		if repoNarrowAdmits(allowed, n.RepoPrefix) {
			out = append(out, n)
		}
	}
	return out
}

func filterNodesByResolvedScope(nodes []*graph.Node, resolved ResolvedScope) []*graph.Node {
	if !resolvedScopeHasFilter(resolved) {
		return nodes
	}
	opts := queryOptionsForResolvedScope(resolved)
	out := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		if n != nil && opts.ScopeAllows(n) {
			out = append(out, n)
		}
	}
	return out
}

func queryOptionsForResolvedScope(resolved ResolvedScope) query.QueryOptions {
	return query.QueryOptions{
		WorkspaceID: resolved.WorkspaceID,
		ProjectID:   resolved.ProjectID,
		RepoAllow:   resolved.RepoAllow,
	}
}

func resolvedScopeHasFilter(resolved ResolvedScope) bool {
	return resolved.WorkspaceID != "" || resolved.ProjectID != "" || len(resolved.RepoAllow) > 0
}

func resolvedScopeAllowsNode(resolved ResolvedScope, n *graph.Node) bool {
	if n == nil {
		return false
	}
	return queryOptionsForResolvedScope(resolved).ScopeAllows(n)
}

func filterSubGraphByResolvedScope(sg *query.SubGraph, resolved ResolvedScope) *query.SubGraph {
	if sg == nil || !resolvedScopeHasFilter(resolved) {
		return sg
	}
	opts := queryOptionsForResolvedScope(resolved)
	nodeIDs := make(map[string]bool, len(sg.Nodes))
	nodes := make([]*graph.Node, 0, len(sg.Nodes))
	for _, n := range sg.Nodes {
		if n != nil && opts.ScopeAllows(n) {
			nodes = append(nodes, n)
			nodeIDs[n.ID] = true
		}
	}
	edges := make([]*graph.Edge, 0, len(sg.Edges))
	for _, e := range sg.Edges {
		if e != nil && nodeIDs[e.From] && nodeIDs[e.To] {
			edges = append(edges, e)
		}
	}
	out := *sg
	out.Nodes = nodes
	out.Edges = edges
	out.TotalNodes = len(nodes)
	out.TotalEdges = len(edges)
	if sg.CallerNotes != nil {
		out.CallerNotes = make(map[string]*graph.ConcurrencyAnnotation, len(sg.CallerNotes))
		for id, ann := range sg.CallerNotes {
			if nodeIDs[id] {
				out.CallerNotes[id] = ann
			}
		}
		if len(out.CallerNotes) == 0 {
			out.CallerNotes = nil
		}
	}
	return &out
}

func filterEdgesByResolvedScope(eng *query.Engine, edges []*graph.Edge, resolved ResolvedScope) []*graph.Edge {
	if eng == nil || !resolvedScopeHasFilter(resolved) {
		return edges
	}
	out := make([]*graph.Edge, 0, len(edges))
	for _, e := range edges {
		if e == nil {
			continue
		}
		if resolvedScopeAllowsNode(resolved, eng.GetSymbol(e.From)) &&
			resolvedScopeAllowsNode(resolved, eng.GetSymbol(e.To)) {
			out = append(out, e)
		}
	}
	return out
}

// filterNodesByKind keeps only nodes whose Kind is in the comma-
// separated list. Empty / unknown kinds in the input are ignored
// (treated as "no constraint of this name") so a typo is graceful
// rather than silently empty. Case-insensitive.
//
// Used by search_symbols' `kind` argument — lets callers scope a
// query to one of the domain-specific node kinds (todo, license,
// team, …) without paying the cost of an unrelated BM25 prefix
// match.
func filterNodesByKind(nodes []*graph.Node, kindArg string) []*graph.Node {
	want := make(map[string]struct{})
	for k := range strings.SplitSeq(kindArg, ",") {
		k = strings.TrimSpace(strings.ToLower(k))
		if k == "" {
			continue
		}
		want[k] = struct{}{}
	}
	if len(want) == 0 {
		return nodes
	}
	out := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		if _, ok := want[strings.ToLower(string(n.Kind))]; ok {
			out = append(out, n)
		}
	}
	return out
}

// filterSubGraph returns a new SubGraph with only nodes/edges whose endpoints
// are in the allowed repo set. If allowed is nil, returns sg unchanged.
func filterSubGraph(sg *query.SubGraph, allowed map[string]bool) *query.SubGraph {
	if allowed == nil {
		return sg
	}
	nodeIDs := make(map[string]bool)
	var nodes []*graph.Node
	for _, n := range sg.Nodes {
		if repoNarrowAdmits(allowed, n.RepoPrefix) {
			nodes = append(nodes, n)
			nodeIDs[n.ID] = true
		}
	}
	var edges []*graph.Edge
	for _, e := range sg.Edges {
		if nodeIDs[e.From] || nodeIDs[e.To] {
			edges = append(edges, e)
		}
	}
	totalEdges := len(edges)
	// Counts-only payloads arrive with Edges == nil and TotalEdges
	// pre-populated — preserve the upstream count instead of zeroing
	// it. Inexact in the presence of a non-trivial filter (we'd need
	// the edges to know which belong to filtered-out nodes), but the
	// gcx output that asks for the count-only path runs with the
	// session's workspace scope already applied at the store, so the
	// filter pass is typically a no-op.
	if len(sg.Edges) == 0 && sg.TotalEdges > 0 {
		totalEdges = sg.TotalEdges
	}
	return &query.SubGraph{
		Nodes:      nodes,
		Edges:      edges,
		TotalNodes: len(nodes),
		TotalEdges: totalEdges,
		Truncated:  sg.Truncated,
	}
}

// compactNodes formats nodes as one-line-per-symbol text.
// Format: "kind qualifiedName file_path:start_line"
// For methods, qualifiedName includes the receiver (e.g., "Indexer.Index")
// so the output can be combined with file_path to reconstruct the full node ID.
func compactNodes(nodes []*graph.Node) string {
	var b strings.Builder
	for _, n := range nodes {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		fmt.Fprintf(&b, "%s %s %s:%d", n.Kind, qualifiedName(n), n.FilePath, n.StartLine)
		// Append the enclosing owner when the node is declared inside
		// one -- a method on a type, a field of a struct, a closure.
		if _, ename := graph.EnclosingFromID(n.ID, n.Kind); ename != "" {
			fmt.Fprintf(&b, " (in %s)", ename)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// qualifiedName returns the symbol part of a node ID (after "::").
// For methods this includes the receiver type (e.g., "Indexer.Index"),
// for functions/types it's the plain name.
func qualifiedName(n *graph.Node) string {
	if idx := strings.LastIndex(n.ID, "::"); idx >= 0 {
		return n.ID[idx+2:]
	}
	return n.Name
}

// enrichSubGraphEdges populates ConfidenceLabel, Origin, and Tier on every
// edge in a SubGraph. Origin is backfilled from kind + confidence +
// semantic_source meta when unset; Tier is the coarse (ast / lsp /
// heuristic) provenance label derived from Origin so clients can filter
// or group without recomputing the mapping.
func enrichSubGraphEdges(sg *query.SubGraph) {
	for _, e := range sg.Edges {
		e.ConfidenceLabel = graph.ConfidenceLabelFor(e.Kind, e.Confidence)
		e.Origin = e.EffectiveOrigin()
		e.Tier = graph.ResolvedBy(e.Origin)
		if e.Meta != nil {
			if v, _ := e.Meta["via"].(string); v != "" {
				e.Via = graph.ViaLabelFor(v)
			}
		}
	}
}

// isNonDefinitionNode reports whether a node kind is NOT a file-level
// definition and should be dropped from a get_file_summary view. It
// excludes the file node itself, imports, and the function-body-internal
// nodes (locals, params, closures, generic params, builtins) that the
// file_path lookup pulls in but that the "symbols a file defines"
// contract never wanted. Without this filter the summary floods with
// hundreds of locals/params (the old defines-edge query excluded them by
// construction; the GetFileNodes-based path does not).
func isNonDefinitionNode(k graph.NodeKind) bool {
	switch k {
	case graph.KindFile, graph.KindImport, graph.KindLocal,
		graph.KindParam, graph.KindClosure, graph.KindGenericParam,
		graph.KindBuiltin:
		return true
	}
	return false
}

// stripNonDefinitionNodes returns a copy of sg with non-definition nodes
// nodes removed (and edges that reference them dropped). Used by
// handleGetFileSummary to keep its output focused on the symbols a
// file *defines* — the file node and per-statement import nodes are
// useful internals (e.g. for the file-neighbourhood walk that drives
// the disk-backend pushdown) but noise in the agent-visible payload.
func stripNonDefinitionNodes(sg *query.SubGraph) *query.SubGraph {
	if sg == nil {
		return nil
	}
	keep := make(map[string]bool, len(sg.Nodes))
	nodes := make([]*graph.Node, 0, len(sg.Nodes))
	for _, n := range sg.Nodes {
		if n == nil || isNonDefinitionNode(n.Kind) {
			continue
		}
		nodes = append(nodes, n)
		keep[n.ID] = true
	}
	edges := make([]*graph.Edge, 0, len(sg.Edges))
	for _, e := range sg.Edges {
		if e == nil || !keep[e.From] || !keep[e.To] {
			continue
		}
		edges = append(edges, e)
	}
	totalEdges := len(edges)
	// Counts-only payloads arrive with Edges == nil and TotalEdges
	// already populated by the store. Keep that count — the file +
	// import nodes we're stripping pulled some edges with them so it's
	// a slight overcount, but the gcx callers that take this path
	// only render it as a header scalar, not as anything load-bearing.
	if len(sg.Edges) == 0 && sg.TotalEdges > 0 {
		totalEdges = sg.TotalEdges
	}
	return &query.SubGraph{
		Nodes:       nodes,
		Edges:       edges,
		TotalNodes:  len(nodes),
		TotalEdges:  totalEdges,
		Truncated:   sg.Truncated,
		CallerNotes: sg.CallerNotes,
	}
}

// compactSubGraph formats a SubGraph as compact text.
func compactSubGraph(sg *query.SubGraph) string {
	var b strings.Builder
	for _, n := range sg.Nodes {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		fmt.Fprintf(&b, "%s %s %s:%d\n", n.Kind, qualifiedName(n), n.FilePath, n.StartLine)
	}
	if sg.Truncated {
		fmt.Fprintf(&b, "... truncated (%d total)\n", sg.TotalNodes)
	}
	// Append edge confidence distribution.
	if len(sg.Edges) > 0 {
		counts := map[string]int{}
		for _, e := range sg.Edges {
			label := e.ConfidenceLabel
			if label == "" {
				label = graph.ConfidenceLabelFor(e.Kind, e.Confidence)
			}
			counts[label]++
		}
		// A row-capped page names both the page size and the full row
		// count, so "total" never mislabels a partial result. The
		// uncapped footer keeps its wire shape.
		if sg.TotalEdges > len(sg.Edges) {
			fmt.Fprintf(&b, "edges: %d of %d total", len(sg.Edges), sg.TotalEdges)
		} else {
			fmt.Fprintf(&b, "edges: %d total", len(sg.Edges))
		}
		for _, label := range []string{"EXTRACTED", "INFERRED", "AMBIGUOUS"} {
			if c := counts[label]; c > 0 {
				fmt.Fprintf(&b, ", %d %s", c, label)
			}
		}
		b.WriteByte('\n')
	}
	// Append concurrency annotations for callers that carry one. Sorted
	// by node ID so the compact output is deterministic.
	if len(sg.CallerNotes) > 0 {
		ids := make([]string, 0, len(sg.CallerNotes))
		for id := range sg.CallerNotes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			a := sg.CallerNotes[id]
			fmt.Fprintf(&b, "concurrency %s:", id)
			if a.SyncGuarded {
				b.WriteString(" sync_guarded")
			}
			if a.CrossConcurrent {
				b.WriteString(" cross_concurrent")
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (s *Server) registerCoreTools() {
	s.addTool(
		mcp.NewTool("index_repository",
			mcp.WithDescription("Index or re-index a local repository path into Gortex. Call once at session start if not already running with --watch."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Absolute path to repository")),
		),
		s.handleIndexRepository,
	)

	s.addTool(
		mcp.NewTool("reindex_repository",
			mcp.WithDescription("Incrementally re-index a repository: re-parses only the files that changed since the last index pass (mtime, or content under Merkle mode) and evicts nodes for deleted files. Much cheaper than index_repository, which rebuilds the whole graph. Pass `paths` to scope the pass to specific files or directories; omit it to scan the whole repository. Returns what was reindexed plus node/edge counts, the stale-file count, and timing."),
			mcp.WithString("path", mcp.Description("Absolute path to the repository, or a tracked repo prefix. Defaults to the active repository.")),
			mcp.WithArray("paths",
				mcp.Description("Optional list of file or directory paths to scope the reindex to — absolute, or relative to the repository root. Directories are walked recursively. When omitted, the whole repository root is scanned."),
				mcp.WithStringItems(),
			),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
		),
		s.handleReindexRepository,
	)

	s.addTool(
		mcp.NewTool("get_symbol",
			mcp.WithDescription("Use instead of Read to locate a function, type, interface, or variable definition. Returns location and signature without reading the whole file."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Node ID (e.g. pkg/server.go::HandleRequest)")),
			mcp.WithString("detail", mcp.Description("brief or full (default: brief)")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("workspace", mcp.Description("Restrict results to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — restricts results to that scope's repositories. Ignored when an explicit repo / project / ref is also given.")),
		),
		s.handleGetSymbol,
	)

	s.addTool(
		mcp.NewTool("search_symbols",
			mcp.WithDescription("Use instead of Grep to find symbols across the whole codebase. Supports natural language queries with camelCase-aware tokenization and BM25 ranking — 'validate token auth' finds validateToken, AuthMiddleware, parseJWT. Localizing a whole task or bug report (not one symbol)? explore returns the ranked neighborhood with source + call paths in one call. For the concrete types that implement an interface, or the methods that override a method, use find_implementations / find_overrides — a name search alone can't tell an implementor from an unrelated same-named symbol."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Search query — symbol name, concept, or keywords. Also accepts inline field-qualified clauses: `kind:function lang:go path:internal/ repo:gortex project:web validateToken` — recognised fields are kind, flavor, lang (aliases ts/js/py/rs/…), path, repo, project; everything else is free text. A field-qualified query that matches nothing retries on the free text alone (response carries `filters_relaxed: true`).")),
			mcp.WithNumber("limit", mcp.Description("Max results (default: 20)")),
			mcp.WithString("cursor", mcp.Description("Opaque pagination cursor returned in `next_cursor` from a previous call. Pass it back to fetch the next page. Omit for the first page.")),
			mcp.WithBoolean("paginate", mcp.Description("When true, the server caps each page at the project default budget and returns `next_cursor` for any tail. Implies the caller will follow `next_cursor` to walk the rest. Default false (full result inline; transport spills to disk if oversized).")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed and `_truncated_by_budget` plus `_max_returned_<key>` / `_original_count_<key>` flags ride on the response. Omit for no cap.")),
			mcp.WithString("fields", mcp.Description("Comma-separated list of fields to keep on each result (e.g. \"id,name,line\"). Drops the rest to save tokens.")),
			mcp.WithBoolean("compact", mcp.Description("Return one-line-per-result text instead of JSON objects (saves 50-70% tokens)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format — round-trippable, ~40% fewer tokens), or toon")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("workspace", mcp.Description("Restrict results to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — restricts results to that scope's repositories. Ignored when an explicit repo / project / ref is also given.")),
			mcp.WithString("path", mcp.Description("Restrict results to one or more sub-paths (comma-separated) -- the monorepo-service slice (e.g. \"services/billing,libs/auth\"). Anchored, slash-segment-boundary prefixes relative to the repo root: \"services/billing\" matches services/billing/x.go, not other/services/billingX. Unions with any inline path: clause and a scope's saved paths.")),
			mcp.WithString("kind", mcp.Description("Filter to one or more node kinds (comma-separated). Standard kinds: function, method, type, interface, variable, constant, field, file, package, import, contract. Coverage kinds: param, closure, enum_member, generic_param, module, table, column, config_key, flag, event, migration, fixture, todo, team, license, release, doc (Markdown prose section).")),
			mcp.WithString("flavor", mcp.Description("Filter to one or more structural flavors (comma-separated, union). Values: class, struct, enum, interface, trait, protocol, object, record, type_alias, newtype, message, service, table, view, module, signature, type_def, instance, hook, play — matched against Meta[\"type_flavor\"] case-insensitively. The special value `component` spans both keys: it matches any UI component node (React / Vue / Svelte / SwiftUI / …). ANDs with kind:.")),
			mcp.WithString("assist", mcp.Description("LLM assist mode: \"auto\" (default — engages on natural-language queries, skips identifier lookups), \"on\" (force engage), \"off\" (bypass), \"deep\" (on + a body-grounded verification pass that reads candidate code and HONESTLY drops irrelevant matches — slower, may return empty results when nothing genuinely matches). Requires an LLM provider configured via `llm.provider` (local / anthropic / openai / azure / ollama / claudecli / codex / copilot / cursor / opencode / gemini / bedrock / deepseek, or a registered custom provider); behaves as \"off\" when none is available.")),
			mcp.WithBoolean("debug", mcp.Description("When true, attach a `rerank` block to the response carrying per-candidate scores and per-signal contributions from the 11-signal rerank pipeline (bm25, semantic, fan_in, hits, fan_out, churn, community, minhash, api_signature, type_signature, recency, feedback) plus the active per-signal weight map. Off by default; enable to inspect ranking decisions or tune `.gortex.yaml::search::weights`.")),
			mcp.WithString("query_class", mcp.Description("Advisory hint that tunes the bm25-vs-semantic balance of the rerank: \"auto\" (default — detect from query shape), \"symbol\" (identifier / API lookup — BM25-heavy), \"concept\" (natural-language description — balanced), \"path\" (file-path query — most BM25-heavy), \"signature\" (type/function-signature fragment — BM25-leaning), \"keyword_soup\" (a degenerate boolean OR-list \u2014 suppresses LLM expansion and splits the soup into per-disjunct BM25 fetches; a `query_advice` nudge rides on the response). The class actually used is echoed back as `query_class` in the response.")),
			mcp.WithString("expand", mcp.Description("Query-expansion channels: \"both\" (default \u2014 LLM expansion when the assist gate engages, plus the deterministic equivalence-class table), \"equivalence\" (only the LLM-free curated synonym table + per-repo auto-mined concepts), \"llm\" (only LLM expansion), \"off\" (pure BM25, no expansion). Equivalence expansion bridges query vocabulary to the words a symbol uses (auth->login, delete->remove) and runs even with no LLM provider configured. For identifier queries (query_class symbol / path / signature) the server auto-disables expansion + vector even when expand is set \u2014 these classes match best on BM25 + exact-name alone.")),
			mcp.WithString("corpus", mcp.Description("Which corpus to search: \"code\" (default \u2014 code symbols only), \"docs\" (only Markdown prose-section nodes), \"content\" (only the pdf / office / text content chunks, data_class=content), \"all\" (code + all prose). With docs / content / all a prose query matches by body text.")),
			mcp.WithBoolean("vocab_anchored", mcp.Description("When true, the LLM query-expander's returned synonyms are post-filtered to the words that actually appear in this repo's symbol names before they feed the BM25 OR-merge -- so a hallucinated-but-plausible term can't dilute the candidate pool. Robust and model-agnostic. Degrades to unconstrained expansion when the repo's mined vocabulary is empty (e.g. a cold index). Default false; the server may set a different default via search.vocab_anchored_expansion. No effect when the LLM expansion channel is off.")),
			mcp.WithNumber("max_per_file", mcp.Description("Cap how many results a single source file may contribute to the diverse head of the result set (default 3). Hits beyond the cap are demoted below not-yet-capped results — never dropped — so the top of the list spans more files. Set 0 to disable diversification.")),
		),
		s.handleSearchSymbols,
	)

	s.addTool(
		mcp.NewTool("get_file_summary",
			mcp.WithDescription("Use instead of Read to understand a file's role: returns the symbols a file defines (functions, methods, types, fields, …) without reading source lines. The file node itself and import nodes are excluded — use find_import_path or get_dependencies for import-shape queries."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Relative file path")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithNumber("max_tokens", mcp.Description(tokenBudgetParamDescription)),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("workspace", mcp.Description("Restrict results to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — restricts results to that scope's repositories. Ignored when an explicit repo / project / ref is also given.")),
			mcp.WithString("if_none_match", mcp.Description("ETag from a previous response — returns not_modified if content unchanged")),
		),
		s.handleGetFileSummary,
	)

	s.addTool(
		mcp.NewTool("get_dependencies",
			mcp.WithDescription("Returns what a symbol or file depends on — imports, calls, type references — without reading any files. Use before editing to understand incoming contracts."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Node ID")),
			mcp.WithNumber("depth", mcp.Description("Traversal depth (default: 2)")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("workspace", mcp.Description("Restrict results to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — restricts results to that scope's repositories. Ignored when an explicit repo / project / ref is also given.")),
			mcp.WithString("min_tier", mcp.Description(minTierParamDescription)),
			mcp.WithBoolean("include_speculative", mcp.Description(includeSpeculativeParamDescription)),
		),
		s.handleGetDependencies,
	)

	s.addTool(
		mcp.NewTool("get_dependents",
			mcp.WithDescription("Returns everything that depends on this symbol (blast radius). Call before changing a function or type to know what else must be updated."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Node ID")),
			mcp.WithNumber("depth", mcp.Description("Traversal depth (default: 3)")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("workspace", mcp.Description("Restrict results to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — restricts results to that scope's repositories. Ignored when an explicit repo / project / ref is also given.")),
			mcp.WithString("min_tier", mcp.Description(minTierParamDescription)),
			mcp.WithBoolean("include_speculative", mcp.Description(includeSpeculativeParamDescription)),
		),
		s.handleGetDependents,
	)

	s.addTool(
		mcp.NewTool("get_call_chain",
			mcp.WithDescription("Traces the call graph forward from a function without reading source. Use to understand what a function ultimately triggers."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Function node ID")),
			mcp.WithNumber("depth", mcp.Description("Traversal depth (default: 4)")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithNumber("max_tokens", mcp.Description(tokenBudgetParamDescription)),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("workspace", mcp.Description("Restrict results to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — restricts results to that scope's repositories. Ignored when an explicit repo / project / ref is also given.")),
			mcp.WithString("min_tier", mcp.Description(minTierParamDescription)),
			mcp.WithBoolean("include_speculative", mcp.Description(includeSpeculativeParamDescription)),
		),
		s.handleGetCallChain,
	)

	s.addTool(
		mcp.NewTool("get_callers",
			mcp.WithDescription("Returns all callers of a function without reading source. Use instead of Grep when you need to know who calls a function. For every reference — not just call sites, but type uses, field access, and imports — use find_usages."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Function node ID")),
			mcp.WithNumber("depth", mcp.Description("Traversal depth (default: 2)")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("workspace", mcp.Description("Restrict results to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — restricts results to that scope's repositories. Ignored when an explicit repo / project / ref is also given.")),
			mcp.WithString("min_tier", mcp.Description(minTierParamDescription)),
			mcp.WithBoolean("include_speculative", mcp.Description(includeSpeculativeParamDescription)),
			mcp.WithBoolean("exclude_tests", mcp.Description("Drop callers originating in test functions (set true when you want production callers only)")),
		),
		s.handleGetCallers,
	)

	s.addTool(
		mcp.NewTool("find_implementations",
			mcp.WithDescription("Finds all concrete types that implement an interface — 'what implements X', interface satisfaction, which structs satisfy this contract. Use before changing an interface to identify all types that will be affected. For method-level overrides (which methods override this one, or the parents it overrides) use find_overrides."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Interface node ID")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("workspace", mcp.Description("Restrict results to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — restricts results to that scope's repositories. Ignored when an explicit repo / project / ref is also given.")),
			mcp.WithString("min_tier", mcp.Description(minTierParamDescription)),
			mcp.WithBoolean("include_speculative", mcp.Description(includeSpeculativeParamDescription)),
		),
		s.handleFindImplementations,
	)

	s.addTool(
		mcp.NewTool("find_overrides",
			mcp.WithDescription("Finds all methods that override the given method (children) or the parent methods it overrides. Backed by EdgeOverrides materialised at index time and promoted to lsp_dispatch when an LSP is available."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Method node ID")),
			mcp.WithString("direction", mcp.Description("'children' (default — overriders) or 'parents' (overridden methods)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("workspace", mcp.Description("Restrict results to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — restricts results to that scope's repositories. Ignored when an explicit repo / project / ref is also given.")),
			mcp.WithString("min_tier", mcp.Description(minTierParamDescription)),
			mcp.WithBoolean("include_speculative", mcp.Description(includeSpeculativeParamDescription)),
		),
		s.handleFindOverrides,
	)

	s.addTool(
		mcp.NewTool("get_class_hierarchy",
			mcp.WithDescription("Returns the inheritance subgraph around a type, interface, or method. Walks EdgeExtends + EdgeImplements + EdgeComposes for type nodes and EdgeOverrides for method nodes — the same graph data find_implementations and find_overrides expose, but as a multi-hop tree so an agent gets the whole chain (parents → root, children → leaves) in one call. Use before refactoring an OO hierarchy or to answer 'what does this class inherit from / who subclasses it'."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Seed node ID — a type, interface, or method")),
			mcp.WithString("direction", mcp.Description("'up' (parents/interfaces this extends or implements; methods this overrides), 'down' (subtypes / implementers / overriders), or 'both' (default)")),
			mcp.WithNumber("depth", mcp.Description("Max hops to walk in each direction (default: 5, hard cap: 64)")),
			mcp.WithBoolean("include_methods", mcp.Description("When true and the seed/visited node is a type or interface, also include its methods (via EdgeMemberOf) and walk the EdgeOverrides chain rooted at each method.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("workspace", mcp.Description("Restrict results to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — restricts results to that scope's repositories. Ignored when an explicit repo / project / ref is also given.")),
			mcp.WithString("min_tier", mcp.Description(minTierParamDescription)),
			mcp.WithBoolean("include_speculative", mcp.Description(includeSpeculativeParamDescription)),
		),
		s.handleGetClassHierarchy,
	)

	s.addTool(
		mcp.NewTool("find_usages",
			mcp.WithDescription("Use instead of Grep to find every reference to a symbol across the codebase. Returns precise locations with zero false positives. For just the call sites of a function use get_callers; for the concrete types that implement an interface use find_implementations."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Node ID")),
			mcp.WithNumber("limit", mcp.Description("Max usage rows returned (default: 50; 0 = no cap). A capped response is marked truncated and total_edges keeps the full count.")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithNumber("max_tokens", mcp.Description(tokenBudgetParamDescription)),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("workspace", mcp.Description("Restrict results to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — restricts results to that scope's repositories. Ignored when an explicit repo / project / ref is also given.")),
			mcp.WithString("min_tier", mcp.Description(minTierParamDescription)),
			mcp.WithBoolean("include_speculative", mcp.Description(includeSpeculativeParamDescription)),
			mcp.WithBoolean("exclude_tests", mcp.Description("Drop references originating in test functions (set true to see only production usages)")),
			mcp.WithString("group_by", mcp.Description("Set to \"file\" to bucket the usages by the file each reference originates in -- each group carries the per-file use count and the enclosing symbol of every reference. Omit for the default flat result.")),
			mcp.WithString("context", mcp.Description("Filter usages by their reference context — the role the symbol plays at each site: parameter_type, return_type, field, value, type, attribute, generic_arg, call, or import (import / re-export statement lines). Every returned usage also carries its classified context. Omit for all usages.")),
			mcp.WithString("return_usage", mcp.Description("Filter call-site usages by how they consume the callee's return value: discarded, assigned, partially_ignored, returned, goroutine, deferred, argument, or condition. Every returned call usage also carries its classification when the extractor recorded one. Use before changing a function's return signature to see who actually uses the return. Omit for all usages.")),
			mcp.WithString("flavor", mcp.Description("Filter usages by the structural flavor of where they originate (comma-separated, union). A type flavor (class, struct, enum, interface, trait, …) keeps usages whose FROM site sits inside an enclosing type of that flavor — \"usages from inside a struct\". The special value `component` keeps usages from inside a UI component (React / Vue / SwiftUI / …). Each returned usage carries the resolved from_type_flavor / from_ui_component.")),
		),
		s.handleFindUsages,
	)

	s.addTool(
		mcp.NewTool("get_cluster",
			mcp.WithDescription("Returns the immediate neighbourhood around a node — all symbols it touches and that touch it. Useful for understanding a module's coupling before refactoring."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Node ID")),
			mcp.WithNumber("radius", mcp.Description("Bidirectional hops (default: 2)")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("workspace", mcp.Description("Restrict results to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — restricts results to that scope's repositories. Ignored when an explicit repo / project / ref is also given.")),
		),
		s.handleGetCluster,
	)

	s.addTool(
		mcp.NewTool("get_repo_outline",
			mcp.WithDescription("Narrative single-call overview of the indexed codebase: primary languages, top communities, load-bearing hotspots, most-imported files, and entry points. Use at session start (or when onboarding to an unfamiliar repo) instead of assembling this from graph_stats + analyze + manual inspection. Output stays under ~1k tokens."),
			mcp.WithString("format", mcp.Description("Output format: json (default) or toon. gcx is accepted but honoured as toon — get_repo_outline is a status-shape payload with no row-shape gain from a hand-tuned GCX encoder.")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleGetRepoOutline,
	)

	s.addTool(
		mcp.NewTool("graph_stats",
			mcp.WithDescription("Returns a compact summary of the indexed codebase: node/edge counts by kind and language. Call at session start to orient Claude in an unfamiliar repo."),
			mcp.WithString("format", mcp.Description("Output format: json (default) or toon. gcx is accepted but honoured as toon — graph_stats is a status-shape payload with no row-shape gain from a hand-tuned GCX encoder.")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleGraphStats,
	)
}

func (s *Server) handleIndexRepository(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}

	// In multi-repo mode, route through multiIndexer so nodes get the correct
	// RepoPrefix and byRepo stays consistent. Using the shared singleton
	// indexer here produces unprefixed nodes that corrupt per-repo stats.
	if s.multiIndexer != nil {
		// Accept either a tracked prefix directly or a filesystem path.
		// Falls back to reconciling from persisted config so users don't
		// have to re-track repos the daemon dropped across warmup (T0.3).
		prefix := s.resolveRepoPrefixOrReconcile(ctx, path)
		if prefix == "" {
			return mcp.NewToolResultError(fmt.Sprintf(
				"path %q is not a tracked repository; use track_repository to add it",
				path)), nil
		}
		result, err := s.multiIndexer.IndexRepo(prefix)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		s.RunAnalysis()
		s.recordIndexTelemetry(result.FileCount)
		return s.respondJSONOrTOON(ctx, req, result)
	}

	result, err := s.indexer.IndexCtx(s.progressCtx(ctx, req), path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	s.RunAnalysis()
	s.recordIndexTelemetry(result.FileCount)
	return s.respondJSONOrTOON(ctx, req, result)
}

// recordIndexTelemetry records one completed full-index pass to opt-in usage
// telemetry: a coarse file-count bucket plus a per-language counter for each
// language in the graph. Gated on an enabled recorder so a disabled / absent
// recorder costs nothing — in particular it does not walk the graph for the
// language set.
func (s *Server) recordIndexTelemetry(fileCount int) {
	if s.recorder == nil || !s.recorder.Enabled() {
		return
	}
	var langs []string
	if s.graph != nil {
		for lang := range s.graph.Stats().ByLanguage {
			langs = append(langs, lang)
		}
		sort.Strings(langs)
	}
	telemetry.RecordIndex(s.recorder, fileCount, langs)
}

// normalizeReindexPaths removes blank entries from a scoped reindex request.
// Omitted, empty, and all-blank path lists all preserve the established
// whole-repository meaning. Mixed lists retain only their non-blank scopes.
func normalizeReindexPaths(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	cleaned := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.TrimSpace(p) != "" {
			cleaned = append(cleaned, p)
		}
	}
	if len(cleaned) == 0 {
		return nil, nil
	}
	return cleaned, nil
}

// handleReindexRepository implements the reindex_repository tool: an
// incremental re-index that re-parses only changed files (and evicts
// deleted ones) instead of rebuilding the whole graph. The optional
// `paths` argument scopes the pass to specific files / directories.
func (s *Server) handleReindexRepository(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	paths, pathsErr := normalizeReindexPaths(req.GetStringSlice("paths", nil))
	if pathsErr != nil {
		return mcp.NewToolResultError(pathsErr.Error()), nil
	}

	pathArg := req.GetString("path", "")
	if control := checkoutControlFromContext(ctx); control != nil {
		if err := control.validateReindexPaths(pathArg, paths); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if control.CheckoutScoped {
			return s.handleCheckoutControlReindex(ctx, req, control, paths)
		}
	}

	var (
		result      *indexer.IndexResult
		err         error
		reindexRoot string
		// eligible is snapshotted BEFORE the pass runs: only receipts that
		// had already failed when the indexer read the file may be resolved
		// by it. A mutation that lands mid-pass, fails its own ingest and
		// completes before this handler returns would otherwise be stamped
		// fresh, certifying bytes the graph never read. Using pass start as
		// the cutoff declines some safe resolutions, which is the right way
		// to be wrong here.
		eligible map[string]struct{}
	)

	// Multi-repo mode: route through the per-repo indexer so nodes keep
	// their RepoPrefix and per-repo stats stay consistent.
	if s.multiIndexer != nil {
		prefix := ""
		if pathArg != "" {
			prefix = s.resolveRepoPrefixOrReconcile(ctx, pathArg)
			if prefix == "" {
				return mcp.NewToolResultError(fmt.Sprintf(
					"path %q is not a tracked repository; use track_repository to add it", pathArg)), nil
			}
		} else {
			// No path given — fall back to the session's bound repo,
			// then to the sole tracked repo when there is exactly one.
			prefix, _ = s.sessionLocality(ctx)
			if prefix == "" {
				if tracked := s.multiIndexer.RepoPrefixes(); len(tracked) == 1 {
					prefix = tracked[0]
				}
			}
			if prefix == "" {
				return mcp.NewToolResultError(
					"reindex_repository: no repository specified and the active repository is ambiguous; pass `path`"), nil
			}
		}
		reindexRoot, _ = s.multiIndexer.RepoRoot(prefix)
		if scopeErr := s.repoPrefixInSessionScope(ctx, prefix, prefix); scopeErr != nil {
			return mcp.NewToolResultError(scopeErr.Error()), nil
		}
		eligible = s.failedReceiptsBefore(paths, reindexRoot)
		result, err = s.multiIndexer.IncrementalReindexRepo(prefix, paths)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	} else {
		if s.indexer == nil {
			return mcp.NewToolResultError("reindex_repository: no indexer available"), nil
		}
		root := pathArg
		if root == "" {
			root = s.indexer.RootPath()
		}
		if root == "" {
			return mcp.NewToolResultError(
				"reindex_repository: no repository root known; pass `path`"), nil
		}
		root, absErr := filepath.Abs(root)
		if absErr != nil {
			return mcp.NewToolResultError(absErr.Error()), nil
		}
		reindexRoot = root
		eligible = s.failedReceiptsBefore(paths, reindexRoot)
		result, err = s.indexer.IncrementalReindexPaths(root, paths)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}

	// A successful scoped re-parse is exactly the evidence a terminally failed
	// freshness receipt for that path is waiting for, so clear it here: a
	// failed ingest otherwise fail-closes change.detect for the whole
	// repository until retention lapses, and reindex is the recovery callers
	// reach for first (#692).
	//
	// Deliberately narrow. IncrementalReindexRepo scopes the pass to the given
	// paths, so with exactly one requested path a non-zero StaleFileCount
	// proves that file was re-parsed. With several paths the count cannot be
	// attributed per path, and clearing a receipt for a file that was not
	// re-parsed would be a false green — the very thing the receipt exists to
	// prevent — so anything wider is left alone.
	if resolved, ok := reindexedReceiptPath(paths, reindexRoot, result); ok {
		s.resolveReindexedPathReceipts(resolved, eligible)
	}

	s.RunAnalysis()

	scope := "repository"
	if len(paths) > 0 {
		scope = "paths"
	}
	payload := map[string]any{
		"scope":            scope,
		"result":           result,
		"node_count":       result.NodeCount,
		"edge_count":       result.EdgeCount,
		"file_count":       result.FileCount,
		"stale_file_count": result.StaleFileCount,
		"full_retrack":     result.FullRetrack,
		"duration_ms":      result.DurationMs,
	}
	if len(paths) > 0 {
		payload["reindexed_paths"] = paths
	}
	if len(result.FailedFiles) > 0 {
		payload["failed_files"] = result.FailedFiles
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// reindexCandidatePath resolves the single requested path against the repo
// root. Split out from reindexedReceiptPath so the pre-pass snapshot and the
// post-pass resolution agree on the spelling by construction: if they ever
// disagreed, the snapshot would bound the wrong receipts.
func reindexCandidatePath(paths []string, root string) (string, bool) {
	if len(paths) != 1 {
		return "", false
	}
	resolved := paths[0]
	if !filepath.IsAbs(resolved) {
		if root == "" {
			return "", false
		}
		resolved = filepath.Join(root, resolved)
	}
	return filepath.Clean(resolved), true
}

// reindexedReceiptPath reports the absolute path whose failed freshness
// receipts a reindex pass has earned the right to resolve, if any.
//
// Deliberately narrow. IncrementalReindexRepo scopes the pass to the requested
// paths, so with exactly one requested path a non-zero StaleFileCount proves
// that file was re-parsed. With several paths the count cannot be attributed
// per path, and clearing a receipt for a file that was not re-parsed would be
// a false green — the very thing the receipt exists to prevent — so anything
// wider resolves nothing.
//
// Two properties worth stating because both are fail-closed by construction
// and would be surprising to rediscover: path comparison is case-sensitive,
// so a spelling difference on a case-insensitive filesystem silently skips a
// resolution rather than making a wrong one; and `paths` may name a directory,
// which returns a directory path here and then matches no receipt, because
// receipts hold file paths and matching is exact equality. If receipt matching
// ever became prefix-based, that second property would turn into a hole.
func reindexedReceiptPath(paths []string, root string, result *indexer.IndexResult) (string, bool) {
	if len(paths) != 1 || result == nil || result.StaleFileCount <= 0 {
		return "", false
	}
	resolved, ok := reindexCandidatePath(paths, root)
	if !ok {
		return "", false
	}

	// FailedFiles carries ABSOLUTE paths (pinned by
	// TestIncrementalReindex_FailedFileSurfacedAndRetried) while paths[0] is
	// whatever spelling the caller passed, so the comparison has to happen
	// after the join — checking the raw request alone lets a relative request
	// walk straight past this guard, which is the only thing standing between
	// a file that failed to index and a barrier that reports it fresh.
	// StaleFileCount counts discovered-stale files INCLUDING the ones that
	// then failed, so it cannot stand in for this check. Both spellings are
	// compared because refusing a resolution is always the safe direction.
	requested := filepath.Clean(paths[0])
	for _, failed := range result.FailedFiles {
		if clean := filepath.Clean(failed); clean == resolved || clean == requested {
			return "", false
		}
	}
	return resolved, true
}

func (s *Server) handleGetSymbol(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := s.symbolIDArg(ctx, req)
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}

	// Auto re-index stale file before querying.
	if parts := strings.SplitN(id, "::", 2); len(parts) == 2 {
		s.ensureFresh([]string{parts[0]})
	}

	node := s.engineFor(ctx).GetSymbol(id)
	if node == nil {
		return symbolNotFoundGuidance(id), nil
	}

	resolved, errResult := s.resolveScope(ctx, req, toolIntentForName(req.Params.Name))
	if errResult != nil {
		return errResult, nil
	}
	if !resolvedScopeAllowsNode(resolved, node) {
		return symbolNotFoundGuidance(id), nil
	}

	s.sessionFor(ctx).recordSymbol(id)

	// On-demand LSP: when the synchronous LSP sweep is off (the default), fault
	// in this symbol's precise type now — one hover on the lazy-spawned server,
	// cached in the graph. No-op when it already has a type or no server serves
	// the language.
	s.enrichNodeOnDemand(node)

	detail := req.GetString("detail", "brief")
	if detail == "brief" {
		return s.respondScopedJSONOrTOON(ctx, req, s.withAbsPath(ctx, node).Brief(), resolved)
	}

	// Full: include node + direct edges.
	eng := s.engineFor(ctx)
	out := filterEdgesByResolvedScope(eng, eng.GetOutEdges(node.ID), resolved)
	in := filterEdgesByResolvedScope(eng, eng.GetInEdges(node.ID), resolved)
	return s.respondScopedJSONOrTOON(ctx, req, map[string]any{
		"node":      s.withAbsPath(ctx, node),
		"out_edges": out,
		"in_edges":  in,
	}, resolved)
}

func (s *Server) handleSearchSymbols(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	q, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("query is required"), nil
	}
	limit := req.GetInt("limit", 20)
	offset := decodeCursor(req.GetString("cursor", ""))

	sess := s.sessionFor(ctx)
	sess.recordSearch(q)

	// Field-qualified query syntax: lift `kind:` / `lang:` / `path:` /
	// `repo:` / `project:` clauses out of the query string. The
	// residual free text drives search, rerank, and classification;
	// the clauses become post-filters and scope, merged with the
	// explicit kind / repo / project arguments.
	fq := parseFieldQuery(q)
	q = fq.Text
	scopeReq := requestWithInlineScopeClauses(req, fq)

	// Apply server-default scope merged with caller args. `workspace`
	// / `project` args win per-field; empty falls through to the
	// server's --workspace flag. SearchSymbolsScoped over-fetches and
	// post-filters, so ranking is preserved while results stay inside
	// the boundary. With pagination we over-fetch to (offset + limit
	// + 10) so the post-filter slack still leaves a full page.
	resolved, errResult := s.resolveScope(ctx, scopeReq, IntentLocate)
	if errResult != nil {
		return errResult, nil
	}
	scopeWS, scopeProj := resolved.WorkspaceID, resolved.ProjectID
	// Per-phase timing for the search hot path. The struct is populated
	// across the engine boundary (BM25 backend call wall-clock attributes
	// to BM25*MS in fetchAndMergeBM25Timed; GetNodes / FindName / Fallback
	// land here from inside Engine.gatherBackendCandidates) and surfaced
	// at the end as a single debug log line. Nil-safe: callers without
	// debug logging pay zero overhead.
	timings := &query.SearchTimings{}
	phaseStart := time.Now()
	scope := query.QueryOptions{WorkspaceID: scopeWS, ProjectID: scopeProj, RepoAllow: resolved.RepoAllow, SearchTimings: timings}

	// Keyword-soup defense: a degenerate boolean / OR-list query
	// ("A OR B OR 'no access'") defeats ordinary retrieval. Detect it
	// up front -- explicit query_class:"keyword_soup" pins it, else the
	// structural detector decides. On a soup query the LLM expansion +
	// rerank passes are suppressed (wasted on garbage) and the soup is
	// split into per-disjunct BM25 fetches; a `query_advice` nudge is
	// attached so the agent learns to send a cleaner query.
	soupMode := s.searchConfig().EffectiveKeywordSoupRewrite()
	soupQueryClass := strings.EqualFold(strings.TrimSpace(req.GetString("query_class", "")), "keyword_soup")
	soupDetected, soupReason := rerank.LooksLikeKeywordSoup(q)
	isSoup := soupMode != config.KeywordSoupOff && (soupDetected || soupQueryClass)
	if isSoup && soupReason == "" {
		soupReason = "query reads as a boolean OR-list; search ranks best on a single concept or symbol name -- run one query per disjunct, or describe the intent in plain words"
	}

	// Identifier-shape fast path. ClassifyQuery is the structural
	// detector the rerank uses; QueryClassSymbol / Path / Signature
	// are queries where the rerank's classWeightTable already proves
	// the semantic channel contributes near-zero signal (0.65 / 0.45 /
	// 0.80 vs the baseline 1.00) — see internal/search/rerank/
	// query_kind.go::classWeightTable. For these classes the handler
	// forces expansion off and tells the engine to skip the vector
	// channel entirely; the rest of the pipeline (BM25 + bundle +
	// rerank) is the only path that matters. An explicit
	// query_class arg pin on one of these three classes engages the
	// fast path too. A soup query never engages the fast path —
	// keyword_soup has its own split-disjunct treatment.
	//
	// Validation of the query_class arg happens here so the early
	// gating uses the same validated value the rerank below uses;
	// invalid input is rejected before the engine runs.
	queryClass := rerank.ClassifyQuery(q)
	qcPinned := false
	if qcArg := strings.TrimSpace(req.GetString("query_class", "")); qcArg != "" {
		parsed, ok := rerank.ParseQueryClass(qcArg)
		if !ok {
			return mcp.NewToolResultError("invalid query_class: " + qcArg + " (want auto, symbol, concept, path, signature, or keyword_soup)"), nil
		}
		if parsed != rerank.QueryClassUnknown {
			queryClass = parsed
			qcPinned = true
		}
	}
	identifierFastPath := !isSoup && isIdentifierClass(queryClass)
	if identifierFastPath {
		scope.SkipVectorChannel = true
	}

	// LLM assist gate: decides whether the expansion + rerank passes
	// run for this query. The service-enabled check is layered inside
	// the helpers so a stub build is a clean bypass. A soup query
	// forces assist off -- neither expansion nor rerank earns its cost
	// on a disjunct list.
	assist := parseAssistMode(req)
	// expand mode picks which query-expansion channels run -- LLM,
	// the deterministic equivalence table, both (default), or off.
	expand := parseExpandMode(req)
	// Identifier-shape queries skip every expansion channel — the
	// rerank's classWeightTable shows BM25 is near-perfect for these
	// classes; expansion would only add the combined-OR fan-out's
	// extra backend call without lifting recall on a literal-token
	// query. The explicit arg pin still wins for soup / concept.
	if identifierFastPath {
		expand = expandOff
	}
	// Vocabulary-anchored expansion: post-filter the LLM expander's
	// returned synonyms to the words this repo's symbol names actually
	// use, so a hallucinated-but-plausible term can't dilute the BM25
	// pool. The explicit `vocab_anchored` argument wins; when omitted,
	// the server-wide config default applies. GetArguments is consulted
	// directly so an absent argument falls through to the config
	// default instead of GetBool's hard-coded false.
	vocabAnchored := s.searchConfig().VocabAnchoredExpansionDefault()
	if v, ok := req.GetArguments()["vocab_anchored"].(bool); ok {
		vocabAnchored = v
	}
	engage := shouldEngageAssist(assist, q) && s.llmService != nil && s.llmService.Enabled()
	if isSoup || !expand.allowsLLMExpansion() {
		engage = false
	}
	// Enrichment-aware first-load gate: an implicit assist request must
	// not trigger a cold load of the local model while a background
	// enrichment pass is in flight — the two would contend for
	// CPU/GPU/RAM. Defer assist for this request (BM25-only ranking) and
	// tell the caller via a rider; the explicit `ask` tool loads
	// regardless. Once the model is already loaded, busy() is irrelevant.
	var assistDeferred bool
	if engage {
		modelLoaded := s.llmService.ModelLoaded()
		busy := s.llmService.EnrichmentBusy()
		engage, assistDeferred = deferColdLoadForAssist(engage, modelLoaded, busy)
		if assistDeferred {
			s.logger.Info("deferred local model cold-load during enrichment",
				zap.String("query", q))
		}
	}

	fetchLimit := offset + limit + 10
	if engage {
		// Slightly widen the BM25 over-fetch when we're going to
		// rerank: more head candidates means a more useful reorder.
		fetchLimit = offset + limit + rerankCap
	} else if identifierFastPath {
		// Identifier-shape fast path: no expansion, no vector channel,
		// no LLM rerank — the only down-stream consumer is the
		// structural rerank pipeline scoring a single FTS-ranked head.
		// A wide head is wasted work; every extra candidate drags an
		// in/out edge pair through the bundle phase. Tighten to
		// +5 so the post-filter slack still leaves a full page.
		fetchLimit = offset + limit + 5
	} else {
		// Concept / degraded-identifier query with no LLM assist: the
		// always-on semantic-cosine rerank still runs, so widen the BM25
		// head to a full rerank pool. A natural-language intent query
		// ("decode bson request body") often has its target sitting past
		// the default +10 over-fetch; the semantic channel can only lift
		// a candidate it actually sees, so this pool is what turns a
		// BM25-rank-15 file hit into a top-5 result. Cheap: ~40 extra
		// FTS rows and their edge pairs, well inside the latency budget.
		if want := offset + limit + semanticRerankPool; want > fetchLimit {
			fetchLimit = want
		}
	}

	// Expansion terms feeding the BM25 OR-merge: LLM-derived synonyms
	// when assist engaged, or the soup's split disjuncts when this is
	// a soup query handled in "split" mode. The two are mutually
	// exclusive -- soup forces assist off above.
	// LLM-derived synonyms (when assist engaged), the soup's split
	// disjuncts (soup query in split mode), and the deterministic
	// equivalence-class siblings all compose into one expansion-term
	// list. fetchAndMergeBM25 dedups the merged result by node ID, so
	// overlapping channels never double-count a candidate.
	var llmTerms, soupFragments, equivTerms []string
	if engage {
		llmTerms = expandSearchTerms(ctx, s, q, vocabAnchored)
	}
	if isSoup && soupMode == config.KeywordSoupSplit {
		soupFragments = rerank.SplitSoupFragments(q)
	}
	if expand.allowsEquivalenceExpansion() {
		equivTerms = s.expandEquivalenceClasses(q)
	}
	expandedTerms := mergeExpansionTerms(soupFragments, llmTerms, equivTerms)

	// Build the rerank context BEFORE the BM25 fetch so the engine's
	// bundle path can seed its edge caches as the BM25 calls land.
	// The handler-side applyRerankBoostsTimed reuses this same rctx,
	// so the merged candidate set's edges are already cached when
	// prepare() runs against the post-filter slice. Without this
	// pre-fetch construction the engine's bundle would build a
	// throwaway cache on each BM25 call and the handler's later
	// rerank would still fetch every candidate's edges itself.
	rctx := s.buildRerankContext(ctx, q)
	scope.RerankContext = rctx

	// Corpus selection: `code` (default) keeps only code symbols,
	// `docs` keeps only prose-section (KindDoc) nodes, `all` keeps
	// both. Parsed BEFORE the fetch so the docs corpus can drive its
	// own retrieval channel (below) rather than relying purely on a
	// post-filter over the single code-shaped fetch.
	corpus, corpusErr := parseCorpus(req)
	if corpusErr != nil {
		return mcp.NewToolResultError(corpusErr.Error()), nil
	}

	var nodes []*graph.Node
	var primaryCount int
	if len(expandedTerms) > 0 {
		nodes, primaryCount = fetchAndMergeBM25Timed(s.engineFor(ctx), q, expandedTerms, fetchLimit, scope, timings)
	} else {
		bm25Start := time.Now()
		nodes = s.engineFor(ctx).SearchSymbolsScoped(q, fetchLimit, scope)
		timings.BM25PrimaryMS += time.Since(bm25Start).Milliseconds()
		primaryCount = len(nodes)
	}

	// Docs retrieval channel: when the corpus admits prose, the single
	// code-shaped fetch above is not enough -- a relevant doc section
	// can rank just past fetchLimit behind code symbols that share the
	// query's tokens and never enter the candidate pool at all, so the
	// corpus post-filter has nothing to keep. Issue a parallel,
	// wider-limit fetch and merge in its KindDoc hits before the
	// corpus filter runs, so prose competes on its own terms. The
	// merge dedupes by node ID; ranking among the merged set is settled
	// by the rerank pass downstream.
	if corpus.includesDocs() {
		nodes = s.mergeDocChannel(ctx, q, nodes, fetchLimit, scope, timings)
	}
	// Content retrieval channel: content (data_class=content) sections live
	// in the dedicated content index, not the symbol search, so the fetches
	// above can't surface them — pull them from the ContentSearcher and
	// merge before the corpus filter runs.
	if corpus.includesContent() {
		nodes = s.mergeContentChannel(ctx, q, nodes, fetchLimit)
	}

	candsAfterGather := len(nodes)
	mergedCount := len(nodes) // pre-filter; comparable to primaryCount

	allowed := resolved.RepoAllow
	// kind filter so callers can scope to a single new node kind
	// (todo, license, team, module, …). Comma-separated list —
	// case-insensitive — applied post-search so BM25 ranking is
	// preserved within the kept set. The explicit `kind` argument
	// wins; an in-query `kind:` clause fills in when it is absent.
	kindArg := strings.TrimSpace(req.GetString("kind", ""))
	if kindArg == "" {
		kindArg = fq.Kind
	}
	flavorArg := strings.TrimSpace(req.GetString("flavor", ""))
	if flavorArg == "" {
		flavorArg = fq.Flavor
	}
	// codegraph-compat shim: a kind: value that is actually a structural
	// flavor (class/struct/enum/component) routes to the flavor filter so
	// `kind:class` doesn't silently return an empty result set.
	if kindArg != "" {
		var movedFlavors string
		kindArg, movedFlavors = reclassifyKindFlavor(kindArg)
		if movedFlavors != "" {
			if flavorArg == "" {
				flavorArg = movedFlavors
			} else {
				flavorArg += "," + movedFlavors
			}
		}
	}
	pathFilter := s.resolvePathFilter(req, fq)

	// applyAllPostFilters runs the full post-search filter sequence
	// (repo / kind / lang+path clauses / sub-path scope / corpus) over
	// a candidate slice in the order the primary path uses it. Lifted
	// into a closure so the zero-result decomposition fallback applies
	// identical filtering to its re-fetched candidates — a fallback
	// that skipped any of these would surface results the caller's
	// scope was supposed to exclude.
	applyAllPostFilters := func(cands []*graph.Node) []*graph.Node {
		cands = filterNodes(cands, allowed)
		if kindArg != "" {
			cands = filterNodesByKind(cands, kindArg)
		}
		// lang: / path: / repo: clauses from the field-qualified syntax.
		cands = applyFieldFilters(cands, fq)
		// flavor: clause / flavor param / reclassified kind: value.
		if flavorArg != "" {
			cands = applyFlavorFilter(cands, flavorArg)
		}
		// Sub-path scoping: anchored, slash-segment prefix narrowing
		// below the repository grain. Distinct from the loose `path:`
		// substring match in applyFieldFilters -- this is a
		// directory-boundary anchored prefix test.
		if len(pathFilter) > 0 {
			cands = applyPathFilter(cands, pathFilter)
		}
		cands = filterNodesByCorpus(cands, corpus)
		return cands
	}
	nodes = applyAllPostFilters(nodes)

	// Post-filter wipeout rescue: the fetch found candidates but the
	// filters dropped every one — the real matches likely sit past the
	// fetch horizon, outranked by rows the filters remove (doc/junk
	// sections sharing the query's tokens; the classic case is a
	// suffix-convention query like "Extensions"). Refetch deeper under
	// the same scope BEFORE the filter-dropping fallbacks below.
	// Content sections live only in content_fts — this channel cannot
	// rescue them, so a content-corpus wipeout skips the refetch.
	fetchEscalated := false
	if len(nodes) == 0 && candsAfterGather > 0 && q != "" && corpus != corpusContent {
		// The requested cursor window must be reachable: a shallow
		// rescue that survives the filters but ends before offset+limit
		// would slice to an empty later page (with no next cursor) even
		// though a deeper fetch fills it. So a partial rescue keeps the
		// best set so far and keeps escalating until the window is
		// reachable or the corpus is exhausted.
		want := offset + limit
		prevDepth := 0
		for _, mult := range []int{5, 25} {
			if ctx.Err() != nil {
				break
			}
			deepLimit := fetchLimit * mult
			// Bound the deepest fetch: past this, bundle materialisation
			// cost dwarfs any plausible rescue.
			if deepLimit > 2000 {
				deepLimit = 2000
			}
			// The cap can collapse successive multipliers into the same
			// effective depth — an identical re-query cannot change the
			// outcome, so don't pay it twice.
			if deepLimit == prevDepth {
				break
			}
			prevDepth = deepLimit
			var refetched []*graph.Node
			if len(expandedTerms) > 0 {
				refetched, _ = fetchAndMergeBM25Timed(s.engineFor(ctx), q, expandedTerms, deepLimit, scope, timings)
			} else {
				refetched = s.engineFor(ctx).SearchSymbolsScoped(q, deepLimit, scope)
			}
			kept := applyAllPostFilters(refetched)
			if len(kept) > 0 {
				nodes = kept
				fetchEscalated = true
			}
			// Done when the window is reachable, or the corpus is
			// exhausted — a short raw page means a deeper fetch cannot
			// surface anything new.
			if len(kept) >= want || len(refetched) < deepLimit {
				break
			}
		}
	}

	// Fuzzy fallback: a field-qualified query that filtered down to
	// nothing retries on the free text alone (still inside the
	// caller's repo / project scope), so an over-narrow or typo'd
	// clause degrades to a useful result set instead of an empty one.
	filtersRelaxed := false
	if len(nodes) == 0 && q != "" && (kindArg != "" || flavorArg != "" || fq.hasFieldFilters()) {
		relaxed := filterNodes(s.engineFor(ctx).SearchSymbolsScoped(q, fetchLimit, scope), allowed)
		if len(relaxed) > 0 {
			nodes = relaxed
			filtersRelaxed = true
		}
	}

	// Leaf-decomposition fallback: a compound / dotted / CamelCase
	// query that found nothing — even after the field-clause relax
	// above — is decomposed into its leaf symbol-name tokens
	// ("UserService.FindUser" -> user / service / find) which are
	// re-fetched through the same BM25 OR-merge path the expansion
	// uses. Gated on the query carrying a separator so a prose miss
	// (which has no leaves to split into) never pays the extra fetch.
	// All post-filters are re-applied so the rescued candidates honour
	// the caller's repo / kind / lang / path / corpus scope.
	decomposed := false
	if len(nodes) == 0 && queryHasDecomposableSeparator(q) {
		if leaves := decomposeQueryToLeaves(q); len(leaves) > 0 {
			rescued, _ := fetchAndMergeBM25Timed(s.engineFor(ctx), "", leaves, fetchLimit, scope, timings)
			rescued = applyAllPostFilters(rescued)
			if len(rescued) > 0 {
				nodes = rescued
				decomposed = true
			}
		}
	}

	// LLM rerank runs AFTER kind/repo filters so the model only sees
	// the candidate pool the caller will actually receive, and BEFORE
	// the combo/frecency boost so per-session signals can still
	// override a stale rerank.
	var verifyDbg verifyDebug
	var verifyRan bool
	if engage {
		nodes = rerankWithLLM(ctx, s, q, nodes)
		// `deep` mode adds a body-grounded verification pass that
		// reads candidate code and HONESTLY drops the ones whose
		// body isn't actually about the query. An empty kept set is
		// preserved — it's the load-bearing "nothing genuinely matches"
		// signal that distinguishes deep mode from plain rerank.
		if assist == assistDeep {
			nodes, verifyDbg, verifyRan = verifyWithLLM(ctx, s, q, nodes)
		}
	}

	// Force-inject the implicit-feedback channel: symbols the agent has
	// reached for on this query before but that BM25 did not surface
	// this time. Injected before the rerank so the combo / feedback
	// signals rank them in context; post-filtered so they honour the
	// caller's repo / kind / lang / path / corpus scope.
	nodes = s.forceInjectLearnedCandidates(ctx, q, nodes, applyAllPostFilters)

	// Rerank: run the I13 11-signal pipeline over the candidate set
	// with the session-aware Context wired in. Structural signals
	// (BM25 rank, fan-in / fan-out, MinHash similarity, signature
	// match, recency, community) discriminate within the backend's
	// BM25 order; session signals (locality, combo, frecency,
	// feedback, churn) layer on top once the agent has spent time
	// in the codebase. Cold queries with no session data fall back
	// to a structural-only pass.
	//
	// rctx was built above (before the BM25 fetch) so the engine's
	// bundle path could seed its edge caches into the same rctx the
	// handler-side rerank will read from.
	// queryClass was classified + validated at the top of the handler
	// so the identifier-shape fast path could read it. Re-apply the
	// soup override here — soup detection happens after classification
	// and reports keyword_soup regardless of what the structural
	// detector thought the query looked like.
	if isSoup {
		queryClass = rerank.QueryClassKeywordSoup
	}
	if rctx != nil {
		rctx.QueryClass = queryClass
		// Continuous α lever: when the class was auto-detected — not
		// pinned by the caller and not a keyword-soup (which keeps its
		// discrete split-disjunct treatment) — score the bm25↔semantic
		// balance on a continuous query-shape axis instead of snapping
		// to a discrete class bucket.
		if !qcPinned && !isSoup {
			rctx.Alpha = rerank.AlphaForContinuous(q)
		}
		// Prose-tuned reranking: a docs-only query scores prose-section
		// nodes that have no call graph, signature, or definition
		// keyword, so suppress the code-structural signals and lift the
		// text + semantic channels. corpusAll still mixes code, so the
		// prose profile engages only for the docs-only corpus where
		// every candidate is a prose section.
		if corpus == corpusDocs || corpus == corpusContent {
			rctx.ProseMode = true
		}
	}
	candsAfterFilter := len(nodes)
	// Capture the post-filter candidate ID set so we can ask the rctx
	// what fraction of these candidates' edges were already cached by
	// the bundle pre-seed (vs needing prepare's own batched fetch).
	// Hit-rate is reported on the debug log as cache_hit_rate.
	if rctx != nil {
		preIDs := make([]string, 0, len(nodes))
		for _, n := range nodes {
			if n != nil {
				preIDs = append(preIDs, n.ID)
			}
		}
		timings.CacheHitRate = rctx.EdgeCacheHitRate(preIDs)
	}
	var rerankBreakdown []*rerank.Candidate
	var rerankPrepare, rerankSignals time.Duration
	nodes, rerankPrepare, rerankSignals = applyRerankBoostsTimed(ctx, s, nodes, q, rctx, &rerankBreakdown)

	// Post-rerank exact-cosine refinement. The merged rerank above
	// scores the semantic channel by RRF rank and discards the raw
	// cosine the vector store computed; this stage recovers it by
	// embedding the query once and re-ordering the ranked head against
	// the candidates' stored vectors. Skipped for identifier-shape
	// queries (the vector channel never ran) and strictly a no-op when
	// the vector channel is otherwise inactive, so it can never regress
	// a text-only search. rerankBreakdown is the candidate slice in the
	// final rerank order; refining it and rebuilding nodes keeps the
	// two aligned for the diversification pass below.
	if !scope.SkipVectorChannel && s.searchConfig().CosineRerankEnabled() && len(rerankBreakdown) > 1 {
		refined := s.engineFor(ctx).RefineByCosine(q, rerankBreakdown, 0)
		rerankBreakdown = refined
		refinedNodes := make([]*graph.Node, 0, len(refined))
		for _, c := range refined {
			if c != nil && c.Node != nil {
				refinedNodes = append(refinedNodes, c.Node)
			}
		}
		if len(refinedNodes) == len(nodes) {
			nodes = refinedNodes
		}
	}

	// Per-file diversification: keep one file's many symbols from
	// monopolising the head of the result set. Runs after the rerank
	// so demotion acts on final scores; nothing is dropped.
	diversifyStart := time.Now()
	nodes, rerankBreakdown = diversifyByFile(nodes, rerankBreakdown, req.GetInt("max_per_file", defaultMaxPerFile))
	diversifyMS := time.Since(diversifyStart).Milliseconds()

	// Flush the prior search's implicit skip-above negatives before this
	// search overwrites the attribution state: results that ranked above
	// the deepest one the agent consumed but were themselves passed over
	// lose a little of their learned per-keyword boost.
	if sess != nil && q != "" {
		if nq, skipped := sess.drainSkippedNegatives(); nq != "" && len(skipped) > 0 {
			s.combo.RecordNegative(nq, skipped)
		}
	}

	total := len(nodes)
	// Slice the (offset, limit) window. nextCursor is empty when the
	// last row in `nodes` is included.
	end := offset + limit
	if end > total {
		end = total
	}
	if offset > total {
		offset = total
	}
	page := nodes[offset:end]
	// Record only the actual post-cursor page. Empty and out-of-range pages
	// deliberately clear any prior attribution state.
	recordLastSearchFromNodes(sess, q, page)
	// Decorate the page with absolute file paths so every output format
	// below surfaces an openable path alongside the repo-relative one.
	page = s.withAbsPaths(ctx, page)
	// Capture the final ranked, scoped page once, before JSON/TOON/GCX encoding.
	captureLocalizationSearchSymbols(ctx, page)
	nextCursor := ""
	if end < total {
		nextCursor = encodeCursor(end)
	}

	indexWarning := s.indexFileFailureWarning(ctx, resolved, pathFilter)
	if isCompact(req) {
		return decorateResultWithScope(decorateIndexFileFailureResult(mcp.NewToolResultText(compactNodes(page)), indexWarning), resolved), nil
	}

	if s.isGCX(ctx, req) {
		res, err := s.gcxResponseWithBudget(req)(encodeSearchSymbols(page, total, len(page)))
		return withScopeResult(decorateIndexFileFailureResult(res, indexWarning), err, resolved)
	}

	if s.isTOON(ctx, req) {
		result := toonSearchResult{
			Results:   nodesToTOONRows(page),
			Total:     total,
			Truncated: end < total,
		}
		data, err := toon.Marshal(result)
		if err == nil {
			return decorateResultWithScope(decorateIndexFileFailureResult(mcp.NewToolResultText(string(data)), indexWarning), resolved), nil
		}
	}

	var results []map[string]any
	for _, n := range page {
		results = append(results, n.Brief())
	}
	resp := map[string]any{
		"results":     results,
		"total":       total,
		"truncated":   end < total,
		"query_class": queryClass.String(),
	}
	stampIndexFileFailureWarning(resp, indexWarning)
	if nextCursor != "" {
		resp["next_cursor"] = nextCursor
	}
	// Exact-identifier miss recovery. An identifier that no extractor turned
	// into a symbol (macro-generated, string-embedded, or simply missed) still
	// occurs verbatim in indexed content, and a page the caller learns nothing
	// from is the only thing the symbol channel can offer. Run one bounded
	// literal search and return the hits in a separate, explicitly non-symbol
	// section.
	//
	// A decomposition rescue counts as the same miss: it runs only after every
	// earlier tier returned nothing for the exact query, so its rows are
	// leaf-token neighbours of the identifier rather than the identifier. Most
	// real identifiers carry a separator or a camelCase boundary, so gating on
	// a literally empty page alone would leave the recovery unreachable.
	//
	// A kind / flavor narrowing forbids it — a raw content line has no node
	// kind to satisfy. The section rides its own field: `results`, `total`,
	// and the graph page captured for the localization recovery contract above
	// are all untouched, so an empty typed page still reads as empty to every
	// contract check.
	if (total == 0 || decomposed) && kindArg == "" && flavorArg == "" {
		if term, identifier := searchSymbolsContentFallbackTerm(q); identifier {
			if section := s.searchSymbolsContentFallback(ctx, term, resolved, scope); section != nil {
				resp["content_matches"] = section
			}
		}
	}
	// A repo-narrowed zero is indistinguishable from "not indexed" in
	// clients that never render _meta. Say it in the body — and since the
	// result is empty anyway, pay one extra BM25 fetch to report whether
	// widening would actually help.
	if total == 0 && len(resolved.RepoAllow) > 0 {
		wide := scope
		wide.RepoAllow = nil
		wideNodes, _ := fetchAndMergeBM25Timed(s.engineFor(ctx), q, expandedTerms, offset+limit, wide, timings)
		resp["scope_note"] = scopeZeroNote(resolved, len(wideNodes))
	}
	if fetchEscalated {
		resp["fetch_escalated"] = true
	}
	if filtersRelaxed {
		resp["filters_relaxed"] = true
	}
	if decomposed {
		resp["decomposed"] = true
	}
	// query_advice nudges the agent toward a cleaner query when the
	// input was detected as keyword-soup. In "split" mode the search
	// still ran (over the split disjuncts); the advice is purely
	// instructional and rides alongside the results.
	if isSoup && soupReason != "" {
		advice := map[string]any{"reason": soupReason}
		if len(soupFragments) > 0 {
			advice["split_into"] = soupFragments
		}
		resp["query_advice"] = advice
	}
	// When LLM assist engaged, expose a small debug surface so callers
	// (and the agent itself) can see what the model contributed.
	// Suppressed when engage was false to keep the common-path response
	// shape unchanged.
	if engage {
		assistDebug := map[string]any{
			"engaged":       true,
			"primary_count": primaryCount, // BM25 hits on original query alone, pre-filter
			"merged_count":  mergedCount,  // BM25 hits after merging expansion terms, pre-filter
			"final_count":   total,        // post-filter, post-rerank — matches the top-level total
		}
		if len(llmTerms) > 0 {
			assistDebug["terms"] = llmTerms
		}
		if len(equivTerms) > 0 {
			assistDebug["equivalence_terms"] = equivTerms
		}
		if verifyRan {
			assistDebug["verify_considered"] = verifyDbg.Considered
			assistDebug["verify_kept_ids"] = verifyDbg.Kept
			assistDebug["verify_kept"] = len(verifyDbg.Kept)
			assistDebug["verify_dropped"] = len(verifyDbg.Considered) - len(verifyDbg.Kept)
		}
		resp["assist"] = assistDebug
	}
	// When the assist cold-load was deferred (enrichment in flight),
	// tell the caller the results are BM25-only and LLM-assisted ranking
	// will be available shortly — so a thin result set isn't read as a
	// hard miss.
	if assistDeferred {
		resp["assist_deferred"] = map[string]any{
			"reason": "enrichment_in_progress",
			"detail": "local model cold-load deferred while background enrichment is running; results are BM25-only. Retry shortly for LLM-assisted ranking, or use the `ask` tool to load the model now.",
		}
	}
	// Surface the 11-signal rerank breakdown when the caller asked
	// for it via debug=true. Page-sliced to match the rows in
	// `results`. Keeps the common-path response shape unchanged.
	if req.GetBool("debug", false) && len(rerankBreakdown) > 0 {
		pageBreakdown := rerankBreakdown
		if offset < len(pageBreakdown) {
			pageBreakdown = pageBreakdown[offset:]
		} else {
			pageBreakdown = nil
		}
		if len(pageBreakdown) > limit {
			pageBreakdown = pageBreakdown[:limit]
		}
		resp["rerank"] = encodeRerankBreakdown(pageBreakdown, s.engineFor(ctx).Rerank())
	}

	// Per-phase Debug log line — single zap.Debug call carrying every
	// timing field for this search_symbols invocation. The bench harness
	// greps for the "search_symbols phases" message at --log-level
	// debug; production runs at info level pay nothing. Tracked phases:
	// BM25 primary / expansion calls (wall-clock around the engine),
	// the inner GetNodesByIDs / FindNodesByName / Fallback hops (from
	// the engine), rerank prepare (batched edge fetch) and signals
	// (in-process scoring), diversify, and the candidate counts at
	// gather → filter → final.
	if s.logger != nil {
		totalMS := time.Since(phaseStart).Milliseconds()
		// "BM25 backend" cost = the BM25 wall-clock minus the inner
		// phases the engine also accumulated under that call. Negative
		// values are clamped to 0 (clock granularity / contention).
		// BundleMS is subtracted too — it's a fold of the FTS + nodes
		// + edge fetches that, on the legacy path, would have shown up
		// in TextBackend / GetNodes / (no field for edges) separately.
		bm25Backend := timings.BM25PrimaryMS + timings.BM25ExpansionMS - timings.GetNodesMS - timings.FindNameMS - timings.FallbackMS - timings.BundleMS
		if bm25Backend < 0 {
			bm25Backend = 0
		}
		s.logger.Debug("search_symbols phases",
			zap.String("query", q),
			zap.Int("expansion_terms", len(expandedTerms)),
			zap.Int64("bm25_primary_ms", timings.BM25PrimaryMS),
			zap.Int64("bm25_expansion_ms", timings.BM25ExpansionMS),
			zap.Int64("bm25_backend_ms", bm25Backend),
			zap.Int64("text_backend_ms", timings.TextBackendMS),
			zap.Int64("embed_ms", timings.EmbedMS),
			zap.Int64("vector_search_ms", timings.VectorSearchMS),
			zap.Int64("engine_rerank_ms", timings.EngineRerankMS),
			zap.Int64("bundle_ms", timings.BundleMS),
			zap.Float64("cache_hit_rate", timings.CacheHitRate),
			zap.Int64("get_nodes_ms", timings.GetNodesMS),
			zap.Int64("find_name_ms", timings.FindNameMS),
			zap.Int64("fallback_ms", timings.FallbackMS),
			zap.Duration("rerank_prepare_ms", rerankPrepare),
			zap.Duration("rerank_signals_ms", rerankSignals),
			zap.Int64("diversify_ms", diversifyMS),
			zap.Int64("total_ms", totalMS),
			zap.Int("cands_after_gather", candsAfterGather),
			zap.Int("cands_after_filter", candsAfterFilter),
			zap.Int("cands_final", len(nodes)),
		)
	}

	return s.respondScopedJSONOrTOON(ctx, req, resp, resolved)
}

// encodeRerankBreakdown converts a candidate slice into a JSON-ready
// breakdown — one row per candidate carrying its final score and the
// per-signal contributions in the canonical order.
func encodeRerankBreakdown(cands []*rerank.Candidate, pipeline *rerank.Pipeline) []map[string]any {
	if len(cands) == 0 {
		return nil
	}
	weights := map[string]float64{}
	if pipeline != nil {
		weights = pipeline.Weights()
	}
	out := make([]map[string]any, 0, len(cands))
	for _, c := range cands {
		row := map[string]any{
			"id":    c.Node.ID,
			"score": roundTo(c.Score, 4),
		}
		if c.TextRank >= 0 {
			row["text_rank"] = c.TextRank
		}
		if c.VectorRank >= 0 {
			row["vector_rank"] = c.VectorRank
		}
		if len(c.Signals) > 0 {
			sigs := make(map[string]float64, len(c.Signals))
			for _, name := range rerank.AllSignalNames() {
				if v, ok := c.Signals[name]; ok {
					sigs[name] = roundTo(v, 4)
				}
			}
			row["signals"] = sigs
		}
		if len(weights) > 0 {
			row["weights"] = weights
		}
		out = append(out, row)
	}
	return out
}

func roundTo(v float64, places int) float64 {
	if v == 0 {
		return 0
	}
	pow := 1.0
	for range places {
		pow *= 10
	}
	return float64(int64(v*pow+0.5)) / pow
}

func (s *Server) handleGetFileSummary(ctx context.Context, req mcp.CallToolRequest) (res *mcp.CallToolResult, retErr error) {
	// Defensive panic recovery — get_file_summary has been observed
	// to crash the MCP transport in multi-repo mode (file-content
	// validation gap). Surface the panic as a tool error so the
	// session survives.
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("get_file_summary panic recovered",
				zap.String("path", req.GetString("path", "")),
				zap.Any("panic", r))
			res = mcp.NewToolResultError(fmt.Sprintf("get_file_summary internal error: %v", r))
			retErr = nil
		}
	}()
	fp, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	// Normalise to the graph's stored path form so a repo-relative path
	// (internal/x.go) doesn't miss the repo-prefixed nodes in multi-repo
	// mode — the cause of spurious file_not_indexed misses.
	fp = s.graphRelPath(ctx, fp)

	// Auto re-index stale file before querying.
	s.ensureFresh([]string{fp})

	// gcx is the high-volume agent format and only emits total_edges
	// in its meta header — never per-edge rows. Route gcx-only calls
	// through the count-only path so the disk backends skip
	// materialising every adjacent edge across cgo (a 4 000-row
	// round-trip on a 500-symbol file becomes two scalar aggregates).
	// compact + json paths still take the full SubGraph because
	// compact summarises edges per confidence label and json ships
	// every edge in the body.
	gcxOnly := s.isGCX(ctx, req) && !isCompact(req)
	var sg *query.SubGraph
	if gcxOnly {
		sg = s.engineFor(ctx).GetFileSymbolsCounts(fp)
	} else {
		sg = s.engineFor(ctx).GetFileSymbols(fp)
	}
	if len(sg.Nodes) == 0 {
		return fileNotIndexedGuidance(fp), nil
	}

	// Apply repo/project/ref filter.
	allowed, filterErr := s.resolveRepoFilter(ctx, req)
	if filterErr != nil {
		return mcp.NewToolResultError(filterErr.Error()), nil
	}
	sg = filterSubGraph(sg, allowed)
	if len(sg.Nodes) == 0 {
		return fileNotIndexedGuidance(fp), nil
	}

	// get_file_summary's contract is "what symbols does this file
	// define" — the file node itself and import nodes ride on
	// GetFileSubGraph because they're useful for other walkers, but
	// the encoder layer wants the symbols-only view. The compact
	// path already filtered both kinds inline; the cleaner home is
	// here so every output format (compact, gcx, json, toon) sees the
	// same shape.
	sg = stripNonDefinitionNodes(sg)
	if len(sg.Nodes) == 0 {
		return fileNotIndexedGuidance(fp), nil
	}

	// ETag conditional fetch — checked before any savings accounting so
	// a not_modified turnaround books nothing (a polling client would
	// otherwise mint fake savings on every poll). Use the structural
	// SubGraph hash — json.Marshal'ing the whole SubGraph + Meta on
	// every call was the dominant cost on large files (~2 ms / call on
	// a 500-symbol file).
	etag := etagSubGraph(sg)
	if ifNoneMatch := req.GetString("if_none_match", ""); ifNoneMatch != "" && ifNoneMatch == etag {
		return notModifiedResult(etag), nil
	}

	// Server-side accounting only — a file summary stands in for
	// reading the whole file. The payload sample tracks the format
	// actually returned (the compact text for compact/gcx, the
	// marshaled result for json/toon) so `returned` reflects what was
	// shipped.
	summaryLang := ""
	for _, n := range sg.Nodes {
		if n != nil && n.Language != "" {
			summaryLang = n.Language
			break
		}
	}

	if isCompact(req) {
		payload := compactSubGraph(sg)
		s.recordFileBaselineSavings(ctx, "get_file_summary", fp, summaryLang, payload)
		return mcp.NewToolResultText(payload), nil
	}

	if s.isGCX(ctx, req) {
		s.recordFileBaselineSavings(ctx, "get_file_summary", fp, summaryLang, compactSubGraph(sg))
		return s.gcxResponseWithBudget(req)(encodeFileSummary(sg, etag))
	}

	// Wrap with etag in response.
	result := map[string]any{
		"nodes":       sg.Nodes,
		"edges":       sg.Edges,
		"total_nodes": len(sg.Nodes),
		"total_edges": len(sg.Edges),
		"truncated":   sg.Truncated,
		"etag":        etag,
	}
	s.attachFileDependents(ctx, result, fp)
	if payload, merr := json.Marshal(result); merr == nil {
		s.recordFileBaselineSavings(ctx, "get_file_summary", fp, summaryLang, string(payload))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

func (s *Server) handleGetDependencies(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := s.symbolIDArg(ctx, req)
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	minTier := req.GetString("min_tier", "")
	resolved, errResult := s.resolveScope(ctx, req, IntentReach)
	if errResult != nil {
		return errResult, nil
	}
	eng := s.engineFor(ctx)
	if node := eng.GetSymbol(id); node != nil && !resolvedScopeAllowsNode(resolved, node) {
		return symbolNotFoundGuidance(id), nil
	}
	opts := query.QueryOptions{
		Depth:       req.GetInt("depth", 2),
		Limit:       req.GetInt("limit", 50),
		Detail:      "brief",
		MinTier:     minTier,
		WorkspaceID: resolved.WorkspaceID,
		ProjectID:   resolved.ProjectID,
		RepoAllow:   resolved.RepoAllow,
	}
	sg := eng.GetDependencies(id, opts)
	sg = filterSubGraphByResolvedScope(sg, resolved)
	sg.FilterByMinTier(minTier)
	sg.FilterSpeculative(req.GetBool("include_speculative", false))
	enrichSubGraphEdges(sg)
	return s.returnScopedSubGraph(ctx, req, sg, resolved)
}

func (s *Server) handleGetDependents(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := s.symbolIDArg(ctx, req)
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	minTier := req.GetString("min_tier", "")
	resolved, errResult := s.resolveScope(ctx, req, IntentReach)
	if errResult != nil {
		return errResult, nil
	}
	eng := s.engineFor(ctx)
	if node := eng.GetSymbol(id); node != nil && !resolvedScopeAllowsNode(resolved, node) {
		return symbolNotFoundGuidance(id), nil
	}
	opts := query.QueryOptions{
		Depth:       req.GetInt("depth", 3),
		Limit:       req.GetInt("limit", 50),
		Detail:      "brief",
		MinTier:     minTier,
		WorkspaceID: resolved.WorkspaceID,
		ProjectID:   resolved.ProjectID,
		RepoAllow:   resolved.RepoAllow,
	}
	sg := eng.GetDependents(id, opts)
	sg = filterSubGraphByResolvedScope(sg, resolved)
	sg.FilterByMinTier(minTier)
	sg.FilterSpeculative(req.GetBool("include_speculative", false))
	enrichSubGraphEdges(sg)
	return s.returnScopedSubGraph(ctx, req, sg, resolved)
}

func (s *Server) handleGetCallChain(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := s.symbolIDArg(ctx, req)
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	minTier := req.GetString("min_tier", "")
	resolved, errResult := s.resolveScope(ctx, req, IntentReach)
	if errResult != nil {
		return errResult, nil
	}
	eng := s.engineFor(ctx)
	if node := eng.GetSymbol(id); node != nil && !resolvedScopeAllowsNode(resolved, node) {
		return symbolNotFoundGuidance(id), nil
	}
	opts := query.QueryOptions{
		Depth:       req.GetInt("depth", 4),
		Limit:       req.GetInt("limit", 50),
		Detail:      "brief",
		MinTier:     minTier,
		WorkspaceID: resolved.WorkspaceID,
		ProjectID:   resolved.ProjectID,
		RepoAllow:   resolved.RepoAllow,
	}
	s.hydrateProxyTargets(ctx, id)
	sg := eng.GetCallChain(id, opts)

	sg = filterSubGraphByResolvedScope(sg, resolved)
	sg.FilterByMinTier(minTier)
	sg.FilterSpeculative(req.GetBool("include_speculative", false))
	enrichSubGraphEdges(sg)
	annotateProxyFreshness(sg)
	return s.returnScopedSubGraph(ctx, req, sg, resolved)
}

func (s *Server) handleGetCallers(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	target, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	id, candidates := s.resolveSymbolTarget(ctx, target)
	if len(candidates) > 0 {
		return s.symbolDisambiguationResult(ctx, req, "get_callers", target, candidates)
	}
	minTier := req.GetString("min_tier", "")
	resolved, errResult := s.resolveScope(ctx, req, IntentReach)
	if errResult != nil {
		return errResult, nil
	}
	eng := s.engineFor(ctx)
	if node := eng.GetSymbol(id); node != nil && !resolvedScopeAllowsNode(resolved, node) {
		return symbolNotFoundGuidance(id), nil
	}
	opts := query.QueryOptions{
		Depth:        req.GetInt("depth", 2),
		Limit:        req.GetInt("limit", 50),
		Detail:       "brief",
		MinTier:      minTier,
		WorkspaceID:  resolved.WorkspaceID,
		ProjectID:    resolved.ProjectID,
		RepoAllow:    resolved.RepoAllow,
		ExcludeTests: req.GetBool("exclude_tests", false),
	}
	// Lazy enrichment: confirm this symbol's callers on demand before
	// answering, so a graph indexed without the eager LSP sweep still returns
	// compiler-grade callers. No-op when already confirmed or eager ran.
	s.confirmSymbolRefsOnDemand(eng.GetSymbol(id))
	s.hydrateProxyTargets(ctx, id)
	sg := eng.GetCallers(id, opts)
	sg = filterSubGraphByResolvedScope(sg, resolved)
	sg.FilterByMinTier(minTier)
	sg.FilterSpeculative(req.GetBool("include_speculative", false))
	if minTier == "" {
		// Same adaptive default as find_usages: hide the name-only
		// fan-out once resolver-verified callers exist.
		sg.SuppressRedundantTextMatches()
		s.attachSuppressionCaveat(ctx, sg, id)
	}
	s.attachNameOnlyCandidates(eng, sg, id, minTier, opts)
	enrichSubGraphEdges(sg)
	annotateCallerConcurrency(eng.Reader(), sg, id)
	if len(sg.Edges) == 0 && sg.TierFiltered == nil {
		// A tier_filtered emptiness is not "no callers" — FilterByMinTier
		// already recorded why, and the caller's own min_tier is what
		// removed them. Classifying on top would double-report it, and
		// now that classification weighs provenance it would report the
		// very tier the caller asked to exclude. A symbol the request's
		// view resolved (overlay-served included) is never a mistyped id.
		// Classification reads the same request-scoped view the query
		// ran on: the base graph misreports overlay-only symbols and
		// still counts callers an overlay tombstone removed.
		if eng.GetSymbol(id) != nil {
			sg.Caveat = graph.CaveatForZeroEdgeResolved(s.readerFor(ctx), id)
		} else {
			sg.Caveat = graph.CaveatForZeroEdge(s.readerFor(ctx), id)
		}
	} else if len(sg.Edges) > 0 && graph.WeakUsageEvidenceOnly(sg.Edges) {
		// Populated, but every caller is a bare-name match — the shape a
		// common method name (`Get`, `Run`, `Close`) produces. Left
		// uncaveated this reads as proof the symbol is live, which is
		// exactly how genuine dead code hides. Judged from the edges
		// already in hand, so no extra graph reads on the hot path.
		sg.Caveat = graph.CaveatForWeakUsageEvidence()
	}
	// Epistemic lower bound: a caller walk over in-edges cannot see callers
	// that reach this symbol through interface dispatch the resolver left
	// unbound. Flag the floor + name the interface so the agent can widen it.
	// Judged from the request-scoped view — the same graph the query ran
	// on — so an overlay-added implements edge surfaces its floor and a
	// tombstoned one stops reporting a stale one.
	reader := s.readerFor(ctx)
	if bs := graph.CallerBoundaries(reader, []string{id}, 0); len(bs) > 0 {
		sg.Boundaries = bs
		sg.LowerBound = graph.LowerBoundCaveat(bs)
		// The reach is a floor because dispatch is dynamic — scan the seed's
		// body for the exact runtime-dispatch sites so the agent gets
		// {site, form, key, candidates} instead of a read-spiral.
		if db := s.dynamicBoundariesForSymbol(ctx, reader, reader.GetNode(id)); len(db) > 0 {
			sg.DynamicBoundaries = db
		}
	}
	annotateProxyFreshness(sg)
	return s.returnScopedSubGraph(ctx, req, sg, resolved)
}

// annotateCallerConcurrency attaches a ConcurrencyAnnotation to every
// caller node in a get_callers result that carries at least one
// concurrency flag. The seed node itself is skipped — it is the
// queried symbol, not one of its callers. File / import nodes are
// skipped because no concurrency fact applies to them. A node with no
// flag set is left out of the map, so an absent entry reads as
// "neither sync_guarded nor cross_concurrent".
func annotateCallerConcurrency(r graph.Reader, sg *query.SubGraph, seedID string) {
	if r == nil || sg == nil {
		return
	}
	for _, n := range sg.Nodes {
		if n == nil || n.ID == seedID {
			continue
		}
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		ann := graph.ClassifyConcurrency(r, n.ID)
		if !ann.Any() {
			continue
		}
		if sg.CallerNotes == nil {
			sg.CallerNotes = make(map[string]*graph.ConcurrencyAnnotation)
		}
		annCopy := ann
		sg.CallerNotes[n.ID] = &annCopy
	}
}

func (s *Server) handleFindOverrides(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := s.symbolIDArg(ctx, req)
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	direction := req.GetString("direction", "children")
	minTier := req.GetString("min_tier", "")
	resolved, errResult := s.resolveScope(ctx, req, IntentReach)
	if errResult != nil {
		return errResult, nil
	}
	var nodes []*graph.Node
	switch direction {
	case "parents", "overridden":
		nodes = s.engineFor(ctx).FindOverridden(id)
	default:
		nodes = s.engineFor(ctx).FindOverridesMinTier(id, minTier)
	}
	// Confine results to the session's workspace — these engine
	// methods don't take QueryOptions, so the boundary is enforced
	// here.
	nodes = s.scopedNodeSlice(ctx, nodes)
	nodes = filterNodesByResolvedScope(nodes, resolved)
	nodes = s.withAbsPaths(ctx, nodes)

	if s.isGCX(ctx, req) {
		sg := &query.SubGraph{Nodes: nodes, TotalNodes: len(nodes)}
		return s.returnScopedSubGraph(ctx, req, sg, resolved)
	}
	if s.isTOON(ctx, req) {
		result := struct {
			Overrides []toonNodeRow `toon:"overrides"`
			Total     int           `toon:"total"`
		}{
			Overrides: nodesToTOONRows(nodes),
			Total:     len(nodes),
		}
		if data, err := toon.Marshal(result); err == nil {
			return decorateResultWithScope(mcp.NewToolResultText(string(data)), resolved), nil
		}
	}
	results := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		results = append(results, n.Brief())
	}
	return s.respondScopedJSONOrTOON(ctx, req, map[string]any{
		"overrides": results,
		"total":     len(results),
		"direction": direction,
	}, resolved)
}

func (s *Server) handleFindImplementations(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := s.symbolIDArg(ctx, req)
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	minTier := req.GetString("min_tier", "")
	resolved, errResult := s.resolveScope(ctx, req, IntentReach)
	if errResult != nil {
		return errResult, nil
	}
	impls, implEdges := s.engineFor(ctx).FindImplementationsWithEdgesMinTier(id, minTier)
	// Confine results to the session's workspace — FindImplementations
	// doesn't take QueryOptions, so the boundary is enforced here.
	impls = s.scopedNodeSlice(ctx, impls)
	impls = filterNodesByResolvedScope(impls, resolved)
	impls = s.withAbsPaths(ctx, impls)

	if s.isGCX(ctx, req) {
		// Keep only the implements-edges whose implementation survived
		// the scope filters, so the `.edges` section always agrees with
		// `.nodes` — an edge to a node the session can't see would leak
		// the boundary the filters just enforced. Copies, not the live
		// graph edges: enrichSubGraphEdges writes display fields, and a
		// read path must never mutate shared graph state.
		keep := make(map[string]bool, len(impls))
		for _, n := range impls {
			keep[n.ID] = true
		}
		var edges []*graph.Edge
		for _, edge := range implEdges {
			if keep[edge.From] {
				c := *edge
				edges = append(edges, &c)
			}
		}
		sg := &query.SubGraph{Nodes: impls, Edges: edges, TotalNodes: len(impls), TotalEdges: len(edges)}
		enrichSubGraphEdges(sg)
		return s.returnScopedSubGraph(ctx, req, sg, resolved)
	}

	if s.isTOON(ctx, req) {
		result := struct {
			Implementations []toonNodeRow `toon:"implementations"`
			Total           int           `toon:"total"`
		}{
			Implementations: nodesToTOONRows(impls),
			Total:           len(impls),
		}
		if data, err := toon.Marshal(result); err == nil {
			return decorateResultWithScope(mcp.NewToolResultText(string(data)), resolved), nil
		}
	}

	var results []map[string]any
	for _, n := range impls {
		results = append(results, n.Brief())
	}
	return s.respondScopedJSONOrTOON(ctx, req, map[string]any{
		"implementations": results,
		"total":           len(results),
	}, resolved)
}

func (s *Server) handleGetClassHierarchy(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := s.symbolIDArg(ctx, req)
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	direction := query.HierarchyDirection(req.GetString("direction", string(query.HierarchyBoth)))
	switch direction {
	case query.HierarchyUp, query.HierarchyDown, query.HierarchyBoth:
		// ok
	default:
		return mcp.NewToolResultError("direction must be one of: up, down, both"), nil
	}
	depth := req.GetInt("depth", 5)
	includeMethods := req.GetBool("include_methods", false)
	minTier := req.GetString("min_tier", "")

	resolved, errResult := s.resolveScope(ctx, req, IntentReach)
	if errResult != nil {
		return errResult, nil
	}
	eng := s.engineFor(ctx)
	if node := eng.GetSymbol(id); node != nil && !resolvedScopeAllowsNode(resolved, node) {
		return symbolNotFoundGuidance(id), nil
	}
	opts := query.QueryOptions{
		WorkspaceID: resolved.WorkspaceID,
		ProjectID:   resolved.ProjectID,
		RepoAllow:   resolved.RepoAllow,
		MinTier:     minTier,
	}
	sg := eng.ClassHierarchy(id, direction, depth, includeMethods, opts)
	sg = filterSubGraphByResolvedScope(sg, resolved)
	enrichSubGraphEdges(sg)
	return s.returnScopedSubGraph(ctx, req, sg, resolved)
}

func (s *Server) handleFindUsages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := s.symbolIDArg(ctx, req)
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	minTier := req.GetString("min_tier", "")

	// find_usages on a tuck symbol returns hits only from tuck.
	// Server-level --workspace + caller `workspace` arg compose the
	// same way as on search_symbols.
	resolved, errResult := s.resolveScope(ctx, req, IntentReach)
	if errResult != nil {
		return errResult, nil
	}
	eng := s.engineFor(ctx)
	// One request-scoped node lookup, resolved once and handed to every
	// classification/flavor surface below, so none can drift back to the
	// base graph while the query runs on an overlay view.
	getter := s.nodeGetterFor(ctx)
	// Barrel re-export resolve-through: a public import path that names a
	// forwarded binding (`src/middleware.ts::persist`) may have no node of its
	// own (an unresolved / over-cap / wildcard barrel) — map it to the
	// canonical declaration so find_usages answers with the real usages instead
	// of not_found. Gated on the id not already being a node, so a real
	// symbol's result is never altered.
	node := eng.GetSymbol(id)
	if node == nil {
		if canon := reExportBindingCanonical(s.readerFor(ctx), id, 0); canon != "" {
			id = canon
			node = eng.GetSymbol(id)
		}
	}
	if node != nil && !resolvedScopeAllowsNode(resolved, node) {
		return symbolNotFoundGuidance(id), nil
	}
	// Lazy enrichment: confirm this symbol's incoming references on demand
	// before answering, so a graph indexed without the eager LSP sweep still
	// converges to compiler-grade usages. No-op when already confirmed or eager
	// ran.
	s.confirmSymbolRefsOnDemand(node)
	opts := query.QueryOptions{
		WorkspaceID:  resolved.WorkspaceID,
		ProjectID:    resolved.ProjectID,
		RepoAllow:    resolved.RepoAllow,
		ExcludeTests: req.GetBool("exclude_tests", false),
	}
	sg := eng.FindUsagesScoped(id, opts)
	// Barrel re-export binding: a query on the public façade node
	// (`src/middleware.ts::persist`) returns its own direct refs — consumer
	// imports that bind to it — plus the canonical declaration's usages,
	// deduped. So the façade an agent actually imports answers with the full
	// usage set instead of only the forwarding site.
	if isReExportNode(node) {
		if canon := reExportNodeCanonical(s.readerFor(ctx), id, 0); canon != "" && canon != id {
			mergeUsageSubGraph(sg, eng.FindUsagesScoped(canon, opts))
		}
	}

	sg = filterSubGraphByResolvedScope(sg, resolved)
	sg.FilterByMinTier(minTier)
	sg.FilterSpeculative(req.GetBool("include_speculative", false))
	if minTier == "" {
		// Adaptive default: with resolver-verified usages present, the
		// text_matched fan-out is noise — suppress it and report the count
		// via text_matched_suppressed. min_tier:"text_matched" restores it.
		sg.SuppressRedundantTextMatches()
		s.attachSuppressionCaveat(ctx, sg, id)
	}
	s.attachNameOnlyCandidates(eng, sg, id, minTier, opts)
	enrichSubGraphEdges(sg)
	// Classify each usage's reference context (parameter_type / return_type
	// / field / value / type / attribute / call) and optionally filter to
	// one context — `find_usages context:"parameter_type"`.
	annotateAndFilterUsageContext(sg, strings.ToLower(strings.TrimSpace(req.GetString("context", ""))))
	// Surface the extractor-stamped return-usage classification on each
	// call usage and optionally filter to one consumption shape —
	// `find_usages return_usage:"discarded"`.
	annotateAndFilterReturnUsage(sg, strings.ToLower(strings.TrimSpace(req.GetString("return_usage", ""))))
	// flavor: keeps only usages whose FROM site resolves to a matching
	// structural flavor (the enclosing owner type for a type flavor; the
	// FROM node's own ui_component for `component`). Orphan nodes left
	// with no incident edge are pruned.
	filterUsagesByFlavor(getter, sg, id, strings.TrimSpace(req.GetString("flavor", "")))
	if len(sg.Edges) == 0 && sg.TierFiltered == nil {
		// A tier_filtered emptiness is not "no usages" — FilterByMinTier
		// already recorded why. Only reach for the extraction-gap / unused
		// classification when the emptiness was NOT caused by min_tier.
		// A node the request's view resolved (an overlay-served symbol
		// among them) is never a mistyped id. Classification reads the
		// same request-scoped view the query ran on, never the base graph.
		if node != nil {
			sg.Caveat = graph.CaveatForZeroEdgeResolved(s.readerFor(ctx), id)
		} else {
			sg.Caveat = graph.CaveatForZeroEdge(s.readerFor(ctx), id)
		}
	} else if len(sg.Edges) > 0 && graph.WeakUsageEvidenceOnly(sg.Edges) {
		// Populated, but every usage is a bare-name match — the shape a
		// common symbol name produces. Left uncaveated this reads as proof
		// the symbol is live, which is exactly how genuine dead code hides.
		// Judged from the edges already in hand, so no extra graph reads.
		sg.Caveat = graph.CaveatForWeakUsageEvidence()
	}
	// Completeness rollup (n_refs / n_files / n_test_refs) so an agent can
	// tell at a glance whether the usage list already covers tests instead
	// of re-grepping *_test.go files. Rides every wire format below; nil
	// for an empty result (the Caveat above covers that case).
	sg.UsageSummary = query.UsageSummaryOf(sg, getter)
	// Proactive discovery cue: when the target is dispatch-heavy, point at
	// find_implementations (once per session). Additive — the only response
	// content the diet adds.
	s.attachRelatedToolsCue(ctx, sg, id)
	// Row cap last, after every filter and after the completeness rollup —
	// the summary stays a statement about the whole usage set while the
	// page below it is bounded. limit:0 opts out, mirroring max_bytes.
	truncateUsageRows(sg, req.GetInt("limit", 50), id)
	// group_by:"file" buckets the usages by the file each reference
	// originates in -- an opt-in shape for callers that want a
	// per-file rollup. The flat SubGraph stays the default so
	// existing consumers are unaffected.
	if gb := strings.ToLower(strings.TrimSpace(req.GetString("group_by", ""))); gb == "file" {
		return s.respondScopedJSONOrTOON(ctx, req, groupUsagesByFile(sg), resolved)
	}
	// compact is an explicit caller choice, so it beats the session's
	// gcx default — the same precedence returnSubGraph applies.
	if s.isGCX(ctx, req) && !isCompact(req) {
		res, err := s.gcxResponseWithBudget(req)(encodeFindUsages(sg, getter))
		return withScopeResult(res, err, resolved)
	}
	// Plain JSON gets curated usage rows that promote the resolved
	// from_type_flavor / from_ui_component to top-level node fields, so
	// they survive the meta-stripping degrade step. TOON / compact /
	// diagram formats go through the shared subgraph renderer.
	format := req.GetString("format", "")
	if !s.isTOON(ctx, req) && !isCompact(req) && format != "mermaid" && format != "dot" {
		sg.Nodes = s.withAbsPaths(ctx, sg.Nodes)
		return s.respondScopedJSONOrTOON(ctx, req, newUsageResponse(sg, getter), resolved)
	}
	return s.returnScopedSubGraph(ctx, req, sg, resolved)
}

// usageNode wraps a find_usages node with the resolved from_* fields
// promoted to top-level JSON (not nested under meta, so they survive
// the meta-stripping degrade step). It is an mcp-package projection —
// it never adds a field to the graph wire contract.
type usageNode struct {
	*graph.Node
	FromTypeFlavor  string `json:"from_type_flavor,omitempty"`
	FromUIComponent string `json:"from_ui_component,omitempty"`
}

// usageResponse is the curated find_usages JSON payload. It embeds the
// SubGraph (so caveat / totals / edges marshal unchanged) and shadows
// its Nodes with the from_*-enriched projection.
type usageResponse struct {
	*query.SubGraph
	Nodes []usageNode `json:"nodes"`
}

// attachNameOnlyCandidates records — and, under an explicit
// min_tier:"text_matched", returns — the call sites that name id but
// never bound to it.
//
// These sites are real calls the extractor saw and the resolver could not
// place: they sit on a bare-name `unresolved::` placeholder, so no
// reverse walk over id's in-edges can see them. Without the count, a
// symbol whose calls all failed to resolve answers "1 caller" and
// change_impact reports risk LOW for a change that breaks a dozen files.
// With it, the same answer reads "1 verified, 22 unverified".
//
// The rows stay behind the explicit opt-in because a name match may
// belong to any same-named symbol; only a caller who asked for the
// weakest tier has said that is the trade they want.
func (s *Server) attachNameOnlyCandidates(eng *query.Engine, sg *query.SubGraph, id, minTier string, opts query.QueryOptions) {
	if sg == nil || eng == nil {
		return
	}
	node := eng.GetSymbol(id)
	if node == nil {
		return
	}
	cands := eng.FindNameOnlyCandidates(node, sg.Edges, opts)
	sg.AttachNameOnlyCandidates(cands, minTier == graph.OriginTextMatched)
}

// attachSuppressionCaveat adds a human-readable rider to sg when the adaptive
// default hid text_matched edges AND the target's file is in the
// re-parsed-but-not-re-enriched window (see suppressionMayBeStale) — so a
// hidden-but-real usage is diagnosable instead of silently dropped. No-op when
// nothing was suppressed or the file is not stale; the existing
// text_matched_suppressed count carries the number in every response either way.
func (s *Server) attachSuppressionCaveat(ctx context.Context, sg *query.SubGraph, id string) {
	if sg == nil || sg.TextMatchedSuppressed == 0 {
		return
	}
	if !s.suppressionMayBeStale(ctx, id) {
		return
	}
	sg.SuppressionCaveat = fmt.Sprintf(
		"%d candidate usage(s) hidden pending re-verification; "+
			"pass min_tier:\"text_matched\" to see them",
		sg.TextMatchedSuppressed)
}

// suppressionMayBeStale reports whether id's file was re-parsed on the live
// watch path without re-running semantic enrichment — the window in which the
// resolver-verified edges that triggered text_matched suppression may sit
// below the enrichment tier, so the suppressed name-only usages could be the
// real ones. Reads graph.MetaReparsePendingEnrichment, which the indexer
// stamps on the file's KindFile node. Conservative: false whenever the marker
// is absent (no rider rather than a false alarm on a converged graph).
func (s *Server) suppressionMayBeStale(ctx context.Context, id string) bool {
	g := s.readerFor(ctx)
	n := g.GetNode(id)
	if n == nil || n.FilePath == "" {
		return false
	}
	for _, fn := range g.GetFileNodes(n.FilePath) {
		if fn.Kind != graph.KindFile {
			continue
		}
		v, ok := fn.Meta[graph.MetaReparsePendingEnrichment]
		b, _ := v.(bool)
		return ok && b
	}
	return false
}

// newUsageResponse resolves the from_* fields for each node (pure read
// — never mutates the graph) and wraps the SubGraph for JSON output.
func newUsageResponse(sg *query.SubGraph, g graph.NodeGetter) *usageResponse {
	wrapped := make([]usageNode, 0, len(sg.Nodes))
	for _, n := range sg.Nodes {
		tf, uc := usageFromFlavor(g, n.ID, n)
		wrapped = append(wrapped, usageNode{Node: n, FromTypeFlavor: tf, FromUIComponent: uc})
	}
	return &usageResponse{SubGraph: sg, Nodes: wrapped}
}

// annotateAndFilterUsageContext stamps each usage edge with its
// reference context (RefContextOf, using the origin node's kind) and, when
// contextFilter is non-empty, drops every usage whose context does not
// match — the engine behind `find_usages context:"parameter_type"`.
func annotateAndFilterUsageContext(sg *query.SubGraph, contextFilter string) {
	if sg == nil {
		return
	}
	nodeByID := make(map[string]*graph.Node, len(sg.Nodes))
	for _, n := range sg.Nodes {
		nodeByID[n.ID] = n
	}
	for _, e := range sg.Edges {
		var fromKind graph.NodeKind
		if fn := nodeByID[e.From]; fn != nil {
			fromKind = fn.Kind
		}
		e.Context = graph.RefContextOf(e, fromKind)
	}
	if contextFilter == "" {
		return
	}
	kept := sg.Edges[:0]
	for _, e := range sg.Edges {
		if e.Context == contextFilter {
			kept = append(kept, e)
		}
	}
	sg.Edges = kept
}

// annotateAndFilterReturnUsage copies the extractor-stamped
// return-usage label (Meta[MetaReturnUsage]) onto each usage edge's
// JSON-visible ReturnUsage field and, when usageFilter is non-empty,
// drops every usage whose label does not match — the engine behind
// `find_usages return_usage:"discarded"`. Edges without a label (non-
// call edges, unclassifiable sites) never match a filter.
func annotateAndFilterReturnUsage(sg *query.SubGraph, usageFilter string) {
	if sg == nil {
		return
	}
	for _, e := range sg.Edges {
		e.ReturnUsage = graph.ReturnUsageOf(e)
	}
	if usageFilter == "" {
		return
	}
	kept := sg.Edges[:0]
	for _, e := range sg.Edges {
		if e.ReturnUsage == usageFilter {
			kept = append(kept, e)
		}
	}
	sg.Edges = kept
}

// usageFromFlavor resolves the structural flavor and UI-component
// framework attributable to a usage edge's FROM site, for the
// find_usages flavor filter and the from_* surfacing. The component
// marker is read off the FROM node itself (React function components /
// Composables are KindFunction); the type flavor is read off the FROM
// node's enclosing owner type, because callers are functions / methods
// that never carry type_flavor themselves. Nil-safe: a FROM node absent
// from both the supplied lookup and the graph yields empty strings.
func usageFromFlavor(g graph.NodeGetter, fromID string, fromNode *graph.Node) (typeFlavor, uiComponent string) {
	if fromNode == nil && g != nil {
		fromNode = g.GetNode(fromID)
	}
	if fromNode == nil {
		return "", ""
	}
	if fromNode.Meta != nil {
		if uc, _ := fromNode.Meta["ui_component"].(string); uc != "" {
			uiComponent = uc
		}
	}
	if ownerID, _ := graph.EnclosingFromID(fromID, fromNode.Kind); ownerID != "" && g != nil {
		if owner := g.GetNode(ownerID); owner != nil && owner.Meta != nil {
			if tf, _ := owner.Meta["type_flavor"].(string); tf != "" {
				typeFlavor = tf
			}
		}
	}
	return typeFlavor, uiComponent
}

// filterUsagesByFlavor drops usage edges whose FROM site does not
// resolve to a matching structural flavor, then prunes nodes left with
// no incident edge (the queried target is always kept). An empty flavor
// argument is a no-op.
func filterUsagesByFlavor(g graph.NodeGetter, sg *query.SubGraph, targetID, flavorArg string) {
	flavors := splitFlavors(flavorArg)
	if sg == nil || len(flavors) == 0 {
		return
	}
	nodeByID := make(map[string]*graph.Node, len(sg.Nodes))
	for _, n := range sg.Nodes {
		nodeByID[n.ID] = n
	}
	kept := sg.Edges[:0]
	for _, e := range sg.Edges {
		tf, uc := usageFromFlavor(g, e.From, nodeByID[e.From])
		if flavorMatchesResolved(tf, uc, flavors) {
			kept = append(kept, e)
		}
	}
	sg.Edges = kept
	// Prune orphan nodes: keep the queried target plus every node still
	// incident to a surviving edge.
	referenced := map[string]struct{}{targetID: {}}
	for _, e := range sg.Edges {
		referenced[e.From] = struct{}{}
		referenced[e.To] = struct{}{}
	}
	keptNodes := sg.Nodes[:0]
	for _, n := range sg.Nodes {
		if _, ok := referenced[n.ID]; ok {
			keptNodes = append(keptNodes, n)
		}
	}
	sg.Nodes = keptNodes
}

// groupUsagesByFile buckets a find_usages SubGraph by the file each
// reference originates in. The `from` endpoint of every edge is the
// usage site; its file path is the bucket key and the from-node's
// name/ID is the enclosing symbol. Files are sorted by descending
// use count, then by path for a stable order.
func groupUsagesByFile(sg *query.SubGraph) map[string]any {
	nodeByID := make(map[string]*graph.Node, len(sg.Nodes))
	for _, n := range sg.Nodes {
		nodeByID[n.ID] = n
	}
	groups := map[string]*query.UsageFileGroup{}
	for _, e := range sg.Edges {
		from := nodeByID[e.From]
		file := e.FilePath
		if file == "" && from != nil {
			file = from.FilePath
		}
		if file == "" {
			file = "(unknown)"
		}
		g := groups[file]
		if g == nil {
			g = &query.UsageFileGroup{File: file}
			groups[file] = g
		}
		item := query.UsageGroupItem{Line: e.Line, EdgeKind: string(e.Kind), Context: e.Context, ReturnUsage: e.ReturnUsage}
		if from != nil {
			item.SymbolID = from.ID
			item.SymbolName = from.Name
		}
		g.Uses = append(g.Uses, item)
		g.Count++
	}
	out := make([]*query.UsageFileGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].File < out[j].File
	})
	grouped := map[string]any{
		"grouped_by": "file",
		"file_count": len(out),
		"total_uses": len(sg.Edges),
		"groups":     out,
		"truncated":  sg.Truncated,
	}
	// On a capped page total_uses counts only the rows below; carry the
	// full row count alongside so the cut stays legible in this shape too.
	if sg.Truncated {
		grouped["total_edges"] = sg.TotalEdges
	}
	return grouped
}

// truncateUsageRows enforces find_usages' advertised `limit` on the
// usage rows (edges). Totals keep the full post-filter counts and
// Truncated marks the cut, so a capped page is legible as partial
// rather than complete. Nodes no longer referenced by a surviving edge
// are pruned; the queried target always stays. A non-positive limit
// opts out of the cap.
func truncateUsageRows(sg *query.SubGraph, limit int, targetID string) {
	if sg == nil || limit <= 0 || len(sg.Edges) <= limit {
		return
	}
	sg.TotalEdges = len(sg.Edges)
	sg.TotalNodes = len(sg.Nodes)
	// One stable global order before the cut, so the kept subset is the
	// strongest evidence and identical across store backends.
	query.SortEdgesForPage(sg.Edges)
	sg.Edges = sg.Edges[:limit]
	referenced := map[string]struct{}{targetID: {}}
	for _, e := range sg.Edges {
		referenced[e.From] = struct{}{}
		referenced[e.To] = struct{}{}
	}
	nodes := make([]*graph.Node, 0, len(referenced))
	for _, n := range sg.Nodes {
		if _, ok := referenced[n.ID]; ok {
			nodes = append(nodes, n)
		}
	}
	sg.Nodes = nodes
	sg.Truncated = true
}

func (s *Server) handleGetCluster(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	resolved, errResult := s.resolveScope(ctx, req, IntentReach)
	if errResult != nil {
		return errResult, nil
	}
	opts := query.QueryOptions{
		Depth:       req.GetInt("radius", 2),
		Limit:       req.GetInt("limit", 50),
		Detail:      "brief",
		WorkspaceID: resolved.WorkspaceID,
		ProjectID:   resolved.ProjectID,
		RepoAllow:   resolved.RepoAllow,
	}
	sg := s.engineFor(ctx).GetCluster(id, opts)
	enrichSubGraphEdges(sg)
	return s.returnScopedSubGraph(ctx, req, sg, resolved)
}

func (s *Server) handleGraphStats(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return s.respondJSONOrTOON(ctx, req, s.buildGraphStatsPayload(ctx))
}

// buildGraphStatsPayload returns the same data the `graph_stats` tool
// emits. Shared with the `gortex://stats` resource so both surfaces
// stay byte-for-byte equal.
func (s *Server) buildGraphStatsPayload(ctx context.Context) map[string]any {
	stats := s.engineFor(ctx).Stats()
	result := map[string]any{
		"total_nodes": stats.TotalNodes,
		"total_edges": stats.TotalEdges,
		"by_kind":     stats.ByKind,
		"by_language": stats.ByLanguage,
	}

	// Provenance churn: how many times an in-graph edge's identity
	// changed because its Origin was upgraded or reverted. Surfaced
	// as the tamper-evidence signal — a non-zero value means edge
	// provenance moved since the graph was built.
	result["edge_identity_revisions"] = s.readerFor(ctx).EdgeIdentityRevisions()

	if s.multiIndexer != nil && s.multiIndexer.IsMultiRepo() {
		// The per-repo dump is bounded two ways. Each entry is only the
		// node/edge totals — no per-repo histogram and no edge join — so it
		// reads from the persisted counters instead of scanning the nodes
		// and edges tables. The number of entries is then capped with a
		// truncation marker. Both matter because the gortex://stats resource
		// is advertised "read at session start to orient": an agent reads it
		// on connect, and on a monorepo that decomposes into hundreds of
		// sub-repos an unbounded full-GraphStats dump overflowed the agent's
		// context window before any user turn (small repos:
		// IsMultiRepo()==false → no dump). Per-repo detail for one repo stays
		// available via graph_stats repo=<prefix>.
		result["per_repo"] = cappedRepoTotals(perRepoTotals(s.readerFor(ctx)), graphStatsPerRepoCap)
	}

	result["token_savings"] = s.tokenStatsFor(ctx).snapshot()

	if cs := s.cumulativeSavingsSnapshot(); cs != nil {
		result["cumulative_savings"] = cs
	}

	if s.semanticMgr != nil && s.semanticMgr.Enabled() {
		result["semantic"] = map[string]any{
			"enabled":   true,
			"providers": s.semanticMgr.Stats(),
		}
	}

	if ns := s.notificationsStatus(); ns != nil {
		result["notifications"] = ns
	}

	// Merkle-keyed RWR walk cache performance — surfaces whether the
	// incremental centrality cache is earning its keep (hit rate) and
	// how many distinct walks it retains.
	if hits, misses, size, capacity, enabled := s.pprCache.stats(); enabled && (hits+misses) > 0 {
		ppr := map[string]any{
			"hits":     hits,
			"misses":   misses,
			"size":     size,
			"capacity": capacity,
		}
		if total := hits + misses; total > 0 {
			ppr["hit_rate"] = round4(float64(hits) / float64(total))
		}
		result["ppr_cache"] = ppr
	}

	return result
}

// graphStatsPerRepoCap bounds how many per-repo entries the
// gortex://stats resource / graph_stats tool inlines. On a large monorepo
// gortex tracks hundreds of sub-repos; inlining an entry per repo into a
// resource that is read "at session start" overflows an agent's context
// window — the bug that made the MCP unusable on big monorepos.
const graphStatsPerRepoCap = 25

// repoTotal is one repository's whole-graph contribution by count. The
// stats dump reports these instead of a full per-repo GraphStats so the
// multi-repo payload stays counter-cheap: the persisted counters already
// hold the totals, so no per-repo node histogram or edge join is run.
type repoTotal struct {
	nodes int
	edges int
}

// perRepoTotals returns each repository's node and edge totals for the
// reader's view. A store or graph that maintains the persisted per-repo
// counters answers from them in O(repos) with no scan; any other reader —
// a composed overlay view — falls back to RepoStats, whose per-repo totals
// are already correct under composition.
func perRepoTotals(r graph.Reader) map[string]repoTotal {
	if c, ok := r.(interface {
		AllRepoMemoryEstimates() map[string]graph.RepoMemoryEstimate
	}); ok {
		est := c.AllRepoMemoryEstimates()
		out := make(map[string]repoTotal, len(est))
		for repo, e := range est {
			out[repo] = repoTotal{nodes: e.NodeCount, edges: e.EdgeCount}
		}
		return out
	}
	rs := r.RepoStats()
	out := make(map[string]repoTotal, len(rs))
	for repo, st := range rs {
		out[repo] = repoTotal{nodes: st.TotalNodes, edges: st.TotalEdges}
	}
	return out
}

// cappedRepoTotals renders per-repo totals into the stats payload:
// verbatim when the repo count is within the cap, otherwise the top-`limit`
// repos by node count plus a `_truncated` marker pointing at graph_stats
// repo=<prefix> for the rest. Keeps the payload bounded regardless of how
// many repos are tracked.
func cappedRepoTotals(totals map[string]repoTotal, limit int) map[string]any {
	entry := func(t repoTotal) map[string]any {
		return map[string]any{"total_nodes": t.nodes, "total_edges": t.edges}
	}
	out := make(map[string]any, len(totals)+1)
	if len(totals) <= limit {
		for k, v := range totals {
			out[k] = entry(v)
		}
		return out
	}
	type kv struct {
		name string
		t    repoTotal
	}
	arr := make([]kv, 0, len(totals))
	for k, v := range totals {
		arr = append(arr, kv{name: k, t: v})
	}
	sort.Slice(arr, func(i, j int) bool { return arr[i].t.nodes > arr[j].t.nodes })
	for i := 0; i < limit; i++ {
		out[arr[i].name] = entry(arr[i].t)
	}
	out["_truncated"] = map[string]any{
		"shown":       limit,
		"total_repos": len(totals),
		"note": fmt.Sprintf("per_repo capped to the top %d of %d tracked repos by node count "+
			"(context-frugal on monorepos); call graph_stats with repo=<prefix> for a specific repo.",
			limit, len(totals)),
	}
	return out
}

// notificationsStatus reports each push-notification channel's live
// subscriber count and last-published payload. nil when no broadcaster
// is wired (single-shot CLI modes). Consumed by graph_stats /
// gortex://stats so a debugging client can see who is subscribed and
// what the broadcasters last sent without standing up its own
// subscriber.
func (s *Server) notificationsStatus() map[string]any {
	out := map[string]any{}
	if s.diagBroadcaster != nil {
		out["diagnostics"] = map[string]any{
			"subscribers": s.diagBroadcaster.subscriberCount(),
		}
	}
	if s.readinessBroadcaster != nil {
		row := map[string]any{
			"subscribers": s.readinessBroadcaster.subscriberCount(),
		}
		if snap := s.readinessBroadcaster.snapshot(); snap != nil {
			row["last_state"] = snap
		}
		out["workspace_readiness"] = row
	}
	if s.healthBroadcaster != nil {
		row := map[string]any{
			"subscribers": s.healthBroadcaster.subscriberCount(),
		}
		if snap := s.healthBroadcaster.snapshot(); snap != nil {
			row["last_snapshot"] = snap
		}
		out["daemon_health"] = row
	}
	if s.staleRefsBroadcaster != nil {
		out["stale_refs"] = map[string]any{
			"subscribers": s.staleRefsBroadcaster.subscriberCount(),
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
