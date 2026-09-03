package mcp

import (
	"context"
	"fmt"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/query"
)

// handleQueryProject runs a read-only symbol search against another
// project (or a bare tracked-repo prefix) without switching the active
// project. It deliberately bypasses the session-workspace clamp — this
// is the sanctioned cross-project read — and never mutates the active
// project or persists config.
func (s *Server) handleQueryProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project, err := req.RequireString("project")
	if err != nil {
		return mcp.NewToolResultError("project is required"), nil
	}
	q, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("query is required"), nil
	}
	if s.configManager == nil {
		return mcp.NewToolResultError("multi-project configuration is not available"), nil
	}
	limit := req.GetInt("limit", 20)
	if limit <= 0 {
		limit = 20
	}
	gc := s.configManager.Global()

	// Resolve the target as a project name / per-repo project tag, or
	// fall back to treating it as a bare tracked-repo prefix.
	var prefixes []string
	if repos, rErr := gc.ResolveRepos(project); rErr == nil {
		for _, r := range repos {
			prefixes = append(prefixes, config.ResolvePrefix(r))
		}
	} else if prefix := s.resolveRepoPrefix(project); prefix != "" {
		prefixes = []string{prefix}
	}
	if len(prefixes) == 0 {
		available := make([]string, 0, len(gc.Projects))
		for name := range gc.Projects {
			available = append(available, name)
		}
		sort.Strings(available)
		return NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeProjectUnknown,
			Message:   fmt.Sprintf("no project or tracked repo named %q", project),
			Data:      map[string]any{"project": project, "available_projects": available},
		}), nil
	}
	allowed := make(map[string]bool, len(prefixes))
	for _, p := range prefixes {
		allowed[p] = true
	}

	// Search the base graph unscoped, then confine to the target's
	// repos. Using s.engine (not engineFor) keeps this off the session
	// overlay and clamp — query_project is an explicit cross-project read.
	nodes := s.engine.SearchSymbolsScoped(q, limit*5, query.QueryOptions{})
	nodes = filterNodes(nodes, allowed)
	if len(nodes) > limit {
		nodes = nodes[:limit]
	}
	nodes = s.withAbsPaths(ctx, nodes)

	results := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		results = append(results, n.Brief())
	}
	sort.Strings(prefixes)
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"project": project,
		"query":   q,
		"repos":   prefixes,
		"total":   len(results),
		"results": results,
	})
}
