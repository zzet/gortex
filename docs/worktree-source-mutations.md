# Source edits in selected worktrees

An exact, live automatic worktree supports `edit.file`, `edit.write`, and
`edit.symbol` (and their legacy tool equivalents). The session may already be
inside the worktree, or select it explicitly with `view.kind: worktree` and a
registered checkout ID or absolute root path. No explicit tracking or dedicated
graph is required.

The old mutation guard refused every routed source tool, including dry runs,
and suggested editing from the checkout's own working copy. That suggestion
could not work: an automatic checkout's own session also receives a routed view.
The replacement admits only the audited checkout-aware write paths.

## Safety and freshness

- Admission obtains the existing checkout coordinator's interactive build
  admission and reconciliation lock. It verifies that the checkout and the
  route epoch still match the view used to interpret the edit.
- File targets must belong to that checkout, including after resolving symlinks
  and existing parent directories. Absolute primary/sibling paths, nested Git
  repositories, and Git metadata are refused rather than redirected.
- Dry runs validate against the selected working copy without committing bytes,
  withdrawing its route, or building a generation.
- Immediately before a real write, the old dirty route becomes pending. Existing
  leased readers remain valid; new requests cannot mistake the old generation
  for the updated working copy.
- The existing single-file commit ledger distinguishes disk commit from graph
  refresh. Once bytes commit, the edit enqueues a checkout refresh ticket and
  returns `reindexed:false`, `reindex_pending:true`, and `graph_status:pending`.
  The middleware releases the write lease before background publication; the
  response does not wait for parsing, enrichment, or the old synchronous refresh
  budget. It includes a `mutation_receipt` for the disk commit and a
  `reindex_receipt` for the graph refresh.
- The existing coordinator loop owns refresh execution. There is no second
  indexer, per-edit build goroutine, or primary-watcher fallback. The physical
  build uses coordinator lifetime, not the request's short wait deadline. A
  disconnected caller does not cancel a committed edit's publication.
- A ticket becomes fresh only after an active route publishes the expected
  checkout incarnation, HEAD, and content. Deferred or rescheduled work remains
  pending. Supersession, disappearance, and terminal failures cannot be mistaken
  for success; the original failure detail is retained. A primary reindex cannot
  certify an automatic checkout's receipt.
- Publication evidence includes the admitted branch/HEAD, checkout/root identity,
  the dirty snapshot fingerprint, and a SHA-256 of the committed target bytes.
  An intervening change to another dirty file can conservatively supersede the
  ticket instead of claiming freshness. This does not strengthen the existing
  whole-checkout sampler, which fingerprints dirty paths and their size/mtime
metadata rather than hashing every file. Historical superseded receipts remain
  inspectable but do not block diagnostics for a newer current view.
- Coordinator shutdown joins admitted writes before checkout state can retire.

All inexact fallbacks and immutable ref/commit views remain read-only. A missing
coordinator refuses the operation before touching files. Save or discard active
session editor buffers before editing on-disk worktree content; buffer-derived
symbol locations are not authority to modify disk.

## Agent lifecycle and recovery

The supported flow is:

`new worktree -> automatic discovery -> search -> exact edit -> pending refresh -> fresh -> next edit`

1. An agent request from a newly added checkout discovers it without adding
   explicit tracking intent. Search may return a labeled, read-only base
   fallback while the selected graph builds. `require_exact:true` refuses that
   substitution.
2. Source edits still require an exact, authorized working-copy view. Busy
   admission is bounded and retryable; graph-dependent operations do not wait
   indefinitely behind a build or silently edit the primary fallback.
3. A successful disk edit returns a pending publication receipt promptly. Query
   `change(operation:"receipt", options:{receipt:"<mutation_receipt>"}, view:...)`
   to observe `pending -> fresh` or a specific failure. Never repeat the disk
   edit merely because its graph publication is pending.
