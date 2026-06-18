# Agent Integrations

`gortex install` (once per machine) and `gortex init` (once per repo)
auto-configure Gortex for every AI coding assistant detected on your
machine. Sixteen adapters ship today.

- `gortex install` writes user-level machinery: `~/.claude.json` MCP,
  `~/.claude/skills/gortex-*`, `~/.claude/commands/gortex-*.md`,
  `~/.gemini/antigravity/` Knowledge Items, user-level Claude Code
  hooks.
- `gortex init` writes per-repo machinery: `.mcp.json`, per-agent
  MCP configs (`.cursor/mcp.json`, `.vscode/mcp.json`, …), repo-local
  Claude Code hooks, per-agent marker-guarded community-routing
  blocks, and `.claude/skills/generated/` per-community SKILL.md.

Run `gortex init doctor` to see what's currently configured. Both
commands accept `--agents=<csv>` to constrain setup and
`--agents-skip=<csv>` to exclude an adapter.

## Adapter matrix

| Name            | What gets written                                                                               | Mode       | Docs link                                                           |
| --------------- | ----------------------------------------------------------------------------------------------- | ---------- | ------------------------------------------------------------------- |
| `claude-code`   | `.mcp.json`, `.claude/*`, `CLAUDE.md`, `.claude/skills/generated/*`, `~/.claude/skills/gortex-*`, `~/.claude/commands/gortex-*.md`, `~/.claude.json` | both       | https://docs.claude.com/en/docs/claude-code/overview                |
| `aider`         | `.aiderignore` block, `CONVENTIONS.md` communities block                                        | project    | https://aider.chat/docs/config/aider_conf.html                      |
| `antigravity`   | `~/.gemini/antigravity/mcp_config.json` + Knowledge Item                                        | user       | https://antigravity.google/docs/mcp                                 |
| `cline`         | `cline_mcp_settings.json` (per VS Code / Cursor globalStorage), `.clinerules/gortex-communities.md` | both     | https://docs.cline.bot/mcp/mcp-overview                             |
| `codex`         | `~/.codex/config.toml` (`[mcp_servers.gortex]`), `AGENTS.md` communities block                  | both       | https://developers.openai.com/codex/mcp                             |
| `continue`      | `.continue/mcpServers/gortex.json`, `.continue/rules/gortex-communities.md`                     | project    | https://docs.continue.dev/customize/deep-dives/mcp                  |
| `cursor`        | `.cursor/mcp.json` (project) or `~/.cursor/mcp.json`, `.cursor/rules/gortex-communities.mdc`    | both       | https://docs.cursor.com/en/context/mcp                              |
| `gemini`        | `.gemini/settings.json` or `~/.gemini/settings.json`, `GEMINI.md` communities block             | both       | https://geminicli.com/docs/tools/mcp-server/                        |
| `hermes`        | `~/.hermes/config.yaml` + `profiles/*/config.yaml` (`mcp_servers`), hooks, `~/.hermes/skills/gortex/SKILL.md` | user       | https://hermes-agent.nousresearch.com/docs/user-guide/features/mcp  |
| `kilocode`      | `mcp_settings.json` + `.kilocode/mcp.json`, `.kilocoderules` communities block                  | both       | https://kilo.ai/docs/features/mcp/using-mcp-in-kilo-code            |
| `kiro`          | `.kiro/settings/mcp.json` + steering/hooks or user-level                                        | both       | https://kiro.dev/docs/mcp/configuration                             |
| `oh-my-pi`      | `.omp/mcp.json`                                                                                 | project    | https://github.com/can1357/oh-my-pi/blob/main/docs/mcp-config.md   |
| `opencode`      | `.opencode/config.json`, `AGENTS.md` communities block                                          | project    | https://opencode.ai/docs/mcp                                        |
| `openclaw`      | `~/.openclaw/openclaw.json` (`mcp.servers.gortex`)                                              | user       | https://docs.openclaw.ai/cli/mcp                                    |
| `vscode`        | `.vscode/mcp.json` (`servers` key, 1.102+), `.github/copilot-instructions.md` communities block | project    | https://code.visualstudio.com/docs/copilot/chat/mcp-servers         |
| `windsurf`      | `~/.codeium/mcp_config.json`, `.windsurfrules` communities block                                | both       | https://docs.windsurf.com/plugins/cascade/mcp                       |
| `zed`           | OS-specific `settings.json` (`context_servers`), `.rules` communities block                     | both       | https://zed.dev/docs/ai/mcp                                         |

Mode legend: **project** writes inside the repo (`gortex init` only);
**user** writes under `$HOME` (`gortex install` only); **both** means
the adapter splits: `gortex install` writes the user-level pieces and
`gortex init` writes the repo-level pieces.

