package mcp

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	wire "github.com/gortexhq/gcx-go"
	"github.com/zzet/gortex/internal/callpath"
	"github.com/zzet/gortex/internal/dataflow"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

// registerDataflowTools wires the CPG-lite dataflow MCP surface.
// Two tools ship today:
//
//   - flow_between(source_id, sink_id, max_depth?, max_paths?) →
//     ranked dataflow paths between a specific pair of symbols.
//   - taint_paths(source_pattern, sink_pattern, max_depth?,
//     limit?) → pattern-driven sweep returning every flow from a
//     matching source to a matching sink.
//
// Both tools accept format=gcx for the GCX1 wire format; the
// per-tool encoders live in this file alongside the handlers.
func (s *Server) registerDataflowTools() {
	s.addTool(
		mcp.NewTool("flow_between",
			mcp.WithDescription("Returns ranked dataflow paths between two symbols. Walks EdgeValueFlow / EdgeArgOf / EdgeReturnsTo forward from source to sink — the CPG-lite primitive that answers \"where does this value flow?\". Pairs with taint_paths for pattern-driven sweeps. Every EdgeStep carries an origin tier (lsp_resolved / ast_resolved / …) and a coarse `tier` label (lsp / ast / heuristic); pass `min_tier` to prune edges below a threshold during traversal."),
			mcp.WithString("source_id", mcp.Required(), mcp.Description("Source symbol node ID — typically a function, method, or parameter")),
			mcp.WithString("sink_id", mcp.Required(), mcp.Description("Sink symbol node ID — function/method/param/field")),
			mcp.WithNumber("max_depth", mcp.Description("Maximum BFS hops (default: 8)")),
			mcp.WithNumber("max_paths", mcp.Description("Maximum number of paths to return (default: 10)")),
			mcp.WithString("min_tier", mcp.Description("Minimum per-edge Origin tier to traverse. Accepts one of: lsp_resolved, lsp_dispatch, ast_resolved, ast_inferred, text_matched. Empty (default) disables the filter.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleFlowBetween,
	)

	s.addTool(
		mcp.NewTool("taint_paths",
			mcp.WithDescription("Pattern-driven dataflow sweep — resolves every symbol matching `source_pattern` and `sink_pattern`, then walks the dataflow graph to find paths between each pair. Use for security-style queries (\"every flow from os.Getenv to db.Query\") and architectural audits. Pattern syntax: bare token = case-insensitive substring on symbol name; `exact:Foo` = exact name; `path:dir/` = file-path prefix; `kind:method` = restrict node kind. Combine clauses with spaces. Pass `min_tier` to prune per-edge traversal below a provenance threshold."),
			mcp.WithString("source_pattern", mcp.Required(), mcp.Description("Source pattern — see description for syntax")),
			mcp.WithString("sink_pattern", mcp.Required(), mcp.Description("Sink pattern — see description for syntax")),
			mcp.WithNumber("max_depth", mcp.Description("Maximum BFS hops per (source,sink) pair (default: 8)")),
			mcp.WithNumber("limit", mcp.Description("Maximum findings to return (default: 20)")),
			mcp.WithString("min_tier", mcp.Description("Minimum per-edge Origin tier to traverse. Accepts one of: lsp_resolved, lsp_dispatch, ast_resolved, ast_inferred, text_matched. Empty (default) disables the filter.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleTaintPaths,
	)

	s.addTool(
		mcp.NewTool("trace_path",
			mcp.WithDescription("Traces the shortest call path from one symbol to another over the CALLS-class graph (calls + cross-service matches + method-value references), and — when no path exists — returns a structured why-unreachable diagnosis. Distinct from flow_between (dataflow: where a *value* moves) and get_call_chain (single-source forward call BFS, no target): trace_path is a *targeted* A→B search using balanced bidirectional BFS, so it returns THE shortest route with per-hop provenance (lsp/ast/heuristic). On failure it names exactly where the chain dies: the furthest functions reachable from the source, the nearest set that can reach the sink, the dynamic-dispatch / external boundaries the reach terminated at, and a classified reason (crosses_dynamic_dispatch, crosses_external_boundary, depth_exceeded, disconnected, src_no_out_edges, sink_no_in_edges)."),
			mcp.WithString("source_id", mcp.Required(), mcp.Description("Source symbol node ID — the call-path origin (typically a function or method)")),
			mcp.WithString("sink_id", mcp.Required(), mcp.Description("Sink symbol node ID — the call-path destination")),
			mcp.WithNumber("max_depth", mcp.Description("Maximum combined BFS depth before giving up (default: 24)")),
			mcp.WithNumber("k", mcp.Description("Number of distinct shortest-length paths to return (default: 1)")),
			mcp.WithNumber("max_frontier", mcp.Description("Cap on the number of frontier nodes reported in the gap diagnosis (default: 25)")),
			mcp.WithString("min_tier", mcp.Description("Minimum per-edge Origin tier to traverse. Accepts one of: lsp_resolved, lsp_dispatch, ast_resolved, ast_inferred, text_matched. Empty (default) disables the filter.")),
			mcp.WithBoolean("include_references", mcp.Description("Traverse EdgeReferences (method-value wiring: mux.HandleFunc, command tables) in addition to direct calls (default: true). Set false for a pure direct-call path.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleTracePath,
	)
}

func (s *Server) handleFlowBetween(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	source, err := req.RequireString("source_id")
	if err != nil {
		return mcp.NewToolResultError("source_id is required"), nil
	}
	sink, err := req.RequireString("sink_id")
	if err != nil {
		return mcp.NewToolResultError("sink_id is required"), nil
	}
	maxDepth := req.GetInt("max_depth", dataflow.DefaultMaxDepth)
	maxPaths := req.GetInt("max_paths", dataflow.DefaultMaxPaths)
	minTier := req.GetString("min_tier", "")

	// dataflow.New builds its traversal state over a graph.Store, so these
	// paths are computed over the base corpus even when the request carries
	// an overlay or a routed view. Under a view that is a base-scoped answer
	// the caller has to be told about.
	engine := dataflow.New(s.graph).WithRefiner(s.dataflowRefiner(ctx))
	paths := engine.FlowBetweenWithTier(source, sink, maxDepth, maxPaths, minTier)
	annotateBaseScoped(ctx, graphview.CapSyntaxGraph, graphview.CapResolutionLocal)

	if s.isGCX(ctx, req) {
		payload, err := encodeFlowBetween(source, sink, paths)
		return s.gcxResponseWithBudget(req)(payload, err)
	}

	result := map[string]any{
		"source_id": source,
		"sink_id":   sink,
		"paths":     paths,
		"total":     len(paths),
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

func (s *Server) handleTracePath(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	source, err := req.RequireString("source_id")
	if err != nil {
		return mcp.NewToolResultError("source_id is required"), nil
	}
	sink, err := req.RequireString("sink_id")
	if err != nil {
		return mcp.NewToolResultError("sink_id is required"), nil
	}
	opts := callpath.Options{
		MaxDepth:          req.GetInt("max_depth", callpath.DefaultMaxDepth),
		K:                 req.GetInt("k", callpath.DefaultK),
		MaxFrontier:       req.GetInt("max_frontier", callpath.DefaultMaxFrontier),
		MinTier:           req.GetString("min_tier", ""),
		IncludeReferences: req.GetBool("include_references", true),
	}
	// callpath.New builds its traversal state over a graph.Store, so this
	// path is computed over the base corpus even when the request carries an
	// overlay or a routed view, and says so on the response under one.
	res := callpath.New(s.graph).ShortestPath(source, sink, opts)
	annotateBaseScoped(ctx, graphview.CapSyntaxGraph, graphview.CapResolutionLocal)

	if s.isGCX(ctx, req) {
		payload, encErr := encodeTracePath(res)
		return s.gcxResponseWithBudget(req)(payload, encErr)
	}
	if s.isTOON(ctx, req) {
		return returnTOON(res)
	}
	return s.respondJSONOrTOON(ctx, req, res)
}

func (s *Server) handleTaintPaths(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	srcRaw, err := req.RequireString("source_pattern")
	if err != nil {
		return mcp.NewToolResultError("source_pattern is required"), nil
	}
	sinkRaw, err := req.RequireString("sink_pattern")
	if err != nil {
		return mcp.NewToolResultError("sink_pattern is required"), nil
	}
	maxDepth := req.GetInt("max_depth", dataflow.DefaultMaxDepth)
	limit := req.GetInt("limit", 20)
	minTier := req.GetString("min_tier", "")

	src := dataflow.ParsePattern(srcRaw)
	if src.Empty() {
		return mcp.NewToolResultError("source_pattern matched no clauses"), nil
	}
	sink := dataflow.ParsePattern(sinkRaw)
	if sink.Empty() {
		return mcp.NewToolResultError("sink_pattern matched no clauses"), nil
	}

	// dataflow.New builds its traversal state over a graph.Store, so these
	// findings are computed over the base corpus even when the request
	// carries an overlay or a routed view, and say so on the response under
	// one.
	engine := dataflow.New(s.graph).WithRefiner(s.dataflowRefiner(ctx))
	findings := engine.TaintPathsWithTier(src, sink, maxDepth, limit, minTier)
	annotateBaseScoped(ctx, graphview.CapSyntaxGraph, graphview.CapResolutionLocal)

	if s.isGCX(ctx, req) {
		payload, err := encodeTaintPaths(srcRaw, sinkRaw, findings)
		return s.gcxResponseWithBudget(req)(payload, err)
	}

	rows := make([]map[string]any, 0, len(findings))
	for _, f := range findings {
		rows = append(rows, map[string]any{
			"source": describeNode(f.Source),
			"sink":   describeNode(f.Sink),
			"paths":  f.Paths,
		})
	}
	result := map[string]any{
		"source_pattern": srcRaw,
		"sink_pattern":   sinkRaw,
		"findings":       rows,
		"total":          len(findings),
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

// dataflowRefiner builds the per-call CFG-backed refiner that
// confirms or prunes same-function value_flow hops on
// flow_between / taint_paths paths. The source resolver reads
// through the session's overlay so unsaved buffers refine
// consistently with every other tool.
func (s *Server) dataflowRefiner(ctx context.Context) *dataflow.Refiner {
	resolve := func(fn *graph.Node) (dataflow.FuncSource, error) {
		if fn.StartLine == 0 || fn.EndLine == 0 {
			return dataflow.FuncSource{}, fmt.Errorf("symbol has no line range: %s", fn.ID)
		}
		absPath, err := s.resolveNodePath(ctx, fn)
		if err != nil {
			return dataflow.FuncSource{}, err
		}
		src, fromLine, _, err := s.readLinesForCtx(ctx, absPath, fn.StartLine, fn.EndLine, 0)
		if err != nil {
			return dataflow.FuncSource{}, err
		}
		return dataflow.FuncSource{Src: []byte(src), StartLine: fromLine}, nil
	}
	// dataflow.NewRefiner caches per-function analyses keyed off a
	// graph.Store, so its symbol lookups read the base corpus even when the
	// request carries an overlay view; only the source resolver above is
	// overlay-aware.
	return dataflow.NewRefiner(s.graph, resolve, 0)
}

// describeNode returns a JSON-shaped summary of a graph node for
// taint findings.
func describeNode(n *graph.Node) map[string]any {
	if n == nil {
		return nil
	}
	return map[string]any{
		"id":         n.ID,
		"kind":       string(n.Kind),
		"name":       n.Name,
		"file_path":  n.FilePath,
		"start_line": n.StartLine,
	}
}

// encodeFlowBetween emits a GCX1 envelope with two sections:
// `flow_between.summary` (one row carrying source, sink, totals and
// the weakest tier seen across all returned paths) and
// `flow_between.paths` (one row per path with the flattened node
// sequence, edge kind sequence, per-step origin + tier sequences,
// and the weakest tier on the path).
func encodeFlowBetween(source, sink string, paths []dataflow.Path) ([]byte, error) {
	var buf bytes.Buffer
	sumEnc := wire.NewEncoder(&buf, wire.Header{
		Tool:   "flow_between.summary",
		Fields: []string{"source", "sink", "paths", "shortest", "worst_tier"},
	})
	shortest := 0
	worstAll := ""
	if len(paths) > 0 {
		shortest = paths[0].Length()
		for _, p := range paths {
			worstAll = mergeWorstTier(worstAll, worstTierOnPath(p.Edges))
		}
	}
	if err := sumEnc.WriteRow(source, sink, len(paths), shortest, worstAll); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}
	pathEnc := wire.NewEncoder(&buf, wire.Header{
		Tool:   "flow_between.paths",
		Fields: []string{"length", "confidence", "worst_tier", "ids", "kinds", "origins", "tiers", "refined"},
		Meta: map[string]string{
			"count": fmt.Sprintf("%d", len(paths)),
		},
	})
	for _, p := range paths {
		ids := joinPathIDs(p.IDs)
		kinds := joinEdgeKinds(p.Edges)
		origins := joinEdgeOrigins(p.Edges)
		tiers := joinEdgeTiers(p.Edges)
		refined := joinEdgeRefined(p.Edges)
		if err := pathEnc.WriteRow(p.Length(), p.Confidence, worstTierOnPath(p.Edges), ids, kinds, origins, tiers, refined); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), pathEnc.Close()
}

// encodeTracePath emits a GCX1 envelope with up to three sections:
// `trace_path.summary` (one row), `trace_path.paths` (one row per returned
// path with the flattened node + edge sequences) and — only when no path was
// found — `trace_path.gap` (one row carrying the why-unreachable diagnosis with
// the frontier and boundary lists flattened).
func encodeTracePath(res callpath.Result) ([]byte, error) {
	var buf bytes.Buffer
	reason := ""
	if res.Gap != nil {
		reason = string(res.Gap.Reason)
	}
	shortest := 0
	worstAll := ""
	if len(res.Paths) > 0 {
		shortest = res.Paths[0].Length
		for _, p := range res.Paths {
			worstAll = mergeWorstTier(worstAll, p.WorstTier)
		}
	}
	sumEnc := wire.NewEncoder(&buf, wire.Header{
		Tool:   "trace_path.summary",
		Fields: []string{"source", "sink", "found", "paths", "shortest", "worst_tier", "reason"},
	})
	if err := sumEnc.WriteRow(res.SrcID, res.SinkID, res.Found, len(res.Paths), shortest, worstAll, reason); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}
	pathEnc := wire.NewEncoder(&buf, wire.Header{
		Tool:   "trace_path.paths",
		Fields: []string{"length", "confidence", "worst_tier", "nodes", "kinds", "origins", "tiers"},
		Meta:   map[string]string{"count": fmt.Sprintf("%d", len(res.Paths))},
	})
	for _, p := range res.Paths {
		if err := pathEnc.WriteRow(p.Length, p.Confidence, p.WorstTier,
			strings.Join(p.Nodes, ","),
			joinTraceField(p.Edges, func(e callpath.PathEdge) string { return e.Kind }),
			joinTraceField(p.Edges, func(e callpath.PathEdge) string { return e.Origin }),
			joinTraceField(p.Edges, func(e callpath.PathEdge) string { return traceTier(e) })); err != nil {
			return nil, err
		}
	}
	if err := pathEnc.Close(); err != nil {
		return nil, err
	}
	if res.Gap != nil {
		gapEnc := wire.NewEncoder(&buf, wire.Header{
			Tool:   "trace_path.gap",
			Fields: []string{"reason", "message", "forward_reached", "backward_reached", "furthest_from_source", "nearest_to_sink", "boundary_hits"},
		})
		if err := gapEnc.WriteRow(
			string(res.Gap.Reason), res.Gap.Message,
			res.Gap.ForwardReached, res.Gap.BackwardReached,
			joinFrontier(res.Gap.FurthestFromSource),
			joinFrontier(res.Gap.NearestToSink),
			joinBoundaries(res.Gap.BoundaryHits)); err != nil {
			return nil, err
		}
		if err := gapEnc.Close(); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func traceTier(e callpath.PathEdge) string {
	if e.Tier != "" {
		return e.Tier
	}
	return graph.ResolvedBy(e.Origin)
}

func joinTraceField(edges []callpath.PathEdge, f func(callpath.PathEdge) string) string {
	if len(edges) == 0 {
		return ""
	}
	parts := make([]string, len(edges))
	for i, e := range edges {
		parts[i] = f(e)
	}
	return strings.Join(parts, ",")
}

func joinFrontier(ns []callpath.FrontierNode) string {
	if len(ns) == 0 {
		return ""
	}
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = fmt.Sprintf("%s:%d", n.ID, n.Depth)
	}
	return strings.Join(parts, ",")
}

func joinBoundaries(bs []callpath.BoundaryHit) string {
	if len(bs) == 0 {
		return ""
	}
	parts := make([]string, len(bs))
	for i, b := range bs {
		parts[i] = fmt.Sprintf("%s|%s", b.Target, b.Reason)
	}
	return strings.Join(parts, ",")
}

// encodeTaintPaths emits a GCX1 envelope with two sections:
// `taint_paths.summary` and `taint_paths.findings`. Each finding
// row carries the best (shortest, highest-confidence) path; the
// other paths are joined into a parallel field for offline drill-
// down without repeating the source/sink columns per path.
func encodeTaintPaths(srcPattern, sinkPattern string, findings []dataflow.TaintFinding) ([]byte, error) {
	var buf bytes.Buffer
	sumEnc := wire.NewEncoder(&buf, wire.Header{
		Tool:   "taint_paths.summary",
		Fields: []string{"source_pattern", "sink_pattern", "findings"},
	})
	if err := sumEnc.WriteRow(srcPattern, sinkPattern, len(findings)); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}
	findEnc := wire.NewEncoder(&buf, wire.Header{
		Tool: "taint_paths.findings",
		Fields: []string{
			"source_id", "source_name", "sink_id", "sink_name",
			"best_length", "best_confidence", "best_worst_tier",
			"paths", "best_ids", "best_kinds", "best_origins", "best_tiers", "best_refined",
		},
	})
	for _, f := range findings {
		best := dataflow.Path{}
		if len(f.Paths) > 0 {
			best = f.Paths[0]
		}
		row := []any{
			nodeIDOf(f.Source),
			nodeNameOf(f.Source),
			nodeIDOf(f.Sink),
			nodeNameOf(f.Sink),
			best.Length(),
			best.Confidence,
			worstTierOnPath(best.Edges),
			len(f.Paths),
			joinPathIDs(best.IDs),
			joinEdgeKinds(best.Edges),
			joinEdgeOrigins(best.Edges),
			joinEdgeTiers(best.Edges),
			joinEdgeRefined(best.Edges),
		}
		if err := findEnc.WriteRow(row...); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), findEnc.Close()
}

func nodeIDOf(n *graph.Node) string {
	if n == nil {
		return ""
	}
	return n.ID
}

func nodeNameOf(n *graph.Node) string {
	if n == nil {
		return ""
	}
	return n.Name
}

func joinPathIDs(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	var b bytes.Buffer
	for i, id := range ids {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(id)
	}
	return b.String()
}

func joinEdgeKinds(edges []dataflow.EdgeStep) string {
	if len(edges) == 0 {
		return ""
	}
	var b bytes.Buffer
	for i, e := range edges {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(e.Kind)
	}
	return b.String()
}

func joinEdgeOrigins(edges []dataflow.EdgeStep) string {
	if len(edges) == 0 {
		return ""
	}
	var b bytes.Buffer
	for i, e := range edges {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(e.Origin)
	}
	return b.String()
}

// joinEdgeRefined flattens the per-step refinement markers; empty
// markers stay empty so positions align with the kinds/tiers fields.
func joinEdgeRefined(edges []dataflow.EdgeStep) string {
	if len(edges) == 0 {
		return ""
	}
	parts := make([]string, len(edges))
	any := false
	for i, e := range edges {
		parts[i] = e.Refined
		if e.Refined != "" {
			any = true
		}
	}
	if !any {
		return ""
	}
	return strings.Join(parts, ",")
}

func joinEdgeTiers(edges []dataflow.EdgeStep) string {
	if len(edges) == 0 {
		return ""
	}
	var b bytes.Buffer
	for i, e := range edges {
		if i > 0 {
			b.WriteString(",")
		}
		tier := e.Tier
		if tier == "" {
			tier = graph.ResolvedBy(e.Origin)
		}
		b.WriteString(tier)
	}
	return b.String()
}

// worstTierOnPath returns the lowest-confidence tier label across
// every step. The mapping is symmetric with graph.ResolvedBy:
// "lsp" > "ast" > "heuristic". An empty step list returns "".
func worstTierOnPath(edges []dataflow.EdgeStep) string {
	if len(edges) == 0 {
		return ""
	}
	worst := ""
	for _, e := range edges {
		tier := e.Tier
		if tier == "" {
			tier = graph.ResolvedBy(e.Origin)
		}
		worst = mergeWorstTier(worst, tier)
	}
	return worst
}

// mergeWorstTier collapses two tier labels into the lower-confidence
// one. Used by both per-path and across-path summaries so callers
// see the weakest link without recomputing the mapping.
func mergeWorstTier(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	if tierRank(a) <= tierRank(b) {
		return a
	}
	return b
}

func tierRank(tier string) int {
	switch tier {
	case "lsp":
		return 3
	case "ast":
		return 2
	case "heuristic":
		return 1
	}
	return 0
}
