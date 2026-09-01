# Multi-repo workspaces

Gortex can index multiple repositories into a single shared graph, enabling cross-repo symbol resolution, impact analysis, and navigation.

## Workspace boundary

Every node and contract is keyed on a **workspace slug**, which is the hard graph boundary for cross-repo work.

> **Scope of this boundary.** It bounds *graph queries* — what symbols, callers and analysis a session can observe. It does **not** bound filesystem access: `read_file`, `edit_file` and the other path-argument tools are confined to the union of every tracked repository root, not to the session's workspace. See [SECURITY.md](../SECURITY.md#file-system-access). Two repos that should pair their contracts (an HTTP server and the client that calls it, a Kafka producer and its consumer, etc.) must declare the same `workspace:` in their `.gortex.yaml` — otherwise contract matching stops at the boundary and they look like orphans.

Slug resolution precedence (first match wins):

1. `RepoEntry.workspace` in `~/.gortex/config.yaml` — overrides everything, ideal for OSS / read-only repos where you don't want to leave an artifact in the tree
2. `workspace:` in the repo's own `.gortex.yaml` — the default for first-party repos
3. The repo prefix — fallback when neither is set, so each unconfigured repo gets its own isolated workspace

The same chain applies to the optional `project:` slug (a sub-bucket inside a workspace). The daemon loads every tracked repo into one shared graph; you scope a query to a single workspace or project at request time rather than at startup.

### Sessions opened above their repos

A session's boundary comes from its working directory. Inside a tracked repo it is that repo's workspace slug. At a directory that *contains* tracked repos — an agent opened at the root above them — the boundary is the set of repos rooted under that directory, and it needs no shared slug: two unrelated repos side by side, each its own workspace by default, bind together. Nothing else is visible, including a repo that declares one of the same slugs from elsewhere on disk — containment is the narrower rule, and it is the one that applies.

Such a session has no single workspace slug, so `_meta.scope_applied` reports `repos:N` rather than `workspace`. `repo:`, `project:`, `workspace:` and `scope:` narrow *within* the contained set; naming a repo or workspace outside it is refused with an error rather than answered empty. `repo:"*"` widens only back to the session's own repos.

A directory that neither lies inside nor contains a tracked repo still fails closed with the structured `repo_not_tracked` error — tracking the parent of your repos is not required, and doing so would index every child a second time. Over the HTTP surface (`gortex daemon start --http-addr ...`) the `/v1/graph` route accepts `?project=` and `?repo=` to narrow the dump, so a typo'd value returns an empty result for that request instead of bringing the whole index up empty.

## Configuration

Two-tier config hierarchy:

- **Global config** (`~/.gortex/config.yaml`) — projects, repo lists, active project, reference tags, and machine-level MCP policy
- **Workspace config** (`.gortex.yaml` per repo) — guards, excludes, local overrides

