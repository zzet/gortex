# Onboarding — First 15 Minutes with Gortex

Just installed Gortex? This is the shortest path from "it's on my machine" to "it's helping my AI agent work faster." Follow it top-to-bottom the first time; skip sections you've already done on return visits.

## Prerequisites

- Gortex is installed (run `gortex version` — if that prints a version, you're good). Not installed? → `curl -fsSL https://get.gortex.dev | sh`, or see [docs/installation.md](installation.md) for Homebrew, package managers, and supply-chain verification.
- A repository you want to work in. We'll call it `~/projects/myapp` in the examples — substitute your own path.
- An AI coding assistant installed. Gortex auto-integrates with Claude Code, Cursor, Kiro, Windsurf, GitHub Copilot (VS Code), Continue.dev, Cline, OpenCode, and Antigravity.

## Two Commands

Setup is split into two commands so codebase-agnostic machinery lives once per user, not once per repo:

- **`gortex install`** — run **once per machine**. Writes user-level artifacts: `~/.claude.json` (MCP config), `~/.claude/skills/gortex-*` (tool-usage skills), `~/.claude/commands/gortex-*.md` (slash commands), `~/.gemini/antigravity/` Knowledge Items, and (optionally) user-level hooks. Also sets up the daemon — pass `--start` to spawn it, `--track` to register the current directory.
- **`gortex init`** — run **once per repo**. Writes per-repo artifacts: `.mcp.json`, `.claude/settings.{json,local.json}`, `CLAUDE.md` with the codebase overview and community routing, `.claude/skills/generated/` per-community SKILL.md files, and a marker-guarded community-routing block in every other detected agent's per-repo instructions file.

You can run them independently — `gortex init` doesn't require `gortex install` first; it just writes less if the user-level wiring isn't there.

```bash
# One-time: machine-wide user-level setup
gortex install --start --track         # install, start daemon, track current dir

# Per repo: drop into the repo and wire it up
cd ~/projects/myapp
gortex init                            # default: indexes repo, generates community routing
```

For CI, scripts, or explicit control, both commands accept the usual flags (`--yes`, `--json`, `--dry-run`, `--agents`, `--agents-skip`, `--force`).

## 30-Second Version

```bash
# Once per machine
gortex install --start --track

# Once per repo
cd ~/projects/myapp
gortex init
```

With `--start`, the daemon is already running and Claude Code will find Gortex on its next run. If you skipped it, `gortex mcp` will try to auto-start the daemon; the legacy per-repo server requires the embedded-fallback opt-in described below.

Open your AI assistant in that repo and ask it to do something real. It'll use Gortex tools automatically. If that worked, the rest of this document is optional detail.

## Step-by-Step

### 1. One-time: user-level setup

```bash
gortex install                    # MCP config, skills, slash commands, KIs at ~/
gortex install --start --track    # also spawn the daemon + track current dir
gortex install --no-hooks         # skip user-level hooks
```

This writes under `$HOME` only. It's idempotent — re-running it is safe. Think of it like `brew install`.

### 2. Per repo: wire it up

```bash
cd ~/projects/myapp
gortex init
```

`gortex init` creates tool-specific config files (auto-detecting which tools you have installed) and runs community detection on the graph so each agent gets codebase-specific routing. Commit the output — your teammates get Gortex for free when they pull.

**Key files `gortex init` creates:**

- `.mcp.json` — tells MCP clients (Claude Code, Cursor, VS Code) how to start the Gortex server
- `CLAUDE.md` — codebase overview (with `--analyze`) plus a marker-guarded community-routing block
- `.claude/settings.local.json` — installs three hooks: `PreToolUse` (redirects `Read` / `Grep` / `Glob` / `Task`), `PreCompact` (injects orientation snapshot before context compaction), `Stop` (post-task diagnostics)
- `.claude/skills/generated/<DirName>/SKILL.md` — one per detected community (via `--skills`, default on)
- `.cursor/mcp.json`, `.kiro/settings/mcp.json`, `.vscode/mcp.json`, etc. — per-agent MCP configs
- Marker-guarded "Gortex Communities" routing block in each detected agent's per-repo instructions file (`AGENTS.md`, `.windsurfrules`, `GEMINI.md`, `.cursor/rules/gortex-communities.mdc`, etc.)

**Tune the community generator:**

```bash
gortex init --analyze                         # include a richer codebase overview in CLAUDE.md
gortex init --no-skills                       # skip community generation entirely
gortex init --skills-min-size 5 --skills-max 10   # raise the floor / lower the ceiling
```

### 3. Start the MCP server

The default setup lets your IDE spawn `gortex mcp`, which connects to or auto-starts the shared daemon. You normally do not run another server process yourself.

To deliberately use the legacy one-shot server, first opt in from the user-level config (`~/.gortex/config.yaml`, or its XDG equivalent):

```yaml
mcp:
  allow_embedded: true
```

Then start it for one repository:

```bash
gortex mcp --index . --watch
```

The one-shot graph lives in a private store that is deleted on exit, so it is rebuilt on every launch. Add `--server` to expose `/v1/*` on `http://localhost:8765`; for a persistent shared HTTP surface, use `gortex daemon start --http-addr 127.0.0.1:7411` instead.

### 4. Verify the integration

Open your AI assistant in the repo. Ask it something concrete that requires understanding the code:

> "What does the authentication flow look like? Trace it from the HTTP handler through to the database."

**What should happen:**

- The assistant calls `graph_stats` or `get_repo_outline` to orient itself
- Then `search_symbols "auth"` or `smart_context "authentication flow"` to find relevant code
- Then `get_call_chain` or `find_usages` on the specific handler
- Finally `get_symbol_source` on the specific functions — not `Read` on whole files

**What should NOT happen:**

- The assistant calls `Read` on 5 files and hunts for auth logic manually. If you see this, the hooks aren't wired up — run `gortex init --hooks-only` to reinstall just the hooks.

**Quick sanity check from the CLI:**

```bash
gortex status --index .
```

Prints node/edge counts, language breakdown, and per-repo stats. If this shows 0 nodes, the index didn't build — check for errors in `gortex mcp` output.

### 5. Your first calls (if you're driving Gortex directly)

For debugging, writing custom agents, or working with the bridge HTTP API — the "good first calls" in order:

1. **`get_repo_outline`** — zero-arg narrative overview: primary languages, top communities, load-bearing hotspots, most-imported files, entry points. Takes ~1k tokens, covers "what is this repo?"
2. **`plan_turn` with your task description** — returns ranked recommended next calls. Example:
   ```json
   {"tool": "plan_turn", "args": {"task": "add rate limiting to auth handler"}}
   ```
   You get back a list like "smart_context → get_editing_context → find_usages" with pre-filled args.
3. **`smart_context` with the task** — does what `plan_turn` recommended as step 1, but assembles the actual context (relevant symbols, entry file structure, related tests) rather than just pointing at tools.
4. **Before editing any file — `get_editing_context` on its path.** Returns all symbols, signatures, direct dependencies, immediate callers. You don't need to read the file.

### 6. What the hooks do automatically

Once installed, three things happen without you lifting a finger:

- **PreToolUse on `Read` / `Grep` / `Glob`** — Gortex suggests the right graph tool instead and, for indexed source files, blocks whole-file reads.
- **PreToolUse on `Task`** — spawned subagents get a task-scoped briefing with `smart_context` results + a tool-swap table, so they don't inherit the bad habit of reaching for `Read`.
- **PreCompact** — just before Claude Code compacts the conversation, Gortex injects an orientation snapshot (recent edits, hotspots, feedback-ranked symbols) so the agent survives compaction without re-exploring.
- **Stop** — after the agent finishes a turn, Gortex runs `detect_changes` → `get_test_targets`, `check_guards`, `analyze dead_code`, `contracts check` and feeds the results back so the agent self-corrects before handoff. The diagnostics are scoped to the symbols *this session* edited: PreToolUse records each write target, and the Stop hook partitions the working-tree diff against that set, so a sibling session editing the same checkout does not turn up as your work. Files it cannot attribute are listed separately rather than dropped, and when the session cannot be identified at all the briefing reports the whole tree and labels itself as such.

All four degrade silently when the bridge is unreachable — they never block your normal flow.

## Troubleshooting

**"Gortex MCP server failed to start" in the IDE.**
Check that `gortex` is on your `PATH` (`which gortex` should resolve). If you installed via Homebrew, restart the IDE — it caches PATH at launch.

**The AI still uses `Read` / `Grep` on source files.**
The hooks didn't install. Re-run `gortex init --hooks-only` and restart the AI tool. On Claude Code, also check that `.claude/settings.local.json` exists and contains `"gortex hook"` invocations under `hooks`.

**`graph_stats` returns `total_nodes: 0`.**
The index is empty. Either `gortex mcp` isn't watching the right directory, or `.gortex.yaml` excludes everything. Run `gortex status --index /absolute/path/to/repo` to verify the paths.

**Indexing a big repo takes forever.**
First-time index of a 100k-symbol repo is ~20-30 seconds. With the daemon, a restart is ~200ms because it reopens the graph store it already wrote and re-indexes only the changed files. Make sure the store directory (`~/.gortex/store`, or wherever `--backend-path` points) isn't being deleted between runs.

**Semantic search isn't working.**
On first use, Gortex downloads the MiniLM-L6-v2 model (~90 MB) to `~/.gortex/models/`. Needs network the first time; after that, fully offline. Check `~/.gortex/models/sentence-transformers_all-MiniLM-L6-v2/` exists.

**"Cannot be opened because Apple cannot check it for malicious software" on macOS.**
You bypassed the curl installer and downloaded the binary by hand — `curl -fsSL https://get.gortex.dev | sh` strips the quarantine xattr automatically (and on macOS routes through Homebrew when `brew` is on PATH). To fix an existing manual install, re-run the installer, reinstall via Homebrew (`brew install zzet/tap/gortex`), or run once: `xattr -d com.apple.quarantine /usr/local/bin/gortex`.

## Next Steps

Once the basics are working:

- **Multi-repo workspaces** — index several repos into one graph for cross-repo impact analysis. See [Multi-Repo Workspaces](#multi-repo-workspaces) below.
- **Guard rules** — add `.gortex.yaml` to declare architectural invariants (e.g., "UI must not import DB directly"). `check_guards` enforces them on every change. See `.gortex.yaml` in this repo for an example.
- **Per-community skills** — already generated by `gortex init --skills` (default on). Each skill auto-activates when the agent asks about that area. Re-run `gortex init` to regenerate after the graph changes; pass `--no-skills` if you want to skip that step.
- **Token savings + cost tracking** — `gortex savings` prints cumulative tokens saved + dollars avoided per model across all sessions. Accumulates automatically; no setup.
- **Compact wire format (GCX1)** — every list-shaped tool accepts `format: "gcx"` for a round-trippable compact response. Median **−27.4% tokens** vs JSON on the benchmark, 100% round-trip integrity. Spec: [docs/wire-format.md](wire-format.md). TypeScript decoder on npm: [`@gortex/wire`](https://www.npmjs.com/package/@gortex/wire). Agents pick it up automatically — the PreToolUse and subagent hooks surface the opt-in. Applies to: `search_symbols`, `find_usages`, `analyze`, `contracts`, `batch_symbols`, `get_callers` / `get_call_chain` / `get_dependencies` / `get_dependents` / `find_implementations`, `get_file_summary`, `get_editing_context`, `smart_context`.
- **Feedback loop** — after a successful task, call the `feedback` MCP tool with `action: "record"`. Future `smart_context` results rerank based on what was actually useful.
- **Custom HTTP integration** — `gortex daemon start --http-addr 127.0.0.1:7411 --cors-origin '*'` exposes every MCP tool as HTTP (`/v1/*` + `/mcp`) for the tracked repos. Good for editor plugins, CI hooks, custom dashboards.

## Daemon Mode

The daemon is a long-living process that holds the graph for every tracked repo. All MCP clients (Claude Code windows, Cursor, Kiro, etc.) connect to it via a Unix socket, so:

- Memory scales with workspace size, not open-editor count — one process instead of one per project.
- Cross-repo queries work by default: an agent in `frontend` can find callers in `backend` without extra config.
- Each session gets isolated per-client state (recent activity, token stats) via handshake-assigned session IDs.

### Setup

```bash
# One-time: user-level MCP config, skills, slash commands, hooks, daemon spawn + track.
gortex install --start --track

# Track additional repos any time:
gortex track ~/projects/backend
gortex track ~/projects/shared-lib

# Remove a repo from the workspace:
gortex untrack backend        # by prefix, or by absolute path

# See state:
gortex status                 # tracked repos, node/edge counts, memory, sessions (via daemon if running)
gortex daemon status          # PID, uptime, socket path
gortex savings                # cumulative tokens saved + $ avoided across all sessions
```

### Daemon lifecycle

```bash
gortex daemon start --detach  # spawn in background
gortex daemon stop            # graceful shutdown; the graph store is already on disk
gortex daemon restart         # stop + start
gortex daemon reload          # re-read config, pick up new/removed repos
gortex daemon logs -n 50      # tail the log
```

`reload` re-reads the global `~/.gortex/config.yaml` *and* every tracked
repo's own `.gortex.yaml`, so an edit to a repo's `exclude:`, `include:`,
`guards:` or workspace slugs applies without restarting the daemon. The
refreshed exclude list is pushed into each repo's live indexer, so the next
walk — full or incremental — honours it.

### Auto-start at login (optional)

Let the OS supervise the daemon so it starts at login and restarts on crash. No sudo required — the unit lives under `$HOME`.

```bash
gortex daemon install-service   # launchd (macOS) or systemd --user (Linux)
gortex daemon service-status    # check installed state + active/inactive
gortex daemon uninstall-service # remove unit, stop service
```

On macOS the unit lands at `~/Library/LaunchAgents/com.zzet.gortex.plist`; on Linux at `~/.config/systemd/user/com.zzet.gortex.service`. After `install-service`, plain `gortex daemon start / stop` still work — they just fight the service for socket ownership, so prefer `gortex daemon service-status` and `launchctl` / `systemctl --user` commands for lifecycle.

If you run an XDG layout (any absolute `XDG_CONFIG_HOME` / `XDG_DATA_HOME` / `XDG_CACHE_HOME`), `install-service` captures those values into the unit so the supervised daemon resolves the same paths as your shell — service supervisors otherwise start with a near-empty environment and the daemon would fall back to `~/.gortex`. Re-run `install-service` if you later change where those variables point.

`install-service` also captures the installing shell's `PATH` (absolute entries only), so the supervised daemon finds language servers installed outside the system directories, such as `~/go/bin` or `~/.cargo/bin`. Run it from the shell whose `PATH` you want the daemon to use, and re-run it after that `PATH` changes. The unit always runs a bare `gortex daemon start`, so set daemon options in config rather than as flags: for the HTTP surface, put `daemon.http_addr` in the global config, `~/.gortex/config.yaml` or its XDG location (see [server.md](server.md)).

### How it works

- `gortex mcp` (what Claude Code spawns via `.mcp.json`) connects to and may auto-start the daemon, then acts as a thin stdio ↔ socket proxy (~5 MB per client). If no compatible daemon can be reached, it exits by default with instructions to start one or enable `mcp.allow_embedded` in the user-level config.
- With that opt-in, the embedded fallback infers its index root from the launch directory but refuses `/`, a drive root, `$HOME`, or a directory that merely contains tracked repositories. An inferred root never receives the repo-local notebook; an explicit `--index` is honoured verbatim and keeps it. The fallback identifies itself as `DEGRADED` in the MCP `initialize` instructions.
- Every tracked repo gets its own fsnotify watcher so edits on disk flow into the graph live; no manual reload needed. `gortex track` attaches a watcher as part of the track operation; `gortex untrack` detaches it before evicting nodes.
- Graph state lives in the on-disk store (`~/.gortex/store/store.sqlite`) as it is indexed. Daemon restarts open the store and re-index only the files whose mtime changed while it was down.
- Opening Claude Code in a directory that neither lies inside nor contains a tracked repository returns a structured `repo_not_tracked` error on every tool call. The agent surfaces it; you run `gortex track .` to include it.
- Opening it at a directory **above** your repos works without tracking the parent. The session binds to the repos rooted under that directory, whether or not they share a `workspace:` slug, and sees nothing else — a repo declaring the same slug from elsewhere on disk stays invisible, because the boundary is containment, not slug membership. `repo:`, `project:` and `workspace:` selectors narrow within that set; naming anything outside it is refused rather than silently answered empty.
- Per-session state is isolated by a handshake-assigned session ID — two Claude Code windows see their own recent-activity and token-savings counters, not a merged view. Cumulative savings in the sidecar ledger (`~/.gortex/sidecar.sqlite`) are still shared.

### Fallback rules

| Invocation | Daemon running | Daemon not running |
|---|---|---|
| Claude Code spawns `gortex mcp` | Proxies through daemon | Exits with guidance by default; embedded server only with `mcp.allow_embedded: true` |
| `gortex track /path` | Immediate re-index + watcher attached via daemon | Writes config; takes effect on next daemon/server start |
| `gortex untrack /path` | Immediate graph eviction + watcher detached | Removes from config |
| `gortex status` | Aggregate across tracked repos | One-shot local index |
| `gortex daemon status` | PID, uptime, memory, sessions | "not running" |

Full architectural notes live under `specs/` in the repo.

## Multi-Repo Workspaces

When you have related repos (frontend + backend, service + SDK, producer + consumer) and want cross-repo `find_usages` / `get_call_chain` / contract matching, Gortex indexes them all into one shared graph.

### Add another repo

```bash
gortex track ~/projects/backend
gortex track ~/projects/shared-lib
gortex status                 # confirms both repos appear
```

Both repos now show up in every query tool. Pass `repo: "backend"` (or `project:` / `ref:`) on any MCP query to scope it; with no scope, the agent sees the full union.

### Workspace slug — make two repos count as one project

By default each tracked repo lives in its own isolated **workspace** — the hard graph boundary. So a server in one repo and the client that calls it in another look like orphans to `contracts check` (and to anything that walks contract pairs). Pin them to the same workspace slug to get cross-repo contract matching:

```bash
gortex workspace list                                       # what each tracked repo declares today
gortex workspace set backend my-saas                        # write workspace=my-saas to backend/.gortex.yaml
gortex workspace set-all my-saas --root ~/work --yes        # bulk-stamp every repo under ~/work
```

For OSS / read-only repos where you don't want a `.gortex.yaml` artifact in the tree, pass `--global` to record the slug in `~/.gortex/config.yaml` instead.

### Projects (optional sub-buckets) and active scope

A **project** is a sub-bucket inside a workspace, useful when you have many repos but a given task only touches a few:

```bash
gortex mcp --project my-saas         # only loads repos in this project
```

The daemon, by contrast, loads every tracked repo and scopes at query time: over its HTTP surface (`gortex daemon start --http-addr ...`) the `/v1/graph` route takes `?project=` / `?repo=` to narrow a single request to one workspace or repo.

Inside the agent, `set_active_project` switches the default scope for every subsequent query — no need to repeat the `project:` parameter on each call.

### Federate to remote daemons (multi-server roster)

If a heavyweight repo lives on a build machine or behind a VPN, your local daemon can route queries to a remote Gortex server transparently:

```bash
gortex daemon server list
gortex daemon server add work --url https://gortex.work.example --auth-token-env WORK_TOKEN
gortex daemon server remove work
```

Stored in `~/.gortex/servers.toml`. Local-socket and remote-HTTPS targets both work. The auth token is read from the named env var on demand — never written to disk.

For the full configuration reference (slug precedence chain, `projects:` block, exclude layering, daemon tuning knobs), see [README → Multi-Repo Workspaces](../README.md#multi-repo-workspaces).

## Getting Help

- File issues and feature requests at [github.com/zzet/gortex/issues](https://github.com/zzet/gortex/issues).
- Full tool reference lives in `CLAUDE.md` (created by `gortex init`). Your AI agent already reads it; you can too.
