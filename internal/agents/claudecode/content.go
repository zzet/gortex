// Package claudecode implements the Gortex init integration for
// Anthropic's Claude Code CLI. It manages six on-disk artifacts:
//
//   - .mcp.json                   (project-level MCP stanza, shared)
//   - .claude/commands/gortex-*.md (slash commands)
//   - .claude/settings.json        (MCP tool permissions, shared)
//   - .claude/settings.local.json  (PreToolUse/PreCompact/Stop hooks)
//   - CLAUDE.md                    (appended instructions block)
//   - ~/.claude/skills/gortex-*    (user-level skills)
//
// Global mode additionally writes ~/.claude.json (user-level MCP
// stanza) and ~/.claude/settings.local.json (user-level hooks).
//
// The bulky content blocks (CLAUDE.md instructions, slash-command
// markdown, skill frontmatter) live in this file so the adapter
// logic in adapter.go stays readable. Content is kept as Go string
// constants rather than embedded files so byte-for-byte reproduction
// of the pre-refactor behaviour is trivially verifiable.
package claudecode

import (
	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/profiles"
)

// ProjectMCPJSON is the starter content for a project's .mcp.json
// when no file exists yet.
const ProjectMCPJSON = `{
  "mcpServers": {
    "gortex": {
      "command": "gortex",
      "args": [
        "mcp"
      ],
      "env": {
        "GORTEX_INDEX_WORKERS": "${GORTEX_WORKERS:-8}"
      }
    }
  }
}
`

// ClaudeMdBlock is the canonical "use Gortex tools instead of
// Read/Grep" instructions appended to a project's CLAUDE.md. The
// byte sequence here must match what the previous implementation
// wrote, or the idempotency check (contains "## MANDATORY: Use
// Gortex MCP tools") would misfire on re-runs.
//
// The shared body lives in `agents.InstructionsBody` so every
// doc-aware adapter writes the same rule table. Claude Code
// additionally advertises its slash commands — appended here so the
// block stays self-contained for CLAUDE.md readers.
const ClaudeMdBlock = agents.InstructionsBody + `
## Gortex slash commands

Discovery & analysis: ` + "`/gortex-guide`" + `, ` + "`/gortex-explore`" + `, ` + "`/gortex-debug`" + `, ` + "`/gortex-impact`" + `, ` + "`/gortex-dataflow-trace`" + `, ` + "`/gortex-cross-repo-usage`" + `, ` + "`/gortex-co-change`" + `, ` + "`/gortex-onboarding`" + `

Refactor & edit (enforce tool-call order): ` + "`/gortex-refactor`" + `, ` + "`/gortex-safe-edit`" + `, ` + "`/gortex-rename`" + `, ` + "`/gortex-extract-function`" + `, ` + "`/gortex-fix-all`" + `, ` + "`/gortex-add-test`" + `

Review & operate: ` + "`/gortex-pr-review`" + `, ` + "`/gortex-architecture-review`" + `, ` + "`/gortex-quality-audit`" + `, ` + "`/gortex-incident-investigation`" + `, ` + "`/gortex-episode-replay`" + `

Follow each command's ordered MCP workflow. Use ` + "`explore`" + ` first for task-shaped work, ` + "`change`" + ` before and after mutations, and ` + "`edit`" + ` or ` + "`refactor`" + ` for writes. Call ` + "`capabilities`" + ` only when an operation's exact arguments are unclear.
`

// ClaudeMdSentinel is the substring used to detect whether
// ClaudeMdBlock has already been appended to a project's
// CLAUDE.md. Kept as a named constant so the doctor subcommand can
// query it without pulling in the entire block. Aliased to the shared
// sentinel so idempotency works across adapters writing to the same
// file (e.g. AGENTS.md shared by Codex + Opencode).
const ClaudeMdSentinel = agents.InstructionsSentinel

