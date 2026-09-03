package mcp

import (
	"context"
	"encoding/json"
	"math"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

// AuditReport is the per-repo complexity-axis health summary backing
// `gortex audit` and its README badge. Exported so the CLI can decode it.
type AuditReport struct {
	SymbolCount  int                `json:"symbol_count"`
	MeanScore    float64            `json:"mean_score"`
	Grade        string             `json:"grade"`
	GradeCounts  map[string]int     `json:"grade_counts"`
	WorstSymbols []AuditSymbolScore `json:"worst_symbols,omitempty"`
}

// AuditSymbolScore is one callable symbol's complexity-axis score.
type AuditSymbolScore struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
	Grade string  `json:"grade"`
	File  string  `json:"file"`
	Line  int     `json:"line"`
}

// auditHealthKinds is the callable kind set the complexity axis scores.
// Declared once so the scoped-node pushdown and ComputeAuditReport's
// defensive re-check agree on what "callable" means.
var auditHealthKinds = []graph.NodeKind{graph.KindFunction, graph.KindMethod}

func (s *Server) registerAuditTool() {
	s.addTool(
		mcp.NewTool("audit_health",
			mcp.WithDescription("Compute a repo-level complexity-axis health grade (A-F) from the graph: per-callable fan-in/fan-out health, the mean grade, the per-grade distribution, and the worst-scored symbols. Backs `gortex audit` and its README badge. Scored symbols are clamped to the session workspace and narrowed further by repo/project/scope."),
			mcp.WithString("repo", mcp.Description("Narrow the audit to a single repository prefix (tracked name or path), clamped to the session workspace.")),
			mcp.WithString("project", mcp.Description("Narrow the audit to the repositories in a project, clamped to the session workspace.")),
			mcp.WithString("workspace", mcp.Description("Restrict the audit to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — its repositories narrow the audit, clamped to the session workspace.")),
		),
		s.handleAuditHealth,
	)
}

// handleAuditHealth grades the callable symbols the caller can actually
// see. It resolves the uniform repo/project/workspace/scope narrowing
// exactly like the analyze dispatcher does, then sources its symbols
// through the scoped-node accessor so a workspace-bound session can
// never be graded against another workspace's symbols — and so an
// explicit repo selector narrows instead of being silently dropped.
func (s *Server) handleAuditHealth(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	g := s.readerFor(ctx)
	if g == nil {
		return mcp.NewToolResultError("audit: graph is not initialised"), nil
	}
	resolved, errResult := s.resolveScope(ctx, req, IntentAnalyze)
	if errResult != nil {
		return errResult, nil
	}
	ctx = withRepoAllow(ctx, resolved.RepoAllow)
	data, err := json.Marshal(ComputeAuditReport(g, s.scopedNodesByKinds(ctx, auditHealthKinds)))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return withScopeResult(mcp.NewToolResultText(string(data)), nil, resolved)
}

// ComputeAuditReport grades the supplied callable nodes and produces the
// repo grade. Callers pass the node set already narrowed to the scope
// they may observe (see Server.scopedNodesByKinds) — this function never
// widens beyond it. Fan-in / fan-out are still counted over the whole
// graph, so a cross-repo caller still contributes to a symbol's score;
// only which symbols get scored is scoped. That matches the health_score
// analyzer, which likewise scopes candidates and counts edges globally.
//
// Complexity-axis-only math matching the health_score analyzer's
// complexity component:
//
//	raw               = fan_in*2 + fan_out*1.5   (per callable symbol)
//	complexity_health = 100 / (1 + raw/20)
//	mean              = mean across callable symbols
//	grade             = auditScoreGrade(mean)
//
// Read-only: g is the caller's request reader, so an overlay-active
// session is graded against its own buffers.
func ComputeAuditReport(g graph.Reader, nodes []*graph.Node) AuditReport {
	type entry struct {
		id, file string
		line     int
		score    float64
	}
	var entries []entry
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		fanIn := 0
		fanOut := 0
		for _, e := range g.GetInEdges(n.ID) {
			if e.Kind == graph.EdgeCalls || e.Kind == graph.EdgeReferences {
				fanIn++
			}
		}
		for _, e := range g.GetOutEdges(n.ID) {
			if e.Kind == graph.EdgeCalls {
				fanOut++
			}
		}
		raw := float64(fanIn)*2.0 + float64(fanOut)*1.5
		complexity := 100.0 / (1.0 + raw/20.0)
		entries = append(entries, entry{id: n.ID, file: n.FilePath, line: n.StartLine, score: complexity})
	}

	report := AuditReport{SymbolCount: len(entries), GradeCounts: map[string]int{}}
	if len(entries) == 0 {
		report.Grade = auditScoreGrade(0)
		return report
	}

	var sum float64
	for _, e := range entries {
		sum += e.score
	}
	report.MeanScore = math.Round((sum/float64(len(entries)))*10) / 10
	report.Grade = auditScoreGrade(report.MeanScore)
	for _, e := range entries {
		report.GradeCounts[auditScoreGrade(e.score)]++
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].score != entries[j].score {
			return entries[i].score < entries[j].score
		}
		if entries[i].file != entries[j].file {
			return entries[i].file < entries[j].file
		}
		return entries[i].line < entries[j].line
	})
	limit := min(5, len(entries))
	report.WorstSymbols = make([]AuditSymbolScore, 0, limit)
	for i := range limit {
		e := entries[i]
		report.WorstSymbols = append(report.WorstSymbols, AuditSymbolScore{
			ID:    e.id,
			Score: math.Round(e.score*10) / 10,
			Grade: auditScoreGrade(e.score),
			File:  e.file,
			Line:  e.line,
		})
	}
	return report
}

// auditScoreGrade mirrors the health_score analyzer's scoreGrade.
func auditScoreGrade(score float64) string {
	switch {
	case score >= 85:
		return "A"
	case score >= 70:
		return "B"
	case score >= 55:
		return "C"
	case score >= 40:
		return "D"
	default:
		return "F"
	}
}
