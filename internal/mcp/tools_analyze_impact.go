package mcp

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
	"github.com/zzet/gortex/internal/reach"
)

// ---------------------------------------------------------------------------
// analyze kind=impact
// ---------------------------------------------------------------------------
//
// Composite per-symbol change-impact score. Where health_score grades
// code *quality*, impact grades *blast radius* — how much breaks, and
// how hard it is to change safely. Five axes, each normalised to
// 0..100 (higher = more impactful), then combined into one weighted
// composite:
//
//   - centrality  — PageRank over the call graph. A symbol central
//                   nodes depend on carries architectural weight.
//   - reach       — transitive dependent count (precomputed reach
//                   index when available, else direct fan-in).
//   - complexity  — McCabe cyclomatic complexity. Branchy bodies are
//                   harder to change without regressions.
//   - co_change   — git-history logical coupling of the symbol's file.
//                   Strong coupling means a change ripples to files
//                   the import graph never connects.
//   - community   — how many distinct graph communities the symbol's
//                   direct neighbours span. Cross-community symbols
//                   are architectural seams.
//
// The composite is a single ranked number so an agent can ask "what
// are the highest-impact symbols" or "how risky is changing X" with
// one call instead of fusing hotspots + explain_change_impact +
// find_co_changing_symbols by hand.

// Per-axis composite weights. The formula is the user-visible
// contract — named constants so a change is deliberate. Centrality
// and reach dominate: they answer "how much depends on this".
const (
	impactWeightCentrality = 2.5
	impactWeightReach      = 2.5
	impactWeightComplexity = 1.5
	impactWeightCoChange   = 1.5
	impactWeightCommunity  = 1.0
)

// Axis saturation constants — the half-saturation point of each
// 100*x/(x+k) curve, picked so a typical "notable" value lands near
// the 60-75 band rather than pinning the axis at 100.
const (
	impactReachK        = 30.0
	impactComplexityK   = 8.0
	impactCoChangeK     = 2.0
	impactCommunityStep = 25.0 // neighbours spanning 4 communities = 100
)

// impactRow is the per-symbol breakdown returned by analyze impact.
type impactRow struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Kind  string  `json:"kind"`
	File  string  `json:"file"`
	Line  int     `json:"line"`
	Score float64 `json:"score"`
	Risk  string  `json:"risk"`

	// Axes — each a 0..100 impact value.
	Centrality float64 `json:"centrality"`
	Reach      float64 `json:"reach"`
	Complexity float64 `json:"complexity"`
	CoChange   float64 `json:"co_change"`
	Community  float64 `json:"community"`

	// Raw inputs behind the axes, for explainability.
	PageRank        float64 `json:"pagerank"`
	ReachCount      int     `json:"reach_count"`
	ReachLowerBound bool    `json:"reach_lower_bound,omitempty"`
	Cyclomatic      int     `json:"cyclomatic"`
	CoChangeFiles   int     `json:"co_change_files"`
	CommunitySpan   int     `json:"community_span"`
	FanIn           int     `json:"fan_in"`

	// Depth and Target are populated only for a target-scoped call:
	// Target marks the symbol the caller asked about, Depth is the
	// row's distance from it (1..3 dependent tiers). The repo-wide
	// ranking has no target to measure from and omits both.
	Depth  int  `json:"depth,omitempty"`
	Target bool `json:"target,omitempty"`
}

// impactTargetScope is the dependency closure behind a target-scoped
// impact call: the seeds the caller named plus every transitive
// dependent the graph knows about, keyed by BFS depth.
type impactTargetScope struct {
	Symbol  string         // resolved target.symbol, when a symbol was named
	File    string         // target.file, when a file was named
	Seeds   []string       // depth-0 symbols — always scored
	Depth   map[string]int // closure member -> 0 (seed) .. 3
	ByDepth map[int]int    // depth -> dependent count
	Total   int            // dependents only, i.e. depth >= 1

	// LowerBound and Truncated ride from the shared blast-radius
	// analyzer. An unbound dispatch site or an exhausted traversal
	// budget makes the closure a floor — never proof that nothing
	// else depends on the target.
	LowerBound bool
	Truncated  bool
}

