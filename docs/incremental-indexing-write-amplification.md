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

### Shared physical builder and reserved initial snapshot checkpoint

The initial immutable dedicated snapshot consumes the positive generation
reserved by the catalog publication protocol; it must not allocate a second
generation. Reservation identity and authority are validated before expensive
work. A physical-flight leader alone opens the committed Git tree and plans the
full source inventory. The leader owns and closes that source, including on
ordinary errors and panics; failure metadata cleanup precedes follower
notification. Followers share the terminal result;
ready reuse does not parse, reopen a seal, or scan payload to reconstruct counts.

The ordinary sparse builder and the reserved snapshot builder share the same
physical runner, retaining producer settlement, enrichment-before-masks,
publication, failed-generation cleanup before follower notification, and runtime
activity accounting. Ordinary sparse planning remains before the activity guard;
leader-owned committed-source preparation and full planning occur inside it.
This does not introduce another planner-statistics check.

The full inventory planner only visits tree metadata. It does not open every
blob or filter away potential languages before the ordinary index admission
rules run. An actual empty initial Git commit is valid: it can build/adopt a
positive empty snapshot and materialize an exact empty view without exposing
generation-zero content. An unborn branch is a different case.

Ready/follower success is not permission to update the primary pointer. The
caller must perform the catalog's guarded adoption on every successful return,
including reuse, so a stale authority or desire cannot publish an old result.
This primitive handles initial full snapshots only, not subsequent incremental
base advancement or live coordinator activation.

The shared ordinary path passed 57 normal and 171 race test executions. Its
empty physical-build benchmark median was 0.571 ms versus 0.555 ms before,
with overlapping ranges and similar allocations; no clear regression or
speedup is established by that small sample. The claimed snapshot's 13 focused
tests, including the empty-commit boundary, passed normally and in three race
repetitions against the actual files without implementation or test overlays.
Each empty-commit run included three ready-reuse cycles with unchanged
logical-write/WAL audits. These checks use real temporary SQLite stores and
committed Git trees; they are component evidence, not a claim of complete
daemon-level or sustained-I/O validation.

This checkpoint does not claim the feature is complete. Managed-writer and
retirement admission, runtime publication/observation ownership, primary dirty
layers, coherent lower reads/source-owned writes, incremental advancement, and
realistic isolated end-to-end plus sustained-I/O validation remain required.

### Sealed-store refusal normalization checkpoint

An isolated public-API regression found that `RetirePayloadGeneration` followed
by a retained handle's `AddBatch` produced a `fmt.wrapError` panic carrying
`ErrPayloadGenerationSealed`. The existing `StorageErrorFromPanic` classifier
rejected it because the chain contained no `StorageError`. The regression failed
normally and in three race repetitions; payload isolation remained intact.

`panicOnFatal` now wraps only this store-emitted lifecycle refusal in a direct
`StorageError`, preserving its complete cause and diagnostic string. Nil,
NoRows, closed-connection precedence, unrelated legacy store errors, and direct
caller/runtime panic classification are unchanged. Tests cover direct, wrapped,
already-typed, and wrapped-typed sealed errors, plus joined nonfatal/sealed errors.
This is a narrow error-boundary fix, not activation of managed admission or proof
of the full physical builder's retirement-loss behavior.

Paired GOMAXPROCS=2 component benchmarks (three samples per case) measured median
sealed-refusal emission/recovery at 365.7 -> 287.2 ns, 128 -> 16 B, and two -> one
allocation. Nil and nonfatal controls remained allocation-free at approximately
6.3-6.4 ns and 10.6-10.8 ns. These are panic-boundary microbenchmarks, not indexing
throughput, sustained disk I/O, or end-to-end feature acceptance.

### Managed generation lifetime and retirement checkpoint

Cleanup must acquire a retirement fence before it can safely delete generation
payload. Observing no readers/builders/references earlier is insufficient: a new
owner can appear between that observation and the sweep. Refusing final catalog
deletion cannot repair payload already deleted. Two deterministic real-component
regressions cover a held materialized reader and a shared physical builder whose
source is blocked in Open; these are ordering bugs, not only data races.

The coupled safety implementation uses the following protocol:

1. Catalog reference admission and the transition to Retiring serialize in the
   actual SQLite writer transaction. Positive missing/Retiring parents and new
   checkout, ref, or dedicated active pointers are rejected. Validate installed
   values, not ignored proposals; preserve stale-epoch/token/identity error
   priority, zero clears, and untouched route slots.
2. Reference diagnostics, retirement admission, and final metadata deletion use
   the same roots: checkout slots, active refs, child ancestry, dedicated active
   pointers, and the current dedicated-publication association. Replacing that
   association must release the historical snapshot unless another owner pins it.
