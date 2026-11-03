# Performance budget checker

The default command consumes repeated, pinned #996 report-v1 files and
[`../budgets.yaml`](../budgets.yaml). The `collect` subcommand runs the merged
load client sequentially against private owned daemons; `plan` starts nothing.
Neither provisions runners nor infers missing observations. The wire mapping is pinned to
`95b7982bf1853b24bd963a0701aa163d9a72a2de`. The policy and input index reject
unknown fields. Metric and scenario names are closed tables in `specs.go`.

```sh
go test ./bench/check-budgets
go run ./bench/check-budgets --budgets bench/budgets.yaml \
  --inputs reviewed-inputs.json --lane smoke --out budget-results.json
```

The output is a before → after table and an optional new JSON evidence file.
Any breach, missing/null metric, wrong unit, omitted safety/censor count,
incomplete report, unavailable prerequisite or incomparable measurement exits
nonzero. Existing output files are never overwritten. The separate
`perf-gate.yml` workflow publishes readiness, explicitly named **not acceptance**.
Its `smoke` job runs the five-step PR smoke and the same-job microbenchmark
comparison on `ubuntu-24.04-arm`, but only when the repository variable
`PERF_PR_SMOKE_ENABLED` is `true`; while it is unset, GitHub shows the job as
skipped. The smoke job is report-only: job-level `continue-on-error: true`
keeps measurement, preparation and artifact failures from failing the PR.
Owned-host nightly jobs are likewise skipped unless their reviewed
runner/config variables are enabled. Several smoke rows still have no exact
observation, so the strict checker reports a failure for those rows. The
report-only job preserves that failed evidence without enforcing acceptance.
Readiness cannot satisfy a required performance acceptance check.

## Repeated evidence and comparison identity

An input index has `schema_version: 1`, full `candidate_revision`,
`main_revision`, `tag_revision`, a default `repeats` of at least three, and
`hosts` mapping reviewed host classes to exact observed machine IDs. `runs`
contains `{role, host_class, scenario, repeat, report}` records; paths are
relative to the index or absolute. Roles are `candidate`, `main`, and `tag`.
The shipped tag is pinned to v0.64.10 / defa9f8e. `scenario_repeats` can declare
larger planned counts, notably `{"idle-stop-20":20}`. A row's `min_repeats`
cannot reduce the three-repeat minimum. Every planned repeat must be present;
missing or failed repetitions are not discarded. Reusing a report/run identity
does not constitute additional repetitions.

All roles within a matching repeat must have the same observed host/limits,
corpus commits and logical targets, actual normalized workload and request-plan
hash. Different owned source paths are expected and are excluded from corpus
identity. Repetition seeds may vary, but only the seed may vary across repeats;
it must agree across roles in that repeat. The load32/load8 denominator permits
only the intentional agent-count difference and still requires matching seeds.
Missing or non-hex plan hashes fail. No other load/scoping difference is silently
normalized. Paired native reports and saturation reports are rejected as formal
capacity evidence rather than relabeled as independent agent runs.

