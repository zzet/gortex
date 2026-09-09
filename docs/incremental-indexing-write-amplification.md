# Incremental indexing and worktree write amplification

Status: implementation in progress. This document separates observed behavior,
required invariants, implementation stages, and measured results. A passing
microbenchmark is not an end-to-end disk-usage verdict.

## Problem

A small source edit can trigger work at several different scales:

1. Git/worktree observation can treat metadata or staging changes as content
   changes, and repeatedly read unchanged dirty bytes.
2. A file-level delta can expand into a much larger semantic closure. Current
   import placement can fall back to a shared directory basename instead of an
   exact Go module/package identity.
3. Incremental resolution can admit broad unresolved-name buckets across
   repositories before it rejects unreachable candidates. Its preparation cost
   is paid even when almost no references resolve.
4. A generation-scoped SQL lookup can scan rows from unrelated generations when
   its predicate does not expose the existing partial index to the planner.
5. Generation builds can repeat extraction, resolution, materialized payload
   writes, FTS work, and other derived passes for unchanged context.
6. The dedicated base must represent the committed tree it advertises. Mutable
   generation zero cannot be made immutable merely by labeling it with a HEAD
   tree or advancing an epoch.

The byte delta, admitted file delta, semantic context, persisted payload changes,
and SQLite/WAL activity are different quantities. They must be measured and
reported separately. A WAL file's current size is not cumulative writes, and
process write counters are not SSD NAND-write measurements.

### Direct evidence collected during this implementation

- A five-second CPU profile of an in-progress dirty generation attributed
  61.19% of sampled CPU to GetRepoNodeSummariesByLanguage and its read path.
  Another concurrent graph query was present, so the sample is not an exclusive
  full-build attribution.
- A separate five-second parse-stage profile attributed 97.28% of sampled CPU
  to AddBatch/addBatchSetOriented through parseGraphBatch.flush. Pwrite accounted
  for 65.76% of sampled CPU. This measures sampled CPU/syscalls, not write bytes.
- One 203-file parse reported 52.346 seconds of wall time, 2.905 seconds of
  accumulated extraction-worker time, and 376.125 seconds of accumulated batch
  worker time. Worker totals overlap; they must not be added to wall time.
- Repeated small mutations reached the 200-file closure cap.
- A later 17-file resolver pass admitted 5,088 outgoing and 32,682 incoming
  pending references across 18 repositories, taking 69.52 seconds. Only two
  incoming references resolved. Concurrent agents were active; this log is not
  attributed exclusively to one worktree.
- The main incremental collector visits every referenceable named definition
  in the supplied files, expands unresolved-name keys (including wildcard member
  forms), and admits all incoming edges in those buckets without an early
  repository/import/dependency predicate. The skip callback applies only to
  outgoing edges. Preparation reads lightweight edge identities; reverse
  resolution later rereads full buckets. Filtering before forward import
  resolution requires special care because import reachability can change.
- The current collector uses one file set for outgoing work and incoming name
  seeds. A safe split must retain a separate exact-restub dependency frontier;
  otherwise a structurally reparsed but semantically equivalent file can leave
  formerly resolved edges unresolved. DefinitionFiles precision and whether
  incoming target coordinates require refresh remain to be verified before
  connecting an optimized seed set.

## Required semantics

### Immutable generations, advancing main graph

Immutable does not mean that the main graph stops advancing. A published
generation is a fixed committed snapshot. The designated primary's active
pointer advances to a newly ready coherent snapshot after a guarded adoption.

Primary uncommitted changes live above that committed base. Typing in the primary
working copy must not mutate the committed lower layer or force every sibling
worktree to rebuild against dirty primary contents.

When committed main advances, dependent worktrees must eventually be recomposed
against the new base. Until their new views are ready, retain old coherent routes
with truthful freshness. Do not splice a new base into an old delta. A correct
rebase may change a delta's representation while leaving its effective source
and reusable semantic payload unchanged.

An implicit worktree stays an automatically discovered overlay. No development
or recovery procedure may explicitly track it merely to obtain tool access.

### Content and observation identities

Keep source identity separate from diagnostic observations:

- Tree content, path, entry type, mode, existence, and bytes affect source
  identity.
- Commit identity is provenance when the tree is unchanged.
- Staging state, an unchanged mtime touch, and same-tree amend must not by
  themselves imply new source content.
