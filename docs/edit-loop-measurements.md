# Edit-loop timing observations

`mutation_status` adds `phases_ms`, `phase_states`, and `phase_timing` beside
the existing `publication_phases` timeline. The timeline keeps its existing
schema, origin and timestamp meanings. The new projection is read from the
same immutable snapshot, with no new graph/store queries or edit-path I/O.

This is partial instrumentation. Presence of every key does **not** mean every
lap has been measured, or that the phase-sum acceptance criterion is met.

The optional `phase_timing.patch_accounting` object is a separate partition of
one exact accepted winning physical attempt P. Its actual setup, inspection,
executor-control and finalization intervals pause around the same actual
admission/lane/deep operations. `patch_analysis` names the observed dataflow,
affected-plan and reference-fact block; resolver meaning stays unchanged. This
uses fixed ordered scalar state, never elapsed-minus-known work or interval
history. Early deep snapshots/logs remain immutable and executor-contained while
outer finalization is open. The actual-return completion publishes a fresh copy
only after exact winner/membership and agreement checks with those early values.

`observed_partition_complete` requires valid integer bounds, all applicable
requested deep laps and a disjoint complete P partition. Missing deep evidence
fails it even if broad control totals numerically fill the wall. `sum_state`
remains `partial` for a valid local partition pending actual acceptance tests;
`missing_laps`, `unclassified_gap`, `invalid_order` and `not_observed` explain
refusals. The residual is diagnostic. Initial quiet/retry backoff, dispatcher
queueing and discarded attempts are not part of this smaller winning-P wall.
Its `background_coverage` remains `not_established`: it does not establish full
per-edit background, ACK, actual100-edit or5%/performance acceptance.

`execution_disposition: native_mtime_duplicate` records the actual native
watcher receipt-match return before executor entry. Only that observed branch
can mark its unexecuted deep and executor-control laps `not_applicable`, with
null durations. Contradictory execution evidence is invalid; an unknown branch
still requires the requested deep evidence. A complete partition of this small
no-op P does not measure the earlier graph publisher. Independent read-assisted
publication R remains non-additive, and its missing deep coverage stays missing.

An accepted `read_assisted_publication` can carry its own `phase_accounting`.
Its window keeps the actual coordinator execution start and return, after
repository lane/batch admission. The same fixed scalar collector measures the
real gate, parse, apply, search, resolve, mtimes, derived and optional analysis
operations. `repair_control` records the actual authority/setup, interstitial
and return work, pausing around those named operations. No pre-R lane, debounce,
P admission or finalization is borrowed. The immutable accounting copy retains
its coordinator generation and is linked only after actual owner, applied bytes
and existing ledger acceptance checks. Missing or contradictory laps refuse
completeness. These R durations must never be added to overlapping A, B or P.
The collector keeps waiting when complete no-op P precedes R retention or B
publication. Only a validated immutable phase failure (or the existing finite
collection bound) ends that wait as incomplete; absent late evidence is not a
proof that the missing work will never be observed.

## Duration evidence

`phases_ms` always lists these keys for a found mutation receipt:

- Acknowledgement: `dispatcher_queue`, `middleware`, `parse`, `disk_write`,
  `publish`, and the explicit additions `reindex_wait`, `edit_control`,
  and `caller_result`.
- Patch: `debounce`, `lane_wait`, `gate_wait`, `patch_parse`, `apply`,
  `search_lap`, `resolve`, `mtimes`, and `derived`, plus the additive actual
  `cohort_admission` and `batch_admission` envelopes.

`edit_control` records actual leaf preparation, validation, formatting, receipt
and response-control intervals online. It opens at leaf entry, pauses around
each existing parse/write/admission/wait operation, resumes at that operation's
actual end, and closes at leaf return. Repeated chunks use fixed scalar totals
and ordering checks. A parse guard that is not applicable keeps that parse
verdict; its actual dispatch interval is control work. No remainder becomes
measured work. Pre-leaf refusal has no invented leaf interval.

`caller_result` is the actual handler-finish through caller-facing reply-finish
interval: bound-handler delivery/finalization before transport serialization.
It starts where `reply_prepare` ends. A detached reply before physical handler
finish has unavailable/outside-window control or caller-result evidence; no
post-reply work is clipped into ACK. The original RPC identity, per-file receipt
ownership and requested phase meanings remain intact. Subtotals stay labelled
partial pending complete-window controls and actual100-edit validation. Observed
production totals and containment use integer nanoseconds before conversion to
public milliseconds; fractional rounding never authorizes tolerance or clipping.
A missing actual reading retains conservative unavailable/partial evidence.

