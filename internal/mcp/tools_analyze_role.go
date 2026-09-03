package mcp

import (
	"context"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// handleAnalyzeRole classifies every function/method node in scope
// with exactly one role tag — the deterministic answer to "what
// shape is this symbol playing in the codebase?"
//
// Six roles:
//
//	dead     zero in-edges  AND zero out-edges. Almost always unused.
//	entry    zero in-edges  AND ≥1  out-edges. Top-of-stack functions
//	         (main, http handlers, cron entry points, test funcs).
//	leaf     ≥1   in-edges  AND zero out-edges. Pure terminals —
//	         helpers, formatters, getters, accessors.
//	adapter  in-edges from one community AND out-edges into a
//	         different community. Boundary-crossers between modules.
//	utility  high fan-in / low fan-out / small file. Widely-used
//	         helpers (≥3 callers, ≤2 callees, ≤30 lines).
//	core    everything else — the load-bearing middle of the graph.
//
// The classifier is heuristic; rule precedence is the order above
// so a tie surfaces the more informative label (entry beats utility,
// utility beats core).
func (s *Server) handleAnalyzeRole(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	pathPrefix := strings.TrimSpace(req.GetString("path_prefix", ""))
	roleFilter := strings.TrimSpace(req.GetString("role", ""))
	limit := max(req.GetInt("limit", 200), 1)

	scoped := s.scopedNodesLight(ctx)
	var nodeToComm map[string]string
	if cr := s.getCommunities(); cr != nil {
		nodeToComm = cr.NodeToComm
	}

	type roleRow struct {
		ID        string `json:"symbol_id"`
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		File      string `json:"file"`
		StartLine int    `json:"start_line"`
		Role      string `json:"role"`
		FanIn     int    `json:"fan_in"`
		FanOut    int    `json:"fan_out"`
	}
	rows := make([]roleRow, 0, len(scoped))
	tally := map[string]int{}

	reader := s.readerFor(ctx)
	for _, n := range scoped {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if !graphpath.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		fanIn := countCallEdges(reader.GetInEdges(n.ID))
		fanOut := countCallEdges(reader.GetOutEdges(n.ID))
		role := classifyRole(n, fanIn, fanOut, reader, nodeToComm)
		tally[role]++
		if roleFilter != "" && role != roleFilter {
			continue
		}
		rows = append(rows, roleRow{
			ID: n.ID, Name: n.Name, Kind: string(n.Kind), File: n.FilePath,
			StartLine: n.StartLine, Role: role,
			FanIn: fanIn, FanOut: fanOut,
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Role != rows[j].Role {
			return rows[i].Role < rows[j].Role
		}
		if rows[i].FanIn != rows[j].FanIn {
			return rows[i].FanIn > rows[j].FanIn
		}
		return rows[i].ID < rows[j].ID
	})
	truncated := false
	if len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"symbols":       rows,
		"total":         len(rows),
		"truncated":     truncated,
		"tally_by_role": tally,
		"path_prefix":   pathPrefix,
		"role_filter":   roleFilter,
	})
}

// classifyRole walks the six-rule precedence ladder and returns
// the first matching label. Rules are deliberately conservative;
// false-negatives (defaulting to "core") are preferable to noisy
// false-positives on a label that pretends to be authoritative.
func classifyRole(n *graph.Node, fanIn, fanOut int, g graph.Reader, nodeToComm map[string]string) string {
	switch {
	case fanIn == 0 && fanOut == 0:
		return "dead"
	case fanIn == 0 && fanOut > 0:
		return "entry"
	case fanIn > 0 && fanOut == 0:
		return "leaf"
	}
	// Adapter: in-edges originate in a different community than
	// the out-edge targets. Skip when communities aren't available.
	if nodeToComm != nil {
		myComm, ok := nodeToComm[n.ID]
		if ok && myComm != "" {
			incomingFromOther := false
			outgoingToOther := false
			for _, e := range g.GetInEdges(n.ID) {
				if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeCrossRepoCalls {
					continue
				}
				if other, ok2 := nodeToComm[e.From]; ok2 && other != myComm {
					incomingFromOther = true
					break
				}
			}
			for _, e := range g.GetOutEdges(n.ID) {
				if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeCrossRepoCalls {
					continue
				}
				if other, ok2 := nodeToComm[e.To]; ok2 && other != myComm {
					outgoingToOther = true
					break
				}
			}
			if incomingFromOther && outgoingToOther {
				return "adapter"
			}
		}
	}
	// Utility: widely-used helper. The size threshold is heuristic;
	// 30 lines is the common "is this a one-screen function" rule.
	lineCount := 0
	if n.EndLine > 0 {
		lineCount = n.EndLine - n.StartLine + 1
	}
	if fanIn >= 3 && fanOut <= 2 && lineCount > 0 && lineCount <= 30 {
		return "utility"
	}
	return "core"
}

// countCallEdges returns the number of distinct call-flavoured
// edges in the slice. Filters out structural edges (defines,
// member_of) so the role classifier reads the call graph cleanly.
func countCallEdges(edges []*graph.Edge) int {
	n := 0
	seen := map[string]bool{}
	for _, e := range edges {
		if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeCrossRepoCalls {
			continue
		}
		var key string
		if seen[e.From+">"+e.To] {
			continue
		}
		key = e.From + ">" + e.To
		seen[key] = true
		n++
	}
	return n
}
