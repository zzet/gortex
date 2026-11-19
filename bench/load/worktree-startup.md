# Linked-checkout startup benchmark

`worktree-startup` measures a newly created linked checkout against a **new
private daemon**, using the public path-scoped control search. It never connects
to the user's daemon or tracks a linked checkout. All repositories are generated
synthetic fixtures; the harness accepts no live repository/store inputs.

It runs three arms, each with a fresh primary clone, daemon, HOME/XDG directories,
store, socket and process group:

- `equal_head`: clean linked checkout at the indexed primary commit.
- `divergent_head`: a committed change of `--changes` files, including a signature
  change with callers throughout the corpus to exercise closure fan-out.
- `head_switch`: the same divergent target, switched to a third committed tree
  after the captured child log reports its first coordinator index starting.

Readiness requires **one indexed search response** with an exact worktree rider,
a checkout ID, the expected marker, and no predecessor markers. An exact rider,
published generation or file read alone does not earn readiness. The primary's
original marker is checked afterward for isolation. This marker oracle does not
prove whole-graph equivalence, complete relations, MCP tool-list binding, or hook
readiness. It measures the control probe used for checkout routing.

The HEAD-switch arm additionally correlates the Git switch with the first
matching publication record. A switch before planning, after publication, or
without phase evidence produces `missed_race` and a failing command. There is no
fixed sleep that silently turns a missed race into a passing sample. Phase offsets
are monotonic inside the daemon; cross-process correlation uses `origin_wall`, so
run on a host with a stable wall clock.

## Run

Use an explicit binary. An installed binary is started as a private child; it is
not used to attach to, restart, or reconfigure its existing service. To build the
current source instead:

```sh
CGO_CFLAGS='-O2 -g -fno-strict-aliasing' \
CGO_CXXFLAGS='-O2 -g -fno-strict-aliasing' \
  go build -o /tmp/gortex-startup-bench ./cmd/gortex/
```

Quick functional smoke:

```sh
go run ./bench/load worktree-startup \
  --binary /tmp/gortex-startup-bench \
  --scratch /tmp/gortex-startup-smoke \
  --files 16 --changes 4 --functions 2 --calls 2 --timeout 90s
```

Closure-cap/loaded-store baseline:

```sh
go run ./bench/load worktree-startup \
  --binary /tmp/gortex-startup-bench \
  --scratch /tmp/gortex-startup-baseline \
  --files 256 --changes 26 --functions 8 --calls 4 \
  --pressure-files 64 --seed-worktrees 1 --repeats 3 \
  --timeout 10m --cpu-profile 15s
```

The harness prints the newly allocated report path on stderr and emits that
report as JSON on stdout. A failing arm is kept in the report and returns a
nonzero exit. Every arm in every repetition is retained; do not calculate
percentiles from only successful samples. Report the denominator, failures and
missed-race count alongside timing. Three repetitions are a diagnostic baseline,
not a defensible population p95.

For a larger graph, increase `--functions` and `--calls` (for example 32 and 16).
`--pressure-files` seeds an unrelated corpus before measurement; it is zero by
default. `--seed-worktrees` retains that many divergent linked-checkout routes
before measurement, **one by default**. The seed also finishes the primary's
committed-base setup before the timed window. Setting it to zero allows that
setup to overlap measurement and compete for shadow admission; such a run can
exercise streamed persistence instead of the pre-drain path. Compare matching
seed settings and inspect the `shadow` admission logs. Seeds use a fourth committed tree,
distinct from both measured targets, so cross-checkout commit adoption cannot
bypass the intended first-build race. These create actual indexed payloads
rather than fabricated SQLite rows. Increase gradually and retain the workload
options: a small synthetic store does **not** reproduce a 24 GB store's behavior.

`--timeout` bounds boot/seeding and, separately, the measured startup. A probe has
a one-second call budget; the poll interval defaults to 100 ms. Readiness is an
observed upper bound including Git worktree creation, request time and polling.
`observed_ms` also includes final evidence collection; use
`first_correct_exact_ms` for the readiness metric.

## Evidence and profiles

Each run retains `report.json`, a reproducible four-commit fixture, and each
arm's private directory. The report records binary SHA-256, host OS/architecture/
CPU count, fixture revisions, options, every probe/error and matching publication
phases. Each case has `complete`, `outcome`, nullable readiness timing and
`met_30s_setup_budget`. **A timeout never becomes zero milliseconds.** A recorded
readiness timing from an arm with `complete:false` remains diagnostic evidence,
not a passing benchmark sample. The 30-second flag reports observed budget
attainment; it is not an enabled CI performance gate.

Private artifacts include:

- `daemon-1.stderr.log`: structured application logs, including closure bounds,
  selected file count, parser/resolve phases, drain start/finish and publication.
  `daemon.log` may be an empty lifecycle log; the race observer uses the child's
  actual stderr stream.
- `before.status.json`, `after.status.json`: raw status, publication phase
  offsets, WAL/store statistics and lane counters. Missing phase evidence is
  labelled unavailable, not emitted as a measured zero. The outer phase markers
  do not attribute every inner context-filter/drain/writer wait separately.
- With `--cpu-profile` (positive whole seconds), `startup.cpu.pprof` plus
  `before/after.goroutine.pprof`, `.block.pprof`, and `.mutex.pprof`.

Profiling is enabled only in the private child's environment. CPU capture uses a
fixed window starting just before creation; a fast arm can finish before the
capture and leave an idle tail. Goroutines are snapshots. Block/mutex sampling is
enabled at child startup, so difference before/after profiles to remove warmup.
Profiling adds overhead: compare runs with matching options.

```sh
go tool pprof -top /path/to/startup.cpu.pprof
go tool pprof -top -base /path/to/before.block.pprof /path/to/after.block.pprof
go tool pprof -top -base /path/to/before.mutex.pprof /path/to/after.mutex.pprof
```

Primary boot and setup are outside the readiness timer, and fresh-store arms
prevent a previous repetition's generation reuse from receiving accidental
credit. Background work may still run after the readiness/enrichment gate; this
is not a claim that every maintenance worker is quiescent. Private children are
stopped/joined even on failure, sockets are removed, and data/logs/profiles remain
for inspection. Cleanup errors or forced shutdown fail the case.

## Tests

Ordinary tests cover rejection of fallback/stale/substr-matched answers,
predecessor leakage, missing identities, missed HEAD races, retained timeout
attempts, log-event filtering, fixture pinning and actual tree-diff size:

```sh
make test 'PKGS=./bench/load -run TestWorktreeStartup -count=1'
```

The real-daemon smoke is opt-in and runs all three arms with freshly generated
16-file repositories:

```sh
GORTEX_STARTUP_BENCH_BINARY=/tmp/gortex-startup-bench \
  make test 'PKGS=./bench/load -run TestWorktreeStartupPrivateDaemon -count=1 -v -timeout=10m'
```

Use the CLI for retained benchmark artifacts and profiles; the integration test
uses the test runner's temporary directory. The existing load report/schema and
ordinary scenarios are unchanged by this separate command.
