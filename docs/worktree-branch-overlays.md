# Worktree and branch graph views

**Status:** Draft specification, decision rounds 3–4 incorporated<br>
**Date:** 2026-08-15<br>
**Scope:** Persistent graph views for Git worktrees and branches<br>
**Audience:** Gortex maintainers, indexer/storage owners, MCP/CLI owners

This document is intentionally both a product specification and an implementation design. Items marked **DECIDED** record the user's approved behavior; decision round 4 (2026-08-15) resolved every previously **OPEN** item, so no open product decisions remain. The words **MUST**, **SHOULD**, and **MAY** are normative.

## Executive summary

Gortex distinguishes explicit user intent from discovered Git state:

- A checkout that the user explicitly tracks owns a **dedicated graph**: a full, independently versioned logical namespace in the shared graph database.
- Another worktree discovered in the same Git repository family is an **automatic checkout view** backed by sparse layers over the family's designated primary graph.
- A checked-out Git commit is an immutable, cacheable **commit layer** over a full graph generation.
- Uncommitted worktree state is a checkout-specific **dirty layer** over its HEAD commit layer.
- Existing unsaved editor buffers remain a session-specific **buffer layer** at the top.
- An inactive ref is ignored unless a user explicitly requests it; that request creates a local-only, read-only commit view on demand.

The resulting order is:

```text
highest priority

  editor buffer layer        session-specific, transient
  dirty checkout layer       checkout-specific, persisted cache
  branch/commit layer        immutable and shareable by tree OID
  dedicated full graph       explicitly tracked, immutable generation

lowest priority
```

A query runs against one coherent, request-pinned `GraphView`. The first layer that owns a file, node, or edge set wins. An explicit deletion stops fallback. Uncovered data falls through to the main graph. Overall search covers only this selected overlay stack plus its main graph; it never unions conflicting branches or graph alternatives.

This is not safely implementable as a small extension to the current in-memory `OverlaidView`. The prerequisite is a request-scoped view abstraction covering graph reads, search, analyzer projections, revision/cache identity, repository roots, freshness, and completeness. Current code has many direct base-store reads, and the existing overlay has verified point/bulk consistency and search gaps.

The data lives in one shared SQLite database. Logical isolation is provided by `GraphID` and immutable `GenerationID` keys plus atomic route pointers, not by creating a database file per graph, worktree, branch, or generation. The final canonical generation-keyed payload schema contains graph tables, a search subfamily, and generation-owned masks/sidecars, with exactly one physical representation for each payload kind; temporary migration shadows are never a second permanent graph model.

Delivery is foundational and staged: first define and enforce the view contract, then add immutable sparse storage, then automatic worktree lifecycle, and finally durable checked-out/on-demand ref caching. Analyzer rollout is capability-by-capability: every producer must either be exact for the selected view or explicitly unavailable/incomplete. LSP parity for inactive refs is post-V1 and is not a V1 gate.

## Recorded decisions

| ID | Decision | Status |
| --- | --- | --- |
| `STORAGE-1` | Dedicated graphs and layers use logical isolation inside one shared SQLite graph store; no database-per-graph or database-per-generation layout. | **DECIDED** |
| `STORAGE-2` | One canonical generation-keyed payload schema contains graph tables, a search subfamily, and generation-owned sidecars/masks; every payload kind has one physical representation, with no permanent legacy-plus-`view_*` duplication. | **DECIDED** |
| `MIGRATION-1` | The vNext composite-key schema may use one temporary shadow store and atomic swap, or a clearly reported cold rebuild when shadow capacity is unavailable. | **DECIDED** |
| `DEDICATED-1` | An explicitly tracked checkout receives a full independently versioned logical graph. | **DECIDED** |
| `INTENT-1` | CLI track, MCP track, manual persisted entries, and explicit project membership are explicit intent. CWD auto-indexing and Git discovery are implicit. | **DECIDED** |
| `BASE-1` | A family has at most one designated primary; every automatic route requires exactly one ready primary. A dedicated-only family may temporarily have no primary after primary loss. | **DECIDED** |
| `BASE-2` | Initial full indexing is anchored to the exact HEAD tree; staged, unstaged, and untracked state is layered above it. A configured ref may advance the base later. | **DECIDED** |
| `INACCESSIBLE-1` | Inaccessibility is not authoritative disappearance for explicit ownership. Exact routing is withdrawn and rebuildable checkout overlays are purged after grace, but explicit intent/config/identity and dedicated full graphs are not forgotten without authoritative removal/untrack. This retention does not extend a disposable automatic checkout identity beyond its availability deadline. | **DECIDED** |
| `REMOVE-2` | After authoritative disappearance/removal grace and reader drain, Gortex forgets the checkout completely: graph, checkout/graph identity, intent/provenance, persisted membership, route, caches, and completed cleanup state are removed. | **DECIDED** |
| `ERASURE-1` | “As if never tracked” means no logical Gortex graph/config/queryable state. Logs, SQLite free pages/WAL, metrics, backups, and snapshots are outside cleanup. | **DECIDED** |
| `PRIMARY-LOSS-1` | Authoritative removal or untrack of the primary erases that primary and every dependent automatic overlay without electing a replacement; healthy independently dedicated graphs are preserved. | **DECIDED** |
| `LAST-PRIMARY-UNTRACK-1` | Live untrack of the last primary is a clearly previewed family forget, not demotion to an impossible base-less overlay. | **DECIDED** |
| `GRACE-ACCESS-1` | Once absence/inaccessibility is confirmed, new exact queries cannot pin the checkout; while the requested checkout identity remains registered during grace, eligible read-only graph/search requests receive a labeled base fallback without dirty/buffer content, while exact/file/edit requests fail and old exact leases alone may finish. | **DECIDED** |
| `BRANCH-1` | Automatically index checked-out worktrees only. Build an inactive ref only on explicit selection/prewarm; V1 view construction never fetches or lazy-fetches objects. | **DECIDED** |
| `BRANCH-2` | Explicit, non-checked-out locally resolvable ref/commit views are included in V1, despite not being implemented end-to-end today. | **DECIDED** |
| `FIDELITY-1` | V1 inactive-ref views implement the named `inactive_ref_structural_v1` profile. LSP/workspace-only capabilities are unavailable and MUST be reported as such; no hidden worktree is created. | **DECIDED** |
| `DIRTY-1` | Dirty views include tracked changes/deletions and non-ignored untracked source/config files, including relevant mode and symlink changes. | **DECIDED** |
| `OVERALL-1` | Overall search is the selected overlay plus its main graph, with no union of conflicting branches/graphs. A higher layer wins only for the same logical symbol or an ownership/tombstone conflict; other visible results compete by relevance. | **DECIDED** |
| `BUILD-1` | A cold build serves a clearly labeled lower-view fallback; a previous ready view may be served with stale/building metadata. | **DECIDED** |
| `PROMOTION-1` | Live non-primary untrack demotes to an automatic overlay over the surviving primary. Primary untrack follows the destructive preview/cascade rules above; last-primary untrack forgets the family. | **DECIDED** |
| `REMOVAL-EVIDENCE-1` | Removal is proven only by explicit `forget`, validated common-directory inventory omission, or a `prunable` record plus positive path/mount evidence; every ambiguous case remains `inaccessible`. | **DECIDED** |
| `AUTOMATIC-GRACE-EXPIRY-1` | An automatic checkout identity is retained only through its availability deadline. Expiry forgets that checkout completely; a stale explicit checkout selector then fails `checkout_inaccessible`, while selector-free/default and explicit-base requests continue against the surviving primary. | **DECIDED** |
| `PRIMARY-DESIGNATION-1` | After primary loss, a new primary requires explicit previewed `set-primary <graph-id>`; Gortex never elects by path, age, or discovery order. | **DECIDED** |
| `PRIMARY-LIVE-IDENTITY-1` | Primary closure forgets dependent automatic checkouts entirely — identity, incarnation, and clocks included; a `no_primary` family holds no durable automatic checkout identities, and later designation observes worktrees as new incarnations. | **DECIDED** |
| `UNTRACK-BLOCKED-1` | Explicit untrack of an accessible non-primary with no different ready primary fails with the exact blocker and rolls back; `intent_change_pending` is reserved for passive config/project reload. | **DECIDED** |
| `SOURCE-DURABILITY-1` | Ready views persist no source bytes. Commit-only file reads use the local Git object database; when required objects are pruned, the file-read capability is atomically withdrawn instead of misreported. | **DECIDED** |
| `REF-CONFIG-TRUST-1` | Inactive-ref indexing is fully offline: source text never goes to a remote provider; vectors use an approved local provider or trusted policy disables them (`disabled_by_config`). | **DECIDED** |
| `SEARCH-AUTHORITY-1` | Composed-view search scoring authority is the canonical `search_*` relations with exact visible-corpus statistics, knowingly reversing PR #527's global-FTS authority; the relations are benchmark-gated in Phase 3. | **DECIDED** |
| `CROSSREPO-1` | Cross-repository structural references are required by `inactive_ref_structural_v1`; per-foreign-repository generation pins and view-keyed bridge generations are V1 scope. | **DECIDED** |
| `API-1` | Selection order: explicit structured `view` selector, then session CWD checkout, then workspace policy, then primary base. | **DECIDED** |
| `REF-SCOPE-1` | Accepted inactive-view selectors: full `refs/heads/*`, peelable-to-commit `refs/tags/*`, already-local `refs/remotes/*`, and exact commit OIDs. | **DECIDED** |
| `BASE-3` | The full base advances only when an explicitly configured `base_ref` advances; automatic density/age compaction is post-V1, after benchmarks. | **DECIDED** |
| `UNBORN-1` | Unborn/orphan HEAD uses an empty committed-tree lower source plus the dirty checkout layer, accepting a dense first overlay. | **DECIDED** |
| `RETENTION-1` | Defaults: 5 GiB of inactive cache per graph, 7 inactive days, and 32 inactive cached tree generations; graph and search bytes are accounted separately and shared payload is counted once. Only currently routed commit generations are excluded from inactive count/byte quotas. Sealed dedicated generations, primary bases, refs, lower-layer references, and active leases remain deletion blockers but do not create another quota-accounting exemption. | **DECIDED** |
| `CACHE-PIN-1` | Checked-out immutable commit reuse is durable catalog state, not process-local ownership. A holder-scoped `(checkout_id, generation_id)` cache pin survives warm daemon restart and denormalizes the generation's `graph_id` for bounded graph-local retention; graph/generation agreement is enforced, dirty generations are never pinned, and logical checkout/graph cleanup removes only the claims in its deletion scope. Every pin deletion durably enqueues generation retirement in the same transaction; re-pin or successful generation deletion clears that handoff. | **DECIDED** |
| `CACHE-IDENTITY-1` | A ready immutable generation's cache identity is independent of checkout/ref/layer/path aliases and includes every content, lower-base, build, configuration, producer, and required-capability input. Stable exact routes perform no claim/lease/pin/route write. | **DECIDED** |
| `MIGRATION-2` | Schema v21 adds checkout commit-cache pins, the durable retirement handoff queue/trigger, and indexes in place, backfills every currently routed commit plus all eligible checkout-owned ready/superseded commit generations before lifecycle retirement runs, and never rewrites or reindexes generation payload. Seed immediately applies retention bounds after the conservative backfill; `user_version=21` is committed only after the catalog migration and integrity checks succeed. | **DECIDED** |
| `ANALYSIS-1` | Analyzer rollout beyond the structural profile is capability-by-capability; every producer is exact for the selected `ViewID` or explicitly unavailable/incomplete. | **DECIDED** |
| `SLO-1` | The proposed latency/storage values are benchmark hypotheses; final targets are approved only after the Phase 3 shared-database prototype. | **DECIDED** |
| `HOOKS-VIEW-1` | A hook/control-socket probe for a file in an undiscovered or grace-period checkout of a known family triggers immediate family reconciliation and fails open until that checkout's view is ready, then enforces normally. | **DECIDED** |
| `TEXT-SEARCH-VIEW-1` | Trigram `search_text` is a filesystem capability of the selected view: worktree views search their own `ViewRoot`; commit-only views report `capability_unavailable`. | **DECIDED** |
| `LSP-SCOPE-1` | Automatic checkouts receive per-checkout workspace/LSP enrichment under a global concurrent-language-server cap with eviction; markers/state key by generation and checkout. | **DECIDED** |
| `PREFIX-1` | A new dedicated worktree graph derives its `RepoPrefix` from the stable worktree administrative name (`<base>@<admin-name>`) with a deterministic offline-reproducible collision rule; existing prefixes are preserved. | **DECIDED** |

## Goals

1. Explicitly tracking any checkout creates an independent full graph identity while that checkout exists.
2. Linked worktrees in an explicitly known Git family are discovered automatically without a separate `track` command.
3. An implicit worktree consumes storage proportional to its changed files plus dependency/invalidation closure, not an unconditional full copy.
4. Queries from an implicit worktree observe exactly that worktree: committed branch state, dirty files, and session buffers, in priority order.
5. Switching to a previously indexed tree selects the same cached immutable generation without reparsing the branch diff, including after a warm daemon restart and across compatible checkout/ref aliases.
6. Cold checked-out or explicitly requested ref indexing is proportional to the exact tree difference and affected closure.
7. Worktree addition, move, inaccessibility, authoritative removal, daemon restart, and rapid remove/re-add are reconciled safely; terminal removal leaves no logical checkout state, while primary loss forgets all dependent automatic checkouts entirely, identities and clocks included.
8. Search, navigation, graph traversal, statistics, and analyzers either reflect the selected view or report an explicit completeness limitation. They MUST NOT silently return base-only data as if it represented the view.
9. Published views are atomic. A query sees one old generation or one new generation, never a partially mutated mixture.
10. Public node IDs, multi-repository workspace behavior, editor-buffer overlays, and explicit config remain compatible across the one-time storage rebuild.
11. All full and sparse generations are logically isolated within one shared database and one final generation-keyed payload schema.

## Non-goals

- Gortex will not create, remove, prune, move, check out, fetch, or otherwise mutate Git worktrees or refs automatically.
- V1 inactive-ref view construction never performs an explicit, automatic, lazy, or promisor-object network fetch. It uses only objects already present in the local object database.
- The first release need not eagerly index every local or remote branch.
- A non-checked-out branch view is read-only for file-editing tools unless it is materialized as a real worktree.
- A sparse representation cannot guarantee small storage for an unrelated-history or nearly wholly changed tree. In that case the correct delta is inherently close to a full graph.
- This feature does not make mutually incompatible branches part of one simultaneous graph. It selects one coherent revision per repository.

## Product semantics

### Explicit intent

An **explicit checkout** has at least one active direct-intent record. Accepted sources are:

- `gortex track <path>`;
- the MCP tracking tool;
- a manually persisted global repository entry;
- a repository added explicitly through project/workspace configuration.

Automatic CWD indexing and Git worktree discovery are implicit. **DECIDED INTENT-1.**

The catalog MUST store every intent source and its provenance independently. Explicitness is the union of all active sources and MUST NOT be inferred from prefixes, in-memory registration, path shape, or one aggregate `origin` field. Removing one source does not demote a checkout while another source still asserts explicit intent. An `untrack` operation that promises demotion must revoke all writable active sources transactionally/journaled or report the sources that still make the checkout explicit.

When ordinary config/project reload observes the last intent source removed, it is an `intent_transition`, never checkout disappearance. A live accessible non-primary checkout enters the safe demotion transaction only when a different ready primary exists. A primary, an inaccessible non-primary, or a non-primary without a different ready primary enters visible `intent_change_pending` and retains its prior ownership state (with exact routing still withdrawn if inaccessible) until CLI/MCP confirmation previews and executes the destructive branch, or an intent source is restored. A config watcher MUST NOT silently perform primary closure/family forget.

### Dedicated graph

A dedicated graph is a full graph with its own stable `GraphID`, node namespace, full base generations, and lifecycle. Normal exact queries do not fall back to another checkout's graph. The missing-grace response is an explicitly inexact, read-only family-primary fallback—not continuation of the dedicated view.

Isolation is logical. Every dedicated graph lives in the shared SQLite graph store and is addressed by `GraphID`/generation keys. The implementation MUST NOT create a physical database per dedicated graph, worktree, branch, or generation. **DECIDED STORAGE-1 / DEDICATED-1.**

A dedicated graph can still use commit, dirty, and buffer layers for its live checkout. “Dedicated” describes the ownership and completeness of its full base, not a requirement to rewrite the full base on every branch switch or unsaved edit.

A dedicated graph is checkout-scoped for lifecycle. If authoritative evidence confirms that its explicit worktree was removed and removal grace expires, or the user explicitly confirms `forget`, Gortex forgets the checkout after active readers drain: it removes the route, `CheckoutID`, dedicated `GraphID`, repo-prefix alias, full/sparse generations and sidecars, every Gortex tracking-intent/provenance record, every Gortex-owned persisted config/project membership that asserts the checkout, and the completed cleanup journal. The terminal catalog state is row absence, not `purged`, `intent_only`, or a tombstone. Reappearance is a new observation with new identities and is automatic unless the user explicitly tracks it again. `untrack` is distinct: a live accessible non-primary checkout demotes to automatic, while primary untrack follows the previewed dependency-closure/family-forget rules. **DECIDED REMOVE-2 / PROMOTION-1.**

This is a logical product-state deletion contract: graph/config/catalog/queryable state is removed. It does not attempt forensic erasure of SQLite free pages/WAL, logs, metrics, backups, or filesystem snapshots. **DECIDED ERASURE-1.**

A failed path access is not proof of disappearance. While an explicit checkout is inaccessible but still declared by authoritative Git/config state, Gortex withdraws its exact route and discards only unvalidated/rebuildable checkout overlays after grace; it retains explicit intent/config, `CheckoutID`, `GraphID`, and sealed dedicated full generations. Retained explicit ownership changes only after authoritative removal/forget or an explicit `untrack` transaction; live non-primary untrack deletes the dedicated ownership but retains the checkout in automatic mode. **DECIDED INACCESSIBLE-1 / PROMOTION-1.**

### Automatic worktree view

A worktree discovered in a known Git family is automatic when its canonical path is not an explicit checkout. It MUST:

- share the selected dedicated graph's logical node IDs;
- have its own stable `CheckoutID` and route;
- select a commit layer for its HEAD;
- have a distinct dirty layer for its filesystem state;
- be indexed and searchable;
- be persisted as a rebuildable cache across daemon restarts;
- enter durable availability grace on confirmed inaccessibility, retaining its identity only until the availability deadline so eligible read-only requests can receive labeled primary fallback;
- reuse the same `CheckoutID` and incarnation if the same checkout recovers before expiry; at expiry, forget the automatic checkout identity, route, clocks, and rebuildable layers after lease drain, so later discovery creates a new incarnation;
- enter `removal_grace` only when automatically observed authoritative Git/path evidence proves that its administrative checkout incarnation disappeared; configuration/intent changes are `intent_transition`s, and explicit `forget` runs its typed administrative transaction without discovery grace;
- treat Git lock status as diagnostic rather than an indefinite-retention exemption;
- never create or remove a global explicit repository entry through discovery itself.

### Branch view

A branch name is a mutable alias. The indexed unit is the resolved immutable commit/tree plus indexer configuration:

```text
CommitLayerKey = hash(
    base_graph_id,
    base_generation_id,
    target_tree_oid,
    indexing_config_hash,
    extractor_version_set,
    resolver_version,
    enrichment_profile,
)
```

Two branch names resolving to the same tree and the same structural indexing fingerprint SHOULD share one commit-layer generation. The resolved commit OID remains on each checkout/ref route and is the response revision; a tree-shared generation's commit field is provenance-only and MUST NOT overwrite alias identity. Commit-sensitive producers such as blame, churn, releases, and history are excluded from the tree-only structural generation; when computed for a view they live under commit-keyed `analysis_generations` (see the table-ownership manifest), and may be shared only when that resolved-revision identity is compatible. Force-updating a branch produces a new alias target; it never mutates the old immutable generation.

### “Overlay takes priority”

Priority means shadowing, not a blanket ranking tier. **DECIDED OVERALL-1.**

- If a layer replaces logical node `N`, all graph and search reads see the highest-layer payload for `N`.
- If a layer deletes `N`, no lower layer may resurrect it.
- Replacing/deleting a file masks lower symbols owned by that file even when their logical IDs changed.
- If a layer says nothing about a node/file/edge-set identity, lookup falls through.
- After masking and deduplication, all remaining visible overlay and main-graph candidates compete in one relevance ordering.
- An overlay-only candidate receives no automatic rank boost merely because it came from the worktree layer.

Thus the overlay wins when both layers contain the same logical symbol or when ownership/tombstone semantics conflict. Unrelated visible results are ranked normally, and lower data cannot leak through a replaced or deleted owner.

## User scenarios

### Implicit worktree discovery

1. The user explicitly tracks `/repo`.
2. Gortex creates dedicated graph `G1` and registers its Git family.
3. The user runs `git worktree add /repo-feature feature`.
4. The family reconciler discovers `/repo-feature` and creates checkout route `C2`.
5. Gortex reuses or builds the commit layer for `feature`, then builds its dirty layer.
6. An MCP session whose CWD is inside `/repo-feature` automatically receives `G1 + feature + C2-dirty + buffers`.
7. No second full base graph is created.

### Promotion to dedicated

1. `/repo-feature` currently has an automatic checkout view.
2. The user explicitly runs `gortex track /repo-feature`.
3. Gortex journals that intent source and samples HEAD plus index/worktree state.
4. It builds full graph `G2` from exactly the sampled HEAD tree and a separate dirty layer for staged, unstaged, and eligible untracked state.
5. It re-samples checkout state, validates the build, and atomically flips routing from the automatic view to `G2`'s checkout view.
6. Explicit intent is durably finalized.
7. The old automatic route and unreferenced generations retire after readers drain.

There MUST be no interval in which the path is routed to two graph identities and no interval in which a failed full build or config write destroys the working automatic view.

### Demotion after explicit non-primary untrack

1. `/repo-feature` has non-primary dedicated graph `G2` and remains a live worktree.
2. `untrack` preflights and revokes every active explicit intent source for that checkout.
3. The reconciler confirms the different family primary, then builds/selects the checkout's commit and dirty layers off-route.
4. Routing flips atomically from `G2` to the automatic view.
5. `G2`'s graph identity and generation rows retire after leases drain; `CheckoutID` remains automatic.
6. If any intent source cannot be revoked or the automatic view cannot be built, nothing flips and the operation reports the exact blocker.

### Primary untrack

1. Before mutation, Gortex shows that untracking the primary will delete the primary graph and every dependent automatic checkout view; healthy independently dedicated graphs are explicitly listed as preserved.
2. The user confirms the destructive preview.
3. Gortex revokes the primary checkout's intent/config sources, withdraws its route and every dependent automatic route, drains leases, and deletes that dependency closure without electing a replacement.
4. Independently dedicated graphs in the same Git family remain queryable but are not silently promoted. The family has no primary and cannot host automatic checkout routes until the user explicitly designates one with `set-primary`. Still-live former-primary and dependent automatic worktrees are forgotten entirely — checkout identity, incarnation, availability/removal clocks, routes, and layers are all removed. While the family is `no_primary`, live worktrees are observed ephemerally only; after a new primary is designated, discovery creates fresh incarnations for them.
5. If this was the last dedicated graph, the operation is a family forget: all remaining family/catalog state disappears after cleanup. The live filesystem checkout is untracked, not automatically rediscovered as a base-less overlay.

**DECIDED PRIMARY-LOSS-1 / LAST-PRIMARY-UNTRACK-1.**

### Branch A to B to A

1. Checkout `C1` currently selects cached generation `A@tree1`.
2. Git changes HEAD to branch B at `tree2`.
3. The coordinator identifies both the symbolic ref and OID, collapses checkout-generated filesystem events, and selects or builds `B@tree2`.
4. Dirty state is recomputed over B.
5. Switching back to A selects `A@tree1` without reparsing it, unless its indexing fingerprint or base generation is no longer valid.

### Inaccessibility

1. A path/read/permission/device failure without authoritative removal evidence starts durable `availability_grace`; it is not authoritative disappearance of explicit ownership.
2. Gortex immediately withdraws exact path/CWD selection for new requests. While the requested checkout identity remains registered during grace, eligible read-only graph/search calls receive a labeled primary-base fallback with no dirty/buffer content; exact, missing-root file, and edit requests fail. Existing exact-view leases may finish.
3. Same-incarnation recovery before expiry cancels grace and revalidates routing.
4. At grace expiry, an automatic checkout enters terminal `forgetting_checkout`: Gortex stops its watchers/builders and deletes its route, `CheckoutID`, incarnation, clocks, and rebuildable commit/dirty/buffer state after leases drain. A stale explicit selector then fails `checkout_inaccessible`; selector-free/default and explicit-base requests remain on the surviving primary. Later discovery creates a new incarnation.
5. An explicitly owned dedicated checkout follows the distinct retention rule: intent/config, checkout/graph identities, and sealed dedicated full generations remain, while rebuildable checkout layers may be discarded. If it owns the primary, its sealed primary can continue as the labeled main fallback; no family cascade occurs.
6. Dedicated recovery rebuilds checkout-specific layers under the retained identity; automatic recovery reuses its identity only before expiry. Lock status is diagnostic, not proof of removal.

### Authoritative disappearance/removal

1. Explicit Gortex `forget`, successfully observed external Git removal, or authoritative Git inventory confirming the checkout record is gone (or prunable together with applicable path/mount evidence) starts removal cleanup. `untrack` of a still-existing checkout follows demotion/primary rules instead; a path access error alone proves neither removal nor untrack.
2. New exact selection is withdrawn with the same read-only fallback rules; same-incarnation recovery during removal grace may cancel an automatically detected removal, but an explicit confirmed `forget` is not auto-cancelled.
3. After grace and lease drain, a removed non-primary checkout is forgotten completely: no route, identity, intent/config membership, graph/layer/search/analysis/LSP data, tombstone, or completed cleanup journal remains.
4. If the removed checkout owns the primary, Gortex deletes that primary and every dependent automatic overlay — including dependent checkout identities, incarnations, and clocks — without replacement. Healthy independently dedicated graphs remain.
5. If no independent dedicated graph remains, the repository family itself is forgotten. Otherwise it remains dedicated-only with no primary/automatic routes until explicit designation.
6. Physical row deletion runs in bounded batches; the journal exists only until crash-safe cleanup succeeds and then deletes itself.

Terminal forgetting is logical graph/config cleanup, not forensic deletion of logs, metrics, SQLite free pages/WAL, backups, or snapshots. **DECIDED INACCESSIBLE-1 / REMOVE-2 / ERASURE-1 / PRIMARY-LOSS-1 / PRIMARY-LIVE-IDENTITY-1.**

## Verified current state

### Implementation audit and performance baseline — 2026-08-26

This section records implementation evidence gathered after the first worktree/branch-view implementation. It does not change the decisions above. It distinguishes the deployed daemon that was measured from the branch source that was audited, because performance claims are invalid when those revisions differ.

#### Revision identity and method

The profiled daemon reported `v0.63.8-152-gce0b5494-dirty+ce0b5494`, while the audited branch HEAD was `1aba704f99ca8f10e40fb2899ab574564e0533d4`. Live timings below therefore characterize the deployed architecture and corpus, not an exact benchmark of that HEAD. Every release and validation run MUST expose and compare daemon revision, client revision, schema version, and benchmark source revision before accepting results.

The audit used daemon CPU/heap/goroutine profiles, structured lifecycle/build logs, current-HEAD call-path and SQL inspection, isolated generated-repository tests, and focused lifecycle/view tests. Broad architecture/coupling/cycle queries repeatedly reached the same approximately 59-second request deadline while the daemon was contended, which corroborates the user-visible symptom but is not itself a microbenchmark.

#### Measured deployed-runtime baseline

| Area | Observation |
| --- | --- |
| Build admission | Thirteen checkout coordinators waited behind one active build for 17–171 minutes; median observed queue age was 63 minutes. |
| Lifecycle teardown | Five lifecycle operations waited in `CheckoutCoordinator.Close` for 17–55 minutes; two were abandoned untrack requests. |
| Sparse generation wall time | Six observed builds took 11.30, 12.93, 13.30, 18.43, 22.53, and 29.24 minutes; median 15.87 minutes. |
| Admission versus parsing | A two-file checkout parse completed in 1.9 ms but retained build admission for 88.114 seconds. |
| Incremental/global passes | One-file derived passes took 252.787 and 583.072 seconds; complete graph patches took 565.412 and 708.390 seconds. |
| Synthesizers | Individual zero- or one-result synthesizers consumed up to 32.252 seconds; value/callback/middleware passes were each about 25–28 seconds. |
| CPU | In one 20.16-second profile, SQLite VM execution was 59.29% cumulative and `Store.queryEdges` was 31.72% cumulative. In another, generation resolution through `MemberMethodsByType` was 44.68% cumulative. |
| Writer/GC pressure | Ten WAL checkpoints were deferred, five were incomplete, one timed out, and three writer-gate failures dropped rebind batches. |
| Process footprint | The daemon used about 591 MiB RSS and 1,916 file descriptors against a roughly 13 GiB store plus 661 MiB WAL. |

The current cold base-index regression fixture remained healthy: 199 nodes, 772 edges, and 10 files indexed in 21.98 ms best-of-four with 8.9 MiB allocated, inside its existing 24.92 ms gate. This isolates the dominant regression to derived-generation scheduling, generation-bound SQL, whole-corpus enrichment, transition/cleanup work, and composed queries rather than basic parsing.

Seven current-HEAD lifecycle/view tests completed successfully but required 36.342 seconds of package execution (51.44 seconds elapsed, about 724 MiB maximum RSS). A five-run profile of the small composed-view fixture spent about 80% of sampled CPU constructing test parser registries, so those fixtures are correctness coverage, not representative performance evidence. Before this audit there were no relevant Go `Benchmark*` functions for coordinators, sparse generations, ref views, mode transitions, generation retirement, or composed search.

#### Current-HEAD behavior already present

Current HEAD already contains the event path from Git topology observation through family reconciliation and a durable grace retry timer. Focused tests for worktree add/remove signals, retry-timer firing, vanished worktree cleanup, availability-grace expiry, and primary-closure retirement pass. Implementation work MUST preserve these mechanisms and prove them under a held/contended build scheduler; it MUST NOT replace them merely because the stale deployed daemon did not exhibit prompt discovery.

#### Confirmed implementation defects

1. **Coordinator lifetime is uncancellable.** The coordinator loop starts cycles with `context.Background()`, while `Close` closes a stop channel and waits without a deadline. A queued or active build can therefore prevent route withdrawal and logical cleanup indefinitely. The existing test that requires close to wait for an in-flight build encodes the wrong terminal behavior and must be replaced.
2. **One global non-preemptive admission lane covers too much work.** Automatic cycles, interactive rehome/demotion, and inactive-ref builds share one active slot. A cycle acquires it before proving that a route needs work. Interactive work can overtake queued background work but cannot cancel/preempt an obsolete active build, and strict priority can starve background work.
3. **Sparse generation construction still invokes a corpus-style pipeline.** A new `Indexer` and normal `IndexCtx` path run global resolution, inference, clone, framework, external/speculative, and enrichment work. Applicability is not screened early enough, so zero-result producers can consume tens of seconds.
4. **Generation-bound SQL does not reliably select generation-leading partial indexes.** Prepared statements commonly express only `view_gen = ?`, while partial indexes are declared with `WHERE view_gen > 0`; SQLite cannot infer the partial predicate for a parameter. Several hot projections also lack measured composite `(view_gen, ...)` indexes. Work can consequently scale with the base store instead of the selected generation.
5. **Context propagation is incomplete.** Closure planning, batched in/out-edge reads, enrichment, global resolution/synthesis, and physical generation deletion include non-context paths. Cancellation at the scheduler cannot make removal prompt until these paths cooperate.
6. **Intent transitions are only partially durable.** Authorization can revoke intent and persist a running transition before synchronous demotion blocks on generation work. Startup recovery resumes cleanup journals but not every incomplete intent transition; repeating the command may be the only recovery. A controller-wide mutex is held across long work.
7. **Physical cleanup competes on the request path.** Dedicated graph retirement and generation payload deletion synchronously traverse configuration, cross-repository/vector state, FTS/sidecars, edges, and nodes. Logical route/state change must be atomic and prompt; bounded lease-aware physical reclamation belongs to a resumable background journal.
8. **Dedicated full graphs are built from the live filesystem.** Promotion/full tracking can bake staged, unstaged, and untracked bytes into the full corpus instead of building exact HEAD and layering dirty state. Dedicated checkouts then bypass coordinator routes and tree reuse. Nonzero active full-base generations are only partly wired because commit construction/materialization still assumes generation zero rather than recursively following `full_base -> commit -> dirty` ancestry.
9. **Tree caching is checkout-local and process-local.** Content identity includes checkout/layer/ref identity, ready catalog generations are not generally adopted across aliases, startup does not hydrate a graph-wide ready cache, and current retention defaults do not meet `RETENTION-1`. The same tree cannot reliably be reused across worktrees, inactive refs, or restart.
10. **Inactive-ref V1 is only partially enforced.** Local ref/OID construction through `GitTreeSource` exists, but the exact `inactive_ref_structural_v1` profile, cross-repository completeness, approved-local-vector policy, and content-key sharing are incomplete.
11. **Search priority and scoring violate `OVERALL-1` and `SEARCH-AUTHORITY-1`.** Source-rank merging gives unique overlay results a blanket advantage after duplicate masking. Symbol/content FTS queries filter returned rows by generation but still use global FTS5 `bm25()` statistics, so hidden/cached/building/retired generations can change selected-view scores. Per-source limiting before masks can also starve valid lower candidates.
12. **Search freshness and capability reporting disagree.** Exact file reads use current checkout bytes, while symbol search waits for a published dirty generation and the checkout trigram cache keys from the last coordinator sample, omitting a newly created untracked path. Composed vector search is explicitly disabled despite generation-keyed vectors. Runtime accepts a structured top-level `view` selector, but published tool/capability schemas do not advertise it.

#### Required benchmark suite and acceptance gates

Implementation MUST add isolated, deterministic benchmarks named or equivalently scoped as follows:

- `BenchmarkViewBuildGateHandoff`: mixed priority, cancellation, queue-depth and starvation cases;
- `BenchmarkCheckoutCoordinatorCycle`: settled no-op, dirty edit, cold switch, cached return, and many-checkout contention;
- `BenchmarkGitWatcherTopologyProbeStableTick` (`files_1`/`files_2000`), `BenchmarkGitWatcherTopologyProbeTransition`, and `BenchmarkGitWatcherBoundedRegistrationByCheckoutSize` (`files_1`/`files_2000`): report attributable descriptors/RSS, inventory/probe work, transition latency, and teardown leakage;
- `BenchmarkSparseGenerationBuild`: 32/512-file PR fixtures and a 4,096-file manual/nightly fixture with closure sizes 0/20/200;
- `BenchmarkRefViewEnsure`: ready reuse, in-flight coalescing, cold local ref, and admission behind other work;
- `BenchmarkCheckoutModeTransition`: promotion/demotion, clean/dirty, and route-fallback interval;
- `BenchmarkRetirePayloadGeneration`: chunk boundaries, graph-heavy and search-heavy payloads, WAL/writer-lock measurements;
- `BenchmarkViewTextCandidates` and `BenchmarkMergeRankedSources`: layer count, duplicate/mask density, and result limits;
- composed symbol/content/vector differential benchmarks against an independently full-indexed equivalent view.

Stable before/after samples use `benchstat` with at least ten repetitions for microbenchmarks; race-enabled correctness runs are separate. Metrics MUST distinguish queue wait from closure planning, parsing/indexing, enrichment, mask/producer publication, route publication, and physical cleanup. High-cardinality checkout/ref identities belong in structured logs, not metric labels.

The initial implementation gates are:

- queued coordinator cancellation below 250 ms and cooperative active-build cancellation below 1 s;
- event-driven worktree discovery/route withdrawal p95 below 2 s, with grace semantics still enforced;
- topology observation remains independent of source-file count and uses at most 128 attributable descriptors for the 2,000-file fixture; repeated dynamic admission, promotion, recovery, and teardown return descriptor/watch registrations to baseline;
- no-op checkout cycles never acquire payload-build admission;
- a one-file derived build scales with changed/affected rows rather than total base-store size; a 100x unchanged-base increase causes at most 2x latency;
- 512-file/one-change derived publication below 5 s p95 and Gortex-sized/one-change publication below 10 s p95 on the benchmark host;
- transition authorization below 250 ms and control-plane writes below 100 ms p95 during bounded GC;
- cached A→B→A selection below 100 ms p95 with zero reparsed files and reuse across restart/aliases;
- deterministic composed graph/search/vector output equal to a separately full-indexed oracle; unrelated hidden generations cause zero identity/order/score changes;
- base-view search p95/allocation regression below 10%, and hot two-layer composed search/vector latency no more than 1.5x the equivalent exact full view;
- no ordinary build/search/control request approaches the 59-second MCP deadline.

A target that proves unrealistic on the agreed benchmark hardware may be revised only by recording the measured evidence and approved replacement here; correctness, truthful freshness/completeness, cancellation, logical-removal, and differential-equivalence gates are not negotiable.

The following behavior was present at the original design audit and constrains the design; later implementation evidence above supersedes statements that describe missing mechanisms now present.

| Area | Current behavior | Consequence |
| --- | --- | --- |
| Shared store | One SQLite graph store contains all tracked repository prefixes. | “Dedicated” currently means a full logical namespace, not a physical database. |
| Worktree detection | [`ResolveWorktree`](../internal/indexer/worktree.go) follows `.git` indirection and `commondir`, correctly distinguishing linked worktrees from submodules. | Reuse this logic, but add stable identity, HEAD/ref, lock, and prunable state. |
| Worktree naming | [`WorktreeInstanceName`](../internal/indexer/worktree.go) creates a special name only for `--as-worktree` or a distinct declared workspace, and only when the path is actually a linked worktree. | Prefix naming cannot represent intent, family, checkout, graph, and layer identity, and it freezes the track-time branch name; see `PREFIX-1`. |
| Full tracking | [`MultiIndexer.TrackRepoCtx`](../internal/indexer/multi.go) fully indexes every registered root. | There is no sparse persistent repository layer today. |
| GC | `GCVanishedWorktrees` scans registered full worktrees on the hourly reconciliation loop and calls `UntrackRepo`. | It does not discover implicit worktrees, misses daemon-down stale entries, is not prompt enough, and mutates in-memory config without persisting it — disk config drifts and vanished worktrees resurrect from disk on restart. |
| Auto-detection | Repository auto-detection was removed entirely (`AutoDetectRepos` and `multi.auto_detect` retired by d8a068db). | Family discovery must be built new; nothing discovers linked worktrees today. |
| CWD routing | `ScopeForCWD` considers registered repository roots. | An undiscovered sibling worktree cannot select a view. |
| Git watcher | [`GitWatcher`](../internal/indexer/git_watcher.go) debounces HEAD changes, diffs old/new SHAs, and mutates the one graph incrementally. | It avoids some full reindexes but has no per-branch cache; A→B→A repeats work. |
| Ref metadata | `RepoEntry.Ref` is used as query/config metadata, not as an indexing or checkout contract. | It must not silently become the new view selector. |
| Editor overlay | [`OverlayLayer` and `OverlaidView`](../internal/graph/overlay.go) replace covered files and fall back to the base. | Useful semantic prototype, but transient, incomplete, and not durable worktree support. |
| Search | The query engine shares the base search provider when only its reader is replaced. | New overlay symbols are not first-class FTS/vector candidates and stale base bundles can leak. |
| Search scoring | PR #527 retired the in-process BM25 index; native FTS5 `bm25()` in the main database is the sole scoring authority, with repo-prefix filtering inside the ranked query. | One global FTS corpus cannot stay authoritative once hidden/cached generations share tables; see `SEARCH-AUTHORITY-1`. |
| Analyzer access | Many MCP/analyzer paths read `s.graph` directly or require `graph.Store`. | A `Reader` wrapper alone cannot make the product view-correct. |
| Lifecycle | CLI/controller, MCP, reload, GC, and auto-index paths differ in watcher, config, scope, search, analysis, and LSP cleanup. | A single reconciler must own all lifecycle side effects. |

### Verified defects that must not become the new contract

The current transient overlay contains these concrete inconsistencies:

1. `GetRepoNodes` can return both base and overlay versions of a re-emitted ID.
2. `AllEdges` and `GetOutEdges` disagree about an unchanged-source edge whose target file is covered but whose target ID is re-emitted.
3. `OverlayLayer.AddNode` replaces its ID map entry but appends duplicate file/name index entries.
4. `Stats`, `RepoStats`, and edge revision data are base-only (documented in-code as deliberate conservatism; this specification reverses that scoping decision).
5. Search can retain already-materialized base candidates and base edge bundles instead of rehydrating through the view.
6. Many analyzers bypass the request-aware reader entirely.

All six were re-verified against the current head; none has a regression test today. The batched edge variants (`GetOutEdgesByNodeIDs`, `GetInEdgesByNodeIDs`) share defect 2's target-filter logic and need the same coverage. These need regression tests before the view abstraction is used for persistent data.

### Lifecycle gaps to close

Today, tracking and removal side effects are fragmented:

| Entry point | Graph | Watchers | Persist config | Invalidate request/analysis state |
| --- | ---: | ---: | ---: | ---: |
| Daemon controller track | yes | add | yes | none (implicit revision staleness only) |
| MCP track | yes | no | yes | yes |
| Daemon controller untrack | yes | remove | yes | none (implicit revision staleness only) |
| MCP untrack | yes | no | yes | yes |
| Config reload | yes | no watcher diff | n/a | none |
| Vanished-worktree GC | remove | no | in-memory only, no disk save | none |
| CWD auto-index | add | no | in-memory only | none |

“None” rows still benefit from implicit graph-revision staleness checks in lazy analysis caches, but perform no explicit session-scope or analysis invalidation. MCP untrack also leaves the removed repository's live watcher attached — exactly the late-fsnotify re-index race the controller path deliberately avoids by detaching first.

The new `CheckoutReconciler` MUST be the only owner of registration, promotion, demotion, publication, watcher attachment, config mutation, search/LSP cleanup, scope invalidation, and GC.

## Identity model

`RepoPrefix` currently carries too many meanings. The following identities MUST be separate:

| Identity | Meaning | Stability |
| --- | --- | --- |
| `RepositoryFamilyID` | One local Git object/ref/worktree family. | Stable only while family state exists; terminal family retirement deletes it. A later explicit track creates a new UUID. |
| `CheckoutID` | One main or linked worktree incarnation. | Based on family plus Git worktree administrative identity; path moves update the route, while terminal purge/reappearance creates a new ID. |
| `GraphID` | One explicitly owned full logical graph identity. | Stable only for the lifetime of that dedicated graph; disappearance cleanup or explicit untrack deletes it rather than retaining an empty identity. |
| `RepoPrefix` | Public logical namespace used in node IDs. | Preserved for compatibility; unique per dedicated graph. New worktree graphs derive it from the worktree administrative name (**DECIDED PREFIX-1**). |
| `GenerationID` | One immutable full or sparse publication and the storage discriminator. | Globally unique within the database; never reused. |
| `LayerID` | Logical commit or dirty-checkout layer. | Stable across its `GenerationID` publications where applicable. Buffer overlays are session state with no `LayerID`/`GenerationID`; they enter a view fingerprint by session and content hash. |
| `ViewID` | Base generation plus ordered layer generations and workspace selections. | Immutable request/cache fingerprint. |

Git remotes cannot identify a local family reliably: local-only repositories, forks, remote changes, and two independent clones can share or lack a URL. The daemon SHOULD generate a family UUID and associate it with a normalized `--git-common-dir` identity. A moved common directory can be re-associated using explicit user intent and repository fingerprints, but automatic heuristic merging MUST be conservative.

For linked worktrees, the administrative directory under the common directory is a better identity than branch or path. The main checkout needs a reserved family-local identity. A removal/re-add race MUST use an incarnation/generation token so an old cleanup task cannot delete a newly created checkout that reused a path or administrative name.

**Incarnation allocation:** an incarnation token is minted exactly once, when a `CheckoutID` is allocated, and is derived from the observed administrative record identity; it never changes for a live `CheckoutID`. “Same administrative incarnation” checks compare this token. The pair `(CheckoutID, incarnation)` exists so a stale saga carrying an old capture can be detected even if a restored database or defect resurrects a reused `CheckoutID`; terminal forgetting deletes the pair, and reappearance allocates a fresh `CheckoutID` with a fresh incarnation.

**DECIDED PREFIX-1:** a newly created dedicated graph for a linked worktree derives its public `RepoPrefix` from the family base prefix plus the stable Git worktree administrative name (`<base>@<admin-name>`), never the checked-out branch — the admin name survives branch switches, while the current track-time-branch naming goes permanently stale on the first switch. Collisions append a short deterministic hash of the administrative identity that is documented and reproducible offline; the current registry-dependent collision suffix cannot be recomputed and must not be carried forward. Already-assigned prefixes are preserved byte-for-byte; nothing renames an existing prefix.

## Base selection

Every automatic layer needs exactly one full base graph generation. A Git family can contain multiple independently dedicated graphs but at most one designated **primary base**. Any family with an automatic route MUST have exactly one ready primary; a dedicated-only family may have none after primary loss. **DECIDED BASE-1.**

1. The first explicit graph becomes primary unless the main checkout or configuration designates another.
2. Other explicit worktrees remain independent dedicated graphs.
3. Automatic worktrees use only the designated primary.
4. Changing the primary explicitly triggers new layer generations and an atomic route flip; it does not reinterpret an existing generation in place.
5. Gortex never silently promotes an independent dedicated graph to primary.
6. After primary loss, a new primary is designated only by explicit previewed `set-primary <graph-id>`: it validates a ready full generation, increments `primary_epoch`, and rebuilds live automatic checkout layers off-route before publishing routes. **DECIDED PRIMARY-DESIGNATION-1.**

If a non-primary dedicated checkout is authoritatively removed, its checkout/graph/intent state is forgotten without disturbing siblings. Mere inaccessibility does not delete a primary or trigger a cascade. If the primary is authoritatively removed or explicitly untracked, Gortex withdraws and deletes that primary plus every automatic route/layer that depends on it — and every dependent automatic checkout identity, incarnation, and clock — without electing a replacement. Healthy independently dedicated graphs survive. While the family is `no_primary`, live worktrees are observed ephemerally and no durable automatic checkout identity is created; automatic routing resumes only after a user explicitly establishes a new primary. **DECIDED PRIMARY-LOSS-1 / PRIMARY-LIVE-IDENTITY-1.**

A live non-primary can demote over the surviving primary. Untracking the primary requires a destructive dependency-closure preview. If it is also the last dedicated graph, the operation forgets the family rather than inventing a base-less automatic overlay. **DECIDED LAST-PRIMARY-UNTRACK-1.**

**`primary_epoch`:** a per-family monotonic counter, initialized when the family's first primary is designated and incremented by every successful `set-primary` and every primary closure. Primary-dependent artifacts — automatic routes, ref views rooted in the primary, cleanup-journal entries — record the epoch they were created under, and sagas refuse to touch state stamped with a different epoch. Checkout routes carry their own `route_epoch`, incremented on every atomic route flip for that checkout (promotion, demotion, primary change, generation publication); a build captures it at start and publication compare-and-swaps against it, exactly as `ref_view_builds.captured_route_epoch` does for ref views.

### Full base revision

A full base generation is pinned to an exact tree and indexing fingerprint. It MUST NOT silently follow a mutable checkout. **DECIDED BASE-2.**

- Initial track publishes the exact current HEAD tree as the first full generation; staged, unstaged, and untracked filesystem state remains visible through the checkout's dirty layer. Unborn/orphan HEAD uses an empty committed-tree lower source plus the dirty layer, accepting a dense first overlay. **DECIDED UNBORN-1.**
- If a `base_ref` is configured, advancement of that ref builds a replacement full generation off to the side. This is the only automatic base movement in V1. **DECIDED BASE-3.**
- Large or long-lived deltas MAY trigger compaction by materializing a newer full generation, subject to post-V1 policy and active dependent layers.
- Old full generations remain until all layer generations and query leases referencing them are retired.

## Graph view contract

### Request pinning

A request MUST resolve one immutable `WorkspaceView` before performing graph, search, path, or analyzer operations. The view contains one selected `RepoView` per participating repository.

Conceptual interfaces:

```go
type ViewID struct {
    BaseGraphID      string
    BaseGeneration  uint64
    Layers           []LayerGenerationRef // bottom to top
    Fingerprint      string
}

type GraphView interface {
    graph.Reader

    ViewID() ViewID
    Search() CompositeSearch
    RootForRepo(repoPrefix string) (string, bool)
    SourceForRepo(repoPrefix string) ContentSource
    RevisionForRepo(repoPrefix string) Revision
    Freshness() Freshness
    Completeness() Completeness

    // Read-side projections currently exposed only by graph.Store.
    NodesByKind(...)
    EdgesByKind(...)
    EdgesByKinds(...)
    // Additional analyzer projections as measured and required.
}
```

Mutable storage APIs MUST be separated from read/analyzer capabilities. An analyzer should not require a mutable `graph.Store` merely to obtain a projection.

The request lease pins all referenced generations. A catalog route may change during the request, but the request continues against its original `ViewID`.

### File ownership

A layer has one of three states for a path:

- `inherit`: this layer has no opinion; fall through;
- `replace`: this layer owns the complete visible file graph;
- `delete`: the path is absent and fallback stops.

A replacement owns all nodes structurally sourced from that file. Re-emitting the same logical node ID replaces its payload. A node present only in a lower version of the covered file is hidden.

Rename correctness does not depend on Git similarity detection. It is always representable as delete old path plus add new path. Rename metadata MAY be retained for diagnostics or cache reuse.

### Node semantics

- `GetNode(id)` returns the highest visible payload for `id`.
- An explicit node/file tombstone returns absent.
- Name and qualified-name lookup merges layers, filters hidden owners, deduplicates by logical ID, and returns only the highest visible payload.
- `AllNodes`, `GetRepoNodes`, and `GetFileNodes` MUST be set-equivalent to point lookup over visible IDs.
- Node IDs MUST NOT gain a branch/worktree suffix. View identity is carried by request context, not by changing the public logical symbol ID.

### Edge semantics

Edges are **source-owned**:

- A layer may inherit or replace the complete outgoing edge set of a source node/file.
- A changed file replaces outgoing structural and resolved edges for its nodes.
- An unchanged dependent file may retain its nodes while replacing its outgoing resolved edges.
- Incoming edges are derived from the same visible outgoing-edge relation. They MUST NOT be composed independently with different rules.
- A base edge from an unchanged source to a replaced target survives when the same logical target ID remains visible.
- The edge is removed when the target is tombstoned or no longer exists.
- `AllEdges` MUST be equivalent to the union of visible point/source reads.

File masks alone are insufficient. Changing a definition can change resolution in an unchanged source file. The layer builder MUST calculate an invalidation closure and write edge-set overrides for affected dependents.

### Produced and global data

Some data is not naturally owned by one source file: cross-repository edges, contracts, clone relations, semantic relationships, PageRank, communities, and other global analyses. Each producer MUST declare one of:

- exact incremental layer support;
- view-keyed recomputation;
- a correctness-preserving composed fallback;
- explicit unsupported/incomplete status.

A tool MUST NOT present base-only producer output as complete for a non-base view.

Artifact, image, and document nodes are file-owned graph payload: they carry a source path, are masked/replaced/deleted by layers exactly like code files, and code→artifact reference edges are source-owned. The `artifacts:` declaration is part of `source.configuration` and is read from the selected source. Their extraction rides the structural producers for files present in the selected tree; capabilities needing external bytes (for example a live database schema) advertise unavailable on non-filesystem views.

### Tombstones and absence

“Not stored in this layer” and “deleted by this layer” are different states everywhere: files, nodes, edge sets, search documents, derived facts, and routes. Implementations MUST use explicit masks/tombstones rather than treating a missing row as deletion.

### Buffer layers

Editor-buffer overlays are session-transient state, not persisted generations: they live with the MCP session, are keyed by content hash, and are never written to the shared database. Where lifecycle or cleanup text mentions deleting buffer state, it means session bindings and in-memory overlay layers; the `generations` catalog has no buffer kind. A buffer layer composes over whatever lower view its session's checkout currently routes to. When the lower commit/dirty generation flips mid-session — a branch switch under an active overlay — the next request recomposes the buffer over the new lower view and re-runs per-file base-drift checks against the session checkout's `ViewRoot`, never against another checkout's file. A drifted buffer file is reported per file (`buffer_base_drift`) rather than silently masking the new lower content. In `ViewID`, a buffer layer contributes a session/content-hash fingerprint component rather than a `LayerGenerationRef`, and the persisted `layers` catalog, like `generations`, has no buffer kind.

## Sparse storage

### Physical design

All dedicated graphs, identities, routes, full generations, sparse generations, masks, and search sidecars live in one shared SQLite graph store. There is no database per graph, worktree, branch, or generation. **DECIDED STORAGE-1.**

The **canonical payload schema** is the fixed set of generation-keyed graph, search, mask, and generation-owned sidecar tables shared by every full and sparse generation. “Single” means one canonical representation per payload kind—no parallel legacy/`view_*`, per-graph, per-view, or per-generation schemas; it does not mean one SQL table. Catalog, route, lease, and composed-view-analysis tables are additional canonical control/derived structures, not alternate graph payload families. Full and sparse generations use the same row codecs and identity rules; masks describe sparse ownership. **DECIDED STORAGE-2 / MIGRATION-1.**

The discriminator MUST be an opaque integer `generation_id`, not a Git “revision” column. A commit SHA is insufficient because the same tree can have different indexing fingerprints, dirty/untracked layers, resolver/enrichment versions, or lower-base dependencies. One global `generation_id` identifies exactly one immutable full or sparse publication and links to its owner/type in the generation catalog.

Public logical node IDs remain unchanged. Storage identity is composite:

```text
(generation_id, logical_node_id)
(generation_id, logical_edge_identity)
(generation_id, repo_prefix, file_path)
```

Generation identity is request/storage context and MUST NOT be appended to public node IDs.

#### Why this is a schema rewrite, not `ALTER TABLE ... ADD revision`

Today's SQLite store assumes one current corpus:

- `nodes.id` is the `WITHOUT ROWID` primary key;
- `edges` is unique on `(from_id, to_id, kind, file_path, line)`;
- `vectors`, clone/constant/enrichment sidecars, and the symbol FTS owner map key by `node_id` alone;
- files/mtimes/freshness key by repo/path alone;
- prepared reads, joins, BFS/analyzer projections, evictions, rebinds, purges, and high-water cursors omit any view identity;
- endpoint joins use `nodes.id = edges.from_id/to_id` and would cross-match generations.

SQLite cannot alter the primary key of a `WITHOUT ROWID` table, and adding a new unique index does not remove the old edge uniqueness constraint. Every affected table and index must therefore be rebuilt. Every read/write/delete/join must run through a generation-bound handle; one missed predicate can leak a building/retired view or destructively delete rows belonging to another generation.

The decided design accepts this cost to obtain one clean final schema. Upgrade first runs capacity preflight, then chooses one reported path. Shadow mode builds one adjacent, uniquely epoch-named SQLite file group (`db` plus its WAL/SHM), never `_vnext` payload tables inside the live database. A durable bootstrap manifest names the active `store_epoch`; new requests bind that epoch, while existing request leases keep the old epoch coherent.

Shadow construction cannot race unrecorded old-epoch writes. Before its consistent snapshot at mutation revision `R0`, migration installs an old-epoch `migration_delta_journal`; every subsequently committed index/catalog mutation increments the monotonic revision and records its affected owners/inputs in the same old-epoch transaction. At cutover Gortex raises a final write barrier, drains in-flight writers, records terminal revision `R1`, replays or deterministically resamples/rebuilds every `R0 < revision <= R1` owner into the shadow, and proves the shadow's applied revision plus route/input fingerprints equal `R1`. Normalized schema/data/query verification runs under that barrier. Only then does one atomic manifest swap direct new requests and writers to vNext; the barrier is released after new-epoch writer binding is confirmed. A changed input or incomplete journal aborts cutover and leaves the old epoch active—no best-effort dual-write is allowed.

The old file group remains only for already-pinned readers, is closed after lease/handle drain, and then its DB/WAL/SHM and migration journal are deleted. Crash recovery trusts the committed epoch: before manifest swap it resumes/abandons the shadow against the still-active old epoch; after swap it resumes old-store cleanup without reopening mixed schemas for new requests. Loss of disk capacity during shadow construction aborts before cutover and never silently changes migration mode.

**DECIDED SOURCE-DURABILITY-1:** ready views persist no source bytes, so migration carries no source-durability preflight and no `migration_waiting_for_source` state. Each generation records only source *metadata* — per-file content hash, mode, and symlink target — which migrates like any other generation-keyed payload.

Capacity preflight has two explicit thresholds. Online shadow requires space for the old epoch, new epoch, peak live WAL/delta journal, verification workspace, and safety headroom. Rollback-preserving cold mode requires space for old plus new epochs and smaller bounded offline verification/headroom; it takes the graph service and writers offline, builds the adjacent new epoch, verifies it, swaps, and reopens. If online capacity fails but this offline threshold passes, cold mode is offered. If neither threshold passes, report `migration_blocked_insufficient_space` and leave the old epoch unchanged and active. V1 never deletes or overwrites the old epoch to make room for migration.

Fresh initialization, shadow migration, and cold rebuild MUST normalize to identical `sqlite_schema`, keys, foreign keys, indexes, search configuration, `user_version`, graph/search results, and query semantics. At steady state exactly one graph SQLite database is active; temporary old/shadow epochs exist only for the accepted migration/drain window. **DECIDED MIGRATION-1.**

### Shared catalog and data model

At minimum, the database needs these logical records (names are provisional):

```text
repository_families(
    family_id, common_dir_identity, display_remote, state,
    primary_epoch, created_at, last_seen
)

tracking_intents(
    intent_id, checkout_id REFERENCES checkouts ON DELETE CASCADE,
    source_kind, source_locator, active, created_at, revoked_at, last_error
)

checkouts(
    checkout_id, incarnation, family_id REFERENCES repository_families,
    root_path, git_dir, state, desired_mode, effective_mode,
    locked, prunable, head_ref, head_commit, head_tree,
    last_accessible, unavailable_since, availability_deadline,
    removal_detected_at, removal_deadline, removal_evidence,
    active_intent_transition_id nullable, last_seen, last_error
)

intent_transitions(
    transition_id, checkout_id UNIQUE REFERENCES checkouts ON DELETE CASCADE,
    cause, prior_desired_mode, prior_effective_mode, requested_mode,
    prior_checkout_state, source_snapshot_hash, state,
    created_at, last_progress, last_error
)

checkout_path_evidence(
    checkout_id PRIMARY KEY REFERENCES checkouts ON DELETE CASCADE,
    root_path_identity, root_volume_kind, root_volume_token,
    nearest_existing_ancestor_path, ancestor_volume_kind, ancestor_volume_token,
    common_dir_volume_kind, common_dir_volume_token,
    sampled_at, sample_generation
)

dedicated_graphs(
    graph_id, owner_checkout_id UNIQUE, repo_prefix, family_id,
    is_primary_base, active_generation_id, state
)

generations(
    generation_id INTEGER PRIMARY KEY,
    owner_kind, graph_id, layer_id nullable, checkout_id nullable,
    generation_kind, base_generation_id nullable,
    lower_view_fingerprint, tree_oid, provenance_commit_oid nullable, config_hash,
    extractor_versions, resolver_version, state,
    covered_files, affected_files, storage_bytes, completeness,
    created_at, published_at, last_selected, error
)

layers(
    layer_id, kind, graph_id, checkout_id nullable,
    target_ref nullable, target_commit, target_tree
)

routes(
    checkout_id, graph_id, commit_generation_id,
    dirty_generation_id, route_epoch, state
)

checkout_commit_cache_pins(
    checkout_id REFERENCES checkouts ON DELETE CASCADE,
    graph_id TEXT,
    generation_id REFERENCES generations ON DELETE RESTRICT,
    last_selected,
    PRIMARY KEY(checkout_id, generation_id)
)

checkout_commit_cache_retirements(
    generation_id REFERENCES generations ON DELETE CASCADE PRIMARY KEY,
    enqueued_at
)

ref_views(
    ref_view_id, graph_id, selector_kind, selector_value,
    desired_ref, desired_commit, desired_tree,
    active_generation_id nullable, active_ref nullable,
    active_commit nullable, active_tree nullable,
    enrichment_profile, desired_build_fingerprint,
    active_build_fingerprint nullable, route_epoch, state, exact,
    last_resolved, last_selected, last_error,
    UNIQUE(graph_id, selector_kind, selector_value, enrichment_profile)
)

ref_view_builds(
    build_id, ref_view_id REFERENCES ref_views,
    desired_ref, desired_commit, desired_tree, base_generation_id,
    enrichment_profile, build_fingerprint,
    generation_id nullable, captured_route_epoch,
    state, build_token UNIQUE, created_at, last_progress, error
)
-- partial UNIQUE(ref_view_id, desired_tree, base_generation_id, build_fingerprint)
-- WHERE state = 'building'

cleanup_journal(
    cleanup_id, opaque_target_ids, reason, phase,
    grace_deadline, primary_epoch, last_progress, last_error
)
```

`checkout_commit_cache_pins` is the durable ownership record for immutable
checked-out commit reuse. It denormalizes `graph_id` from the referenced
generation so retention selection, count, and byte accounting remain bounded to
one graph without a cross-catalog scan. Every insert/update and integrity check
MUST prove `pin.graph_id = generation.graph_id`. There is deliberately no graph
foreign key; graph cleanup is an explicit bounded delete by the denormalized
key. The generation foreign key is restrictive so a generic generation delete
cannot erase a live claim accidentally; deleting a checkout cascades only that
checkout's claims. Promotion, demotion, and rehome revoke that checkout's pins in the old
graph and pin the newly routed compatible commit in the new graph as part of
the authorized transition. Intentional graph/family cleanup first deletes every
pin naming a graph in the authorized deletion closure, then retires payload
after the ordinary reader/lease drain.

`checkout_commit_cache_retirements` is the crash-safe handoff between durable
pin deletion and physical generation retirement. An `AFTER DELETE` trigger on
`checkout_commit_cache_pins` conservatively enqueues the deleted pin's
generation in the same transaction, including checkout-FK cascades and future
deletion paths. Inserting or refreshing a valid pin removes that generation's
queue row in the same transaction. Queue readers are read-only and expose only
rows with no current pin, route, ref-view, lower-layer, or dedicated-graph
owner; the final retirement transaction rechecks every owner and live handoff
lease. Refusal keeps the queue row. Successful re-pin removes it, and successful
generation deletion removes it through `ON DELETE CASCADE`.

Schema v21 is an additive catalog migration over the canonical generation-keyed
payload schema. In one transaction it creates the pin table, retirement queue,
pin-deletion trigger, and graph/age/generation indexes, backfills every current
ready commit route, and
conservatively backfills every eligible checkout-owned ready or superseded
immutable commit generation whose owner checkout and graph still exist. The
migration query itself is not quota-capped: immediately after recovery resumes,
and before ordinary orphan deletion, Seed applies `RETENTION-1` per graph to the
complete conservative set. Ref-owned generations retain their existing ref
holders and gain a checkout pin only when a checkout route actually adopts
them. This catalog-only
`INSERT ... SELECT` never scans, copies, reparses, or rewrites node, edge, file,
mask, search, vector, or sidecar payload. It MUST complete before startup's first
orphan/retirement sweep. Failure rolls the transaction back, leaves schema v20
authoritative, and retries on the next open; `PRAGMA user_version=21` is the
transaction's final durable success marker.

`cleanup_journal` intentionally does not cascade from the rows it is deleting; it is removed last after successful cleanup. A partial unique index enforces one `is_primary_base = 1` graph per family instead of a destructive `family.primary_graph_id ↔ graph.family_id` cascade cycle. Active-generation pointers are nullable/deferred during construction and `RESTRICT` deletion once published. Route, lower-generation, and active-lease references also use `RESTRICT` so bases cannot disappear while selected. Family/checkout ownership uses cascades only where bounded relational deletion is safe; FTS/search, analyzer, LSP, config-file, and cross-repository cleanup remains explicit and integrity-checked.

`ref_views` is the non-checkout routing record; `ref_view_builds` records desired builds separately. Neither can own a `tracking_intent`, checkout, dedicated graph, or global repository-config entry. Explicit selection/prewarm creates only these records plus an evictable immutable generation. On selection, ref resolution computes the full `CommitLayerKey` as `desired_build_fingerprint`; a changed desired target or build fingerprint increments `route_epoch`. An older `active_generation_id` may remain the labeled `exact=false` actual fallback while the desired target builds or fails. Concurrent selection/prewarm of the same `(ref_view_id, desired_tree, base_generation_id, build_fingerprint)` coalesces behind one build token. Publication first compare-and-swap revalidates the current route epoch, target tree, base generation, and complete build fingerprint, including indexing config, extractor versions, resolver version, and enrichment profile. If only the selector's commit/ref metadata changed while resolved tree, base generation, and build fingerprint stayed identical, the publisher MUST adopt the completed immutable content generation under the current route epoch and MUST stamp the active route with the newly resolved commit/ref metadata. Any tree, base, or fingerprint change makes the completion `superseded`; an old commit must never be reported merely because its tree was reusable. Ref movement is re-resolved on next selection and never schedules idle work. `enrichment_profile` is part of reuse identity, so a workspace/LSP-enriched checkout generation cannot masquerade as an inactive-ref structural generation merely because the tree OID matches.

Core payload tables keep their canonical logical names but gain generation-composite keys:

```text
nodes(
    generation_id, id, payload...,
    PRIMARY KEY(generation_id, id)
)

edges(
    row_id, generation_id, from_id, to_id, kind, file_path, line, payload...,
    UNIQUE(generation_id, from_id, to_id, kind, file_path, line)
)

files(
    generation_id, repo_prefix, file_path, payload...,
    PRIMARY KEY(generation_id, repo_prefix, file_path)
)

generation_source_files(
    generation_id, repo_prefix, file_path, content_hash, mode, symlink_target nullable,
    PRIMARY KEY(generation_id, repo_prefix, file_path)
)

file_mtimes(generation_id, repo_prefix, file_path, ...)
repo_index_state(generation_id, repo_prefix, ...)
enrichment_state(generation_id, repo_prefix, provider, ...)
contract_state(generation_id, repo_prefix, ...)
clone_shingles(generation_id, node_id, ...)
constant_values(generation_id, node_id, ...)
semantic_binding_types(generation_id, repo_prefix, file_path, line, name, ...)
ref_facts(generation_id, repo_prefix, from_id, to_id, kind, line, ...)
search_documents(
    generation_id, search_kind, document_id, document_length, payload_hash, ...,
    PRIMARY KEY(generation_id, search_kind, document_id)
)
search_document_owners(
    generation_id, search_kind, document_id, logical_node_id, repo_prefix, file_path, ...,
    PRIMARY KEY(generation_id, search_kind, document_id),
    FOREIGN KEY(generation_id, search_kind, document_id)
        REFERENCES search_documents(generation_id, search_kind, document_id)
)
search_postings(
    generation_id, search_kind, term, document_id, term_frequency, positions, ...,
    PRIMARY KEY(generation_id, search_kind, term, document_id),
    FOREIGN KEY(generation_id, search_kind, document_id)
        REFERENCES search_documents(generation_id, search_kind, document_id)
)
search_statistics(
    generation_id, search_kind, term,
    owned_document_frequency, owned_document_count, owned_total_length,
    PRIMARY KEY(generation_id, search_kind, term)
)
vectors(generation_id, node_id, model_fingerprint, ...)
```

All secondary indexes used by bound reads become generation-leading where required, for example `(generation_id, name, id)`, `(generation_id, file_path, id)`, `(generation_id, from_id, kind)`, and `(generation_id, to_id, kind)`. Exact key order must be benchmarked against the current forced-index query plans. The wider `WITHOUT ROWID` keys and one shared SQLite writer are explicit performance costs of `STORAGE-2` and require before/after cache-density, WAL, build-contention, and query-plan benchmarks.

Prefix-only consumers of `repo_index_state`, freshness, and status counters — the freshness rider, daemon status, `graph_stats` — resolve through the route: checkout/status surfaces read the row belonging to the route's active generation, and family/admin surfaces aggregate across generations only through typed cross-generation administration. The current single per-prefix dirty bit becomes per-checkout state carried by the checkout's dirty layer and route, not a repo-global flag.

**DECIDED SOURCE-DURABILITY-1:** Gortex persists no source bytes. `generation_source_files` is metadata only — the content hash, mode, and symlink target that a generation's masks and fingerprints promise. A full generation maps every admitted source file; a sparse generation maps only files it replaces, with lower generations supplying inherited mappings. File reads on a filesystem-rooted view read the checkout; file reads on a commit-only view read the local Git object database through `GitTreeSource` at request time. If a required object has been pruned, the read fails with `source_object_missing` and the view's file-read capability is atomically withdrawn: the generation stays `ready` for graph/search capabilities, but file-read requests report `capability_unavailable` until a later selection finds the objects present again. Deletion/movement of a Git ref followed by aggressive object pruning can therefore decay a cached view's file reads — by design, reported truthfully rather than silently. Gortex does not create protective Git refs.

