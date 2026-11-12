# Daemon load profiles

This Go command measures a **new private daemon**, using its public socket
protocol. It never attaches to an existing daemon. A manifest's source roots
are read-only inputs: each scenario clones the pinned commits into fresh owned
directories. Configuration, HOME/XDG directories, store, socket, and process
group are private. Edit scenarios call the public `edit.file` operation; their
acknowledgement latency and subsequent indexed visibility are separate samples.

The harness and its synthetic fixtures are checked in. Corpora, daemon binaries,
runtime state, measurements and profiles belong outside the repository.

```sh
go test -race ./bench/load/...
go run ./bench/load run --binary /private/bin/gortex \
  --manifest /private/corpus.json --scratch /private/load-artifacts \
  --profile agent --scenario read --agents 8 --duration 60s --host native
go run ./bench/load delta /private/tag.json /private/main.json
go run ./bench/load schema
```

`--git-sha` supplies a full revision only when the binary lacks Go VCS metadata;
it cannot contradict embedded metadata or override a modified-source build.
The emitted schema and representative **synthetic** report are locked by the
goldens in `testdata/`. Schema version 1 uses `null` with a reason for unavailable
metrics. An unavailable counter is never reported as an observed zero.

## Corpus contract

Supply a JSON array with neutral labels, full commit SHAs, repo-relative file
and symbol IDs, and meaningful search terms. For example (synthetic paths and
revision; replace with real values):

```json
[
  {
    "label": "sample",
    "source": "/private/read-only-source",
    "commit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "file": "sample.go",
    "symbol": "sample.go::Example",
    "query": "Example",
    "proposed_signature": "func Example() int",
    "edit_anchor": "package sample\n",
    "edit_template": "\nfunc {{marker}}() {}\n"
  }
]
```

The source target must exist in the pinned checkout. `ext::` source targets are
rejected before dispatch. The daemon's actual repo prefix is obtained from its
readiness receipt, rather than guessed from a filesystem path. File targets
must resolve inside an owned clone; symlink escapes and scratch paths inside
source roots are rejected.

Editing requires an anchor that occurs once and remains after insertion, plus
a language-valid template containing exactly one `{{marker}}`. The template
must declare that identifier as an indexed source symbol. Concurrent requests
use the same stable insertion anchor without a harness lock around the edit
RPC. Conflicts and parse errors remain errors; the client does not suppress or
rewrite them. Visibility requires an exact `name`/`id` in indexed search
results, not the echoed query or freshly read disk content.

## Scenarios and offered load

`--scenario` selects `read`, `edit`, `many`, `many-edit`, `worktrees`, `cold`,
`restart`, `sweep`, or `all`. Read/edit/cold/restart/sweep use the first manifest
entry. Many-repository scenarios require at least two entries and load all of
them into one store. Worktrees use one private primary and `--worktrees` linked
checkouts. Only the primary is an indexed base corpus; each linked CWD must
produce an exact automatic-worktree view receipt before measurement.

The default `agent` profile waits 2–5 seconds before each step, then issues
3–5 parallel reading calls. `--agents` counts **pure readers**; `--edit-agents`
counts combined agents that edit, observe indexed visibility, and then issue the
reading burst. Both defaults are 4. For exactly N combined edit/readback agents,
use `--scenario edit --agents 0 --edit-agents N` (also supported by `many-edit`
and native paired edit scenarios). This adds no hidden reader population.
`--agents 4 --edit-agents 4` is the separate four-pure-reader/four-editor tail
workload. The counts remain explicit in the report and workload hash.

The PR smoke entry uses a versioned offered-window/drain profile. Read drains
retain version 1. Explicit edit drains (`--pr-smoke-drain-version 2`, with
`--pr-smoke-drain 2m`) offer work for 20 seconds, then stop new logical edits and
reads. Already admitted edits retain their original 120-second call/visibility
budgets, including required visibility probes. The finite drain ends no later
than 140 seconds after phase start; an earlier parent cancellation still wins.
Reports retain admission times, distinct edit/read identities, every attempt,
visibility follow-ups, and the full elapsed drain time. Expired or canceled work
remains a failure. This opt-in changes the workload hash; ordinary runs and the
frozen reference profile retain their existing phase-clipping behavior.

An explicit corpus `edit_tool: "edit.write"` selects whole-file write intent for
the same narrow Go, TypeScript or Python intrafile-call fixture. It additionally
requires `edit_initial_content` containing the exact pinned initial file bytes.
Before opening probe sessions, the client compares those bytes with its clone.
The complete offered requests are precomputed: write number N contains the
initial file plus additions 0 through N. The client does not reread or regenerate
content after another edit, serialize offered arrivals, or change retry intent.
Out-of-order overlapping writes can overwrite later changes; ordinary stale,
readback, refusal and evidence failures remain in the full denominator. The same
seven oracles, per-edit sessions, own-ACK readbacks and receipt accounting apply.
Write, symbol and batch receipts are not interchangeable. `edit.symbol` uses
an exact splice within an actual indexed declaration. The one-file `edit.batch`
adapter offers exact whole-file before/after strings and checks the durable
transaction, shared acknowledgement and per-file graph ticket separately.
Implementation controls and a one-edit diagnostic do not establish the
100-edit, reference, traffic-matrix or no-regression gates.

The optional `edit_kind` selects `append_leaf`, `rename`, `delete`,
`modify_body`, `modify_caller_body`, `change_signature`, `add_crossfile_call`
or the Go-only `relocate`; omission retains `add_intrafile_call`. These are explicit fixture cells, not arbitrary
source transformations. A leaf must be a numeric zero-argument function with
no incoming or outgoing calls in complete relation results. Rename/delete
fixtures precreate each unique `GortexEditProbe…Before` declaration in the
pinned `edit_initial_content`; offered splices target that exact declaration.
Whole-file write/batch requests retain the precomputed offered prefix, including
out-of-order or lost-write outcomes. No state is regenerated after dispatch.

