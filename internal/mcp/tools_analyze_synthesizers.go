package mcp

import (
	"context"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analyzer"
)

// handleAnalyzeSynthesizers rolls up the framework dynamic-dispatch
// synthesizer engine's output: every edge carrying a `synthesized_by`
// provenance marker, grouped by the synthesizer that produced it. This
// is the queryable face of the engine — "which framework-dispatch passes
// fired, and how many edges did each materialise?" — letting an agent
// audit the heuristic, framework-wired call edges (gRPC stub → handler,
// Temporal proxy → activity, event-channel emit → listener, native
// bridge call → implementation) separately from compiler-verified ones.
//
// The aggregation itself lives in internal/analyzer.AnalyzeSynthesizers so
// the same logic backs both this MCP tool and the `gortex analyze` CLI.
//
// Optional `name` filters to a single synthesizer.
func (s *Server) handleAnalyzeSynthesizers(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	nameFilter := strings.TrimSpace(stringArg(args, "name"))

	var opts []analyzer.SynthesizersOption
	if nameFilter != "" {
		opts = append(opts, analyzer.WithSynthesizerNameFilter(nameFilter))
	}
	// Clamp to the session workspace (synthesizer edge enumeration is
	// global) so a workspace-bound caller never sees edges from sibling
	// workspaces.
	wsRepos, bound := s.sessionWorkspaceRepoSet(ctx)
	if bound && len(wsRepos) > 0 {
		opts = append(opts, analyzer.WithSynthesizerRepoScope(wsRepos))
	}
	result := analyzer.SynthesizersResult{}
	if !bound || len(wsRepos) > 0 {
		result = analyzer.AnalyzeSynthesizers(s.readerFor(ctx), opts...)
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range result.Synthesizers {
			b.WriteString(r.Name)
			b.WriteString(": ")
			b.WriteString(strconv.Itoa(r.Edges))
			b.WriteString(" edges (")
			b.WriteString(r.Provenance)
			b.WriteString(")\n")
		}
		if len(result.Synthesizers) == 0 {
			b.WriteString("no synthesized edges\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	resp := map[string]any{
		"synthesizers": result.Synthesizers,
		"total_edges":  result.TotalEdges,
	}
	if blk := s.workspaceScopeBlock(ctx, req, "synthesizers"); blk != nil {
		resp["scope"] = blk
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}
