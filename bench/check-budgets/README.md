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
Owned-host measurement jobs are skipped unless their reviewed runner/config
variables are enabled. There is no formal PR smoke measurement job yet: hosted
ARM capacity is not the reference cgroup, and exact smoke observations remain
unavailable. Readiness cannot satisfy a required performance acceptance check.

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

Reference Linux requires the actual validated cgroup/host observations: 8 CPU,
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
go run ./bench/check-budgets plan --lane smoke --out NEW-smoke-plan.json
go run ./bench/check-budgets plan --config reviewed-lane.json

# Run supported observations and retain every failure. The full policy still
# fails when a mandatory fixture/metric is unavailable; this is not a subset pass.
go run ./bench/check-budgets collect --config reviewed-lane.json --out NEW-evidence-dir
go run ./bench/release-gate --config reviewed-release.json --out NEW-release-dir

# Inspect downloaded scheduled-main artifacts, replay all passing raw reports,
# require five actual adjacent UTC dates, and flag two-night row misses.
go run ./bench/check-budgets history --inputs downloaded-nightlies.json \
  --through 2026-10-30 --hosts reference_linux
```

`downloaded-nightlies.json` is an array of retained artifact-directory paths,
relative to that index or absolute. Review their run provenance before use:
this command checks local content and replays raw evidence, not the authenticity
of arbitrary local files. Only `schedule` events on `refs/heads/main` count.
Dispatch, readiness, duplicate host/date, omitted dates, shortened tables and
changed policies cannot manufacture five accepted nights. Use `--through` for
the last required night before the candidate; stale history is not a current
release verdict. Consecutive block misses are labeled `BLOCKER`; consecutive
disk/memory guard misses are labeled `GUARD REVIEW`. Missing history fails.
The workflow preserves each artifact; downloading/curating the history index
and calling this check before the first RC remain release-owner actions.

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
`PERF_MAC_NIGHTLY_ENABLED` to `true`. A manual main dispatch can verify plumbing
but cannot count as one of the five scheduled nights. Leave both enabled
variables unset while prerequisites remain; GitHub displays measurement jobs
as skipped. Removing the variable stops future scheduling without cancelling
an in-flight owned daemon. A host being busy is reason to leave its lane
disabled, not to attach to an unrelated active session.

## Remaining lane and acceptance work

Before enabling measurement jobs or external setup, review the source plan,
official baseline evidence and resolved metrics above.
No self-hosted runner is currently registered. A runner owner must provide the
reference cgroup/disk and reviewed Apple host inventory; this command does not
register services, alter Docker/Brew, install runners, set secrets or spend
model calls. A PR smoke lane must remain separate from CI test jobs and run on
`ubuntu-24.04-arm`; protected-main nightlies require available owned hosts.
Reports/table artifacts need effective retention of at least 90 days.

The implemented plan declares all five smoke steps but refuses the unavailable
idle-stop fixture and exact observations. The measured PR runner, comparable
latest-main artifact retrieval, existing micro-benchmark benchstat and measured
duration estimate remain to implement after smoke infrastructure is reviewed.
The frozen default
60-second duration is not a measured feasibility guarantee; the retained native
read1 cohort used 120-second windows. Those 120-second read/edit windows repeated
three times already consume substantial time; do not assume the full lane fits
15 minutes. Any shorter smoke profile
needs matching baselines and enough observations, not reused 120-second data.
Nightly/release plans must preserve failed/censored runs, consecutive-night row
history, floor-host scope restrictions and every daemon's stop/lock receipts.
The floor class and extra lifecycle adapters require explicit reviewed support;
do not relabel floor runs as reference Linux.

Actual acceptance is still required: 20 real smoke runs for wall-time/flake
rates, linked seeded latency/edit-wait regressions, five actual nights on main,
a seeded nightly failure, a measured release subset under eight hours per host,
no forced daemons, and 20-PR CI wall-time comparison. None is simulated by unit
tests. #996 must land before #997; #998 is merged. #1000's deferred 400-task
harness is not recreated here. A small release scripted-agent fixture remains
separate future work, requiring its own reviewed plan.