3. After the fence commits, cleanup rechecks Store-owned physical flights and
   shared reader leases. Early owner checks are only cheap refusals. A late busy
   generation stays Retiring and resumable; no sweep occurs until owners drain.
   Omitting an external lease callback never disables the Store flight check.
4. Readers pin before fresh servability validation and pin the full ancestry
   before assembly. Physical leaders register a flight before freshly validating
   the catalog, even when earlier allocation reported a new generation. The
   existing materializer already had the required ordering and was not rewritten.
5. Managed positive-generation handles admit writes only while Building, checked
   inside each actual write transaction. Creating/deriving a handle grants no
   liveness promise. Same-generation forwarding preserves qualification. Missing
   managed rows produce the typed sealed failure without wrapping sql.ErrNoRows,
   which legacy mutation emitters deliberately ignore.
6. Guarded late publication, supersession, failure, and abandonment must preserve
   a retirement fence. The low-level state setter still allows explicit generic
   transitions when no expected state is supplied; runtime late transitions must
   use the guarded contracts rather than treating the generic setter as a fence.

No catalog transaction remains open while waiting for owners or sweeping payload,
and no durable per-query lease heartbeat is introduced. Generation-zero and
deliberately unmanaged compatibility handles retain their distinct semantics.
Normal SQLite AUTOINCREMENT allocation does not reuse deleted generation IDs;
retained-handle tests additionally prove that a later allocation cannot turn an
old managed handle into an unmanaged writer or mutate the replacement namespace.

#### Component validation and measured cost

At the fixture-repair checkpoint, the full Store suite passed 816 tests with two
expected skips. The 36 new lifetime regressions plus the repaired legacy identity
fixture passed three race repetitions (111 top-level passes, no race reports).
The 16 graph-base guard cases passed normally and in three race repetitions.
The original held-reader and actual physical-builder retirement reproductions
also passed normally and in three race repetitions, including unread payload,
typed follower refusal, preserved Retiring after abandonment, and resumed cleanup.
Eight additional parent/terminal boundary regressions passed normally and in
three race repetitions. The combined Store checkpoint passed 824 tests with two
expected skips (66.924 seconds), and all 45 focused lifetime/fixture tests passed
together under the race detector (128.179 seconds). That combined run used the
actual landed 36-test file plus one absent authored parent/terminal test overlay;
it did not substitute production or existing test files. After both files were
physically landed, the no-overlay Store suite again passed 824 tests with two
expected skips (65.510 seconds), and Store vet passed. The physical-file binary
was byte-identical to the earlier combined normal binary. Final reader/builder
permanent-source checks remain separately recorded; these counts are not an
end-to-end verdict.

After the reader regression was physically landed, the complete reader package
passed all 86 tests normally (3.172 seconds) and under the race detector
(57.894 seconds). Both no-overlay runs matched their frozen-binary test
inventories, with stable production/test inputs and no race reports.

After the physical-builder regression landed, all 12 selected builder, claimed
publication, graph-base, and related watcher tests passed normally (10.553
seconds) and under the race detector (33.207 seconds). These no-overlay runs
matched their frozen-binary inventories; they are not the entire indexer suite.
Final vet and golangci-lint across Store, reader, and indexer packages passed,
with zero lint issues. All 17 changed Go files were gofmt-clean and the diff
passed whitespace checks. Inputs remained stable during those checks.

The compatibility fixtures now allocate real healthy generations instead of
inventing IDs. Missing/Retiring graph-base inputs remain deliberately stale caller
values rather than invalid roots persisted through normal catalog admission.
Original first-insert, zero-reset, identity-update, and error assertions remain.

Paired managed single-row SetFileMtime benchmarks measured median file-backed
26.083 -> 46.257 us (9 -> 38 allocations), and bulk-started 21.989 -> 41.219 us
(9 -> 37 allocations). Generic controls were 29.559 -> 28.171 us and
23.421 -> 26.624 us. This fixture therefore exposes approximately 20 us of added
managed transaction cost. Metadata batching can amortize that cost only if the
liveness check and every protected write remain in the same transaction; an
unchecked cache cannot replace the guard.

After-only catalog controls measured FlipCheckoutRouteSlot at median 34.161 us
for zero clears versus 48.347 us for healthy positive pointers, and route upsert
at 42.723 versus 55.544 us. These are not paired pre-change results: positive
pointers also add foreign-key/storage work. Neither this difference nor logical
route-write counts establishes cumulative WAL, filesystem, or SSD NAND writes.

#### Remaining integration and failure gates