- A content cache stores bounded digests/identity records, not whole file bytes.
- A failed or canceled sample must not publish a new cache.

Metadata-based reuse is not an absolute atomic-filesystem snapshot guarantee.
The planned local-filesystem cache requires descriptor-level identity checks,
a conservative quiet-time eligibility rule, clock-discontinuity handling,
root/path replacement fences, and immediate rehash on uncertain evidence.
Unsupported filesystems and recent/unstable writes must not wait for eligibility;
they rehash. Long-lived mmap writers and similar timestamp limitations must be
documented rather than silently represented as stronger exactness guarantees.

### Metadata-only graph updates

Presentation changes can alter node/edge locations while preserving semantics.
Pair edges by owned source/kind/alias and unique resolved target, not ordinal
position after sorting by line and target.

Preserve group cardinality and existing resolver provenance. Duplicate
indistinguishable identities, missing targets, added targets, or unsupported
ownership cases use the structural fallback. Foreign-file rows must neither
participate in owned-file cardinality checks nor be accidentally rewritten.

A real pipeline test must assert that the metadata-only path was used.
Changing a function body can legitimately change body/clone hashes; a helper-only
test or a structurally reindexed file does not prove the metadata fast path.

### Context versus output

Go-only exact import aliases must not contaminate the shared physical-directory index. Record actual extraction-language provenance: Go-only imports may narrow, mixed Go/non-Go origins must union the Go candidates with legacy candidates, and unknown/non-Go origins stay conservative without probing Go manifests. Validate module identities before joining paths. Module-manifest-only subtree invalidation is a separate requirement; exact alias placement alone does not solve it.

Changed output and read-only semantic context are distinct. Do not omit required
resolution context to make a benchmark faster. Conversely, do not persist an
unchanged context file merely because it was needed to resolve changed output.

Concrete incoming dependencies and restub identities must survive admission.
Name equality alone is a candidate, not a proven dependency. Early rejection
requires positive evidence of non-reachability; missing metadata and dynamic
language cases remain conservative.

Verified downstream constraints prevent a simplistic file-list optimization:

- CrossRepoResolver's current definitionFiles parameter controls materialized
  cross-repository edges, not incoming resolution admission.
- The watcher path widens resolution files with derived/context/provider files
  and still calls the broad file resolver.
- SQLite intentionally excludes edges with restub provenance from UnresolvedFiles.
  Putting them into ordinary forward resolution would consume the stub before
  the incoming pass can restore its previous provenance tier.
- Existing receipts do not expose a complete exact incoming-edge/provenance
  frontier. Added-node identity comparison alone is not semantic equivalence.
  Producer dispatch and in-memory-store parity must be verified before treating
  an empty definition set as proof that global name reseeding is unnecessary.
- Provider work happens after the parse/evict receipt closes. Capture its own
  bounded mutation evidence or retain conservative handling; do not extend the
  original receipt across resolver writes and recursively enlarge the frontier.

### Publication, ownership, and retirement

Building/sealing a generation is separate from making it active. Adoption must
check owner checkout/incarnation, lifecycle/mode, previous active pointer,
desired target tree, and the complete current build fingerprint.

Current catalog findings:

- UpsertDedicatedGraph overwrites active_generation_id without CAS.
- DedicatedGraph has no general desired-tree/build-fingerprint/publication epoch.
- Checkout has an incarnation and last observed HEAD, but no config/publication
  fingerprint. Observation updates compare only checkout ID and incarnation;
  they do not prevent an older observation from overwriting a newer one.
- The family primary_epoch governs changing which graph is primary. It must not
  be repurposed as a base-content publication counter.
- Ref-view adoption provides a useful transactional pattern.
- Database retirement already guards active pointers, routes, ref views and
  direct descendant generation references. Runtime reader-pin acquisition and
  ancestry materialization still need independent verification.

Use a dedicated desired-build protocol or a verified equivalent fence. Comparing
a candidate fingerprint only with the original request does not prove that the
current configuration still wants it. Cached/coalesced results require adoption
guards too; a callback executed only during physical building is insufficient.

A committed snapshot build must not obtain current dirty working-copy bytes through a resolver or enrichment side channel. Providers need a verified snapshot-aware input contract or must be explicitly unavailable for that immutable view. Primary working-copy enrichment must remain truthfully scoped above the committed base.