// impactTargetPayload is the GCX payload for a target-scoped call. The
// closure metadata rides in the encoder header because a caller who
// only sees rows cannot tell a complete blast radius from a bounded
// one.
type impactTargetPayload struct {
	Target *impactTargetScope
	Rows   []impactRow
}

// zeroDependentsCaveat is the wording change impact already uses for an
// empty blast radius. An extraction or resolution gap looks exactly
// like a genuinely leaf symbol, so the two must not be reported
// identically without saying so.
const zeroDependentsCaveat = "zero observed dependents is not proof of zero impact; extraction or resolution gaps may exist"

// impactClosureTimeout bounds the closure walk. It matches the pre-edit
// safety gate's deadline so the same question costs the same whether it
// is asked through change(impact) or analyze(impact).
const impactClosureTimeout = 3 * time.Second

func (t *impactTargetScope) isSeed(id string) bool {
	if t == nil {
		return false
	}
	depth, ok := t.Depth[id]
	return ok && depth == 0
}

// label names the target for the compact and GCX headers.
func (t *impactTargetScope) label() string {
	switch {
	case t == nil:
		return ""
	case t.Symbol != "":
		return t.Symbol
	default:
		return t.File
	}
}

// summary is the response envelope that tells the caller which
// question was answered — which target, how wide its closure, and
// whether that width is exact or a floor.
func (t *impactTargetScope) summary() map[string]any {
	if t == nil {
		return nil
	}
	out := map[string]any{
		"seeds":      t.Seeds,
		"dependents": t.Total,
		"dependents_by_depth": map[string]int{
			"1": t.ByDepth[1], "2": t.ByDepth[2], "3": t.ByDepth[3],
		},
	}
	if t.Symbol != "" {
		out["symbol"] = t.Symbol
	}
	if t.File != "" {
		out["file"] = t.File
	}
	if t.LowerBound {
		out["lower_bound"] = true
	}
	if t.Truncated {
		out["truncated"] = true
	}
	if t.Total == 0 && !t.LowerBound && !t.Truncated {
		out["zero_dependents_caveat"] = zeroDependentsCaveat
	}
	return out
}

func (t *impactTargetScope) compactSummary() string {
	if t == nil {
		return ""
	}
	line := fmt.Sprintf("target %s  dependents=%d (d1=%d d2=%d d3=%d)",
		t.label(), t.Total, t.ByDepth[1], t.ByDepth[2], t.ByDepth[3])
	switch {
	case t.LowerBound || t.Truncated:
		line += "  lower bound — more dependents may exist"
	case t.Total == 0:
		line += "  " + zeroDependentsCaveat
	}
	return line
}

