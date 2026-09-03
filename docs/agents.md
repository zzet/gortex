# Agent Integrations

`gortex install` (once per machine) and `gortex init` (once per repo)
auto-configure Gortex for every AI coding assistant detected on your
machine. 20 adapters ship today.

- `gortex install` writes user-level machinery: `~/.claude.json` MCP,
  `~/.claude/skills/gortex-*`, `~/.claude/commands/gortex-*.md`,
  `~/.gemini/antigravity/` Knowledge Items, and user-level hooks.
- `gortex init` writes per-repo machinery: `.mcp.json`, per-agent
  MCP configs (`.cursor/mcp.json`, `.vscode/mcp.json`, …), repo-local
  hooks where supported, marker-guarded community-routing surfaces
  (`AGENTS.md` is canonical for Claude Code), and
  `.claude/skills/generated/` per-community SKILL.md.

Run `gortex doctor` to see what's currently configured — and, past what a
config file can prove, whether the hooks it declares are actually running. Both
commands accept `--agents=<csv>` to constrain setup and
`--agents-skip=<csv>` to exclude an adapter.

## Worktrees and branch views

Agent sessions inherit a view from their CWD. A linked worktree is discovered automatically as an overlay over its Git family's designated primary; agents should not explicitly track it unless the user asks for a dedicated logical graph. Overall queries remain coherent: overlay and primary results are composed, overlay data wins only for duplicate logical identities, and tombstones hide deleted base identities.

Agents must inspect response freshness. `exact: false` means the requested view was not served, and every fallback is read-only. Use `require_exact: true` to reject fallback, `require_fresh: true` to wait for current filesystem state, and an absolute RFC3339 `wait_deadline` to bound the wait. When CWD is not the target, pass an explicit selector such as `view:{kind:"worktree",checkout_id:"…"}` or `view:{kind:"git_ref",value:"refs/heads/release",graph_id:"…"}`. Inactive refs provide immutable committed source and structural graph only—no working-copy LSP, `search.text`, or edits. Exact worktree edits are supported through the approved coordinator-backed path.

Checkout removal and primary changes are destructive administration. Preview primary closure, family forget, and `set-primary` effects and obtain explicit user confirmation before confirming them.

## Adapter matrix