Do not immediately delete a previous base after advancing the active pointer.
Old coherent routes, descendants and pinned readers may still require it.
Generation ancestry is not useful unless every base consumer reads the composed
ancestry rather than only the newest physical generation.

### Storage and migration safety

Use shared logical storage, not a database per graph/worktree/generation.
A future-schema database must be refused before opening a writer or running
writer PRAGMAs, even when rebuilding older schemas is allowed.

Refusal preserves logical graph data and database contents. This is not forensic
cleanup or a promise that WAL shared-memory reader bookkeeping never changes.
Protection requires exclusive ownership across the preflight-to-writer interval.
Already released older binaries cannot be retroactively made safe.

Legacy mutable generation zero is not a trusted committed snapshot. A migration
must reconstruct committed content or explicitly retain a labeled legacy/fallback
state. Do not declare a positive pointer valid when its generation is missing,
owned by another graph, unservable, or lacks a tree identity.

## Staged implementation

Each fix is separately committed, with targeted regressions, a benchmark or
measured operational check, and relevant broader validation.

1. Add regression baselines and safety guards. Protect newer schemas, preserve
   actionable startup failures, release locks on failed initialization, repair
   metadata pairing, and separate dirty content identity from observations.
2. Enforce truthful base identities and design guarded committed-base adoption.
   Cover stale owners/observations, HEAD/config changes, ABA, cache/coalescing,
   failed publication, removal and recreation.
3. Land measured no-DDL amplification fixes. Scope generation queries, resolve
   exact module/package placement, bound safe manifest reads, and narrow semantic
   work only where correctness evidence permits.
4. Remeasure before selecting a broad shared-payload/storage redesign. Prototype
   reuse and context/output separation with real data. Do not assume a new table
   layout alone removes extraction, resolver, FTS, or checkpoint costs.
5. Implement the actual committed base and primary working overlay, incremental
   advancement/rebase, bounded ancestry/compaction and safe retirement. Never
   satisfy this step by freezing the initial base forever or making a full
   duplicate corpus on every edit.
6. Run isolated cold/warm, worktree lifecycle, branch-switch, concurrency,
   migration and sustained-I/O verification before opening the PR.

## Validation matrix

Compare optimized views with a clean target index, including source text, nodes,
resolved edge targets and locations, deletion masks and search behavior.

Required workloads include:

- Idle clean and dirty checkouts; repeated samples; touch, stage/unstage and
  same-tree amend; same-size writes with restored mtime; atomic replacement.
- Comment/presentation edits, body edits, signature/export/import changes,
  deletion, rename, mode changes, symlinks and non-ignored untracked files.
- New implicit worktree: discover → search/read → edit → fresh read.
- Primary dirty edits versus committed main advancement, with multiple sibling
  worktrees; rapid HEAD changes and stale build completion.
- Same-named packages in unrelated modules and real imported cross-repository
  consumers; nested, removed, invalid, oversized and unreadable go.mod files.
- Missing optional source capabilities, path confinement, FIFO/symlink races,
  Git advertised/header size bounds, cancellation and recovery.
- Migration from supported old schemas; refusal of newer schemas with and
  without rebuild permission; failed startup lock release.
- Full restart of the isolated test daemon only; no restart/config/store changes
  to the user's live daemon.

For each workload record admitted/changed/context files, parsed bytes and time,
resolution candidates and outcomes, materialized node/edge row mutations, FTS and
derived work, generation publications/reuse, retained store growth, WAL/checkpoint
activity, and process writes where available. Fixture SQL triggers may measure
payload row mutations in an untimed probe; they are not total SQLite writes.

## Results so far

### Committed: future-schema protection

Commit d11dab97. No schema bump or DDL change.

The store refuses a future version before writer initialization and rechecks at
the writer-side migration plan. Existing downgrade/rebuild test expectations
were updated to require refusal and retained data.

Full SQLite-store suite passed; new regressions passed repeated race tests;
vet, formatting and diff checks passed. Native graph contract checks timed out,
so this is not a claim of complete graph-certified coverage.

Future-schema fixture median refusal changed from about 0.990 ms to 0.143 ms
(default) and from destructive rebuild at 28.942 ms to refusal at 0.120 ms
(WithRebuild). The supported V18 migration benchmark showed a small variable
opening overhead; this is not a production I/O-saving claim.

