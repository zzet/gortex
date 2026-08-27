package query

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// Name-only call sites.
//
// An extractor emits every call it sees, including the ones it cannot
// bind: `Techs.get_tech(...)` leaves the site parked on the bare-name
// placeholder `unresolved::get_tech`. The resolver then either lands the
// edge on a real symbol or — for a weak-tier guess whose target is not
// import-reachable — reverts it to that same placeholder (see
// resolver.guardCrossPackageCallEdges). Reverting is the right call:
// a guessed binding is worse than none.
//
// But a placeholder is not a node, so those edges are invisible to every
// reverse walk. get_callers on a symbol with 23 call sites and one bound
// edge answers "1 caller", and `min_tier:"text_matched"` cannot widen it
// — there is no text_matched edge to admit, only edges pointing
// somewhere else entirely. The confident wrong answer ("risk: LOW")
// then propagates into impact and rename.
//
// This is the honest floor under that answer. The placeholders a symbol's
// name owns are looked up directly, and the call sites parked on them are
// reported as CANDIDATES: sites that name the symbol and bound to
// nothing. They are counted on every response, so an emptiness is legible
// as "1 verified, 22 unverified" rather than as proof of safety, and the
// rows themselves are returned only under an explicit
// `min_tier:"text_matched"` — they are name-matched evidence and must
// never dilute a default answer.

// NameOnlyCandidateCap bounds how many candidate rows one query
// materialises. A common method name (`Get`, `run`, `handle`) can own
// thousands of unbound sites; the count stays exact past the cap so the
// rollup never under-reports what the row list truncated.
const NameOnlyCandidateCap = 200

// NameOnlyCandidates is the result of a name-only lookup: the total
// number of unbound call sites naming the symbol, and up to
// NameOnlyCandidateCap of them projected as edges pointing at the symbol.
type NameOnlyCandidates struct {
	Total int
	Edges []*graph.Edge
	Nodes []*graph.Node
}

// FindNameOnlyCandidates returns the unbound call sites naming node.
//
// bound is the already-resolved edge set for the same query; a site
// present there is a real, verified reference and is not double-counted
// as a candidate. Candidate edges are COPIES retargeted onto node — the
// graph's own edges still point at their placeholder and are never
// mutated — stamped at the text_matched tier with Meta["name_only"] so
// no consumer can mistake one for a resolved binding.
func (e *Engine) FindNameOnlyCandidates(node *graph.Node, bound []*graph.Edge, opts QueryOptions) NameOnlyCandidates {
	var out NameOnlyCandidates
	if e == nil || node == nil {
		return out
	}
	seen := make(map[callSiteKey]struct{}, len(bound))
	for _, b := range bound {
		if b != nil {
			seen[callSiteKeyOf(b)] = struct{}{}
		}
	}
	fromIDs := make([]string, 0, 32)
	var candidates []*graph.Edge
	// One batched reverse lookup for all four placeholder shapes — on a
	// disk backend the per-id form would be four query round-trips on the
	// find_usages / get_callers hot path.
	placeholders := graph.UnresolvedNameCandidateIDs(node)
	byPlaceholder := e.g.GetInEdgesByNodeIDs(placeholders)
	for _, placeholder := range placeholders {
		for _, edge := range byPlaceholder[placeholder] {
			if edge == nil || !isCallLikeUsageKind(edge.Kind) {
				continue
			}
			key := callSiteKeyOf(edge)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			candidates = append(candidates, edge)
			fromIDs = append(fromIDs, edge.From)
		}
	}
	if len(candidates) == 0 {
		return out
	}
	// Deterministic order — GetInEdges across several placeholders has no
	// inherent ordering, and the cap below must not slice an arbitrary
	// subset differently on each call.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].FilePath != candidates[j].FilePath {
			return candidates[i].FilePath < candidates[j].FilePath
		}
		if candidates[i].Line != candidates[j].Line {
			return candidates[i].Line < candidates[j].Line
		}
		return candidates[i].From < candidates[j].From
	})
	out.Total = len(candidates)

	// Scope filtering needs the source node, and the response needs it as
	// a row — one batched lookup covers both.
	fromByID := e.g.GetNodesByIDs(fromIDs)
	nodes := make(map[string]*graph.Node, len(fromIDs))
	for _, edge := range candidates {
		from := fromByID[edge.From]
		if opts.hasScopeFilter() && (from == nil || !opts.ScopeAllows(from)) {
			out.Total--
			continue
		}
		if opts.ExcludeTests && e.isTestSource(from) {
			out.Total--
			continue
		}
		if len(out.Edges) >= NameOnlyCandidateCap {
			continue
		}
		out.Edges = append(out.Edges, projectNameOnlyEdge(edge, node.ID))
		if from != nil {
			nodes[from.ID] = from
		}
	}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, n)
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].ID < out.Nodes[j].ID })
	return out
}

// AttachNameOnlyCandidates records the candidate count on sg and, when
// include is true, merges the candidate rows into the result.
//
// The count always rides the response; the rows do not. A name match is
// the weakest evidence the graph has, and silently mixing it into a
// default answer would trade one wrong confident answer for another.
func (sg *SubGraph) AttachNameOnlyCandidates(c NameOnlyCandidates, include bool) {
	if sg == nil || c.Total == 0 {
		return
	}
	sg.NameOnlyCandidates = c.Total
	if !include {
		return
	}
	have := make(map[string]struct{}, len(sg.Nodes))
	for _, n := range sg.Nodes {
		have[n.ID] = struct{}{}
	}
	for _, n := range c.Nodes {
		if _, dup := have[n.ID]; dup {
			continue
		}
		have[n.ID] = struct{}{}
		sg.Nodes = append(sg.Nodes, n)
	}
	sg.Edges = append(sg.Edges, c.Edges...)
	sg.TotalNodes = len(sg.Nodes)
	sg.TotalEdges = len(sg.Edges)
}

// projectNameOnlyEdge copies edge with its target rewritten to targetID
// and the weakest tier stamped. The copy is essential: the source edge is
// live graph state whose target must keep pointing at the placeholder so
// a later resolver pass can still bind it.
func projectNameOnlyEdge(edge *graph.Edge, targetID string) *graph.Edge {
	cp := *edge
	cp.To = targetID
	cp.Origin = graph.OriginTextMatched
	cp.Confidence = 0
	cp.ConfidenceLabel = ""
	// Meta is json:"-", so the marker also rides a wire field — otherwise a
	// consumer could not tell this projection from a text_matched edge the
	// resolver actually bound.
	cp.NameOnly = true
	meta := make(map[string]any, len(edge.Meta)+1)
	for k, v := range edge.Meta {
		meta[k] = v
	}
	meta["name_only"] = true
	meta["unresolved_target"] = edge.To
	cp.Meta = meta
	return &cp
}

// callSiteKey identifies one call site so a candidate that the resolver
// also bound (or that two placeholder shapes both yield) is counted once.
type callSiteKey struct {
	from string
	file string
	line int
	kind graph.EdgeKind
}

func callSiteKeyOf(e *graph.Edge) callSiteKey {
	return callSiteKey{from: e.From, file: e.FilePath, line: e.Line, kind: e.Kind}
}

// isCallLikeUsageKind narrows the candidate set to the edge kinds a
// bare-name placeholder can legitimately carry — a call or a
// method-value reference. Import / contract / infrastructure kinds park
// on structured targets, never on a bare name.
func isCallLikeUsageKind(k graph.EdgeKind) bool {
	switch k {
	case graph.EdgeCalls, graph.EdgeReferences:
		return true
	}
	return false
}
