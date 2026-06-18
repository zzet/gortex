// Instructions shared across every doc-aware adapter. Centralising the
// body here avoids per-adapter drift: Cursor's .cursor/rules file,
// Copilot's .github/copilot-instructions.md, Codex's AGENTS.md, and
// Claude Code's CLAUDE.md all read from the same constant, so when the
// "prefer Gortex over Read/Grep" story evolves we update it once and
// every agent sees the change on the next `gortex init`.
//
// The claudecode adapter extends this body with its own slash-commands
// appendix — that part is Claude-Code-specific and lives in
// claudecode/content.go, keyed off the same sentinel so idempotency
// checks line up across adapters.
package agents

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// InstructionsSentinel is the substring every doc-aware adapter checks
// for when deciding whether to append the instructions block. If it's
// already present (wherever it came from — a prior `gortex init`, a
// user-copied block, another adapter writing to a shared rules file
// like AGENTS.md) we skip to stay idempotent.
const InstructionsSentinel = "## MANDATORY: Use Gortex MCP tools"

// CommunitiesStartMarker / CommunitiesEndMarker fence the generated
// community-routing block that `gortex init` writes into per-repo
// instructions files. Fenced (not just start-only) because this block
// is regenerated on every `init` re-run as the codebase evolves, so
// we need to identify and overwrite it precisely without clobbering
// user edits around it.
const (
	CommunitiesStartMarker = "<!-- gortex:communities:start -->"
	CommunitiesEndMarker   = "<!-- gortex:communities:end -->"
)

// GlobalRulesStartMarker / GlobalRulesEndMarker fence the rule block
// that `gortex install` merges into ~/.claude/CLAUDE.md. The block is
// idempotent (re-running install replaces it in place) and removable
// (user can delete the marked region by hand without other side
// effects). Distinct from the communities markers above because this
// block lives at user level and survives every project init.
const (
	GlobalRulesStartMarker = "<!-- gortex:rules:start -->"
	GlobalRulesEndMarker   = "<!-- gortex:rules:end -->"
)

