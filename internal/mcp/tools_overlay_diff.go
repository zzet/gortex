package mcp

import (
	"context"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/query"
)

// registerOverlayDiffTool wires the `compare_with_overlay` MCP tool.
// Called from registerOverlayTools so the tool only exists when
// overlay support is enabled. Independent of the apply-style overlay
// tools because compare_with_overlay runs the query TWICE — once
// against the base graph and once against the calling session's
// shadow view — and reports the delta. The base-vs-overlay diff is
// what `non-destructive overlay` actually buys you: side-by-side
// answers to "what would change if I committed this buffer?" without
// touching the saved-view graph.
func (s *Server) registerOverlayDiffTool() {
	s.addControlTool(
		mcp.NewTool("compare_with_overlay",
			mcp.WithDescription("Run a graph query against both the base (saved-buffer) graph and the calling session's overlay (editor-buffer) view, then report the delta. The base side answers \"what's true now?\"; the overlay side answers \"what would be true if I committed this buffer?\". Useful for previewing the impact of an unsaved edit on callers, dependents, or call chains. Supported `kind` values: find_usages, get_callers, get_call_chain, get_dependencies, get_dependents."),
			mcp.WithString("kind", mcp.Required(), mcp.Description("Query kind to run. One of: find_usages, get_callers, get_call_chain, get_dependencies, get_dependents.")),
			mcp.WithString("id", mcp.Required(), mcp.Description("Symbol node ID to query (e.g. \"target.go::Target\").")),
			mcp.WithNumber("depth", mcp.Description("Traversal depth for chain / dependency queries (default 2).")),
			mcp.WithNumber("limit", mcp.Description("Maximum number of nodes to return per side (default 50).")),
		),
		s.handleCompareWithOverlay,
	)
}

// handleCompareWithOverlay is the registered handler. It does NOT go
// through s.addTool (which would wrap with the overlay-injecting
// middleware) — we build views explicitly here so we can run the
// query against both base and overlay in a single call.
func (s *Server) handleCompareWithOverlay(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	kind, err := req.RequireString("kind")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if s.overlays == nil {
		return mcp.NewToolResultError("overlay support is not enabled on this server"), nil
	}
	if OverlayCohortIDFromContext(ctx) == "" {
		return mcp.NewToolResultError("compare_with_overlay requires an MCP session; connect via the daemon or set Mcp-Session-Id"), nil
	}
	ctx, view, viewErr := s.prepareOverlayRequest(ctx)
	if viewErr != nil {
		if ctxErr := requestContextError(ctx, viewErr); ctxErr != nil {
			return nil, ctxErr
		}
		return mcp.NewToolResultError(viewErr.Error()), nil
	}
	if view == nil {
		return mcp.NewToolResultError("session has no overlay attached; push an overlay before calling compare_with_overlay"), nil
	}

	depth := int(req.GetFloat("depth", 2))
	limit := int(req.GetFloat("limit", 50))
	opts := query.QueryOptions{Depth: depth, Limit: limit, Detail: "brief"}

	baseEng := s.engine
	overlayEng := s.engine.WithReader(view)

	baseIDs := runQueryKind(baseEng, kind, id, opts)
	overlayIDs := runQueryKind(overlayEng, kind, id, opts)

	added, removed, common := diffIDSets(baseIDs, overlayIDs)
	result := map[string]any{
		"kind":          kind,
		"id":            id,
		"depth":         depth,
		"limit":         limit,
		"overlay_paths": view.Layer().FilePaths(),
		"base":          baseIDs,
		"overlay":       overlayIDs,
		"delta": map[string]any{
			"added":   added,
			"removed": removed,
			"common":  common,
			"summary": map[string]any{
				"added_count":   len(added),
				"removed_count": len(removed),
				"common_count":  len(common),
				"base_count":    len(baseIDs),
				"overlay_count": len(overlayIDs),
			},
		},
	}
	return mcp.NewToolResultText(jsonOK(result)), nil
}

// runQueryKind dispatches a query kind against the supplied engine
// (base or overlay) and returns the resulting node IDs in stable
// sorted order. Sorting up front keeps diff output deterministic
// even when the underlying query returns nodes in shard-walk order.
func runQueryKind(eng *query.Engine, kind, id string, opts query.QueryOptions) []string {
	if eng == nil {
		return nil
	}
	var sg *query.SubGraph
	switch kind {
	case "find_usages":
		sg = eng.FindUsagesScoped(id, opts)
	case "get_callers":
		sg = eng.GetCallers(id, opts)
	case "get_call_chain":
		sg = eng.GetCallChain(id, opts)
	case "get_dependencies":
		sg = eng.GetDependencies(id, opts)
	case "get_dependents":
		sg = eng.GetDependents(id, opts)
	default:
		return nil
	}
	if sg == nil {
		return nil
	}
	ids := make([]string, 0, len(sg.Nodes))
	for _, n := range sg.Nodes {
		if n == nil || n.ID == id {
			continue // exclude the seed itself
		}
		ids = append(ids, n.ID)
	}
	sort.Strings(ids)
	return ids
}

// diffIDSets computes (added, removed, common) between two sorted ID
// slices. `added` = ids present in overlay but not base. `removed` =
// ids present in base but not overlay. `common` = ids in both.
func diffIDSets(base, overlay []string) (added, removed, common []string) {
	in := make(map[string]int, len(base)+len(overlay))
	for _, id := range base {
		in[id] |= 1
	}
	for _, id := range overlay {
		in[id] |= 2
	}
	for id, mask := range in {
		switch mask {
		case 1:
			removed = append(removed, id)
		case 2:
			added = append(added, id)
		case 3:
			common = append(common, id)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(common)
	return added, removed, common
}
