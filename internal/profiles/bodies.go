package profiles

import (
	"fmt"
	"strings"
)

// The instruction bodies are composed from the shared sections below —
// no profile hand-authors its own copy of a section. Body text uses §
// as a backtick placeholder (bt renders it) so markdown tables stay
// readable inside Go string literals.

// bt renders § as a backtick.
func bt(s string) string { return strings.ReplaceAll(s, "§", "`") }

// sectionHeader renders the mandatory-rule opener every profile
// keeps: the MUST-prefer statement and the deny-hook warning are the
// two positioning cues that buy tool adoption, so they survive even
// the lean rendering — lean only condenses the prose around them. The
// heading doubles as the idempotency sentinel other adapters check
// (agents.InstructionsSentinel).
func sectionHeader(lean bool) string {
	if lean {
		return bt(`## MANDATORY: Use Gortex MCP tools instead of Read/Grep/Glob

A machine-wide §gortex§ MCP server indexes this code. You MUST prefer graph queries over file reads. Hook posture is configurable; follow every Gortex hook instruction even when raw file tools remain callable.

`)
	}
	return bt(`## MANDATORY: Use Gortex MCP tools instead of Read/Grep/Glob

A Gortex daemon is configured machine-wide via the §gortex§ MCP server. Whenever you operate on indexed source (any repo the daemon tracks — check §gortex daemon status§), you MUST prefer graph queries over file reads. Hook posture is configurable; follow every Gortex hook instruction even when raw file tools remain callable.

`)
}

// sectionFullRuleTable is the classic nine-row instead-of table used
// by the core and full profiles.
// sectionExploreOpener positions the one-shot localization verb as
// the opening move (standard rendering; the lean profile carries its
// own condensed line).
var sectionExploreOpener = bt(`For an explicitly named file to read/review/summarize, on the first direct file read caused by a new user request call §read(operation:"file", target:{file:"<path>"}, options:{new_user_task:true})§; do not start localization. When the file, symbol, or evidence must be discovered, call §explore(operation:"localize")§ and obey §completion.required_action§; after §answer_ready§ answer from §completion.final_response§ and make no further tool calls. For diagnosis or requested changes, call §explore(operation:"task")§, make at most one focused follow-up, then proceed to impact, edit, and test.

`)

// sectionCompactWorkflow is the ambient workflow for the default compact MCP
// surface. It is deliberately directive and names only public tools an agent
// can actually call; exact operation schemas remain on demand.
var sectionCompactWorkflow = bt(`For every coding task:

1. Named-file read/review/summarize: first new-task call §read(operation:"file", target:{file:"<path>"}, options:{new_user_task:true})§; do not localize. When the file, symbol, or evidence must be discovered, call §explore(operation:"localize")§ and obey §completion.required_action§; after §answer_ready§, answer from §completion.final_response§ and stop. For diagnosis or modification, call §explore(operation:"task")§.
2. In a diagnosis/change flow, make at most one follow-up call on one unresolved symbol with §search§, §read§, §relations§, or §trace§, then continue to step 3. Never reopen indexed source with Read/Grep/Glob or shell equivalents.
3. Before mutation, call §change(operation:"impact")§; for a signature change, also call §change(operation:"verify")§ with the proposed signature. Mutate only with §edit§ or §refactor§. After mutation, call §change(operation:"detect")§, then use its symbol IDs with §change(operation:"tests")§, §change(operation:"guards")§, and §change(operation:"contract")§.
4. Call §capabilities§ only when you need the exact fields for an operation.

Common calls: §search(operation:"symbols", query:"<name>")§ · §read(target:{symbol:"<id>"})§ · §relations(operation:"usages", target:{symbol:"<id>"})§.

If the Gortex server is configured but these tools are missing from the callable MCP tools, report a Gortex MCP integration failure and stop. Do not start a daemon or switch to a CLI/shell fallback.

`)

var sectionCompactMemory = bt(`Use §recall§ before revisiting prior work. Call §remember§ immediately for a durable decision, invariant, constraint, or gotcha.

`)

// WorktreeBranchRoutingPolicy is the single agent-facing contract for graph
// ownership, composition, fallback, and lifecycle. Keep this compact: it is
// reused verbatim by profiles, adapter instructions, initialize guidance,
// lifecycle hooks, skills, and sub-agents. Those surfaces must concatenate
// this constant rather than paraphrase it; routing is safety policy, and two
// almost-equivalent copies eventually teach agents different destructive
// actions.
const WorktreeBranchRoutingPolicy = "- The session/CWD selects the view. An implicit or session-discovered checkout (including a linked worktree) is an automatic overlay; do not explicitly `track` it. Only an explicit user request to track creates a dedicated logical graph.\n" +
	"- **Overall** means the selected overlay plus exactly one designated primary in its Git family, never a union of incompatible branches. Unique overlay/base matches keep normal relevance order; for a duplicate logical identity the overlay wins, and deletion/tombstone masks hide the base copy.\n" +
	"- Follow each response's freshness and capability metadata. A building overlay may return a labeled base fallback for eligible graph reads. During removal or availability grace the fallback is sealed primary-only: no dirty state or editor buffers. Exact source/symbol/file reads, AST or filesystem text search, LSP, and edits may refuse when the selected view cannot provide their required capability; never bypass that refusal through another checkout.\n" +
	"- An inactive ref/commit view is an immutable committed structural/source snapshot: it has no working-copy LSP, `search.text`, or edits.\n" +
	"- Explicitly untracking a non-primary dedicated worktree demotes it to automatic only when another ready primary survives. Before primary closure, family forget, or `set-primary`, preview the effects and obtain user confirmation.\n"

