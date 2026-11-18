# Per-community skills & agent usage

## Usage with Claude Code

After `gortex install` (once per machine) and `gortex init` (once per repo), Claude Code automatically starts Gortex via `.mcp.json`. The agent gets:

- **Slash commands (20):** installed to `~/.claude/commands/` by `gortex install`. Three groups:
  - *Discovery & analysis (8)* — `/gortex-guide`, `/gortex-explore`, `/gortex-debug`, `/gortex-impact`, `/gortex-dataflow-trace`, `/gortex-cross-repo-usage`, `/gortex-co-change`, `/gortex-onboarding`
  - *Refactor & edit (enforce tool-call order) (6)* — `/gortex-refactor`, `/gortex-safe-edit`, `/gortex-rename`, `/gortex-extract-function`, `/gortex-fix-all`, `/gortex-add-test`. These wrap the speculative-execution (`preview_edit` / `simulate_chain`) and LSP code-actions (`get_code_actions` / `apply_code_action` / `fix_all_in_file`) paths so the agent does not bypass the safety steps by calling `Edit` / `Write` directly.
  - *Review & operate (graph-grounded playbooks) (6)* — `/gortex-pr-review`, `/gortex-pr-review-agent`, `/gortex-architecture-review`, `/gortex-quality-audit`, `/gortex-incident-investigation`, `/gortex-episode-replay`. These wrap the discovery + impact + memory surfaces into ordered playbooks so postmortems, audits, and PR reviews are graph-grounded.
- **Tool-usage skills (21):** the same 20 plus `gortex-cli` (skill only — it teaches the shell verbs for a harness with no MCP transport), installed as model-invoked skills to `~/.claude/skills/` by `gortex install` — one copy per user, used across every repo
- **Sub-agents (2):** installed to `~/.claude/agents/` by `gortex install`. Claude Code auto-routes matching prompts to them; each runs in a fresh context window and returns a single summary, keeping the parent's context clean. Tool allowlists are pinned to gortex graph tools only — Bash / Grep / Glob are unavailable to the sub-agent by construction.
  - `gortex-search` — locate code, trace call paths, explore architecture
  - `gortex-impact` — assess blast radius before editing (`verify_change`, `simulate_chain`, `check_guards`, `get_test_targets`)
- **PreToolUse hook:** automatic graph context + graph-tool suggestions on Read/Grep/Glob. The posture is selectable via `gortex install --hook-mode` — `deny` (default), `enrich`, `consult-unlock` (deny fallback reads only until the graph has been queried once this session), or `nudge` (a rate-limited soft reminder instead of a hard deny). Gortex's own MCP tools are auto-approved under the host's permissive permission modes
- **SessionStart hook:** orientation snapshot at session start; after a compaction (`source="compact"`) it also carries the condensed post-compaction briefing so the agent resumes without re-exploring. Claude Code discards PreCompact output, so no PreCompact hook is installed
- **Stop hook:** post-task diagnostics — tests to run, guard violations, dead code, and contract issues on the changed symbols — injected as context before the agent hands off
- **AGENTS.md + CLAUDE.md:** `gortex init --skills` writes the marker-guarded community routing once to canonical `AGENTS.md`; Claude Code consumes it through one `@AGENTS.md` import in `CLAUDE.md`, whose managed block remains reserved for the per-repo `--analyze` overview

## The `gortex-cli` skill — a zero-schema consumption path

Alongside the MCP transport, `gortex install` writes a single user-level skill to `~/.claude/skills/gortex-cli/SKILL.md` that drives the same Gortex workflow entirely through `gortex` **shell verbs** — with no MCP server mounted and no tool schemas loaded into the model's context. It is a first-class consumption pattern, not a fallback:

