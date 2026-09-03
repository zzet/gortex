package daemon

import "sort"

// ToolEffect describes externally relevant state changes a tool may make.
// It is a bitmask because a single tool can cross more than one boundary
// (for example overlay_merge changes session state and can write files).
//
// EffectNone is intentionally the zero value: tools absent from ToolEffects
// are read-only from the permission system's point of view. Incidental cache
// touches and read telemetry do not count as effects here.
type ToolEffect uint8

const EffectNone ToolEffect = 0

const (
	// EffectFilesystemWrite covers working-tree writes and other durable files
	// such as notes, memories, generated exports, and notebook metadata.
	EffectFilesystemWrite ToolEffect = 1 << iota
	// EffectGraphWrite covers index or enrichment changes to the graph store.
	EffectGraphWrite
	// EffectConfigWrite covers durable daemon, project, scope, and review policy.
	EffectConfigWrite
	// EffectSessionWrite covers volatile state scoped to a connection/session.
	// It is observable but deliberately does not trigger the read-only gate: a
	// planning session must still be able to switch mode, stop a workflow, and
	// manage subscriptions or speculative overlays.
	EffectSessionWrite
	// EffectExternalWrite covers mutations outside the local Gortex process,
	// such as posting review comments to a forge.
	EffectExternalWrite
)

const durableToolEffects = EffectFilesystemWrite | EffectGraphWrite | EffectConfigWrite | EffectExternalWrite

// ToolEffects is the canonical effect registry for state-changing MCP tools.
// Permission gates, federation, tool descriptors, and facade adapters should
// consult this registry instead of maintaining verb/prefix-based denylists.
//
// Classification is conservative at tool-name granularity: if any supported
// operation or argument shape can write, the tool carries that write effect.
// Keep this map immutable after package initialization; MutatingTools is
// derived from it for compatibility with existing callers.
var ToolEffects = map[string]ToolEffect{
	// Source and filesystem mutation.
	"apply_code_action":  EffectFilesystemWrite,
	"batch_edit":         EffectFilesystemWrite,
	"edit_file":          EffectFilesystemWrite,
	"edit_symbol":        EffectFilesystemWrite,
	"fix_all_in_file":    EffectFilesystemWrite,
	"inline_symbol":      EffectFilesystemWrite,
	"move_symbol":        EffectFilesystemWrite,
	"rename_symbol":      EffectFilesystemWrite,
	"safe_delete_symbol": EffectFilesystemWrite,
	"scaffold":           EffectFilesystemWrite,
	"write_file":         EffectFilesystemWrite,

	// Conditional output writers are write-classified for the whole tool.
	"export_graph":   EffectFilesystemWrite,
	"generate_docs":  EffectFilesystemWrite,
	"generate_skill": EffectFilesystemWrite,
	"generate_wiki":  EffectFilesystemWrite,

	// Durable agent knowledge and learning stores.
	"edit_memory":      EffectFilesystemWrite,
	"feedback":         EffectFilesystemWrite,
	"notebook_save":    EffectFilesystemWrite,
	"notebook_used":    EffectFilesystemWrite,
	"rename_memory":    EffectFilesystemWrite,
	"save_note":        EffectFilesystemWrite,
	"store_memory":     EffectFilesystemWrite,
	"surface_memories": EffectFilesystemWrite,
	"suppress_finding": EffectConfigWrite,

	// Graph/index mutation.
	"enrich_churn":       EffectGraphWrite,
	"enrich_releases":    EffectGraphWrite,
	"index_repository":   EffectGraphWrite,
	"reindex_repository": EffectGraphWrite,

	// Repository/project/scope lifecycle mutates both configuration and, for
	// tracking, the indexed graph materialized from that configuration.
	"delete_scope":       EffectConfigWrite,
	"save_scope":         EffectConfigWrite,
	"set_active_project": EffectConfigWrite | EffectSessionWrite,
	"track_repository":   EffectConfigWrite | EffectGraphWrite,
	"untrack_repository": EffectConfigWrite | EffectGraphWrite,

	// Checkout administration. The confirm argument is what separates a
	// preview from a transaction, and the classification is per tool name, so
	// all three carry the write effect their confirmed call has.
	"forget_checkout":      EffectConfigWrite | EffectGraphWrite,
	"set_primary_checkout": EffectConfigWrite | EffectGraphWrite,
	"reconcile_checkouts":  EffectConfigWrite | EffectGraphWrite,

	// Review publication mutates a remote forge. dry_run does not weaken the
	// tool-level classification because ordinary calls can still post.
	"post_review": EffectExternalWrite,

	// overlay_merge normally changes session state and can also apply the
	// branch to disk when to_disk=true.
	"overlay_merge": EffectSessionWrite | EffectFilesystemWrite,

	// Intentional legacy exception: the unified analyze tool contains durable
	// graph enrichers (blame, coverage, sql_rebuild, and temporal_verify).
	// Marking the legacy tool at name granularity would hide every read-only
	// analysis in planning mode. The compact public surface rejects those kinds
	// on analyze and routes them through workspace_admin. Remove this exception
	// when the legacy dispatcher is retired.
	//
	// change_contract has a similar conditional legacy write (ack=true). The
	// compact change.contract adapter fixes ack=false and exposes durable risk
	// acknowledgement as remember.risk_ack.

	// Volatile MCP/session controls. These are effectful for introspection but
	// remain available in planning/read-only mode (see durableToolEffects).
	"agent_registry":                  EffectSessionWrite,
	"nav":                             EffectSessionWrite,
	"overlay_delete":                  EffectSessionWrite,
	"overlay_drop":                    EffectSessionWrite,
	"overlay_drop_branch":             EffectSessionWrite,
	"overlay_fork":                    EffectSessionWrite,
	"overlay_keepalive":               EffectSessionWrite,
	"overlay_push":                    EffectSessionWrite,
	"overlay_register":                EffectSessionWrite,
	"overlay_switch":                  EffectSessionWrite,
	"proxy_disable":                   EffectSessionWrite,
	"proxy_enable":                    EffectSessionWrite,
	"set_planning_mode":               EffectSessionWrite,
	"simulate_chain":                  EffectSessionWrite,
	"subscribe_daemon_health":         EffectSessionWrite,
	"subscribe_diagnostics":           EffectSessionWrite,
	"subscribe_graph_invalidated":     EffectSessionWrite,
	"subscribe_stale_refs":            EffectSessionWrite,
	"subscribe_workspace_readiness":   EffectSessionWrite,
	"unsubscribe_daemon_health":       EffectSessionWrite,
	"unsubscribe_diagnostics":         EffectSessionWrite,
	"unsubscribe_graph_invalidated":   EffectSessionWrite,
	"unsubscribe_stale_refs":          EffectSessionWrite,
	"unsubscribe_workspace_readiness": EffectSessionWrite,
	"tools_search":                    EffectSessionWrite,
	"workflow":                        EffectSessionWrite,

	// MCP facade-v1 effect boundaries. Facades intentionally remain
	// homogeneous so clients can authorize them by tool name.
	"edit":            EffectFilesystemWrite | EffectSessionWrite,
	"refactor":        EffectFilesystemWrite,
	"remember":        EffectFilesystemWrite | EffectConfigWrite,
	"workspace_admin": EffectFilesystemWrite | EffectGraphWrite | EffectConfigWrite | EffectSessionWrite,
	"publish_review":  EffectExternalWrite,
	"overlay":         EffectSessionWrite,
	"session":         EffectSessionWrite,
}