// SlashCommands maps the filename under .claude/commands/ to its
// markdown content. Each file is a slash command Claude Code
// auto-discovers.
var SlashCommands = map[string]string{
	"gortex-guide.md":                  commandGuide,
	"gortex-explore.md":                commandExplore,
	"gortex-debug.md":                  commandDebug,
	"gortex-impact.md":                 commandImpact,
	"gortex-refactor.md":               commandRefactor,
	"gortex-safe-edit.md":              commandSafeEdit,
	"gortex-fix-all.md":                commandFixAll,
	"gortex-extract-function.md":       commandExtractFunction,
	"gortex-rename.md":                 commandRename,
	"gortex-cross-repo-usage.md":       commandCrossRepoUsage,
	"gortex-dataflow-trace.md":         commandDataflowTrace,
	"gortex-add-test.md":               commandAddTest,
	"gortex-incident-investigation.md": commandIncidentInvestigation,
	"gortex-episode-replay.md":         commandEpisodeReplay,
	"gortex-co-change.md":              commandCoChange,
	"gortex-onboarding.md":             commandOnboarding,
	"gortex-quality-audit.md":          commandQualityAudit,
	"gortex-architecture-review.md":    commandArchitectureReview,
	"gortex-pr-review.md":              commandPRReview,
	"gortex-pr-review-agent.md":        commandPRReviewAgent,
}

// GlobalSkills maps the directory name under ~/.claude/skills/ to
// the SKILL.md body. Skill files get YAML frontmatter so Claude Code
// can show them in its skill picker.
var GlobalSkills = map[string]string{
	"gortex-guide": `---
name: gortex-guide
description: "Gortex reference: available tools, graph schema, and workflow."
---
` + commandGuide,

	"gortex-explore": `---
name: gortex-explore
description: "Understand how code works or trace an execution flow."
---
` + commandExplore,

	"gortex-debug": `---
name: gortex-debug
description: "Debug a bug, trace an error, or find why something fails."
---
` + commandDebug,

	"gortex-impact": `---
name: gortex-impact
description: "Assess what breaks if you change X before editing it."
---
` + commandImpact,

	"gortex-refactor": `---
name: gortex-refactor
description: "Rename, extract, split, or restructure code safely."
---
` + commandRefactor,

	"gortex-safe-edit": `---
name: gortex-safe-edit
description: "Preview an edit's blast radius on the shadow graph before writing."
---
` + commandSafeEdit,

	"gortex-fix-all": `---
name: gortex-fix-all
description: "Clear LSP diagnostics — one error, a file, or source.fixAll."
---
` + commandFixAll,

	"gortex-extract-function": `---
name: gortex-extract-function
description: "Extract code into a function or method via LSP refactor."
---
` + commandExtractFunction,

	"gortex-rename": `---
name: gortex-rename
description: "Rename a symbol and update every reference atomically."
---
` + commandRename,

	"gortex-cross-repo-usage": `---
name: gortex-cross-repo-usage
description: "Find who uses a symbol across all consumer repos."
---
` + commandCrossRepoUsage,

	"gortex-dataflow-trace": `---
name: gortex-dataflow-trace
description: "Trace where a value flows through the code."
---
` + commandDataflowTrace,

	"gortex-add-test": `---
name: gortex-add-test
description: "Add tests for under-tested code and coverage gaps."
---
` + commandAddTest,

	"gortex-incident-investigation": `---
name: gortex-incident-investigation
description: "Walk a production symptom back to its root cause."
---
` + commandIncidentInvestigation,

	"gortex-episode-replay": `---
name: gortex-episode-replay
description: "Reconstruct what changed in a window (postmortem, release, PR)."
---
` + commandEpisodeReplay,

	"gortex-co-change": `---
name: gortex-co-change
description: "Find what changes together and hidden coupling."
---
` + commandCoChange,

	"gortex-onboarding": `---
name: gortex-onboarding
description: "Structured tour of an unfamiliar repo."
---
` + commandOnboarding,

	"gortex-quality-audit": `---
name: gortex-quality-audit
description: "Repo-scale quality scan — dead code, hotspots, clones."
---
` + commandQualityAudit,

	"gortex-architecture-review": `---
name: gortex-architecture-review
description: "Graph-grounded architectural read of a repo."
---
` + commandArchitectureReview,

	"gortex-pr-review": `---
name: gortex-pr-review
description: "Graph-grounded review of a pending change or PR."
---
` + commandPRReview,

	"gortex-pr-review-agent": `---
name: gortex-pr-review-agent
description: "Coding-agent review verdict via the gortex review verb."
---
` + commandPRReviewAgent,

	// gortex-cli is skill-only (no slash-command twin) and self-contained:
	// the frontmatter lives inside skillGortexCLI rather than being prefixed
	// here, because the body is shell commands rather than a shared command*
	// constant.
	"gortex-cli": skillGortexCLI,
}