| Name            | What gets written                                                                               | Mode       | Docs link                                                           |
| --------------- | ----------------------------------------------------------------------------------------------- | ---------- | ------------------------------------------------------------------- |
| `claude-code`   | `.mcp.json`, `.claude/*`, `AGENTS.md` communities block, `CLAUDE.md` import/overview, `.claude/skills/generated/*`, `~/.claude/skills/gortex-*`, `~/.claude/commands/gortex-*.md`, `~/.claude.json` | both       | https://docs.claude.com/en/docs/claude-code/overview                |
| `aider`         | `.aiderignore` block, `CONVENTIONS.md` communities block                                        | project    | https://aider.chat/docs/config/aider_conf.html                      |
| `antigravity`   | `~/.gemini/antigravity/mcp_config.json` + Knowledge Item                                        | user       | https://antigravity.google/docs/mcp                                 |
| `cline`         | `cline_mcp_settings.json` (per VS Code / Cursor globalStorage), `.clinerules/gortex-communities.md` | both     | https://docs.cline.bot/mcp/mcp-overview                             |
| `codex`         | `~/.codex/config.toml` (`[mcp_servers.gortex]` + `SessionStart`, `UserPromptSubmit`, Bash/Gortex-read `PreToolUse`, and Bash/`apply_patch` `PostToolUse` hooks), `~/.codex/AGENTS.md` rule block, `~/.agents/skills/gortex-*`, `~/.codex/agents/gortex-*.toml`, repo `AGENTS.md` communities block, repo `.agents/skills/gortex-*` | both       | https://learn.chatgpt.com/docs/extend/mcp                           |
| `copilot-cli`   | `~/.copilot/mcp-config.json` (`mcpServers`), `~/.copilot/copilot-instructions.md` rule block, `~/.copilot/skills/gortex-*`, `~/.copilot/agents/gortex-*.agent.md`, `~/.copilot/hooks/gortex.json`, repo `.github/copilot-instructions.md` communities block, repo `.github/skills/gortex-*` | both       | https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/add-mcp-servers |
| `continue`      | `.continue/mcpServers/gortex.json`, `.continue/rules/gortex-communities.md`                     | project    | https://docs.continue.dev/customize/deep-dives/mcp                  |
| `cursor`        | `.cursor/mcp.json` (project) or `~/.cursor/mcp.json`, `.cursor/rules/gortex-communities.mdc`    | both       | https://docs.cursor.com/en/context/mcp                              |
| `gemini`        | `.gemini/settings.json` or `~/.gemini/settings.json`, `GEMINI.md` communities block             | both       | https://geminicli.com/docs/tools/mcp-server/                        |
| `hermes`        | `~/.hermes/config.yaml` + `profiles/*/config.yaml` (`mcp_servers`), hooks, `~/.hermes/skills/gortex/SKILL.md` | user       | https://hermes-agent.nousresearch.com/docs/user-guide/features/mcp  |
| `kilocode`      | `mcp_settings.json` + `.kilocode/mcp.json`, `.kilocoderules` communities block                  | both       | https://kilo.ai/docs/features/mcp/using-mcp-in-kilo-code            |
| `kimi`          | `.kimi-code/mcp.json` (project) or `~/.kimi-code/mcp.json` + `~/.kimi-code/config.toml` (`UserPromptSubmit` / `PreToolUse` / `Stop` / `SubagentStart` hooks) | both       | https://www.kimi.com/code/docs/en/kimi-code-cli/customization/hooks.html |
| `kiro`          | `.kiro/settings/mcp.json` + steering/hooks or user-level                                        | both       | https://kiro.dev/docs/mcp/configuration                             |
| `oh-my-pi`      | `.omp/mcp.json`                                                                                 | project    | https://github.com/can1357/oh-my-pi/blob/main/docs/mcp-config.md   |
| `opencode`      | `opencode.json` (or existing `opencode.jsonc`) and `~/.config/opencode/opencode.json` MCP stanzas, `AGENTS.md` communities block, repo `.opencode/skills/gortex-*`, `~/.config/opencode/skills/gortex-*`, `~/.config/opencode/commands/gortex-*.md`, `~/.config/opencode/plugin/gortex.js` | both       | https://opencode.ai/docs/mcp                                        |
| `openclaw`      | `~/.openclaw/openclaw.json` (`mcp.servers.gortex`)                                              | user       | https://docs.openclaw.ai/cli/mcp                                    |
| `pi`            | `.pi/extensions/gortex/index.ts` (project) or `~/.pi/agent/extensions/gortex/index.ts`; `AGENTS.md` communities block only when `--skills` | both | https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/extensions.md |
| `vscode`        | `.vscode/mcp.json` (`servers` key, 1.102+), `.github/copilot-instructions.md` communities block | project    | https://code.visualstudio.com/docs/copilot/chat/mcp-servers         |
| `windsurf`      | `~/.codeium/mcp_config.json`, `.windsurfrules` communities block                                | both       | https://docs.windsurf.com/plugins/cascade/mcp                       |
| `zed`           | OS-specific `settings.json` (`context_servers`), `.rules` communities block                     | both       | https://zed.dev/docs/ai/mcp                                         |

Mode legend: **project** writes inside the repo (`gortex init` only);
**user** writes under `$HOME` (`gortex install` only); **both** means
the adapter splits: `gortex install` writes the user-level pieces and
`gortex init` writes the repo-level pieces.

Note that `copilot-cli` is the standalone `copilot` binary
(`npm i -g @github/copilot`). Copilot inside VS Code is the separate
`vscode` adapter, and the `copilot` LLM provider in `.gortex.yaml` is a
third, unrelated thing — see [`llm.md`](llm.md).

## Skill and hook surfaces per host

Four hosts receive the same 21 skill bodies, each re-wrapped in that
host's own frontmatter from one authoring source. The curated pack is
always user-level, so `gortex init` never adds Gortex markdown to a
repo that teammates have to review; only per-community skills, which
describe that specific codebase, are written into the repo.

| Host | Skills | Slash commands | Sub-agents | Enforcement hooks |
| --- | --- | --- | --- | --- |
| Claude Code | `~/.claude/skills/` | `~/.claude/commands/` | `~/.claude/agents/` | native, `deny` / `enrich` postures |
| Codex CLI | `~/.agents/skills/` | **no** — see below | `~/.codex/agents/*.toml` | native, 4 events |
| OpenCode | `~/.config/opencode/skills/` | `~/.config/opencode/commands/` | not written | **no hook system** — JS plugin bridge |
| GitHub Copilot CLI | `~/.copilot/skills/` | **no** — see below | `~/.copilot/agents/` | native, 4 of 14 events |