Rename retains the seven positive new-symbol probes and adds a distinct
old-source absence probe/readback. Delete requires the exact scoped producer
`symbol not found` refusal, complete nontruncated graph/search collections and
a surviving declaration. A missing body, transport failure or empty partial
page cannot stand in for absence. The same bound prober proves the original
symbol exists during untimed setup; only replies sent after the actual edit
wire start can earn post-edit absence. Expected refusals retain their own raw
identity, endpoint and report count; they are not ordinary successful RPCs.
These new operation cells require actual runtime evidence before acceptance;
other kinds and unsupported combinations fail explicitly.

Body/signature fixtures retain an adjacent following declaration. Their relative
start-line gap proves the changed body even when concurrent earlier edits shift
both declarations. Signature changes preserve valid zero-argument callers;
TypeScript/Python coarse signature metadata alone is not parameter proof.
Crossfile-call fixtures require two root files, exact imports where applicable,
and an independently admitted callee in the other file.

Go relocation uses `edit_tool: "refactor.move"` (also `move_symbol`) with
`edit_kind: "relocate"`, distinct root Go files in the same package, and pinned
`edit_destination` / `edit_destination_content`. The offered source declarations
already exist; no client-generated declaration is substituted for a real move.
The seven destination/call probes plus explicit old-source absence must all
succeed. Immediate own-ACK readbacks check the destination file, new symbol,
old-symbol refusal and surviving source file before timing queries begin.
Standalone move observations retain one shared RPC A and each file's actual
write and publication windows. A direct move reconcile and an independent
public-read repair keep separate route records and actual request causes; a
later direct no-op cannot supply an earlier reader's work. Exact coalesced
execution identities are evaluated once while retaining both route records;
conflicting windows refuse. Each measured window must partition its own actual
owner and endpoints. A reader window can begin before the move RPC, remains
non-additive with shared A, and must accept the original file bytes after that
file's writer return. Scheduled background is explicitly inapplicable, not a
measured zero; physical mutation and batch receipts cannot supply missing move
clocks. Baseline controls without observations can establish public
functional completion while graph receipt proof and phases remain unavailable.
Cleanup uses one public two-file batch only after completed outcomes, then
verifies both original byte strings. Unsupported language, package, path and
operation combinations refuse explicitly. These implementation controls do not
establish live relocation timing or the broader traffic/reference gates.

`edit_expect: "refusal"` turns a TypeScript or Python relocation fixture
(`edit_kind: "relocate"`, `edit_tool: "refactor.move"`, distinct root source and
destination files of one language) into a live refusal cell. The actual move is
sent, and the cell passes only on the producer's exact scoped refusal
(`unsupported language: move_symbol only supports Go files (got "<path>")`),
followed at most by the known session notes, with the same session's immediate
file readback unchanged, the original symbol's source present, and both files'
bytes unchanged on disk. Its latency is the `relocation_refusal` readback; the
record has no probes, receipt, phases or restore batch, and the post-window step
only rechecks the bytes. Go relocation is supported and is never offered as a
refusal.

`edit_output: "toon"` opts this fixture into TOON for the offered edit ACK,
visibility probes and immediate readbacks. Omission or `"json"` retains the
existing JSON plan and request identity. The decoder evaluates the requested
text; a valid structured JSON sibling, JSON fallback, malformed/truncated TOON
or unsafe numeric value cannot supply format credit. Unknown modes refuse, and
`"gcx"` refuses as unavailable (below).
Timing-status queries, initial presence/callee admissions and cleanup are
separate JSON controls, preserving exact generation/nanosecond arithmetic.

TOON traversal subgraphs expose `total` plus explicit truncation and emitted
rows; they omit the JSON `total_nodes` and untrimmed full `total_edges` fields.
The harness does not insert those missing producer fields. Complete-row checks
require the known producer operation, exact declared node total, explicit
nontruncation and actual emitted collections; missing, clipped or contradictory
rows remain failures. File summaries retain their full explicit counts, signature metadata and end lines.
Compact traversal rows omit signature/end-line metadata. The pointed TOON
`change_signature` fixture instead traverses its unchanged following caller and
checks the changed relative start-line gap plus actual call relation; full
summary and editing-context signature checks remain separate. Actual Go,
TypeScript and Python producer controls establish that wire shape. TOON source implementation and offline
controls do not establish live, reference, traffic or performance coverage.

`edit_output: "compact"` requests compact text (`output.compact`) only from the
four probes whose operation has a compact producer: file summary, symbol
search, callers and call chain. Source, editing-context and text probes,
readbacks, the edit ACK and timing controls stay JSON. Compact text is one
`<kind> <qualified name> <path>:<line>` row per symbol (symbol search may add
` (in <owner>)`), and subgraph answers may follow their rows with a truncation
line, one edge-count footer and concurrency notes; any other line, a missing
final newline or a JSON answer is a decode failure, and an empty answer is zero
rows. Because the rows carry no node IDs, edge rows, signatures or freshness
fields, compact proves presence only: the marker (or, from the marker's call
chain, the callee) as a top-level function row at its owned fixture path, with
a nonzero edge footer for relations. Truncated rows never prove absence. Plans
refuse compact for every kind except `add_intrafile_call` and
`add_crossfile_call`.

`edit_output: "cli"` sends every oracle request, with the same lowered facade
arguments, through one owned `gortex call <facade> --json <args> --format json
--index <clone> --quiet` process using the run's config and environment, in its
own process group that is killed at the probe deadline or phase end. The edit,
readbacks and receipt status stay MCP JSON. CLI stdout is credited only as one
JSON object whose numbers stay exact after the CLI's float64 re-encoding; the
`{"tool","format":"text","text"}` prose wrapper is refused, and a nonzero exit
is a probe error with its stdout kept as evidence only. Probe cadence is bounded
by process start, daemon dial and exit rather than 25 ms, and a probe's start is
its process start. Rename, delete and relocation are refused in CLI mode,
because their expected absence must be the exact scoped not-found refusal, and
paired runs refuse CLI probes.

Each report's `output_modes` accounts every mode: `json`, `toon`, `compact`,
`cli` and `gcx`, each `selected`, `not_selected` or `unavailable` with a
reason, with probe answers and decode failures counted by the actual mode of
each probe request, claims fresh, stale answers claimed fresh after the ACK and
pre-commit mismatches. GCX is unavailable: there is no GCX decoder here, and
`read_file` answers a GCX request with TOON, so a GCX answer is never decoded or
credited as TOON. A mode without answers, or one whose payload cannot carry a
freshness claim (compact, GCX), reports its stale claimed-fresh count
unavailable rather than 0.