A duration is a monotonic millisecond number, or `null`. `phase_states` gives
the evidence for each key:

| State | Meaning |
| --- | --- |
| `measured` | Both boundaries were observed in order; zero is valid. |
| `pending` | The start was observed, but the end has not been observed and the receipt is not terminal. |
| `not_observed` | Boundaries are absent, including on paths without a publication timeline. |
| `not_applicable` | A graph-refresh/background record has no source-mutation acknowledgement. |
| `invalid_order` | Observed offsets contradict the interval; the duration stays null. |

The acknowledgement projection measures `disk_write` (`disk_write_started` to
`receipt_committed`) and `publish` (`receipt_committed` to `ticket_enqueued`)
for MCP-edit timelines. The latter includes post-commit capture and ticket
admission. It does not include waiting for the admitted publication to finish.
The projection uses nanosecond offsets before converting to milliseconds,
rather than subtracting rounded timeline values or parsing wall-clock strings.

Canonical single-file commits retain their own disk-start boundary and timing
record through the existing bounded commit ledger. Admission binds the original
watcher sequence before debounce can wake; only the existing ticket-result
consumer records completion. A newer coalesced application can serve that
original ticket, but does not become its ticket identity, a checkout generation,
or invented per-receipt patch laps. Native events have no MCP acknowledgement.

Immediate-success edits keep the existing empty public freshness-receipt field;
their commit receipt can still render its retained timing. Different files or
versions within one request own distinct disk intervals even though they share
the middleware origin. Their receipt walls overlap and must not be added as RPC
time. Timing pointers are cleared on ledger expiry/cap eviction even when source
evidence keeps a commit object alive; a late callback cannot resurrect them.
Rebinding a commit to a replacement freshness receipt does not borrow that
replacement's timing or retarget the original record. Atomic batch/journal
timing and response/transport timing remain separate pending work.

Canonical point execution additionally collects `cycle_started`,
`patch_execution_started`, and `patch_execution_finished` on a local attempt.
Only the exact successful winner at the existing guarded completion can absorb
those stamps, outside the watcher lock and before result delivery. Older
coalesced receipts receive completion without the winner's execution spans.
Retry, superseded and stopped attempts discard their collectors; native paths
without an instrumented outstanding waiter allocate no collector. Outstanding waiter ownership is
released by the existing completion/stop paths rather than a tracing registry.

When a native successor serves instrumented predecessors, status may additionally
expose `coalesced_publication`: a separate bounded recorder entry for that actual
physical winner, with explicit requested and applied generations. The original
receipt's accepted-P fields remain unavailable. Each actually served retained
predecessor may refer to the same winner; these are references to shared work,
not additive per-edit spans. When the actual native arm reading is available,
the independent winner's origin is that observed arm start and `debounce` uses
that winner's arm-to-callback pair. Otherwise its origin remains callback entry
and quiet wait remains unavailable. Original receipt timestamps never move. P
still measures callback entry through actual return, excluding debounce, and
`background_coverage` remains `not_established`. Eviction, absent ownership,
failure or supersession leaves this independent evidence unavailable; weak links
cannot retain evicted entries or rebind a previously served receipt.

For the specifically observed case of one requested timer canceled before its
callback and one native successor, `requested_background` may expose an
independent B window: accepted scheduling-call entry through the actual serving
attempt's return. MultiWatcher routing, admission checks, mutex wait and setup
precede the first timer arm in the genuinely measured
`scheduling_admission_setup` lap. Request parsing and disk work before scheduler
entry are outside B; A and B can overlap and must not be added together.

The fixed partition measures initial timer-arm work, its canceled quiet wait,
the successful timer-stop call, actual coalescing setup, successor timer-arm work,
successor quiet wait, and the already validated physical P partition. These use
actual ordered endpoints, not wall time minus known laps. The B wall is measured
independently from its own start/end. Original accepted-P fields remain null.
Earlier snapshots remain unchanged; only a fresh late observation can contain B.
The winner's whole `debounce` pair is not added again beside B's measured
successor arm/quiet split. Status renders related P and B from one fenced
immutable observation, so complete B cannot accompany an older missing P.