// GlobalInstructionsBody is the rule block written into the
// user-level ~/.claude/CLAUDE.md by `gortex install`. Mirrors
// InstructionsBody (the per-project rules) but trimmed to the
// always-applicable parts — multi-repo specifics, project-skill
// generation, and contracts hygiene are project-scoped and stay in
// per-repo CLAUDE.md.
const GlobalInstructionsBody = `## MANDATORY: Use Gortex MCP tools instead of Read/Grep/Glob

A Gortex daemon is configured machine-wide via the ` + "`gortex` MCP server" + `. Whenever you are operating on indexed source code (any repo registered with the daemon — check ` + "`gortex daemon status`" + `), you MUST prefer graph queries over file reads. PreToolUse hooks deny ` + "`Read`" + ` / ` + "`Grep`" + ` / ` + "`Glob`" + ` against indexed source — the deny message names the right tool.

### Optional: delegate research to a local agent

When ` + "`llm.provider`" + ` is configured (one of ` + "`local`" + ` / ` + "`anthropic`" + ` / ` + "`openai`" + ` / ` + "`azure`" + ` / ` + "`ollama`" + ` / ` + "`claudecli`" + ` / ` + "`codex`" + ` / ` + "`copilot`" + ` / ` + "`cursor`" + ` / ` + "`opencode`" + ` / ` + "`gemini`" + ` / ` + "`bedrock`" + ` / ` + "`deepseek`" + ` — pick one in ` + "`.gortex.yaml`" + ` or ` + "`~/.gortex/config.yaml`" + `, or via ` + "`GORTEX_LLM_PROVIDER`" + ` / ` + "`GORTEX_LLM_MODEL`" + `), the ` + "`ask`" + ` MCP tool is registered. It runs a grammar-constrained agent that uses gortex tools to research one question and returns a synthesized answer — useful when you'd otherwise issue many ` + "`search_symbols`" + ` / ` + "`get_callers`" + ` / ` + "`contracts`" + ` calls. Only the ` + "`local`" + ` provider requires a ` + "`-tags llama`" + ` build; the others are pure-Go HTTP / subprocess adapters available in every binary.

| When you'd otherwise...               | Consider...                              |
|---------------------------------------|------------------------------------------|
| Run many calls to answer one open-ended question | ` + "`ask`" + ` (one call, ~5-30s, ~200-400 token answer) |
| Trace a request across repos (consumer → contract → handler → downstream) | ` + "`ask`" + ` with ` + "`chain: true`" + ` |
| Look up a single known fact | Skip ` + "`ask`" + ` — direct tools are faster |

If ` + "`ask`" + ` isn't in ` + "`tools/list`" + `, no provider could construct (missing model / API key, ` + "`local`" + ` without ` + "`-tags llama`" + `, ` + "`claudecli`" + ` without ` + "`claude`" + ` on ` + "`$PATH`" + `, ` + "`bedrock`" + ` without AWS credentials). Fall through to direct tools.

### Search and Navigation

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| ` + "`Grep`" + ` / ` + "`grep`" + ` / ` + "`rg`" + ` for a symbol      | ` + "`search_symbols`" + ` (BM25 + camelCase-aware)|
| ` + "`Grep`" + ` for references                 | ` + "`find_usages`" + ` (zero false positives)     |
| ` + "`Grep`" + ` to find callers                | ` + "`get_callers`" + ` / ` + "`get_call_chain`" + `         |
| ` + "`Glob`" + ` over source files (` + "`**/*.go`" + `)  | ` + "`get_repo_outline`" + ` / ` + "`search_symbols`" + `    |
| Multiple ` + "`Read`" + ` calls to explore      | ` + "`smart_context`" + ` (one call)               |

### Reading Source

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| ` + "`Read`" + ` whole file for one function    | ` + "`get_symbol_source`" + ` (80% fewer tokens; add ` + "`compress_bodies: true`" + ` when you only need the surface signature) |
| ` + "`Read`" + ` to understand a file           | ` + "`get_file_summary`" + ` / ` + "`get_editing_context`" + ` (the latter emits ` + "`source_compressed`" + ` when ` + "`compress_bodies: true`" + `) |
| ` + "`Read`" + ` to check a signature           | ` + "`get_symbol`" + ` (signature in ` + "`meta.signature`" + `) |
| ` + "`Read`" + ` to trace calls                 | ` + "`get_call_chain`" + ` / ` + "`get_callers`" + `         |
| ` + "`Read`" + ` on a non-indexed / raw file    | ` + "`read_file`" + ` (atomic, honours editor-buffer overlays; ` + "`compress_bodies: true`" + ` elides function bodies for ~30-40% of original tokens) |

### Editing and Refactoring

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| ` + "`Edit`" + ` whole file by string match    | ` + "`edit_file`" + ` (Gortex MCP — no pre-Read required, atomic write, auto-reindex; pass ` + "`dry_run`" + ` to preview) |
| ` + "`Write`" + ` a new file or full rewrite   | ` + "`write_file`" + ` (no pre-Read required; creates parent dirs; pass ` + "`dry_run`" + ` to preview) |
| Read→Edit roundtrip for one symbol    | ` + "`edit_symbol`" + ` (edit by ID)               |
| Manual find-and-replace for renames   | ` + "`rename_symbol`" + ` (cross-file refs)        |
| Sequencing multi-file edits yourself  | ` + "`batch_edit`" + ` (dependency-ordered)        |

### Dataflow (CPG-lite)

The ` + "`flow_between`" + ` and ` + "`taint_paths`" + ` MCP tools answer **"where does this value flow?"** by walking the new dataflow edges (` + "`value_flow`" + ` intra-procedural; ` + "`arg_of`" + ` caller-arg→callee-param; ` + "`returns_to`" + ` callee→assignment).

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Tracing a value through helpers by hand | ` + "`flow_between(source_id, sink_id, max_depth=8)`" + ` — ranked dataflow paths between two symbols |
| Grepping for sources / sinks         | ` + "`taint_paths(source_pattern, sink_pattern)`" + ` — pattern-driven sweep. Patterns: bare token = name substring; ` + "`exact:Foo`" + `; ` + "`path:dir/`" + `; ` + "`kind:method`" + `. Sinks auto-expand functions to their params. |
| Asking "can A reach B?" over the call graph | ` + "`trace_path(source_id, sink_id)`" + ` — shortest A→B call path (bidirectional BFS); on no-path returns a why-unreachable diagnosis naming the dynamic-dispatch / external boundary where the chain breaks. CLI: ` + "`gortex trace <from> <to>`" + `. |

### Structural Code Search

` + "`search_ast`" + ` answers "find every code site whose AST matches this shape" — the missing primitive between ` + "`search_symbols`" + ` (name-based) and ` + "`find_usages`" + ` (target-required). Cross-language; every match enriched with the enclosing function's ` + "`symbol_id`" + `.

| Instead of...                            | You MUST use...                          |
|------------------------------------------|------------------------------------------|
| Grep for an anti-pattern across the repo | ` + "`search_ast`" + ` with a bundled ` + "`detector`" + ` (` + "`error-not-wrapped`" + `, ` + "`sql-string-concat`" + `, ` + "`weak-crypto`" + `, ` + "`panic-in-library`" + `, ` + "`goroutine-without-recover`" + `, ` + "`http-client-no-timeout`" + `, ` + "`hardcoded-secret`" + `, ` + "`empty-catch`" + `, ` + "`java-string-equality`" + `, ` + "`python-mutable-default-arg`" + `). |
| Grep for a code shape (e.g. ` + "`.Get(_, nil)`" + `) | ` + "`search_ast`" + ` with ` + "`pattern: \"...\"`" + ` (raw tree-sitter S-expression) + ` + "`language`" + `. Capture nodes with ` + "`@name`" + `, anchor with ` + "`@match`" + `, predicates ` + "`(#eq? @x \"…\")`" + ` / ` + "`(#match? @x \"…\")`" + `. |
| Scoping the audit to load-bearing code   | Pass ` + "`min_fan_in_of_enclosing_func: <N>`" + ` — drops matches in functions with fewer than N callers. |

### Clone Detection

` + "`find_clones`" + ` surfaces near-duplicate function/method clusters from the ` + "`similar_to`" + ` graph layer — a MinHash + LSH pass over normalised tokens that catches copy-paste and renamed-variable (Type-1/Type-2) clones.

| Instead of...                            | You MUST use...                          |
|------------------------------------------|------------------------------------------|
| Eyeballing the repo for copy-paste       | ` + "`find_clones`" + ` — near-duplicate clusters; filter with ` + "`min_similarity`" + ` / ` + "`path_prefix`" + ` / ` + "`repo`" + `. |
| Hunting safe-to-delete duplicates        | ` + "`find_clones`" + ` with ` + "`dead_only: true`" + ` — clusters containing a dead symbol: "dead duplicates of live code". |

### Code Quality and Analysis

The ` + "`analyze`" + ` MCP tool is a unified dispatcher. Pass ` + "`kind: \"<name>\"`" + ` for one of:

- Structural: ` + "`dead_code`" + `, ` + "`hotspots`" + `, ` + "`cycles`" + `, ` + "`would_create_cycle`" + `
- Comments / churn: ` + "`todos`" + `, ` + "`stale_code`" + `, ` + "`ownership`" + `
- Coverage / releases: ` + "`coverage`" + `, ` + "`coverage_gaps`" + `, ` + "`coverage_summary`" + `, ` + "`releases`" + `, ` + "`blame`" + `
- Schema: ` + "`orphan_tables`" + `, ` + "`unreferenced_tables`" + `
- Flags / interop: ` + "`stale_flags`" + `, ` + "`cgo_users`" + `, ` + "`wasm_users`" + `
- Edge-driven: ` + "`channel_ops`" + `, ` + "`goroutine_spawns`" + `, ` + "`field_writers`" + `, ` + "`annotation_users`" + `, ` + "`config_readers`" + `, ` + "`event_emitters`" + `, ` + "`error_surface`" + `, ` + "`external_calls`" + `
- Framework layer: ` + "`routes`" + ` (handler ↔ HTTP/gRPC/WS/GraphQL/topic), ` + "`models`" + ` (ORM class ↔ DB table), ` + "`components`" + ` (parent → child JSX)
- Infrastructure: ` + "`k8s_resources`" + ` (KindResource fan-out by kind/namespace), ` + "`images`" + ` (KindImage with consumer count), ` + "`kustomize`" + ` (KindKustomization overlay tree)
- Data transformation: ` + "`dbt_models`" + ` (dbt / SQLMesh models, seeds, snapshots, sources with column counts + lineage fan-in/out)
- Multi-repo: ` + "`cross_repo`" + ` (repo-boundary-crossing calls / implements / extends grouped by source → target repo)

The ` + "`gortex enrich blame|coverage|releases|all`" + ` CLI hydrates the graph with the metadata that the ` + "`stale_*`" + `, ` + "`coverage*`" + `, ` + "`ownership`" + `, and ` + "`releases`" + ` analyzers need.

### PR / Change Review

Review a diff through the graph instead of hand-walking each gate. ` + "`analyze`" + ` with ` + "`kind: \"review\"`" + ` runs the idiomatic / correctness rulepack (NPE, thread-safety check-then-act, N+1, logic-error; Go + Python) with a graph-grounded false-positive-reduction pass. The ` + "`review`" + ` / ` + "`review_pack`" + ` MCP tools (or ` + "`gortex review [--diff|--base <ref>] [--audience agent|human]`" + `) return one fused packet — verdict + ` + "`file:line`" + ` findings + cost. For a queue of open PRs, ` + "`list_prs`" + ` / ` + "`triage_prs`" + ` / ` + "`pr_risk`" + ` / ` + "`get_pr_impact`" + ` / ` + "`suggest_reviewers`" + ` (or ` + "`gortex prs --triage`" + `) rank by graph-derived blast radius and route reviewers.

### Token Economy

For list-shaped responses (` + "`search_symbols`" + `, ` + "`find_usages`" + `, ` + "`analyze`" + `, ` + "`batch_symbols`" + `, ` + "`get_callers`" + `, ` + "`get_call_chain`" + `, ` + "`get_dependencies`" + `, ` + "`get_dependents`" + `, ` + "`find_implementations`" + `, ` + "`get_file_summary`" + `, ` + "`get_editing_context`" + `, ` + "`smart_context`" + `, ` + "`contracts`" + `), pick a wire format. Order of preference: **gcx > toon > json**.

- ` + "`format: \"gcx\"`" + ` — GCX1 compact wire format. Round-trippable, ~27% fewer tokens. Decode with ` + "`@gortex/wire`" + ` (npm) or ` + "`github.com/gortexhq/gcx-go`" + ` (Go). **Default for known clients (claude-code, cursor, vscode, zed, aider, kilocode, opencode, openclaw, codex, omp-coding-agent)** when the request omits ` + "`format`" + `.
- ` + "`format: \"toon\"`" + ` — TOON tabular text. Lossy but compact; useful for clients without a GCX decoder.
- ` + "`format: \"json\"`" + ` — verbose legacy default. Falls back automatically for unknown clients.

Explicit ` + "`format`" + ` arg always overrides the session default in either direction.

### Token Economy (content compression)

` + "`compress_bodies: true`" + ` is an orthogonal axis: GCX1 shrinks the response *shape*; ` + "`compress_bodies`" + ` shrinks the response *content*. **Compose them** for stacked savings.

The flag replaces every function/method body in the returned source with a ` + "`{ /* N lines elided */ }`" + ` stub (Python: ` + "`...  # N lines elided`" + `, Ruby: ` + "`# N lines elided`" + `, Elixir: ` + "`do\\n  # N lines elided\\nend`" + `). Signatures, doc-comments, imports, top-level constants/types, and structure stay intact. A 200-line file lands at ≤ 60 lines (~30-40% of original tokens). Wired in 16 languages: go, typescript, tsx, javascript, python, rust, java, c, cpp, csharp, kotlin, scala, php, ruby, bash, elixir.

| Instead of...                                          | You MUST use...                          |
|--------------------------------------------------------|------------------------------------------|
| Reading a whole 2k-line file to learn the surface      | ` + "`read_file`" + ` with ` + "`compress_bodies: true`" + ` |
| Pulling a class's full source to see its method shapes | ` + "`get_symbol_source`" + ` with ` + "`compress_bodies: true`" + ` |
| Calling ` + "`get_editing_context`" + ` then fetching every neighbour's source for signatures | ` + "`get_editing_context`" + ` with ` + "`compress_bodies: true`" + ` — emits ` + "`source_compressed`" + ` alongside the structural sections |

When the language has no grammar binding or tree-sitter can't parse the input, the flag is a no-op — raw source comes back and the response's ` + "`bodies_elided`" + ` flag stays absent. Safe to set unconditionally.

### Pagination, sparse fieldsets, and graceful degradation

Every list-shaped tool runs through a per-response budget by default — the agent harness's spill-to-disk fallback is a true edge case, not the routine outcome on real-world payloads. When a response would exceed the budget, the server runs a priority-aware cascade:

1. **Strip verbose meta** (` + "`doc`" + `, raw ` + "`meta`" + ` blobs) — cheapest cut, never drops rows.
2. **Drop tier-3 rows** — params, closures, generic params, ` + "`param_of`" + ` / ` + "`typed_as`" + ` / ` + "`value_flow`" + ` edges, low-confidence (` + "`text_matched`" + `) edges. High-noise rows agents almost never need on the first response.
3. **Drop tier-2 rows** — fields, constants, variables, references, instantiates, etc.
4. **Last-resort tail-trim** of the longest remaining tier-1 list.

Each escape adds metadata: ` + "`_meta_stripped`" + `, ` + "`_dropped_tier_<N>_<list>`" + `, ` + "`_truncated_by_budget`" + `, ` + "`_max_returned_<list>`" + `, ` + "`_original_count_<list>`" + `. Use them to decide whether to narrow the filter, raise ` + "`max_bytes`" + `, or paginate.

Knobs you can pull when you need something different:

- **Pagination** — ` + "`search_symbols`" + `, ` + "`winnow_symbols`" + `, ` + "`prefetch_context`" + `, and ` + "`contracts`" + ` (action=list) accept ` + "`cursor`" + ` (opaque token from a previous ` + "`next_cursor`" + `). Don't parse the cursor; round-trip what the server gave you.
- **Explicit budget** — pass ` + "`max_bytes: <N>`" + ` to override the project default. Pass ` + "`max_bytes: 0`" + ` to opt OUT of budgeting entirely — full result inline, transport spills if oversized. Use the opt-out only when you genuinely need every row (security audits, exhaustive enumeration).
- **Sparse fieldsets** — pass ` + "`fields: \"id,line\"`" + ` (comma-separated) to drop columns at the row level. Pure size win, no priority drops.
- **Limit defaults** — most tools default to 20–50 rows; raise ` + "`limit`" + ` only when a single page is too small. Pagination is preferred over a giant ` + "`limit`" + `.

### MCP Resources

Bootstrap-state tools (` + "`graph_stats`" + ` / ` + "`index_health`" + ` / ` + "`workspace_info`" + ` / ` + "`list_repos`" + ` / ` + "`get_active_project`" + `) are also exposed as MCP resources at ` + "`gortex://stats`" + ` / ` + "`gortex://index-health`" + ` / ` + "`gortex://workspace`" + ` / ` + "`gortex://repos`" + ` / ` + "`gortex://active-project`" + `. Subscribe via ` + "`resources/subscribe`" + ` to receive ` + "`notifications/resources/updated`" + ` after each graph re-warm — no polling. Tools stay registered for clients that don't speak resources.

Analyzer rollups (read-only summaries of the current indexed state): ` + "`gortex://report`" + ` (orientation), ` + "`gortex://god-nodes`" + ` (top hotspots), ` + "`gortex://surprises`" + ` (cycles + dead code + hubs), ` + "`gortex://audit`" + ` (CLAUDE.md drift), ` + "`gortex://questions`" + ` (TODOs).

### Session Memory (save_note / query_notes / distill_session)

Gortex remembers code; this triplet remembers **why you made a call**. Notes persist per-repo across daemon restarts and context compactions, scoped to the session's workspace, auto-linked to symbols mentioned in the body.

| Trigger                                                  | You MUST call                                                                 |
|----------------------------------------------------------|-------------------------------------------------------------------------------|
| Session start in a touched repo (after a compaction or on a fresh run) | ` + "`distill_session`" + ` — top symbols, pinned notes, decisions, recent excerpts. Seed your mental model before reading any file. |
| Making a decision, rejecting an alternative, hitting a non-obvious constraint, committing to an invariant | ` + "`save_note tags:\"decision\" body:\"<what+why>\"`" + ` — mention symbol IDs in the body for auto-linking; pin (` + "`pinned:true`" + `) anything load-bearing. |
| Before editing a symbol you've touched before            | ` + "`query_notes symbol_id:\"<id>\"`" + ` — prior decisions and warnings ride on each symbol. |

**Save:** decisions, non-obvious constraints, follow-ups, bug reproductions, surprising graph findings, partial-progress hand-offs. **Skip:** play-by-play (the diff says it), patterns derivable from the graph, anything already in CLAUDE.md. Canonical tags: ` + "`decision`" + `, ` + "`bug`" + `, ` + "`follow-up`" + `, ` + "`gotcha`" + `, ` + "`invariant`" + ` — ` + "`decision`" + ` gets its own section in ` + "`distill_session`" + `.

### Development Memories (store_memory / query_memories / surface_memories)

` + "`save_note`" + ` is a **per-session scratchpad**; ` + "`store_memory`" + ` is the **workspace-wide durable knowledge base**. Memories outlive sessions, agents, and teammates — every future agent in the workspace inherits them.

| Trigger                                                  | You MUST call                                                                 |
|----------------------------------------------------------|-------------------------------------------------------------------------------|
| Immediately after ` + "`smart_context`" + ` (every new task)            | ` + "`surface_memories task:\"<task>\" symbol_ids:\"<top hits>\"`" + ` — ranked memories anchored to your working set. Each hit carries ` + "`match_reasons`" + ` so you know *why* it surfaced. |
| You discover a durable invariant / gotcha / decision worth teaching the team | ` + "`store_memory kind:\"<invariant|gotcha|convention|decision|constraint|incident>\" body:\"<what+why>\" symbol_ids:\"<id>\" importance:5`" + ` — pin load-bearing memories. |
| You discover a memory is no longer true                  | ` + "`store_memory body:\"<corrected>\" supersedes:\"<old-id>\"`" + ` — preserves audit trail; the old memory is hidden from ` + "`surface_memories`" + ` by default. |

**Store:** invariants (violating them breaks the system), conventions (this package never X), incident learnings, API contracts not enforced by types, debugging traps, cross-cutting decisions. **Skip:** anything derivable from code, session-local play-by-play (use ` + "`save_note`" + ` instead), CLAUDE.md content. Canonical kinds: ` + "`invariant`" + `, ` + "`constraint`" + `, ` + "`convention`" + `, ` + "`gotcha`" + `, ` + "`decision`" + `, ` + "`incident`" + `, ` + "`reference`" + `.

### Session Start

The SessionStart hook injects daemon status (tracked repos, cwd coverage, ready/warmup state). If you see "daemon is not running" — run ` + "`gortex daemon start --detach`" + ` and re-run the task. If you see "cwd is not covered by any tracked repo" — graph tools won't be available for that directory.

Once the daemon is up, **call** ` + "`distill_session`" + ` next — surfaces decisions / pinned notes / recent excerpts saved in prior sessions in this workspace so a context compaction or a fresh process doesn't erase what was already learned.
`

