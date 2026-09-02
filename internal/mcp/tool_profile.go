package mcp

import (
	"context"
	"slices"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/daemon"
)

// IsToolEnabled reports whether a tool is reachable in this server's
// current profile — registered either as a live tool (in tools/list)
// or as a deferred tool behind tools_search. An empty name, or a name
// that was never registered, returns false.
func (s *Server) IsToolEnabled(name string) bool {
	if name == "" {
		return false
	}
	// A hide-mode preset removes the tool from the surface entirely
	// (filtered from tools/list and call-gated), even though it stays
	// registered in the underlying MCP server.
	if s.toolPolicy.hideMode() && !s.toolPolicy.allows(name) {
		return false
	}
	if _, ok := s.mcpServer.ListTools()[name]; ok {
		return true
	}
	if s.lazy != nil && slices.Contains(s.lazy.DeferredNames(), name) {
		return true
	}
	return false
}

// toolStatus classifies one tool name as live (eagerly in tools/list),
// deferred (hidden behind tools_search), or absent (not registered).
func (s *Server) toolStatus(name string) string {
	if s.toolPolicy.hideMode() && !s.toolPolicy.allows(name) {
		return "blocked"
	}
	if _, ok := s.mcpServer.ListTools()[name]; ok {
		return "live"
	}
	if s.lazy != nil && slices.Contains(s.lazy.DeferredNames(), name) {
		return "deferred"
	}
	return "absent"
}