Sparse-only ownership needs new tables; payload absence alone cannot express deletion or edge replacement:

```text
generation_file_masks(generation_id, repo_prefix, file_path, ownership_mode, ...)
generation_node_tombstones(generation_id, node_id, ...)
generation_edge_sources(generation_id, source_id, ownership_mode, ...)
generation_producer_completeness(generation_id, producer, state, reason, ...)
```

`generation_source_files` is the sole authority for visible source content hash, mode, and symlink target. `generation_file_masks` contains only path ownership semantics such as replace/delete; it MUST NOT repeat content hashes, modes, or symlink payload. A replacement has a mask plus exactly one source mapping, while a deletion has a mask and no source mapping. Integrity checks reject divergent or orphaned pairs.

#### Search scoring constraint under STORAGE-2

Search is not an exception to `STORAGE-2`'s physical layout. The four illustrated `search_*` relations are normative physical relations replacing the current scoring-authoritative FTS layout; their composite keys/foreign keys are required. One shared FTS5 virtual index MAY exist only as a rebuildable candidate-acceleration index over those canonical rows, with generation-bound ownership. Its global `bm25()` is never a scoring authority.

This knowingly reverses PR #527, which retired the in-process BM25 index and made the main database's global FTS5 `bm25()` the sole scoring authority. That was correct for a single-corpus store; it stops being sound once hidden, cached, building, and retired generations share one FTS corpus, because global document frequencies and lengths leak across views. The reversal is deliberate (**DECIDED SEARCH-AUTHORITY-1**), and the postings/statistics relations are benchmark-gated in Phase 3 before the schema freezes.

`search_statistics` stores additive statistics for documents owned by one graph generation, not mergeable per-layer scores. For a selected `ViewID`, the composer first applies file/document ownership masks, then derives or caches exact visible-corpus statistics under a view-owned analysis generation, and finally scores/ranks the visible documents. Candidate enumeration must either consider every query-matching visible document or use bounds proved safe for that exact composed view. It MUST NOT truncate independently to a generation-local top-k. Hidden, cached, building, and retired generations cannot affect the selected composed-view candidates, normalized scores, order, or top-k.

All graph generations share this one search subfamily. Per-generation FTS tables/databases, permanent legacy-plus-`view_*` search schemas, and a global `bm25()` followed only by `generation_id` filtering are prohibited. Symbol/content/vector owner mappings include graph generation identity; bundle and reranker caches include the full `ViewID`. Search is part of the differential equivalence oracle, not a best-effort sidecar.

Ownership is normative and machine-readable. A `table_ownership_manifest` classifies every table/operation as: `graph_generation` (`generation_id`; source metadata in `generation_source_files` is ordinary generation-keyed payload), `composed_analysis` (`analysis_generation_id` linked to immutable `ViewID` plus resolved-revision fingerprint), `transient_view_cache` (`ViewID`), or `control_catalog`. Commit-sensitive churn/coverage/release/blame and other composed analyses live under `analysis_generations(analysis_generation_id, view_id, resolved_revisions_hash, producer, fingerprint, state, ...)`, not a tree-shared graph generation. The current singleton `analysis_active_generation` and process-wide mutation revision become view-scoped. This manifest drives cleanup and the mechanical unscoped-SQL check.

Request-serving and mutation operations MUST use their declared owner binding. Explicitly typed, allowlisted cross-generation administration (quota accounting, integrity audit, bounded GC discovery, migration) may scan multiple owners but cannot return user query data or mutate payload without a second owner-scoped step.

Branch/ref aliases remain separate from immutable generations. Removing or renaming a ref changes aliases, not rows still referenced by a checkout, explicit-ref route, cache entry, or request lease.

### Sparse-generation contents

A sparse generation needs:

- added/replaced files and file tombstones;
- nodes for replaced files;
- explicit node tombstones where file ownership cannot express deletion;
- outgoing edge-set replacement masks;
- visible replacement edges;
- sparse FTS/content/vector documents;
- ref facts, contracts, semantic data, and other supported sidecars;
- producer-specific masks/completeness;
- source content hashes, modes, and symlink targets (metadata only);
- build fingerprint and integrity checksum.

Rows for a generation are immutable after it reaches `ready`. Immutability is enforced by the storage API and state transitions, not by making a database file read-only. Throughout this document, a **sealed** generation means exactly this ready-and-immutable state; there is no separate sealing step, flag, or file operation.

### Atomic publication in one database

1. Capture the lower `ViewID`, target ref/OID/tree, dirty snapshot, and indexing fingerprint.
2. Insert a generation catalog row in `building` state. No active route may reference it.
3. Write its generation-keyed payload in bounded transactions so the shared WAL and writer lane remain healthy.
4. Compute the Git/file diff, dependency invalidation closure, production resolver/enrichment data, search indexes, and declared-complete sidecars.
5. Validate point/bulk invariants, referential integrity, completeness, lower-generation availability, and target state.
6. Re-read HEAD/tree/dirty fingerprint and lower generation. If any captured input changed, mark the build `superseded` and rebuild from the latest state.
7. In one final SQLite transaction, mark the generation `ready`, atomically update the route/active-generation pointer, and mark the previous generation `retiring`.
8. Notify waiting sessions and invalidate caches keyed by the old route.
9. Delete retiring generation rows only after all routes and request leases release them. Large deletion is chunked/batched to avoid a long writer lock.

A failed build leaves the prior ready route intact. A crash leaves unreferenced `building`, `superseded`, or `retiring` rows that startup recovery resumes when safely journaled or removes idempotently. There is no generation-file discovery, sealing, handle LRU, or per-generation connection lifecycle.

### Shared-database operational constraints

- Builders and GC MUST use the existing mutation-lane discipline and bounded batches.
- Publication pointer changes MUST be short transactions.
- Queries pin generation IDs and may use normal SQLite read snapshots; they never infer visibility from partially written rows.
- Indexes begin with or efficiently filter by generation ID to prevent cross-generation scans.
- GC metrics must expose rows/bytes pending, oldest lease, batch duration, WAL growth, and writer contention.
- A storage quota evicts only unreferenced cached generations; it cannot evict a live checkout route or primary base.

## Indexing architecture

### Content source abstraction

The current indexer is rooted in filesystem paths. Branches without a checkout require an abstraction such as:

```go
type ContentSource interface {
    Open(path string) (io.ReadCloser, FileMeta, error)
    Stat(path string) (FileMeta, error)
    Walk(ctx context.Context, fn func(path string, meta FileMeta) error) error
    ConfigFiles(ctx context.Context) ([]SourceFile, error)
    Identity() ContentSourceID
}
```

Implementations:

- `FilesystemSource`: a checked-out worktree, including dirty content;
- `GitTreeSource`: blobs and modes from an exact locally available tree OID — used both at build time and for ready commit-only view reads (**DECIDED SOURCE-DURABILITY-1**);
- `LayeredSource`: selected higher sources falling back to a lower source.

Build-time Git-tree content SHOULD be obtained with long-lived `git cat-file --batch-command -Z` object access when the installed Git supports `-Z`; a compatibility path may use newline-delimited batches only with already-validated hexadecimal OIDs, never arbitrary path/ref text. It MUST NOT create any checkout, execute checkout hooks, or fetch/lazy-fetch objects. A missing ref, commit, tree, blob, or promisor object makes the build `failed`: no exact route is published, the caller receives `ref_not_available_locally`, and any optional lower-view response remains clearly labeled as an inexact fallback rather than the requested view. Ready commit-only view file reads also use `GitTreeSource` at request time; later Git object GC can therefore decay the file-read capability, which is atomically withdrawn per **DECIDED SOURCE-DURABILITY-1** rather than misreported.

Language detection, ignore rules, `.gortex.yaml`, module manifests, workspace manifests, generated-file rules, and extractor configuration must be read from the selected source, not accidentally from the base checkout. A configuration change can expand invalidation beyond the textual diff and may require a dense rebuild.

### Commit-layer build

For exact reconstruction of target tree `T` over base tree `B`, the file delta MUST be the direct tree-to-tree difference `B → T`, not a three-dot/merge-base diff. A merge-base diff describes branch development but does not mask files that exist in B and not T after divergence.

Build steps:

1. Resolve and validate the requested full ref or commit with argument-safe Git invocation.
2. Resolve the exact target commit and tree.
3. Diff base tree directly to target tree using NUL-delimited output.
4. Treat rename/copy records as deletion plus addition for correctness.
5. Read added/modified blobs from the target tree.
6. Compute syntax/config/dependency invalidation.
7. Re-index changed and affected files into a new sparse generation.
8. Build search and supported sidecars.
9. Publish against the captured base generation.

A branch name is recorded only as an alias/routing hint. Cache reuse is by tree/content fingerprint.

### Dirty-layer build

A dirty layer is based on a checkout snapshot:

```text
(checkout_id, incarnation, HEAD tree, index state, worktree generation,
 config fingerprint, lower commit-layer generation)
```

Recommended contents:

- tracked files whose visible filesystem content differs from HEAD, whether staged or unstaged;
- tracked deletions;
- non-ignored untracked source/config files;
- mode and symlink changes;
- dependency-driven edge overrides in otherwise unchanged files.

Ignored files are excluded unless the repository indexing configuration explicitly includes them. Submodules and nested repositories retain existing repository-boundary semantics. Non-ignored untracked source and configuration files are included. **DECIDED DIRTY-1.**

### Workspace enrichment across checkouts

Checked-out views — dedicated and automatic — receive workspace/LSP enrichment per checkout: each live checkout is its own LSP workspace root, and enrichment markers/state key by generation and checkout rather than repository prefix alone. The current prefix→root enrichment map cannot express two roots for one family and must be replaced. Concurrent language servers are bounded by a global cap with least-recently-used eviction; a checkout whose server was evicted reports its LSP-derived capabilities as building on next demand rather than silently serving stale facts. **DECIDED LSP-SCOPE-1.** Inactive refs remain LSP-free per FIDELITY-1.

### Enrichment fidelity and inactive refs

Ordinary operation does not inspect non-checked-out branches. Automatic discovery and reconciliation cover live checked-out worktrees only. Updating, adding, deleting, or moving an unrelated inactive ref MUST schedule no indexing work. A previously requested ref is re-resolved only on its next selection/prewarm; no idle alias is continuously indexed.

Explicit inactive locally resolvable ref/commit views are nevertheless part of V1. **DECIDED BRANCH-2.** Selection/prewarm resolves only objects already in the local object database, creates a read-only `ref_view` plus an evictable immutable generation, and creates no tracking intent, checkout, dedicated graph, project membership, hidden worktree, or Git mutation. V1 never fetches or permits promisor lazy-fetch. If any required ref/commit/tree/blob object is absent, the build is `failed`, no exact route is published, and the stable error is `ref_not_available_locally`.

This is not already implemented end-to-end. Current checked-out branch support mutates the one graph after [`GitWatcher`](../internal/indexer/git_watcher.go) or polling observes a new `HEAD`; it has no per-tree cache, so A→B→A repeats work. Current [`RepoEntry.Ref`](../internal/config/global.go) / [`TrackParams.Ref`](../internal/daemon/proto.go) is repository metadata: [`resolveRepoFilterArgs`](../internal/mcp/tools_core.go) compares the configured tag to the requested `ref`, while [`TrackRepoCtx`](../internal/indexer/multi.go) never resolves it and calls the filesystem-rooted indexer on `entry.Path`. Normal CLI/MCP tracking surfaces do not expose a Git-content selector, the indexer walks/reads the checkout filesystem, and no `GitTreeSource` exists. V1 therefore preserves existing `ref` tag/filter semantics byte-for-byte and introduces a distinct Git-view selector; this feature does not deprecate or supersede metadata `ref`.

V1 defines the versioned completeness profile `inactive_ref_structural_v1`. It is a closed producer manifest, not a broad label. A generation is `ready` for this profile only when these stable producer IDs satisfy their declared state:

- `source.snapshot`: Git-tree paths, modes, symlinks, and per-file content hashes; ready-view file reads go to the local object database and are atomically withdrawn if required objects are pruned (**DECIDED SOURCE-DURABILITY-1**);
- `source.configuration`: source-safe `.gortex.yaml` values, ignore/generated-file rules, manifests/workspace manifests, language admission, and extractor configuration from that tree, merged with separately trusted host policy;
- `graph.syntax`: source/tree-sitter nodes and structural edges;
- `graph.resolution.repository_local`: deterministic name/import/module resolution that needs no live workspace;
- `graph.incoming_index`: incoming edges derived from the same visible outgoing relation;
- `search.symbol` and `search.content`: exact visible symbol/content corpora;
- `search.vector`: exact visible vector corpus computed by an approved **local** provider when trusted graph policy enables vectors for inactive refs, or `disabled_by_config` when that trusted policy disables them. Inactive-ref indexing never sends source text to a remote provider (**DECIDED REF-CONFIG-TRUST-1**). Local-provider unavailability or refusal is not `disabled_by_config` and blocks publication of this profile.

No unnamed “source-derived sidecars” are implicitly required. Adding a required producer changes the profile version/hash. Ref facts, contracts, clones, global analyses, and other producers advertise separate capability IDs; a request that needs one evaluates that capability explicitly. Cross-repository structural references are part of the V1 required manifest as `graph.resolution.cross_repository` (**DECIDED CROSSREPO-1**); see the cross-repository views section for the foreign-pinning and bridge-generation contract. Trigram `search_text` is a filesystem capability of the selected view, not a profile producer: worktree views search their own `ViewRoot`, and commit-only views — including every inactive-ref view — report it `capability_unavailable` (**DECIDED TEXT-SEARCH-VIEW-1**).

The profile does not promise workspace/LSP-only enrichment. Capability truth is reported individually, for example:

```text
structural_graph: complete
symbol_search: complete
content_search: complete
vector_search: complete | disabled_by_config
cross_repository.references: complete
text_search: unavailable
lsp.references: unavailable
lsp.diagnostics: unavailable
lsp.hover: unavailable
lsp.rename: unavailable
lsp.code_actions: unavailable
```

A structurally ready view is not globally “incomplete” merely because LSP is unavailable. `require_complete` expands the capabilities required by the requested operation; entries in `required_capabilities` are fail-closed, while `optional_capabilities` only annotate the rider and never fail the request. Structural search therefore succeeds when its profile is complete; an LSP-only operation fails with `capability_unavailable`, and a required-but-building/failed structural producer returns `required_capability_incomplete`. Gortex MUST NOT start an LSP, borrow checkout/base LSP facts, or label a lower fallback as the requested ref view.

`CommitLayerKey.enrichment_profile` includes the complete profile/version fingerprint. Same-tree reuse is valid only when completeness/enrichment fingerprints match, or when the reusable structural subset is physically isolated and independently validated. A checked-out generation containing workspace/LSP facts cannot be reused wholesale as an inactive-ref generation solely because its tree OID matches.

V1 does not require inactive-ref LSP parity. Every unsupported capability is explicit; all producers in the versioned `inactive_ref_structural_v1` manifest remain required. **DECIDED FIDELITY-1.**

## Search and query composition

### Composite search

`Engine.WithReader` is insufficient because the existing backend can return base-only candidates and pre-materialized bundles. A view owns a `CompositeSearch` that:

1. queries generation-local FTS/content/vector candidates for every generation in the selected view;
2. removes lower candidates whose file/node/search document is masked by a higher owner;
3. rehydrates every candidate through the graph view;
4. discards missing/tombstoned IDs and stale bundled edges;
5. deduplicates by logical node ID, keeping the highest visible payload;
6. normalizes comparable text/vector signals using view-correct corpus/model statistics;
7. ranks all remaining visible candidates together by relevance, regardless of whether they came from a layer or the main generation;
8. merges vector scores only when embedding model/config fingerprints match;
9. applies the reranker only after masking/deduplication and never permits a lower payload for the same logical identity to replace the visible higher payload.

New overlay-only symbols are first-class candidates, not dependent on exact-name or substring rescue paths. The main graph participates whenever its candidates remain visible; it is not a lower ranking tier. **DECIDED OVERALL-1.**

### Analyzer and cache rules

**DECIDED ANALYSIS-1:** analyzer rollout beyond the structural profile is capability-by-capability. Any analyzer that is enabled for a view must obey the following safety rules.

- Every MCP/HTTP/CLI query path MUST obtain the request-pinned view.
- Direct base-store reads inside request handlers are forbidden unless the operation is explicitly documented as base administration.
- Communities, processes, PageRank, SCC/WCC, adjacency, reachability, hotspots, cycles, coverage, routes, models, SQL analysis, dataflow, and similar caches MUST include `ViewID` and completeness profile in their key.
- Correct but slower composed fallbacks over visible nodes/edges SHOULD precede optimized layer-aware projections.
- A static check SHOULD prevent new direct `s.graph` request reads after migration.
- Stats must be exact for the view or return an explicit unsupported/incomplete result; silent base-only counts are prohibited.

### Cross-repository views

**DECIDED CROSSREPO-1:** cross-repository structural reference edges are required by the V1 profile. The composition contract below is V1-normative wherever the capability is advertised complete.

A workspace query selects one coherent revision for each repository, represented by `WorkspaceViewID`. If a session is inside a worktree for repository A, it selects that checkout view for A and the configured/default dedicated view for other repositories.

Cross-repository edges are source-owned and must obey masks. When a change in A affects resolution in B, the implementation must either:

- rebuild the affected cross-repository edge set in a view-keyed bridge generation; or
- mark the relevant producer incomplete and surface that state.

Base cross-repository edges MUST NOT survive silently when their source resolution or target existence is invalid in the selected workspace view.

Cross-repository edges are built against concrete foreign generations. Every generation that bakes cross-repository resolution MUST record, per foreign repository, the exact foreign `generation_id` its edges were resolved against (a foreign-pin catalog record). A workspace view whose selected foreign generation differs from the pinned one either composes through a view-keyed bridge generation built for that pairing or marks the producer incomplete; pinned-versus-selected divergence is never silent. Today's whole-store resolution pass records no such pin and is replaced by pinned per-pair resolution.

## View selection and paths

### Selection precedence

**DECIDED API-1:** Selection order:

1. Explicit request selector (`view`, checkout, full ref, or commit).
2. Session-bound checkout selected from the handshake CWD using longest-root matching.
3. Explicit project/workspace view policy.
4. Primary dedicated base view.

Proposed selectors:

```text
auto
base:<graph-id>
worktree:<checkout-id>
git-ref:<graph-id>:<full-ref>
commit:<graph-id>:<oid>
delta:<layer-generation-id>   # diagnostics only
```

V1 MUST reject short branch names, ambiguous names, and revision expressions. Callers use a full accepted ref name or exact commit OID; no family-local guess or precedence rule is permitted.

### Meaning of “overall”

“Overall” means one coherent selected `WorkspaceView`: the selected worktree/Git-ref overlay stack plus its main graph for each repository. It does not union sibling worktrees, inactive refs, or conflicting graph alternatives. Higher layers mask/rewrite the same identities and owners; every other visible candidate from overlay or main graph participates in one relevance order. **DECIDED OVERALL-1.**

Unioning mutually exclusive revisions would produce contradictory definitions, duplicate logical IDs, and graph paths that never coexist. A separate administrative `views list/search --all-views` operation MAY inspect cached alternatives, but every result must be labeled by `ViewID` and it must not pretend to be one graph.

### Filesystem correctness

Graph identity and filesystem location are distinct. Every selected checkout view carries a `ViewRoot` and `ContentSource`.

- File reads and edits from a worktree view resolve to that worktree even if the same path exists in the canonical checkout.
- Absolute paths returned to a client are rooted in the selected checkout where one exists.
- A commit-only view uses Git-object source reads and is read-only for edit operations. It has no filesystem root and MUST NOT fabricate an absolute path under the primary/base checkout.
- Public file identity is `(ViewID, RepoPrefix, repo_relative_path)`. Commit-only results expose an opaque `gortex-view://<view-id>/<repo-prefix>/<percent-encoded-path>` URI plus the repository-relative path; file-read tools accept that identity and route through the local Git object database (`GitTreeSource`), with atomic file-read withdrawal when required objects are missing (**DECIDED SOURCE-DURABILITY-1**). Filesystem-only clients receive an explicit non-filesystem-source limitation rather than a misleading base path.
- Trigram text search (`search_text`) is `ViewRoot`-scoped: a worktree view searches its own root with a per-checkout searcher; a commit-only view reports `capability_unavailable`. It never serves another checkout's bytes for the selected view. **DECIDED TEXT-SEARCH-VIEW-1.**
- A worktree outside the explicitly tracked directory is authorized only after its Git common-directory identity is verified against a tracked family.
- Longest-root matching and path canonicalization must use the existing cross-platform path-identity utilities.

The current fallback that reroots only when the canonical file is missing is not sufficient and should be removed after view-aware paths ship.

### Hook and control-socket front door

MCP is not the only entry point: PreToolUse hooks and the daemon control socket ask graph questions — deny probes, file-indexed checks, evidence lookups — outside any MCP session. These probes MUST resolve a view exactly like an MCP request: the file path selects the owning checkout by longest-root match, and the probe runs against that checkout's pinned view. For a file inside a known family's not-yet-reconciled worktree, the probe triggers immediate family reconciliation and the hook fails open (native tools remain allowed) until that checkout's view is ready; once ready, normal enforcement applies. During availability/removal grace, probes follow the same rules as read-only queries: they may answer from the labeled primary fallback, and a deny message must never present fallback evidence as exact. **DECIDED HOOKS-VIEW-1.**

### Session scope and view selection

Session workspace/repository ceilings and view selection are orthogonal axes: the ceiling decides which repositories a session may see; the view decides which revision of each. A view selector naming a checkout or graph outside the session's ceiling fails with `selector_out_of_scope` rather than widening the ceiling. A contained-scope session whose CWD spans several worktrees of one family selects each checkout's own view for paths under that checkout and the primary base view for family repositories not represented by a contained checkout. In scope terms an automatic checkout occupies its family primary's `RepoPrefix`; scope ceilings never gain a revision dimension.

## Discovery and lifecycle

### Authoritative inventory

For each family reachable through an active intent, retained dedicated ownership, `intent_change_pending`, or an unfinished cleanup journal, discovery MUST:

1. resolve absolute Git directory and common directory using Git plumbing rather than assuming `.git` layout;
2. enumerate `git worktree list --porcelain -z`;
3. parse main/linked path, HEAD, branch/detached state, lock reason, and prunable state;
4. associate each record with a stable checkout/incarnation identity;
5. if `intent_change_pending` exists, preserve its recorded desired/effective ownership regardless of current source count; otherwise classify a checkout as dedicated when the union of normalized active intent records is non-empty, or automatic when it is empty;
6. reconcile catalog state, durable grace, primary-base validity, and routes;
7. attach watchers for live, accessible checkouts;
8. schedule required commit/dirty generation builds.

The stable NUL-delimited porcelain format is required because paths may contain whitespace or newlines. The main worktree is listed first; code must not infer “main” only from path shape.

A successful common-directory inventory is authoritative about whether an administrative worktree record exists; a failed inventory is not authoritative about removal. An inventory omission proves removal only after the reconciler verifies that it queried the same `common_dir_identity` and that the family was not replaced. A `prunable` record still exists administratively and is not removal by itself. To classify it as removed, Gortex additionally needs positive local path/mount evidence that distinguishes a deleted checkout from an unavailable filesystem. Explicit confirmed Gortex `forget` or successfully observed external Git removal is independently authoritative; `untrack` is an intent transition, not proof that the Git checkout vanished. All conflicting or insufficient evidence resolves to `inaccessible`, never deletion.

A common-directory or family-inventory failure sets `family_inventory_unavailable` and sends every checkout whose administrative/HEAD identity cannot be independently validated through availability handling. A checkout-root-only failure affects only that checkout. Neither class starts removal grace.

While a checkout is accessible, Gortex records its root/nearest-existing-parent filesystem or volume identity. A still-listed `prunable` record may prove removal only when common-directory inventory succeeds, `lstat(root)` reports absence, the nearest existing ancestor is reachable on the same recorded filesystem/volume, and the recorded mount is known present. Permission/I/O errors, unknown volume identity, an absent mount point, or inability to perform a portable identity check remain `inaccessible`. On a platform without a reliable volume-identity probe, `prunable` alone never authorizes forgetting; inventory omission or explicit `forget` is required.

Inventory MUST NOT allocate/persist a new `CheckoutID` for a record currently classified prunable/disappeared/inaccessible unless it is matching an already-retained inaccessible identity. After terminal forgetting, such records are observed ephemerally only; a later accessible checkout creates a new incarnation. While a family is in `no_primary`, live accessible worktrees are likewise observed ephemerally: no durable automatic `CheckoutID` is allocated until a primary is designated. **DECIDED PRIMARY-LIVE-IDENTITY-1.**

Discovery scope is deliberately limited to Git families reachable from active intent or retained transition/ownership/cleanup state. A sole-source family's pending transition remains reconciliation-scoped across restart even while its active-intent count is zero. Gortex must not scan arbitrary parent directories or the whole filesystem for repositories.

An MCP handshake CWD that resolves to a known family but a not-yet-reconciled worktree SHOULD trigger immediate family reconciliation. A hook or control-socket probe naming a file inside such a worktree triggers the same reconciliation (**DECIDED HOOKS-VIEW-1**).

### Watchers plus reconciliation

Topology observation MUST use a bounded control-plane watch/probe set whose size does not grow with repository source-file count. Git worktree inventory remains authoritative; bounded probes only accelerate reconciliation. Probe registration failure, event overflow, or a missed event MUST enter one coalesced authoritative-inventory fallback rather than widening the watched source tree. Dynamic admission, promotion, recovery, owner handoff, removal, and daemon teardown MUST release every watch registration and leave no stale family observer.

No filesystem watch is authoritative by itself. The subsystem uses:

- a family-level trigger for common Git worktree/ref administration changes;
- per-checkout HEAD/index and source-tree triggers;
- the existing debounced file-event batching concepts;
- a periodic authoritative inventory;
- an immediate inventory on startup and relevant session CWD.

Git implementations may store refs differently (including reftable), linked-worktree HEAD is per-worktree while most refs are common, and recursive ref changes are not safely covered by one nonrecursive directory watch. Therefore raw-path watches are accelerators; Git commands are the source of truth.

Gortex MUST NOT install or overwrite Git hooks. Existing hooks such as `post-checkout` or `reference-transaction` may be an optional user-configured accelerator, never the only correctness mechanism.

### Checkout state machine

Availability expiry is mode-specific. An automatic checkout follows `availability_grace -> forgetting_checkout -> ∅`: its identity is retained only through the deadline, then its route, incarnation, clocks, and layers are terminally forgotten while the family primary survives. Only explicitly retained ownership may follow `availability_grace -> checkout_unavailable` after `purge_inaccessible_layers`, preserving intent/config/identity/sealed full generations. Any generic `checkout_unavailable` wording below is constrained by this branch and MUST NOT retain a disposable automatic identity after expiry.

```text
checkout_ready(automatic)
  -- access failure, removal unproven --> availability_grace(automatic)
       (exact route withdrawn; labeled read-only base fallback while identity is registered)
availability_grace(automatic)
  -- same checkout becomes accessible before deadline --> reconciling --> checkout_ready(automatic)
  -- availability deadline --> forgetting_checkout --> ∅ automatic checkout
       (route, identity, incarnation, clocks, and rebuildable layers absent; family primary retained)

checkout_ready(dedicated)
  -- access failure, removal unproven --> availability_grace(dedicated)
       (exact route withdrawn; labeled read-only base fallback)
availability_grace(dedicated)
  -- same checkout becomes accessible --> reconciling --> checkout_ready(dedicated)
  -- availability deadline --> checkout_unavailable(dedicated)
       (rebuildable checkout layers absent; explicit identity/intent/full graph retained)
checkout_unavailable(dedicated)
  -- later accessible --> reconciling/rebuild --> checkout_ready(dedicated)

checkout_ready | availability_grace | checkout_unavailable
  -- first positive automatically detected removal at t --> removal_grace(t)
removal_grace
  -- same administrative incarnation reappears accessible --> reconciling
  -- same administrative incarnation reappears but root inaccessible --> availability_grace | checkout_unavailable
  -- removal deadline, non-primary --> forgetting_checkout --> ∅ checkout
  -- removal deadline, primary --> primary_closure_retiring --> ∅ primary + ∅ every dependent automatic checkout
       (identities and clocks included; independent dedicated graphs survive; ∅ family only when none remain)

checkout_ready(dedicated, non-primary)
  -- explicit untrack --> demoting --> checkout_ready(automatic)
availability_grace | checkout_unavailable (dedicated, non-primary)
  -- confirmed untrack/forget --> forgetting_checkout --> ∅ checkout
checkout_ready(non-primary)
  -- confirmed forget --> forgetting_checkout --> ∅ checkout
checkout_ready | availability_grace | checkout_unavailable (primary)
  -- confirmed untrack/forget --> primary_closure_retiring
primary | inaccessible non-primary | non-primary without a different ready primary
  -- last intent disappears via reload --> intent_change_pending
  -- intent restored --> prior state
  -- CLI/MCP confirms preview --> forgetting_checkout | primary_closure_retiring

any off-route build may fail/supersede while the previous ready lease remains valid
```

`intent_change_pending` is a durable ownership-control substate orthogonal to the live availability/removal axis. While pending, access failure and recovery still advance `checkout_ready ↔ availability_grace ↔ checkout_unavailable`; positive removal evidence still starts its own fresh `removal_grace`. Source restoration restores ownership intent but does not cancel independently valid removal evidence; only same-incarnation reappearance cancels automatic removal grace. Confirmation revalidates the latest availability/removal state and chooses demotion only if the checkout is then accessible and a different ready primary exists, otherwise the documented destructive branch. Confirmation, restoration, and removal expiry serialize on `(checkout_id, incarnation, transition_id)` so no stale prior-state snapshot overwrites newer evidence.

`∅` means terminal logical row/config absence, not forensic erasure and not a persisted `data_purged`, `retired`, or tombstone state. A cleanup journal exists transiently for crash recovery and deletes itself last. `availability_grace`/`checkout_unavailable` are not removal states: explicit intent/config, identity, and sealed dedicated generations remain. **DECIDED INACCESSIBLE-1 / ERASURE-1.**

`checkouts.state` stores the availability axis and terminal cleanup phases: `checkout_ready`, `availability_grace`, `checkout_unavailable`, `reconciling`, `demoting`, `forgetting_checkout`, and `primary_closure_retiring` (the older draft name `checkout_retiring` denoted the same state as `forgetting_checkout`). The removal axis is not a `state` value: a checkout is in `removal_grace` exactly when `removal_detected_at`/`removal_deadline` are set, while `state` continues to track availability. The intent axis rides `active_intent_transition_id`: `intent_change_pending` is the predicate that a live transition row exists. `desired_mode` and `effective_mode` take exactly the values `dedicated` and `automatic`; with **DECIDED PRIMARY-LIVE-IDENTITY-1** no `no_primary` mode value exists, because checkouts that would need one are forgotten instead.

`desired_mode` records the last fully reconciled union of intent sources; `effective_mode` records what the published route/graph actually implements. When the last source vanishes outside a confirmed administrative transaction, Gortex durably creates `intent_change_pending` with prior desired/effective modes, prior availability state, and a source-set fingerprint before changing either mode. Discovery MUST NOT classify that checkout as automatic merely because its current active-intent count is zero. Restart resumes the transition record; source restoration returns to the persisted prior ownership mode while preserving any newer availability/removal evidence, while previewed confirmation revalidates and completes the currently applicable demotion/forget/primary-closure transaction. Only successful publication/cleanup commits the new effective mode and removes the transition record.

### Removal and inaccessibility policy

Availability expiry is not one uniform cleanup action: automatic mode runs terminal `forget_checkout`, after which a stale explicit selector fails and selector-free/default or explicit-base selection remains on the surviving primary; retained explicit ownership alone runs `purge_inaccessible_layers` and may remain `checkout_unavailable`.

**DECIDED REMOVAL-EVIDENCE-1:** The following conservative evidence classifier is normative. Git distinguishes explicit removal, successful authoritative inventory, prunable administration after manual deletion, locked worktrees on removable media, and ordinary access failures:

- `authoritatively_removed`: an explicit confirmed Gortex `forget`, successfully observed external Git removal, or successful access to the common Git administration followed by authoritative inventory that proves the checkout administrative identity is gone. A prunable record counts only when independent path/mount evidence confirms disappearance.
- `inaccessible`: authoritative inventory still contains the checkout, or inventory/root access cannot complete because of lock, permission, I/O, unavailable mount, or other uncertainty.
- `intent_transition`: explicit `untrack` while the Git checkout still exists. It is not evidence of disappearance; it selects non-primary demotion, inaccessible-checkout forget, or primary-closure retirement according to the promotion/demotion rules.
- Ambiguous evidence is `inaccessible`. `ENOENT` alone is not universally proof of removal because an unavailable mount can look identical.

`checkout_path_evidence` is durable, not an in-memory probe cache. On every validated accessible reconciliation it records the platform identity kind and opaque stable token for the checkout root's volume, common-directory volume, root path identity, and nearest existing ancestor. A later absence claim is usable only when a fresh probe can prove the recorded root volume is mounted/reachable and the common-directory inventory was read from the same validated family identity. A reachable ancestor on a different volume, a changed/unsupported token, or failure to revalidate the common directory yields `inaccessible`, never removal. Restart must use the persisted sample; it cannot substitute the daemon's current parent mount.

The routing/cleanup contract is:

1. Either state immediately withdraws the exact path/CWD route for new requests. Eligible read-only graph/search requests select a ready family primary base and carry the standard fallback rider: `requested_view` naming the checkout, the actual view/revision fields, `fallback_reason`, the applicable deadline/`retry_after`, and `exact=false`. Missing dirty and checkout-rooted buffer data is excluded.
2. `require_exact`/`require_fresh`, filesystem reads requiring the unavailable root, and every edit/mutation operation fail explicitly. Writes are never redirected. Existing exact-view leases may finish.
3. Inaccessibility starts durable `availability_grace`. Same-incarnation automatic recovery before the deadline revalidates and restores the route. At automatic expiry, `forget_checkout` removes the route, identity, incarnation, clocks, and rebuildable layers after leases drain; that checkout's fallback ends, a stale explicit selector fails, and selector-free/default or explicit-base requests continue against the surviving primary. Explicitly retained dedicated ownership instead deletes only rebuildable checkout layers and preserves intent/config/identity/sealed full generations; any applicable labeled fallback follows that retained explicit view. **DECIDED AUTOMATIC-GRACE-EXPIRY-1 / INACCESSIBLE-1.**
4. Automatically detected authoritative removal starts durable `removal_grace`. Same-incarnation recovery before expiry may cancel it. At expiry, terminal forgetting removes all logical checkout graph/config state and the cleanup journal.
5. Explicit confirmed `untrack` or `forget` does not wait through discovery grace. The administrative transaction selects its documented demotion/forget/primary-closure branch, journals config changes, and drains affected readers.
6. Authoritative primary removal/untrack deletes the primary dependency closure—primary graph plus every automatic/ref/dirty route, layer, alias, cache, and buffer session binding whose lower chain depends on it, including dependent automatic checkout identities. Independent sibling identities/intents/config/full generations/routes/unrelated payload survive; only bridge/cross-view/derived rows referencing the retired primary are invalidated/rebuilt.
7. If independent dedicated graphs survive, the family enters `no_primary`; automatic checkout routing/building is disabled until explicit designation. Ref views rooted in the lost primary are deleted/refused, but an explicit ref view may still select a surviving ready dedicated `graph_id`. If no dedicated graph survives, family identity is deleted.
8. Reappearance after authoritative terminal forgetting or automatic availability-deadline forgetting is a new incarnation. Automatic recovery reuses the retained identity only before its deadline; explicitly retained dedicated recovery may reuse its preserved identity afterward and rebuild discarded layers.
9. Gortex never runs `git worktree prune`, remove, repair, or another mutating Git command.

If no ready designated primary exists, no fallback is synthesized from an independent dedicated sibling. Return `no_primary` for a family with no designation, or `primary_not_ready` (carrying the primary's build error) when a designation exists but has no ready generation; the response still names the requested view and `exact=false`.

**DECIDED PRIMARY-LIVE-IDENTITY-1:** primary closure forgets dependent automatic checkouts entirely. `retire_primary_closure(..., cause)` still distinguishes causes for preview and reporting, but the outcome for dependents is uniform:

- Whatever the cause — `primary_authoritatively_removed`, `primary_untracked`, or family forget — every automatic checkout whose lower chain depends on the retired primary loses its route, layers, and caches **and** its `CheckoutID`, incarnation, and availability/removal clocks. No `discovered_no_primary` or `effective_mode=no_primary` identity survives.
- A dependent's own independently detected removal evidence needs no preservation: its identity is deleted with the closure either way. Only checkout rows *not* dependent on the retired primary — independent dedicated checkouts — keep their identities, clocks, and evidence.
- If an independent dedicated graph survives, the family enters `no_primary`; live worktrees are observed ephemerally and re-acquire durable identities as new incarnations only after explicit `set-primary` designation (or explicit track).
- If none survives, the operation is `forget_family` and every remaining family/catalog/config row disappears.

Reconciliation MUST NOT create durable automatic checkout state while the family has no primary.

The default availability/removal discovery grace is 30 seconds. Timestamps/deadlines are durable; restart does not reset them. Routing withdrawal targets p95 below 2 seconds when events arrive, or one reconciliation interval after a missed event. Physical deletion completes after grace where applicable, lease drain, and the cleanup SLO.

When an inaccessible checkout first gains positive removal evidence at time `t`, Gortex sets `removal_detected_at=t` and a fresh removal deadline; elapsed availability grace never counts toward removal grace. If the same administrative incarnation reappears before that deadline, automatic removal cleanup is cancelled: accessible roots reconcile to ready, while still-inaccessible roots return to their persisted `availability_grace` or `checkout_unavailable` state. Only an explicitly confirmed administrative `forget`/destructive untrack branch is non-cancellable.

Total forgetting that removes explicit intent or external config requires a recoverable saga because SQLite and external config/project files cannot commit together. It is used for authoritative removal/forget and the explicitly previewed destructive untrack branches (inaccessible-checkout forget or primary closure), never to erase explicitly retained ownership merely because access failed. Automatic availability-deadline expiry is a separate config-free disposable-checkout cleanup; live non-primary demotion uses its separate route/config transaction. Gortex removes every owned intent/config source with compare-and-swap guards, resumes idempotently after crashes, verifies the operation-specific residual-state postcondition, and deletes the journal last. A source-write failure remains visibly `retiring/retrying`; it MUST NOT claim completion.

No generation may outlive its lower base. Primary-closure deletion explicitly preserves independent dedicated graphs. **DECIDED INACCESSIBLE-1 / REMOVE-2 / PRIMARY-LOSS-1 / ERASURE-1.**

### Startup recovery

Recovery MUST resume the mode-specific deadline branch. An expired automatic availability deadline resumes terminal automatic-checkout forgetting; an explicitly retained checkout resumes rebuildable-layer purge while preserving its intent/config/identity and sealed dedicated corpus. Startup MUST NOT convert an expired automatic checkout into a durable `checkout_unavailable` identity.

Startup MUST reconcile catalog, two distinct durable grace classes, intent sources, and authoritative Git inventory before declaring warmup complete:

- Every explicit intent source is reloaded independently while its checkout is merely inaccessible. A completed authoritative-removal/untrack saga has removed every checkout-owned source instead.
- An `intent_change_pending` record is restored before mode classification. Zero currently active sources cannot override its persisted prior desired/effective modes or prior availability state; reconciliation either observes source restoration and rolls back coherently, or waits for/completes the previewed administrative transition.
- Persisted automatic routes and checkout layers are rebuildable cache, not explicit intent. Sealed dedicated full generations are retained across inaccessibility.
- `unavailable_since`/`availability_deadline` and `removal_detected_at`/`removal_deadline` survive daemon downtime and MUST NOT be conflated. Restart does not reset either clock.
- If common-directory inventory, checkout-root access, or mount evidence is unavailable, startup classifies the checkout as inaccessible; it does not infer removal from `ENOENT`, lock, permission, I/O, or a prunable flag alone.
- Automatic availability expiry resumes terminal `forget_checkout`: route, checkout identity, incarnation, clocks, and rebuildable layers disappear after lease drain while the family primary remains. Explicitly retained dedicated expiry resumes `purge_inaccessible_layers`: exact routing stays withdrawn and rebuildable layers disappear, but intent/config/checkout/graph IDs and sealed full generations remain for later validated recovery.
- Authoritative-removal expiry resumes `forget_checkout` or `retire_primary_closure` before any affected route is advertised. Once terminal row/config absence is committed, later discovery creates a new incarnation even if an old lease briefly delayed physical row deletion.
- Worktrees authoritatively removed while the daemon was stopped are collected even when stale prefixes would otherwise have registered them; uncertainty remains inaccessible until authoritative evidence is available.
- A ready generation is reusable only if its owner-independent cache identity,
  required capabilities, enrichment profile, and lower generations remain
  valid. A cache pin is a retention reference, not evidence of compatibility;
  withdrawal of `source.snapshot` or another required capability bypasses the
  pinned candidate and schedules/reuses a compatible replacement.
- Startup reconstructs checked-out commit reuse from durable cache pins before
  orphan discovery or retirement. Coordinator-local maps are lookup
  accelerators only. A branch changed while the daemon was stopped reuses the
  cached target when compatible, preserves the formerly routed generation as an
  inactive cache entry, publishes coherent checkout HEAD/route state, and
  supersedes the obsolete startup-readiness target so warmup cannot remain
  indefinitely at `checkout_builds_pending`.
- Generic discovery of unqueued `ready` checkout commit/dirty orphans runs only
  in Seed, after pin backfill and before coordinator build admission opens. A
  runtime sweep MUST NOT infer retirement from a momentary absence of a route,
  pin, or live-registry entry: normal and temporary transition coordinators
  both have a valid build-return-to-route-publication window. Runtime cleanup
  consumes explicit coordinator/lifecycle backlogs, the durable pin-retirement
  queue, intrinsically terminal/abandoned rows, and rows whose graph is gone.
  A crash in the publication window is recovered by the next Seed scan.
- An unchanged warm restart reparses zero files and physically builds zero
  immutable commit generations. Store deletion is a cold rebuild and has no
  cache-reuse promise; schema migration alone is not a cold rebuild.
- Orphaned `building` generations are resumed only with sufficient journal state; otherwise their generation-keyed rows are deleted.
- `retiring` checkout/family sagas finish idempotent bounded cleanup, verify operation-specific postconditions, and delete their journals on success.
- A `no_primary` family may expose its independent dedicated graphs and explicit ref views rooted in any such ready graph. It MUST NOT reconstruct/advertise automatic checkout routes, durable automatic checkout identities, or any ref view rooted in the lost primary until a primary is explicitly designated with `set-primary`.
- Watcher, LSP, search, analysis, and session routing state is reconstructed from reconciled routes rather than assumed.

### Promotion and demotion

Promotion is a transaction, not `AddRepo`:

1. preserve the current automatic route;
2. durably journal the new explicit intent source;
3. sample HEAD ref/commit/tree plus index/worktree fingerprint;
4. build a full generation containing exactly the sampled HEAD tree and a separate dirty layer for staged, unstaged, and eligible untracked state;
5. re-sample and supersede the build if checkout state changed;
6. atomically publish and reroute the checkout;
7. finish persistence of the intent source;
8. retire unreferenced automatic layers after leases drain.

SQLite and external configuration files cannot commit in one transaction, so the journal must make every failure recoverable. If the build or intent/config write fails, the automatic route remains active and the pending intent reports a retryable error.

Demotion after explicit untrack is required only for a live, accessible, non-primary dedicated checkout when a different ready primary exists. **DECIDED PROMOTION-1.** Administrative untrack is authoritative user action and does not enter discovery grace. If no different ready primary exists, the untrack fails with the exact blocker (`no_primary` or `primary_not_ready`) and rolls back, naming the valid paths: designate a primary first with `set-primary`, or run the previewed destructive `forget`. It does not park the checkout in `intent_change_pending`; that state is reserved for passive config/project reload observation. **DECIDED UNTRACK-BLOCKED-1.**

1. Preflight every active intent source for the checkout. Remove/revoke all sources that can be durably changed; if a project/manual source still asserts explicit intent, report it and do not pretend demotion succeeded.
2. Confirm the checkout is accessible and a different ready primary remains. `auto_discover` gates discovery of new siblings; it does not block this explicit transition of an already-known checkout to automatic mode.
3. Build/select the commit and dirty layers off-route.
4. Atomically reroute the live checkout to the automatic view.
5. Delete the former dedicated `GraphID` and generations after old leases drain while retaining `CheckoutID` in automatic mode.

`forget_checkout` leaves no suppression/tombstone record. If the filesystem checkout remains live in a still-known family, a later authoritative inventory may rediscover it as a new automatic incarnation; persistent suppression would require a separate explicit `ignore` feature that stores exclusion config and is outside this zero-state forgetting contract. Sole-primary family forget is not rediscovered implicitly because the family/discovery scope itself is gone. Untracking an inaccessible non-primary checkout cannot safely demote it and therefore performs a previewed `forget_checkout` after revoking its intent sources. On any source-write, primary-base, build, or publication failure, intent and routing remain in their prior coherent state.

A primary checkout is never demoted implicitly. Its untrack preview enumerates the primary graph and all automatic/ref/dirty/buffer state whose lower generation belongs to that primary, plus every independent dedicated sibling and sibling-rooted ref view that will be preserved. Confirmation runs `retire_primary_closure` without electing a replacement; dependent automatic checkouts are forgotten with the closure (**DECIDED PRIMARY-LIVE-IDENTITY-1**). Surviving independent dedicated graphs leave the family in `no_primary`; if the primary was the only dedicated graph, the same operation is a family forget and removes all remaining logical family/config state. **DECIDED PRIMARY-LOSS-1 / LAST-PRIMARY-UNTRACK-1.**

## Branch switching without indexing storms

### Why the current watcher is insufficient

The existing Git watcher has valuable safeguards—debounce, single-flight rerun, repository mutation lanes, and bounded incremental batches—but it mutates the only graph and stores only one current SHA. Consequently A→B→A repeats parsing, and regular filesystem events generated by checkout overlap with Git reconciliation.

Today a checkout can be signaled independently by the recursive file watcher, `GitWatcher`, the adaptive poller, the `post-checkout` notification path, and the periodic GC/reconcile loop. The mutation coordinator serializes work and unions queued scopes, but a signal arriving during an active pass schedules another pass; it does not guarantee one parse generation per Git checkout transaction. The new checkout coordinator must collapse all of these sources into one settled state snapshot.

It also needs stronger Git state identity:

- SHA alone misses switching symbolic branch names at the same OID. Attached/detached/unborn state and the full symbolic ref must be sampled explicitly, using `git symbolic-ref -q HEAD` plus an OID/tree probe where resolvable.
- In linked worktrees, HEAD is per-worktree while most refs and packed refs live in the common directory. The current watcher resolves the linked worktree's administrative Git directory, so a same-branch commit can be missed by fsnotify until polling observes the OID.
- Watching loose `refs/heads` is not sufficient for packed refs, nested names such as `feature/foo`, or reftable storage.
- Freshness/worktree-mismatch results must be invalidatable; the current one-shot cache cannot represent worktrees added or removed after the first check.

### Unified checkout event coordinator

One coordinator per `CheckoutID` MUST combine Git metadata and filesystem signals:

1. An event marks the checkout dirty and starts a quiet-window debounce.
2. The coordinator samples `(head_ref, head_commit, head_tree, index fingerprint, filesystem dirty fingerprint)`.
3. All events from the same checkout transaction collapse into that state transition.
4. It resolves/selects the immutable commit layer.
5. It builds a new dirty layer against that lower view.
6. It re-samples state before publication.
7. If state changed, the build is superseded and reruns once from the latest snapshot.
8. Routing flips once to the coherent result.

Regular file events during branch checkout MUST NOT mutate the dedicated base or publish piecemeal dirty state. The current 300 ms debounce is a useful starting value, but it should be configurable and measured.

### Checked-out and explicit-ref population policy

**DECIDED BRANCH-1 / BRANCH-2 (V1):**

- automatically ensure views only for currently checked-out worktrees;
- branch switching reconciles that checkout to its new HEAD; branch names are metadata/aliases, not checkout identity;
- ignore creation, deletion, or movement of unrelated inactive refs;
- build a non-checked-out ref only when a user explicitly selects or prewarms it;
- resolve accepted selectors to canonical commit/tree OIDs using only the local object database;
- never fetch or promisor-lazy-fetch during V1 construction, even for an explicit selection;
- if any required object is missing, fail the build with `ref_not_available_locally`, publish no exact route, and optionally retain only a labeled lower-view fallback;
- retain recently selected immutable tree generations under an LRU/byte budget.

A moved explicitly requested ref resolves again on the next request, while an already leased/cached OID generation remains immutable. Same-tree aliases share content. Default/overall search never includes unrelated inactive refs.

### Base advancement and force updates

- A force-update affecting a checked-out HEAD is reconciled normally. An explicit-ref alias is re-resolved by an active/new selection request and then targets a new immutable generation; movement of an idle alias creates no background indexing work.
- Old generations remain while leased/referenced and then become cache candidates for GC.
- Updating a configured base ref builds a new full base generation atomically.
- Live automatic checkout routes are rebuilt/rebased against the new base before their routes switch.
- Idle inactive-ref caches are never eagerly rebased. A cached ref view may remain exact on and pin its old immutable base until normal LRU/byte-budget eviction; selection may reuse it while valid. If evicted or policy chooses compaction, rebuild occurs only on the next explicit selection/prewarm (or as foreground-adjacent work), never as a fan-out storm from base advancement.
- If a target tree equals the base tree, the commit layer is an empty valid layer.
- An unborn/orphan branch uses an empty committed-tree lower source plus the dirty layer, accepting a dense first overlay. **DECIDED UNBORN-1.**
- Unrelated histories can generate a near-full overlay and are not an error.

**DECIDED BASE-3:** the daemon advances the full base only when an explicitly configured `base_ref` advances. Automatic density/age-based compaction is post-V1 and gated on benchmarks; until then, compaction is explicit policy.

## Configuration and API proposal

Names below are provisional pending implementation.

```yaml
graph_views:
  worktrees:
    auto_discover: true
    reconcile_interval: 30s
    inaccessible_grace: 30s                   # availability_grace default
    removal_grace: 30s

  branches:
    initial_base: head
    follow_base_ref: null                     # DECIDED BASE-3: only this ref advances the base
    accept_local_remote_tracking_refs: true   # DECIDED REF-SCOPE-1
    retain_inactive: 7d                       # DECIDED RETENTION-1
    max_cached_generations: 32                # DECIDED RETENTION-1

  storage:
    max_bytes_per_graph: 5GiB                 # DECIDED RETENTION-1
    gc_batch_rows: 10000
```

`checked_out_and_on_demand`, `allow_fetch=false`, `inactive_ref_structural_v1`, inactive-ref LSP unavailability, dirty-layer inclusion of non-ignored untracked files (`DIRTY-1` — the former `include_untracked` knob is removed), and fully offline inactive-ref indexing (`REF-CONFIG-TRUST-1`) are fixed V1 invariants, not feature knobs. Implementations MAY expose them as read-only diagnostics/reserved fields, but configuration that asks V1 to fetch, eagerly scan refs, claim inactive-ref LSP support, exclude untracked files from dirty layers, or route inactive-ref content to a remote provider MUST fail validation rather than silently changing capability truth. Completeness requirements are per operation/request, not one coarse `require_complete_analyzers` setting.

### CLI/MCP operations

The product should expose equivalent administrative capabilities through CLI and MCP:

- list repository families, dedicated graphs, checkouts, routes, layers, and generations;
- show active view, base/target revisions, freshness, completeness, size, and last error;
- explicitly select a checkout or locally resolvable ref/commit view through a distinct `view` selector, never the existing metadata `ref` filter;
- prewarm a locally resolvable ref view without tracking the ref or fetching;
- promote an automatic checkout to dedicated;
- designate a family primary with previewed `set-primary <graph-id>` after primary loss;
- list every active explicit-intent source and its provenance;
- `untrack` all revocable intent sources and atomically demote a live accessible non-primary checkout when a different ready primary exists;
- preview and confirm primary untrack, including the deleted dependency closure and preserved independent dedicated siblings; treat sole-primary untrack as family forget;
- `forget` a checkout with zero retained suppression state, and expose a separate future `ignore` concept if persistent exclusion is required;
- show availability/removal evidence, both grace deadlines, blocking leases, primary dependents, and `no_primary` state;
- drop an inactive cached generation;
- force family reconciliation;
- explain why a path selected a particular view.

The proposed public selector is one structured `view` field (CLI renders the same variants as `--view ...`):

```text
view: {kind: auto}
view: {kind: base, graph_id: G}
view: {kind: worktree, checkout_id: C}
view: {kind: git_ref, graph_id: G, value: refs/heads/feature}
view: {kind: commit, graph_id: G, value: <exact-oid>}
```

The existing `ref` parameter remains permanently compatible as a repository metadata tag/filter. It is neither deprecated nor interpreted as Git content selection. If both metadata `ref` and structured `view` are supplied, metadata filtering first selects the repository/graph set; the `view.graph_id` must belong to that set or the request fails with `selector_conflict`.

**DECIDED REF-SCOPE-1:** accepted namespaces are full `refs/heads/*`, peelable-to-commit `refs/tags/*`, already-local `refs/remotes/*`, and exact commit OIDs; no selector fetches. Resolution mechanics: short/ambiguous names and arbitrary revision expressions fail `invalid_view_selector`; a full tag is peeled with commit semantics and an unpeelable/non-commit target fails `ref_not_commit`; an exact OID is validated for the repository's object format and must name a commit. The selector resolves canonical commit/tree OIDs before build identity is computed. `graph_id` MUST name a ready dedicated graph, but need not be the family primary; explicit ref views may therefore root in a surviving dedicated graph during `no_primary`.

`view.kind=git_ref` always means the committed tree as a read-only commit view, even when that ref is checked out in one or more dirty worktrees. It never absorbs index/worktree/buffer state; `view.kind=worktree` is the only explicit selector for those higher layers.

Query controls are orthogonal and have these defaults/meanings:

```text
view?                    # absent means precedence-based selection
require_exact: false     # true forbids actual_view != requested_view
require_fresh: false     # true waits until deadline; stale/building fallback is forbidden
require_complete: false  # shorthand for all default capabilities of this operation
required_capabilities: []
optional_capabilities: []
wait_deadline?
```

The operation first resolves/validates the requested selector, then selects an exact or allowed fallback view, then evaluates freshness, mutability, and the operation's capability set against that actual pinned view. `require_complete` expands only the operation's registered default producers; it never means “all Gortex capabilities.” An explicitly unavailable required producer yields `capability_unavailable`; a supported producer that is building/failed/incomplete yields `required_capability_incomplete`; an optional producer changes the rider but does not fail the operation. A cold exact request that could become ready returns retryable `view_building` immediately when no `wait_deadline` is supplied, and waits up to the deadline otherwise; a deterministically missing local Git object returns `ref_not_available_locally`; an unavailable checkout requested exactly returns `checkout_inaccessible`; any write against a commit-only/fallback view returns `view_read_only`. `require_exact` and `require_fresh` never convert a fallback into an exact result; `require_fresh` over a stale view with no active build triggers the rebuild it waits on.

Every selected/cold response exposes enough information to prevent fallback confusion:

```text
requested_view, actual_view, graph_id, checkout_id?,
requested_ref?, requested_commit_oid?, requested_tree_oid?,
actual_ref?, actual_commit_oid?, actual_tree_oid?,
active_generation_id?, building_generation_id?, view_id?,
requested_state: building | ready | failed,
actual_state: none | ready | stale,
exact: true | false,
freshness, capabilities,
build_token?, retry_after?, fallback_reason?, error?
```

A normal exact ready response MAY abbreviate this rider only for a base/checkout whose view was implicitly selected; an explicitly selected inactive-ref response always includes resolved revision, exactness, and capabilities. Stable errors include `invalid_view_selector`, `ref_not_commit`, `selector_conflict`, `selector_out_of_scope`, `ref_not_available_locally`, `view_building`, `view_read_only`, `capability_unavailable`, `required_capability_incomplete`, `checkout_inaccessible`, `no_primary`, `primary_not_ready`, and `source_object_missing`. Per **DECIDED SOURCE-DURABILITY-1**, a ready inactive-ref view's file reads use the local Git object database; when a required object has been pruned, the read fails `source_object_missing` and the view's file-read capability is atomically withdrawn until the objects are present again. Per-file/per-key stable diagnostic codes such as `buffer_base_drift` and `untrusted_config_key_ignored` ride result payloads and are a separate namespace from the request-level stable errors above. LSP rename planning and code-action discovery fail with `capability_unavailable`; applying a rename/action, direct editing, or any other filesystem mutation fails with `view_read_only`.

Selection/prewarm is cache population only: it MUST NOT create a tracking intent, checkout, dedicated graph, project membership, or global repository entry. An explicit selector overrides the session CWD (**DECIDED API-1**); regardless, a returned fallback always distinguishes `requested_view` from `actual_view` and sets `exact=false`.

The existing session “overlay branch” terminology should be renamed internally to `buffer branch` or `speculative branch` so it cannot be confused with Git branch layers. External compatibility aliases may remain.

## Consistency, failure, and cleanup

### Consistency guarantees

- Generation rows are immutable after state becomes `ready`.
- Route updates are atomic shared-database transactions.
- A request pins a complete `WorkspaceViewID`.
- Generation rows cannot be deleted while routed or leased.
- A stale build cannot publish after checkout removal/re-add because checkout incarnation and captured state are revalidated.
- Search, graph, paths, and analyzer caches all use the same view fingerprint.
- A base-generation change cannot reinterpret an existing layer; it creates a new layer generation.
- No layer survives loss of its lower base; it is rebuilt/rerouted first or cascade-retired.

### Durable checked-out commit cache

An immutable checked-out commit generation remains reusable because the catalog
persists a holder pin; a coordinator's local LRU is never its lifetime owner.
The durable holder identity is `(checkout_id, generation_id)` and each row also
stores the referenced generation's `graph_id`. The denormalized graph value is
validated on every mutation and makes retention/cleanup graph-local and bounded.
Tree/base/fingerprint identity, physical size, and state still come from the
generation. Promotion, demotion, or rehome revokes that checkout's old-graph
pins and pins its newly routed compatible commit; graph teardown revokes every
pin for that graph before generation deletion.

The normal A→B route transaction has one atomic contract:

1. claim a ready B by the owner-independent compatibility key and hold a
   short-lived cache-claim lease;
2. revalidate checkout ID/incarnation/root/state, primary epoch, expected old
   route, graph/base ancestry, B state, and every required capability;
3. set A's holder `last_selected` to the database transaction time (the start
   of its inactivity), insert/refresh B's holder, publish B's commit slot and
   the coherent route/HEAD metadata, and consume the lease in that transaction;
4. on stale compare-and-set, roll back all pin/route timestamps and leave the
   lease for its caller to release; and
5. build/rebind the checkout-specific dirty layer separately. Dirty layers are
   never branch-cache entries and may be rebuilt after a switch or restart.

An already exact route takes a read-only settled path: it does not claim a ready
generation, acquire/release a cache lease, refresh a pin timestamp, rewrite
checkout HEAD, advance the route epoch, or touch the WAL. Owner checkout/ref,
layer ID, path alias, and ref spelling are excluded from the canonical ready
identity. Graph, lower-base generation/fingerprint, target tree, sanitized
source configuration, extractor/resolver/producer versions, completeness
profile, and all producer-declared commit-sensitive inputs are included.
`source.snapshot` and every operation-required producer must still be complete;
a pinned but withdrawn/incompatible generation is bypassed and may be retired
after its independent references drain.

Warm restart reconstructs cache eligibility from pins before retirement. Thus a
stored A→B→A sequence, daemon restart, and another B→A sequence reuses the same
immutable generation IDs with zero physical commit builds and zero reparsed
files while fingerprints and retention remain valid. If Git HEAD changes while
the daemon is stopped, startup treats the persisted route as the old exact view,
adopts an already cached new HEAD when possible, retains the old commit, and
updates the startup-readiness expectation to the current sample. It MUST NOT
wait forever for an obsolete tree or report permanent 99% warmup after a
successful route switch.

Pin eviction and payload deletion are deliberately two transactions. The pin
transaction's delete trigger writes a durable retirement-queue row first; the
lifecycle repeatedly offers only currently unowned queue candidates to the
ordinary lease-aware generation retirement path. A refusal never consumes the
queue row. A concurrent route/claim that pins the generation also removes the
row atomically; if retirement reaches `retiring` first, the route/claim rejects
the generation and rebuilds or reschedules. This journal is what makes cleanup
survive both lost process-local backlog memory and checkout deletion through a
foreign-key cascade.

`RETENTION-1` applies to inactive cache with one accounting exemption:

- a currently routed commit generation is excluded from the 32-generation and
  5-GiB inactive allowance and cannot be quota-evicted;
- a ref, lower-layer/base, sealed dedicated/primary, reader/build/cache-claim
  lease, or other durable reference remains a deletion blocker, but does not
  create another count/byte-accounting exemption;
- for a non-routed candidate, effective inactivity begins at the maximum holder
  `last_selected`; shared holders do not duplicate count or bytes;
- graph and search bytes are measured separately against the same generation
  set, and quota selection is per referenced generation graph;
- expired/over-quota candidates are ordered by oldest effective selection time
  with generation ID as a stable tie break; and
- pruning first removes only expired/selected holder pins, then transactionally
  rechecks routes, refs, lower/base relationships, capabilities, leases, and all
  remaining pins before payload deletion. One holder's removal never invalidates
  another holder.

Ordinary same-graph route withdrawal or branch switch preserves the checkout's
inactive commit pins. Authoritative checkout forgetting cascades that holder's pins.
Explicit inaccessible-layer purge removes that checkout's pins with other
rebuildable state. Promotion/demotion/rehome atomically revokes that checkout's
old-graph pins and pins the newly routed commit in the new graph. Any authorized
graph deletion removes every pin for that graph. Primary/family forgetting
removes the primary dependency closure's checkout holders and graph payload,
while healthy independently dedicated graphs and their holders remain.

### Building behavior

**DECIDED BUILD-1:**

- If a prior ready generation exists, it may be served while the next generation builds only with `stale/building`, requested/actual revisions, and `exact=false` whenever it does not equal the newly resolved target.
- If this checkout/ref has never had a ready generation, serve the lower canonical view only with an explicit `building`/fallback rider and `exact=false`, unless the caller requests `require_fresh`, in which case wait up to a deadline or return retryable `view_building`. A deterministically missing local Git object returns `ref_not_available_locally`, not an endless building state.
- Never present fallback as the requested exact view.
- Missing-grace withdrawal is separate from cold-build fallback: once absence/inaccessibility is confirmed, no new request pins the checkout. Eligible read-only queries receive the labeled primary-base fallback described by `GRACE-ACCESS-1`; `require_exact` and mutations fail, and only already pinned exact leases finish.

### Cleanup completeness

`purge_inaccessible_layers` applies only when explicit ownership is intentionally retained. Automatic availability-grace expiry uses `forget_checkout` and reaches zero route/layer/checkout-identity/clock state while preserving the family primary and its unrelated state.

Cleanup is operation-typed; a generic “purge everything with this path” API is forbidden:

- `purge_inaccessible_layers(checkout_id, incarnation)` withdraws exact routing, unregisters checkout watchers/LSP/session bindings (including buffer-overlay sessions), and cancels builders. It detaches the checkout from shared commit generations and removes that checkout's durable cache pins; after lease drain it deletes checkout-owned dirty generations and their masks/search/sidecars. A commit generation is garbage-collected only when no checkout route, ref view, lower-layer reference, cache retention pin, reader/build/cache-claim lease, sealed-base reference, or other durable holder remains. The early candidate query, reference classifier, and final transactional delete predicate MUST all recognize pins so a concurrent successful route bind cannot race physical deletion. The operation preserves checkout/graph identities, every explicit intent/config record, and sealed dedicated full generations, and proves that no surviving reference was invalidated.
- `forget_checkout(checkout_id, incarnation)` additionally removes the checkout identity, dedicated graph identity, every owned generation and sidecar, routes/aliases/caches, every Gortex-owned intent/config/project membership, and finally its cleanup journal. Its postcondition is zero live logical state for that checkout.
- `retire_primary_closure(graph_id, primary_epoch, cause)` removes the primary plus every automatic checkout/ref/dirty generation, route, alias, cache, watcher/session binding (including buffer overlays), and derived payload whose lower chain depends on it — and every dependent automatic checkout identity, incarnation, and clock (**DECIDED PRIMARY-LIVE-IDENTITY-1**). Independent sibling `GraphID`, intent/config, full generations, routes, and unrelated owned payload remain unchanged. Bridge/cross-view/derived rows that reference the retired primary are invalidated or rebuilt under a new view generation. The family then enters `no_primary` or invokes `forget_family` when none survive.
- `forget_family(family_id, primary_epoch)` removes the final primary closure and every remaining logical family/catalog/config row, then deletes the journal last.

Every deletion mode covers its owned generation-keyed node/edge/file/mask rows; `generation_source_files` metadata; canonical search document/owner/posting/statistics and vector rows; ref facts, contracts, graph-generation sidecars; affected analysis generations/view caches; LSP registrations; CWD/scope/session bindings; pending/superseded builds; and unbounded metric labels. Logs and other forensic remnants explicitly remain outside the logical cleanup contract.

All cleanup is idempotent, retryable, bounded, lease-aware, and protected by checkout incarnation plus `primary_epoch`. A crash between logical retirement, external-config mutation, and physical row deletion resumes the same saga. Completion is reported only after the operation-specific integrity query passes and the cleanup journal has deleted itself.

## Security and trust boundaries

- Automatic discovery is limited to families derived from explicit intent.
- A worktree outside a tracked root must prove the same Git common-directory identity before it is authorized.
- Ref names and OIDs are validated and passed through argument-safe Git helpers with option termination.
- Git content and target-tree configuration are untrusted input. Config keys are schema-classified as `source_safe` (pure admission/ignore/language/resolution semantics) or `host_trusted` (credentials, embedding provider/API URL/model download, plugins, commands/toolchains, network/process access). An inactive ref may change only `source_safe` values.
- **DECIDED REF-CONFIG-TRUST-1:** inactive-ref indexing is fully offline. Source text from an inactive ref is never sent to a remote provider — even one already approved by trusted policy; vector search for an inactive ref uses an approved local provider, or trusted policy disables the capability for the profile. `host_trusted` values come only from already trusted user/global/project policy outside the selected tree. Target-tree `host_trusted` keys are deterministically removed from the effective configuration, each emits stable `untrusted_config_key_ignored` diagnostics, and they cannot trigger SSRF, source exfiltration, model download, plugin loading, or command execution. When trusted policy enables vectors, local-provider unavailability or refusal blocks `inactive_ref_structural_v1` readiness and returns `required_capability_incomplete` (or `capability_unavailable` when policy forbids the capability); prewarm never prompts, downloads, or changes policy. `disabled_by_config` is reserved for trusted policy that disables vectors.
- Git content receives the same file-size, language, ignore, and resource limits as filesystem indexing, and the sanitized effective configuration is part of the generation fingerprint.
- V1 inactive-ref construction performs no explicit, automatic, lazy, or promisor-object network fetch.
- No automatic Git hook installation is allowed.
- No checkout, prune, remove, or ref mutation is allowed as part of indexing.
- Symlink and case-folding behavior must use canonical path-identity helpers and preserve repository confinement.
- Generation and route keys are opaque internal IDs; raw ref names and user paths are never used as SQL identifiers or storage partitions.

## Observability

Metrics/logs should include bounded identifiers and these events:

- family inventory duration and discovery lag;
- checkout add/move/inaccessible/prunable/authoritatively-removed/recover transitions and the evidence class used;
- all active explicit-intent sources and reassertion conflicts;
- `unavailable_since`/availability deadline and `removal_detected_at`/removal deadline, expiry, blocked leases, and operation-typed cleanup completion;
- primary-base dependents, preserved independent siblings, `no_primary` transitions, cascade decisions, and rebuild outcomes;
- commit/dirty generation build duration and route;
- changed files versus invalidation-closure files;
- sparse bytes versus estimated full-graph bytes;
- cache hit/miss for branch A→B→A;
- generation publication, supersession, failure, and retirement;
- route switch latency;
- active generation leases and GC delay;
- duplicate watcher events collapsed;
- fallback/stale/incomplete query counts;
- view-aware search and analyzer latency;
- base compaction/rebase counts.

Logs must make it possible to answer: “Which exact graph/layers did this query use?”, “Why is this view stale?”, “Which evidence classified this checkout as inaccessible or removed?”, and “Why has this generation not been deleted?” Historical logs may retain checkout/path/cleanup events according to ordinary logging retention; logical forgetting does not rewrite them. Metrics MUST avoid unbounded checkout/path labels.

## Proposed performance targets

**DECIDED SLO-1:** these values are approved as benchmark hypotheses only; final targets are set after the Phase 3 shared-database prototype, measured on Gortex, VS Code, and Linux-scale fixtures.

