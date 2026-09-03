# CLI reference

```
gortex install               One-time machine-wide setup (user-level MCP, skills, hooks, daemon wiring)
gortex init [path]           Per-repo setup (.mcp.json, hooks, community routing, per-community SKILL.md)
gortex init --dry-run-intake Emit a privacy-safe intake manifest and exit before parsing/writes
gortex doctor                Zero-op state report: adapter drift, hook activity, adoption, savings (human or --json)
gortex mcp [flags]           Start the MCP stdio proxy (connects to or auto-starts the daemon; embedded fallback requires config opt-in; --server adds HTTP API)
gortex daemon start [flags]  Start the daemon; --http-addr <addr> serves the HTTP/JSON API under /v1/* plus the MCP /mcp transport (--http-auth-token, --cors-origin)
gortex daemon <sub>          start / stop / restart / reload / status / logs / install-service / service-status / uninstall-service / server (multi-server roster)
gortex eval <sub>            Retrieval + coverage benchmarks — recall / embedders / pack / swebench / stdbench / tokens / baselines / quality / parity (substrate; prefer `gortex bench` for the user-facing surface). `parity` measures per-language cross-file coverage against the committed baseline
gortex eval-server [flags]   HTTP server used by the swebench harness
gortex bench <sub>           User-facing benchmark suite — recall / tokens / tokens-efficiency / embedders / perf / daemon-latency / swebench / all
gortex audit [flags]         A-F repo health grade derived from per-symbol complexity-axis health score
gortex gain [flags]          Forward-looking per-call USD savings projection from the latest bench tokens output
gortex context [flags]       Generate portable context briefing for a task
gortex savings [flags]       Token-savings dashboard (Today / Last 7 days / All time + USD avoided)
gortex status [flags]        Show index status (per-repo and per-project in multi-repo mode)
gortex repos [--json]        List every tracked repo with git head-commit SHA, last-indexed time, and a freshness flag (MISSING when the path is gone)
gortex repos <sub>           families / explain-view / forget / reconcile / set-primary — worktree and checkout administration
gortex track <path>          Add a repository to the tracked workspace
gortex untrack <path>        Remove a repository from the tracked workspace
gortex workspace <sub>       list [--json] / set / set-all — manage workspace + project slugs across tracked repos
gortex config exclude ...    add / list / remove entries in the effective ignore list
gortex query <sub>           Query the knowledge graph from the CLI
gortex prs [number]          List open PRs with a one-shot review-state classification, or deep-dive one PR's blast radius (gortex prs bundle <n> writes a reviewer graph bundle)
gortex review [path]         Review a changeset and print line-anchored inline comments + a BLOCK/REVIEW/APPROVE verdict (--diff, --base, --audience, --post)
gortex wiki [path]           Generate a multi-page markdown wiki (per-community + processes + analysis)
gortex docs [path]           Generate a "living docs" bundle (recent changes + ownership + stale + blame)
gortex export [path]         Export the graph to Cypher, GraphML, or Mermaid (--format mermaid --scope all)
gortex githook <sub>         install / uninstall / status — manage the post-commit hook
gortex clean                 Remove Gortex files from a project
gortex telemetry <sub>       on / off / status — control anonymous, opt-in usage telemetry (off by default; honours DO_NOT_TRACK)
gortex guide [topic]         Print the reference guide (providers, capabilities, tokens, analyze, search_ast, resources, workflow) — same content as the gortex://guide resource
gortex version               Print version
```

## One-time machine setup