The gaps are the hosts', not ours:

- **Codex has no slash-command surface worth targeting.** `$CODEX_HOME/prompts`
  is deprecated in favour of skills and is user-level only, with no
  project directory. Skills are the substitute.
- **Copilot CLI does not read prompt files.** `.github/prompts/*.prompt.md`
  is a VS Code / Visual Studio surface; the CLI maintainers closed the
  request as superseded by skills.
- **OpenCode has no lifecycle-hook configuration at all.** Its only
  extension point is a JS/TS plugin, so Gortex installs one that speaks
  the same bridge protocol the Pi extension uses. It goes to the
  user-level plugin directory rather than the repo: it is the only
  executable artifact Gortex writes, and the repo-level directory is
  committed.
- **Copilot CLI's `agentStop` carries no context channel** (only a
  decision and a reason), so there is no turn-end briefing on that host.
- **OpenCode sub-agents are not written.** Their `tools` map is keyed by
  tool name and the spelling for an MCP-served tool is unverified; a
  wrong key silently grants nothing.
- **No sub-agent gets a tool allowlist except Claude Code's.** Codex has
  no per-agent allowlist at all, and Copilot's `tools` key expects names
  that Claude's `mcp__gortex__*` spelling does not resolve to. An
  unrecognised entry grants nothing rather than falling back to the full
  toolbox, so both inherit the session's tools instead.

Codex sub-agents are worth one extra note, because the file format has
two ways to fail that leave no error behind. Codex rejects an agent file
outright on a single unknown key, and its `mcp_servers` field is a table
of full server definitions rather than a list of server names — the
intuitive `mcp_servers = ["gortex"]` voids the whole agent. Gortex
therefore emits four keys and no `mcp_servers`; omitting it inherits the
session's servers, which is what we want anyway. Project-scoped
`.codex/agents/` is skipped entirely unless the project is trusted, so
these install user-level only.

Verify a Codex install with `codex doctor`: a rejected agent file shows
up there as a startup warning mentioning `agent role`, while the exit
code stays 0 and everything else loads normally.

`~/.agents/skills` is a shared cross-agent root that both Codex and
OpenCode scan, so one write serves both. Copilot CLI dropped it in
v1.0.66 and needs `~/.copilot/skills`; it also stopped reading
home-level `~/.claude/*` in v1.0.36.

Hook postures differ by host and are worth knowing before debugging a
quiet integration: Codex gates every hook behind per-hook trust
(`/hooks` inside Codex), Copilot CLI fails **closed** on a `preToolUse`
non-zero exit but fails **open** on a timeout, and the OpenCode bridge
fails open on any error so a broken plugin can never break a session.

Tool-usage guidance is agent-neutral. Every named MCP connection receives the
same mandatory, compact workflow in the server initialization response.
Adapters also install that workflow where their host exposes a persistent
rules, skill, or extension surface; the wording names only the 21 public tools
and the exact `gortex call` fallback. Codebase-derived community routing still
lives in per-repo instruction files because it is workspace-specific.

## Compact MCP surface

