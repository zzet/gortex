# MCP surface

Gortex exposes a knowledge-graph query surface over the [Model Context Protocol](https://modelcontextprotocol.io): **100+ tools, 18 resources, 3 prompts**. Agents call the same surface from stdio, the daemon Unix socket, or the MCP 2026 Streamable HTTP endpoint.

- [Daemon availability and embedded fallback](#daemon-availability-and-embedded-fallback)
- [Compact MCP surface](#compact-mcp-surface)
- [Tool discovery (lazy mode)](#tool-discovery-lazy-mode)
- [Restricting the tool surface (presets)](#restricting-the-tool-surface-presets)
- [Core navigation](#core-navigation)
- [Graph traversal](#graph-traversal)
- [Search & traversal extensions](#search--traversal-extensions)
- [Dataflow (CPG-lite)](#dataflow-cpg-lite)
- [Structural search](#structural-search)
- [Diagnostics & code actions](#diagnostics--code-actions)
- [Proactive notifications](#proactive-notifications)
- [Coding workflow](#coding-workflow)
- [Agent-optimized (token efficiency)](#agent-optimized-token-efficiency)
- [Response re-cutting](#response-re-cutting)
- [Analysis](#analysis)
- [Proactive safety](#proactive-safety)
- [Code quality](#code-quality)
- [Code generation](#code-generation)
- [PR review](#pr-review)
- [Multi-repo management](#multi-repo-management)
- [Worktree views and checkouts](#worktree-views-and-checkouts)
- [Live editor buffers (overlay sessions)](#live-editor-buffers-overlay-sessions)
- [Speculative execution](#speculative-execution)
- [MCP resources (18)](#mcp-resources-18)
- [MCP prompts (3)](#mcp-prompts-3)

## Daemon availability and embedded fallback

`gortex mcp` connects to and may auto-start the shared daemon. If no compatible daemon can be reached, it exits by default instead of starting a private in-process server. This prevents multiple MCP clients from silently indexing the same repository in parallel.

To retain the legacy standalone behavior, opt in from the user-level config (`~/.gortex/config.yaml`, or `$XDG_CONFIG_HOME/gortex/config.yaml`):

```yaml
mcp:
  allow_embedded: true
```

Repository-local `.gortex.yaml` files cannot enable this machine-level permission.

With the opt-in, daemon-unavailable launches—including `gortex mcp --index ...`—use a private temporary SQLite store that is removed on exit and rebuild the tree on every launch.

## Compact MCP surface

The compact surface consolidates the legacy catalogue into 21 domain tools with compact, stable schemas. Every MCP connection with a non-empty `clientInfo.name` selects it automatically in `hide` mode unless a higher-precedence forwarded, operator, or instruction-profile policy overrides it. Empty and pre-initialize sessions retain the server default. Select it explicitly with the neutral `compact` preset alias:

```bash
GORTEX_TOOLS=compact gortex mcp
```

With that surface, the client receives all 21 names in its first `tools/list`: `explore`, `search`, `read`, `relations`, `trace`, `analyze`, `ask`, `change`, `review`, `pr`, `recall`, `workspace`, `response`, `capabilities`, `edit`, `refactor`, `remember`, `workspace_admin`, `overlay`, `session`, and `publish_review`. They are static for the session—there is no `tools_search` promotion or `tools/list_changed` dependency. `capabilities` discovers operation schemas, not additional tool names. Session-lifetime controls use `session`, for example `{"operation":"subscribe","channel":"diagnostics"}`; durable workspace changes remain under `workspace_admin`.

```jsonc
// Read a source file.
{"name":"read","arguments":{"target":{"file":"internal/mcp/server.go"}}}

// Preview a file edit; omit dry_run (or set false) to apply it.
{"name":"edit","arguments":{"target":{"file":"internal/mcp/server.go"},"match":"old text","replacement":"new text","dry_run":true}}

// Fetch the exact schema for a read operation.
{"name":"capabilities","arguments":{"domain":"read","operation":"file","detail":"schema"}}
```

The compact surface delegates to the existing handlers. Existing `agent`, `core`, `full`, and specialist presets retain their legacy schemas; the CLI, HTTP routes, and legacy MCP names remain compatible. Names shared by both surfaces (such as `explore`, `analyze`, and `review`) advertise the compact schema only in a compact session. See the [compact MCP surface specification](mcp-facade-v1.md) for effects, schemas, migration, and acceptance gates.

Authorization follows observable effects. `analyze` is strictly read-only: `blame`, coverage enrichment, SQL rebuild, and Temporal verification are exposed through `workspace_admin`; model-assisted concepts/search and lazy graph enrichment are fixed off on local read operations. Stateful `nav` is exposed as `session(operation="cursor")`. `change.contract` cannot acknowledge risk; durable acknowledgement is explicit through `remember(operation="risk_ack")`.

## Tool discovery (lazy mode)

The fallback server default is a curated **`core`** preset in **`defer`** mode (see [presets](#restricting-the-tool-surface-presets)): ~34 dev-cycle workhorse tools are published eagerly in the initial `tools/list`, and the rest of the ~180-tool catalogue is deferred—fetched on demand through `tools_search`. Named MCP clients instead default to the static compact surface described above. Opt into the full eager surface with preset `full` (`GORTEX_TOOLS=full`).

`tools_search` returns each deferred tool's schema **inline** (in a `<functions>{…}</functions>` block) and promotes it into `tools/list`, firing `notifications/tools/list_changed`. Clients that honour that notification (or read the inline schema) reach deferred tools transparently. `GORTEX_LAZY_TOOLS=1` is the older, all-or-nothing switch that defers everything except a hard-coded hot set regardless of preset; the `core`/`defer` default supersedes it for the common case.

```jsonc
// With GORTEX_LAZY_TOOLS=1 set:
// Browse — list deferred tool names without schemas.
{"name":"tools_search","arguments":{}}

// Fetch schemas for specific tools by name (auto-promotes them into tools/list).
{"name":"tools_search","arguments":{"query":"select:flow_between,taint_paths,find_clones"}}

// Keyword search with required-token filter, ranked, capped at max_results.
{"name":"tools_search","arguments":{"query":"+overlay drop","max_results":5}}

// Fuzzy keyword match across name + description.
{"name":"tools_search","arguments":{"query":"memories invariants"}}
```

Returned tools are auto-promoted (`promote:false` opts out) and the server fires `notifications/tools/list_changed`. The `tool_profile` tool reports the active surface — which tools are live vs. deferred, their scopes and categories, the active preset (below), and (with a `tool` argument) a single tool's enabled status.

**Two front doors over one set of handlers.** The same tool handlers back both the MCP transport and the `gortex` CLI. The daemon routes a tool call **by name** over its socket, independent of which tools a given client eagerly published in `tools/list` — so the CLI reaches the **full** surface, including tools that are deferred under the `core` preset, with no `tools_search` round-trip. `gortex call <tool>` invokes any tool by name, and the dedicated CLI verb groups (`gortex edit …`, `gortex memory …`, `gortex analyze`, `gortex flow` / `taint` / `clones` / `feedback`) are ergonomic front-ends over the most-used tools. Driving Gortex through those verbs mounts no tool schemas into the model's context — see [`cli.md`](cli.md#full-tool-surface-from-the-cli) and the consumption-path trade-off in [`cli.md`](cli.md#choosing-a-consumption-path).

## Restricting the tool surface (presets)

The full ~180-tool surface is more than many agents need. A **tool preset** picks what the server publishes — the basis both for the lean shipped default and for a minimal, headless editing harness (an agent on a trusted box driving a remote daemon through a small, fixed tool set).

Eight built-in presets:

| Preset | Surface |
|--------|---------|
| `facade-v1` (**default for named MCP clients**) | Stable config identifier for the 21 static, effect-homogeneous domain tools; operation schemas are discovered through `capabilities`, with no tool promotion. Aliases: `compact`, `facade`, `agent-v2` |
| `agent` | Legacy lean coding-agent working set (~20 tools): `explore` (the one-shot localization verb) + search/navigate + read (incl. `batch_symbols`) + orient + edit/verify. Parameter descriptions are compacted (the full prose is one `tools_search` / `full` hop away). Aliases: `coding-agent` |
| `core` (**fallback server default**) | the curated dev-cycle set (~35 tools): orient (incl. `explore`) + search/navigate + read + edit + verify/test + `analyze` + review + the memory workflow. Aliases: `default`, `classic` |
| `full` | every tool (the pre-`core` behaviour — opt back in here) |
| `readonly` | everything except the mutating tools (`edit_file`, `write_file`, `index_repository`, …) |
| `edit` | the minimal headless editing set — orient + navigate + mutate + verify (`smart_context`, `search_symbols`, `find_files`, `edit_file`, `verify_change`, `get_test_targets`, …) |
| `nav` | read-only navigation / exploration; no editors |
| `localization` | the diet "where is the code that does X" set (~10 tools, read-only, compacted descriptions): `smart_context` + search + trace + read. The eager list is sourced from the instruction-profile table, so this surface and the `localization` profile's instructions body cannot drift. Aliases: `locate`, `find` |

For legacy presets, `tool_profile` and `tools_search` are always kept. The compact surface uses `capabilities` instead and is closed: it always contains exactly its 21 public tools, so `allow` / `deny` deltas are ignored for `facade-v1`. Select a legacy or custom surface when per-tool deltas are required.

**Client-aware default.** With no higher-precedence selection, every connection with a non-empty MCP `clientInfo.name` gets the compact 21-tool surface in `hide` mode. Empty and pre-initialize sessions retain the server default. Client identity and wire format are separate: an unknown named client still gets the compact tools but remains on JSON unless it is independently GCX-capable. `GORTEX_TOOLS` always overrides. The `gortex mcp` proxy forwards its `GORTEX_TOOLS` / `--tools` to the daemon in the handshake, so a client's preset applies over the shared daemon (it can both narrow and widen the surface, not just subtract).

**Instruction profiles.** The machine's active instruction profile (`gortex instructions switch <core|localization|full>` — see [`cli.md`](cli.md#gortex-instructions--instruction-profiles)) can carry a tool preset; sessions pick it up between the forwarded spec and the client-aware default. Full precedence: **forwarded spec (`GORTEX_TOOLS` / `--tools`) > operator-pinned `mcp.tools` config > active instruction profile > client-aware default > server default**. The shipped `core` profile carries no preset, so nothing changes until a machine explicitly switches; profile changes apply to new sessions only.

**Two modes** (`mode`):

- `defer` (the default mode for `core`) — non-allowed tools are kept out of the cold `tools/list` but stay reachable through `tools_search`, which returns their schema inline and promotes them (firing `notifications/tools/list_changed`). The lean-but-complete surface: nothing is lost, the rare tool is one discovery call away.
- `hide` (the default for `facade-v1` and the explicit `edit` / `nav` / `readonly` harness presets) — non-allowed tools are removed from `tools/list` **and** calls to them are hard-blocked. The locked-down surface; works identically on every client.

Select a preset three ways (precedence: **env > flag > config > default**):

```yaml
# .gortex.yaml — config file
mcp:
  tools:
    preset: full          # compact | agent | core (default) | full | readonly | edit | nav
    mode: defer           # defer | hide
    allow: [find_files]   # add tools on top of the preset
    deny: [write_file]    # remove tools from the preset
```

```bash
# env (overrides config) — spec is "preset,+add,-remove"
export GORTEX_TOOLS="edit,+analyze,-write_file"
export GORTEX_TOOLS_MODE=hide

# CLI flags (override config; env still wins)
gortex mcp --tools edit --tools-mode hide
gortex daemon start --tools readonly      # propagates to the detached child
```

**Per-connection (client-driven) scoping.** The selectors above applied to `gortex daemon start` narrow the *whole daemon* for every client. To let one client pick its own surface while the daemon keeps serving the full set to everyone else, set `--tools` / `GORTEX_TOOLS` on that client's **`gortex mcp`** invocation — the stdio proxy filters just that connection's `tools/list` and blocks calls to tools outside the set. Because the filter applies from the first `tools/list`, it works on **every** MCP client (no `tools/list_changed` dependency).

```jsonc
// An MCP client config giving this client a minimal editing surface,
// against a daemon that still serves the full catalogue to others:
{
  "command": "gortex",
  "args": ["mcp", "--tools", "search_symbols,find_files,edit_file,verify_change"],
  // or: "args": ["mcp", "--tools", "edit"]  (a named preset)
  // or: "env": { "GORTEX_TOOLS": "readonly" }
}
```

A spec whose first token isn't a known preset (`search_symbols,find_files,…`) is an **explicit allow list** — exactly those tools — for experts who want to hand-pick the surface. A known preset followed by names (`edit,find_files`) keeps preset semantics plus the extra tools.

`tool_profile` reports the active `preset` / `preset_mode`, the narrowed `live` set, and a `categories{}` map grouping every tool into a functional family (nav / read / edit / analysis / review / pr / memory / overlay / subscription / enrich / workspace / admin) for prefix-style filtering.

**Prompt-injection screening.** Every tool call is screened by middleware that scans arguments and result text for injection patterns. On a hit it attaches a non-blocking `_meta.gortex_security` advisory — the call still succeeds and the result body is never mutated. Disable with `GORTEX_MCP_SANITIZE=0`.

**Unknown-option guard.** Tools published with a closed schema (`additionalProperties: false`) enforce it at dispatch. By default an unknown option still executes the call and the result carries an `_ignored_options` rider naming the unknown keys and the valid ones — the self-correct signal for a mistyped or hallucinated option (#597). `GORTEX_TOOL_ARG_GUARD=reject` upgrades that to a refusal before the handler runs; `GORTEX_TOOL_ARG_GUARD=0` / `false` / `off` / `no` disables enforcement. Response-shaping keys generic layers honor on any tool (`format`, `fields`, `max_bytes`, `max_tokens`, `cursor`) are always accepted, and facade tools are exempt — their compatibility wrappers deliberately take legacy call shapes.

## Core navigation

| Tool | Description |
|------|-------------|
| `graph_stats` | Node/edge counts by kind, language, per-repo stats, session token savings, and an `edge_identity_revisions` counter (edges re-keyed when their provenance changed) |
| `search_symbols` | Find symbols by name (replaces Grep). Inline `kind:`/`flavor:`/`lang:`/`path:` field clauses (also a top-level `flavor` param) + `query_class` / `max_per_file` tuning; accepts `repo`, `project`, `ref`, `scope` params. `flavor:` filters type nodes by their structural shape — `class`/`struct`/`enum`/`interface`/`trait`/`protocol`/`object`/`record`/`type_alias`/`newtype`/`message`/`service`/`table`/`view`/`module`/… — with `flavor:component` spanning every UI component (React / Vue / Svelte / SwiftUI / Compose / Flutter / Angular / …); a `kind:class`-style value that is only a flavor routes to this filter automatically. `corpus: code\|docs\|all` selects the corpus (`docs` has its own retrieval channel + prose-tuned ranking); `vocab_anchored: true` constrains LLM expansion to the repo's own vocabulary; a zero-result identifier query is auto-decomposed into leaf terms (`decomposed: true`) |
| `search_text` | Trigram-accelerated literal (or `regexp: true`) code search across the repo — the alt grep backbone. Returns file/line/text rows, each carrying the enclosing symbol (`symbol_id` / `symbol_name`) |
| `find_files` | Find source files by **name** — the file-name counterpart of `search_symbols`. `query` (basename/path substring, ranked exact > prefix > substring) and/or `glob` (e.g. `internal/**/*_test.go`), with optional `fuzzy` subsequence matching and `path` / `repo` scoping. File nodes are excluded from the symbol index, so `search_symbols kind:file` cannot return them — use this |
| `winnow_symbols` | Structured constraint-chain retrieval — `kind`, `language`, `community`, `path_prefix`, `min_fan_in`, `min_fan_out`, `min_churn`, `text_match` with per-axis score contributions |
| `get_symbol` | Symbol location and signature (replaces Read). Accepts `repo`, `project`, `ref` params |
| `get_file_summary` | All symbols and imports in a file. Accepts `repo`, `project`, `ref`, `max_bytes` / `max_tokens` budget caps |
| `get_editing_context` | **Primary pre-edit tool** — symbols, signatures, callers, callees. Accepts `max_bytes` / `max_tokens` budget caps; `compress_bodies` stubs bodies, and `fidelity_globs` (e.g. `internal/**:full,*_test.go:omit,vendor/**:compress`) sets a per-glob full/compress/omit tier |
| `get_repo_outline` | Narrative single-call repo overview — top languages, communities, hotspots, most-imported files, entry points |
| `plan_turn` | Opening-move router — returns ranked next calls with pre-filled args for a task description (~200 tokens) |

## Graph traversal

| Tool | Description |
|------|-------------|
| `get_dependencies` | What a symbol depends on |
| `get_dependents` | What depends on a symbol (blast radius) |
| `get_call_chain` | Forward call graph. Accepts `max_bytes` / `max_tokens` budget caps |
| `get_callers` | Reverse call graph. Carries a `caveat` whenever the answer must not be taken at face value, so a pre-edit safety check isn't silently disarmed: `likely_unused` (indexed, nothing uses it), `possible_extraction_gap` (no edges at all — the extractor probably missed it), or `coverage_incomplete` (the only evidence is import-level, unresolved same-name candidates, or callers matched by name alone — including a populated caller list where *every* row is a name-only match) |
| `find_usages` | Every reference to a symbol. Each usage carries its reference `context` (parameter_type / return_type / field / value / type / attribute / call); pass `context:` to filter (e.g. "where is this type used as a parameter?"). `flavor:` filters by where a usage originates — a type flavor resolves the usage's enclosing owner type ("usages from inside a struct"), and `flavor:component` keeps usages originating inside a UI component; each usage surfaces the resolved `from_type_flavor` / `from_ui_component`. Accepts `max_bytes` / `max_tokens` budget caps. Carries the same `caveat` as `get_callers` — `likely_unused` / `possible_extraction_gap` / `coverage_incomplete`, the last also on a populated result whose every usage is a name-only match |
| `find_implementations` | Types implementing an interface |
| `find_overrides` | Methods that override (children) or are overridden by (parents) a method — backed by `EdgeOverrides` |
| `get_class_hierarchy` | Multi-hop inheritance subgraph around a type, interface, or method. Walks `EdgeExtends` + `EdgeImplements` + `EdgeComposes` (type nodes) and `EdgeOverrides` (method nodes); `direction` ∈ up / down / both, `include_methods` pulls members + their override chain |
| `get_cluster` | Bidirectional neighborhood |

## Search & traversal extensions

| Tool | Description |
|------|-------------|
| `find_declaration` | Use-site → declaration resolver. Accepts a literal substring or (with `regex: true`) a regex matching a use site like `fooBar(`; returns the declaration node plus the matching use locations. Trigram-prefiltered. Optional `path_prefix` / `kind` filters |
| `walk_graph` | Token-budgeted free-form graph traversal — walks arbitrary `edge_kinds` (CSV) outward / inward / both from a starting symbol; auto-stops at `token_budget`. Surfaces `budget_hit` / `stopped_at_depth` on the response. `community` (ID or label) confines the walk to a detected community |
| `context_closure` | Dependency-closure context selection — given a set of seed files / symbols, walks the transitive import / dependency closure and packs it under one `token_budget` (reusing the graded-manifest tiers), ranked by graph distance from the nearest seed or, with `rank: "proximity"`, by seeded random-walk proximity |
| `graph_query` | Ad-hoc graph-query escape hatch — small read-only DSL with `nodes` / `traverse` / `filter` stages joined by `\|`, e.g. `nodes kind=interface name~Handler \| traverse implements in \| filter path=internal/mcp/`. Bounded by `limit` and a five-stage cap |
| `nav` | Per-session symbol cursor — verb-dispatched via `action`: `goto` / `into` (a callee) / `up` (a caller) / `sibling` / `back` / `where` / `read`. Adjacency preview rides on every response; the cursor lives in session state and resets on disconnect |

## Dataflow (CPG-lite)

| Tool | Description |
|------|-------------|
| `flow_between` | Ranked dataflow paths between two symbols — walks `value_flow` / `arg_of` / `returns_to` edges |
| `taint_paths` | Pattern-driven source→sink dataflow sweep for security and architecture audits |

## Structural search

| Tool | Description |
|------|-------------|
| `search_ast` | Cross-language structural search by AST shape — raw tree-sitter S-expression `pattern` or a bundled `detector` (e.g. `sql-string-concat`, `weak-crypto`, `hardcoded-secret`) |

## Diagnostics & code actions

Wired across every running language server (gopls, tsserver, pyright, rust-analyzer, …). Server-driven capability registration via `client/registerCapability` / `client/unregisterCapability` is honoured live, so servers (jdtls, tsserver, rust-analyzer) that announce features *after* `initialize` no longer return empty results.

| Tool | Description |
|------|-------------|
| `subscribe_diagnostics` | Opt the session into push `notifications/diagnostics`; initial state replays immediately, deltas thereafter. Filter by `min_severity` / `path_prefix` |
| `unsubscribe_diagnostics` | Opt back out — idempotent, fires automatically on session disconnect |
| `get_diagnostics` | Latest stored diagnostics for a file; `wait: true` blocks on the first publish |
| `get_code_actions` | LSP code actions (quickfix / organizeImports / refactor / source) at a file location |
| `apply_code_action` | Apply a single code action to disk — atomic temp+rename |
| `fix_all_in_file` | Loop codeAction → apply → re-collect until convergence over the whole file |

## Proactive notifications

Four additional push channels modeled on `subscribe_diagnostics` — per-session opt-in, delta-filtered, initial replay, auto-cleanup on disconnect.

| Tool | Description |
|------|-------------|
| `subscribe_workspace_readiness` | `notifications/workspace_readiness` — daemon warmup phase transitions (snapshot_loaded → parallel_parse → deferred_passes_all → global_resolve → end_batch → watcher_started → ready). Last-known phase replayed to late subscribers. A graph tool *called during warmup* does not need this subscription to cope: it returns an in-band `warming` block plus best-effort partial results instead of blocking or erroring |
| `unsubscribe_workspace_readiness` | Opt back out — idempotent |
| `subscribe_daemon_health` | `notifications/daemon_health` — periodic ticker (default 15 s, `interval_ms` clamped to 1 s..5 min) snapshots uptime, alloc/sys/heap, num_goroutine, num_gc, tracked_repos, sessions, lsp_alive, graph nodes/edges. Ticker only runs while ≥1 subscriber is attached |
| `unsubscribe_daemon_health` | Opt back out — idempotent |
| `subscribe_stale_refs` | `notifications/stale_refs` — per-session intersect of watcher symbol-change events against the session's viewed/modified working set. Fires only when a change actually touches what *this* session has consumed |
| `unsubscribe_stale_refs` | Opt back out — idempotent |
| `subscribe_graph_invalidated` | `notifications/graph_invalidated` — coarse "the graph was rebuilt, drop cached results" signal. `{node_count, edge_count, reason, ts}`; unfiltered |
| `unsubscribe_graph_invalidated` | Opt back out — idempotent |

## Request lifetime (why a call can never hang)

Every tool call and resource read is bounded, on every transport. A request that
overruns its budget is **abandoned**: the client gets a terminal, structured
error and the transport's slot is released immediately, so the session keeps
serving later requests. The handler itself keeps running — it may be inside a
store call that cannot be interrupted — so treat any side effect of an abandoned
call as *unknown* and re-read before assuming it did or did not land.

| Layer | Bound | Knob |
|---|---|---|
| Tool call / resource read / prompt fetch (all transports) | 60 s | `GORTEX_MCP_TOOL_TIMEOUT` (Go duration; `off` disables) |
| Daemon socket, per JSON-RPC request | 60 s, terminal `-32001` | — |
| Daemon socket, concurrent dispatches | 8 | `GORTEX_MCP_MAX_CONCURRENT_DISPATCHES` (max 64) |
| Control RPC (`daemon status` / `search_symbols` / `proxy`) | 30 s, terminal `timeout` | — |
| Control RPC (`shutdown`) | unbounded on the daemon — the store flush precedes the ack. `gortex daemon stop` bounds its own wait at 30 s, then watches the process for up to 2 min and force-kills | — |
| Control RPC (`track` / `untrack` / `reload` / `enrich_*`) | unbounded by design | — |

The handler bound fires just inside any deadline the transport already imposed,
so the client receives the tool-shaped diagnosis rather than an opaque transport
timeout. Track / reload / enrichment are deliberately left unbounded: they are
long by design, and a user who starts one is waiting on purpose. Shutdown is
unbounded for a different reason — abandoning a half-done store flush is worse
than a slow stop — so the *command* carries the bound instead.

`GORTEX_MCP_TOOL_TIMEOUT` is read in the server's own process, so set it in the
**daemon's** environment (or, when embedded fallback is enabled, the `gortex mcp` process),
not in the MCP client's config. It can always tighten the bound. It can only
*raise* it past 60 s on the embedded stdio server and Streamable HTTP — on the
daemon socket the per-request lifetime is a hard 60 s that clamps it. Raise it
when a tool legitimately runs longer than a minute: a first `index_repository`
over a very large tree, or `ask` against a slow local model.

## Coding workflow

| Tool | Description |
|------|-------------|
| `get_symbol_source` | Source code of a single symbol (80% fewer tokens than Read). Returns `tokens_saved` per call. `compress_bodies` stubs bodies (with an optional `keep` subset); `max_lines` salience-truncates to a control-flow skeleton |
| `batch_symbols` | Multiple symbols with source and a capped 1-hop callers/callees sample (`callers_truncated` / `callees_truncated` mark a cut; full neighbourhood is `get_callers` / `get_call_chain`) |
| `find_import_path` | Correct import path for a symbol |
| `explain_change_impact` | Risk-tiered blast radius with affected processes. A zero-edge target carries the same per-symbol `likely_unused` / `possible_extraction_gap` / `coverage_incomplete` caveat as `get_callers` |
| `get_recent_changes` | Files/symbols changed since timestamp. Rows are clamped to the session workspace and narrowed further by `repo`/`project`/`scope`; each multi-repo row names its `repo` |
| `edit_symbol` | Edit a symbol's source directly by ID — no Read needed. Line-ending tolerant: an LF-authored `old_source` matches a CRLF file (and vice versa) and the replacement adopts the file's endings (`eol_normalized: true` rides on the response). Optional `base_sha` content-hash guard refuses the write when the on-disk SHA has drifted; every success carries `new_sha` so the next edit can pipeline without re-reading |
| `edit_file` | Edit any file (markdown, config, spec, template, source) by exact string replacement — accepts absolute paths or repo-rooted paths. Line-ending tolerant: an LF-authored `old_string` matches a CRLF file (and vice versa) and the replacement is written with the file's own endings (`eol_normalized: true` rides on the response). Same `base_sha` / `new_sha` drift guard. Kills Read-before-Edit for files not in the graph |
| `write_file` | Create or overwrite any file — atomic temp+rename, re-indexes on write. Same `base_sha` / `new_sha` drift guard |
| `mutation_status` | What a file mutation actually did, after the fact. Reports `disk_status` (`committed` / `not_applied` / `failed` / `in_flight`) separately from `graph_status` (`fresh` / `pending` / `stale` / `failed`), selected by `receipt`, `mutation_id`, or `path`. Use it instead of retrying when an edit call was abandoned at its deadline |
| `rename_symbol` | Coordinated multi-file rename with all references — definition, graph usages, receiver lines, and test names that embed the old identifier. Replacement is whole-identifier, so renaming `Get` leaves `GetUser` intact. Every target line is re-verified against disk and every affected file is parse-gated before anything is written, so the rename lands completely or is refused; `dry_run: true` returns the identical edit list without writing. Successful responses carry `status` (`applied` / `would_apply` / `no_edits`) plus per-file `bytes_written` / `new_sha` / `reindexed`. An existing unindexed target returns a structured `symbol_not_indexed` error only when the configured extractor anchors the requested declaration. Its `safe_fallback.request` is a guarded exact edit of that declaration line (`scope: declaration_only`); same-file and cross-file references remain explicitly unproven, and the refusal itself writes no bytes |
| `move_symbol` | Relocate a function / method / type / variable / const to another file. Cross-package moves rewrite every qualified reference, drop the source import, add the target import, synthesise the target file if missing. Go for now |
| `inline_symbol` | Replace every callsite of a trivial single-statement / single-expression callee with the body — refuses cleanly on defer, spawn, close-over-scope, multi-return, or side-effecting arg. `delete_after: true` removes the declaration. Go for now |
| `safe_delete_symbol` | Atomic dead-code removal with a graph-aware safety gate. A `cascade` parameter (`off` / `preview` / `apply`) drives a fixed-point orphan-propagation pass; cross-workspace and out-of-closure callers (and, by default, test-only callers) disqualify a candidate |
| `set_planning_mode` | Switch the session between a guaranteed no-writes planning phase and editing mode |
| `workflow` | Drive a phase-enforcement state machine (explore → implement → verify) — editing tools are gated until the implement phase |

### Content hashes: five fields, five meanings

Several tools return a hash and none of them are interchangeable. Pick by what
you need to prove, not by which field is nearest.

| Field | Algorithm | Covers | Observed when |
|---|---|---|---|
| `base_sha` (request) | git blob SHA-1 | the full file the caller read earlier | — (a value you supply) |
| `new_sha` (response) | git blob SHA-1 | the bytes the tool was **about to** write | **before** the write |
| `content_sha256` (`read_file`) | SHA-256 | the full file on disk | during the read |
| `before_sha256` / `after_sha256` (mutations) | SHA-256 | the full file on disk, before and after | before / **after** the write |
| `etag` | SHA-256 of the response payload | the returned representation, not the file | as the response is built |

Two consequences worth internalising:

- **`new_sha` is a pipelining token, not physical evidence.** It is a git blob
  SHA-1 (`sha1("blob <len>\0" + data)`, the same value `git hash-object` prints)
  computed from the in-memory buffer *before* `AtomicWriteFile` runs. It is what
  you feed to the next call's `base_sha`. It is not a SHA-256, it is not read
  back from disk, and `dry_run` returns one for bytes that were never written.
- **`etag` is not a file hash.** It covers the JSON response — window, elision,
  redaction and all — so two reads of an unchanged file with different options
  carry different ETags.

To prove what is actually on disk, ask for it explicitly with
`physical_evidence: true` (see below). Anything else is a prediction or a
cache key.

### Disk-verified receipts (`physical_evidence`)

`read_file`, `edit_file`, `write_file` and `edit_symbol` accept
`physical_evidence: true` (with an optional `digest`, `sha256` only, which is
also the default). The read side returns `content_sha256` plus `hash_scope`,
`content_source`, `same_buffer_as_content` and `resolved_path`; the mutation
side returns `before_sha256` / `after_sha256` re-read from disk **after** the
atomic write, plus `resolved_path`, `byte_count` and `verified_at`. Both are
`hash_algorithm: sha256`, `hash_scope: full_file`, `content_source: disk`.

Contract details that matter for an evidence workflow:

- **The mutation digest is an observation, not a prediction.** The file is read
  back through the same hardened path `read_file` uses — non-regular files
  refused, symlink resolution recorded, repo-root confinement re-checked against
  the target that supplied the bytes.
- **A creating `write_file` reports `before_absent: true`** instead of a
  `before_sha256`, so "no prior bytes" is distinguishable from "not requested".
- **`dry_run` + `physical_evidence` is refused.** A dry run writes nothing, so
  there are no disk bytes to attest.
- **Evidence failure never fails the call.** If the read-back cannot be
  completed or confinement is violated, the write is still reported as applied
  and the response carries `evidence_error` with no digests — failing the call
  would invite a retry that applies the edit twice.
- **Secret-shaped config leaves are withheld.** `before_sha256` digests bytes
  the caller did not supply, so a `.env`-shaped file refuses the receipt (the
  edit itself still works). Read its digest with
  `read_file{allow_secrets: true}` if you genuinely need it.
- Multi-file mutations (`rename_symbol`, `batch_edit`) do not yet take the flag;
  `batch_edit`'s transaction journal carries its own `before_sha256` /
  `after_sha256`, computed from in-memory buffers for crash recovery — not a
  disk observation.

Whether the graph caught up with the write is a separate question, answered by
`graph_status` (and the `reindexed` / `reindex_pending` / `reindex_generation`
fields behind it) on every mutation response. Whether the write happened *at
all* is a third question — see the next section.

### Mutating tools and the transport deadline

A tool call is bounded (`GORTEX_MCP_TOOL_TIMEOUT`, default 60s). When a handler
outruns that budget the transport is released and the handler keeps running, so
a write can land *after* the client was answered. The mutating tools make that
observable instead of leaving it unknown:

- **Before the disk commit**, a cancelled request is refused and nothing is
  written. The response says `disk_status=not_applied` and retrying is safe.
- **After the disk commit**, the abandoned-call error names what landed —
  path, `new_sha`, and a receipt id — and carries a machine-readable
  `mutation_commit={...}` tail. Re-applying the edit would duplicate it.
- **Either way**, the receipt is queryable for 30 minutes with
  `mutation_status` (facade: `change` with `operation: "receipt"`), which
  reports the disk state and the graph-freshness state independently.
- `edit_file` / `write_file` / `edit_symbol` accept an optional `mutation_id`
  idempotency key. Retrying with the same key and the identical edit replays
  the original result without writing again; reusing it for a different edit is
  refused. `batch_edit` has the equivalent `transaction_id`.

Successful edit responses carry `disk_status`, `graph_status`, and
`mutation_receipt` alongside the existing `new_sha` / `reindexed` fields.

`physical_evidence` and `disk_status` answer different questions and compose:
the first attests *what bytes* are on disk, the second whether the write
happened at all. A call abandoned before its response returns no evidence block
— only the receipt — which is exactly when `disk_status` is the field you need.

## Agent-optimized (token efficiency)

| Tool | Description |
|------|-------------|
| `explore` | One-shot localization: free task/bug text in, the ranked neighborhood out — likely symbols with source + call paths (1-hop callers/callees), a file map, and a completeness cue, packed under a `token_budget` (default 9000; bodies demote to signatures past it, truncation reported honestly). The opening move for any task-shaped request — folds the whole search/read/callers exploration phase into one call |
| `smart_context` | Task-aware minimal context — replaces 5-10 exploration calls. The working set is ranked through the full rerank pipeline. Always emits a `blast_radius` block (callers grouped by file + covering tests + a `no covering tests found` warning) and a file-clustered `working_set`; seed count and `token_budget` scale with graph size when unset. `fidelity: "graded"` returns a graph-distance-tiered `context_manifest` (large interchangeable symbol families are skeletonized to one representative) under one `token_budget`; `estimate: true` projects token cost without fetching; `if_none_match` dedups an unchanged pack to `not_modified` |
| `get_edit_plan` | Dependency-ordered edit sequence for multi-file refactors |
| `get_test_targets` | Maps changed symbols to test files and run commands |
| `get_untested_symbols` | Inverse of `get_test_targets` — functions/methods not reached from any test file, ranked by fan-in |
| `suggest_pattern` | Extracts code pattern from an example — source, registration, tests |
| `export_context` | Portable markdown/JSON context briefing for sharing outside MCP |
| `feedback` | `action: "record"`: report useful/missing symbols. `action: "query"`: aggregated stats — most useful, most missed, accuracy metrics |
| `ask` | Optional in-process LLM research agent (`-tags llama` + `llm.model`) — navigates the graph and returns a synthesized answer; `chain: true` for cross-repo call-chain tracing |

## Response re-cutting

Gortex captures every large tool response into a bounded per-session ring; these tools re-cut a captured response without re-issuing the original query.

| Tool | Description |
|------|-------------|
| `ctx_stats` | List the session's buffered responses — handles, tools, line / byte / token counts |
| `ctx_grep` / `grep_results` | Regex (or literal) search over a buffered response — structured `matches[]` plus a grep-style block with `-A`/`-B`/`-C` context |
| `ctx_slice` | An explicit line range of a buffered response |
| `ctx_peek` | Head + tail preview of a buffered response |
| `head_results` | The first N lines of a buffered response |

## Analysis

| Tool | Description |
|------|-------------|
| `get_communities` | Functional clusters (Louvain). Without `id`: list all. With `id`: members and cohesion for one community. Members and files are clamped to the session workspace; the partition is global, so a `repo`/`project`/`scope` narrowing is widened to the workspace and the response discloses it |
| `get_processes` | Discovered execution flows. Without `id`: list all. With `id`: step-by-step trace. Clamped to the session workspace and narrowed further by `repo`/`project`/`scope` — out-of-scope steps are excised by subtree so the surviving chain keeps its real call shape |
| `detect_changes` | Git diff mapped to affected symbols, plus a file-level view (`changed_files` / `file_changes` with added/modified/deleted/renamed) that stays populated for changes carrying no indexed symbol. Untracked and ignored files are not observed — every scope reads `git diff`. |
| `index_repository` | Index or re-index a repository path |
| `reindex_repository` | Incrementally re-index a tracked repository — whole-root, or scoped to an optional `paths` subset. Multi-repo aware |
| `contracts` | API contracts. `action: "list"` (default): detected HTTP/gRPC/GraphQL/topics/WebSocket/env/OpenAPI. `action: "check"`: orphan providers/consumers |
| `find_co_changing_symbols` | Ranked git co-change neighbours for a symbol — over the mined cosine-weighted `co_change` edge layer |
| `search_artifacts` | Full-text search over the context-artifacts manifest — DB schemas, API specs, infra configs, ADRs registered via `.gortex.yaml::artifacts` |
| `get_artifact` | Fetch one context artifact by id, with its content and the symbols it references |

## Proactive safety

| Tool | Description |
|------|-------------|
| `verify_change` | Check proposed signature changes against all callers and interface implementors |
| `check_guards` | Evaluate project guard rules (`.gortex.yaml`) against changed symbols |
| `audit_agent_config` | Scan CLAUDE.md / AGENTS.md / `.cursor/rules` / `.github/copilot-instructions.md` / `.windsurf/rules` / `.antigravity/rules` for stale symbol references, dead file paths, and bloat — validated against the live graph |

## Code quality

| Tool | Description |
|------|-------------|
| `analyze` | Unified graph analysis dispatcher. `kind` ∈ `dead_code`, `hotspots`, `cycles`, `would_create_cycle`, `connectivity_health`, `todos`, `blame`, `coverage`, `coverage_gaps`, `coverage_summary`, `stale_code`, `stale_flags`, `ownership`, `releases`, `cgo_users`, `wasm_users`, `orphan_tables`, `unreferenced_tables`, `channel_ops`, `goroutine_spawns`, `field_writers`, `race_writes`, `unclosed_channels`, `unsafe_patterns`, `health_score`, `impact`, `annotation_users`, `config_readers`, `env_var_users`, `sql_call_sites`, `fixes_history`, `edge_audit`, `domain`, `named`, `tests_as_edges`, `clusters`, `event_emitters`, `pubsub`, `string_emitters`, `error_surface`, `log_events`, `sql_rebuild`, `external_calls`, `routes`, `models`, `components`, `k8s_resources`, `images`, `kustomize`, `cross_repo`, `dbt_models`, `synthesizers`, `resolution_outcomes`. `clusters` takes an `algorithm` arg (`leiden` / `louvain` / `spectral`). `impact` takes an optional `target` (`{symbol}` or `{file}`): with one it ranks that target's blast radius — the target row first, then its transitive dependents with their `depth` — and reports the closure width plus whether it is exact; without one it ranks the whole repo. An unresolvable target is a structured error, never a silent fall-back to the repo-wide ranking. `synthesizers` rolls up every framework-dispatch-synthesized edge by the pass that produced it; `resolution_outcomes` classifies unresolved call/reference edges by why the resolver gave up (`ambiguous_multi_match` / `candidate_out_of_scope` / `cross_language_only` / `stub_only` / `no_definition`) |
| `find_clones` | Near-duplicate function/method clusters from the MinHash + LSH `similar_to` layer; `dead_only: true` finds dead duplicates of live code |
| `index_health` | Health score, parse failures, stale files, language coverage, tracked-repo path liveness (`tracked_repo_paths_ok` + `missing_repo_paths` — a repo whose directory was deleted still holds its registration and silently drops out of workspace-wide answers), per-(repo, provider) semantic-enrichment lifecycle (`semantic_enrichment`: running / completed / partial / abandoned / failed with edge counts, plus a `semantic_enrichment_ok` rollup) — a green file count with a `partial` enrichment state means LSP-tier edges are incomplete. `path_liveness` asks the same question one level down, per file: it stats the paths the graph itself claims and reports how many indexed files no longer exist on disk (`orphan_files` / `orphan_rate` / `orphans_by_repo`, sampled with `truncated: true` past 20k files). `stale_files` only covers files the daemon still tracks, so a deletion it never witnessed shows up here and nowhere else; a non-zero `orphan_files` caps `health_score` |
| `get_symbol_history` | Symbols modified this session with counts; flags churning (3+ edits) |
The `analyze` dispatcher also accepts a set of **facade-aliased kinds** that route to the captured legacy handler instead of the dispatcher switch: `processes` → `get_processes`, `communities` → `get_communities`, `contracts` → `contracts`, `architecture` → `get_architecture`, `clones` → `find_clones`, `health` → `audit_health`, `inspections` → `run_inspections`, `recent_changes` → `get_recent_changes`, and the other entries of the facade analyze migration table (see `mcp-facade-v1.md`). These aliases are **surface-independent**: they work for named (facade-v1), unnamed (legacy), and session-less HTTP callers alike, with no `tools_search` promotion — the HTTP dashboard endpoints depend on this under the `core`/`defer` default.


The in-graph coverage tools above (`analyze kind=coverage*`, `index_health` language coverage) have an offline, whole-corpus counterpart for regression testing: the `gortex eval parity` CLI benchmarks per-language *resolved cross-file-dependent* coverage against a frozen baseline and is CI-fenced three ways — a per-language coverage floor, a frozen at-or-beyond-parity language count, and per-feature extraction goldens. See [features.md](features.md#coverage-churn-ownership).

## Code generation

| Tool | Description |
|------|-------------|
| `scaffold` | Generate code, registration wiring, and test stubs from an example symbol |
| `batch_edit` | Atomically apply `edit_symbol`, `edit_file`, `move_file`, and `delete_file` operations with durable rollback receipts |
| `diff_context` | Git diff enriched with callers, callees, community, processes, per-file risk |
| `prefetch_context` | Predict needed symbols from task description and recent activity. Accepts `max_bytes` / `max_tokens` budget caps |

## PR review

A graph-grounded pull-request review surface. The forge-data tools self-serve PR data via the daemon's own forge client (needs `GH_TOKEN` / `GITHUB_TOKEN` in the daemon environment), or accept caller-supplied data to skip the network; all are read-only. The review gate is AST-grounded — the deterministic correctness rulepack runs over the changeset and a graph-grounding pass drops false positives, with an opt-in LLM fold-in. The CLI exposes the same surface as `gortex prs` / `gortex review` ([cli.md](cli.md#pull-request-review)).

| Tool | Description |
|------|-------------|
| `list_prs` | List a repo's PRs with a one-shot review-state classification — a state label (DRAFT / BASE_MISMATCH / CHANGES_REQUESTED / APPROVED / STALE / READY), a normalized CI rollup (NONE / FAILURE / PENDING / SUCCESS), and merge blockers. Pass `prs` to classify an already-fetched set with no network call |
| `get_pr_impact` | Graph-joined blast radius + risk score for one PR — maps the PR's changed files to symbols, scores five risk axes (blast-radius flow, caller fan-in, coverage gap, security keywords, community span), groups the affected surface by community and caller/test file. `receipt: true` emits a privacy-safe review receipt |
| `triage_prs` | Rank a repo's open PRs by graph-derived review priority — `get_pr_impact` per PR ordered by composite risk (deterministic; `use_llm` re-ranks with one compact LLM pass + per-PR rationale). Decides which PR to review first |
| `pr_risk` | PR-level composite risk score for a set of changed symbols — five 0-100 axes into one score + a LOW/MEDIUM/HIGH/CRITICAL level and an ordered `review_priorities` list. Pass `ids` (mapped symbol IDs) or `base` (a git ref — changed set from the diff) |
| `conflicts_prs` | Surface merge-order conflict risk — maps each open PR to the graph communities it touches and reports the communities touched by more than one PR, with colliding PR numbers, a suggested safe merge order, and a conflict-risk score. Plan a merge train that minimises rebases |
| `suggest_reviewers` | Rank the people / teams best placed to review a changeset — blends CODEOWNERS matches, recent authorship of the changed symbols, and co-change experts into one ranked list with per-reviewer reasons. Pass `ids`, `base`, or `number` |
| `suggested_review_questions` | Prioritised, symbol-anchored review questions mapping the changeset to graph anomalies — bridge / hub_risk / surprising / thin_community / untested_hotspot — each tied to a symbol id + file + line with a HIGH/MEDIUM/LOW severity |
| `pr_review_context` | Deterministic, LLM-free PR-review rollup in one call — composes `diff_context`, `verify_change`, `simulate_chain` (gated on an explicit overlay session), and `audit_agent_config` into a composite PASS / WARN / BLOCK verdict. The cheap counterpart to `review_pack` |
| `sibling_diff_context` | Raw unified diff of the OTHER changed files in a changeset — the sibling changes a per-symbol / per-file review view filters out, ranked by relatedness to the focus (shared community/process → co-change → directory proximity) |
| `review` | Review a changeset → line-anchored inline comments + a BLOCK/REVIEW/APPROVE verdict. Runs the deterministic correctness rulepack (graph-grounded to drop false positives) over the changeset (`base` / `scope`, or a pasted `diff`); `use_llm` folds in LLM findings relocated to exact lines |
| `review_pack` | The single AST-grounded PR-review entrypoint — folds the graph-grounded review, per-symbol semantic classification, per-file risk, contract-impact + guard/architecture checks, and impacted test targets into one envelope, with a derived `verification_command` and a privacy-safe receipt |
| `critique_review` | Second, adversarial self-critique pass over a prior review's findings — asks the LLM (grounded in the diff) which findings are genuine vs false positives, returns the kept set, the dropped set each with a reason, and a revised verdict. Conservative: a disabled LLM keeps everything |
| `post_review` | Post review findings as inline comments on a GitHub PR / GitLab MR — each anchored to its file + line, batched into one review. Every body is secret-redacted before any payload is built; public / fork PRs require `confirm_public: true`; `dry_run: true` returns the would-post payloads with no network call |
| `suppress_finding` | Durably silence a review finding as a false positive (or `list` / `remove`) for the current repo — keyed over rule / category / symbol / file / source text so it survives the finding shifting lines. A permanent per-repo never-flag-again list (sidecar-backed) |

`analyze` also takes `kind: "review"` — the idiomatic/correctness rulepack (NPE / thread-safety check-then-act / N+1 / logic-error, Go + Python) with the same graph-grounded false-positive-reduction post-pass that backs the `review` tool.

## Multi-repo management

| Tool | Description |
|------|-------------|
| `track_repository` | Add a repo at runtime — indexes immediately, persists to global config |
| `untrack_repository` | Remove a repo — evicts nodes/edges, persists to global config |
| `set_active_project` | Switch active project scope for all subsequent queries |
| `get_active_project` | Return current project name and its member repositories |
| `list_repos` | List every project/repo in the active workspace |
| `workspace_info` | Workspace identity — bind mode, root directory, marker contents, discovered member set |
| `query_project` | Search symbols in another project or repo without a `set_active_project` switch — read-only cross-project lookup |
| `save_scope` | Save a named, reusable set of repository prefixes — accepted by `search_symbols` / `smart_context` via `scope` |
| `list_scopes` | List every saved repository scope |
| `delete_scope` | Delete a saved repository scope by name |

## Worktree views and checkouts

A **view** is what one request reads through. The session CWD automatically selects its checkout: an ordinary linked worktree is discovered and represented as an overlay over the family's designated primary, without an explicit `track_repository` call. Explicit tracking is reserved for a user-requested dedicated logical graph. Any request may name a different view explicitly. See [multi-repo.md](multi-repo.md#checkout-families-and-worktree-views) for the storage and lifecycle model.

### The `view` selector

`view` is request context, not a tool parameter: the server reads it off the arguments and strips it before parameter reconciliation and before any handler runs, so every tool honours it and no tool schema declares it.

```jsonc
// Search the index as it stands on another branch.
{"name":"search_symbols","arguments":{"query":"Login","view":{"kind":"git_ref","value":"refs/heads/release-2","graph_id":"graph-1f0c…"}}}

// Read a file through one registered worktree.
{"name":"read_file","arguments":{"path":"internal/auth/login.go","view":{"kind":"worktree","checkout_id":"018f…"}}}
```

Graph and checkout ids are opaque; `list_checkouts` (CLI: `gortex repos families`) lists them per family alongside the repo prefix each graph serves.

| `kind` | Required field | Selects |
|---|---|---|
| `auto` (default when `view` is omitted) | — | the session's own view: its cwd's checkout when that checkout is served automatically, else the base corpus |
| `base` | `graph_id` | one persisted base graph by id |
| `worktree` | `checkout_id` | one registered checkout by id, including its working-tree edits |
| `git_ref` | `value` | the commit a **full** ref points at — under `refs/heads/`, `refs/tags/`, or `refs/remotes/` |
| `commit` | `value` | one commit by object id — a full lowercase hex oid, 40 (SHA-1) or 64 (SHA-256) characters |

`git_ref` and `commit` also take an optional `graph_id`. `kind`, `graph_id`, `checkout_id` and `value` are the only accepted fields, and each must be a string.

Rules worth knowing before a client builds selectors:

- **Full names only.** Short refs (`main`), `HEAD`, revision expressions (`main~1`, `a..b`, `x@{1}`) and abbreviated object ids are rejected — they resolve against ambient state, and a pinned view may not. Values are never trimmed: surrounding whitespace is a malformed value, not a typo.
- **Multi-repo disambiguation.** A `git_ref` / `commit` selector with no `graph_id` resolves only when the session reaches exactly one repository; reaching several fails with `invalid_view_selector` naming them, so name one with `graph_id`.
- **Scope still applies.** A `base` or `worktree` selector naming a graph or checkout outside the session's workspace is refused with `selector_out_of_scope`, and the scope check runs before the readiness check so a session cannot probe a sibling workspace's build state.
- **Substitution is always labelled.** `exact: false` means the requested view was not served. Every fallback is read-only. Set `require_exact: true` when substitution is unacceptable.
- **Exact worktree edits are supported through the coordinator-backed write path.** Fallback, inactive ref, and commit views remain read-only and return `view_read_only` for mutation.
- A view of a committed tree has no working copy, so its files are served out of the object store and a file location is reported as a `gortex-view://<view-fingerprint>/<repo-prefix>/<path>` identity instead of an on-disk path.

The view is resolved before the session's overlay is prepared, so pushed editor buffers layer on top of whatever answers.

### Freshness rider

Every view-aware response says which view answered, in the same `freshness` block file-drift provenance already uses. It always rides on the response envelope's `_meta`, and is additionally merged into the payload where the wire format has a home for it — into a JSON object's own `freshness`, or into a GCX header's meta channel — so a client that reads only the payload still sees it. Shapes with no structural home (TOON, the one-line text form, a diagram) carry it on the envelope alone. An empty field is omitted.

| Field | Meaning |
|---|---|
| `requested_view` | the selector the caller sent, rendered as `kind` plus its payload — `auto`, `base:<graph-id>`, `worktree:<checkout-id>`, `git_ref:<ref>` or `git_ref:<graph-id>:<ref>` |
| `actual_view` | what served the request — a selector string, or the view fingerprint when the server pinned one |
| `exact` | whether `actual_view` is the view that was requested |
| `fallback_reason` | why it is not; set exactly when `exact` is false |
| `view_fingerprint` | identity of the content that answered — the authority half of every `gortex-view://` URI in the same response |
| `requested_ref` / `resolved_ref` / `resolved_commit` / `resolved_tree` | the ref or object id the selector named, and what it resolved to when the request was served (`resolved_ref` is empty for a `commit` selector) |
| `build_token` | an in-progress build the caller can poll |
| `retry_after` | poll hint, in whole seconds |
| `degraded_capabilities` | capabilities this view does not serve completely that the request did not require — each with the state it was found in |
| `base_scoped` | capabilities a base-scoped engine answered while a routed view served the request |

### Capability arguments

A view is rarely complete all at once: source bytes are readable long before the syntax graph is resolved, and vector search may never be enabled at all. Three request-level arguments — read and stripped on the same seam as `view` — let a caller state what it needs instead of accepting a silently thin answer.

| Argument | Type | Effect |
|---|---|---|
| `require_complete` | bool (a `"true"` / `"false"` string is accepted) | promotes the calling operation's own default capabilities to required |
| `required_capabilities` | array of names, or one comma-separated string | adds to whatever `require_complete` produced; the request fails if the view cannot serve them |
| `optional_capabilities` | same shapes | annotates only — never fails a request |

The schemas also publish three consistency controls on every view-aware tool: `require_exact` rejects substitution, `require_fresh` waits until the selected worktree reflects current filesystem state, and `wait_deadline` is the absolute RFC3339 deadline bounding that wait. A deadline without a wait requirement is rejected rather than ignored.

Defaults that were not required are still evaluated and reported under `degraded_capabilities`. A request the base corpus serves is exempt: the base is a plain whole index with no producer rows to read.

The capability vocabulary is closed — an unknown name is refused with `invalid_view_selector` rather than silently requiring nothing:

| Group | Ids |
|---|---|
| source | `source.snapshot`, `source.config` |
| graph | `graph.syntax`, `graph.resolution.local`, `graph.resolution.cross_repo`, `graph.incoming_edges`, `graph.similarity` |
| search | `search.symbols`, `search.content`, `search.vector`, `search.text` |
| lsp | `lsp.references`, `lsp.diagnostics`, `lsp.hover`, `lsp.rename`, `lsp.code_actions` |

Each is in one of five states for a given view: `complete`, `incomplete`, `building`, `unavailable`, `disabled_by_config`. A required capability in a terminal state (`unavailable`, `disabled_by_config`, or undeclared by the view) refuses the request with `capability_unavailable` — waiting cannot clear it. One that is merely `building` or `incomplete` refuses with `required_capability_incomplete`, which a retry can clear.

### Typed errors

Every refusal on the view path carries a stable code as the first token of the message. The codes are a wire contract — a client may switch on them, and none is ever reworded.

| Code | Meaning |
|---|---|
| `invalid_view_selector` | malformed selector — unknown kind or field, a non-string value, a missing required field, a ref name git itself would reject, an abbreviated object id, or an unresolvable repository for a bare ref |
| `selector_conflict` | the selector carries a field its kind does not use |
| `selector_out_of_scope` | the selector names a repository or checkout outside the caller's workspace |
| `ref_not_commit` | the selector resolved to an object that is not a commit |
| `ref_not_available_locally` | the ref or object is well-formed but the local object store does not have it |
| `view_building` | the view exists but is still being built; the response carries `build_token` and `retry_after` (2 s for a ref view) |
| `view_read_only` | the request would mutate a view that only serves reads |
| `capability_unavailable` | a required capability cannot be served by this view at all |
| `required_capability_incomplete` | a required capability exists but is still building or only partly populated |
| `checkout_inaccessible` | the checkout backing the view cannot be read — not registered, not ready, unmounted, or permission denied |
| `no_primary` | the family has no primary base graph to compose a view over |
| `source_object_missing` | the source bytes a result points at are gone from the object store |

A ref view that is rebuilding while already serving an older generation answers with that generation rather than refusing, marked `exact: false` with the `view_building` reason and the build token to poll — so the answer is never mistaken for the requested tree.

### Tools

Checkout administration is one surface with two front doors; the CLI verbs under `gortex repos` call exactly these tools ([cli.md](cli.md#worktrees-and-checkouts)). Every destructive tool previews by default: a call without `confirm` reads the catalog, returns what would happen, and writes nothing.

| Tool | Description |
|------|-------------|
| `list_checkouts` | List the checkout families this daemon tracks — per family the primary corpus and epoch, its dedicated graphs, every registered working copy (mode, state, both reconciler clocks with their deadlines, path evidence, route, whether a build coordinator is live) and the views rooted in its graphs. `family` narrows by family id / graph id / repo prefix / a path inside a tracked repo. Reads the catalog only |
| `set_primary_checkout` | Make one corpus (`graph`: a graph id, repo prefix, or path) the base every automatic checkout of its family composes over. Previews the incumbent, the epoch, whether the move is accepted, and every checkout that must rebuild its layers; `confirm: true` runs it |
| `forget_checkout` | Remove one checkout (`path`: a path or repo prefix), its corpus and everything rooted in it. Unlike `untrack_repository` it never demotes the checkout into the family's automatic lane. Previews the closure; `confirm: true` runs it |
| `reconcile_checkouts` | Reconcile families against git and the filesystem now instead of waiting for the janitor — identities confirmed or allocated, the availability and removal clocks moved, build coordinators brought in line. `family` scopes it; omit for every family |
| `explain_view` | Explain which graph answers for one filesystem `path`: the checkout it binds to, how that checkout is served, its route and the generations behind it — or the step in the chain that could not be taken and left the base corpus to answer |

All five take `format` (`json` default, `gcx`, `toon`) and `max_bytes`.

## Live editor buffers (overlay sessions)

Editor extensions push in-flight (unsaved) buffers as **overlays**. Gortex composes a per-request **shadow view** on top of the immutable base graph and threads it through the tool dispatch context — every subsequent `tools/call` from the same MCP session reads through the shadow. Graph-walking tools (`find_usages`, `get_call_chain`, `analyze`, …) and source-reading tools (`get_symbol_source`, `get_editing_context`, …) all see the editor-buffer state without per-tool changes.

**Base is never mutated by overlay flow.** Concurrent sessions each see their own view; the file watcher's reindex passes don't race with overlay queries; cross-file edges from non-overlaid files into overlaid symbols are preserved.

| Tool | Description |
|------|-------------|
| `overlay_register` | Bind an overlay session to the current MCP session ID (idempotent) |
| `overlay_push` | Push (or update) a single file overlay; `base_sha` enables drift detection, `deleted: true` previews a delete |
| `overlay_list` | List every overlay attached to the session — path / size / deleted / base_sha |
| `overlay_delete` | Remove one overlay from the session |
| `overlay_drop` | Tear down the session and discard every overlay |
| `overlay_keepalive` | Refresh the session's idle timer without re-pushing buffer content; cheap option for debugger / wizard pauses |
| `compare_with_overlay` | Run `find_usages` / `get_callers` / `get_call_chain` / `get_dependencies` / `get_dependents` against base AND overlay; returns added / removed / common ID sets |

**Branching — N parallel speculative sessions off one baseline.** Each overlay session carries an active-branch pointer plus a branches map; every legacy overlay tool operates on the active branch, so callers that never touch branches see exactly one implicit `main` branch and behave unchanged. With branches, an agent can hold strategy A and strategy B simultaneously off the same baseline, evaluate each, and merge the winner.

| Tool | Description |
|------|-------------|
| `overlay_fork` | Clone the active (or named) branch into a new branch; optional `activate: true` flips the session pointer |
| `overlay_branches` | List every branch with active flag, file count, `base_sha` anchor count, parent, and `created_at` |
| `overlay_switch` | Flip the session's active branch |
| `overlay_merge` | Fold one branch into another (default target: `main`) or write the branch to disk through the same atomic-write + `base_sha` drift guard as `edit_file`. Same-path divergent content is refused without `force: true`; `force` resolves last-writer-wins |
| `overlay_drop_branch` | Delete a named branch — refuses to drop the active branch or the implicit `main` |
| `compare_branches` | Run `find_usages` / `get_callers` / `get_call_chain` / `get_dependencies` / `get_dependents` against two branches and report each side plus the delta |

HTTP transport mirrors the surface at `/v1/overlay/sessions/*`. The `/v1/tools/<name>` entry point resolves the caller's real session identity from `Mcp-Session-Id` (preferred) or `?session_id=` — this identity drives every per-session subsystem: tool-policy gating, token-stats accounting, notes/memory scoping, and so on. `X-Gortex-Overlay-Session`, when present, is a narrower, independent override that scopes *only* overlay state to a different cohort id than the caller's own session (e.g. a CI harness orchestrating several overlay scopes from one connection) — it never substitutes for the real session identity anywhere else. Overlays are bound to their cohort id; the synchronous drop-on-disconnect only fires for a cohort that matches its own MCP transport session (`ReleaseSession` drops by session id) — a cohort explicitly named via `X-Gortex-Overlay-Session` lives until the idle TTL regardless of the owning connection's lifetime. Idle TTL is a fail-safe (default 30 m, configurable via `GORTEX_OVERLAY_IDLE_TTL`); every tool call against a live overlay refreshes it.

## Speculative execution

Built on the same shadow-graph substrate, `preview_edit` and `simulate_chain` answer **"what would change if I applied this WorkspaceEdit?"** without ever touching disk or mutating the base graph. The input is a standard LSP `WorkspaceEdit` (`changes` / `documentChanges`), so any agent that already produces WorkspaceEdits for code actions can speculate on them directly. Per-step impact: touched files, added / removed / renamed symbols (non-trivial-signature rename heuristic), broken callers, broken interface implementors, blast-radius rollup, suggested test targets, and (when an LSP is configured) round-trip diagnostics restored to the on-disk state at simulation end.

| Tool | Description |
|------|-------------|
| `preview_edit` | Single-shot WorkspaceEdit → impact report. Optional `diagnostics: false` skips the LSP round-trip. `inherit_overlay: true` layers on top of the caller's current overlay |
| `simulate_chain` | Ordered sequence of WorkspaceEdits applied in order with per-step impact + cumulative rollup + per-step diagnostics delta. `stop_on_error: true` (default) aborts on the first new ERROR-severity diagnostic. `keep: true` promotes the final simulated state into a real overlay session bound to the caller |

## MCP resources (18)

Read-only, URI-addressable, no args. Clients that speak resources can `resources/subscribe` once and receive `notifications/resources/updated` after each graph re-warm — no polling.

| Resource | Description |
|----------|-------------|
| `gortex://session` | Current session state and activity |
| `gortex://stats` | Graph statistics (node/edge counts) |
| `gortex://schema` | Graph schema reference |
| `gortex://guide` | On-demand reference: LLM-provider matrix, capabilities, token-economy, analyze/search_ast catalogs, workflow |
| `gortex://guide/{topic}` | One guide section by topic (providers, capabilities, tokens, analyze, search_ast, resources, workflow) |
| `gortex://index-health` | Health score, parse failures, stale files |
| `gortex://workspace` | Workspace identity and discovered member set |
| `gortex://repos` | Tracked repo / project list |
| `gortex://active-project` | Active project name and member repos |
| `gortex://communities` | Community list with cohesion scores |
| `gortex://community/{id}` | Single community detail |
| `gortex://processes` | Execution flow list |
| `gortex://process/{id}` | Single process trace |
| `gortex://report` | High-level orientation — graph size, top languages/kinds, hotspot / dead-code / todo counts |
| `gortex://god-nodes` | Top 20 hotspots |
| `gortex://surprises` | Cycles + dead code + cross-community call hubs |
| `gortex://audit` | `audit_agent_config` with discovery defaults |
| `gortex://questions` | TODO / FIXME / XXX / HACK / QUESTION rollup grouped by tag and assignee |

## MCP prompts (3)

| Prompt | Description |
|--------|-------------|
| `pre_commit` | Review uncommitted changes — shows changed symbols, blast radius, risk level, affected tests |
| `orientation` | Orient in an unfamiliar codebase — graph stats, communities, execution flows, key symbols |
| `safe_to_change` | Analyze whether it's safe to change specific symbols — blast radius, edit plan, affected tests |