The policy has 837 rows: 689 block and 148 guard. Its 38 PR smoke targets use
`hosted_arm_smoke`, exclusively for the issue's `ubuntu-24.04-arm` lane. This is
the public standard four-CPU runner, rather than reference-capacity evidence.
The harness reports actual native Linux ARM64 CPU/memory/disk observations;
the consumer requires four logical CPUs, positive observed memory/disk values,
and current GitHub-hosted Linux ARM job attestation. Advertised 16-GB memory
and 14-GB disk are not fabricated exact byte observations. All 38 targets,
units, comparisons and repetition minima are unchanged. This profile is illegal
for nightly/release policy or checking. See the
[official runner inventory](https://docs.github.com/en/actions/reference/runners/github-hosted-runners).

Reference Linux still requires actual validated cgroup/host observations: 8 CPU,
16 GiB memory and a 57–60 GiB usable filesystem. Native Mac class assignment
requires a separately reviewed runner inventory; the checker binds it to the
exact Darwin ARM64 machine and reported limits, without claiming to discover
whether a host is reference or large. Linux PSS and Mac physical footprint are
distinct fixed memory observations, compared only within the same host class
and measurement identity. Neither silently substitutes RSS when unavailable.

Each repeat contributes its own p95/p99/p50 or counter observation. The checker
uses the median of at least three repeats, not a pooled percentile. A ratio row
uses the ratio of repeated medians. Hard edit/stop maxima, zero-safety counters
and correctness floors apply to every repeat; a median cannot hide one bad
run. Complete reports must retain successful stop evidence, zero crashes and
uncensored/error-free observations. Busy attempts remain in the busy-fraction
denominator, including refused attempts whose eventual retry succeeded.
Linux OOM safety requires the authoritative `scenario_cgroup_oom_kills` event
delta alongside zero log counts. Missing counters fail. Mac authoritative
OOM-kill evidence is an explicit prerequisite; zero OOM log lines are not
substituted for zero kills.

## Limits and named allowances

`block` uses its absolute target. In smoke, p95 observations also fail when their
median is more than 20% worse than main; `read_p95_vs_main` expresses the read
comparison without inventing an absolute one-repo p95 ceiling.

`guard` starts with the shipped median × 1.15. Only a disk/memory cell whose
pinned main median already exceeds shipped × 1.15 selects main instead. This
selection is independent of the candidate. A missing shipped observation cannot
trigger fallback. Named additive allowances then add their declared amounts.

The WAL-reclaim allowance is a **guard limit**, not an extra 2 GiB and not an
absolute cap: the resulting limit is `max(noise-adjusted baseline, 2048 MiB)`.
The table/JSON retain the effective added headroom. A baseline already above
2 GiB keeps its noise allowance; the WAL entry adds zero headroom in that case.
The pending-edit and dispatch-queue allowances add 64 MiB each only where
explicitly named/applicable, with their owning releases recorded.

A guard can retain a known future absolute `target`, ignored until its mode
changes to `block`. Guard allowances are not applied after that promotion.
An unknown future target is explicitly null. Unresolved `owning_release` is
explicitly `unassigned`, legal only for guards; those rows cannot be promoted
without resolving ownership. No measured baseline numbers live in the policy.

Known scheduling evidence: #1065 owns 15 MiB/edit at 1/4/8 agents in v0.64.15;
#1061 owns the 8/16-worktree peak in v0.64.14; #1076 owns distinct 60-second
idle/Go-Sys observations in v0.64.16; #1088 owns noncold WAL in v0.64.17; #1092
states the 1 MiB/min idle-write goal in v0.65.0. The conditional 6 MiB/edit
diff-on target from #1075 is **not** made the default: it depends on 14 clean
nightlies. #997 names v0.64.14 for cold writes while #1073's actual milestone is
v0.64.15; the policy uses the concrete owning issue's v0.64.15 milestone and
records that cross-reference conflict. Its future absolute target remains null
until the 20/40/45 GiB cells have matching measured topology definitions.
Generic peak/2-second-idle ownership
remains unassigned rather than borrowing stronger, different observations.

## Exact frozen-wire mapping and prerequisites

| Policy observation | Frozen source / limitation |
| --- | --- |
| Edit acknowledgement | `edit_latency_ms.p95/max`, ms converted to s; not indexed visibility |
| Read p95 | Successful workload `attempts[].logical_ms`, nearest-rank per repeat; includes backoff, excludes think time |
| Tool timing | `tools[search.symbols/read.source/explore.outline/analyze.dead_code].latency_ms`; completed count required |
| Busy fraction | Workload busy attempts / all workload attempts; probes use another denominator |
| CPU, WAL, stop | Phase metrics `daemon_cpu_avg_cores`, `peak_wal_bytes`, `stop_seconds`; actual `force_killed` required |
| Memory | Linux PSS / Mac physical footprint, with distinct peak and `idle_after_2s_` keys |
| Cold bytes | `startup_logical_written_bytes`; startup includes enrichment/analysis, not an isolated parser call |
| MiB/edit | Top-level inferred-contained patch WAL + main counters / acknowledged edits; reject missing, reset/lower-bound, boundary or overlapping evidence. This is amortized process-interval activity, not exclusive edit ownership. Tag cannot emit the new counters: an approved equivalent baseline strategy or explicit policy change is needed; whole-process bytes are not substituted. |
| Exact file readback/correctness | Missing. Indexed new-symbol visibility and mixed reading bursts are weaker observations. |
| Unflagged stale output / relations / derived lag | Missing structured output-mode correctness/publication observations; never inferred from visibility. |
| Reclaim writer hold / read-gate closures / cancellation release | Missing distributions/censuses, not inferred from WAL size or client deadline. |
| Primary stale refusals / restart refusal census / ready-to-first-OK | Missing structured reason and configured-root/worktree probes. Launch-to-queryable is not ready-to-first-OK. |
| Six-repo per-scope sweep | Frozen `sweep` loads only manifest[0]. `many`'s fast read mix is not the required sweep. |
| Linked-worktree edit probes | Frozen `worktrees` drives read bursts; its CPU/memory are not the combined edit scenario. |
| Idle/churn/schema21/freelist/lapsed/stop20 fixtures | Not distinct measured CLI scenarios yet. Stop20 requires 20 planned reports, not three zeros. |
| 60-second idle / cold Go Sys peak / idle bytes per minute | Missing. Two-second footprint and status snapshots cannot stand in for those intervals. |

Known unavailable metrics/scenarios stay declared and fail with their reason;
they are not silently omitted from the selected host/lane. The staged rows and
synthetic unit fixtures do not prove any owning product target passes.

## Collection, history and release entry point

```sh
# Source-only preflight. Exits nonzero with the exact unavailable prerequisites.
go run ./bench/check-budgets plan --lane smoke --host-class hosted_arm_smoke --out NEW-smoke-plan.json
go run ./bench/check-budgets plan --config reviewed-lane.json

# Run supported observations and retain every failure. The full policy still
# fails when a mandatory fixture/metric is unavailable; this is not a subset pass.
go run ./bench/check-budgets collect --config reviewed-lane.json --out NEW-evidence-dir
go build -o /benchmark/bin/release-gate ./bench/release-gate
/benchmark/bin/release-gate --config reviewed-release.json --out /benchmark/NEW-release-dir

# Inspect downloaded scheduled/manual-main artifacts and replay all passing
# raw reports. Require one complete run at the reviewed main revision across
# every required host; keep two-night row-miss monitoring separately.
go run ./bench/check-budgets history --inputs downloaded-nightlies.json \
  --revision "$REVIEWED_MAIN_SHA" --through 2026-10-30 --hosts reference_linux
```

Build the release entry before invoking it so a supervisor can address its PID
directly, without an outer `go run` process. It invokes the Go tool from `PATH`
to compile the checker into a fresh owned temporary directory before workloads;
select the repository's pinned toolchain there and use `GOTOOLCHAIN=local`.
Once workloads can exist, it launches the compiled collector directly. SIGTERM
or Interrupt sent only to the entry PID is forwarded to that collector, and
the entry waits for its cleanup and retained stop receipts before exiting.
The temporary binary is then removed; evidence is preserved. Compilation counts
toward the same release start time, cleanup reserve and wall-time receipt.

`downloaded-nightlies.json` is an array of retained artifact-directory paths,
relative to that index or absolute. Review their run provenance before use:
this command checks local content and replays raw evidence, not the authenticity
of arbitrary local files. One complete qualifying main nightly-equivalent run
is required: either a scheduled event or an explicit workflow_dispatch on
refs/heads/main, at the reviewed --revision with the same pinned policy,
complete matrix, independent repetitions, host coverage, raw schema and budget
table. Required host artifacts must belong to the SAME workflow run/attempt;
unrelated hosts, revisions or reruns cannot be assembled into a qualifying run.
The --through date is a review cutoff, not a calendar waiting period. PR,
readiness, synthetic, shortened or incomplete artifacts do not qualify. Same-day
separate real runs remain inspectable; duplicate run/attempt/host artifacts fail.
Nightly monitoring continues: consecutive block misses are labeled BLOCKER and
consecutive disk/memory guard misses GUARD REVIEW. Manual passes and same-day
retries do not erase scheduled misses. A seeded RED nightly gate
proof remains separately required; neither this green-run check nor synthetic
unit fixtures supplies it. The workflow preserves each artifact; reviewing the
provenance/index and the separate seeded RED proof remains release-owner work.

A lane configuration is strict schema1 JSON. Example below is **configuration
shape only**, with synthetic paths/revisions and no measured baseline:

```json
{
  "schema_version": 1,
  "lane": "release",
  "host_class": "reference_linux",
  "machine": "reviewed-machine-id",
  "load_binary": "bin/load",
  "manifest": "corpus.json",
  "builds": {
    "candidate": {"binary": "bin/candidate", "revision": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
    "main": {"binary": "bin/main", "revision": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
    "tag": {"binary": "bin/tag", "revision": "defa9f8e7a670792b5b693744d88488f741a8160"}
  },
  "repeats": 3,
  "duration": "120s",
  "deadline": "120s",
  "warm_timeout": "30m",
  "stop_timeout": "30s",
  "sweep_duration": "10m",
  "sweep_repetitions": 15
}
```

Paths in the configuration resolve relative to its file. Manifest sources are
absolute read-only inputs under the #996 contract; outputs must not overlap
them, including through symlinks. Use `gortex` and `corpus-1` through `corpus-5`
aliases. The collector selects one manifest entry for each single-repo sweep,
the first entry for ordinary one-repo scenarios, and six for `many-edit`.
It does not relabel the frozen one-root sweep as a six-root sweep, or reading
worktrees as editing worktrees. Floor configs are refused until the supported
envelope has an explicit policy and fixtures; floor evidence cannot become
reference evidence.

Each supported scenario runs the policy's full planned repetitions with matching
seeds across roles and rotating sequential main/candidate/tag order. Tag runs
are needed only for scenarios with guard rows. Release plans require at least
ten warm restarts and twenty idle stops. The latter fixture is unavailable and
does not become twenty generic clean stops. The collector retains `inputs.json`,
raw reports/logs, a JSON budget table and text table when reports are decodable,
plus `lane.json` with failed/missing tasks, wall time and workflow provenance.
If startup fails before any raw report exists, the receipt/index still retain
the failure. No failed repeat is discarded. Collector cancellation interrupts
the load client and waits for owned-daemon cleanup rather than killing the
client first. The release collector interrupts workload at seven hours and
57 minutes, reserving three minutes for the harness's independent owned-daemon
stop (configuration requires at most two minutes), logs, cleanup and checking.
It counts all
startup, teardown and checking toward the actual wall limit. An overrun fails;
this is not a claim that the subset has measured under eight hours.

The nightly jobs build the exact clean workflow candidate and load client.
`--expected-candidate` binds the candidate revision; the harness rejects binary
metadata disagreement. The owner supplies pinned standalone main/tag binaries,
corpora and machine binding via `PERF_REFERENCE_NIGHTLY_CONFIG` or
`PERF_MAC_NIGHTLY_CONFIG`, and enables only the matching `*_ENABLED` variable.
Dedicated labels are `perf-reference-linux` and `perf-apple-silicon`. These jobs
never execute PR code on owned hosts. No registration, service install, secret
or infrastructure change is performed. Before enabling artifact publication,
review neutral corpus labels and runtime paths; do not publish private source
names or local benchmark paths. Repository/organization retention settings must
actually permit the requested 90 days.

The reviewed inventory must name each runner's exact reported `machine`, class,
OS/architecture, kernel and observed limits. Linux requires an existing isolated
cgroup v2 with effective 8 CPUs/16 GiB, sufficient physical capacity, and an
observed 57–60 GiB benchmark filesystem. Mac requires Darwin ARM64 and an
explicit reference/large classification; its uncapped measurements must not be
relabeled Linux capacity. Each dedicated host needs a clean checkout, Go from
`go.mod`, the required C/C++ compiler, the pinned standalone controls and corpus,
and enough exclusive time to avoid simultaneous benchmark sessions. The
workflow requests only `contents: read`; artifact upload uses the Actions job's
normal artifact runtime token. No write-scoped PAT or repository secret is
needed by the collector. Restrict the runner group to this repository's trusted
main workflow and dedicated labels, and review neutral artifact contents before
enablement. Runner registration, any host changes and repository variables are
separate owner-approved infrastructure actions. No existing VM is adopted.

After that review, the owner places the external config, sets only its matching
config-path variable, and sets `PERF_REFERENCE_NIGHTLY_ENABLED` or
`PERF_MAC_NIGHTLY_ENABLED` to `true`. A manual main dispatch qualifies only when
its dedicated nightly jobs collect the same complete policy, matrix, required
hosts, independent repeats, raw JSON and budget table as a scheduled run.
A smoke or plumbing-only dispatch does not qualify. Leave both enabled
variables unset while prerequisites remain; GitHub displays measurement jobs
as skipped. Removing the variable stops future scheduling without cancelling
an in-flight owned daemon. A host being busy is reason to leave its lane
disabled, not to attach to an unrelated active session.

## Trusted exact-main artifacts and same-job smoke

`perf-main.yml` is an opt-in build-only producer, separate from CI tests and
measurement jobs. `PERF_MAIN_ARTIFACT_ENABLED` remains unset. Its trusted main
push binds clean daemon/test binaries and the pinned tool to full VCS SHA,
Go/C/C++ flags, platform, workflow run/attempt and individual hashes. No build-host timing
is compared with PR measurements. Current retained artifacts have no usable
main daemon or benchmark bundle; publishing the first reviewed exact-main
artifact remains required.

```sh
# Supply the job-scoped GH_TOKEN. The consumer needs actions: read for run/
# artifact metadata and downloads, alongside contents: read; no write PAT.
go run ./bench/check-budgets fetch-main --main-revision FULL_MAIN_SHA \
  --out /benchmark/NEW-main-bundle

go run ./bench/check-budgets collect --config reviewed-smoke.json --job-host \
  --out /benchmark/NEW-smoke-evidence

go run ./bench/check-budgets verify-smoke --evidence /benchmark/smoke-evidence \
  --main-bundle /benchmark/main-bundle
```

The lookup accepts exactly the requested full main revision from the dedicated
successful push workflow/repository, never a PR/fork, another workflow, older
green revision, expired artifact, missing digest or mismatched platform. The
newest successful matching run/attempt must have exactly one bundle. GitHub's
archive SHA256, safe ZIP file inventory, bundled binary hash and metadata are
verified. Redirects cannot forward authentication to another host. Offline
verification checks content; trusted GitHub download/owner review still provide
provenance, not arbitrary local JSON.

A smoke config requires `main_artifact` pointing to that downloaded directory,
and `builds.main.binary` to its `gortex` file. Both main and candidate execute
sequentially in this one PR run/attempt/job under the same host/corpus/workload.
Go/platform/build settings must match; only VCS revision/time differ. Every raw
report start must lie inside its recorded task interval. The unchanged checker
then applies all planned repeats, absolute limits, deterministic counters,
median-of-three p95 and +20% rule. `--job-host` binds the actual ephemeral
hostname only for this owned hosted PR context; it cannot relabel reference
capacity. Missing jobs, stale reports, mismatched builds or incomplete policy
remain failures.

## Microbenchmark and benchstat integration

`microbench --inputs reviewed-micro.json --out NEW-DIR` runs prebuilt exact-main
and candidate test executables for the existing CI packages on the same job:
`internal/parser/languages`, `internal/query`, and `internal/graph`. It requires
at least three sequential repeats, rotates build order, retains all raw outputs
and failures, and refuses differing benchmark/unit censuses. It invokes only
buildinfo-verified benchstat at the existing `bench-arm.yml` pin:
`golang.org/x/perf/cmd/benchstat@v0.0.0-20260409210113-8e83ce0f7b1c`.
No module dependency or new version is selected. Build tools/test executables
with the pinned Go; use `go test -c -buildvcs=true` and required C/C++ flags so
clean test-binary VCS/build identity is present.

The strict JSON input fields are `schema_version:1`, `main_revision`,
`candidate_revision`, `fixture`, `fixture_revision`, `benchstat_binary`,
`benchtime`, `timeout`, `repeats`, `main_bundle`, `candidate_builds`, and
`binaries`. The latter maps exactly those
three package paths to `{main,candidate}` executable paths. Paths resolve
relative to the config. Time/iteration profile and timeout are explicit; no
shorter profile is silently chosen. The query benchmark indexes `../..`, so
both builds run from package directories under the **same clean pinned fixture
snapshot**, never arbitrary binary-download CWDs. Fixture cleanliness is checked
again after runs and before benchstat. Changed input fails. Interrupted/missing
samples remain failed evidence. Linux query test executables (fixture preflight
and benchmark group) enable GC logs and all-goroutine tracebacks. At the original
context deadline they receive SIGQUIT, with at most ten seconds to capture output
before forced kill/join. Query processes own a separate process group; surviving
descendants receive TERM then KILL with at most two seconds to join after each.
Leftover work is failed evidence even when cleanup succeeds. The receipt records `linux_query_sigquit_v1` and the
capture grace separately. Crossing the original deadline always fails, including
a child that handles SIGQUIT and exits zero. The monotonic deadline is checked
after Wait as well, so a delayed timer callback cannot admit a late zero exit.
The passing budget is unchanged.
Other processes retain Interrupt and the two-second kill/join bound;
abnormal exit/context errors cannot become success.
The daemon collector instead joins owned cleanup without killing its client.

For a separately reviewed query setup diagnosis, the test executable accepts
`GORTEX_BENCH_QUERY_SETUP_TRACE` as a new output-file prefix. It writes exclusive
`.start.json` and `.finish.json` sidecars with shared setup and indexing clocks,
monotonic elapsed durations, and the indexed file/worker census. A start without
a finish remains incomplete evidence. An existing sidecar or write failure
fails the requested diagnosis. The unset default emits no sidecars. This trace
does not change query bodies, fixture scope, timers, or child safety bounds;
shared setup runs once per executable and remains outside the benchmark timers.

Outputs include per-run raw text, combined main/candidate samples, benchstat
text and `microbench.json`. `formal_acceptance` is always false: the workload
gate must independently pass. The trusted bundle now has a complete microbenchmark extension: three exact-main
test executables plus pinned benchstat, with observed package/module/toolchain,
clean embedded VCS, flags, sizes and hashes. Daemon-only legacy bundles remain
insufficient for `microbench`. Candidate executables must come from the prepared
current PR job receipt; arbitrary paths or altered binaries fail. The runtime
fixture commit/tree must match the trusted bundle. Synthetic fixture tests are
not measured benchmark evidence, and the first real trusted producer artifact
and same-job execution remain pending.

## Owned fixture, executable and tool preparation

The producer stays disabled. Its reviewed fixed query microbenchmark input
is public Gortex commit `c6c6de19da02fc698db7a89f9922a8ec49cc61b5`, tree
`47debc33a46a06eed403a3ea455feda97ca9a609`. There is no moving-main fixture
revision. The build-only workflow prepares that public Gortex snapshot,
compiles all three test executables without running them, and installs only the
existing benchstat pin into a fresh owned tool directory. It publishes the
daemon, test executables, tool and one hash/provenance manifest. Reruns replace
the same artifact name rather than leaving ambiguous older attempts.

This contemporary snapshot is also the reviewed load-smoke corpus; there is no
historical-fixture substitution. Query benchmarks share explicit symbol IDs
with the executable fixture check. `GetDependencies`/`GetCluster` use
`NewServer`; `GetCallChain` uses the current `mcp.go::runMCP` entry point.
`GetDependents`, `GetCallers` and `FindUsages` use `graph.go::keyOf`,
`graph.go::fnv1aEdge` and `graph.go::hashEdgeKey`, respectively: these graph
kernel functions have explicit local call edges. A type node's existence
alone is not evidence of a meaningful call/dependency traversal. Parser
benchmarks still use embedded samples and graph benchmarks build synthetic
graphs; neither consumes repository contents.

Before any timings, both query executables must advertise and pass
`TestBenchmarkFixtureTargets` under the runner's explicit opt-in. That check
indexes the pinned fixture at the scope the daemon indexes it: the checkout's
own `.gortex.yaml` (built-in, `.gitignore` and configured exclusions, worker
count) with at least two parse workers, never the host's global config. It
refuses any indexed `_test.go` source, verifies every shared benchmark symbol
ID, and requires nonempty search/statistics and meaningful nodes plus edges for
every traversal. The runner retains each binary's test census, raw check output
and observed result counts. Missing tests, skipped checks, absent targets or
empty traversals refuse all timings. First usable trusted main publication
therefore requires this test-only harness contract to land on main. Existing
main `c6c6de19` executables do not contain it and cannot pass by reporting "no
tests to run". Ordinary package tests skip this opt-in check and fetch no
fixture.

```sh
# All outputs are NEW external directories. Use the prescribed Go/C/C++ env.
go run ./bench/check-budgets prepare-fixture --revision REVIEWED_FIXTURE_SHA \
  --out /benchmark/NEW-fixture

go run ./bench/check-budgets prepare-tests --source . --revision EXACT_SOURCE_SHA \
  --out /benchmark/NEW-candidate-tests

go run ./bench/check-budgets prepare-benchstat --out /benchmark/NEW-tool
```

Fixture preparation uses one fixed public repository URL and a depth-one fetch
of the explicit commit, then verifies actual clean HEAD/tree. The test builder
requires clean pinned source before and after `go test -c -buildvcs=true
-mod=readonly -p=2`, records every executable individually, and refuses absent
or mismatched embedded provenance. These are setup/build steps, never test or
benchmark execution. A native graph-test probe confirmed the expected module,
package path, Go version, clean VCS and C/C++ flags; its roughly 284-MiB size is
an observation, not an estimate for every platform or all executables. The
other package/platform files are each inspected during actual preparation.

A full archive has a closed six-file inventory and per-file hash/size checks,
with bounded compressed/expanded size. The main source tree, pinned fixture
tree, each executable and the tool remain distinct identities. The microbench
runner requires `main_bundle` and `candidate_builds`, uses only their declared
fixed filenames, checks current-job candidate provenance, and rechecks actual
files before running. Every missing/failed setup retains its receipt and exits
nonzero. Preparing these artifacts does not establish 15-minute feasibility,
acceptance samples or a passing workload lane.

## Fixed-store PR entry and unresolved feasibility

The disabled hosted ARM job invokes `smoke` after preparing the exact-main
bundle, clean candidate executables and contemporary fixture. It reuses the
normal checker and microbenchmark runner; unavailable observations still fail.
The fixed `bench/load smoke` entry owns exactly two fresh stores, one per build,
and runs three rotated main/candidate sequences of read16, edit/read4, sweep1,
warm restart and idle stop. It has no resume/borrowed-path CLI. Only this entry
owns final state deletion; each phase retains its real stop/join receipt.

The PR workflow resolves the current job's prepared Go build and module caches
before the daemon isolates HOME. Both builds reuse those tool caches while
keeping their graph stores fresh and separate. Owned cleanup restores writable
directory permissions without following symlinks, including read-only module
directories created during a failed attempt. Uploads select reports, receipts,
foreground logs and raw microbenchmark output, excluding clones, stores and
tool caches. A readiness failure names the last observed ready/enriched/coverage
state; it does not relax those gates or establish full-job feasibility.

There are 36 explicit process starts: two empty-store initializations and 34
retained-store starts, including the six restart steps. Stores never cross
build ownership. Edits are reset only inside owned clones while their daemon is
joined. Exact pinned file bytes and clean Git state are verified; ready/enriched
and public marker-removal fences precede later measurements. Every report
retains its store/start identity, current foreground logs and execution interval;
the checker refuses changed/shared stores, missing restoration or role rotation,
missing steps, failures and altered paths. Default run/all, nightly/release
profiles and legacy request hashes are unchanged.

The explicit PR experiment uses 20-second read/edit windows, with 30 seconds an
alternative. Read offers drain under their original 120-second product deadline.
Edit/probe drain and four-editor exact readback still require integration with
the dedicated edit/readback instrumentation; legacy indexed-symbol visibility
is not substituted for correctness. The distinct idle step warms the configured
root, closes MCP sessions, observes two idle seconds and retains actual normal
stop evidence. It does not establish the separate 60-second idle memory/write
contracts.

Setup is a measured constraint. Thirty independently initialized phases would
spend about 94 minutes at an observed 188-second daemon initialization, and even
six initializations would consume 18.8 minutes before offered work. Two stores
reduce requested cold initialization to about 376 seconds at that observation.
At 20-second read/edit offers, those windows add 240 seconds across both builds
and three repeats. A 25-second sweep estimate adds 150 seconds, leaving only
134 of 900 seconds for warm starts/stops, idle intervals, drain, preparation and
microbenchmarks. This is an estimate, not a feasible-profile result.

The contemporary query fixture check indexes the checkout's own `.gortex.yaml`
scope described above; indexing the whole checkout with an empty config
exceeded the 120-second bound. One sample at `GOMAXPROCS=2` passed in about 51
seconds, well inside that unchanged bound. Confirmation on the reference runner
is still pending, so that sample earns no query acceptance. Query process order
remains rotated; the runner does not silently batch samples and never changes
fixture scope itself.

Actual tool counts remain authoritative. With mean 3.5-second think time and
four-call bursts, optimistic 20-second estimates are about 328 load16 calls and
19 edits/75 reads for edit4; publication speed is unmeasured. Per-tool counts
will be much smaller, and p99 can effectively be a maximum. Neither sample
adequacy nor the full-job p95 <=15 minutes is established. The entry cancels
measurement at 120 minutes and reserves two minutes for owned cleanup.
Strict startup/enrichment uses the remaining measurement deadline; parent
cancellation interrupts the load client for cleanup. The job caps setup steps
at twenty minutes in total, the measurement step at 122 minutes including
cleanup, and always-upload at three minutes, within a 145-minute job bound.
These are measurement safety bounds, not accepted performance targets.

The original full-job p95 <=15-minute target is explicitly deferred while a
complete run is measured; it has not become a 120-minute target. The receipt
retains the original 900-second target, reports deferred timing acceptance and
whether the entry alone exceeded that target, and never grants formal
acceptance. Full-job timing includes build/download/preparation; an entry below
fifteen minutes would not establish full-job acceptance either. One naturally
triggered complete attempt is reviewed before any repeat campaign. Fixture,
readiness, five phases, three repeats, both fresh stores, thirty-six starts and
all other strict checker thresholds remain unchanged.

The measurement job is visibly skipped until `PERF_PR_SMOKE_ENABLED` is reviewed
and enabled. Main publication separately requires its new harness contract on
main and producer enablement; no pre-merge main artifact bypass exists. Hosted
jobs can still contend for account concurrency, so the twenty-PR CI wall-time
comparison remains required. No runner or infrastructure is provisioned.

## Remaining lane and acceptance work

Before enabling measurement jobs or external setup, review the source plan,
official baseline evidence and resolved metrics above.
No self-hosted runner is currently registered. A runner owner must provide the
reference cgroup/disk and reviewed Apple host inventory; this command does not
register services, alter Docker/Brew, install runners, set secrets or spend
model calls. A PR smoke lane must remain separate from CI test jobs and run on
`ubuntu-24.04-arm`; protected-main nightlies require available owned hosts.
Reports/table artifacts need effective retention of at least 90 days.

The five-step driver and strict full-bundle setup are implemented source;
positive contemporary query setup, four-editor exact probes, missing safety/
restart/reclaim observations and a measured end-to-end attempt remain required.
No measurement lane is enabled or accepted. Short PR reports need matching
same-job baselines and sufficient actual observations; retained 120-second
baseline data cannot be relabeled as a 20-second smoke profile.
Nightly/release plans must preserve failed/censored runs, consecutive-night row
history, floor-host scope restrictions and every daemon's stop/lock receipts.
The floor class and extra lifecycle adapters require explicit reviewed support;
do not relabel floor runs as reference Linux.

Actual acceptance is still required: 20 real smoke runs for wall-time/flake
rates, linked seeded latency/edit-wait regressions, one complete qualifying main
nightly-equivalent run (scheduled or manual) with raw JSON and budget table,
a separate seeded nightly failure, a measured release subset under eight hours per host,
no forced daemons, and 20-PR CI wall-time comparison. None is simulated by unit
tests. #996 and #998 implementations are merged; baseline acceptance remains separate. #1000's deferred 400-task
harness is not recreated here. A small release scripted-agent fixture remains
separate future work, requiring its own reviewed plan.