4. Receipt inspection and checkout-scoped recovery validate checkout identity
   and authorization independently of graph materialization. They work while a
   route is building. Their checkout-scope metadata is not a claim that an exact
   graph was served. Successful scoped recovery can clear previously failed
   tickets from the current-view safety barrier, but leaves their historical
   failure receipts intact. It cannot clear failures admitted after that recovery
   request or failures from another checkout/incarnation.
5. `workspace_admin.reindex` for an automatic checkout queues its coordinator;
   it does not require promoting the checkout to a dedicated graph or invoke
   canonical incremental indexing. Scoped paths must remain inside the selected
   working copy. These working-copy controls accept automatic/worktree selection;
   explicit base/ref/commit selectors are refused rather than silently operating
   on a different working copy.
6. `change.detect` always examines Git changes in the selected checkout. If the
   corresponding graph is not ready, it reports the file changes with incomplete
   graph analysis and unknown risk, rather than returning a false clean-tree
   verdict from the main checkout.

The old flow violated these rules in three connected places: a short synchronous
refresh error became a frozen terminal receipt even though the coordinator later
retried; ready-graph routing blocked receipt inspection and recovery; and change
detection used the canonical repository root despite an exact-worktree rider.
The repair needs neither a schema migration nor a daemon restart protocol.

First-CWD discovery is limited to a checkout proven by Git's worktree inventory
to belong to an already known family with a designated primary. It records only
that missing checkout and activates its coordinator. It does not sweep other
families, perform retirement/forget cleanup, modify explicit tracking intent, or
wait for a graph build. Both daemon CWD admission and direct tool routing use the
same path lookup, so a first request need not wait for a periodic reconciliation.

### Observed slow-refresh incident (2026-09-06)

In the PR-744 worktree, an edit committed at 10:11:51 UTC. A subsequent sparse
build reached the 200-file dependency-closure cap. Parsing took 46.165 seconds;
the eight workers accumulated 0.132 seconds reading, 5.739 seconds extracting,
and 305.566 seconds in batch processing. Those worker times overlap and must
not be added to derive wall time. Synthesis, store work, and semantic enrichment
continued until 10:16:40 UTC. The exact route later recovered while the receipt
still claimed terminal failure. The retained receipt did not expose the original
refresh error, so this evidence alone does not prove its initial trigger.

This fix removes request-lifetime cancellation and recovery dead ends from that
flow. It does not claim that a large dependency closure becomes cheap: background
build cost and interactive request latency must be measured separately.

## Deliberately unsupported write paths

Batch editing/recovery, filesystem lifecycle operations, and LSP refactors have
separate write and refresh machinery. They remain refused through routed views
until their checkout ownership and recovery are integrated. The refusal states
this limitation rather than recommending a CWD change that cannot solve it.

Ordinary canonical-checkout editing is unchanged. This change requires no schema
migration, daemon restart procedure, new tracking intent, or additional database.

## Regression validation and benchmark scope

The regression fixtures create real temporary Git families, automatically
register/activate a linked checkout through the lifecycle, and invoke MCP
handlers and the registered facade. They cover own-CWD and explicit path/ID
selection, explicit and inferred operations, dry runs, disk isolation, and
subsequent exact graph/source reads. Separate tests cover stale epochs, dirty
state, cancellation, shutdown, panic cleanup, root replacement, and failed
publication after a successful disk commit.

The lifecycle regressions additionally add a worktree after startup and search
it without tracking; block the real extractor on post-edit content; verify that
edit returns pending before the parser is released; query receipts, queue scoped
recovery, and detect selected-root changes during the blocked build; then release
the parser, observe fresh publication, and perform a second edit. Pending exact
writes remain refused and the primary's bytes remain unchanged throughout.

On an Apple M1 Pro, three 10-iteration runs against a tiny fixture measured:

| Operation | Time per operation |
| --- | --- |
| Checkout admission and release (no build) | 10.06–10.21 ms |
| Complete MCP worktree dry run | 12.35–19.55 ms |

