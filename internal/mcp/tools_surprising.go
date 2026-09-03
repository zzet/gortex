package mcp

import (
	"context"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// registerSurprisingConnectionsTool wires get_surprising_connections —
// the ranked anomaly surface. Improves on the read-only
// `gortex://surprises` resource by scoring every edge against five
// signals and returning the top-N as a structured tool response.
func (s *Server) registerSurprisingConnectionsTool() {
	s.addTool(
		mcp.NewTool("get_surprising_connections",
			mcp.WithDescription("Rank edges in the graph by how anomalous they are. Composite score from five signals: cross-community (+0.30), cross-language (+0.20), peripheral-to-hub (+0.20), cross-test boundary (+0.15), unusual edge kind (+0.15). Returns the top-N with per-signal breakdown so the caller can see why each edge surfaced. Use as an audit aid — anomalies often flag layering violations, accidental coupling, or test code reaching into production paths."),
			mcp.WithNumber("limit", mcp.Description("Cap the result set (default: 25).")),
			mcp.WithNumber("min_score", mcp.Description("Drop edges below this composite score (default: 0.3 — at least one signal fires).")),
			mcp.WithNumber("hub_threshold", mcp.Description("In-edge count above which a node counts as a hub for the peripheral-to-hub signal (default: 5).")),
			mcp.WithNumber("rare_kind_pct", mcp.Description("Edge kinds whose share of all edges is at or below this percentage are 'unusual' (default: 5 — i.e. 5%%).")),
			mcp.WithString("path_prefix", mcp.Description("Scope to edges where at least one endpoint lies under this file-path prefix.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleGetSurprisingConnections,
	)
}

// surprisingEdgeRow is the wire shape: rich enough that an agent can
// decide whether the anomaly is real or expected without an extra
// get_symbol_source round-trip.
type surprisingEdgeRow struct {
	From     string             `json:"from"`
	FromName string             `json:"from_name,omitempty"`
	FromFile string             `json:"from_file,omitempty"`
	To       string             `json:"to"`
	ToName   string             `json:"to_name,omitempty"`
	ToFile   string             `json:"to_file,omitempty"`
	Kind     string             `json:"kind"`
	Score    float64            `json:"score"`
	Signals  map[string]float64 `json:"signals"`
	Reasons  []string           `json:"reasons"`
}

func (s *Server) handleGetSurprisingConnections(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	limit := max(req.GetInt("limit", 25), 1)
	minScore := req.GetFloat("min_score", 0.3)
	hubThreshold := max(req.GetInt("hub_threshold", 5), 1)
	rareKindPct := req.GetFloat("rare_kind_pct", 5.0)
	if rareKindPct < 0 {
		rareKindPct = 0
	}
	pathPrefix := strings.TrimSpace(req.GetString("path_prefix", ""))

	// Build a fast scoped-node index. We still need ALL kinds here —
	// edges in the surprise tally can land on any node, not just
	// function/method. Use scopedNodes' single bulk pull rather than
	// the per-edge GetNode lookups the legacy path fell back to.
	scopedSet := make(map[string]*graph.Node, 1024)
	for _, n := range s.scopedNodesLight(ctx) {
		scopedSet[n.ID] = n
	}

	rows := s.collectSurprisingEdges(ctx, scopedSet, pathPrefix, minScore, hubThreshold, rareKindPct)

	truncated := false
	if len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"connections": rows,
		"total":       len(rows),
		"truncated":   truncated,
		"thresholds": map[string]any{
			"limit":         limit,
			"min_score":     minScore,
			"hub_threshold": hubThreshold,
			"rare_kind_pct": rareKindPct,
		},
		"signal_weights": surprisingSignalWeights(),
	})
}

// collectSurprisingEdges runs the anomaly-mining pass over the scoped
// node set and returns the scored rows, sorted highest-score-first.
// It is the shared core behind both the get_surprising_connections
// tool and the surprising category of suggested_review_questions: the
// caller passes the scoped node index (a single bulk pull it already
// holds) plus the tuning thresholds, and gets back every edge whose
// composite score clears minScore. The caller owns the limit/truncate
// step — this returns the full sorted set so a downstream consumer can
// pick its own cap.
func (s *Server) collectSurprisingEdges(
	ctx context.Context,
	scopedSet map[string]*graph.Node,
	pathPrefix string,
	minScore float64,
	hubThreshold int,
	rareKindPct float64,
) []surprisingEdgeRow {
	// Communities resolve cross-community; missing community result
	// just disables that signal rather than failing the call.
	var nodeToComm map[string]string
	if cr := s.getCommunities(); cr != nil {
		nodeToComm = cr.NodeToComm
	}

	// Kind tally — short-circuit the AllEdges scan when the backend
	// implements EdgeKindCounter (returns one row per distinct kind,
	// not one per edge — a few-dozen-row response replaces a ~286k
	// edge round-trip on a disk backend). The total edge count then comes
	// from the per-kind sum so we don't need a second backend call.
	reader := s.readerFor(ctx)
	kindCounts := make(map[graph.EdgeKind]int, 16)
	totalEdges := 0
	var allEdges []*graph.Edge
	if counter, ok := reader.(graph.EdgeKindCounter); ok {
		for k, c := range counter.EdgeKindCounts() {
			kindCounts[k] = c
			totalEdges += c
		}
	} else {
		allEdges = reader.AllEdges()
		for _, e := range allEdges {
			kindCounts[e.Kind]++
		}
		totalEdges = len(allEdges)
	}

	// In-degree still walks edges Go-side — the per-edge anomaly walk
	// further down already pulls the full edge stream, so bucketing
	// fan-in during that traversal is free. The InDegreeForNodes
	// capability runs one COUNT { … } per id; on the gortex workspace
	// the scoped set is ~30k function/method nodes, and tens of
	// thousands of indexed subqueries are noticeably slower than the
	// single AllEdges materialisation the anomaly walk already pays.
	if allEdges == nil {
		allEdges = reader.AllEdges()
	}
	inDegree := make(map[string]int, len(scopedSet))
	for _, e := range allEdges {
		if _, ok := scopedSet[e.To]; ok {
			inDegree[e.To]++
		}
	}

	// Determine which edge kinds are "unusual" — share of total
	// edges is at or below rare_kind_pct. Recomputed once per call.
	rareKinds := make(map[graph.EdgeKind]bool, len(kindCounts))
	if totalEdges > 0 {
		thresholdFrac := rareKindPct / 100.0
		for k, c := range kindCounts {
			if float64(c)/float64(totalEdges) <= thresholdFrac {
				rareKinds[k] = true
			}
		}
	}

	rows := make([]surprisingEdgeRow, 0, 256)
	for _, e := range allEdges {
		from, fromOK := scopedSet[e.From]
		to, toOK := scopedSet[e.To]
		// Either endpoint outside scope → skip (avoids surfacing
		// edges into unrelated repos in multi-repo mode).
		if !fromOK || !toOK {
			continue
		}
		if pathPrefix != "" &&
			!graphpath.HasPrefix(from.FilePath, pathPrefix) &&
			!graphpath.HasPrefix(to.FilePath, pathPrefix) {
			continue
		}

		signals, reasons := scoreSurprisingEdge(from, to, e, nodeToComm, inDegree, hubThreshold, rareKinds)
		score := 0.0
		for _, v := range signals {
			score += v
		}
		if score < minScore {
			continue
		}
		rows = append(rows, surprisingEdgeRow{
			From: e.From, FromName: from.Name, FromFile: from.FilePath,
			To: e.To, ToName: to.Name, ToFile: to.FilePath,
			Kind:    string(e.Kind),
			Score:   roundScore(score),
			Signals: signals,
			Reasons: reasons,
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		// Stable secondary order so identical scores are deterministic.
		if rows[i].From != rows[j].From {
			return rows[i].From < rows[j].From
		}
		return rows[i].To < rows[j].To
	})

	return rows
}

// scoreSurprisingEdge fires the five composite signals and returns
// only the ones that contributed, so the caller can render a clean
// reason list. Threshold checks deliberately match the row weights
// in the spec — easy to re-tune from one place.
func scoreSurprisingEdge(
	from, to *graph.Node,
	e *graph.Edge,
	nodeToComm map[string]string,
	inDegree map[string]int,
	hubThreshold int,
	rareKinds map[graph.EdgeKind]bool,
) (map[string]float64, []string) {
	weights := surprisingSignalWeights()
	signals := map[string]float64{}
	reasons := []string{}

	if nodeToComm != nil {
		fc, fok := nodeToComm[from.ID]
		tc, tok := nodeToComm[to.ID]
		if fok && tok && fc != "" && tc != "" && fc != tc {
			signals["cross_community"] = weights["cross_community"]
			reasons = append(reasons, "cross_community("+fc+"→"+tc+")")
		}
	}

	if from.Language != "" && to.Language != "" && from.Language != to.Language {
		signals["cross_language"] = weights["cross_language"]
		reasons = append(reasons, "cross_language("+from.Language+"→"+to.Language+")")
	}

	if inDegree[from.ID] <= 2 && inDegree[to.ID] >= hubThreshold {
		signals["peripheral_to_hub"] = weights["peripheral_to_hub"]
		reasons = append(reasons, "peripheral_to_hub(in:"+itoaPair(inDegree[from.ID], inDegree[to.ID])+")")
	}

	if isTestPath(from.FilePath) != isTestPath(to.FilePath) {
		signals["cross_test"] = weights["cross_test"]
		reasons = append(reasons, "cross_test")
	}

	if rareKinds[e.Kind] {
		signals["unusual_kind"] = weights["unusual_kind"]
		reasons = append(reasons, "unusual_kind("+string(e.Kind)+")")
	}

	return signals, reasons
}

// surprisingSignalWeights is the single source of truth for the
// per-signal contribution. The spec calls for the exact values in
// the gap-analysis row: +0.30/+0.20/+0.20/+0.15/+0.15.
func surprisingSignalWeights() map[string]float64 {
	return map[string]float64{
		"cross_community":   0.30,
		"cross_language":    0.20,
		"peripheral_to_hub": 0.20,
		"cross_test":        0.15,
		"unusual_kind":      0.15,
	}
}

// isTestPath mirrors the helper used by the impact analyzer — kept
// inline so this file doesn't depend on internal/analysis just for
// one predicate.
func isTestPath(p string) bool {
	for _, s := range []string{"_test.go", ".test.ts", ".test.js", ".spec.ts", ".spec.js", "__tests__/", "test_"} {
		if strings.Contains(p, s) {
			return true
		}
	}
	return false
}

// roundScore trims float jitter so identical edges hash the same in
// downstream consumers without lying about the actual sum.
func roundScore(v float64) float64 {
	return float64(int(v*1000+0.5)) / 1000.0
}

// itoaPair renders the two-int hint "1,7" without pulling in fmt.
// Kept tiny and inline because it's called per-edge.
func itoaPair(a, b int) string {
	return itoa(a) + "," + itoa(b)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
