package claudecode

import (
	"strings"

	"github.com/zzet/gortex/internal/profiles"
)

// SubAgents maps the filename under .claude/agents/ to a graph-only
// sub-agent definition. Each allowlist names tools present on the compact MCP
// surface received by every named client.
var SubAgents = map[string]string{
	"gortex-search.md": subagentSearch,
	"gortex-impact.md": subagentImpact,
}

// SubAgentTools parses the tools allowlist from YAML frontmatter.
func SubAgentTools(def string) []string {
	for _, line := range strings.Split(def, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "tools:") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(t, "tools:"))
		var out []string
		for _, name := range strings.Split(rest, ",") {
			if n := strings.TrimSpace(name); n != "" {
				out = append(out, n)
			}
		}
		return out
	}
	return nil
}

const subagentSearch = `---
name: gortex-search
description: "Locate code, trace call paths, or map architecture in a fresh context."
tools: mcp__gortex__capabilities, mcp__gortex__explore, mcp__gortex__search, mcp__gortex__read, mcp__gortex__relations, mcp__gortex__trace, mcp__gortex__analyze, mcp__gortex__recall, mcp__gortex__workspace
---

Answer the delegated code-navigation question using only Gortex.

1. Call ` + "`explore`" + ` with ` + "`operation: \"task\"`" + ` and the delegated task.
2. Use ` + "`search`" + ` with ` + "`operation: \"symbols\"`" + ` for names or ` + "`operation: \"text\"`" + ` for exact literals.
3. Use ` + "`read`" + ` with ` + "`operation: \"source\"`" + ` or ` + "`operation: \"summary\"`" + `; do not request unrelated whole files.
4. Use ` + "`relations`" + ` for callers, usages, dependencies, dependents, or implementations. Use ` + "`trace`" + ` for call chains and dataflow.
5. Use ` + "`analyze`" + ` for architecture, communities, processes, or a named graph analysis.
6. Call ` + "`capabilities`" + ` with ` + "`detail: \"schema\"`" + ` if an operation's exact arguments are not visible.

Return the answer first, then symbol IDs and file:line evidence, then caveats. Do not dump raw tool output.

## Worktree and branch routing

` + profiles.WorktreeBranchRoutingPolicy

const subagentImpact = `---
name: gortex-impact
description: "Assess a change's blast radius, contracts, guards, and tests."
tools: mcp__gortex__capabilities, mcp__gortex__explore, mcp__gortex__search, mcp__gortex__read, mcp__gortex__relations, mcp__gortex__trace, mcp__gortex__analyze, mcp__gortex__change, mcp__gortex__recall, mcp__gortex__workspace
---

Convert the delegated change into a concise, graph-grounded impact report. Do not mutate files.

1. Resolve the working set with ` + "`explore`" + `; use ` + "`search`" + ` and ` + "`read`" + ` only to disambiguate targets.
2. Call ` + "`change`" + ` with ` + "`operation: \"impact\"`" + `. For signature changes also call ` + "`operation: \"verify\"`" + `.
3. Use ` + "`relations`" + ` for callers, usages, and dependents; use ` + "`analyze`" + ` with ` + "`kind: \"contracts\"`" + ` when a boundary may change.
4. Call ` + "`change`" + ` with ` + "`operation: \"guards\"`" + ` and ` + "`operation: \"tests\"`" + `.
5. Call ` + "`capabilities`" + ` with ` + "`detail: \"schema\"`" + ` if an operation's exact arguments are not visible.

Return: one-line safe/risky/breaking verdict; broken callers, contracts, or guards with file:line; then exact tests to run.

## Worktree and branch routing

` + profiles.WorktreeBranchRoutingPolicy