The inspected RefViewManager.runBuild raw generation handle is a lower-base
input, not a direct writer. That local result is not an exhaustive transitive
audit. Preserve managed provenance through physical passes, enrichment,
postprocessing, claimed builds, and future incremental-base advancement.

The real isolated runtime precursor registered a primary, automatically
discovered an already-existing linked checkout, activated it once, and published
positive commit/dirty generations using the actual shared lease registry. A
physical source edit refreshed the new view while a pinned old view remained
unchanged, with no generation-zero leakage and no manual reindex/reactivation.
Both checks passed, but that tiny edit took 13.522 seconds to become observable
in one run. This remains an unresolved latency finding, not an immediate-readiness
claim or attribution to a particular watcher/polling/scheduling stage.

A second diagnostic changed only the fixture logger and added write markers.
It passed with a 10.643-second write-to-view interval. The observed write itself
took about 0.221 ms; 10.482 seconds then elapsed before the first update-indexer
record, followed by 157.939 ms to the route record (labeled `reason="poll"`)
and 3.319 ms to the coherent-view assertion. The old generation's retirement was
explicitly deferred because it remained leased. No notification or reconcile-start
record was emitted: this locates the observed wait before the logged indexing
work, but does not establish why a watcher event was absent/delayed, nor prove
that this SharedServer fixture includes every daemon watcher-start hook. The
instrumented run is diagnostic, not a clean paired performance benchmark.

Public untracking/primary closure needs its own held-reader and physical-flight
tests: repository mutation draining alone does not establish reader protection,
and a direct administrative PurgeRepo test cannot certify the public lifecycle.
The first isolated real primary-closure run returned success while the linked
worktree reader remained pinned, but both its held view and a positive-generation
Store lookup lost the previously unread sentinel. A paired healthy control then
proved the sentinel was populated in the positive generation and held view, not
generation zero; the same frozen binary reproduced the untrack failure in a
separate fresh process without warming that process's node cache. The high-level
release/purge path therefore has a confirmed reader-lifetime defect, not covered
by the passing Store retirement tests. Its fix must preserve intentional
administrative all-generation purge semantics in unrelated callers. Purging only
generation zero is not a complete workaround: a pinned delta can still depend on
that mutable lower base. A further paired run populated a concrete generation-zero
base symbol and the positive dirty symbol, left the composed reader cold, and
observed both direct and held-view lookups disappear during public untrack while
leased. Its healthy arm passed in 1.42 seconds; the untrack arm failed in 31.46
seconds using the same binary and fresh private environments. This confirms loss
of both observed portions of the view; it does not claim exhaustive ancestry
ownership. Full coherent teardown requires the planned positive
immutable-base activation or genuine per-graph lower-base ownership; global
generation-zero blocking would unnecessarily couple unrelated repositories.

The same run still saw the catalog row after 30 seconds, with payload absent.
That is an observation, not yet a second confirmed production GC defect: the
SharedServer fixture does not establish which daemon scheduler invokes lifecycle
Sweep. Its inspected public Sweep does call retirement even with no families;
the scheduling/lease-release trigger remains to be validated. Blocking until
readers drain is permitted; without a verified drain boundary that outcome is
inconclusive, not proof of deletion or a nonblocking-removal failure.

The concrete SQLite driver error 13 path remains a separate fix. A private
max_page_count ceiling reproduced AddBatch's ordinary error panic and lost driver
cause for shared-build followers. The narrow proposal is typed normalization of
concrete SQLite FULL at the legacy emitter, preserving nonfatal precedence and
unrelated programmer panics. It does not promise general IOERR/NOMEM handling,
retry/backoff, or successful cleanup on a truly full filesystem.

This safety checkpoint does not activate the immutable primary snapshot or solve
repeated large delta rebuilds. Runtime registration/publication ownership,
committed-base advancement, primary dirty layering, coherent lower reads and
source-owned writes, incremental dependency correctness, MCP edit/fresh-search
flows, cold/warm indexing, and sustained isolated disk-I/O validation remain gates.

### Genuine SQLite-full errors at the legacy panic boundary

The private bounded SQLite page-limit reproducer returned a real driver primary
code 13 from `Store.AddBatch`, but the legacy emitter wrapped it in a generic
error. The direct-only `StorageErrorFromPanic` classifier correctly refused that
payload. Consequently, the physical builder could re-panic and its waiting
followers could lose the original driver cause.

The narrow repair normalizes only genuine `*sqlite.Error` primary-code-13 causes
at `panicOnFatal`, alongside its existing typed sealed-generation refusal. It
does not broaden the panic classifier or recover arbitrary programmer panics.
The existing nil, no-rows, connection-done, and closed-store precedence remains.
Errors merely implementing `Code() int` or containing a disk-full message do not
qualify. Original error chains and messages must remain available.

