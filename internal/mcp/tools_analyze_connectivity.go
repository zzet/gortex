// connectivity_health — a graph-EXTRACTION quality diagnostic.
//
// This analyzer reports the connectivity health of the knowledge graph
// itself: how many nodes are isolated (zero edges of any kind), how
// many are leaves / source-only / sink-only, the effective-vs-nominal
// graph size, and which source files contribute the most isolated /
// leaf nodes.
//
// It is deliberately DISTINCT from kind=dead_code. dead_code reports
// symbols with zero *incoming usage* edges — a narrower signal than
// "unreachable", and not a removal verdict, since an unresolved or
// name-only call site leaves the same zero-incoming shape. This
// analyzer reports isolated nodes — zero edges of *any* kind, structural
// edges included — which a normally extracted symbol never has. An isolated
// node signals the indexer mis-extracted the symbol or its file, not
// that the code is unused; the response carries a `note` spelling out
// the distinction so a reader does not delete live code.

package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
)

// handleAnalyzeConnectivityHealth answers analyze kind=connectivity_health.
//
// It walks the session-scoped node set once, classifying each node via
// the shared graph.ClassifyZeroEdge (isolated == zero edges of any
// kind) and by in/out degree (leaf / source-only / sink-only), then
// rolls the isolated + leaf counts up per node kind and per source
// file.
//
// Args:
//
//   - `limit`: cap the dead_weight_by_file ranking (default 50).
func (s *Server) handleAnalyzeConnectivityHealth(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	fileLimit := 50
	if v, ok := args["limit"].(float64); ok && v > 0 {
		fileLimit = int(v)
	}

	report := analysis.GraphConnectivity(s.readerFor(ctx), s.scopedNodesLight(ctx), fileLimit)

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeAnalyze("connectivity_health", report))
	}

	if isCompact(req) {
		var b strings.Builder
		fmt.Fprintf(&b, "effective %d / nominal %d  (ratio %.3f)\n",
			report.EffectiveNodes, report.NominalNodes, report.EffectiveRatio)
		fmt.Fprintf(&b, "isolated %d  leaf %d  source_only %d  sink_only %d\n",
			report.Isolated, report.Leaf, report.SourceOnly, report.SinkOnly)
		for _, k := range report.ByKind {
			if k.Isolated == 0 && k.Leaf == 0 {
				continue
			}
			fmt.Fprintf(&b, "  %-12s total %-5d isolated %-4d leaf %d\n",
				k.Kind, k.Total, k.Isolated, k.Leaf)
		}
		for _, f := range report.DeadWeightByFile {
			fmt.Fprintf(&b, "  %3d dead-weight  %s  (isolated %d, leaf %d)\n",
				f.DeadWeight, f.FilePath, f.Isolated, f.Leaf)
		}
		b.WriteString("\nnote: " + report.Note + "\n")
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, report)
}
