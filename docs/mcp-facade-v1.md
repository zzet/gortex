# Compact MCP surface specification

| Field | Value |
| --- | --- |
| Status | Implemented |
| Surface/config identifier | `facade-v1` |
| Specification version | `1.0.1` |
| Last updated | 2026-07-12 |
| Replaces | No existing API; this is an additive facade over the legacy MCP tools |

## 1. Summary

Gortex exposes a broad code-intelligence API, but its flat MCP namespace makes clients and harnesses choose among many tools that differ only by operation or target. This specification introduces a small, stable set of **effect-homogeneous domain tools** over the existing handlers.

The compact surface is a compatibility layer, not a rewrite. Existing handlers remain the implementation substrate, existing CLI and HTTP entry points keep their contracts, and legacy MCP names remain available during migration. Every named MCP client receives a complete eager coding loop without depending on deferred-tool promotion or `notifications/tools/list_changed`.

The core workflow becomes:

```text
explore -> search / read / relations -> change(impact; verify a proposed signature)
        -> edit / refactor -> change(detect -> tests / guards / contract)
```

The facade MUST NOT collapse all behavior into one universal tool. Tool names are authorization boundaries in MCP hosts, so operations with different effects remain separate even when their input shapes are similar.

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, and **MAY** in this document are normative requirements.

## 2. Goals

The compact surface MUST:

1. Reduce the initial MCP description and schema cost for every MCP client and Bash-only harness.
2. Remove unnecessary choices such as `get_symbol` versus `get_symbol_source`, or `edit_file` versus `edit_symbol`.
3. Publish an eager surface that supports the complete locate/read/edit/verify loop.
4. Keep authorization, planning-mode, federation, and approval boundaries correct.
5. Use stable facade request shapes and additive response metadata so adding an operation rarely changes a tool schema or its legacy payload.
6. Preserve legacy MCP, CLI, and HTTP behavior throughout a measured migration.
7. Make the effective per-session surface observable and testable across named MCP clients and harnesses.
8. Generate policy, documentation, compatibility aliases, and introspection from one canonical operation registry.
9. Produce privacy-safe telemetry that detects discovery failures, schema inflation, invalid facade calls, and fallback to non-Gortex source access.

## 3. Non-goals

The compact surface does not:

- Rewrite the graph algorithms or existing handler implementations.
- Remove legacy tool names in the release that introduces the facade.
- Replace existing CLI or HTTP entry points; the compact `gortex call` mirror is additive.
- Put every operation schema into a single large `oneOf` union.
- Depend on a model understanding a dynamic tool-promotion protocol.
- Weaken stale-write guards, dry-run support, planning mode, or remote-write restrictions.
- Combine read, local-write, durable control-write, session-write, and external-write operations under one tool name.
- Wrap or normalize legacy handler payloads; facade responses preserve them and add routing metadata only.

## 4. Current-state evidence

The implementation snapshot used for this specification registers **178 tools**. `tool_profile` reports 87 live and 91 deferred tools for the audited core/defer session. The category classifier is best-effort rather than a security boundary, but it illustrates the size and fragmentation of the surface:

| Current category | Tool count |
| --- | ---: |
| Navigation | 28 |
| Other / context | 31 |
| Overlay | 19 |
| Analysis | 15 |
| Edit | 14 |
| Admin | 14 |
| Memory | 13 |
| Subscription | 10 |
| Workspace | 9 |
| Review | 9 |
| Read | 8 |
| PR | 6 |
| Enrichment | 2 |
| **Total** | **178** |

The checked-in `tools/list` budget tests record these reference points:

| Surface | Reference payload |
| --- | ---: |
| Full preset baseline | 289,808 bytes |
| Core preset baseline | 95,060 bytes |
| Agent preset measured payload | 27,883 bytes |
| Agent preset ceiling | 28,200 bytes |

The legacy agent preset has a 20-tool floor, plus the always-kept discovery and introspection tools. Even that curated surface contains several decisions that are implementation details rather than user intent. The full descriptions also repeat the same server guidance for each tool; in one audited agent-host rendering, the repeated common prefix accounted for approximately 66% of the description characters.

The audit also exposed a general discovery failure mode: a server-side preset contained `read_file` while one agent bridge did not expose it as callable. `tool_profile` reported global live state rather than the exact client-visible session surface. The compact surface reduces the number of opportunities for this mismatch, but it also requires direct protocol and bridge-exposure regression tests; consolidation alone is not sufficient.

## 5. Design principles

### 5.1 Consolidate by intent

A caller selects a domain verb and supplies intent, not an implementation handler. High-frequency defaults are inferred from selectors: `read(target={symbol:"Server.addTool"})` selects source, while `edit(target={symbol:"Server.addTool"}, ...)` selects symbol editing. An explicit operation remains available for less common views.

### 5.2 Preserve effect boundaries

Every facade tool has exactly one effect class. If two operations need different authorization, planning, egress, or persistence treatment, they MUST use different facade tools.

This rule intentionally splits:

- Local review from review publication.
- Local deterministic analysis from LLM-backed research.
- Memory reads from memory writes.
- Workspace reads from indexing, tracking, and scope mutation.
- Overlay inspection from overlay-session mutation and disk application.
- Code-action discovery from code-action application.
- Inline generation from writing generated output.

### 5.3 Keep the common schema small

Common, high-frequency fields remain typed in the advertised MCP schema. Rare operation-specific fields live under `options` and are validated from the operation registry. `capabilities` returns the exact schema for an operation when needed.

The server MUST NOT advertise a union of all legacy input schemas on every turn.

### 5.4 Keep ordinary coding static and eager

An agent MUST be able to explore, search, read, inspect relations, edit, refactor, and verify from the first `tools/list`. Ordinary coding MUST NOT require `tools_search`, inline schema extraction, or a `tools/list_changed` notification.

### 5.5 Adapt before deleting

Facade handlers delegate to existing handlers. Compatibility adapters remain until usage and parity gates show that removal is safe, and removal follows the repository's versioning policy.

## 6. Public facade surface

The compact surface defines 21 public tools. The effect is a property of the tool, not merely of an individual operation.