Prior callback entry, Stop=false, repeated resets, retries, multiple observed
predecessors, missing/reversed boundaries, incomplete P or lost membership leave
B unavailable. A newer event before guarded acceptance rejects the attempt;
later work after acceptance cannot erase the historical serving identity during
final release. No additional active-owner registry retains completed waiters. This case-specific
partition keeps `sum_state:partial` and does not establish the100-edit or broader
background/performance acceptance gates.

The execution pair surrounds the common point executor, including its actual
standalone or MultiWatcher parse/apply/resolve/derived implementation. It excludes
before-image inspection and post-executor event work. `cycle_started` names the
accepted runnable attempt, which may follow retries. These are outer execution
events, not a pure debounce/lane/gate measurement, a full patch wall, or a split
of nested deep phases. No deep checkout-builder phase is relabelled onto the
contextless canonical pipeline, and internal storm/atomic batches remain outside
this observation. These executor events alone do not establish a full patch wall;
no phase-sum or CPU attribution acceptance follows from these events.

For an accepted initial canonical point attempt, `debounce` measures the actual
initial timer arm through callback entry. It excludes the callback's claim lock,
cohort admission and repository lane. Only the exact claimed generation's
successful winner publishes the immutable pair; older coalesced receipts do not
borrow it. A retry's timer is backoff, so a winning retry leaves `debounce`
null/`not_observed` rather than borrowing its failed initial attempt's interval.
The existing physical-attempt log carries the same optional quiet-wait evidence
under its attempt ticket, before final receipt validation.

For an accepted canonical point attempt, `lane_wait` uses the actual repository
lane channel wait and grant, after batch admission and its hook. The pair is
separate from executor-contained laps and is published only for that exact
winner, with enqueue/cycle/executor containment validated. Canceled waits have
no grant; rejected or coalesced attempts do not borrow another attempt's pair.
This does not measure the cohort semaphore, batch lock, or full patch lifetime.

Two additive admission envelopes use actual calls: `cohort_admission` surrounds
the watcher's bounded cohort-slot admission, and `batch_admission` surrounds the
repository batch RLock/admission hook when either exists. They include their own
call/lock/hook work and are not pure repository lane waits. Complete pairs require
exact winner ownership, ordered enqueue/cycle/executor/terminal boundaries, and
cohort-before-batch-before-lane ordering. An absent batch lock/hook remains
unobserved, rather than a fabricated measured zero. These pairs also extend the
same immutable attempt log; setup, inspection and finalization gaps remain
unclassified, so accounting is still partial. Initial debounce and retry backoff
precede P and must never be summed against P's smaller wall.

The same optional attempt-local collector now follows the actual standalone or
MultiWatcher point executor to its precise derived-tail call. The raw pair
`patch_derived_started` to `patch_derived_finished` becomes `phases_ms.derived`
only with observed admission and enclosing executor boundaries in monotonic
order. Resolver, reference-fact, semantic and metadata work before that call
is excluded. A branch that bypasses the derived call has no pair and remains
null/`not_observed`; an actually reached zero-duration call is measured zero.
The existing exact-winner absorption remains the only publication point, so
failed/retried/stopped/coalesced sibling attempts cannot publish these spans.
There is no ambient Indexer tracing state or additional history.

## Canonical cumulative patch laps

A bound canonical attempt also collects a fixed set of scalar totals and
observation counts, without retaining per-file spans or a tracing registry.
Repeated chunks and same/cross-repository resolver calls contribute their actual
elapsed intervals. First-to-last timestamp envelopes are not used as a sum.

| Lap | Actual observed boundary |
| --- | --- |
| `gate_wait` | The call to `reach.BeginTopologyMutation`, until acquisition returns. |
| `patch_parse` | Each chunk's preparation: source read, parse admission, extraction and fingerprint preparation; excludes prior-graph loading. |
| `apply` | Deletion and staged graph application, explicitly split before/after the search refresh block. |
| `search_lap` | The explicit search/FTS refresh block; structural FTS deletion stays with graph replacement in `apply`. |
| `resolve` | Actual deferred same-repository/name and cross-repository resolver calls; intervening dataflow, reference-fact and semantic work is excluded. |
| `mtimes` | Source-version receipt validation/mtime persistence, deletion-mtime pruning and MultiIndexer metadata publication. |
| `derived` | The precise derived-tail call above. |

