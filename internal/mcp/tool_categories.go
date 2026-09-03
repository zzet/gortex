package mcp

import "strings"

// Tool categories group the flat tool namespace into coarse functional
// families for tool_profile introspection and prefix-style filtering —
// the "tools grouped for quick filtering" an operator reaches for when
// the 170-tool surface is overwhelming. This is a best-effort label, NOT
// a security boundary: enforcement lives in the tool preset (allow/deny),
// the per-tool scope, and the authoritative daemon.MutatingTools set.
const (
	toolCatNav          = "nav"          // search / navigation / traversal
	toolCatRead         = "read"         // read a file or symbol's source
	toolCatEdit         = "edit"         // mutate source / refactor
	toolCatAnalysis     = "analysis"     // graph analyses / metrics / ask
	toolCatReview       = "review"       // code-review engine
	toolCatPR           = "pr"           // pull-request triage / risk
	toolCatMemory       = "memory"       // notes / memories / notebooks
	toolCatOverlay      = "overlay"      // overlay / speculative / change-contract
	toolCatSubscription = "subscription" // subscribe / unsubscribe pairs
	toolCatEnrich       = "enrich"       // churn / coverage / release enrichment
	toolCatWorkspace    = "workspace"    // repo / project / scope management
	toolCatAdmin        = "admin"        // index / health / introspection
	toolCatOther        = "other"        // unclassified
)

// toolCategoryOverrides pins the category for tools whose name prefix
// does not reveal (or actively misleads about) their family — e.g.
// edit_memory starts with "edit_" but belongs to the memory family.
var toolCategoryOverrides = map[string]string{
	// facade-v1 dispatchers
	"explore": toolCatNav, "search": toolCatNav, "relations": toolCatNav, "trace": toolCatNav,
	"read": toolCatRead, "edit": toolCatEdit, "refactor": toolCatEdit,
	"change": toolCatOverlay, "overlay": toolCatOverlay,
	"publish_review": toolCatReview, "pr": toolCatPR,
	"recall": toolCatMemory, "remember": toolCatMemory,
	"workspace": toolCatWorkspace, "workspace_admin": toolCatWorkspace,
	"session": toolCatAdmin, "capabilities": toolCatAdmin, "response": toolCatOther,
	// nav (no find_/search_ prefix)
	"smart_context": toolCatNav, "nav": toolCatNav, "walk_graph": toolCatNav,
	"graph_query": toolCatNav, "trace_path": toolCatNav, "flow_between": toolCatNav,
	"taint_paths": toolCatNav, "context_closure": toolCatNav, "get_repo_outline": toolCatNav,
	"get_callers": toolCatNav, "get_call_chain": toolCatNav, "get_dependencies": toolCatNav,
	"get_dependents": toolCatNav, "get_class_hierarchy": toolCatNav, "winnow_symbols": toolCatNav,
	"get_cluster": toolCatNav, "suggest_queries": toolCatNav, "graph_completion_search": toolCatNav,
	// read
	"read_file": toolCatRead, "get_symbol": toolCatRead, "get_symbol_source": toolCatRead,
	"get_file_summary": toolCatRead, "get_editing_context": toolCatRead, "get_cfg": toolCatRead,
	"get_symbol_history": toolCatRead, "batch_symbols": toolCatRead,
	// edit (no edit_/write_ prefix)
	"scaffold": toolCatEdit, "suggest_pattern": toolCatEdit, "apply_code_action": toolCatEdit,
	"fix_all_in_file": toolCatEdit, "get_code_actions": toolCatEdit, "get_edit_plan": toolCatEdit,
	// analysis
	"analyze": toolCatAnalysis, "contracts": toolCatAnalysis, "ask": toolCatAnalysis,
	"get_communities": toolCatAnalysis, "get_coupling_metrics": toolCatAnalysis,
	"get_processes": toolCatAnalysis, "get_architecture": toolCatAnalysis,
	"get_surprising_connections": toolCatAnalysis, "get_knowledge_gaps": toolCatAnalysis,
	"get_extraction_candidates": toolCatAnalysis, "get_churn_rate": toolCatAnalysis,
	"get_untested_symbols": toolCatAnalysis, "get_recent_changes": toolCatAnalysis,
	"get_coupling": toolCatAnalysis, "find_clones": toolCatAnalysis,
	"find_co_changing_symbols": toolCatAnalysis,
	// review
	"review": toolCatReview, "review_pack": toolCatReview, "post_review": toolCatReview,
	"critique_review": toolCatReview, "suppress_finding": toolCatReview,
	"sibling_diff_context": toolCatReview, "diff_context": toolCatReview,
	"suggested_review_questions": toolCatReview, "pr_review_context": toolCatReview,
	// pr
	"pr_risk": toolCatPR, "list_prs": toolCatPR, "get_pr_impact": toolCatPR,
	"triage_prs": toolCatPR, "conflicts_prs": toolCatPR, "suggest_reviewers": toolCatPR,
	// memory (some carry edit_/rename_ prefixes)
	"save_note": toolCatMemory, "query_notes": toolCatMemory, "distill_session": toolCatMemory,
	"store_memory": toolCatMemory, "query_memories": toolCatMemory, "surface_memories": toolCatMemory,
	"edit_memory": toolCatMemory, "rename_memory": toolCatMemory,
	// overlay / speculative / change-contract
	"preview_edit": toolCatOverlay, "simulate_chain": toolCatOverlay,
	"compare_with_overlay": toolCatOverlay, "change_contract": toolCatOverlay,
	"symbols_for_ranges": toolCatOverlay, "verify_change": toolCatOverlay,
	"explain_change_impact": toolCatOverlay, "detect_changes": toolCatOverlay,
	// workspace
	"list_repos": toolCatWorkspace, "workspace_info": toolCatWorkspace,
	"get_active_project": toolCatWorkspace, "set_active_project": toolCatWorkspace,
	"save_scope": toolCatWorkspace, "delete_scope": toolCatWorkspace, "list_scopes": toolCatWorkspace,
	"track_repository": toolCatWorkspace, "untrack_repository": toolCatWorkspace,
	"list_checkouts": toolCatWorkspace, "set_primary_checkout": toolCatWorkspace,
	"forget_checkout": toolCatWorkspace,
	// admin / introspection
	"graph_stats": toolCatAdmin, "index_health": toolCatAdmin, "index_repository": toolCatAdmin,
	"reindex_repository": toolCatAdmin, "tool_profile": toolCatAdmin, "feedback": toolCatAdmin,
	"get_diagnostics": toolCatAdmin, "plan_turn": toolCatAdmin, "set_planning_mode": toolCatAdmin,
	"prefetch_context": toolCatAdmin, "workflow": toolCatAdmin, "check_guards": toolCatAdmin,
	"reconcile_checkouts": toolCatAdmin, "explain_view": toolCatAdmin,
	"get_test_targets": toolCatAdmin, LazyToolsSearchName: toolCatAdmin,
}

