package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
)

// registerBootstrapResources surfaces the bootstrap-state tools as
// MCP resources. Every session reads these at startup; resources are
// the right shape because they take no arguments, are URI-addressable,
// and can push `notifications/resources/updated` after each graph
// re-warm so agents stop polling.
//
// The corresponding tools (`graph_stats`, `index_health`,
// `workspace_info`, `list_repos`, `get_active_project`) stay
// registered. Client support for resources is patchier than tools —
// keeping the tool form alive avoids regressing those clients.
func (s *Server) registerBootstrapResources() {
	s.addResource(
		mcp.NewResource(
			"gortex://index-health",
			"Index Health",
			mcp.WithResourceDescription("Health score, parse failures, stale files, language coverage. Read at session start to confirm the index is current. Same payload as the `index_health` tool. Read from a session bound to a view, the payload names `base_scoped`: this probe describes the indexed corpus, not the selected checkout."),
			mcp.WithMIMEType("application/json"),
		),
		s.handleResourceIndexHealth,
	)

	s.addResource(
		mcp.NewResource(
			"gortex://workspace",
			"Workspace Info",
			mcp.WithResourceDescription("Bind mode, root directory, marker contents, the auto-discovered member set, and any unknown marker keys. Same payload as the `workspace_info` tool."),
			mcp.WithMIMEType("application/json"),
		),
		s.handleResourceWorkspace,
	)

	s.addResource(
		mcp.NewResource(
			"gortex://repos",
			"Workspace Repos",
			mcp.WithResourceDescription("Every project in the active workspace — the legal `repo` values for any scope: repo or scope: fan-out tool call. Same payload as the `list_repos` tool."),
			mcp.WithMIMEType("application/json"),
		),
		s.handleResourceRepos,
	)

	s.addResource(
		mcp.NewResource(
			"gortex://active-project",
			"Active Project",
			mcp.WithResourceDescription("Current active project name and its repo list. Same payload as the `get_active_project` tool."),
			mcp.WithMIMEType("application/json"),
		),
		s.handleResourceActiveProject,
	)
}

func (s *Server) handleResourceIndexHealth(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	payload, err := s.buildIndexHealthPayloadCtx(ctx)
	if err != nil {
		return nil, err
	}
	if payload == nil {
		payload = map[string]any{
			"error": "no indexer available",
		}
	}
	return jsonResource(req.Params.URI, payload)
}

func (s *Server) handleResourceWorkspace(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	return jsonResource(req.Params.URI, s.buildWorkspaceInfoPayload(ctx))
}

func (s *Server) handleResourceRepos(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	return jsonResource(req.Params.URI, s.buildListReposPayload(ctx))
}

func (s *Server) handleResourceActiveProject(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	return jsonResource(req.Params.URI, s.buildActiveProjectPayload(ctx))
}

// bootstrapResourceURIs lists the resources whose payloads change when
// the graph is re-warmed. The resource broadcaster pushes a
// `notifications/resources/updated` for each on re-warm completion.
func bootstrapResourceURIs() []string {
	return []string{
		"gortex://stats",
		"gortex://index-health",
		"gortex://workspace",
		"gortex://repos",
		"gortex://active-project",
	}
}