`--scenario edit-and-probe --profile agent --agents 0 --edit-agents 1
--worktrees 0` selects an initial `add_intrafile_call` / `edit.file` / JSON
accounting cell. Go retains its existing AST validation. The explicit `.ts`
fixture cell accepts only top-level `[export] function Name(): number`
declarations returning a decimal integer or a plain zero-argument call; its
template must make the unique marker return the selected intrafile callee.
`testdata/edit-probe/typescript/main.ts` provides ten initial symbols for the
ordinary warmup gate. Other TypeScript syntax is not supported by this fixture
cell. Its token checks discard comments/strings as proof and require the actual
producer symbol ID and file/path. TypeScript implementation does not constitute
a completed runtime or reference gate. The `.py` fixture cell similarly permits
only top-level `def Name():` definitions with one four-space-indented return of
a decimal integer or plain zero-argument call. Its fixture is
`testdata/edit-probe/python/main.py`; nested bodies, strings and comment-only
calls cannot provide source proof. Both new language cells are implementation
coverage only until their actual runtime checks pass. The finite plan contains 100 precomputed Poisson
arrivals (mean 15 seconds); `--edit-count 1` is an explicit diagnostic only.
Each edit has its own session opened before dispatch, with independent 25 ms
probes and immediate same-editing-session file/source readbacks. Session setup
is outside the edit window. Future arrivals do not wait for earlier ACKs or
probe replies. Canceled/unstarted edits remain in the offered denominator.

`--edit-mean DURATION` explicitly changes the per-editor offered Poisson mean
for `edit-and-probe`, including paired hot-file offers. The default (or `0`)
preserves the original 15-second plan and seeding. For example, `250ms` offers
four edits per second on average from each fixed editor. The plan records its
actual mean and due times; a changed offered rate is a separate measurement,
not evidence for an earlier frozen plan or an accepted performance gate.

`--edit-agents N` (1 to 32) defaults to N fixed editors against one
repository. Each editor offers its own open-loop Poisson stream with the same
15 second mean; editor 0 keeps the one-editor seeding, so `--edit-agents 1`
offers exactly the original plan. The streams merge by due time (ties by editor
index) and the merge order is the global ordinal, so markers and mutation IDs
stay keyed by ordinal and each ordinal is owned by one editor for the whole run.
Plan steps and edit records name that editor. An edit and its immediate
file/source readbacks go through its own editor's session; probes and receipt
status stay on the edit's separate prober session, and run-level status and
stats use editor 0. The singleton restore batch also uses editor 0. A blocked editor ACK does not delay another
editor's arrival. In a singleton cell, editors share one file. `edit.write` carries whole-file
content precomputed in ordinal order, so an out-of-order write would silently
drop another editor's marker until the next write restored it; it is refused
with more than one editor. An out-of-order `edit.batch` fails its `old_string`
precondition and stays a failed record, and `edit.file` anchor insertions do
not depend on order. The 1-edit diagnostic and
the paired no-regression cell stay one editor.

A full gate record may use outcome `proven_superseded` only in an admitted
shared physical worktree. Its optional `superseded_receipt` joins the latest
terminal receipt status (receipt, mutation, RPC, generation and publication
identity) to one exact owned daemon-log observation of a covered completion
without acceptance. Both raw evidence references and hashes remain in the
proof. The original accounting error, refusal and unavailable P/B windows stay
visible; no zero timing is substituted, and the outcome has its own count.
ACK and edit/read correctness, output modes, resources and the full offered
denominator remain required. Accepted attempts, separate checkouts and missing,
ambiguous or capped evidence keep their strict requirements. This additive
report/checker classification does not rewrite historical failed reports.

`--edit-repos 6 --edit-agents 6 --agents 0 --worktrees 0` selects exactly
six distinct pinned repositories. Source checkouts that share one Git common
directory count as one repository. For linked views of one pinned primary,
use `--edit-repos 1 --worktrees 3 --edit-agents 4 --agents 0`; linked counts
3, 7 and 15 give 4, 8 and 16 total checkouts. These routed cells require 100
global offered edits, at least one fixed editor per checkout, and ordinary Go
`add_intrafile_call` / `edit.file` / JSON fixtures. Pure readers, native and
lifecycle senders, moves and other output/tool/language cells are explicitly
unsupported for routed runs; their existing singleton cells remain separate.

The plan pins every route's logical repository, checkout identity, commit,
file and callee. A fixed editor, its immediate readbacks, and each edit's
independent prober and receipt query stay on that checkout's cwd. Linked
checkouts require their own exact public view admission even though their
repository prefix is shared. Linked edit records retain that admitted checkout
ID; a missing or different receipt checkout ID cannot supply terminal graph
or accounting evidence. Each route restores its admitted original bytes
through its own editor and transaction after mutation outcomes settle. The
source repositories stay read-only. Route steps preserve the original merged
editor arrival stream and global marker/mutation IDs; a route with no offered
ordinal is not silently given an extra edit. Implementation and fixture tests
do not establish a completed live or reference campaign.

`edit_sender` selects a native or lifecycle traffic cell; without it every plan
is unchanged, and paired runs refuse it. A native write goes around MCP, so its
record has no ACK, receipt, phases or derived time (each unavailable with its
reason) and accounting `not_applicable_native_write`. It keeps its own write
window and the file's size, mtime and inode before and after. Its immediate file
readback gates `t_readback_ms`, graph-backed readbacks are kept as
`observation_only`, and it completes once every probe is first-correct inside
the deadline. The daemon learns of a native write only through its watcher, so
a fresh claim after one is counted unassessed rather than stale. Native writes
touch only the registered fixture file, or the step's own created file.

- `shell_after_tool` (100 edits, one editor, `edit.file` call addition): even
  ordinals are MCP edits; odd ordinals insert their own marker with an in-place
  truncate-and-write at their due time or the preceding tool edit's own ACK,
  whichever is later. A tool edit without its own ACK makes the shell write an
  explicit dependency failure, and nothing is written.