The new public-ingress fixture uses the real `SparseGenerationBuilder.Build`,
with internal Store test exports and an external Store test. Planning opens pass
through; a source read during an actual positive-generation physical flight
provides the barrier for installing the private page ceiling and admitting two
real waiting callers. The fixture, not Build, owns the supplied source's Close.
The fixture adds no test API to production and uses no private indexer entry
point or production overlay. The before binary included the dormant predicate only, with the old
emitter unchanged: healthy passed in 1.66s; FULL failed in 0.84s with a generic panic,
lost returned generation identity, and generic errors for both waiting callers.

The same public fixture after the emitter change passed healthy in 1.51s and FULL
in 0.88s. FULL returned generation 1 and retained concrete SQLite code 13 for the
leader and both followers without escaping a panic. Both modes passed three
race-detector repetitions (36.656s package time, no reported race). Both arms
retained generation-zero data, drained the real flight and all participants, and
closed the caller-owned source once. These are test observations, not throughput
comparisons. The small FULL fixture could mark the row Failed before restoring
capacity; this does not guarantee metadata writes succeed on a full filesystem.

The actual on-disk internal Store tests then passed all six selected tests in
0.434s. The same-test Store baseline on 11c2dcbf reproduced exactly the expected
AddBatch/matrix failures, with healthy and unrelated-error controls passing.
The paired observable-sink benchmark ran all four cases three times, 200ms per
case, GOMAXPROCS=2. Genuine FULL recognition changed from 0 to 1; nil/no-rows stayed
at 0. Results below are nanoseconds per operation (median and observed range):

| Case | Before ns/op | After ns/op | Before → after B/op | Allocations/op |
| --- | ---: | ---: | ---: | ---: |
| nil | 6.581 (6.559–6.589) | 6.254 (6.180–6.318) | 0 → 0 | 0 → 0 |
| nonfatal no-rows | 10.69 (10.60–10.76) | 9.951 (9.922–10.080) | 0 → 0 | 0 → 0 |
| real FULL | 483.9 (463.4–884.8) | 294.4 (293.8–294.5) | 80 → 24 | 2 → 2 |
| wrapped real FULL | 385.6 (377.1–454.2) | 325.1 (321.5–327.3) | 96 → 24 | 2 → 2 |

These measure emission/recovery overhead, not database filling or indexing. Host
contention was uncontrolled, and the before-FULL outlier is retained. No claim of
statistical significance or indexing-throughput improvement follows. The before
binary used the original combined test; the after binary used its exact contents
plus test-only writer exports. Benchmark bodies and observable sinks were kept
byte-for-byte. Both were frozen from verified actual production inputs; neither
replaced production source through an overlay.

