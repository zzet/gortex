package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	wire "github.com/gortexhq/gcx-go"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// isGCX reports whether the caller requested the GCX1 compact wire
// format for this tool call. Selection order:
//
//  1. Explicit `format` arg on the request — wins unconditionally.
//     `format: "json"` returns false even when the session default is
//     gcx, so an agent can opt out per-call when it needs a JSON
//     payload (e.g. piping to a non-GCX consumer).
//  2. Per-session default driven by the MCP `clientInfo.name` snooped
//     at handshake — claude-code / cursor / vscode / zed / etc. ship
//     with the @gortex/wire (or gcx-go) decoder so the daemon emits
//     gcx by default for them. Other clients fall through.
//  3. JSON fallback for unknown clients.
//
// The Server receiver is needed only for the session lookup; nil
// receiver collapses to "explicit arg only" semantics, matching the
// pre-session-default behaviour.
func (s *Server) isGCX(ctx context.Context, req mcp.CallToolRequest) bool {
	if v, ok := req.GetArguments()["format"].(string); ok && v != "" {
		return wire.ParseFormat(v) == wire.FormatGCX
	}
	if s == nil {
		return false
	}
	return s.resolveSessionFormat(ctx) == "gcx"
}

// gcxResponseWithBudget binds a request to the gcx-response builder.
// Returns a 2-arg function so it can be called inline with the
// encoder's `(payload []byte, err error)` return tuple — the same
// invocation shape as the legacy gcxResponse. Byte-level row-tail
// trimming kicks in when the caller opted into a budget (`max_bytes`
// or `paginate: true`); without opt-in the payload is forwarded
// untouched, so non-budgeted callers retain pre-budgeting behaviour
// (full result inline, transport-spilled if the harness cap fires).
func (s *Server) gcxResponseWithBudget(req mcp.CallToolRequest) func([]byte, error) (*mcp.CallToolResult, error) {
	budget := effectiveBudget(req)
	// The token-budget comment lands after the row trim; reserve its
	// bytes so the decorated payload stays in cap. A budget the reserve
	// itself cannot fit inside drops the comment instead of letting it
	// become the bytes that break the contract.
	reserve := tokenBudgetDecorationReserve(req)
	decorate := reserve == 0 || reserve < budget
	if reserve > 0 && decorate {
		budget -= reserve
	}
	return func(payload []byte, err error) (*mcp.CallToolResult, error) {
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("wire encode failed: %v", err)), nil
		}
		if budget > 0 {
			if trimmed, didTrim := trimGCXBytes(payload, budget); trimmed != nil {
				payload = trimmed
				if didTrim && decorate {
					payload = decorateTokenBudgetGCX(payload, req)
				}
			}
		}
		return mcp.NewToolResultText(string(payload)), nil
	}
}

// newGCX creates an encoder writing to w with the given tool + fields.
// Encoders own their header so section layout stays visible at the
// call site.
func newGCX(w *bytes.Buffer, tool string, fields []string, metaKV ...string) *wire.Encoder {
	meta := map[string]string{}
	for i := 0; i+1 < len(metaKV); i += 2 {
		// A GCX header is a space-separated run of key=value tokens and the
		// wire escaper does not encode spaces, so a value containing one
		// splits into a token with no "=" and the decoder rejects the whole
		// payload — losing the rows as well as the meta. Prose therefore
		// cannot ride the header: emit it as a section row (see
		// writeNotesSection). Dropping the pair here is the backstop that
		// keeps a payload decodable if a new prose meta slips in.
		if strings.ContainsAny(metaKV[i], " \t\n") || strings.ContainsAny(metaKV[i+1], " \t\n") {
			continue
		}
		meta[metaKV[i]] = metaKV[i+1]
	}
	return wire.NewEncoder(w, wire.Header{
		Tool:   tool,
		Fields: fields,
		Meta:   meta,
	})
}

// nodeShort returns the short form of a node ID — whatever follows
// the last "::" separator. For methods this carries the receiver; for
// functions / types it is the plain name.
func nodeShort(n *graph.Node) string {
	if n == nil {
		return ""
	}
	// A prose-section node's ID tail is the slugified heading path
	// ("doc:readme-setup-build") -- noise. Its Name is the readable
	// breadcrumb, so use that.
	if n.Kind == graph.KindDoc {
		return n.Name
	}
	if idx := strings.LastIndex(n.ID, "::"); idx >= 0 {
		return n.ID[idx+2:]
	}
	return n.Name
}

// nodeSig returns the rendered signature string for a node, falling
// back to "" when no signature was extracted.
func nodeSig(n *graph.Node) string {
	if n == nil || n.Meta == nil {
		return ""
	}
	// Prose-section nodes carry no signature -- surface a short
	// snippet of the section body in the sig column instead, so a
	// docs hit is self-describing in the compact GCX output.
	if n.Kind == graph.KindDoc {
		if txt, ok := n.Meta["section_text"].(string); ok && txt != "" {
			const snippetCap = 160
			if len(txt) > snippetCap {
				return txt[:snippetCap] + "\u2026"
			}
			return txt
		}
		return ""
	}
	if s, ok := n.Meta["signature"].(string); ok {
		return s
	}
	return ""
}

// nodeIsTest reports whether a node was flagged as a test by the
// indexer's test-edge pass (Meta["is_test"]).
func nodeIsTest(n *graph.Node) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	v, _ := n.Meta["is_test"].(bool)
	return v
}

// nodeTestRole returns the node's specific test role — "test",
// "benchmark", "fuzz", or "example" — or "" for non-test nodes.
func nodeTestRole(n *graph.Node) string {
	if n == nil || n.Meta == nil {
		return ""
	}
	r, _ := n.Meta["test_role"].(string)
	return r
}

// nodeTypeFlavor returns the node's structural flavor (class / struct /
// enum / interface / …) stamped by the language extractors, or "".
func nodeTypeFlavor(n *graph.Node) string {
	if n == nil || n.Meta == nil {
		return ""
	}
	v, _ := n.Meta["type_flavor"].(string)
	return v
}

// nodeUIComponent returns the node's UI-component framework name
// (react / vue / svelte / swiftui / …) stamped on a detected component
// node, or "".
func nodeUIComponent(n *graph.Node) string {
	if n == nil || n.Meta == nil {
		return ""
	}
	v, _ := n.Meta["ui_component"].(string)
	return v
}

// nodeTestRunner returns the node's resolved test-runner identifier —
// "mocha" / "bun-test" / "jest" / "vitest" / "node-test" / "playwright"
// / "cypress" / "gotest" / "pytest" / "unittest" / "rspec" / "minitest"
// — or "" when the test-edge pass found no signal. Surfaced on the
// listing rows alongside is_test / test_role so agents can scope test
// pickers per runner (e.g. `--testNamePattern` vs `--grep`).
func nodeTestRunner(n *graph.Node) string {
	if n == nil || n.Meta == nil {
		return ""
	}
	r, _ := n.Meta["test_runner"].(string)
	return r
}

// shouldSkipGraphNode filters File and Import pseudo-nodes the way the
// legacy compact / TOON formatters do — they add noise without
// informational value in symbol-oriented outputs.
func shouldSkipGraphNode(n *graph.Node) bool {
	if n == nil {
		return true
	}
	return n.Kind == graph.KindFile || n.Kind == graph.KindImport
}

// --------------------------------------------------------------------
// Hand-tuned encoders for the top-10 hot-path tools.
// --------------------------------------------------------------------

// encodeWinnowSymbols emits one row per ranked hit with per-axis score
// contributions. The contributions column is a pipe-separated list of
// `axis=value` pairs so decoders can recover the attribution without a
// nested structure.
func encodeWinnowSymbols(rows []winnowResult, total, limit int, weights map[string]float64) ([]byte, error) {
	truncated := total > limit
	if weights == nil {
		weights = winnowAxisWeights
	}
	var buf bytes.Buffer
	enc := newGCX(&buf, "winnow_symbols",
		[]string{"id", "kind", "name", "path", "line", "sig", "score", "fan_in", "fan_out", "churn", "community", "contributions", "is_test", "test_role", "test_runner"},
		"total", fmt.Sprintf("%d", total),
		"truncated", boolString(truncated),
		"weights", formatAxisWeights(weights),
	)
	if err := enc.WriteComment(fmt.Sprintf("%d result(s)", len(rows))); err != nil {
		return nil, err
	}
	for _, r := range rows {
		if shouldSkipGraphNode(r.Node) {
			continue
		}
		if err := enc.WriteRow(
			r.Node.ID,
			string(r.Node.Kind),
			nodeShort(r.Node),
			r.Node.FilePath,
			r.Node.StartLine,
			nodeSig(r.Node),
			roundFloat(r.Score),
			r.FanIn,
			r.FanOut,
			r.Churn,
			r.Community,
			formatContributions(r.Contributions),
			nodeIsTest(r.Node),
			nodeTestRole(r.Node),
			nodeTestRunner(r.Node),
		); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// formatContributions renders the per-axis attribution map as a stable
// pipe-separated key=value list (sorted by key for determinism).
func formatContributions(m map[string]float64) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('|')
		}
		fmt.Fprintf(&b, "%s=%.3f", k, m[k])
	}
	return b.String()
}

// formatAxisWeights mirrors formatContributions for the meta header.
func formatAxisWeights(m map[string]float64) string {
	return formatContributions(m)
}

