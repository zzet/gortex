# Gortex

Code intelligence engine written in Go. Indexes repositories into a knowledge graph held in a local SQLite store and exposes it via CLI and MCP Server.

## Build & Test

```bash
go build -o gortex ./cmd/gortex/   # requires CGO (tree-sitter C bindings)
go test -race ./...                 # all test packages must pass
```

## Codebase Overview

- **Languages:** go (primary)
- **Entry point:** `cmd/gortex/main.go`
- **Source:** 1,338 Go files (728 non-test) across the `cmd/` and `internal/` trees
- **Graph size:** ~31k nodes, ~206k edges when the daemon indexes this repo

## MANDATORY: Use Gortex's graph tools instead of Read/Grep/Glob

Gortex's workhorse tools are reachable two equivalent ways — over the registered **MCP server**, or via the equivalent **`gortex` CLI verbs** (`gortex edit verify`, `gortex memory surface`, `gortex analyze`, and `gortex call <tool>` for anything else; the verb reference is in `docs/cli.md`). Use whichever your harness has mounted — they are two front doors over the same handlers, and the daemon routes tool calls by name so the CLI reaches the full surface even under the `core` preset.

Either way, you **MUST** prefer graph queries over file reads on every task in this repo — `search_symbols`, `find_usages`, `get_symbol_source`, `get_editing_context`, `smart_context`, `edit_symbol` / `edit_file` / `rename_symbol` / `batch_edit` (or the matching `gortex` verbs). PreToolUse hooks deny `Read` / `Grep` / `Glob` against indexed source; the deny message names the right tool. The MCP server registers 180+ tools but by default publishes only a curated `core` preset (~34 dev-cycle workhorses) eagerly in `tools/list`, deferring the rest behind the `tools_search` discovery tool (the `core`/`defer` default — see `docs/mcp.md`). Opt into the full eager surface with `GORTEX_TOOLS=full`; `GORTEX_LAZY_TOOLS=1` is the older all-or-nothing defer switch. The cross-project rule tables live in `~/.claude/CLAUDE.md` — neither is restated here. This file carries only project-specific guidance.

### Discovery (read once, then keep using)

- **Graph schema** — `gortex://schema` resource (node kinds, edge kinds, what each carries).
- **Analyzer rollups** — `gortex://report`, `gortex://surprises`, `gortex://god-nodes`, `gortex://questions`, `gortex://audit`.
- **Bootstrap state** — `gortex://stats`, `gortex://index-health`, `gortex://workspace`, `gortex://repos`, `gortex://active-project`.

### LLM provider (powers `ask` and `search_symbols assist:` modes)

Selected via `llm.provider` in `.gortex.yaml` or `~/.config/gortex/config.yaml`. The HTTP and subprocess providers are pure Go — available without `-tags llama`.

| Provider | Backend | Requires |
|---|---|---|
| `local` (default) | in-process llama.cpp | `-tags llama` build + `llm.local.model` (a `.gguf` path) |
| `anthropic` | Messages API | `llm.anthropic.model` + `ANTHROPIC_API_KEY` |
| `openai` | Chat Completions | `llm.openai.model` + `OPENAI_API_KEY`. Optional `llm.openai.effort` → `reasoning_effort`. |
| `azure` | Azure OpenAI Service | `llm.azure.deployment` + endpoint (`llm.azure.endpoint` or `AZURE_OPENAI_ENDPOINT`) + `AZURE_OPENAI_API_KEY`. Deployment-in-path + `api-version` query + `api-key` header; `llm.azure.api_version` defaults to a recent GA. Same json_schema structured output as `openai`. |
| `ollama` | Ollama daemon | `llm.ollama.model` (+ `llm.ollama.host`, default `localhost:11434`) |
| `claudecli` | `claude` CLI subprocess | `claude` on `$PATH` (signed in once); optional `llm.claudecli.model` (`sonnet`/`opus`/…). Reuses the user's Claude Code subscription. |
| `codex` | OpenAI `codex` CLI subprocess | `codex` on `$PATH` (signed in once); optional `llm.codex.model`. Runs `codex exec` in a read-only sandbox; reuses the user's Codex / ChatGPT sign-in. |
| `copilot` | GitHub Copilot CLI subprocess | `copilot` on `$PATH` (signed in via `gh`); optional `llm.copilot.model`. Runs `copilot -p`. |
| `cursor` | Cursor Agent CLI subprocess | `cursor-agent` on `$PATH` (signed in once); optional `llm.cursor.model`. Runs `cursor-agent --output-format text -p`. |
| `opencode` | opencode CLI subprocess | `opencode` on `$PATH` (signed in once); optional `llm.opencode.model` (`provider/model`). Runs `opencode run`. |
| `gemini` | Google Gemini `generateContent` REST | `llm.gemini.model` (default `gemini-2.5-pro`) + `GEMINI_API_KEY`. Structured output via `responseSchema` (`additionalProperties` stripped — Gemini rejects it). |
| `bedrock` | AWS Bedrock Converse API (SigV4) | `llm.bedrock.model_id` (e.g. `anthropic.claude-sonnet-4-20250514-v1:0`) + `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY` (+ optional `AWS_SESSION_TOKEN` for STS). Region defaults to `us-east-1` (`llm.bedrock.region`). Structured output via forced `respond` tool. No AWS SDK dependency — SigV4 is implemented in ~100 LOC of stdlib. |
| `deepseek` | DeepSeek Chat Completions (OpenAI-compatible) | `llm.deepseek.model` (default `deepseek-chat`) + `DEEPSEEK_API_KEY`. Structured output uses `response_format: json_object` plus a schema hint in the system prompt — DeepSeek does not support strict `json_schema`. |