Every MCP connection with a non-empty `clientInfo.name` receives the
same 21-tool compact surface in `hide` mode by default. The complete
static roster is documented in [`mcp.md`](mcp.md#compact-mcp-surface).
Empty and pre-initialize sessions retain the server default. Explicit
forwarded, operator, or instruction-profile policy wins over the
named-client default. Wire-format negotiation is independent, so an
unknown named client remains on JSON unless it is separately
GCX-capable.

An MCP-capable host MUST call the compact tools directly. In
particular, use `read({target:{file:"..."}})` for a
known source file; it MUST NOT spawn a shell command merely to relay
the same request. A harness that exposes Bash but no native Gortex MCP
functions MUST use the exact CLI mirror instead:

```bash
gortex call read \
  --arg target='{"file":"internal/mcp/server.go"}'

gortex call search \
  --arg operation=symbols \
  --arg query=Server.addTool
```

The CLI accepts the same compact tool names and argument objects; it
does not translate them to legacy names. See the
[`gortex call` reference](cli.md#compact-surface-mirror-for-bash-only-harnesses)
for mutation previews, schema discovery, repository selection, and
the distinction between tool `dry_run` and CLI `--dry`.

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
gortex init --hooks-only             # refresh supported agent hooks only

# Observe-only
gortex doctor                        # config + hook activity + adoption
gortex doctor --json                 # machine-readable report (always complete)
gortex doctor --redact               # safe to paste into an issue
gortex doctor --all                  # include adapters that are not installed
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

Every write funnels through one of the shared helpers, never through
`os.WriteFile` directly:

| Helper | Use it for |
| --- | --- |
| `agents.WriteIfNotExists` | a file the user owns after we create it (never overwritten) |
| `agents.WriteOwnedFile` | a file Gortex regenerates (skips when byte-identical) |
| `agents.MergeJSON` / `MergeTOML` / `MergeYAML` | a config file shared with the host and the user |
| `agents.UpsertMarkedBlock` | a marker-guarded block inside a human-edited markdown file |
| `agents.UpsertMCPServer` / `RemoveMCPServer` | the `gortex` MCP stanza in any config shape |
| `agents.UpsertMCPServerApprovalList` | a per-tool approval list, preserving a user-narrowed one |

Those helpers provide:

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
- `AGENTS.md` — canonical marker-guarded community-routing block
  (`<!-- gortex:communities:start -->` /
  `<!-- gortex:communities:end -->`) when `--skills` is enabled
- `CLAUDE.md` — imports `@AGENTS.md` exactly once when community routing
  is generated; its marker-guarded block carries only the Claude-specific
  codebase overview from `--analyze`
- `.claude/skills/generated/<DirName>/SKILL.md` — one per detected
  community, regenerated each run so the content tracks the graph

Hooks installed today: **PreToolUse**, **PostToolUse**, **PreCompact**,
**Stop**, **SessionStart**, **UserPromptSubmit**, **SubagentStart**,
**SubagentStop**.

SessionStart carries the orientation block. It is registered with no
matcher, so it fires for every source — `startup`, `resume`, `clear`,
`fork`, and `compact` — and the `compact` source additionally receives
the post-compaction snapshot. That placement is deliberate: Claude Code
rebuilds a compacted window from the summary, the most recent exchanges,
and up to five recently-read files that it re-reads and re-injects as
`system-reminder` blocks, and SessionStart is the only compaction-adjacent
event that can inject context to say so. PreCompact cannot — its output
contract is `decision: "block"` only — so gortex's PreCompact handler
records telemetry and deliberately emits nothing.

> **Daemon outage = per-call enforcement stands down.** Every per-call
> surface (PreToolUse denies and graph advisories, PostToolUse follow-ups,
> and the Codex / Kimi / Pi / Hermes mirrors) checks daemon reachability
> first and stays quiet while the daemon socket cannot be dialed — guidance
> that mandates Gortex MCP tools is untrue the moment the daemon cannot
> serve them. The SessionStart briefing announces rule-only enforcement to a
> session that starts mid-outage; a session the outage begins in gets one
> PreToolUse notice (once per session) carrying the same stance. The
> localization terminal deny is daemon-independent and deliberately stays
> live. Enforcement resumes automatically on the next tool call once the
> daemon is back.

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
upsert a `[mcp_servers.gortex]` table there. `gortex install` also merges the
active instruction profile into `~/.codex/AGENTS.md` between
`<!-- gortex:rules:start -->` / `<!-- gortex:rules:end -->` markers — Codex
loads that file into every session, so it is the Codex analogue of
`~/.claude/CLAUDE.md`. The body is inlined rather than `@`-included because
Codex reads AGENTS.md as literal markdown; `gortex instructions switch`
refreshes the copy in place. Skip it with `--no-claude-md`. If
`~/.codex/AGENTS.override.md` exists, Codex reads that instead and the
installer says so.

> **Trust the hooks or they do nothing.** Codex records trust against each
> non-managed hook's current hash and **skips new or changed hooks until you
> review them**. A fresh `gortex install` therefore leaves the Gortex hooks
> configured but inert, and nothing in `config.toml` distinguishes the two
> states. Run `/hooks` inside Codex, review the gortex entries, and trust
> them — and again after any upgrade that changes a hook definition (the
> installer prints a reminder whenever it writes or changes one). Until then
> `SessionStart` never fires, which is the surface that puts the Gortex rule
> in front of the model.

To tell the two states apart, run `gortex doctor`. It correlates the Codex
config, the hook-invocation log, the Codex session transcripts, and the savings
ledger into one verdict — *hooks configured but none has run* is the
untrusted-hook signature, reported as a blocker with the `/hooks` remedy.
`--redact` hashes repo paths and branch names so the report can be pasted into
an issue, `--json` emits it machine-readably, and the exit status is non-zero
when a blocker is found.

Codex takes hook declarations from two files per configuration layer —
`~/.codex/hooks.json` and inline `[hooks]` tables in `~/.codex/config.toml` —
and **merges** them when both are present, running every hook declared in both
once per declaration. Gortex only ever writes the inline TOML representation,
but `gortex doctor` counts both and prints the file each count came from, so a
hooks.json-only setup is never reported as having no hooks. Splitting different
events across the two files is fine and reported as such; only an event declared
in *both* actually runs twice, and that is what doctor warns about — naming the
event, and pointing the cleanup at `hooks.json`, since `gortex install` rewrites
config.toml. `gortex install` and `gortex init --hooks-only` say the same thing
at the moment they would create such a duplicate; the hooks.json entries are
yours, so neither touches them. `[features] hooks = false` turns hook execution off
wholesale and is reported as a blocker in its own right, rather than as an
untrusted-hook signature.

When hooks are enabled
(the default), Codex receives user-level hooks. The default posture remains
advisory. A team can opt into `deny`, `rewrite`, or `suppress` by setting
`GORTEX_CODEX_HOOK_MODE` while running `gortex init --hooks-only`; the selected
posture is written into the managed hook commands.

Current Codex hook coverage:

| Surface | Coverage |
| ------- | -------- |
| `SessionStart` | Matches `startup|resume|clear|compact` and emits graph-tools orientation for new, resumed, cleared, and compacted sessions. |
| Hook-observable local-tool `PreToolUse` terminal gate | Uses one match-all installer-owned hook so an enforceable localization `answer_ready` contract blocks every local tool Codex routes through `PreToolUse`, including Bash, `apply_patch`, MCP tools, and other local function tools. Hosted tools such as `WebSearch`, and specialized paths that opt out of the default hook path, are outside this host boundary. Without a terminal marker it is a local no-op; advisory completion still permits non-navigation contract operations. |
| Bash `PreToolUse` | Advises by default; `deny` hard-blocks graph-detectable fallback reads/searches; `rewrite` converts only an unambiguous indexed `cat <source>` into the exact public `gortex call read` mirror. Compound or ambiguous commands remain advisory. A shell command that would *rewrite* indexed source — `sed -i` / `perl -pi`, a `>` / `>>` redirect, `tee`, or an inline interpreter script opening the path for writing — gets the same redirect Edit and Write get, escalating to a hard block under `GORTEX_HOOK_BLOCK_EDIT`. Writing a path the daemon does not know is a new file and passes through. |
| Gortex MCP `read` `PreToolUse` | Advises broad file/editing-context reads by default; `deny` blocks them; `rewrite` preserves the request and adds `options.compress_bodies=true`. Selector-driven reads with no explicit operation are covered. |
| Bash `PostToolUse` | Adds graph context for grep/search, source reads, and conservative file-list shapes: `find -name`, `fd`, `ls`, `tree -fi`, and `git ls-files`; bounded `sed`/`awk` reads get file graph context. Execution-capable or ambiguous forms are no-ops. |
| `apply_patch` `PostToolUse` | Runs `detect_changes`, extracts affected symbol IDs, then reports tests, guards, and contracts. The completed mutation is never rolled back by the hook. Codex payloads carry no `session_id`, so this path reports the whole working tree with an explicit label instead of attributing the diff to the session. |
| `UserPromptSubmit` | Re-surfaces prompt-relevant indexed symbols on every turn so long sessions do not rely on the initial reminder alone. |
| `gortex init --hooks-only` | Refreshes Codex hooks without rewriting the MCP server config, `AGENTS.md`, or other adapter surfaces. |

Postures:

```bash
# Existing behavior (default).
GORTEX_CODEX_HOOK_MODE=enrich gortex init --hooks-only

# Enforce graph-detectable fallback reads/searches.
GORTEX_CODEX_HOOK_MODE=deny gortex init --hooks-only

# Rewrite only requests whose semantics can be preserved.
GORTEX_CODEX_HOOK_MODE=rewrite gortex init --hooks-only

# Replace enrichable raw PostToolUse results with graph feedback.
GORTEX_CODEX_HOOK_MODE=suppress gortex init --hooks-only
```

Current [Codex hook behavior](https://developers.openai.com/codex/hooks)
implements deny and `updatedInput`. It does not implement the nominal
`suppressOutput` field; the `suppress` posture therefore uses the supported
PostToolUse result-replacement decision and never emits
`suppressOutput`.

Intentional Codex lifecycle gaps are tracked separately from missing tool
coverage:

| Surface | Intentional status |
| ------- | ------------------ |
| `PreCompact` / `PostCompact` | Not installed. `SessionStart(source="compact")` restores the mandatory workflow after compaction without paying for two more lifecycle hooks. |
| `Stop` | Not installed. A Stop hook can continue a turn, so adding it is a separate policy decision rather than implicit mutation handling. |
| Native output-suppression fields | Not emitted while Codex rejects `suppressOutput` and `updatedMCPToolOutput`; the opt-in `suppress` posture uses supported PostToolUse result replacement. |

`apply_patch` mutation handling and opt-in deny/rewrite/suppress postures are
implemented and are no longer intentional gaps.

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

### kimi

Kimi Code CLI keeps MCP servers in `.kimi-code/mcp.json` (project, via
`gortex init`) or `~/.kimi-code/mcp.json` (user, via `gortex install`),
and user-level lifecycle hooks in `~/.kimi-code/config.toml` as a
`[[hooks]]` array. Kimi appends a hook's plain stdout to the model
context on exit 0, so Gortex sends soft guidance as plain text and uses
the documented `hookSpecificOutput.permissionDecision = "deny"` shape
only to block an indexed whole-file read.

Current Kimi hook coverage (all gated to Gortex-enabled projects so the
machine-wide user hook stays inert elsewhere):

| Surface | Coverage |
| ------- | -------- |
| `UserPromptSubmit` | Injects graph symbols relevant to the prompt before the model runs. |
| `PreToolUse` | Redirects native `Read`/`Grep`/`Glob`/`Bash` to graph tools — a hard `deny` for an indexed whole-file read, soft plain-stdout guidance otherwise — plus compact `read` shaping for broad file/editing-context operations. Shell rewrites of indexed source (`sed -i`, `>` redirects, `tee`) are redirected to `edit` / `refactor` on the same terms as `Edit` and `Write`. |
| `Stop` | Runs post-turn diagnostics (changed symbols → test targets, guards, dead code, coverage, contracts) and feeds them back so the agent self-corrects before handoff. |
| `SubagentStart` | Briefs a spawned subagent with `smart_context` results and the tool-swap table so it doesn't default to raw `Read`/`Grep`. |

The diagnostics and briefing hooks reach the daemon over its unix
socket (the hook port's HTTP surface is served only under
`--http-addr`), and every path degrades to a silent no-op when the
payload is malformed, the cwd is outside a Gortex project, or the
daemon is unreachable — so normal Kimi flow is never blocked by the
integration.

### kiro

Workspace `.kiro/settings/mcp.json` + steering/hooks via `gortex
init`; `~/.kiro/settings/mcp.json` via `gortex install` (steering
and hooks are project-scoped in Kiro's runtime so they stay per-repo).
The MCP entry carries `autoApprove` and explicit `disabled: false`
keys Kiro's UI expects.

### opencode

The MCP config is written to a root `opencode.json` (or merged into an
existing `opencode.json` / `opencode.jsonc`) — the files OpenCode
actually reads. It does **not** read `.opencode/config.json`, which
Gortex wrote historically. OpenCode's schema also differs from the
canonical form: top-level `mcp.<name>` (not `mcpServers`), `command` is
an array, env key is `environment`, plus a `$schema` pointer at
`https://opencode.ai/config.json`.

### openclaw

Config lives at `~/.openclaw/openclaw.json`. OpenClaw advertises
JSON5 but accepts strict JSON, which is what we emit. Servers go
under `mcp.servers.<name>`.

### pi

**Pi has no MCP support — by design**, so instead of an `mcpServers`
stanza this adapter ships a self-contained TypeScript extension at
`.pi/extensions/gortex/index.ts` (project) or
`~/.pi/agent/extensions/gortex/index.ts` (global). The extension is a
thin MCP client of its own: it spawns one persistent `gortex mcp`
child per session, registers the daemon's tool surface natively, 
and re-creates the same read-discipline enforcement the other
agents get.

**Project-local extensions require a one-time trust confirmation** the
first time Pi opens the repo — nothing the installer can do beyond
writing the file.

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