// MutatingTools is the compatibility set used by the planning-mode and
// federation write gates. It contains durable and external writes, but not
// session-only effects. New code should prefer EffectOf when it needs the
// finer distinction.
var MutatingTools = buildMutatingTools(ToolEffects)

func buildMutatingTools(effects map[string]ToolEffect) map[string]bool {
	mutating := make(map[string]bool)
	for name, effect := range effects {
		if effect&durableToolEffects != 0 {
			mutating[name] = true
		}
	}
	return mutating
}

// EffectOf reports all known effects for a tool. An unclassified tool is
// read-only from the permission system's point of view.
func EffectOf(name string) ToolEffect { return ToolEffects[name] }

// HasEffect reports whether a tool carries every bit in effect.
func HasEffect(name string, effect ToolEffect) bool {
	return effect != EffectNone && EffectOf(name)&effect == effect
}

// IsEffectful reports whether a tool changes durable, external, or session
// state. It is intended for introspection, not the planning-mode write gate.
func IsEffectful(name string) bool { return EffectOf(name) != EffectNone }

// IsMutating reports whether a tool can mutate durable local state or an
// external system. Session-only controls deliberately return false.
func IsMutating(name string) bool { return EffectOf(name)&durableToolEffects != 0 }

// SortedMutatingTools returns the canonical mutating-tool names in stable
// order — used where a deterministic list is surfaced to a client.
func SortedMutatingTools() []string {
	out := make([]string, 0, len(MutatingTools))
	for n := range MutatingTools {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// SortedEffectfulTools returns every classified effectful tool in stable
// order, including session-only tools omitted from SortedMutatingTools.
func SortedEffectfulTools() []string {
	out := make([]string, 0, len(ToolEffects))
	for n := range ToolEffects {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