`GORTEX_LLM_PROVIDER` / `GORTEX_LLM_MODEL` / `GORTEX_LLM_{CLAUDECLI,CODEX,COPILOT,CURSOR,OPENCODE}_BINARY` / `GORTEX_LLM_BEDROCK_REGION` / `GORTEX_LLM_AZURE_{ENDPOINT,DEPLOYMENT,API_VERSION}` / `GORTEX_LLM_EFFORT` override the file config. `GORTEX_LLM_MODEL` targets the active provider's model field (Gemini → `llm.gemini.model`, Bedrock → `llm.bedrock.model_id`, DeepSeek → `llm.deepseek.model`, etc.). If the active provider can't construct (missing model / API key, `local` without `-tags llama`, `claudecli` / `codex` without `claude` / `codex` on `$PATH`, `bedrock` without AWS credentials), the daemon logs a warning and `ask` stays unregistered — fall through to direct tools.

**Custom providers.** Any OpenAI-compatible endpoint can be registered by name with `gortex provider add/list/show/remove` (writes `providers.json` next to the config; a repo-local `.gortex/providers.json` loads only under `GORTEX_ALLOW_LOCAL_PROVIDERS=1`). A registered name is selectable like any built-in via `llm.provider` / `GORTEX_LLM_PROVIDER`; entries carry `base_url` + `model` + optional `api_key_env`, `schema_mode` (`json_schema`/`json_object`/`prompt`), headers, and informational pricing.

**Anthropic tuning.** `llm.anthropic.model` accepts the tier sentinels `claude-haiku` / `claude-sonnet` / `claude-opus`, resolved to the newest live model id (per-auth cache + pinned fallback; override per tier via `GORTEX_LLM_ANTHROPIC_{HAIKU,SONNET,OPUS}_MODEL`). Opt-in `llm.anthropic.prompt_caching` (+ `cache_ttl`) marks the system prompt and structured-output tool as ephemeral cache breakpoints; `llm.anthropic.thinking_mode` (`off`/`auto`/`manual`/`adaptive`) drives extended thinking on freeform requests; `llm.anthropic.effort` / `GORTEX_LLM_EFFORT` sends a model-gated reasoning effort.

`llm.routing` (off by default) routes the `ask` agent to a cheaper or more capable model by graph-derived task complexity — set `routing.enabled`, `routing.simple_model`, `routing.complex_model`; the chosen `model` / `complexity` ride on the `ask` response.

`search_symbols assist:` modes: `auto` (default — skips LLM for identifier queries, expands NL queries), `on` (forces expansion+rerank), `off` (pure BM25), `deep` (adds a body-grounded verification pass; +1.5–4 s; quality is highly model-dependent — unreliable on 3B local models, fine on 7B+ or hosted).

### Non-obvious capabilities worth knowing