These figures measure the new admission path, not cold indexing or large-repo
scaling. Tests additionally assert that a dry run leaves the route epoch and
generation IDs unchanged. Reproduce with
`go test ./internal/indexer -run '^$' -bench '^BenchmarkCheckoutMutation' -benchmem`
and
`go test ./internal/mcp -run '^$' -bench '^BenchmarkWorktreeMutationDryRun$' -benchmem`.

Measure enqueue latency independently of a stalled parser with
`go test ./internal/mcp -run '^$' -bench '^BenchmarkWorktreeMutationEnqueueWithBlockedIndexer$' -benchtime=5x -benchmem`.

For the lifecycle repair on 2026-09-06, three five-iteration runs measured
11.74–12.50 ms per dry run, compared with 11.21–12.17 ms before the repair
(medians 12.38 and 12.07 ms respectively). The median allocation count increased
from 2,835 to 3,062 with the additional checkout binding and scope checks.
Committed edits with the real extractor blocked returned in 33.55, 34.45, and
37.57 ms per operation (three run averages; median 34.45 ms). Fixture setup and
eventual publication are outside that edit timing. These tiny-repository
measurements demonstrate separation from the stalled indexer, not a prediction
of large-repository indexing throughput or a production latency guarantee.

Receipt inspection during pending publication measured 0.167–0.239 ms per call
(three 100-iteration run averages), including checkout binding and authorization.

First-checkout observation measured 65.40, 64.66, and 76.91 ms per admission
(three five-admission run averages; 15 successful admissions, zero busy replies
in that final run). An earlier loaded run reached the 250 ms admission bound;
this exposed a duplicated Git inventory scan and a lost busy classification.
Observation now reuses the validated inventory and preserves retryable busy
errors through daemon admission instead of presenting them as untracked CWDs.
The bound is not increased, and no pending request is granted graph authority.

Formatting the daemon's retryable admission-error response measured 2.34–2.38 µs
per response (three 200 ms benchmark runs, 1,601 bytes and 26 allocations per
operation). Wire-level tests preserve initialization/capability metadata while
denying graph reads and source/configuration mutations until admission succeeds.

### CI regression: graph-free pending detection

The first lifecycle patch created an empty in-memory graph to suppress stale
symbols during pending detection. CI's `TestNewIsFencedToIndexerStaging` correctly
rejected that production allocation. Pending detection now passes an explicit nil
reader to the Git diff mapper. Both symbol-join paths skip graph access while
preserving Git hunks and added, modified, deleted, and renamed file metadata.
Non-nil readers retain their existing behavior; ordinary untracked files are
not newly included by this change. Pending analysis remains incomplete with
unknown risk, not a claim that no symbols changed.

Regression tests cover empty-file deletion, intent-to-add, both non-nil symbol
joins, Git failures, and a pending reader that panics if consulted. The
constructor guard remains unchanged. On Apple M1 Pro, three benchmark runs
measured a median of 7,999 ns / 17,504 B / 313 allocations for the former
placeholder join versus 234.3 ns / 208 B / 4 allocations for file-only mode.
The real Git-diff benchmark remained dominated by subprocess time (medians
9.98 ms and 9.29 ms); these runs do not establish an end-to-end speedup.

Compile-only checks do not execute the constructor guard. Validation must run
`go test ./internal/graph` as well as the affected analysis and MCP tests.

### Windows physical-root identity