| Facade | Effect | Purpose | Typical operations |
| --- | --- | --- | --- |
| `explore` | read | Task-shaped localization and context assembly | `task`, `context`, `closure`, `outline`, `plan`, `prefetch`, `suggest`, `wakeup` |
| `search` | read | Search by domain | `symbols`, `text`, `files`, `ast`, `artifacts`, `completion`, `winnow` |
| `read` | read | Read a target; file/symbol/batch/artifact defaults are selector-driven | `file`, `source`, `symbols`, `summary`, `editing_context`, `history`, `artifact` |
| `relations` | read | Fixed-purpose graph relationships | `usages`, `callers`, `dependencies`, `dependents`, `implementations`, `overrides`, `hierarchy`, `cluster`, `declaration`, `import_path`, `references` |
| `trace` | read | Advanced traversal, execution, control, and data flow | `call_chain`, `walk`, `path`, `graph`, `flow`, `taint`, `cfg` |
| `analyze` | read | Deterministic graph analysis | `help`, `todos`, `hotspots`, `dead_code`, `contracts`, `architecture`, and the other read-only registered kinds |
| `ask` | read, open-world | LLM-backed research | `research` |
| `change` | read | Change detection, diagnostics, impact, planning, preview, and verification | `contract`, `detect`, `diagnostics`, `impact`, `preview`, `simulate`, `verify`, `guards`, `tests`, `code_actions`, overlay comparisons |
| `edit` | local write | Textual/file mutation and application of generated or overlay content | `file`, `symbol`, `write`, `batch`, `scaffold`, `docs`, `skill`, `wiki`, `export_graph`, `apply_overlay` |
| `refactor` | local write | Semantic or LSP-backed mutation | `rename`, `move`, `inline`, `delete`, `apply_code_action`, `fix_all` |
| `review` | read, open-world | Review and review context | `run`, `pack`, `critique`, `diff_context`, `sibling_context`, `pr_context`, `questions` |
| `publish_review` | external write | Publish review results to a forge | `post` |
| `pr` | read, open-world | Read and analyze forge pull requests | `list`, `impact`, `risk`, `triage`, `conflicts`, `reviewers` |
| `recall` | read | Read notes, memories, and notebooks | `memories`, `surface`, `notes`, `notebook_find`, `notebook_list`, `notebook_show`, `onboarding`, `distill` |
| `remember` | local write | Store or update durable knowledge | `memory`, `note`, `edit_memory`, `rename_memory`, `notebook`, `notebook_used`, `risk_ack`, `suppress_finding` |
| `workspace` | read | Inspect repositories, projects, scopes, index, and proxy state | `info`, `repos`, `project`, `scopes`, `index`, `graph`, `active_project`, `proxy` |
| `workspace_admin` | control write | Mutate durable repository, project, scope, index, enrichment, feedback, or persisted analysis state | `track`, `untrack`, `index`, `reindex`, `set_active_project`, `save_scope`, `delete_scope`, `enrich_churn`, `enrich_releases`, `blame`, `coverage`, `sql_rebuild`, `temporal_verify`, `feedback` |
| `overlay` | session write | Mutate speculative overlay sessions without writing to disk | `register`, `push`, `delete`, `drop`, `keepalive`, `fork`, `switch`, `drop_branch`, `simulate`, `merge` |
| `session` | session write | Mutate connection-lifetime planning, workflow, proxy, navigation, coordination, or notification state | `planning_mode`, `workflow`, `cursor`, `proxy_enable`, `proxy_disable`, `agents`, `subscribe`, `unsubscribe` |
| `response` | read | Re-cut or export buffered tool responses | `stats`, `grep`, `slice`, `peek`, `export_context` |
| `capabilities` | read | List public domains/operations or return an operation schema | `detail=summary`, `detail=schema` |

### 6.1 Effect rules

The canonical effect classes are:

```text
read
local_write
session_write
control_write
external_write
```

The following invariants apply:

- A facade MUST resolve to one effect class across all of its operations.
- Effects describe externally observable, authorizable behavior. Bounded memoization that cannot change a result's semantics is not an effect; persisted verdicts, graph enrichment, durable memories, and session mutations are effects.
- The effect describes mutation posture. Open-world behavior is represented independently by the MCP tool annotation: `ask`, `pr`, and `review` are read operations with `openWorldHint`; `publish_review` is both an external write and open-world.
- Read operations MUST NOT persist semantic state. Mutations route through `local_write`, `session_write`, `control_write`, or `external_write` facades. `recall(operation="surface")` therefore forces `mark_accessed=false`; explicit legacy calls retain their historical access-counter behavior.
- `blame`, `coverage`, `sql_rebuild`, and `temporal_verify` persist graph or cache state. The compact surface MUST reject all four under `analyze` and expose them only through the matching `workspace_admin` operation.
- Read-only adapters MUST neutralize optional effects without changing legacy defaults: `search.symbols` fixes `assist=off`; `analyze.concepts` fixes `use_llm=false`; `analyze.co_change` fixes `refresh=false`; native `impact` fixes `refresh_cochange=false`; native `sql_call_sites` fixes `materialize=false`.
- `change.contract` fixes `ack=false`; durable risk acknowledgement uses `remember(operation="risk_ack")` with `ack=true` fixed by the server.
- Forge reads MUST route through `pr`; forge writes MUST route through `publish_review`.
- Generation and graph export operations route through the local-write `edit` facade because their legacy handlers can write output. `edit(operation="wiki")` forces `enhance=false`, so this local-write boundary cannot invoke an LLM; enhanced wiki generation remains on the explicit legacy compatibility surface.
- `edit(operation="batch")` carries whole-file lifecycle items — `move_file` and `delete_file` — alongside `edit_file` and `edit_symbol`; whole-file relocation and removal are local writes on the `edit` facade, not semantic operations on `refactor`.
- Overlay comparison and preview are reads under `change`; overlay mutation uses `overlay`; applying overlay content to disk uses `edit`.
- Effect-split overlay operations are fixed-safe: `change.simulate` forces `keep=false`, `overlay.simulate` forces `keep=true`, `overlay.merge` forces `to_disk=false`, and `edit.apply_overlay` forces `to_disk=true`. Caller arguments cannot override these values.
- Proxy reads use `workspace`; cursor navigation, agent registry operations, and proxy/planning/workflow changes use `session`. Listing or comparing overlays uses `change`; mutating overlays uses `overlay`.
- Event subscription lifecycle uses `session(operation="subscribe"|"unsubscribe", channel=<channel>)`, separate from durable workspace mutation.
- Planning-mode and federation policy MUST be generated from the effect class instead of a hand-maintained tool-name list.