Final actual-file acceptance includes the portable public test (`os.DevNull` for
Git's disabled global config), with no test or production overlays:

- Full Store suite: 828 passed, 2 expected skips, 0 failures, 71.117s; its 830
  terminal test names exactly matched the same frozen binary's inventory.
- Seven selected tests, including the public builder, passed all three race
  repetitions: 21 passes, no failures or race reports, 46.949s.
- Vet and lint passed for Store, graphview, and indexer; lint reported zero issues.
  Formatting checks passed for both production files and both permanent tests.
- Production/test manifests stayed unchanged across validation. Recursive Store
  package file/hash snapshots matched before and after the final full run.

The first full invocation used an empty private working directory and failed the
source-census test `TestGenerationCapabilityChecklistIsComplete`; the other 827
tests passed and 2 skipped. Native source inspection confirmed its explicit
`os.ReadDir(".")` and relative `parser.ParseFile` dependency. The earlier passing
Store runner had used the actual package working directory. The same frozen
binary passed the census control and full rerun after restoring that directory,
with private config/data/cache/state/temp paths and credential-free environment
unchanged. No source was copied and no test assertion was changed or skipped.
The initial failure remains recorded, not relabeled as a successful invocation.
Two lint launches also failed before analysis because the isolated runner lacked
a lint cache and selected Go 1.26 instead of required Go 1.27. Explicit private
cache and verified toolchain selection corrected the runner; no code was changed
for those failures.

Graph-assisted post-analysis remains limited: detect and contract checks timed
out; test mapping did not identify the executed emitter tests; no guard rules
were configured. Acceptance here rests on the recorded actual-file executions
and static checks, not a claim that those graph checks passed.

This is error propagation, not a cure for disk-write amplification. A page-limit
fixture does not establish survival under full filesystems, successful metadata
cleanup while space remains exhausted, retry throttling, disk reclamation, WAL
size, cold/warm throughput, or sustained background I/O. Those remain separate
acceptance gates, as do immutable primary activation and public untrack safety.

### 2026-09-09: tracking preparation split, without runtime activation

The first runtime-integration seam is now separated without adding another
physical index. `TrackRepoCtx` and its source-aware overload retain their public
behavior. The overload calls `prepareTrackRepo` and then
`trackPreparedRepoSourceCtx`; the existing constructor, coordinated second
duplicate check, physical build, metadata/indexer installation, deferred global
work and uninstalled-indexer cleanup retain their order. A byte-level reverse
check reconstructs the original file from the single reviewed replacement.

Preparation resolves the canonical root, identity, final prefix and effective
configuration, performs the existing early duplicate checks, and invokes the
existing tracking hook outside the registry lock. It does not construct or
publish an Indexer. Its value is neither a namespace reservation nor an immutable
configuration snapshot; configuration loading and the hook remain side effects.
Concurrent preparations can both invoke the hook. The coordinated duplicate
check remains authoritative for installation. Already-tracked calls still
return nil without indexing newly changed source.

The nonempty-mtime warm `ReconcileRepoCtx` path is deliberately unchanged. It
does not invoke the cold tracking hook, has different early admission/dedup
checks, and still installs its restored Indexer after a successful nonnil
result, including a clean census. Blindly sharing cold preparation with that
path would introduce behavior changes. Installed-runtime observations also
cannot obtain publisher inputs merely by calling a preparation helper that
returns nil for an already-tracked repository.

The actual SharedServer tracking hook constructs/registers a resolver LSP
helper; replacing it with publication wiring would remove existing behavior.
Startup also calls Track/Reconcile directly, so lifecycle Register-only wiring
would miss cold/warm producers. Exact startup source confirms that a closed
view-build gate is installed before catalog seeding, while that gate opens only
after warmup and enrichment. An initial immutable publisher must not wait behind
a gate whose opening depends on its own completion. Early query readiness also
precedes deferred global writes: it is not an immutable sealing/adoption event.
These are integration constraints, not new runtime behavior in this extraction.

Actual-file acceptance:

- Four new preparation tests cover returned inputs, the unlocked hook,
  concurrent non-reserving preparation, validation and public duplicate exits.
  Their private Git fixtures clear inherited Git-routing environment variables
  before both explicit and internal Git calls.
- The isolated public regression builds a real SharedServer/SQLite store,
  verifies cold symbol persistence, changes private source, and verifies that
  repeated public tracking calls do not silently reindex it. Its ordinary test
  parent launches an isolated child and requires a unique success marker after
  cleanup; a skipped or empty child cannot pass the test.
- Indexer: 32 selected normal tests passed in 2.763s; 12 selected race tests
  repeated three times produced 36 passes in 4.273s. Both builds used actual
  source/test files without overlays, and their inventories matched selection.
- Public cold/no-op check: one pass in 1.007s. Permanent public regression:
  three normal passes in 2.401s and three race passes in 10.996s, with three
  distinct cleanup markers in each run. Its builds included only the absent,
  frozen benchmark/diagnostic test fixture as an extra test source; no
  production or permanent-test files were replaced.
- No selected test failed or skipped, and no race was reported. Formatting,
  vet and lint passed for the affected Indexer/SharedServer packages. Static
  checks used actual files without overlays. Source/provenance manifests stayed
  stable throughout compilation and validation.

Before/after benchmarks used the same frozen fixture, three fresh processes per
case, and one timed operation per sample. The no-op operation is a batch of 20
public calls; every cold sample observes the expected function, and every no-op
sample preserves the old function without materializing the newly added one.

| Operation | Before median (range), ms | After median (range), ms | After allocations |
| --- | ---: | ---: | ---: |
| Public cold call | 380.792 (200.306–411.435) | 64.235 (61.805–64.497) | 5,526,168 B; 16,660 allocs, medians |
| 20-call public no-op batch | 1,084.865 (741.745–1,329.037) | 243.259 (232.844–246.896) | 879,680 B; 1,940 allocs in every sample |

Cold allocation ranges were 5,492,704–5,527,016 B and 16,659–16,678 allocations.
No-op allocations exactly match the baseline: 43,984 B and 97 allocations per
call. The lower wall times are **not a causal speedup claim**: the baseline had
large uncontrolled host contention, and this extraction adds no faster indexing
algorithm. These payload oracles do not count parser invocations, loser Close
calls, database writes, WAL growth or SSD writes.

Runtime CWD/config/data/cache/state/temp paths were private and credentials were
unset before package initialization; normal shared Go compiler/module caches
were reused. Go 1.27.0 reported no active parent go.work. The live daemon,
application store and tracking configuration were not restarted or modified.

Native post-detect timed out; test mapping found no covering tests and no guard
rules were configured. Contract analysis still described the pre-extraction
170-line body and warned about broad caller impact. These are not green
post-change graph checks; acceptance rests on the actual-file executions,
reviewed byte-preservation proof and static checks above.

This is a behavior-preserving prerequisite, not the completed disk-usage fix.
Immutable cold/warm/steady publication, coherent primary dirty views and
dependent rebasing, public-untrack safety, full MCP end-to-end validation and
sustained large-corpus I/O acceptance remain open.

### 2026-09-10: validation-only dedicated build claims

A stale builder could read its current claim A, lose ownership to replacement B,
then call the allocating claim API after both attempts had failed. The later
caller-side mismatch check rejected the new claim, but could not undo the new
association/generation already allocated by that call.

`ClaimDedicatedBaseBuildRequest.ExistingGenerationID` now selects atomic
validation-only behavior when positive. Inside the same writer transaction it
requires the current generation, attempt, desire and a live building/ready/adopted
state; all candidate and ownership guards still apply. This mode cannot enter
historical binding or allocation paths. Zero retains ordinary claiming/retry;
negative values are invalid. The initial claimed builder explicitly uses this
mode. There is no schema or payload-format change.

Four regressions passed normally and three times under the race detector. They
cover A-fails/B-fails/stale-A, ordinary retry compatibility, unchanged live-state
validation, incorrect IDs/tokens/desires, negative input, and direct failure.
Actual initial build/ready replay and SQL logical-write audits also passed.
Three serial 100-iteration samples measured validation-only calls at a median
144.891 microseconds (143.038–148.778), 11,949 B and 326 allocations. Ordinary
current-claim lookup measured 149.057 microseconds (146.131–154.365). This is a
small correctness/overhead benchmark, not a sustained disk-saving claim.

The combined actual-file cohort passed 26 normal tests and 78 race executions;
vet/lint passed for store_sqlite, graphview, indexer and serverstack. Compilation
and static source manifests stayed stable. Native post-detect remained
unavailable/incomplete; no covering tests were mapped, no guards configured,
and contract analysis warned about broad impact. These are not green native
post-change checks. All runtime paths were private; the live daemon was untouched.

### 2026-09-10: owned effective configuration snapshots

`snapshotDedicatedBaseConfig` freezes the complete effective IndexConfig,
including nested maps/slices, rather than retaining ConfigManager's shared
references. Its versioned fingerprint also includes repository, workspace and
project output context. Framework selection is normalized as a set for hashing
only; the frozen configuration retains its original selection values. Automatic
selection remains distinct from explicitly disabled selection. A pointer to a
nil selection slice is preserved correctly across the JSON ownership copy.

Five actual-file tests passed normally and in three race repetitions, including
all-field ownership/mutation checks, framework semantics, output-context and
effective-field fingerprints, and a future JSON-shape guard. Three serial
100-iteration benchmarks measured default snapshots at 16.794 microseconds
(16.754–17.123), 8,832 B and nine allocations; configured snapshots at 22.821
microseconds (21.502–23.706), 10,448 B and 33 allocations. The combined four-package
vet/lint and stable-source validation described above also cover these files.

This helper is not yet the complete runtime identity binding. Output-affecting
services/capabilities outside IndexConfig and extractor/resolver stamps must be
bound by the publisher; a correct hash of incomplete inputs is not sufficient.

### 2026-09-10: ordered initial publication runtime

The initial publication primitive now uses one stable, context-cancelable graph
observation gate across publisher replacements. Installation acquires authority
once; observations validate it, sample fresh inputs, and record/claim under that
gate. Physical work and follower waits run after releasing it. Every successful
return, including ready reuse, passes guarded adoption. A canceled follower must
not mark its leader's physical claim failed. Idle gates are removed without
splitting the gate used by queued waiters.

An already-active but different tree/config/extractor/resolver identity returns
an explicit incremental-advancement requirement without changing the desire.
It must not become a full rebuild on every commit. This initial-only primitive
also refuses ancestry it cannot consume; incremental dispatch is separate work.

Ten tests passed normally and in three race repetitions. They cover no-write
ready replay, observation ordering, authority replacement, external adoption,
fresh builder requirements, canceled followers, ready-before-adoption retry,
and gate serialization/ABA/cancellation/cleanup. Three serial 100-iteration
benchmarks measured unchanged ready replay at 437.608 microseconds
(431.518–445.524), 37,950 B and 1,060 allocations, with zero audited logical
catalog/payload writes. Shared-gate acquisition measured 846.2 ns median;
independent parallel gates measured 415.4 ns, with five allocations per operation.
These small fixtures do not measure sustained daemon I/O or large-corpus latency.

Startup, warm reconciliation and watchers are not wired to this primitive yet.
Replacing legacy indexing must preserve complete global/cross-repository reads
and separate writable outputs. Simply installing an empty registry shell or
pointing legacy writers at a sealed generation would be incorrect. The combined
actual-file test/static validation above covers this component, not that runtime
integration or end-to-end feature completion.

### 2026-09-10: complete lower ancestry for coordinator builds

Commit and dirty builds now consume the complete composed lower ancestry, not
only the newest sparse payload handle. The coordinator holds the materialized
read leases through physical planning/building and both dirty confirmation
attempts, then releases them idempotently. Initial generation-zero behavior is
preserved. Committed dedicated roots do not accidentally inherit dirty legacy
generation-zero payload. Ref-fact hints still come from the immediate corpus;
the full composed reader remains authoritative for closure discovery.

Six new regressions and the existing nonzero-base regression passed normally
and in three race repetitions. They cover inherited nodes, replace/delete masks,
node tombstones, missing/retiring/wrong-owner ancestry, real Git incoming-caller
closure from the oldest ancestor, and leases across two dirty retries. Initial
synthetic tests failed because top nodes lacked required file-ownership masks.
The fixtures now create those masks and assert raw payload presence; production
ownership guards were not relaxed. An explicit outgoing source-mask oracle is
still separate from these incoming-closure checks.

Three serial 100-iteration small-fixture benchmarks measured reader construction
and release at 176.934 microseconds (173.998–184.178) for depth one, and 396.461
microseconds (393.876–400.852) for depth three. Median allocations were
23,696 B/589 and 48,069 B/1,219 respectively. This measures composition overhead,
not full query throughput, parser work or disk-write savings. The actual-file
normal/race and four-package static checks above cover this change.

The full release gate remains open: effective runtime/service identity binding,
single-pass cold/warm publication and global read/write separation, primary dirty
and committed advancement dispatch, source/context reuse and import semantics,
bounded ancestry/cleanup, public-untrack reader/build lifetime, isolated MCP
end-to-end behavior and sustained realistic-corpus I/O must still be validated.

### 2026-09-10: repository-owner read admission and drain primitive

`LeaseManager` now has a separate repository-owner admission domain, including
legacy generation-zero reads. Registration requires the complete graph,
checkout, incarnation and repository-prefix identity. Prefix exclusivity matches
the existing `dedicated_graphs.repo_prefix UNIQUE` storage contract. Closing
admission retains an identity-specific tombstone until cleanup is explicitly
finalized; a stale drain handle cannot delete a replacement owner.

Explicit scopes acquire atomically. Broad/all-repository readers also protect
owners registered after acquisition, including an initially empty graph.
Closing one owner rejects new broad reads but still permits explicit reads of
other healthy owners. Releases are concurrent/idempotent, notification callbacks
run outside the mutex and release-once guard, and shutdown permanently closes
admission while waiting for existing readers. These operations perform no SQL
and create no per-query goroutine. The primitive does not itself delete payload,
configuration or catalog state.

All 14 new tests passed normally and in three race repetitions (42 race-test
passes). Tests cover acquisition/close linearization, scope atomicity, stale
handles and incarnation replacement, notification cancellation/reentry,
initially empty broad readers, bounded finalized state, and shutdown races.
The actual-source normal/race binaries and strict run logs are recorded in
`/private/tmp/gortex-component-validation.fwgMDk`. Four-package vet and lint
also passed, with stable source manifests; the new graphview test and both
production-file hashes were checked independently before and after static runs.

Three serial 100-iteration acquire/release runs measured the following medians:

| Owners | Explicit scope | All scope | Allocations |
| --- | --- | --- | --- |
| 0 | 58.75 ns | 57.50 ns | 48 B / 1 |
| 1 | 72.08 ns | 94.58 ns | 56 B / 2 |
| 20 | 573.3 ns | 1,008 ns | 208 B / 2 |

These are small in-memory component benchmarks, not contention limits or daemon
I/O results. Public MCP admission, durable closing authorization, deferred
cleanup continuation, restart recovery and shutdown joining still need wiring
and end-to-end validation. The full feature release gate remains open.

### 2026-09-10: claimed sparse dedicated-base advancement

The dedicated delta builder consumes an already reserved publication claim and
the complete leased lower ancestry. It validates the current owner, claim,
parent tree and policy before building into that exact positive generation;
publication remains the caller's responsibility. Ready-claim replay performs no
new physical build. An incompatible extraction policy requires a fresh full
base rather than inheriting incompatible rows.

Six actual-source tests cover real Git changes, two consecutively adopted
layers, strict complete node/edge payload parity, stale claims, single-flight
physical builds, ready replay, tree-equivalent commits and policy changes. The
full-payload reference is an independently built positive generation at the same
source identity and scope; a separate generation-zero cold reference remains a
structural control. Generation-zero builtin scope differs from the scoped
positive-generation path, so it is not used as a full-field oracle. The earlier
failing evidence is retained rather than discarded. A body-only edit explicitly
does not reparse an unchanged caller, while a real signature change must reparse
that caller and preserve its dependency edges.

Validation on actual source (no production overlays):

- Normal: all 6 top-level tests pass, 10.781 s.
- Race detector: all 6 tests pass three times, 18 passes, 93.130 s.
- Three serial 100-iteration benchmark cohorts: ready replay median 324,050 ns,
  30,822 B and 834 allocations per operation; one-file delta against a fixed
  full base median 147,634,870 ns, 5,924,551 B and 23,644 allocations per
  operation, with 2 indexed paths per operation.
- Vet and lint pass for store_sqlite, graphview, indexer and serverstack with a
  stable source manifest.

The delta benchmark builds an unadopted child of the same base on each
iteration; it is not a growing-chain benchmark, a before/after I/O result, or a
measurement of SSD writes. Full cold/warm lifecycle integration, bounded
ancestry maintenance, global enrichment and sustained daemon I/O remain release
gates. Reproduction records and retained failure analysis are in
`/private/tmp/gortex-claimed-dedicated-delta.aXnXca/STRICT-ORACLE-V2-VALIDATION.md`.

### Go package ownership regression: validation checkpoint (2026-09-10)

An independent cold/sparse SQLite fixture reproduced four wrong import/call
targets when same-named Go packages in one repository were confused. The fix
rejects only a candidate positively certified to belong to a different package.
Unknown ownership preserves existing behavior; it neither invents missing
candidates nor treats foreign repositories, replacements, workspaces, vendor
paths or unsupported sources as certified. Parsing may use a narrowed source,
but manifest authority comes from the full selected source, atomically paired
with it. Immutable sources never consult dirty working-copy manifests.

The actual resolver prepares this authority once per pass epoch, before candidate
lookups and worker activity. A real SQLite test resolves 2,049 pending rows across
a 2,048-row page boundary with one factory invocation. Both provider-disabled
and provider-enabled foreign-repository controls retain the original target.

Final actual-source validation: 31 focused tests pass; the full resolver package
executes 1,020 tests with 1,018 passing and exactly two expected copied-store probe
skips. Vet and golangci-lint pass for resolver, indexer and serverstack. The prior
27-test race cohort passes three times (81 runs); the final delta after that run
is four existing-test error checks and one error-message capitalization change.
The wider 65-test indexer selection passes in a 64-plus-1 split: its promisor
positive control needed a separate local-file-only Git environment. The missing
relative resolver fixture was supplied inside the isolated CWD, without changing
tests. Original harness failures and their corrections remain in the run ledger.

Serial component timing (three samples, 10,000 iterations) measures the nil gate
at 3.68–3.74 ns/op with zero allocations, fixture-certified rejection at
132.5–145.6 ns/op with one allocation, and unchanged-epoch preparation at
104.2–105 ns/op with zero allocations. These are not production-source costs.
An additional isolated benchmark invokes the actual selected-source provider
(three samples, 100 iterations). At 1,025 inventory files, filesystem preparation
takes 56.0–58.6 ms and about 3.01 MB/39,120 allocations per pass; Git-tree
preparation takes 1.98–2.04 ms and about 1.55 MB/18,606 allocations. Prepared
lookup takes approximately 0.47–0.68 us with zero allocations. SQL inventory,
outer master locks, parsing, global passes and end-to-end daemon work are outside
those measurements. The filesystem result is Darwin evidence, not a Windows claim.

This checkpoint fixes candidate rejection, not module-root/directory-mismatch
candidate placement. Global immutable-base activation, owner-routed derived
writes, cleanup integration, full daemon E2E and sustained write-amplification
validation remain separate acceptance gates. Native post-change detection
returned an incomplete, read-only fallback while the checkout rebuilt; graph
tests/guards/contracts were unavailable, not green. Compiler, test and static
results above are separately recorded actual-source evidence.

Run ledger: `/private/tmp/gortex-go-package-after.2H03VR/LANDED-VALIDATION.md`
(SHA-256 `f2a389dfb115639b33aabb84f25b76a66de45533b1acfef18eb4a1b4a311d312`),
with the exact 22-file manifest, baseline reproduction, final tests, race runs,
benchmarks and preserved failures. All validation used private configuration,
state and storage; the live daemon was not restarted or reconfigured.