The collector rejects overlap, inverted clocks, incomplete begin/end counts,
and intervals outside the original admission/executor/terminal window. Its
scalar derived interval must match the existing raw derived pair, so they cannot
be counted twice or conceal an overlapping subtotal. The legacy
`publication_phases` JSON fields stay unchanged; its first-wins timeline alone
cannot reconstruct a repeated lap. `phases_ms` is projected from the same
immutable in-process receipt snapshot with its accepted scalar evidence.

A reached zero-duration operation is measured zero. A bypassed or unobserved
operation remains null. The legacy combined parse/apply fallback makes the full
`patch_parse`, `apply` and `search_lap` totals unavailable for that attempt,
including any earlier measured chunks; it is not assigned a fabricated split.
Other independently proven laps remain available. Uninstrumented transitions,
contract work, semantic/dataflow work and notification costs stay in the
publication residual. This does not establish complete 5% sum coverage.

For observed canonical attempts, the existing `graph patch complete` line adds
`attempt_<lap>_ms`, `attempt_<lap>_state`, `attempt_<lap>_count` and the original
`attempt_ticket`. Its `patch_lap_scope` is
`physical_attempt_before_receipt_validation`: the log happens before final
watcher winner validation and does not claim that a superseded attempt's timing
will be published into any receipt. The encoder formats an immutable copy and
never samples clocks or a live collector. Native/unobserved patches keep their
original log fields, with no new timing snapshot, phase clock or encoder.
No extra patch line is emitted. Total instrumentation overhead remains
unmeasured and needs the prescribed no-regression runs.

## Separate lifetimes

`phase_timing.clock` is `monotonic` when a publication or acknowledgement observation exists, otherwise
`unavailable`; `coverage` is currently `partial`.

- `ack_wall_ms` is null until the outer bound tool call returns a caller-facing
  result/error. Its denominator is entry into `boundToolHandler` through that
  return, **before** daemon serialization/socket delivery. The separate
  `acknowledgement` object names this scope and shared RPC identity. It is not
  the harness's send-to-receive wall. Legacy publication origin remains MCP
  middleware entry and does not move to this earlier boundary.
- `patch_wall_ms` is the accepted canonical timer callback entry through actual
  mutation return, after the existing completion, Done delivery and cohort
  release. It excludes prior debounce/retry backoff and timing-publication
  overhead. It may finish after the receipt terminal and caller ACK; those
  walls are independent and must never be summed. An early snapshot can have
  a terminal receipt but a null patch wall until physical return. A later
  fresh immutable observation appears only for the exact still-retained
  accepted attempt; absent recorder ownership, eviction, failure or a foreign
  attempt leaves it unavailable. Previously returned snapshots do not change.
  The early patch log keeps its original scope; one matching completion line
  records the final wall and attempt retry ordinal. This endpoint evidence
  remains partial accounting, without a complete phase partition or5% claim.
  One coalesced patch may serve multiple receipts.
- `publication_wall_ms`, when terminal boundaries exist, measures
  `received` to `ticket_completed` or `ticket_failed`. It is a receipt interval,
  not the acknowledgement time or the physical patch wall.
- Without a shared RPC acknowledgement, `publication_unattributed_ms` is that publication wall minus the measured,
  disjoint acknowledgement and observed patch-lap intervals. Patch intervals
  must be wholly inside the admitted executor and publication window. Invalid ordering leaves
  it null. The residual contains
  uninstrumented work and waits; it does not classify them. In particular it
  does not measure the acknowledgement's default reindex wait.
  When shared RPC laps replace per-file acknowledgement values, this residual
  is unavailable: subtracting them from a different-origin per-file interval
  would be invalid.

Acknowledgement and asynchronous patch execution can overlap. Their walls
must not be added. Likewise `cycle_started` to `admitted` is not labelled pure
lane wait, and existing nested structural-apply spans are not summed as
disjoint patch laps. Complete sum accounting needs explicit boundaries at
those operations; it cannot be obtained by renaming the timestamp gaps.

Pending/unobserved durations must remain missing in benchmark inputs. They
must not become zero-valued successful observations. This initial projection
does not claim the 5% phase-sum, 100-edit, runtime-overhead, or real read-back
acceptance gates in #1005.

## Store-level WAL observations

The store's `WALReclaimStats` adds cancellation/refusal counts for the first
consumed writer-parked, announced-intent or original-operation cancellation
cause of a final non-reset reclaim attempt. A positively observed parked writer
has precedence when both demand signals are positive; this is observation
precedence, not a claim that only one signal caused contention. Multiple polls,
writer-credit stages and helper cleanup do not become multiple attempts.
Successful final resets, normal helper cleanup, cycle/bulk withdrawal and a
child budget expiration are not counted as other caller/shutdown cancellation.
The existing common writer-waiting error remains available through `errors.Is`.