## 7. Eager surface and client defaults

Every MCP connection with a non-empty `clientInfo.name` defaults to the compact surface in `hide` mode unless a higher-precedence forwarded, operator, or instruction-profile policy selects another surface. Its first `tools/list` advertises exactly all 21 compact names and no legacy tool names. Direct calls to hidden legacy tools are hard-blocked; compact dispatch reaches their captured handlers internally.

The 21 names are static for the connection. `read` is therefore always directly callable, and neither ordinary nor rare facade operations depend on `tools_search`, inline promotion, or `notifications/tools/list_changed`. `capabilities` discovers operation summaries and schemas without changing `tools/list`.

Empty or pre-initialize sessions retain the server default. Client identity and wire format are separate: an unrecognized non-empty client name still gets the compact tools but keeps JSON unless it is independently GCX-capable. `facade-v1` is the stable internal config identifier; the neutral `compact` alias is accepted by operator-facing preset selection. Operators may instead select `agent`, `core`, or `full` when compatibility requires the old surface. The compact contract is closed and ignores `allow`/`deny` deltas so `tools/list` and the hard call gate always agree on the same 21 names. Full precedence is forwarded selection (`GORTEX_TOOLS`/`--tools`) > operator-pinned `mcp.tools` config > active instruction profile > named-client default > server default.

## 8. Common protocol model

### 8.1 Request envelope

Each facade advertises only its high-frequency stable fields. A read request illustrates the common shape:

```json
{
  "target": {"file": "internal/mcp/server.go"},
  "context": {"compress_bodies": true},
  "options": {"offset": 100, "limit": 80},
  "output": {"max_bytes": 20000}
}
```

Every field above is published by `read.file`. Containers are closed per operation, so an envelope is only portable across operations to the extent that `capabilities(...detail="schema")` says it is — `read.file` takes a line window as `options.offset`/`options.limit`, while another operation may spell the same intent differently or not accept it at all.

The dispatcher flattens the advertised `arguments`, `options`, `source`, `context`, `guard`, and `output` objects into the selected legacy handler's arguments. Common top-level fields win where the adapter defines a friendly alias. Operation-specific fields that are not worth mounting in every model turn remain discoverable through `capabilities`.

`operation` is a normalized string rather than an ever-growing schema enum. It MAY be omitted when a selector uniquely determines the common read/edit default. The public description lists common operations, while `capabilities(domain=<tool>, operation=<operation>, detail="schema")` returns both the exact operation-specific public input schema and a `request_shape`. The `request_shape` object is the caller-level argument object: pass its fields directly to the named facade tool, without nesting it under an `arguments` or `params` key. The dispatcher MUST reject unknown values and return the valid operation names. Capability lists use the public keys `domains` and `domain`; compatibility terminology is not exposed to callers.

### 8.2 Target references

When a request supplies `target`, it MUST contain exactly one direct selector:

| Field | Meaning |
| --- | --- |
| `file` | Repository-relative or absolute file path |
| `symbol` | One symbol identifier or name |
| `symbols` | Array of symbol identifiers |
| `query` | Search/query target used by an operation |
| `artifact` | Artifact identifier or path |
| `repo` | Repository selector |

Examples are `{"file":"internal/mcp/server.go"}` and `{"symbol":"Server.addTool"}`. There is no nested `kind`, `path`, or `id` discriminator. The server rejects an unknown selector, a non-object target, or a target containing zero or multiple non-empty selectors. Individual operations may omit `target` and use their other advertised fields instead.

### 8.3 Operation arguments and output

High-frequency fields are direct and typed. In particular, `edit` advertises `match`, `replacement`, `content`, `guard`, `changes`, and `dry_run` at the top level. `refactor` similarly advertises `new_name`, `destination`, and `dry_run`. The adapter translates friendly fields such as `match`/`replacement` into the selected legacy handler's vocabulary.

Cold domain tools accept an `arguments` object; common tools may additionally accept `options`, `source`, or `context`. Repository/project/scope fields use the operation schema returned by `capabilities` and are passed through to the handler. `output` is a stable open object for response shaping such as `format`, `max_bytes`, `limit`, `cursor`, and `fields`; adding another response control does not change the outer tool schema. Cursors remain opaque.

A field that states **which repository or workspace** an operation should act on MUST be refused with `invalid_argument` when the selected operation cannot consume it. Silently dropping it answers about the active repository while the caller believes another one was addressed, which is a wrong answer the caller cannot detect, and on a write operation it is a write to the wrong repository.

The rule applies at the top level and in every container, in any letter case, and covers the spellings a caller may reasonably invent as well as the published one: `repo`, `repo_path`, `repository`, `repository_path`, `repo_root`, `repository_root`, `repoPath`, `repo-path`, `repo_dir`, `root`, `cwd`, `dir`, `worktree`, `work_tree`, `base_repo`, `workspace`, `project`. Inventing a name does not make the intent less clear, so it must not make the failure quieter. `path` and `scope` are deliberately excluded: on this surface they name a file or a working-tree scope far more often than a repository.

Refusal is decided per operation by whether the field reaches a reader, not by the name alone — an operation that genuinely consumes `workspace` or `root` still receives it. Most operations consume no repository selector at all and run against the active project; their refusal carries `data.reason = "no_repository_selector"` and names `workspace_admin.set_active_project` as the way to change scope. An operation that does publish one names it in `data.suggested_field` — `options.repo` for the common domains, `arguments.repo` for cold domains. `capabilities(...detail="schema")` is authoritative for which selector, if any, an operation accepts.

Fields outside this class that an operation does not consume are still forwarded and ignored. Closing that wider gap requires enumerating every server-side reader — the handler, the response layer, and facade middleware each read from the normalized arguments — and an incomplete enumeration would refuse working calls, so the guarantee here is deliberately limited to fields that name a target.

