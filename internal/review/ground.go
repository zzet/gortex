// Package review holds the graph-grounding post-pass that refines the
// pure-AST review rulepack (internal/astquery, Category "review").
//
// The astquery detectors are deliberately graph-agnostic: a detector's
// PostFilter only ever sees (parser.QueryResult, []byte), so the
// undecidable-from-AST-alone rules — N+1 query-in-loop and
// check-then-act on a shared map/dict — are emitted optimistically and
// then confirmed or refuted here, where a graph reader is reachable.
//
// Grounding takes the match row (file + line + enclosing symbol) and
// consults graph metadata / resolved edges. A match that the graph
// proves is benign (e.g. a "loop query" whose enclosing function
// provably contains no loop) is dropped; everything else — including
// every decidable rule, which needs no grounding — survives.
package review

import (
	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/graph"
)

// Detector names whose findings are undecidable from the AST alone and
// therefore subject to the graph-grounding refinement. Every other
// review detector is decidable and passes through untouched.
const (
	detectorLoopQueryGo    = "go-loop-query-call"
	detectorLoopQueryPy    = "py-loop-query-call"
	detectorCheckActMapGo  = "go-check-then-act-map"
	detectorCheckActDictPy = "py-check-then-act-dict"
)

// mutatingEdgeKinds are the resolved out-edges that prove the enclosing
// symbol of a check-then-act match actually performs the "act" half —
// i.e. it writes state. Absent any of these the flagged read/check is
// not paired with a real mutation in the resolved graph and the match
// is refuted.
var mutatingEdgeKinds = map[graph.EdgeKind]struct{}{
	graph.EdgeWrites:        {},
	graph.EdgeWritesCol:     {},
	graph.EdgeWritesConfig:  {},
	graph.EdgeAccessesField: {},
}

// GroundReviewMatches runs the graph-grounding post-pass over a set of
// review matches and returns the subset that survives. Decidable rules
// (every detector not in the undecidable set) pass through unchanged;
// the N+1 and check-then-act rows are kept only when the graph confirms
// them. A nil store grounds nothing — every row is returned as-is, so
// the feature degrades to pure-AST behaviour rather than dropping
// findings on a missing graph.
func GroundReviewMatches(g graph.Reader, matches []astquery.Match) []astquery.Match {
	if len(matches) == 0 {
		return matches
	}
	out := matches[:0:0]
	for _, m := range matches {
		if keepReviewMatch(g, m) {
			out = append(out, m)
		}
	}
	return out
}

func keepReviewMatch(g graph.Reader, m astquery.Match) bool {
	switch m.Detector {
	case detectorLoopQueryGo, detectorLoopQueryPy:
		return GroundLoopCall(g, m)
	case detectorCheckActMapGo, detectorCheckActDictPy:
		return GroundCheckThenAct(g, m)
	default:
		// Decidable rule — no grounding needed.
		return true
	}
}

// GroundLoopCall returns true when an N+1 match should be kept: the
// enclosing symbol of the flagged call provably contains at least one
// loop body (loop_depth >= 1, stamped at index time). When the graph
// shows the enclosing function has no loop at all (loop_depth absent or
// zero) the query call cannot be inside a loop-over-collection, so the
// match is graph-refuted and dropped.
//
// A nil store or an unresolvable symbol leaves the AST verdict intact
// (keep) — grounding only ever removes a row it can affirmatively
// refute.
func GroundLoopCall(g graph.Reader, m astquery.Match) bool {
	if g == nil || m.SymbolID == "" {
		return true
	}
	n := g.GetNode(m.SymbolID)
	if n == nil {
		return true
	}
	return loopDepth(n) >= 1
}

// GroundCheckThenAct returns true when a check-then-act match should be
// kept: the enclosing symbol carries a resolved mutating out-edge, so
// the "act" half (the write that follows the check) is a real mutation
// in the graph — the hallmark of a genuine read-modify-write race. When
// the graph shows the enclosing function performs no mutation at all
// the flagged shape is benign (e.g. the body only reads / logs) and the
// row is refuted.
//
// A nil store or an unresolvable symbol leaves the AST verdict intact
// (keep).
func GroundCheckThenAct(g graph.Reader, m astquery.Match) bool {
	if g == nil || m.SymbolID == "" {
		return true
	}
	for _, e := range g.GetOutEdges(m.SymbolID) {
		if e == nil {
			continue
		}
		if _, ok := mutatingEdgeKinds[e.Kind]; ok {
			return true
		}
	}
	return false
}

// loopDepth reads the index-time loop-nesting metric off a symbol node.
// The metric is stamped only when > 0, so an absent key means zero
// loops. Meta values may be int / int64 / float64 depending on the
// backend's gob/json round-trip, so all three are accepted.
func loopDepth(n *graph.Node) int {
	if n == nil || n.Meta == nil {
		return 0
	}
	switch v := n.Meta["loop_depth"].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}