- `editor_save` (one editor, rename fixture through `edit.file`): the fixture
  predeclares placeholders `GortexEditProbe<seed>P<ordinal>`, exactly the
  marker's length. Each save writes same-size bytes to a temporary file beside
  the fixture and renames it over the file; the placeholder must become absent.
- `create_delete` (100 edits, root-level Go fixture with pinned
  `edit_initial_content`, JSON or TOON): even ordinals create
  `gortex_edit_probe_<marker>.go` in the fixture's package, calling the
  fixture's callee; odd ordinals delete that file once every probe of its create
  was first-correct. A delete's probes start at its own activation, so an
  absence answer sent before its create completed is not applicable to it.
  Deletion is proved by the exact scoped not-found refusal and complete
  search, text and caller collections; a missing file's summary, editing
  context and call chain have no pinned contract and are not probed.
  Created files still present are removed by one timed restore batch.
  TypeScript and Python create/delete are not implemented: a gap, not an
  inapplicable cell.
- `hot_file` (100 edits, `--edit-agents 2`, `edit.file` call addition): MCP
  traffic in which both editors send each pair of edits to the one file at the
  same due time, taken from the single-editor stream. A lost write leaves its
  marker invisible and is a measured failure.
- `restart` (`--edit-count 1`, one editor, no readers, JSON `edit.file` call
  addition): the edit ACK waits for its graph receipt, so the cell does not
  wait for it. While the ACK is outstanding, the edit's status is polled by its
  `mutation_id` every 5 ms; once the bytes are committed and the receipt is not
  yet terminal, the harness stops waiting for the ACK and kills the owned
  daemon. `t_ack_ms` is then unavailable and the abandoned call is counted
  censored, not as an error. An ACK that returns first leaves one status read
  just before the kill to decide: a receipt already terminal there qualifies
  the record `receipt terminal before kill` and it fails. The daemon is
  restarted on the same store and awaited until ready, and the editor and
  prober sessions are reopened. Probes pause during the downtime. First-correct
  times come only from the restarted daemon, from send and from ready, and
  answers before the kill are kept apart. The status after the restart is
  recorded verbatim, the committed marker must be on disk, and the interrupted
  receipt reports no phases, accounting or derived time. The run-level receipt
  after the workload is taken on the reopened editor. Process counters cover
  the pre-kill daemon only.

Each edit-and-probe report names its `role`. A standalone run is the candidate
unless `--edit-probe-role baseline` is given; the paired command assigns roles
by build order and refuses the flag. A candidate still fails any record whose
phase accounting is missing. Every record carries `phases_ms` for each ACK and
patch lap the daemon names, taken from the record's own latest receipt status
(the per-file observation status for batches). A build without `phases_ms`
reports each lap unavailable with that reason, a producer null keeps the
producer's phase state as its reason, and a relocation keeps its per-file move
accounting instead. Missing laps are never zero.

Records also carry client times from edit send: `t_readback_ms` (last correct
immediate same-session readback), `t_file_graph_ms` (source, summary, editing
context and old-source absence), `t_relations_ms` (callers and call chain) and
`t_search_ms` (symbol and text search). A group time is its slowest member's
first-correct time; any member that never answered correctly leaves the group
unavailable. `t_derived_ms` is daemon-clock time from receipt origin to
`patch_derived_finished` in the record's own terminal publication timeline,
because no probe observes the derived step; it is not additive with client
times and stays unavailable for coalesced receipts and relocations.

The report `summary` covers the complete offered denominator: outcome counts
and, for every per-record time, each readback, each `phases_ms` lap and each
oracle's first-correct time, nearest-rank p50/p95/p99/max with percentile
bootstrap 95% intervals (2000 resamples, seeded from the plan seed and the
measure name). A value missing from a record that did not complete (deadline,
run end or failure) is `censored`: the record ranks above every observed value,
so a quantile or interval bound whose nearest rank falls among censored records
is unavailable rather than understated. A value missing from a completed record
(a native write's ACK, a coalesced receipt's derived time), or a measure the
record does not carry, is `not_applicable`. An interval needs at least 2, 20 or
100 ranked records for p50, p95 or p99 and is unavailable below that. Nothing is
sampled as zero.

`--agents R` (0 to 32) adds pure readers on their own sessions, running the
ordinary reading plan with agent pacing. With readers, the phase opens with a
30 second no-edit window before the edit origin; the offered edit schedule,
relative to that origin, is unchanged, and the run budget includes the window.
Each read is classed when it is issued: `no_edit_baseline` before the origin,
`under_edit` while any sent edit's receipt is pending, otherwise
`between_edits`. A sent edit is pending until the harness first observes its
receipt terminal; an edit without one to observe (a failed edit, or a native
write) stays pending until its loop ends. An edit sent while a read is in
flight does not reclass it, so a read can straddle an edit's start by up to its
own latency. The report's `reads` table gives, per operation and
class, calls, busy refusals, errors, censored calls and the latency quantiles
of successful calls, in which calls cut off by their deadline or the run end
rank as censored; busy and failed calls are not applicable. Paired runs refuse
readers.

The report retains raw call identities, independent ACK/physical/requested
background accounting, and missing evidence. RPC return timestamps delimit
latency; raw JSONL persistence runs on owned per-edit writers with separately
reported encoding/write overhead. The initial tiny-fixture raw budget is
8 MiB per edit, shared by accepted pending and in-flight bytes. One wake signal
coalesces small-row bursts without a separate message-count refusal. Exhaustion or persistence failure is
`harness_evidence_incomplete`, never product failure or a successful check.
Reference inputs require a reviewed evidence budget before execution. One timed
public batch restores the file after the measurement window; unknown/pending
mutations prevent that cleanup rewrite. The native paired path uses the same
cell machinery; other cells and reference acceptance remain separate gates.
An owner-matching immutable physical return with explicitly missing or invalid
deep laps ends that edit as failed coverage once its graph and read evidence
have arrived. Receipt terminality alone still waits for late physical evidence.
A complete native duplicate P keeps collecting until independent publisher R
and requested-background B arrive; absent late evidence is not terminal proof
of missing work. A validated immutable R phase failure can end failed coverage.
An original-publication-bound `requested_background_unavailable` is also
terminal failed coverage, not a measured zero or a successful accounting check.
Unknown or foreign diagnostics do not stop late-evidence collection.

