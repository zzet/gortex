// Package kiro implements the Gortex init integration for Kiro IDE.
package kiro

import "github.com/zzet/gortex/internal/agents"

// SteeringFiles maps filename to content under .kiro/steering/.
var SteeringFiles = map[string]string{
	"gortex-workflow.md": steeringWorkflow,
	"gortex-explore.md":  steeringExplore,
	"gortex-debug.md":    steeringDebug,
	"gortex-impact.md":   steeringImpact,
	"gortex-refactor.md": steeringRefactor,
}

// HookFiles maps filename to Kiro agent-hook JSON under .kiro/hooks/.
var HookFiles = map[string]string{
	"gortex-smart-context.json": hookTaskContext,
	"gortex-post-edit.json":     hookPostEdit,
	"gortex-pre-read.json":      hookPreRead,
}

// legacyHookFiles contains only the exact Kiro hook payloads Gortex shipped
// before Kiro's v1 schema. Semantic comparison permits whitespace changes but
// preserves any user customization instead of claiming ownership by filename.
var legacyHookFiles = map[string]string{
	"gortex-smart-context.json": legacyHookTaskContext,
	"gortex-post-edit.json":     legacyHookPostEdit,
	"gortex-pre-read.json":      legacyHookPreRead,
}

const legacyHookTaskContext = `{
  "name": "Gortex: Task Context",
  "version": "1.0.0",
  "description": "Localize each coding task with the graph before opening files.",
  "when": {"type": "userTriggered"},
  "then": {
    "type": "askAgent",
    "prompt": "For a coding task, call Gortex explore with operation task and the user's task text before reading or searching files. Skip this only for non-coding conversation."
  }
}
`

const legacyHookPostEdit = `{
  "name": "Gortex: Post-Edit Check",
  "version": "1.0.0",
  "description": "Check impact and tests after a source edit.",
  "when": {
    "type": "fileEdited",
    "patterns": ["**/*.go", "**/*.ts", "**/*.tsx", "**/*.js", "**/*.jsx", "**/*.py", "**/*.rs", "**/*.java", "**/*.kt", "**/*.scala", "**/*.swift", "**/*.rb", "**/*.cs", "**/*.php"]
  },
  "then": {
    "type": "askAgent",
    "prompt": "Call Gortex change with operation detect for unstaged changes. Use its affected symbol IDs with operations tests, guards, and contract; run the selected tests and report every result."
  }
}
`

const legacyHookPreRead = `{
  "name": "Gortex: Enrich Source Read",
  "version": "1.0.0",
  "description": "Use indexed source context before a raw source-file read.",
  "when": {"type": "preToolUse", "toolTypes": ["read"]},
  "then": {
    "type": "askAgent",
    "prompt": "For indexed source code, use Gortex read with operation editing_context or source instead of a raw file read. Skip generated files, metadata, documentation, configuration, and non-source assets."
  }
}
`

const steeringWorkflow = `---
inclusion: always
---

# Gortex workflow

Use Gortex MCP tools for indexed code. This is mandatory.

1. Start every coding task with ` + "`explore`" + ` using ` + "`operation: \"task\"`" + ` and the user's task text.
2. Use ` + "`search`" + ` for symbols, text, files, or AST shapes; use ` + "`read`" + ` for file, source, summary, or editing context.
3. Use ` + "`relations`" + ` for usages, callers, dependencies, dependents, and implementations; use ` + "`trace`" + ` for call chains and dataflow.
4. Before mutation, call ` + "`change`" + ` with ` + "`operation: \"impact\"`" + `; for a signature change, also call operation ` + "`verify`" + ` with the proposed signature.
5. Mutate only with ` + "`edit`" + ` or ` + "`refactor`" + `. After mutation, call ` + "`change`" + ` operations ` + "`detect`" + `, ` + "`tests`" + `, ` + "`guards`" + `, and ` + "`contract`" + `.
6. Call ` + "`capabilities`" + ` with ` + "`domain`" + `, ` + "`operation`" + `, and ` + "`detail: \"schema\"`" + ` when exact arguments are not already visible.

Do not replace graph reads or searches with shell commands. If the configured Gortex tools are missing from the callable MCP tools, report a Gortex MCP integration failure and stop; do not start a daemon or use a CLI/shell fallback.

For durable context, use ` + "`recall`" + ` (` + "`surface`" + `/` + "`notes`" + `/` + "`memories`" + `) before editing known code and ` + "`remember`" + ` (` + "`note`" + `/` + "`memory`" + `) for decisions and invariants.
`

const steeringExplore = `---
inclusion: manual
---

# Explore with Gortex

1. Call ` + "`explore({operation:\"task\", task:\"<question>\"})`" + `.
2. Narrow names with ` + "`search({operation:\"symbols\", query:\"<name>\"})`" + ` or literals with ` + "`operation:\"text\"`" + `.
3. Read only the needed source with ` + "`read`" + ` (` + "`source`" + `/` + "`summary`" + `/` + "`editing_context`" + `).
4. Prove relationships with ` + "`relations`" + ` or execution paths with ` + "`trace`" + `.
5. Return symbol IDs, file:line locations, and the shortest evidence needed.
`