// sectionWorktreeViews teaches agents the ownership boundary that keeps
// ordinary linked-worktree use cheap. It is shared by every profile so a lean
// session cannot accidentally turn an automatic overlay into a dedicated graph.
const sectionWorktreeViews = "## Worktree and branch routing\n\n" + WorktreeBranchRoutingPolicy +
	"Never restart the daemon, delete its store, or re-track merely to expose a worktree.\n\n"

var sectionFullRuleTable = bt(`| Instead of...                       | Use...                                   |
|-------------------------------------|------------------------------------------|
| Explicitly named file to read / review / summarize | First new-task read: §read(operation:"file", target:{file:"<path>"}, options:{new_user_task:true})§ |
| Unknown file / symbol / evidence / "where is X" | §explore(operation:"localize")§; obey §completion§ |
| Diagnosing or changing code           | §explore(operation:"task")§, then impact/edit/test |
| §Grep§ / §grep§ / §rg§ for a symbol | §search_symbols§ (BM25 + camelCase-aware) |
| §Grep§ for references               | §find_usages§ (zero false positives)     |
| Reading / grepping to find callers  | §get_callers§ / §get_call_chain§         |
| §Glob§ over source files            | §get_repo_outline§ / §search_symbols§    |
| §Read§ a file for one symbol        | §get_symbol_source§ (§compress_bodies:true§ for the signature only) |
| §Read§ to understand a file         | §get_file_summary§ / §get_editing_context§ |
| §Read§ a non-indexed / raw file     | §read_file§                              |
| §Read§ several symbols' bodies at once | §batch_symbols§ (one call, many bodies) |
| Multiple reads to explore a task    | §smart_context§ (one call)               |
| §Edit§ / §Write§ source             | §edit_file§ / §write_file§ / §edit_symbol§ / §rename_symbol§ / §batch_edit§ |

`)

// sectionMCPRequired keeps the legacy/full MCP profile from silently changing
// transports when a host bridge fails to expose a configured tool.
var sectionMCPRequired = bt(`**Native MCP required:** if a configured Gortex tool is missing from the callable MCP tools, report a Gortex MCP integration failure and stop. Do not start a daemon or use a CLI/shell fallback.

`)

// sectionReadDiscipline is the scope-vs-fidelity paragraph.
var sectionReadDiscipline = bt(`Graph queries *narrow scope*; they do not replace reading the implementation. For the symbol you are about to change or depend on — especially behavior-critical code (migrations, retry / fallback / error-recovery paths, concurrency, compatibility shims) — read the real body with §get_symbol_source§ and do NOT pass §compress_bodies:true§, which elides the branches that carry the risk. §format:"gcx"§ (compact wire, ~27% fewer tokens) and §compress_bodies:true§ (body-eliding) exist on the read / list tools; the parameter legend is in the MCP server instructions.

`)

// sectionMemoryFull is the full memory-workflow section (core / full).
var sectionMemoryFull = bt(`## MANDATORY: Session + development memory

The graph remembers code; these tools remember **why you made a call**. They are behavior-critical — run each at its trigger, not "optionally":

- **Session start / after a compaction** — call §distill_session§ first: prior top symbols, pinned notes, decisions, recent excerpts. Seed your mental model before reading any file.
- **Immediately after §smart_context§** — call §surface_memories task:"<task>" symbol_ids:"<top hits>"§: cross-session invariants / gotchas / decisions anchored to your working set. If it returns nothing, don't probe further.
- **At every decision** — pick an approach, reject an alternative, hit a non-obvious constraint, or commit to an invariant → §save_note tags:"decision" body:"<what+why>"§. Mention symbol IDs (§pkg/foo.go::Bar§) in the body for auto-linking; §pinned:true§ for load-bearing notes.
- **When you learn a durable fact worth teaching the team** — §store_memory kind:"<invariant|gotcha|convention|decision|constraint|incident>" body:"<what+why>" symbol_ids:"<id>" importance:5§. §save_note§ is the per-session scratchpad; §store_memory§ is the workspace-wide store every future agent inherits. Supersede a stale memory with §supersedes:"<old-id>"§.
- **Before editing a symbol you've touched before** — §query_notes symbol_id:"<id>"§ / §query_memories symbol_id:"<id>"§ surface prior decisions and "do not change this without …" warnings.

**Save / store:** decisions, non-obvious constraints, invariants, follow-ups, incident learnings, bug reproductions. **Skip:** play-by-play the diff already shows, anything derivable from the graph, content already in CLAUDE.md.

`)