Requested B supports ordered native timer handoffs only when each previous
timer's actual `Stop` succeeds before its callback starts. Watcher, path and
actual generation bind each handoff; generation gaps never infer event counts.
Repeated native arm, quiet, stop and coalescing-control intervals accumulate
into fixed named scalars, without interval history. A callback that already ran,
an observed successor or retry/backoff remains unsupported, with its first
actual immutable invalidation reason retained. Later events cannot erase an
already accepted historical B window. Such a diagnostic may end collection
before other late evidence arrives; it does not prove that evidence stays
unavailable forever.

`mutation_status` can separately expose `read_assisted_publication` when an
already-needed read repair executes for the exact pending committed bytes.
Its monotonic window covers the actual coordinator execution, with that
drain's owner generation and bounded reader admission causes. A reused wire ID
is not a unique publisher identity. This window can overlap the original
receipt's quiet wait; it is not added to A, B or a later no-op watcher P.
Its own `phase_accounting` measures the actual gate, parse, apply, search,
resolve, mtimes, derived and optional analysis operations, with an online
`repair_control` interval around them. The harness validates that independent
integer partition before accepting it as publisher deep coverage; the no-op
P's unexecuted deep values remain null, never retroactively filled from R.
R's internal `requested_receipt` matches the original `publication_phases.key`
and requested ticket, not the public commit `receipt`, caller `mutation_id` or
coalesced physical winner. The relationship to additive requested-background
accounting remains unavailable. Missing, failed, broad, evicted, rebound or content-mismatched
observations do not supply that evidence. Fresh same-byte inert validation
does not claim the publication performed by an earlier operation. One observed
independent partition does not establish the 100-edit accounting, paired
no-regression, reference or larger language/tool/native acceptance gates.
R is retained only by the existing bounded mutation ledger. Evicting the
original publication recorder's index entry does not erase an accepted ledger
observation or make canonical P/B available; ledger eviction clears R, and a
freshness rebind refuses the old relation. Earlier returned snapshots remain
immutable.

Edit scenarios require at least one editor; zero total or negative counts are
rejected. Zero pure readers are allowed only for `edit`/`many-edit`; `all` needs
both populations, and other scenarios need at least one reader. Inactive editor
counts may be zero outside edit scenarios. The floor still permits at most four
total active agents (for example, zero readers plus four combined editors, or
two plus two). `saturation` is an explicit closed-loop opt-in. Both profiles back
off on busy refusal `-32002`: 50, 100, 200, 400, 800 ms, then at most 1 second.
Busy refusals never cause an immediate retry loop.

Sustained reading uses ten operations: search symbols/text; read source/file/
editing_context/summary; relations callers/usages; explore context; change
impact. It omits terminal localization and mutually exclusive audits. Each
load agent owns its session. The sweep instead preopens one independent session
per operation, before timing, so one terminal localization cannot invalidate
another tool's probe. Its two expensive analyses also occupy different bursts.
Both the actual operation subset and session model are recorded and compared.
The sweep defaults to 15 repetitions per operation and a separate explicit
10-minute `--sweep-duration` budget. `--sweep-repetitions 1` is available for
short wire smoke tests. Each localization repetition starts a new logical task;
session creation is excluded from call timing. Every failed/censored repetition
is retained, and the repetition count/budget are part of the workload hash.

The typed workload records scenario, reader/editor counts, linked checkouts,
duration, product deadline, seed, profile and normalized operation plans.
Absolute clone paths and binary revisions are excluded from its hash. Attempts
retain agent/step/burst slot/request hashes, errors, refusals, and censoring.
Retries contribute to logical successful-call latency, including backoff;
think time contributes to the phase throughput denominator.

The 21-operation benchmark contract is:

| Domain | Operations |
| --- | --- |
| search | symbols, text, files |
| read | source, file, editing_context, summary |
| relations | callers, usages, dependents, implementations |
| explore | context, outline, localize |
| change | impact, verify, tests, detect |
| trace | call_chain |
| analyze | hotspots, dead_code |

The sweep inventories advertised operations, marks other operations
`outside_scope`, and records missing benchmark operations explicitly. It does
not claim to exercise every administrative, provider, forge or mutation API.
`relations.dependents` carries a symbol selector. `change.verify` carries the
source-backed symbol ID plus the manifest's proposed signature in canonical
`source.changes`; these handlers do not accept ignored file/symbol selectors.
The two mutually exclusive analyses run in different deterministic sweep bursts.
The ordinary concurrent reading workload retains analysis refusals as errors.
Missing, unsupported or failed benchmark operations prevent a complete sweep.

Steady phases require both queryable and enriched readiness, source-backed
per-root target checks, and a current analysis consumer followed by the public
`gortex://communities` durable-generation receipt. Enrichment is not analysis
publication. Cold startup polls readiness while the boot-time configured repos
index; queryability and enrichment are separate observed upper bounds with a
recorded polling interval. Older daemons without `loaded_roots` retain their
legacy receipt and must still pass source-backed per-root checks.

## Startup admission

`--scenario startup` (never part of `all`) measures what agents see while a
daemon starts. Every manifest repository is configured; `--worktrees` linked
checkouts of the first one are discovered through it. Probing starts before
the first daemon start, so the window before the socket exists is observed
too. `--seed-store <file>` copies a store (for example one written by an
older schema) into the private run first, which makes the initial boot an
upgrade; the source is only read. `--restarts N` adds N warm restarts. Each
boot lasts until every configured repository is ready and enriched, plus
`--duration`.

Every second, from the primary, the first linked worktree, and a directory
whose `.gortex.yaml` names a workspace that a private `servers.toml` roster
claims, the probe opens fresh sessions: `initialize` (the instructions
variant is recorded), `search_symbols` and `resources/read
gortex://index-health` on the legacy surface, and `workspace(operation:
"index")` on the compact one. The roster's only server never listens, so a
routed call falls back to the daemon itself. From the same directories it
asks the PreToolUse hook about a native Read every second, and runs `gortex
call search_symbols` through a pipe (from the start) and through a pty
(`script(1)`, once the socket answers), each until its first success in that
boot. Every answer keeps `error_code`, `retryable`, phase, percent, latency,
exit code, and whether any refusal text advises `gortex track` or
`gortex repos reconcile`. A watcher polls the readiness probe every 100 ms
and, for 2 s after the primary registers, the checkout coordinator count.