```bash
gortex install                      # interactive-free: MCP + skills + slash commands + sub-agents at ~/.claude/
gortex install --start --track      # also spawn the daemon and track the current directory
gortex install --no-hooks           # skip user-level hook installation

# Daemon lifecycle (also spawned by `gortex install --start`):
gortex daemon start --detach        # spawn in background
gortex daemon start --detach --http-addr 127.0.0.1:7411 --log-level debug
                                    # --detach forwards every other flag to the background process,
                                    # so a detached start behaves like the same command without it
gortex daemon status                # PID, uptime, memory, tracked repos, sessions, server roster, search backend, warmup + enrichment progress
                                    # Answers while a track / reload / enrichment holds the daemon: the repo
                                    # table is then the last one computed and the heading says so
                                    # ("cached 45s ago — a track/reload is in progress"). Everything else —
                                    # readiness, runtime, the view census — is live either way.
gortex daemon status --exact        # recount nodes/edges from the store instead of reading the counters the
                                    # indexer maintains, and repair any that have drifted. Proportional to the
                                    # whole corpus — tens of seconds on a large store — so it waits without a
                                    # timeout, and waits for the busy daemon rather than serving the cached
                                    # table. Routine status never scans.
gortex daemon stop                  # graceful shutdown; the graph store is already on disk
gortex daemon restart               # stop + start
gortex daemon reload                # re-read the global config AND every tracked repo's .gortex.yaml, pick up new/removed repos
gortex daemon logs -n 50            # tail the log file

# Multi-server roster — let the daemon route to additional Gortex servers (local sockets or remote HTTPS):
gortex daemon server list                                                  # show ~/.gortex/servers.toml
gortex daemon server add work --url https://gortex.work.example --auth-token-env WORK_TOK
gortex daemon server remove work

# Auto-start at login (launchd on macOS, systemd --user on Linux):
gortex daemon install-service
gortex daemon service-status
gortex daemon uninstall-service

# Track / untrack repos (daemon-first dispatch; falls back to config-only when no daemon):
gortex track ~/projects/backend
gortex untrack backend

# Per-repo status + daemon-wide status share the same command — it picks:
gortex status
```

## Per-repo setup

```bash
cd ~/projects/myapp
gortex init                             # writes .mcp.json, .claude/settings.*, CLAUDE.md with community routing
gortex init --analyze                   # also index first for a richer CLAUDE.md overview
gortex init --no-skills                 # skip community-routing generation
gortex init --skills-min-size 5 --skills-max 10   # tune the generator
gortex init --hooks-only                # (re)install repo-local hooks only, skip everything else
gortex init --no-hooks                  # full init but skip hook installation
gortex init --dry-run-intake --json     # inspect admitted/skipped corpus buckets; no parsing/storage/writes,
                                        # no raw paths or file contents in the report

# Standalone mode requires `mcp.allow_embedded: true` in the user-level config.
# `--no-daemon` is deprecated and does not bypass the opt-in.
gortex mcp --index /path/to/repo --watch

# Require the shared daemon even when embedded fallback is globally allowed.
gortex mcp --proxy
```

## Worktrees and checkouts