// InstructionsBody is the shared rule block every adapter writes to
// its agent's instructions file. Tool names in the tables (Read, Grep)
// are Claude-Code-specific flavour; models outside Claude Code read
// them as "any file-reading tool" — the principle stays the same so
// we keep one body rather than branch by agent.
const InstructionsBody = `## MANDATORY: Use Gortex MCP tools instead of Read/Grep

Gortex is running as an MCP server. You MUST use graph queries instead of file reads whenever possible. This saves thousands of tokens per task.

### Optional: delegate research to a local agent

When ` + "`llm.provider`" + ` is configured (one of ` + "`local`" + ` / ` + "`anthropic`" + ` / ` + "`openai`" + ` / ` + "`azure`" + ` / ` + "`ollama`" + ` / ` + "`claudecli`" + ` / ` + "`codex`" + ` / ` + "`copilot`" + ` / ` + "`cursor`" + ` / ` + "`opencode`" + ` / ` + "`gemini`" + ` / ` + "`bedrock`" + ` / ` + "`deepseek`" + ` — pick one in ` + "`.gortex.yaml`" + ` or ` + "`~/.gortex/config.yaml`" + `, or via ` + "`GORTEX_LLM_PROVIDER`" + ` / ` + "`GORTEX_LLM_MODEL`" + `), the ` + "`ask`" + ` MCP tool is registered. It runs a grammar-constrained agent that uses gortex tools to research one question and returns a synthesized answer — useful when you'd otherwise issue many ` + "`search_symbols`" + ` / ` + "`get_callers`" + ` / ` + "`contracts`" + ` calls. Only the ` + "`local`" + ` provider requires a ` + "`-tags llama`" + ` build; the others are pure-Go HTTP / subprocess adapters available in every binary.

| When you'd otherwise...               | Consider...                              |
|---------------------------------------|------------------------------------------|
| Run many calls to answer one open-ended question | ` + "`ask`" + ` (one call, ~5-30s, ~200-400 token answer) |
| Trace a request across repos (consumer → contract → handler → downstream) | ` + "`ask`" + ` with ` + "`chain: true`" + ` |
| Look up a single known fact | Skip ` + "`ask`" + ` — direct tools are faster |

If ` + "`ask`" + ` isn't in ` + "`tools/list`" + `, no provider could construct (missing model / API key, ` + "`local`" + ` without ` + "`-tags llama`" + `, ` + "`claudecli`" + ` without ` + "`claude`" + ` on ` + "`$PATH`" + `, ` + "`bedrock`" + ` without AWS credentials). Fall through to direct tools.

### Navigation and Reading

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| ` + "`Read`" + ` a whole file for one function  | ` + "`get_symbol_source`" + ` with ` + "`id: \"path/to/file.go::SymbolName\"`" + ` (80% fewer tokens; add ` + "`compress_bodies: true`" + ` to drop the implementation body and keep only the signature) — use ` + "`get_file_summary`" + ` first if you don't know the symbol name |
| ` + "`Read`" + ` to find a function             | ` + "`get_symbol`" + ` or ` + "`get_editing_context`" + `    |
| Multiple ` + "`get_symbol`" + ` calls           | ` + "`batch_symbols`" + ` (one call for N symbols) |
| ` + "`Grep`" + ` for references                 | ` + "`find_usages`" + ` (zero false positives)     |
| ` + "`Grep`" + ` to find a symbol by name       | ` + "`search_symbols`" + ` (BM25 + camelCase-aware)|
| Filtering ` + "`search_symbols`" + ` by hand    | ` + "`winnow_symbols`" + ` — structured constraint chain (kind, language, community, path_prefix, min_fan_in, min_churn) with per-axis score contributions |
| ` + "`Read`" + ` to understand a file           | ` + "`get_file_summary`" + ` or ` + "`get_editing_context`" + ` (pass ` + "`compress_bodies: true`" + ` to ` + "`get_editing_context`" + ` to also receive a ` + "`source_compressed`" + ` view of the file) |
| ` + "`Read`" + ` on a non-indexed / raw file    | ` + "`read_file`" + ` (honours editor-buffer overlays, etag-aware; ` + "`compress_bodies: true`" + ` elides function bodies for ~30-40% of original tokens) |
| ` + "`Read`" + ` multiple files to trace calls  | ` + "`get_call_chain`" + ` / ` + "`get_callers`" + `         |
| Walking up/down an inheritance chain  | ` + "`get_class_hierarchy`" + ` — multi-hop EdgeExtends + EdgeImplements + EdgeComposes (type nodes) and EdgeOverrides (method nodes); ` + "`direction`" + ` ∈ up/down/both, ` + "`include_methods`" + ` pulls members + their override chain |
| Guessing an import path               | ` + "`find_import_path`" + `                       |
| ` + "`Read`" + ` to check a function signature  | ` + "`get_symbol`" + ` (signature is in ` + "`meta.signature`" + `) |
| 5-10 calls to explore for a task      | ` + "`smart_context`" + ` (one call)               |

### Token Economy (wire format)

Order of preference: **gcx > toon > json**. For known clients (claude-code, cursor, vscode, zed, aider, kilocode, opencode, openclaw, codex, omp-coding-agent) Gortex serves ` + "`gcx`" + ` automatically when a request omits the ` + "`format`" + ` arg — explicit ` + "`format`" + ` always wins.

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Default JSON on multi-row responses   | Rely on the per-session default (gcx) for known clients, or pass ` + "`format: \"gcx\"`" + ` explicitly on ` + "`search_symbols`" + `, ` + "`find_usages`" + `, ` + "`analyze`" + `, ` + "`contracts`" + `, ` + "`batch_symbols`" + `, ` + "`get_callers`" + ` / ` + "`get_call_chain`" + ` / ` + "`get_dependencies`" + ` / ` + "`get_dependents`" + ` / ` + "`find_implementations`" + ` / ` + "`find_overrides`" + ` / ` + "`get_class_hierarchy`" + `, ` + "`get_file_summary`" + `, ` + "`get_editing_context`" + `, ` + "`smart_context`" + ` |
| GCX-blind tooling needing tabular text| Pass ` + "`format: \"toon\"`" + ` — TOON is the second-tier fallback (lossy but ~10–15% smaller than JSON) |
| Parsing compact text output           | Use ` + "`@gortex/wire`" + ` (npm) or the Go ` + "`github.com/gortexhq/gcx-go`" + ` package (MIT) — both decode GCX back to structured rows |
| Reading ` + "`compact: true`" + ` output        | Prefer ` + "`format: \"gcx\"`" + ` — lossy text is being phased out; GCX is round-trippable and tokenizer-optimised |

### Token Economy (content compression)

` + "`compress_bodies: true`" + ` is orthogonal to GCX1 — GCX1 compresses the response *shape*, ` + "`compress_bodies`" + ` compresses the response *content*. **Compose them** for stacked savings.

When set on ` + "`get_symbol_source`" + ` / ` + "`get_editing_context`" + ` / ` + "`read_file`" + `, every function/method body in the returned source is replaced by a ` + "`{ /* N lines elided */ }`" + ` stub (Python: ` + "`...  # N lines elided`" + `; Ruby: ` + "`# N lines elided`" + `; Elixir: ` + "`do\\n  # N lines elided\\nend`" + `). Signatures, doc-comments, imports, top-level constants/types, and structure stay intact. A 200-line file lands at ≤ 60 lines (~30-40% of original tokens). Wired in 16 languages: go, typescript, tsx, javascript, python, rust, java, c, cpp, csharp, kotlin, scala, php, ruby, bash, elixir.

| Instead of...                                          | You MUST use...                          |
|--------------------------------------------------------|------------------------------------------|
| Reading a whole 2k-line file just to learn its surface | ` + "`read_file`" + ` with ` + "`compress_bodies: true`" + ` |
| Pulling a class's full source for method signatures    | ` + "`get_symbol_source`" + ` with ` + "`compress_bodies: true`" + ` |
| Calling ` + "`get_editing_context`" + ` then fetching every neighbour's source for signatures | ` + "`get_editing_context`" + ` with ` + "`compress_bodies: true`" + ` — emits ` + "`source_compressed`" + ` alongside the structural sections |

When the language has no grammar binding or tree-sitter can't parse the input, the flag is a no-op — raw source comes back and the response's ` + "`bodies_elided`" + ` flag stays absent. Safe to set unconditionally.

### Pagination, sparse fieldsets, and opt-in budget

Tools default to "return what you have" — full result inline, MCP transport spills to a side file if your harness cap fires. Three opt-in knobs let you trade rows for staying inline:

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Walking thousands of results in one call | Pass ` + "`cursor`" + ` (opaque token from a previous ` + "`next_cursor`" + `) on ` + "`search_symbols`" + ` / ` + "`winnow_symbols`" + ` / ` + "`prefetch_context`" + ` / ` + "`contracts`" + `; set ` + "`paginate: true`" + ` to also cap each page at the project default budget |
| Returning every column on every row   | Pass ` + "`fields: \"id,line\"`" + ` (comma-separated) to drop verbose ` + "`meta`" + ` / ` + "`doc`" + ` / signature columns. Pure size win, no truncation |
| Asking for a giant ` + "`limit`" + `              | Use the default page size and paginate; or pass ` + "`max_bytes: <N>`" + ` to cap the response (longest list is trimmed; truncation metadata rides on the response) |

### Impact Analysis and Safety

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Reading files to assess change scope  | ` + "`explain_change_impact`" + ` (includes cross-community warnings) |
| Guessing which tests to run           | ` + "`get_test_targets`" + `                       |
| Manual dependency ordering            | ` + "`get_edit_plan`" + `                          |
| Hoping signature changes are safe     | ` + "`verify_change`" + ` — checks callers and interface implementors |
| Manually checking team conventions    | ` + "`check_guards`" + ` — evaluates guard rules from .gortex.yaml |
| Wondering if a new dep creates a cycle| ` + "`analyze`" + ` with ` + "`kind: \"would_create_cycle\"`" + ` — checks before you add it |

### Diagnostics and Code Actions

Wired to every running language server (gopls / tsserver / pyright / rust-analyzer / clangd / jdtls / kotlin-language-server / omnisharp / ruby-lsp / phpactor / lua-language-server / sourcekit-lsp / haskell-language-server / elixir-ls / ocamllsp / zls / terraform-ls / yaml-language-server / json-language-server / bash-language-server). One unified surface across all of them:

| Instead of...                            | You MUST use...                          |
|------------------------------------------|------------------------------------------|
| Polling for diagnostics after every edit | ` + "`subscribe_diagnostics`" + ` — opt the session into push ` + "`notifications/diagnostics`" + `. Initial state replays immediately as ` + "`initial_replay: true`" + `; thereafter only delta-changed files are pushed (hash-suppressed). Filter with ` + "`min_severity`" + ` (1=error, 2=warning, 3=info, 4=hint) or ` + "`path_prefix`" + ` (absolute prefix). |
| Manual diagnostics fetch                 | ` + "`get_diagnostics`" + ` — latest stored ` + "`publishDiagnostics`" + ` for a file. Pass ` + "`wait: true`" + ` + ` + "`timeout_ms`" + ` to block on the first publish (e.g. right after a fresh ` + "`didOpen`" + `). |
| Forgetting to opt back out               | ` + "`unsubscribe_diagnostics`" + ` — idempotent, also fires automatically on session disconnect. |
| Hand-applying compiler suggestions       | ` + "`get_code_actions`" + ` for a file (and optional range), then ` + "`apply_code_action`" + ` on the chosen one. Atomic temp+rename, both legacy ` + "`changes`" + ` and modern ` + "`documentChanges`" + `, UTF-16 column math. |
| Walking the whole file to apply every fix | ` + "`fix_all_in_file`" + ` — runs ` + "`source.fixAll`" + ` over the whole file in one round-trip. |

### Structural Code Search

` + "`search_ast`" + ` answers "find every code site whose AST matches this shape" — the missing primitive between ` + "`search_symbols`" + ` (name-based) and ` + "`find_usages`" + ` (target-required). Cross-language, graph-aware, every match enriched with the enclosing function's ` + "`symbol_id`" + `.

| Instead of...                            | You MUST use...                          |
|------------------------------------------|------------------------------------------|
| Grep for an anti-pattern across the repo | ` + "`search_ast`" + ` with a bundled ` + "`detector`" + `: ` + "`error-not-wrapped`" + ` / ` + "`sql-string-concat`" + ` / ` + "`weak-crypto`" + ` / ` + "`panic-in-library`" + ` / ` + "`goroutine-without-recover`" + ` / ` + "`http-client-no-timeout`" + ` / ` + "`hardcoded-secret`" + ` / ` + "`empty-catch`" + ` / ` + "`java-string-equality`" + ` / ` + "`python-mutable-default-arg`" + `. |
| Grep for a code shape (e.g. every ` + "`.Get(_, nil)`" + ` call) | ` + "`search_ast`" + ` with ` + "`pattern: \"...\"`" + ` (raw tree-sitter S-expression) + ` + "`language: \"...\"`" + `. Capture nodes with ` + "`@name`" + `, anchor the match span with ` + "`@match`" + `, predicates: ` + "`(#eq? @x \"literal\")`" + ` / ` + "`(#match? @x \"regex\")`" + `. |
| Scoping the audit to load-bearing code   | Pass ` + "`min_fan_in_of_enclosing_func: <N>`" + ` — drops matches in functions with fewer than N callers. Combine with ` + "`path_prefix`" + ` / ` + "`repo`" + ` / ` + "`project`" + ` / ` + "`ref`" + ` to narrow further. |

### Code Quality and Analysis

The ` + "`analyze`" + ` MCP tool is a unified dispatcher. Supported ` + "`kind`" + ` values:

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Manually hunting unused code          | ` + "`analyze`" + ` with ` + "`kind: \"dead_code\"`" + ` — zero incoming edges (excludes entry points, tests, exports) |
| Guessing which symbols are over-coupled| ` + "`analyze`" + ` with ` + "`kind: \"hotspots\"`" + ` — ranks by fan-in, fan-out, community crossings |
| Manually scanning for circular deps   | ` + "`analyze`" + ` with ` + "`kind: \"cycles\"`" + ` — Tarjan's SCC with severity classification |
| Wondering if a new dep creates a cycle| ` + "`analyze`" + ` with ` + "`kind: \"would_create_cycle\"`" + ` — checks before you add it |
| Grepping for TODO / FIXME             | ` + "`analyze`" + ` with ` + "`kind: \"todos\"`" + ` — KindTodo nodes, filter by tag/assignee/ticket |
| Walking blame by hand                 | ` + "`analyze`" + ` with ` + "`kind: \"blame\"`" + ` — stamps meta.last_authored from git blame |
| Reading a cover.out profile manually  | ` + "`analyze`" + ` with ` + "`kind: \"coverage\"`" + ` — stamps meta.coverage_pct on executable symbols |
| Hunting symbols nobody touches        | ` + "`analyze`" + ` with ` + "`kind: \"stale_code\"`" + ` — symbols older than ` + "`older_than`" + ` days |
| Asking who owns a package             | ` + "`analyze`" + ` with ` + "`kind: \"ownership\"`" + ` — author rollup with symbol/file counts |
| Finding undertested symbols           | ` + "`analyze`" + ` with ` + "`kind: \"coverage_gaps\"`" + ` — symbols inside [min_pct, max_pct) |
| Per-package coverage rollup           | ` + "`analyze`" + ` with ` + "`kind: \"coverage_summary\"`" + ` — directory-level avg/covered/partial/uncovered |
| Stale feature flags                   | ` + "`analyze`" + ` with ` + "`kind: \"stale_flags\"`" + ` — flag callers untouched for ` + "`older_than`" + ` days |
| Walking git tags by hand              | ` + "`analyze`" + ` with ` + "`kind: \"releases\"`" + ` — stamps meta.added_in on file nodes |
| Surveying cgo / wasm boundaries       | ` + "`analyze`" + ` with ` + "`kind: \"cgo_users\"`" + ` or ` + "`kind: \"wasm_users\"`" + ` — files crossing the FFI boundary |
| Finding tables without migrations     | ` + "`analyze`" + ` with ` + "`kind: \"orphan_tables\"`" + ` — queried tables missing EdgeProvides |
| Finding migrations without users      | ` + "`analyze`" + ` with ` + "`kind: \"unreferenced_tables\"`" + ` — provided tables with zero EdgeQueries |
| Spotting channel send/recv mismatches | ` + "`analyze`" + ` with ` + "`kind: \"channel_ops\"`" + ` — channels grouped by sends/recvs |
| Finding goroutine spawn hotspots      | ` + "`analyze`" + ` with ` + "`kind: \"goroutine_spawns\"`" + ` — EdgeSpawns grouped by target + mode |
| Finding mutability hotspots           | ` + "`analyze`" + ` with ` + "`kind: \"field_writers\"`" + ` — fields ranked by EdgeWrites; pass ` + "`id`" + ` for one field |
| Listing every @Deprecated use         | ` + "`analyze`" + ` with ` + "`kind: \"annotation_users\"`" + ` — pass ` + "`id`" + ` or ` + "`name`" + ` for one annotation |
| Tracing config-key readers            | ` + "`analyze`" + ` with ` + "`kind: \"config_readers\"`" + ` — config_key nodes grouped by EdgeReadsConfig |
| Tracing event/log emitters            | ` + "`analyze`" + ` with ` + "`kind: \"event_emitters\"`" + ` — events grouped by EdgeEmits, ` + "`level`" + ` filter optional |
| Mapping event pub/sub topics          | ` + "`analyze`" + ` with ` + "`kind: \"pubsub\"`" + ` — pub/sub topics with publishers (EdgeEmits) + subscribers (EdgeListensOn) from NATS / Kafka / RabbitMQ / Redis / EventEmitter / Socket.IO; ` + "`transport`" + ` / ` + "`name`" + ` / ` + "`role`" + ` filters |
| Mapping the error surface             | ` + "`analyze`" + ` with ` + "`kind: \"error_surface\"`" + ` — function/method nodes with their EdgeThrows targets |
| Surveying stdlib / module-cache reach | ` + "`analyze`" + ` with ` + "`kind: \"external_calls\"`" + ` — KindModule nodes grouped by call/symbol counts; pass ` + "`id`" + ` for per-symbol detail, ` + "`module_kind`" + ` for stdlib/module_cache filter |
| Listing every HTTP/gRPC/WS route      | ` + "`analyze`" + ` with ` + "`kind: \"routes\"`" + ` — handler→route pairs from the EdgeHandlesRoute graph layer; ` + "`method`" + `, ` + "`path`" + `, ` + "`type`" + ` filters (` + "`type`" + ` ∈ http/grpc/ws/graphql/topic) |
| Mapping ORM models to tables          | ` + "`analyze`" + ` with ` + "`kind: \"models\"`" + ` — class→table edges from EdgeModelsTable across gorm / SQLAlchemy / Django / ActiveRecord / JPA / TypeORM / Ecto; ` + "`orm`" + `, ` + "`table`" + `, ` + "`model`" + ` filters |
| Walking the component tree            | ` + "`analyze`" + ` with ` + "`kind: \"components\"`" + ` — parent↔child fan-in/out from EdgeRendersChild (JSX/TSX + Phoenix HEEx); pass ` + "`id`" + ` for per-component child list |
| Surveying K8s manifests in the repo   | ` + "`analyze`" + ` with ` + "`kind: \"k8s_resources\"`" + ` — every KindResource with infra-edge fan-out (depends_on / configures / mounts / exposes / uses_env); ` + "`k8s_kind`" + `, ` + "`namespace`" + `, ` + "`name`" + ` filters |
| Listing container images in use       | ` + "`analyze`" + ` with ` + "`kind: \"images\"`" + ` — every KindImage (Dockerfile FROM target or K8s container.image) with consumer count; ` + "`role`" + ` (base/stage), ` + "`ref`" + `, ` + "`tag`" + ` filters |
| Mapping the Kustomize overlay tree    | ` + "`analyze`" + ` with ` + "`kind: \"kustomize\"`" + ` — every KindKustomization with base / resource fan-out; ` + "`dir`" + ` filter |
| Auditing what crosses repo boundaries | ` + "`analyze`" + ` with ` + "`kind: \"cross_repo\"`" + ` — calls / implements / extends edges whose endpoints live in different repos, grouped by (source repo → target repo, relation); ` + "`repo`" + `, ` + "`base_kind`" + `, ` + "`path_prefix`" + ` filters |
| Surveying dbt / SQLMesh models        | ` + "`analyze`" + ` with ` + "`kind: \"dbt_models\"`" + ` — dbt / SQLMesh models, seeds, snapshots, sources with column count + EdgeDependsOn lineage fan-in/out; ` + "`framework`" + `, ` + "`type`" + `, ` + "`materialized`" + `, ` + "`name`" + ` filters |
| Checking if the index is stale        | ` + "`index_health`" + ` — health score, parse failures, stale files |
| Wondering what changed this session   | ` + "`get_symbol_history`" + ` — modification counts, flags churning (3+ edits) |
| Hydrating blame / coverage / releases | ` + "`gortex enrich blame|coverage|releases|all`" + ` (CLI) — bulk-stamps the graph for the ` + "`stale_*`" + `, ` + "`coverage_*`" + `, ` + "`ownership`" + `, and ` + "`releases`" + ` analyzers |

### Code Generation and Editing

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Reading files to learn a pattern      | ` + "`suggest_pattern`" + `                        |
| Manually scaffolding from a pattern   | ` + "`scaffold`" + ` — generates code, wiring, and test stubs from an example |
| Read→Edit roundtrip for one symbol    | ` + "`edit_symbol`" + ` — edit source by ID, no Read needed |
| Read→Edit roundtrip for any file      | ` + "`edit_file`" + ` — string-replace any file by absolute or repo-relative path; atomic write, auto-reindex; pass ` + "`dry_run`" + ` to preview |
| Read→Write roundtrip for new files    | ` + "`write_file`" + ` — create or overwrite any file with given content; creates parent dirs; pass ` + "`dry_run`" + ` to preview |
| Manual find-and-replace for renames   | ` + "`rename_symbol`" + ` — coordinated rename across all references |
| Sequencing multi-file edits yourself  | ` + "`batch_edit`" + ` — applies edits in dependency order, re-indexes between steps |
| Reading a diff without graph context  | ` + "`diff_context`" + ` — enriches git diff with callers, callees, community, risk |
| Guessing what context you need next   | ` + "`prefetch_context`" + ` — predicts needed symbols from task + recent activity |

### API Contracts

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Manually tracking API routes/services | ` + "`contracts`" + ` (default ` + "`action: \"list\"`" + `) — lists HTTP, gRPC, GraphQL, topic, WebSocket, env, OpenAPI; filter by ` + "`repo`" + `, ` + "`project`" + `, or ` + "`ref`" + ` |
| Guessing if APIs match across repos   | ` + "`contracts`" + ` with ` + "`action: \"check\"`" + ` — detects orphan providers/consumers and mismatches; scope with ` + "`repo`" + ` / ` + "`project`" + ` / ` + "`ref`" + ` |
| Assessing what breaks before editing a route handler | ` + "`api_impact(route|file)`" + ` — one fused report: response shape, consumers + accessed fields, response-shape mismatches, middleware, execution flows, blast radius (callers + tests to run), fused risk |

### PR / Change Review

A graph-grounded change-review surface walks a diff (staged / branch / pasted patch / PR) through the graph so findings ride on real callers / contracts / coverage / guards, not style nits — instead of hand-walking each gate yourself.

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Eyeballing a diff for correctness bugs | ` + "`analyze`" + ` with ` + "`kind: \"review\"`" + ` — idiomatic / correctness rulepack (NPE, thread-safety check-then-act, N+1, logic-error; Go + Python) with a graph-grounded false-positive-reduction pass |
| Hand-walking detect_changes / diff_context / verify_change / contracts / check_guards one call at a time | ` + "`review`" + ` / ` + "`review_pack`" + ` (MCP) — one fused review packet (verdict + ` + "`file:line`" + ` findings + cost), or the ` + "`gortex review [--diff|--base <ref>] [--audience agent|human] [--post]`" + ` CLI for a terse machine-first verdict |
| Triaging which open PRs need attention | ` + "`list_prs`" + ` / ` + "`triage_prs`" + ` / ` + "`pr_risk`" + ` / ` + "`get_pr_impact`" + ` / ` + "`conflicts_prs`" + ` / ` + "`suggest_reviewers`" + ` (MCP), or the ` + "`gortex prs`" + ` CLI (` + "`--triage`" + ` / ` + "`--conflicts`" + ` / ` + "`--worktrees`" + `) — graph-ranked PR queue, blast radius, reviewer routing |

### CPG-lite Dataflow

The ` + "`flow_between`" + ` and ` + "`taint_paths`" + ` MCP tools answer **"where does this value flow?"** by walking three new edge kinds — ` + "`value_flow`" + ` (intra-procedural), ` + "`arg_of`" + ` (caller arg → callee param), and ` + "`returns_to`" + ` (callee → assignment).

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Hand-tracing a value through helper functions | ` + "`flow_between`" + ` — ranked dataflow paths between two symbol IDs; pass ` + "`max_depth`" + ` (default 8) and ` + "`max_paths`" + ` (default 10); supports ` + "`format: \"gcx\"`" + ` |
| Grepping for sources / sinks         | ` + "`taint_paths`" + ` — pattern-driven sweep returning every flow from a matching source to a matching sink. Pattern syntax: bare token = case-insensitive substring on name; ` + "`exact:Foo`" + ` = exact match; ` + "`path:dir/`" + ` = file-path prefix; ` + "`kind:method`" + ` = node-kind filter; combine clauses with spaces (AND). Sinks expand functions to their params automatically. |
| Reading callers to verify a refactor | ` + "`flow_between`" + ` from the changed return symbol to a downstream consumer's param to find every consumer site, including those reached through helper functions. |

### Clone Detection

` + "`find_clones`" + ` materialises the ` + "`similar_to`" + ` graph layer — a MinHash + LSH pass that hashes every function/method body into a 64-slot signature, LSH-bands the signatures into candidate pairs, and keeps the pairs whose estimated Jaccard similarity crosses the index-time threshold. Catches copy-paste and renamed-variable (Type-1/Type-2) clones.

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Eyeballing the repo for copy-paste    | ` + "`find_clones`" + ` — near-duplicate function/method clusters; pass ` + "`min_similarity`" + ` / ` + "`path_prefix`" + ` / ` + "`repo`" + ` / ` + "`limit`" + ` to scope |
| Finding safe-to-delete duplicates     | ` + "`find_clones`" + ` with ` + "`dead_only: true`" + ` — clusters containing a dead-code symbol ("dead duplicates of live code"); each member is also flagged ` + "`is_dead`" + ` in the default view |

### Multi-Repo Management

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Manually adding a repo to config      | ` + "`track_repository`" + ` — indexes immediately, persists to config |
| Manually removing a repo from config  | ` + "`untrack_repository`" + ` — evicts nodes/edges, persists to config |
| Refreshing the graph after edits      | ` + "`reindex_repository`" + ` — incremental re-index of changed files only; pass ` + "`paths`" + ` to scope to specific files / directories |
| Wondering which project is active     | ` + "`get_active_project`" + ` — returns project name and repo list |
| Switching project context             | ` + "`set_active_project`" + ` — re-scopes all subsequent queries |
| Scoping a query to one repo           | Pass ` + "`repo`" + ` param to ` + "`search_symbols`" + `, ` + "`find_usages`" + `, etc. |
| Scoping a query to a project          | Pass ` + "`project`" + ` param to any query tool |
| Filtering by reference tag            | Pass ` + "`ref`" + ` param to any query tool |

### Live Editor Buffers (Shadow-Graph Overlay Sessions)

Editor extensions push in-flight (unsaved) buffers as **overlays**. Gortex composes a per-request **shadow view** on top of the immutable base graph and threads it through the tool dispatch context. After ` + "`overlay_register`" + ` and one or more ` + "`overlay_push`" + ` calls, every subsequent ` + "`tools/call`" + ` from the same MCP session reads through the shadow view — graph-walking tools (` + "`find_usages`" + `, ` + "`get_call_chain`" + `, ` + "`get_file_summary`" + `, …) and source-reading tools (` + "`get_symbol_source`" + `, ` + "`get_editing_context`" + `, …) all see the overlay.

**Load-bearing invariant: base is never mutated by overlay flow.** Concurrent sessions (A1 / A2 / B) each see their own view; the file watcher's reindex passes don't race with overlay queries; cross-file edges from non-overlaid files into overlaid symbols are preserved because base's edges are untouched.

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Asking the user to save before a query | ` + "`overlay_register`" + ` then ` + "`overlay_push`" + ` — pushes one editor buffer; subsequent tool calls see the overlay |
| Listing what an extension has staged   | ` + "`overlay_list`" + ` — path / size / deleted / base SHA for the current session |
| Cancelling a single overlay           | ` + "`overlay_delete`" + ` with ` + "`path`" + ` — saved-buffer view returns for that path |
| Tearing down an overlay session       | ` + "`overlay_drop`" + ` — discards every overlay attached to the session in one call |
| Previewing impact of an unsaved edit  | ` + "`compare_with_overlay`" + ` with ` + "`kind: find_usages / get_callers / get_call_chain / get_dependencies / get_dependents`" + ` — runs the query against base and overlay simultaneously, returns added / removed / common ID sets |

Pass an editor-captured git blob SHA as ` + "`base_sha`" + ` on ` + "`overlay_push`" + ` to enable drift detection: when the next tool call needs that path, Gortex compares ` + "`base_sha`" + ` to the on-disk hash and returns ` + "`overlay base SHA mismatch`" + ` if they disagree, so the client knows to re-read and resubmit. Push with ` + "`deleted: true`" + ` to model a tombstone — the file's symbols vanish from the shadow view (but are untouched in base).

**Lifecycle.** Overlays are bound to the MCP session that registered them — when the MCP session ends, the overlay is dropped synchronously. Idle TTL (default 30 min, configurable via ` + "`GORTEX_OVERLAY_IDLE_TTL`" + `) is a fail-safe for missed disconnects; every tool call against a live overlay refreshes it. Use ` + "`overlay_keepalive`" + ` for genuine idle gaps without re-pushing content. ` + "`overlay_list`" + ` returns ` + "`expires_at`" + ` / ` + "`idle_seconds`" + ` so extensions can schedule keepalives proactively.

HTTP transport mirrors the surface at ` + "`/v1/overlay/sessions/*`" + `; the ` + "`/v1/tools/<name>`" + ` entry reads the active session from ` + "`Mcp-Session-Id`" + ` / ` + "`X-Gortex-Overlay-Session`" + ` / ` + "`?session_id=`" + `.

### MCP 2026 Streamable HTTP transport (` + "`/mcp`" + `)

` + "`gortex daemon --http-addr <addr>`" + ` exposes the **MCP 2026 Streamable HTTP transport** on` + "`POST /mcp`" + `, ` + "`GET /mcp`" + ` (SSE upstream for server-initiated notifications), ` + "`DELETE /mcp`" + ` (session termination), ` + "`OPTIONS /mcp`" + ` (CORS preflight). One endpoint, one wire format — the spec the June 2026 MCP release locks in. Stateless per request: every POST carries ` + "`Mcp-Session-Id`" + ` and the transport replays state out of a ` + "`streamable.SessionStore`" + ` (in-memory by default, TTL-evicted; swap in a shared backend like Redis to run multiple workers behind a load balancer). ` + "`initialize`" + ` mints the id and returns it on the response header; an unknown id replies with a JSON-RPC ` + "`-32001 session not found`" + ` envelope. JSON-RPC batches preserve order; notification-only batches return HTTP 202. ` + "`tools/call`" + ` frames flow through the same multi-server router that serves ` + "`/v1/tools/<name>`" + `, so workspace scoping carries over unchanged. ` + "`gortex daemon`" + ` enables it via ` + "`--http-addr 127.0.0.1:7411 [--http-auth-token <token>]`" + `; non-localhost binds require an auth token (or ` + "`$GORTEX_DAEMON_HTTP_TOKEN`" + `). ` + "`/healthz`" + ` is exempt from auth so liveness probes work. When ` + "`--http-addr`" + ` is set the daemon serves both ` + "`/mcp`" + ` and the ` + "`/v1/*`" + ` REST surface on one listener.

### Speculative Execution (Simulation Sessions)

Built on the same shadow-graph substrate, ` + "`preview_edit`" + ` and ` + "`simulate_chain`" + ` answer **"what would change if I applied this WorkspaceEdit?"** — graph diff, broken callers/implementors, blast-radius impact, suggested test targets, and (when an LSP is configured) round-trip diagnostics — all without ever mutating the base graph or writing to disk. The input is a standard LSP ` + "`WorkspaceEdit`" + ` (` + "`{changes}`" + ` or ` + "`{documentChanges}`" + `), so any agent that already produces WorkspaceEdits for code actions can speculate on them directly.

| Instead of...                                       | You MUST use...                          |
|-----------------------------------------------------|------------------------------------------|
| Writing the file then running tests to see breakage | ` + "`preview_edit`" + ` with ` + "`workspace_edit: <LSP WorkspaceEdit JSON>`" + ` — single-shot impact report (touched files, broken callers, broken implementors, impact rollup, test targets, optional LSP diagnostics). Disk untouched. |
| Sequencing multiple speculative edits manually      | ` + "`simulate_chain`" + ` with ` + "`steps: [<WorkspaceEdit>, ...]`" + ` — applies steps in order, returns per-step impact + cumulative rollup + per-step diagnostics delta. Pass ` + "`stop_on_error: true`" + ` (default) to abort on the first new ERROR-severity diagnostic. |
| Discarding the simulation when it actually worked   | ` + "`simulate_chain`" + ` with ` + "`keep: true`" + ` — promotes the final simulated state into a real overlay session bound to the calling MCP session; the response carries ` + "`overlay_session_id`" + `. From there, the editor can commit, ` + "`overlay_drop`" + `, or ` + "`compare_with_overlay`" + ` further. |
| Speculating on top of unsaved buffer state          | Either tool with ` + "`inherit_overlay: true`" + ` — layers the simulation on top of the calling session's existing overlay instead of pristine base. |

**Rename heuristic.** When a base symbol disappears from the overlay and exactly one overlay symbol matches its kind + non-trivial signature (after name stripping), the simulator pairs them as a ` + "`symbols_renamed`" + ` entry rather than flagging the change as ` + "`symbols_removed`" + ` + ` + "`symbols_added`" + `. Trivial signatures (parameterless void functions) are intentionally NOT paired — too many false positives. ` + "`Encode(payload []byte) []byte`" + ` → ` + "`Marshal(payload []byte) []byte`" + ` surfaces as a rename; a wholesale rewrite surfaces as removed + added.

**Diagnostics restoration.** The diagnostics pass opens each touched file on the relevant LSP server, sends a ` + "`didChange`" + ` with the simulated content, waits for ` + "`publishDiagnostics`" + `, then sends a second ` + "`didChange`" + ` restoring the on-disk bytes — so concurrent sessions on the same daemon never observe the simulated state as authoritative. Set ` + "`diagnostics: false`" + ` to skip the LSP round-trip when only the graph delta matters.

### Session Memory (save_note / query_notes / distill_session)

Gortex remembers code; this triplet remembers **why you made a call**. Notes persist per-repo across daemon restarts and context compactions, are scoped to the session's workspace, and are auto-linked to symbols mentioned in the body (` + "`pkg/foo.go::Bar`" + ` IDs resolve directly; bare identifiers resolve via the graph's name index, ambiguous names dropped for precision). Use these on every meaningful task — not "optional."

| Trigger                                                  | You MUST call                                                                 |
|----------------------------------------------------------|-------------------------------------------------------------------------------|
| At session start in any touched repo (after a compaction or on a fresh run) | ` + "`distill_session`" + ` — returns top symbols, pinned notes, decisions, recent excerpts from prior sessions. Seed your mental model with this before reading any file. |
| You make a decision, reject an alternative, find a non-obvious constraint, or commit to an invariant | ` + "`save_note tags:\"decision\" body:\"<what+why>\"`" + ` — mention affected symbol IDs in the body for auto-linking; pin (` + "`pinned:true`" + `) anything load-bearing. |
| Before editing a symbol you've touched before            | ` + "`query_notes symbol_id:\"<id>\"`" + ` — surfaces prior decisions, gotchas, "do not change this without …" warnings attached to that symbol. |
| Mid-task discovery worth a follow-up                     | ` + "`save_note tags:\"follow-up\" body:\"<what to revisit and why>\"`" + ` — keeps the lead alive after compaction. |
| Looking for what was tried in this area before           | ` + "`query_notes text:\"<keyword>\"`" + ` or ` + "`query_notes file_path:\"<path>\"`" + ` — full filter set: symbol_id, file_path, tag, text, session_id (` + "`all`" + ` for cross-session), since (RFC-3339), pinned_only. |

**What to save:** decisions, non-obvious constraints, follow-ups ("revisit when …"), bug reproductions, surprising graph findings, partial-progress hand-offs.
**What to skip:** play-by-play of what you just did (the diff says it), code patterns derivable from the graph, anything already in CLAUDE.md / AGENTS.md.

Canonical tags: ` + "`decision`" + `, ` + "`bug`" + `, ` + "`follow-up`" + `, ` + "`gotcha`" + `, ` + "`invariant`" + `. The ` + "`decision`" + ` tag gets its own section in ` + "`distill_session`" + `.

` + "`save_note`" + ` also accepts ` + "`id`" + ` (switches to update mode), ` + "`links`" + ` (comma-separated symbol IDs to attach explicitly), ` + "`no_autolink: true`" + ` (when the body intentionally mentions identifiers that should not be linked), and ` + "`file_path`" + ` (when the note is about a file, not a symbol). ` + "`distill_session`" + ` accepts ` + "`max_symbols`" + ` / ` + "`max_files`" + ` / ` + "`max_tags`" + ` / ` + "`max_recent`" + ` / ` + "`excerpt_chars`" + ` to tune digest size; default fits in ~1k tokens.

### Development Memories (store_memory / query_memories / surface_memories)

` + "`save_note`" + ` is a **per-session scratchpad**; ` + "`store_memory`" + ` is the **workspace-wide durable knowledge base**. Memories outlive sessions, agents, and teammates: every future agent in this workspace inherits them. Memories carry ` + "`kind`" + ` (` + "`invariant`" + ` / ` + "`constraint`" + ` / ` + "`convention`" + ` / ` + "`gotcha`" + ` / ` + "`decision`" + ` / ` + "`incident`" + ` / ` + "`reference`" + `), ` + "`importance`" + ` (1..5), ` + "`confidence`" + ` (0..1), and ` + "`supersedes`" + ` for evolution. Use these on every meaningful task — the memory store compounds the longer your team uses Gortex.

| Trigger                                                  | You MUST call                                                                 |
|----------------------------------------------------------|-------------------------------------------------------------------------------|
| Immediately after ` + "`smart_context`" + ` (every new task)            | ` + "`surface_memories task:\"<task>\" symbol_ids:\"<top hits>\"`" + ` — returns memories ranked by anchor symbol overlap, file overlap, keyword hits, importance, pinning, recency, and confidence. Each hit carries ` + "`match_reasons`" + ` so you know *why* it surfaced. |
| You discover a durable invariant / gotcha / decision worth teaching the team | ` + "`store_memory kind:\"<kind>\" body:\"<what+why>\" symbol_ids:\"<id>\" importance:5`" + ` — pin (` + "`pinned:true`" + `) anything load-bearing. ` + "`title:\"<one-liner>\"`" + ` becomes the headline. |
| Before editing a symbol with a known history             | ` + "`query_memories symbol_id:\"<id>\"`" + ` — all memories anchored or auto-linked to that symbol. |
| A memory is no longer true                                | ` + "`store_memory body:\"<corrected>\" supersedes:\"<old-id>\"`" + ` — old entry stays in the store (audit), hidden from ` + "`surface_memories`" + ` by default. Don't delete unless the original was wrong. |
| Browsing all wisdom anchored to a file                    | ` + "`query_memories file_path:\"<path>\"`" + ` or ` + "`query_memories kind:\"invariant\"`" + ` to filter by kind. |

**Store:** invariants ("Bar must hold the lock"), conventions ("this package never uses gob"), incident learnings, API contracts not enforced by types, debugging traps, cross-cutting decisions with non-obvious rationale.
**Skip:** anything derivable from the code (the graph already knows), session-local play-by-play (use ` + "`save_note`" + ` instead), CLAUDE.md / AGENTS.md content (already loaded), one-off observations with no actionable consequence.

### MCP Resources

Bootstrap-state tools are also exposed as MCP resources (read-only, URI-addressable, no args). Subscribe via ` + "`resources/subscribe`" + ` once and receive ` + "`notifications/resources/updated`" + ` after each graph re-warm — no polling. Tools stay registered for back-compat with clients that don't speak resources; both surfaces share builder helpers so payloads match byte-for-byte.

| Resource URI                                              | Same payload as tool          |
|-----------------------------------------------------------|-------------------------------|
| ` + "`gortex://stats`" + `                                          | ` + "`graph_stats`" + `                 |
| ` + "`gortex://index-health`" + `                                   | ` + "`index_health`" + `                |
| ` + "`gortex://workspace`" + `                                      | ` + "`workspace_info`" + `              |
| ` + "`gortex://repos`" + `                                          | ` + "`list_repos`" + `                  |
| ` + "`gortex://active-project`" + `                                 | ` + "`get_active_project`" + `          |
| ` + "`gortex://schema`" + `                                         | (graph schema reference)      |
| ` + "`gortex://session`" + `                                        | (recent activity replay)      |
| ` + "`gortex://communities`" + ` / ` + "`gortex://community/{id}`" + `        | community detection rollups |
| ` + "`gortex://processes`" + ` / ` + "`gortex://process/{id}`" + `            | process discovery rollups   |

Analyzer-backed rollups (read-only summaries; the only "argument" is the current state of the indexed code):

| Resource URI            | Synthesises                                              |
|-------------------------|----------------------------------------------------------|
| ` + "`gortex://report`" + `       | High-level orientation: graph size, top languages/kinds, hotspot count, dead-code count, todo count |
| ` + "`gortex://god-nodes`" + `    | Top 20 hotspots (subset of ` + "`analyze kind:hotspots`" + `) |
| ` + "`gortex://surprises`" + `    | Cycles + dead code + cross-community call hubs           |
| ` + "`gortex://audit`" + `        | ` + "`audit_agent_config`" + ` with discovery defaults             |
| ` + "`gortex://questions`" + `    | TODO/FIXME rollup grouped by tag and assignee            |

## Session start (Gortex)

1. Call ` + "`graph_stats`" + ` to confirm Gortex is running and get repo orientation.
2. If ` + "`total_nodes`" + ` is 0, call ` + "`index_repository`" + ` with path ` + "`\".\"`" + `.
3. Call ` + "`distill_session`" + ` to recover prior session memory for this workspace — decisions, pinned notes, recent excerpts. Use the digest to seed your mental model before reading any file.
4. In multi-repo mode, call ` + "`get_active_project`" + ` to check scope. Use ` + "`set_active_project`" + ` to switch if needed.
5. For a new task, call ` + "`smart_context`" + ` with the task description. Immediately after, call ` + "`surface_memories task:\"<task>\" symbol_ids:\"<top hits>\"`" + ` to pick up any cross-session invariants / gotchas / decisions anchored to your working set.
6. For every file you are about to edit, call ` + "`get_editing_context`" + ` first. If you've touched the symbol before, also call ` + "`query_notes symbol_id:\"<id>\"`" + ` and ` + "`query_memories symbol_id:\"<id>\"`" + ` to surface prior notes and durable memories.
7. Before changing a function signature, call ` + "`verify_change`" + ` to catch contract violations — checks callers across all repos.
8. Before any refactor, call ` + "`get_edit_plan`" + ` for dependency-ordered file list. Use ` + "`batch_edit`" + ` to apply atomically.
9. After editing, call ` + "`check_guards`" + ` to verify team conventions, then ` + "`get_test_targets`" + ` for tests to run (includes cross-repo test files).
10. After making a meaningful decision or hitting a non-obvious constraint, call ` + "`save_note tags:\"decision\" body:\"<what+why>\"`" + ` so the next session can recover it.
11. Before committing, call ` + "`detect_changes`" + ` to verify scope. Use ` + "`diff_context`" + ` for graph-enriched review.

## Graph Schema (Gortex)

**Node kinds** (filter ` + "`search_symbols`" + ` with ` + "`kind`" + `):
- Code structure: ` + "`file`" + `, ` + "`package`" + `, ` + "`function`" + `, ` + "`method`" + `, ` + "`type`" + `, ` + "`interface`" + `, ` + "`field`" + `, ` + "`variable`" + `, ` + "`constant`" + `, ` + "`import`" + `, ` + "`contract`" + `, ` + "`param`" + `, ` + "`closure`" + `, ` + "`enum_member`" + `, ` + "`generic_param`" + `
- Coverage extensions: ` + "`module`" + ` (ecosystem deps), ` + "`table`" + `/` + "`column`" + ` (db schema), ` + "`config_key`" + ` (env/viper), ` + "`flag`" + ` (feature flags), ` + "`event`" + ` (logs/metrics/spans), ` + "`migration`" + `, ` + "`fixture`" + ` (test data), ` + "`todo`" + ` (TODO/FIXME comments), ` + "`team`" + ` (CODEOWNERS), ` + "`license`" + `, ` + "`release`" + ` (tag boundaries)
- Infrastructure: ` + "`resource`" + ` (K8s manifest — Deployment/Service/Ingress/ConfigMap/Secret/CronJob/…), ` + "`kustomization`" + ` (Kustomize overlay), ` + "`image`" + ` (Dockerfile FROM target or K8s ` + "`container.image`" + `)

**Edge kinds** (used internally; pass kind name to ` + "`analyze`" + ` to query):
- Calls / structure: ` + "`calls`" + `, ` + "`imports`" + `, ` + "`defines`" + `, ` + "`implements`" + `, ` + "`extends`" + `, ` + "`references`" + `, ` + "`member_of`" + `, ` + "`instantiates`" + `, ` + "`provides`" + `, ` + "`consumes`" + `, ` + "`composes`" + `, ` + "`aliases`" + `, ` + "`typed_as`" + `, ` + "`returns`" + `, ` + "`captures`" + `, ` + "`param_of`" + `
- Concurrency: ` + "`spawns`" + ` (goroutine/async), ` + "`sends`" + ` / ` + "`recvs`" + ` (channels)
- Mutation: ` + "`reads`" + ` / ` + "`writes`" + ` (fields), ` + "`reads_config`" + ` / ` + "`writes_config`" + `
- Dataflow (CPG-lite, ` + "`flow_between`" + ` / ` + "`taint_paths`" + `): ` + "`value_flow`" + ` (intra-procedural assignment / return / range), ` + "`arg_of`" + ` (caller arg → callee param), ` + "`returns_to`" + ` (callee → assignment LHS)
- Metadata: ` + "`annotated`" + ` (decorators), ` + "`emits`" + ` (events + pub/sub publish), ` + "`listens_on`" + ` (pub/sub subscribe), ` + "`throws`" + ` (errors), ` + "`queries`" + ` (SQL), ` + "`reads_col`" + ` / ` + "`writes_col`" + `, ` + "`toggles_flag`" + `, ` + "`depends_on_module`" + `, ` + "`matches`" + ` (fixtures), ` + "`generated_by`" + `, ` + "`tests`" + ` (test → tested symbol), ` + "`covered_by`" + `, ` + "`owns`" + ` (CODEOWNERS), ` + "`authored`" + `, ` + "`licensed_as`" + `
- Infrastructure (K8s / Kustomize / Dockerfile): ` + "`configures`" + ` (workload → ConfigMap/Secret via env/envFrom), ` + "`mounts`" + ` (workload → volume source: ConfigMap/Secret/PVC), ` + "`exposes`" + ` (Resource/Image → ` + "`port::<proto>::<n>`" + `), ` + "`depends_on`" + ` (Ingress→Service / stage→base / overlay→base / Resource→Image), ` + "`uses_env`" + ` (Resource/Image → ` + "`cfg::env::<NAME>`" + ` config_key — shared ID with ` + "`os.Getenv`" + ` so the cross-ref between infra declaration and code-side reads is automatic)
- Similarity (` + "`find_clones`" + `): ` + "`similar_to`" + ` (function/method near-duplicate — MinHash + LSH clone detection; symmetric; ` + "`Meta[\"similarity\"]`" + ` carries the estimated Jaccard score)
- Cross-repo (` + "`analyze kind=cross_repo`" + `): ` + "`cross_repo_calls`" + ` / ` + "`cross_repo_implements`" + ` / ` + "`cross_repo_extends`" + ` (parallel edge emitted alongside a calls/implements/extends edge whose From and To nodes live in different repos; base edge also gets ` + "`Edge.CrossRepo`" + ` set)
`

