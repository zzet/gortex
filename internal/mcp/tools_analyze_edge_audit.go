// edge_audit is a self-diagnostic analyzer: rather than answering a
// question about the code, it reports where the *graph itself* is
// likely incomplete. Missing edge types are the root cause of
// dead-code false positives — a symbol looks unreachable only because
// the edge that reaches it was never extracted — so this view grades
// call-graph resolution confidence and surfaces the symbols most at
// risk of a wrong "dead" verdict.
package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

// edgeTierLabel buckets an edge Origin into a coarse confidence tier.
func edgeTierLabel(origin string) string {
	switch origin {
	case graph.OriginLSPResolved, graph.OriginLSPDispatch:
		return "lsp"
	case graph.OriginASTResolved:
		return "ast_resolved"
	case graph.OriginASTInferred:
		return "ast_inferred"
	case graph.OriginTextMatched:
		return "text_matched"
	default:
		return "unknown"
	}
}

// handleAnalyzeEdgeAudit grades graph completeness. It reports the
// distribution of edges (and call edges specifically) across
// resolution-confidence tiers, plus the symbols most likely to be
// dead-code false positives: interfaces with no implementor, targets
// reached only from test code, and call edges resolved by the weakest
// text-matching tier.
func (s *Server) handleAnalyzeEdgeAudit(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	sample := 10
	if v, ok := args["limit"].(float64); ok && v > 0 {
		sample = int(v)
	}

	edgeTiers := map[string]int{}
	callTiers := map[string]int{}
	inCalls := map[string][]string{} // target → caller IDs
	implemented := map[string]bool{} // interface ID → has an implementor
	visibleRepos := map[string]struct{}{}
	var weakCalls []string // text-matched "from -> to"

	// When the request narrows scope (workspace-bound session or repo
	// allow-set), drop edges/nodes outside it so every count map and
	// diagnostic array recomputes from the in-scope subgraph. Unbound
	// requests skip the gate entirely — a byte-for-byte no-op.
	scoped := s.scopeFiltersActive(ctx)

	// Both scans and every endpoint lookup share one reader, so the
	// tiers grade the state this request reads — the caller's buffers
	// when a view is active.
	reader := s.readerFor(ctx)
	for _, e := range reader.AllEdges() {
		if scoped && (!s.analyzeNodeVisible(ctx, reader.GetNode(e.From)) || !s.analyzeNodeVisible(ctx, reader.GetNode(e.To))) {
			continue
		}
		tier := edgeTierLabel(e.Origin)
		edgeTiers[tier]++
		switch e.Kind {
		case graph.EdgeCalls:
			callTiers[tier]++
			inCalls[e.To] = append(inCalls[e.To], e.From)
			if e.Origin == graph.OriginTextMatched {
				weakCalls = append(weakCalls, e.From+" -> "+e.To)
			}
		case graph.EdgeImplements:
			implemented[e.To] = true
		}
	}

	// Interfaces with no implementor.
	var unimplemented []string
	// Targets reached only from test symbols — dead-code false
	// positives once test callers are policy-excluded.
	var testOnly []string
	for _, n := range reader.AllNodes() {
		if scoped && !s.analyzeNodeVisible(ctx, n) {
			continue
		}
		if scoped && n.RepoPrefix != "" {
			visibleRepos[n.RepoPrefix] = struct{}{}
		}
		switch n.Kind {
		case graph.KindInterface:
			if !implemented[n.ID] {
				unimplemented = append(unimplemented, n.ID)
			}
		case graph.KindFunction, graph.KindMethod:
			callers := inCalls[n.ID]
			if len(callers) == 0 {
				continue // uncalled, but not "test-only" — out of scope here
			}
			allTest := true
			for _, c := range callers {
				cn := reader.GetNode(c)
				if cn == nil || !auditIsTestNode(cn) {
					allTest = false
					break
				}
			}
			if allTest {
				testOnly = append(testOnly, n.ID)
			}
		}
	}
	sort.Strings(unimplemented)
	sort.Strings(testOnly)
	sort.Strings(weakCalls)

	totalEdges := 0
	for _, c := range edgeTiers {
		totalEdges += c
	}
	totalCalls := 0
	for _, c := range callTiers {
		totalCalls += c
	}
	highConf := callTiers["lsp"] + callTiers["ast_resolved"]
	highPct := 0.0
	if totalCalls > 0 {
		highPct = float64(highConf) * 100 / float64(totalCalls)
	}

	var scopedRepos []string
	if scoped {
		scopedRepos = make([]string, 0, len(visibleRepos))
		for repo := range visibleRepos {
			scopedRepos = append(scopedRepos, repo)
		}
		sort.Strings(scopedRepos)
	}
	integrity, err := s.edgeAuditGraphIntegrity(ctx, sample, scoped, scopedRepos)
	if err != nil {
		return nil, err
	}

	payload := map[string]any{
		"edge_tiers": edgeTiers,
		"call_tiers": callTiers,
		"summary": map[string]any{
			"total_edges":              totalEdges,
			"total_call_edges":         totalCalls,
			"high_confidence_call_pct": round1(highPct),
		},
		"unimplemented_interfaces": auditBucket(unimplemented, sample),
		"test_only_targets":        auditBucket(testOnly, sample),
		"weak_call_edges":          auditBucket(weakCalls, sample),
		"graph_integrity":          integrity,
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeAnalyze("edge_audit", []map[string]any{payload}))
	}
	if isCompact(req) {
		var b strings.Builder
		fmt.Fprintf(&b, "edges=%d calls=%d high_conf=%.1f%%\n", totalEdges, totalCalls, highPct)
		fmt.Fprintf(&b, "unimplemented_interfaces=%d test_only_targets=%d weak_call_edges=%d\n",
			len(unimplemented), len(testOnly), len(weakCalls))
		fmt.Fprintf(&b, "graph_integrity since_open_status=%s writes=%d reads=%d persisted_status=%s persisted_rows=%d\n",
			integrity.SinceOpen.Status, integrity.SinceOpen.Totals.WriteRejected,
			integrity.SinceOpen.Totals.ReadSuppressed, integrity.Persisted.Status, integrity.Persisted.TotalRows)
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// auditBucket packs a count plus a capped, ordered sample.
func auditBucket(ids []string, sample int) map[string]any {
	out := map[string]any{"count": len(ids)}
	if len(ids) > sample {
		out["sample"] = ids[:sample]
		out["truncated"] = true
	} else if len(ids) > 0 {
		out["sample"] = ids
	}
	return out
}

// auditIsTestNode reports whether n is test code — by the Meta flags
// the test-edge pass stamps, or (via isTestNode) by its file path.
func auditIsTestNode(n *graph.Node) bool {
	if n == nil {
		return false
	}
	if n.Meta != nil {
		if v, _ := n.Meta["is_test"].(bool); v {
			return true
		}
		if v, _ := n.Meta["is_test_file"].(bool); v {
			return true
		}
	}
	return isTestNode(n)
}

// round1 rounds to one decimal place.
func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}