`startup-probe.jsonl` holds every answer. The phase gate `startup_probe`
counts answers per surface and directory in three windows (no socket,
loading, ready) and records each boot's time to ready and time to first
success. Headline metrics include `startup_repo_not_tracked_before_ready`,
`startup_nonconforming_loading_refusals`, the track-advice and hook-deny
counts, `startup_restart_ready_to_first_ok_p95_ms`,
`startup_restart_time_to_ready_p95_ms`, `startup_initial_time_to_ready_ms`,
`startup_initial_first_ok_ms` and `startup_coordinators_within_2s_min`.
Restart figures are unavailable unless every restart produced them, and
`delta` compares two startup reports like any other.

## Measurements and artifacts

One JSON file contains per-operation successful p50/p95/p99/max, successful
calls/s, busy attempts, errors and incomplete samples. Workload and visibility /
readiness probes have separate denominators. A call clipped by an earlier
phase deadline is `phase_censored`, including when its return is delayed beyond
both deadlines. That is not a product timeout. An acknowledged edit not seen
before the actual product visibility deadline is `never_visible`; a clipped
visibility wait is recorded separately.

Daemon CPU and write counters use process endpoints. macOS records CPU,
resident size, `phys_footprint`, `ri_logical_writes` and disk I/O writes
separately. On macOS, `ri_logical_writes` reads the internal-volume ledger;
external-volume writes use a separate ledger that this API field does not
include. Use internal-volume source/store/HOME for comparisons that require
this logical-write meter, and record runtime and cache volume placement.
Mixed-volume activity is not a whole-process logical-write total. Physical
disk I/O writes remain a separate meter and must not replace logical writes.
Linux records procfs CPU, RSS, PSS, RssAnon and disk/syscall writes;
the latter is not relabeled as macOS logical writes. Phase memory/store/WAL
peaks are sampled at 500 ms. The post-workload memory sample is explicitly
labeled after a 2-second settle period, not proof that background work is idle.
Whole-client CPU is a conservative upper bound on refusal-handling CPU, not
overlapping per-request CPU attribution.

Status and `gortex://stats` receipts preserve view-build counters, WAL pending
frames, checkpoint estimates, reclaim counters, graph size and runtime heap.
Daemon stdout/stderr and available daemon/crash logs are copied after stop,
including failed runs. Panic, SIGABRT and OOM log-line counts remain distinct
from authoritative cgroup OOM-kill counters.

`gate.patch_write_intervals` collects only exact top-level JSON
`graph patch complete` records from the copied logs. It retains source file,
kind, delta class, logged end and inferred start (log timestamp minus duration),
WAL-reset flag, WAL/main bytes, total/per-table rows, transactions and incident
edges. Nested phase logs are excluded. The duration is captured before final handling
and counter capture; its inferred start is not the exact counter-start time.
Log timestamp formatting adds skew too. Window/boundary classifications are
therefore inferred and cannot establish exact counter-window attribution.
Inferred outside/boundary events stay in the report but are excluded from totals. Duplicate copies of
one log record in different sinks are counted once. Overlapping contained
intervals are counted explicitly: each is process-wide activity and can include
the same writes or background work, so their sum is **not exclusive edit cost**.
Zap sampling and log rotation can also omit records; these totals describe
observed intervals, not a complete accounting assertion.

The same evidence records successful edit acknowledgements and indexed-visible
edits separately, with amortized interval-counter totals per acknowledged edit.
Missing older-version counters, invalid records and WAL resets remain
unavailable rather than zero. The raw events permit inspection when an
aggregate is unavailable. `gate.process_counter_window` records the independent
process meter endpoints; the phase timestamps and paired group window identify
the workload interval. Whole-process meters include writes outside observed
patch intervals and deferred I/O, while overlapping intervals can overcount:
the two must not be forced to reconcile. Both macOS logical and physical disk
write meters are retained for comparison.

One session per loaded root opts into public graph invalidations before the
workload. Subscription availability/response is recorded explicitly; an older
build without support is not treated as having zero invalidations. Received
notifications retain client timestamps and raw payloads, with a 1,000-record
per-session cap and an explicit dropped count. Notifications supplement the
public readiness/publication gates rather than replacing them.

Standalone `run` and fixed `smoke` accept `--retain-read-source-failures` for
private diagnosis. It defaults off. When enabled, selected `read.source` calls
observe their actual transport frames; only failed attempts retain a
`failure_rpc` envelope. The first eight failures per report can retain exact
base64 request/response bytes, including line framing, with a combined 64-KiB
limit per envelope. Oversize, missing, partial, unsupported and cap-exhausted
observations have explicit statuses and never claim exact bytes. Offered
handshake options and response-observed facade metadata are separate fields.
Raw frames may contain source or private paths: keep these reports private.
Enabled healthy calls incur transient observation work; failure encoding occurs
after the measured return timestamp. This option adds no RPC or retry and does
not change request-plan identity, deadlines, pacing or success criteria.

Completed private stores/clones are removed by default while JSON, logs and
profiles remain. This prevents earlier scenarios' retained stores from using
the next scenario's disk budget. `--keep-state` is only available for native
diagnostics. A forced stop kills only the invocation's owned process group;
the JSON records its duration and `force_killed` flag.

## Host profiles and reproducibility

`native` means **native/uncapped diagnostic**, never reference or floor evidence.
The harness validates existing host limits and never provisions or restarts a
container, VM or global service.

Reference requires Linux cgroup v2 with effective 8-CPU/16-GiB limits and actual
host capacity at least that large. Limits are minima through all visible
ancestors, not just a nominal child configuration. Scratch must be on the
intended 60-GiB benchmark filesystem: 57–60 GiB capacity allows up to 5% format
overhead and rejects an accidental 30-GiB OS disk. Free space and declared quota
values are not proof. Floor requires effective 4-CPU/8-GiB limits and one corpus
with at most 1,000 tracked files, four total agents and four total checkouts
(including the primary). Its `all` omits many-repository scenarios explicitly.
Use a separate pinned floor manifest, rather than trimming the reference corpus.