const steeringDebug = `---
inclusion: manual
---

# Debug with Gortex

1. Localize the symptom with ` + "`explore`" + `.
2. Use ` + "`search`" + ` with ` + "`operation:\"text\"`" + ` for an exact error and ` + "`operation:\"symbols\"`" + ` for named code.
3. Use ` + "`relations({operation:\"callers\", ...})`" + ` and ` + "`trace({operation:\"call_chain\", ...})`" + ` to follow execution.
4. Use ` + "`trace`" + ` with ` + "`flow`" + ` or ` + "`taint`" + ` when the bug concerns values crossing helpers.
5. Read only the suspect symbols, then state the root cause and evidence.
`

const steeringImpact = `---
inclusion: manual
---

# Assess change impact with Gortex

1. Resolve the target with ` + "`explore`" + ` or ` + "`search`" + `.
2. Call ` + "`change`" + ` with ` + "`operation:\"impact\"`" + `.
3. For signature changes, call ` + "`change`" + ` with ` + "`operation:\"verify\"`" + `.
4. Check ` + "`relations`" + ` usages/dependents and ` + "`analyze`" + ` contracts when boundaries are involved.
5. Call ` + "`change`" + ` with ` + "`operation:\"tests\"`" + ` and report concrete tests.
`

const steeringRefactor = `---
inclusion: manual
---

# Refactor with Gortex

1. Start with ` + "`explore`" + ` and read the target through ` + "`read`" + `.
2. Call ` + "`change`" + ` operations ` + "`impact`" + ` and ` + "`edit_plan`" + ` before writing; for a signature change, also call ` + "`verify`" + ` with the proposed signature.
3. Use ` + "`refactor`" + ` for rename, move, inline, delete, or code actions. Use ` + "`edit`" + ` for file, symbol, batch, or new-file changes.
4. After writing, call ` + "`change`" + ` operations ` + "`detect`" + `, ` + "`tests`" + `, ` + "`guards`" + `, and ` + "`contract`" + `.
5. Run the project tests selected by the graph.
`

const hookTaskContext = `{
  "version": "v1",
  "hooks": [
    {
      "name": "Gortex: Task Context",
      "description": "Localize each coding task with the graph before opening files.",
      "trigger": "UserPromptSubmit",
      "action": {
        "type": "agent",
        "prompt": "For a coding task, call Gortex explore with operation task and the user's task text before reading or searching files. Skip this only for non-coding conversation."
      }
    }
  ]
}
`

const hookPostEdit = `{
  "version": "v1",
  "hooks": [
    {
      "name": "Gortex: Post-Edit Check",
      "description": "Check impact and tests after a source edit.",
      "trigger": "PostFileSave",
      "matcher": "\\.(go|ts|tsx|js|jsx|py|rs|java|kt|scala|swift|rb|cs|php)$",
      "action": {
        "type": "agent",
        "prompt": "Call Gortex change with operation detect for unstaged changes. Use its affected symbol IDs with operations tests, guards, and contract; run the selected tests and report every result."
      }
    }
  ]
}
`

const hookPreRead = `{
  "version": "v1",
  "hooks": [
    {
      "name": "Gortex: Enrich Source Read",
      "description": "Use indexed source context before a raw source-file read.",
      "trigger": "PreToolUse",
      "matcher": "read",
      "action": {
        "type": "agent",
        "prompt": "For indexed source code, use Gortex read with operation editing_context or source instead of a raw file read. Skip generated files, metadata, documentation, configuration, and non-source assets."
      }
    }
  ]
}
`

// AutoApproveTools follows the compact MCP surface. External review
// publishing remains approval-gated by the shared policy.
var AutoApproveTools = agents.CompactMCPAutoApproveTools()

// v060AutoApproveTools is the exact approval policy shipped by gortex
// v0.60.0. It is only a safe migration fingerprint. The concrete retirement
// gate is documented in docs/versioning.md.
var v060AutoApproveTools = []string{
	"graph_stats", "search_symbols", "winnow_symbols", "get_symbol", "get_file_summary",
	"get_editing_context", "get_dependencies", "get_dependents", "get_call_chain", "get_callers",
	"find_implementations", "find_usages", "get_cluster", "get_symbol_source", "batch_symbols",
	"find_import_path", "explain_change_impact", "get_recent_changes", "smart_context", "get_edit_plan", "get_test_targets", "suggest_pattern",
	"get_communities", "get_processes", "detect_changes", "index_repository", "reindex_repository",
	"verify_change", "check_guards", "prefetch_context", "analyze", "diff_context", "index_health", "get_symbol_history",
	"scaffold", "batch_edit", "contracts", "feedback", "save_note", "query_notes", "distill_session",
	"store_memory", "query_memories", "surface_memories",
}