// toolCategory classifies a tool name into a functional family. An
// explicit override wins; otherwise a handful of unambiguous name
// prefixes decide; anything else is "other".
func toolCategory(name string) string {
	if c, ok := toolCategoryOverrides[name]; ok {
		return c
	}
	switch {
	case strings.HasPrefix(name, "overlay_"):
		return toolCatOverlay
	case strings.HasPrefix(name, "subscribe_"), strings.HasPrefix(name, "unsubscribe_"):
		return toolCatSubscription
	case strings.HasPrefix(name, "enrich_"):
		return toolCatEnrich
	case strings.HasPrefix(name, "notebook_"):
		return toolCatMemory
	case strings.HasPrefix(name, "find_"), strings.HasPrefix(name, "search_"):
		return toolCatNav
	case strings.HasPrefix(name, "edit_"), strings.HasPrefix(name, "write_"),
		strings.HasPrefix(name, "rename_"), strings.HasPrefix(name, "move_"),
		strings.HasPrefix(name, "inline_"), strings.HasPrefix(name, "batch_"),
		strings.HasPrefix(name, "safe_delete"):
		return toolCatEdit
	default:
		return toolCatOther
	}
}

// toolCategories returns a name → category map for the given tool names.
func toolCategories(names []string) map[string]string {
	out := make(map[string]string, len(names))
	for _, n := range names {
		out[n] = toolCategory(n)
	}
	return out
}
