// Package agents holds the content artifacts every agent adapter consumes:
// slash-command bodies, skill playbooks, sub-agent definitions, and the
// instructions block appended to a host's rule file. One copy of each body
// serves every host; an adapter supplies only its own frontmatter dialect.
//
// The bodies are in commands_bodies.go; this file holds the composed blocks
// and the shared rule sections they embed.
package agents

import (
	"github.com/zzet/gortex/internal/profiles"
)

// worktreeBranchRoutingRules is appended to every slash-command and skill
// body so the model knows how to route across worktree branches.
const worktreeBranchRoutingRules = `
## Worktree and branch routing

` + profiles.WorktreeBranchRoutingPolicy

// nativeMCPRules is the shared "Required behavior" section appended to
// every slash-command and skill body.
const nativeMCPRules = `
## Required behavior

- Native Gortex MCP is mandatory for this MCP-configured workflow. If its callable tools are missing, report a Gortex MCP integration failure and stop; do not start a daemon or switch to a CLI/shell fallback.
- Do not substitute shell reads, file search, or Git plumbing.
- Start task-shaped work with ` + "`explore`" + `. For one already-known symbol, start with ` + "`search`" + `.
- Use ` + "`change`" + ` before a mutation and again after it. Write only through ` + "`edit`" + ` or ` + "`refactor`" + `.
- Call ` + "`capabilities({domain: \"<tool>\", operation: \"<operation>\", detail: \"schema\"})`" + ` only when an operation's exact arguments are unclear.
- Report graph-backed paths and symbol IDs. Never invent a result when an operation returns no match.
` + worktreeBranchRoutingRules

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

// InstructionsWithCommands is InstructionsBody followed by an index of
// the slash commands this pack installs. It is what a doc-aware adapter
// appends to its host's rule file — CLAUDE.md, AGENTS.md, or whatever
// that host reads. Callers detect an existing copy with
// InstructionsSentinel, which is shared so idempotency holds across
// adapters writing to the same file.
const InstructionsWithCommands = InstructionsBody + `
## Gortex slash commands

Discovery & analysis: ` + "`/gortex-guide`" + `, ` + "`/gortex-explore`" + `, ` + "`/gortex-debug`" + `, ` + "`/gortex-impact`" + `, ` + "`/gortex-dataflow-trace`" + `, ` + "`/gortex-cross-repo-usage`" + `, ` + "`/gortex-co-change`" + `, ` + "`/gortex-onboarding`" + `

Refactor & edit (enforce tool-call order): ` + "`/gortex-refactor`" + `, ` + "`/gortex-safe-edit`" + `, ` + "`/gortex-rename`" + `, ` + "`/gortex-extract-function`" + `, ` + "`/gortex-fix-all`" + `, ` + "`/gortex-add-test`" + `

Review & operate: ` + "`/gortex-pr-review`" + `, ` + "`/gortex-architecture-review`" + `, ` + "`/gortex-quality-audit`" + `, ` + "`/gortex-incident-investigation`" + `, ` + "`/gortex-episode-replay`" + `

Follow each command's ordered MCP workflow. Use ` + "`explore`" + ` first for task-shaped work, ` + "`change`" + ` before and after mutations, and ` + "`edit`" + ` or ` + "`refactor`" + ` for writes. Call ` + "`capabilities`" + ` only when an operation's exact arguments are unclear.
`
