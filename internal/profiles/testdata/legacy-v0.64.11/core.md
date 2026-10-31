## MANDATORY: Use Gortex MCP tools instead of Read/Grep/Glob

A Gortex daemon is configured machine-wide via the `gortex` MCP server. On indexed source (any repo it tracks — check `gortex daemon status`), you MUST prefer graph queries over file reads. Hook posture is configurable; follow every Gortex hook instruction even when raw file tools remain callable.

For every coding task:

1. Named-file read/review/summarize: first new-task call `read(operation:"file", target:{file:"<path>"}, options:{new_user_task:true})`; do not localize. To discover a file, symbol, or evidence, call `explore(operation:"localize")` and obey `completion.required_action`; after `answer_ready`, answer from `completion.final_response` and stop. For diagnosis or modification, call `explore(operation:"task")`.
2. In a diagnosis/change flow, make at most one follow-up on one unresolved symbol with `search`, `read`, `relations`, or `trace`; then step 3. Never reopen indexed source with Read/Grep/Glob or shell equivalents.
3. Before mutation, call `change(operation:"impact")`; for a signature change, also `change(operation:"verify")` with the proposed signature. Mutate only with `edit` or `refactor`. After mutation, call `change(operation:"detect")`, then its symbol IDs with `change(operation:"tests")`, `change(operation:"guards")`, and `change(operation:"contract")`.
4. Call `capabilities` only for an operation's exact fields.

Example: `search(operation:"symbols", query:"<name>")`.

If the Gortex server is configured but these tools are missing from the callable MCP tools, report a Gortex MCP integration failure and stop. Do not start a daemon or fall back to a CLI/shell. Outside a Gortex-tracked repo, or when the user says to proceed without Gortex, say so once and continue with native tools.

Use `recall` before revisiting prior work; `remember` immediately for a durable decision, invariant, constraint, or gotcha.

## Worktree and branch routing

- The session/CWD selects the view. A session-discovered checkout (a linked worktree included) is an automatic overlay; do not `track` it. Only an explicit user request to track creates a dedicated logical graph.
- **Overall** means the selected overlay plus exactly one designated primary in its Git family, never a union of incompatible branches. Unique overlay/base matches keep normal relevance order; for a duplicate identity the overlay wins, and deletion/tombstone masks hide the base copy.
- Freshness `exact:false` = the requested view was not served. Every fallback is read-only. `require_exact:true` refuses substitution; `require_fresh:true` awaits filesystem state under RFC3339 `wait_deadline`, routed automatic checkouts only; else `fresh:false`.
- Select explicitly when CWD is not the target: `view:{kind:"worktree",checkout_id:"…"}` or `view:{kind:"git_ref",value:"refs/heads/release",graph_id:"…"}`. Inactive ref/commit views are immutable committed structural/source snapshots: no working-copy LSP, `search.text`, or edits. Approved coordinator-backed exact worktree edits are supported; fallback and ref/commit views are not.
- Untracking a non-primary dedicated worktree demotes it to automatic only if another ready primary survives. Before primary closure, family forget, or `set-primary`, preview the effects and obtain user confirmation.
Never restart the daemon, delete its store, or re-track merely to expose a worktree.

## Reference and discovery

- **`gortex://guide` resource** (or the `gortex guide [topic]` CLI) is the full reference: LLM providers, capabilities, analyze / search_ast catalogs, token economy, MCP resources, session-start checklist. Read it on demand.
- **`capabilities`** — request an exact operation schema only when the compact tool description is insufficient; ordinary coding needs no tool discovery.
- MCP startup manages daemon availability; do not start a daemon manually. "cwd is not covered by any tracked repo" means graph tools are unavailable there: use native tools.
- **Instruction profiles** — this block is the active `core` profile. `gortex instructions list` shows the others (`core` balanced · `localization` lean · `full` maximum); switch with `gortex instructions switch <name>` — applies to NEW sessions only (instructions, tools/list and skills all load at session start).