### Committed: generation-scoped node summaries

Commit 1b9ccc54. Exposes view_gen > 0 in the positive-generation query so SQLite
can use the existing nodes_by_generation partial index. No INDEXED BY or DDL.

Actual modernc query plans are tested before and after ANALYZE; correctness also
holds without that index. Fixtures cover generation zero, overlapping identities,
sibling generations, repository/language selection and omitted large metadata.

Standalone benchmark, GOMAXPROCS=4, 100 iterations, three runs, medians:

| Base fixture nodes | Selected nodes | Production | Legacy SQL |
| --- | --- | --- | --- |
| 1,000 | 10 | 70.0 µs | 4.83 ms |
| 20,000 | 10 | 75.6 µs | 30.73 ms |

The full store package passed in 59.015 seconds with these regressions included.
Selected tests passed race detection three times. These are query-level results,
not end-to-end disk or startup numbers.

### Committed: startup failure handling

Commit 15d4c646.

The new backend regressions first reproduced misleading daemon restart advice
for a future schema and a retained lock after failed initialization. Production
error handling now preserves the typed newer-schema refusal and releases
constructor resources on backend-open failure.

The full server-stack package passed in 2.623 seconds on the final commit candidate. Both new regressions
passed race detection three times (1.669 seconds); vet, gofmt and diff checks
passed. A three-run, 100-iteration refusal benchmark measured 84.6–211.2 µs/op
(median 87.5 µs) versus the 152.5 µs baseline median. This small startup fixture
is variable and not evidence of reduced sustained indexing I/O.

### Committed: metadata-only edge pairing

Commit f3765cac.

Point and batch refresh use one owned-file matcher, preserving group cardinality
and pairing unique resolved targets. Ambiguous duplicates fall back to structural
indexing. The regression first demonstrated wrong target locations and incorrect
fast-path acceptance. Real pipeline tests assert metadata-only admission rather
than merely exercising a helper.

The selected 37 top-level tests passed in 1.543 seconds, with repeated race runs
passing in 24.989 seconds. Presentation-only timing stayed within the measured
range (before 2.828–3.046 ms; after 2.704–3.130 ms). Ambiguous cases correctly
became more expensive because they now take structural fallback. Untimed SQLite
trigger probes measure only node/edge row events, not total writes or NAND wear.

### Validated private draft: module-aware import placement

A public production-flow regression fails against the original builder because
it carries an unrelated same-named directory. The draft excludes that directory
and matches a clean index's calls to the intended definition, not merely equal
unresolved placeholders. Module tests, the broader closure suite and three race
runs pass. Implementation has not yet landed.

Review found that an exact nested-module alias could suppress already-admitted
vendored packages. A reproducing regression now passes after unioning known
vendor/<import> candidates with an existing declared alias. This does not add
vendor-only narrowing or change physical-directory precedence. Module race tests
passed three times after the correction (22.676 seconds).

The combined private Go overlay with the source capability, module placement and
dirty sampler passed the full internal/indexer package (437.561 seconds,
-count1 -timeout15m, no skips). This is useful integration evidence, not a final
landed-source result or isolated daemon end-to-end validation. That run predates
the stronger adversarial same-symbol target-resolution failures described below;
it does not certify correct import/call targets in those cases.

In a sequential 200-directory placement fixture, candidates fall from 200 to 1.
Cold lookup preparation increases from 28.240 to 282.705 microseconds (median):
it performs 402 bounded manifest probes to establish ownership. Warm lookups
reuse that index with no additional manifest probes. These probes use a fake
in-memory capability; they are not measured filesystem/Git reads. The intended
saving is downstream extraction/resolution/persistence, not lookup latency.
After the vendor correction, the same lookup benchmark measured 287.255 µs cold
and 87.08 ns warm medians, with unchanged candidate/probe/allocation counts.

A separate real generation-build probe used 16 and 64 same-named package
directories and a single changed import. Both old and new variants matched
actual resolved calls to a clean target. At 64 directories, closure files fell
65 → 2, reported source bytes 2,288 → 155, materialized nodes 132 → 6, edges
68 → 5, and replace masks 66 → 3. At 16 directories the corresponding counts
were 17 → 2, 656 → 155, 36 → 6, 20 → 5, and 18 → 3. No closure cap truncation
occurred.