// switchBullet renders the instruction-profile discovery line — the
// line every profile carries so a switched-down machine can always
// find its way back. The lean rendering keeps the verb and the
// next-session caveat, dropping only the roster prose.
func switchBullet(active string, lean bool) string {
	if lean {
		return bt(fmt.Sprintf(`- **Profiles:** active §%s§. Broader guidance: §gortex instructions switch core§ (or §full§; §list§ shows all) — NEW sessions only.
`, active))
	}
	return bt(fmt.Sprintf(`- **Instruction profiles** — this block is the active §%s§ profile. §gortex instructions list§ shows the others (§core§ balanced default · §localization§ lean · §full§ maximum guidance); switch with §gortex instructions switch <name>§ — applies to NEW sessions only (instructions, tools/list, and skills all load at session start).
`, active))
}

// sectionDiscovery builds the reference-and-discovery section shared
// by core and full; surfaceLine describes what ships eagerly.
func sectionDiscovery(active, surfaceLine string) string {
	return bt(`## Reference and discovery

- **§gortex://guide§ resource** (or the §gortex guide [topic]§ CLI) is the full reference: LLM-provider matrix, capabilities catalog, analyze / search_ast catalogs, token-economy detail, MCP resources, session-start checklist. Read it on demand — it is not pre-paid here.
- `) + surfaceLine + bt(`- MCP startup manages daemon availability. If the configured server or its tools are unavailable, report a Gortex MCP integration failure; do not start a daemon manually. "cwd is not covered by any tracked repo" means graph tools are unavailable there.
`) + switchBullet(active, false)
}

var compactSurfaceLine = bt(`**§capabilities§** — request an exact operation schema only when the compact tool description is insufficient; ordinary coding requires no tool discovery or promotion.
`)

var fullSurfaceLine = bt(`**§tools_search§** — under this profile the server publishes the full documented dev-cycle preset (~34 workhorse tools) eagerly; the long tail still loads by keyword via §tools_search§ (every tool stays callable by name).
`)

// coreBody is the balanced default: today's slim policy core plus the
// profile-switch discovery line.
func coreBody() string {
	return sectionHeader(false) +
		sectionCompactWorkflow +
		sectionCompactMemory +
		sectionWorktreeViews +
		sectionDiscovery("core", compactSurfaceLine)
}

// fullBody differs from core only in the eager-surface description —
// the ToolPreset column of the table is what actually widens the
// surface.
func fullBody() string {
	return sectionHeader(false) +
		sectionExploreOpener +
		sectionFullRuleTable +
		sectionMCPRequired +
		sectionReadDiscipline +
		sectionMemoryFull +
		sectionWorktreeViews +
		sectionDiscovery("full", fullSurfaceLine)
}

// localizationRow maps an eager tool to its instead-of table row.
// Tools cued outside the table (the one-shot opener and the liveness
// check) are listed in localizationNonTableTools; the body test
// asserts every eager tool is covered by one of the two so the preset
// and the body cannot drift.
type localizationRow struct {
	tool, instead, use string
}

var localizationRows = []localizationRow{
	{"explore", "Localizing a task / bug / \"where is X\"", "§explore§ (one call: ranked neighborhood + source + call paths)"},
	{"search_symbols", "§Grep§ / §rg§ for a symbol", "§search_symbols§ (BM25 + camelCase-aware)"},
	{"search_text", "§Grep§ for a literal / regex", "§search_text§ (trigram-indexed)"},
	{"find_usages", "§Grep§ for references", "§find_usages§ (zero false positives)"},
	{"get_callers", "Reading to find callers", "§get_callers§"},
	{"find_implementations", "Hunting interface implementors", "§find_implementations§"},
	{"get_symbol_source", "§Read§ a file for one symbol", "§get_symbol_source§"},
	{"batch_symbols", "Reading several symbols one by one", "§batch_symbols§ (one call, many bodies)"},
	{"get_file_summary", "§Read§ to understand a file", "§get_file_summary§"},
	{"read_file", "§Read§ a non-indexed / raw file", "§read_file§"},
}

// localizationNonTableTools are eager tools cued in prose rather than
// table rows.
var localizationNonTableTools = map[string]bool{
	"smart_context": true, // the open-with-one-call cue
	"index_health":  true, // the liveness line in discovery
}

// localizationBody is the lean profile: every positioning cue (MUST
// rule, deny warning, one-shot opener, memory triggers, discovery /
// switch-back path) survives; reference elaboration moves to
// gortex://guide.
func localizationBody() string {
	return sectionHeader(true) +
		sectionCompactWorkflow +
		sectionCompactMemory +
		sectionWorktreeViews +
		bt(`**Reference:** call §capabilities§ for an exact operation schema; use §gortex://guide§ only for deeper background.
`) + switchBullet("localization", true)
}