`ObservedPeakWALBytes` is a sampled high-water mark since the shared store core
opened. `WALPeakObserved` distinguishes an actual size observation from missing
evidence; in-memory/unsupported stores do not fabricate a zero WAL observation.
It reuses existing successful size reads and adds no file/stat/SQL work to edits.
Generation handles share the same peak; independent stores do not. Truncate,
shrink and status reads never reset it. This is a lower bound on instantaneous
physical peak and cannot be subtracted into a warm-run peak after startup.
These store observations do not by themselves establish per-run peak or
cancellation/CPU attribution acceptance.

`graph_stats` and `gortex://stats` share a `wal_reclaim_observation` block. Its
scope is `physical_store_since_open`, independent of the selected graph/view or
repository. The optional store capability is queried once per payload through
its existing `WALReclaimStats` snapshot: a short counter mutex and atomic loads,
with no file stat, WAL-index read, SQL query or writer wait. Unsupported/masked
stores report `snapshot_available=false`, a reason and null counters/peak;
there is no unwrap or SQL fallback. A supported zero counter is numeric zero.

The fields `cancelled_writer_parked`, `cancelled_write_intent` and
`cancelled_other` preserve the store's final-attempt observation semantics.
`observed_peak_wal_bytes` is null while `wal_peak_observed=false`. An actually
observed empty WAL is `0` with `wal_peak_observed=true`; it is different from
missing evidence. `peak_scope=sampled_since_store_open_lower_bound` explicitly
excludes an exact instantaneous or warm-run peak. In-memory stores have no WAL
size observation even when their counter snapshot is supported.

`observation_epoch` reuses the Server's immutable observation binding and
`epoch_scope=server_binding`. It is **not** a durable store-open identity or a
per-run reset token. Production binds the Server's physical store at construction;
generation handles share the store core and its observations. A literal Server
without that binding has an empty epoch. Do not join samples across a changed
binding/reopen, or subtract two high-water marks into a run peak. Stats reads,
WAL resets and generation changes never clear these counters.

Daemon status JSON also exposes these new causes and nullable peak under
`storage.wal_reclaim`, extending its already-collected snapshot. This adds no
status collection call or I/O; the new MCP block does not call the daemon's
file-size/WAL-index collection path. Other daemon storage/status fields keep
their existing semantics.

## Private acknowledgement ownership (transport pending)

Measured source-tool calls now own one private, call-scoped acknowledgement
record at the `boundToolHandler` boundary. It reuses the existing five-tool
classification. Nested calls on the same Server borrow its identity, while a
different Server owns a different record. Reads allocate no acknowledgement
record or context value and take no acknowledgement clock sample.

The origin is `mcp_bound_call`, before the deadline firewall; it is neither
dispatcher entry nor the legacy `toolReceivedAt` middleware origin. The latter
and `publication_phases` remain unchanged. Caller response-ready freezes when
the outer bound call returns. Actual handler completion is observed separately,
including a detached physical commit that finishes later. Those walls overlap
and must never be added together or presented as client receipt/socket delivery.
Missing endpoints stay pending, missing origins stay unavailable and invalid
ordering stays invalid; none becomes measured zero. Snapshots own their scalar
values and never evaluate a live clock during formatting.

Physical file receipts written by one RPC now share that private observation
through the existing bounded mutation ledger. Both expiry and capacity eviction
clear the reference, even when pending-source evidence keeps the physical receipt
alive. A late handler can finish its call-scoped observation but cannot reinstall
it on an evicted record. Raw/unobserved writes have no MCP acknowledgement.

The ownership does not attach pointers to `batchTransactions`, whose existing
map has no bounded eviction. Actual parse/disk/enqueue
laps and additive-sum validation are partial work. It does not change the validated actual-completion counters or CPU/WAL
sampling. Read/native-path and mutation overhead still require measurement.

