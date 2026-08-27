package analysis

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// connectivity.go reports the connectivity *health of the graph itself* —
// a diagnostic for extraction/indexing quality, not a code-quality
// finding.
//
// This is deliberately DISTINCT from dead-code analysis (FindDeadCode):
//
//   - Dead-code analysis reports symbols with zero *incoming usage*
//     edges. Such a symbol is still a normally extracted node: its file
//     `defines` it, a method is `member_of` its type. That is a narrower
//     signal than "unreachable", and it is not a removal verdict: a call
//     site the resolver left unresolved — an ambiguous member call, a
//     name-only match — leaves exactly the same zero-incoming shape as
//     genuinely unused code. Confirm a dead-code row against the source
//     before deleting anything.
//
//   - This analyzer reports *isolated* nodes — nodes with zero edges of
//     *any* kind, structural edges included. A normally extracted
//     function or method always carries at least the structural edge
//     from its file (`defines`); a method additionally a `member_of`
//     edge to its type. A node with zero total edges therefore almost
//     never reflects "unused code" — it reflects that the extractor
//     never processed the symbol (or its file). The finding is a graph
//     *quality* signal: localise the extraction gap, do not delete the
//     code.
//
// The isolated/leaf classification reuses graph.ClassifyZeroEdge — the
// same zero-edge classification used for per-symbol caveats — so the
// definition of "isolated" stays in lockstep with the rest of Gortex.

// ConnectivityFileEntry attributes dead-weight (isolated + leaf) nodes
// to a single source file, so an extraction gap can be localised.
type ConnectivityFileEntry struct {
	FilePath string `json:"file_path"`
	// Isolated is the count of zero-edge nodes contributed by this file.
	Isolated int `json:"isolated"`
	// Leaf is the count of degree-1 nodes contributed by this file.
	Leaf int `json:"leaf"`
	// DeadWeight is Isolated+Leaf — the rank key.
	DeadWeight int `json:"dead_weight"`
}

// ConnectivityKindEntry breaks the isolated/leaf counts down by node
// kind, so a gap concentrated in one kind (e.g. only methods) is
// visible.
type ConnectivityKindEntry struct {
	Kind     string `json:"kind"`
	Total    int    `json:"total"`
	Isolated int    `json:"isolated"`
	Leaf     int    `json:"leaf"`
}

// GraphConnectivityReport is the structured connectivity-health report
// for a set of graph nodes.
type GraphConnectivityReport struct {
	// NominalNodes is the total node count — the graph's reported size.
	NominalNodes int `json:"nominal_nodes"`
	// EffectiveNodes is the count of nodes with at least one edge — the
	// graph's *connected* size. The two diverge when the extractor
	// dropped edges.
	EffectiveNodes int `json:"effective_nodes"`
	// EffectiveRatio is EffectiveNodes/NominalNodes (1.0 when every node
	// is connected, 0.0 for an empty graph).
	EffectiveRatio float64 `json:"effective_ratio"`

	// Isolated is the count of nodes with zero edges of any kind —
	// structural edges included. The headline extraction-gap signal.
	Isolated int `json:"isolated"`
	// Leaf is the count of degree-1 nodes (exactly one edge, in or out).
	Leaf int `json:"leaf"`
	// SourceOnly is the count of nodes with only outgoing edges.
	SourceOnly int `json:"source_only"`
	// SinkOnly is the count of nodes with only incoming edges.
	SinkOnly int `json:"sink_only"`

	// ByKind breaks the totals down by node kind (only kinds that
	// contributed at least one node are listed).
	ByKind []ConnectivityKindEntry `json:"by_kind"`
	// DeadWeightByFile ranks source files by their isolated+leaf node
	// contribution, so an extraction gap can be localised.
	DeadWeightByFile []ConnectivityFileEntry `json:"dead_weight_by_file"`

	// Note explains, in human-readable form, how this report differs
	// from a dead-code finding — so a reader does not mistake an
	// isolated node for unused code.
	Note string `json:"note"`
}

// connectivityNote is the standing human-readable caveat distinguishing
// this analyzer from dead-code analysis.
const connectivityNote = "Connectivity health is a graph-EXTRACTION diagnostic, not a " +
	"code-quality finding. Isolated nodes have zero edges of ANY kind " +
	"(structural `defines`/`member_of` included) — a normally extracted " +
	"symbol always has at least a structural edge, so an isolated node " +
	"signals the indexer mis-extracted the symbol, NOT that the code is " +
	"unused. This is distinct from dead code (analyze kind=dead_code), " +
	"which reports symbols with zero INCOMING usage edges. That is a " +
	"narrower signal, not a removal verdict: a call site the resolver " +
	"left unresolved — an ambiguous member call, a name-only match — " +
	"leaves the same zero-incoming shape as genuinely unused code, so " +
	"confirm a dead_code row against the source before removing anything."

