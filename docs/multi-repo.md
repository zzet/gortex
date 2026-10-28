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

A directory that neither lies inside nor contains a tracked repo still fails closed with the structured `repo_not_tracked` error — tracking the parent of your repos is not required, and doing so would index every child a second time. A configured repo that warmup has not registered yet after a daemon start is not "untracked": its sessions get `repo_loading` (retryable) until it is ready, then work in place without reconnecting — see [Startup states](mcp.md#startup-states). Over the HTTP surface (`gortex daemon start --http-addr ...`) the `/v1/graph` route accepts `?project=` and `?repo=` to narrow the dump, so a typo'd value returns an empty result for that request instead of bringing the whole index up empty.

## Repository prefixes

Each indexed root needs a unique repository prefix. The default is its directory
basename, so two unrelated directories named `src` cannot both use `src`.
Use `gortex track --name other-src /path/to/other/src` for the second root.
The MCP `track_repository` tool accepts the equivalent `name` parameter.
Re-tracking the same root does not create another corpus.

The CLI rejects configured collisions before writing a new entry, including
when the daemon is unavailable. The daemon also checks live and configured
ownership, including requests that race for one prefix or arrive before warmup loads the owner.
For a linked worktree, the first configured member of its Git family supplies
`<family-base>@<admin>`, where `admin` is Git's stable worktree administration
name. That member can be a linked checkout or a bare repository. Workspace-based
instance naming is the fallback when no family corpus supplies a base.
Online, the daemon chooses the name. Offline, the CLI inventories the target's
Git family once and applies the same admin-name rule.

For a collision already stored in config, startup prefers the root bound in the
live metadata. A checkout catalog binding wins only while a base publication
protects it and its root is still configured under that prefix. Otherwise the
first config entry wins. Startup can repair a wrong unpublished binding from
older versions when both roots remain configured and no live owner protects it.
The repair changes identity metadata only. It retains the corpus for reconcile.
An abandoned unpublished binding can also move to a replacement root when the
old owner is neither configured nor live. Published, configured and live owners
remain protected against that rebind.

The skipped entry does not replace the owner's workspace, excludes, gitignore
root, or catalog graph. For an already configured path, CLI `--name`, MCP `name`
and control `Track` refuse a different name rather than accepting a rename that
would disappear on restart. Set `name:` in the global config and run
`gortex daemon reload`. Tracking a configured but unloaded root without a name
keeps its configured name.

Collisions use the `prefix_collision` code across control, MCP and probe
responses, with the prefix, owner path and captured owner state. A missing
owner is different from an owner whose availability could not be established:
only a confirmed missing owner produces advice to inspect its untrack preview.
The colliding path is never a remedy for removing the owner's corpus.

Same-directory spellings and same-folder-name symlink aliases remain no-ops.
Selected configured roots retain their pinned name and workspace; admission
does not scan unrelated configured filesystems.

Alternatively, `gortex untrack <absolute-path>` can remove an exact configured
root that the daemon proves is not loaded, then reload the config. It does not
untrack a containing live root or start retirement of an implicit live root.
An older daemon that omits loaded-root information cannot prove this condition;
restart with the matching daemon build before using this fallback. For a loaded
root, inspect the untrack preview before confirming removal.

## Configuration

Two-tier config hierarchy:

- **Global config** (`~/.gortex/config.yaml`) — projects, repo lists, active project, reference tags, and machine-level MCP policy
- **Workspace config** (`.gortex.yaml` per repo) — guards, excludes, local overrides

Excludes are layered — builtin → the repo's `.gitignore` chain → global → per-repo entry → workspace — with gitignore semantics. `.gitignore` is respected by default so you don't have to re-declare entries already curated for git; opt out per-workspace with `respect_gitignore: false` in `.gortex.yaml`. Use `!pattern` in a later layer to re-include an excluded path only if all its parent folders are included, as in Git. To keep only `agent/skills/` after `/*`, reopen each parent in order: `!/agent/`, `/agent/*`, then `!/agent/skills/`. The matcher propagates folder negations to descendants, so repeat dependency and generated-file excludes (for example `node_modules/`, `dist/`, `*.tmp`) after this chain to retain them. `include:` is a separate force-include override. A leading `/` or an inner `/` anchors an entry to the repo root; otherwise it matches at any depth. An anchored literal path such as `vendor/acme/lib/` keeps its ancestors open and overrides the exclude layers. An unanchored literal such as `Pods/` reopens that folder at any depth when its parent is open; it cannot reopen an excluded ancestor. Wildcard includes can reopen their matching folder when its parents are open (for example `gen*/`, `gen?/`, `**/generated/`, or `out*`). They cannot reach through an excluded ancestor: `*.proto` does not reopen `node_modules/`, and `vendor/**` or `build/**` does not reopen an excluded `vendor/` or `build/` itself. Use `/Pods/` or `ios/Pods/` to reopen a specific excluded folder. Per-directory `.gortexignore`, `.ignore`, and `.rgignore` files still apply to force-included paths. Git metadata, non-MCP agent state, and escaping symlinks remain excluded. Beyond `.gitignore`, the index walk also honors per-directory `.gortexignore` files (Gortex's own ignore file, a sibling to `.gitignore`) and ripgrep's `.ignore` / `.rgignore` — each scoped to the directory that contains it.

When a tracked root sits below its git root — the monorepo case, `gortex track repo/projects/App` with the repository at `repo/` — every `.gitignore` from the git root down to the tracked root applies, as it would for git, with ancestor patterns re-anchored onto the tracked root. A deeper file can override a shallower rule, but a `!pattern` next to your code cannot re-include a path below an excluded parent folder. Ancestor patterns that describe a sibling subtree are dropped, and so is one that would ignore the tracked root itself: you asked Gortex to index that directory explicitly.

```yaml
# ~/.gortex/config.yaml
active_project: my-saas

mcp:
  allow_embedded: false               # Require the shared daemon (default)

# daemon:
#   http_addr: 127.0.0.1:7411         # Opt in to /mcp + /v1 over HTTP (unset: socket only)
#   log_max_size: 50                  # Rotate daemon.log at this many MB (0 disables)
#   log_max_backups: 3                # Number of rotated logs to retain (minimum 1)

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

`index.framework_synthesizers` controls framework-derived resolution work. An
omitted key preserves the complete registry, `[]` disables the whole framework
pipeline, and a non-empty list enables only those named synthesizers and
claiming resolvers. For a shared graph pass, an omitted key in any participating
repository enables the complete registry; otherwise Gortex uses the union of
the participating repositories' allow-lists. This keeps a repository from
silently losing framework edges requested by a sibling while allowing a
workspace where every repository opts out to avoid the scans entirely.

## Daemon tuning (optional)

The background daemon writes JSON lines to `daemon.log` and rotates it while
running. In the global config, `daemon.log_max_size` sets the size in MB
(default `50`, where 1 MB = 1,048,576 bytes). `daemon.log_max_backups` sets the
number of timestamped backups to retain (default `3`, minimum `1`). Out-of-range
integers warn and use the default for that setting. A non-integer value such as
`log_max_size: many` makes the global config fail to load; the daemon warns and
uses the default log policy. Collectors should follow `daemon.log` by name
(`tail -F` style), because rotation renames the file. On upgrade, a large legacy
`daemon.log` becomes the newest backup; it can be deleted by hand.

Launchers capture crashes and other output in `daemon.stderr.log`, in the same
directory. Existing Unix and Windows installs that inherit `daemon.log` handles
are redirected there automatically when rotation is enabled; no reinstall is
needed for those installs.
New Windows Task Scheduler logon tasks explicitly enable log capture: rotating
JSON goes to `daemon.log`, and raw stdout/stderr and Go runtime panic output go
to `daemon.stderr.log`. Reinstall an existing scheduled task with
`gortex daemon install-service` to capture logs in these files; an old task
captures no logs. Other Windows starts keep
their output destinations, including terminals, pipes, shell redirections, CI
captures, and third-party service wrappers such as NSSM and WinSW.

If inherited descriptors cannot be redirected, the daemon warns and keeps stderr
logging without rotation. Setting `daemon.log_max_size: 0` also keeps stderr
logging without rotation and leaves existing descriptors unchanged, so legacy
installs continue writing to their inherited destination, including `daemon.log`.
New launchers and new Windows logon tasks capture both JSON and raw output in
`daemon.stderr.log` when rotation is disabled.

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
- `GORTEX_DAEMON_HTTP_ADDR` — TCP listen address for the daemon HTTP surface (`/mcp` + `/v1`), e.g. `127.0.0.1:7411`. Overrides the `daemon.http_addr` global config value; a non-empty `--http-addr` flag wins over both. Resolved at startup only. Unset in all three leaves HTTP disabled. Non-loopback binds require `--http-auth-token` or `GORTEX_DAEMON_HTTP_TOKEN`.
- `GORTEX_DAEMON_MEMLIMIT` — standing soft memory limit installed at daemon boot, as a human size (`4GiB`, `2048MiB`, `2G`) or `off` / `0` to disable. The daemon is a long-lived background service; a soft limit makes the GC pace against a ceiling and resist heap balloon growth rather than letting the high-water climb toward machine RAM. Overrides the `daemon.memory_limit` config value; an explicit `GOMEMLIMIT` overrides both (the runtime already honors it). Unset applies the default policy: a quarter of host RAM, clamped to `[1GiB, 8GiB]`. The cold-index window temporarily raises this to a larger budget and restores it afterward.
- `GORTEX_POTION=0` — pin the rerank semantic-cosine channel to the small baked GloVe word vectors instead of the bundled `potion-code-16M-v2` code model, saving roughly 31 MiB of resident heap at the cost of natural-language rerank quality. For symbol search, natural-language and other nonliteral queries can initialize the model; symbol, path, and signature queries only reuse an already loaded provider. The effective `query_class` override controls this choice. Other reranking consumers retain their existing initialization behavior. This is independent of the `embedding:` section (that drives vector indexing; this drives rerank scoring), and the provider is dropped after 30 minutes with no rerank traffic. The supported config form is `search.rerank_embedder: false`, which disables the channel entirely.
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

## Checkout families and worktree views

A tracked repository is rarely one directory. Git's linked worktrees give the
same repository several working copies off one shared administrative directory,
and Gortex models that set as a **checkout family**: every worktree of one
repository, keyed by the common git dir, with the repository's own checkouts
inside it identified by `(family, administrative name)` — the name git keeps
under `<commondir>/worktrees/`. That identity is what survives a path going
away and coming back.

**Discovery is automatic.** Reconciliation reads `git worktree list` for each
tracked family and mints a durable identity for every linked worktree it has
not seen, provided the worktree is usable (git does not already call it
prunable, its root is reachable, it has an administrative name) and the family
already has a **primary corpus** to serve from. Nothing is tracked twice: a
newly discovered worktree becomes an *automatic* checkout, served from the
family's shared lane, rather than a second indexed repository. `gortex track` on
a worktree still makes it *dedicated* — its own graph, indexed in its own right.

**Automatic checkouts are dormant until selected.** Minting the identity is
cheap; standing up a coordinator and building its commit and dirty layers is
not, and a family with dozens of linked worktrees would pay that cost, serialized
through the build gate, for every one at startup — most of them never read. So an
automatic checkout the startup inventory sees keeps only its catalog row until a
session or query selects it; the first selection wakes its coordinator, and the
retry that follows finds the composed view built. A worktree already being served
resumes on restart through its persisted route. A `git worktree add` performed
while the daemon is running is built eagerly on discovery by default — set
`views.lazy_worktree_activation: true` (or `GORTEX_WORKTREE_LAZY_ACTIVATION=1`,
which overrides the file either way) to defer those too, for worktree-heavy trees
that never want an unselected view built.

**One corpus, layered per checkout.** The primary corpus is the index built
from one checkout of the family; every automatic checkout is served by composing
two generations on top of it — a **commit layer** that turns the corpus at the
primary's tree into the corpus at that checkout's HEAD tree, and a **dirty
layer** that turns that into what its working tree currently holds. A
per-checkout coordinator keeps both in step: a filesystem signal opens a quiet
window, the state is sampled after the window closes (not when the signal
arrived), and each slot is reconciled against the identity of what it is routed
to. Because a commit layer's identity is the tree it targets over the base it
sits on, switching branch A → B → A re-routes A's existing generation instead of
re-indexing A's tree. Every route flip is a compare-and-set on the route's
epoch, so a coordinator that loses the race supersedes what it built and
reschedules. `gortex repos set-primary` moves the primary; every automatic
checkout then rebuilds its layers over the new base, and one that cannot finish
in time keeps its old route — a stale view of a real state rather than a
failure.

**Views of committed state.** A `git_ref` or `commit` selector
([mcp.md](mcp.md#worktree-views-and-checkouts)) names a tree nobody has checked
out. It is served by building one generation describing that tree over the
graph's corpus — no dirty slot, because a ref means the committed tree by
definition. Those generations are cached and reclaimed on three bounds: the
generation a view selected inside the retention window is always kept; past the
per-graph count cap candidates go least-recently-selected first; and the same
order pays down the per-graph byte budget. Eviction never forces anything —
every candidate is offered to the same guarded retire the coordinators use, so a
routed, based-upon or leased generation is simply offered again next sweep. A
selector whose payload was evicted resolves and rebuilds on its next selection.

```yaml
# ~/.gortex/config.yaml (or per-repo .gortex.yaml)
views:
  retain_inactive: 168h          # keep a generation this long after the last
                                 # selection of the view serving it (default 7 days)
  max_cached_generations: 32     # per-graph cap on cached ref-view generations
  max_bytes_per_graph: 5368709120  # per-graph payload budget in bytes (default 5 GiB)
  lazy_worktree_activation: false  # keep a runtime-discovered worktree dormant
                                   # until it is selected, too (default: eager on
                                   # discovery; startup inventory is always dormant)
```

Each is "unset" at zero (or at an unparseable duration, which is logged) and
takes the shipped bound; a cap of zero would evict a generation the moment it
was published. Language-server enrichment of a routed checkout has its own
bounds — see [`semantic.checkout_lsp`](lsp.md#checkout-workspaces).

**A view and a checkout at the same commit build separately.** That is by
design, not a missed cache hit: a checkout's commit layer is keyed per checkout
(`commit-<checkout id>`, owned by the graph) and a ref view's layer is keyed per
view (`refview-layer-<view id>`, owned by the view), because the two have
different owners and different retirement lifecycles — a checkout's layers
follow its coordinator's route flips, a ref view's are collected by the sweep
above. Sharing one generation row between them would still be safe — the
guarded retire refuses any generation a ref view points at, whoever offered it
— but it would entangle the two owners' accounting. The view's pointer would
pin the checkout's layer, so a branch switch could never reclaim it and every
sweep would re-offer a generation that is refused again; and each side's bounds
— the view's retention window, count cap and byte budget against the
coordinator's branch-switch reuse cache — would be sizing a payload the other
one holds. The payloads are not interchangeable either: a ref
view describes a tree nobody has checked out, so it carries neither the
language-server enrichment nor the text-search rows a checkout's layers get from
a root on disk. What the duplication costs, in the rare case where a view and a
checkout do sit on the same tree over the same base, is one redundant build and
the rows it writes.

**Removal needs evidence.** Anything the daemon cannot positively prove is
treated as a checkout that is temporarily unreachable, never as one that is
gone: a failed stat looks identical either way and only one of the two is
recoverable. Availability and removal are separate clocks — being unreachable
for an hour brings a checkout no closer to being forgotten. The removal clock
starts only on evidence, of which there are two kinds: a trustworthy inventory
of the family that did not list the checkout (git's administrative data is the
authority on which worktrees exist, so its silence is a statement), or git
still listing the worktree as prunable *and* the filesystem agreeing the root
is absent from a volume that is still mounted. Both are recorded on the
catalog row beside the clock, so a reader can tell afterwards which proof was
used, and deletion runs through journalled sagas whose every phase is
idempotent — a crash resumes rather than re-running side effects.
`gortex repos families` shows both clocks with their deadlines and the last
evidence; `gortex repos reconcile` runs the pass now instead of waiting for the
janitor.

**Builds wait for warmup.** A warm restart brings everything back at once: the
warmup tail re-resolves the graph while every automatic checkout the catalog
remembers wants its two layers built, all through one store writer. Build work
is therefore held until warmup completes. Nothing else is: registration,
catalog seeding, route reads and serving a generation that is already published
never consult the gate. A repository answers graph calls once warmup registers
it; until then its sessions get `repo_loading` (see
[Startup states](mcp.md#startup-states)). A coordinator that could not start
because its primary was not registered yet is started when that primary
registers, not at the janitor's next pass. Held work is not dropped either — a coordinator cycle
reschedules onto the gate's own wake, and a ref view's build keeps its claim, so
the selection that made it still gets a token to poll and later selections
coalesce onto the same one.

## CLI

```bash
gortex track /path/to/repo          # Add a repo to the workspace
gortex untrack /path/to/repo        # Remove a repo from the workspace
gortex mcp --track /path/to/repo    # Track additional repos on startup
gortex mcp --project my-saas        # Set active project scope
gortex status                       # Per-repo and per-project stats
gortex repos                        # List tracked repos — head-commit SHA, last-indexed time, freshness
gortex repos --json                 # Same, machine-readable (for scripts / CI)

# Worktree + checkout administration lives under `gortex repos` (see cli.md)
gortex repos families               # Families, their primary corpus, working copies, routes, views
gortex repos explain-view <path>    # Which graph answers for a path, and why
gortex repos reconcile [family]     # Reconcile against git + the filesystem now
gortex repos set-primary <graph>    # Move a family's primary corpus (preview; --confirm runs it)
gortex repos forget <path>          # Remove a checkout outright (preview; --confirm runs it)

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
- **Cross-repo edges** — the resolver links symbols across repo boundaries with same-repo preference. Cross-repo edges carry a `cross_repo: true` flag. JS/TS imports can link a declared npm dependency to a tracked repository whose `package.json` claims that package name, including npm and pnpm workspace members. A provider in the caller's workspace wins over eligible foreign providers. Multiple manifest claims in the winning tier keep the import external, even if only one entry is indexed.
  - Entry lookup respects `exports` keys, wildcard subpaths and `null` blocks. It probes the selected `import`/`default` target first, then a `types` condition when that file is not indexed. Without `exports`, it tries `types`, `main` and directory indexes. Unsupported-condition subpaths never guess a directory target.
  - Bare calls use the caller file's named import binding, including its local alias, not another same-named function anywhere in the provider. A per-binding import links to a symbol marked public by the extractor; otherwise it stays at `external::<specifier>::<name>` with its `npm_import` marker and creates no call binding. Linked imports retain their specifier, survive provider eviction as external imports, and are rechecked by whole-graph resolution.
  - **Limits:** Build-output-only entries, such as `main` and `types` pointing into excluded `dist/`, stay external unless an indexed target is declared. Gortex does not infer `src/` from `dist/`. The importing repository's own packages, `npm:` aliases and workspace paths that escape the repository are not linked by this pass. Sibling dependencies declared with `file:`, `link:` or `portal:` are not supported. Default-import bare calls have no exact binding evidence yet and stay unresolved. After an upgrade, JavaScript and TypeScript files are re-extracted once (extractor version 2), so existing imports acquire the specifier and alias metadata; their links appear after that pass. Namespace member calls, CommonJS `require` calls and package re-exports are not linked by this npm import pass. Export targets without `./` are accepted leniently, as in workspace resolution, even though Node rejects them. Call binding is file-scoped: a parameter or local callable with the imported name can bind to the import.
  - Go package-selector calls such as `library.Hello()` use tracked roots' `go.mod` module paths, not checkout names. An import of `example.org/library/sub` can bind to `<provider>/sub/file.go::Hello` even when the provider prefix is `b`.
  - Precise own-module, vendor and staging paths take priority. Otherwise the longest matching module path wins. A primary/non-worktree checkout wins a tie with dedicated worktree copies; ties between independent primary checkouts are refused. Cross-workspace calls still require the caller's `cross_workspace_deps` declaration.
  - A cross-repo module binding carries confidence `0.6` and origin `ast_inferred`. It is a source hint, not proof that the tracked checkout matches the required version. The edge retains its symbol-bearing import identity so provider edits and full reindexes can rebind it without editing the caller. Module-map changes re-check existing bindings.
  - **Limits:** Only root `go.mod` identities are used; nested modules and `go.work` selection are not resolved. Required versions and module/version replacements are not verified. A local `replace` whose destination is untracked prevents module-path binding instead of selecting another checkout with the same module name. Tracked local replacements remain source hints, not full Go build-list resolution. Calls on values typed from another module, such as `client.Do()` for a `library.Client`, can remain `unresolved::*.Do`; receiver-type linking is not implemented here.
- **Impact analysis** — `explain_change_impact`, `verify_change`, and `get_test_targets` follow cross-repo edges automatically, grouping results by repository.
- **Shared repos** — the same repo can appear in multiple projects with different reference tags. It's indexed once and shared across projects.