The daemon's existing allocated slot lease now retains immutable original
admission-entry and actual channel-grant endpoints. A source-tool observer
copies them once; no context, registry, classifier or per-tool transport parse
is added. Replay/cache followers and unbounded/no-lease paths have no grant
observation. Releasing capacity or attaching another grant cannot replace the
original pair; a refused first attempt cannot borrow a later attempt's grant.
Nil-lease primitives sample no clock. Successfully admitted leased requests,
including reads, add two monotonic clock samples and existing-lease mutex work;
that overhead is unmeasured, with a primitive benchmark supplied for later
measurement rather than a performance claim. Current HTTP paths without that
admission lease remain unavailable. Private queue and grant-to-observer
preamble snapshots precede the bound-call origin: scoped wire exposure is
described below, and they must never be added to its existing subtotal
or silently widen its wall. No dispatcher policy or capacity changes here.

The private fixed acknowledgement accumulator now observes the actual pre-write
parse-gate call in `edit_file`/`write_file`, the `AtomicWriteFile` call (ending
before committed-hash/receipt bookkeeping), and canonical/checkout refresh
enqueue calls. Repeated chunks occupy fixed slots rather than a span history.
An unsupported parse gate is not applicable; an instrumented raw commit that
did not call a gate does not acquire a fabricated parse duration. Other tools'
actual parser and durability work is not inferred from these slots.

Each slot retains begin/completion counts, actual elapsed execution and interval
validity. Snapshot `ObservedMS` is completed execution; `AckMS` is an alternative
value present only when every applicable chunk is valid and contained in the
frozen reply window. A late interval keeps its full execution observation and
no acknowledgement duration. Overlap, missing ends and invalid containment
produce explicit states, not clipped or residual-derived timings. The
`LapsDisjoint` flag alone proves neither coverage nor the 5% sum requirement;
middleware, waits, reply preparation and unsupported paths remain unclassified.
No additional patch log line is emitted by acknowledgement projection.

Atomic batches now reserve one scalar `batch-ack-*` reference only in the
original execution arm. The timing payload occupies a distinct batch-only kind
inside the same bounded mutation ledger and shares its existing expiry/cap;
there is no second registry or timer. Transaction state retains no timing
pointer or snapshot, and the durable manifest/version is unchanged. Duplicate,
status and recovery replies cannot replace the original RPC wall. Eviction or
restart makes the private lookup explicitly unavailable.

The batch-only ledger kind cannot enter physical receipt/replay/freshness/path
indexes, source-evidence registration, physical status/listings, commit notes,
publication records or disk/graph outcome setters. It supplies no invented
disk or freshness verdict. The private lookup checks the scalar reference and
fixed-size transaction identity before borrowing an owned timing snapshot.
Batch text splices have no parser lap. Forward file writes/removals and the
deduplicated target-directory sync now have concrete timing boundaries, as
described below. Journal, backup, rollback, cleanup and planning work remain
unclassified; complete critical-path coverage and the measured 5% sum gate
remain pending.

## Receipt acknowledgement wire and critical-path denominator

`mutation_status` adds `acknowledgement`, with a bounded shared `rpc_id`, scope,
caller response-ready and actual-handler walls/states, per-lap begin/completion
counts, and owned duration values. Its nine fixed acknowledgement keys include
explicitly unavailable dispatcher queue/preamble when no bounded lease was
observed, and an unavailable transport boundary.
The existing fifteen-key `phases_ms`/`phase_states`
shape remains: its six acknowledgement keys use the shared RPC observation
when available; its nine patch keys retain their independent per-receipt
evidence. `publication_phases` stays byte-for-byte the existing snapshot. Legacy
records without an acknowledgement retain independently valid publication laps
and explicitly unavailable RPC ownership, never an origin borrowed from polling.

`reindex_wait` now surrounds the existing passive receipt selects in canonical
and batch paths. It excludes enqueue/capture, outcome formatting and synchronous
inline reindex work. The timers, cancellation behavior and dispatch-slot release
are unchanged. Immediate completed tickets can have a real reached near-zero
wait; an async checkout admission that never waits has no invented duration.

Batch edit/status JSON or TOON responses carry a response-only
`acknowledgement_receipt` reference; compact text carries the same reference.
Query it through `mutation_status`. This explicit `batch_acknowledgement` variant
has no disk/graph verdict, path or physical mutation listing entry. Durable
transaction status remains authoritative for those facts. Eviction, restart or
wrong checkout/incarnation returns unavailable timing rather than a different
call's clock. Neither this envelope nor a timing snapshot enters the journal.