// resolveImpactTarget turns the caller's target selector into the
// dependency closure impact must rank. It returns (nil, nil) when no
// selector was passed — the repo-wide ranking keeps its old meaning.
//
// Every failure is a structured error rather than a fallback to that
// repo-wide ranking: a blast-radius question answered with a global
// hotspot list is indistinguishable from a correct answer, and the
// caller has no way to notice their target was dropped.
func (s *Server) resolveImpactTarget(ctx context.Context, symbol, file string) (*impactTargetScope, *mcp.CallToolResult) {
	if symbol == "" && file == "" {
		return nil, nil
	}
	if symbol != "" && file != "" {
		return nil, NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeInvalidArgument,
			Message:   "analyze impact accepts one target selector — symbol or file, not both",
			Data:      map[string]any{"field": "target", "selectors": []string{"symbol", "file"}},
		})
	}
	if s == nil || s.graph == nil {
		return nil, NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeSymbolNotFound,
			Message:   "impact has no graph to resolve the target against",
			Data:      map[string]any{"field": "target"},
		})
	}

	reader := s.readerFor(ctx)
	scope := &impactTargetScope{Depth: map[string]int{}, ByDepth: map[int]int{}}
	if symbol != "" {
		canonical, ambiguous := s.resolveFacadeSymbolShorthand(ctx, symbol)
		if len(ambiguous) > 0 {
			return nil, NewStructuredErrorResult(StructuredError{
				ErrorCode: ErrCodeInvalidArgument,
				Message:   fmt.Sprintf("impact target %q is ambiguous", symbol),
				Data:      map[string]any{"field": "target.symbol", "symbol": symbol, "candidates": ambiguous},
			})
		}
		node := reader.GetNode(canonical)
		if node == nil || !s.nodeInSessionScope(ctx, node) {
			return nil, NewStructuredErrorResult(StructuredError{
				ErrorCode: ErrCodeSymbolNotFound,
				Message:   fmt.Sprintf("impact target %q is not an indexed symbol in this scope", symbol),
				Data:      map[string]any{"field": "target.symbol", "symbol": symbol},
			})
		}
		scope.Symbol = node.ID
		scope.Seeds = []string{node.ID}
	} else {
		scope.File = file
		for _, node := range reader.GetFileNodes(file) {
			if node == nil || !reach.ImpactSeedKind(node.Kind) || !s.nodeInSessionScope(ctx, node) {
				continue
			}
			scope.Seeds = append(scope.Seeds, node.ID)
		}
		sort.Strings(scope.Seeds)
		if len(scope.Seeds) == 0 {
			return nil, NewStructuredErrorResult(StructuredError{
				ErrorCode: ErrCodeFileNotIndexed,
				Message:   fmt.Sprintf("no indexed symbols in %q — impact has no target to score", file),
				Data:      map[string]any{"field": "target.file", "file": file},
			})
		}
	}
	for _, id := range scope.Seeds {
		scope.Depth[id] = 0
	}

	// The closure comes from the analyzer the pre-edit safety gate
	// already uses (change impact), so the two can never disagree about
	// what depends on the target.
	closureCtx, cancel := context.WithTimeout(ctx, impactClosureTimeout)
	defer cancel()
	communities, processes := s.tryImpactAnalysisSnapshots()
	if result := analysis.AnalyzeImpactContext(closureCtx, reader, scope.Seeds, communities, processes); result != nil {
		scope.LowerBound = result.LowerBound
		scope.Truncated = result.Truncated
		for depth := 1; depth <= 3; depth++ {
			for _, entry := range result.ByDepth[depth] {
				if entry.ID == "" {
					continue
				}
				if _, seen := scope.Depth[entry.ID]; seen {
					continue
				}
				scope.Depth[entry.ID] = depth
				scope.ByDepth[depth]++
				scope.Total++
			}
		}
	}
	return scope, nil
}