- Cached branch/view selection: p95 below 100 ms, excluding client reconnect.
- Ordinary `git worktree add/remove` discovery and routing withdrawal: p95 below 2 s when events are delivered. Graph-data deletion still waits for the configured grace and active lease drain.
- Cold sparse build work: proportional to changed files plus measured invalidation closure; no unconditional full repository parse.
- Point graph lookup overhead for a two-layer view: p95 less than 15% over base-only lookup.
- Search overhead for a two-layer view: p95 less than 25% after cache warmup.
- Atomic route publication: no partially visible generation under fault injection.
- Storage: no full graph/search payload copy for a sparse checkout/branch layer; report graph and search owner bytes and delta/full ratios separately.
- A→B→A with an unchanged valid A generation: zero reparsed files on the second selection of A.

A layer whose affected closure approaches the whole repository may legitimately approach full size/time. The system must report this honestly rather than violating correctness to preserve a percentage target.

## Implementation plan and gates

### Phase 0: correctness baseline

- Add regression tests for duplicate overlay IDs, point/bulk edge equivalence, idempotent layer insertion, base search-bundle leakage, and base-only stats.
- Strengthen current worktree tests so “coalescing” assertions verify result/metadata counts, not only name shape.
- Add end-to-end cleanup tests for config, watcher, LSP, search, analysis, and daemon restart.

**Gate:** current transient overlay semantics are internally consistent and documented.

### Product-decision gate

Decision round 4 (2026-08-15) resolved every previously open item; the gate now records which decisions each phase implements rather than blocking on them:

- Phase 1/4 lifecycle publication implements `REMOVAL-EVIDENCE-1`, `AUTOMATIC-GRACE-EXPIRY-1`, `PRIMARY-DESIGNATION-1`, `PRIMARY-LIVE-IDENTITY-1`, and `UNTRACK-BLOCKED-1`;
- Phase 2 request precedence implements `API-1`, the session-scope composition rules, and the hook front door (`HOOKS-VIEW-1`);
- Phase 3 source schema/migration publication implements `SOURCE-DURABILITY-1` (no persisted bytes) and benchmark-gates `SEARCH-AUTHORITY-1`;
- Phase 5 profile/security publication implements `REF-CONFIG-TRUST-1`, `REF-SCOPE-1`, `RETENTION-1`, `UNBORN-1`, and the V1 `CROSSREPO-1` pinning/bridge contract;
- Phase 6 policy implements `BASE-3` and `ANALYSIS-1`; `SLO-1` values remain benchmark hypotheses until measured.

If implementation evidence contradicts a decided branch, this specification must be revised with the selected alternative and its tests before the affected phase passes.

### Phase 1: identities and lifecycle coordinator

- Add family, checkout, graph, mode, generation, and incarnation identities.
- Normalize every accepted intent source into provenance-preserving `tracking_intent` records; explicitness is their union while the checkout exists.
- Enforce at most one primary graph per live family and a primary epoch for stale-cleanup protection.
- Add distinct durable availability/removal evidence and deadlines plus cleanup/publication sagas that coordinate SQLite with external config files.
- Introduce one daemon-owned `CheckoutReconciler`, conservative removal-evidence classification, operation-typed cleanup, family retirement, and integrity checks.
- Route CLI, MCP, reload, startup, auto-index, and GC through it.
- Preserve explicit identity/config/full generations across inaccessibility; retain automatic checkout identity only through its availability deadline and then forget it; prevent uncertain/prunable evidence from deleting explicit ownership; prevent authoritative-removal cleanup or stale epochs from recreating/deleting the wrong incarnation.

**Gate:** every lifecycle entry point has identical watcher/config/cache cleanup behavior.

### Phase 2: request-scoped graph views

- Introduce `GraphView`, `WorkspaceView`, view leases, roots, freshness, and completeness.
- Replace request-time direct `s.graph` reads.
- Split read projections from mutable store APIs.
- Key query/analyzer caches by `ViewID`.
- Make existing buffer overlays compose over the selected lower view.
- Route hook/control-socket probes through the same view resolution and reconcile-then-enforce posture (**DECIDED HOOKS-VIEW-1**), and implement selector-versus-ceiling composition for session scopes.

**Gate:** a static audit finds no unapproved base-store bypass in user queries.

### Phase 3: immutable sparse generations

- Introduce the canonical vNext payload schema, table-ownership manifest, graph/analysis generation domains, composite keys, and generation-leading indexes.
- Build owner-bound store handles; mechanically reject unscoped request/mutation SQL while allowlisting typed cross-owner administration.
- Capacity-preflight the accepted migration; implement adjacent store epochs, atomic active-epoch swap, old-reader drain, crash recovery, and the identical offline cold-build result.
- Implement masks, atomic route publication, recovery, leases, bounded generation-row GC, and cross-generation corruption tests.
- Adapt incremental extraction/invalidation to write unpublished immutable generation rows.
- Implement the decided source contract (**SOURCE-DURABILITY-1**): `ContentSource` with `FilesystemSource`/`GitTreeSource`/`LayeredSource`, generation-keyed source metadata, commit-only file reads through the local object database, and atomic file-read capability withdrawal when objects are missing.
- Implement exact composed-view FTS/content/vector search with visible-corpus statistics rather than per-generation top-k or global BM25 filtering.

**Gate:** composed views are equivalent to a separately full-indexed reference tree for the supported completeness profile.

### Phase 4: automatic worktree views

- Implement NUL-safe authoritative family inventory.
- Add family/checkouts watchers plus periodic reconciliation.
- Implement CWD view selection and correct filesystem paths.
- Build/rebuild dirty layers.
- Build per-checkout trigram searchers and per-checkout LSP workspaces under the global concurrency cap (**DECIDED TEXT-SEARCH-VIEW-1 / LSP-SCOPE-1**).
- Implement distinct availability/removal grace, immediate exact-route withdrawal, labeled read-only base fallback only while the requested checkout identity remains registered during grace, and explicit failure for exact/filesystem/mutating operations.
- Persist both clocks/evidence through restart; recover an automatic identity only before its availability deadline, forget it at expiry, and create a new incarnation on later discovery. Retained explicit/dedicated ownership follows `INACCESSIBLE-1` instead.
- Implement `purge_inaccessible_layers`, `forget_checkout`, `retire_primary_closure`, and `forget_family` with their distinct postconditions, external-config saga recovery, and stale incarnation/primary-epoch guards.
- Preserve independent dedicated siblings and their ref views on primary loss, forget dependent automatic checkout identities with the closure, enter `no_primary` without auto-election, disable automatic checkout routes, and reject only ref views rooted in the lost primary.
- Implement promotion, all-intent-source non-primary demotion, previewed primary/sole-primary untrack transactions, and previewed `set-primary` designation (**DECIDED PRIMARY-DESIGNATION-1**).

**Gate:** the full worktree lifecycle/evidence matrix passes without full-copy storage, restart-reset grace, unintended explicit-state loss, sibling-dedicated loss, or any automatic route surviving without a valid primary.

### Phase 5: checked-out tree and explicit-ref caching

- Cache immutable tree generations encountered by checked-out worktrees with
  durable holder pins and make A→B→A reuse the same generation across warm
  restart and compatible checkout/ref aliases.
- Ship the additive schema-v21 pin migration/backfill before startup retirement;
  make route bind, lease consumption, old/new holder timestamps, and route/HEAD
  publication one compare-and-set transaction.
- Persist every final-pin deletion in a generation-keyed retirement queue in
  the same transaction, including checkout/graph foreign-key cascades. Consume
  that queue during ordinary runtime cleanup, and reserve generic READY-layer
  orphan discovery for Seed before build admission so an off-route generation
  in the build-to-publication window cannot be reclaimed.
- Use one owner-independent ready-generation identity and a zero-write settled
  route fast path; owner/layer/path/ref labels never cause claim/release or WAL
  churn.
- Enforce the 7-day/32-generation/5-GiB per-graph retention policy with only
  currently routed commits excluded from inactive count/bytes, shared-holder
  accounting, and pin-aware transactional retirement guards.
- Ensure unrelated inactive-ref changes schedule no indexing work.
- Add local-only explicit-ref selection/prewarming with direct base→target tree diff, build-time `GitTreeSource`, and no tracking/config/checkout side effects.
- Implement desired/active `ref_views`, coalesced CAS-guarded `ref_view_builds`, cold descriptors, stable errors, LRU retention, and `inactive_ref_structural_v1`.
- Apply **DECIDED SOURCE-DURABILITY-1**: no persisted source bytes; ready inactive-ref file reads use the local object database with atomic capability withdrawal when objects are missing.
- Enforce **DECIDED REF-CONFIG-TRUST-1** (fully offline inactive refs) plus the fixed host-trusted-key isolation; fail missing-object/promisor builds without an exact route, LSP, checkout, or Git fetch.
- Record per-foreign-repository generation pins for baked cross-repository edges and build view-keyed bridge generations where selected pairings diverge (**DECIDED CROSSREPO-1**).
- Handle detached/unborn HEAD, commit-sensitive producers, ref movement on next access, non-storming base advancement, and unrelated histories.

**Gate:** A→B→A reuse across warm restart and aliases with identical commit
generation IDs, zero reparsing/physical cache-hit builds, and below-100-ms p95
selection; zero-write settled polling; schema-v21 upgrade/backfill before
startup retirement; crash-safe pin-retirement handoff and publication-window
protection; profile-compatible tree sharing with correct commit
metadata; full structural/search differential equivalence; the decided
source-withdrawal behavior; no-fetch/no-hidden-worktree/offline-config-trust
behavior; capability-scoped completeness; cross-repository pinning; pin-aware
retirement races; build coalescing/CAS; and zero idle-ref work all pass under
concurrent events.

### Phase 6: analyzer expansion and optimization

- Add or optimize exact view support producer-by-producer; unsupported producers remain explicitly unavailable/incomplete.
- Extend cross-repository bridge generations beyond the V1 pairings.
- Explore inactive-ref LSP/virtual-filesystem support post-V1; it is not a V1 acceptance gate.
- Benchmark and tune shared-SQLite generation indexes, writer contention, WAL growth, bounded GC, and composition.
- Add pattern/eager prewarming and automatic compaction policies only after evidence; explicit single-ref prewarm already ships in Phase 5.

**Gate:** no tool silently returns base-only data for a selected non-base view, and capability truth remains operation-specific.

## Validation strategy

### Equivalence oracle

The strongest correctness test is differential:

1. Create a base repository and full-index it.
2. Create a target branch/worktree with a controlled change.
3. Build the sparse composed view.
4. Independently full-index the exact target tree into a disposable graph.
5. Normalize view-only metadata.
6. Compare visible nodes, node payloads, outgoing and incoming edges, file sets, ordered search results plus normalized scores, and supported analyzer outputs.

Property tests should generate combinations of add, modify, delete, rename, symbol rename, import change, target deletion/recreation, and dependency changes. Any composed operation must match the fully materialized reference.

For an inactive-ref oracle, both sides use the exact `inactive_ref_structural_v1` profile with LSP/workspace producers disabled. The composed Git-tree view is compared to an independent full index of the same target tree and same source/config/profile fingerprint; this tests structural completeness without pretending unavailable LSP capabilities are present.

### Required graph tests

- replacement/add/delete/rename matrix for every `GraphView` method;
- explicit tombstone versus absence/fallback;
- three and four stacked layers;
- same ID re-emitted multiple times;
- file replacement that removes only some symbols;
- unchanged-source edge to retained, renamed, and deleted targets;
- dependent file with inherited nodes and replaced edge set;
- point/batch/bulk node and edge equivalence;
- exact stats or explicit incomplete response;
- cross-repository edge masking and rebuild;
- producer completeness propagation;
- file replacement/deletion integrity proves `generation_source_files` is the sole content/mode authority and mask rows cannot carry or diverge from payload.

### Required search tests

- overlay-only symbols in FTS and vector candidates;
- hidden/deleted base symbols absent from every search mode;
- replaced symbol returns only the overlay payload;
- stale base bundles and edges cannot leak;
- same logical symbol keeps only the highest visible payload, while unrelated overlay/base matches share one deterministic relevance order;
- inserting arbitrary `building`, cached, `retiring`, or unrelated ready generations leaves the selected view's candidates, exact scores, ordering, and top-k unchanged;
- adversarial document-frequency/candidate-retrieval fixtures prove a true selected composed-view top-k result cannot be truncated before exact visible-corpus scoring/reranking;
- identical `logical_node_id` search-owner rows in two generations remain isolated when one generation is rebuilt/deleted;
- the same `document_id` in symbol/content corpora remains isolated by `(generation_id, search_kind, document_id)`, and a mismatched-kind owner/posting fails its composite foreign key;
- incompatible embedding fingerprints do not merge raw scores;
- content search respects file tombstones;
- result paths use the selected worktree root.

### Required lifecycle tests

- topology resource coverage includes `TestGitWatcherTopologyProbeStableTickDoesNotReinventory`, `TestGitWatcherTopologyProbeDetectsFirstDirectoryTransitionsPromptly`, `TestGitWatcherTopologyProbeStopCancelsAndJoinsInventory`, `TestGitWatcherTopologyRegistrationsStayBoundedWithLargeCheckout`, and `TestGitWatcherRefWatchesUseOnlyExactFiles`;
- a 2,000-file source tree is exercised under repeated worktree add/remove/move, dynamic admission, promotion, owner recovery/handoff, missed-event/overflow fallback, and teardown; descriptor/watch counts remain independent of file count, authoritative inventory converges every change, and teardown leaks zero registrations;

- `git worktree add`, `remove`, `move`, `lock`, `unlock`, `prune`, and manual directory deletion;
- an evidence/action matrix separately covers authoritative inventory omission, still-listed/prunable records, inaccessible common directory, deleted local path, unavailable mount, permission denial, I/O failure, explicit `forget`, and each `untrack` branch;
- ambiguous evidence and `ENOENT` without trustworthy mount/inventory context classify as inaccessible, never removed;
- dedicated inaccessibility immediately withdraws exact routing, preserves intent/config/`CheckoutID`/`GraphID`/sealed full generation, and after deadline deletes only rebuildable checkout layers;
- automatic inaccessibility immediately withdraws exact routing, serves eligible labeled fallback only during grace, and at the deadline forgets the checkout identity, route, clocks, and rebuildable layers without deleting the surviving primary;
- inaccessible-primary behavior causes no family cascade; its sealed base may serve only the labeled read-only fallback, and independent dedicated siblings remain unchanged;
- recovery before expiry reuses an automatic checkout identity, revalidates state, rebuilds discarded layers, and restores exact routing; after automatic expiry the stale ID fails and later discovery creates a new incarnation, while explicitly retained dedicated ownership continues to reuse its preserved checkout/graph identities;
- authoritative non-primary removal/`forget` and previewed inaccessible-non-primary untrack leave zero checkout graph/identity/intent/config/cache/journal rows and later discovery creates new identities; live accessible non-primary untrack is tested separately as demotion;
- primary removal/untrack deletes exactly the primary dependency closure when an independent dedicated sibling survives, preserves that sibling's identities/config/intents/full generations/routes/unrelated payload, and invalidates only bridge/cross-view/derived rows that reference the retired primary; sole-primary authoritative removal is separately verified to run terminal family forgetting;
- primary loss with surviving dedicated siblings enters `no_primary`, elects nobody, rejects automatic checkout and lost-primary ref routing, but preserves/selects ref views rooted in surviving ready dedicated graphs;
- explicit `set-primary` validates a ready full generation, increments `primary_epoch`, rebuilds live automatic checkout layers off-route before publishing, and rejects a non-ready or non-dedicated `graph_id`; no automatic path ever designates a primary;
- cause-aware primary cleanup covers removed, accessible-untracked, and inaccessible-untracked primaries: whatever the cause, every dependent automatic checkout identity, incarnation, clock, route, and layer is deleted with the closure; independent dedicated siblings keep identities, clocks, and evidence untouched; sole-primary removal/untrack and explicit family forget delete every family identity;
- inaccessible cleanup detaches shared commit generations, deletes checkout-owned dirty/buffer state, and never invalidates another route/ref/lower-layer/cache/lease;
- sole-primary untrack shows a family-forget preview and leaves zero logical family/config state after lease-aware saga completion;
- live accessible non-primary untrack demotes after all intent sources are revoked; inaccessible non-primary untrack uses previewed `forget_checkout` rather than demotion; untrack of an accessible non-primary with no different ready primary fails with the exact blocker and leaves intent and routing unchanged (**UNTRACK-BLOCKED-1**);
- an explicitly retained long-inaccessible checkout, or an automatic checkout that gains positive removal evidence before its availability deadline, receives a fresh removal clock; reappearance before that removal deadline cancels automatic removal and restores ready or prior availability state;
- daemon restart preserves distinct availability/removal evidence and deadlines and resumes the correct cleanup mode;
- schema-v20→v21 startup creates pin keys/FKs/indexes, conservatively backfills
  all eligible checkout-owned ready/superseded commits plus every exact routed
  commit without applying migration-query quota caps, then Seed immediately
  applies retention bounds before ordinary retirement; it performs zero payload
  writes/parses/builds, commits `user_version=21` last, and rolls back/retries
  cleanly after failures before each migration step;
- lifecycle Seed preserves pinned non-routed commit generations before
  coordinators exist while retiring an otherwise identical unpinned orphan;
- promotion, demotion, rehome, checkout forgetting, dedicated-graph deletion,
  primary closure, and family forgetting leave exactly the holder pins allowed
  by their authorized checkout/graph deletion closure; independent checkout/ref
  holders and independently dedicated graphs survive;
- a common-directory inventory failure applies family availability handling, a root-only failure affects one checkout, and neither is absence; a pending zero-source family remains inventory-scoped through restart;
- missed filesystem events are repaired by periodic authoritative inventory without treating a failed inventory as absence;
- the recorded-volume/prunable evidence algorithm persists root/common-directory volume kinds/tokens and nearest-ancestor evidence, survives restart, distinguishes the same reachable mount from an unavailable or changed volume, and never treats a different parent volume as proof; a terminally forgotten prunable/inaccessible record remains ephemeral until genuinely accessible;
- linked `.git` indirection, submodule exclusion, checkout outside the canonical parent, locked removable storage, and worktree moves are covered;
- rapid authoritative remove/re-add at the same path uses a new incarnation and cannot be deleted by stale work;
- promotion during an active query and promotion build/config failure rollback;
- demotion/forget with one and multiple active intent sources; last-intent removal by config reload safely demotes an accessible non-primary only when a different ready primary exists, and enters `intent_change_pending` for primary, inaccessible, `no_primary`, or alternate-primary-unready cases; desired/effective modes, source fingerprint, and prior availability state survive restart; a sole-source pending family remains discovery-scoped, zero active sources cannot auto-reclassify it, restored intent cancels pending state, and reload cannot silently re-promote after confirmed revocation;
- `intent_change_pending` is exercised through access failure, availability expiry, recovery, positive removal evidence/removal grace, source restoration, and confirmation races; incarnation/transition guards prevent stale restoration or confirmation from overwriting newer evidence;
- primary closure while a dependent automatic checkout has unexpired independent removal grace deletes that identity with the closure; a `no_primary` family allocates no durable automatic identities, and later designation observes surviving worktrees as new incarnations;
- non-revocable intent or config-write failure leaves intent and routing in the prior coherent state;
- each cleanup mode performs its exact watcher/LSP/search/analysis/session/config/payload postcondition, including canonical search tables;
- labeled fallback excludes dirty/buffer content, reports requested versus actual view and `exact=false`, and rejects exact/root-file/write requests while the requested checkout identity remains registered during grace; after automatic forgetting a stale explicit ID fails without capturing selector-free/default or explicit-base requests, which continue against the surviving primary; no ready primary returns a stable error and never borrows an independent sibling;
- a hook probe naming a file in a known family's unreconciled worktree triggers immediate reconciliation and fails open until the view is ready, then enforces; during grace, hook probes follow labeled-fallback rules and never present fallback evidence as exact;
- cleanup crash/restart at every SQLite/external-config saga phase;
- stale checkout incarnation/`primary_epoch` cannot delete recovered, preserved-sibling, or newly tracked state;
- an active query lease beyond either deadline delays physical row deletion without keeping new exact routing active;
- terminal logical cleanup does not promise deletion of logs, WAL/free pages, metrics history, backups, or snapshots.

### Required Git/ref tests

- A→B→A cache reuse and zero work for updating/adding/deleting/moving an unrelated inactive ref;
- after building A and B, a warm store reopen followed by B→A reuses the same A
  and B immutable generation IDs with zero physical builds and zero reparsed
  files; changing HEAD from A to already-cached B while the daemon is stopped
  adopts B, retains A, and reaches ready rather than leaving startup pending;
- two checkouts and one ref may hold one compatible generation: deleting holders
  one at a time preserves payload until the final route/ref/lower/lease/pin is
  gone, and a concurrent pin bind always defeats the final transactional
  retirement predicate;
- route-bind success atomically consumes its claim lease, pins/restamps old and
  new commit generations, and advances one route epoch; stale incarnation,
  root, graph/base, or expected-route comparison changes no pin/timestamp/route
  and does not consume the lease;
- withdrawing `source.snapshot` from a pinned A, switching to B, and returning
  to A rebuilds or adopts a newly compatible A rather than routing the withdrawn
  generation; restoring a compatible candidate returns to normal cache reuse;
- retention boundaries cover exactly 7 inactive days, 32→33 inactive unique
  generations, and graph/search byte thresholds around 5 GiB; live
  route/base/ref/lower/lease protection, shared-holder single accounting,
  deterministic oldest/tie-break eviction, and per-graph isolation are proved;
- stable exact reconciliation of a locally built, checkout-adopted, ref-adopted,
  and sibling-adopted commit performs zero ready claims, lease operations, pin
  timestamp writes, route/HEAD writes, WAL growth, and physical builds;
- repeated settled reconciliation after canonical commit adoption preserves both the dirty generation ID and route epoch; one real dirty edit rebuilds exactly once and subsequent settled polls stabilize;
- existing metadata `ref` filtering remains byte-for-byte compatible; combining it with structured `view` either selects a graph in the filtered set or returns `selector_conflict`, never reinterprets either field;
- the structured `view` selector applies `REF-SCOPE-1`, rejects short names/revision expressions, peels tags only to commits, validates exact OIDs/object format, returns stable selector errors, and accepts any ready dedicated `graph_id` including a surviving non-primary graph in `no_primary`;
- explicit selection/prewarm creates only `ref_view`/evictable generation state—no tracking intent, checkout, dedicated graph, project/global config, hidden worktree, HEAD/index change, hook execution, or Git ref/object/promisor network access; vector enrichment, when enabled by trusted policy, uses only an approved local provider (**REF-CONFIG-TRUST-1**);
- a ready inactive-ref view is differentially equivalent to a full index of the exact target tree under `inactive_ref_structural_v1`, including visible nodes, both edge directions, FTS/content/configured-vector search, and required structural sidecars;
- ref-only symbols are searchable, while deleted/replaced lower symbols/files are masked in every search mode;
- source-safe `.gortex.yaml`, ignore/generated rules, manifests, workspace files, modes, and symlinks come from the selected Git tree, while every target-tree host-trusted key is omitted from the effective fingerprinted config, emits `untrusted_config_key_ignored`, leaves the build using external trusted policy, and cannot change API URLs, leak source, download models, load plugins, or execute commands;
- fixed V1 configuration rejects fetch/eager-ref/LSP claims and any configuration that requests remote embedding for inactive refs;
- missing ref/commit/tree/blob/promisor objects set the build `failed`, return `ref_not_available_locally`, publish no exact/incomplete route, and trigger no explicit or lazy fetch;
- concurrent identical selection/prewarm requests produce exactly one unique profile-specific `ref_view` row and one build token; within that route, a mid-build change to indexing config, extractor set, resolver version, target tree, or base changes the complete build fingerprint and makes the old completion `superseded`; a different enrichment profile owns a separate `ref_view`, may coexist, and can never satisfy the original profile's request;
- C1→C2 ref movement to a different commit with the same tree during a build adopts the parsed generation only after current-epoch/full-fingerprint CAS revalidation, reports C2/ref metadata, and performs no second parse;
- a moved requested ref re-resolves on next access while the old OID generation remains immutable; if the new target/object is missing, desired state fails while the old ready generation remains only a labeled `exact=false` fallback; idle movement triggers no proactive indexing;
- cold and stale/failure descriptors exercise the cross-product that can occur: requested `building|failed|ready` versus actual `none|ready|stale`; every case reports the split `requested_state`/`actual_state`, requested/actual view and OIDs, build token where applicable, capabilities, fallback reason, and truthful `exact`;
- structural search with all required structural producers complete succeeds even though LSP capabilities are unavailable;
- when trusted policy enables vector search, local-provider refusal/unavailability publishes no exact `inactive_ref_structural_v1` route, preserves at most a labeled fallback, returns `required_capability_incomplete` or `capability_unavailable` according to policy, and never reports `disabled_by_config`; trusted vector disablement alone reports `disabled_by_config`;
- LSP-only operations return `capability_unavailable`, start no LSP, and never borrow checkout/base LSP facts;
- `require_complete` expands only operation-default producers; structurally complete search succeeds, required unavailable/incomplete capabilities fail with the stable mapped error, and missing `optional_capabilities` only annotate the rider;
- same-tree aliases reuse only a profile-compatible structural generation or isolated subset; two different commits with the same tree still report their own route revision and never share commit-sensitive blame/churn/release/history output incorrectly;
- ready file reads route through the local Git object database via `(ViewID, RepoPrefix, relative_path)`/`gortex-view://` and never leak a base-checkout path; they survive ref deletion and daemon restart while objects remain, and after aggressive object pruning the read fails `source_object_missing` and the view's file-read capability is atomically withdrawn without disturbing graph/search capabilities;
- a generation's cross-repository edges record their foreign generation pins; a workspace view selecting a different foreign generation either routes through the bridge generation built for that pairing or reports the capability incomplete — never edges resolved against another revision;
- `search_text` on a worktree view searches that checkout's `ViewRoot` bytes; on a commit-only view it returns `capability_unavailable`; it never serves the canonical checkout's bytes for a non-base view;
- LSP rename planning/code-action discovery fail with `capability_unavailable`, while applying them and every filesystem mutation fail with `view_read_only`;
- explicit-selector/CWD behavior follows the resolution of `API-1` and never silently substitutes CWD for a returned exact explicit view;
- default/overall search never leaks symbols from unrelated inactive refs;
- a same-branch commit in a linked worktree is detected promptly rather than only by periodic polling;
- two branch names at the same OID switch selector identity with zero parsing and reuse one compatible content generation;
- the same branch in two worktrees retains distinct dirty states, while explicitly selecting it as `git_ref` always returns only the shared committed tree;
- detached HEAD and unborn/orphan HEAD;
- checked-out reset, rebase, force update, packed/nested refs, and reftable-compatible authoritative reconciliation;
- base/ref movement during a layer build supersedes stale publication; advancing a base with many idle ref caches triggers zero eager ref rebuilds, preserves exact old-base caches until retention eviction, and rebuilds lazily on selection when needed;
- unrelated histories and near-full deltas remain exact and are reported honestly.

### Required concurrency/fault tests

- concurrent query during generation publication;
- process crash at every publication step;
- stale build completion after checkout removal;
- stale build completion after path reuse;
- watcher overflow and event storms during checkout;
- simultaneous base advancement and dirty-file edit;
- generation-row GC while a search/analyzer request is leased;
- large bounded generation deletion concurrent with builds and reads;
- shared-database writer contention, wider generation-leading index cost, and WAL growth under several worktree builds;
- sufficient-capacity adjacent-epoch shadow migration: rollback before cutover, normalized verification, atomic active-epoch swap, and fallback coherence;
- old-epoch index/catalog mutations during shadow construction and exactly at cutover are journaled with monotonic revisions, replayed/resampled under the final writer barrier, and either appear in vNext or abort the swap—none are lost;
- an in-flight old-store reader spans cutover while new requests use vNext; old handles/files drain before DB/WAL/SHM deletion;
- crash after epoch swap but before old-store cleanup recovers vNext as active and completes cleanup without reopening legacy for new requests;
- disk capacity disappearing during shadow construction aborts before cutover and does not silently switch to cold mode;
- capacity preflight separately exercises online-shadow headroom and rollback-preserving offline-cold headroom; when only the latter fits, cold rebuild has explicit offline/reporting/restart behavior and converges on identical results, while insufficient space for both returns `migration_blocked_insufficient_space` with the old epoch byte-for-byte authoritative and no destructive attempt;
- fresh-init, shadow, and cold paths have normalized `sqlite_schema`, keys/FKs/indexes/search configuration/`user_version`, graph/search data, ordered query results, and scores;
- post-drain introspection/filesystem checks find zero legacy, permanent `view_*`, per-generation FTS, `_vnext`, old-epoch, shadow DB, WAL, or SHM payload artifacts;
- deliberately omitted owner scope in test-only request reads, inserts, upserts, updates, joins, evictions, purges, backfills, search-owner maintenance, and deletes is caught mechanically; allowlisted cross-owner audit/GC/migration scans are typed by the ownership manifest and can mutate only in a subsequent owner-scoped step;
- query-plan fences cover every forced generation-leading index;
- ready generation rows without a route after crash;
- route pointer transaction absent after payload completion;
- route pointer can never reference a non-ready or incomplete generation.

## Acceptance criteria

Availability acceptance is mode-specific: explicit/dedicated inaccessibility preserves intent/config/identity/sealed full generations while withdrawing exact routing; a disposable automatic identity provides labeled fallback only during grace and is forgotten at the deadline. After forgetting, its stale explicit ID MUST fail `checkout_inaccessible` and MUST NOT capture selector-free/default or explicit-base requests, all of which continue against the surviving primary. Topology observation MUST also stay independent of source-file count, remain at or below 128 attributable descriptors for the 2,000-file fixture, and return registrations to baseline after admission/recovery/teardown.

The feature is complete when all of these are true:

1. Explicit tracking creates a full logical graph identity for a main or linked worktree while that checkout state exists.
2. An implicit sibling worktree is discovered automatically from a known family and never creates an unconditional full graph copy.
3. Every live family with automatic routes has exactly one primary base, and every automatic route references a ready lower primary generation; terminal cascade may leave no family at all.
4. Initial full generation is exactly the sampled HEAD tree; staged, unstaged, and eligible untracked state is visible only through the dirty layer.
5. CWD and explicit selection route every graph/search/path operation to the same coherent request-pinned view.
6. The visible composed graph matches an independent full index of the target for all declared-complete capabilities.
7. Deleted/replaced data never leaks from a lower layer; the highest payload wins identity/ownership conflicts, while unrelated visible overlay/base results share one relevance ordering.
8. New overlay symbols participate in normal search, and unselected cached generations cannot influence result identity or ranking statistics.
9. Cached A→B→A returns to the same immutable A generation with zero reparsing
   and zero physical commit builds when fingerprints remain valid, including
   across warm daemon restart and compatible aliases. Stable exact polling
   performs zero cache-claim, lease, pin, route, HEAD, or WAL writes; unrelated
   inactive refs cause zero work.
10. A new generation is published atomically and old requests remain consistent.
11. Authoritative disappearance/`forget` (and destructive inaccessible non-primary checkout untrack) plus lease drain leave zero logical checkout graph/identity/intent/config/cache/journal state; later discovery gets new identities. Live accessible non-primary untrack instead retains `CheckoutID` and demotes. Logs, WAL/free pages, metrics history, backups, and snapshots are outside logical erasure.
12. Availability and removal evidence/deadlines remain distinct across restart. Mere inaccessibility preserves explicit intent/config/identity/sealed dedicated generations and later recovers the same IDs. Automatic availability-deadline forgetting and authoritative terminal forgetting both create a new incarnation on later discovery.
13. While the requested checkout identity remains registered during availability/removal grace, eligible new reads receive a truthful read-only primary fallback with requested/actual view and `exact=false`; dirty/buffer/root content is excluded, exact/filesystem/mutating requests fail, and existing exact leases alone may finish. Automatic fallback ends when its identity is forgotten; any post-grace fallback for explicitly retained ownership is governed separately by `INACCESSIBLE-1`.
14. Primary loss elects no replacement and deletes exactly the primary dependency closure — including every dependent automatic checkout identity, incarnation, and clock (**PRIMARY-LIVE-IDENTITY-1**). Independent sibling identities/intents/config/full generations/routes/unrelated payload survive; bridge/derived references to the lost primary are invalidated. A new primary exists only after explicit previewed `set-primary` (**PRIMARY-DESIGNATION-1**). Sole-primary authoritative removal/untrack runs family forgetting, so the family and all remaining identities disappear.
15. Explicit untrack atomically demotes a live accessible non-primary checkout after revoking all intent sources only when a different ready primary exists; otherwise it fails with the exact blocker and leaves intent and routing unchanged (**UNTRACK-BLOCKED-1**). Last-intent loss observed by config reload enters `intent_change_pending` only for a primary, an inaccessible checkout, or a non-primary without a different ready primary; otherwise reload demotes it safely. Primary untrack requires a cascade preview; sole-primary untrack is family forget, with no partial mutation.
16. Every lifecycle entry point invokes the correct typed cleanup and proves its watcher/intent/config/search/LSP/analysis/session/payload postcondition.
17. No analyzer silently treats base-only or checkout-LSP data as the selected view; completeness is capability- and operation-specific.
18. Explicit inactive refs are V1, local-only/read-only committed-tree views implementing the finalized `inactive_ref_structural_v1`; they never Git-fetch/create a checkout/intent/config entry, enforce fully offline target-config trust (**REF-CONFIG-TRUST-1**), follow the decided **SOURCE-DURABILITY-1** withdrawal contract (object-database reads, atomic file-read withdrawal on pruned objects), fail missing-object builds without an exact route, preserve metadata-`ref` semantics, and report LSP capabilities unavailable.
19. One canonical shared-database schema has one representation per payload kind. Graph/search/mask/producer rows bind `generation_id`; composed analyses bind `analysis_generation_id`/`ViewID`; transient caches bind `ViewID`; source metadata is generation-keyed payload with no persisted bytes. No per-graph/view/generation database or search-table family and no permanent legacy-plus-`view_*` schema exists.
20. Adjacent-epoch shadow and offline cold-rebuild paths are recoverable and converge on identical normalized schema/data/query results. Shadow cutover uses a revisioned mutation journal plus final writer barrier so no old-epoch mutation is lost. Request-serving and mutation operations are owner-scoped; only typed allowlisted audit/GC/migration discovery may scan across owners, followed by owner-scoped mutation. The later additive schema-v21 pin migration is catalog-only, conservatively backfills all eligible checkout-owned ready/superseded commits plus every routed commit without query-time quota caps, lets Seed immediately enforce retention bounds before ordinary retirement, writes `user_version=21` last, and never triggers graph payload migration or repository indexing.
21. Explicit ref selection/prewarm creates only desired/active ref-view cache/build records and evictable generations, coalesces identical builds, CAS-rejects changed-tree/base/full-build-fingerprint publication while permitting revalidated same-tree adoption with current route metadata, returns truthful requested/actual/fallback descriptors and stable errors, and re-resolves movement only on next access.
22. Storage and latency metrics demonstrate sparse behavior for sparse diffs and quantify the wider-key/shared-writer cost.
23. Documentation explains selection, freshness, capability completeness, retention, promotion, demotion, inaccessibility, authoritative removal, fallback, no-primary, and migration semantics.