Tool-usage guidance (how to prefer graph tools over `Read`/`Grep`) no
longer gets duplicated into every repo. For Claude Code and
Antigravity — the two adapters whose upstream tool exposes a
user-level instructions surface — the guidance lives once per user
(installed by `gortex install`). For the other 13, MCP tool
descriptions carry the teaching. Only codebase-derived community
routing lands in per-repo instructions files.

## Subagent tool propagation

A *subagent* runs in a fresh context window with its own scoped tool
allowlist. Whether a subagent can use Gortex's MCP tools depends
entirely on whether that allowlist names them — a subagent does **not**
automatically inherit every tool the parent has.

- **Claude Code** has a first-class subagent concept with per-subagent
  tool allowlists, and Gortex propagates explicitly. `gortex install`
  writes two subagent definitions to `~/.claude/agents/` —
  `gortex-search` (locate / trace / explore) and `gortex-impact`
  (blast-radius / verification) — each with an explicit
  `tools: mcp__gortex__…` frontmatter listing exactly the graph tools it
  needs. The allowlist is **graph-only by construction**: it contains no
  `Bash`/`Grep`/`Glob`, so a spawned subagent cannot escape the graph.
  A `PreToolUse` Task hook additionally briefs spawned subagents. The
  allowlists are validated in CI (`subagents_test.go`) so a tool rename
  can't silently drop a subagent's access. Programmatic access:
  `claudecode.SubAgentTools(def)` parses an allowlist out of a definition.
- **Session-inheriting hosts** (e.g. `opencode`): subagents inherit the
  *parent's* MCP session at the client level, so they see Gortex tools
  transparently **only if the host propagates the session to the child**.
  This is a client-side behaviour — Gortex configures the MCP server for
  the host but cannot force a client to share its session with a subagent.
- **Hosts with no subagent concept**: the question does not arise; the
  single agent already holds the configured Gortex tools.

If a subagent reports it cannot see `mcp__gortex__*` tools, check the
subagent's own tool allowlist first — that is where propagation is
decided, not the server.

## Common CLI flags

```
# Machine-wide (run once)
gortex install                       # user-level MCP, skills, slash commands, hooks
gortex install --start --track       # also spawn daemon + track current dir
gortex install --agents=claude-code  # constrain to one adapter
gortex install --dry-run --json      # plan-only, JSON report

# Per repo (run in each project)
gortex init                          # interactive: only asks about hooks
gortex init --yes                    # skip prompt, use defaults
gortex init --analyze                # include a richer CLAUDE.md codebase overview
gortex init --no-skills              # skip community-routing generation
gortex init --skills-min-size 5 --skills-max 10
gortex init --agents=claude-code,cursor     # allow-list
gortex init --agents-skip=antigravity       # block-list
gortex init --dry-run --json         # plan, emit JSON report
gortex init --force                  # overwrite merge-preserved keys
gortex init --hooks-only             # refresh Claude Code hooks only

# Observe-only
gortex init doctor                   # read-only state report
gortex init doctor --json            # machine-readable report
```

## Adapter contract

Every adapter under `internal/agents/<name>/` implements the
`agents.Adapter` interface:

- `Name()` — stable identifier used by `--agents`
- `DocsURL()` — upstream docs link (for `--json` reports)
- `Detect(env)` — cheap filesystem/`PATH` probe; never writes
- `Plan(env)` — returns the set of files Apply *would* touch,
  without writing
- `Apply(env, opts)` — performs the writes, respecting
  `opts.DryRun` and `opts.Force`

Every write funnels through `agents.WriteIfNotExists`,
`agents.MergeJSON`, or `agents.MergeTOML`. Those helpers provide:

- Atomic temp-file-plus-rename — a partial failure can't leave a
  half-written config
- Uniform dry-run handling — no adapter has its own bool
- Structured `FileAction` results — `--json` and doctor speak the
  same vocabulary
- Malformed-file backup — a user with broken JSON gets a `.bak`
  sibling instead of silent data loss

## Per-agent notes

### claude-code

The primary integration, split across the two commands.

**`gortex install` (user-level, once per machine)** writes:

- `~/.claude.json` — MCP stanza pointing at `gortex mcp`
- `~/.claude/settings.local.json` — user-level Claude Code hooks
  (unless `--no-hooks`)
- `~/.claude/skills/gortex-*/SKILL.md` — curated tool-usage skills
  (`gortex-guide`, `gortex-explore`, `gortex-debug`, `gortex-impact`,
  `gortex-refactor`), one source of truth per user instead of copied
  into every repo
- `~/.claude/commands/gortex-*.md` — slash commands
  (`/gortex-guide`, etc.), also codebase-agnostic and therefore
  user-level

**`gortex init` (per repo)** writes:

- `.mcp.json` — project MCP stanza
- `.claude/settings.json` — MCP permissions merge
  (`mcp__gortex__*` allowlist)
