package mcp

// defaultToolScopes is the canonical scope for every Gortex MCP tool
//
// The wire-format slice carries this same map onto the schema (`scope`
// field on each tool definition); this server-side copy lets the
// dispatcher validate `repo` per-call without re-deriving it. Both
// must agree — feature-owner's reconcile step at merge time confirms
// the schema and this map have the same entry for every tool.
//
// Classification rules:
//
//   - scope: repo — tools that operate against ONE project's index.
//     Most per-symbol / per-file query and edit tools fall here.
//   - scope: workspace — tools with no per-repo target; report on or
//     mutate the workspace as a unit. Examples: list_repos,
//     workspace_info, get_active_project, set_active_project.
//   - scope: fan-out — tools that legitimately span several indexes
//     in one call. The current surface has only one such tool today —
//     `audit_agent_config` walks every repo's CLAUDE.md / AGENTS.md /
//     editor configs and reports cross-repo. Cross-cutting analysis
//     tools that produce a workspace-level summary (`analyze`,
//     `contracts`) may ALSO be classified fan-out in a future ADR
//     follow-up; iteration 1 keeps them at scope: repo because their
//     existing per-call `repo` parameter already produces a single
//     focused result and migration to fan-out is a UX change worth a
//     separate review.
//
// Note: `analyze` now accepts the uniform repo / project / workspace /
// scope overrides (resolved via resolveScope, applied as a RepoAllow in
// the scoped-node accessors and the analyzeNodeVisible Tier-2 filter),
// clamped to the session workspace. The narrowing reaches its
// graph-node kinds (dead_code, hotspots, cycles, health_score, todos,
// ownership, impact, …); edge-walk / file-scan / git-mining kinds stay
// workspace-bound but are not repo-narrowed in v1. The ScopeRepo wire-
// schema classification below is orthogonal to that narrowing and is
// unchanged.
var defaultToolScopes = map[string]ToolScope{
	// --- Workspace-shaped tools ----------------------------------
	// "What can I ask about?" — bootstrap surface so an agent can
	// discover member repos before issuing any scope: repo call.
	"list_repos":         ScopeWorkspace,
	"workspace_info":     ScopeWorkspace,
	"get_active_project": ScopeWorkspace,
	"set_active_project": ScopeWorkspace,
	"track_repository":   ScopeWorkspace,
	"untrack_repository": ScopeWorkspace,

	// --- Fan-out tools -------------------------------------------
	// audit_agent_config inherently scans every repo's agent config
	// files; explicit `["*"]` makes that breadth visible in logs.
	"audit_agent_config": ScopeFanOut,

	// --- Per-repo tools ------------------------------------------
	// Index management against one repo.
	"index_repository":   ScopeRepo,
	"reindex_repository": ScopeRepo,

	// Symbol-level query surface.
	"get_symbol":           ScopeRepo,
	"get_symbol_source":    ScopeRepo,
	"get_symbol_history":   ScopeRepo,
	"search_symbols":       ScopeRepo,
	"winnow_symbols":       ScopeRepo,
	"batch_symbols":        ScopeRepo,
	"find_usages":          ScopeRepo,
	"find_implementations": ScopeRepo,
	"find_overrides":       ScopeRepo,
	"get_class_hierarchy":  ScopeRepo,
	"get_diagnostics":      ScopeRepo,
	"get_code_actions":     ScopeRepo,
	"apply_code_action":    ScopeRepo,
	"fix_all_in_file":      ScopeRepo,
	"find_import_path":     ScopeRepo,
	"get_call_chain":       ScopeRepo,
	"get_callers":          ScopeRepo,
	"get_dependencies":     ScopeRepo,
	"get_dependents":       ScopeRepo,
	"get_cluster":          ScopeRepo,
	"get_communities":      ScopeRepo,
	"get_processes":        ScopeRepo,
	"get_recent_changes":   ScopeRepo,
	"get_untested_symbols": ScopeRepo,
	"suggest_queries":      ScopeRepo,
	"search_text":          ScopeRepo,
	"find_files":           ScopeRepo,

	// File / repo-level summaries.
	"get_file_summary":    ScopeRepo,
	"get_editing_context": ScopeRepo,
	"get_repo_outline":    ScopeRepo,
	"graph_stats":         ScopeRepo,
	"index_health":        ScopeRepo,

	// Editing / code generation.
	"edit_symbol":     ScopeRepo,
	"edit_file":       ScopeRepo,
	"write_file":      ScopeRepo,
	"mutation_status": ScopeRepo,
	"rename_symbol":   ScopeRepo,
	"scaffold":        ScopeRepo,
	"suggest_pattern": ScopeRepo,
	"batch_edit":      ScopeRepo,
	"move_symbol":     ScopeRepo,
	"inline_symbol":   ScopeRepo,

	// Planning / impact / change support.
	"smart_context":         ScopeRepo,
	"prefetch_context":      ScopeRepo,
	"export_context":        ScopeRepo,
	"plan_turn":             ScopeRepo,
	"explain_change_impact": ScopeRepo,
	"verify_change":         ScopeRepo,
	"detect_changes":        ScopeRepo,
	"diff_context":          ScopeRepo,
	"sibling_diff_context":  ScopeRepo,
	"get_edit_plan":         ScopeRepo,
	"get_test_targets":      ScopeRepo,
	"check_guards":          ScopeRepo,
	"feedback":              ScopeRepo,

	// Analysis. Historically takes an optional `repo`; classified
	// scope: repo to keep behavior unchanged. A follow-up may upgrade
	// to fan-out once the UX is reviewed.
	"analyze":         ScopeRepo,
	"contracts":       ScopeRepo,
	"run_inspections": ScopeRepo,

	// CPG-lite dataflow primitives. Both walk the active graph by
	// node ID / name pattern; like analyze + contracts, they take
	// per-call scope hints today and can upgrade to fan-out once
	// cross-repo dataflow joins land.
	"flow_between": ScopeRepo,
	"taint_paths":  ScopeRepo,
	"trace_path":   ScopeRepo,

	// Per-function control-flow graphs: built from one symbol's
	// source in the active repo.
	"get_cfg": ScopeRepo,
}

