package mcp

import (
	"context"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// bottleneckRow is one function/method ranked by computation-bottleneck
// risk. It surfaces the per-function metrics stamped at index time plus
// the interprocedural signals (transitive loop depth across the call
// graph, recursion) computed here.
type bottleneckRow struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	File                string   `json:"file"`
	Line                int      `json:"line"`
	Cyclomatic          int      `json:"cyclomatic,omitempty"`
	Cognitive           int      `json:"cognitive,omitempty"`
	LoopDepth           int      `json:"loop_depth,omitempty"`
	TransitiveLoopDepth int      `json:"transitive_loop_depth,omitempty"`
	Recursive           bool     `json:"recursive,omitempty"`
	MaxAccessDepth      int      `json:"max_access_depth,omitempty"`
	LinearScanInLoop    bool     `json:"linear_scan_in_loop,omitempty"`
	AllocInLoop         bool     `json:"alloc_in_loop,omitempty"`
	RecursionInLoop     bool     `json:"recursion_in_loop,omitempty"`
	Score               int      `json:"score"`
	Reasons             []string `json:"reasons"`
}

// metricInt reads an integer metric stamped on a node's Meta, tolerating
// the int / int64 / float64 shapes the gob and JSON round-trips produce.
func metricInt(n *graph.Node, key string) int {
	if n == nil || n.Meta == nil {
		return 0
	}
	switch v := n.Meta[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// metricBool reads a boolean signal stamped on a node's Meta, tolerating
// the bool / string shapes the gob, JSON, and flat-binary round-trips
// produce.
func metricBool(n *graph.Node, key string) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	switch v := n.Meta[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

// handleAnalyzeBottlenecks (NEW-CBM-1) ranks functions by computation-
// bottleneck risk. It combines the index-time per-function metrics
// (cyclomatic + cognitive complexity, max loop depth) with two
// interprocedural signals derived from the call graph here:
//
//   - transitive_loop_depth: the deepest chain of nested loops across
//     calls — a function that loops and calls another function that
//     loops is a hidden-O(n^2) candidate even when neither alone is.
//   - recursive: the function participates in a call cycle (direct
//     self-recursion or a short mutual cycle); recursion with no
//     branching base case is flagged as unguarded.
//
// It also surfaces four index-time loop-region signals stamped on the
// function node (decided by structural loop-ancestor membership, not line
// range): linear_scan_in_loop (a linear-scan call inside a loop —
// accidental O(n^2)), recursion_in_loop (self-call inside a loop),
// alloc_in_loop (allocation inside a loop — churn / GC pressure), and
// max_access_depth (deepest member-access chain — pointer-chasing). Each
// contributes a reason and weight to the risk score.
//
// Args: limit (default 50), path_prefix, kinds (default function,method),
// min_score.
func (s *Server) handleAnalyzeBottlenecks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))
	limit := intArg(args, "limit", 50)
	minScore := intArg(args, "min_score", 1)

	allowed := map[graph.NodeKind]struct{}{graph.KindFunction: {}, graph.KindMethod: {}}
	if k := strings.TrimSpace(stringArg(args, "kinds")); k != "" {
		allowed = parseAnalyzeKindsFilter(k)
	}

	// Gather candidate functions and their stamped metrics.
	type fnMetrics struct {
		node         *graph.Node
		cyc, cog     int
		loop         int
		accessDepth  int
		linearInLoop bool
		allocInLoop  bool
		recurInLoop  bool
	}
	metrics := map[string]*fnMetrics{}
	for _, n := range s.scopedNodes(ctx) {
		if n == nil {
			continue
		}
		if _, ok := allowed[n.Kind]; !ok {
			continue
		}
		if !graphpath.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		metrics[n.ID] = &fnMetrics{
			node:         n,
			cyc:          metricInt(n, "complexity"),
			cog:          metricInt(n, "cognitive"),
			loop:         metricInt(n, "loop_depth"),
			accessDepth:  metricInt(n, "max_access_depth"),
			linearInLoop: metricBool(n, "linear_scan_in_loop"),
			allocInLoop:  metricBool(n, "alloc_in_loop"),
			recurInLoop:  metricBool(n, "recursion_in_loop"),
		}
	}

	// Call adjacency restricted to resolved function/method targets in
	// the candidate set, so interprocedural walks stay bounded.
	callees := map[string]map[string]struct{}{}
	for e := range s.readerFor(ctx).EdgesByKind(graph.EdgeCalls) {
		if e == nil {
			continue
		}
		if _, ok := metrics[e.From]; !ok {
			continue
		}
		if _, ok := metrics[e.To]; !ok {
			continue
		}
		if callees[e.From] == nil {
			callees[e.From] = map[string]struct{}{}
		}
		callees[e.From][e.To] = struct{}{}
	}

	// transitive loop depth: tld(F) = loop(F) + max over callees G of
	// tld(G). A non-looping intermediate still threads a deeper callee's
	// loop depth up to its caller, since tld(G) already carries it.
	// Memoised, cycle-guarded.
	tldMemo := map[string]int{}
	var tld func(id string, onPath map[string]bool) int
	tld = func(id string, onPath map[string]bool) int {
		if v, ok := tldMemo[id]; ok {
			return v
		}
		if onPath[id] {
			return metrics[id].loop // break the cycle at this node's own depth
		}
		onPath[id] = true
		best := 0
		for callee := range callees[id] {
			if d := tld(callee, onPath); d > best {
				best = d
			}
		}
		delete(onPath, id)
		v := metrics[id].loop + best
		tldMemo[id] = v
		return v
	}

	// recursion: direct self-call, or a short cycle back to F.
	isRecursive := func(id string) bool {
		if _, self := callees[id][id]; self {
			return true
		}
		for g := range callees[id] {
			if _, back := callees[g][id]; back { // F -> G -> F
				return true
			}
		}
		return false
	}

	rows := make([]bottleneckRow, 0, len(metrics))
	for id, m := range metrics {
		transitive := tld(id, map[string]bool{})
		recursive := isRecursive(id)

		var reasons []string
		score := 0
		if m.loop >= 2 {
			reasons = append(reasons, "nested loops within the function (depth "+itoa(m.loop)+") — O(n^"+itoa(m.loop)+")")
			score += m.loop * 4
		} else if m.loop == 1 {
			score += 1
		}
		if transitive > m.loop && transitive >= 2 {
			reasons = append(reasons, "deep loop nesting across calls (transitive depth "+itoa(transitive)+") — hidden-O(n^"+itoa(transitive)+")")
			score += (transitive - m.loop) * 5
		}
		if m.cog >= 15 {
			reasons = append(reasons, "high cognitive complexity ("+itoa(m.cog)+")")
			score += m.cog
		} else {
			score += m.cog / 3
		}
		if m.cyc >= 10 {
			reasons = append(reasons, "high cyclomatic complexity ("+itoa(m.cyc)+")")
			score += m.cyc / 2
		}
		if recursive {
			if m.cyc <= 1 {
				reasons = append(reasons, "unguarded recursion (recursive with no branching base case)")
				score += 8
			} else {
				reasons = append(reasons, "recursive")
				score += 3
			}
		}
		if m.linearInLoop {
			reasons = append(reasons, "linear-scan call inside a loop — accidental O(n^2)")
			score += 6
		}
		if m.recurInLoop {
			reasons = append(reasons, "self-recursion inside a loop — compounding blow-up")
			score += 7
		}
		if m.allocInLoop {
			reasons = append(reasons, "allocation inside a loop — per-iteration churn / GC pressure")
			score += 3
		}
		if m.accessDepth >= 4 {
			reasons = append(reasons, "deep member-access chain (depth "+itoa(m.accessDepth)+") — pointer-chasing / Law of Demeter")
			score += m.accessDepth
		}
		if score < minScore || len(reasons) == 0 {
			continue
		}
		rows = append(rows, bottleneckRow{
			ID: id, Name: m.node.Name, File: m.node.FilePath, Line: m.node.StartLine,
			Cyclomatic: m.cyc, Cognitive: m.cog, LoopDepth: m.loop,
			TransitiveLoopDepth: transitive, Recursive: recursive,
			MaxAccessDepth:   m.accessDepth,
			LinearScanInLoop: m.linearInLoop,
			AllocInLoop:      m.allocInLoop,
			RecursionInLoop:  m.recurInLoop,
			Score:            score, Reasons: reasons,
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		return rows[i].ID < rows[j].ID
	})
	total := len(rows)
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"kind":      "bottlenecks",
		"total":     total,
		"returned":  len(rows),
		"functions": rows,
	})
}