The current additive set is only valid, complete and disjoint measured chunks
of middleware, parse-gate, disk-call, ticket-admission, passive-wait and reply
cleanup work. Middleware measures the existing bound-call observation origin
(after source-tool classification) to the first wrapped leaf invocation. It
includes the firewall handoff and real view/repository/mutation preparation;
those nested costs are not added again. Reply preparation measures the leaf's
return (including its own defers and any in-leaf encoding) to the existing actual
handler-completion endpoint, after middleware decorators, view/repository release
and activity cleanup. It excludes final protocol serialization/socket delivery
and the later caller firewall return. A pre-leaf refusal has measured middleware
and not-applicable reply preparation; an unwrapped raw invocation has neither.
Nested facade/raw calls cannot replace the first leaf boundaries. Completed
operation laps must lie inside the leaf, disjoint from both outer intervals;
misplaced laps invalidate the subtotal instead of being double-counted.

A phase crossing
the frozen response boundary has only its complete execution observation and
no acknowledgement duration. `observed_ack_subtotal_ms` sums only eligible
chunks. `ack_unattributed_ms` is a labelled unclassified gap to the bound-call
wall, never a phase; invalid counts, overlaps or totals leave both null instead
of clipping. `sum_state=partial` cannot satisfy the 5% gate, even if a particular
gap happens to be small. Several file receipts with one `rpc_id` must be
deduplicated; adding their RPC walls would multiply the same call.

For the eventual 5% check, acknowledgement critical-path stages must cover the
same caller-facing wall once each; background patch stages cannot be added to
that wall. The current bound-call wall is a measured subset of the harness's
monotonic edit-send→ack-receive wall. The missing admission/serialization/client
transport boundaries and other handler work keep that full comparison open.
The physical patch subtotal has its own accepted-attempt patch wall; the receipt
publication wall is a third, overlapping lifetime. No combined acknowledgement
plus asynchronous-patch sum is asserted against any of these denominators.

### Concrete batch/move boundaries and admission envelope

Atomic batch forward source-file writes, removals and the existing deduplicated
target-directory sync are separate `disk_write` chunks. Journal/backup/rollback
and later cleanup durability remain unclassified, not nested into the same
subtotal. A failed forward call still has observed work; it does not imply a
successful commit. Canonical batch set admission, point fallback and checkout
batch admission now measure their actual scheduler call only. The original
Background/WithoutCancel scheduler lifetimes, synchronous fallback work and
shared passive wait policy are unchanged; fallback reindex execution is not
called queue admission. A later status/recovery RPC cannot modify the bounded
original batch ACK.

Move source durability surrounds each existing `AtomicWriteFile`, excluding
parent-directory preparation. Explicit Go parser calls cover source, target,
package-clause inference and caller rewriting. `parse_scope` identifies these
as `explicit_go_parser_calls_formatting_unattributed`: `go/format` internally
combines parsing and formatting and remains unclassified, not falsely reported
as pure parse. File writes/edits name their actual old/new parse gate. Symbol
and atomic batch text splices explicitly report not-applicable parse with null
time; unsupported non-Go moves retain their tool refusal and no parser time.
The requested tool/language matrix therefore contains unsupported combinations;
these source controls do not claim 120 successful combinations or a benchmark.

Queue and grant-to-observer preamble values are exposed with
`dispatcher_scope=queue_and_preamble_precede_bound_call_subtotal` when the
original lease proves them. They are **not** added to `observed_ack_subtotal_ms`
or subtracted from its bound-call wall. A separately named
`dispatcher_to_caller_response_ready_ms` measures original bounded admission
entry to outer caller return, still before transport serialization/delivery;
its state distinguishes pending, invalid and unavailable. The existing
`caller_response_ready_ms` and `phase_timing.acknowledgement_wall_ms` retain
their original after-classification bound-call origin. These are alternative
envelopes, not additive walls. No partial subtotal becomes a complete 5% proof.

## Logical source-operation witness (transport pending)

The MCP Server privately counts actual completion of the five measured source
operations: `write_file`, `edit_file`, `edit_symbol`, `batch_edit`, and
`move_symbol`, including their `edit`/`refactor` facade spellings and selector
inference. These counters span that Server's sessions and views. The daemon's
SharedServer serves its clients through the same Server; the counters are not
per selected repository, per client or exclusive process-CPU attribution.

A fixed request-local witness runs inside the actual handler execution, including
when the deadline firewall has already answered the caller. A nested handler on
the same Server borrows its owner; a distinct Server has distinct counters.
Normal reads do not allocate a witness. Hooks only observe existing successful
forward source-write/removal seams and already-read before images: no additional
source reads, graph/store queries or process samples are introduced. A raw caller
without a witness also skips the added byte comparison.