// AppendInstructions appends body to path, creating the file if
// missing. Idempotent: when `sentinel` is already present anywhere in
// the file we skip with ActionSkip and log the reason. Callers pass
// the adapter's ApplyOpts through so --dry-run / --global / --force
// all flow to the right FileAction status.
//
// Not atomic. Rules files are plaintext a human edits, matching the
// historical CLAUDE.md append behaviour — a concurrent external writer
// during init is extraordinarily unlikely and atomic rename of a file
// a human is editing would fight their editor.
func AppendInstructions(w io.Writer, path, body, sentinel string, opts ApplyOpts) (FileAction, error) {
	existing, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return FileAction{}, fmt.Errorf("read %s: %w", path, readErr)
	}
	existed := readErr == nil
	if existed && strings.Contains(string(existing), sentinel) {
		if w != nil {
			_, _ = fmt.Fprintf(w, "[gortex init] skip %s (Gortex block already present)\n", path)
		}
		return FileAction{Path: path, Action: ActionSkip, Reason: "block-present"}, nil
	}

	if opts.DryRun {
		action := ActionWouldMerge
		if !existed {
			action = ActionWouldCreate
		}
		return FileAction{Path: path, Action: action, Keys: []string{"gortex-block"}}, nil
	}

	// Two blank lines between existing content and the block so the
	// appended section reads as a separate document and doesn't glue
	// onto the last paragraph the user wrote.
	prefix := ""
	if existed && len(existing) > 0 {
		prefix = "\n\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return FileAction{}, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return FileAction{}, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(prefix + body); err != nil {
		return FileAction{}, err
	}
	if w != nil {
		_, _ = fmt.Fprintf(w, "[gortex init] appended Gortex block to %s\n", path)
	}
	action := ActionMerge
	if !existed {
		action = ActionCreate
	}
	return FileAction{Path: path, Action: action, Keys: []string{"gortex-block"}}, nil
}