Build-report Duration medians at 64 directories were 89.035 → 62.521 ms, but the
new samples ranged 61.486–217.220 ms under live-host contention. Whole fixture time,
including clean base and target setup, did not improve materially. These are
generation payload counts, not total SQL mutations or measured WAL/process/NAND
bytes. Sustained end-to-end write savings remain unmeasured.

### Committed: dirty content sampler

Commit 8fa2fac4.

The content.v2 fingerprint separates source content from observation metadata.
It includes tree, canonical path, entry type/mode/existence, and Git blob hashes
of admitted working-copy bytes. A bounded per-sampler digest cache stores at
most 4,096 identity/digest records, not source bytes. Failed or canceled samples
do not replace the previous cache; a clean sample clears it. The new fingerprint
version intentionally invalidates earlier dirty identities once on upgrade;
that is distinct from recurring rebuilds of unchanged content.

Descriptor identity, root/path replacement checks, a two-second quiet window,
and wall/monotonic clock checks govern reuse on supported local filesystems.
Recent or uncertain writes are rehashed without waiting. Unsupported platforms
or filesystems do not obtain trusted metadata-only cache hits. These safeguards
are not a claim of atomic filesystem snapshots or detection of every mmap write.

All sampler production and regression files are now on disk. The full gitstate
package passed on the final corrected source without overlays or skips: normal
tests 16.121 seconds, race tests 20.299 seconds, and scoped lint zero issues.
Windows/amd64 and Linux/amd64 test binaries also compile; that is not platform
runtime execution. Final native graph certification is not yet available.

Correctness has a sampling cost: a tracked-file benchmark median increased from
8.569 to 17.135 ms, using two Git commands instead of one. Untracked-file fixtures
similarly grew from roughly 8.5–9.0 to 17.9–19.6 ms. These host timings may overlap
other work. Stable sampler-cache hits read zero file-content bytes themselves,
but Git subprocess reads are excluded from that accounting. The intended saving
is fewer unnecessary generation builds, not faster individual Git observations;
end-to-end publication/write savings remain to be measured.

### Dirty-source admission boundary to validate end to end

The new raw content fingerprint covers Git-status-reported paths. Git clean/EOL
filters can hide raw checkout differences after normalization/staging. This is
an inherited admission boundary and a filter-driven exception to a global
staging-invariance claim. Document and test the precise clean-normalized case;
do not claim global raw-byte identity or solve it with persistent-cache-only
residue that would disagree with fresh SampleDirty confirmation. The final
base/working-view validation must explicitly cover this case.

### Adversarial Go target resolution remains open

A stronger exact-import fixture gives both an imported package and an unrelated
same-basename package a Use function. In one orientation, the clean and composed
indexes both resolve the call to the unrelated package; in the other, the cold
import edge and call edge choose different packages. The source/module overlay
shows the same behavior as baseline, so this is not a newly introduced narrowing
regression. Both packages remain in the closure and the alias optimization alone
does not reduce this same-symbol case.

Source inspection establishes that parser-preserved full extern import paths
reach Resolver.resolveExtern. Its same-repository branch accepts a last-directory
basename suffix and immediately returns the first matching referenceable node.
resolveImport uses a separate directory lookup/fallback, allowing import and call
targets to diverge. The shared ownership/matching policy needs further verification
before a correction is implemented. Equality with a clean index is insufficient
as a semantic oracle when the clean index has this defect; tests must additionally
assert the intended target implied by the exact module/package import.

### Not yet completed

Isolated successful server construction has now passed in a separate private
test process (0.63s test time), with all three absolute XDG directory overrides
installed before initialization and external model endpoints absent. Graph,
config and actual sidecar paths were checked for confinement; semantic and
embedding providers were disabled and the private startup lock was released.
This exposed/covered the constructor's unconditional global memories sidecar
and reloaded ConfigManager defaults. No daemon socket/HTTP/pprof listener was
started and no live daemon or live store/config was modified. This is an
isolation preflight, not the feature's final end-to-end workload.

