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
// The content comes from the parent agents package; the names below are
// this package's handles on it, spelled the way Claude Code's artifacts
// are named.
package claudecode

import "github.com/zzet/gortex/internal/agents"

// ProjectMCPJSON is the starter content for a project's .mcp.json
// when no file exists yet.
const ProjectMCPJSON = agents.ProjectMCPJSON

// ClaudeMdBlock is the canonical "use Gortex tools instead of
// Read/Grep" instructions appended to a project's CLAUDE.md. The byte
// sequence must match what previous releases wrote, or the idempotency
// check would misfire on re-runs.
const ClaudeMdBlock = agents.InstructionsWithCommands

// ClaudeMdSentinel is the substring used to detect whether
// ClaudeMdBlock has already been appended to a project's CLAUDE.md.
// Aliased to the shared sentinel so idempotency works across adapters
// writing to the same file (e.g. AGENTS.md shared by Codex + Opencode).
const ClaudeMdSentinel = agents.InstructionsSentinel

// SlashCommands maps the filename under .claude/commands/ to its
// markdown content. Each file is a slash command Claude Code
// auto-discovers.
var SlashCommands = agents.SlashCommands

// GlobalSkills maps the directory name under ~/.claude/skills/ to
// the SKILL.md body. Skill files get YAML frontmatter so Claude Code
// can show them in its skill picker.
var GlobalSkills = agents.GlobalSkills