Record the kernel, cgroup limits and actual page size. `memory.swap.current`
is current swap occupancy, **not** swap I/O. When available, `memory.stat`
`pswpin`/`pswpout` page-count deltas are multiplied by actual page size.
Unavailable per-cgroup swap I/O stays unavailable; host-global `/proc/vmstat`
is never substituted. OOM kills come from `memory.events`. Whole-scenario
cgroup counters include startup and stop; workload-only endpoints are separate.

## Coupled native comparisons

On a noisy native host, start two or three private builds together with `paired`:

```sh
go run ./bench/load paired --build tag=/private/bin/tag \
  --build main=/private/bin/main --manifest /private/corpus.json \
  --scratch /private/paired-artifacts --host native --scenario edit \
  --agents 4 --edit-agents 4 --duration 60s --out /private/paired.json
```

An optional third `--build candidate=/private/bin/candidate` uses the same
protocol. Each build must have clean embedded Go VCS metadata; archive builds
without that metadata can use standalone `run --git-sha` instead. All private
builds pass their own readiness/publication/target gates before the shared
workload barrier. Failure or cancellation before the barrier cancels and cleans
up every owned daemon; no partial startup becomes a measured successful group.

For each logical request, every build receives the same normalized request and
corpus identity back-to-back. The first build rotates independently within each
tool/role category. The report retains tuple hashes, dispatch/completion times,
all failures and matched latency ratios; the summary uses the **median of
matched per-request ratios**, never a ratio of raw percentile summaries. Failed,
censored or mismatched pairs remain explicit exclusions. Workload and probe
categories are separate. These are coupled comparisons, not independently
measured saturation throughput or reference/floor capacity. Paired mode rejects
saturation, reference/floor, cold/restart and CPU-profile options; use standalone
runs for those measurements.

Edits apply the same logical marker to each build's independent clone. Each
build has its own acknowledgement and visibility deadline. Visibility ends at
that build's first successful indexed probe completion, before waiting for any
later build's probe. Further paired probes for already-visible builds remain
probe-only evidence and cannot change completed visibility observations.

Process counters bracket one common group window with each actual snapshot
endpoint retained. Client CPU and shared cgroup counters are reported once at
group scope, rather than attributed repeatedly to every daemon. Preserve raw
reports and failures, repeat on the same host before p95 repeatability claims,
and balance standalone build order per scenario. Coupled ratios do not replace
independent scaling measurements.

The initial Go `add_intrafile_call` / `edit.file` / JSON accounting cell can use
the same dispatcher with exactly two builds, baseline first and candidate
second:

```sh
go run ./bench/load paired --build main=/private/bin/main \
  --build candidate=/private/bin/candidate --manifest /private/corpus.json \
  --scratch /private/paired-artifacts --host native --scenario edit-and-probe \
  --agents 0 --edit-agents 1 --worktrees 0 --edit-count 100 --seed 42 \
  --out /private/paired.json
```

Both daemons run concurrently with independent clones, stores and sockets. They
share the exact precomputed offered Poisson schedule. Edits and all seven oracle
requests use the existing rotating back-to-back dispatch and matched per-request
median ratios. Each build retains one separately opened prober session per edit;
setup precedes the measurement window. A blocked ACK or probe does not serialize
later offered edits. Actual lateness, errors and canceled/unstarted entries remain
in the full offered denominator.

Each build starts its immediate file/source readbacks after its own ACK, without
waiting for its partner. Those readbacks, receipt polling and stats are build-local
and **unpaired**; their latencies are excluded from matched ratios. First-correct
times are coupled paired-probe observations: waiting for the partner response can
delay the next probe, so these are not independent 25 ms freshness-budget results.
Missing baseline phase instrumentation stays unavailable, while candidate A/P/B
and applicable independent R accounting remain required. Evidence loss cancels
that edit's paired probes and is a failed/censored observation, never a product
deadline or a successful pair. This initial cell does not establish reference
capacity, the larger tool/language/native matrix, or a no-regression verdict
without the actual qualified paired run.

`delta` rejects incompatible workload/corpus/target/host-limit identities rather
than printing an unqualified verdict. It lists every comparable timing metric
more than 15% slower, without introducing budgets or CI pass/fail lanes.

`--cpu-profile 60s` records an owned loopback pprof capture during an eligible
8-reader or primary-plus-4-linked-worktree phase (duration must fit the phase).
Inspect artifacts with `go tool pprof -top -nodecount=30 profile.cpu.pprof` and
retain the exact run JSON alongside them. Real reference/floor baselines,
repeatability, throughput scaling, and CPU-cause attribution require measured
evidence; fixture tests or native diagnostics do not satisfy those gates.

## Memory diagnosis

`--gauges` (standalone runs only) subscribes one extra session to the daemon's
`daemon_health` channel at a 10-second cadence for each measured phase, from
the moment the daemon reports ready to the end of the phase, so the series also
covers the warmup gates and the level after enrichment before any load. Every
push becomes a row of `gate.gauge_series`: the daemon's own gauges (Go heap,
`sys`, released heap, resident and proportional set or macOS footprint, SQLite
page cache, live checkout coordinators) next to the harness's external sample
of the daemon process taken as the push arrived. On Linux the sample adds
`children_pss_bytes`, the proportional set of every descendant of the daemon
(language servers and other helpers, found through each thread's
`/proc/<pid>/task/<tid>/children`), and in a cgroup it adds the group's
`memory.current`, which charges those children too, with its anonymous and
file-backed parts from `memory.stat` (`cgroup_anon_bytes`,
`cgroup_file_bytes`). The daemon runs its health ticker only while a session is
subscribed, so the gauges cost nothing in runs that do not ask, and a
`--gauges` run compared with the same run without it measures what the gauges
cost. `--heap-profiles` (with `--gauges`) also saves two heap profiles per load
window: one at load start and one at the highest footprint the series sampled
during the load (or at its end, when that is higher). The peak captures run
inside the measured window, so leave the option off when latency is being
compared. If the OS/build has no process-footprint reader, the peak profile is
unavailable with a reason in the phase gate or worktree step, and no peak
artifact is advertised. Load-start and collected idle profiles remain available.
Heap profiles are taken as of the daemon's last GC; no collection is forced
during load.

