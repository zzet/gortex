package hooks

import (
	"strings"

	"github.com/zzet/gortex/internal/profiles"
	"github.com/zzet/gortex/internal/toolref"
)

// enrichTask produces a condensed graph-orientation briefing for a Task
// (subagent) spawn. Claude Code's `Task` tool receives PreToolUse
// additionalContext before the subagent begins, so this is the hook point
// for "subagent start".
//
// The briefing combines:
//   - repo orientation (graph_stats)
//   - task-relevant symbols (smart_context over description + prompt)
//   - recently-modified symbols from this session (get_symbol_history)
//
// Returns an empty result when the bridge is unreachable or when there is no
// meaningful task text to derive context from. The hook must degrade silently
// and never block subagent spawning.
func enrichTask(toolInput map[string]any, port int) enrichResult {
	description, _ := toolInput["description"].(string)
	prompt, _ := toolInput["prompt"].(string)

	task := strings.TrimSpace(description + "\n" + prompt)
	if task == "" {
		return enrichResult{}
	}
	// Cap the task text we send to the bridge — full prompts can be huge.
	const maxTaskLen = 2000
	if len(task) > maxTaskLen {
		task = task[:maxTaskLen]
	}

	stats := callServerTool(port, "graph_stats", nil)
	if stats == "" {
		// Bridge unreachable — silent.
		return enrichResult{}
	}

	var sb strings.Builder
	sb.WriteString("[Gortex] Subagent briefing — this repo has a Gortex MCP server.\n")
	sb.WriteString("Subagents don't inherit CLAUDE.md, so the rules below are restated inline:\n\n")

	sb.WriteString(gortexToolGuidance)
	sb.WriteString(toolref.MCPRequiredLine())
	sb.WriteString("\n")

	if summary := renderStatsSummary(stats); summary != "" {
		sb.WriteString("**Index:** ")
		sb.WriteString(summary)
		sb.WriteString("\n\n")
	}

	if ctx := renderTaskContext(port, task); ctx != "" {
		sb.WriteString("### Relevant Symbols (from `explore`)\n\n")
		sb.WriteString(ctx)
		sb.WriteString("\n")
	}

	if churn := renderSymbolHistory(port); churn != "" {
		// Not session-scoped: renderSymbolHistory reads the process-global
		// Server.symHistory, shared by every client of the daemon — and a
		// subagent turn never wrote any of it.
		sb.WriteString("### Recently Modified (Gortex-tracked edits on this daemon, all clients)\n\n")
		sb.WriteString(churn)
		sb.WriteString("\n")
	}

	sb.WriteString("_First call: `explore` with the task. Inspect with `search`, `read`, `relations`, or `trace`. Before mutation call `change(operation:\"impact\")`; mutate only with `edit` or `refactor`. After mutation call `change(operation:\"detect\")`, then use its symbol IDs with `change` operations `tests`, `guards`, and `contract`._\n")
	sb.WriteString("_For compact output, pass `output:{format:\"gcx\"}`._\n")

	return enrichResult{context: sb.String()}
}

// gortexToolGuidance is the condensed tool-swap reference injected into every
// subagent briefing. Kept short (~14 lines) so the token overhead per Task
// spawn stays small; the full table lives in CLAUDE.md for parent-agent use.
const gortexToolGuidance = "### MUST use Gortex MCP tools instead of Read/Grep/Glob\n" +
	"\n" +
	"1. Call `explore` with the delegated task.\n" +
	"2. Inspect indexed code only with `search`, `read`, `relations`, and `trace`.\n" +
	"3. Before mutation call `change(operation:\"impact\")`; for a signature change also call `change(operation:\"verify\")` with the proposed signature. Mutate only with `edit` or `refactor`. After mutation call `change(operation:\"detect\")`, then use its symbol IDs with `change` operations `tests`, `guards`, and `contract`.\n" +
	"4. Call `capabilities` only when an operation's exact fields are unknown.\n" +
	"\n### Worktree and branch routing\n\n" + profiles.WorktreeBranchRoutingPolicy

// renderTaskContext calls smart_context with the subagent task text and
// returns a capped body. Falls back to empty on any error. Only declared
// options ride the call — smart_context has no compact option, and an
// undeclared key would draw the dispatch arg guard's rider into the
// briefing text.
func renderTaskContext(port int, task string) string {
	raw := callServerTool(port, "smart_context", map[string]any{
		"task": task,
	})
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return cappedLines(raw, 12)
}