- **`compress_bodies: true`** on `read_file` / `get_symbol_source` / `get_editing_context` elides function bodies to stubs while keeping signatures + doc-comments + structure. ~30–40% of original tokens. 14 languages.
- **Overlay sessions** (`overlay_push`, `overlay_list`, `overlay_drop`, `compare_with_overlay`) let editor extensions push unsaved buffers as a per-session shadow graph — every subsequent tool call reads through it without mutating base. Bound to the MCP session lifecycle; idle TTL via `GORTEX_OVERLAY_IDLE_TTL` (default 30m).
- **Speculative execution** (`preview_edit`, `simulate_chain`) takes an LSP `WorkspaceEdit` and returns the graph diff + broken callers/implementors + impact rollup + suggested tests + (optional) LSP diagnostics — disk untouched. `simulate_chain` with `keep: true` promotes the final state into a real overlay.
- **Change-contract pipeline** (`change_contract`, `symbols_for_ranges`) — one envelope every change source lowers into. `change_contract` takes a WorkspaceEdit, a git diff range (`source:diff base:…`), an explicit symbol set, or file line-ranges, runs LOWER → PREDICT → EVALUATE (guards + architecture + event-boundary rule families) → SCORE → CLASSIFY → EMIT, and returns one verdict `{allow|warn|refuse}` with reasons, risk, a `verification_command`, a checkable `stop_condition`, and an `edit_strategy`. `lens:api` focuses it on public-surface / API drift; `risk_gate:true` requires a TTL'd impact-review ack (`ack:true`, stored as a development memory) for load-bearing symbols. `symbols_for_ranges` is the standalone lowering primitive. The pre-write **parse gate** on `edit_file` / `write_file` refuses an edit that would introduce new tree-sitter parse errors (override with `allow_parse_errors`); `safe_delete_symbol propagate:true` patches surviving call sites; `analyze kind=suggest_boundaries` seeds an `architecture:` block from detected communities.
- **MCP 2026 Streamable HTTP** at `POST /mcp` — `gortex server` always mounts it; `gortex daemon --http-addr <addr>` opts the daemon in (non-localhost binds require `--http-auth-token`).
- **Session memory** (`save_note`, `query_notes`, `distill_session`) persists agent-authored notes per repo, auto-linked to symbols mentioned in the body. Notes survive daemon restarts and context compactions, scoped to the session's workspace.
- **Development memories** (`store_memory`, `query_memories`, `surface_memories`) — cross-session, symbol-linked durable knowledge that compounds the longer a team uses Gortex. Memories carry `kind` (invariant / constraint / convention / gotcha / decision / incident / reference), `importance` (1..5), `confidence` (0..1), and are surfaced *proactively* by `surface_memories` when their anchor symbols / files enter the agent's working set.
- **Artifacts** — non-code knowledge files (DB schemas, API specs, ADRs, infra configs) declared in `.gortex.yaml` `artifacts:` are indexed as `artifact` nodes; `search_artifacts` / `get_artifact` surface them and `EdgeReferences` links code to the spec it implements.
- **Code search beyond symbols** — `search_text` is a trigram-indexed literal / regex search (the grep replacement for non-symbol strings); `search_ast` runs structural tree-sitter queries; `analyze kind=sast` is a 190-rule, CWE/OWASP-tagged security scan across 8 languages.
- **Push notifications** — beyond `notifications/diagnostics`, the server pushes `graph_invalidated` (graph hot-reload), `daemon_health`, `stale_refs`, and `workspace_readiness`. `subscribe_*` once per session instead of polling.
- **`get_architecture`** — one-call architectural snapshot (languages, communities, hotspots, processes); pass `resolution` for a hierarchical symbol → file → package → service → system rollup.
- **Capability edges** — `reads_env` / `executes_process` / `accesses_field` are first-class traversable edges (in the `walk_graph`/`nav`/`graph_query` surface) synthesised post-resolution, so a supply-chain / least-privilege audit can ask "what reads $AWS_SECRET", "what shells out", "what writes this field" in one hop.
- **PR review, end-to-end** — `gortex prs` triages open pull requests from the graph (`gortex prs <N>` for one PR; `--triage` / `--conflicts` / `--worktrees` / `--base` / `--format`; `gortex prs bundle`), and `gortex review [<base>|--diff] [--audience agent|human] [--post]` reviews a diff. The MCP surface mirrors it: `pr_risk` / `list_prs` / `get_pr_impact` / `triage_prs` / `conflicts_prs` / `suggest_reviewers` score and rank PRs against the graph, while `review` / `review_pack` / `post_review` / `pr_review_context` / `suggested_review_questions` / `critique_review` / `suppress_finding` drive the review itself.
- **Multimodal + broad ingest** — image files (`KindImage` assets with format/dimensions/sha256) and PDF documents (per-page searchable `KindDoc` nodes) are graph nodes; new first-class extractors cover Terraform/HCL cross-block references, Helm charts/templates, Ansible playbooks, .NET `.sln`/`.csproj`, MCP server configs, Quarto `.qmd`, Luau, COBOL paragraphs + JCL, and C/C++ `#define` macros. Grammar-less languages can register a regex fallback chunker (`index.fallback_chunkers`) or an external extractor plugin (`index.extractor_plugins`) from config — no fork. `gortex db schema --postgres <dsn>` ingests a live database's schema.
- **`analyze` is a 61-kind dispatcher** — beyond the structural kinds, it now covers `impact` (composite change-risk score), `bottlenecks` (interprocedural computation-bottleneck risk — cognitive complexity, loop depth, transitive/hidden-O(n^k) loop nesting across calls, unguarded recursion), `health_score` (per-symbol A–F grade), `sast` / `named` / `unsafe_patterns` (security), `clusters`, `suggest_boundaries` (Leiden-community-seeded architecture-layer suggestions), `connectivity_health`, `tests_as_edges`, `synthesizers` (framework-dispatch-synthesized edges, grouped by pass + provenance), `resolution_outcomes` (structured why-unresolved taxonomy), `review` (an idiomatic/correctness rulepack — NPE, thread-safety check-then-act, N+1, logic errors across Go + Python — with a graph-grounded false-positive-reduction pass), and more.