const worktreeBranchRoutingRules = `
## Worktree and branch routing

` + profiles.WorktreeBranchRoutingPolicy

const nativeMCPRules = `
## Required behavior

- Native Gortex MCP is mandatory for this MCP-configured workflow. If its callable tools are missing, report a Gortex MCP integration failure and stop; do not start a daemon or switch to a CLI/shell fallback.
- Do not substitute shell reads, file search, or Git plumbing.
- Start task-shaped work with ` + "`explore`" + `. For one already-known symbol, start with ` + "`search`" + `.
- Use ` + "`change`" + ` before a mutation and again after it. Write only through ` + "`edit`" + ` or ` + "`refactor`" + `.
- Call ` + "`capabilities({domain: \"<tool>\", operation: \"<operation>\", detail: \"schema\"})`" + ` only when an operation's exact arguments are unclear.
- Report graph-backed paths and symbol IDs. Never invent a result when an operation returns no match.
` + worktreeBranchRoutingRules

const commandGuide = `# Gortex Guide

Use this sequence for codebase work:

1. ` + "`explore({operation: \"task\", task: \"<goal or bug>\"})`" + ` — localize the task and obtain the working set.
2. ` + "`search({operation: \"symbols\", query: \"<name>\"})`" + ` or ` + "`search({operation: \"text\", query: \"<literal>\"})`" + ` — resolve a precise anchor.
3. ` + "`read({operation: \"source\", target: {symbol: \"<id>\"}})`" + ` — read only the source needed.
4. ` + "`relations({operation: \"usages\", target: {symbol: \"<id>\"}})`" + ` or ` + "`trace({operation: \"call_chain\", target: {symbol: \"<id>\"}})`" + ` — follow verified graph edges.
5. ` + "`change({operation: \"impact\", source: {symbols: [\"<id>\"]}})`" + ` — assess a planned change.
6. Apply with ` + "`edit`" + ` or ` + "`refactor`" + `, then run ` + "`change`" + ` operations ` + "`detect`" + `, ` + "`guards`" + `, and ` + "`tests`" + `.
` + nativeMCPRules

const commandExplore = `# Explore a Codebase with Gortex

1. Call ` + "`explore({operation: \"task\", task: \"<question>\"})`" + `.
2. Read only a returned anchor with ` + "`read({target: {symbol: \"<id>\"}})`" + `.
3. Choose the needed relationship (` + "`callers`" + `, ` + "`usages`" + `, or ` + "`dependencies`" + `), for example ` + "`relations({operation: \"callers\", target: {symbol: \"<id>\"}})`" + `; use ` + "`trace({operation: \"call_chain\", target: {symbol: \"<id>\"}})`" + ` for execution order.
4. Answer with the execution path, file locations, symbol IDs, and any graph uncertainty.
` + nativeMCPRules

const commandDebug = `# Debug with Gortex

1. Localize the exact symptom with ` + "`explore({operation: \"task\", task: \"<error and observed behavior>\"})`" + `.
2. For a literal error, call ` + "`search({operation: \"text\", query: \"<exact text>\"})`" + `. For a name, use operation ` + "`symbols`" + `.
3. Walk toward the cause with ` + "`relations({operation: \"callers\", target: {symbol: \"<id>\"}})`" + ` and ` + "`trace({operation: \"call_chain\", target: {symbol: \"<id>\"}})`" + `. Use ` + "`flow`" + ` or ` + "`taint`" + ` only after resolving both ` + "`target`" + ` and ` + "`to`" + ` endpoints.
4. Confirm repository-wide error propagation with ` + "`analyze({kind: \"error_surface\"})`" + `; use ` + "`options.repo`" + ` to narrow a multi-repository workspace.
5. Before fixing, run ` + "`change({operation: \"impact\", source: {symbols: [\"<id>\"]}})`" + `. After fixing, run ` + "`change`" + ` operations ` + "`detect`" + `, ` + "`tests`" + `, and ` + "`guards`" + `.
` + nativeMCPRules