### 8.4 Response compatibility and metadata

Facade dispatch MUST return the selected legacy handler's `CallToolResult` unchanged: text/structured content, error state, truncation fields, cursors, and existing metadata retain their established shape. The adapter adds only `_meta.gortex_facade`:

```json
{
  "content": [{"type": "text", "text": "<legacy payload>"}],
  "_meta": {
    "gortex_facade": {
      "surface_version": "facade-v1",
      "facade": "read",
      "operation": "file",
      "canonical_tool": "read_file"
    }
  }
}
```

The metadata is additive and is not a mandatory `ok/data/warnings` wrapper. Existing consumers can ignore it. Facade validation failures use the existing Gortex structured-error mechanism and include stable error codes plus relevant data such as `valid_operations` or `valid_selectors`; they do not masquerade as a legacy handler payload.

### 8.5 Representative calls

Read a file without selecting a legacy reader:

```json
{
  "operation": "file",
  "target": {"file": "internal/mcp/server.go"},
  "context": {"start_line": 100, "end_line": 180}
}
```

Read one symbol's source:

```json
{
  "target": {"symbol": "Server.addTool"},
  "context": {"context_lines": 3}
}
```

The result is the existing `get_symbol_source` payload, including location, signature, and the metadata that handler already returns. The narrower legacy `get_symbol` metadata-only view is intentionally not a public operation.

Preview a guarded textual file edit:

```json
{
  "target": {"file": "internal/mcp/server.go"},
  "match": "old text",
  "replacement": "new text",
  "guard": {"base_sha": "observed-blob-sha"},
  "dry_run": true
}
```

Write complete file content:

```json
{
  "target": {"file": "docs/example.md"},
  "content": "# Example\n",
  "dry_run": true
}
```

Preview one atomic batch that moves a whole file and deletes another:

```json
{
  "operation": "batch",
  "changes": [
    {"op": "move_file", "source": "internal/mcp/old_name.go", "destination": "internal/mcp/new_name.go"},
    {"op": "delete_file", "path": "internal/mcp/obsolete.go"}
  ],
  "dry_run": true
}
```

`move_file` and `delete_file` operate on whole files of any language and
rewrite no callers; `refactor` remains the semantic path for one symbol
(`refactor move` is Go-only). `dry_run` returns the resolved plan without
writing — a lifecycle entry carries `resolved_path` and `source_state`, a
`move_file` entry adds `resolved_destination` and `destination_state`, and a
path state is `{exists, kind, git, ignored}` plus `sha256` on a regular-file
source. A lifecycle precondition or the aliasing rule reports `status:
"conflict: …"` and an argument failure reports `status: "failed: …"`, with the
top-level `conflicts` counting both; content edits are not checked against disk
until the locked commit. `transaction_id` makes the applying call idempotent
under a lost response; under `dry_run` it is inert.

Preview a semantic refactor:

```json
{
  "operation": "rename",
  "target": {"symbol": "oldName"},
  "new_name": "newName",
  "dry_run": true
}
```

Detect a working-tree change independently of how it was applied:

```json
{
  "operation": "detect",
  "source": {"base": "HEAD"}
}
```

Ask for the impact of one known symbol with the same selector shape used by
`read` and `relations`:

```json
{
  "operation": "impact",
  "target": {"symbol": "Server.addTool"}
}
```

`target.symbols` is the batch form. `change` translates both forms to the
captured impact handler's identifier list; callers do not need to know its
legacy `options.ids` spelling.

The `change` facade applies equally to mutations made through `edit`, `refactor`, `apply_patch`, an IDE, or another tool. After mutation, run `detect` and use its symbol IDs with `tests`, `guards`, and `contract`. Run `verify` before a signature mutation with the proposed signature. Use `capabilities` when an exact operation schema is needed.

## 9. Capability discovery

`capabilities` replaces public-client dependence on `tool_profile` and `tools_search`.

All 21 facade tool names are already present in `tools/list`. `capabilities` discovers operations and their schemas; it MUST NOT introduce, promote, or dynamically register tool names.

Its stable arguments are:

- `domain`: facade name; omit it to list all facade domains.
- `operation`: operation name; omit it to list the domain's operations.
- `detail`: `summary` (default) or `schema`.

Fetch the exact public operation schema for reading a file:

```json
{
  "domain": "read",
  "operation": "file",
  "detail": "schema"
}
```

The result includes `surface_version`, `operation`, `effect`, `available`, and `summary`; schema detail additionally includes the operation-specific public `input_schema`, an actionable `request_shape`, fixed server arguments, and `schema_hash`. Unified analysis schemas are filtered by kind and express conditional requirements such as coverage profiles and cycle endpoints. Availability reflects whether the underlying handler was registered in this server configuration.

## 10. Canonical operation registry

Facade routing and legacy compatibility are driven by one per-server operation registry. Its implemented contract is equivalent to:

```go
type facadeOperationSpec struct {
    Facade    string         // stable MCP facade name
    Operation string         // stable request discriminator
    Legacy    string         // captured legacy tool/handler
    Effect    facadeEffect
    Fixed     map[string]any // trusted arguments injected after caller input
}
```

The registry:

1. Facade MCP registration and dispatch.
2. Captures the existing legacy tool definition and handler when it is registered.
3. Supplies operation availability and summaries, then projects captured handler fields into the stable public facade envelope returned by `capabilities`; captured implementation schemas are never returned as the public contract.
4. Supplies facade/operation/canonical-tool metadata and privacy-safe telemetry dimensions.
5. Provides the complete mapping used by coverage, effect-parity, schema-budget, and dispatch tests.

Validation and tests MUST fail when:

- A facade contains operations with different effects.
- Two entries claim the same `(facade, operation)` pair.
- Any registered legacy tool is absent from the mapping; the v1 baseline requires all 178.
- A legacy mutator maps to a read effect.
- The 21-name facade preset and registry disagree.
- An effect-sensitive legacy variant is not made safe with a fixed argument or a separate facade operation.