## MANDATORY: Session memory — save, recall, distill

The `save_note` / `query_notes` / `distill_session` triplet is the agent's durable scratchpad. The graph remembers code; these tools remember **why you made a call**. Without them, every compaction erases hard-won context.

Three triggers — not suggestions:

1. **After a context compaction (or at session start in a touched repo)** — **call** `distill_session` first thing. Returns top symbols, pinned notes, decisions, and recent excerpts from prior sessions in this workspace. Use the digest to seed your mental model before reading any file.
2. **At every decision point** — **call** `save_note tags:"decision" body:"<what+why>"` when you pick an approach, reject an alternative, discover a non-obvious constraint, or commit to an invariant. Mention the affected symbol/file by ID (`pkg/foo.go::Bar`) so the auto-linker attaches the note to the graph. Pin (`pinned:true`) anything that should survive the store cap.
3. **Before editing a symbol you've touched before** — **call** `query_notes symbol_id:"<id>"`. Prior decisions, bug-fix notes, or "do not change this without …" warnings ride on the symbol's note list and you should see them before re-deriving (or worse, reverting) past work.

What to save vs. skip:
- **Save:** decisions ("chose X over Y because Z"), non-obvious constraints, follow-ups ("revisit when …"), bug reproductions, surprising graph findings, partial-progress hand-offs.
- **Skip:** play-by-play of what you just did (the diff says it), code patterns derivable from the graph, anything already in CLAUDE.md.

Useful tags: `decision`, `bug`, `follow-up`, `gotcha`, `invariant`. `decision`-tagged notes are surfaced in their own section by `distill_session`.

## MANDATORY: Development memories — store, query, surface

`save_note` is a **per-session scratchpad**; `store_memory` is the **workspace-wide durable knowledge base**. The two are complementary, not redundant:

| | `save_note` (session) | `store_memory` (cross-session) |
|---|---|---|
| Scope | session_id | workspace-wide |
| Lifetime | survives compaction | survives daemon restarts, agent changes, team rotation |
| Audience | future-you in this session | every future agent in this workspace |
| Surfacing | `distill_session` (manual) | `surface_memories` (proactive, ranked) |
| Right when | "remember this for the next 30 min" | "every agent touching `Bar` should know this" |

Three triggers — not suggestions:

1. **At task start, after `smart_context`** — **call** `surface_memories task:"<task>" symbol_ids:"<top hits from smart_context>"`. Returns memories ranked by anchor symbol overlap, file overlap, task-keyword hits, importance, pinning, recency, and confidence. Memories prefixed with `match_reasons:["symbol:pkg/foo.go::Bar"]` are direct evidence the memory applies to your working set. If `surface_memories` returns nothing, don't probe further.
2. **When you discover a durable fact worth teaching the team** — **call** `store_memory kind:"<invariant|gotcha|convention|decision|constraint|incident>" body:"<what+why>" symbol_ids:"pkg/foo.go::Bar" importance:5`. Pin (`pinned:true`) anything load-bearing. Set `kind` honestly: `invariant` means "violating this breaks the system", `gotcha` means "an agent will get this wrong without warning". Title (`title:"..."`) the memory if the body is long — it becomes the headline.
3. **When a memory is no longer true** — **call** `store_memory id:"<new>" supersedes:"<old-id>" body:"<corrected fact>"`. The old memory stays in the store (for audit) but is hidden from `surface_memories` by default. Don't delete unless the original was wrong; supersession preserves history.