// encodeSearchSymbols emits one row per search hit with the minimum
// fields an agent needs to decide whether to fetch more detail.
func encodeSearchSymbols(nodes []*graph.Node, total, limit int) ([]byte, error) {
	truncated := total > limit
	if len(nodes) > limit {
		nodes = nodes[:limit]
	}
	var buf bytes.Buffer
	enc := newGCX(&buf, "search_symbols",
		[]string{"id", "kind", "name", "path", "path_abs", "line", "sig", "enclosing", "is_test", "test_role", "test_runner", "type_flavor", "ui_component"},
		"total", fmt.Sprintf("%d", total),
		"truncated", boolString(truncated),
	)
	if err := enc.WriteComment(fmt.Sprintf("%d result(s)", len(nodes))); err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if shouldSkipGraphNode(n) {
			continue
		}
		_, encName := graph.EnclosingFromID(n.ID, n.Kind)
		if err := enc.WriteRow(
			n.ID,
			string(n.Kind),
			nodeShort(n),
			n.FilePath,
			n.AbsoluteFilePath,
			n.StartLine,
			nodeSig(n),
			encName,
			nodeIsTest(n),
			nodeTestRole(n),
			nodeTestRunner(n),
			nodeTypeFlavor(n),
			nodeUIComponent(n),
		); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// encodeGetSymbolSource emits a single row carrying the node
// metadata plus the full source. Source is escaped line-by-line.
func encodeGetSymbolSource(node *graph.Node, source string, fromLine int, etag, omissionKinds string) ([]byte, error) {
	var buf bytes.Buffer
	meta := []string{"etag", etag}
	if omissionKinds != "" {
		meta = append(meta, "omissions", omissionKinds)
	}
	enc := newGCX(&buf, "get_symbol_source",
		[]string{"id", "kind", "name", "path", "start_line", "end_line", "from_line", "sig", "etag", "source"},
		meta...,
	)
	if err := enc.WriteRow(
		node.ID,
		string(node.Kind),
		node.Name,
		node.FilePath,
		node.StartLine,
		node.EndLine,
		fromLine,
		nodeSig(node),
		etag,
		source,
	); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

// encodeBatchSymbols emits one row per requested symbol; missing
// symbols carry an error cell instead of a real node.
func encodeBatchSymbols(rows []map[string]any, includeSource bool) ([]byte, error) {
	fields := []string{"id", "kind", "name", "path", "start_line", "end_line", "sig"}
	if includeSource {
		fields = append(fields, "source")
	}
	fields = append(fields, "error")
	var buf bytes.Buffer
	enc := newGCX(&buf, "batch_symbols", fields,
		"count", fmt.Sprintf("%d", len(rows)),
	)
	for _, r := range rows {
		values := []any{
			str(r["id"]),
			str(r["kind"]),
			str(r["name"]),
			str(r["file_path"]),
			r["start_line"],
			r["end_line"],
			str(r["signature"]),
		}
		if includeSource {
			values = append(values, str(r["source"]))
		}
		values = append(values, str(r["error"]))
		if err := enc.WriteRow(values...); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// zeroEdgeCaveatMeta renders a zero-edge caveat's machine-checkable class
// as a GCX header meta pair. Returns nil when the caveat is absent so an
// uncaveated result carries no extra meta.
//
// The prose message is deliberately NOT here — it rides the
// `<tool>.notes` section instead. It used to be emitted as
// `caveat_message=<sentence>`, which made every caveated GCX payload
// undecodable (see newGCX), so the safety wording an agent needs before
// deleting a symbol was silently destroyed by the very format meant to
// deliver it compactly.
func zeroEdgeCaveatMeta(c *graph.ZeroEdgeCaveat) []string {
	if c == nil {
		return nil
	}
	return []string{"caveat", string(c.Class)}
}

// writeNotesSection emits a `<tool>.notes` section carrying the prose an
// agent has to read alongside the rows: the zero-edge / weak-evidence
// classification, the note explaining hidden text matches, and the
// related-tools discovery cue. `kind` names the channel, `class` carries
// the machine-checkable ZeroEdgeClass on the caveat row, `message` the
// sentence. Nothing is written when the result carries none of them, so
// unannotated payloads keep their existing bytes.
//
// Prose belongs in rows rather than header meta because a row value is
// tab-delimited and properly escaped, while a header value cannot contain
// a space at all. The related-tools cue is here rather than in the header
// for a second reason: markCueOnce spends its once-per-session budget
// before the encoder runs, so a cue dropped at encode time is never
// re-emitted.
func writeNotesSection(buf *bytes.Buffer, tool string, sg *query.SubGraph) error {
	if sg == nil || (sg.Caveat == nil && sg.SuppressionCaveat == "" && sg.RelatedTools == "") {
		return nil
	}
	enc := newGCX(buf, tool+".notes", []string{"kind", "class", "message"})
	if sg.Caveat != nil {
		if err := enc.WriteRow("caveat", string(sg.Caveat.Class), sg.Caveat.Message); err != nil {
			return err
		}
	}
	if sg.SuppressionCaveat != "" {
		if err := enc.WriteRow("text_matched_suppressed", "", sg.SuppressionCaveat); err != nil {
			return err
		}
	}
	if sg.RelatedTools != "" {
		if err := enc.WriteRow("related_tools", "", sg.RelatedTools); err != nil {
			return err
		}
	}
	return enc.Close()
}

// tierFilteredCaveatMeta lowers a min_tier-filtered caveat into GCX meta so a
// tier-emptied result reads as "filtered", not "no usages".
func tierFilteredCaveatMeta(c *graph.TierFilteredCaveat) []string {
	if c == nil {
		return nil
	}
	return []string{
		"tier_filtered", c.Class,
		"edges_below_min_tier", fmt.Sprintf("%d", c.EdgesBelowMinTier),
		"max_available_tier", c.MaxAvailableTier,
	}
}

// encodeFindUsages emits one row per usage edge. Each row names the
// caller symbol, its location, the edge kind, and the origin tier so
// agents can filter without a second call.
func encodeFindUsages(sg *query.SubGraph, g graph.NodeGetter) ([]byte, error) {
	var buf bytes.Buffer
	meta := []string{"edges", fmt.Sprintf("%d", len(sg.Edges))}
	// Truncation meta rides only on a capped result so an uncapped
	// response keeps its wire shape byte-for-byte.
	if sg.Truncated {
		meta = append(meta,
			"truncated", "true",
			"total_edges", fmt.Sprintf("%d", sg.TotalEdges),
		)
	}
	meta = append(meta, zeroEdgeCaveatMeta(sg.Caveat)...)
	meta = append(meta, tierFilteredCaveatMeta(sg.TierFiltered)...)
	if sg.TextMatchedSuppressed > 0 {
		meta = append(meta, "text_matched_suppressed", fmt.Sprintf("%d", sg.TextMatchedSuppressed))
	}
	if s := sg.UsageSummary; s != nil {
		// Completeness rollup, same three numbers as the JSON / TOON
		// usage_summary so an agent reads the split without re-grepping
		// tests. n_refs mirrors edges above; both are kept so the summary
		// is self-contained across wire formats.
		meta = append(meta,
			"n_refs", fmt.Sprintf("%d", s.NRefs),
			"n_files", fmt.Sprintf("%d", s.NFiles),
			"n_test_refs", fmt.Sprintf("%d", s.NTestRefs),
		)
	}
	enc := newGCX(&buf, "find_usages",
		[]string{"from", "to", "edge_kind", "context", "return_usage", "origin", "tier", "confidence", "from_name", "from_path", "from_line", "from_is_test", "from_test_role", "from_test_runner", "from_type_flavor", "from_ui_component"},
		meta...,
	)
	nodeIdx := indexNodes(sg.Nodes)
	for _, e := range sg.Edges {
		fn := nodeIdx[e.From]
		var fname, fpath string
		var fline int
		if fn != nil {
			fname = nodeShort(fn)
			fpath = fn.FilePath
			fline = fn.StartLine
		}
		// Prefer the edge's own call-site line over the enclosing
		// symbol's start. Two calls from the same caller used to
		// surface as duplicate rows pinned to the caller's first
		// line; the edge line is what the agent actually wants to
		// jump to.
		if e.Line > 0 {
			fline = e.Line
		}
		if e.FilePath != "" {
			fpath = e.FilePath
		}
		tier := e.Tier
		if tier == "" {
			tier = graph.ResolvedBy(e.Origin)
		}
		ftf, fuc := usageFromFlavor(g, e.From, fn)
		if err := enc.WriteRow(
			e.From, e.To, string(e.Kind), e.Context, e.ReturnUsage, e.Origin, tier, e.Confidence,
			fname, fpath, fline, graph.NodeIsTest(g, fn), nodeTestRole(fn), nodeTestRunner(fn), ftf, fuc,
		); err != nil {
			return nil, err
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	if err := writeNotesSection(&buf, "find_usages", sg); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// encodeSubGraph is a shared encoder for the edge-returning traversal
// tools (get_callers, get_call_chain, get_dependencies, get_dependents,
// find_implementations). Emits two sections — nodes then edges — plus a
// third caller_notes section when get_callers attached concurrency
// annotations. The third section is omitted entirely when empty so the
// other traversal tools' wire output is byte-identical to before.
func encodeSubGraph(tool string, sg *query.SubGraph, g graph.NodeGetter) ([]byte, error) {
	var buf bytes.Buffer
	nodes := make([]*graph.Node, 0, len(sg.Nodes))
	for _, n := range sg.Nodes {
		if shouldSkipGraphNode(n) {
			continue
		}
		nodes = append(nodes, n)
	}
	nodeMeta := []string{
		"total", fmt.Sprintf("%d", sg.TotalNodes),
		"truncated", boolString(sg.Truncated),
	}
	// Budgeted traversals (walk_graph) ride two extra meta fields. They
	// are emitted only when meaningful so non-budgeted subgraphs keep
	// their existing wire shape byte-for-byte.
	if sg.BudgetHit {
		nodeMeta = append(nodeMeta, "budget_hit", boolString(sg.BudgetHit))
	}
	if sg.StoppedAtDepth > 0 {
		nodeMeta = append(nodeMeta, "stopped_at_depth", fmt.Sprintf("%d", sg.StoppedAtDepth))
	}
	// Epistemic lower-bound flag rides the node meta only when set, so a
	// result with no dispatch boundary keeps its wire shape byte-for-byte.
	if sg.LowerBound {
		nodeMeta = append(nodeMeta, "lower_bound", boolString(sg.LowerBound))
	}
	nodeEnc := newGCX(&buf, tool+".nodes",
		[]string{"id", "kind", "name", "path", "path_abs", "line", "is_test", "test_role", "test_runner"},
		nodeMeta...,
	)
	for _, n := range nodes {
		if err := nodeEnc.WriteRow(n.ID, string(n.Kind), nodeShort(n), n.FilePath, n.AbsoluteFilePath, n.StartLine, graph.NodeIsTest(g, n), nodeTestRole(n), nodeTestRunner(n)); err != nil {
			return nil, err
		}
	}
	if err := nodeEnc.Close(); err != nil {
		return nil, err
	}
	edgeMeta := []string{"count", fmt.Sprintf("%d", len(sg.Edges))}
	edgeMeta = append(edgeMeta, zeroEdgeCaveatMeta(sg.Caveat)...)
	if sg.TextMatchedSuppressed > 0 {
		edgeMeta = append(edgeMeta, "text_matched_suppressed", fmt.Sprintf("%d", sg.TextMatchedSuppressed))
	}
	edgeEnc := newGCX(&buf, tool+".edges",
		// line + file_path on the edge let the caller distinguish two
		// call sites with the same (from, to, kind). Without them
		// walk_graph / get_callers / get_call_chain etc. surfaced
		// duplicate-looking rows that an agent couldn't tell apart and
		// couldn't jump to.
		[]string{"from", "to", "kind", "origin", "tier", "via", "confidence", "label", "line", "file_path"},
		edgeMeta...,
	)
	for _, e := range sg.Edges {
		label := e.ConfidenceLabel
		if label == "" {
			label = graph.ConfidenceLabelFor(e.Kind, e.Confidence)
		}
		tier := e.Tier
		if tier == "" {
			tier = graph.ResolvedBy(e.Origin)
		}
		via := e.Via
		if via == "" && e.Meta != nil {
			if v, _ := e.Meta["via"].(string); v != "" {
				via = graph.ViaLabelFor(v)
			}
		}
		if err := edgeEnc.WriteRow(e.From, e.To, string(e.Kind), e.Origin, tier, via, e.Confidence, label, e.Line, e.FilePath); err != nil {
			return nil, err
		}
	}
	if err := edgeEnc.Close(); err != nil {
		return nil, err
	}
	if err := writeNotesSection(&buf, tool, sg); err != nil {
		return nil, err
	}
	// caller_notes — one row per caller carrying a concurrency flag. Emitted
	// only when get_callers attached annotations, so other traversal tools'
	// wire output is byte-identical to before.
	if len(sg.CallerNotes) > 0 {
		noteIDs := make([]string, 0, len(sg.CallerNotes))
		for id := range sg.CallerNotes {
			noteIDs = append(noteIDs, id)
		}
		sort.Strings(noteIDs)
		noteEnc := newGCX(&buf, tool+".caller_notes",
			[]string{"id", "sync_guarded", "sync_guarded_why", "cross_concurrent", "cross_concurrent_why"},
			"count", fmt.Sprintf("%d", len(noteIDs)),
		)
		for _, id := range noteIDs {
			a := sg.CallerNotes[id]
			if err := noteEnc.WriteRow(id, a.SyncGuarded, a.SyncGuardedWhy, a.CrossConcurrent, a.CrossConcurrentWhy); err != nil {
				return nil, err
			}
		}
		if err := noteEnc.Close(); err != nil {
			return nil, err
		}
	}
	// boundaries — the epistemic lower-bound diagnosis. Emitted only when the
	// walk crossed a dynamic-dispatch / unresolved site, so results with none
	// keep their existing wire bytes.
	if len(sg.Boundaries) > 0 {
		bEnc := newGCX(&buf, tool+".boundaries",
			[]string{"seed_id", "target", "edge_kind", "reason", "direction"},
			"count", fmt.Sprintf("%d", len(sg.Boundaries)),
		)
		for _, b := range sg.Boundaries {
			if err := bEnc.WriteRow(b.SeedID, b.Target, b.EdgeKind, string(b.Reason), b.Direction); err != nil {
				return nil, err
			}
		}
		if err := bEnc.Close(); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// encodeFileSummary emits one row per symbol in a file plus a trailing
// edge-distribution comment. Pulls the edge total from sg.TotalEdges
// rather than len(sg.Edges) so the count-only handler path (which
// leaves the Edge slice nil to avoid materialising every adjacent
// edge over cgo) still reports the right number.
func encodeFileSummary(sg *query.SubGraph, etag string) ([]byte, error) {
	var buf bytes.Buffer
	totalEdges := sg.TotalEdges
	if totalEdges == 0 {
		totalEdges = len(sg.Edges)
	}
	enc := newGCX(&buf, "get_file_summary",
		[]string{"id", "kind", "name", "line", "sig"},
		"total_nodes", fmt.Sprintf("%d", sg.TotalNodes),
		"total_edges", fmt.Sprintf("%d", totalEdges),
		"truncated", boolString(sg.Truncated),
		"etag", etag,
	)
	for _, n := range sg.Nodes {
		if shouldSkipGraphNode(n) {
			continue
		}
		if err := enc.WriteRow(n.ID, string(n.Kind), nodeShort(n), n.StartLine, nodeSig(n)); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// encodeAnalyze routes to per-kind encoders. The `kind` field is
// placed in the meta header rather than per-row so consumers can
// dispatch without inspecting every row.
func encodeAnalyze(kind string, payload any) ([]byte, error) {
	var buf bytes.Buffer
	switch kind {
	case "dead_code":
		items, _ := payload.([]deadCodeItem)
		enc := newGCX(&buf, "analyze.dead_code",
			[]string{"id", "kind", "name", "path", "line", "reason"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.ID, it.Kind, it.Name, it.Path, it.Line, it.Reason); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "hotspots":
		items, _ := payload.([]hotspotItem)
		enc := newGCX(&buf, "analyze.hotspots",
			[]string{"id", "name", "path", "line", "fan_in", "fan_out", "cross_cut", "betweenness", "score"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.ID, it.Name, it.Path, it.Line, it.FanIn, it.FanOut, it.CrossCommunity, it.Betweenness, it.Score); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "cycles":
		cycles, _ := payload.([]cycleItem)
		enc := newGCX(&buf, "analyze.cycles",
			[]string{"size", "severity", "nodes"},
			"count", fmt.Sprintf("%d", len(cycles)),
		)
		for _, c := range cycles {
			if err := enc.WriteRow(c.Size, c.Severity, strings.Join(c.Nodes, ",")); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "def_use":
		items, _ := payload.([]defUseItem)
		enc := newGCX(&buf, "analyze.def_use",
			[]string{"symbol", "var", "use_line", "use_text", "def_lines"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.Symbol, it.Var, it.UseLine, it.UseText, it.DefLines); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "channel_ops":
		items, _ := payload.([]channelOpItem)
		enc := newGCX(&buf, "analyze.channel_ops",
			[]string{"channel", "sends", "recvs", "senders", "receivers"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.Channel, it.Sends, it.Recvs, it.Senders, it.Receivers); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "goroutine_spawns":
		items, _ := payload.([]spawnItem)
		enc := newGCX(&buf, "analyze.goroutine_spawns",
			[]string{"target", "mode", "spawns", "spawners", "sync_guarded", "sync_guarded_why", "cross_concurrent", "cross_concurrent_why"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.Target, it.Mode, it.Spawns, it.Spawners, it.SyncGuarded, it.SyncGuardedWhy, it.CrossConcurrent, it.CrossConcurrentWhy); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "field_writers":
		items, _ := payload.([]fieldWriterItem)
		enc := newGCX(&buf, "analyze.field_writers",
			[]string{"field", "writes", "writers"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.Field, it.Writes, it.Writers); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "race_writes":
		items, _ := payload.([]raceWriteItem)
		enc := newGCX(&buf, "analyze.race_writes",
			[]string{"field", "writer", "file", "line", "reason"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.Field, it.Writer, it.FilePath, it.Line, it.Reason); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "unclosed_channels":
		items, _ := payload.([]unclosedChannelItem)
		enc := newGCX(&buf, "analyze.unclosed_channels",
			[]string{"channel", "file", "line", "sends", "recvs", "senders", "risk", "reason"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.Channel, it.FilePath, it.Line, it.Sends, it.Recvs, it.Senders, it.Risk, it.Reason); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "annotation_users":
		items, _ := payload.([]annotatedItem)
		enc := newGCX(&buf, "analyze.annotation_users",
			[]string{"symbol", "file", "line", "args"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.Symbol, it.File, it.Line, it.Args); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "annotation_users.list":
		items, _ := payload.([]annotationItem)
		enc := newGCX(&buf, "analyze.annotation_users.list",
			[]string{"id", "name", "users"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.ID, it.Name, it.Users); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "config_readers":
		items, _ := payload.([]configReaderItem)
		enc := newGCX(&buf, "analyze.config_readers",
			[]string{"id", "name", "source", "reads", "readers"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.ID, it.Name, it.Source, it.Reads, it.Readers); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "event_emitters":
		items, _ := payload.([]eventEmitterItem)
		enc := newGCX(&buf, "analyze.event_emitters",
			[]string{"id", "name", "event_kind", "emits", "emitters"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.ID, it.Name, it.Kind, it.Emits, it.Emitters); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "pubsub":
		items, _ := payload.([]pubsubItem)
		enc := newGCX(&buf, "analyze.pubsub",
			[]string{"id", "name", "transport", "publishes", "subscribes", "publishers", "subscribers"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.ID, it.Name, it.Transport, it.Publishes, it.Subscribes, it.Publishers, it.Subscribers); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "string_emitters":
		items, _ := payload.([]stringEmitterItem)
		enc := newGCX(&buf, "analyze.string_emitters",
			[]string{"id", "context", "value", "emits", "emitters"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.ID, it.Context, it.Value, it.Emits, it.Emitters); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "error_surface":
		items, _ := payload.([]errorSurfaceItem)
		enc := newGCX(&buf, "analyze.error_surface",
			[]string{"symbol", "file", "line", "throws", "errors", "error_msgs"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.Symbol, it.File, it.Line, it.Throws, it.Errors, it.ErrorMsgs); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "log_events":
		items, _ := payload.([]logEventItem)
		enc := newGCX(&buf, "analyze.log_events",
			[]string{"id", "value", "level", "emits", "emitters"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.ID, it.Value, it.Level, it.Emits, it.Emitters); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "sql_rebuild":
		items, _ := payload.([]sqlRebuildItem)
		enc := newGCX(&buf, "analyze.sql_rebuild",
			[]string{"strings_visited", "tables_created", "columns_created", "query_edges", "reads_col_edges", "writes_col_edges", "emitters_linked", "skipped"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.StringsVisited, it.TablesCreated, it.ColumnsCreated, it.QueryEdges, it.ReadColEdges, it.WriteColEdges, it.EmittersLinked, it.Skipped); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "cross_repo":
		items, _ := payload.([]crossRepoItem)
		enc := newGCX(&buf, "analyze.cross_repo",
			[]string{"from_repo", "to_repo", "kind", "count", "samples"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.FromRepo, it.ToRepo, it.Kind, it.Count, it.Samples); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "unsafe_patterns":
		items, _ := payload.([]unsafePatternItem)
		enc := newGCX(&buf, "analyze.unsafe_patterns",
			[]string{"detector", "severity", "language", "file", "line", "symbol", "text"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.Detector, it.Severity, it.Language, it.File, it.Line, it.Symbol, it.Text); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "health_score":
		items, _ := payload.([]healthScoreItem)
		enc := newGCX(&buf, "analyze.health_score",
			[]string{
				"id", "name", "kind", "file", "line",
				"score", "grade",
				"coverage_pct", "complexity_pct", "recency_pct", "churn_pct",
				"fan_in", "fan_out", "crossings",
				"age_days", "mods", "axes_used",
			},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(
				it.ID, it.Name, it.Kind, it.File, it.Line,
				it.Score, it.Grade,
				it.CoveragePct, it.ComplexityPct, it.RecencyPct, it.ChurnPct,
				it.FanIn, it.FanOut, it.Crossings,
				it.AgeDays, it.Mods, it.AxesUsed,
			); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "health_score.rollup":
		items, _ := payload.([]healthRollupItem)
		enc := newGCX(&buf, "analyze.health_score.rollup",
			[]string{
				"scope", "key",
				"avg_score", "min_score", "max_score",
				"symbols", "grade",
				"count_a", "count_b", "count_c", "count_d", "count_f",
			},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(
				it.Scope, it.Key,
				it.AvgScore, it.MinScore, it.MaxScore,
				it.Symbols, it.Grade,
				it.CountA, it.CountB, it.CountC, it.CountD, it.CountF,
			); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "impact.target":
		p, _ := payload.(impactTargetPayload)
		if p.Target == nil {
			p.Target = &impactTargetScope{}
		}
		// The closure width and its exactness ride in the header: a
		// caller who sees only rows cannot tell a complete blast radius
		// from a bounded one.
		meta := []string{
			"target", p.Target.label(),
			"dependents", fmt.Sprintf("%d", p.Target.Total),
			"count", fmt.Sprintf("%d", len(p.Rows)),
		}
		switch {
		case p.Target.LowerBound || p.Target.Truncated:
			meta = append(meta, "lower_bound", "true")
		case p.Target.Total == 0:
			// Header values are single tokens, so the flag stands in for
			// the sentence the JSON envelope spells out: an empty closure
			// is unproven, not proof that nothing depends on the target.
			meta = append(meta, "zero_dependents_unproven", "true")
		}
		enc := newGCX(&buf, "analyze.impact.target",
			[]string{"depth", "risk", "score", "id", "name", "kind", "file", "line", "fan_in", "reach_count"},
			meta...,
		)
		for _, r := range p.Rows {
			if err := enc.WriteRow(r.Depth, r.Risk, r.Score, r.ID, r.Name, r.Kind, r.File, r.Line, r.FanIn, r.ReachCount); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	default:
		// Fall back to generic so analyze variants without a hand-tuned
		// encoder still produce valid GCX instead of failing.
		if err := wire.EncodeAny(&buf, "analyze."+kind, payload); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
}

// Narrow row contracts used by analyze.* encoders so the caller can
// build them from heterogeneous internal structs.
type deadCodeItem struct {
	ID, Kind, Name, Path, Reason string
	Line                         int
}

type hotspotItem struct {
	ID, Name, Path string
	Line, FanIn    int
	FanOut         int
	CrossCommunity int
	Betweenness    float64
	Score          float64
}

type cycleItem struct {
	Size     int
	Severity string
	Nodes    []string
}

// Row contracts for the edge-driven analyzers (see
// tools_analyze_edges.go). Values are pre-flattened (slices joined)
// so the GCX encoder writes one scalar column per field.
type channelOpItem struct {
	Channel   string
	Sends     int
	Recvs     int
	Senders   string
	Receivers string
}

type spawnItem struct {
	Target string
	Mode   string
	Spawns int
	// Spawners is a comma-joined list of caller node IDs.
	Spawners string
	// Concurrency columns carry the shared classification of the
	// spawned target — sync_guarded (goroutine body on a lock-holding
	// type) and cross_concurrent (reached across a concurrency
	// boundary). Empty when the target carries neither flag.
	SyncGuarded        bool
	SyncGuardedWhy     string
	CrossConcurrent    bool
	CrossConcurrentWhy string
}

type fieldWriterItem struct {
	Field   string
	Writes  int
	Writers string
}

// raceWriteItem is one row of the `race_writes` analyzer: a field
// write that fires from inside a goroutine-reachable function with
// no detected lock guard.
type raceWriteItem struct {
	Field    string
	Writer   string
	FilePath string
	Line     int
	Reason   string
}

// unclosedChannelItem is one row of the `unclosed_channels`
// analyzer: a channel that takes sends but nobody (sender or
// receiver) calls close() on.
type unclosedChannelItem struct {
	Channel  string
	FilePath string
	Line     int
	Sends    int
	Recvs    int
	Senders  int
	Risk     string
	Reason   string
}

type annotatedItem struct {
	Symbol string
	File   string
	Line   int
	Args   string
}

type annotationItem struct {
	ID    string
	Name  string
	Users int
}

type configReaderItem struct {
	ID      string
	Name    string
	Source  string
	Reads   int
	Readers string
}

type eventEmitterItem struct {
	ID       string
	Name     string
	Kind     string
	Emits    int
	Emitters string
}

type pubsubItem struct {
	ID          string
	Name        string
	Transport   string
	Publishes   int
	Subscribes  int
	Publishers  string
	Subscribers string
}

type errorSurfaceItem struct {
	Symbol    string
	File      string
	Line      int
	Throws    int
	Errors    string
	ErrorMsgs string
}

type logEventItem struct {
	ID       string
	Value    string
	Level    string
	Emits    int
	Emitters string
}

type sqlRebuildItem struct {
	StringsVisited int
	TablesCreated  int
	ColumnsCreated int
	QueryEdges     int
	ReadColEdges   int
	WriteColEdges  int
	EmittersLinked int
	Skipped        int
}

type crossRepoItem struct {
	FromRepo string
	ToRepo   string
	Kind     string
	Count    int
	Samples  string
}

// unsafePatternItem is one row of `analyze kind=unsafe_patterns`:
// a single match from one of the bundled unsafe-pattern detectors.
type unsafePatternItem struct {
	Detector string
	Severity string
	Language string
	File     string
	Line     int
	Symbol   string
	Text     string
}

// healthScoreItem is one row of `analyze kind=health_score`: a
// per-symbol composite health value with the per-axis breakdown.
// "_pct" fields are the 0..100 axis values; NaN encodes "no data
// for this axis" (the GCX encoder serializes NaN as empty so a
// downstream reader can distinguish missing from zero).
type healthScoreItem struct {
	ID            string
	Name          string
	Kind          string
	File          string
	Line          int
	Score         float64
	Grade         string
	CoveragePct   float64
	ComplexityPct float64
	RecencyPct    float64
	ChurnPct      float64
	FanIn         int
	FanOut        int
	Crossings     int
	AgeDays       int
	Mods          int
	AxesUsed      int
}

// healthRollupItem is one row of `analyze kind=health_score` when
// `roll_up` aggregates the per-symbol scores up to file or repo
// scope.
type healthRollupItem struct {
	Scope    string
	Key      string
	AvgScore float64
	MinScore float64
	MaxScore float64
	Symbols  int
	Grade    string
	CountA   int
	CountB   int
	CountC   int
	CountD   int
	CountF   int
}

// --------------------------------------------------------------------
// contracts
// --------------------------------------------------------------------

// contractFields is the fixed column layout for one contract row.
// method + path are promoted out of Meta so HTTP/gRPC filters don't
// have to parse the compact meta column.
var contractFields = []string{
	"type", "role", "repo", "method", "path",
	"file", "line", "id", "symbol_id", "confidence", "meta",
}

// writeContractRow emits one contract using contractFields.
func writeContractRow(enc *wire.Encoder, c contracts.Contract) error {
	method, _ := c.Meta["method"].(string)
	path, _ := c.Meta["path"].(string)
	return enc.WriteRow(
		string(c.Type), string(c.Role), c.RepoPrefix, method, path,
		c.FilePath, c.Line, c.ID, c.SymbolID, c.Confidence,
		formatContractMeta(c.Meta, "method", "path"),
	)
}

// formatContractMeta renders Meta as a stable semicolon-separated k=v
// list, dropping excluded keys already promoted to dedicated columns.
func formatContractMeta(m map[string]any, exclude ...string) string {
	if len(m) == 0 {
		return ""
	}
	skip := make(map[string]bool, len(exclude))
	for _, k := range exclude {
		skip[k] = true
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if skip[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(';')
		}
		fmt.Fprintf(&b, "%s=%v", k, m[k])
	}
	return b.String()
}

// encodeContractsList emits one row per contract. The by_repo grouping
// from the JSON payload is flattened — rows carry repo in-band so
// consumers can regroup without walking a tree.
func encodeContractsList(rows []contracts.Contract, total int, extraMeta ...string) ([]byte, error) {
	var buf bytes.Buffer
	meta := append([]string{"total", fmt.Sprintf("%d", total)}, extraMeta...)
	enc := newGCX(&buf, "contracts.list", contractFields, meta...)
	if err := enc.WriteComment(fmt.Sprintf("%d contract(s)", len(rows))); err != nil {
		return nil, err
	}
	for _, c := range rows {
		if err := writeContractRow(enc, c); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// encodeContractsCheck emits three sections: matched pairs, orphan
// providers, orphan consumers. Orphan sections reuse contractFields.
func encodeContractsCheck(result contracts.MatchResult) ([]byte, error) {
	var buf bytes.Buffer

	matchedFields := []string{
		"contract_id", "cross_repo",
		"provider_repo", "provider_file", "provider_line", "provider_symbol",
		"consumer_repo", "consumer_file", "consumer_line", "consumer_symbol",
	}
	matchedEnc := newGCX(&buf, "contracts.check.matched", matchedFields,
		"count", fmt.Sprintf("%d", len(result.Matched)),
	)
	for _, m := range result.Matched {
		if err := matchedEnc.WriteRow(
			m.ContractID, m.CrossRepo,
			m.Provider.RepoPrefix, m.Provider.FilePath, m.Provider.Line, m.Provider.SymbolID,
			m.Consumer.RepoPrefix, m.Consumer.FilePath, m.Consumer.Line, m.Consumer.SymbolID,
		); err != nil {
			return nil, err
		}
	}
	if err := matchedEnc.Close(); err != nil {
		return nil, err
	}

	provEnc := newGCX(&buf, "contracts.check.orphan_providers", contractFields,
		"count", fmt.Sprintf("%d", len(result.OrphanProviders)),
	)
	for _, c := range result.OrphanProviders {
		if err := writeContractRow(provEnc, c); err != nil {
			return nil, err
		}
	}
	if err := provEnc.Close(); err != nil {
		return nil, err
	}

	consEnc := newGCX(&buf, "contracts.check.orphan_consumers", contractFields,
		"count", fmt.Sprintf("%d", len(result.OrphanConsumers)),
	)
	for _, c := range result.OrphanConsumers {
		if err := writeContractRow(consEnc, c); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), consEnc.Close()
}

// --------------------------------------------------------------------
// get_editing_context
// --------------------------------------------------------------------

// encodeEditingContext emits four sections: defines, imports,
// called_by, calls. File metadata (id, language, etag) lives in the
// defines-section header so there is no single-row wrapper section.
func encodeEditingContext(file map[string]any, defines, imports, calledBy, calls []map[string]any, etag, omissionKinds string) ([]byte, error) {
	var buf bytes.Buffer

	// file_id (the path) is not echoed in the header: paths on macOS /
	// Windows can contain spaces, and the GCX header tokeniser splits on
	// raw spaces. Callers can recover the file path from any defines
	// row's ID prefix (`<path>::<symbol>`).
	var language string
	if v, ok := file["language"]; ok {
		language = fmt.Sprint(v)
	}

	defMeta := []string{
		"etag", etag,
		"language", language,
		"count", fmt.Sprintf("%d", len(defines)),
	}
	if omissionKinds != "" {
		defMeta = append(defMeta, "omissions", omissionKinds)
	}
	defEnc := newGCX(&buf, "get_editing_context.defines",
		[]string{"id", "kind", "name", "line", "sig"},
		defMeta...,
	)
	for _, d := range defines {
		if err := defEnc.WriteRow(
			str(d["id"]),
			str(d["kind"]),
			str(d["name"]),
			d["start_line"],
			str(d["signature"]),
		); err != nil {
			return nil, err
		}
	}
	if err := defEnc.Close(); err != nil {
		return nil, err
	}

	impEnc := newGCX(&buf, "get_editing_context.imports",
		[]string{"id", "external"},
		"count", fmt.Sprintf("%d", len(imports)),
	)
	for _, im := range imports {
		if err := impEnc.WriteRow(str(im["id"]), im["external"]); err != nil {
			return nil, err
		}
	}
	if err := impEnc.Close(); err != nil {
		return nil, err
	}

	cbEnc := newGCX(&buf, "get_editing_context.called_by",
		[]string{"id", "name", "path", "line"},
		"count", fmt.Sprintf("%d", len(calledBy)),
	)
	for _, c := range calledBy {
		if err := cbEnc.WriteRow(str(c["id"]), str(c["name"]), str(c["file_path"]), c["start_line"]); err != nil {
			return nil, err
		}
	}
	if err := cbEnc.Close(); err != nil {
		return nil, err
	}

	callEnc := newGCX(&buf, "get_editing_context.calls",
		[]string{"id", "name", "path", "line"},
		"count", fmt.Sprintf("%d", len(calls)),
	)
	for _, c := range calls {
		if err := callEnc.WriteRow(str(c["id"]), str(c["name"]), str(c["file_path"]), c["start_line"]); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), callEnc.Close()
}

// --------------------------------------------------------------------
// smart_context
// --------------------------------------------------------------------

// encodeSmartContext emits up to seven sections — symbols, cross_repo,
// entry_file, callers, callees, tests, files — in the order the
// orchestrator populates them. Only non-empty sections are written so
// small tasks stay small. Each section has its own field list to
// avoid wide rows padded with empty cells.
func encodeSmartContext(result map[string]any) ([]byte, error) {
	var buf bytes.Buffer

	// The task string is not echoed in the header: the GCX header tokeniser
	// splits on spaces and escape() doesn't escape them, so a free-text
	// task would corrupt parsing. The caller already knows what they
	// asked for — no need to round-trip it.

	symbols, _ := result["relevant_symbols"].([]map[string]any)
	symEnc := newGCX(&buf, "smart_context.symbols",
		[]string{"id", "kind", "name", "path", "line", "sig", "source"},
		"count", fmt.Sprintf("%d", len(symbols)),
		"etag", str(result["etag"]),
	)
	for _, s := range symbols {
		if err := symEnc.WriteRow(
			str(s["id"]),
			str(s["kind"]),
			str(s["name"]),
			str(s["file_path"]),
			s["start_line"],
			str(s["signature"]),
			str(s["source"]),
		); err != nil {
			return nil, err
		}
	}
	if err := symEnc.Close(); err != nil {
		return nil, err
	}

	if mani, ok := result["context_manifest"].(map[string]any); ok {
		entries, _ := mani["entries"].([]map[string]any)
		manEnc := newGCX(&buf, "smart_context.manifest",
			[]string{"id", "kind", "name", "path", "line", "tier", "relation", "distance", "sibling_count", "compressed", "sig", "source"},
			"token_budget", str(mani["token_budget"]),
			"tokens_used", str(mani["tokens_used"]),
			"omitted", str(mani["omitted"]),
		)
		for _, e := range entries {
			if err := manEnc.WriteRow(
				str(e["id"]),
				str(e["kind"]),
				str(e["name"]),
				str(e["file_path"]),
				e["start_line"],
				str(e["tier"]),
				str(e["relation"]),
				e["distance"],
				e["sibling_count"],
				str(e["compressed"]),
				str(e["signature"]),
				str(e["source"]),
			); err != nil {
				return nil, err
			}
		}
		if err := manEnc.Close(); err != nil {
			return nil, err
		}
	}

	if crossRepo, ok := result["cross_repo_dependencies"].([]map[string]any); ok && len(crossRepo) > 0 {
		enc := newGCX(&buf, "smart_context.cross_repo",
			[]string{"id", "kind", "name", "path", "repo", "edge_kind", "sig"},
			"count", fmt.Sprintf("%d", len(crossRepo)),
		)
		for _, d := range crossRepo {
			if err := enc.WriteRow(
				str(d["id"]),
				str(d["kind"]),
				str(d["name"]),
				str(d["file_path"]),
				str(d["repo_prefix"]),
				str(d["edge_kind"]),
				str(d["signature"]),
			); err != nil {
				return nil, err
			}
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
	}

	if entryFile, ok := result["entry_file_symbols"].([]string); ok && len(entryFile) > 0 {
		enc := newGCX(&buf, "smart_context.entry_file",
			[]string{"desc"},
			"count", fmt.Sprintf("%d", len(entryFile)),
		)
		for _, d := range entryFile {
			if err := enc.WriteRow(d); err != nil {
				return nil, err
			}
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
	}

	if callers, ok := result["callers"].([]string); ok && len(callers) > 0 {
		enc := newGCX(&buf, "smart_context.callers",
			[]string{"id"},
			"count", fmt.Sprintf("%d", len(callers)),
		)
		for _, id := range callers {
			if err := enc.WriteRow(id); err != nil {
				return nil, err
			}
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
	}

	if callees, ok := result["callees"].([]string); ok && len(callees) > 0 {
		enc := newGCX(&buf, "smart_context.callees",
			[]string{"id"},
			"count", fmt.Sprintf("%d", len(callees)),
		)
		for _, id := range callees {
			if err := enc.WriteRow(id); err != nil {
				return nil, err
			}
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
	}

	if tests, ok := result["related_test_files"].([]string); ok && len(tests) > 0 {
		enc := newGCX(&buf, "smart_context.tests",
			[]string{"path"},
			"count", fmt.Sprintf("%d", len(tests)),
		)
		for _, p := range tests {
			if err := enc.WriteRow(p); err != nil {
				return nil, err
			}
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
	}

	if files, ok := result["files_to_edit"].([]string); ok && len(files) > 0 {
		enc := newGCX(&buf, "smart_context.files",
			[]string{"path"},
			"count", fmt.Sprintf("%d", len(files)),
		)
		for _, p := range files {
			if err := enc.WriteRow(p); err != nil {
				return nil, err
			}
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
	}

	if ws, ok := result["working_set"].([]map[string]any); ok && len(ws) > 0 {
		enc := newGCX(&buf, "smart_context.working_set",
			[]string{"file", "is_test", "symbols"},
			"count", fmt.Sprintf("%d", len(ws)),
		)
		for _, c := range ws {
			ids, _ := c["symbols"].([]string)
			if err := enc.WriteRow(str(c["file"]), c["is_test"], strings.Join(ids, ",")); err != nil {
				return nil, err
			}
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
	}

	if br, ok := result["blast_radius"].(map[string]any); ok {
		callerGroups, _ := br["callers_by_file"].([]map[string]any)
		callerMeta := []string{"count", fmt.Sprintf("%d", len(callerGroups))}
		// Header values are single tokens, so a flag stands in for the
		// sentence the JSON envelope spells out — the same substitution
		// analyze.impact.target makes for zero_dependents_unproven.
		if str(br["warning"]) != "" {
			callerMeta = append(callerMeta, "no_covering_tests", "true")
		}
		callerEnc := newGCX(&buf, "smart_context.blast_callers",
			[]string{"file", "callers"},
			callerMeta...,
		)
		for _, g := range callerGroups {
			ids, _ := g["callers"].([]string)
			if err := callerEnc.WriteRow(str(g["file"]), strings.Join(ids, ",")); err != nil {
				return nil, err
			}
		}
		if err := callerEnc.Close(); err != nil {
			return nil, err
		}

		tests, _ := br["covering_tests"].([]map[string]any)
		testEnc := newGCX(&buf, "smart_context.blast_tests",
			[]string{"file", "function"},
			"count", fmt.Sprintf("%d", len(tests)),
		)
		for _, tr := range tests {
			if err := testEnc.WriteRow(str(tr["file"]), str(tr["function"])); err != nil {
				return nil, err
			}
		}
		if err := testEnc.Close(); err != nil {
			return nil, err
		}
	}

	if inPack, ok := result["in_pack"].(map[string]any); ok {
		if cps, ok := inPack["call_paths"].([]map[string]any); ok && len(cps) > 0 {
			cpEnc := newGCX(&buf, "smart_context.call_paths",
				[]string{"root", "anchor", "length", "confidence", "nodes"},
				"count", fmt.Sprintf("%d", len(cps)),
			)
			for _, cp := range cps {
				nodes := ""
				if ns, ok := cp["nodes"].([]string); ok {
					nodes = strings.Join(ns, ">")
				}
				if err := cpEnc.WriteRow(
					str(cp["root"]),
					str(cp["anchor"]),
					cp["length"],
					cp["confidence"],
					nodes,
				); err != nil {
					return nil, err
				}
			}
			if err := cpEnc.Close(); err != nil {
				return nil, err
			}
		}
		if flows, ok := inPack["flows"].(map[string]any); ok {
			if spine, ok := flows["spine"].([]string); ok && len(spine) > 0 {
				spEnc := newGCX(&buf, "smart_context.flow_spine",
					[]string{"nodes"},
					"length", fmt.Sprintf("%d", len(spine)),
				)
				if err := spEnc.WriteRow(strings.Join(spine, ">")); err != nil {
					return nil, err
				}
				if err := spEnc.Close(); err != nil {
					return nil, err
				}
			}
			if bs, ok := flows["boundaries"].([]map[string]any); ok && len(bs) > 0 {
				bEnc := newGCX(&buf, "smart_context.flow_boundaries",
					[]string{"from", "target", "reason"},
					"count", fmt.Sprintf("%d", len(bs)),
				)
				for _, b := range bs {
					if err := bEnc.WriteRow(str(b["from"]), str(b["target"]), str(b["reason"])); err != nil {
						return nil, err
					}
				}
				if err := bEnc.Close(); err != nil {
					return nil, err
				}
			}
		}
		if conf, ok := inPack["confidence"].(map[string]any); ok && len(conf) > 0 {
			cEnc := newGCX(&buf, "smart_context.confidence",
				[]string{"verdict", "top1", "top2", "ratio_top1_top2", "k", "std_dev"},
			)
			if err := cEnc.WriteRow(
				str(conf["verdict"]),
				conf["top1"],
				conf["top2"],
				conf["ratio_top1_top2"],
				conf["k"],
				conf["std_dev"],
			); err != nil {
				return nil, err
			}
			if err := cEnc.Close(); err != nil {
				return nil, err
			}
		}
	}

	if rec, ok := result["recovered_edges"].([]map[string]any); ok && len(rec) > 0 {
		rEnc := newGCX(&buf, "smart_context.recovered_edges",
			[]string{"from", "to", "kind"},
			"count", fmt.Sprintf("%d", len(rec)),
		)
		for _, e := range rec {
			if err := rEnc.WriteRow(str(e["from"]), str(e["to"]), str(e["kind"])); err != nil {
				return nil, err
			}
		}
		if err := rEnc.Close(); err != nil {
			return nil, err
		}
	}

	if sibs, ok := result["hierarchy_siblings"].([]map[string]any); ok && len(sibs) > 0 {
		sEnc := newGCX(&buf, "smart_context.hierarchy_siblings",
			[]string{"id", "name", "kind", "parent"},
			"count", fmt.Sprintf("%d", len(sibs)),
		)
		for _, sb := range sibs {
			if err := sEnc.WriteRow(str(sb["id"]), str(sb["name"]), str(sb["kind"]), str(sb["parent"])); err != nil {
				return nil, err
			}
		}
		if err := sEnc.Close(); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// encodeSmartContextEstimate encodes the dry-run token-cost projection
// of smart_context as a single-row GCX section.
func encodeSmartContextEstimate(result map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	est, _ := result["estimate"].(map[string]any)
	enc := newGCX(&buf, "smart_context.estimate",
		[]string{"fidelity", "symbol_count", "projected_tokens", "token_budget", "focus", "ring", "outline", "omitted"},
		"count", "1",
	)
	if err := enc.WriteRow(
		str(est["fidelity"]),
		str(est["symbol_count"]),
		str(est["projected_tokens"]),
		str(est["token_budget"]),
		str(est["focus"]),
		str(est["ring"]),
		str(est["outline"]),
		str(est["omitted"]),
	); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// --------------------------------------------------------------------
// prefetch_context, explain_change_impact, check_guards, feedback.query
// --------------------------------------------------------------------

// encodePrefetchContext emits one row per ranked candidate with the
// per-axis score contributions inlined (search/proximity/community)
// so consumers can recover the attribution without a nested object.
// Source — when include_source was set on the request — rides as the
// last field, escaped via the wire encoder so newlines stay one row.
func encodePrefetchContext(candidates []prefetchCandidate, total int, truncated bool, includeSource bool) ([]byte, error) {
	fields := []string{"id", "kind", "name", "path", "line", "confidence", "search", "proximity", "community", "reason"}
	if includeSource {
		fields = append(fields, "source")
	}
	var buf bytes.Buffer
	enc := newGCX(&buf, "prefetch_context",
		fields,
		"total", fmt.Sprintf("%d", total),
		"truncated", boolString(truncated),
	)
	if err := enc.WriteComment(fmt.Sprintf("%d candidate(s)", len(candidates))); err != nil {
		return nil, err
	}
	for _, c := range candidates {
		row := []any{
			c.ID,
			c.Kind,
			nodeShort(c.Node),
			c.FilePath,
			c.StartLine,
			roundFloat(c.Confidence),
			roundFloat(c.SearchRelevance),
			roundFloat(c.GraphProximity),
			roundFloat(c.CommunityBonus),
			c.Reason,
		}
		if includeSource {
			row = append(row, c.Source)
		}
		if err := enc.WriteRow(row...); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// encodeChangeImpact emits a multi-section envelope for the
// explain_change_impact tool. Section layout:
//
//   - explain_change_impact.summary (one row): risk, total, communities/processes
//     joined with `,` so the section stays one row regardless of fan-out.
//   - explain_change_impact.entries (one row per affected symbol): the
//     by_depth + by_repo maps are flattened into a single table — `repo`
//     is empty unless the change crosses repos.
//   - explain_change_impact.contracts (one row when contract_impact is set):
//     breaking/warning/info counters + the validate-pass risk upgrade flag.
func encodeChangeImpact(result map[string]any) ([]byte, error) {
	var buf bytes.Buffer

	risk, _ := result["risk"].(analysis.RiskLevel)
	summaryStr, _ := result["summary"].(string)
	totalAffected, _ := result["total_affected"].(int)
	crossRepo, _ := result["cross_repo_impact"].(bool)
	processes, _ := result["affected_processes"].([]string)
	communities, _ := result["affected_communities"].([]string)
	testFiles, _ := result["test_files"].([]string)
	warning, _ := result["cross_community_warning"].(string)
	communityNote, _ := result["community_note"].(string)
	contractRiskUpgrade, _ := result["contract_risk_upgrade"].(string)

	sumEnc := newGCX(&buf, "explain_change_impact.summary",
		[]string{"risk", "summary", "total_affected", "cross_repo", "processes", "communities", "test_files", "warning", "note", "risk_upgrade"},
	)
	if err := sumEnc.WriteRow(
		string(risk),
		summaryStr,
		totalAffected,
		boolString(crossRepo),
		strings.Join(processes, ","),
		strings.Join(communities, ","),
		strings.Join(testFiles, ","),
		warning,
		communityNote,
		contractRiskUpgrade,
	); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}

	// Flatten by_depth (and by_repo when cross-repo) into a single
	// rows section. We carry depth + repo so consumers can regroup.
	entryFields := []string{"depth", "repo", "id", "name", "kind", "path", "line", "edge_confidence", "confidence_label"}
	entryEnc := newGCX(&buf, "explain_change_impact.entries", entryFields)
	totalRows := 0
	if byDepth, ok := result["by_depth"].(map[int][]analysis.ImpactEntry); ok {
		// Sort depths so output is deterministic.
		depths := make([]int, 0, len(byDepth))
		for d := range byDepth {
			depths = append(depths, d)
		}
		sort.Ints(depths)
		for _, d := range depths {
			for _, e := range byDepth[d] {
				if err := entryEnc.WriteRow(
					d,
					e.RepoPrefix,
					e.ID,
					e.Name,
					e.Kind,
					e.FilePath,
					e.Line,
					roundFloat(e.EdgeConfidence),
					e.ConfidenceLabel,
				); err != nil {
					return nil, err
				}
				totalRows++
			}
		}
	}
	if err := entryEnc.Close(); err != nil {
		return nil, err
	}

	if ci, ok := result["contract_impact"].(*contractImpact); ok && ci != nil {
		ciEnc := newGCX(&buf, "explain_change_impact.contracts",
			[]string{"breaking", "warning", "info", "affected"},
		)
		if err := ciEnc.WriteRow(ci.Breaking, ci.Warning, ci.Info, len(ci.Affected)); err != nil {
			return nil, err
		}
		if err := ciEnc.Close(); err != nil {
			return nil, err
		}
	}

	// zero_impact_caveat — one row per input symbol whose empty blast
	// radius could not be told apart from an extraction gap.
	if caveats, ok := result["zero_impact_caveat"].([]graph.ZeroImpactCaveat); ok && len(caveats) > 0 {
		cavEnc := newGCX(&buf, "explain_change_impact.caveats",
			[]string{"id", "class", "message"},
		)
		for _, c := range caveats {
			if err := cavEnc.WriteRow(c.ID, string(c.Class), c.Message); err != nil {
				return nil, err
			}
		}
		if err := cavEnc.Close(); err != nil {
			return nil, err
		}
	}

	// boundaries — the epistemic lower-bound diagnosis. Emitted only when the
	// blast radius crossed a dynamic-dispatch / interface site, so results
	// with none keep their existing wire bytes. The section's presence (and
	// its lower_bound meta flag) signals the count is a floor.
	if boundaries, ok := result["boundaries"].([]graph.EpistemicBoundary); ok && len(boundaries) > 0 {
		lb, _ := result["lower_bound"].(bool)
		bEnc := newGCX(&buf, "explain_change_impact.boundaries",
			[]string{"seed_id", "seed_name", "target", "edge_kind", "reason", "direction"},
			"count", fmt.Sprintf("%d", len(boundaries)),
			"lower_bound", boolString(lb),
		)
		for _, b := range boundaries {
			if err := bEnc.WriteRow(b.SeedID, b.SeedName, b.Target, b.EdgeKind, string(b.Reason), b.Direction); err != nil {
				return nil, err
			}
		}
		if err := bEnc.Close(); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// encodeAPIImpact emits the fused route report as three GCX sections:
// api_impact.routes (one row per route), api_impact.consumers and
// api_impact.mismatches (one row each, carrying the route id so they regroup).
func encodeAPIImpact(reports []apiImpactReport) ([]byte, error) {
	var buf bytes.Buffer
	routeEnc := newGCX(&buf, "api_impact.routes",
		[]string{"route", "method", "path", "handler", "repo", "response_success", "response_error", "middleware", "risk", "direct_consumers", "affected_callers", "affected_flows", "test_files", "warning", "contract_risk_upgrade"},
		"count", fmt.Sprintf("%d", len(reports)),
	)
	for _, r := range reports {
		mw := strings.Join(r.Middleware, ",")
		if mw == "" {
			mw = r.MiddlewareDetection
		}
		if err := routeEnc.WriteRow(
			r.Route, r.Method, r.Path, r.Handler, r.Repo,
			strings.Join(r.ResponseShape.Success, ","),
			strings.Join(r.ResponseShape.Error, ","),
			mw,
			r.ImpactSummary.RiskLevel,
			r.ImpactSummary.DirectConsumers,
			r.ImpactSummary.AffectedCallers,
			r.ImpactSummary.AffectedFlows,
			strings.Join(r.ImpactSummary.TestFilesToRun, ","),
			r.ImpactSummary.Warning,
			r.ImpactSummary.ContractRiskUpgrade,
		); err != nil {
			return nil, err
		}
	}
	if err := routeEnc.Close(); err != nil {
		return nil, err
	}

	consEnc := newGCX(&buf, "api_impact.consumers",
		[]string{"route", "name", "file", "repo", "accesses", "attribution_note"})
	for _, r := range reports {
		for _, c := range r.Consumers {
			if err := consEnc.WriteRow(r.Route, c.Name, c.File, c.Repo, strings.Join(c.Accesses, ","), c.AttributionNote); err != nil {
				return nil, err
			}
		}
	}
	if err := consEnc.Close(); err != nil {
		return nil, err
	}

	mmEnc := newGCX(&buf, "api_impact.mismatches",
		[]string{"route", "consumer", "field", "reason", "confidence"})
	for _, r := range reports {
		for _, m := range r.Mismatches {
			if err := mmEnc.WriteRow(r.Route, m.Consumer, m.Field, m.Reason, m.Confidence); err != nil {
				return nil, err
			}
		}
	}
	return buf.Bytes(), mmEnc.Close()
}

// encodeCheckGuards emits one row per guard violation. The empty case
// (no rules / no violations) still produces a valid envelope so agents
// can rely on the GCX1 header to differentiate from an error payload —
// the meta `status=no_rules_configured` flag carries the "no rules"
// hint as a single token (the decoder splits the header on raw spaces,
// so multi-word meta values must stay tokenised).
func encodeCheckGuards(violations []analysis.GuardViolation, noRulesConfigured bool) ([]byte, error) {
	var buf bytes.Buffer
	meta := []string{"total", fmt.Sprintf("%d", len(violations))}
	if noRulesConfigured {
		meta = append(meta, "status", "no_rules_configured")
	}
	enc := newGCX(&buf, "check_guards",
		[]string{"rule_name", "kind", "description"},
		meta...,
	)
	for _, v := range violations {
		if err := enc.WriteRow(v.RuleName, v.Kind, v.Description); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// encodeFeedbackQuery emits a multi-section envelope:
//
//   - feedback.summary (one row): total_entries, accuracy
//   - feedback.most_useful, feedback.most_missed, feedback.most_demoted
//     (one row per ranked symbol): id, score, count
//
// Empty lists still emit their section so consumers can iterate without
// branching on existence — the meta count is the source of truth.
func encodeFeedbackQuery(stats map[string]any) ([]byte, error) {
	var buf bytes.Buffer

	totalEntries := 0
	if v, ok := stats["total_entries"].(int); ok {
		totalEntries = v
	}
	accuracy := 0.0
	if v, ok := stats["accuracy"].(float64); ok {
		accuracy = v
	}
	sumEnc := newGCX(&buf, "feedback.summary",
		[]string{"total_entries", "accuracy"},
	)
	if err := sumEnc.WriteRow(totalEntries, roundFloat(accuracy)); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}

	emit := func(section string, key string) error {
		enc := newGCX(&buf, section,
			[]string{"id", "score", "count"},
		)
		// AggregatedStats produces an unexported `ranked` slice we
		// can only see through reflection. Coerce via JSON shape:
		// the published schema is {id, score, count}, so we read
		// it through a generic any-decoder.
		rows, _ := stats[key].([]any)
		if rows == nil {
			// Strongly-typed path — re-marshal whatever AggregatedStats
			// returned and decode into a slice of {id, score, count}.
			if raw, err := json.Marshal(stats[key]); err == nil {
				var coerced []struct {
					ID    string  `json:"id"`
					Score float64 `json:"score"`
					Count int     `json:"count"`
				}
				_ = json.Unmarshal(raw, &coerced)
				for _, r := range coerced {
					if err := enc.WriteRow(r.ID, roundFloat(r.Score), r.Count); err != nil {
						return err
					}
				}
				return enc.Close()
			}
		}
		for _, row := range rows {
			m, _ := row.(map[string]any)
			id, _ := m["id"].(string)
			score, _ := m["score"].(float64)
			count := 0
			if v, ok := m["count"].(int); ok {
				count = v
			} else if v, ok := m["count"].(float64); ok {
				count = int(v)
			}
			if err := enc.WriteRow(id, roundFloat(score), count); err != nil {
				return err
			}
		}
		return enc.Close()
	}
	if err := emit("feedback.most_useful", "most_useful"); err != nil {
		return nil, err
	}
	if err := emit("feedback.most_missed", "most_missed"); err != nil {
		return nil, err
	}
	if err := emit("feedback.most_demoted", "most_demoted"); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// --------------------------------------------------------------------
// small utilities
// --------------------------------------------------------------------

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func indexNodes(nodes []*graph.Node) map[string]*graph.Node {
	m := make(map[string]*graph.Node, len(nodes))
	for _, n := range nodes {
		m[n.ID] = n
	}
	return m
}

// encodePRRisk emits the pr_risk report as two GCX sections:
//   - pr_risk.summary (one row): the composite score, risk label, supporting
//     counts (total_affected / uncovered / community_span / changed_symbols)
//     and the joined security hits.
//   - pr_risk.priorities (one row per axis): the ordered review_priorities —
//     axis, 0-100 score, and the human-readable reason.
//
// The map shape is whatever prRiskPayload built, so JSON and GCX stay a single
// source of truth for field names.
func encodePRRisk(result map[string]any) ([]byte, error) {
	var buf bytes.Buffer

	score, _ := result["score"].(float64)
	risk, _ := result["risk"].(string)
	totalAffected, _ := result["total_affected"].(int)
	uncovered, _ := result["uncovered_symbols"].(int)
	communitySpan, _ := result["community_span"].(int)
	changedSymbols, _ := result["changed_symbols"].(int)
	hits, _ := result["security_hits"].([]string)

	sumEnc := newGCX(&buf, "pr_risk.summary",
		[]string{"score", "risk", "total_affected", "uncovered_symbols", "community_span", "changed_symbols", "security_hits"},
	)
	if err := sumEnc.WriteRow(
		roundFloat(score),
		risk,
		totalAffected,
		uncovered,
		communitySpan,
		changedSymbols,
		strings.Join(hits, ","),
	); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}

	prEnc := newGCX(&buf, "pr_risk.priorities", []string{"axis", "score", "reason"})
	if priorities, ok := result["review_priorities"].([]map[string]any); ok {
		for _, p := range priorities {
			axis, _ := p["axis"].(string)
			pscore, _ := p["score"].(float64)
			reason, _ := p["reason"].(string)
			if err := prEnc.WriteRow(axis, roundFloat(pscore), reason); err != nil {
				return nil, err
			}
		}
	}
	if err := prEnc.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// encodeSuggestReviewers encodes the suggest_reviewers payload as GCX1: a
// one-row summary section plus a per-reviewer row section. The map shape is
// whatever suggestReviewersPayload built, so JSON and GCX stay a single source
// of truth for field names.
func encodeSuggestReviewers(result map[string]any) ([]byte, error) {
	var buf bytes.Buffer

	total, _ := result["total"].(int)
	changedFiles, _ := result["changed_files"].(int)
	codeownersFound, _ := result["codeowners_found"].(bool)

	sumEnc := newGCX(&buf, "suggest_reviewers.summary",
		[]string{"total", "changed_files", "codeowners_found"},
	)
	if err := sumEnc.WriteRow(total, changedFiles, codeownersFound); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}

	revEnc := newGCX(&buf, "suggest_reviewers.reviewers",
		[]string{"reviewer", "kind", "score", "reasons", "matched_files"},
	)
	if reviewers, ok := result["reviewers"].([]map[string]any); ok {
		for _, r := range reviewers {
			reviewer, _ := r["reviewer"].(string)
			kind, _ := r["kind"].(string)
			score, _ := r["score"].(int)
			reasons, _ := r["reasons"].([]string)
			matched, _ := r["matched_files"].([]string)
			if err := revEnc.WriteRow(
				reviewer,
				kind,
				score,
				strings.Join(reasons, "; "),
				strings.Join(matched, ","),
			); err != nil {
				return nil, err
			}
		}
	}
	if err := revEnc.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// encodeReviewQuestions encodes the suggested_review_questions payload as
// GCX1: a one-row summary section (total + truncated + per-category
// counts) followed by a per-question row section. Severity / score ride
// as row values so the prioritised order is reconstructable from the
// wire form, and signals are joined with ";" since the row is
// tab-delimited.
func encodeReviewQuestions(questions []reviewQuestion, byCategory map[string]int, truncated bool) ([]byte, error) {
	var buf bytes.Buffer

	sumEnc := newGCX(&buf, "suggested_review_questions.summary",
		[]string{"total", "truncated", "bridge", "hub_risk", "surprising", "thin_community", "untested_hotspot"},
	)
	if err := sumEnc.WriteRow(
		len(questions),
		truncated,
		byCategory[rqCatBridge],
		byCategory[rqCatHubRisk],
		byCategory[rqCatSurprising],
		byCategory[rqCatThinCommunity],
		byCategory[rqCatUntestedHotspot],
	); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}

	qEnc := newGCX(&buf, "suggested_review_questions.questions",
		[]string{"id", "category", "severity", "score", "symbol_id", "symbol_name", "file", "line", "question", "evidence", "signals"},
	)
	for _, q := range questions {
		if err := qEnc.WriteRow(
			q.ID,
			q.Category,
			q.Severity,
			roundFloat(q.Score),
			q.SymbolID,
			q.SymbolName,
			q.File,
			q.Line,
			q.Question,
			q.Evidence,
			strings.Join(q.Signals, ";"),
		); err != nil {
			return nil, err
		}
	}
	if err := qEnc.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// encodeListPRs encodes the list_prs payload as GCX1: a one-row summary
// (total) plus a per-PR row section. The map shape mirrors listPRsPayload
// so JSON and GCX share one set of field names. A degradation payload
// (error/hint) is never routed here — those go through the JSON/TOON path.
func encodeListPRs(result map[string]any) ([]byte, error) {
	var buf bytes.Buffer

	total, _ := result["total"].(int)
	sumEnc := newGCX(&buf, "list_prs.summary", []string{"total"})
	if err := sumEnc.WriteRow(total); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}

	prEnc := newGCX(&buf, "list_prs.prs",
		[]string{"number", "title", "author", "age_days", "ci", "review", "state", "blockers"},
	)
	if prs, ok := result["prs"].([]map[string]any); ok {
		for _, p := range prs {
			number, _ := p["number"].(int)
			title, _ := p["title"].(string)
			author, _ := p["author"].(string)
			ageDays, _ := p["age_days"].(int)
			ci, _ := p["ci"].(string)
			review, _ := p["review"].(string)
			state, _ := p["state"].(string)
			blockers, _ := p["blockers"].([]string)
			if err := prEnc.WriteRow(
				number, title, author, ageDays, ci, review, state,
				strings.Join(blockers, ","),
			); err != nil {
				return nil, err
			}
		}
	}
	if err := prEnc.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// encodePRImpact encodes the get_pr_impact payload as GCX1: a one-row
// summary, a review-priorities section, and a changed-symbols section.
// The bulky blast map is omitted from the compact form — callers that
// need the full blast grouping request json. The map shape mirrors
// prImpactForNumber.
func encodePRImpact(result map[string]any) ([]byte, error) {
	var buf bytes.Buffer

	number, _ := result["number"].(int)
	risk, _ := result["risk"].(string)
	score, _ := result["score"].(float64)
	changedFiles, _ := result["changed_files"].([]string)
	communities, _ := result["communities"].([]string)

	sumEnc := newGCX(&buf, "get_pr_impact.summary",
		[]string{"number", "risk", "score", "changed_files", "communities"},
	)
	if err := sumEnc.WriteRow(
		number, risk, roundFloat(score), len(changedFiles), len(communities),
	); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}

	prEnc := newGCX(&buf, "get_pr_impact.priorities", []string{"axis", "score", "reason"})
	if priorities, ok := result["review_priorities"].([]map[string]any); ok {
		for _, p := range priorities {
			axis, _ := p["axis"].(string)
			pscore, _ := p["score"].(float64)
			reason, _ := p["reason"].(string)
			if err := prEnc.WriteRow(axis, roundFloat(pscore), reason); err != nil {
				return nil, err
			}
		}
	}
	if err := prEnc.Close(); err != nil {
		return nil, err
	}

	symEnc := newGCX(&buf, "get_pr_impact.changed_symbols", []string{"id", "name", "kind", "file"})
	if syms, ok := result["changed_symbols"].([]map[string]any); ok {
		for _, sym := range syms {
			id, _ := sym["id"].(string)
			name, _ := sym["name"].(string)
			kind, _ := sym["kind"].(string)
			file, _ := sym["file"].(string)
			if err := symEnc.WriteRow(id, name, kind, file); err != nil {
				return nil, err
			}
		}
	}
	if err := symEnc.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// encodeTriagePRs encodes the triage_prs payload as GCX1: a one-row
// summary plus the score-ranked per-PR rows. The map shape mirrors the
// triage_prs payload.
func encodeTriagePRs(result map[string]any) ([]byte, error) {
	var buf bytes.Buffer

	total, _ := result["total"].(int)
	sumEnc := newGCX(&buf, "triage_prs.summary", []string{"total"})
	if err := sumEnc.WriteRow(total); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}

	rankEnc := newGCX(&buf, "triage_prs.ranked",
		[]string{"number", "title", "author", "risk", "score"},
	)
	if ranked, ok := result["ranked"].([]map[string]any); ok {
		for _, r := range ranked {
			number, _ := r["number"].(int)
			title, _ := r["title"].(string)
			author, _ := r["author"].(string)
			risk, _ := r["risk"].(string)
			score, _ := r["score"].(float64)
			if err := rankEnc.WriteRow(number, title, author, risk, roundFloat(score)); err != nil {
				return nil, err
			}
		}
	}
	if err := rankEnc.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// encodeConflictsPRs encodes the conflicts_prs payload as GCX1: a one-row
// summary plus one row per conflict cluster — the community, its size, the
// colliding PR numbers, a suggested merge order, and the conflict-risk
// score. The PR-number lists are flattened to comma-joined strings, the
// same shape list_prs uses for its blocker list.
func encodeConflictsPRs(result map[string]any) ([]byte, error) {
	var buf bytes.Buffer

	total, _ := result["total"].(int)
	sumEnc := newGCX(&buf, "conflicts_prs.summary", []string{"total"})
	if err := sumEnc.WriteRow(total); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}

	conEnc := newGCX(&buf, "conflicts_prs.conflicts",
		[]string{"community", "size", "prs", "suggested_order", "risk"},
	)
	if conflicts, ok := result["conflicts"].([]map[string]any); ok {
		for _, c := range conflicts {
			community, _ := c["community"].(string)
			size, _ := c["size"].(int)
			prs, _ := c["prs"].([]int)
			order, _ := c["suggested_order"].([]int)
			risk, _ := c["risk"].(float64)
			if err := conEnc.WriteRow(
				community, size, joinInts(prs), joinInts(order), roundFloat(risk),
			); err != nil {
				return nil, err
			}
		}
	}
	if err := conEnc.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// encodeSiblingDiffContext encodes the sibling_diff_context payload as GCX1: a
// one-row summary (total + focus list) plus a per-sibling section carrying the
// relation tag, relatedness score, and the raw diff text. The map shape mirrors
// siblingDiffPayload so JSON and GCX share one set of field names.
func encodeSiblingDiffContext(result map[string]any) ([]byte, error) {
	var buf bytes.Buffer

	total, _ := result["total"].(int)
	focus, _ := result["focus"].([]string)
	sumEnc := newGCX(&buf, "sibling_diff_context.summary", []string{"total", "focus"})
	if err := sumEnc.WriteRow(total, strings.Join(focus, ",")); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}

	sibEnc := newGCX(&buf, "sibling_diff_context.siblings",
		[]string{"file", "relation", "score", "diff"},
	)
	if siblings, ok := result["siblings"].([]map[string]any); ok {
		for _, sib := range siblings {
			file, _ := sib["file"].(string)
			relation, _ := sib["relation"].(string)
			score, _ := sib["score"].(float64)
			diff, _ := sib["diff"].(string)
			if err := sibEnc.WriteRow(file, relation, roundFloat(score), diff); err != nil {
				return nil, err
			}
		}
	}
	if err := sibEnc.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// encodeReview renders the review tool's verdict envelope + line-anchored
// inline comments + per-file risk into GCX1: a one-row summary section, a
// comments row-section, and a file-risk row-section.
func encodeReview(result map[string]any) ([]byte, error) {
	var buf bytes.Buffer

	verdict, _ := result["verdict"].(string)
	summary, _ := result["summary"].(string)
	total, _ := result["total"].(int)
	depth, _ := result["depth"].(string)
	sumEnc := newGCX(&buf, "review.summary", []string{"verdict", "total", "depth", "summary"})
	if err := sumEnc.WriteRow(verdict, total, depth, summary); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}

	comEnc := newGCX(&buf, "review.comments",
		[]string{"file", "line", "severity", "category", "rule", "source", "message"},
	)
	if comments, ok := result["comments"].([]map[string]any); ok {
		for _, c := range comments {
			file, _ := c["file"].(string)
			line, _ := c["line"].(int)
			severity, _ := c["severity"].(string)
			category, _ := c["category"].(string)
			rule, _ := c["rule"].(string)
			source, _ := c["source"].(string)
			message, _ := c["message"].(string)
			if err := comEnc.WriteRow(file, line, severity, category, rule, source, message); err != nil {
				return nil, err
			}
		}
	}
	if err := comEnc.Close(); err != nil {
		return nil, err
	}

	riskEnc := newGCX(&buf, "review.file_risk", []string{"file", "risk", "findings", "affected", "symbols", "uncovered"})
	if risks, ok := result["file_risk"].([]map[string]any); ok {
		for _, r := range risks {
			file, _ := r["file"].(string)
			risk, _ := r["risk"].(string)
			findings, _ := r["findings"].(int)
			affected, _ := r["affected"].(int)
			symbols, _ := r["symbols"].(int)
			uncovered, _ := r["uncovered"].(int)
			if err := riskEnc.WriteRow(file, risk, findings, affected, symbols, uncovered); err != nil {
				return nil, err
			}
		}
	}
	if err := riskEnc.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// encodePRReviewContext renders the deterministic PR-review rollup into GCX1:
// a one-row summary (verdict + changed-file / changed-symbol counts), a gates
// row-section (one row per evaluated section with its status + detail), and a
// diff_context row-section carrying each changed symbol's risk + fan counts.
// The append-only section layout keeps the encoder forward compatible — a new
// section adds a row-section, never reorders an existing one.
func encodePRReviewContext(out prReviewContext) ([]byte, error) {
	var buf bytes.Buffer

	sumEnc := newGCX(&buf, "pr_review_context.summary",
		[]string{"verdict", "changed_files", "changed_symbols"},
	)
	if err := sumEnc.WriteRow(out.Verdict, len(out.ChangedFiles), out.ChangedSymbols); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}

	gateEnc := newGCX(&buf, "pr_review_context.gates",
		[]string{"name", "status", "detail"},
	)
	for _, g := range out.Gates {
		if err := gateEnc.WriteRow(g.Name, g.Status, g.Detail); err != nil {
			return nil, err
		}
	}
	if err := gateEnc.Close(); err != nil {
		return nil, err
	}

	dcEnc := newGCX(&buf, "pr_review_context.diff_context",
		[]string{"id", "kind", "risk", "callers", "callees", "signature"},
	)
	for _, sym := range out.DiffContext {
		if err := dcEnc.WriteRow(sym.ID, sym.Kind, sym.Risk, len(sym.Callers), len(sym.Callees), sym.Signature); err != nil {
			return nil, err
		}
	}
	if err := dcEnc.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// encodeCritiqueReview renders the self-critique result into GCX1: a one-row
// summary (revised verdict + kept/dropped/uncertain counts + whether the LLM
// adjudicated), a kept-findings row-section, and a dropped-findings row-section
// carrying the critique verdict + reason per finding. The append-only section
// layout keeps the encoder forward compatible — a new section adds a row-section,
// never reorders an existing one.
func encodeCritiqueReview(out critiqueReviewResult) ([]byte, error) {
	var buf bytes.Buffer

	sumEnc := newGCX(&buf, "critique_review.summary",
		[]string{"verdict", "kept", "dropped", "uncertain", "total", "llm_used", "elapsed_ms", "summary"},
	)
	if err := sumEnc.WriteRow(out.Verdict, out.KeptCount, len(out.Dropped), out.Uncertain,
		out.Total, out.LLMUsed, out.ElapsedMs, out.Summary); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}

	keptEnc := newGCX(&buf, "critique_review.kept",
		[]string{"file", "line", "severity", "category", "rule", "message"},
	)
	for _, f := range out.Kept {
		line := f.Line
		if line == 0 {
			line = f.StartLine
		}
		if err := keptEnc.WriteRow(f.File, line, string(f.Severity), f.Category, f.Rule, f.Message); err != nil {
			return nil, err
		}
	}
	if err := keptEnc.Close(); err != nil {
		return nil, err
	}

	dropEnc := newGCX(&buf, "critique_review.dropped",
		[]string{"file", "line", "severity", "category", "rule", "critique_verdict", "critique_reason", "message"},
	)
	for _, d := range out.Dropped {
		f := d.Finding
		line := f.Line
		if line == 0 {
			line = f.StartLine
		}
		if err := dropEnc.WriteRow(f.File, line, string(f.Severity), f.Category, f.Rule,
			string(d.Verdict), d.Reason, f.Message); err != nil {
			return nil, err
		}
	}
	if err := dropEnc.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// encodeReviewPack renders the packaged review envelope into GCX1: a one-row
// summary (verdict + the gate rollups), a changed-symbol classification
// row-section, a per-file risk row-section, a findings row-section, and a guards
// row-section. The append-only section layout keeps the encoder forward
// compatible — a new gate adds a section, never reorders an existing one.
func encodeReviewPack(result map[string]any) ([]byte, error) {
	var buf bytes.Buffer

	verdict, _ := result["verdict"].(string)
	summary, _ := result["summary"].(string)
	verCmd, _ := result["verification_command"].(string)
	total, _ := result["total"].(int)
	depth, _ := result["depth"].(string)
	guards, _ := result["guards"].([]analysis.GuardViolation)
	breaking := 0
	if ci, ok := result["contracts"].(*contractImpact); ok && ci != nil {
		breaking = ci.Breaking
	}
	sumEnc := newGCX(&buf, "review_pack.summary",
		[]string{"verdict", "findings", "depth", "guard_violations", "contract_breaking", "verification_command", "summary"},
	)
	if err := sumEnc.WriteRow(verdict, total, depth, len(guards), breaking, verCmd, summary); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}

	symEnc := newGCX(&buf, "review_pack.changed_symbols",
		[]string{"id", "name", "class", "risk"},
	)
	if syms, ok := result["changed_symbols"].([]map[string]any); ok {
		for _, sym := range syms {
			id, _ := sym["id"].(string)
			name, _ := sym["name"].(string)
			class, _ := sym["class"].(string)
			risk, _ := sym["risk"].(string)
			if err := symEnc.WriteRow(id, name, class, risk); err != nil {
				return nil, err
			}
		}
	}
	if err := symEnc.Close(); err != nil {
		return nil, err
	}

	riskEnc := newGCX(&buf, "review_pack.file_risk", []string{"file", "risk", "findings", "affected", "symbols", "uncovered"})
	if risks, ok := result["file_risk"].([]map[string]any); ok {
		for _, r := range risks {
			file, _ := r["file"].(string)
			risk, _ := r["risk"].(string)
			findings, _ := r["findings"].(int)
			affected, _ := r["affected"].(int)
			symbols, _ := r["symbols"].(int)
			uncovered, _ := r["uncovered"].(int)
			if err := riskEnc.WriteRow(file, risk, findings, affected, symbols, uncovered); err != nil {
				return nil, err
			}
		}
	}
	if err := riskEnc.Close(); err != nil {
		return nil, err
	}

	findEnc := newGCX(&buf, "review_pack.findings",
		[]string{"file", "line", "severity", "category", "rule", "source", "message"},
	)
	if findings, ok := result["findings"].([]map[string]any); ok {
		for _, f := range findings {
			file, _ := f["file"].(string)
			line, _ := f["line"].(int)
			severity, _ := f["severity"].(string)
			category, _ := f["category"].(string)
			rule, _ := f["rule"].(string)
			source, _ := f["source"].(string)
			message, _ := f["message"].(string)
			if err := findEnc.WriteRow(file, line, severity, category, rule, source, message); err != nil {
				return nil, err
			}
		}
	}
	if err := findEnc.Close(); err != nil {
		return nil, err
	}

	guardEnc := newGCX(&buf, "review_pack.guards",
		[]string{"rule_name", "kind", "description"},
	)
	for _, v := range guards {
		if err := guardEnc.WriteRow(v.RuleName, v.Kind, v.Description); err != nil {
			return nil, err
		}
	}
	if err := guardEnc.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// joinInts renders a slice of ints as a comma-joined string for GCX1
// scalar columns that carry a small list (e.g. colliding PR numbers).
func joinInts(xs []int) string {
	if len(xs) == 0 {
		return ""
	}
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ",")
}