// liveToolNames returns the sorted names of every tool currently in
// tools/list (the eagerly-visible surface).
func (s *Server) liveToolNames() []string {
	live := s.mcpServer.ListTools()
	out := make([]string, 0, len(live))
	for n := range live {
		// In hide mode the toolSurfaceFilter strips non-allowed tools
		// from tools/list; mirror that here so the reported live surface
		// matches what the agent actually sees.
		if s.toolPolicy.hideMode() && !s.toolPolicy.allows(n) {
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// sessionLiveToolNames returns the exact eagerly-visible surface for ctx.
// It deliberately drives the same per-session filter used by tools/list so
// client-aware presets, planning/workflow mode, learned promotions, and host
// exclusions cannot drift from what tool_profile reports.
func (s *Server) sessionLiveToolNames(ctx context.Context) []string {
	registered := s.mcpServer.ListTools()
	tools := make([]mcp.Tool, 0, len(registered))
	for _, entry := range registered {
		if entry == nil {
			continue
		}
		tools = append(tools, entry.Tool)
	}

	tools = s.toolSurfaceFilter(ctx, tools)
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		out = append(out, tool.Name)
	}
	sort.Strings(out)
	return out
}

// registeredToolNames returns the complete catalog behind this server: both
// the currently registered MCP tools and the lazy registry's cold tools.
//
// Read order matters: lazyToolRegistry.Promote holds its lock for the
// entire mark-and-register transition (see lazy_tools.go), so
// DeferredNames' RLock cannot return mid-promotion — it either observes a
// name still deferred, or observes it already excluded because Promote
// (registration included) has fully completed. Reading DeferredNames
// FIRST and ListTools SECOND therefore guarantees no false miss: a name
// excluded from the first read is guaranteed live by the time of the
// second (registration only ever moves deferred -> live, never back).
// Reading ListTools first (the previous order) raced a concurrent
// Promote: ListTools could snapshot before AddTool ran and DeferredNames
// could snapshot after the name was marked promoted, missing the name in
// both and misclassifying a legitimately live tool as "absent".
func (s *Server) registeredToolNames() []string {
	names := make(map[string]bool)
	if s.lazy != nil {
		for _, name := range s.lazy.DeferredNames() {
			names[name] = true
		}
	}
	for name := range s.mcpServer.ListTools() {
		names[name] = true
	}

	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// sessionToolBlocked reports whether a registered tool is intentionally
// unavailable in this session rather than merely withheld from tools/list.
func (s *Server) sessionToolBlocked(ctx context.Context, p *toolPolicy, name string) bool {
	if p != nil && p.preset == FacadeSurfaceVersion && !isFacadeToolName(name) {
		return true
	}
	if isDedicatedFacadeTool(name) {
		return true
	}
	if name == "ask" {
		if _, available := s.facades.legacy("ask"); !available {
			return true
		}
	}
	if p != nil && p.hideMode() && !s.sessionAllows(p, name) {
		return true
	}
	if s.editingToolsHidden(ctx) && daemon.IsMutating(name) {
		return true
	}
	return s.sessionHostContext(ctx).excluded[name]
}

// sessionToolStatus classifies name against the effective surface for ctx,
// rather than the process-global registration split.
func (s *Server) sessionToolStatus(ctx context.Context, name string) string {
	if !slices.Contains(s.registeredToolNames(), name) {
		return "absent"
	}
	if slices.Contains(s.sessionLiveToolNames(ctx), name) {
		return "live"
	}
	if s.sessionToolBlocked(ctx, s.effectiveSessionPolicy(ctx), name) {
		return "blocked"
	}
	return "deferred"
}

// IsToolEnabledForSession reports whether name is callable in ctx's effective
// surface. Unlike the legacy process-global IsToolEnabled helper, this honors
// client/preset negotiation plus planning, workflow, and host restrictions.
// The daemon dispatcher consults it before promote-on-demand so a rejected
// hidden call cannot mutate the shared lazy registry first.
func (s *Server) IsToolEnabledForSession(ctx context.Context, name string) bool {
	status := s.sessionToolStatus(ctx, name)
	return status == "live" || status == "deferred"
}

// registerToolProfileTool wires the `tool_profile` introspection tool.
func (s *Server) registerToolProfileTool() {
	s.addTool(
		mcp.NewTool("tool_profile",
			mcp.WithDescription("Report the effective MCP tool profile for this session so the agent knows what is actually available instead of guessing. With no arguments: returns `{lazy_enabled, total, live_count, deferred_count, blocked_count, live[], deferred[], blocked[], scopes{}, categories{}}` plus `preset` / `preset_mode` when a tool preset narrows the surface — `live` exactly matches this session's current tools/list, `deferred` tools remain callable on demand, and `blocked` tools are prohibited by this session's hide/planning/workflow/host policy. With `tool:\"<name>\"`: returns `{tool, enabled, status, scope, category}` for that one tool (status ∈ live | deferred | blocked | absent)."),
			mcp.WithString("tool", mcp.Description("Optional — report only this tool's enabled status and scope instead of the whole profile.")),
		),
		s.handleToolProfile,
	)
}

func (s *Server) handleToolProfile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	scopes := s.toolScopes.snapshot()

	// Per-tool advisory mode.
	if name, _ := args["tool"].(string); name != "" {
		status := s.sessionToolStatus(ctx, name)
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"tool":     name,
			"enabled":  status == "live" || status == "deferred",
			"status":   status,
			"scope":    scopes[name],
			"category": toolCategory(name),
		})
	}

	// Full-profile mode.
	p := s.effectiveSessionPolicy(ctx)
	live := s.sessionLiveToolNames(ctx)
	liveSet := make(map[string]bool, len(live))
	for _, name := range live {
		liveSet[name] = true
	}
	var deferred, blocked []string
	for _, name := range s.registeredToolNames() {
		if liveSet[name] {
			continue
		}
		if s.sessionToolBlocked(ctx, p, name) {
			blocked = append(blocked, name)
			continue
		}
		deferred = append(deferred, name)
	}
	lazyEnabled := false
	if s.lazy != nil {
		lazyEnabled = s.lazy.Enabled()
	}
	profile := map[string]any{
		"lazy_enabled":   lazyEnabled,
		"total":          len(live) + len(deferred),
		"live_count":     len(live),
		"deferred_count": len(deferred),
		"blocked_count":  len(blocked),
		"live":           live,
		"deferred":       deferred,
		"blocked":        blocked,
		"scopes":         scopes,
		"categories":     toolCategories(append(append([]string{}, live...), deferred...)),
		// Per-tool metadata catalog (category / mutating / presets /
		// summary) for every registered tool — the CLI consumes this over
		// the socket instead of re-deriving each tool's classification.
		"descriptors": s.ToolDescriptors(),
	}
	// Effective tool preset for THIS session (forwarded mcp.tools /
	// GORTEX_TOOLS, client-aware default, then daemon global): report the
	// name and mode so an agent knows its surface was deliberately narrowed
	// rather than the daemon mis-registering tools.
	if p != nil && p.isActive() {
		profile["preset"] = p.preset
		profile["preset_mode"] = p.mode
	}
	// Per-host runtime context. Guidance is emitted at initialize time from the
	// effective tool policy; tool_profile may describe a legacy surface and
	// therefore must not inject compact-only names.
	if hc := s.sessionHostContext(ctx); hc.name != "" {
		profile["host"] = hc.name
	}
	return s.respondJSONOrTOON(ctx, req, profile)
}