Checkout and worktree administration lives under **`gortex repos`** — there is no
`gortex checkouts` command. Each subcommand is a thin shell over one daemon tool,
so the CLI and an agent over MCP get the same answer from the same handler; see
[mcp.md](mcp.md#worktree-views-and-checkouts) for the tools and their payloads.

| Command | MCP tool | Argument |
|---|---|---|
| `repos families` | `list_checkouts` | none |
| `repos set-primary <graph\|prefix\|path>` | `set_primary_checkout` | the corpus to promote |
| `repos forget <path\|prefix>` | `forget_checkout` | the checkout to remove |
| `repos reconcile [family\|prefix\|path]` | `reconcile_checkouts` | one family; omit for every family |
| `repos explain-view <path>` | `explain_view` | a filesystem path |

All five accept `--index` / `--repo <path>` (default `.`) to name the repository
the daemon must track, and `--format text|json` (default `text`). The `--json`
flag on the bare `gortex repos` table is unrelated and does not apply here.

```bash
gortex repos families                              # every family the daemon tracks
gortex repos families --family backend --format json
gortex repos explain-view ../feature-worktree/internal/auth/login.go
gortex repos reconcile                             # reconcile every family now
gortex repos forget ../old-worktree                # preview
gortex repos forget ../old-worktree --confirm      # run it
```

**`families`** reads the catalog and stats nothing on disk. Per family it prints
the primary corpus and its epoch, every dedicated graph (`primary`, `served`,
active generation), every registered working copy (`<effective mode>/<state>`,
its HEAD — `detached@<short sha>` when there is no ref — graph, whether a build
coordinator is live, tracking intents, the route with its epoch and commit /
dirty generations, both reconciler clocks with their deadlines, and the last path
evidence), and the ref and commit views rooted in its graphs. `--family` narrows
to one family by family id, graph id, repo prefix, or a path inside a tracked
repository. Run `repos reconcile` first for a fresh look at the filesystem.

**`set-primary`** and **`forget`** preview by default and write nothing. Both
close the preview with the command that carries the same request through, so the
`--confirm` run is a copy-paste rather than a re-derivation.

- `set-primary` previews the incumbent, the family's epoch, whether the move
  would be accepted, and every automatic checkout that has to rebuild its layers
  over the new base. Confirmed, it reports how many rebuilt and which kept their
  old route because they could not finish in time.
- `forget` previews the plan, the closure it would remove, what it would keep,
  and any blockers — an intent another tracking source still holds blocks it.
  Confirmed, it reports the revoked intents and the removed node and edge counts.
  It is the deliberate removal: unlike `gortex untrack` it never demotes the
  checkout into its family's automatic lane.

**`reconcile`** runs the pass the janitor runs on its own schedule: identities
are confirmed or allocated, the availability and removal clocks move, and the
build coordinators are brought in line with the verdicts. It prints one row per
checkout (`admin name`, action, path) and closes with a summary line — families
reconciled, checkouts removed, coordinators live, generations retired, view
generations retired.

**`explain-view`** walks one path's binding chain: the checkout the path binds
to, the graph that answers for it, the route and the generations behind it,
whether a composed view served it, and — when none did — the step in the chain
that could not be taken and left the base corpus to answer.

## Query subcommands

```
gortex query symbol <name>              Find symbols matching name
gortex query deps <id>                  Show dependencies
gortex query dependents <id>            Show blast radius
gortex query callers <func-id>          Show who calls a function
gortex query calls <func-id>            Show what a function calls
gortex query implementations <iface>    Show interface implementations
gortex query usages <id>                Show all usages
gortex query stats                      Show graph statistics
```

All query commands support `--format text|json|dot` (DOT output for Graphviz visualization).

## Pull-request review

```bash
# Triage the review queue — open PRs with CI rollup, review decision, age, and a
# one-shot review-state label (DRAFT / BASE_MISMATCH / CHANGES_REQUESTED / APPROVED
# / STALE / READY). Needs a GitHub token (GH_TOKEN / GITHUB_TOKEN).
gortex prs
gortex prs --worktrees                  # flag PRs whose head branch is checked out locally
gortex prs --base main --format json    # override the base branch; machine-readable output

# Deep-dive one PR: join its changed files against the graph for blast radius + risk.
gortex prs 1234                          # needs a running daemon that tracks the repo

# Write a reviewer graph bundle (impact + privacy-safe receipt + ranked reviewers).
gortex prs bundle 1234 -o pr-1234.json   # deterministic for an unchanged PR — diffable in CI

# Review a changeset → verdict (BLOCK / REVIEW / APPROVE) + line-anchored inline comments.
gortex review                            # unstaged changes (default scope)
gortex review --base main                # compare HEAD against a ref
gortex review --diff - < patch.diff      # review a pasted unified diff from stdin
gortex review --use-llm                  # fold in LLM-found findings (needs a configured provider)
gortex review --audience agent           # terse machine-first packet (vs the default human render)
gortex review --base main --post --pr 1234   # post the gated findings as inline PR comments (secrets redacted)
```

The deterministic correctness rulepack always runs (graph-grounded to drop false positives); `--use-llm` adds LLM findings relocated to exact lines. Posting to a public / fork PR is opt-in via `--confirm-public`; `--dry-run` prints the already-redacted payloads without any network call. The same surface is exposed to agents over MCP — see [mcp.md](mcp.md#pr-review).

## Full tool surface from the CLI

The verbs below give the `gortex` CLI parity with the daemon's MCP tool surface — the same handlers back both front doors. Each verb is a thin shell over one MCP tool on the daemon that owns the repo, so a skill (or a shell script) can drive the whole graph-query, edit-safety, and memory workflow with **no MCP transport mounted and no tool schemas loaded into the model's context**. Every group accepts `--index`/`--repo <path>` (default `.`) to name the repository the daemon must track, and `--format` to pick the wire format.

Because the daemon dispatches a tool call **by name** regardless of which tools are eagerly published, every MCP tool is reachable from the CLI even under the lean `core` preset — including tools that are otherwise [deferred behind `tools_search`](mcp.md#tool-discovery-lazy-mode). `gortex call <tool>` is the generic escape hatch; the dedicated verb groups are ergonomic front-ends over the most-used tools.

### Shared structured-input convention

The edit verbs that take a JSON-shaped parameter (an array of changes, a WorkspaceEdit, a steps/edits array, a ranges array) accept it three interchangeable ways — pick whichever suits the caller:

- **inline** — `--<name> '<json>'` (e.g. `--workspace-edit '{…}'`)
- **file** — `--<name>-file <path>` (e.g. `--edits-file ./edits.json`)
- **stdin** — `--<name> -` (a lone `-` reads the JSON from stdin)

The bytes are validated as well-formed JSON before the call. The same `inline / -file / -` triad covers `verify`'s `--changes`, `preview`/`contract`'s `--workspace-edit`, `simulate`'s `--steps`, `batch`'s `--edits`, and `contract`'s `--ranges`.

### `--arg` coercion (`call` and `analyze`)

`gortex call` and `gortex analyze` assemble their argument object from `--arg key=value` pairs (repeatable). Coercion is deterministic:

| Token | Lowered value |
|---|---|
| `key=true` / `key=false` | bool |
| `key=42` / `key=1.5` | number |
| `key=null` | null |
| `key=[…]` / `key={…}` | parsed JSON array / object (falls back to the literal string if it doesn't parse) |
| `key:=<raw>` | the right-hand side is parsed as raw JSON — `version:="1.0"` stays the **string** `"1.0"` |
| `key=` | the empty string |
| anything else | string |

The separator is chosen by the **key** position: `:=` opens a raw-JSON token only when it comes before the first `=`. A `:=` inside the value is ordinary text, so `--arg task=Dim x := 5` passes the snippet through verbatim.

Repeating a key replaces the earlier value. For `call`, a base object can also come from `--json '<obj>'`, `--json-file <path>`, or `--json -` (stdin); precedence is **file < `--json` < `--arg`** (last wins per key).

### `gortex call <tool>` — invoke any tool by name

The generic relay: invoke any tool the daemon's MCP surface registers, even one with no dedicated verb. Best-effort name validation runs against the live catalog (an unknown name lists the nearest matches and points at `gortex tools search`); calling a mutating tool prints a one-line stderr note unless `--quiet`.

| Flag | Meaning |
|---|---|
| `--arg key=value` | one argument, repeatable; coercion table above |
| `--json '<obj>'` / `--json -` | base object inline or from stdin |
| `--json-file <path>` | base object from a file |
| `--format json\|gcx\|toon\|text` | wire format forwarded to the tool (default `json`). `json` always prints parseable JSON: a tool that answers in prose — `explore` with `operation: "task"`, which renders a markdown page — is wrapped as `{"tool":…,"format":"text","text":…}`. Use `--format text` for the page itself. |
| `--dry` | print the lowered argument object + target tool **without** calling the daemon (works offline) |
| `--quiet` | suppress the mutating-tool stderr note |
| `--legacy` | use the historical handler contract for a name shared by the compact and legacy surfaces (`analyze`, `explore`, `review`, `ask`) |

### Compact-surface mirror for Bash-only harnesses

Use this path only when a coding harness exposes Bash but does not
expose Gortex MCP functions natively. An MCP-capable host MUST call the
compact MCP tools directly. A Bash-only harness MUST use `gortex call`,
which accepts the same compact tool names and argument objects without
translating them to legacy names.

The 21 public names select the compact contract by default. Existing scripts
that call a shared legacy name can retain its historical schema with
`gortex call --legacy analyze ...`; dedicated legacy names already select the
legacy-compatible connection automatically.

```bash
# Read an uncompressed source file.
gortex call read \
  --arg target='{"file":"internal/mcp/server.go"}'

# Search for a symbol.
gortex call search \
  --arg operation=symbols \
  --arg query=Server.addTool

# Discover the exact schema for one operation.
gortex call capabilities \
  --arg domain=read \
  --arg operation=file \
  --arg detail=schema

# Preview an edit without writing the file.
gortex call edit \
  --arg target='{"file":"README.md"}' \
  --arg 'match=old text' \
  --arg 'replacement=new text' \
  --arg dry_run=true
```

Because `target` starts with `{`, normal `--arg` coercion parses it as
a JSON object. `--json` can supply the whole argument object instead.
The edit example's `--arg dry_run=true` calls the tool and asks the
tool to preview the mutation. CLI `--dry` is different: it only prints
the lowered request and never contacts the daemon.

Non-dry calls require a running daemon that tracks the repository. Use
the shared `--index`/`--repo <path>` selector when the target repository
is not the current directory. The direct mirror covers all 21 compact
names listed in [`mcp.md`](mcp.md#compact-mcp-surface) and negotiates
the compact surface for that daemon connection without changing global
configuration. Operations under `session` are connection-scoped; a
one-shot `gortex call` connection closes after the result, so use a
persistent native MCP session when later calls must observe that state
or receive subscriptions.

### Legacy and full-catalog examples

The same relay can call a legacy or deferred tool explicitly when a
compatibility workflow requires it:

```bash
gortex call smart_context --arg task="add rate limiting to the login handler"
gortex call find_usages --arg id="internal/auth/login.go::Login" --format gcx
gortex call overlay_push --json-file ./buffer.json          # reach a deferred tool by name
gortex call edit_file --arg path=README.md --arg old_string=foo --arg new_string=bar --arg dry_run=true
```

### `gortex tools …` — discover & describe the surface

| Verb | MCP tool | Key flags |
|---|---|---|
| `tools list` | `tool_profile` | `--category <c>`, `--mutating`, `--preset compact\|core\|edit\|nav\|readonly`, `--format text\|json` |
| `tools search <q>` | `tools_search` | `--limit <n>` (default 10) — ranks the deferred surface |
| `tools describe <name>` | `tools_search select:<name>` | prints the tool's full parameter schema |
| `tools receipt` | `tool_profile` | `--format yaml\|json` (default `yaml`) |

`tools list` prints a `NAME · CATEGORY · R/W · PRESETS · SUMMARY` table. `tools receipt` emits a **context-budget receipt** — transport, advertised vs deferred tool counts, and `registered_tool_schemas: 0` — the auditable record that driving Gortex through the CLI mounts no tool schemas into the model's context. Searches and describes are inspection-only: they never promote a tool into the live set.

### `gortex edit …` — edit-safety verbs

The daemon's safe-edit surface as CLI verbs. The read-only verbs (`context`, `verify`, `plan`, `preview`, `simulate`, `guards`, `tests`, `contract`, `rename`) never write; the mutating verbs (`apply`, `symbol`, `batch`, `safe-delete`) touch the working tree.

| Verb | MCP tool | Key flags |
|---|---|---|
| `edit context <file>` | `get_editing_context` | `--detail brief\|full`, `--compress` |
| `edit verify` | `verify_change` | `--change 'id=newsig'` (repeatable sugar) **or** `--changes` / `--changes-file` / `-`; `--compact` |
| `edit plan` | `get_edit_plan` | `--ids <csv>` (required), `--depth` (default 3) |
| `edit preview` | `preview_edit` | `--workspace-edit` / `--workspace-edit-file` / `-`; `--no-diagnostics`, `--inherit-overlay` |
| `edit simulate` | `simulate_chain` | `--steps` / `--steps-file` / `-`; `--keep`, `--no-stop-on-error`, `--inherit-overlay` |
| `edit batch` | `batch_edit` | `--edits` / `--edits-file` / `-`; `--dry-run`, `--compact`. Each edit selects one of four ops: `edit_symbol`, `edit_file`, `move_file` (whole file), `delete_file` (whole file) |
| `edit apply <file>` | `edit_file` | `--old`, `--new` (required), `--replace-all`, `--dry-run`, `--allow-parse-errors`, `--expected <n>` |
| `edit symbol <id>` | `edit_symbol` | `--old`, `--new` (required), `--dry-run` |
| `edit rename <id>` | `rename_symbol` | `--to <name>` (required), `--dry-run` |
| `edit guards` | `check_guards` | `--ids <csv>` (required), `--compact` |
| `edit tests` | `get_test_targets` | `--ids <csv>` (required), `--depth` (default 3) |
| `edit contract` | `change_contract` | `--source auto\|diff\|edit\|symbols\|ranges`, `--lens api`, `--risk-gate`, `--ack`, `--base <ref>`, `--workspace-edit*`, `--symbols <csv>`, `--ranges*` / `--path` + `--start-line` + `--end-line` |
| `edit safe-delete <id>` | `safe_delete_symbol` | **dry run unless `--apply`**; `--force`, `--cascade off\|preview\|apply`, `--cascade-into-tests`, `--propagate` |

```bash
gortex edit context internal/auth/login.go --compress
gortex edit verify --change 'internal/auth/login.go::Login=func(ctx context.Context) error'
gortex edit apply README.md --old "old text" --new "new text" --dry-run
gortex edit safe-delete 'internal/legacy/old.go::Unused' --propagate   # dry run; add --apply to commit
```

### `gortex memory …` — session & durable memory

Session notes are scoped to a session and survive context compactions; durable memories are workspace-wide and survive daemon restarts and team rotation.

| Verb | MCP tool | Key flags |
|---|---|---|
| `memory note` | `save_note` | `--body`, `--symbol`, `--file`, `--tags`, `--links`, `--pin`, `--id`, `--no-autolink` |
| `memory notes` | `query_notes` | `--symbol`, `--file`, `--tag`, `--text`, `--session`, `--since`, `--limit`, `--pinned` |
| `memory distill` | `distill_session` | `--session`, `--max-symbols`, `--max-files`, `--max-tags`, `--max-recent` |
| `memory store` | `store_memory` | `--body`, `--title`, `--symbols`, `--files`, `--tags`, `--kind`, `--source`, `--importance`, `--confidence`, `--pin`, `--supersedes`, `--scope`, `--id`, `--no-autolink` |
| `memory recall` | `query_memories` | `--symbol`, `--file`, `--tag`, `--kind`, `--source`, `--author`, `--text`, `--since`, `--min-importance`, `--pinned`, `--include-superseded`, `--limit`, `--scope` |
| `memory surface` | `surface_memories` | `--task`, `--symbols`, `--files`, `--limit`, `--min-score`, `--include-superseded`, `--scope` |

```bash
gortex memory surface --task "fix the auth bug" --symbols internal/auth/login.go::Login
gortex memory store --kind invariant --importance 5 --symbols pkg/foo.go::Bar \
  --body "Bar must hold the lock before mutating the cache"
gortex memory note --tags decision --body "chose token bucket over leaky bucket because of burst tolerance"
```

### `gortex instructions …` — instruction profiles

An **instruction profile** bundles four agent-facing surfaces, generated from one table so they cannot drift: the instructions body (`~/.gortex/instructions/<name>.md`, @-included into `~/.claude/CLAUDE.md` through an `active.md` byte copy), the MCP tool preset sessions default to, the installed skills subset, and the hook-verbosity tier. Three profiles ship: `core` (balanced default), `localization` (lean code-finding surface: ~2 KB body, ~10 read-only tools, 3 skills, lean hooks), `full` (maximum guidance: the ~34-tool dev-cycle preset eager).

| Verb | Effect |
|---|---|
| `instructions list` | profiles with the active marker, body bytes, preset, skills count, hook tier |
| `instructions show <name>` | print one profile's instructions body |
| `instructions switch <name>` | atomically repoint `active.md`, record the state, reconcile installed skills |
| `instructions regen` | re-derive the profile files from the running binary (selection unchanged) |

Switching applies to **new sessions only** — the @-include, `tools/list`, and skills all load at session start. A forwarded `GORTEX_TOOLS` or an operator-pinned `mcp.tools` config always beats the profile's preset (see [`mcp.md`](mcp.md#restricting-the-tool-surface-presets)); `GORTEX_INSTRUCTIONS_PROFILE=<name>` pins the profile per process tree (benchmarks, CI).

```bash
gortex instructions list
gortex instructions switch localization   # lean machine: diet body + 10-tool surface + lean hooks
gortex instructions switch core           # back to the balanced default
```

### `gortex analyze` — unified analysis dispatcher

`gortex analyze --kind <k>` runs the daemon's `analyze` dispatcher. `gortex analyze kinds` lists the valid kinds (no daemon needed). Universal typed flags (`--limit`, `--compact`, `--path-prefix`, `--format json|gcx|toon|text`) cover the common parameters; kind-specific parameters ride on `--arg key=value` (same coercion as `call`, and a `--arg` pair overrides the matching typed flag).

```bash
gortex analyze kinds
gortex analyze --kind hotspots --arg threshold:=0.8 --limit 5
gortex analyze --kind coverage_gaps --path-prefix internal/auth/ --arg max_pct:=80 --format gcx
gortex analyze --kind todos --arg tag=FIXME --arg has_assignee=true
```

### `gortex flow` / `taint` / `clones` / `feedback`

| Verb | MCP tool | Key flags |
|---|---|---|
| `flow` | `flow_between` | `--from` / `--to` (required), `--max-depth`, `--max-paths`, `--min-tier`, `--format` |
| `taint` | `taint_paths` | `--source` / `--sink` (required, pattern syntax), `--max-depth`, `--limit`, `--min-tier`, `--format` |
| `clones` | `find_clones` | `--dead-only`, `--min-similarity`, `--path-prefix`, `--repo-filter`, `--limit`, `--format` |
| `feedback record` | `feedback action=record` | `--task`, `--useful`, `--not-needed`, `--missing`, `--tool-source` |
| `feedback query` | `feedback action=query` | `--tool-source`, `--top-n`, `--compact` |

`taint`'s `--source` / `--sink` take a pattern (a bare token is a case-insensitive substring on the symbol name; `exact:Foo`, `path:dir/`, `kind:method`, combined with spaces).

```bash
gortex flow --from pkg/a.go::Input --to pkg/b.go::Sink --max-depth 6
gortex taint --source 'path:handlers/' --sink 'exact:Exec' --limit 30
gortex clones --dead-only --path-prefix internal/
gortex feedback record --task "fix the auth bug" --useful pkg/a.go::Foo,pkg/b.go::Bar
```

### Choosing a consumption path

The CLI verbs and the MCP install are two front doors to the **same** handlers. Pick by how much of the agent's context budget you want to spend on tool schemas versus what transport-level features you need:

| | MCP install (full / core) | skill + CLI |
|---|---|---|
| Baseline context | high (full surface) / ~34 schemas (core) | zero schemas |
| Push notifications | yes | no |
| Overlay sessions | yes | no (use `gortex call overlay_*`) |
| Edit-safety + memory workflow | yes | yes |
| Discovery | `tools_search` | `gortex tools search` |

The skill-driven CLI path trades the live transport features (server-pushed `notifications/*`, session-bound overlay shadow graphs) for a zero-schema baseline: nothing is loaded into the model's context until a verb is actually run. Overlay tools remain reachable by name through `gortex call overlay_*`, but without a persistent MCP session they don't compose into a per-session shadow graph the way the MCP transport does. The edit-safety and memory workflows are fully available on both paths.

## Other commands

```bash
gortex track . && gortex daemon start --http-addr 127.0.0.1:7411  # HTTP/JSON API on :7411 (/v1/* + /mcp). UI lives at github.com/gortexhq/web.
gortex savings [--verbose] [--json]      # Today / Last 7 days / All time bar-chart dashboard + $ avoided
gortex bench <sub>                       # user-facing benchmark suite (recall / tokens / tokens-efficiency / perf / daemon-latency / embedders / swebench / all)
gortex audit [--badge|--format svg|json|text]  # A-F repo health grade + README-ready SVG shield
gortex gain [--since 7d]                 # forward-looking per-call USD savings + optional history slice
gortex version
```

## Generated wiki + living diagrams

Run `gortex wiki .` to produce a Markdown wiki under `wiki/<repo-slug>/`:

```
wiki/
  index.md                    # top-level (single repo today, multi-repo extension point)
  <repo>/
    index.md                  # community navigation
    architecture.md           # community-level system overview
    communities/<n>-<slug>.md # one page per detected community
    processes/<slug>.md       # one page per discovered execution flow (Mermaid sequenceDiagram)
    contracts/api-surface.md  # HTTP / gRPC / GraphQL contracts
    analysis/{hotspots,cycles,semantic}.md
    _assets/community-graph.mermaid
  _workspace/                 # reserved for multi-repo pages
```

Pair with `gortex githook install post-commit --regen-mermaid --regen-wiki` to keep diagrams and docs in sync after every commit. The hook is idempotent and preserves any non-gortex content in the existing hook file.

For CI, drop `examples/.github/workflows/gortex-architecture.yml` into your repo: it re-runs `gortex export --format mermaid --scope all` on every push and opens a PR when the diagrams drift.

`gortex wiki --enhance` enables LLM-augmented narrative summaries via the configured `llm.provider` (claudecli for MVP — uses your local Claude Code subscription). Results are cached by `(node, content_hash)` so re-runs on unchanged inputs produce byte-identical output without re-invoking the LLM.