The actual in-process MCP transport also initialized successfully and supplied
the advertised primitive tool schemas. The first tiny checkout-flow test then
failed before linked-worktree creation: primary tracking reported one file,
four nodes and two edges, but exact primary search remained view_building for
30 seconds (whole test 31.74 seconds; instrumented repeat 31.08 seconds). The
private logs show parsing completed in about 68 ms, but no coordinator existed.
Source verification identifies an explicit policy gate: activateCheckout returns
unless the checkout is ready AND its effective mode is automatic. Our explicitly
tracked primary is dedicated. This is not evidence of slow indexing or a missing
constructor factory. Exact dedicated-primary worktree routing remains a red
acceptance test for the planned primary working overlay. A separate test will
use the existing primary base selector to validate implicit linked discovery and
editing without representing that as completion of primary-overlay semantics.
No reindex/reconcile recovery or longer timeout was used to force a pass.

The manifest-only investigation has not established a sensitive cold-index
oracle yet. Root and nested module-identity changes produced identical old,
target and composed source/edge projections in the attempted fixtures, so
their passing comparisons do not prove invalidation correctness. No broad
subtree-invalidation hook was added from that evidence. Old module provenance
and a semantic scope/importer contract still need verification.

The positive-base guard now has a real private SQLite regression suite:
16 scenarios, 9 expected failures and 7 passes on current code. Missing positive
generations silently fall back to owner HEAD; incompatible/unservable positive
rows are incorrectly accepted. Green validation awaits the native guard edit;
no compressed or truncated full-file source was used to fabricate an overlay.

Safe regular-source reader, module alias repair and module-manifest-only subtree
invalidation, positive-base validation,
committed-base publication, read-through/context reuse, final isolated
benchmarks/E2E, PR and CI.

## Implementation checkpoint: publication and lifetime safety (2026-09-09)

This checkpoint supersedes earlier implementation-status paragraphs. The
positive-base guard is committed as `9fdcd266`, the regular-source reader as
`4428743b`, and dedicated-root isolation/validation as `01ba7596`, with their
actual-source validation recorded. Earlier references to nine failing
positive-base scenarios describe the pre-fix baseline, not current failures.

The catalog publication protocol and v22 schema wiring are implemented, but
the new dedicated-base runtime path remains dormant. Installing authority or
advancing active pointers is not safe until configuration identity, primary
dirty layering, coherent lower-view reads, source-owned writes, and retirement
and build-lifetime protection are integrated. This is not a completed-feature
or end-to-end disk-I/O verdict.

### Publication and migration evidence

The new metadata table records publication authority, desired content and a
transactionally associated build claim; it does not duplicate payload tables.
Both physical builders and ready-payload reuse must pass guarded adoption.
Unchanged, already-adopted cycles require zero catalog writes. Selecting a
historical ready candidate may require bounded claim/adoption metadata writes,
but must not rebuild or rewrite its unchanged payload.

In the captured authority-present fixture, the actual public identity-upsert
wrapper previously performed 20 updates for 20 identical calls, growing a
private test WAL by 333,720 bytes. The corrected wrapper preserves the adopted
pointer and skips unchanged writes; tests prove zero writes with authority
absent as well as present. Paired
100-iteration benchmarks, repeated three times on Darwin/arm64, show:

| Identical public upsert | Before median | After median |
| --- | ---: | ---: |
| No publication authority | 62.667 µs | 35.116 µs |
| Publication authority present | 62.400 µs | 33.728 µs |

The guarded reads increase allocation cost from approximately 0.8 KB and 18
allocations to 3.12 KB and 82 allocations per call. This is an explicit tradeoff,
not a zero-cost shortcut. The WAL measurement is isolated fixture evidence,
not NAND-write accounting or an explanation for the entire reported process
write total.

Actual v22 migration tests cover fresh installation, populated v21 upgrade,
rollback/replay, cascade behavior, failed-open retry without premature version
stamping, warm authority preservation, and future-schema refusal before DDL.
There is no payload or authority backfill. A failed startup may have created
the new companion table before a later phase fails; replay is idempotent, not
a claim that all startup DDL is one transaction.

After all catalog helpers, schema wiring, the public wrapper and the combined
test file landed, the actual complete `store_sqlite` suite passed in 66.193s,
including the 33 new top-level tests; no source or test overlays were used.
The 33 new tests then passed three race-detector repetitions (99 executions,
172.173s), without race reports. Actual-package `go vet` and changed-lines
catalog lint also passed. A final no-overlay 100-iteration, three-repetition
benchmark confirmation measured 34.550 µs without authority, 36.242 µs with
authority, and 290.642 µs for the unchanged observation/claim/adoption cycle;
the earlier closely paired before/after table remains the comparison above.
The full-package linter found one
unchanged warning in this branch's earlier downgrade-guard change
(`schema_version_downgrade.go:43`, an ineffectual assignment), which is queued
as a separate atomic repair rather than reported as a green full lint run.