Live gauges and heap profiles are refused for `startup-probe`, which records
its own boot observations without a gauge series, for `idle-stop`, whose
measured interval requires every MCP session closed, and for the edit-and-probe
`edit_sender: restart` cell, which replaces the process during its measured
phase. Ordinary `--scenario restart` is supported: its series starts only
after the replacement daemon is ready. `all` keeps independent per-scenario
series; steady read/edit runs and worktree-step measurements remain supported.

`--worktree-steps 0,1,2,4` (worktrees scenario, ending at `--worktrees`)
measures what each linked worktree costs. One daemon starts with the first
count of linked worktrees and grows through the rest. At each count it serves
the reading agents for `--duration` from fresh sessions spread over every
checkout, then sits idle for `--duration`; a session stays parked in each
linked checkout throughout. Each idle window opens with one forced collection
and a heap profile taken right after it, so every idle level follows a load of
its own count and holds what the checkouts retain rather than the load's
garbage. `gate.worktree_steps` records each count's load and idle windows,
load-window process peaks, per-tool latency and heap profile paths, and the
gauge series spans the whole run. The phase session model names the counts
(`per_agent_per_worktree_step:0,1,2,4`), so `delta` refuses to compare a step
run with an ordinary worktrees run or with a step run over other counts.
Inspect a profile with
`go tool pprof -sample_index=inuse_space -top worktrees.step3.heap-load-peak.pprof`,
and what one more worktree retains with
`go tool pprof -sample_index=inuse_space -top -base worktrees.step2.heap-idle.pprof worktrees.step3.heap-idle.pprof`.

### Live checkout discovery containment

`--scenario worktree-discovery` starts with the first two pinned repositories
in the manifest. It keeps their reading/editing sessions open while adding
`--worktrees` detached checkouts alternating the two repositories during the offered
window. Each added checkout gets separate root and subdirectory query sessions.
Use `--agents 0 --edit-agents 8` for eight combined edit/readback agents, or
`--agents 8 --edit-agents 0` for a read-only control. Editing requires the same
validated edit fixtures as the ordinary edit scenarios.

`phase.gate.checkout_discovery` records every added root, first request, first
observed exact worktree view, and the resulting visibility lag. That lag is an
upper bound on catalog identity allocation; it does not claim an unobserved
earlier allocation time. Each cwd has its class, call/error/refusal counts,
complete-call read latency distribution and separately counted censored reads.
Attempts retain the actual session cwd and structured RPC `error_data`, including
the refused root, family, reason and retryability when supplied by the daemon.
A refusal or completed error outside a currently pending checkout fails the run.

Run an otherwise identical `--worktrees 0` control to compare primary read p95.
Keep both reports and all refused/censored attempts. Zero refusals without any
`pending_refusals` is not evidence of fault containment under a stuck discovery.
A hung-discovery fixture, upgraded-store fixture and the reference-host runs
remain separate acceptance cells; this scenario does not manufacture them.
All measurements require an isolated host window and fresh owned clones/stores.


Discovery additions alternate the primary and unrelated control families. Each
new root keeps its own family query and prefix for the exact-view witness;
foreign-family capacity refusals fail the containment gate even while that
new root is pending. `--discovery-stall-first` installs a daemon-only Git shim
for the first new primary-family checkout. Its child ignores TERM and retains
stdout until explicit release or a 60-second bound. The harness releases it
before daemon shutdown and retains child/root/operation events. A missing
stall or pending-refusal observation fails this cell.

For upgraded fixtures, `--discovery-prepare <descriptor.json>` prepares owned
roots without opening a store or launching a daemon. Add
`--discovery-existing-worktrees 20 --discovery-existing-nested 5` to create five
in-repository and fifteen sibling checkouts of the first family. The unrelated
control is retained, so descriptor counts are two primaries plus twenty linked
checkouts; selected-family counts are one plus twenty. The descriptor records
actual paths, Git common directories, commits, source and configuration hashes,
and a random owner token. Preparation requires a separate exclusive fixture
producer to populate and upgrade the store at those exact paths.

For a lapsed topology, combine `--discovery-prepare` with
`--discovery-topology <declaration.json>`. This preparation-only opt-in accepts
`{"mode":"literal_lapsed_v1","roots":[...]}`. Each root declares an `id`,
`role`, `corpus_kind`, and `family`; primary and dedicated roots also select a
manifest entry with `corpus_label`. IDs are unique path components. Linked
roots may set `nested: true`. Entry order does not determine ownership.

The declaration must contain three primaries (one code repository, one PDF
repository, and one SQL dump repository), twenty code-family linked roots,
four additional dedicated worktrees (two Go, one PDF, and one SQL), and two
stale code-family checkouts. Use `corpus_kind: "gortex"` for the code primary
and its linked/stale roots. Each dedicated worktree has an isolated Git family
with an explicitly recorded, unconfigured support clone without a checkout.
The descriptor records 27 live roots, two stale registrations to capture, and
four nonlive support roots. Pinned HEADs may repeat across independent families.
Git checkout kind is separate from served graph view: configured dedicated
worktrees serve their indexed base graph, while automatic linked roots serve
worktree views.
Graph owner IDs remain null until observed by the separate fixture producer.
Preparation opens no store, does not remove stale roots, and does not extend
the existing two-family discovery resume workload. Broader manifests without
the explicit opt-in are refused.

`--discovery-resume <lineage.json>` consumes that prepared root once. Lineage
must bind the descriptor, old and upgrade binary/source pins, schema-44 main
and WAL hashes, and structured normal-stop/catalog-identity receipts. Unknown,
live, changed or relocated identities are refused. The stopped immutable
snapshot remains inside the owned root; the destination store must be absent.
The existing seed copier copies only main/WAL files, then both source and
copied hashes are checked again. There is no SQL or catalog path rewriting.
These discovery-only flags preserve the startup scenario's `--seed-store`
contract. A copied fixture or synthetic parser control is not upgrade acceptance.
