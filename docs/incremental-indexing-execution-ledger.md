# Incremental indexing execution ledger

Purpose: the single checked-in, append-only record of execution state for the incremental indexing
and worktree write-amplification work on branch `fix/incremental-index-write-amplification`. It
tracks work items W1–W9, the ten acceptance gates, and every validation result with its exact
source identity and command. The [design and implementation
history](incremental-indexing-write-amplification.md) records invariants and chronological
component narrative; this ledger records *what is actually done, proven, or blocked right now*.

## State vocabulary

These eight states are used verbatim and exclusively. No other status word is permitted anywhere
in this ledger.

| State | Meaning |
| --- | --- |
| `proposed` | Described and scoped; no source written for it yet. |
| `implemented` | Source exists on disk (committed or dirty). **Never implies production activation.** |
| `compiled` | The owning package builds from actual source after the last relevant edit. |
| `tested` | Named regressions ran and passed against that compiled source, with counts recorded. |
| `wired` | A production entrypoint actually reaches the code on a default path (no private overlay, no test-only caller). |
| `E2E validated` | Proven through an isolated public-path end-to-end run, not a unit fixture. |
| `complete` | `wired` + `E2E validated` + its acceptance gate closed with durable recorded evidence. |
| `blocked with evidence` | Cannot progress; the blocking failure is named with file, test, and observed output. |

`implemented` never implies production activation. A component that exists and passes tests but is
not reachable from a default startup, watcher, or Git callback path is `implemented`, never
`wired` and never `complete`.

## Resume state (2026-09-10, corrected)

| Item | Verified value |
| --- | --- |
| Implementation worktree | `/Users/zzet/code/my/gortex/worktrees/fix-incremental-write-amplification` |
| Branch | `fix/incremental-index-write-amplification` |
| HEAD | `16efbf700ea28d3f588bc13152ea544ff3aeeab5` (was `297f5a44` when the W1 lanes ran; the W1 dirty inventory has since been committed as atomic topic commits) |
| Rebased onto main | `56a1c29d` (v0.64.3) |
| Branch on origin | Not yet pushed |
| Dirty-tree safety snapshot | `refs/backup/incremental-dirty-resume-20260910` = `b6ddfc96be1304f01c6ec88d12bfdc5972487e2a`, tree `001a6214`, 69 entries = 68 unfinished files + the handoff document, `+7194/−433` lines vs HEAD |
| Dirty-manifest sha256 (harness-computed, W1 suite) | `b341db31419ea47e9428a4639cde16ed9b4b3f6bd5db761dfe74d4dfdef4d73b` — identical on every compile and every run of the W1 exit suite; no source drift during the suite |
| Dirty-manifest sha256 (harness-computed, wave 1 exit suite) | `b9e80af941b8f56cee5158f0bda45efae458b07085d746cb2144ccde17889017` — identical on all 9 compiles and all 14 runs of the wave 1 exit suite; no source drift during the suite |
| Recovery branch (do NOT reapply) | `backup/incremental-before-main-20260910-1310` = `a2ccbe07` |
| Recovery stash (do NOT reapply) | `6e9db642` |
| Live daemon | `v0.64.1-36-ga2ccbe07-dirty`, pid 18828 — a **pre-rebase** binary. Must not be restarted, re-tracked, or reconfigured. |
| Worktree checkout_id | `01a081ae-649e-7e31-a0c9-8233489d60e9` |
| Graph identity | `graph-b54bb040c8c8d551ccafd6e20748ec0e` |
| Route | active/ready; commit generation 547, dirty generation 550 |
| Toolchain | go1.27.0 darwin/arm64, 10 CPUs, 16 GiB RAM; ~17 GiB free disk after the 2026-09-10 cleanup |
| `go.work` | Exists only in the main checkout, which is **not** a parent of this worktree. `GOWORK=off` is still enforced for every build. |

### Correction 1 — the `/private/tmp` recovery artifacts were never purged

The previous resume row recorded them as "Absent (purged)". That was **wrong**: the reading came
from an `rtk`-wrapped `ls`, which prints `(empty)` for non-empty directories. Every small artifact
has been copied to `/Users/zzet/code/my/gortex/recovery-incremental-20260910/` — the
`validate.cjs` harness, the `result.json` / `output.log` result directories, `REBASE.md`,
`rebase.json`, `historical-proposal-tool-results.json`, the `CONTINUATION*.md` / `REPAIR.md`
notes, plus the rebuilt `harness/` tree and the lane `reports/`. The ~300 MB of compiled test
binaries under those `/private/tmp/gortex-*` directories were **deleted on 2026-09-10** to recover
disk; they are reproducible from source and their `sha256` values survive inside the copied
`result.json` files, so every historical count stays attributable to an exact binary identity.
Pre-rebase counts remain **historical**, but they are no longer unrecoverable.

### Correction 2 — the validation harness exists and was rebuilt from scratch

`validate.cjs` (node) was recovered as a file but not as a runnable harness; a bash replacement
was written from scratch. See "Validation harness". Every W1 count here cites it.

### Correction 3 — W1 sub-item renumbering

W1 sub-items are renumbered to the execution-plan-v2 §0 scheme, which is now the single numbering
used by the plan, the lane reports and this ledger. Mapping from the previous ledger revision:

| previous id | current id | lane report id |
| --- | --- | --- |
| W1.1 dedicated identity fixture | **W1.2** | STORE S1 |
| W1.2 publication/retirement fixtures | **W1.2** (same root cause) | STORE S2 |
| W1.3 generation capability isolation | **W1.1** | STORE S3 |
| W1.4 lifecycle Close test | **W1.3** | INDEXER I1 |
| W1.5 cleanup journal retry | **W1.4** | INDEXER I2 |
| W1.6 shared inspection budget | **W1.5** (first half) | GRAPH G1 |
| W1.7 overlay bounded-reader refusal | **W1.5** (second half) | GRAPH G2 |
| — (new) | **W1.6** | W1 exit suite |
| — (new) | **W1.7** | verifier follow-up: pin the two fail-closed guards |
| — (new) | **W1.8** | `TestSparseGenerationClaimsPathlessIdentities` |

## W1 — Stabilize current unfinished primitives

Deliverable: stabilize current unfinished primitives. Exit condition: diagnose the failures;
compile and validate actual source; review identity masks, bounded reads and raw-owner components.

Shared fields for all W1 sub-items unless overridden:

- Source commit `297f5a44`; dirty manifest sha256
  `b341db31419ea47e9428a4639cde16ed9b4b3f6bd5db761dfe74d4dfdef4d73b`.
- Harness: `.../scratchpad/harness/validate.sh` (see "Validation harness").
- Commit/PR references: none; nothing committed, branch not pushed.
- **No production `.go` file was changed by any W1 lane.** Every W1 fix is a test change;
  production mutations were temporary, made only inside isolated exports or byte-restored
  (md5-verified) in place.

### W1.1 — Generation capability isolation probes (STORE S3)

- State: `tested`.
- Scope/files: `internal/graph/store_sqlite/store_generation_read_test.go` (+121, purely
  additive).
- Root cause: three `var _ graph.X = (*Store)(nil)` assertions had no checklist entry, so
  `TestGenerationCapabilityChecklistIsComplete` failed three times —
  `graph.BoundedIncomingSourceCandidateReader` (`incoming_source_candidates.go:11`),
  `graph.ScopedIncomingSourceReader` and `graph.IncomingSourceNodeChecker`
  (`incoming_sources_scoped.go:211-212`).
- Contract verified in production source: all three scoped readers bind `view_gen = ?` to
  `s.viewGen` — `readIncomingSourceCandidatesSQL:16-20`, `scopedIncomingSourcePageSQL:15-21`,
  `incomingSourceNodeExistsSQL:22`. `FindIncomingSourcesScoped` charges every raw physical row to
  the caller's `*graph.IncomingSourceBudget` before filtering (`:150-153`).
- Change: three helpers + three probes in `generationReadProbes()` + three checklist entries using
  `probe:`, never `skip:` and never `identicalOK`.
- Result: `TestGenerationCapabilityChecklistIsComplete` PASS; `TestGenerationReadIsolation` PASS
  including the three new subtests.
- Acceptance gate(s): G1, G8.
- Verifier verdict: **PASS**. MUT-E1/E2/E3 each bind exactly one predicate one-to-one; MUT-F
  proves the budget assertion has teeth; MUT-H proves the checklist is a live gate.

### W1.2 — Dedicated identity + publication/retirement fixtures (STORE S1, S2)

- State: `tested`.
- Scope/files: `internal/graph/store_sqlite/generation_lifecycle_test.go`, lines 719, 762 (this
  lane) and 956 (pre-applied by the handoff, never compiled until now).
- Root cause: fixtures seeded `dedicated_graphs.state` with the raw literal `"ready"`.
  `dedicatedBaseOwnerTx` (`catalog_dedicated_base.go:187-191`) requires `graphState ==
  DedicatedGraphReady` (`= "graph_ready"`, `catalog_types.go:17`). The column is free-form TEXT
  with no Go validator, so `"ready"` writes fine and then fails every readiness comparison.
- Contract verified in production source: the same constant is required by reconciliation
  (`internal/reconcile/intent.go:21`) and indexer admission
  (`internal/indexer/repository_admission.go:87`); the only other legal value is
  `DedicatedGraphClosing`. Corroboration: `repository_publisher_registration_test.go:13` lists
  bare `"ready"` among the **inadmissible** states, and
  `TestDedicatedPublicationAuthorizationRejectsUnsupportedOwners` has a subtest literally named
  `generation_ready_is_not_graph_ready`.
- Per-site decision (not a sweep): `:719` and `:956` changed (load-bearing); `:762` changed as a
  fixture-fidelity improvement only; `:56` and `:1187` left as `"ready"` because no path they
  reach reads the column.
- Result: `TestDedicatedIdentityAdmissionGuardedIgnoredTargetsRemainNoop` PASS;
  `TestCatalogPublicationCurrentAssociationRetirementRoots` PASS (3/3 subtests);
  `TestCatalogPublicationReplacementDoesNotPinHistoricalBase` PASS.
- Acceptance gate(s): G6, G7.
- Verifier verdict: **PASS**. MUT-A (revert `:719`) RED with the exact baseline text; MUT-B
  (revert `:762`) GREEN, confirming the lane's own disclosure; **MUT-C (revert `:956`) RED (pass=0
  fail=5)** — closing the one claim the lane had left unproven. MUT-D shows the production guard
  itself is pinned elsewhere.
- Verifier residual (minor): `:762` is an unpinned test-only edit — no test can protect it.

### W1.3 — CheckoutLifecycle.Close retry-admission contract (INDEXER I1)

- State: `tested`.
- Scope/files: `internal/indexer/checkout_lifecycle_test.go` (body now 353-410).
- Root cause: the test asserted `Close must restore retry admission for lifecycle reuse` (`:361`).
  **Production is right and the test was stale.** `a8eb927e` introduced a `defer { retryClosing =
  false }`; HEAD `297f5a44` deliberately removed it and added the doc comment at
  `checkout_lifecycle.go:2161-2162`: *"Close permanently closes lifecycle admission …"*. The test
  was not updated with it.
- Contract verified in production source: `Close` (`:2195-2204`) sets `retryClosing` and never
  resets; `scheduleFamilyRetryAt:1385` and `runFamilyRetry:1411` both refuse while closing; every
  sibling gate (`repositoryAdmissionsClosed`, `coordinatorClosing`, `transitionClosed`,
  `observationClosed`) is likewise permanent. The only production caller is the shutdown hook
  `internal/serverstack/shared_server.go:655`; there is no lifecycle-reuse path.
- Change: the expectation was inverted and strictly widened — (a) admission after Close rejected
  on three entrypoints with the typed `graphview.ErrRepositoryAdmissionsStopped`, (b) `Close`
  idempotent within 2 s, (c) a delayed timer callback cannot resurrect drained work. The in-flight
  drain half (`:288-351`) is untouched.
- Acceptance gate(s): G7.
- Verifier verdict: **PASS**. Lane mutants M1/M2/M3 RED; verifier-added M4 (Close leaves timers
  behind) and M4b (remove `retryWG.Wait()`) also RED, proving the pre-existing drain half survived
  the rewrite.

### W1.4 — Cleanup release ordering / pending-continuation bookkeeping (INDEXER I2)

- State: `tested`.
- Scope/files: `internal/indexer/repository_untrack_retry_test.go`.
- Root cause: **two defects, both in the test.** The handoff's predicted `:352` journal-ordering
  failure is stale — that assertion now passes. (1) HEAD `297f5a44` split cleanup into a durable
  half and a process-local half: `purgeRepoForCleanup` calls
  `untrackRepoCheckedRetainingAdmission(…, retainAdmission=true)`
  (`repository_cleanup_lane.go:75-80`), and with that flag `repository_untrack.go:209-211` returns
  *without* deleting `pendingRepositoryUntracks[prefix]`. Release happens only in
  `finalizeRepositoryCleanupLane`. The test stopped at `rec.Resume`, the durable half only. (2)
  The fixture runs the real cleanup runtime, whose 1 s repair timer concurrently replays the same
  journal, so a single `ListCleanupEntries` sample can catch the `deleting` transient.
- Contract settled: the child entry owns `release_graph`; `deleting` is an in-flight transient and
  `failed` the parked state (`internal/reconcile/saga.go:332-344`); destructive phases commit at
  most once (purge/vector stay 1/1); release is two-phase and ordered, durable then process-local,
  and both production drivers pair the halves (`repository_cleanup.go:339-341`,
  `checkout_lifecycle.go:1079`).
- Change: bounded wait for the parked `failed`/`release_graph` entry; retry-tolerant `Resume` loop
  waiting on the outcome; explicit `finalizeRepositoryCleanups` call asserting `(false, nil)`; the
  no-repeated-payload-purge guarantee asserted twice.
- Acceptance gate(s): G7, G9.
- Verifier verdict: **PASS**. Lane mutants M5/M6 RED; verifier-added M8 (drop the
  `retainAdmission` early return) and M10 (never park in `failed`) RED. M9 shows the `:422-432`
  ordering gate is *not* pinned by this test but is covered by
  `TestRepositoryLateCleanupFinishCannotSignalRetrackedState`.
- Verifier residuals (minor): V-1 the `rec.Resume` error assertion is genuinely weaker (5/5
  instrumented runs returned `nil`, so the strict form would also have passed); V-3 two 10 s / 10
  ms-poll waits plus a 2 s `Close` bound are wall-clock dependent — there is no injectable clock
  seam on `runRepositoryCleanup`'s timer without a production change.

### W1.5 — Shared inspection budget + overlay bounded-reader refusal (GRAPH G1, G2)