The claimed-builder boundary also passed 12 tests repeated three times normally
and under the race detector against actual catalog/cold-schema/graphview code,
without the earlier test-only schema installer. That builder remains a private
additive candidate: ordinary sparse Build routing and runtime startup are not
validated by those component results.

### Lifetime races that still require repair before activation

Public-API regressions reproduce reference-check/state-change races that let
retirement delete a newly adopted or parented payload, a successfully pinned
view, or an active physical build. A legitimate allocation delayed until after
retirement/deletion can also enter the fresh-leader fast path. The tests do not
establish how frequently the production janitor selects these candidates.

Retirement must atomically check durable references, including publication
claims, and fence the generation, then take a decisive fresh lease/flight check
before deletion. Parent validation belongs inside the allocator transaction,
before both coalesced reuse and insertion. Chunked payload deletion stays
outside one enormous writer transaction. A final row-deletion check cannot
repair payload deleted earlier.

There is a further seal-lifetime concern: allocation's unconditional open can
overwrite a retirement seal. Removing that unconditional reset alone does not
cover a removed cache entry, because generic positive-generation handles
intentionally permit missing catalog rows for unmanaged use. Managed write
admission needs explicit provenance; a flag that only changes the unknown-seal
case is insufficient if an unmanaged cached-open verdict already exists.

A private strict per-transaction lookup candidate passes file-backed and
asserted pinned-bulk tests, including a live catalog state change while the
same bulk connection remains pinned. Its measured precheck cost is about
9.4 µs, 940 bytes and 25 allocations; it is not selected or wired yet. In-memory
coverage and complete managed-handle propagation remain unverified. Existing
retained-handle late writes are correctly refused by the old seal, so this must
not be described as a demonstrated universal write-seal bypass.

One read-only runtime snapshot at 08:49:30 UTC found a dirty build waiting in
enrichment, its worker blocked on the SQLite write gate, and retirement
actively deleting payload through SQLite. The build and sweep referenced the
same coordinator, but the snapshot did not identify its checkout or connect
it to a particular mutation receipt. Native source confirms that
`deletePayloadChunk` holds the shared writer gate through chunk SQL and commit.
This makes retirement/enrichment contention a concrete performance lead, not
a proven explanation for the whole publication delay. Bounded retirement
throughput and competing-writer latency need explicit isolated measurements.
The inspected sweep drains each selected table until a chunk removes no rows,
checking context between chunks but without a total row, chunk or elapsed-time
budget. Chunking alone therefore does not establish fair foreground latency;
the bounded writer-probe experiment must measure it separately.

### Observation ordering and release gates

Fresh Git/config sampling must occur inside one context-cancelable per-graph
gate, held through recording the desire and released before physical building.
One stable daemon/MultiIndexer registry spans every publisher and authority
replacement; replacing an actor cannot create a second gate while its previous
observer can still execute. Epoch CAS alone does not order stale observations.
The private gate candidate passes eight tests and five race repetitions, but
runtime binding remains unimplemented.

Release validation must retain the original coherent-view and lifecycle
contracts: one selected overlay plus one designated primary, duplicate-identity
precedence without globally promoting unique overlay hits, tombstone masking,
immutable inactive refs, read-only labeled fallbacks, complete logical cleanup,
independent dedicated-graph survival on primary loss, correct demotion and
previewed last-primary family forget.

The final isolated run must cover realistic corpus size and multiple dependent
worktrees, not only a one-file fixture. Measure cold/warm startup, unchanged
polls, same-byte/comment/semantic saves, branch switches, primary advancement,
removal and failure recovery. Include disk mutation → exact fresh search →
next edit latency: development publication waits have taken minutes between
successive source writes, which component tests do not explain or certify.
Separate initialization from steady-state I/O and report payload/metadata
writes, WAL behavior, process counters, peak memory and retained generations.
Use a separate temporary Git family and isolated daemon paths; never add test
worktrees to a family watched by the main daemon.