- `.claude/settings.local.json` — repo-local hooks (unless
  `--no-hooks`)
- `CLAUDE.md` — marker-guarded block (`<!-- gortex:communities:start -->`
  / `<!-- gortex:communities:end -->`) carrying the codebase overview
  (via `--analyze`) and the community routing (via `--skills`,
  default on); if neither flag produces content, no block is written
- `.claude/skills/generated/<DirName>/SKILL.md` — one per detected
  community, regenerated each run so the content tracks the graph

Hooks installed today: **PreToolUse**, **PreCompact**, **Stop**,
**SessionStart** — SessionStart fires on new or resumed sessions to
prime the first turn with graph orientation; PreCompact fires on
summary boundaries.

### aider

Aider has no native MCP client today. We install an `.aiderignore`
block telling Aider to skip Gortex's cache dirs so it doesn't waste
tokens ingesting them.

### antigravity

Two artifacts: a native MCP registration at
`~/.gemini/antigravity/mcp_config.json` (new in 2026) plus a
Knowledge Item at `~/.gemini/antigravity/knowledge/gortex-workflow/`
that documents how to use Gortex via `run_command`. The KI stays
because it gives workflow intent the raw MCP registration doesn't.

### cline

Extension ID `saoudrizwan.claude-dev`. We write
`cline_mcp_settings.json` to each VS Code and Cursor globalStorage
directory that exists. Auto-approval field is `alwaysAllow` (not
`autoApprove`, which is a different field in the schema).

### codex

OpenAI Codex CLI stores config in `~/.codex/config.toml`. We
upsert a `[mcp_servers.gortex]` table there.

### continue

Continue.dev still accepts JSON block files under
`.continue/mcpServers/` even though its native format is YAML with
metadata headers. We write the JSON form today for zero-dependency
simplicity; upgrading to the YAML+metadata form is tracked.

### cursor

Project-level `.cursor/mcp.json` (written by `gortex init`);
`~/.cursor/mcp.json` (written by `gortex install`). Env key is `env`
(not `environment`). Cursor does not expose a user-level rules
surface, so community-routing lives per-repo at
`.cursor/rules/gortex-communities.mdc` — regenerated each `gortex
init` run so it tracks the current graph.

### gemini

Gemini CLI reads `.gemini/settings.json` (project) and
`~/.gemini/settings.json` (user). Distinct from the antigravity
adapter despite the shared `~/.gemini/` prefix.

### kilocode

Kilo Code is a Cline fork with its own globalStorage key
(`kilocode.kilo`). We write to every candidate globalStorage path
(VS Code + Cursor + Insiders variants) plus `.kilocode/mcp.json`
when a project-level directory exists.

### kiro

Workspace `.kiro/settings/mcp.json` + steering/hooks via `gortex
init`; `~/.kiro/settings/mcp.json` via `gortex install` (steering
and hooks are project-scoped in Kiro's runtime so they stay per-repo).
The MCP entry carries `autoApprove` and explicit `disabled: false`
keys Kiro's UI expects.

### opencode

OpenCode's schema differs from the canonical form: top-level
`mcp.<name>` (not `mcpServers`), `command` is an array, env key
is `environment`, plus a `$schema` pointer at
`https://opencode.ai/config.json`.

### openclaw

Config lives at `~/.openclaw/openclaw.json`. OpenClaw advertises
JSON5 but accepts strict JSON, which is what we emit. Servers go
under `mcp.servers.<name>`.

### vscode

**Schema changed in 2026.** VS Code's native MCP runtime (1.102+)
uses `{"servers": {...}}`, not the Copilot-Chat legacy
`{"mcpServers": {...}}`. `type` is inferred from `command`
presence, so stdio servers don't need a type field.

### windsurf

**Path changed in 2026.** Current canonical path is
`~/.codeium/mcp_config.json`. The legacy
`~/.codeium/windsurf/mcp_config.json` is left in place unless
`--force` is passed, which removes it as part of the migration.

### zed

Zed calls its MCP registry `context_servers`, not `mcpServers`.
Settings file is platform-specific:

- macOS: `~/Library/Application Support/Zed/settings.json`
- Linux: `~/.config/zed/settings.json`
- Windows: `%APPDATA%\Zed\settings.json`

Each entry takes `source: "custom"` alongside the usual
`command/args/env`.

## Troubleshooting

- **Config file malformed**: If an adapter finds invalid JSON/TOML
  it writes a `.bak` sibling before replacing the file with the
  merged result. Check alongside the original.
- **Hook command points at `/tmp/…`**: `gortex init` heals stale
  ephemeral paths automatically on re-run.
- **"Already configured" but tools missing**: re-run with
  `--force` to overwrite our entries; or delete the `gortex`
  stanza from the config and re-run without `--force`.
- **CI / scripted install**: pass `--yes --json` and parse the
  report.
