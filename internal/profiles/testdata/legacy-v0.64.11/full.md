## MANDATORY: Use Gortex MCP tools instead of Read/Grep/Glob

A Gortex daemon is configured machine-wide via the `gortex` MCP server. On indexed source (any repo it tracks — check `gortex daemon status`), you MUST prefer graph queries over file reads. Hook posture is configurable; follow every Gortex hook instruction even when raw file tools remain callable.

For an explicitly named file to read/review/summarize, on the first direct file read caused by a new user request call `read(operation:"file", target:{file:"<path>"}, options:{new_user_task:true})`; do not start localization. When the file, symbol, or evidence must be discovered, call `explore(operation:"localize")` and obey `completion.required_action`; after `answer_ready` answer from `completion.final_response` and make no further tool calls. For diagnosis or requested changes, call `explore(operation:"task")`, make at most one focused follow-up, then proceed to impact, edit, and test.

| Instead of...                       | Use...                                   |
|-------------------------------------|------------------------------------------|
| Explicitly named file to read / review / summarize | First new-task read: `read(operation:"file", target:{file:"<path>"}, options:{new_user_task:true})` |
| Unknown file / symbol / evidence / "where is X" | `explore(operation:"localize")`; obey `completion` |
| Diagnosing or changing code           | `explore(operation:"task")`, then impact/edit/test |
| `Grep` / `grep` / `rg` for a symbol | `search_symbols` (BM25 + camelCase-aware) |
| `Grep` for references               | `find_usages` (zero false positives)     |
| Reading / grepping to find callers  | `get_callers` / `get_call_chain`         |
| `Glob` over source files            | `get_repo_outline` / `search_symbols`    |
| `Read` a file for one symbol        | `get_symbol_source` (`compress_bodies:true` for the signature only) |
| `Read` to understand a file         | `get_file_summary` / `get_editing_context` |
| `Read` a non-indexed / raw file     | `read_file`                              |
| `Read` several symbols' bodies at once | `batch_symbols` (one call, many bodies) |
| Multiple reads to explore a task    | `smart_context` (one call)               |
| `Edit` / `Write` source             | `edit_file` / `write_file` / `edit_symbol` / `rename_symbol` / `batch_edit` |

**Native MCP required:** if a configured Gortex tool is missing from the callable MCP tools, report a Gortex MCP integration failure and stop. Do not start a daemon or use a CLI/shell fallback. Outside a Gortex-tracked repo, or when the user says to proceed without Gortex, say so once and continue with native tools.

Graph queries *narrow scope*; they do not replace reading the implementation. For the symbol you are about to change or depend on — especially behavior-critical code (migrations, retry / fallback / error-recovery paths, concurrency, compatibility shims) — read the real body with `get_symbol_source` and do NOT pass `compress_bodies:true`, which elides the branches that carry the risk. `format:"gcx"` (compact wire, ~27% fewer tokens) and `compress_bodies:true` (body-eliding) exist on the read / list tools; the parameter legend is in the MCP server instructions.

## MANDATORY: Session + development memory

The graph remembers code; these tools remember **why you made a call**. They are behavior-critical — run each at its trigger, not "optionally":

- **Session start / after a compaction** — call `distill_session` first: prior top symbols, pinned notes, decisions, recent excerpts. Seed your mental model before reading any file.
- **Immediately after `smart_context`** — call `surface_memories task:"<task>" symbol_ids:"<top hits>"`: cross-session invariants / gotchas / decisions anchored to your working set. If it returns nothing, don't probe further.
- **At every decision** — pick an approach, reject an alternative, hit a non-obvious constraint, or commit to an invariant → `save_note tags:"decision" body:"<what+why>"`. Mention symbol IDs (`pkg/foo.go::Bar`) in the body for auto-linking; `pinned:true` for load-bearing notes.
- **When you learn a durable fact worth teaching the team** — `store_memory kind:"<invariant|gotcha|convention|decision|constraint|incident>" body:"<what+why>" symbol_ids:"<id>" importance:5`. `save_note` is the per-session scratchpad; `store_memory` is the workspace-wide store every future agent inherits. Supersede a stale memory with `supersedes:"<old-id>"`.
- **Before editing a symbol you've touched before** — `query_notes symbol_id:"<id>"` / `query_memories symbol_id:"<id>"` surface prior decisions and "do not change this without …" warnings.

**Save / store:** decisions, non-obvious constraints, invariants, follow-ups, incident learnings, bug reproductions. **Skip:** play-by-play the diff already shows, anything derivable from the graph, content already in CLAUDE.md.

## Worktree and branch routing

- The session/CWD selects the view. A session-discovered checkout (a linked worktree included) is an automatic overlay; do not `track` it. Only an explicit user request to track creates a dedicated logical graph.
- **Overall** means the selected overlay plus exactly one designated primary in its Git family, never a union of incompatible branches. Unique overlay/base matches keep normal relevance order; for a duplicate identity the overlay wins, and deletion/tombstone masks hide the base copy.
- Freshness `exact:false` = the requested view was not served. Every fallback is read-only. `require_exact:true` refuses substitution; `require_fresh:true` awaits filesystem state under RFC3339 `wait_deadline`, routed automatic checkouts only; else `fresh:false`.
- Select explicitly when CWD is not the target: `view:{kind:"worktree",checkout_id:"…"}` or `view:{kind:"git_ref",value:"refs/heads/release",graph_id:"…"}`. Inactive ref/commit views are immutable committed structural/source snapshots: no working-copy LSP, `search.text`, or edits. Approved coordinator-backed exact worktree edits are supported; fallback and ref/commit views are not.
- Untracking a non-primary dedicated worktree demotes it to automatic only if another ready primary survives. Before primary closure, family forget, or `set-primary`, preview the effects and obtain user confirmation.
Never restart the daemon, delete its store, or re-track merely to expose a worktree.

## Reference and discovery

- **`gortex://guide` resource** (or the `gortex guide [topic]` CLI) is the full reference: LLM providers, capabilities, analyze / search_ast catalogs, token economy, MCP resources, session-start checklist. Read it on demand.
- **`tools_search`** — under this profile the server publishes the full documented dev-cycle preset (~34 workhorse tools) eagerly; the long tail still loads by keyword via `tools_search` (every tool stays callable by name).
- MCP startup manages daemon availability; do not start a daemon manually. "cwd is not covered by any tracked repo" means graph tools are unavailable there: use native tools.
- **Instruction profiles** — this block is the active `full` profile. `gortex instructions list` shows the others (`core` balanced · `localization` lean · `full` maximum); switch with `gortex instructions switch <name>` — applies to NEW sessions only (instructions, tools/list and skills all load at session start).
