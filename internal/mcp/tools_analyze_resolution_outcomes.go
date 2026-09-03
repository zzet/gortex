package mcp

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analyzer"
	"github.com/zzet/gortex/internal/graph"
)

// Structured resolver-suppression taxonomy. The canonical constants live in
// internal/analyzer; these aliases keep existing MCP-package call sites and
// tests compiling against a single source of truth.
const (
	outcomeAmbiguousMultiMatch = analyzer.OutcomeAmbiguousMultiMatch
	outcomeCandidateOutOfScope = analyzer.OutcomeCandidateOutOfScope
	outcomeCrossLanguageOnly   = analyzer.OutcomeCrossLanguageOnly
	outcomeNoDefinition        = analyzer.OutcomeNoDefinition
	outcomeStdlibHeader        = analyzer.OutcomeStdlibHeader
)

// handleAnalyzeResolutionOutcomes classifies every unresolved call /
// reference edge by the structured reason the resolver gave up, and
// returns a per-reason rollup plus example rows. Optional `reason`
// filters to one outcome; optional `limit` caps the example rows.
//
// The classification itself lives in
// internal/analyzer.AnalyzeResolutionOutcomes — a pure Calculation — so the
// same taxonomy logic is independently testable and reusable across surfaces.
func (s *Server) handleAnalyzeResolutionOutcomes(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	reasonFilter := strings.TrimSpace(stringArg(args, "reason"))
	limit := intArg(args, "limit", 50)

	result := analyzer.AnalyzeResolutionOutcomes(s.readerFor(ctx), reasonFilter, limit)

	if isCompact(req) {
		var b strings.Builder
		reasons := make([]string, 0, len(result.ByReason))
		for r := range result.ByReason {
			reasons = append(reasons, r)
		}
		sort.Slice(reasons, func(i, j int) bool { return result.ByReason[reasons[i]] > result.ByReason[reasons[j]] })
		for _, r := range reasons {
			b.WriteString(r)
			b.WriteString(": ")
			b.WriteString(strconv.Itoa(result.ByReason[r]))
			b.WriteByte('\n')
		}
		if len(result.ByReason) == 0 {
			b.WriteString("no unresolved edges\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"by_reason": result.ByReason,
		"total":     result.Total,
		"rows":      result.Rows,
	})
}

// nodeIsDefinitionKind reports whether a node kind is a callable / type
// definition an unresolved call or reference could legitimately bind to.
// The resolution-outcome classifier itself now lives in internal/analyzer;
// this helper stays in the mcp package because id_resolve.go also relies on it.
func nodeIsDefinitionKind(k graph.NodeKind) bool {
	switch k {
	case graph.KindFunction, graph.KindMethod, graph.KindType,
		graph.KindInterface, graph.KindVariable, graph.KindConstant, graph.KindField:
		return true
	}
	return false
}