- **One copy per user.** Like the other model-invoked skills it lives once at `~/.claude/skills/`, used across every repo.
- **No transport, no baseline tax.** Because nothing is published over MCP on this path, the agent pays zero context for tool schemas until it actually runs a verb. `gortex tools receipt` is the auditable record of that — it reports `registered_tool_schemas: 0`.
- **The full workflow in shell.** The skill routes the agent through `gortex` verbs that map 1:1 onto the MCP tools: `gortex call <tool>` (any tool by name), `gortex tools search` / `tools list` (discovery), `gortex edit context|verify|plan|preview|simulate|batch|apply|symbol|rename|guards|tests|contract|safe-delete` (the edit-safety surface), `gortex memory surface|store|recall|note|notes|distill` (session + durable memory), and `gortex analyze` / `flow` / `taint` / `clones` / `feedback`. The verb reference lives in [`cli.md`](cli.md#full-tool-surface-from-the-cli).
- **Same handlers, same daemon.** Each verb is a thin shell over one MCP tool on the daemon that owns the repo; the daemon dispatches the call by name, so the CLI reaches the **full** surface (including tools that are otherwise deferred behind `tools_search`) even under the lean `core` preset.

The trade-off versus an MCP install — push notifications and per-session overlay shadow graphs in exchange for a zero-schema baseline — is laid out in [`cli.md`](cli.md#choosing-a-consumption-path). The edit-safety and memory workflows are identical on both paths.

## Usage with other agents

`gortex install` (user-level) and `gortex init` (repo-level) together auto-detect and configure 19 other AI coding assistants — Kiro, Cursor, VS Code / Copilot, GitHub Copilot CLI, Windsurf, Continue.dev, Cline, OpenCode, Antigravity, Codex CLI, Gemini CLI, Zed, Aider, Kilo Code, OpenClaw, Hermes, Kimi, Pi, oh-my-pi. Each adapter writes only when its host is present on the machine, and every re-run is idempotent.

Four hosts read the same skill bodies Claude Code gets, each re-wrapped in its own frontmatter: Claude Code, Codex CLI, OpenCode and GitHub Copilot CLI. Codex and OpenCode share the cross-agent `~/.agents/skills` root, so one write serves both. For the remaining hosts, MCP tool descriptions carry the teaching and `gortex init` adds only a per-repo community-routing block — no duplicated instructions blocks in every repo.

- **Adapter matrix + per-agent schema notes:** [`agents.md`](agents.md)
- **Audit what's currently configured:** `gortex doctor` (zero-op; `--json` for CI consumers)
- **Constrain setup:** `gortex init --agents=claude-code,cursor` or `--agents-skip=antigravity` (same flags accepted by `gortex install`)
- **CI / scripted install:** `gortex install --yes --json` then `gortex init --yes --json --dry-run`

## Per-community skills

`gortex init --skills` (default on) analyzes your codebase, detects functional communities via Louvain clustering, and generates targeted SKILL.md files that Claude Code auto-discovers:

```bash
# Runs as part of `gortex init` by default — community generation is folded in
gortex init

# Tune or disable:
gortex init --skills-min-size 5 --skills-max 10
gortex init --no-skills
```

Each generated skill includes:

- **Community metadata** — size, file count, cohesion score
- **Key files table** — files and their symbols
- **Entry points** — main functions, handlers, controllers detected via process analysis
- **Cross-community connections** — which other areas this community interacts with
- **MCP tool invocations** — pre-written `get_communities`, `smart_context`, `find_usages` calls

For Claude Code, skills are written to `.claude/skills/generated/<DirName>/SKILL.md`, while the routing table is inserted once into canonical `AGENTS.md` between `<!-- gortex:communities:start/end -->` markers. `CLAUDE.md` imports that file with `@AGENTS.md`. Every other detected agent gets the same routing table inside its per-repo instructions surface (`AGENTS.md` for Codex/OpenCode, `.windsurfrules` for Windsurf, `GEMINI.md` for Gemini CLI, `.cursor/rules/gortex-communities.mdc` for Cursor, etc.) — so the routing is consistent across tools on the same repo without duplicating the Claude copy.