const commandImpact = `# Assess Change Impact with Gortex

1. Resolve the target with ` + "`search({operation: \"symbols\", query: \"<name>\"})`" + `.
2. Query ` + "`relations`" + ` operations ` + "`usages`" + `, ` + "`dependents`" + `, and ` + "`implementations`" + ` for the resolved symbol.
3. Call ` + "`change({operation: \"impact\", source: {symbols: [\"<id>\"]}})`" + `; add operation ` + "`api_impact`" + ` for a public API.
4. For a signature change, require ` + "`change({operation: \"verify\", source: {changes: [{symbol_id: \"<id>\", new_signature: \"<signature>\"}]}})`" + `.
5. Report direct callers, transitive risk, interfaces, contracts, and the tests returned by ` + "`change({operation: \"tests\", source: {symbols: [\"<id>\"]}})`" + `.
` + nativeMCPRules

const commandRefactor = `# Refactor with Gortex

1. Call ` + "`explore({operation: \"task\", task: \"<refactor goal>\"})`" + ` and resolve every target with ` + "`search`" + `.
2. Run ` + "`change({operation: \"impact\", source: {symbols: [\"<id>\"]}})`" + `. Before altering a signature, also run ` + "`change({operation: \"verify\", source: {changes: [{symbol_id: \"<id>\", new_signature: \"<signature>\"}]}})`" + `.
3. Choose exactly one ` + "`refactor`" + ` operation: ` + "`rename`" + `, ` + "`move`" + `, ` + "`inline`" + `, ` + "`delete`" + `, or ` + "`apply_code_action`" + `. For example: ` + "`refactor({operation: \"rename\", target: {symbol: \"<id>\"}, new_name: \"<name>\"})`" + `.
4. Require ` + "`change`" + ` operations ` + "`detect`" + `, ` + "`tests`" + `, ` + "`guards`" + `, and ` + "`contract`" + ` after the mutation. Resolve every violation before finishing.
` + nativeMCPRules

const commandSafeEdit = `# Make a Safe Edit with Gortex

1. Localize with ` + "`explore`" + ` and inspect ` + "`read({operation: \"editing_context\", target: {file: \"<path>\"}})`" + `.
2. Run ` + "`change({operation: \"impact\", source: {symbols: [\"<id>\"]}})`" + ` before editing.
3. Choose ` + "`edit`" + ` operation ` + "`file`" + `, ` + "`symbol`" + `, or ` + "`batch`" + `. Preview with ` + "`dry_run: true`" + `; after review, repeat the same guarded request with ` + "`dry_run: false`" + `. Use ` + "`change({operation: \"simulate\", source: {steps: \"<WorkspaceEdit JSON array>\"}})`" + ` for a multi-step semantic edit.
4. Run ` + "`change`" + ` operations ` + "`detect`" + `, ` + "`tests`" + `, ` + "`guards`" + `, and ` + "`contract`" + `. Signature verification belongs before mutation with the proposed signature.
` + nativeMCPRules

const commandFixAll = `# Fix Diagnostics with Gortex

1. Read diagnostics using ` + "`change({operation: \"diagnostics\", source: {file: \"<path>\"}})`" + `.
2. Fetch applicable fixes with ` + "`change({operation: \"code_actions\", source: {file: \"<path>\"}})`" + `.
3. Apply one reviewed action with ` + "`refactor({operation: \"apply_code_action\", target: {file: \"<path>\"}, options: {...}})`" + `, or use operation ` + "`fix_all`" + ` only when the user asked for all safe fixes.
4. Re-run diagnostics, then run ` + "`change`" + ` operations ` + "`detect`" + ` and ` + "`tests`" + `.
` + nativeMCPRules