// GraphConnectivity computes the connectivity-health report over the
// supplied nodes. The caller passes the node slice (e.g. a
// workspace-scoped slice) and the graph the nodes belong to; edge
// lookups go through g so the report reflects the live edge set.
//
// fileLimit caps how many files DeadWeightByFile carries — files are
// ranked by dead-weight descending, ties broken by path; pass 0 or a
// negative value for no cap.
//
// Backends that implement graph.NodeDegreeAggregator serve every
// per-node count from one bulk pass; the fallback path runs the legacy
// per-node GetInEdges + GetOutEdges trio. The arithmetic is identical
// either way — this analyzer needs only the in/out totals, which is
// why a kind-only aggregate row suffices. It deliberately does NOT
// reproduce ClassifyZeroEdge: that classifier also weighs each usage
// edge's provenance, which no edge-count row can express.
func GraphConnectivity(g graph.Store, nodes []*graph.Node, fileLimit int) GraphConnectivityReport {
	report := GraphConnectivityReport{Note: connectivityNote}
	if g == nil {
		return report
	}

	type kindAgg struct {
		total    int
		isolated int
		leaf     int
	}
	type fileAgg struct {
		isolated int
		leaf     int
	}
	byKind := map[graph.NodeKind]*kindAgg{}
	byFile := map[string]*fileAgg{}

	// Bulk per-node count fetch when the backend supports it; one
	// bulk pair vs. 3N per-node round-trips for the legacy path
	// (the killer on a disk backend — see the NodeDegreeAggregator doc-comment
	// for the workspace-scale numbers). Returns a map keyed on node ID
	// or nil when the capability isn't available; the fallback path
	// re-queries per node via the closure below.
	counts := collectConnectivityCounts(g, nodes)

	for _, n := range nodes {
		if n == nil {
			continue
		}
		report.NominalNodes++

		ka := byKind[n.Kind]
		if ka == nil {
			ka = &kindAgg{}
			byKind[n.Kind] = ka
		}
		ka.total++

		var inCount, outCount int
		if counts != nil {
			row := counts[n.ID]
			inCount = row.InCount
			outCount = row.OutCount
		} else {
			inCount = len(g.GetInEdges(n.ID))
			outCount = len(g.GetOutEdges(n.ID))
		}
		degree := inCount + outCount

		if degree > 0 {
			report.EffectiveNodes++
		}

		// Isolated == zero edges of any kind. ClassifyZeroEdge returns
		// ZeroEdgePossibleExtractionGap for exactly this case (for a
		// known node), so the "isolated" definition stays bound to the
		// shared zero-edge classification used for per-symbol caveats.
		// We derive it from the counts directly; the underlying
		// classifier's check is in == 0 && out == 0 for a known id.
		isolated := degree == 0
		leaf := degree == 1

		if isolated {
			report.Isolated++
			ka.isolated++
		}
		if leaf {
			report.Leaf++
			ka.leaf++
		}
		if degree > 0 && inCount == 0 {
			report.SourceOnly++
		}
		if degree > 0 && outCount == 0 {
			report.SinkOnly++
		}

		// Dead-weight attribution: an isolated or leaf node is a
		// candidate extraction gap; tally it against its source file
		// so the gap can be localised.
		if isolated || leaf {
			fa := byFile[n.FilePath]
			if fa == nil {
				fa = &fileAgg{}
				byFile[n.FilePath] = fa
			}
			if isolated {
				fa.isolated++
			}
			if leaf {
				fa.leaf++
			}
		}
	}

	if report.NominalNodes > 0 {
		report.EffectiveRatio = float64(report.EffectiveNodes) / float64(report.NominalNodes)
	}

	// Per-kind breakdown — only kinds that contributed a node, sorted
	// by kind name for deterministic output.
	report.ByKind = make([]ConnectivityKindEntry, 0, len(byKind))
	for kind, agg := range byKind {
		report.ByKind = append(report.ByKind, ConnectivityKindEntry{
			Kind:     string(kind),
			Total:    agg.total,
			Isolated: agg.isolated,
			Leaf:     agg.leaf,
		})
	}
	sort.Slice(report.ByKind, func(i, j int) bool {
		return report.ByKind[i].Kind < report.ByKind[j].Kind
	})

	// Dead-weight attribution by file — ranked by dead-weight
	// descending, ties broken by path so output is deterministic.
	report.DeadWeightByFile = make([]ConnectivityFileEntry, 0, len(byFile))
	for path, agg := range byFile {
		report.DeadWeightByFile = append(report.DeadWeightByFile, ConnectivityFileEntry{
			FilePath:   path,
			Isolated:   agg.isolated,
			Leaf:       agg.leaf,
			DeadWeight: agg.isolated + agg.leaf,
		})
	}
	sort.Slice(report.DeadWeightByFile, func(i, j int) bool {
		if report.DeadWeightByFile[i].DeadWeight != report.DeadWeightByFile[j].DeadWeight {
			return report.DeadWeightByFile[i].DeadWeight > report.DeadWeightByFile[j].DeadWeight
		}
		return report.DeadWeightByFile[i].FilePath < report.DeadWeightByFile[j].FilePath
	})
	if fileLimit > 0 && len(report.DeadWeightByFile) > fileLimit {
		report.DeadWeightByFile = report.DeadWeightByFile[:fileLimit]
	}

	return report
}

// collectConnectivityCounts returns per-node in/out/usage counts for
// the supplied node slice via the backend's NodeDegreeAggregator
// capability. Returns nil when the backend doesn't implement the
// capability — GraphConnectivity then falls back to the legacy
// per-node g.GetInEdges/g.GetOutEdges path so semantics never differ.
//
// We pass UsageInboundEdgeKinds so the server fills UsageInCount —
// today GraphConnectivity only consumes In/Out totals, but the usage
// count rides on the same row at no extra round-trip cost and makes
// the capability self-contained for callers that need it next.
func collectConnectivityCounts(g graph.Store, nodes []*graph.Node) map[string]graph.NodeDegreeRow {
	agg, ok := g.(graph.NodeDegreeAggregator)
	if !ok {
		return nil
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n == nil || n.ID == "" {
			continue
		}
		ids = append(ids, n.ID)
	}
	if len(ids) == 0 {
		return map[string]graph.NodeDegreeRow{}
	}
	rows := agg.NodeDegreeCounts(ids, graph.UsageInboundEdgeKinds())
	out := make(map[string]graph.NodeDegreeRow, len(rows))
	for _, r := range rows {
		out[r.NodeID] = r
	}
	return out
}
