package mcp

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

// QRT-4: the forward direction of the gate — when change_contract flags a
// change to a heavy symbol, it doesn't just refuse, it hands back the safe
// path. edit_strategy names a refactoring technique, ranks its complexity
// impact, and lists the graph-derived safety signals that make the refactor
// checkable. Refuse + remedy in one reply.

// paramCount returns the number of parameters declared on a function/method
// node — params point at their owner via EdgeParamOf.
func (s *Server) paramCount(ctx context.Context, id string) int {
	g := s.readerFor(ctx)
	if g == nil {
		return 0
	}
	n := 0
	for _, e := range g.GetInEdges(id) {
		if e.Kind == graph.EdgeParamOf {
			n++
		}
	}
	return n
}

// dominantSymbol picks the function/method in the changed set most worth a
// refactor — the widest line span, tie-broken by fan-out.
func (s *Server) dominantSymbol(nodes []*graph.Node) *graph.Node {
	var best *graph.Node
	bestSpan := -1
	for _, n := range nodes {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		span := n.EndLine - n.StartLine
		if span > bestSpan {
			best = n
			bestSpan = span
		}
	}
	return best
}

func ccImpactTier(lineCount, fanOut int) string {
	switch {
	case lineCount >= 120 || fanOut >= 18:
		return "High"
	case lineCount >= 50 || fanOut >= 9:
		return "Medium"
	default:
		return "Low"
	}
}

// buildEditStrategy derives a named refactoring technique for the dominant
// changed symbol, or nil when the change is small enough not to warrant one.
func (s *Server) buildEditStrategy(ctx context.Context, p *prediction) *editStrategy {
	node := s.dominantSymbol(p.nodes)
	if node == nil {
		return nil
	}
	lineCount := node.EndLine - node.StartLine + 1
	params := s.paramCount(ctx, node.ID)
	fanIn, fanOut := computeFanInOut(s.readerFor(ctx), []*graph.Node{node})
	callers := fanIn[node.ID]
	callees := fanOut[node.ID]

	var technique string
	var steps []string
	switch {
	case params >= 5:
		technique = "Introduce Parameter Object"
		steps = []string{
			fmt.Sprintf("Group the %d parameters of %s into a single options/request struct.", params, node.Name),
			"Add the struct type next to the function and migrate the body to read its fields.",
			fmt.Sprintf("Use change_contract source=edit with the call-site rewrite to confirm no caller of %s breaks before applying.", node.Name),
		}
	case lineCount >= 60 || callees >= 10:
		technique = "Extract Method"
		steps = []string{
			fmt.Sprintf("Identify the %d-line body of %s and pull cohesive blocks into named helpers.", lineCount, node.Name),
			"Keep the public signature stable so callers are unaffected.",
			"Re-run change_contract after each extraction to confirm the blast radius stays bounded.",
		}
	case callers >= 10 && lineCount >= 30:
		technique = "Extract Method"
		steps = []string{
			fmt.Sprintf("%s is %d lines and called from %d sites — extract the stable core into a helper to localise future edits.", node.Name, lineCount, callers),
			"Keep the original as a thin wrapper so existing callers are untouched.",
		}
	default:
		// Small, low-fan-out symbol: an in-place edit is fine, no strategy.
		return nil
	}

	var safety []string
	if p.impact != nil && len(p.impact.TestFiles) > 0 {
		safety = append(safety, fmt.Sprintf("%d covering test file(s) exercise the changed set", len(p.impact.TestFiles)))
	} else {
		safety = append(safety, "no covering tests found — add one before refactoring")
	}
	safety = append(safety, fmt.Sprintf("%d caller(s) are graph-tracked; use rename_symbol / change_contract source=edit to verify them", callers))
	if p.step != nil && len(p.step.brokenCallers) == 0 {
		safety = append(safety, "current simulation shows no broken callers")
	}

	return &editStrategy{
		Technique: technique,
		Steps:     steps,
		CCImpact:  ccImpactTier(lineCount, callees),
		Safety:    safety,
	}
}