const commandExtractFunction = `# Extract a Function with Gortex

1. Inspect the file with ` + "`read({operation: \"editing_context\", target: {file: \"<path>\"}})`" + `.
2. Resolve the selected range with ` + "`change({operation: \"ranges\", source: {file: \"<path>\", range: {...}}})`" + `.
3. Request extraction actions with ` + "`change({operation: \"code_actions\", source: {file: \"<path>\", range: {...}}})`" + `.
4. Apply the selected action through ` + "`refactor({operation: \"apply_code_action\", target: {file: \"<path>\"}, options: {...}})`" + `.
5. Run ` + "`change`" + ` operations ` + "`detect`" + `, ` + "`tests`" + `, ` + "`guards`" + `, and ` + "`contract`" + `.
` + nativeMCPRules

const commandRename = `# Rename a Symbol with Gortex

1. Resolve exactly one symbol with ` + "`search({operation: \"symbols\", query: \"<old name>\"})`" + `.
2. Enumerate references and implementations with ` + "`relations`" + ` operations ` + "`usages`" + ` and ` + "`implementations`" + `.
3. Run ` + "`change({operation: \"impact\", source: {symbols: [\"<id>\"]}})`" + `. For a public signature rename, also run ` + "`change({operation: \"verify\", source: {changes: [{symbol_id: \"<id>\", new_signature: \"<signature with new name>\"}]}})`" + `.
4. Preview with ` + "`refactor({operation: \"rename\", target: {symbol: \"<id>\"}, new_name: \"<new>\", dry_run: true})`" + `, then repeat it with ` + "`dry_run: false`" + ` to apply.
5. Require ` + "`change`" + ` operations ` + "`detect`" + `, ` + "`guards`" + `, and ` + "`tests`" + `; confirm no old usages remain.
` + nativeMCPRules

const commandCrossRepoUsage = `# Find Cross-Repository Usage with Gortex

1. Confirm tracked repositories with ` + "`workspace({operation: \"repos\"})`" + `.
2. Resolve the provider symbol with ` + "`search({operation: \"symbols\", query: \"<name>\", options: {repo: \"<provider>\"}})`" + `.
3. Call ` + "`relations({operation: \"usages\", target: {symbol: \"<id>\"}})`" + ` and group results by repository.
4. Add ` + "`analyze({kind: \"cross_repo\", options: {repo: \"<provider>\"}})`" + ` for repository boundaries and ` + "`analyze({kind: \"contracts\", options: {action: \"bridge\", mode: \"impact\", symbol: \"<id>\"}})`" + ` for wire-level consumers.
5. Report indexed coverage and name any untracked repositories as an explicit gap.
` + nativeMCPRules

const commandDataflowTrace = `# Trace Data Flow with Gortex

1. Call ` + "`explore({operation: \"task\", task: \"trace <value> from <source> to <sink>\"})`" + `.
2. Resolve endpoints with ` + "`search({operation: \"symbols\", query: \"<source or sink>\"})`" + `.
3. Use ` + "`trace({operation: \"flow\", target: {symbol: \"<source-id>\"}, to: {symbol: \"<sink-id>\"}})`" + `. Use operation ` + "`taint`" + ` when source/sink security semantics matter.
4. Cross-check control flow with operation ` + "`call_chain`" + ` when the data-flow graph has a gap.
5. Report each hop with its symbol ID and confidence; distinguish no path from incomplete indexing.
` + nativeMCPRules

const commandAddTest = `# Add Tests with Gortex

1. Localize the behavior with ` + "`explore`" + `.
2. Confirm the specific gap with ` + "`change({operation: \"tests\", source: {symbols: [\"<id>\"]}})`" + `. Use repository-wide ` + "`analyze({kind: \"untested\"})`" + ` or path-scoped ` + "`analyze({kind: \"coverage_gaps\", options: {path_prefix: \"<path>\"}})`" + ` only for broader coverage discovery.
3. Inspect callers with ` + "`relations({operation: \"callers\", target: {symbol: \"<id>\"}})`" + ` and request targets with ` + "`change({operation: \"tests\", source: {symbols: [\"<id>\"]}})`" + `.
4. Add the narrowest test with ` + "`edit({operation: \"file\", target: {file: \"<existing test path>\"}, ...})`" + `; use operation ` + "`write`" + ` for a new test file.
5. Run the test, then require ` + "`change`" + ` operations ` + "`detect`" + ` and ` + "`guards`" + `.
` + nativeMCPRules