// CursorMDCFrontmatter wraps the instructions body in the YAML
// frontmatter Cursor expects for MDC rules files. Cursor reads
// `alwaysApply: true` rules on every chat turn — which is what we
// want for the MANDATORY-prefer-Gortex block.
//
// Kept separate from AppendInstructions because MDC files are
// one-rule-per-file (Cursor owns the filename, not the content), so
// they use WriteIfNotExists semantics, not append.
func CursorMDCFrontmatter(body string) string {
	return `---
description: Gortex code intelligence — prefer graph tools over file reads
alwaysApply: true
---

` + body
}

// UpsertMarkedBlock writes `body` into `path` between `startMarker`
// and `endMarker`. Unlike AppendInstructions, this is idempotent AND
// regeneratable: if the markers already exist the block between them
// is replaced; otherwise the block is appended with a blank-line gap
// to existing content. If `body` is empty and the markers exist, the
// block is removed (migration use case). Creates the file if missing.
//
// Designed for the per-repo community-routing block which regenerates
// on every `gortex init` run as the graph evolves.
func UpsertMarkedBlock(w io.Writer, path, body, startMarker, endMarker string, opts ApplyOpts) (FileAction, error) {
	existing, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return FileAction{}, fmt.Errorf("read %s: %w", path, readErr)
	}
	existed := readErr == nil
	text := ""
	if existed {
		text = string(existing)
	}

	hasBlock := existed && strings.Contains(text, startMarker) && strings.Contains(text, endMarker)
	empty := strings.TrimSpace(body) == ""

	// Nothing to do: empty body and no existing block.
	if empty && !hasBlock {
		return FileAction{Path: path, Action: ActionSkip, Reason: "no-communities"}, nil
	}

	fenced := startMarker + "\n" + body + "\n" + endMarker + "\n"

	var next string
	switch {
	case hasBlock:
		start := strings.Index(text, startMarker)
		end := strings.Index(text, endMarker) + len(endMarker)
		// Trim trailing newline after the end marker so we don't
		// accumulate blank lines on repeated re-runs.
		if end < len(text) && text[end] == '\n' {
			end++
		}
		if empty {
			next = text[:start] + text[end:]
		} else {
			next = text[:start] + fenced + text[end:]
		}
	case !existed:
		next = fenced
	default:
		prefix := ""
		if len(text) > 0 {
			if !strings.HasSuffix(text, "\n") {
				prefix = "\n\n"
			} else if !strings.HasSuffix(text, "\n\n") {
				prefix = "\n"
			}
		}
		next = text + prefix + fenced
	}

	// Skip when the file would end up byte-identical to what's
	// already there — important for AssertIdempotent semantics and
	// for avoiding spurious mtime bumps on `gortex init` re-runs
	// when the graph hasn't changed.
	if existed && next == text {
		return FileAction{Path: path, Action: ActionSkip, Reason: "unchanged"}, nil
	}

	if opts.DryRun {
		switch {
		case !existed:
			return FileAction{Path: path, Action: ActionWouldCreate, Keys: []string{"communities-block"}}, nil
		case hasBlock:
			return FileAction{Path: path, Action: ActionWouldMerge, Keys: []string{"communities-block"}}, nil
		default:
			return FileAction{Path: path, Action: ActionWouldMerge, Keys: []string{"communities-block"}}, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return FileAction{}, err
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return FileAction{}, err
	}
	if w != nil {
		verb := "updated"
		if !existed {
			verb = "wrote"
		}
		_, _ = fmt.Fprintf(w, "[gortex init] %s %s (communities block)\n", verb, path)
	}
	action := ActionMerge
	if !existed {
		action = ActionCreate
	}
	return FileAction{Path: path, Action: action, Keys: []string{"communities-block"}}, nil
}
