package mcp

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// Proactive deferred-tool surfacing: the measured failure is that agents
// consult tools_search once and never again, so a deferred tool is
// effectively invisible. When a response's CONTENT matches a deferred
// tool's trigger, we append one compact, additive `related_tools` cue —
// once per tool per session, at most one per response — in the same style
// as the existing completeness cues (SuppressionCaveat). Pure rules on
// response shape; no LLM calls, no other response content changes.

// dispatchImplementorCount counts the concrete implementors / overriders of
// a symbol from the resolved graph — the signal that a plain find_usages on
// this symbol is under-selling the picture because dispatch fans out to
// implementations the reference list doesn't name.
func (s *Server) dispatchImplementorCount(ctx context.Context, id string) int {
	if s == nil || id == "" {
		return 0
	}
	reader := s.readerFor(ctx)
	if reader == nil {
		return 0
	}
	n := 0
	for _, e := range reader.GetInEdges(id) {
		if e == nil {
			continue
		}
		if e.Kind == graph.EdgeImplements || e.Kind == graph.EdgeOverrides {
			n++
		}
	}
	return n
}

// attachRelatedToolsCue attaches a related_tools cue to a find_usages
// subgraph when the target is dispatch-heavy (an interface / base method
// with concrete implementors), pointing at find_implementations — the tool
// that names those implementors a reference list cannot. Once per session.
func (s *Server) attachRelatedToolsCue(ctx context.Context, sg *query.SubGraph, id string) {
	if sg == nil || sg.RelatedTools != "" {
		return
	}
	// Don't compete with the zero-edge / tier-filtered caveats — those own
	// the "why is this empty" story.
	if sg.Caveat != nil || sg.TierFiltered != nil {
		return
	}
	const minImplementors = 2
	n := s.dispatchImplementorCount(ctx, id)
	if n < minImplementors {
		return
	}
	sess := s.sessionFor(ctx)
	if sess == nil || !sess.markCueOnce("find_implementations") {
		return
	}
	sg.RelatedTools = fmt.Sprintf(
		"target has %d implementations/overrides; find_implementations lists the concrete ones dispatch reaches",
		n)
}