const commandIncidentInvestigation = `# Investigate an Incident with Gortex

1. Paste the exact symptom into ` + "`explore({operation: \"task\", task: \"<alert, error, and time window>\"})`" + `.
2. Search exact error text with ` + "`search({operation: \"text\", query: \"<literal>\"})`" + `.
3. Walk ` + "`relations`" + ` operation ` + "`callers`" + ` and ` + "`trace`" + ` operations ` + "`call_chain`" + `, ` + "`flow`" + `, or ` + "`taint`" + ` from symptom toward cause.
4. Correlate repository-wide ` + "`analyze({kind: \"recent_changes\"})`" + ` with symbol-scoped ` + "`recall({operation: \"surface\", arguments: {symbol_ids: \"<id>\", task: \"<symptom>\"}})`" + `.
5. Separate evidence, hypothesis, and unknowns. Gate any fix through ` + "`change`" + ` before and after the mutation.
` + nativeMCPRules

const commandEpisodeReplay = `# Replay a Change Episode with Gortex

1. Run ` + "`analyze({kind: \"replay\", options: {from: \"<start>\", to: \"<end>\"}})`" + ` for the requested window.
2. Project the relevant diff with ` + "`change({operation: \"detect\", source: {scope: \"<scope>\"}})`" + `.
3. Surface decisions with ` + "`recall({operation: \"surface\", arguments: {task: \"<episode>\"}})`" + `.
4. Trace important before/after paths using ` + "`trace`" + ` and identify which change altered behavior.
5. Produce a timestamped narrative with evidence links, impact, missed signals, and remaining uncertainty.
` + nativeMCPRules

const commandCoChange = `# Analyze Co-Change with Gortex

1. Resolve the anchor with ` + "`search({operation: \"symbols\", query: \"<name>\"})`" + `.
2. Call symbol-scoped ` + "`analyze({kind: \"co_change\", target: {symbol: \"<id>\"}})`" + ` and repository-wide ` + "`analyze({kind: \"churn\"})`" + `.
3. Compare structural coupling through ` + "`relations`" + ` operations ` + "`cluster`" + ` and ` + "`dependents`" + `.
4. Report strong historical pairs, graph-confirmed dependencies, likely hidden coupling, and an actionable boundary or guard.
` + nativeMCPRules

const commandOnboarding = `# Onboard to a Repository with Gortex

1. Call ` + "`explore({operation: \"outline\"})`" + `, then ` + "`explore({operation: \"task\", task: \"explain the repository's main responsibilities and entry points\"})`" + `.
2. Run ` + "`analyze`" + ` with kinds ` + "`architecture`" + `, ` + "`communities`" + `, and ` + "`processes`" + `.
3. Trace one representative request or job with ` + "`trace({operation: \"call_chain\", target: {symbol: \"<entry-id>\"}})`" + `.
4. Read only the key returned symbols. Deliver a concise map of entry points, boundaries, data flow, tests, and operational risks.
` + nativeMCPRules

const commandQualityAudit = `# Audit Repository Quality with Gortex

1. Establish scope with ` + "`workspace({operation: \"info\"})`" + ` and ` + "`explore({operation: \"outline\"})`" + `.
2. Run ` + "`analyze`" + ` for ` + "`health`" + `, ` + "`dead_code`" + `, ` + "`hotspots`" + `, ` + "`cycles`" + `, and ` + "`clones`" + `.
3. Validate high-risk findings with ` + "`read`" + ` and ` + "`relations`" + `; do not report an unverified heuristic as a fact.
4. Rank findings by evidence, impact, and remediation cost. Include exact paths, symbol IDs, and a smallest-first action plan.
` + nativeMCPRules