Fixed arguments are applied after caller arguments and cannot be overridden. They enforce overlay splits; `edit.wiki(enhance=false)`; local search and analysis postures; `change.contract(ack=false)` versus `remember.risk_ack(ack=true)`; normalized native analysis kinds; and the four admin-only analysis kinds. `capabilities` reports them as `fixed_arguments`. Planning, mutation, open-world, and federation tests MUST remain in parity with the registry effects.

## 11. Compatibility and versioning

### 11.1 Surface selection

The server exposes the compact surface under the `facade-v1` operator/config preset alongside the existing legacy presets:

| Selection | Advertised tools | Intended use |
| --- | --- | --- |
| `facade-v1` (`compact`, `facade`, `agent-v2`) | Exactly 21 compact tools in `hide` mode | Every named MCP client and explicitly pinned sessions |
| `agent`, `core`, `full`, and specialist presets | Existing legacy tool definitions and behavior | Explicit compatibility/rollback and empty or pre-initialize server defaults |

Every connection with a non-empty MCP `clientInfo.name` selects the compact surface in `hide` mode when no higher-precedence surface is selected. Empty or pre-initialize sessions retain the server default. `GORTEX_TOOLS` or `--tools` has the highest precedence, so `GORTEX_TOOLS=full` (or another legacy preset) deliberately restores that connection's legacy surface.

Several facade names intentionally reuse strong existing names, including `explore`, `analyze`, `ask`, and `review`. Tool definitions are therefore session-surface-specific:

- In a legacy preset, a reused name advertises and accepts its legacy schema.
- In a compact session, the same name advertises its compact definition and routes through the operation registry (`analyze` retains `kind` as its primary discriminator).
- A connection MUST select its surface before the first `tools/list`, and a name's schema MUST NOT change during that connection.

Legacy clients that require an old colliding schema select a legacy preset. The underlying handler remains the same; only the session-visible definition and adapter differ.

### 11.2 Legacy behavior

- Introducing the compact surface is additive.
- Legacy MCP names continue to accept their existing arguments and return their existing responses.
- Legacy CLI and HTTP routes remain available and continue to delegate to the same handlers.
- Compact `hide` mode advertises no legacy names and hard-blocks direct legacy calls. Its dispatchers call the captured legacy handlers internally without promoting or exposing their schemas.
- An explicit legacy preset restores legacy MCP compatibility for that connection.
- Exact aliases such as `grep_results` and `head_results` are deprecated first; their canonical legacy handlers remain reachable until the compatibility window closes.
- Removing a legacy tool, required argument, or response field follows `docs/versioning.md`. It is a breaking change after Gortex reaches 1.0 and requires the repository's documented pre-1.0 migration treatment before then.

### 11.3 Facade evolution

- Adding an operation or optional field is backward compatible.
- Removing or changing the meaning of an operation requires a new surface version.
- The facade tool names remain unversioned; `capabilities` and `_meta.gortex_facade.surface_version` carry the version.
- `capabilities(detail="schema")` returns a per-operation `schema_hash` derived from the captured legacy input schema.
- Clients MAY cache schemas by `(surface_version, domain, operation, schema_hash)` and invalidate them when the hash changes.

## 12. Named-client and harness discovery contract

An integration is correct only when the server, stdio proxy, client bridge, and session introspection agree on the visible surface for every named MCP client and harness.

The following sequence is normative:

```text
initialize(clientInfo.name = "<non-empty-client-name>")
  -> resolve the compact / hide policy for this session
  -> tools/list returns exactly 21 static compact names and no legacy names
  -> read is present
  -> capabilities(domain="read", operation="file", detail="schema")
     describes read_file without promoting it
```

Requirements:

1. Raw protocol tests MUST initialize with representative canonical client IDs, supported host aliases, and a deliberately unknown non-empty client name, then assert the exact tool roster.
2. Each named-client test MUST assert exactly 21 compact names, `read` included, and zero legacy-name leakage. A separate empty/pre-initialize test MUST assert that the server default is retained.
3. `read(target={file:"..."})` MUST infer the file operation and return source without daemon CLI fallback.
4. `capabilities(domain="read", operation="file", detail="schema")` MUST return the captured schema without promoting `read_file`.
5. A direct hidden legacy call MUST remain blocked while facade dispatch to the same captured handler succeeds.
6. The stdio proxy MUST apply the same per-connection hide policy from the first `tools/list`.
7. The ordinary coding loop MUST work when the client ignores `notifications/tools/list_changed`.
8. Each MCP-capable integration harness SHOULD compare the raw MCP roster with the functions exposed by its client bridge. If the bridge drops a tool, telemetry and diagnostics MUST identify the missing facade rather than recommend an uncallable name.
9. A Bash-only harness with no native MCP function exposure MUST be able to invoke the same compact names and argument objects through `gortex call`; this CLI path is a direct mirror, not a translation to legacy tool names.
10. An agent adapter MUST use the host's supported eager/direct namespace control when the host otherwise defers MCP tools. Discovery in a settings screen or a successful raw `tools/list` is not sufficient: the facade functions MUST be present in the model-visible callable registry on the first turn.
11. An agent adapter MUST NOT make Gortex a hard precondition for the host starting. Startup or discovery failure should fail visibly rather than silently leaving the agent with source-access guidance it cannot follow, but "visibly" MUST stop short of taking the host down: a coding assistant that cannot be started is worse than one running without graph tools. Where a host offers a required-server flag (Codex's `required`), the adapter MUST leave it alone — Gortex has ordinary states, such as a stopped daemon, in which its server does not answer the initialize handshake. Surface that state through `gortex doctor` instead.
12. Guidance installed for an MCP-capable host MUST NOT recommend daemon startup or the Bash mirror when a Gortex callable handle is missing. That state is a host integration failure and MUST be surfaced. `gortex call` remains the mirror only for a harness that genuinely has no MCP transport.

For Codex 0.142.0 and newer, `gortex init` adds the current `mcp__gortex`
namespace and its non-prefixed `gortex` form to
`features.code_mode.direct_only_tool_namespaces` without removing
user-configured namespaces. This is the host-supported bypass for deferral: the
active Gortex namespace remains a direct model tool even when other MCP
namespaces are available only through tool search. Older Codex releases reject
that field, so the adapter version-gates it rather than invalidating their
config.

The adapter also writes a startup timeout long enough for Gortex's bounded
daemon autostart, and pins `command` to the absolute path of the installed
binary. Codex launches the server itself, so the bare name is only resolvable
when Codex inherits the PATH `gortex init` ran under — a GUI-launched Codex
does not.

Releases v0.61.0 through v0.63.x also wrote `required = true`. That was a
mistake: Codex aborts the session outright when a required server fails to
initialize, so a stopped daemon or an unresolvable binary stopped the CLI from
starting at all (gortexhq/gortex#607). The adapter now removes the flag from
its own entry on the next `gortex install` / `gortex init`, and never writes
it. An explicit user `required = false` is preserved. These are transport/host
settings, not agent instructions and not part of the facade request schema.

## 13. Telemetry and privacy

Facade telemetry follows the consent, endpoint, bucketing, and privacy requirements in `docs/telemetry.md`. It remains opt-in and off by default. No query, code, path, symbol, repository name, diff, prompt, response body, or exact high-cardinality identifier may be recorded.

The implementation distinguishes local diagnostic observations from anonymous aggregate telemetry. Any newly transmitted metric key MUST be added to the hard allow-list and documented before release.

Hook-effectiveness observations are local diagnostics, not anonymous aggregate
telemetry: they are written to a separate cache JSONL, are never uploaded, and
contain no command, query, prompt, path, symbol, source, or output text.

At minimum, privacy-safe buckets SHOULD cover:

| Event | Safe dimensions |
| --- | --- |
| Surface initialized | Client family from a fixed allow-list, surface version, live-tool-count bucket, `tools/list` byte bucket, schema hash prefix |
| Facade call | Bounded registered `facade.operation` dimension; overlong values use a deterministic safe suffix |
| Validation failure | Facade, operation, stable error code |
| Discovery | `capabilities` domain/detail, legacy `tools_search` use, promotion success/failure |
| Workflow effectiveness | Calls-to-first-useful-result bucket, direct-read success, verification-after-mutation flag |
| Integration fallback | Gortex read available, Gortex read used, shell/raw-read fallback observed |
| Hook effectiveness | Local-only allow-listed event, emitted-context flag, daemon reachability, capped alternation-segment count, and latency |

Success and regression dashboards SHOULD track:

- Cold `tools/list` bytes and tool count by client family.
- Facade versus legacy call share.
- Invalid-argument rate by facade operation.
- Calls to first useful source/context result.
- Direct MCP source-read success rate.
- Deferred-promotion attempts in compact sessions; the expected value is zero for ordinary coding.
- Shell/raw-read fallback rate when a Gortex read facade is available.
- Mutation followed by `detect`, `tests`, `guards`, and `contract` checks.
- Adapter overhead and end-to-end latency buckets.

## 14. Rollout plan

### Phase 0: establish truthful diagnostics

- Make tool-surface introspection session-aware.
- Add raw `initialize -> tools/list` protocol tests for representative known names, an unknown non-empty name, and an empty/pre-initialize session.
- Capture current tool counts, schema bytes, call sequences, invalid arguments, and legacy usage.
- Complete the effect audit, including code actions, fix-all, memories, overlays, generated files, enrichment, proxy control, and review publication.

### Phase 1: introduce the registry

- Add the canonical operation registry without changing externally visible behavior.
- Generate legacy descriptors, policy classifications, and completeness tests from it.
- Require all 178 audited legacy tools to be mapped or explicitly justified as legacy-only.

### Phase 2: add facade adapters

- Implement facade dispatch as thin adapters over existing handlers.
- Preserve legacy `CallToolResult` payloads and attach `_meta.gortex_facade` routing metadata.
- Add operation-level validation and structured errors.
- Add golden parity tests comparing facade operations with their legacy handler results.

### Phase 3: add compact profiles

- Publish the 21-tool compact surface under the `facade-v1` config identifier, with the 11-tool coding core protected in restricted coding profiles.
- Keep all existing legacy presets available as explicit compatibility/rollback selections.
- Remove repeated common guidance from individual facade descriptions and publish it once as server instructions or a resource.
- Add the permanent `tools/list` byte ceiling.

### Phase 4: canary named clients and harnesses

- Default named MCP clients and harnesses to the compact surface in `hide` mode while preserving explicit `GORTEX_TOOLS` rollback.
- Run task-level evaluations for localization, source reading, editing, refactoring, verification, review, memory, workspace, and overlay workflows.
- Compare success, round trips, invalid arguments, latency, and fallback against the legacy surface.

### Phase 5: stabilize named-client defaults

- Verify representative known client IDs, supported host aliases, and unknown non-empty names against all acceptance gates.
- Keep empty and pre-initialize sessions on the server default.
- Keep explicit legacy rollback configuration.
- Publish migration documentation and deprecation warnings for exact aliases.

### Phase 6: retire legacy advertisement

- Hide unused legacy names by default after the compatibility window.
- Keep CLI/HTTP compatibility longer where it has independent users.
- Remove legacy names only in a release permitted by `docs/versioning.md` and only after telemetry and repository search show negligible use.

## 15. Acceptance criteria

### 15.1 Completeness and parity

- Every one of the 178 baseline tools is represented in the registry; no registered legacy tool is silently excluded.
- No legacy tool maps to more than one effectful operation without an explicit effect split.
- Facade dispatch preserves each legacy handler payload and adds `_meta.gortex_facade` with `surface_version`, `facade`, `operation`, and `canonical_tool`.
- Facade/legacy contract tests cover every mapped operation family.
- Existing legacy MCP, CLI, and HTTP tests remain green.

### 15.2 Context economy

- A default initialization with any non-empty `clientInfo.name` resolves the compact surface in `hide` mode and publishes exactly the 21 tools defined in this specification and no legacy tools.
- Its cold serialized `tools/list` is at most **15,000 bytes**.
- Common server guidance appears once and contributes no more than 10% repeated text across facade descriptions.
- Adding a rare operation does not increase `tools/list` unless its common summary changes.

### 15.3 Agent effectiveness

- Task-level success does not regress against the legacy agent preset.
- Median calls to the first useful source/context result do not increase.
- A known file is readable with `read(target={file:"..."})`; known symbol source is readable with `read(target={symbol:"..."})`. Explicit operations remain valid.
- Target validation accepts exactly one of `file`, `symbol`, `symbols`, `query`, `artifact`, or `repo` and rejects ambiguous/unknown selectors.
- A guarded edit, semantic refactor preview, and post-mutation verification complete without legacy-tool discovery.
- A natural-language localization query cannot have its complete result head monopolized by repeated, same-named data leaves; the highest-ranked leaf remains, while callable/type targets remain discoverable. Literal symbol queries retain their exact-name ordering.
- Invalid-argument and raw-file fallback rates are no worse than the legacy baseline and trend downward during canarying.

### 15.4 Named-client and harness correctness

- Representative known client IDs, supported host aliases, and an unknown non-empty name return the exact expected compact roster from `initialize -> tools/list`.
- An empty or pre-initialize session retains the server default unless an explicit policy selects the compact surface.
- Wire-format negotiation remains independent: an unknown named client remains on JSON unless it is separately GCX-capable.
- `read` is present, directly callable, and can return uncompressed source.
- `capabilities(domain="read", operation="file", detail="schema")` returns an available operation, its operation-specific public `input_schema`, canonical `request_shape`, and `schema_hash`; the shape validates against both that schema and the static `read` facade schema, while captured selector names and fixed server arguments stay out of caller input.
- Direct calls to hidden legacy tools are blocked in the same session.
- The integration passes when `tools/list_changed` is ignored.
- A bridge-exposure regression fails loudly instead of silently recommending a missing tool.
- A Bash-only harness can call the same compact tool names with the same argument objects through `gortex call`, without legacy-name translation.
- Host integrations that support namespace deferral controls expose Gortex directly on the first model turn; an MCP settings screen and raw `tools/list` alone do not satisfy this criterion.
- Host integrations that support required MCP servers fail startup visibly when Gortex cannot initialize or enumerate tools.
- MCP-profile hook guidance reports a missing callable handle as an integration failure and never tells the agent to start a daemon or switch to `gortex call`; Bash-only generated guidance continues to document the CLI mirror.

### 15.5 Safety

- Registry validation proves every facade is effect-homogeneous.
- Planning/read-only mode blocks every write facade.
- Federation and remote routing deny every non-permitted effect by generated policy.
- `publish_review` is the only facade that writes to a forge.
- `edit` and `refactor` retain stale-write guards and dry-run behavior where supported.
- Applying external mutations follows the same `change` pipeline: `detect`, then `tests`, `guards`, and `contract` using the detected symbol IDs. The Codex `apply_patch` PostToolUse adapter runs this pipeline automatically; other harnesses can call the public operations directly.
- `change(operation="impact", target={symbol:...})` and its `target.symbols` batch form produce the same result as the canonical impact handler selector.
- `analyze(kind="impact", target={symbol:...})` ranks that symbol's dependency closure — the target itself plus its transitive dependents — and the target MUST appear in its own result. Its filters narrow the dependents only. Without a target the kind keeps its repo-wide ranking. A target the server cannot resolve MUST return a structured error (`symbol_not_found` / `file_not_indexed`); silently answering with the repo-wide ranking is a fail-open the caller cannot detect.
- A reach-index entry is usable only after its generation and completeness marker are published with all distance tiers. An incomplete or concurrently replaced entry MUST fall back to a live graph walk; repeated identical impact queries MUST NOT decrease to a false-safe zero result while the graph is unchanged.

### 15.6 Observability

- Surface size, facade calls, legacy alias calls, validation errors, discovery use, latency, direct-read success, and fallback are measurable without collecting source identifiers.
- The local, non-transmitted hook-effectiveness JSONL exposes emitted-context rate, daemon reachability, capped alternation count, and latency by allow-listed event without recording commands, source, paths, prompts, or symbols.
- Telemetry remains off by default and its hard allow-list and documentation agree.

## 16. Legacy migration table

This table is grouped by destination operation family. The operation registry is the executable source of truth; tests MUST compare its complete legacy-name set with the registered legacy tool set.

| Legacy tool(s) | Compact-surface destination | Notes |
| --- | --- | --- |
| `explore`, `smart_context`, `context_closure`, `get_repo_outline`, `plan_turn`, `prefetch_context`, `suggest_queries`, `gortex_wakeup` | `explore` | Operations: `task`, `context`, `closure`, `outline`, `plan`, `prefetch`, `suggest`, `wakeup` |
| `search_artifacts`, `search_ast`, `graph_completion_search`, `find_files`, `search_symbols`, `search_text`, `winnow_symbols` | `search` | Operations: `artifacts`, `ast`, `completion`, `files`, `symbols`, `text`, `winnow` |
| `get_artifact`, `get_editing_context`, `read_file`, `get_symbol_history`, `get_symbol_source`, `get_file_summary`, `batch_symbols` | `read` | Operations: `artifact`, `editing_context`, `file`, `history`, `source`, `summary`, `symbols`; selector-driven defaults cover file, symbol source, symbol batches, and artifacts |
| `get_symbol` | hidden compatibility mapping | Public symbol reads use `read.source`, which already includes location and signature |
| `get_callers`, `get_cluster`, `find_declaration`, `get_dependencies`, `get_dependents`, `get_class_hierarchy`, `find_implementations`, `find_import_path`, `find_overrides`, `check_references`, `find_usages` | `relations` | Operations: `callers`, `cluster`, `declaration`, `dependencies`, `dependents`, `hierarchy`, `implementations`, `import_path`, `overrides`, `references`, `usages` |
| `get_call_chain`, `get_cfg`, `flow_between`, `graph_query`, `trace_path`, `taint_paths`, `walk_graph` | `trace` | Operations: `call_chain`, `cfg`, `flow`, `graph`, `path`, `taint`, `walk` |
| `audit_agent_config`, `get_architecture`, `verify_citation`, `find_clones`, `find_co_changing_symbols`, `get_communities`, `contracts`, `get_coupling_metrics`, `get_extraction_candidates`, `analyze`, `audit_health`, `run_inspections`, `list_inspections`, `get_knowledge_gaps`, `lint_file`, `get_processes`, `get_recent_changes`, `replay_episode`, `get_surprising_connections`, `get_untested_symbols`, `why`, `get_churn_rate` | `analyze` | Read operations and native read-only kinds; `help` is the safe default, and effect-sensitive queries carry fixed no-refresh/no-LLM arguments |
| `ask` | `ask(operation="research")` | Optional open-world research handler |
| `api_impact`, `get_code_actions`, `compare_branches`, `compare_with_overlay`, `change_contract`, `detect_changes`, `get_diagnostics`, `get_edit_plan`, `check_guards`, `explain_change_impact`, `overlay_branches`, `overlay_list`, `suggest_pattern`, `preview_edit`, `symbols_for_ranges`, `get_test_targets`, `verify_change`, `simulate_chain` | `change` | Read operations; `simulate` fixes `keep=false`, and `contract` fixes `ack=false` |
| `overlay_merge`, `batch_edit`, `generate_docs`, `export_graph`, `edit_file`, `scaffold`, `generate_skill`, `edit_symbol`, `generate_wiki`, `write_file` | `edit` | Operations: `apply_overlay`, `batch`, `docs`, `export_graph`, `file`, `scaffold`, `skill`, `symbol`, `wiki`, `write`; `apply_overlay` fixes `to_disk=true` |
| `apply_code_action`, `safe_delete_symbol`, `fix_all_in_file`, `inline_symbol`, `move_symbol`, `rename_symbol` | `refactor` | Operations: `apply_code_action`, `delete`, `fix_all`, `inline`, `move`, `rename` |
| `critique_review`, `diff_context`, `review_pack`, `pr_review_context`, `suggested_review_questions`, `review`, `sibling_diff_context` | `review` | Operations: `critique`, `diff_context`, `pack`, `pr_context`, `questions`, `run`, `sibling_context` |
| `post_review` | `publish_review(operation="post")` | Separate external-write authorization boundary |
| `conflicts_prs`, `get_pr_impact`, `list_prs`, `suggest_reviewers`, `pr_risk`, `triage_prs` | `pr` | Operations: `conflicts`, `impact`, `list`, `reviewers`, `risk`, `triage` |
| `distill_session`, `query_memories`, `notebook_find`, `notebook_list`, `notebook_show`, `query_notes`, `check_onboarding_performed`, `surface_memories` | `recall` | Operations: `distill`, `memories`, notebook reads, `notes`, `onboarding`, `surface` |
| `edit_memory`, `store_memory`, `save_note`, `notebook_save`, `notebook_used`, `rename_memory`, `suppress_finding`, `change_contract(ack=true)` | `remember` | Local-write operations for durable knowledge; risk acknowledgement uses `operation="risk_ack"` with fixed `ack=true` |
| `get_active_project`, `graph_stats`, `index_health`, `workspace_info`, `query_project`, `proxy_status`, `list_repos`, `list_scopes` | `workspace` | Read operations: `active_project`, `graph`, `index`, `info`, `project`, `proxy`, `repos`, `scopes` |
| `delete_scope`, `enrich_churn`, `enrich_releases`, `feedback`, `index_repository`, `reindex_repository`, `save_scope`, `set_active_project`, `track_repository`, `untrack_repository` | `workspace_admin` | Durable control-write operations for workspace, graph, configuration, feedback, and enrichment state |
| `analyze` kinds `blame`, `coverage`, `sql_rebuild`, `temporal_verify` | matching `workspace_admin` operation | The kind is fixed by the server; all four are rejected under read-only `analyze` because they persist graph or cache state |
| `overlay_delete`, `overlay_drop`, `overlay_drop_branch`, `overlay_fork`, `overlay_keepalive`, `overlay_push`, `overlay_register`, `overlay_switch`, `simulate_chain`, `overlay_merge` | `overlay` | Session-write operations; `simulate` fixes `keep=true` and `merge` fixes `to_disk=false`. Disk application remains `edit(operation="apply_overlay")` |
| `nav`, `agent_registry`, `set_planning_mode`, `workflow`, `proxy_enable`, `proxy_disable`, `subscribe_daemon_health`, `unsubscribe_daemon_health`, `subscribe_diagnostics`, `unsubscribe_diagnostics`, `subscribe_graph_invalidated`, `unsubscribe_graph_invalidated`, `subscribe_stale_refs`, `unsubscribe_stale_refs`, `subscribe_workspace_readiness`, `unsubscribe_workspace_readiness` | `session` | Session-only state. `nav` becomes `cursor`; notification calls use `subscribe`/`unsubscribe` plus `channel` |
| `export_context`, `ctx_grep`, `ctx_peek`, `ctx_slice`, `ctx_stats` | `response` | Operations: `export_context`, `grep`, `peek`, `slice`, `stats` |
| `grep_results`, `head_results` | `response` compatibility operations | Exact legacy aliases retained as `grep_compat` and `head_compat` mappings |
| `tool_profile`, `tools_search` | `capabilities` compatibility mappings | Recorded as `legacy_profile` and `legacy_search`; compact clients use `domain`/`operation`/`detail` without promotion |

Any registered legacy name absent from this table is a specification defect unless its registry entry is explicitly marked `legacy_only` with a rationale and removal plan.

## 17. Revision history

| Version | Date | Change |
| --- | --- | --- |
| `1.0.1` | 2026-07-12 | Required direct host exposure where supported; projected every capability schema into the public envelope; normalized `change.impact` selectors; made repeated impact results safe against incomplete reach publication; kept natural-language explore results diverse; and made missing MCP handles fail visibly instead of triggering a CLI fallback |
| `1.0.0` | 2026-07-12 | Generalized the default across named MCP clients and Bash-only harnesses; added selector defaults, exact CLI mirroring, per-operation schemas, and audited effect splits |
| `1.0.0-draft.3` | 2026-07-11 | Replaced the notification-only facade with the broader session-only `session` facade and kept durable mutation under `workspace_admin` |
| `1.0.0-draft.2` | 2026-07-11 | Aligned selectors, operation calls, capability discovery, response metadata, and hide-mode behavior with the implementation |
| `1.0.0-draft.1` | 2026-07-11 | Initial compact-surface design based on the 178-tool audit and a client discovery investigation |