| Private counter | Actual evidence |
| --- | --- |
| `completed` | At least one proven source change, the whole source operation committed, and the actual handler returned a nonnil, non-tool-error result without a Go error or typed committed-journal warning. This is source completion, not graph freshness or acknowledgement delivery. |
| `committedWithError` | The complete source effect landed, but the actual handler result was nil/error/`IsError`, or a typed `committed`/`committed` batch receipt carried a journal warning. Committed bytes are not labelled unapplied. |
| `failedAfterSourceEffect` | Forward source effects were observed, but the operation did not reach complete commit. This includes rollback and retained/conflicted partial effects. |
| `rolledBack` | The subset above with typed batch `DiskStatus=rolled_back`. Rollback writes are not counted as another edit. |
| `unknownSourceComparison` | An otherwise clean complete write lacked readable prior bytes, so a logical content change cannot be established. It is excluded from clean completed edits instead of guessing a no-op/change. |

The first four outcome classes above are exclusive except for the named rollback
subset. A successful filesystem call is positive effect evidence; absence of a
witness does not prove that an opaque I/O error left every byte unchanged. Existing
typed disk/transaction verdicts remain authoritative. Failed-source counts cover
observed forward effects, not a new filesystem failure-detection mechanism.

The original batch handler performs physical writes, directory durability and
journal publication synchronously. A caller can stop waiting while that handler
continues its deliberately non-cancellable commit. Only that original new
transaction owns source effects; duplicate/status/recovery replies own no
new source operation. The typed Status/DiskStatus distinguish aborted, rolled
back or conflicted outcomes from committed source plus a terminal-journal warning;
`err == nil` and a non-tool-error response alone are insufficient. Multi-file
batch and move writes contribute one logical operation, not one per file.

Idempotency replay, status-only calls, dry runs, unchanged rewrites and refusals
before source effects do not increment completed edits. Native editor stimuli
remain harness-owned: filesystem events/coalesced watcher patches cannot identify
a logical native edit and are never added to these MCP counters.

Process CPU sampling is separate from these operation hooks. Protocol fixtures
are not installed-client or performance evidence.

## Process CPU/count stats window

`graph_stats` and `gortex://stats` both add `process_edit_window` beside their
view-derived graph fields. Its `count_scope` is `server_all_sessions_views` and
its `cpu_scope` is `whole_process_including_background`. Process CPU includes
reads, indexing, other sessions, unfinished work and any other Server in the
process. It is not exclusive CPU attributed to the selected graph or an edit.

An immutable `epoch` identifies this Server's observation window. At the end of
construction, before handlers become callable, it captures process CPU between
two zero-count snapshots. A literal/uninitialized Server reports no baseline;
a failed baseline is retained rather than silently replaced on a later read.
No stats read resets counters or the epoch.

`baseline` and `sample` expose raw cumulative `cpu_seconds`, monotonic
`sample_started_seconds`/`sample_finished_seconds` within that epoch, and full
`counts_before`/`counts_after` brackets. A normal stats read takes exactly one
process CPU sample. Unavailable CPU is null with `cpu_available=false` and a
reason; a known zero CPU sample is a valid zero. Unix uses current-process
user+system Getrusage; Windows uses GetProcessTimes kernel+user durations;
unsupported platforms report unavailable.

`cpu_seconds_per_edit` divides the process CPU delta from that same baseline
by clean completed source operations from the matching zero-count baseline.
It is numeric only for available, ordered CPU/clocks, a stable complete count
bracket, at least one clean completed edit, and no committed-with-error,
partial-effect or unknown-comparison outcome in the window. Otherwise it is
null with `average_state=unavailable` and an explicit `average_reason`.
A measured value is labelled `measured_process_window`, not exclusive edit CPU.
The rollback subset is never added again as another operation.

Raw samples remain available even when that since-baseline average is null.
A harness can compare later warm-run endpoints only with the same nonempty
epoch, valid stable brackets and an eligible outcome delta. Native logical
operations still require the harness's own completed-stimulus denominator.
Sampled since-store-open WAL peaks cannot be subtracted into a warm-window peak.
No run-reset API, per-edit/process CPU syscall or tracing registry is introduced.
Instrumentation cost, real client read-back, complete phase sums and the full
workload matrix remain unmeasured acceptance work.