What to store vs. skip:
- **Store:** invariants ("Bar must hold the lock"), conventions ("this package never uses gob"), incident learnings ("once, doing X under Y crashed prod"), API contracts not enforced by types, debugging traps, cross-cutting decisions with non-obvious rationale.
- **Skip:** anything derivable from the code (the graph already knows), session-local play-by-play (use `save_note`), CLAUDE.md content (it's already loaded), one-off observations with no actionable consequence.

Useful kinds and tags: `invariant`, `constraint`, `convention`, `gotcha`, `decision`, `incident`, `reference`. Tag liberally — `query_memories tag:"<x>"` is the primary lookup path when you don't know the anchor symbol.

## Required workflow (every task on this repo)

These are not suggestions — run each step at the trigger.

1. Confirm the daemon is up with `index_health` (cheap liveness + scope). Call `graph_stats` only when you actually need node/edge counts or `per_repo` orientation — it returns a large payload and can block during warmup.
2. If `total_nodes` is 0, **call** `index_repository` with `"."` before anything else.
3. In multi-repo mode, **call** `get_active_project` to see scope; use `set_active_project` to switch.
4. Open a non-trivial task with `smart_context` for orientation. For a single known symbol or file, go straight to `search_symbols` / `get_symbol_source` — don't front-load `smart_context` before every read.
5. Immediately after `smart_context`, **call** `surface_memories task:"<task>" symbol_ids:"<top hits>"` to pick up any cross-session invariants / gotchas / decisions anchored to your working set. Skipping this re-derives knowledge other agents have already recorded.
6. Before editing a file, **call** `get_editing_context` on it first.
7. Before changing any function signature, **call** `verify_change` to catch broken callers and interface implementors (cross-repo).
8. For any refactor, **call** `get_edit_plan` for the dependency-ordered file list, then **`batch_edit`** to apply atomically.
9. Verify with the project's real build/test (`go build` / `go test`). Reserve `check_guards` for guard-relevant changes and `get_test_targets` (includes cross-repo tests) to find the tests covering a substantive change — not mechanically after every edit.
10. Before committing, **call** `detect_changes` for scope and `diff_context` for graph-enriched review.
11. When the task is done, if you used `smart_context`, optionally **call** `feedback action: "record"` to score which suggestions were useful / not needed / missing — it improves future context quality. If the task surfaced a durable invariant / decision / gotcha worth teaching the team, **call** `store_memory` so the next agent inherits the lesson.

# Engineering Standards — follow these on every task

## Core principle
Write the simplest code that solves the current task. You are optimizing
for a senior engineer reading this diff, not for hypothetical future
requirements.

## Abstraction rules (IMPORTANT)
- **Single-use code stays inline.** If logic is used in exactly one place,
  do NOT extract it into a shared util, helper, hook, or component. Local
  and inline is correct for single-use code. A private function within the
  same file is fine if it aids readability — but never move it to a shared
  location.
- **Rule of three:** only extract shared code when the SAME logic appears
  in 3+ places. Two occurrences = leave duplicated with a `// note:` comment.
- **No speculative generality.** No abstract base classes, generic type
  params, config options, plugin systems, or "manager"/"factory"/"provider"
  layers unless the current task explicitly requires them.
- **YAGNI:** implement only what this task needs. No future-proofing,
  no unused parameters, no "while I'm here" additions.

## Reuse rules
- Before creating ANY new function, type, component, or util, query Gortex
  (smart_context / semantic search) to check whether an equivalent already
  exists. Extend or reuse existing code before writing new code.
- Never create a second implementation of something that already exists.
  If the existing one is close but not exact, extend it or refactor it —
  don't fork it.

## File & component structure
- One responsibility per file. If a file mixes concerns (e.g. data
  fetching + rendering + formatting), split it — but only along real
  seams, not into fragments for its own sake.
- Target < 150 lines per component / < 300 per module. Exceeding this is
  a signal to split, not a hard rule — never split into pieces that can't
  be understood alone.
- Follow the existing folder conventions in this repo. Look at how
  neighboring code is organized and match it. Never invent a new
  structure or pattern when one already exists here.

## Dependencies & patterns
- No new dependencies, design patterns, or architectural layers without
  stating the justification in the plan and getting confirmation.
- Prefer boring, direct code: early returns over nesting, plain functions
  over classes where possible, standard library over clever one-liners.

## Self-review before finishing any task
Before declaring a task done, review your own diff and confirm:
1. Nothing was extracted/shared that is only used once
2. Nothing duplicates an existing symbol (verify via Gortex)
3. No abstraction exists that the current task didn't require
4. Files follow existing repo conventions and stay single-purpose
5. The diff is the SMALLEST reasonable change that completes the task
If any check fails, fix it before finishing.
