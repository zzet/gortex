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

`delta` rejects incompatible workload/corpus/target/host-limit identities rather
than printing an unqualified verdict. It lists every comparable timing metric
more than 15% slower, without introducing budgets or CI pass/fail lanes.

`--cpu-profile 60s` records an owned loopback pprof capture during an eligible
8-reader or primary-plus-4-linked-worktree phase (duration must fit the phase).
Inspect artifacts with `go tool pprof -top -nodecount=30 profile.cpu.pprof` and
retain the exact run JSON alongside them. Real reference/floor baselines,
repeatability, throughput scaling, and CPU-cause attribution require measured
evidence; fixture tests or native diagnostics do not satisfy those gates.
