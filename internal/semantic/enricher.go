package semantic

import (
	"github.com/zzet/gortex/internal/graph"
)

// ConfirmEdge upgrades an edge's confidence to EXTRACTED and records the
// semantic source. Origin is set to LSP-grade (lsp_dispatch for interface
// implementations, lsp_resolved for everything else) since only compiler /
// type-system providers call ConfirmEdge.
//
// A non-LSP prior origin is preserved under meta confirmed_from_origin
// BEFORE the flip: the LSP dispatch gate's evidence probe tells extractor /
// resolver hierarchy edges from the sweep's own recoveries by origin, and a
// confirm that erased the origin in place would decay that evidence one
// pass at a time — the gate admits hierarchy types, the sweep confirms
// their edges, the probe loses them. Confirmation upgrades the tier; it
// must not erase which lane produced the edge. The marker's presence is the
// signal (an extractor stub's origin is legitimately empty) and it is
// written once — a re-confirmation sees an LSP-grade origin and leaves it.
func ConfirmEdge(e *graph.Edge, provider string) {
	if e.Meta == nil {
		e.Meta = make(map[string]any)
	}
	if e.Origin != graph.OriginLSPResolved && e.Origin != graph.OriginLSPDispatch {
		if _, tagged := e.Meta["confirmed_from_origin"]; !tagged {
			e.Meta["confirmed_from_origin"] = string(e.Origin)
		}
	}
	e.Confidence = 1.0
	e.ConfidenceLabel = "EXTRACTED"
	e.Origin = originForSemanticKind(e.Kind)
	e.Meta["semantic_source"] = provider
}

// RefuteEdge removes a false-positive edge from the graph.
// Returns true if the edge was removed.
func RefuteEdge(g graph.Store, e *graph.Edge) bool {
	return g.RemoveEdge(e.From, e.To, e.Kind)
}

// AddSemanticEdge adds a new edge discovered by semantic analysis. Origin is
// tagged LSP-grade (see ConfirmEdge).
func AddSemanticEdge(g graph.Store, from, to string, kind graph.EdgeKind, filePath string, line int, provider string) *graph.Edge {
	e := NewSemanticEdge(from, to, kind, filePath, line, provider)
	g.AddEdge(e)
	return e
}

// NewSemanticEdge constructs, but does not persist, a semantic edge. Providers
// use it to stage bounded AddBatch writes instead of committing one store
// transaction per compiler/LSP reference.
func NewSemanticEdge(from, to string, kind graph.EdgeKind, filePath string, line int, provider string) *graph.Edge {
	return &graph.Edge{
		From:            from,
		To:              to,
		Kind:            kind,
		FilePath:        filePath,
		Line:            line,
		Confidence:      1.0,
		ConfidenceLabel: "EXTRACTED",
		Origin:          originForSemanticKind(kind),
		Meta: map[string]any{
			"semantic_source": provider,
		},
	}
}

// originForSemanticKind maps edge kind to the appropriate LSP-grade tier.
// Interface → implementation is a dispatch resolution (one step less direct
// than a literal target match), so it gets lsp_dispatch; direct target
// references get lsp_resolved. Method overrides are method-level
// dispatch — same tier as EdgeImplements.
func originForSemanticKind(kind graph.EdgeKind) string {
	if kind == graph.EdgeImplements || kind == graph.EdgeOverrides {
		return graph.OriginLSPDispatch
	}
	return graph.OriginLSPResolved
}

// EnrichNodeMeta sets semantic type information on a node.
func EnrichNodeMeta(n *graph.Node, key string, value any, provider string) {
	if n.Meta == nil {
		n.Meta = make(map[string]any)
	}
	n.Meta[key] = value
	n.Meta["semantic_source"] = provider
}

// FindMatchingEdge searches for an existing edge between two nodes of a given kind.
func FindMatchingEdge(g graph.Store, from, to string, kind graph.EdgeKind) *graph.Edge {
	edges := g.GetOutEdges(from)
	for _, e := range edges {
		if e.To == to && e.Kind == kind {
			return e
		}
	}
	return nil
}

// NodesByLanguage returns all nodes in the graph that match the given language.
func NodesByLanguage(g graph.Store, language string) []*graph.Node {
	return g.GetNodesByLanguage(language)
}

// EdgesByLanguage returns all edges whose source node matches the given language.
func EdgesByLanguage(g graph.Store, language string) []*graph.Edge {
	nodes := g.GetNodesByLanguage(language)
	if len(nodes) == 0 {
		return nil
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n != nil {
			ids = append(ids, n.ID)
		}
	}
	edgesBySource := g.GetOutEdgesByNodeIDs(ids)
	var result []*graph.Edge
	for _, id := range ids {
		result = append(result, edgesBySource[id]...)
	}
	return result
}