var toolIntent = map[string]ToolIntent{
	// Locate: find definitions/files/text near the session's home repo by default.
	"search_symbols": IntentLocate,
	"search_text":    IntentLocate,
	"find_files":     IntentLocate,

	// Reach: traversal/call-graph tools need the workspace to answer cross-repo use.
	"find_usages":          IntentReach,
	"get_callers":          IntentReach,
	"get_call_chain":       IntentReach,
	"get_dependencies":     IntentReach,
	"get_dependents":       IntentReach,
	"find_implementations": IntentReach,
	"find_overrides":       IntentReach,
	"get_class_hierarchy":  IntentReach,
	"get_cluster":          IntentReach,
	"walk_graph":           IntentReach,
	"context_closure":      IntentReach,
	"contracts":            IntentReach,

	// Analyze: rollups stay at workspace breadth unless explicitly narrowed.
	"analyze":             IntentAnalyze,
	"review":              IntentAnalyze,
	"sast":                IntentAnalyze,
	"hotspots":            IntentAnalyze,
	"dead_code":           IntentAnalyze,
	"cycles":              IntentAnalyze,
	"health_score":        IntentAnalyze,
	"impact":              IntentAnalyze,
	"connectivity_health": IntentAnalyze,
}

func toolIntentForName(name string) ToolIntent {
	if intent, ok := toolIntent[name]; ok {
		return intent
	}
	return IntentAnalyze
}

// applyDefaultToolScopes registers the canonical scope for every
// known tool on this Server. Called once during NewServer after the
// register*Tools sweep so the registry mirrors the registered MCP
// tool surface.
//
// Tools that aren't in the map are simply not registered — the
// dispatcher treats them as unscoped (legacy single-repo behavior),
// which preserves byte-for-byte backwards compatibility for any
// future tool added before its scope is decided.
func (s *Server) applyDefaultToolScopes() {
	for name, scope := range defaultToolScopes {
		s.RegisterToolScope(name, scope)
	}
}