Excludes are layered — builtin → the repo's `.gitignore` chain → global → per-repo entry → workspace — with gitignore semantics. `.gitignore` is respected by default so you don't have to re-declare entries already curated for git; opt out per-workspace with `respect_gitignore: false` in `.gortex.yaml`. Use `!pattern` in a later layer to re-include something an earlier layer excluded. Beyond `.gitignore`, the index walk also honors per-directory `.gortexignore` files (Gortex's own ignore file, a sibling to `.gitignore`) and ripgrep's `.ignore` / `.rgignore` — each scoped to the directory that contains it.

When a tracked root sits below its git root — the monorepo case, `gortex track repo/projects/App` with the repository at `repo/` — every `.gitignore` from the git root down to the tracked root applies, as it would for git, with ancestor patterns re-anchored onto the tracked root. A deeper file overrides a shallower one, so a `!pattern` next to your code still wins over the repository-wide rule. Ancestor patterns that describe a sibling subtree are dropped, and so is one that would ignore the tracked root itself: you asked Gortex to index that directory explicitly.

```yaml
# ~/.gortex/config.yaml
active_project: my-saas

mcp:
  allow_embedded: false               # Require the shared daemon (default)

exclude:                            # Applies to every tracked repo
  - "**/*.generated.*"
  - "node_modules/"                 # Already in the builtin baseline

repos:
  - path: /home/user/projects/gortex
    name: gortex
    exclude:                        # Extra patterns just for this repo
      - "results/**"

projects:
  my-saas:
    repos:
      - path: /home/user/projects/frontend
        name: frontend
        ref: work
      - path: /home/user/projects/backend
        name: backend
        ref: work
      - path: /home/user/projects/shared-lib
        name: shared-lib
        ref: opensource
```

The embedded MCP fallback is a machine-level, default-off policy. Enable `mcp.allow_embedded` only in the user-level config; see [Daemon availability and embedded fallback](mcp.md#daemon-availability-and-embedded-fallback).

`synthesize_external_calls: true` (opt-in, default off — set in `.gortex.yaml` or the global config) makes the resolver synthesize placeholder nodes for calls into un-indexed external packages or sibling services, so call-chains keep the external hop instead of terminating at the indexed boundary.

## Daemon tuning (optional)

The daemon's defaults handle typical workflows without configuration. These knobs exist for monorepos, branch-heavy workflows, or filesystems without fsnotify support.

```yaml
# ~/.gortex/config.yaml (or per-repo .gortex.yaml)
watch:
  debounce_ms: 150            # per-file patch debounce (default 150)

  # Storm mode — when more than N events land within the window,
  # switch from per-file debounced patching to a batched reconcile
  # that defers cross-file resolver + search work until a quiet
  # period has passed. Amortises the cost of bulk operations
  # (rsync, npm install, branch checkout, bulk format-on-save,
  # find-and-replace) and, more importantly, collapses the burst into
  # one timer instead of one goroutine per changed path.
  storm_threshold: 0     # 0 = built-in default (50); negative disables
  storm_window_ms: 500
  storm_quiet_period_ms: 500
```

Environment variables:

- `GORTEX_RECONCILE_INTERVAL` — janitor tick that walks every tracked repo and runs the full-tree `IncrementalReindexPaths` pipeline against disk. Insurance against fsnotify gaps on NFS/SMB mounts, inotify watch-limit exhaustion, or daemon downtime where edits happened offline. Default `1h`; `"0"` or `"off"` disables; otherwise any Go duration string (e.g., `15m`).
- The daemon also watches each tracked repo's `.git/HEAD`, so branch switches and rebases reconcile incrementally (via `git diff --name-status`) rather than by re-indexing every changed file individually — no configuration needed.
- `GORTEX_WARMUP_FULL_RETRACK=1` — force every repo through a whole-repo re-track (evict + re-parse every file) on the next warm restart instead of the default scoped reconcile. An escape hatch for when the on-disk change census itself is suspect.
- `GORTEX_WARMUP_FULL_RESOLVE=1` — force the warm-restart master resolve to re-examine the whole graph instead of scoping to changed repos; also makes the resolver ignore the durable terminal-edge stamp and re-attempt every previously-given-up-on edge. Use when a scoped resolve is suspected of missing edges.
- `GORTEX_WARMUP_FORCE_ENRICH=1` — bypass the persisted per-repo enrichment-completion markers and re-run semantic enrichment for every repo on warm restart, even ones whose marker already matches HEAD on a clean tree.
- `GORTEX_DAEMON_MEMLIMIT` — standing soft memory limit installed at daemon boot, as a human size (`4GiB`, `2048MiB`, `2G`) or `off` / `0` to disable. The daemon is a long-lived background service; a soft limit makes the GC pace against a ceiling and resist heap balloon growth rather than letting the high-water climb toward machine RAM. Overrides the `daemon.memory_limit` config value; an explicit `GOMEMLIMIT` overrides both (the runtime already honors it). Unset applies the default policy: a quarter of host RAM, clamped to `[1GiB, 8GiB]`. The cold-index window temporarily raises this to a larger budget and restores it afterward.
- `GORTEX_POTION=0` — pin the rerank semantic-cosine channel to the small baked GloVe word vectors instead of the bundled `potion-code-16M-v2` code model, saving roughly 31 MiB of resident heap at the cost of natural-language rerank quality. The code model is loaded on the first search that reranks, independently of the `embedding:` section (that drives vector indexing; this drives rerank scoring), and is dropped again after 30 minutes with no rerank traffic. The supported config form is `search.rerank_embedder: false`, which disables the channel entirely.
- `GORTEX_TRIGRAM_MAX_MB` — ceiling on the summed estimated heap of every live trigram searcher (default `256`; `0` disables the byte ceiling). The trigram index is the in-memory literal-search structure behind `search_text` / `find_declaration`, built lazily per repo on first use. A count cap alone does not bound it — three indexes of an arbitrarily large repo is still arbitrarily large — so this is the rule that makes the worst case a number. `gortex daemon status` prints a `trigram` line with the live count, current heap and the active budget.
- `GORTEX_TRIGRAM_MAX_LIVE` — how many repos may hold a built trigram index at once (default `3`). `0` means never build one: every text search then streams over the repo's known file list, holding no index state at the cost of scan latency. Binary files are excluded from the index and from literal search regardless of these settings.
- `GORTEX_TRIGRAM_IDLE_TTL` — how long an unused trigram index is kept before it is dropped (default `10m`, any Go duration string). A repo being actively grepped re-touches its entry on every query, so the TTL only reclaims repos that have gone quiet.
- `GORTEX_WATCHER_STARTUP_BARRIER_TIMEOUT` — how long a macOS watcher waits at startup for FSEvents to hand back its own handshake marker, proving the stream is live and its replay has been ordered. Default `5s`; any Go duration string. Raise it if `daemon: some repositories are not being watched` appears on a very large tree or a busy machine. Exceeding it no longer stops the repo from being watched — the watcher continues in a degraded state, backed by the adaptive poller, and reports the reason.
- `GORTEX_DAEMON_MEMRELEASE=0` — disable the post-burst heap-to-OS release. By default the daemon calls `debug.FreeOSMemory()` at allocation-burst boundaries (warmup completion, a reconcile-janitor tick that reindexed something, the close of a cold-index window, and a whole-graph analysis pass) so a burst's high-water footprint is returned to the OS promptly instead of pinning resident memory at the peak. It only ever fires at those boundaries, never on a timer.

### When a repository stops being watched

A dead watcher is the one failure mode that looks like success: the graph still
answers, `gortex repos` still prints `fresh` (it compares indexed SHA against
`git rev-parse`, not against what the watcher is doing), and every answer comes
from a graph that stopped advancing. The daemon reports it in three places:

- **Startup.** `daemon: watching` logs `repos` (live) alongside `configured`.
  When they differ, a `daemon: some repositories are not being watched` warning
  names the first reason.
- **Health push.** Subscribers receive a `degraded` readiness phase carrying
  `watch_degraded`, both for a watcher that never started and for one that
  degraded later.
- **Read tools.** `read_file`, `get_symbol_source` and friends attach
  `index_frozen` with the reason, so an agent sees it without polling.

Causes worth knowing:

- **A watcher that never started** — a repository root removed under a running
  daemon, or a root the daemon cannot write a startup marker into. The repo is
  not watched at all until the daemon restarts.
- **A degraded watcher** — inotify or file-descriptor exhaustion (raise
  `fs.inotify.max_user_watches` / `ulimit -n`), a slow mount, or a macOS
  startup barrier that did not complete within
  `GORTEX_WATCHER_STARTUP_BARRIER_TIMEOUT`. Live watching continues, with the
  adaptive poller covering what the native backend misses.

`GORTEX_RECONCILE_INTERVAL` bounds how long any of this can hide drift: the
janitor walks every tracked repo against disk on that tick regardless of
watcher health.

## CLI

```bash
gortex track /path/to/repo          # Add a repo to the workspace
gortex untrack /path/to/repo        # Remove a repo from the workspace
gortex mcp --track /path/to/repo    # Track additional repos on startup
gortex mcp --project my-saas        # Set active project scope
gortex status                       # Per-repo and per-project stats
gortex repos                        # List tracked repos — head-commit SHA, last-indexed time, freshness
gortex repos --json                 # Same, machine-readable (for scripts / CI)

# Stamp workspace / project slugs across tracked repos (migration helper)
gortex workspace list                                       # Show what each tracked repo currently declares
gortex workspace list --json                                # Same, machine-readable
gortex workspace set backend api                            # Write workspace=api to backend's .gortex.yaml
gortex workspace set upstream-lib api --global              # OSS-friendly: pin to api in ~/.gortex/config.yaml
gortex workspace set-all api --root ~/projects/work --yes   # Bulk: stamp every tracked repo under a prefix

# Manage the effective ignore list used by indexing + watching
gortex config exclude list                          # Show all layers (builtin, global, repo entry, workspace)
gortex config exclude add pkg/generated             # Default target: workspace .gortex.yaml
gortex config exclude add '**/*.bak' --global       # Write to ~/.gortex/config.yaml
gortex config exclude add testdata/ --repo backend  # Write to a RepoEntry
gortex config exclude remove pkg/generated          # Remove from the same target
```

### Deleted checkouts

Tracking outlives the directory: nothing removes a repo entry when you
delete, rename, or unmount its checkout. Such an entry can never be
indexed again, so all three inventory views agree on flagging it —
`gortex repos` renders `MISSING` in place of a freshness value (`missing:
true` under `--json`), `gortex status` and `gortex daemon status` mark the
row `MISSING`, and `index_health` reports `tracked_repo_paths_ok: false`
with the dead paths under `missing_repo_paths`. Each prints the
`gortex untrack <path>` that clears it.

The repo also stays listed after a daemon restart that failed to index
it: `daemon status` reconciles the live indexer registry against
`~/.gortex/config.yaml`, so a repo the daemon could not load shows as
`not indexed` rather than dropping out of one view while the other keeps
listing it.

### Empty indexes

A repo that finished indexing with no files at all is reported as
`EMPTY`, not as an ordinary zero-count row. It is the one failure that
otherwise reads as success everywhere: the repo is tracked, loaded, and
answering — with an empty graph, so `find_usages` returns "no callers"
and `analyze` returns "likely unused" with full confidence.

`gortex status` and `gortex daemon status` mark the row `EMPTY` and print
the affected paths below the table; `index_health` reports
`repos_hold_files_ok: false` with the prefixes under `empty_repos`. The
daemon log names the exact cause, including the ignore pattern
responsible and one file it excluded:

```
gortex daemon logs | grep 'no source files were indexed'
```

The usual cause is an ignore rule that matches more than intended — a
`.gitignore`, `.gortexignore`, or `.gortex.yaml` `excludes` entry. A repo
that genuinely holds no source Gortex can parse is not flagged.

## MCP tools

Agents can manage repos at runtime without CLI access:

| Tool | Description |
|------|-------------|
| `track_repository` | Add a repo, index immediately, persist to config |
| `untrack_repository` | Remove a repo, evict nodes/edges, persist to config |
| `set_active_project` | Switch project scope for all subsequent queries |
| `get_active_project` | Return current project name and repo list |

Locate, reach, and analyze query tools uniformly accept `repo`, `project`, `workspace`, and `scope` parameters for scoping (plus `ref` where reference tags apply). All are clamped to the session workspace — the hard boundary for graph queries. Default breadth now follows **tool intent** when `scope.intent_defaults` is enabled (the default); see [Tool scoping by intent](#tool-scoping-by-intent) below.

For `analyze`, the overrides genuinely narrow its **graph-node** kinds — `dead_code`, `hotspots`, `cycles`, `health_score`, `todos`, `stale_code`, `ownership`, `coverage_gaps`, `coverage_summary`, `impact`, `bottlenecks`, `role`, `k8s_resources`, `images`, `kustomize`, `dbt_models`, `external_calls`, and the like — and, since v1, its **edge-walk / graph-algorithm / framework / file-AST-scan** kinds too (`channel_ops`, `pubsub`, `routes`, `models`, `pagerank`, `kcore`, `edge_audit`, `tests_as_edges`, `sast`, `review`, …), which prune their rows / re-tally their counts against the same workspace + repo allow-set. The narrowing also resolves the two kind-specific collisions: `kind=cross_repo` keeps `repo` as its boundary filter and `kind=cycles` keeps `scope` as a file-path / package prefix (both are stripped from the uniform scope-resolution view). **v1 caveat:** the remaining long-tail kinds — community detection (`clusters`, `concepts`, `suggest_boundaries`), git/disk-mining (`blame`, `coverage`, `fixes_history`, `retrieval_log`, `temporal_verify`), per-id (`would_create_cycle`, `def_use`), `synthesizers` / `resolution_outcomes`, and `sql_rebuild` — remain workspace-bound but are **not** repo-narrowed — passing a narrowing arg on such a kind stamps a `scope_note` on the response disclosing the no-op.

Some `analyze` operation names are not dispatcher kinds at all: the facade maps them to a separate legacy tool, which the dispatcher — and so `resolveScope` — never sees. Those tools carry the clamp themselves, and they split the same two ways:

- **Workspace-clamped and repo-narrowed:** `health` (`audit_health`), `clones` (`find_clones`), `inspections` (`run_inspections`), `processes` (`get_processes`), `recent_changes` (`get_recent_changes`). Their rows are per-node, per-pair, or per-file, so a `repo` / `project` / `scope` selector narrows them for real.
- **Workspace-clamped only:** `communities` (`get_communities`), for the same reason as the `clusters` / `concepts` kinds — one partition is computed over the whole index, so a narrowing arg is resolved for `scope_applied` and then widened to the workspace, with the response disclosing it.

Their by-id lookups (`get_communities id:`, `get_processes id:`) resolve against the clamped set, so an out-of-scope id reports the same miss as one that never existed.

## Tool scoping by intent

Tools are split by intent — each group has a different default scope:

| Intent | Tools | Default scope |
|--------|-------|---------------|
| **Locate** ("where is X defined") | `search_symbols`, `search_text`, `find_files` | current repo |
| **Reach** ("who consumes X") | `find_usages`, `get_callers`, `get_call_chain`, `contracts` | workspace |
| **Analyze** | `analyze`, `review`, sast | workspace (graph-node + edge-walk / algorithm / framework / scan kinds narrow to `repo`/`project`/`scope`; community / git-mining / per-id kinds stay workspace-bound — see the caveat above) |

Other query tools (`get_symbol`, `get_file_summary`, `smart_context`, etc.) keep their existing per-tool scope classification; the intent defaults above apply to the locate/reach/analyze groups listed in the table.

### `scope.intent_defaults` config flag

- Controls the intent-based default scoping described above
- **Defaults ON** (enabled out of the box — this is the new behavior after upgrade)
- **Narrow-only invariant:** the intent defaults only ever *narrow* within the session workspace (the hard boundary for graph queries); they never widen past it, and an explicit `repo` / `project` / `workspace` / `scope` arg always overrides the default
- Opt out: set `scope.intent_defaults: false` in `.gortex.yaml`, or set env var `GORTEX_SCOPE_INTENT_DEFAULTS=0`

**⚠ Upgrade note (behavior change):** When upgrading to this version:

- Locate tools narrow their default: project → repo (you now need `repo:"*"` to search the whole workspace)
- Reach tools widen their default: project → workspace (cross-repo callers surface automatically)
- Restore the old behavior with `scope.intent_defaults: false` or `GORTEX_SCOPE_INTENT_DEFAULTS=0`

### Widen sentinels

When intent defaults are on, you can still widen or narrow explicitly:

- `repo:"*"` — widen a locate tool back to the whole workspace
- `project:<name>` — select the middle rung (explicit project scope)
- `scope:<name>` — select a named saved scope

### Uniform parameter set

Every locate/reach/analyze tool now uniformly accepts `repo`, `project`, `workspace`, and `scope` parameters — including the legacy tools the `analyze` facade forwards to (`audit_health`, `find_clones`, `run_inspections`, `get_communities`, `get_processes`, `get_recent_changes`). All are clamped to the session workspace (the hard boundary for graph queries). For `analyze` this narrows the graph-node, edge-walk, graph-algorithm, framework, and file/AST-scan kinds; the remaining community / git-mining / per-id / synthesizer kinds are workspace-bound but not repo-narrowed in v1 (see the [MCP tools](#mcp-tools) caveat above).

### Response metadata

Scoped tool responses carry a `scope_applied` meta field plus a one-line widen hint naming an explicit override that re-broadens the result (e.g. `repo:"*"` for the whole workspace, or `project:<name>` / `scope:<name>` to re-scope to a deliberate rung). `analyze` additionally stamps a `scope_note` when a narrowing arg is passed to a kind that does not repo-narrow its rows in v1, so the no-op is self-documenting rather than silent.

## How it works

- **Qualified node IDs** — IDs are always `<repo_prefix>/<path>::<Symbol>` (e.g., `frontend/src/app.ts::App`), including for a workspace tracking a single repo: a lone repo is simply the first tracked repo. Tools still accept an unqualified path (`src/app.ts`) when exactly one repo is tracked, since there is then nothing to be ambiguous about.
- **Cross-repo edges** — the resolver links symbols across repo boundaries with same-repo preference. Cross-repo edges carry a `cross_repo: true` flag.
- **Impact analysis** — `explain_change_impact`, `verify_change`, and `get_test_targets` follow cross-repo edges automatically, grouping results by repository.
- **Shared repos** — the same repo can appear in multiple projects with different reference tags. It's indexed once and shared across projects.