const commandArchitectureReview = `# Review Architecture with Gortex

1. Build the map with ` + "`explore({operation: \"outline\"})`" + ` and ` + "`analyze`" + ` kinds ` + "`architecture`" + ` and ` + "`communities`" + `.
2. Measure boundaries with ` + "`analyze({kind: \"coupling\"})`" + ` and cycles with ` + "`analyze({kind: \"cycles\"})`" + `.
3. Trace representative paths with ` + "`trace`" + ` and inspect public boundaries with ` + "`analyze({kind: \"contracts\", options: {action: \"list\"}})`" + `.
4. Deliver observed structure, intended structure, violations, consequences, and prioritized changes. Cite graph evidence for every finding.
` + nativeMCPRules

const commandPRReview = `# Review a Change with Gortex

1. Project the working tree with ` + "`change({operation: \"detect\", source: {scope: \"unstaged\"}})`" + `; use ` + "`staged`" + ` or ` + "`compare`" + ` only when that is the requested review scope.
2. Build review context with ` + "`review({operation: \"run\", source: {scope: \"<scope>\"}})`" + ` and operation ` + "`diff_context`" + ` when per-file context is needed.
3. Require ` + "`change`" + ` operations ` + "`guards`" + `, ` + "`tests`" + `, and ` + "`contract`" + `. When the proposed signature is known, run operation ` + "`verify`" + ` before mutation.
4. Check wire boundaries with ` + "`analyze({kind: \"contracts\", options: {action: \"check\"}})`" + ` when APIs or events changed.
5. Report only actionable findings with severity, ` + "`file:line`" + `, evidence, consequence, and correction. State an explicit clean verdict when none remain.
` + nativeMCPRules

const commandPRReviewAgent = `# Review a Change as a Sub-Agent

## MCP-capable harness

Native Gortex MCP is mandatory. Call ` + "`review({operation: \"run\", source: {scope: \"<scope>\"}})`" + `, then use ` + "`change`" + ` operations ` + "`guards`" + `, ` + "`tests`" + `, and ` + "`contract`" + `. When the proposed signature is known, run operation ` + "`verify`" + ` before mutation. If the configured callable tools are missing, report a Gortex MCP integration failure and stop; do not start a daemon or use the Bash path below.

## Bash-only harness

Use this path only when the harness has no MCP transport by design:

` + "```bash" + `
gortex review --audience agent --format json
` + "```" + `

In either mode, return ` + "`VERDICT: clean`" + ` or ` + "`VERDICT: findings`" + ` followed by actionable findings with severity and ` + "`file:line`" + `. Do not replace graph-backed review with ad-hoc ` + "`git diff`" + ` inspection.
` + worktreeBranchRoutingRules

const skillGortexCLI = `---
name: gortex-cli
description: "Bash-only mirror of Gortex MCP tools for harnesses without MCP support."
---
# Gortex from a Bash-Only Harness

Use this skill only in a harness that has no MCP transport by design. If a Gortex MCP server is configured but its callable tools are missing, report an integration failure; do not use this skill as a fallback.

Every public MCP tool has the same name through ` + "`gortex call`" + `:

` + "```bash" + `
gortex call explore --arg task='locate the authentication flow'
gortex call search --arg operation=symbols --arg query=UserStore
gortex call read --arg target='{"symbol":"internal/store.go::UserStore"}'
gortex call relations --arg operation=usages --arg target='{"symbol":"internal/store.go::UserStore"}'
gortex call change --arg operation=impact --arg target='{"symbol":"internal/store.go::UserStore"}'
gortex call edit --arg target='{"file":"internal/store.go"}' --arg match='old text' --arg replacement='new text'
gortex call change --arg operation=detect --arg source='{"scope":"all"}'
` + "```" + `

For an exact operation schema:

` + "```bash" + `
gortex call capabilities --arg domain=read --arg operation=source --arg detail=schema
` + "```" + `

The required order is ` + "`explore`" + ` → targeted ` + "`search/read/relations/trace`" + ` → pre-change ` + "`change`" + ` → ` + "`edit/refactor`" + ` → post-change ` + "`change`" + `. Do not use shell file reads or search as a substitute.
` + worktreeBranchRoutingRules