Windows CI exposed a real replacement-check failure: `os.Stat` can defer file-ID
lookup until `os.SameFile`, reopening a pathname after another directory has
replaced it. Mutation admission and refresh tickets must capture identity before
waiting, not when comparing later. On Windows, identity now comes from an open
directory handle's `File.Stat`; that handle is closed immediately. Non-Windows
platforms retain their eager `os.Stat` path. Rooted hashing also compares identity
obtained from its opened root, not a lazily resolved pathname. See Go's
[Windows file identity implementation](https://go.dev/src/os/types_windows.go)
and [handle-based stat implementation](https://go.dev/src/os/stat_windows.go).

The regression captures identity, renames the original, recreates its pathname,
and only then compares identities. It must reject the replacement while still
recognizing the renamed original. This ordering matters: an earlier `SameFile`
call would hide the Windows bug by populating its lazy ID. Missing roots and
regular files fail closed, and no persistent directory lock is introduced.

Local mutation/refresh/root race tests passed, as did Windows cross-compilation
of the standalone identity helper and tests. Windows execution remains a CI
check, not a claim based on Darwin tests. Three Darwin benchmark runs measured
2.139–2.527 microseconds for pathname stat and 1.904–2.030 microseconds for the
final capture helper, both 304 bytes and two allocations per call. These results
show no material non-Windows overhead; they do not measure Windows performance.

### Slow Git discovery must make progress across requests

Windows CI exposed starvation in the original first-request budget: the entire
Git inventory and checkout observation were canceled after 250 ms. If metadata
work consistently exceeded that budget, every retry restarted it from scratch.
A second routing bug converted the retryable discovery error into a successful
empty search. Neither behavior satisfied the new-worktree agent flow.

The 250 ms caller wait remains, but discovery now belongs to the lifecycle:

- Requests for the same canonical path share one job with a five-second
  cooperative work deadline. A timed-out caller does not discard its progress.
- At most 32 jobs may exist. Expired or canceled work still occupies its slot
  until its worker exits, so slow cancellation cannot evade the concurrency cap.
  Lifecycle shutdown cancels and joins the workers.
- The first phase only reads Git metadata and the known-family catalog. Every
  caller independently authorizes the proven primary before observation can
  start or a shared successful result can be returned. Denied callers cannot
  start observation or borrow another caller's authority.
- Cached proof records physical root, Git-directory and common-directory
  identities, plus `.git` and `commondir` identity/content/absence. A directory
  resolution after capture verifies that this proof belongs to the inventory's
  family. This uses the existing Git resolver without repeating the worktree
  inventory. Later validation is filesystem/catalog-only, including before
  activation and before returning a cached result.
- Pending discovery returns typed `view_building`; stale, stopped, denied, and
  canceled admission cannot become a base result. A generic catalog failure may
  use labeled fallback only when independent tracked-root metadata proves that
  the canonical checkout owns the CWD, excluding nested or sibling checkouts.

Agents retry a pending request in the same session. Once admission establishes
the checkout, ordinary labeled base fallback may serve search while its graph
builds; edits still require the exact selected view. No explicit tracking or
additional graph copy is introduced.

Deterministic regressions delay both inventory and HEAD sampling beyond the
caller budget, then verify eventual success with each operation executed once.
Other tests cover caller-specific authorization, canceled and expired jobs,
shutdown, proof replacement, and exact/nonexact search refusal and recovery.
Windows path assertions use normalized paths, and the relative Git-path fixture
changes to its own temporary directory so CI's D: checkout/C: temporary-directory
layout does not invalidate or skip relative-path coverage.

After binding hardening, three five-admission benchmark runs on Apple M1 Pro
measured 90.49, 79.68, and 105.89 ms per new checkout, with 15/15 successful
admissions and zero busy responses. The extra binding check costs time compared
with the earlier 65–77 ms runs; it is not a claimed performance improvement.
These measurements cover a tiny cold metadata fixture, not Windows runtime,
large-family inventory, or graph construction. The improvement under slow Git
is preserved progress with bounded caller waits, not faster subprocesses.

Completed-proof validation measured 0.253, 0.445, and 0.301 ms per request in
three runs, about 21.6 KB and 288 allocations per operation. Each run performed
one inventory across 1,873–2,390 requests, demonstrating coalesced reuse without
skipping physical-binding checks. This benchmark passes no authorizer callback;
per-caller authorization is covered separately by denied-caller regressions.