## Alternatives considered

### Keep fully indexing every worktree

This is simplest and already partly happens, but duplicates most graph/search/sidecar data and does not solve cached branch switching. Rejected for automatic worktrees.

### Extend only the current in-memory `OverlaidView`

It provides useful lookup precedence but does not persist, use the production resolver/enrichments, index search, cover analyzer store bypasses, or own lifecycle. Verified consistency defects also make it unsuitable as the contract. Rejected as the whole solution; reusable as a prototype and top-layer implementation.

### Add a plain `revision`/`layer_id` column in place

A plain column is insufficient because today's primary/unique keys still reject repeated logical IDs, joins still cross revisions, deletes still affect every matching ID, and global FTS statistics still mix hidden generations. Rejected.

The accepted variant is a complete vNext rewrite: give graph-owned payload/search/mask/source-owner rows composite `generation_id` keys, give composed analyses their own `analysis_generation_id`/`ViewID`, bind operations through the ownership manifest, implement exact composed-view search, and remove legacy objects at cutover. Shadow migration uses one temporary adjacent store epoch, never a permanent or live-database parallel payload schema. **DECIDED STORAGE-2 / MIGRATION-1.**

### Use a database file per graph or generation

Separate immutable files simplify physical sealing and deletion, but a repository with many worktrees and cached trees would accumulate many databases, connections, handles, and recovery units. Rejected by **DECIDED STORAGE-1**. Immutability and isolation are logical properties enforced by generation keys, state transitions, leases, and atomic route pointers in the shared database.

### Persist source bytes for ready views

A content-addressed blob store would guarantee that file reads survive Git object pruning, but it duplicates working-tree data into the graph database, adds blob GC, and forces a migration source-durability preflight with an indefinite waiting-for-source state. Rejected by **DECIDED SOURCE-DURABILITY-1**: ready commit-only views read the local object database and withdraw the file-read capability honestly when objects disappear.

### Materialize every branch eagerly

Repositories can have thousands of local/remote refs, many never queried. Eager indexing creates its own indexing and storage storm. Rejected. Explicit single-ref prewarming is part of V1; pattern-based or eager prewarming is future work after measurement.

### Use merge-base/three-dot branch diffs

This describes changes introduced by a branch, not the exact transformation from the chosen full base tree to the target tree. It can leave incorrect base files visible. Rejected for view construction; direct tree-to-tree diff is required.

### Depend on Git hooks

Hooks can provide low-latency signals but are user-owned, configurable, and not guaranteed to run for every external mutation. Rejected as the correctness source; permitted as an opt-in accelerator.

### Union all branch overlays for “overall” search

Mutually exclusive symbol versions would coexist and graph paths could traverse states that never exist together. Rejected for normal graph queries; an explicitly labeled administrative all-views search can be separate.

## Risks and mitigations

| Risk | Mitigation |
| --- | --- |
| Dependency closure makes a small text diff a large graph delta. | Measure and report changed versus affected files; retain correctness; optionally select a closer base or compact by policy. |
| Shared generation tables widen hot keys and contend on one SQLite writer. | Generation-leading query-plan benchmarks, bounded build/GC batches, short publication transactions, WAL/cache-density/latency metrics, and storage quotas for unreferenced caches. |
| One missed generation predicate leaks or deletes another view. | Generation-bound store handles, no raw versioned-table access outside the storage package, mechanical SQL checks, and destructive cross-generation fault tests. |
| Shared search rows let hidden generations alter BM25 statistics or per-layer top-k truncation lose true results. | Apply masks first, derive exact selected-`ViewID` corpus statistics, score visible documents together, and use only composed-view-safe candidate bounds; global FTS BM25 is never authoritative. |
| LSP requires real filesystem files. | Checked-out worktrees retain normal LSP; inactive refs declare LSP/workspace capabilities unavailable in V1, never borrow base facts, and create no hidden worktree. |
| An untrusted branch changes config to trigger network/process access or source exfiltration. | Allow only source-safe target-tree keys; keep inactive-ref indexing fully offline (**REF-CONFIG-TRUST-1**); take endpoints, credentials, downloads, plugins, and commands from separately trusted policy; include the sanitized effective config in fingerprints and test hostile refs. |
| Git prunes objects behind a cached inactive ref. | Accepted by **SOURCE-DURABILITY-1**: the read fails `source_object_missing` and the file-read capability is withdrawn atomically while graph/search capabilities remain; Gortex never mutates Git with protective refs. |
| Base advancement fans out into an inactive-ref rebuild storm. | Rebase live checkout routes only; let idle exact caches pin old immutable bases until retention eviction and rebuild lazily on explicit access. |
| Search/analyzer bypasses produce mixed revisions. | One request-pinned view, static audit, cache key enforcement, differential tests. |
| Cross-repository derived edges become stale. | Source ownership, workspace view fingerprints, bridge generations or explicit incomplete status. |
| Filesystem/Git events race publication. | State re-sampling, immutable builds, incarnation tokens, single coordinator. |
| Transient access failure causes premature data loss. | Treat uncertain evidence as inaccessibility and use a distinct durable availability clock. Preserve explicit intent/config/identity/sealed full generations; retain disposable automatic identity only through grace, then forget it while preserving the family primary. |
| Several intent sources reassert dedicated mode after untrack. | Store provenance per source, treat explicitness as a union, preflight/revoke all sources, and roll back on any durable-write failure. |
| Primary deletion surprises users or damages independent graphs. | Preview the exact dependency closure, guard it with `primary_epoch`, preserve independent dedicated siblings, never auto-elect, and expose `no_primary` until explicit designation. |
| Logical deletion is mistaken for forensic erasure. | State the decided boundary explicitly: queryable graph/config/catalog state is removed; free pages/WAL/logs/metrics/backups/snapshots follow their own retention/security policies. |
| Worktree paths move or are reused. | Administrative checkout identity plus incarnation, route update, generation validation. |
| Overlay is nearly a full graph. | Accept inherent cost, report density, allow policy-based compaction without changing explicit/dedicated semantics silently. |
| vNext migration strands old readers or exposes a mixed/partial schema. | Use adjacent store epochs, an atomic durable active-epoch pointer, old-reader/handle drain, crash-resumable old DB/WAL/SHM cleanup, and normalized equivalence against fresh/cold builds. |

## Decision interview record

Rounds 1–3 are recorded near the top. Round 4 (2026-08-15) resolved every remaining item; the decisions table is the authoritative record. Outcomes, noting where the previous recommendation was rejected:

- `REMOVAL-EVIDENCE-1`, `PRIMARY-DESIGNATION-1`, `API-1`, `REF-SCOPE-1`, `BASE-3`, `RETENTION-1`, `ANALYSIS-1`, `UNBORN-1`, `SLO-1`: accepted as recommended.
- `AUTOMATIC-GRACE-EXPIRY-1`: clarified on 2026-08-29 from isolated validation; it supersedes the earlier `POST-GRACE-FALLBACK-1` wording by making automatic checkout identity/fallback expire at the availability deadline while default/base selection survives.
- `PRIMARY-LIVE-IDENTITY-1`: recommendation **rejected** — dependents are forgotten entirely with the primary; no `discovered_no_primary` identity exists.
- `SOURCE-DURABILITY-1`: recommendation **rejected** — no persisted source bytes; commit-only file reads use the local object database with atomic capability withdrawal.
- `REF-CONFIG-TRUST-1`: recommendation **rejected** — inactive-ref indexing is fully offline; no remote provider ever receives inactive-ref source text.
- `CROSSREPO-1`: required in V1, adding foreign-generation pinning and bridge generations to V1 scope.
- New round-4 decisions from codebase verification: `SEARCH-AUTHORITY-1` (conscious reversal of PR #527), `UNTRACK-BLOCKED-1`, `HOOKS-VIEW-1`, `TEXT-SEARCH-VIEW-1`, `LSP-SCOPE-1`, `PREFIX-1`.

## Code map

Primary implementation areas:

- [`internal/indexer/worktree.go`](../internal/indexer/worktree.go): current `.git`/`commondir` detection and naming.
- [`internal/indexer/multi.go`](../internal/indexer/multi.go): repository registry, indexing, reconcile, untrack, and current worktree GC.
- [`internal/indexer/git_watcher.go`](../internal/indexer/git_watcher.go): HEAD transition batching.
- [`internal/indexer/multi_watcher.go`](../internal/indexer/multi_watcher.go): per-repository filesystem/Git watcher registry.
- [`internal/indexer/incremental_batch.go`](../internal/indexer/incremental_batch.go): reusable parse/invalidation work, currently non-atomic across all sidecars.
- [`internal/graph/overlay.go`](../internal/graph/overlay.go): transient precedence prototype and correctness fixes.
- [`internal/graph/store_sqlite/schema.go`](../internal/graph/store_sqlite/schema.go): current global-key schema constraints.
- [`internal/mcp/overlay_view.go`](../internal/mcp/overlay_view.go): request middleware for editor buffers.
- [`internal/query/engine.go`](../internal/query/engine.go): search candidate composition and stale bundle risk.
- [`internal/daemon/overlay.go`](../internal/daemon/overlay.go): transient session/buffer lifecycle and terminology.
- [`internal/forge/worktree.go`](../internal/forge/worktree.go): existing porcelain parser precedent, currently PR-only and not NUL-safe.
- [`internal/mcp/tools_fileops.go`](../internal/mcp/tools_fileops.go): current ambiguous worktree path rerooting.
- [`internal/serverstack/shared_server.go`](../internal/serverstack/shared_server.go): shared store/indexer/server construction and lifecycle hooks.
- [`cmd/gortex/daemon.go`](../cmd/gortex/daemon.go): reconcile janitor ticker driving vanished-worktree GC and `ReconcileAll`.
- [`internal/hooks/pretooluse.go`](../internal/hooks/pretooluse.go) and [`internal/hooks/probe.go`](../internal/hooks/probe.go): hook front door; probes become view-resolving with the reconcile-then-enforce posture (`HOOKS-VIEW-1`).
- [`internal/search/trigram/searcher.go`](../internal/search/trigram/searcher.go): filesystem-rooted trigram search; becomes per-checkout `ViewRoot`-scoped (`TEXT-SEARCH-VIEW-1`).
- [`internal/semantic/manager.go`](../internal/semantic/manager.go) and [`internal/semantic/lsp/registry.go`](../internal/semantic/lsp/registry.go): enrichment root mapping and LSP clients; become per-checkout under a global cap (`LSP-SCOPE-1`).
- [`internal/mcp/scope_resolve.go`](../internal/mcp/scope_resolve.go): session scope ceilings that compose with view selection.
- [`internal/resolver/resolver.go`](../internal/resolver/resolver.go): whole-store cross-repository resolution, replaced by pinned per-pair bridge resolution (`CROSSREPO-1`).

Suggested new boundaries:

```text
internal/graphview/        immutable ViewID, composition, leases, completeness
internal/layerstore/       shared-DB catalog and generation-keyed full/sparse storage
internal/gitstate/         family inventory, refs, checkout identity/state
internal/reconcile/        single CheckoutReconciler and event coordinator
internal/indexer/source/   filesystem, Git tree, and layered ContentSource
internal/indexer/layer/    sparse build/invalidation/publication pipeline
internal/mcp/view/         selection, request pinning, riders, diagnostics
```

Names are illustrative; package-cycle analysis is required before final placement.

## External Git facts used by this design

- [`git worktree`](https://git-scm.com/docs/git-worktree.html) documents the stable porcelain format, NUL termination, main/linked records, `locked`, and `prunable` state.
- [Git repository layout](https://git-scm.com/docs/gitrepository-layout) documents gitfile indirection, the common directory, per-worktree `HEAD`/index, shared refs/objects, and `worktrees/<id>` administration.
- [`git rev-parse`](https://git-scm.com/docs/git-rev-parse) provides `--git-dir`, `--git-common-dir`, `--git-path`, and absolute path formatting needed to avoid layout assumptions.
- [`git diff`](https://git-scm.com/docs/git-diff.html) distinguishes direct two-tree comparison from merge-base/three-dot comparison.
- [`git cat-file`](https://git-scm.com/docs/git-cat-file.html) provides `--batch-command`/`-Z` object access suitable for indexing tree blobs without a checkout.
- [`git gc`](https://git-scm.com/docs/git-gc) and [`git prune`](https://git-scm.com/docs/git-prune) document removal of unreachable objects; a Gortex cache alias is not a Git reachability root, which is why the decided **SOURCE-DURABILITY-1** contract withdraws file-read capability honestly instead of persisting bytes or creating protective refs.
- [`git for-each-ref`](https://git-scm.com/docs/git-for-each-ref.html) exposes ref information including worktree association.
- [`git symbolic-ref`](https://git-scm.com/docs/git-symbolic-ref.html) distinguishes attached symbolic HEAD from detached state; an unresolved OID must also represent an unborn branch.
- [Git refs](https://git-scm.com/docs/git-refs.html) and [reftable](https://git-scm.com/docs/reftable) show why loose-ref directory watches cannot be the authoritative ref-change mechanism.
- [Git hooks](https://git-scm.com/docs/githooks) documents `post-checkout` and `reference-transaction`; they are optional signals, not the authoritative lifecycle mechanism.

## Implementation audit and validation (2026-08-27)

This section records the implementation and measurements on `feat/worktree-branch-views` after post-implementation validation. It is evidence, not a second semantic contract: the decisions and acceptance criteria above remain authoritative, and a discrepancy is a defect rather than an implicit specification change. The audited repair series runs from the lifecycle correction in `eebe4c4a` through the degraded-ref regression coverage in `0bf03d62`.

### Findings closed during validation

| Finding | Failure mode | Implemented invariant and evidence | Atomic commit(s) |
| --- | --- | --- | --- |
| Topology discovery was not event-driven | A linked worktree added after startup could wait for manual reconciliation or the hourly janitor. | The Git watcher observes common-directory worktree topology, schedules an authoritative family census, and treats the janitor as a correctness backstop. Add/remove events are coalesced; reconciliation performs one authoritative walk, not the previous double walk. Covered by topology-event, real-Git reconciliation, disappearance, and grace tests. | `eebe4c4a` |
| Removal and grace routing were fragmented | An inaccessible checkout could retain a route long enough for new requests to read dirty/worktree state, and cleanup differed by entry point. | Inaccessibility withdraws the exact route immediately. New requests receive only the labeled, read-only primary fallback; exact, file, and mutation operations refuse. Existing pinned readers may drain. Authoritative disappearance purges logical checkout/graph/intent/config/cache/journal state after grace. | `eebe4c4a` |
| Dedicated bases followed the mutable checkout | A full dedicated graph could accidentally absorb dirty filesystem state instead of representing the tracked revision. | A dedicated graph is anchored to the exact initial HEAD tree. Staged, unstaged, and eligible untracked files are a distinct dirty generation. Unborn HEAD uses an empty committed lower tree. | `bb0ae5d3` |
| Identical view builds churned generations | Settled checkout cycles and A→B→A ref selection could parse and publish content that already existed. Concurrent builders could also publish duplicate compatible winners. | A shared ready-generation catalog keys immutable content by tree/base/build fingerprint and capability profile. Checkout and ref routes lease and bind a compatible canonical generation; retirement invalidates the cache. Cache hits report zero builds. | `13658405`, `dccd98cf`, `e65db8a5` |
| Canonical selection could converge on the wrong winner | A lower generation ID finishing late could replace a newer generation already protected by a live lease or durable checkout/ref/dedicated binding. | Compatible ready generations with a live lease or durable binding outrank unbound candidates; canonical selection then uses a stable oldest-winner rule. Withdrawn or expired candidates cannot become new bindings. Normal 100× and race 20× convergence stress passed. | `e65db8a5` |
| Source withdrawal could be lost under SQLite contention | A pruned Git object had to withdraw `source.snapshot`, but a direct writer could block, fail, or leave capability state indefinitely optimistic. | Withdrawal is a durable, canonical, coalescing queue. The catalog writer persists producer unavailability, retry is quiet and bounded, and store close drains pending work. Only the named producer changes; generation readiness and structural producers remain intact. | `d868d0bc`, `89bd56b7` |
| Immutable Git reads could lazy-fetch promisor objects | Revision verification, tree construction, batch blob reads, diffing, or MCP file reads could contact a remote merely because an object was missing locally. `ls-tree` alone also did not prove its listed blobs existed locally. | Every immutable-ref path uses the no-lazy Git execution contract and verifies required objects locally. Missing commit/tree/blob objects return the stable local-unavailability/source-withdrawal behavior; no hidden fetch, checkout, hook, or protective ref is created. Instrumented tests observed zero `upload-pack` invocations. | `d3dd4cc5` |
| MCP family routing used a stale graph snapshot | `Server.viewFamilies` derived scope from `Graph.RepoPrefixes()` captured before a newly discovered family existed. A fully built automatic route therefore waited for a 60-second fallback timeout even though catalog state was ready. | The live repository-family catalog is authoritative and ordered. Repository-prefix lookup remains only a compatibility fallback for old stores without family rows. Families added after server construction and after the first lookup route without restart. | `55bf8f03` |
| Exact-HEAD dedicated search seeded the wrong base | Checkout text search always seeded file inventory from generation 0. Exact-HEAD dedicated payload lives at the graph's active generation, so unchanged files disappeared from primary search. | `CheckoutCoordinator.textCorpus` resolves the checkout's logical `primaryBase` and seeds from that generation before applying commit and dirty claims. Both automatic and route-owned dedicated CWDs materialize coherently. | `55bf8f03` |
| Legacy dedicated bases were mistaken for routed views | Routing every ready dedicated checkout manufactured `view_building` freshness for legacy graphs whose canonical corpus intentionally has no checkout route. | Route existence is the compatibility boundary: a ready dedicated checkout with no route remains the ordinary base corpus; exact-HEAD dedicated checkouts with a route retain worktree freshness and labeled fallback semantics. | `9834dddb` |
| Source degradation incorrectly invalidated structural ref currency | After a pruned blob withdrew only `source.snapshot`, `activeIsCurrent` treated the whole generation as stale. The next `get_symbol` entered rebuild/cache-adoption and stopped seeing otherwise-valid graph data. | Active-route currency is payload identity plus a servable generation state. Producer degradation is evaluated per operation: graph/search stay on the same generation, source reads refuse, and a new route still cannot adopt a source-degraded generation. | `247a3339`, `0bf03d62` |

### Verified implementation invariants

1. Logical isolation uses one shared SQLite database and one canonical generation-keyed payload/search schema. `family_id`, `graph_id`, `generation_id`, route epochs, owner bindings, and producer state provide the distinct attributes; there is no database per graph, worktree, branch, or revision.
2. CLI track, MCP track, durable manual configuration, and explicit project membership are explicit intent and create a dedicated logical graph. CWD discovery alone is implicit and creates an automatic route over the designated family primary.
3. Initial dedicated payload is exact HEAD. Checkout dirty state, including eligible non-ignored untracked source, is composed above it and never mutates the lower generation.
4. A coherent request selects one base plus its route-owned layers. Overlay data replaces the same logical identity from lower layers; unrelated overlay and base matches share the normal relevance ordering. Normal search never unions incompatible branches or unrelated cached views.
5. A non-primary dedicated worktree that is explicitly untracked loses its dedicated graph and, while still present, can be rediscovered as an automatic overlay over a different ready primary. Last-primary untrack is a previewed family forget; primary loss deletes only the primary dependency closure and preserves healthy independently dedicated siblings.
6. Authoritative disappearance means logical forgetting. Queryable graph/catalog/intent/config/cache/journal state for that checkout is removed; WAL/free pages, logs, metrics, backups, and snapshots follow their separate retention policies.
7. Inaccessibility and authoritative disappearance are distinct evidence states. During the accepted 30-second grace, new requests get only labeled read-only primary fallback; dirty/buffer/root bytes are excluded and exact/file/edit operations refuse.
8. Inactive local refs are V1 structural/read-only views. They are selected explicitly, never fetched, never create a hidden worktree or tracking intent, and report LSP-only capabilities unavailable.
9. Existing active routes may retain structural graph/search after an optional producer such as `source.snapshot` degrades. A new binding must satisfy its requested capability profile and cannot borrow that degraded generation as a source-complete view.

### Benchmark protocol and results

Measurements used macOS/arm64 on an Apple M1 Pro. Unless noted otherwise, Go benchmarks ran with `-benchmem -count=5`; ranges are the five samples and are engineering evidence, not yet production SLOs. The daemon and filesystem cache were live, so latency ranges intentionally report observed variation instead of false precision.

| Path | Result | Allocation/build/network signal |
| --- | --- | --- |
| Authoritative clean worktree census | 14.68–17.24 ms/op | 654–656 KB, 5,168 allocs |
| Legacy double worktree walk (comparison) | 30.08–48.92 ms/op | about 1.31 MB, 10,340 allocs |
| Settled checkout coordinator cycle | 85.84–90.17 ns/op | 96 B, 2 allocs |
| Contended producer-withdrawal scheduling | 183.2–188.9 ns/op | 0 B, 0 allocs |
| Catalog producer-withdrawal drain | 102.6–109.8 µs/op | about 3.16 KB, 66 allocs |
| Store close while writer is held | 9.91–10.46 ms/op | about 4.75 KB, 101–106 allocs |
| Canonical checkout-generation adoption | 0.674–1.457 ms/op | zero builds; about 11.9 KB, 371–372 allocs |
| Begin generation retirement | 210.6–313.0 µs/op | about 1.84 KB, 49 allocs |
| Canonical ref-view warm hit | 57.6–121.2 ms/op | zero builds; 176–181 KB, 1,555–1,559 allocs |
| Ready-cache hit with live lease | 376–429 µs/op | compatible ready generation retained |
| Ready-cache pinned-winner selection | 923–956 µs/op | live/durable winner retained |
| Ref file read, complete local object | 15.95–43.82 µs/op | zero `upload-pack`; 848 B, 7 allocs |
| Ref file read, missing local blob | 42.13–107.64 µs/op | zero `upload-pack`; 744 B, 16 allocs |
| Catalog-backed family enumeration: 1 / 10 / 100 families | 14.4–19.3 / 23.4–24.2 / 98.2–102.6 µs/op | 1.26 / 6.68 / 54.3 KB; 36 / 121 / 934 allocs |
| Routed worktree text-search lifecycle | 3.14 s/cycle over 20 cycles | previous behavior deterministically timed out at about 61 s |
| Legacy dedicated CWD selection | 61.8–67.4 µs/op | 6.62 KB, 155 allocs |
| `activeIsCurrent`, source complete / unavailable | 26.9–28.4 / 27.2–28.1 µs/op | identical 3.35 KB, 85 allocs, zero builds |
| Full `EnsureRefView`, source complete / unavailable | 28.0–29.9 / 27.8–31.3 ms/op | about 86 KB, 790–791 allocs, zero builds |

The no-lazy closure was also measured at each Git boundary: revision closure verification 17.75–48.03 ms, tree construction 18.02–41.62 ms, batch object reads 27.0–94.7 µs, generic no-lazy command execution 8.52–34.81 ms, and tree diff 9.59–32.38 ms. Every variant recorded zero `upload-pack` operations.

### Focused validation completed

- Topology discovery, disappearance, grace fallback, untrack authorization, exact-HEAD anchoring, checkout/ref cache reuse, generation retirement, producer withdrawal, and no-lazy promisor fixtures pass their focused normal and race suites.
- Routed text search plus catalog-family routing passed normal `count=20` in 62.775 seconds and race `count=10` in 51.931 seconds before the legacy compatibility addition; the combined routed/legacy/catalog race matrix then passed `count=10` in 76.755 seconds.
- Ref-generation convergence passed normal `count=100` and race `count=20`; tests cover live lease, durable ref/checkout binding, withdrawn pinned bypass, expired lease, and concurrent winner ordering.
- Source withdrawal retained the same structural ref generation and graph visibility for 20 isolated MCP cycles after the fix. The direct indexer regression passed normal `count=20` and race `count=10`, while a new alias correctly rejected the degraded cache and built a source-complete generation.
- Broad normal `internal/graph/store_sqlite` and `internal/indexer` suites passed in 98.025 and 399.049 seconds respectively on the audited branch. Final full MCP, full indexer race, and rebuilt-daemon lifecycle validation are recorded in the final validation addendum before push.

### Final isolated validation addendum (2026-08-29)

This addendum records the final implementation evidence. `CONFIRMED` means the behavior passed either a deterministic regression/benchmark or the complete isolated-daemon replay at the named code revision. There are no remaining implementation or replay items marked `TBD` in this addendum.

#### Isolation and revision identity

The final lifecycle run used an environment-scrubbed foreground daemon with independent `HOME`, `TMPDIR`, every XDG directory, socket, PID, log, configuration, and SQLite store. The process environment was built from an allowlist, telemetry and Git prompts/global configuration were disabled, `GORTEX_RECONCILE_INTERVAL=0`, and only disposable Git repositories under the sandbox were tracked. The production daemon, its socket, configuration, store, repositories, and agent worktrees were not stopped, restarted, reconfigured, or contacted.

- Final validated code revision: `a1794604f6a55515789e163063fc5b67379c588b`.
- Final branch-built binary SHA-256: `81dcef8ff91dec1e7356a388e8e505bc99b1cc5a4a350d5d692a4b2e78dd7860`.
- Final schema version: `18` (`PRAGMA user_version` in the isolated store).
- Final passing sandbox: `/private/tmp/gortex-overlay-e2e.e1BCgQ` (retained with `PASS.json`, logs, JSON responses, descriptor snapshots, idle samples, and SQLite state).
- Diagnostic ABA-failure sandbox: `/private/tmp/gortex-overlay-e2e.KL3hyC` (retained to preserve the pre-fix failure evidence).
- The final replay source status contained only this specification edit and pre-existing user-owned untracked notes; all production and test changes were committed before the binary was built.

#### Findings resolved during final closure

| Finding | Resolution and evidence | Atomic commit |
| --- | --- | --- |
| Automatic grace semantics in the draft exceeded implemented identity lifetime | The prior isolated 30-second lifecycle run proves deadline-driven automatic forgetting. The focused request regression proves the routing boundary: during grace, the automatic checkout ID remains in the rider and eligible graph/search receives labeled primary fallback; exact/file/edit policies remain strict. After explicit catalog forgetting in the fixture, a stale explicit ID fails `checkout_inaccessible` before the handler executes, while selector-free/default plus explicit-base requests still read the surviving primary. This was a specification mismatch, not a production grace-routing defect. | Prior isolated lifecycle plus `336e6456` |
| Settled adopted commits rebuilt dirty state repeatedly | When a checkout adopted another checkout's canonical commit generation while retaining a valid nonzero dirty generation, the unchanged reconcile path moved the ready commit slot, cleared the dirty route, rebuilt it, and advanced the route epoch on every poll. The fixed path returns early when the validated ready generation already equals the routed commit, releases the transient lease, and leaves dirty base/fingerprint validation to the independent dirty slot. A real edit still rebuilds once and then stabilizes. The old predicate failed on its first unchanged poll; the regression passed `-count=5` in 8.735 s and the focused set passed in 9.014 s. | `3b2b05df` |
| Git topology observation opened source-count-proportional descriptors on Darwin | The isolated 2,000-file/two-checkout fixture recorded 4,108 open entries, about 4,031 attributable to the stress family. `e5964687` replaced recursive control-plane registrations with one cheap family probe and bounded exact ref observation; `3910f17d` added linked-worktree HEAD identity; `d08fc8c4` joined reconcile callbacks during teardown. The final 2,000-file E2E measured only 3 descriptors rooted at the base, 1 at the linked worktree, and a total worktree delta of 9. | `e5964687`, `3910f17d`, `d08fc8c4` |
| Persisted automatic worktrees had no recursive source signal after cold restore | Catalog routes/coordinators were restored, but newly created dirty or untracked files could wait for the 15-second safety poll because only explicitly configured bases reattached ordinary watchers. The lifecycle now owns one signal-only recursive watcher per ready automatic checkout, repairs it with bounded joined retry, fences callbacks by checkout/incarnation/family/root/epoch, and tears it down on grace, promotion, disappearance, family loss, or close. Real Darwin resource tests prove descriptor cost is independent of files per root and teardown returns to baseline. | `99c6749a` |
| Removal-grace fallback was labeled but did not search the primary base | The selector built a base rider around a nil legacy reader, so a real `search_symbols` returned no base results and emitted no base-scoped capability list. Grace fallback now pins and opens the primary graph's exact sealed `dedicated_base` generation, suppresses commit/dirty/filesystem/buffer layers, and fails closed on wrong graph, owner, layer, ancestry, tree, state, or checkout identity. A production-shaped search regression proves base-only and masked base symbols are returned while commit/dirty symbols are excluded. | `07659e59` |
| A rapid linked-worktree A→B→A transition could be missed between 1-second topology samples | The final replay first exposed this as a 15-second timeout: a source signal published the transient ref, while the topology probe compared only equal pre/post HEAD contents and missed the return. HEAD and active loose-ref identities now include stable modification time and size, preserving a bounded probe while detecting sampled ABA. Deterministic HEAD/ref regressions pass normal `count=50` and race `count=20`; the formerly failing final E2E transition settled in 1 second. | `a1794604` |

#### End-to-end lifecycle matrix

| Scenario | Final isolated evidence | Status |
| --- | --- | --- |
| Isolation and zero-config start | Foreground scrubbed environment, private store/socket/config, exact child PID stop/reap; production daemon untouched. | `CONFIRMED` |
| Explicit base tracking | One family and dedicated primary graph `graph-a493649b09564d87fb643d356642175e` were created. Initial full payload was generation 1 and checkout routing composed commit/dirty layers above it. | `CONFIRMED` |
| Dynamic linked-worktree discovery | A worktree added after daemon start was discovered and ready in 2 seconds with automatic mode, the same primary graph, its own route/coordinator, and no explicit config entry. No janitor/manual reconciliation was used. | `CONFIRMED` |
| Overlay composition and precedence | One logical replacement symbol resolved to overlay data without a base duplicate; unchanged/base-only data fell through; a non-ignored untracked source appeared only in the overlay; a deletion tombstone suppressed the base symbol; an ignored file remained absent. | `CONFIRMED` |
| A→B→A and dirty stability | A selected generation 4, B selected generation 8, and return to A reused generation 4 without reparsing. Signaling completed in 1 second and the route was ready within 2 seconds. The settled adopted-commit regression separately proves idle polls do not rebuild dirty state or advance the route epoch. | `CONFIRMED` |
| Rapid missing-ref creation and A→B→A ABA | The first commit on a previously missing active loose ref was observed, and return to branch A settled in 1 second with the revision-aware probe. | `CONFIRMED` |
| Promotion and demotion | The automatic worktree promoted to an independent dedicated graph; explicit untrack removed that graph/CLI intent and restored the same checkout as an automatic overlay without manual discovery. | `CONFIRMED` |
| Explicit worktree disappearance | A still-dedicated worktree disappeared; its checkout, dedicated graph, config intent, generations, and cleanup state were logically erased while the healthy primary survived. | `CONFIRMED` |
| Inactive local ref | Structured local-ref selection resolved the requested commit/tree into a persisted read-only structural view, returned branch-specific source, created no hidden worktree, performed no fetch, and rejected mutation as `view_read_only`. | `CONFIRMED` |
| 75-second idle stability | Zero physical builds, stable route/generation identity, bounded descriptors and DB/WAL, and no monotonic RSS growth were observed. | `CONFIRMED` |
| Cold isolated restart | Family/graph/base checkout, automatic route generations, and inactive-ref identity survived restart; startup recorded zero sparse rebuilds. A new dirty/untracked marker was then indexed, proving automatic source-watch reattachment. | `CONFIRMED` |
| Removal grace and expiry | Removal entered labeled `removal_grace` immediately. Read-only search returned the sealed primary base with `search.symbols` listed in `base_scoped`; dirty/untracked data was absent and exact-source access was rejected. The checkout was logically forgotten after 30 seconds; its stale explicit ID failed while ordinary base search survived. | `CONFIRMED` |
| 2,000-file topology resource bound | The final fixture measured base-root descriptors 3, linked-worktree-root descriptors 1, and total worktree FD delta 9, independent of the 2,000 tracked files. | `CONFIRMED` |

#### Resource and performance measurements

| Measurement | Confirmed result | Interpretation |
| --- | --- | --- |
| Explicit-membership snapshot | 42.6–43.6 µs/op, 25,016 B/op, 81 allocs/op | Canonical-path-deduplicated top-level/project membership union. |
| Coalesced watcher retry scheduling | 64.17–106.7 ns/op, reported 1 B/op and 0 allocs/op; independent review measured 21.14 ns/op, 0 B/op, 0 allocs/op | Retry admission itself is negligible and coalesced. |
| Existing watcher ensure | 24.64–30.0 ns/op, 0 allocs/op | Idempotent dynamic admission fast path. |
| Topology registration, 1/8/64 worktrees | 63.7–64.5 µs / 253.0–255.5 µs / 1.780–1.860 ms | One inventory per operation and no duplicate registrations in the measured implementation. |
| Real missing-path ensure, 1/8/64 | 198.6–209.8 ms / 1.561–1.706 s / 13.990–14.230 s | Exact watcher/path counts were 1/2, 8/17, and 64/129; teardown leaked none in that admission fixture. |
| Recovery scheduling, follower/owner | 166.2–215.4 ns/op, 144 B/op, 2 allocs/op / 311.2–353.3 ns/op, 288 B/op, 4 allocs/op | Owner-only topology nudge remains bounded. |
| Dedicated point-update guard | 35,707 ns/op, 2,136 B/op, 24 allocs/op over 31,196 calls | Zero base/generation-zero mutation and zero fallback results/errors. |
| Stable adopted-commit dirty reconciliation | `BenchmarkCoordinatorStableAdoptedCommitDirtyReconcile`: 30,676,637 ns/op, 71,469 B/op, 892 allocs/op | 0 physical builds/op and 0 route-epoch advances/op. |
| Pre-fix 2,000-file daemon footprint | RSS 131,568 KiB (about 128.5 MiB) with 4,108 open entries, about 4,031 attributable to the stress family | Memory remained modest while descriptor growth was unacceptable; descriptor count, not RSS, exposed this defect. |
| Activity Monitor incident evidence | Process-list memory rose from 14.29 GB to 43.17 GB, while the inspector reported real/private memory of 333.5/272.0 MiB and later 1.02/1.01 GiB; virtual memory moved from 433.06 to 461.49 GB | The list-column figures document a serious operational symptom but are not evidence of a 43 GB Go heap. Acceptance uses process RSS/private memory, Go heap, descriptors, WAL/store growth, build count, and route churn separately. |
| Automatic source-watcher Darwin resource contract | 1-file and 10,000-file roots both consumed exactly 12 descriptors; `StopAll` returned to the exact baseline. A 10,000-write settled burst emitted at most 2 signals; one-file readiness/signal latency remained below 2 seconds. At 64 roots RSS delta stayed below 64 MiB and repeated cleanup slope below 2 MiB/cycle. | Recursive signal cost is per root, not per source file; teardown is joined and leak-free. |
| Final isolated daemon resource envelope | Automatic discovery 2 s; total worktree FD delta 9; base-root descriptors 3; worktree-root descriptors 1; 75-second idle zero builds/stable route; maximum idle RSS 359,024 KiB; final RSS 155,536 KiB; final DB+WAL 5,575,416 bytes; final sandbox 353,436 KiB. | The former O(files) descriptor and repeated-build failure modes were absent. |
| Exact grace base materialization | `BenchmarkMaterializeBase`, five 1-second samples: median 161,245 ns/op (range 160,785–161,536), 17,632 B/op, 432 allocs/op. | Opens, validates, pins, assembles, and closes one sealed generation per request; no commit/dirty/filesystem/buffer layer is admitted. |
| Revision-aware linked-worktree HEAD probe | `BenchmarkAppendWorktreeHeadIdentityStable`, five 1-second samples: median 41,292 ns/op (range 39,825–41,769), 3,752 B/op, 26 allocs/op. | Two bounded control files are read/statted; worktree source count does not affect cost. |

#### Full test matrix

The earlier rows preserve pre-fix audit evidence. The final rows are the post-fix gates run against code revision `a1794604` (or, for the grace-only package gates, its immediately preceding atomic commit `07659e59` with identical package code):

| Command/scope | Result |
| --- | --- |
| `go test ./internal/config` | PASS, 0.932 s |
| `go test ./cmd/gortex` | PASS, 46.275 s after lifecycle fixtures attached a watcher |
| `go test ./internal/indexer` | PASS, 492.997 s |
| `go test -race ./internal/config` | PASS, 2.457 s |
| `go test -race ./cmd/gortex` | PASS, 80.374 s |
| `go test -race -timeout 30m ./internal/indexer` | PASS, 1,148.955 s; the prior 10-minute test timeout was raised because the process was still in unrelated SQLite/FTS fixture setup rather than a demonstrated race failure |
| Grace fallback/expiry regression at `336e6456` | PASS: fallback before deadline, strict exact/file/edit policies, stale-ID refusal after forgetting, default/base survival |
| Dirty reconciliation regression at `3b2b05df` | PASS `-count=5` in 8.735 s; focused set PASS in 9.014 s |
| Automatic checkout source-watcher focused normal/race/resource suites | PASS; real Darwin back-end covered readiness, file-count-independent descriptors, burst coalescing, 64-root RSS bounds, retry repair, identity/epoch fences, and exact teardown. |
| Grace materialization focused repetitions | `TestMaterializeBase*` PASS normal `count=20` in 2.125 s; `TestRemovalGraceSearchUsesPrimaryGenerationStack` PASS normal `count=20` in 11.666 s and race `count=10` in 19.813 s. |
| Worktree HEAD/ref ABA focused repetitions | Two regressions PASS normal `count=50` in 0.456 s and race `count=20` in 1.710 s. |
| Final graphview + MCP build/normal suite | PASS: graphview 7.189 s; MCP 157.976 s; streamable 0.471 s. |
| Final graphview + MCP race suite | PASS: graphview 44.035 s; MCP 417.148 s; streamable passed/cached as applicable. |
| Final full indexer normal suite | PASS: indexer 492.311 s; merkle 0.087 s; source 2.633 s. |
| Final full indexer race suite (`-timeout 30m`) | PASS: indexer 1,010.460 s; merkle/source cached. |
| Final clean isolated-daemon lifecycle replay | PASS 42/42 at code revision `a1794604`; binary SHA-256 `81dcef8ff91dec1e7356a388e8e505bc99b1cc5a4a350d5d692a4b2e78dd7860`; evidence `/private/tmp/gortex-overlay-e2e.e1BCgQ`. |

#### Atomic commit ledger

The earlier audit covers `eebe4c4a` through `0bf03d62`. Subsequent atomic history is grouped here by contiguous responsibility and was verified against the local branch before push:

- Routing, cache, and readiness: `cb766166` through `b0b8ada2` (inclusive).
- Retirement, promotion, and build lifecycle: `d9e398b8` through `607834c4` (inclusive).
- Cleanup, sparse recovery, and topology handoff: `d83bdaf7` through `7fba6338` (inclusive).
- Dedicated point guard and dynamic watcher admission:
  - `43727a39` — guard dedicated route-owned corpora from point mutation;
  - `fa9eb8b3` — snapshot all explicit repository memberships;
  - `144c78ba` — make dynamic watcher admission authoritative;
  - `6901aabb` — keep promoted repositories attached to watchers;
  - `3fd40597` — reconcile watchers after dynamic tracking;
  - `baab0067` — attach watchers in lifecycle fixtures.
- Final regressions:
  - `336e6456` — prove mode-specific grace fallback/forgetting behavior and default/base survival;
  - `3b2b05df` — keep an adopted commit's settled dirty generation and route epoch stable.
- Bounded and reliable observation:
  - `e5964687` — bound Git family topology monitoring independently of source count;
  - `0c3fcc95` — normalize unborn Git baselines;
  - `d08fc8c4` — join Git reconcile callbacks on stop;
  - `3910f17d` — include linked-worktree HEAD/ref contents in the family probe;
  - `99c6749a` — attach lifecycle-owned recursive signal watchers to automatic checkout sources;
  - `a1794604` — include HEAD/ref revision metadata so sampled A→B→A transitions cannot collapse to equality.
- Final MCP and graph-view closure:
  - `7fd23a3a` — align routed MCP fixture identities with production generations;
  - `07659e59` — materialize and capability-label the exact primary base during removal/availability grace.
- Specification/addendum: this commit records the final evidence; its own hash is intentionally not self-referential.

The push gate is satisfied: final binary identity, bounded topology/source-watch measurements, focused and broad normal/race suites, and the clean 42-assertion isolated lifecycle replay are all recorded above.

### Mainline merge and epoch-safety addendum (2026-08-30)

This addendum supersedes only the revision and schema-number facts in the
2026-08-29 snapshot. That snapshot remains historical evidence for its named
revision. The feature branch was subsequently merged with main at
`f9e3f442`, and the five textual conflicts were resolved as one semantic
integration rather than by choosing either side wholesale.

#### Conflict resolutions

| Conflict | Integrated contract |
| --- | --- |
| `file_batch_evict.go` | Main's exact mutation-receipt accounting is retained, while every node, edge, binding, candidate, and receipt deletion is scoped by `view_gen`. Ordinary file/repository replacement affects the current generation; authoritative forgetting can remove every generation and invalidates every receipt because an all-generation broadcast cannot be represented as one exact bounded receipt. |
| `schema_version.go` | The current schema is version 19. Shipped-main version 13 retains its coverage-spelling purge; version 14 is the idempotent convergence point for the two former version-13 lineages; versions 15–18 retain generation keys, enumeration indexes, and masks; version 19 replays the coverage purge after generation re-keying so databases upgraded through either lineage converge. |
| `sqlite_busy_test.go` | Main's writer-contention and rollback assertions remain, with eviction invoked through current-generation exact-receipt semantics. A rolled-back eviction leaves both graph rows and its receipt unchanged. |
| `store.go` | Main's pool, checkpoint, bulk-mode, and index-creation behavior remains in the shared core. Generation-aware graph/sidecar helpers create the main indexes only after their corresponding migrations. |
| `tools_fileops.go` | Main's fidelity admission, physical evidence, symlink/TOCTOU confinement, and file-dependents behavior is preserved. Worktree requests additionally use their selected physical root and routed reader; committed ref views use `GitTreeSource` and remain read-only. |

An independent four-way audit compared merge base, feature side, main side, and
the resolved working tree for all five files. It found no dropped mainline
behavior in the four SQLite files. Its one MCP integration finding led to the
post-publication syntax-health correction described below.

#### Base refresh and publication epoch invariants

- A dedicated base is current only when its owner/kind/layer/checkout/tree and
  config, extractor, and resolver pipeline identities are all canonical.
- A stale base is admitted into one serialized/coalesced background refresh.
  Admission atomically marks the graph `graph_refreshing`, retains old pointers
  solely for labeled fallback/pinned readers, advances every non-retired
  checkout route epoch (including an already-pending route), and makes ref
  views inexact.
- Every normal checkout publication now proves both `graph_ready` and the exact
  base generation captured by its build. This fence applies to active rows,
  pending rows, slot flips, and the formerly uncovered case where a coordinator
  observed no route before refresh admission and attempted to insert one
  afterward.
- Refresh rebuilds the exact immutable old base tree under the current pipeline,
  atomically swaps the base, invalidates dependent checkout/ref generations,
  and retires old generations after readers drain. Failure leaves the sealed
  old base available only as labeled fallback and does not create retry storms.

Focused regressions cover active, already-pending, and absent-route races.
Store and indexer race suites passed in 11.731 s and 109.332 s. Guarded route
publication measured a median of about 33.2 µs/op (1.61 KB, 42 allocations),
refresh coalescing about 14 ns/op with zero allocations, and the ready
`graphBase` path about 25.3 µs/op (3,048 B, 83 allocations).

#### Routed mutation and fallback invariants

- Only `edit_file`, `write_file`, and single-file `edit_symbol` are admitted on
  a ready mutable checkout. Multi-file `rename_symbol` and every other mutator
  fail before planning until they can publish through all affected checkout
  coordinators atomically.
- Mutation tickets bind checkout ID, mutation generation, observed route epoch,
  and published route epoch. Disk commit is followed by detached publication,
  so request cancellation cannot strand the graph; a timed-out request receives
  a route-bound pending receipt. A receipt cannot heal another checkout or be
  consumed through a different generation handle.
- Absolute paths are attributed relative to the selected graph. Primary/sibling
  paths and symlinks escaping into another checkout are rejected for reads and
  writes. Primary bytes, graph health, generation pointers, and route epoch are
  unchanged by a selected-worktree mutation.
- A pending route may retain old generation pointers for pinned readers, but an
  implicit new request receives only a labeled base fallback. It never composes
  the retained overlay. Explicit worktree selection and exact/file/edit
  operations reject the fallback as `view_building`.
- The request reader is deliberately pinned to its pre-mutation generation.
  Therefore completed routed mutations obtain syntax health from a short-lived
  materialization of the exact published route epoch, verify the same ready
  epoch before and after materialization, and release the lease immediately.
  If the route moves, health is unknown rather than being reported from the
  wrong generation. This mutation-only check measured a median 347.7 µs/op,
  about 39.8 KB and 991 allocations.

The routed mutation normal/race suites passed in 15.187/32.416 s; the final
syntax-health and retained-pointer fallback subset passed in 7.387/17.228 s.

#### Merge regression discovered by confinement

The strict absolute-path guard initially caused 16 analyzer/AST tests to return
zero matches. Production behavior was correct: those tests created source files
in a second `TempDir`, outside the repository root already returned by
`setupTestServer`, so `buildASTTargets` correctly discarded them before parsing.
The fixtures now write beneath the registered indexed root; the production
guard was not weakened. All 16 regressions pass normally and under the race
detector.

#### Post-merge isolated package evidence

No machine daemon was stopped, restarted, reconfigured, or mutated for these
tests. Package fixtures use temporary repositories and SQLite stores. The
isolated lifecycle gate used a freshly built binary, disposable HOME/XDG
directories, an explicit private socket/config/backend, and a disposable CWD;
every lifecycle CLI operation was bound to that socket. Mandatory preflight
graph inspection made two read-only calls to the configured daemon but no
control, tracking, configuration, or mutation call.

| Scope | Result |
| --- | --- |
| Full `internal/graph/store_sqlite` | PASS, 52.791 s |
| Full `internal/indexer` | PASS, 585.803 s |
| Full `internal/mcp` after fixture and routed-health corrections | PASS, 172.922 s |
| Detector/AST regression matrix | PASS normal in 0.983 s; PASS race in 2.704 s |
| SQLite schema/eviction focused race suites | PASS; migration and rollback repetitions covered both version-13 lineages and generation-scoped receipts |
| Repository-wide bounded gate | `go test -p=2 ./... -count=1 -timeout=30m` PASS; within that run SQLite was 53.464 s, indexer 562.623 s, MCP 187.861 s, and every other package passed |
| Contract race gate | `go test -race -p=2 ./internal/graph/store_sqlite/... ./internal/mcp/... -count=1 -timeout=30m` PASS; SQLite 935.971 s, MCP 492.156 s, streamable MCP 1.773 s; observed test-process RSS stayed bounded rather than growing with generations |
| Isolated daemon lifecycle | PASS with dedicated base plus automatically discovered linked-worktree overlay; same-symbol search deduplicated and selected the overlay payload, dirty tracked and non-ignored untracked symbols appeared only in the overlay, removal first returned labeled `removal_grace` fallback and then purged the checkout in about 8.602 s |
| Isolated daemon resources | Automatic discovery reached ready in 357 ms, dirty/untracked refresh in 541 ms, daemon peak RSS was 198,574,080 bytes, final SQLite store was 577,536 bytes, shutdown removed the private socket; binary SHA-256 `8e3de6c63ca3b734877adf8ecfffd3a12302828df42bfe0550a08d21e4f8e8ee` |

The repository-wide, contract-race, and isolated lifecycle gates were run on
the fully resolved staged source tree immediately before commit; the only later
change was this documentation-only evidence update. The retained isolated
artifact is `/private/tmp/gortex-isolated-lifecycle.Bbs031/PASS.json`.

### Cold-start, migration, and rollout follow-up (2026-08-30)

This follow-up records the reliability incident found while validating the implemented feature, the resulting implementation invariants, and the evidence still required before release. It refines validation and rollout requirements without changing the product decisions above. Measurements in this section are observations of the pre-fix feature branch unless explicitly labeled as post-fix acceptance evidence.

#### Incident: generation zero was indexed without a publishable primary

A cold restart after deleting the store exposed two startup paths that both believed they owned explicitly configured Git roots:

1. `warmupDaemonState` sent every configured root through the legacy `MultiIndexer.TrackRepoCtx`/`ReconcileRepoCtx` path. That path built the full corpus in legacy generation zero and populated generation-zero file metadata.
2. `CheckoutLifecycle.Seed` persisted the checkout, manual intent, and a `graph_ready` dedicated graph, but—unlike live `Register`—did not start promotion. The graph therefore had no nonzero active generation and no publishable checkout route.

The captured catalog had 29 active `manual_config` intents, 29 primary dedicated graphs whose `active_generation_id` was `NULL`, 53 desired/effective automatic checkouts, zero checkout routes, zero view generations, zero view layers, and 11,154 file-mtime rows assigned to `view_gen = 0`. Automatic coordinators retried roughly every 15 seconds and reported that the primary had no active generation. Thus the expensive legacy corpus was present but could not serve as the lifecycle primary. Any subsequent recovery that built the dedicated graph repeated corpus work; across configured repositories this was an N+1-shaped startup path, not sparse overlay construction.

During the user-visible incident the daemon reached more than 40 GiB resident memory and made the machine unusable even after indexing appeared to finish. The catalog and control-flow evidence above identifies the duplicate/unpublished corpus path that made the design unsafe. It does not attribute every byte of the 40+ GiB peak to a single allocation site; the process was restarted before a complete incident profile could be retained.

A separate measured cold run after store removal covered 29 repositories and 11,092 files:

| Milestone or phase | Observed time |
| --- | ---: |
| Open SQLite to socket serving | 0.520 s |
| Start to queryable | 369.690 s |
| Start to fully enriched | 898.514 s |
| Parse | 206.482 s |
| Resolve | 315.240 s |
| Deferred enrichment | 218.093 s |
| Global pass | 0.151 s |
| End-batch/persistence | 145.993 s |

After that run, in-use heap was about 189 MiB and ordinary post-warmup RSS was about 0.5–0.85 GiB, but cumulative allocation was 73.95 GiB. The largest allocation families were SQLite row materialization (`columnText`, node/edge scans, and `Rows.Next`), parser node arenas, and file reads. The store occupied about 4.4 GiB on disk (approximately 4.659 GB database plus a 64 MB WAL). These are diagnostic baselines for the old path, not acceptance results for the corrected lifecycle.

#### Required startup ownership and build-count invariant

There MUST be exactly one physical owner of each configured Git corpus:

- An explicitly configured Git checkout is lifecycle-managed. Legacy generation-zero warmup MUST exclude it.
- `CheckoutLifecycle.Seed` MUST follow the same explicit-intent promotion contract as `Register`: persist intent, restore only an O(1) transient shell for an already-ready durable route, or start one promotion for a cold/pending checkout.
- Promotion MUST index the exact HEAD base once, publish a nonzero active dedicated generation and route atomically, then reconcile the family so dependent automatic worktrees can build sparse views.
- An automatic coordinator MUST NOT start until its designated primary has a ready active generation.
- A ready warm restart MUST restore the transient shell and durable route without reparsing or rebuilding the full corpus.
- Non-Git configured roots remain on the legacy warmup path because they have no Git-family lifecycle.
- Recovery from an older feature store MAY retire an obsolete generation-zero payload before promotion, but MUST still build at most one replacement full corpus and MUST never retain both as independently routable bases.

The release gates are consequently quantitative: a fresh cold start performs exactly one full physical corpus build per explicitly configured Git checkout; an unchanged warm start performs zero full physical corpus builds; `GraphID`, active `GenerationID`, `CheckoutID`, and routes remain stable across the warm restart. Tests MUST count physical build admission/completion, not infer it from high-level lifecycle calls.

#### Current-owner topology reconciliation and fallback behavior

The disappearing-worktree CI failure exposed a second ownership gap. `AttachWatcherContext` previously nudged only families returned while registering the current watcher set. If the current-owner checkout disappeared from that snapshot first, its watcher source could be removed before any reconciliation nudge was retained, even though the catalog still owned the family. Reconciliation MUST therefore seed topology dispatch from every catalog family before refreshing configured watchers. Duplicate watcher and catalog nudges remain coalesced by the family singleflight mechanism.

When absence or inaccessibility is confirmed, the 30-second grace has immediate routing consequences:

- new eligible graph/search requests receive a clearly labeled primary-base fallback;
- that fallback is read-only and contains neither dirty checkout state nor editor buffers;
- exact-view, checkout-root file, and edit requests are rejected;
- only queries that already pinned the exact view may drain on it;
- authoritative disappearance proceeds to the logical cleanup contract after grace, while ambiguous inaccessibility follows the retention rules recorded above.

Tests for the current-owner case MUST remove the watcher membership before the administrative checkout record disappears, drain stale topology notifications, then prove that catalog-seeded reconciliation observes and forgets the vanished checkout. Repeated/race-enabled execution is required because a single pass does not exercise the scheduling window that caused the macOS CI failure.

#### Observable migration and daemon startup

The feature store reaches schema version 20 through migrations 14–20. Migrations
14–19 establish generation-scoped graph/view storage and replay; migration 20
adds the bounded `checkout_root_moves` recovery journal. A changed checkout root
and its journal marker commit atomically under the expected incarnation and
prior-root compare-and-set guard. The marker preserves the earliest uncompleted
previous root and newest current root for one checkout incarnation. Migrations
run synchronously before the daemon listens and pending steps are committed as
one transaction, so failure rolls the transaction back and restart repeats it
safely.

The prior `daemon restart` failure cannot be proven from its original daemon log because that log was overwritten by the later cold restart. Store deletion removed the startup delay and is consistent with a large in-place migration exceeding the controller's fixed 60-second socket deadline, but that remains an inference rather than retained proof. The measured 0.520-second open-to-listen interval above was a fresh-store run with no pending large migration and MUST NOT be cited as migration latency.

Startup MUST remain unavailable until the store is safe to serve; it MUST NOT open the normal socket early merely to satisfy the controller. Instead, the child writes an atomic, same-directory runtime-state record with these observable phases:

```text
opening_store -> migrating -> serving
                         \-> failed
```

The record includes PID, phase timestamps, source and target schema versions, current migration version/name or step, a fresh heartbeat, and a sanitized terminal error. Migration callbacks also emit structured start/progress/completion timings. The detached parent keeps waiting beyond the legacy 60 seconds only while the child PID is alive and the state heartbeat/progress is fresh; absent state, stale state, an exited child, or a terminal failure preserves prompt failure behavior. User cancellation remains effective. This turns a healthy long migration into visible progress without disguising a hung process.

Migration timing and peak-space claims require an isolated copy of a realistically large pre-v14 store. The overwritten incident log and a fresh empty-store startup are insufficient. Final validation MUST record per-step wall time, total migration time, peak RSS, peak database/WAL/temp disk use, heartbeat cadence, parent wait behavior, rollback/retry behavior, and the first post-migration query.

#### Bounded performance evidence and safety caveat

The bounded pre-fix/fix-development benchmarks captured the following ranges on this machine:

| Path | Observed range |
| --- | ---: |
| Dedicated-base refresh admission/coalescing | 14.5–15.0 ns/op, 0 allocs/op |
| Stable adopted-commit dirty reconciliation | 22.8–74.8 ms/op, about 72–74 KiB and 897–899 allocs/op |
| Stable Git topology watcher tick | 39–69 µs/op, about 4.7–5.2 KiB and 37–38 allocs/op |
| Ready-generation catalog hit | 330–359 µs/op, about 4.9 KiB and 150 allocs/op |
| Pinned generation winner selection | 0.82–1.12 ms/op |
| Cold 100-item generation payload | 5.8–7.9 ms/op, about 510 KiB and 5,668 allocs/op |
| Checkout adoption | 454–487 µs/op, about 11.9 KiB and 372 allocs/op |
| Ready dedicated-graph lookup | about 12.7 µs/op, 1,120 B and 37 allocs/op |
| Generation-scoped `AllNodes` / `AllEdges` | about 101 µs/op / 52 µs/op |

These microbenchmarks bound admission, catalog, watcher, and small-payload mechanics; they do not prove whole-repository cold/warm behavior. An attempted self-repository incremental benchmark was stopped after roughly five minutes when actual RSS reached about 4.1 GiB (about 5.1 GiB reported by the benchmark process). It is excluded from acceptance evidence and MUST NOT be rerun against the shared developer environment. Repository-scale validation uses isolated stores, bounded fixtures, explicit time/memory limits, and phase metrics.

#### Implemented agent-facing worktree guidance

Commit `f9edcb14` centralizes the routing contract in
`profiles.WorktreeBranchRoutingPolicy` and renders it through profiles, agent
adapters, Claude/Cursor integrations, session-start and subagent hooks, and MCP
initialize instructions. The generated instructions teach the routing model
once in the shared profile rather than duplicating divergent prose through
every skill:

- use the session/current working-tree checkout and let discovery create an automatic overlay;
- do not explicitly track every linked worktree unless the user asks for an independent dedicated graph;
- understand “overall” as the selected overlay plus its one designated family primary, never a union of incompatible branches. Unique overlay/base results retain their relevance order; only duplicate logical identities prefer the overlay, while tombstone/path masks hide deleted base data;
- distinguish a normal building fallback from a sealed availability/removal fallback. Both must be labeled. The sealed fallback has no checkout dirty state or editor buffers; capability metadata governs exact/source/file/AST, filesystem `search.text`, LSP, and edits, which must refuse rather than silently use another checkout;
- treat an inactive `git_ref`/commit view as a committed graph and Git-object source snapshot: structural/source/content operations may work, but working-copy LSP, filesystem `search.text`, and edits do not;
- expect explicit tracking to promote to dedicated ownership and supported explicit untrack to demote over a surviving primary;
- require preview/confirmation before moving the primary or forgetting a primary closure/family;
- report view/freshness/capability metadata instead of silently describing a fallback as exact.

Golden renders, profile drift tests, adapter/hook tests, and MCP initialize tests
assert that the canonical policy appears exactly once. State-aware tests also
distinguish an unrelated repository from either direction of an already-tracked
Git family and explicitly suppress `gortex track` while automatic discovery is
pending. Individual task skills may link to the shared rule; they need separate
prose only when they add workflow-specific behavior.

#### Post-fix validation ledger

Focused correctness coverage MUST include:

1. cold `Seed` performs exactly one dedicated-base build, publishes one active generation and route, and persists one explicit intent;
2. unchanged second `Seed` performs zero corpus builds and preserves graph/generation/checkout/route identities;
3. a linked automatic checkout has no coordinator while promotion is gated, then starts after base publication;
4. current-owner topology reconciliation discovers addition and forgets authoritative removal under repeated and race-enabled scheduling;
5. new grace-period requests receive only labeled read-only primary fallback, while exact/file/edit requests fail and pinned readers drain;
6. startup-state tests cover fresh progress, stale/no state, child exit, failure, cancellation, and a migration lasting longer than 60 seconds;
7. migration rollback/retry leaves a valid pre-migration store and eventually publishes schema 20 exactly once;
8. generated agent instructions describe automatic overlays, explicit dedicated graphs, and fallback limitations (`f9edcb14`);
9. inactive local refs retain the V1 structural-only behavior and report unavailable LSP-only capabilities.
10. project-only configuration cold/warm startup produces one physical build,
    no synthetic top-level config row, stable identities, and exact
    provenance-bearing intents; removing one source preserves every independent
    remaining source;
11. primary retirement plus explicit retrack does not elect a primary,
    idempotent rebinding preserves the current primary, and concurrent first
    bindings produce exactly one primary;
12. implicit session/CWD admission creates only an automatic checkout over an
    already-ready primary, never calls `TrackRepoCtx`, never writes explicit
    intent/config, is coalesced per canonical checkout, and remains deferred
    without a primary;
13. v19→v20 migration creates the move journal, preserves existing data,
    retries safely after failure, reopens idempotently, and cascade-deletes
    markers with checkout/family cleanup;
14. A→B→C rejects stale root observations and stale completion, preserves the
    earliest previous/latest current roots, repairs after each crash cut, and
    leaves zero journal rows after success;
15. automatic and dedicated moves preserve checkout/incarnation/graph/
    generation/route identity, stop old runtime sources, bind new ones, avoid
    corpus builds, relocate config and path-bearing intents, preserve logical
    project locators, and retry after config-save failure;
16. canonical aliases and missing old leaves are identity-equal for assertions,
    config repair, cleanup, and journal completion.

The final isolated validation MUST use a dedicated configuration/store/socket namespace and MUST NOT stop, restart, attach to, or mutate the machine-wide daemon or its store. Record the following before replacing these placeholders:

| Acceptance evidence | Final isolated result |
| --- | --- |
| Feature revision and binary version | **TODO(final isolated E2E):** revision/version |
| Fixture shape and pre-migration schema/store size | **TODO(final isolated E2E):** repositories, worktrees, refs, files, bytes, and an explicit populated v19 store so migration 20 is exercised |
| Migration duration/progress and peak RSS/disk | **TODO(final isolated E2E):** per-step/total time, heartbeat observations, memory and disk peaks |
| Fresh cold start | **TODO(final isolated E2E):** queryable/full-ready times and exactly-one physical build count |
| Unchanged warm start | **TODO(final isolated E2E):** startup time, zero physical builds, stable identities/routes |
| Automatic worktree discovery | **TODO(final isolated E2E):** discovery-to-ready latency and sparse generation size |
| Overlay correctness | **TODO(final isolated E2E):** same-symbol shadowing, unrelated-result ranking, deletion masking, dirty/untracked visibility |
| Grace/removal behavior | **TODO(final isolated E2E):** immediate labeled fallback, exact/file/edit rejection, reader drain, 30-second cleanup |
| Branch/ref behavior | **TODO(final isolated E2E):** checked-out switch/cache reuse and inactive-ref structural capability report |
| Configuration/provenance and implicit CWD admission | **TODO(final isolated E2E):** project-only cold/warm registration, independent provenance, same-family admission, and no-primary refusal |
| Automatic/dedicated move and restart-journal recovery | **TODO(final isolated E2E):** stable identities, zero rebuilds, A→B→C, crash cuts, alias identity, and empty journal after convergence |
| Resource ceiling | **TODO(final isolated E2E):** peak RSS, allocations if profiled, database/WAL size, descriptor count |
| Test/benchmark commands | **TODO(final isolated E2E):** focused, race, migration, benchmark, lint, and full-suite outcomes |

No release claim is complete while any final isolated E2E placeholder remains unresolved.

#### Corrected cold-start failure chain and atomic repair evidence

The later clean-store reproduction disproved the hypothesis that the remaining
failure was still generation-zero duplication. Its startup census reported
`configured_repos=29` and `legacy_jobs=0`, so configured Git repositories were
already excluded from the legacy owner. The actual first failure was the
configured `growth` repository: it was a valid unborn Git repository with no
commit, but with source present in its index/working tree. Promotion attempted
to resolve `HEAD^{tree}`, treated the missing commit tree as fatal, rolled the
provisional graph back, and left a coordinator referring to the deleted graph.
Three secondary bugs then amplified that first failure:

1. the durable transition worker refilled its queue only after success, so the
   failed first item starved the remaining 27 configured repositories;
2. retry reconstruction looked up the deleted provisional graph before it
   recovered the stable repo prefix/config identity, so the promotion could
   not repair itself without a restart;
3. the stale coordinator polled every 15 seconds and repeatedly reported
   `primary base unavailable: designated graph ... does not exist` instead of
   retiring after the authoritative graph deletion.

The daemon consequently advertised ready after roughly two seconds with zero
documents while the intended configured-startup cohort had not published. The
published readiness state was process readiness, not corpus readiness. This is
the direct explanation for the observed `ready (warmup 2s)`, `docs=0`, and 29
`not indexed` rows after the clean restart.

An unborn repository now uses Git's canonical empty-tree object (SHA-1 or
SHA-256 as appropriate) as its immutable base, with staged and non-ignored
untracked files represented only in the dirty layer. Ignored files remain
excluded. Empty-tree resolution is cached per coordinator. A failed promotion
no longer blocks later durable work; retry restores the stable prefix from
config/root identity; coordinators are installed only over published graphs
and terminate after authoritative graph deletion. Startup readiness freezes the
exact configured-Git cohort after seed and remains `building`/`degraded` until
every member has a valid route. The socket may serve during that interval, but
responses carry partial/fallback metadata rather than a false global-ready
claim.

The following changes were deliberately committed as independent fixes. The
measurements are development-machine bounds, not whole-daemon acceptance
results:

| Atomic repair | Focused benchmark/result |
| --- | ---: |
| durable queue continues after one failed promotion | 143–153 ns/op, 448 B, 3 allocs |
| unborn empty-tree resolution | first resolve 6.1–6.6 ms; cached 22.8–23.5 ns, 0 allocs |
| failed-promotion prefix recovery | 512-entry worst lookup 0.993–1.004 ms |
| publication/coordinator classification | missing graph 9.5–10 us; unpublished graph 21.2–21.6 us |
| configured startup readiness snapshot | 256 repos 14.4–15.0 us, 28.2 KiB, 5 allocs |
| generation shadow publication | 2,050 files: direct 2.54–2.64 s; shadow 1.67–1.69 s |
| generation content finalization | 10,000 rows / 5,000 retained files 72.6–74.8 ms |
| concurrent daemon-start loser wait | 700 polls / 70 virtual seconds 28.8–29.5 us, 48 B |
| migration success boundary | v18 tail 5.25–5.67 ms; representative 10k-row v13→v19 10.12–10.47 ms |
| routed status aggregation | 256 rows 130–149 us; no graph-row recount |
| sparse reachability scope | 2,050 files: direct 3.04–3.24 s; shadow 1.75–1.97 s |
| removal-grace text refusal | 1.13–1.37 us, 969 B, 14 allocs |
| provenance-bearing registration snapshot (`1624e938`) | 256 physical/768 sources: 4.292–4.397 ms, about 1.47 MB, 13,575 allocs; 1,000 physical/3,000 sources: 17.067–17.571 ms, about 5.72–5.75 MB, 53,019 allocs |
| primary designation (`02b09b20`, benchmark `2def4188`) | first graph in an empty family: 0.326–1.417 ms, 3,552–3,553 B, 101 allocs; family with 256 surviving non-primary graphs: 0.722–1.005 ms, 150,272 B, 3,182 allocs; idempotent current-primary binding: 12.84–13.08 us, 1,167–1,168 B, 37 allocs |
| agent automatic-checkout guidance (`c4124d52`) | 1 tracked root: 40.3–76.4 us; 32 roots: 573.3–618.5 us; 256 roots: 4.42–4.68 ms; same-family matches and unrelated repositories measured separately |

The shadow benchmark's isolated peak RSS was approximately 210 MiB versus
196 MiB for the direct path (about +14 MiB / +7.1%). It therefore demonstrates
bounded staging overhead for the fixture; it does not by itself close the
repository-scale 40+ GiB incident. That closure requires the final isolated
cold/warm run and resource ceiling in the ledger above.

Generation payload publication also exposed two integrity requirements that
are easy to miss when optimizing the build:

- derived generations must take the shadow fast path without being mistaken
  for generation zero; the drain must preserve workspace/project identity,
  builtins, masks, producer metadata, symbol/content FTS ownership, and sibling
  generations;
- authoritative content cleanup is keyed to completion of the content-source
  walk, not to an mtime snapshot. Git/file-set sources intentionally have zero
  mtimes. Cleanup runs only after all parse workers join and cancellation is
  checked, and only against the target generation; an interrupted walk retains
  unvisited rows for retry.

Likewise, reachability invalidation records whether the destination writes base
topology before any in-memory shadow substitution. Re-reading the staging graph
at the deferred invalidation point misclassifies a derived build as base and
retires unrelated base reach records.

#### Status, migration, and removal-grace observability contracts

`daemon status` must describe the selected routed view, never the empty
generation-zero process shell used to host a durable route. Exact files/nodes/
edges may be reported from persisted dedicated-base counters only when every
upper generation is provably a no-op. A changed sparse overlay remains `ready`
but reports `counts_known=false`; exact composed totals are not manufactured by
summing overlapping physical generations. Memory attribution is a separate
truth value: structural counts do not make process-wide search/vector bytes
attributable to one routed generation, so routed rows currently report
`memory_known=false`. Workspace totals propagate unknown counts. If a catalog
read fails, only identities from the last successful routed snapshot degrade;
unrelated non-Git/generation-zero rows remain authoritative.

Migration `PRAGMA user_version` is the final durable success marker, after
in-place steps, core/sidecar index repair, and old-shape analysis invalidation.
A failure in that tail leaves the old version so the next `Open` retries the
idempotent work. Concurrent autostart callers use a 60-second *inactivity*
budget renewed only by a fresh PID-bound startup heartbeat; a healthy migration
may therefore exceed the old 65-second total ceiling, while cancellation, stale
progress, process exit, socket availability, and lock release remain prompt
terminal conditions.

Raw `search_text` is a selected-working-copy filesystem capability under
`TEXT-SEARCH-VIEW-1`; an immutable graph generation has no raw-code trigram
corpus. During removal grace it MUST NOT fall through to the removed checkout,
the primary checkout's potentially dirty bytes, or editor buffers. The request
may resolve the sealed fallback identity solely to return a labeled
`capability_unavailable`; it MUST NOT claim `search.text` was base-scoped.
Graph/symbol searches that need no selected filesystem remain eligible for the
read-only labeled base fallback. Exact file/read/edit requests remain strict.

#### Additional lifecycle gaps found by the post-fix audit

The audit findings now have mixed dispositions. Runtime changes remain
benchmark- and E2E-gated; policy-only and test-only corrections are
validation-gated. Resolved findings remain listed as regression contracts:

1. **RESOLVED by `1624e938`:** configuration exposes both a canonical physical
   registration and every independent global/project source. Startup, reload,
   watchers, and offline cleanup consume that provenance. Project-only cold and
   warm startup, independent-source retention, CLI/MCP ownership survival,
   offline removal, and fail-closed unresolved aliases are regression contracts;
2. implicit CWD discovery must never call the full `TrackRepoCtx` path or bind
   a dedicated graph. With a primary it records an automatic checkout and lets
   the coordinator build sparse layers; without a primary it creates no hidden
   primary/full corpus and remains explicitly deferred. Admission uses the
   session CWD/path, not the daemon process CWD, and runs before the dispatcher
   rejects an uncovered root. A daemon-wide `sync.Once` is invalid because
   different sessions can introduce different linked worktrees. Concurrent
   admission is coalesced per canonical checkout and catalog allocation is
   guarded by the same ready primary generation observed before the write;
3. **RESOLVED by `02b09b20`:** only the first graph in an empty family is
   automatically designated primary. Primary loss with surviving dedicated
   siblings stays primary-less until the explicit, previewed `set-primary`
   action;
4. `git worktree move` is implemented by the schema-v20 root-move patch and
   remains benchmark/E2E-gated. A real root change updates the checkout and
   upserts `checkout_root_moves` in one transaction under `(checkout_id,
   incarnation, expected_root_path)`. The journal—not incidental config/locator
   mismatch—is the authoritative crash-recovery marker. It retains the earliest
   uncompleted previous root and newest current root across A→B→C. Automatic
   moves swap only the process coordinator/source watcher; dedicated moves
   rebind the process shell, coordinator, dirty sampler, watcher, config, and
   path-bearing CLI/MCP/manual locators without `TrackRepoCtx`, corpus rebuild,
   generation change, or route-epoch change. Project-membership intent locators
   remain logical and pathless. Completion deletes only the exact journal row
   after all runtime, config, and intent state has converged, so delayed A→B
   completion cannot clear B→C;
5. **PARTIALLY RESOLVED by `72f92185`:** lifecycle tests now compare canonical
   identity in embedded-index and offline track assertions. The schema-v20 move
   patch owns the broader config relocation, missing-old-leaf canonicalization,
   cleanup, and move-lifecycle identity contract, so macOS `/var/...` and
   `/private/var/...` aliases neither fail valid checks nor hide stale state.

The audit revalidated event-driven add/remove discovery (one-second bounded
topology probe), ordinary demotion/primary closure, A→B→A commit-layer reuse,
inactive-ref structural views and write refusal, and duplicate-only overlay
precedence. Those areas remain subject to the final isolated E2E, but no new
implementation gap was found in their focused coverage.

### CI follow-up: reload demotion atomicity (2026-08-30)

The first CI run after the mainline merge found 30 deterministic lint issues
and two failures that appeared only in Linux's repository-wide `-race` job.
macOS and the narrower local suites passed, so both failing tests were
reproduced with the exact race/coverage shape before their contracts changed.

#### Reload removal is a guarded demotion

`CheckoutLifecycle.ApplyReload` previously sent a configured checkout that
left the config through `Reconciler.RetireCheckout`. That path predates sparse
worktree views and deliberately forgot a demotable non-primary checkout. The
reload result therefore reported one removal and lost the automatic checkout,
route, coordinator, and prompt source monitoring. The test also saved config
without reloading its `ConfigManager`, unlike the real controller, which made
the stale behavior filesystem/mtime dependent.

Reload now resolves the checkout, previews untrack, and uses the same guarded
demotion transaction as explicit untrack only for `UntrackPlanDemote`.
Primary, inaccessible, and otherwise non-demotable cases keep their existing
pending transition. A successful demotion counts as neither removed nor
pending because the checkout remains live as an automatic overlay.

Demotion publication has the following atomic contract:

1. A dormant coordinator prepares the primary commit and dirty generations
   off-route while capturing the complete old route.
2. `CommitAuthorizedDemotion` revalidates checkout/incarnation, intent,
   primary epoch, active primary base, candidate generation chain, and the old
   route's graph, commit, dirty, epoch, and state in one SQLite transaction.
3. That transaction publishes the primary stack with the next route epoch,
   flips effective mode to automatic, and journals retirement of the owned
   graph. A concurrent old-coordinator write makes the transaction stale and
   leaves mode, route, and cleanup unchanged.
4. Automatic checkout route writers may name only their family's designated
   primary. This fences an old dedicated coordinator after commit across route
   install, full-stack commit, route flip, and slot/lease publication.
5. Only after durable commit does a pointer-CAS replace the registered
   coordinator. The old coordinator is closed and drained, so its mutation
   tickets fail truthfully rather than transferring to another generation.
   Exactly one lifecycle source watcher is then attached to the new
   primary-bound coordinator.

Idempotent replay recognizes the exact committed route/mode before requiring
the retired owned graph to still exist. A lost response can therefore replay
after cleanup without advancing the route twice. Pre-commit failure admits no
watcher and preserves the old coordinator/route; post-commit cleanup failure
still preserves the coherent automatic coordinator and route.

The regression writes a new non-ignored source file, injects only a filesystem
watcher event, and requires publication within five seconds, below the
coordinator's 15-second polling interval. It also proves repeated admission
keeps one backend and removal synchronously stops and unregisters it.

#### Inactive-ref withdrawal remains asynchronous

`TestRefViewPrunedObjectWithdrawsTheSourceCapability` asserted producer state
immediately after `ScheduleProducerWithdrawal`. The file read correctly
returned `source_object_missing`, but under Linux race scheduling the worker
had not always changed `source.snapshot` from `complete`. The production path
remains intentionally non-blocking. The test now polls the public producer
state with a bounded timeout, then still verifies the same generation/tree,
the withdrawn source capability, and the surviving structural graph. It does
not call or drain the worker itself, so a rejected or failed withdrawal still
fails visibly.

#### Lint reconciliation

The 30 merge-era findings comprised eight unchecked errors, one ineffective
cleanup assignment, eight staticcheck simplifications, and thirteen unused
helpers. Asynchronous watcher stop/retirement errors are now recorded and
logged. The unused production methods were zero-caller compatibility wrappers;
their guarded/context-aware replacements remain. No lint suppression was
added.

#### Final evidence

| Scope | Result |
| --- | --- |
| Exact lint | `golangci-lint run --timeout=10m` PASS, 0 issues |
| Reload/demotion repetitions | Normal `-count=20` PASS in 114.562 s; race `-count=10` PASS in 115.263 s; race+coverage `-count=5` PASS in 59.199 s |
| Atomic catalog matrix | Success/epoch-once/cleanup-completed replay, route/base staleness, injected route/mode rollback, and stale Flip/stack/slot/upsert writers all PASS; normal `-count=20` 4.292 s, race `-count=10` 45.065 s |
| Ref withdrawal | Target normal `-count=50` PASS in 57.794 s; race `-count=30` PASS in 92.409 s; exact race+coverage `-count=10` PASS in 28.217 s |
| Full contract race | SQLite PASS 941.664 s; indexer PASS 1104.686 s; reconcile PASS 41.975 s |
| Repository-wide bounded gate | `go test -p=2 ./... -count=1 -timeout=30m` PASS; SQLite 53.878 s, indexer 553.052 s, MCP 207.555 s, every package green |

No machine daemon was stopped, restarted, reconfigured, tracked, untracked, or
mutated during this follow-up.

### Topology publication and mutation-fence audit (2026-09-01)

The final concurrency audit found four correctness hazards that ordinary
single-checkout tests could not expose:

1. a primary-closure saga could invoke a nested checkout-removal callback while
   an outer family/checkout guard was still held. Root convergence acquires the
   global publication mutex before those guards, so the callback created the
   reverse edge and a hard AB/BA deadlock;
2. shutdown waited for retry/transition workers before failing their
   transferred mutation tickets, and it tried to drain a checkout cohort while
   holding that cohort incrementally. Either shape could deadlock a nested
   primary closure;
3. coordinator convergence waited for per-checkout fences while holding the
   global root-publication locks, blocking unrelated Git families. It also acted
   on the report captured before that wait, so a stale non-ready report could
   withdraw a route and coordinator that had already recovered;
4. the family, checkout, and graph semaphore registries retained every identity
   ever seen. Deleting a raw map entry was not safe: a caller can resolve the
   old gate before entering its semaphore, then overlap a recreated identity on
   a new gate.

The implemented lock and publication contract is:

```text
family gate -> complete sorted checkout cohort -> graph gate

root publication:
  topologyPublishMu -> moveMu -> durable root convergence
  release moveMu -> publish topology event -> release topologyPublishMu
  then acquire per-family/per-checkout gates for coordinator convergence

terminal saga publication:
  durable terminal journal delete
  -> unwind every inherited family/checkout guard
  -> graph completion(s)
  -> checkout completion(s)
  -> family completion
```

Primary retirement precomputes and acquires the complete family checkout
cohort in one canonical call. A nested primary closure refuses a partially
inherited cohort instead of extending it in an order that cannot be proven
safe. Terminal events are collected by the outermost saga boundary, deduped by
durable identity, and flushed after all topology guards unwind. Completed
nested events still flush if a later parent phase fails, because their journal
rows are already gone and replay cannot reproduce them.

`CheckoutLifecycle.Close` closes retry admission first, cancels and fails every
registered and started coordinator's mutation waiters before joining workers,
then drains checkout gates one at a time. It never owns a multi-checkout cohort
during shutdown. Coordinator reports are now wakeups only: after publication
locks are released, each entry acquires its family/checkout fence, rereads the
current checkout, incarnation, move journal, graph and route, and acts only on
that current state.

#### Lease-aware gate reclamation

Every registry lookup now acquires a reference while holding the registry
mutex, before waiting on the semaphore. A multi-key lookup reserves all
references atomically or none. Retirement marks the entry `retiring`, makes new
lookups wait and re-resolve, drains outstanding holders and pre-acquire waiters,
then runs an authoritative catalog guard. Cancellation, catalog error, or guard
loss reactivates the same entry. Successful deletion wakes waiters so they
resolve a fresh gate; old and new identities can therefore never execute on two
independent semaphores.

Reclamation is tied to logical terminal edges, not daemon shutdown:

- checkout gates retire after the exact-incarnation checkout-removal callback;
- family gates retire only after `forget_family` has deleted its terminal
  journal. Primary loss with independent dedicated graphs does not retire the
  family gate;
- graph gates retire after terminal graph removal or successful provisional
  promotion rollback. The final guard is one SQLite snapshot over
  `dedicated_graphs`, `checkout_routes`, `view_generations`, and `ref_views`.
  Retained generations/ref views keep a process-local pending ID that is retried
  after generation and ref-view sweeps. `view_layers` are immutable metadata and
  are deliberately excluded because they have no graph-gate users and may
  outlive the graph.

Terminal callbacks notify without holding `topologyPublishMu` during fence
drain. One shared one-second deadline bounds each retirement pass; a stalled
holder leaves the ID pending instead of multiplying janitor latency by the
number of identities. The retry sets are process-local because the gates
themselves do not survive restart.

#### Measured cost and regression evidence

Measurements are five runs with `-benchtime=2s -benchmem` on Apple M1 Pro:

| Mechanism | Final observed range |
| --- | ---: |
| Empty lifecycle close | 456.1–471.1 ns/op, 1,072 B, 8 allocs |
| Gate registry steady acquire/release | 141.0–151.3 ns/op, 96 B, 4 allocs |
| Gate retirement plus same-ID recreation | 317.2–336.6 ns/op, 448 B, 8 allocs |
| Full mutation admission/publication | 239.8–261.6 µs/op, about 5.03 KiB, 141–142 allocs |
| Ready-route guard | 11.82–12.01 µs/op, 1,032 B, 32 allocs |
| Checkout topology token acquire/release | 106.7–107.8 µs/op, about 9.34 KiB, 248 allocs |
| Family topology token acquire/release | 141.9–144.5 ns/op, 96 B, 4 allocs |
| Settled coordinator cycle | 98.77–100.8 ns/op, 96 B, 2 allocs |

Relative to the pre-reclamation mutation admission sample (108.361 µs,
9,279 B, 246 allocations), the topology-token path remains latency-neutral;
the two lookup leases cost approximately 65 B and two allocations. This is the
bounded price of closing the lookup-before-semaphore race.

The combined race command ran the nested primary closure, partial-cohort
refusal, ordered terminal publications, later-parent failure, shutdown ticket
cycle, individual gate drain, stale coordinator recovery, unrelated-family
publication, checkout/graph/family reclamation, retained generation, catalog
ownership, concurrent recreation, and 10,000-ID churn tests ten times. All
three packages passed (`reconcile` 32.087 s, `indexer` 81.798 s,
`store_sqlite` 29.314 s). `golangci-lint run --timeout=10m` reported zero
issues, `go vet` passed for the four affected packages, and `git diff --check`
passed. These results close the concurrency findings; they do not replace the
isolated production-daemon cold/warm acceptance ledger above.

### Durable checkout commit cache and schema-v21 follow-up (2026-09-01)

The final branch-switch/restart audit found that same-process A→B→A success did
not yet prove the V1 cache contract. This section is normative where it uses
MUST/MUST NOT and records the implementation evidence that motivated schema
v21. It does not mark the final isolated acceptance ledger complete.

#### Restart defect and ownership model

The coordinator's retained-commit LRU was process-local. On a fresh process it
was empty, even though immutable ready generations remained in SQLite.
`CheckoutLifecycle.Seed` ran its retirement sweep before configured
registration/family reconciliation recreated coordinators. At that point the
served-coordinator set was empty and the lifecycle classified every ready
checkout commit/dirty generation not named by the one persisted route as an
orphan. Consequently the general cache was deterministically discarded during
warm restart, not merely vulnerable to a rare race.

A branch changed while the daemon was stopped exposed a second edge. The old A
generation survived the pre-coordinator sweep only because the persisted route
still named A. Reconciliation then adopted/built B and released the old routed
A through the process-local cache path, which immediately made A retireable.
The daemon could therefore answer B correctly while destroying the generation
needed for a subsequent cached return to A. Restart reuse already worked for
durably retained inactive-ref views; checked-out branches had no equivalent
durable holder.

The corrective ownership model is:

```text
durable payload identity     ReadyGenerationCacheKey -> generation_id
durable checkout claim       (checkout_id, graph_id, generation_id, last_selected)
transient build handoff      ready-generation lease
transient lookup accelerator coordinator retained/LRU map
```

Only the first two survive process restart. A local LRU may avoid catalog
lookups, but losing it changes no lifetime or reuse semantics. The durable
claim is checkout-scoped and denormalizes the generation's graph so quota and
cleanup remain graph-local and bounded. Every write verifies that pin and
generation graph IDs agree. Promotion, demotion, and rehome revoke that
checkout's old-graph claims and pin the newly routed commit in the new graph.
Deleting the holder checkout revokes all of its claims; deleting a graph
explicitly revokes every claim for that graph before generation deletion.

#### Schema-v21 migration and SQL integrity

Schema v21 adds `checkout_commit_cache_pins`,
`checkout_commit_cache_retirements`, the narrow pin-delete handoff trigger, and
the pin indexes. The pin primary key is `(checkout_id, generation_id)`. It
includes a denormalized `graph_id TEXT` with no graph foreign key; catalog
mutations and integrity checks require it to equal the referenced generation's
graph. The checkout foreign key uses `ON DELETE CASCADE`, and the generation
foreign key uses `ON DELETE RESTRICT`. The retirement queue has one row per
generation and uses `ON DELETE CASCADE`, because it is retry work rather than an
owner.

Upgrade runs synchronously inside the normal store-open migration transaction:

1. create the pin table, retirement queue, pin-delete trigger, uniqueness key,
   generation-reference index, and retention-order support;
2. conservatively backfill every eligible checkout-owned ready or superseded
   immutable commit generation whose owner checkout and graph still exist;
3. backfill every current ready checkout commit route, including a route that
   adopted a generation owned by a ref or different checkout;
4. verify that every inserted generation is eligible immutable commit content,
   has a matching denormalized graph,
   belongs to an existing graph, and that every current ready commit route has
   exactly one holder row; and
5. set `PRAGMA user_version=21` last and commit.

The migration query is deliberately not count/TTL/byte capped: immediately
after recovery resumes, and before ordinary orphan deletion, Seed applies
`RETENTION-1` per graph to the complete conservative backfill. The migration
MUST finish before `CheckoutLifecycle.Seed` calls orphan or retirement
discovery. It is a catalog-sized `INSERT ... SELECT`, not a payload
migration: it reads no source tree, starts no indexer/coordinator, and writes no
node, edge, file, mask, FTS, vector, or sidecar row. A failure rolls back the
whole v21 step and leaves v20 authoritative for retry. Startup progress reports
the v21 step and heartbeat through the existing migration state channel; a
socket wait timeout MUST NOT be disguised as repository indexing. Existing
v20 cache candidates without checkout ownership or a routed checkout adopter
remain governed by their existing ref/lower/base references; every eligible
checkout-owned candidate and every current routed commit is conservatively
represented before Seed applies the bounds.

The following SQL paths MUST understand pins:

- ready claim and checkout bind, including lease consumption;
- route upsert, full-route flip, commit-slot flip, base-route install, and
  authorized promotion/demotion publication;
- checkout and dedicated-graph deletion;
- Seed-only ready-layer orphan discovery, runtime durable-queue candidates,
  and explicit lifecycle/coordinator retirement backlogs;
- generation reference reporting/refusal; and
- both the early retirement query and final transaction-time payload delete
  predicate.

The final delete guard is mandatory even when candidate discovery already saw
no pin: a concurrent successful route bind may create one between those steps.
The restrictive generation foreign key is a last integrity fence, not a
substitute for returning a truthful `generation_referenced` result. Intentional
checkout/graph/family cleanup deletes only pins inside its authorized
incarnation/primary-epoch closure and then retries bounded lease-aware payload
retirement.

Every pin deletion MUST enqueue its generation in the same SQLite transaction.
This includes explicit retention pruning, route/graph transitions, direct
catalog deletion, and implicit checkout foreign-key cascade. A semantic helper
alone is insufficient because a future caller or cascade can bypass it; schema
v21 therefore uses a narrow `AFTER DELETE` trigger on
`checkout_commit_cache_pins`. Re-pinning removes the queued row atomically,
successful generation deletion removes it by foreign-key cascade, and a
refused retirement deliberately leaves it for retry. Runtime candidate reads
are read-only and exclude pins, routes, ref views, child layers, and dedicated
graph ownership. Lazy ready-generation leases are checked by the final
writer-gated retirement predicate, so candidate discovery cannot manufacture a
lease merely to prove that one is absent.

When a candidate is re-pinned, both coordinator and lifecycle process-local
retirement debt MUST be discarded after the durable holder is observed. The
pin is now the lifetime authority, and its eventual deletion trigger will
recreate the durable handoff. Retaining the stale in-memory offer would make
every janitor poll reread it and let independent prune/re-pin races grow retry
maps without bound.

Generic READY-layer orphan inference is startup recovery only. `Seed` first
backfills routed durable pins and then performs the conservative scan before any
checkout build is admitted. Runtime cleanup consumes explicit coordinator and
lifecycle backlogs, terminal/abandoned rows, missing-graph rows, and the durable
retirement queue. It MUST NOT infer liveness from a snapshot of registered or
served coordinators: temporary promotion/base-refresh coordinators are not
necessarily registered, and both commit and dirty builders have a valid
build-return-to-route-publication interval. A crash inside that interval is
recovered by the next Seed scan; a live publication is protected from runtime
collection without extending process-local ownership into a durability
contract.

#### Owner-independent compatibility and idle writer contract

The ready key MUST exclude `owner_kind`, checkout/ref/layer IDs, repository path
aliases, and selector spelling. It MUST include graph, lower/base identity,
target tree, complete build fingerprint, sanitized source configuration,
extractor/resolver/producer versions, completeness profile, and any
producer-declared commit-sensitive input. A generation produced for a ref or
sibling checkout is therefore adoptable without changing its immutable owner
metadata when the key and required capabilities match.

Pins do not override capability truth. A candidate whose `source.snapshot` or
another required producer has been withdrawn is bypassed even while a holder
row exists. The coordinator must build/adopt a compatible replacement, publish
it atomically, and allow the obsolete claim to expire or be revoked by the
cleanup path. A withdrawal race can never make an exact file read route to a
generation that no longer has source capability.

Once the routed commit and independently validated dirty layer match the current
sample, every subsequent poll is observational. It performs zero ready-cache
claims, lease acquisition/release, pin timestamp refreshes, checkout-HEAD
writes, route/epoch writes, generation builds, or WAL writes. `last_selected`
is refreshed only on an actual selection/route transition; otherwise polling
would make inactive generations immortal and recreate the writer/WAL storm that
the cache is intended to prevent.

#### Retention and cleanup boundaries

The quota is an inactive-cache allowance, not a total graph-size ceiling. Only
a currently routed commit generation is excluded from the 32 inactive
generations and 5-GiB inactive byte allowance. A ref, lower/base relationship,
sealed dedicated/primary ownership, reader/build/cache-claim lease, or other
durable reference still blocks deletion while present, but does not create a
second accounting exemption. A non-routed candidate's effective age is the
maximum `last_selected` across holders. Graph/search physical bytes are tracked
separately and a generation shared by several holders is counted once in its
denormalized pin graph.

Retention pruning applies all three defaults—seven inactive days, 32 inactive
tree generations, and 5 GiB per graph—and selects deterministic oldest victims
with generation ID as tie break. It removes an expired/selected holder only
after checking whether another holder should keep it; payload retirement occurs
through the durable queue and only after the final transactional reference
guard passes. Refusal preserves the queue row; re-pin clears it, and pagination
must eventually visit more than one cleanup page without losing or duplicating
ownership. The following cleanup boundaries are explicit:

- same-graph branch switch or ordinary route withdrawal retains inactive
  commit pins;
- availability/removal grace creates or refreshes no pin and cannot route a
  fallback through cached checkout content;
- `purge_inaccessible_layers` revokes that checkout's pins with its other
  rebuildable state while preserving explicit identity/sealed graph;
- `forget_checkout` cascades that checkout holder and leaves shared/ref holders;
- promotion, demotion, and rehome revoke that checkout's old-graph pins and pin
  the newly routed compatible commit in the new graph as part of the authorized
  transition; an idempotent publication replay removes only pins outside the
  target graph, preserving branch history selected after the first publication;
- deletion of a provisional/owned/primary graph revokes every pin for that
  graph before payload retirement;
- primary closure/family forget removes only the authorized primary/dependent
  closure; healthy independent dedicated graphs and their holders survive; and
- source-capability withdrawal is compatibility invalidation, not a reason to
  ignore a live route/ref/lease deletion guard.

#### Cold-indexing failure lessons

Durable branch caching does not weaken the cold-start ownership invariant. A
store deletion has no generations to reuse, but it still performs exactly one
full physical build per explicitly configured Git corpus. The lifecycle path
owns configured Git checkout indexing; the legacy generation-zero warmup path
MUST exclude it. Dependent automatic worktrees start only after their primary
base is ready and then build sparse commit/dirty layers. This prevents the
observed N+1-shaped duplicate corpus, unpublished generation-zero rows, and the
40+ GiB machine-wide reliability incident described above.

A later isolated cold attempt exposed a separate failure mode: a sparse
generation physical build ran for approximately 2,910 seconds, `AddBatch`
received SQLite `database or disk is full`, and a fatal-store panic terminated
the daemon. Capacity/resource exhaustion is an expected build failure, not a
process invariant violation. Cold and sparse builders MUST:

- preflight bounded database/WAL/temp headroom and report the estimate;
- bound concurrent corpus memory and writer admission across repositories;
- publish phase/file/row/byte progress so a long build is distinguishable from
  a stalled migration or dead daemon;
- convert `SQLITE_FULL`, quota, I/O, and cancellation into a failed unpublished
  generation plus structured error, release every memory/writer/lease token,
  and keep the daemon/control socket alive;
- clean or resume partial generation payload idempotently without deleting a
  prior ready route; and
- checkpoint opportunistically without blocking the only writer or treating a
  deferred passive checkpoint as build success.

No repository-scale benchmark may run against the developer's shared daemon or
store. Cold/warm acceptance uses an isolated socket, state/config/XDG roots,
store, bounded repository fixture, explicit disk/RSS/time ceilings, and exact
process cleanup.

#### Required validation and benchmarks

The durable-cache change is not complete until all of the following pass in
normal, race-enabled, crash/reopen, and isolated process shapes as applicable:

| Contract | Required proof |
| --- | --- |
| schema v21 | v20 fixture upgrades catalog-only, creates pin/queue/index/trigger definitions identical to a fresh store, uncapped-backfills every eligible checkout-owned ready/superseded commit plus every routed commit, then Seed enforces bounds before ordinary retirement; it writes `user_version=21` last and retries every injected rollback point with zero payload/index builds |
| restart cache | build A/B, route A, reopen store/coordinator, switch B→A; generation IDs are unchanged and physical build/reparse counters remain zero |
| stopped branch change | stop on A, change Git HEAD to cached B, reopen; B is exact, A remains pinned, checkout HEAD equals route, and warmup reaches ready |
| shared holders | two checkouts plus a ref share one generation; removing each holder preserves payload until the final route/ref/lower/lease/pin drains |
| route CAS | successful A→B restamps/pins/consumes lease once; stale root/incarnation/graph/base/route changes no pin/timestamp/route and consumes no lease |
| retirement race | a pin created after candidate discovery defeats the final SQL delete predicate; generation FK restriction remains intact |
| publication window | runtime cleanup between commit/dirty build return and route/stack publication cannot collect either generation, including an unregistered promotion/base-refresh coordinator; the next Seed reclaims a generation left by a crash in that window |
| retirement handoff | explicit pin deletion plus checkout/graph cascade enqueue exactly once; refusal under route/ref/lower/lease ownership survives reopen; re-pin clears the queue row and both process-local debt maps; successful deletion cascades the row; more than 512 queued candidates drain across pages; concurrent prune versus route bind preserves the winning pin/route |
| capability withdrawal | pinned A loses `source.snapshot`; B→A never reuses withdrawn A and a compatible replacement later caches normally |
| retention | exact seven-day, 32→33, and 5-GiB boundaries; unique shared-byte accounting; per-graph isolation; only current routes excluded from count/bytes; base/ref/lower/lease deletion protection without an accounting exemption; deterministic eviction |
| cleanup modes | promotion, demotion, rehome, inaccessible purge, checkout/graph/family forget leave exactly the scoped pin and graph sets; a lost-response promotion/demotion replay preserves newer target-graph cache pins while deleting foreign-graph pins |
| idle stability | adopted local/ref/sibling routes poll with zero DB writes, lease churn, route epochs, physical builds, or monotonic WAL/RSS growth |
| cold failure | one full corpus build per configured Git checkout; `SQLITE_FULL`/cancel leaves daemon alive, old ready routes intact, and partial generation recoverable |

Benchmarks are named or equivalently scoped as follows and report `benchstat`
over at least ten repetitions for microbenchmarks:

- `BenchmarkCheckoutCommitPinBind`: successful transition, stale CAS, and
  shared-generation holder cases, including writes/op and WAL bytes;
- `BenchmarkCheckoutCommitCacheWarmRestart`: reopen plus cached hit, separating
  store hydration, claim, route publication, and dirty rebuild;
- `BenchmarkCoordinatorStableAdoptedCommitDirtyReconcile`: local, ref-owned, and
  sibling-owned commit routes, requiring zero writes/leases/builds per op;
- `BenchmarkCheckoutCommitRetentionSweep`: 32, 256, and 4,096 candidate/holder
  fixtures with shared holders and graph/search byte limits;
- `BenchmarkCheckoutCommitCacheRetirementCandidates4096`: read-only queue
  discovery plus paginated filtering, separately from writer-gated deletion;
- runtime and startup retirement scans over 10,000 READY generations, proving
  that runtime does no generic orphan scan while Seed recovery remains bounded;
- v20→v21 migration at 10,000 and 100,000 eligible generations, reporting
  transaction latency and final database/WAL bytes;
- `BenchmarkReadyGenerationCacheKey`: every compatibility field and
  owner/alias independence; and
- the existing repository-scale isolated cold/warm gate, with physical build
  counts, parsed files, phase timings, peak RSS, database/WAL/temp bytes, and
  daemon-survival outcome.

The externally observed cache-hit gate remains below 100 ms p95 with zero
reparsed files and zero physical immutable-commit builds. Warm restart performs
no full corpus build. Cold store initialization performs exactly one full build
per configured Git checkout and never an N+1 duplicate. These measurements,
plus the final repository-wide tests, race gates, lint, vet, diff check, and
isolated end-to-end A→B→A/restart/failure replay, are required before the
acceptance ledger may be marked complete.

Development measurements for the schema-v21 repair on Apple M1 Pro are:

| Mechanism | Observed range | Contract proved |
| --- | ---: | --- |
| Commit-cache bind (two pin upserts) | 0.401–0.833 ms/op, about 8.4–8.6 KiB/op, 264–266 allocs/op | Route publication pays bounded durable-holder work. |
| Retention prune, 96→32 | median 5.07 ms/op; 4.29–70.82 ms full range including scheduler/cold outliers | Sixty-four evictions enqueue durable retirement work in one bounded sweep. |
| Target-graph transition replay with 32 cached pins | 0.306–0.632 ms/op; 32 target pins preserved and one foreign pin removed per op | Promotion/demotion replay does not erase later same-graph branch history. |
| 4,096 retirement candidates | 6.328–6.491 ms/op, about 188,208 B/op, 7,960 allocs/op | Queue discovery is read-only and paginates independently of physical cleanup. |
| Runtime retirement, 10,000 READY rows | 21.74–27.32 ms/op, 12,400 B/op, 274 allocs/op, four queries | Runtime does not enumerate generic READY orphans. |
| Seed recovery, 10,000 checkout READY rows | 67.76–70.80 ms/op, about 30.27 MiB/op, about 252,773 allocs/op | Conservative orphan inference is bounded and startup-only. |
| v20→v21, 10,000 eligible rows | median 101.8 ms (79.21–168.38 ms); DB 4,227,072 B; WAL 1,994,112 B | Catalog-only migration; no payload build. |
| v20→v21, 100,000 eligible rows | median 830.8 ms (767.7–925.1 ms); DB 37,163,008 B; WAL 19,228,072 B | Approximately linear catalog scaling at the required large-fixture size. |
| Read-only ready-generation match | 38.05–41.27 µs/op versus 356–371 µs/op for the former claim path | Settled validation avoids writer/lease/timestamp churn. |
| Stable adopted reconciliation | 18.13–22.97 ms/op | Zero physical builds and zero route-epoch advances per operation. |
| Pinned coordinator-debt transfer | 13.05–14.58 µs/op, 808 B/op, 19 allocs/op | One stale local debt entry is dropped with zero writer transactions; the durable deletion trigger remains the future retirement authority. |

These are ten-repetition development samples. They satisfy the microbenchmark
sample-count gate but do not replace the isolated process replay.

#### Final isolated acceptance (2026-09-01)

The final process replay passed in a synthetic Git family under a private
foreground daemon. The branch binary, configuration, SQLite store, socket,
PID/log/snapshot paths, and every XDG root lived in one temporary sandbox. Git
system/global configuration was disabled, `HOME` and `CODEX_HOME` were not
overridden, the supervisor-owned daemon was never contacted, and shutdown
signalled only the exact recorded foreground PID. The successful sandbox was
moved to trash only after both private processes exited.

- Explicit tracking completed in 1.92 seconds and created dedicated graph
  `graph-731412e8fd439360891a9b6bf50c2cbd` with base generation 1 and
  `cli_track` intent. Its linked worktree was discovered automatically as an
  automatic checkout on the same graph, initially routing commit generation 2
  and dirty generation 4.
- Exact worktree searches returned the overlay copy of a duplicate symbol,
  exposed an untracked overlay-only file, and fell through to unchanged base
  source. Freshness was exact and the requested/actual view remained the
  worktree checkout.
- The first A→B→A sequence routed commit generations `2 → 6 → 2`. B's first
  immutable layer was physically built in 631.894 ms; returning to A reused
  generation 2. Cleaning dirty content kept generation 2 and advanced only the
  dirty layer.
- A same-store foreground restart was queryable in 450.783 ms and completed
  warmup in 865.768 ms with `repos_changed=0` and `files_reindexed=0`. A stayed
  on generation 2; switching to B reused pre-restart generation 6 and built
  only a clean dirty layer.
- Authoritative linked-worktree removal immediately withdrew its coordinator
  and route and exposed a labeled read-only primary fallback. After the
  recorded 30-second grace, only the primary remained. Closed-store checks
  found zero removed-checkout, route, or cache-pin rows; worktree generations
  4–9 were physically gone while primary generations 1–3 remained.
- Both private daemons exited with status 0. Their captured logs contained no
  warning, error, panic, or fatal record.

The committed tree then passed `go test ./... -count=1 -timeout=45m` with no
failure, panic, or timeout (slowest package `internal/indexer`, 782.972 s;
approximately 13m30s total wall), `golangci-lint run --timeout=5m` with zero
issues, `go vet ./...`, and `git diff --check`. Focused publication, retirement,
route, replay, and migration races plus all four per-fix benchmarks also passed.
This closes the durable checkout-cache and worktree-overlay acceptance ledger;
the separate fault-injection gate for an actually exhausted filesystem remains
part of the cold-failure hardening contract above.
