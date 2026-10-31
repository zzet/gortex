package daemon

// MCPDispatchClass is the admission lane of an effective tool call. It does
// not replace ToolEffects or any authorization/mode policy.
type MCPDispatchClass uint8

const (
	MCPDispatchFast MCPDispatchClass = iota
	MCPDispatchMutating
	MCPDispatchSlow
)

func (c MCPDispatchClass) String() string {
	switch c {
	case MCPDispatchMutating:
		return "mutating"
	case MCPDispatchSlow:
		return "slow"
	default:
		return "fast"
	}
}

// toolDispatchClasses is the complete local tool catalogue. New registrations
// must make an explicit cost/admission decision here; catalogue tests cover
// live, deferred and facade implementations. Facade calls are lowered through
// their existing operation registry before this table is consulted.
//
// Incidental read caches/telemetry do not turn a read into a mutation. Legacy
// analyze is deliberately slow for every kind, including its conditional
// durable kinds; the effect-safe workspace_admin facade is mutating instead.
// Conditional legacy change_contract acknowledgements and co-change refreshes
// persist state, while their read facades fix those write switches off.
var toolDispatchClasses = map[string]MCPDispatchClass{

	"apply_code_action":        MCPDispatchMutating,
	"batch_edit":               MCPDispatchMutating,
	"change_contract":          MCPDispatchMutating,
	"delete_scope":             MCPDispatchMutating,
	"edit":                     MCPDispatchMutating,
	"edit_file":                MCPDispatchMutating,
	"edit_memory":              MCPDispatchMutating,
	"edit_symbol":              MCPDispatchMutating,
	"enrich_churn":             MCPDispatchMutating,
	"enrich_releases":          MCPDispatchMutating,
	"export_graph":             MCPDispatchMutating,
	"feedback":                 MCPDispatchMutating,
	"find_co_changing_symbols": MCPDispatchMutating,
	"fix_all_in_file":          MCPDispatchMutating,
	"forget_checkout":          MCPDispatchMutating,
	"generate_docs":            MCPDispatchMutating,
	"generate_skill":           MCPDispatchMutating,
	"generate_wiki":            MCPDispatchMutating,
	"index_repository":         MCPDispatchMutating,
	"inline_symbol":            MCPDispatchMutating,
	"move_symbol":              MCPDispatchMutating,
	"notebook_save":            MCPDispatchMutating,
	"notebook_used":            MCPDispatchMutating,
	"overlay_merge":            MCPDispatchMutating,
	"post_review":              MCPDispatchMutating,
	"publish_review":           MCPDispatchMutating,
	"reconcile_checkouts":      MCPDispatchMutating,
	"refactor":                 MCPDispatchMutating,
	"reindex_repository":       MCPDispatchMutating,
	"remember":                 MCPDispatchMutating,
	"rename_memory":            MCPDispatchMutating,
	"rename_symbol":            MCPDispatchMutating,
	"safe_delete_symbol":       MCPDispatchMutating,
	"save_note":                MCPDispatchMutating,
	"save_scope":               MCPDispatchMutating,
	"scaffold":                 MCPDispatchMutating,
	"set_active_project":       MCPDispatchMutating,
	"set_primary_checkout":     MCPDispatchMutating,
	"store_memory":             MCPDispatchMutating,
	"suppress_finding":         MCPDispatchMutating,
	"surface_memories":         MCPDispatchMutating,
	"track_repository":         MCPDispatchMutating,
	"untrack_repository":       MCPDispatchMutating,
	"workspace_admin":          MCPDispatchMutating,
	"write_file":               MCPDispatchMutating,

	"analyze":                    MCPDispatchSlow,
	"audit_agent_config":         MCPDispatchSlow,
	"audit_health":               MCPDispatchSlow,
	"contracts":                  MCPDispatchSlow,
	"dead_code":                  MCPDispatchSlow,
	"find_clones":                MCPDispatchSlow,
	"get_architecture":           MCPDispatchSlow,
	"get_churn_rate":             MCPDispatchSlow,
	"get_communities":            MCPDispatchSlow,
	"get_coupling_metrics":       MCPDispatchSlow,
	"get_extraction_candidates":  MCPDispatchSlow,
	"get_knowledge_gaps":         MCPDispatchSlow,
	"get_processes":              MCPDispatchSlow,
	"get_recent_changes":         MCPDispatchSlow,
	"get_repo_outline":           MCPDispatchSlow,
	"get_surprising_connections": MCPDispatchSlow,
	"get_untested_symbols":       MCPDispatchSlow,
	"lint_file":                  MCPDispatchSlow,
	"list_inspections":           MCPDispatchSlow,
	"outline":                    MCPDispatchSlow,
	"replay_episode":             MCPDispatchSlow,
	"run_inspections":            MCPDispatchSlow,
	"verify_citation":            MCPDispatchSlow,
	"why":                        MCPDispatchSlow,

	"agent_registry":                  MCPDispatchFast,
	"api_impact":                      MCPDispatchFast,
	"ask":                             MCPDispatchFast,
	"batch_symbols":                   MCPDispatchFast,
	"capabilities":                    MCPDispatchFast,
	"change":                          MCPDispatchFast,
	"check_guards":                    MCPDispatchFast,
	"check_onboarding_performed":      MCPDispatchFast,
	"check_references":                MCPDispatchFast,
	"compare_branches":                MCPDispatchFast,
	"compare_with_overlay":            MCPDispatchFast,
	"conflicts_prs":                   MCPDispatchFast,
	"context_closure":                 MCPDispatchFast,
	"critique_review":                 MCPDispatchFast,
	"ctx_grep":                        MCPDispatchFast,
	"ctx_peek":                        MCPDispatchFast,
	"ctx_slice":                       MCPDispatchFast,
	"ctx_stats":                       MCPDispatchFast,
	"detect_changes":                  MCPDispatchFast,
	"diff_context":                    MCPDispatchFast,
	"distill_session":                 MCPDispatchFast,
	"explain_change_impact":           MCPDispatchFast,
	"explain_view":                    MCPDispatchFast,
	"explore":                         MCPDispatchFast,
	"export_context":                  MCPDispatchFast,
	"find_declaration":                MCPDispatchFast,
	"find_files":                      MCPDispatchFast,
	"find_implementations":            MCPDispatchFast,
	"find_import_path":                MCPDispatchFast,
	"find_overrides":                  MCPDispatchFast,
	"find_usages":                     MCPDispatchFast,
	"flow_between":                    MCPDispatchFast,
	"get_active_project":              MCPDispatchFast,
	"get_artifact":                    MCPDispatchFast,
	"get_call_chain":                  MCPDispatchFast,
	"get_callers":                     MCPDispatchFast,
	"get_cfg":                         MCPDispatchFast,
	"get_class_hierarchy":             MCPDispatchFast,
	"get_cluster":                     MCPDispatchFast,
	"get_code_actions":                MCPDispatchFast,
	"get_dependencies":                MCPDispatchFast,
	"get_dependents":                  MCPDispatchFast,
	"get_diagnostics":                 MCPDispatchFast,
	"get_edit_plan":                   MCPDispatchFast,
	"get_editing_context":             MCPDispatchFast,
	"get_file_summary":                MCPDispatchFast,
	"get_pr_impact":                   MCPDispatchFast,
	"get_symbol":                      MCPDispatchFast,
	"get_symbol_history":              MCPDispatchFast,
	"get_symbol_source":               MCPDispatchFast,
	"get_test_targets":                MCPDispatchFast,
	"gortex_wakeup":                   MCPDispatchFast,
	"graph_completion_search":         MCPDispatchFast,
	"graph_query":                     MCPDispatchFast,
	"graph_stats":                     MCPDispatchFast,
	"grep_results":                    MCPDispatchFast,
	"head_results":                    MCPDispatchFast,
	"index_health":                    MCPDispatchFast,
	"list_checkouts":                  MCPDispatchFast,
	"list_prs":                        MCPDispatchFast,
	"list_repos":                      MCPDispatchFast,
	"list_scopes":                     MCPDispatchFast,
	"mutation_status":                 MCPDispatchFast,
	"nav":                             MCPDispatchFast,
	"notebook_find":                   MCPDispatchFast,
	"notebook_list":                   MCPDispatchFast,
	"notebook_show":                   MCPDispatchFast,
	"overlay":                         MCPDispatchFast,
	"overlay_branches":                MCPDispatchFast,
	"overlay_delete":                  MCPDispatchFast,
	"overlay_drop":                    MCPDispatchFast,
	"overlay_drop_branch":             MCPDispatchFast,
	"overlay_fork":                    MCPDispatchFast,
	"overlay_keepalive":               MCPDispatchFast,
	"overlay_list":                    MCPDispatchFast,
	"overlay_push":                    MCPDispatchFast,
	"overlay_register":                MCPDispatchFast,
	"overlay_switch":                  MCPDispatchFast,
	"plan_turn":                       MCPDispatchFast,
	"pr":                              MCPDispatchFast,
	"pr_review_context":               MCPDispatchFast,
	"pr_risk":                         MCPDispatchFast,
	"prefetch_context":                MCPDispatchFast,
	"preview_edit":                    MCPDispatchFast,
	"proxy_disable":                   MCPDispatchFast,
	"proxy_enable":                    MCPDispatchFast,
	"proxy_status":                    MCPDispatchFast,
	"query_memories":                  MCPDispatchFast,
	"query_notes":                     MCPDispatchFast,
	"query_project":                   MCPDispatchFast,
	"read":                            MCPDispatchFast,
	"read_file":                       MCPDispatchFast,
	"recall":                          MCPDispatchFast,
	"relations":                       MCPDispatchFast,
	"response":                        MCPDispatchFast,
	"review":                          MCPDispatchFast,
	"review_pack":                     MCPDispatchFast,
	"search":                          MCPDispatchFast,
	"search_artifacts":                MCPDispatchFast,
	"search_ast":                      MCPDispatchFast,
	"search_symbols":                  MCPDispatchFast,
	"search_text":                     MCPDispatchFast,
	"session":                         MCPDispatchFast,
	"set_planning_mode":               MCPDispatchFast,
	"sibling_diff_context":            MCPDispatchFast,
	"simulate_chain":                  MCPDispatchFast,
	"smart_context":                   MCPDispatchFast,
	"subscribe_daemon_health":         MCPDispatchFast,
	"subscribe_diagnostics":           MCPDispatchFast,
	"subscribe_graph_invalidated":     MCPDispatchFast,
	"subscribe_stale_refs":            MCPDispatchFast,
	"subscribe_workspace_readiness":   MCPDispatchFast,
	"suggest_pattern":                 MCPDispatchFast,
	"suggest_queries":                 MCPDispatchFast,
	"suggest_reviewers":               MCPDispatchFast,
	"suggested_review_questions":      MCPDispatchFast,
	"symbols_for_ranges":              MCPDispatchFast,
	"taint_paths":                     MCPDispatchFast,
	"tool_profile":                    MCPDispatchFast,
	"tools_search":                    MCPDispatchFast,
	"trace":                           MCPDispatchFast,
	"trace_path":                      MCPDispatchFast,
	"triage_prs":                      MCPDispatchFast,
	"unsubscribe_daemon_health":       MCPDispatchFast,
	"unsubscribe_diagnostics":         MCPDispatchFast,
	"unsubscribe_graph_invalidated":   MCPDispatchFast,
	"unsubscribe_stale_refs":          MCPDispatchFast,
	"unsubscribe_workspace_readiness": MCPDispatchFast,
	"verify_change":                   MCPDispatchFast,
	"walk_graph":                      MCPDispatchFast,
	"winnow_symbols":                  MCPDispatchFast,
	"workflow":                        MCPDispatchFast,
	"workspace":                       MCPDispatchFast,
	"workspace_info":                  MCPDispatchFast,
}

// ToolDispatchClass returns the explicit lane and whether the tool is known.
// Unknown tool names stay fast so normal tool validation supplies the error;
// they are not allowed to acquire a privileged mutation reservation.
func ToolDispatchClass(name string) (MCPDispatchClass, bool) {
	class, known := toolDispatchClasses[name]
	return class, known
}

// MCPDispatchClassifier lowers a tools/call to its effective admission class
// without executing the handler or changing session state. Implementations
// reuse the product's operation registry for facade calls.
type MCPDispatchClassifier interface {
	DispatchClass(name string, arguments map[string]any) MCPDispatchClass
}