// handleAnalyzeImpactComposite ranks symbols by composite change
// impact.
//
// Target — target:{symbol} (or target:{file}) switches the call from
// the repo-wide ranking to that symbol's blast radius: the target
// itself at depth 0 plus its transitive dependents at depths 1..3,
// ranked by composite score. The target row is always present and
// always first; the response carries the closure width and whether it
// is exact. An unresolvable target is a structured error, never a
// silent fallback to the repo-wide ranking.
//
// Filters (applied to the dependents, never to the target):
//   - ids         — comma-separated symbol IDs; score only these
//     (a fixed symbol set, no closure walk).
//   - path_prefix — keep only symbols whose file path starts here.
//   - kinds       — comma-separated (default function,method); "all"
//     keeps every kind.
//   - min_score / max_score — composite-score band filter.
//   - limit       — cap rows (default 100); total reports the
//     pre-truncation count.
func (s *Server) handleAnalyzeImpactComposite(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))
	idFilter := splitIDSet(args["ids"])
	limit := intArg(args, "limit", 100)
	minScore := -1.0
	if v, ok := args["min_score"].(float64); ok {
		minScore = v
	}
	maxScore := -1.0
	if v, ok := args["max_score"].(float64); ok {
		maxScore = v
	}

	// The facade lowers target:{symbol} to id and target:{file} to path.
	target, targetErr := s.resolveImpactTarget(ctx,
		strings.TrimSpace(stringArg(args, "id")),
		strings.TrimSpace(stringArg(args, "path")))
	if targetErr != nil {
		return targetErr, nil
	}

	allowedKinds := map[graph.NodeKind]struct{}{
		graph.KindFunction: {},
		graph.KindMethod:   {},
	}
	if target != nil {
		// A blast radius is not a function-and-method question: a
		// dependent type, field, or constant breaks just as loudly.
		allowedKinds = parseAnalyzeKindsFilter("all")
	}
	if k := strings.TrimSpace(stringArg(args, "kinds")); k != "" {
		allowedKinds = parseAnalyzeKindsFilter(k)
	}

	// Legacy analyze historically starts the co-change mine lazily. The compact
	// read-only adapter fixes refresh_cochange=false, so it consumes only the
	// daemon-prewarmed cache and cannot cause durable EdgeCoChange writes.
	if requestBoolDefault(req, "refresh_cochange", true) {
		s.ensureCoChange()
	}

	pr := s.getPageRank()
	maxPR := 0.0
	if pr != nil {
		maxPR = pr.Max
	}
	nodeToComm := map[string]string{}
	if c := s.getCommunities(); c != nil && c.NodeToComm != nil {
		nodeToComm = c.NodeToComm
	}

	// Build the candidate id set up front so both the fan-in
	// aggregator and the per-edge community walk stay bounded by
	// the kinds / path / ids the caller actually asked for. Without
	// this, the analyzer paid for an unfiltered AllEdges()
	// materialisation per call -- ~500k edges over cgo on the gortex
	// workspace, the bulk of the wall-clock cost on a disk backend.
	scoped := s.scopedNodes(ctx)
	candidateIDs := make([]string, 0, len(scoped))
	candidateSet := make(map[string]struct{}, len(scoped))
	for _, n := range scoped {
		if n == nil {
			continue
		}
		if target != nil {
			if _, inClosure := target.Depth[n.ID]; !inClosure {
				continue
			}
		}
		// The named target is always scored. The filters below narrow the
		// dependents around it; none of them may drop the symbol the
		// question is about.
		if !target.isSeed(n.ID) {
			if allowedKinds != nil {
				if _, ok := allowedKinds[n.Kind]; !ok {
					continue
				}
			}
			if !graphpath.HasPrefix(n.FilePath, pathPrefix) {
				continue
			}
			if len(idFilter) > 0 {
				if _, ok := idFilter[n.ID]; !ok {
					continue
				}
			}
		}
		candidateIDs = append(candidateIDs, n.ID)
		candidateSet[n.ID] = struct{}{}
	}

	// fan-in: uses the NodeFanAggregator capability when the
	// backend supports it (one bulk query per direction over the
	// candidate id set) and falls back to a per-kind EdgesByKind
	// stream otherwise. fanOutKinds is empty -- impact only reads
	// fan-in.
	reader := s.readerFor(ctx)
	// The published reach index is built over the base corpus and keyed off
	// base node metadata, so it is consulted only when this request reads the
	// base graph itself. Under a request overlay or a routed view the fan-in
	// collected here through the request's own reader is the depth-1 stand-in,
	// exactly as it is when no reach record has been published.
	baseReach := baseReachUsable(ctx)
	fanIn, _ := analysis.CollectFanCounts(reader, candidateIDs,
		[]graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences},
		nil,
	)

	// neighborComms[n] = set of distinct communities of n's call /
	// reference neighbours (both directions). Streamed via
	// EdgesByKind per kind so neither backend pays for an
	// unfiltered AllEdges walk; the per-kind MATCH on disk backends
	// is the same plan EdgesByKind feeds every other analyzer.
	// Membership is restricted to candidate ids -- a node outside
	// the result set has nowhere to receive a span count.
	neighborComms := map[string]map[string]struct{}{}
	addComm := func(node, comm string) {
		if comm == "" {
			return
		}
		if _, ok := candidateSet[node]; !ok {
			return
		}
		set := neighborComms[node]
		if set == nil {
			set = map[string]struct{}{}
			neighborComms[node] = set
		}
		set[comm] = struct{}{}
	}
	if target != nil {
		// A target closure is a handful of symbols: two batched edge
		// fetches over the candidate set beat streaming every call and
		// reference edge in the graph. addComm already ignores
		// non-candidates, so either endpoint may be the candidate.
		for _, byNode := range []map[string][]*graph.Edge{
			reader.GetInEdgesByNodeIDs(candidateIDs),
			reader.GetOutEdgesByNodeIDs(candidateIDs),
		} {
			for _, edges := range byNode {
				for _, e := range edges {
					if e == nil || (e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences) {
						continue
					}
					addComm(e.From, nodeToComm[e.To])
					addComm(e.To, nodeToComm[e.From])
				}
			}
		}
	} else {
		for _, kind := range []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences} {
			for e := range reader.EdgesByKind(kind) {
				if e == nil {
					continue
				}
				addComm(e.From, nodeToComm[e.To])
				addComm(e.To, nodeToComm[e.From])
			}
		}
	}

	rows := make([]impactRow, 0, len(candidateIDs))
	for _, n := range scoped {
		if n == nil {
			continue
		}
		if _, ok := candidateSet[n.ID]; !ok {
			continue
		}

		prVal := pr.ScoreOf(n.ID)
		centrality := 0.0
		if maxPR > 0 {
			centrality = 100 * prVal / maxPR
		}

		// Reach: consume an already-published transitive set when available,
		// otherwise direct fan-in is the depth-1 proxy. This must be cache-only:
		// invoking lazy BFS once per candidate turns a ranking call into an
		// accidental eager all-symbol reach build.
		reachCount := fanIn[n.ID]
		reachLowerBound := false
		if d1, d2, d3, hit, bounded := s.impactReachCached(baseReach, n.ID); hit {
			observed := len(d1) + len(d2) + len(d3)
			if observed > reachCount {
				reachCount = observed
			}
			reachLowerBound = bounded
		} else if bounded {
			// Lock/cancellation failure is not evidence of a small reach set.
			reachLowerBound = true
		}
		reachScore := saturate(float64(reachCount), impactReachK)

		cyc := cyclomaticOf(n)
		complexityScore := saturate(float64(cyc-1), impactComplexityK)

		var ccSum float64
		ccFiles := 0
		for _, sc := range s.coChangeScores(n.FilePath) {
			ccSum += sc
			ccFiles++
		}
		coChangeScore := saturate(ccSum, impactCoChangeK)

		span := len(neighborComms[n.ID])
		communityScore := math.Min(100, float64(span)*impactCommunityStep)

		composite := (centrality*impactWeightCentrality +
			reachScore*impactWeightReach +
			complexityScore*impactWeightComplexity +
			coChangeScore*impactWeightCoChange +
			communityScore*impactWeightCommunity) /
			(impactWeightCentrality + impactWeightReach + impactWeightComplexity +
				impactWeightCoChange + impactWeightCommunity)

		row := impactRow{
			ID:              n.ID,
			Name:            n.Name,
			Kind:            string(n.Kind),
			File:            n.FilePath,
			Line:            n.StartLine,
			Score:           roundTo(composite, 2),
			Risk:            impactRisk(composite),
			Centrality:      roundTo(centrality, 2),
			Reach:           roundTo(reachScore, 2),
			Complexity:      roundTo(complexityScore, 2),
			CoChange:        roundTo(coChangeScore, 2),
			Community:       roundTo(communityScore, 2),
			PageRank:        prVal,
			ReachCount:      reachCount,
			ReachLowerBound: reachLowerBound,
			Cyclomatic:      cyc,
			CoChangeFiles:   ccFiles,
			CommunitySpan:   span,
			FanIn:           fanIn[n.ID],
		}
		if reachLowerBound && row.Risk == "LOW" {
			row.Risk = "MEDIUM"
		}
		if target != nil {
			row.Depth = target.Depth[n.ID]
			row.Target = row.Depth == 0
		}
		if !row.Target {
			if minScore >= 0 && row.Score < minScore {
				continue
			}
			if maxScore >= 0 && row.Score > maxScore {
				continue
			}
		}
		rows = append(rows, row)
	}

	// Rank by composite descending — the highest-impact symbols, the
	// ones to change with the most care, surface first.
	sort.Slice(rows, func(i, j int) bool {
		// A target leads its own blast radius; its dependents follow by
		// composite score.
		if rows[i].Target != rows[j].Target {
			return rows[i].Target
		}
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		if rows[i].File != rows[j].File {
			return rows[i].File < rows[j].File
		}
		return rows[i].ID < rows[j].ID
	})

	total := len(rows)
	truncated := false
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	if s.isGCX(ctx, req) {
		if target != nil {
			return s.gcxResponseWithBudget(req)(encodeAnalyze("impact.target",
				impactTargetPayload{Target: target, Rows: rows}))
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("impact", rows))
	}
	if isCompact(req) {
		var b strings.Builder
		if target != nil {
			fmt.Fprintln(&b, target.compactSummary())
		}
		for _, r := range rows {
			if target != nil {
				fmt.Fprintf(&b, "d%d %-8s %6.2f  %s:%d  %s\n", r.Depth, r.Risk, r.Score, r.File, r.Line, r.ID)
				continue
			}
			fmt.Fprintf(&b, "%-8s %6.2f  %s:%d  %s\n", r.Risk, r.Score, r.File, r.Line, r.ID)
		}
		if len(rows) == 0 {
			b.WriteString("no symbols scored\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	resp := map[string]any{
		"symbols":   rows,
		"total":     total,
		"truncated": truncated,
		"weights": map[string]float64{
			"centrality": impactWeightCentrality,
			"reach":      impactWeightReach,
			"complexity": impactWeightComplexity,
			"co_change":  impactWeightCoChange,
			"community":  impactWeightCommunity,
		},
	}
	if target != nil {
		resp["scope"] = "target_closure"
		resp["target"] = target.summary()
	}
	if truncated {
		resp["limit"] = limit
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}

// baseReachUsable reports whether this request may consult the published reach
// index at all. The records are built and stamped on the base store's nodes,
// so only a request that reads the base graph itself describes the corpus they
// were built over: an overlay carries the caller's unsaved edits, and a routed
// view answers about a checkout the base index never saw. Either way the
// records belong to a different corpus than the one the caller asked about.
func baseReachUsable(ctx context.Context) bool {
	return OverlayViewFromContext(ctx) == nil && !requestViewFromContext(ctx).routed()
}

// impactReachCached reads an already-published transitive reach record for one
// candidate, and only for a request the records describe. Reporting a miss
// instead sends the caller back to the fan-in the request's own reader
// produced, which is the same depth-1 stand-in used whenever no record has
// been published.
func (s *Server) impactReachCached(usable bool, id string) (d1, d2, d3 []reach.Entry, hit, truncated bool) {
	if !usable {
		return nil, nil, nil, false, false
	}
	return reach.LookupCached(s.graph, id)
}

// saturate maps a non-negative raw value onto 0..100 with a
// half-saturation point of k: x==k yields 50, x==3k yields 75.
func saturate(x, k float64) float64 {
	if x <= 0 || k <= 0 {
		return 0
	}
	return 100 * x / (x + k)
}

// impactRisk buckets a composite score into a coarse risk label.
func impactRisk(score float64) string {
	switch {
	case score >= 75:
		return "CRITICAL"
	case score >= 55:
		return "HIGH"
	case score >= 35:
		return "MEDIUM"
	default:
		return "LOW"
	}
}

// cyclomaticOf reads the McCabe complexity stamped on a function node
// by the language extractors. The extractors only stamp values above
// 1, so an absent key means the canonical "no branches" score of 1.
func cyclomaticOf(n *graph.Node) int {
	if n == nil || n.Meta == nil {
		return 1
	}
	switch v := n.Meta["complexity"].(type) {
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	case float64:
		if v > 0 {
			return int(v)
		}
	}
	return 1
}

// splitIDSet parses a symbol-ID list into a set, accepting every encoding the
// public surface produces (see splitSymbolIDField). Unlike parseCSVSet it
// preserves case — symbol IDs are case-sensitive.
func splitIDSet(in any) map[string]struct{} {
	out := map[string]struct{}{}
	for _, id := range splitSymbolIDField(in) {
		out[id] = struct{}{}
	}
	return out
}