- State: `tested`.
- Scope/files: `internal/graph/bounded_incoming_sources_budget_test.go` (new, untracked, 250 lines
  — the report's "238" is a miscount). **No production change.**
- Constants re-verified before writing: `MaxIncomingSourceCandidateRows =
  MaxBoundedAdjacencyInspectedEdges = 16384` (`bounded_incoming_source_candidates.go:19`,
  `bounded_adjacency.go:22`), so the 8192 + 8192 = 16384 / 16385-refuses contract is real. The
  test derives `halfIncomingSourceCandidateBudget` from the constant rather than hard-coding it.
- G1 contract: `(*OverlaidView).FindIncomingSourcesScoped` hands the **same**
  `*IncomingSourceBudget` pointer to the lower read (`:180`) and the upper read (`:196`); a nil
  budget becomes one fresh budget per public query (`:149-151`). Refusals return the zero
  projection — no partial answer.
- G2 contract: with `v.layer == nil` the view delegates once verbatim
  (`bounded_incoming_sources.go:203-215`); with any non-nil layer, a base lacking
  `ScopedIncomingSourceReader` refuses with `ErrBoundedLocalizationUnavailable` **before**
  touching the lower reader (`:176-179`), because a lower answer already capped at `limit` cannot
  be post-filtered without turning "more than limit exist" into a short exact list.
- Result: both regressions passed on their first clean run; **no production bug surfaced**.
  `internal/graph` 522/0/0 in both normal and race.
- Acceptance gate(s): G1, G8.
- Verifier verdict: **PASS**, with two majors recorded as W1.7 below. M1–M7 confirm every claimed
  oracle (including M6, which proves the fixture really hides the 8192 lower rows by edge
  provenance, and M7, which pins that refusal precedes any lower read).
- Verifier residuals: MINOR-3 the shared budget bounds *kind-matching candidate rows*, not raw
  slot inspection (that is a separate per-reader counter), so the report's "physical rows"
  phrasing overstates it; MINOR-4 the typed refusal is emitted identically by the per-reader cap,
  so the refusal subtest alone cannot attribute itself to the shared budget (the `Remaining()==0`
  and empty-base oracles carry it); MINOR-5 the cap's numeric value is unpinned — M8 shrank it
  16384 → 1024 and both new tests stayed green, so a *raise* would not be caught.

### W1.6 — W1 exit suite

- State: `tested`, with one named failure (W1.8).
- Source identity: HEAD `297f5a44`, dirty-manifest sha256
  `b341db31419ea47e9428a4639cde16ed9b4b3f6bd5db761dfe74d4dfdef4d73b`, identical on all 9 compiles
  and all 13 runs. `fallback=false` on every compile.
- Compiles: 9/9 OK — graph/store/graphview/indexer/reconcile normal, plus
  graph/store/graphview/indexer race.

| # | package | flavor | selection | pass/fail/skip | result dir |
| --- | --- | --- | --- | --- | --- |
| 1 | `internal/graph` | normal | `.` | 522 / 0 / 0 | `results/graph-normal-_-2` |
| 2 | `internal/graphview` | normal | `.` | 435 / 0 / 0 | `results/graphview-normal-_-1` |
| 3 | `internal/reconcile` | normal | `.` | 92 / 0 / 0 | `results/reconcile-normal-_-1` |
| 4 | `internal/graph/store_sqlite` | normal | `.` | 1757 / 0 / **2** | `results/store-normal-_-2` |
| 5 | `internal/indexer` | normal | chunk1 | 527 / 0 / 0 | `results/indexer-normal-__TestAdmitWalkEntryReportsOversize_TestAffected-2` |
| 6 | `internal/indexer` | normal | chunk2 | 502 / 0 / **1** | `results/indexer-normal-__TestAffectedBy_CapBoundsFanout_TestAffectedBy_-3` |
| 7 | `internal/indexer` | normal | chunk3 | 482 / 0 / **1** | `results/indexer-normal-__TestAdmitWalkFileKnownType_EscapingSymlink_Tes-3` |
| 8 | `internal/indexer` | normal | chunk4 | 509 / **1** / 0 | `results/indexer-normal-__TestAdmitWalkFileKnownType_ExclusionPrecedesSn-2` |
| 9 | `internal/indexer` | normal | chunk5 | 532 / 0 / 0 | `results/indexer-normal-__TestAdmitWalkEntry_SymlinkEscapeRefusedBeforeS-2` |
| 10 | `internal/graphview` | race | `.` | 435 / 0 / 0 | `results/graphview-race-_-1` |
| 11 | `internal/graph/store_sqlite` | race | `TestDedicatedIdentity\|TestCatalogPublication\|TestGenerationRead\|TestGenerationCapability\|TestPublication\|TestRetirement` | 164 / 0 / 0 | `results/store-race-TestDedicatedIdentity_TestCatalogPublication_Tes-2` |
| 12 | `internal/indexer` | race | `Rehome\|CheckoutMutation\|SourceMutation\|StartupRepository\|SeedRestores\|CheckoutLifecycleClose\|RepositoryCleanupSaga` | 23 / 0 / 0 | `results/indexer-race-Rehome_CheckoutMutation_SourceMutation_StartupRe-1` |
| 13 | `internal/graph` | race | `Bounded\|Scoped\|Overlay\|Localization` | 124 / 0 / 0 | `results/graph-race-Bounded_Scoped_Overlay_Localization-1` |

- Indexer totals across the five chunks: **2552 pass / 1 fail / 2 skip**. Grand total runs 1–13:
  **5344 pass / 1 fail / 4 skip**. `DATA RACE` occurrences: **0** across all four race logs.
- Chunking is mandatory: `validate.sh` hardcodes `-test.timeout 8m` (lines 221, 253) and
  `internal/indexer` exceeds it in a single process. The five duration-bin-packed regex files
  `scratchpad/chunk1.pat … chunk5.pat` were reused; their union was re-verified this run to be
  exactly the 1728 unique `func Test…` names in the package. No test ran twice. **Limitation:**
  cross-test interference visible only in a single 1728-test process is not observed by this
  method; the one 8-minute single-process attempt reached 256 top-level tests with the same single
  failure and no others.
- Named skips (4, none introduced by any W1 lane):
  - `TestBundlePackageKeyNeverUsesOSSeparator` — `bundle_cache_test.go:111`, guarded by
    `filepath.Separator == '/'`; vacuous on darwin by construction.
  - `TestMetaBlobCensus` — `meta_census_probe_test.go:17`, opt-in diagnostic needing
    `GORTEX_BENCH_STORE`.
  - `TestBackendBench` — `zzbench_backends_test.go:39`, environment-gated bench harness,
    pre-existing. **Outside the two skips the exit criterion allowed — recorded, not waived.**
  - `TestMeasureEditLatency` — `editlatency_measure_test.go:26`, environment-gated measurement
    harness, pre-existing. **Same status.**
- `go vet`: graph, store, graphview, indexer, reconcile all VET OK. `go vet ./...` over the whole
  module (which also type-checks every `_test.go`) → exit 0, no output
  (`scratchpad/w1-vet-all.log`). **This is the compile-break evidence: no compile error anywhere
  in the tree.**
- `go build ./...` → **inconclusive, to be re-run.** Exit 1 with **zero compile errors**: every
  reported error is host disk exhaustion at the *link* stage (`ld: write() failed, errno=28`,
  `strip`/`dsymutil` `No space left on device`) with `df -h /` reporting 2.0–2.4 GiB free
  throughout. A retry with `-ldflags=-w` narrowed it to a single package (`bench/daemon-latency`),
  still `errno=28`. All affected packages are `main` binaries and are type-checked clean by `go
  vet ./...`. Logs: `scratchpad/w1-gobuild.log`, `w1-gobuild-nodwarf.log`,
  `w1-gobuild-retry2.log`. Per the operating rules no cache was cleared to make room.
- `all_green = false`, solely because of W1.8 (and strictly because two indexer skips fall outside
  the two the exit criterion named).

### W1.7 — Pin the two fail-closed guards (verifier follow-up)

- State: `wired` (test-only item; the guards it pins are pre-existing production code reached from
  the public bounded API, so no separate activation step exists).
- Agent: wave 1, Lane S.
- Scope/files: `internal/graph/bounded_incoming_sources_failclosed_test.go` (new, 235 lines,
  untracked) carrying `TestScopedIncomingSourcesRefuseLayerWithoutScopedReader`,
  `TestScopedIncomingSourcesRefuseLayerWithoutNodeChecker` and
  `TestIncomingSourceCandidateRowCapPinned`.
  **No production change** — both guards were re-read and left byte-identical:
  `internal/graph/bounded_incoming_sources_scoped.go:185-189` (layer-side
  `ScopedIncomingSourceReader` refusal; the plan's cited `:186-189` had drifted by one line) and
  `:84-87` (`identityVisible` refusal when a covering layer is not an `IncomingSourceNodeChecker`).
- Invariant: a composition that cannot evidence an answer refuses with
  `ErrBoundedLocalizationUnavailable` before reading anything; it never degrades to a silent empty
  projection (fail-closed) and never treats an unverifiable identity as visible (fail-open).
- Baseline/reproduction: verifier mutants **M9** and **M10** each replaced one guard with a silent
  fallback and the **entire 522-test `internal/graph` suite stayed green**; M10's fallback is
  fail-*open*, silently resurrecting covered identities.
- Change: two refusal regressions built on `NewOverlaidViewWithLayer` (`overlay.go:342`) with bespoke
  fakes that implement exactly the surface the composition is entitled to, with the capability
  under test withheld — plus `TestIncomingSourceCandidateRowCapPinned`, which closes MINOR-5 by
  pinning the cap constant against a *raise* as well as a shrink. Each test asserts through both
  the scoped door and `FindIncomingSourcesBounded` (`bounded_incoming_sources.go:231`).
- Acceptance gate(s): G1, G8.
- Harness evidence (wave 1 exit suite): `internal/graph` normal `.` → **530 / 0 / 0**
  (`results/graph-normal-_-4`); race `Bounded|Scoped|Overlay|Localization|FailClosed` → **131 / 0 /
  0**, 0 `DATA RACE` (`results/graph-race-Bounded_Scoped_Overlay_Localization_FailClosed-1`).
- Harness evidence (wave W1x exit suite): carried unchanged. `internal/graph` normal `.` →
  **530 / 0 / 0** (`results/graph-normal-_-5`); race
  `Bounded|Scoped|Overlay|Localization|FailClosed` → **131 / 0 / 0**, 0 `DATA RACE`
  (`results/graph-race-Bounded_Scoped_Overlay_Localization_FailClosed-2`). Both counts reproduce
  the wave 1 figures exactly.
- Commit: `141dc997f2c1f5fd5b495895da8d7dbdc3eb42ad` — *graph: pin the fail-closed guards in the
  bounded incoming-source readers* (1 file, new, test-only).
- Limitations: the tests are fixture-driven compositions, not an end-to-end read; they prove the
  refusal shape, not that any production caller supplies such a layer today.
- Deviations: the item also pins the cap constant (previously scoped out as MINOR-5), because the
  same fixture file could carry it at ~10 lines.
- Verifier verdict: **PASS** (no blockers). M9, M10, M8-raise and M8-shrink each RED and each
  killed by exactly one test — no blanket assertion. Full `internal/graph` re-run in the verifier's
  export reproduced 530 / 0 / 0 independently.

### W1.8 — `TestSparseGenerationClaimsPathlessIdentities`

- State: `blocked with evidence` — being settled concurrently with this ledger update.
- Scope/files: `internal/indexer/builder_acceptance_test.go:826-840`; the composition change lives
  in `internal/graph/overlay_detached_nodes.go`,
  `internal/graph/store_sqlite/generation_node_identity_masks.go`,
  `internal/graph/store_sqlite/node_identity_localization_projection.go`,
  `internal/graphview/generation_layer_identity.go` (all new/untracked on this branch).
- Baseline: `builder_acceptance_test.go:835` *"GetRepoNodes now returns 5 nodes against the flat
  index's 5"* and `:840` *"NodeCount is 5 against the flat index's 5"*.
- Attribution: **pre-existing and out of every W1 lane's scope.** Independently reproduced twice
  with both INDEXER-lane test files reverted to their HEAD content
  (`results/indexer-normal-TestSparseGenerationClaimsPathlessIdentities-1`, verifier
  `vilogs/headrevert.log`).
- Intended behavior: this is a **characterization** test whose own comment says a fix to the
  composition *should* break it. Deciding it requires the GRAPH/STORE owners to confirm the
  pathless-identity union is intended for `GetRepoNodes` **and** `NodeCount` alike. Flipping the
  expectation from inside a test file was correctly refused by the INDEXER lane.
- Acceptance gate(s): G1.
- Result: **TBD** — settlement is running concurrently with this ledger update.
- Next action: record the owners' determination and the resulting counts here.
- Observation (wave W1x exit suite): the test **passes** in the whole-package `internal/indexer`
  chunk `^Test[S-T]` (`results/indexer-normal-_Test_S_T_-1`, 330 / 0 / 0). The state above is left
  as the owners recorded it; this ledger records only that the named baseline no longer reproduces
  in the current tree, not a determination.

### W1.9 — Triage and settle the `internal/mcp` failures present at the branch base

- State: `tested`. Test-only item on the request surface; the production gate it pins
  (`internal/indexer/ref_view_service.go:44-48`) is pre-existing branch code already reached from
  the default request path, so no separate activation step exists. The verifier's wiring check
  confirms the new regression drives that path through `wrapToolHandler` rather than calling the
  primitive directly, so the admission pin is `wired` in the only sense available to it.
- Agent: wave W1x, Lane R.
- Scope/files: `internal/mcp/view_ref_test.go` (+34/−9 — `refStack` gains a `lifecycle` field, the
  old `newRefStack` body becomes `newUnadmittedRefStack`, and a new `newRefStack` wraps it and
  registers the owner), `internal/mcp/view_ref_admission_test.go` (new, 104 lines, 2 tests).
  **No production file changed**; `internal/indexer/ref_view_service.go` was mutated for M2 and
  restored byte-identically (`git diff HEAD` empty).
- Invariant: a ref view is served only for a graph **this process** admitted. Catalog rows are
  durable state a previous process wrote; the repository-read admission is this process's promise
  not to purge what it is serving, and no row can imply it. A fixture that writes the rows by hand
  must also perform the lifecycle action those rows imply, or it is testing a state production
  never reaches.
- Baseline/attribution: `internal/mcp` at `16efbf70` carried **18** top-level failures
  (`results/internal_mcp-normal-_-2`, 3382 / 18 / 3). Sixteen were **branch regressions** — they
  pass on a `git archive 56a1c29d` main export and fail on the branch — all with one cause: the
  branch added `AcquireRepositoryRead` to the ref-view entry point, `refViewError`
  (`view_ref.go:240-257`) has no arm for `graphview.ErrRepositoryOwnerUnknown`, and the `default:`
  arm re-spells it as `checkout_inaccessible`. The remaining two are **upstream**, fail
  byte-identically on the main export, and are harness-environment artifacts (see limitations).
- Change: the fixture registers the repository owner the ordinary way, and two regressions hold the
  gate in place **disjointly** from the fixture repair. `TestRefViewRefusesAGraphThisProcessNeverAdmitted`
  refuses an unadmitted graph at the lifecycle boundary, then through `read_file` under a `git_ref`
  selector (asserting the refusal names the admission failure and leaks neither the committed bytes
  nor the working copy's), then registers the owner and re-issues the identical request, which
  serves. `TestRefViewAdmissionSurvivesAnIdempotentReRegistration` pins that re-registering an
  already-open owner neither errors nor withdraws the admission a served view depends on — the
  property that makes the fixture's registration the same action the boot seed performs.
- Acceptance gate(s): G1, G7.
- Harness evidence (wave W1x exit suite): `./internal/mcp` normal `.` → **5926 / 2 / 8**
  (`results/internal_mcp-normal-_-6`, 261.6 s); race
  `ViewBase|BaseSelector|RequestView|Capabilit|RefView|SearchText` → **238 / 0 / 0**, 0 `DATA RACE`
  (`results/internal_mcp-race-ViewBase_BaseSelector_RequestView_Capabilit_RefV-1`). The wave 1 exit
  suite's race lane over the narrower selection was 169 / **2** / 0; both of those failures are the
  ref-view regressions this item fixes.
- Limitations: the two surviving `internal/mcp` failures —
  `TestReadFilePhysicalEvidenceRejectsSpecialFiles` (`read_file_physical_evidence_nonblocking_test.go:61`)
  and `TestRetrievalSavingsCreditIsPerSession` (`savings_retrieval_test.go:178`) — are **not fixed
  and not attributable to any wave item**: they fail identically on the main export, and both flip
  to PASS purely by shortening `TMPDIR` (the harness's private `HOME` is 113+ chars, and a
  `t.TempDir()`-rooted unix socket path overruns the 104-byte darwin `sun_path` cap). With a short
  `TMPDIR` the entire package is green — 3407 top-level PASS / 0 FAIL / 3 SKIP. Beyond that, the
  item is fixture work: it proves the gate refuses and admits, not that any deployment's seed order
  registers owners before the first ref-view request.
- Deviations: none. No production file was touched, no guard weakened, no skip added — the three
  `t.Skipf` sites in `view_ref_test.go` are pre-existing and outside the diff.
- Verifier verdict: **PASS** — no blockers, 2 minor documentation-level findings. M1 (drop the
  fixture's registration, i.e. revert the item) is RED on exactly the 16 branch regressions at the
  same `file:line`; M2 (delete the production admission gate) is RED on exactly the new admission
  test. The two mutants bind disjointly, which is the point: under M1 the new tests stay green, so
  they are not restating the fixture; under M2 the 16 restored tests stay green, so the fixture
  change is not papering over the gate.
- Commit: `0d8ccf2f3351eaaf89ddfb681379ba9a1c3677b5` — *mcp: admit the repository owner in the
  ref-view fixtures* (2 files, test-only).

## W2 — Freeze complete resolver-visible inputs

- State: `proposed`. Dependencies: W1. Items: W2.1a, W2.1b, W2.1c, W2.2, W2.4 (in MVI); W2.5, W2.6
  out of scope (D10).
- Scope anchors: `internal/indexer/dependency_revision.go` (new), `dedicated_base_advance.go`,
  `builder_dedicated_delta.go`, `checkout_coordinator.go`, `checkout_lifecycle.go`,
  `internal/resolver/version.go` (new), `ref_views.go`.
- Invariant: dependency revisions must describe the **complete** frozen inputs — repository
  roster, ownership, source identity, configuration, producer policy and capabilities. A digest of
  an incomplete roster is not a freshness certificate; an incomplete cohort must fail closed
  (empty ⇒ legacy) rather than digest a partial roster.
- Acceptance gate(s): G6, G3, G1.
- Limitations: schema 23 propagates dependency-revision **metadata only**; it computes no digest
  and certifies no legacy output. W2.1c and W2.4 each invalidate every cached generation once on
  first deploy (D8).
- Next action: W2.1c (bind the revision into `GenerationIdentity`) in wave 2. W2.1a and W2.1b
  landed in wave 1 and are recorded below.

### W2.1a — Dependency-revision cohort digest producer

- State: `tested`. **Not `wired`** — the verifier confirmed by search over its export that
  `ComputeDependencyRevision` has zero non-test callers; W2.1c owns the binding, by design.
- Agent: wave 1, Lane B. Repair round applied after a failed first verification.
- Scope/files: `internal/indexer/dependency_revision.go` (new, 629 lines),
  `internal/indexer/dependency_revision_test.go` (new, 697 lines, 9 tests). Nothing else touched.
- Invariant: a dependency revision describes the **complete** frozen input cohort — repository
  roster, ownership, source identity, configuration, producer policy and capabilities — or it is
  not issued at all. A digest over a partial roster is a false freshness certificate.
- Change: `ComputeDependencyRevision` renders the cohort as `cohort-v1:<sha256 hex>` over a
  canonical length-delimited encoding `<label>:<len>:<value>\x00`
  (`dependency_revision.go:614`), the shape `generationIdentityKey` already uses
  (`checkout_coordinator.go`); every list is sorted and de-duplicated before encoding and no map
  is ranged over, so the digest is deterministic and order-free, and the length prefix makes the
  encoding injective. Any cohort that cannot describe itself completely yields **no** revision
  (`ErrDependencyRevisionIncomplete`, empty string), which `generationIdentityKey` renders
  byte-for-byte as the legacy identity — fail-closed to legacy, never a partial digest.
- Acceptance gate(s): G6, G3, G1.
- Harness evidence (wave 1 exit suite): the 9 tests ran inside `internal/indexer` normal chunk 6
  (`results/indexer-normal-__TestClaimedDedicatedDeltaParentRevisionChangeR-1`, 28 / 0 / 0) and
  inside the race lane (`results/indexer-race-DependencyRevision_DedicatedBase_Dedicated_TextS-1`).
- Harness evidence (wave W1x exit suite): carried unchanged and re-run whole-package. The nine
  tests sit in `internal/indexer` normal chunk `^Test[D-H]` → **526 / 0 / 0**
  (`results/indexer-normal-_Test_D_H_-1`), and in the race lane
  `DependencyRevision|DedicatedBase|Dedicated|TextSearch|SearchText|Rehome|CheckoutMutation` →
  **167 / 0 / 0**, 0 `DATA RACE` (`results/indexer-race-DependencyRevision_DedicatedBase_Dedicated_TextS-2`).
- Commit: `f2fa834693710a19c4027285b0b0450037b21d6d` — *indexer: derive the dependency revision
  from the frozen input cohort* (2 files, both new).
- Limitations: the producer is inert on this branch — every production `DependencyRevision` is
  still empty, so it certifies nothing until W2.1c binds it. `cohort-v1` is a new encoding
  version; a stored revision names the encoding that produced it, but no migration reads it yet.
- Deviations: the repair round replaced a vacuous anti-aliasing assertion with a two-ordering
  sub-check, pinned the empty-roster refusal, and made the fail-closed posture symmetric for
  `ConfigSections` and `Ownership` (previously documented rather than detected).
- Verifier verdict: **PASS** (no blockers), 3 minor / non-attributable findings. The repair
  round's four new oracles are mutation-bound (r2m1–r2m5 RED); the export reproduced 9/9 PASS and
  the gated mutation binary with `VXMUT` unset reproduced the pristine result exactly.

### W2.1b — A revision change roots a new chain, never extends one (D2)

- State: `tested`. **Not `wired`**: the verifier confirmed the two primitives' only non-test
  callers are inside `dedicatedBaseRuntime`, and `dedicatedBaseRuntime` / `dedicatedBasePublisher`
  have **zero non-test constructors** anywhere in `internal/` or `cmd/` — pre-existing at the
  branch base (`git grep -l dedicatedBaseRuntime 16efbf70` returns the same three files), so the
  item is wired only *within* an as-yet-unmounted subsystem. W4.1 owns mounting it.
- Agent: wave 1, Lane B.
- Scope/files: `internal/indexer/dedicated_base_advance.go` (+17/−…, planner),
  `internal/indexer/builder_dedicated_delta.go` (+15, builder guard),
  `internal/indexer/dedicated_base_advance_revision_test.go` (new, 147 lines, 2 tests),
  `internal/indexer/builder_dedicated_delta_test.go` (+51/−0, 1 new test).
  `dedicated_base_advance_test.go` is not in the diff — no existing assertion was deleted or
  relaxed.
- Invariant (D2): the advancement policy tuple must include `DependencyRevision`. A revision-only
  change must root a NEW full base, never extend an existing chain, and a delta claim whose parent
  was frozen under a different revision must be refused with a typed error.
- Change: the planner's policy tuple at `dedicated_base_advance.go:53-56` now carries
  `DependencyRevision`, so both the ancestry-validity loop (`:66-67`) and the target-vs-active
  comparison (`:87-88`) require chain revision-homogeneity; a mismatch falls to `return 0, nil`, a
  full root. The 32-ancestor full-root policy is unchanged. The builder gains
  `errDedicatedDeltaParentRevision` (`builder_dedicated_delta.go:37`), which wraps
  `store_sqlite.ErrDedicatedBaseCandidate` so every existing `errors.Is` handler keeps working,
  and refuses at `:105-106` before the ready/superseded coalesce. The guard is reachable
  production code, not dead defence: `validateDedicatedBaseChainTx`
  (`catalog_dedicated_base.go:341-345`) checks the revision only at `depth == 0` and only under
  `exactTree`, and the proposed-parent arm passes `exactTree=false`.
- Acceptance gate(s): G6, G8.
- Harness evidence (wave 1 exit suite): the item's own three tests pass in
  `results/indexer-normal-__TestClaimedDedicatedDeltaParentRevisionChangeR-1` (28 / 0 / 0). **But
  the same guard fails a pre-existing test outside this item's ownership — see the
  regression below.**
- **Regression (attributed; RESOLVED in wave W1x — see the follow-up block after this row):
  `TestDependencyRevisionClaimedFullAndDeltaOutput`**
  (`internal/indexer/builder_dependency_revision_test.go:133`) fails with
  `err=invalid dedicated base candidate: dedicated delta parent dependency revision differs:
  parent generation 1`, in `results/indexer-normal-__TestAffectedBy_CapBoundsFanout_TestAffectedBy_-5`
  (501 / 1 / 1) and again under `-race` in
  `results/indexer-race-DependencyRevision_DedicatedBase_Dedicated_TextS-1` (165 / 1 / 0).
  Attribution is proven, not inferred: the same test **passes** on a pristine `git archive HEAD`
  export of `16efbf70` with none of the wave's dirty files
  (`/private/tmp/gxbase-mcp`, single-test run exit 0, `--- PASS … (1.14s)`). The test builds a
  delta whose parent carries `cohort-v1:a` while the output identity carries `cohort-v1:b` — the
  exact composition D2 forbids — so the test encodes the pre-D2 contract and the guard is doing
  what the item intends. The pre-existing test was not re-run by the item or its verifier because
  its name matches neither the item's own selection nor the verifier's `-run 'Dedicated'` lane.
- Limitations: (a) a stored heterogeneous chain from a mixed-binary window is refused rather than
  re-rooted; (b) the planner hard-errors on a non-homogeneous stored chain. Reachability today is
  nil — every production `DependencyRevision` is empty, so both new comparisons are `"" != ""`.
- Deviations: two comparisons were filled instead of one (both arms are independently
  mutation-pinned); the refusal is a wrapping sentinel rather than a new error type; the guard
  sits before the coalesce short circuit deliberately.
- Verifier verdict: **PASS** (no blockers), 1 major + 2 minor findings. Mutants B, C, D and a
  verifier-added full pre-edit revert E are each RED. The major finding (F1) predicted exactly the
  refusal shape that the pre-existing test above now demonstrates: fail-closed instead of
  re-rooting, with the recommendation that W6.9 degrade both refusals to a full-root verdict.

#### W2.1b follow-up (wave W1x) — the planner re-roots instead of wedging

- State: `tested`. Wiring is unchanged from the row above: the two primitives' only non-test
  callers are inside `dedicatedBaseRuntime`, which still has no production constructor. W4.1 owns
  mounting it.
- Agent: wave W1x, Lane B.
- Scope/files: `internal/indexer/dedicated_base_advance.go` (planner degrade, `:50-52` comment,
  `:63-69` condition, `:70-81` new verdict arm),
  `internal/indexer/dedicated_base_advance_revision_test.go` (assertion flipped at `:88-90`,
  `:143-159`; one new test), `internal/indexer/builder_dependency_revision_test.go` (the
  pre-existing test rewritten to the D2 contract in three arms).
  `internal/indexer/builder_dedicated_delta.go` is **byte-identical** to the wave 1 shape
  (md5 `e31889c3cff6d01553ac19234f90fe65`) — the builder's typed refusal is kept verbatim as the
  last line of defence. `internal/graph/store_sqlite/catalog_dedicated_base.go` was not touched.
- Invariant (unchanged, D2): a revision change roots a NEW full base and never extends a chain.
  What changes is the **verdict shape**: a stored chain that is merely not revision-homogeneous is
  not corrupt, so it must degrade to a full root, never to a publication error.
- Two defects settled: (a) the pre-existing
  `TestDependencyRevisionClaimedFullAndDeltaOutput` encoded the **pre-D2** contract — it composed a
  delta whose parent carried `cohort-v1:a` under an output identity carrying `cohort-v1:b` and
  asserted success — and is rewritten, not relaxed; (b) the planner folded
  `row.DependencyRevision != policy.DependencyRevision` into the *invalid-ancestry* condition, so
  such a chain returned `ErrDedicatedBaseCandidate` out of `ensureObserved`
  (`dedicated_base_runtime.go:212-216` propagates it verbatim) with **no path back to a root** —
  every later `ensureCurrent` failed identically, forever. This is the W2.1b verifier's finding F1.
- Change: the revision clause moves out of the invalid-ancestry condition into its own `return 0,
  nil` verdict, placed **after** the invalid-ancestry guard so that genuinely corrupt ancestry
  (wrong owner, wrong state, broken `ConfigHash`) still hard-errors. Depth 0 cannot reach the new
  arm by construction (`policy.DependencyRevision` is `active.DependencyRevision` and the first row
  is `active`). The shape is reachable, not a defence against an impossible state:
  `validateDedicatedBaseChainTx` (`catalog_dedicated_base.go:333-345`) checks `ConfigHash` /
  `ExtractorVersions` / `ResolverVersion` at every depth but the dependency revision **only at
  depth 0 and only under `exactTree`**, and the proposed-parent arm passes `exactTree=false`
  (`:520`).
- Acceptance gate(s): G6, G8.
- Harness evidence (wave W1x exit suite): `internal/indexer` was run as six chunks; the item's
  files land in `^Test[D-H]` → **526 / 0 / 0** (`results/indexer-normal-_Test_D_H_-1`) — the
  attributed regression above is green — and `^Test[A-C]` → **522 / 0 / 1**
  (`results/indexer-normal-_Test_A_C_-1`), which carries
  `TestClaimedDedicatedDeltaParentRevisionChangeRefused`. Race lane
  `DependencyRevision|DedicatedBase|Dedicated|TextSearch|SearchText|Rehome|CheckoutMutation` →
  **167 / 0 / 0**, 0 `DATA RACE` (`results/indexer-race-DependencyRevision_DedicatedBase_Dedicated_TextS-2`).
- Limitations: limitation (b) of the row above is lifted — the planner no longer hard-errors on a
  non-homogeneous stored chain. Limitation (a) is **replaced**: such a chain is now re-rooted
  rather than refused, which costs a full rebuild of that base. Reachability is still nil — every
  production `DependencyRevision` is empty, so both comparisons are `"" != ""` today.
- Deviations: the degrade is placed after the invalid-ancestry guard rather than merged into it
  (merging would have let corrupt ancestry produce a silent full-root verdict — a real weakening);
  `builder_dedicated_delta.go` was not touched at all, the cleanest reading of "keep the builder's
  typed refusal"; the plan's W2.1b block is stale (it cites pre-wave-1 line numbers and describes
  work already present), so the coordinator brief and the source were followed instead.
- Verifier verdict: **PASS** — no blockers, 2 major + 3 minor findings. M1 (revert the degrade),
  M2 (delete the degrade `if`, i.e. treat non-homogeneous ancestry as *valid*), M3 (delete the
  builder guard), M4 (invert the builder guard) are each RED, and M2 and M4 together prove the fix
  is a verdict rather than a removed check and that the rewritten test is not one-sided —
  over-refusal is caught as well as under-refusal. A verifier mutant reordering the two guards
  (M7) is RED on the new `brokenPolicy` probe, which is what pins the ordering.
- Commit: `465601252678daae333f88424203fc7b79711e7f` — *indexer: root a new dedicated base when
  the dependency revision changes* (5 files; the two `builder_dedicated_delta*` files carry the
  unchanged wave 1 content into the same commit).

## W3 — Bind all writers and producers

- State: `proposed`. Dependencies: W2. Items: W3.1–W3.7, all in MVI.
- Invariant: every writer participates in the same authority and writes only to the correct output
  generation/owner. **No committed build reads current dirty files through a side channel.**
- Acceptance gate(s): G3, G6, G2.
- Limitations: a generation-zero/owner lease protects lifetime but does **not** freeze filesystem
  bytes; raw external repositories need a verified immutable image tied to their admitted data
  (out of scope, D10). Migration number allocation is single-owner: W3.3 takes **v25** (branch is
  at v24, `schema_version.go:37`) — two lanes each claiming v25 is the known salt-collision class.
- Next action: the authority (W3.1) in wave 4. W3.5 landed in wave 1 and is recorded below.

### W3.5 — `search_text` capability truth for committed identities (D5)

- State: `wired`. The verifier's wiring check confirms the production path:
  `declareProducers` runs on every generation publish, and `GrepCheckout`'s route gate is reached
  from `internal/mcp/view_search_text.go` on the default request path.
- Agent: wave 1, Lane B. Repair round applied after a failed first verification.
- Scope/files: `internal/indexer/builder_generation.go` (+96/−…),
  `internal/indexer/checkout_text_search.go` (+61/−…),
  `internal/indexer/builder_generation_test.go` (new, 179 lines, 3 tests),
  `internal/indexer/checkout_text_search_capability_test.go` (new, 269 lines, 5 tests).
  `internal/indexer/grep.go` and `checkout_text_search_test.go` are **byte-identical to base**, and
  no file under `internal/mcp` was touched.
- Invariant (D5): a view must not answer `search_text` out of a working copy its identity does not
  describe, and must not positively assert a capability it cannot vouch for. Silence — inheriting
  from the layer below — is the honest declaration where neither serving nor withdrawing is true.
- Root cause the repair round fixed: narrowing a checkout's **commit-layer generation** is wrong
  because `Materializer.completeness` (`internal/graphview/materialize.go:749-770`) takes
  `worst()` over every handle in the assembled stack, so any narrowing on a commit layer or on the
  dedicated base beneath it is worst-cased into every live routed view above it. That produced a
  false negative and turned the pre-existing end-to-end pin
  `internal/mcp/view_search_text_e2e_test.go:225` red. The lie the plan describes is a property of
  the **route**, not of any generation.
- Change, two halves: (a) `textSearchProducer` (`builder_generation.go:1201-1252`) now returns
  `(row, declared bool)` — a working-tree layer declares `complete`, a ref view declares
  `unavailable` with `noWorkingCopyTextSearchReason`, and a checkout commit layer, a dedicated base
  or an unrecognised kind declare **nothing at all**; `declareProducers` appends only a declared
  row. (b) `routeDescribesTheWorkingCopy` (`checkout_text_search.go:97-122`) reads the checkout's
  route and `GrepCheckout` (`:61-95`) returns `served = false` when no routed layer describes the
  bytes on its root — which the existing reader path turns into the typed `CodeCapabilityUnavailable`
  refusal. A checkout with no route at all keeps its answer, deliberately.
- Acceptance gate(s): G3, G6, G2.
- Harness evidence (wave 1 exit suite): all 8 tests ran in `internal/indexer` normal chunk 6
  (`results/indexer-normal-__TestClaimedDedicatedDeltaParentRevisionChangeR-1`, 28 / 0 / 0); the
  `TextSearch|SearchText` arm of the race lane
  (`results/indexer-race-DependencyRevision_DedicatedBase_Dedicated_TextS-1`) is green with 0
  `DATA RACE`. The consuming package's mandated race lane
  (`results/internal_mcp-race-ViewBase_BaseSelector_RequestView_Capabilit-1`) carries two failures
  that are **proven pre-existing at `16efbf70`** and are not attributable to this item — see the
  wave 1 exit-suite entry in the Evidence log.
- Harness evidence (wave W1x exit suite): carried unchanged. The eight tests sit in
  `internal/indexer` normal chunks `^Test[A-C]` (`results/indexer-normal-_Test_A_C_-1`, 522 / 0 / 1)
  and `^Test[S-T]` (`results/indexer-normal-_Test_S_T_-1`, 330 / 0 / 0); the
  `TextSearch|SearchText` arm of the race lane is green with 0 `DATA RACE`
  (`results/indexer-race-DependencyRevision_DedicatedBase_Dedicated_TextS-2`, 167 / 0 / 0). The
  consuming package's race lane is now **238 / 0 / 0**
  (`results/internal_mcp-race-ViewBase_BaseSelector_RequestView_Capabilit_RefV-1`): the two
  failures cited above were the ref-view admission regressions W1.9 fixed, and they are gone.
- Commit: `679a5707845ef8e0a9c5f33aab2ee440dea0624b` — *indexer: stop claiming search_text for
  identities that do not describe bytes* (4 files).
- Limitations: declared limitation 1 (D5) stands unchanged — a generation-scoped text corpus does
  not exist, so the honest declaration is a withdrawal, not a served answer. The route gate is a
  point-in-time read: a route that changes between the gate and the searcher build is not fenced.
- Deviations: the first attempt's narrowing of the commit-layer generation was **withdrawn**, not
  patched; the item now narrows nothing on a generation that a live routed view stacks on.
- Verifier verdict: **PASS with findings** (1 major, 5 minor), no blocker. M1 (default arm returns
  `{CapSearchText, Complete}`) and M3 (ref-view withdrawal deleted) are each RED across the
  expected test sets; the export reproduced 18 / 0 / 0 on the item's selection.

## W4 — Activate coherent base and working routes

- State: `proposed`. Dependencies: W2, W3. Items: W4.1–W4.4, W4.6–W4.8 in MVI; W4.5 out of scope.
- Invariant: `B0 + D0 = T` and `B1 + D1 = T` — the logical delta changes when the base changes
  while valid payload is reused. `ActiveGenerationID` alone does not construct an exact
  working-copy route; missing/in-progress routes must refuse or return an explicitly labeled
  read-only fallback, and generation zero is never presented as an exact positive working route.
- Acceptance gate(s): G4, G5, G6.
- Limitations: `ensureCurrent` exists as a primitive but is not activated in daemon startup,
  watchers, or Git callbacks. The rebase conflict resolution in `CheckoutCoordinator.RehomeTo`
  acquires `acquireCycleLock` before the graph gate and removes the old second acquisition —
  reintroducing both lock paths would deadlock. W4.3's audit must also cover `watcher.go:1459` and
  `incremental_watcher_batch.go:349`, two further `IncrementalReindexPaths` entrypoints into the
  legacy generation-0 writer.
- Next action: W4.2 in wave 4, W4.3 in wave 5.

## W5 — Integrate request lifetime and selected readers

- State: `proposed`. Dependencies: W4. Items: W5.1–W5.12, all in MVI.
- Invariant: borrowed readers and source providers stay alive until actual worker completion,
  including cancellation tails; caches and sidecars are keyed by selected snapshot/capability
  identity.
- Acceptance gate(s): G1, G7.
- Limitations: a correct graph plus global source/search/cache data can still produce a mixed
  view. The historical "coherent request-reader integration packet" proposal is reference-only —
  it was private, uncompiled and not activated; its counts (52 proposed files, 90 edits across 21
  files, 78 tests, 12 benchmarks) are proposal counts, not landed files or passing tests. Its
  source JSON is preserved in the recovery directory as `historical-proposal-tool-results.json`.
- Next action: W5.2 and W5.3 in wave 2. W5.1 and W5.8 landed in wave 1 and are recorded below.

### W5.1 — Lease-handoff primitive (detach / joined consumer)

- State: `tested`. **Not `wired`** — correctly declared: the three `context.WithoutCancel`
  detaches (`internal/mcp/view_mutation_state.go`, `edit_serialization.go`, `tools_multi.go`) and
  `boundHandler` still do not call `Handoff`. W5.2 owns binding them.
- Agent: wave 1, Lane V.
- Scope/files: `internal/graphview/lease.go` (+111/−…), `internal/graphview/materialize.go` (+86),
  `internal/graphview/lease_handoff_test.go` (new, 483 lines, 12 tests). `lease_test.go` and
  `materialize_test.go` are untouched — confirmed by the verifier's diffstat.
- Invariant: a borrowed reader stays alive until actual worker completion, including cancellation
  tails; a handle that can no longer pin anything must refuse rather than hand back a handle that
  pins nothing.
- Change: `Lease` refcounts **holders** (acquirer plus one per joined consumer,
  `lease.go:58-68`, `:74-94`); `Release` drops one holder under the existing `sync.Once`
  (`:195-202`) and the manager's per-id pins are released exactly once, on the last holder
  (`drop`, `:205-219`). `Lease.Handoff()` (`:239-281`) joins a consumer and
  `RepoView.Handoff()` (`materialize.go:170`) hands the same reader / identity / completeness /
  generation sources to detached work over that joined handle. Both refuse (return `nil`) once
  every holder has released. `LeaseManager.WaitDrain` is untouched and therefore waits for joined
  consumers for free, because the pins it watches are still present. This is the
  `RawRepositorySnapshotLease` shape (`repository_raw_data.go:34-37`) applied to payload
  generations. Exported signatures are additive only; `Release` / `Close` stay idempotent and
  nil-safe.
- Acceptance gate(s): G1, G7.
- Harness evidence (wave 1 exit suite): `internal/graphview` normal `.` → **447 / 0 / 0**
  (`results/graphview-normal-_-5`); race `Lease|Handoff|Materialize|Drain` → **79 / 0 / 0**, 0
  `DATA RACE` (`results/graphview-race-Lease_Handoff_Materialize_Drain-1`).
- Harness evidence (wave W1x exit suite): carried unchanged. `internal/graphview` normal `.` →
  **447 / 0 / 0** (`results/graphview-normal-_-6`); race `Lease|Handoff|Materialize|Drain` →
  **79 / 0 / 0**, 0 `DATA RACE` (`results/graphview-race-Lease_Handoff_Materialize_Drain-2`). Both
  counts reproduce the wave 1 figures exactly.
- Commit: `be4ae174878a6e2bd1faf1b55c908e7a37c91e5c` — *graphview: let a lease serve a joined
  consumer for its whole lifetime* (3 files).
- Limitations: the primitive protects **lifetime**, not filesystem bytes; a handed-off reader can
  still observe a working copy that changed underneath it. Nothing enforces that a detached worker
  actually releases its handle — a leaked handoff pins a generation indefinitely, and no budget or
  deadline reaps one.
- Deviations: none.
- Verifier verdict: **PASS** (no blockers), 3 minor findings, all forward-looking for W5.2.
  Mutants M1 (unconditional `release`), M2b (build the handle without joining), M3 (release pins on
  every holder drop) and M4 (drop the `holders == 0` refusal) are each RED, and the harness
  reproduced the implementer's binary sha256 byte-for-byte.

### W5.8 — Make `view:{kind:"base"}` actually narrow the reader (D13)

- State: `wired`. The verifier traced the production path
  `wrapToolHandler` → `resolveRequestView` → `viewForBaseSelector` on the default request path.
- Agent: wave 1, Lane R. Repair round applied after a failed first verification.
- Scope/files: `internal/mcp/view_request.go` (+557/−…),
  `internal/mcp/view_capabilities.go` (+132/−…),
  `internal/mcp/view_base_selector_test.go` (new, 705 lines, 11 tests).
- Invariant (D13): a labelled base view must read what its label says. A rider claiming
  `exact:true, graph_id:X` must not be served by a reader over every tracked repository, and the
  capability declaration must state what the narrowing costs and what it cannot evidence.
- Root cause: `viewForBaseSelector` returned `&requestView{rider: rider}` with reader / candidates
  / viewRoot zero; because `routed()` is reader-presence (`view_request.go:137`),
  `requestBaseReader` handed back the whole `s.graph` and `evaluateRequestCapabilities` skipped the
  contract entirely.
- Change: the selector now gets a reader scoped to the graph's repository prefix, with every lane
  narrowed (name lookup refills past foreign matches rather than over-fetching once, `:1017`;
  `Stats()` never reports the corpus, `:1173`; `keepEdges` never names another graph's symbol,
  `:914`). The capability declaration stops claiming language-server completeness (`:327`) and
  gives a non-strict fallback its own completeness contract (`:348`), and the
  `!view.routed()` exemption at `view_capabilities.go:399` is gone.
- Acceptance gate(s): G1, G7.
- Harness evidence (wave 1 exit suite): race `ViewBase|BaseSelector|RequestView|Capabilit` over
  `./internal/mcp` → **169 pass / 2 fail / 0 skip**, 0 `DATA RACE`
  (`results/internal_mcp-race-ViewBase_BaseSelector_RequestView_Capabilit-1`). Both failures are
  **proven pre-existing at `16efbf70`** and are not attributable to this item or to any wave item
  — see the wave 1 exit-suite entry in the Evidence log.
- Limitations: `internal/mcp` carries 19 failing top-level tests at the branch base, independently
  reproduced by the verifier on a pristine tree with an empty symmetric difference against the
  shipped tree; this item neither adds to nor fixes that set. The narrowing is by repository
  prefix, not by generation, so it bounds *which repository* a base read can see, not which
  snapshot.
- Deviations: the repair round kept the pre-change **write** behaviour rather than the refusal the
  first round introduced. A base selector has `viewRoot == ""`, so path resolution keeps the
  ordinary heuristic and writes exactly where an unrouted base request wrote; narrowing what a base
  selector *reads* must not silently convert every base-labelled edit into `view_read_only`. The
  new `baseNarrowed` field and `readsOwnCheckout()` predicate (`:139-143`) carry that distinction,
  pinned by `TestBaseSelectorStillAdmitsSourceEdits`, which drives a real `edit_file` through
  `wrapToolHandler` and also asserts that a fallback view is still refused.
- Wave W1x repair round: the verifier failed the first round on one blocker — the site filter's
  cost. `siteInScope` decided every out-of-prefix edge site with its own per-path store round trip,
  memoized only for one request's reader: on a 2-repo, 2000-sibling-file corpus `AllEdges` went
  0 → 2000 calls *for no change in the answer*, `GetInEdges` 0 → 2000. Three changes, each pinned
  by its own counter assertion. **Ordering:** `edgeInScope` (`view_request.go:942-961`) asks the
  endpoints first and the site last, and `AllEdges` (`:1312`) and the streaming `EdgesByKind` lane
  (`:1380`) apply the endpoint half as an explicit pre-pass, so an edge the endpoints drop never
  reaches a site lookup (`&&` is commutative — the answer is identical and only the work moves).
  **Batching:** `prefetchSites` (`:1040-1066`) resolves every still-undecided site of a batch in
  one `GetFileNodesByPaths` behind a `fileNodesBatchReader` assertion (`:1023-1026`), with the
  per-path door surviving only as the fallback for a backend with no batch primitive; the batched
  walk takes one **union** prefetch (`adjacencyByNodeIDs:1292-1300`) so a fan-out over anchors
  cannot reappear as a fan-out over batches. The streaming lane buffers `baseGraphEdgeWindow = 512`
  edges and decides one window in two batched round-trips, yielding in the corpus's own order.
  **Endpoint residue:** `endpointScope` (`:1347-1376`) now decides every id it asked about instead
  of falling back to a per-id `GetNode` for an id with no row — an unresolved caller is exactly
  what a cross-repository reference looks like, so that arm was the common case, not the rare one.
  Additionally `requestView.kind` became a checked vocabulary (`:75`, `:530-546`), so a typo at a
  producer can no longer ship a telemetry series `internal/viewmetrics/catalog.go:402-404` never
  declared; and the two new site shapes are pinned **through a handler** against the real SQLite
  store (`view_base_selector_test.go:1326`, `:1384`, plus a compile-time
  `var _ fileNodesBatchReader = (graph.Store)(nil)` at `:1374`, because a type assertion that
  misses is silent).
- Rejected deliberately: the verifier also asked for `GetFileNodesContext` "where a context is
  reachable". `graph.Reader` takes no context at any method boundary, so the only context available
  is one captured on the reader — and that context outlives the request through the very detach
  sites this work is repairing. A cancelled captured context returns no rows, which this predicate
  reads as "not my site", so the reader would silently truncate an edge list still riding
  `exact:true`. Trading a slow answer for a quietly wrong one is not available; the volume problem
  is fixed instead, bounded to one batched round-trip per lane call.
- Harness evidence (wave W1x exit suite): `./internal/mcp` normal `.` → **5926 / 2 / 8**
  (`results/internal_mcp-normal-_-6`); race
  `ViewBase|BaseSelector|RequestView|Capabilit|RefView|SearchText` → **238 / 0 / 0**, 0 `DATA RACE`
  (`results/internal_mcp-race-ViewBase_BaseSelector_RequestView_Capabilit_RefV-1`), against
  169 / **2** / 0 over the narrower wave 1 selection. Neither surviving failure lives in a file
  this item touches; both are the upstream `TMPDIR`-length artifacts W1.9 characterizes.
- Limitation added by the repair round: optional store capabilities are **withdrawn** under the
  narrowing — `baseGraphReader` forwards only `graph.ContentSearcher`, so
  `BoundedFileNodeReader` / `BoundedExactNameReader` / `BoundedIncomingSourceReader` /
  `NodeDegreeByKinds` / `FileEditingContext` / `NodesInFilesByKindFinder` all miss. Most callers
  degrade, but the localization lane is explicitly fail-closed
  (`localization_projection.go:74,97`, `enclosing.go:309-311`), so under `view:{kind:"base"}` it
  serves *less* evidence than HEAD did with nothing on the rider naming it. Fail-closed and never
  fabricating, but the completeness declaration does not cover it yet.
- Named exemption: a reader-less producer that fails to declare its kind is **annotated rather than
  refused** (`view_capabilities.go`, exercised by `TestUndeclaredReaderlessViewIsServedAndAnnotated`).
  The arm is unreachable today — all three reader-less producers declare — and the alternative (a
  nil `Completeness` denying everything, hence a blanket refusal) is worse, but it is an exemption
  and is recorded here as one.
- Successor change, unchanged from the first round: `internal/mcp/view_ref.go:150` still constructs
  its `requestView` with no `kind`, so the inference cannot be deleted. One line closes it:
  `routed := &requestView{kind: requestViewKindRef, …}`.
- Commit: `d402aecf2bc95b8554acd6e25e1c564331e646ed` — *mcp: narrow the reader a base-selected view
  actually serves* (3 files).
- Verifier verdict (wave W1x): the first W1x round was **failed** — 1 blocker (the site filter's
  cost, measured above) and 4 minor. After the repair round: **PASS — 0 blockers, 5 minor
  findings**, with **12 / 12** revert-red mutations binding. The verifier's own export measures the
  package as 3409 top-level pass / 3 fail / 3 skip against the 5926 / 2 / 8 recorded here; the two
  statements reconcile once the counting basis (subtests included) and the git-checkout dependence
  of `TestDetectChanges_EchoesMeasuredScope` are stated, and both are.
- Verifier verdict (wave 1): **PASS** — 1 major, 5 minor findings, no blocker. Mutants M1–M7 are each RED
  and each named a single expected test; the whole-package failing set was `comm`-compared against
  a pristine rebuild in both directions and came back empty on both sides.

## W6 — Finish bounded reuse and storage maintenance

- State: `proposed`. Dependencies: W2, W3, W4. Items: W6.1–W6.5, W6.7, W6.9–W6.12 in MVI; W6.6 and
  W6.8 out of scope.
- Invariant: read-only context does not gain duplicate payload or replacement ownership merely
  because it was needed for resolution; reuse never drops supported derived output. A lower-layer
  reader must not turn scoped resolution into whole-corpus `ResolveAll`.
- Baseline: historical — five builds, 1,218 indexed file instances for 391 modified/added paths
  and 205 deletions, at least 827 instances outside the raw modified/added set, all five hitting
  the 200-additional-context-file cap; one six-modified-file example produced a 204-file build.
  Motivating, **not** a verified current baseline.
- Acceptance gate(s): G3, G4, G8.
- Limitations: the current allocation policy proposes a full root at 32 active ancestors against a
  hard 64-generation catalog ancestry limit — **provisional**: not proven compaction, not a
  bounded steady-state disk figure, not an established reseed cost. W6.1 is the plan's only XL and
  is strictly serialized S → V → B.
- Next action: W6.2 in wave 3.

## W7 — Complete lifecycle/recovery integration

- State: `proposed`. Dependencies: W1.3, W1.4, W4, W5. Items: W7.1, W7.2, W7.4, W7.6, W7.7 in MVI;
  W7.3, W7.5 out of scope (D10).
- Invariant: closing rejects new admission, drains existing work, and finalizes only the captured
  registration; deletion/recreation, path/ID reuse, follower cancellation and delayed callbacks
  cannot resurrect or damage another owner.
- Baseline: W1.3 and W1.4 are now settled in favour of production; their contracts are the live
  evidence for this wave rather than counter-evidence.
- Acceptance gate(s): G7, G9.
- Limitations: stale publishers, drain handles and cleanup callbacks must not affect replacement
  registrations at the same path or with reused IDs. The `internal/persistence` sidecar DB
  obligation is currently unowned — see the plan bookkeeping table (B1).
- Next action: W7.4 in wave 4.

## W8 — Prove end-to-end correctness and I/O benefit

- State: `proposed`. Dependencies: W1–W7. Items: W8.1–W8.12, all in MVI.
- Invariant: an isolated daemon, config, state, storage and source fixtures are used. **Never**
  the user's live daemon or store as a destructive fixture.
- Baseline: no current baseline exists. The ~459 GB / ~15 h process-accounted write report from
  [issue 767](https://github.com/zzet/gortex/issues/767) is a reported symptom, not a measurement
  of NAND wear and not a verified current baseline.
- Acceptance gate(s): all ten, specifically G8 and G10.
- Limitations: a current WAL size is not cumulative writes; SQL trigger row counts omit other
  SQLite writes; process writes are not SSD NAND writes; small-fixture replay latency is not
  sustained daemon behavior. Budgets must be **frozen by W8.4 from the baseline arm before the
  candidate is measured** (W8.12).
- Next action: W8.1/W8.2 in wave 9.

## W9 — Deliver reviewable change

- State: `proposed`. Dependencies: W1–W8. Items: W9.1–W9.5, all in MVI.
- Invariant: the PR distinguishes measured improvements from unmeasured dimensions and reports
  every open gate plainly. No feature-complete or disk-fixed claim rests on microbenchmarks,
  private overlays, or old binaries.
- Acceptance gate(s): G10.
- Limitations: the 68-file dirty inventory must not be split into commits before the W1 suites
  pass (see Decisions). The declared-limitations list below must appear verbatim in the PR body.
- Next action: TBD after W8.

## Plan (execution-plan-v2)

### Waves

One shared worktree; each wave's items have disjoint owner files. Each wave ends with `go build
./...`, the touched packages' normal suites, a race selection where concurrency-relevant, and the
item's own named verification.

| wave | items |
| --- | --- |
| 0 | W1.3, W1.4, W1.6 |
| 1 | W2.1a, W2.1b, W3.5, W5.1, W5.8 |
| 2 | W2.1c, W2.2, W3.4, W5.2, W5.3 |
| 3 | W2.4, W4.1, W6.2, W5.5, W5.9 |
| 4 | W4.2, W3.1, W7.4, W6.3, W5.10 |
| 5 | W4.3, W7.2, W3.3, W3.2 + W3.7, W5.4 |
| 6 | W4.4, W5.6, W5.7, W7.6, W6.5 |
| 7 | W4.6, W6.7, W5.11, W5.12, W7.7 |
| 8 | W4.8, W7.1, W6.4, W6.9, W3.6 |
| 9 | W6.1 (→ W6.11 serialized in-slot), W6.10, W6.12, W8.1, W8.2 |
| 10 | W4.7, W8.3, W8.4, W8.5, W8.6 |
| 11 | W8.7, W8.8, W8.9, W8.10, W8.11 |
| 12 | W8.12, W9.1, W9.3 |
| 13 | W9.2, W9.4, W9.5 |

Serialization notes: W3.2/W3.7 share `tools_enhancements.go`; W6.1/W6.11 share
`builder_generation.go`; W5.6/W5.11 share `tool_deadline.go` and are therefore in different waves.

### Lanes

Five lanes, disjoint by file. **Default rule: any file not listed in a lane is owned by Lane I**,
and the lane that needs it files the exact hunk as a request.

| lane | ownership |
| --- | --- |
| **S** Storage & catalog | `internal/graph/store_sqlite/**` (minus four Integrator-owned files) plus `internal/graph/bounded_*.go`, `localization_*.go`, `overlay*.go`, `restub_provenance.go`, `mutation_receipt.go`, `analysis_cache.go`, `index_state.go` |
| **V** Graphview composition & leases | `internal/graphview/**` — leases, materialization, generation layers, composition |
| **B** Build & resolution runtime | `internal/indexer/builder_*.go`, `dedicated_base_*.go`, the affected-by / batch / alias / text-search readers, and `internal/resolver/**` |
| **R** Request surface | `internal/mcp/**` except `overlay.go`, plus `internal/profiles/**` and `internal/server/**` |
| **I** Integrator | `cmd/gortex/**`, `internal/serverstack/**`, `internal/reconcile/**`, `internal/daemon/**`, `internal/viewmetrics/**`, `docs/**`, the shared indexer/catalog choke-point files (migration numbering included), **and every file not otherwise listed** |

### Items

Size key: S ≤ 1 day · M 2–4 days · L 1–2 weeks · XL > 2 weeks. MVI roll-up (from the critique's
own arithmetic, M6): **≈ 1 XL + 5 L + 20 M + 5 S**.

| id | title | size | in_mvi |
| --- | --- | --- | --- |
| W2.1a | Dependency-revision cohort digest producer | M | yes |
| W2.1b | A revision change roots a new chain, never extends one (D2) | S | yes |
| W2.1c | Bind the revision + widen the config digest into `GenerationIdentity` | M | yes |
| W2.2 | Wire `snapshotDedicatedBaseConfig` at coordinator/builder construction | M | yes |
| W2.4 | Derive a real `ResolverVersion` instead of the literal `"1"` | S | yes |
| W2.5 | Raw-repository immutable source image + `*source.FilesystemSource` rejection | L | no |
| W2.6 | `BeginRawRepositorySourceMutation` durable revision floor | M | no |
| W3.1 | Single output-generation authority, installed in `NewSharedServer` (D12) | L | yes |
| W3.2 | Route blame/churn/coverage/release/LSP enrichment **writes** through an authority handle | M | yes |
| W3.3 | Give the analysis cache a real `view_gen` axis (D9) | L | yes |
| W3.4 | Close the committed-build disk side channels (compile DB, npm/tsconfig alias readers) | M | yes |
| W3.5 | `search_text` capability truth for committed identities (D5) | M | yes |
| W3.6 | Hoist ANALYZE / VACUUM / checkpoint out of the publish path | M | yes |
| W3.7 | Close the blame / coverage **input** side channel | M | yes |
| W4.1 | Install `dedicatedBaseRuntime` in `NewSharedServer` before any owner registration (D1) | M | yes |
| W4.2 | `ensureInitial` on cold **and** warm startup, before `Seed` registers owners (D1) | L | yes |
| W4.3 | `ensureCurrent` from the Git watcher's HEAD-change finalize path (D1) | L | yes |
| W4.4 | Advance `checkouts.head_tree` atomically with adoption; fan out; fence the observation write | L | yes |
| W4.5 | Primary dirty upper layer + exact dedicated-primary routing | L | no |
| W4.6 | Catalog-backed layer reuse lookup by identity key (survives restart) | M | yes |
| W4.7 | Dirty-layer reuse — fingerprint-keyed in-process cache (D14) | M | yes |
| W4.8 | Pin a routed dependent to its built-against base; recompose, do not rebuild (D15) | M | yes |
| W5.1 | Lease-handoff primitive (detach / joined consumer) on `Lease` + `RepoView` | M | yes |
| W5.2 | Bind detached workers and the deadline firewall to a handed-off lease | M | yes |
| W5.3 | Pin generation 0 for the duration of a request | M | yes |
| W5.4 | Repository-owner lease for the serving request and for background builds | M | yes |
| W5.5 | Re-root the editor-buffer overlay onto the request view; key its cache by view identity | M | yes |
| W5.6 | Bind or truthfully annotate analyses / health / resources / prompts | L | yes |
| W5.7 | Snapshot coherence for `search_text` and worktree source bytes | M | yes |
| W5.8 | Make `view:{kind:"base"}` actually narrow the reader (D13) | S | yes |
| W5.9 | Implement `require_fresh` / `wait_deadline`; **publish** `require_exact` (D6) | M | yes |
| W5.10 | Key caches / sidecars by selected snapshot identity | M | yes |
| W5.11 | HTTP non-tool endpoints, `resources/read`, `prompts/get`: bind a view or ride a truthful rider | L | yes |
| W5.12 | CLI front-door parity: controller probe-view + `gortex enrich` base writes | M | yes |
| W6.1 | Context/output separation via a context ownership mode (D4, route b) | XL | yes |
| W6.2 | Exact restub gate on the legacy path | M | yes |
| W6.3 | Whole-batch bound + completeness fact for the legacy affected-by union | M | yes |
| W6.4 | Bound Path A's incoming stub admission with the scoped projection | M | yes |
| W6.5 | Import-placement parity harness against the resolver cascade (test half) | M | yes |
| W6.6 | Forward-frontier delta gate for `collectDependencies` | L | no |
| W6.7 | Mark superseded and retire replaced dedicated chains (D7) | M | yes |
| W6.8 | Compaction / reseed of a delta chain | XL | no |
| W6.9 | Bound ancestry depth by refusing / forcing a full root before the catalog hard limit (D7) | M | yes |
| W6.10 | Ref-fact hint scope on the commit-layer base | M | yes |
| W6.11 | Gate 3 second clause: reuse re-derives or explicitly re-claims each of the ten derived outputs | L | yes |
| W6.12 | Conservative handling for missing metadata and dynamic-language cases | M | yes |
| W7.1 | Fix the confirmed public-untrack reader-lifetime defect (D11) | L | yes |
| W7.2 | Activate the publisher drain half | M | yes |
| W7.3 | Raw registration / roster for untrack, recreate, path/ID reuse | L | no |
| W7.4 | Move dedicated `CloseRepositoryAdmission` to handle identity | S | yes |
| W7.5 | Restart / crash recovery for raw mutation revision | M | no |
| W7.6 | Consume `SafeStorageFailureReason`; handle ENOSPC on retirement / sweep | S | yes |
| W7.7 | Budget the retirement sweep (rows / chunks / elapsed) | S | yes |
| W8.1 | Extract the shared fixture; sustained-workload harness skeleton | M | yes |
| W8.2 | Deterministic fixture generator, 1 Hz sampler, WAL-header checkpoint counter, manifest | M | yes |
| W8.3 | Emit `viewmetrics` counters; render `StatusResponse.Views.Counters`; `daemon status --format json` | M | yes |
| W8.4 | Baseline arm on `main 56a1c29d`; **freeze budgets before looking at the candidate** | M | yes |
| W8.5 | E2E matrix 1 — idle / no-op family | M | yes |
| W8.6 | E2E matrix 2 — edit taxonomy | M | yes |
| W8.7 | E2E matrix 3 — resolution / provenance / manifests | L | yes |
| W8.8 | E2E matrix 4 — view lifecycle | M | yes |
| W8.9 | E2E matrix 5 — primary dirty vs main advancement with ten dependents | L | yes |
| W8.10 | E2E matrix 6 — untrack / recreate / drains / cancellation / crash / disk-full | L | yes |
| W8.11 | E2E matrix 7 — adversarial fan-out, chain maintenance, cross-surface coherence | M | yes |
| W8.12 | Paired candidate run; verdict against the frozen budgets; regressions preserved | M | yes |
| W9.1 | Execution-ledger rows for every W2–W8 item, in this state vocabulary | M | yes |
| W9.2 | Atomic commits, one per integration unit, with its evidence | M | yes |
| W9.3 | `golangci-lint` + `go vet` + full normal and race suites on final source | M | yes |
| W9.4 | Final diff review | S | yes |
| W9.5 | PR text with truthful validation, limitations and the upgrade cost | S | yes |

### Coordinator decisions D1–D15

| id | decision |
| --- | --- |
| D1 | Activate the dedicated-base runtime on the default path: install it in `NewSharedServer` (`shared_server.go:636`, not `cmd/gortex/daemon.go`, so the embedded one-shot lifecycle shares it), `ensureInitial` on cold **and** warm startup before `Seed` registers owners, `ensureCurrent` from the Git watcher's HEAD-change finalize path. |
| D2 | A dependency-revision change **roots a new chain**; it never extends an existing one. Directly supported by source: `dedicatedBaseParentForAdvance`'s policy tuple (`dedicated_base_advance.go:44-47`, compared at `:77`) and `BuildClaimedDedicatedDelta`'s parent check both omit `DependencyRevision` today, so without W2.1b a revision change would silently extend. |
| D3 | **Determination: committed roots are self-contained**, not layered above generation 0. `BuildClaimedDedicatedBase` builds from `Base: graph.New()` and rejects any non-zero base/layer/lower-fingerprint (`builder_dedicated_claimed.go:37-38,57-58,93`); `materialize.go:522-527` treats a dedicated first row as the base. |
| D4 | **Determination: route (b)** — the pass keeps reading context files, and separation is enforced by a context **ownership mode** plus publish validation, rather than by read-through. This is W6.1, the plan's only XL. |
| D5 | `search_text` on a committed/dedicated identity is declared *not served completely*; a generation-scoped text corpus is out of scope. W3.5 makes the capability tell the truth. |
| D6 | **Corrected by evidence.** D6 assumed the view-selector schema blocked the freshness knobs. The selector is a separate object `{kind, graph_id, checkout_id, value, path}` (`view_request.go:178-180`); the blocking schema is **each guarded tool's own `InputSchema`** — `server.go:3285-3286` stamps `AdditionalProperties=false` and `arg_schema_guard.go:159-175` rejects any key not in `InputSchema.Properties`. Extra finding: **`require_exact` is in no tool's Properties either**, so the already-shipped knob is itself rejected on guarded tools today. W5.9 must publish all three through `publishViewSelectorSchema` (`view_request.go:240-248`). |
| D7 | Mark superseded and retire replaced dedicated chains, and bound ancestry by refusing / forcing a full root before the catalog hard limit. **No real compaction or reseed** (W6.8 is out of scope). |
| D8 | W2.1c and W2.4 each invalidate every cached generation once on first deploy. Bundled into one deliberate deploy and documented as an upgrade cost. |
| D9 | **Corrected by evidence.** D9 asked whether an authority-issued stamp with the existing `expectedRevision` CAS could substitute for a `view_gen` column. Verified: it cannot. `analysisMutationRevision` is a coarse, **process-local** counter on the shared `storeCore` (`store.go:46,:129`; `store_generation.go:28-34`) bumped only on committed graph mutations (`analysis_generation_state.go:88-93`) and reset on reopen (`analysis_generation_read.go:79-81`) — it is shared across generations. **The `view_gen` column via an additive migration (v25) is the branch that applies** (W3.3). |
| D10 | Non-Git external repositories get no immutable source image this round. W2.5, W2.6, W7.3, W7.5 are out of scope; gate 1 and gate 7 closure is scoped to Git-backed repositories. |
| D11 | The confirmed public-untrack reader-lifetime defect is fixed inside W7.1 (cleanup saga + graphview repository lease). |
| D12 | There is exactly **one** output-generation authority, at `coordinateRepositoryMutation` / `BeginCheckoutMutation`, installed in `NewSharedServer` (W3.1). Without it there is no mutation authority to fence gate 6 on. |
| D13 | `view:{kind:"base"}` must actually narrow the reader; today `viewForBaseSelector` does not (W5.8). |
| D14 | Dirty-layer reuse is an in-process, fingerprint-keyed cache only. The catalog-backed, survive-restart half is **not** implemented — a restart between two identical dirty states pays a full rebuild. |
| D15 | **Corrected by evidence.** D15 proposed re-keying a dependent's delta so the key survives a base advance. Not representable: the delta's payload is a **diff-tree against its base** (`diffTreeChanges(BaseTreeOID, TargetTreeOID)`, `builder_commit.go:65`), so the same bytes are simply not valid over a different base. The achievable goal — a base advance recomposes rather than rebuilds — is delivered by **pinning the base the dependent was built against and recomposing** (W4.8). |

### Plan bookkeeping — open obligations the waves must honour

From `execution-plan-v2-critique.md`. Each row is `proposed` until the named wave absorbs it.

| id | obligation |
| --- | --- |
| B1 | **`internal/persistence` sidecar DB is neither planned nor rejected.** A separate database file (notes, memories, scopes, notebooks) with no generation axis, carrying the obligation "must not be confused with payload during untrack/retire/cleanup". Gate 9's own words are "no database per worktree/generation". The plan mentions `persistence` **zero** times. Either add it to W7.1/W7.2's invariant and W8.10's untrack/recreate bullet, or record an explicit reasoned rejection in the declared limitations. |
| B2 | **Plain-filesystem writers are unattributed in the W8 measurement design.** Six non-graph byte writers (`daemon/server.go:1015` pid, `stop_intent.go:43`, `telemetry/send.go:51,191`, `consent_store.go:65`, `workspace/workspace_gitignore.go:47`, `indexer/watcher.go:682`) are all counted by the primary `ri_logical_writes` process-accounted metric. The headline budget is a ratio, and pairing cancels common-mode noise only if both arms emit the same non-graph writes. W8.2 must name these six, state whether the fixture disables them, and say how the remainder is attributed. |
| B3 | **W8/W9 carry no concrete commands.** Gate 10 requires evidence recorded *with commands*. No `go test -run` selector for any of the seven `w8_matrix_*_test.go` files, no opt-in build tag / env gate, no build command or output path for the paired baseline/candidate binaries, no `-tags`, no `-count`, no reference to the compile-then-test harness discipline. Add a command column or per-item command block to all twelve W8 and five W9 items. |
| M1 | **W3.1's caller enumeration.** The approach says "8 production callers" and then lists 11 sites in 7 files: `multi.go:605`, `git_watcher.go:266`, `indexer.go:2356/4350/5104/5152`, `poller.go:512`, `repository_topology_batch.go:100`, `watcher.go:1564/2357/2768`. The count is wrong and `owner_paths` omits `git_watcher.go`, `indexer.go` (a **Lane B** file), `poller.go`, `repository_topology_batch.go` and `watcher.go`. W3.1 needs a cross-lane split row or the paths added. |
| M2 | **`ProxyToolCtx` header forwarding has no owner path.** `internal/daemon/servers.go:421-445` does not forward `X-Gortex-Cwd` / `Mcp-Session-Id`, so a proxied call resolves its own view from the body's `cwd` alone. W5.11's approach names the defect but the file is in no item's owner paths. Assign `internal/daemon/servers.go` to **W5.11** under Lane I ownership. |
| M3 | **Two waves are not file-disjoint.** Wave 12: W8.12 and W9.1 both own this ledger, with no `depends_on` edge; wave 9: W8.1 and W8.2 both own `cmd/gortex/w8_sustained_io_integration_test.go`. Both need the one-line in-slot serialization note the plan already gives W3.2/W3.7 and W6.1/W6.11. |
| M4 | W4.1's "five lines before" is off: `shared_server.go:636` is correct as the install point, but `multiOpts` is at `:669` / `CheckoutLifecycle:` at `:672` — 36 lines later. |
| M5 | Three items under-state their owner paths: W4.3 (missing `watcher.go`, `incremental_watcher_batch.go`), W2.1b (missing the Lane-I `catalog_dedicated_base.go` and no splits row), W6.11 (ten outputs, four files). |
| M6 | **MVI size roll-up recorded:** ≈ **1 XL + 5 L + 20 M + 5 S**. Stated here because the plan carries per-item sizes but no total. |

## Declared limitations

These must appear verbatim in the PR body (W9.5).

1. **D5** — `search_text` on a committed/dedicated identity is declared *not served completely*; a
   generation-scoped text corpus does not exist (`internal/search/trigram` has no serialization
   path) and is out of scope.
2. **D7 / W6.8** — no real compaction or reseed. Chain growth is bounded only by W6.7's retention
   and W6.9's forced-full-root policy, and is reported as a **measured** number from W8, not as a
   proven bound.
3. **D10 / W2.5, W2.6, W7.3, W7.5** — non-Git external repositories have no immutable source
   image. Gate 1 and gate 7 closure is scoped to Git-backed repositories.
4. **D14 / W4.7** — the catalog-backed, survive-restart half of dirty-layer reuse is not
   implemented; a restart between two identical dirty states pays a full rebuild.
5. **W4.5** — the primary's own working route stays on legacy generation 0; the primary's
   uncommitted edits remain the standing skew at `checkout_coordinator.go:970-975`.
6. **W6.6** — the forward-frontier delta gate for `collectDependencies` (`builder_closure.go:872`)
   is not implemented; the forward closure stays as wide as it is today.
7. **W3.5 / `gortex repos`** — repository freshness is read at `view_gen = 0` by construction
   (`read_index_state.go:59-61`) and does not reflect worktree or derived generations.
8. **D8** — W2.1c and W2.4 each invalidate every cached generation once on first deploy. Bundled
   into one deliberate deploy; documented as an upgrade cost.
9. **Gate-3 producer incompletenesses** — clone/similarity symmetry stays
   `ProducerStateIncomplete` for a sparse generation; the closure's ref-fact reverse question is
   ancestry-scoped by W6.10 but the sidecar itself is not generation-inherited.
10. **Measurement honesty** — until W8's paired arms run with preserved artifacts, *any statement
    that the write-amplification problem is fixed is unsupported by the branch's own evidence.*
11. **Gate 8 is unclaimable before W8** — it is partially served by W3.6, W6.3, W6.4, W6.7, W6.9
    and W7.7, but cannot be claimed until W8.4 freezes budgets and W8.12 measures against them.

## Acceptance gates

| ID | Gate | State | Evidence |
| --- | --- | --- | --- |
| G1 | Snapshot correctness — composed/incremental view matches a fresh isolated index of the same target snapshot, configuration and producer set. Equal placeholder or node counts are insufficient. | `proposed` | None end-to-end. Component-level generation-isolation probes are `tested` (W1.1). Open counter-evidence: W1.8. |
| G2 | No-op behavior — after initialization an effective no-op does not extract/resolve again, mutate unchanged payload, or allocate a payload generation from polling/selection alone. | `proposed` | None. Historical replay measurements in the design document are component timings, not this gate. |
| G3 | Context/output separation — read-only context gains no duplicate payload or replacement ownership; semantic changes still invalidate consumers; reuse never drops supported derived output. | `proposed` | None. Route (b) determined (D4); W6.1 unwritten. |
| G4 | Same-branch reuse — repeated dirty edits, undo/redo, committing unchanged content and compatible cached-tree switches reuse valid work. | `proposed` | None. |
| G5 | Advancing main — main publishes new coherent committed generations; ten dependent worktrees update correctly while reusing valid payload; old routes stay available with truthful freshness; no new base spliced into an old delta. | `proposed` | None. D15 determination recorded; W4.8 unwritten. |
| G6 | Atomic publication and authority — adoption fences owner/incarnation, mode/lifecycle, previous active pointer, desired tree and complete build identity including dependency inputs; cached/coalesced completion carries the same guards. | `proposed` | None end-to-end. The steady-dedicated readiness guard is now mutation-pinned (W1.2, MUT-A/MUT-C/MUT-D). |
| G7 | Lifetime and cleanup — readers/workers/physical builds survive until actual completion; closing rejects admission, drains, finalizes only the captured registration; deletion/recreation, path/ID reuse, follower cancellation and delayed callbacks cannot resurrect or damage another owner. | `proposed` | Component-level: W1.3 (Close is terminal, idempotent, late callbacks refused) and W1.4 (two-phase release, exactly-once destructive phases) are `tested` and mutation-verified. No isolated E2E. |
| G8 | Bounded costs — candidate inspection, ancestry, versions, caches, queues, cleanup backlog and retained storage are bounded; no silent truncation, full-view materialization, whole-corpus membership rewrite disguised as reuse, or indefinitely deferred garbage; reseed/compaction peaks measured. | `proposed` | Component-level: W1.5 pins the shared 16384-row budget and the fail-closed overlay refusal (mutation-verified, M1–M7). Two sibling fail-closed guards remain untested (W1.7); the cap's numeric value is unpinned against a raise; the 32-ancestor policy is provisional. Unclaimable before W8. |
| G9 | Storage/recovery safety — shared logical storage stays shared (no database per worktree/generation); supported migration, newer-schema refusal, invalid/missing/foreign generations, crashes and disk-full preserve catalog/tracking integrity with bounded peak space; legacy generation zero is not certified by relabeling. | `proposed` | Component work `implemented` (future-schema refusal, transactional v22→23 migration). No isolated recovery E2E. `internal/persistence` sidecar obligation unowned (B1). |
| G10 | Reproducible release evidence — final-source tests/static checks, isolated public-path E2E and sustained representative I/O comparisons recorded with source identity and commands; all failures and skips explained. | `tested` | **For the W1 evidence only.** The isolated harness exists and is described below; the W1 exit suite ran 9 compiles and 13 selections at a single pinned source identity (HEAD `297f5a44`, dirty-manifest `b341db31…`) with per-run binary sha256, 5344 pass / 1 fail / 4 skip, 0 data races, module-wide `go vet` clean; the one failure and all four skips are named and explained (W1.6, W1.8). `go build ./...` is inconclusive on disk exhaustion and must be re-run. E2E and paired I/O evidence (W8) do not exist, so the gate is **not** `complete`. |

## Validation harness

Path: `.../scratchpad/harness/validate.sh` (bash 3.2), copied alongside the `results/` tree and
the lane reports into `/Users/zzet/code/my/gortex/recovery-incremental-20260910/`.

Actions: `compile <alias|./pkg> <normal|race>` · `test <alias|./pkg> <normal|race> '<run regex>'
[count]` · `vet <alias>` · `build`. Aliases `graph`, `store`, `graphview`, `indexer`, `reconcile`,
`cmd`; any `./internal/…` / `./cmd/…` path also works.

- **Toolchain / proxy.** Every `go` invocation runs from the implementation worktree with
  `GOWORK=off GOTOOLCHAIN=local GOFLAGS="-mod=mod -buildvcs=false" GOPROXY=off` and the shared
  `GOCACHE`/`GOMODCACHE`, never cleared. `GOPROXY=off` resolved everything — no fallback fired on
  any compile (`goproxy_fallback: false` throughout); a fallback path exists and records itself if
  it ever does.
- **Compile discipline.** `test` never rebuilds and refuses to run if the compiled binary is
  missing. Compile again after every relevant source change.
- **Environment.** `test` runs under `env -i` with an explicit allowlist, so no `ANTHROPIC_*` /
  `OPENAI_*` / `AWS_*` / `AZURE_*` / `GEMINI_*` / `DEEPSEEK_*` credential can reach a test.
  Private `HOME` / `TMPDIR` / `XDG_{CONFIG,DATA,CACHE,STATE}_HOME` / `XDG_RUNTIME_DIR` per run
  under `harness/home-<run-id>/`. Git isolated (`GIT_CONFIG_GLOBAL`/`_SYSTEM=/dev/null`,
  `GIT_CONFIG_NOSYSTEM=1`, `GIT_TERMINAL_PROMPT=0`, deterministic identity).
- **CWD** is the package source directory, so source-auditing tests such as
  `TestGenerationCapabilityChecklistIsComplete` see their package.
- **Daemon unreachability.** `GORTEX_AUTOSTART=0` plus
  `GORTEX_DAEMON_SOCKET/PIDFILE/STATEFILE/LOGFILE/SNAPSHOT` pinned into a short private root
  `/private/tmp/gxh-<pid>-<seq>/`. The short root is required —
  `internal/daemon/paths.go:socketAddrMax()` clamps AF_UNIX paths to 104 bytes on darwin and the
  harness prefix is already 113 characters, so a socket under `harness/home-*` would be silently
  redirected. The live daemon socket is unreachable from any harness run.
- **Run knobs.** `GOMAXPROCS=2`, `GOMEMLIMIT=2GiB`, **`-test.timeout 8m` hardcoded
  (`validate.sh:221,253`)**, `-test.v`, `-test.run`, `-test.count`.
- **Output.** `results/<alias>-<flavor>-<pattern-slug>-<seq>/` with `output.log`, `events.jsonl`
  and `result.json` carrying pass/fail/skip and top-level counts, `failures[]`, `skips[]` with
  reasons, duration, exit code, **binary sha256**, **HEAD sha**, **dirty-manifest sha256**, argv,
  env allowlist, cwd, private roots, proxy mode and the run knobs. The wrapper prints named-skip
  and named-failure blocks and a one-line `SUMMARY`, and exits non-zero on any FAIL — a wrapper
  that reported failure for a skip while Go exited zero was the failure mode this design removes,
  so package result, counts and skip reasons are each inspected separately.

## Decisions

1. **The 68-file dirty inventory is preserved as a unit.** It will be split into atomic topic
   commits only after the W1 suites pass. Until then nothing is committed, the snapshot
   `refs/backup/incremental-dirty-resume-20260910` (`b6ddfc96`, tree `001a6214`) is the recovery
   point, and the recovery branch `backup/incremental-before-main-20260910-1310` (`a2ccbe07`) plus
   stash `6e9db642` are retained but **never reapplied** over these files.
2. **The handoff document is a working prompt and is NOT committed.**
   `docs/incremental-indexing-handoff-2026-09-10.md` stays untracked; its durable facts live in
   this ledger. The design document remains the invariant and history reference and links here.
3. **W1 sub-item ids follow execution-plan-v2 §0.** The mapping from the previous ledger numbering
   is recorded in "Correction 3" and must not be re-derived.
4. **`rtk`-wrapped output is not evidence.** `rtk`'s `ls` printed `(empty)` for non-empty
   directories and produced the false "recovery artifacts purged" record. Any command whose output
   is cited in this ledger runs under `rtk proxy` or not at all.

## Evidence log

Append-only. Newest entries at the bottom. Every entry names the date, the source identity, the
command, and the limitation of what it proves.

### 2026-09-10 — Resume state recorded

- Source identity: HEAD `297f5a448a0dda9ecb3ac28678314e7082dae4f3`, rebased onto main `56a1c29d`
  (v0.64.3), 29 commits ahead, branch not on origin. Dirty tree snapshotted at
  `refs/backup/incremental-dirty-resume-20260910` = `b6ddfc96`, tree `001a6214`, 69 entries,
  `+7194/−433` vs HEAD.
- Commands: read-only `git rev-parse`, `git rev-list --count`, `git status --short`.
- Result: no build, test, or benchmark was run.
- **Superseded in part**: this entry recorded the `/private/tmp` recovery artifacts as purged.
  That reading came from an `rtk`-wrapped `ls`; see the 2026-09-10 corrections entry below.
- Limitation: establishes workspace identity only.

### 2026-09-10 — Harness rebuilt; baseline reproduced

- Source identity: HEAD `297f5a44`, dirty manifest sha256
  `9e85a307843cf9800a858d81fc8d47de0394248c722c0b4749c88d2f0c15ec4c` (69 paths).
- Command: `bash validate.sh compile <alias> normal` ×5, `bash validate.sh build`.
- Result: **5/5 packages compile clean**, including the never-compiled
  `generation_lifecycle_test.go` fixture edit. `harness/bin/gortex` built in 17.8 s. Baseline test
  runs reproduced the handoff's predicted failures at `generation_lifecycle_test.go:897`,
  `store_generation_read_test.go:1896` ×3 and `checkout_lifecycle_test.go:361`, and showed the
  journal failure had **moved** to `repository_untrack_retry_test.go:377`.
- Limitation: baseline only; nothing was fixed and no source file was modified.

### 2026-09-10 — W1 fixes (W1.1–W1.5), lanes STORE / INDEXER / GRAPH

- Source identity: HEAD `297f5a44`, dirty-manifest sha256 `b341db31…`.
- Files changed — **all tests, no production**: `store_generation_read_test.go` (+121),
  `generation_lifecycle_test.go` (`:719`, `:762`), `checkout_lifecycle_test.go`,
  `repository_untrack_retry_test.go`, `bounded_incoming_sources_budget_test.go` (new, 250 lines).
- Commands: `bash validate.sh compile <alias> <flavor>`, then `bash validate.sh test <alias>
  <flavor> '<pattern>' <count>`; per-item result dirs are named in each W1 sub-item above.
- Result: every named W1 regression passes. Independent adversarial verification of all three
  lanes returned **PASS** with **0 blockers**, on isolated `git archive` exports (`verify-store`,
  `verify-indexer`, `verify-graph`) with the live worktree provably unwritten (7/7 sha256 match
  for INDEXER; md5-verified restores for STORE and GRAPH). 25+ independent mutants were run across
  the three lanes; every fixed test goes RED under a mutation of the behaviour it claims to pin.
  Residual findings are recorded on their items (W1.2 minor, W1.4 V-1/V-3, W1.5 MINOR-3/4/5) and
  as new item W1.7.
- Limitation: these are **unit and package regressions on a pinned source identity**. They prove
  nothing about production activation, end-to-end correctness, or disk I/O. No production `.go`
  file changed, so no gate moved past `proposed` on the strength of them.

### 2026-09-10 — W1 exit suite

- Source identity: HEAD `297f5a44`, dirty-manifest sha256 `b341db31…`, identical on all 9 compiles
  and all 13 runs — no source drift during the suite.
- Commands: 9 × `bash validate.sh compile`, 13 × `bash validate.sh test`, 5 × `bash validate.sh
  vet`, plus `go vet ./...` and `go build ./...` run from the worktree with the isolated
  environment.
- Result: **5344 pass / 1 fail / 4 skip**, 0 `DATA RACE` occurrences across four race logs, `go
  vet` clean including the whole module. The single failure is
  `TestSparseGenerationClaimsPathlessIdentities` (W1.8), pre-existing and out of every lane's
  scope. All four skips are named with their exact reason strings. `go build ./...` is
  **inconclusive** — exit 1 with zero compile errors, every error a link-stage `errno=28` at
  2.0–2.4 GiB free disk. Full table in W1.6.
- Limitation: `internal/indexer` was covered as five chunk processes, not one, so single-process
  cross-test interference is unobserved. `-race` was run over selections, not whole packages,
  except `internal/graphview`. `go build ./...` must be re-run on a machine with headroom before
  gate 10 can cite it.

### 2026-09-10 — Disk incident during the W1 suite

- Observation: the Go build cache had grown to **64 GB** and the host was down to **2.2 GiB free**
  during the suite, which is what made `go build ./...` inconclusive (link-stage `errno=28`;
  `strip` / `dsymutil` / `ld` all reported `No space left on device`).
- Action: session artifacts were removed and cache entries unused for more than one day were
  trimmed. The ~300 MB of compiled test binaries under the `/private/tmp/gortex-*` recovery
  directories were deleted (reproducible from source; their sha256 values survive in the copied
  `result.json` files). **No shared cache was cleared wholesale and no git state was touched.**
- Result: **17 GiB free** afterwards. Verifier exports now write to fixed slot paths rather than
  freshly-named temp directories, so their build-cache entries are shared across runs instead of
  multiplying.
- Limitation: this is housekeeping, not evidence about the branch. It does, however, invalidate
  the `go build ./...` arm of the W1 suite, which is recorded as "to be re-run" rather than as a
  pass.

### 2026-09-10 — Dirty inventory split into atomic commits

- Precondition: the worktree tree at `297f5a44` built and vetted clean before any commit was made
  — `go build ./...` exit 0 (zero output) and `go vet ./internal/graph/... ./internal/graphview/...
  ./internal/indexer/... ./internal/reconcile/...` exit 0, both from the worktree under the
  isolated environment (`GOWORK=off GOTOOLCHAIN=local GOFLAGS="-mod=mod -buildvcs=false"
  GOPROXY=off`), with 15 GiB free. This closes the "inconclusive `go build ./...`" arm recorded in
  the W1 suite entry above: the failure was the disk incident, not the source.
- Grouping was derived from actual symbol dependencies, not from file names: package order
  `internal/graph` → `internal/graph/store_sqlite` → `internal/graphview` →
  `internal/indexer`/`internal/reconcile` follows `go list` imports, and within each package the
  cut lines follow cross-file references (for example `overlay.go` needs
  `OverlayDetachedNodeReader`, `localization_identity_projection.go` needs it too, and
  `bounded_incoming_sources.go` now delegates to `FindIncomingSourcesScoped`).
- The dedicated-graph ready-state rename is the one deliberately cross-package commit: the catalog
  constant, its two production consumers in `internal/indexer` and `internal/reconcile`, and their
  fixtures land together, because splitting them would leave a history point where a graph the
  reconciler calls ready is rejected by the catalog.

| # | sha | subject | files |
| --- | --- | --- | --- |
| 1 | `b7f6eff0` | graph: allow the dedicated claimed builder to construct a staging graph | 1 |
| 2 | `84a0ad2b` | graph: audit the checkout that supplied the parity test source | 1 |
| 3 | `6c65ff8f` | graph: serve detached node identities through the overlay | 3 |
| 4 | `7d967314` | graph: push identity ownership into the bounded localization readers | 5 |
| 5 | `33bc0820` | graph: bound incoming-source candidate inspection across composed layers | 6 |
| 6 | `a7e6f222` | store: separate node identity-only ownership masks from file masks | 7 |
| 7 | `7dad4460` | store: project node identity summaries for bounded localization | 3 |
| 8 | `b04bc364` | store: read incoming sources through scoped, budgeted candidate pages | 10 |
| 9 | `7e67d4e7` | catalog: name the dedicated graph ready state instead of a bare literal | 11 |
| 10 | `d36573b4` | graphview: compose node-identity ownership into the generation layer | 7 |
| 11 | `a76cd2a9` | graphview: serve scoped incoming-source candidates from the generation layer | 7 |
| 12 | `1c708197` | graphview: admit raw repository owners alongside dedicated ones | 7 |
| 13 | `24755c00` | indexer: pin checkout lifecycle close as permanently terminal | 1 |
| 14 | `b4e3dbde` | indexer: pin untrack retry against the live cleanup repair timer | 1 |
| 15 | `05e8fdad` | indexer: cover repository readiness restored at lifecycle seed | 1 |
| 16 | `6893a89e` | indexer: keep checkout text layer claims out of the file inventory | 1 |
| 17 | `4ad382da` | indexer: pin the composed view's pathless identity union | 1 |
| 18 | `b10ded03` | docs: add the incremental indexing execution ledger | 2 |

- Per-commit isolation: every commit was exported on its own with `git archive <sha> | tar -x` into
  a fixed slot directory and rebuilt there from scratch. All 18 exports returned `go build ./...`
  exit 0 **and** `go vet` exit 0 over the packages that commit touched — so each commit compiles,
  and its test files compile, without any later commit. No reordering or `git reset --soft` was
  needed; the dependency-derived order held on the first pass.
- Final-HEAD suite at `b10ded03` (harness `validate.sh`, `normal` mode, isolated env, one process
  per row; `internal/indexer` again split across the five chunk patterns):

| Package | pass | fail | skip | exit | seconds |
| --- | --- | --- | --- | --- | --- |
| `internal/graph/store_sqlite` | 1757 | 0 | 2 | 0 | 93.79 |
| `internal/graph` | 522 | 0 | 0 | 0 | 4.61 |
| `internal/graphview` | 435 | 0 | 0 | 0 | 4.84 |
| `internal/reconcile` | 92 | 0 | 0 | 0 | 2.81 |
| `internal/indexer` chunk 1 | 527 | 0 | 0 | 0 | 111.89 |
| `internal/indexer` chunk 2 | 502 | 0 | 1 | 0 | 132.96 |
| `internal/indexer` chunk 3 | 482 | 0 | 1 | 0 | 117.53 |
| `internal/indexer` chunk 4 | 510 | 0 | 0 | 0 | 119.37 |
| `internal/indexer` chunk 5 | 532 | 0 | 0 | 0 | 104.22 |
| **Total** | **5359** | **0** | **4** | — | — |

- Named skips, all four with their exact reason strings and none newly introduced:
  `TestBundlePackageKeyNeverUsesOSSeparator` (`bundle_cache_test.go:111`, "separator matches the
  contract on this platform"), `TestMetaBlobCensus` (`meta_census_probe_test.go:17`, needs
  `GORTEX_BENCH_STORE`), `TestBackendBench` (`zzbench_backends_test.go:39`, needs
  `GORTEX_BENCH_ROOT`/`GORTEX_BENCH_BACKEND`), `TestMeasureEditLatency`
  (`editlatency_measure_test.go:26`, needs `GORTEX_MEASURE_REPO`).
- `TestSparseGenerationClaimsPathlessIdentities` — the single failure in the earlier W1 suite —
  passes here, inside chunk 4, as the rewritten positive regression carried by commit `4ad382da`.
- Source identity: the `internal/indexer` normal test binary at final HEAD hashes to
  `9995fdf618f74b85cff701984656a9dd62b1529bba96ea11382f7db9ace9c3a1`, byte-identical to the binary
  the W1 lane recorded before the split, so the commit boundaries moved no indexer source.
- Not committed, by decision: `docs/incremental-indexing-handoff-2026-09-10.md` stays untracked.
- Limitation: per-commit verification is compile + vet, not test execution. An intermediate commit
  can therefore be red — for instance the store's generation capability checklist gains its three
  incoming-source interface rows one commit after the graph package declares the interfaces. Only
  the final HEAD is claimed green.

### 2026-09-10 — Wave 1 exit suite (W2.1a, W2.1b, W3.5, W5.1, W5.8, W1.7)

Runs executed 2026-09-10/11 local time; the entry is dated by the wave, not by the clock.

- Source identity: HEAD `16efbf700ea28d3f588bc13152ea544ff3aeeab5`, dirty-manifest sha256
  `b9e80af941b8f56cee5158f0bda45efae458b07085d746cb2144ccde17889017`, **identical on all 9
  compiles and all 14 runs** — no source drift during the suite. `goproxy_fallback=false` on every
  compile. Every count below carries its binary sha256 inside the run's `result.json`.
- Commands, all from the implementation worktree under the isolated environment
  (`GOWORK=off GOTOOLCHAIN=local GOFLAGS="-mod=mod -buildvcs=false" GOPROXY=off`):
  `rtk proxy go build ./...` (exit 0, zero output) ·
  `rtk proxy go vet ./internal/graph/ ./internal/graphview/ ./internal/indexer/ ./internal/mcp/ ./cmd/gortex/`
  (exit 0, zero output) · 7 × `bash validate.sh vet <alias>` (graph, store, graphview, indexer,
  reconcile, cmd, `./internal/mcp` — all VET OK) · 9 × `bash validate.sh compile` ·
  14 × `bash validate.sh test`.

| # | package | flavor | selection | pass / fail / skip | result dir |
| --- | --- | --- | --- | --- | --- |
| 1 | `internal/graph` | normal | `.` | 530 / 0 / 0 | `results/graph-normal-_-4` |
| 2 | `internal/graphview` | normal | `.` | 447 / 0 / 0 | `results/graphview-normal-_-5` |
| 3 | `internal/reconcile` | normal | `.` | 92 / 0 / 0 | `results/reconcile-normal-_-3` |
| 4 | `internal/graph/store_sqlite` | normal | `.` | 1757 / 0 / **2** | `results/store-normal-_-4` |
| 5 | `internal/indexer` | normal | chunk 1 | 527 / 0 / 0 | `results/indexer-normal-__TestAdmitWalkEntryReportsOversize_TestAffected-4` |
| 6 | `internal/indexer` | normal | chunk 2 | 501 / **1** / **1** | `results/indexer-normal-__TestAffectedBy_CapBoundsFanout_TestAffectedBy_-5` |
| 7 | `internal/indexer` | normal | chunk 3 | 482 / 0 / **1** | `results/indexer-normal-__TestAdmitWalkFileKnownType_EscapingSymlink_Tes-5` |
| 8 | `internal/indexer` | normal | chunk 4 | 510 / 0 / 0 | `results/indexer-normal-__TestAdmitWalkFileKnownType_ExclusionPrecedesSn-6` |
| 9 | `internal/indexer` | normal | chunk 5 | 532 / 0 / 0 | `results/indexer-normal-__TestAdmitWalkEntry_SymlinkEscapeRefusedBeforeS-4` |
| 10 | `internal/indexer` | normal | chunk 6 (new) | 28 / 0 / 0 | `results/indexer-normal-__TestClaimedDedicatedDeltaParentRevisionChangeR-1` |
| 11 | `internal/graph` | race | `Bounded\|Scoped\|Overlay\|Localization\|FailClosed` | 131 / 0 / 0 | `results/graph-race-Bounded_Scoped_Overlay_Localization_FailClosed-1` |
| 12 | `internal/graphview` | race | `Lease\|Handoff\|Materialize\|Drain` | 79 / 0 / 0 | `results/graphview-race-Lease_Handoff_Materialize_Drain-1` |
| 13 | `internal/mcp` | race | `ViewBase\|BaseSelector\|RequestView\|Capabilit` | 169 / **2** / 0 | `results/internal_mcp-race-ViewBase_BaseSelector_RequestView_Capabilit-1` |
| 14 | `internal/indexer` | race | `DependencyRevision\|DedicatedBase\|Dedicated\|TextSearch\|SearchText\|Rehome\|CheckoutMutation` | 165 / **1** / 0 | `results/indexer-race-DependencyRevision_DedicatedBase_Dedicated_TextS-1` |

- `internal/indexer` normal totals across the six chunks: **2580 pass / 1 fail / 2 skip**. Grand
  total runs 1–14: **5950 pass / 4 fail / 4 skip** (the indexer failure is one test counted once in
  its normal chunk and once in the race lane, so **3 distinct failing tests**). `DATA RACE`
  occurrences: **0** across all four race logs.
- Chunking is mandatory and now covers **six** files: `validate.sh` hardcodes `-test.timeout 8m`
  and `internal/indexer` exceeds it in a single process. The five duration-bin-packed regex files
  `scratchpad/chunk1.pat … chunk5.pat` were re-verified to be exactly the 1728 test names they were
  packed from; this wave added 20 new `func Test…` names to the package (1728 → **1748**), so
  `scratchpad/chunk6.pat` was written to carry exactly those 20. The six-file union was verified
  programmatically against the package's current 1748 names — no name missing, no name run twice.
- Named skips (4, none introduced by any wave item, all with their exact reason strings):
  - `TestBundlePackageKeyNeverUsesOSSeparator` — `bundle_cache_test.go:111`, *"separator matches
    the contract on this platform"*; vacuous on darwin by construction. One of the two skips the
    exit criterion names.
  - `TestMetaBlobCensus` — `meta_census_probe_test.go:17`, *"set GORTEX_BENCH_STORE to a copied
    store.sqlite to run"*. The other skip the exit criterion names.
  - `TestBackendBench` — `zzbench_backends_test.go:39`, *"bench harness; set GORTEX_BENCH_ROOT and
    GORTEX_BENCH_BACKEND"*; environment-gated, pre-existing. **Outside the two skips the exit
    criterion allowed — recorded, not waived.**
  - `TestMeasureEditLatency` — `editlatency_measure_test.go:26`, *"set GORTEX_MEASURE_REPO=/abs/path
    to run"*; environment-gated, pre-existing. **Same status.**
- Named failures (3 distinct), each attributed against a pristine `git archive HEAD` export of
  `16efbf70` extracted to `/private/tmp/gxbase-mcp` and compiled and run there under a private
  `HOME`/`XDG`/`TMPDIR`:
  1. `TestDependencyRevisionClaimedFullAndDeltaOutput` —
     `internal/indexer/builder_dependency_revision_test.go:133`,
     `err=invalid dedicated base candidate: dedicated delta parent dependency revision differs:
     parent generation 1`. **PASSES on the pristine export** (`--- PASS … (1.14s)`, exit 0), so this
     is a real regression **attributed to W2.1b**'s builder guard
     (`internal/indexer/builder_dedicated_delta.go:105-106`). The pre-existing test builds a delta
     whose parent carries `cohort-v1:a` under an output identity carrying `cohort-v1:b` — the
     composition D2 forbids — so it encodes the pre-D2 contract. The file is outside every wave
     item's ownership and was **not** repaired here.
  2. `TestRefViewPrunedObjectWithdrawsTheSourceCapability` — `internal/mcp/view_ref_test.go:559`,
     *"no ref view generation was published: []"*.
  3. `TestSearchTextRefusalIsTheCapabilityEvaluationsRefusal` —
     `internal/mcp/view_search_text_test.go:94`, *"error \"checkout_inaccessible: … repository owner
     is not registered\" does not carry \"capability_unavailable\""*.
  Failures 2 and 3 reproduce **byte-identically at the same file:line on the pristine export**
  (24 top-level pass / 2 fail on the same selection), and both are members of the 19-test
  branch-base failing set the W5.8 verifier independently established with an empty symmetric
  difference in both directions. They are **pre-existing at `16efbf70`** and attributable to no
  wave item.
- `all_green = false`, solely because of failure 1 (a genuine W2.1b regression) and failures 2–3
  (pre-existing at the branch base), and strictly because two of the four skips fall outside the
  two the exit criterion named.
- **Nothing was committed by this wave.** The commit gate is `all_green` plus a passing verifier
  verdict per item; `all_green` is false, so all six items stay dirty and reversible. The five
  verifier verdicts and W1.7's are each **PASS**, so every item is commit-ready on its own merits
  once failure 1 is settled — either by updating
  `internal/indexer/builder_dependency_revision_test.go` to the D2 contract, or by W6.9 degrading
  the refusal to a full-root verdict as the W2.1b verifier recommends.
- Limitations of what this suite proves: `internal/indexer` was covered as six chunk processes, not
  one, so single-process cross-test interference is unobserved. `-race` was run over selections,
  not whole packages. `internal/mcp` was **not** run as a whole package here — only the mandated
  selection — so the branch-base failing set is cited from the W5.8 verifier's run, not
  re-measured. No end-to-end or paired-I/O evidence exists; gates G1–G9 are unchanged by this wave.

### 2026-09-10 — Wave W1x exit suite (W2.1b, W5.8, W1.9; carried W2.1a, W1.7, W5.1, W3.5)

- Source identity of every run below, read from the harness `result.json` / `meta.json` files:
  `head_sha = 16efbf700ea28d3f588bc13152ea544ff3aeeab5`,
  `dirty_manifest_sha256 = 45a7c9bad1578afaf069687e8a380b6effd352c7be4f313bc80ec00e674769d1`,
  `goproxy = off`, `goproxy_fallback = false` on all ten compiles, `GOMAXPROCS=2`,
  `GOMEMLIMIT=2GiB`, `-test.timeout 8m`, `-test.count 1`, go1.27.0 darwin/arm64. The seven commits
  recorded below were created **after** the last run, from exactly that tree, so the committed
  content is the validated content.
- Commands, all from the implementation worktree under the isolated environment
  (`GOWORK=off GOTOOLCHAIN=local GOFLAGS="-mod=mod -buildvcs=false" GOPROXY=off`):
  `rtk proxy go build ./...` → exit 0 ·
  `rtk proxy go vet ./internal/indexer/ ./internal/mcp/ ./internal/graph/ ./internal/graphview/
  ./internal/graph/store_sqlite/ ./internal/reconcile/ ./cmd/gortex/` → exit 0 ·
  10 × `bash validate.sh compile` (six normal, four race) — all COMPILE OK ·
  13 × `bash validate.sh test`.

| # | package | flavor | selection | pass / fail / skip | result dir |
| --- | --- | --- | --- | --- | --- |
| 1 | `internal/graph` | normal | `.` | 530 / 0 / 0 | `results/graph-normal-_-5` |
| 2 | `internal/graphview` | normal | `.` | 447 / 0 / 0 | `results/graphview-normal-_-6` |
| 3 | `internal/reconcile` | normal | `.` | 92 / 0 / 0 | `results/reconcile-normal-_-4` |
| 4 | `internal/graph/store_sqlite` | normal | `.` | 1757 / 0 / **2** | `results/store-normal-_-5` |
| 5 | `internal/mcp` | normal | `.` | 5926 / **2** / **8** | `results/internal_mcp-normal-_-6` |
| 6 | `internal/indexer` | normal | `^Test[A-C]` | 522 / 0 / **1** | `results/indexer-normal-_Test_A_C_-1` |
| 7 | `internal/indexer` | normal | `^Test[D-H]` | 526 / 0 / 0 | `results/indexer-normal-_Test_D_H_-1` |
| 8 | `internal/indexer` | normal | `^Test[I-M]` | 463 / 0 / **1** | `results/indexer-normal-_Test_I_M_-1` |
| 9 | `internal/indexer` | normal | `^Test[N-R]` | 603 / 0 / 0 | `results/indexer-normal-_Test_N_R_-1` |
| 10 | `internal/indexer` | normal | `^Test[S-T]` | 330 / 0 / 0 | `results/indexer-normal-_Test_S_T_-1` |
| 11 | `internal/indexer` | normal | `^Test[U-Z]` | 138 / 0 / 0 | `results/indexer-normal-_Test_U_Z_-1` |
| 12 | `internal/graph` | race | `Bounded\|Scoped\|Overlay\|Localization\|FailClosed` | 131 / 0 / 0 | `results/graph-race-Bounded_Scoped_Overlay_Localization_FailClosed-2` |
| 13 | `internal/graphview` | race | `Lease\|Handoff\|Materialize\|Drain` | 79 / 0 / 0 | `results/graphview-race-Lease_Handoff_Materialize_Drain-2` |
| 14 | `internal/indexer` | race | `DependencyRevision\|DedicatedBase\|Dedicated\|TextSearch\|SearchText\|Rehome\|CheckoutMutation` | 167 / 0 / 0 | `results/indexer-race-DependencyRevision_DedicatedBase_Dedicated_TextS-2` |
| 15 | `internal/mcp` | race | `ViewBase\|BaseSelector\|RequestView\|Capabilit\|RefView\|SearchText` | 238 / 0 / 0 | `results/internal_mcp-race-ViewBase_BaseSelector_RequestView_Capabilit_RefV-1` |

  Counts are every test event the harness parsed, subtests included. `grep -c "DATA RACE"` on runs
  12–15 is **0** in each.
- **Chunking.** `internal/indexer` as one process **exceeded the 8-minute cap**: the whole-package
  run reached 1589 passes in 480.4 s and was killed by `panic: test timed out after 8m0s` while
  entering `TestReconcileContractEdges_TopicEdges_KafkaPair`
  (`results/indexer-normal-_-4`, exit 2, **0 failures before the timeout**). It was therefore split
  into the six disjoint, exhaustive name-range chunks 6–11 above (2582 passes total), which is the
  same device the wave 1 exit suite used. The split is by first letter after `Test`, so it covers
  the binary's whole `-test.list '.*'` set; `Benchmark*` entries are outside every chunk by
  construction and are not run. Cost: single-process cross-test interference across a chunk
  boundary is unobserved.
- **Named skips (12 across the suite), every one with its reason.**
  - `TestBundlePackageKeyNeverUsesOSSeparator` — `bundle_cache_test.go:111`, *"separator matches the
    contract on this platform"*. Windows path-separator test; one of the two the exit criterion
    names.
  - `TestMetaBlobCensus` — `meta_census_probe_test.go:17`, *"set GORTEX_BENCH_STORE to a copied
    store.sqlite to run"*. The copied-store census; the other skip the exit criterion names.
  - `TestBackendBench` — `zzbench_backends_test.go:39`, *"bench harness; set GORTEX_BENCH_ROOT and
    GORTEX_BENCH_BACKEND"*; environment-gated, pre-existing, recorded not waived.
  - `TestMeasureEditLatency` — `editlatency_measure_test.go:26`, *"set GORTEX_MEASURE_REPO=/abs/path
    to run"*; environment-gated, pre-existing, recorded not waived.
  - `internal/mcp`, 8 skips, all pre-existing and none added by this wave: five subtests of
    `TestAnalyzeScope_AllScopeAwareKinds_NoCrossWorkspaceLeak` (`analyze_scope_test.go:660` —
    `coverage_gaps` / `coverage_summary` need a coverage profile, `ownership` / `stale_code` /
    `stale_flags` need git-blame data; each *"cannot fixture in-memory; emits empty, never leaks"*);
    `TestATradeThatCannotSaveTheOutlineIsGivenBack` (`localization_file_outline_test.go:857`, *"the
    fixture is no longer tight enough to drop the index"*);
    `TestLocalizationTextMatchNormalisesNativePathToGraphKey`
    (`localization_text_index_test.go:120`, *"a native path differs from the graph spelling only on
    Windows"*); `TestCheckoutMutationResolvedRootRejectsFoldedDistinctDirectory`
    (`view_mutation_state_test.go:175`, *"filesystem cannot represent case-distinct directories"*).
- **Named failures (2, both in `internal/mcp`, both UPSTREAM and attributable to no wave item).**
  1. `TestReadFilePhysicalEvidenceRejectsSpecialFiles` —
     `read_file_physical_evidence_nonblocking_test.go:61`,
     *"listen unix …/home-internal_mcp-normal-_-6-57951/tmp/gxev…/evidence.sock: bind: invalid
     argument"*.
  2. `TestRetrievalSavingsCreditIsPerSession` — `savings_retrieval_test.go:178`,
     *"\"0\" is not greater than \"0\""*.
  Root cause, reproduced directly in this suite and not merely inherited: both tests root their
  fixture at `t.TempDir()` / `os.MkdirTemp("")`, which honours `TMPDIR`; the harness sets
  `TMPDIR=$HARNESS_DIR/home-<runid>/tmp` (122+ chars) and darwin caps AF_UNIX `sun_path` at 104
  bytes. Re-running the **same binary** from the **same tree** under the full harness env with only
  `HOME`/`TMPDIR` shortened to `/private/tmp/gxprobe-77082` gives **PASS** for both; re-running with
  a 125-char `TMPDIR` and no `GORTEX_*` variables at all gives **FAIL** for the second, so `TMPDIR`
  length alone is the trigger. Both fail byte-identically on a pristine `git archive 56a1c29d`
  export of **main** (W1.9 §3 run 2, W1.9-verify §1), so they are neither branch nor wave defects,
  and W1.9's supplementary whole-package run under a short `TMPDIR` is green — 3407 top-level PASS
  / 0 FAIL / 3 SKIP. Neither file is owned or touched by any item of this wave.
- **The wave 1 exit suite's one attributed regression is closed.**
  `TestDependencyRevisionClaimedFullAndDeltaOutput` is green in run 7; the W2.1b follow-up rewrote
  it to the D2 contract and degraded the planner's wedge to a full-root verdict. The wave 1 suite
  also recorded 2 failures in the `internal/mcp` race lane and 18 in the package at the branch
  base; run 15 is **238 / 0 / 0** and run 5 leaves only the two upstream failures above, i.e. W1.9
  closed 16 branch regressions.
- `all_green = true` **for this wave**, on this reading and no wider one: every run is green except
  the two upstream failures characterized above, which fail identically on `main`, are caused by
  the harness's own path length rather than by any code, and touch no file any item owns. Both are
  recorded, not waived. Every skip is named with its exact reason; the two beyond the pair the exit
  criterion pre-blessed (`TestBackendBench`, `TestMeasureEditLatency`) are environment-gated and
  pre-existing, and the eight in `internal/mcp` are pre-existing subtest/platform gates.
- **Commits.** Seven, one per item, in dependency order, each staging only that item's files
  (`git add <paths>`, never `-A`); the ledger commit is eighth and last. Nothing outside this
  wave's ownership was staged, and `git status --porcelain` after the run shows only this ledger
  and the untracked handoff working note.

  | # | commit | item | subject |
  | --- | --- | --- | --- |
  | 1 | `f2fa834693710a19c4027285b0b0450037b21d6d` | W2.1a | indexer: derive the dependency revision from the frozen input cohort |
  | 2 | `141dc997f2c1f5fd5b495895da8d7dbdc3eb42ad` | W1.7 | graph: pin the fail-closed guards in the bounded incoming-source readers |
  | 3 | `be4ae174878a6e2bd1faf1b55c908e7a37c91e5c` | W5.1 | graphview: let a lease serve a joined consumer for its whole lifetime |
  | 4 | `679a5707845ef8e0a9c5f33aab2ee440dea0624b` | W3.5 | indexer: stop claiming search_text for identities that do not describe bytes |
  | 5 | `465601252678daae333f88424203fc7b79711e7f` | W2.1b | indexer: root a new dedicated base when the dependency revision changes |
  | 6 | `d402aecf2bc95b8554acd6e25e1c564331e646ed` | W5.8 | mcp: narrow the reader a base-selected view actually serves |
  | 7 | `0d8ccf2f3351eaaf89ddfb681379ba9a1c3677b5` | W1.9 | mcp: admit the repository owner in the ref-view fixtures |

- **Limitations of what this suite proves.** `internal/indexer` was covered as six chunk processes,
  not one, so cross-chunk single-process interference is unobserved — and the one whole-package
  attempt was killed by the time cap, so no single-process whole-package result exists for that
  package at all. `-race` was run over the mandated selections, not whole packages. `internal/mcp`
  and `internal/graph/store_sqlite` were run whole, in one process each. No item advanced past
  `tested` or `wired`: there is still **no end-to-end and no paired-I/O evidence**, and gates
  G1–G9 are unchanged by this wave. The commits make the work durable and reviewable; they do not
  make it activated. `W2.1a` and `W5.1` remain inert primitives with no production caller, and
  `W2.1b`'s two revision comparisons are still `"" != ""` in production.
