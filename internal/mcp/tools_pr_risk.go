package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
)

// registerPRRiskTool registers the pr_risk MCP tool — a PR-level composite
// risk scorer over a set of changed symbols. Forge-agnostic: the caller hands
// in already-mapped symbol IDs, or a base ref to derive the changed set from a
// git diff against the indexed repo.
func (s *Server) registerPRRiskTool() {
	s.addTool(
		mcp.NewTool("pr_risk",
			mcp.WithDescription("PR-level composite risk score for a set of changed symbols. Blends five 0-100 axes — blast-radius flow, caller fan-in, test-coverage gap, security-keyword sensitivity, and community span — into one 0-100 score with a coarse risk level (LOW/MEDIUM/HIGH/CRITICAL) and an ordered review_priorities list. Pass `ids` (comma-separated symbol IDs you already mapped) OR `base` (a git ref like main — the changed symbols are derived from the diff against it). Use to triage how carefully a change needs reviewing before reading the code."),
			mcp.WithString("ids", mcp.Description("Comma-separated changed symbol IDs (already mapped). One of ids|base is required.")),
			mcp.WithString("base", mcp.Description("Base git ref (e.g. main). Derives the changed symbols from `git diff base...HEAD` against the indexed repo. One of ids|base is required.")),
			mcp.WithString("repo", mcp.Description("Repository prefix to resolve the working tree for the `base` diff (multi-repo mode).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handlePRRisk,
	)
}

// handlePRRisk scores PR-level risk for the changed symbols named by `ids`, or
// derived from a `base`-ref diff, and projects the result to the pr_risk wire
// shape.
func (s *Server) handlePRRisk(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError("no graph available — index a repo first"), nil
	}

	idsStr := strings.TrimSpace(req.GetString("ids", ""))
	base := strings.TrimSpace(req.GetString("base", ""))
	repo := strings.TrimSpace(req.GetString("repo", ""))

	var symbolIDs []string
	var changedFiles []string
	reader := s.readerFor(ctx)

	switch {
	case idsStr != "":
		for _, id := range strings.Split(idsStr, ",") {
			if id = strings.TrimSpace(id); id != "" {
				symbolIDs = append(symbolIDs, id)
			}
		}
		// Derive the changed-file set from the mapped symbols so the
		// security axis (path-based) still has signal on the ids path.
		fileSeen := make(map[string]bool)
		for _, id := range symbolIDs {
			if n := reader.GetNode(id); n != nil && n.FilePath != "" && !fileSeen[n.FilePath] {
				fileSeen[n.FilePath] = true
				changedFiles = append(changedFiles, n.FilePath)
			}
		}
	case base != "":
		root, prefix := s.diffRepoScope(ctx, repo)
		if root == "" {
			return mcp.NewToolResultError("could not resolve a repository root for the base diff"), nil
		}
		diff, err := analysis.MapGitDiff(reader, root, prefix, "compare", base)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("git diff against %q failed: %v", base, err)), nil
		}
		for _, cs := range diff.ChangedSymbols {
			symbolIDs = append(symbolIDs, cs.ID)
		}
		changedFiles = diff.ChangedFiles
	default:
		return mcp.NewToolResultError("either ids or base is required"), nil
	}

	communities := s.getCommunities()
	var nodeToComm map[string]string
	if communities != nil {
		nodeToComm = communities.NodeToComm
	}

	result := analysis.ScorePRRisk(reader, analysis.PRRiskInput{
		SymbolIDs:    symbolIDs,
		ChangedFiles: changedFiles,
		NodeToComm:   nodeToComm,
		Communities:  communities,
		Processes:    s.getProcesses(),
	})

	payload := prRiskPayload(result, len(symbolIDs))

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodePRRisk(payload))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(payload)
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// prRiskPayload projects a PRRiskResult onto the pr_risk wire shape: a 0-100
// score, the risk label, the ordered review_priorities, and the supporting
// counts.
func prRiskPayload(result analysis.PRRiskResult, changedSymbols int) map[string]any {
	priorities := make([]map[string]any, 0, len(result.Factors))
	for _, f := range result.Factors {
		priorities = append(priorities, map[string]any{
			"axis":   f.Axis,
			"score":  f.Score,
			"reason": f.Reason,
		})
	}
	hits := result.SecurityHits
	if hits == nil {
		hits = []string{}
	}
	return map[string]any{
		"score":             result.Score,
		"risk":              string(result.Risk),
		"review_priorities": priorities,
		"total_affected":    result.TotalAffected,
		"uncovered_symbols": result.UncoveredSymbols,
		"community_span":    result.CommunitySpan,
		"security_hits":     hits,
		"changed_symbols":   changedSymbols,
	}
}

// pickRepoRoot resolves a single working-tree root from the collected map. In
// single-repo mode the root is keyed by the lone prefix (possibly empty); in
// multi-repo mode the caller's `repo` prefix selects it, falling back to the
// sole entry when there is exactly one.
func pickRepoRoot(roots map[string]string, repo string) string {
	if repo != "" {
		if root, ok := roots[repo]; ok {
			return root
		}
	}
	if len(roots) == 1 {
		for _, root := range roots {
			return root
		}
	}
	// Prefer a non-empty-prefix entry deterministically would require
	// ordering; with multiple repos and no selector we cannot guess, so
	// fall back to the empty-prefix (single-indexer) root if present.
	return roots[""]
}
