# Incremental indexing — paired sustained-I/O measurements

This document records the paired measurement the incremental-indexing work is judged on: one
baseline arm on `main`, one candidate arm on this branch, the same generated corpus and the same
nine-phase workload driven against both, with the baseline's budgets frozen before the candidate's
numbers were read.

It is the evidence half of acceptance gate 10 (*reproducible release evidence*) and the only place
in this repository where a performance claim about the branch may be made. The execution ledger
cites it; it does not restate it.

## 1. What this document is, and what it is not

It **is** a record of process-accounted write volume, store and WAL behaviour, WAL resets, store
census and view-metrics counters produced by two real daemon binaries running the same workload on
one host, with every identity, knob and acceleration named.

It is **not**:

- a statement about SSD wear. `ri_logical_writes` counts bytes a process asked the kernel to write.
  Write-back coalescing, the filesystem's own bookkeeping and the drive's FTL all sit between that
  number and NAND. **Process writes are not NAND writes.**
- a cumulative write total derived from file sizes. **A WAL size is not cumulative writes** — a
  WAL that reads 30 MB may have been written many times over, and a WAL that reads 0 may have
  absorbed hundreds of megabytes before a checkpoint truncated it. WAL sizes appear here as state
  at a phase boundary, next to the write series, never instead of it.
- a checkpoint count. `wal_resets` counts observed restarts of the log (checkpoint-sequence or
  salt-1 movement in the WAL header, or the log disappearing). A PASSIVE checkpoint that does not
  restart the log is invisible from outside the process, so `wal_resets` is a **lower bound**.
- sustained production behaviour. **A small-fixture replay is not sustained daemon behaviour.**
  1,500 generated Go files over ~10 minutes per arm-repetition, with an accelerated janitor, is a
  controlled comparison between two binaries, not a forecast of a week on a real repository.
- a default-configuration measurement. Section 4 lists every knob that differs from the product's
  defaults, and each one changes the numbers.

## 2. Arms

Both arms run the **same** harness binary against **different** daemon binaries. The harness drives
a private child daemon through the public CLI; it is the daemon under measurement, not the test.

| | baseline arm | candidate arm |
|---|---|---|
| source | `main` at `56a1c29d514d8f7d3b5feec455c7b57de358b1d3` | `fix/incremental-index-write-amplification` at `2fd5db821ab0acede84ec0cb8bda65944d088e20` |
| how built | `git archive <commit> \| tar -x` into a scratch tree, then `go build ./cmd/gortex/` | same |
| binary | `harness/bin/gortex-baseline-56a1c29d` | `harness/bin/gortex-2fd5db82` |
| binary sha256 | `8785a8b4545c466185f4792440519286c05d854963a8f7a14f3f35af7b9c472b` | `68680abc948b283532cdd678ca2a0fef2778853db2c7b120738777cb2d282d63` |
| `version --short` | `v0.64.3` | `v0.64.3` |
| store schema version | 21 (`internal/graph/store_sqlite/schema_version.go:37`) | 25 (same file) |

The schema gap is why **the two arms never share a store**: a schema-21 binary cannot open a
schema-25 store, and a shared store would silently make one arm's numbers a migration measurement.
Each arm gets its own private root, its own config, its own state, its own socket and its own
fixture copy.

Both arms were built from exported trees rather than from the working checkout, because several
agents edit `_test.go` files in that checkout concurrently; an exported tree is a source identity a
reader can recreate with one `git archive`.

**The arm identities are checkable without trusting the build recipe.** A Go binary built with
`-buildvcs=false` carries no commit stamp, so the table above would otherwise rest on the operator's
word. It does not: each arm creates its own store, and the schema version a store is created at is
the binary's own `currentSchemaVersion`. After the measured run,

```
sqlite3 file:<baseline fixture>/store.sqlite?mode=ro 'PRAGMA user_version'   -> 21
sqlite3 file:<candidate fixture>/store.sqlite?mode=ro 'PRAGMA user_version'  -> 25
```

which is exactly `56a1c29d`'s 21 and this branch's 25. A binary that was not built from those two
trees could not produce that pair. All eight measured stores were checked this way — three baseline
repetitions at 21, three candidate repetitions at 25, plus the 6,000-file pair — and the readings are
recorded in `scratchpad/logs/w84-arm-store-schema.log`.

**Toolchain and host**

| | |
|---|---|
| Go | `go1.27.0 darwin/arm64`, `GOTOOLCHAIN=local`, `CGO_ENABLED=1` (tree-sitter) |
| build env | `GOWORK=off GOFLAGS=-mod=mod -buildvcs=false GOPROXY=off`, shared `GOCACHE`/`GOMODCACHE` |
| host | Darwin 25.6.0, 10 CPUs, 16 GiB RAM |

## 3. The instrument

The harness is `cmd/gortex/w8_sustained_io_integration_test.go` plus
`w8_sampler_test.go`, `w8_fixture_generator_test.go` and the shared private-daemon fixture in
`issue767_fixture_shared_test.go`. It is opt-in: without `GXW8_TEST_BINARY` it skips with a named
reason and never runs in a default `go test ./...`.

**Series recorded per phase, per arm**

| series | source | note |
|---|---|---|
| `ri_logical_writes` | `proc_pid_rusage(RUSAGE_INFO_V4)` on the child daemon | **primary** |
| `ri_diskio_byteswritten` | same call | reported beside the primary, **never alone** — it has been observed as 0 in this very measurement while logical writes were millions of bytes |
| `ri_diskio_bytesread`, CPU user/system, `ri_phys_footprint` | same call | context |
| store / WAL / SHM / daemon-log bytes | `os.Stat` at 1 Hz and at both phase boundaries | state, not cumulative writes |
| `wal_resets` | WAL header checkpoint-sequence and salt-1, sampled at 1 Hz | lower bound on checkpoints |
| `page_count`, `freelist_count`, `view_generations` census, `sqlite_sequence.seq`, checkouts, routes | read-only SQLite connection | per-boundary census |
| `viewmetrics` counters | `daemon status --format json` | candidate only; the flag does not exist on the baseline binary, and the harness records that unavailability by name instead of substituting zeros |
| destination census | walk of the private root, every byte attributed to a named writer bucket | catches bytes the process counters cannot attribute |

The sampler runs on its own goroutine at 1 Hz across the daemon's start and stop. Every reader it
uses (`pid`, `readIO`, `readSize`, `readWAL`) is wired explicitly by `w8NewRunSampler`; a reader
that is not wired is named in every sample (`unwired_readers`) and costs the sample its health,
because a zero from a missing wiring is indistinguishable from a real measurement of zero.

On the Linux arm the process reader would use `/proc/<pid>/io write_bytes`; this measurement ran on
Darwin only.

## 4. Environment, isolation and accelerations

Every process runs under `env -i` with an explicit allowlist: no inherited credentials, private
`HOME`, `TMPDIR`, `XDG_*`, private gitconfig, `GIT_CONFIG_NOSYSTEM=1`. The daemon's own private root
lives under a short `/private/tmp/gxh-w84/...` path. **The user's live daemon, store and
configuration are never addressed.**

The fixture builds the daemon's environment itself. It **drops every inherited `GORTEX_*`, `XDG_*`
and `GIT_*` variable** and appends its own list
(`cmd/gortex/issue767_fixture_shared_test.go:94-107`), so the only product knobs in force are the
ones below; a variable exported by the operator around the run cannot reach either daemon. Each
run's `manifest.json` records the child's environment verbatim, which is where this table is
checkable.

Knobs in force, both arms, each of which changes the numbers:

| knob | value | where | why, and what it costs |
|---|---|---|---|
| `GORTEX_RECONCILE_INTERVAL` | `5s` (default `1h`) | fixture env (`issue767_fixture_shared_test.go:104`) | the janitor has to run inside a ~10-minute workload at all. **These are not default-configuration numbers**; the default interval would move almost all reconcile work outside every measured phase. |
| `--embeddings=false` | off | daemon start flag (`issue767_fixture_shared_test.go:215`) | embeddings are a separate, much larger I/O source and are not what this measures. Note this suppresses embedding *work*, not the model file: both arms retain an identical 33,514,412-byte `embedding_model` bucket in the destination census, which therefore cancels in every comparison. |
| `GORTEX_TELEMETRY=0` | off | fixture env (`:106`) | telemetry is dormant without an endpoint, but its consent/rollup files under the private data dir are writes like any other. |
| `GORTEX_DAEMON_PPROF_ADDR` | `127.0.0.1:0` | fixture env (`:104`) | an ephemeral local pprof listener; it writes nothing unless profiled, and it is identical on both arms. |
| `--backend sqlite --backend-path <root>/store.sqlite`, `--no-progress` | — | daemon start flags (`:215`) | the measured store is one named file inside the private root, so the destination census can attribute it. |
| `GXW8_EDIT_INTERVAL` | `10s` (harness default `30s`) | harness knob, not a product knob | six arm-repetitions at 30 s between edits add 20 minutes of pure waiting. The edits themselves are unchanged; only the quiet time between them is shorter, which makes `P2_small_edits` a denser workload than the default and is applied identically to both arms. |

Two knobs the execution plan's hazard list asks for are **not** applied, and this is deliberate:

- **`GORTEX_SKIP_STORE_COMPACT=1`** cannot reach the daemon (the fixture drops inherited `GORTEX_*`),
  and it would be a no-op here in any case: `maybeCompactStore` VACUUMs only when the freelist is
  **both** larger than 1 GiB and more than half the file's pages
  (`cmd/gortex/daemon_compact.go:47-65`, byte-identical at `56a1c29d`), while the largest store any
  arm reached is ~330 MB. The boot VACUUM never ran in either arm, so nothing was suppressed and
  nothing the plan wanted excluded was measured. Verified by source, not by absence of evidence.
- **`GORTEX_QUERY_LOG_DISABLE=1`** is likewise unreachable, and the query log belongs to the MCP
  surface (`internal/mcp/query_log.go:121`). The harness drives the CLI; no query-log bytes appear
  in either arm's destination census, whose 14 buckets account for every retained byte.

Host load is **not** controlled. Other agents were compiling and running test suites on this
machine throughout; `uptime`'s load averages, free disk, CPU count and RAM are recorded at the start
and end of the run in `scratchpad/logs/w84-paired1500-host.log`. This is the reason the protocol
interleaves the arms `A/B/A/B/A/B` rather than running all of one arm and then all of the other:
drift in host load is shared by adjacent repetitions instead of being absorbed entirely by one arm.

## 5. Protocol

**Corpus.** Generated, not cloned, so a manifest can rebuild it byte for byte:
`w8GenerateFixture{Files: 1500, Packages: 60, Seed: 767}` — one `go.mod`, a root package holding the
call target, one `marker.go` per checkout, and 1,500 package files spread round-robin over 60
packages with intra-package and cross-package calls (cross-package imports always point at a higher
package index, so the import graph is acyclic by construction). The fixture digest is recorded in
every run's `manifest.json` and both arms are refused a comparison if their digests differ.

**Workload — nine phases**, in order, with a `settle()` (three identical generation snapshots)
between them:

| phase | what it does |
|---|---|
| `P0_cold_index` | cold index of the 1,500-file corpus, to the first exact primary answer |
| `P1_idle_cold` | 60 s idle on the fresh store, one read-only search every 5 s |
| `P2_small_edits` | 10 edits 10 s apart, rotating over the corpus; each rewrites one function body and renames a one-line revision stub, awaited to exact **from the edited file** |
| `P3_touch_stage_unstage` | touch with identical bytes, then `git add -A`, then `git reset` |
| `P4_amend_same_tree` | `git commit --amend --no-edit`: a new commit id over an unchanged tree |
| `P5_main_advance` | 10 dependent worktrees **discovered, never tracked**, then 20 commits on main touching 3 files each, each awaited to exact on the primary **and on all ten dependents** |
| `P6_dependent_edits` | dirty edits in the dependents, awaited to exact, primary isolation rechecked |
| `P7_dependent_untrack_retrack` | `track --as-worktree` one dependent, then `untrack` it back to automatic discovery |
| `P8_idle_warm` | 60 s idle on the worked store |

**Repetitions.** 3, interleaved `baseline rep1 → candidate rep1 → baseline rep2 → …`. Each
arm-repetition builds a brand-new fixture, a brand-new store and a brand-new daemon, so every run is
cold by construction; "warm" is represented **within** a run by `P8_idle_warm` against
`P1_idle_cold`, not by a separate warm arm.

**Statistics.** Median of the three repetitions, with min and max always printed beside it. A
repetition whose primary series is missing is **named** as unavailable and excluded from the
median — never folded in as a zero.

**The freeze.** Budgets are derived from the baseline arm only and written to `budgets.json`, which
refuses to be overwritten; the file carries a SHA-256 of its own canonical contents, and the verdict
refuses to judge against a budget set whose contents no longer match its digest. The reduction step
(`TestW8PairedArmsVerdict` in `cmd/gortex/w8_paired_arms_test.go`) is run twice: once with
`GXW8_BUDGETS_ONLY=1`, which reads only the `baseline_rep*` artifacts and stops, and once without,
which produces the verdict. Section 6 was written from the first run's output before the second was
started.

**Ceilings**, from the execution plan's paired-I/O protocol:

| phase | ceiling |
|---|---|
| `P1_idle_cold`, `P8_idle_warm` | min(baseline median, 8 MiB per 60 s scaled to the phase's measured length) |
| `P5_main_advance` | 0.50 × baseline median — the headline claim, reported as a ratio |
| `P2_small_edits` | ≤ baseline median |
| retained bytes after teardown | ≤ 1.20 × baseline median |
| every other phase | no ceiling; the baseline median is recorded, and a candidate above 1.10 × baseline is preserved as a regression |

## 6. The frozen baseline (budgets)

Frozen **2026-09-13T21:14:17Z** from `baseline_rep1`, `baseline_rep2`, `baseline_rep3` — the
`main 56a1c29d` arm, binary `8785a8b4…`, fixture `ff6ebe14…`. Digest of the frozen set:

```
aca001040dca77635558f84dc8f38aadfcebb840b30066f457098b9acb538366
```

`artifacts/W8m-W8.4/paired1500/budgets.json` carries that digest inside itself; the verdict step
recomputes it and refuses to judge against a set whose bytes have moved. The file refuses to be
rewritten.

**What "frozen before the candidate was read" means here, exactly.** The ordering is enforced by
code, not by memory: `w8FreezeBudgets` (`cmd/gortex/w8_paired_arms_test.go:215-228`) refuses any run
whose manifest is not the baseline arm, so no budget can absorb a candidate number even by accident;
`w8WriteFrozenBudgets` (`:373-395`) never overwrites; the ceiling formulas in `w8LimitFor`
(`:299-320`) were written before any number existed. The freeze pass is a separate invocation with
`GXW8_BUDGETS_ONLY=1` that stops before loading the candidate directories, and this section was
written from its output (`logs/w84-freeze.log`) before the verdict pass ran. The one thing the
operator is **not** blind to is the run's own console log, which prints both arms as they interleave;
that is inherent to an A/B/A/B/A/B run in one process, and it is why the mechanism lives in the code
rather than in a claim about who looked at what.

| phase | baseline `ri_logical_writes` median (min / max) | ≈ | `ri_diskio_byteswritten` median (min / max) | WAL bytes at phase end (median) | WAL resets | wall s | frozen ceiling |
|---|---|---|---|---|---|---|---|
| `P0_cold_index` | 949,066,148 (947,921,636 / 961,942,788) | 905 MiB | 1,001,312,256 (999,518,208 / 1,002,758,144) | 15,042,152 | 8 (8/8) | 26 | none |
| `P1_idle_cold` | 1,490,944 (1,323,008 / 1,556,480) | 1.4 MiB | 0 (0 / 0) | 15,141,032 | 0 | 64 | **1,490,944** |
| `P2_small_edits` | 16,879,664 (16,781,360 / 17,084,464) | 16.1 MiB | 36,864 (32,768 / 40,960) | 29,837,072 | 0 | 107 | **16,879,664** |
| `P3_touch_stage_unstage` | 364,544 (286,720 / 458,752) | 0.35 MiB | 0 (0 / 0) | 29,898,872 | 0 | 31 | none |
| `P4_amend_same_tree` | 401,408 (303,104 / 446,464) | 0.38 MiB | 0 (0 / 0) | 29,956,552 | 0 | 33 | none |
| `P5_main_advance` | 5,147,295,504 (4,958,552,632 / 5,152,481,816) | 4.79 GiB | 7,395,172,352 (7,184,576,512 / 7,425,323,008) | 67,108,864 | 53 (49/54) | 172 | **2,573,647,752** |
| `P6_dependent_edits` | 11,392,456 (11,113,944 / 13,308,200) | 10.9 MiB | 327,680 (180,224 / 393,216) | 67,108,864 | 0 | 56 | none |
| `P7_dependent_untrack_retrack` | 1,464,290,236 (1,379,998,724 / 1,550,549,748) | 1.36 GiB | 1,888,002,048 (1,790,296,064 / 2,138,386,432) | 67,108,864 | 11 (10/11) | 54 | none |
| `P8_idle_warm` | 14,820,872 (3,166,256 / 15,038,576) | 14.1 MiB | 9,142,272 (0 / 9,568,256) | 67,108,864 | 1 (0/1) | 62 | **8,639,500** |

Retained bytes after teardown: **434,887,204** (431,987,279 / 441,040,810); ceiling 521,864,644
(1.2 ×).

The ceilings, in the words that travel with them in `budgets.json`:

- `P1_idle_cold` — `min(baseline median 1,490,944, 8 MiB per 60 s scaled to 64 s = 8,878,313)`.
- `P8_idle_warm` — `min(baseline median 14,820,872, 8 MiB per 60 s scaled to 62 s = 8,639,500)`.
  Note which way this one binds: the baseline itself **exceeds** the plan's absolute idle budget, so
  the ceiling the candidate is judged against is the plan's 8 MiB figure, not the baseline's
  behaviour.
- `P2_small_edits` — at most the baseline median, 16,879,664.
- `P5_main_advance` — the headline: 0.50 × 5,147,295,504 = 2,573,647,752.
- every other phase — no ceiling; the baseline median is recorded and a candidate above 1.10 × it is
  preserved as a regression.

**What the baseline arm looks like, beyond the ceilings.** All three repetitions completed all nine
phases with no failed phase, 250 exactness waits each, and one sample failure each (the first 1 Hz
sample, taken before the child daemon has a pid — it is named in the sample line, not counted as a
zero). Per-run WAL resets: 73 / 68 / 73. The `view_generations` sequence on `main` moves by +1 at
`P0`, **0** across `P1`, `P2`, `P3`, `P4` and `P8` in all three repetitions, +66/+62/+66 at `P5`,
+3 at `P6` and +2 at `P7` — so on the baseline the twenty commits of `P5` are what produce
generations, and the dirty-edit, no-op and idle phases produce none. Dependent isolation held in
every baseline repetition; the harness's baseline-only isolation exemption
(`w8_sustained_io_integration_test.go:996-1005`) was never exercised.

**The one number to keep in view**: on the baseline, `P5_main_advance` — twenty commits on main with
ten dependent worktrees discovered — accounts for 4.79 GiB of process-accounted writes, 60 % of the
whole run. That is the write amplification this branch exists to attack, measured on `main` before
anything about the candidate was read.

## 7. The verdict

Judged against the digest-`aca00104…` budget set. Artifacts: `verdict.json`, `verdict.md`; console
log `logs/w84-verdict.log`. Three candidate repetitions, medians with min/max, `ri_logical_writes`
primary with `ri_diskio_byteswritten` beside it in every row.

| phase | baseline median (min/max) | candidate median (min/max) | ratio | ceiling | verdict | baseline `ri_diskio_byteswritten` | candidate `ri_diskio_byteswritten` |
|---|---|---|---|---|---|---|---|
| `P0_cold_index` | 949,066,148 (947,921,636/961,942,788) | 1,797,413,884 (1,744,675,148/1,800,013,452) | **1.89×** | none | regression > 10 % | 1,001,312,256 | 2,079,567,872 |
| `P1_idle_cold` | 1,490,944 (1,323,008/1,556,480) | 1,388,592 (1,114,112/1,466,368) | 0.93× | 1,490,944 | within budget | 0 | 0 |
| `P2_small_edits` | 16,879,664 (16,781,360/17,084,464) | 58,040,984 (52,944,440/81,863,752) | **3.44×** | 16,879,664 | **OVER BUDGET** | 36,864 | 60,366,848 |
| `P3_touch_stage_unstage` | 364,544 (286,720/458,752) | 325,568 (316,280/401,296) | 0.89× | none | recorded | 0 | 0 |
| `P4_amend_same_tree` | 401,408 (303,104/446,464) | 108,454,232 (106,776,048/111,444,288) | **270.18×** | none | regression > 10 % | 0 | 94,855,168 |
| `P5_main_advance` | 5,147,295,504 (4,958,552,632/5,152,481,816) | 1,321,807,648 (887,223,600/1,391,619,344) | **0.26×** | 2,573,647,752 | **within budget** | 7,395,172,352 | 1,565,605,888 |
| `P6_dependent_edits` | 11,392,456 (11,113,944/13,308,200) | 11,959,912 (10,926,264/12,065,824) | 1.05× | none | recorded | 327,680 | 278,528 |
| `P7_dependent_untrack_retrack` | 1,464,290,236 (1,379,998,724/1,550,549,748) | 785,255,028 (765,310,796/810,964,780) | 0.54× | none | recorded | 1,888,002,048 | 733,286,400 |
| `P8_idle_warm` | 14,820,872 (3,166,256/15,038,576) | 15,181,176 (3,609,392/**194,968,104**) | 1.02× | 8,639,500 | **OVER BUDGET** | 9,142,272 | 9,109,504 |

Retained bytes after teardown: candidate **406,852,628** (387,440,832 / 409,036,318) against a
ceiling of 521,864,644 — **within budget**, and in fact below the baseline's 434,887,204. No phase
was incomparable; every phase has three readings on both arms.

### 7.1 The headline holds

`P5_main_advance` — twenty commits on `main` with ten dependent worktrees, every commit awaited to
exact on the primary **and on all ten dependents** — costs the candidate **0.26 ×** the baseline's
process-accounted writes (1.23 GiB against 4.79 GiB), against a ceiling of 0.50 ×. The disk counter
agrees in direction and magnitude (1.46 GiB against 6.89 GiB). This is the claim the branch exists
to make, and on this workload, at this scale, it is supported.

The reuse counters say what produced it (candidate arm, summed over its three repetitions, so divide
by three for a per-run figure):

| counter | `P5_main_advance` | reading |
|---|---|---|
| `views_dedicated_base_advance_total{outcome=dispatched}` | 60 | one advance per commit per run |
| `views_dedicated_base_publish_total{shape=delta}` | 59 | **every publish but one was a delta, not a root** |
| `views_dedicated_base_claim_total{outcome=built}` | 59 | the dependents' bases are built… |
| `views_dedicated_base_claim_total{outcome=reused}` | 0 | …and at this cadence never reused within the phase |
| `views_dependent_recomposition_total` | 60 | each advance recomposes the dependents rather than reindexing them |
| `views_generation_published_total{owner=checkout}` | 119 | ~40 generations published per run |
| `views_probe_answer_total{kind=worktree,exact=exact}` | 679 | every dependent answer in the phase was proven exact |
| `views_probe_answer_total{kind=unrouted,exact=fallback}` | 77 | answers taken before the route existed, during waits |
| `views_coordinator_cycle_total{outcome=built_commit / built_dirty / skipped}` | 30 / 30 / 811 | most coordinator cycles decide there is nothing to do |

So the mechanism behind the 0.26 × is **delta-shaped publishes plus recomposition**, not claim
reuse: within one phase the claim path builds almost every time (`reused=0`), and the saving comes
from what a publish writes, not from how often one is avoided.

### 7.2 Three regressions, preserved

These are not rounded away. Two of them are over a frozen ceiling.

1. **`P4_amend_same_tree`, 270 ×** (401 KB → 108 MB). `git commit --amend --no-edit` produces a new
   commit id over a **byte-identical tree** (the harness logs the tree hash: identical before and
   after). `main` treats it as nothing to do. The candidate dispatches a dedicated-base advance and
   rebuilds: per run, 2 advances, 1 claim built + 1 claim reused, 1 delta publish, 1 re-adopted
   publication, 1 generation published, and the store grows ~7 MiB. A commit-id change with no
   content change is the cleanest possible no-op, and this branch pays 108,454,232 bytes (~103 MiB) of process-accounted writes for it, of which ~95 MiB also reach the disk counter.
   **This is a real defect in the candidate**, not a measurement artefact, and it is exactly the case
   the no-op E2E matrix covers; the ledger's gate-8 row must carry it.
2. **`P2_small_edits`, 3.44 × and over the frozen ceiling** (16.9 MB → 58.0 MB) for ten dirty edits
   in the primary. The phase's counters show no coordinator build at all — no
   `views_coordinator_cycle_total{built_dirty}`, no dedicated-base activity — so these writes are the
   primary's own re-index path, not the view machinery. The candidate's store also grows during the
   phase (116 MB vs the baseline's flat 58 MB). The plan's own budget for this phase was "at most the
   baseline"; it is missed by more than 3 ×.
3. **`P0_cold_index`, 1.89 ×** (905 MiB → 1.67 GiB), and the store it leaves is twice the size
   (113–115 MiB against 58 MiB). This is the cost of the generation model at index time. It is worth
   reading next to the end state: after `P7`, the candidate's store is *smaller* than the baseline's
   (267–289 MiB against 307–315 MiB), and its retained bytes after teardown are 6 % below the
   baseline's. The candidate front-loads storage and recovers it as the workload runs.

And one ceiling miss that is not a regression against the baseline:

4. **`P8_idle_warm` over the absolute idle ceiling** — 15.2 MB median against 8,639,500. The ratio to
   the baseline is 1.02 ×, i.e. the two arms idle almost identically; what fails is the plan's
   absolute 8 MiB/60 s figure, which **the baseline also exceeds** (14.8 MB). Both binaries write
   more than the plan's idle budget on a worked store, so this is an absolute-budget miss shared by
   the arms rather than something the branch introduced. One candidate repetition is an outlier —
   **194,968,104 bytes** in a 62-second idle window — and its cause is visible in the artifact: that
   repetition's idle phase published two generations (`seq_delta=2`, one `built_commit` and one
   `built_dirty` coordinator cycle), i.e. a coordinator cycle landed inside the idle window instead
   of before it. With `GORTEX_RECONCILE_INTERVAL=5s` such a cycle is far more likely to land inside a
   60-second window than at the 1 h default; the median (15.2 MB) is the honest number, and the
   outlier is recorded rather than trimmed.

### 7.3 What else moved

- **`P7_dependent_untrack_retrack` 0.54 ×** (1.36 GiB → 749 MiB): tracking one dependent as a
  worktree and untracking it back costs the candidate about half of what it costs `main`. Still the
  second most expensive phase on both arms — a re-index either way.
- **`P1_idle_cold` 0.93 ×, `P3_touch_stage_unstage` 0.89 ×, `P6_dependent_edits` 1.05 ×**: inside
  the noise of three repetitions. `P3` (touch with identical bytes, `git add -A`, `git reset`) is a
  true no-op on both arms — 0.33 MB and 0.36 MB, with zero disk-counter bytes and no generation
  movement anywhere.
- **Generation sequence across idle and no-op phases** (the plan's `sqlite_sequence.seq` invariant,
  recorded here, asserted in the no-op matrix): `+0` for `P1`, `P2`, `P3` in all three candidate
  repetitions and `+0` for `P8` in two of three — the third is the outlier above. The baseline is
  `+0` for `P1`–`P4` and `P8`. Note `P4`: the candidate moves the sequence by **+1** on a same-tree
  amend where the baseline moves it by 0 — the same defect as §7.2 (1), visible in a second series.
- **WAL resets** (a lower bound on checkpoints): 73/68/73 per baseline run against 38/35/33 per
  candidate run — the candidate restarts the log about half as often.
- **`ri_diskio_byteswritten` read 0 for `P1`, `P3` and `P4` on the baseline** while logical writes
  were 0.3–1.5 MB. The in-tree hazard, confirmed again: the disk counter is not a substitute for the
  primary series, and it never appears here without it.

### 7.4 The verdict, in one paragraph

On a 1,500-file generated corpus, three interleaved repetitions per arm, the branch reduces the
process-accounted writes of the phase it targets — main advancing under ten dependent worktrees — to
**0.26 ×** of `main 56a1c29d`, comfortably inside the 0.50 × ceiling frozen from the baseline, and it
leaves **less** retained storage behind. It pays for that with **1.89 ×** the writes of a cold index
and a store twice as large until the workload works it down, **3.44 ×** the writes of ten dirty edits
(over its frozen ceiling), and a **270 ×** regression on a same-tree amend that `main` handles for
nothing. Idle behaviour is unchanged within noise, and both binaries exceed the plan's absolute idle
budget on a worked store. Nothing here says the write-amplification problem is fixed; it says one
named phase improved by a measured factor on one fixture, under the accelerations of §4, and that
three other phases got worse in ways the branch should answer for.

### 7.5 The 6,000-file arm — one repetition, and the candidate did not finish it

The plan's scale axis asks for `{1500, 6000}`. With 56 GiB free the 6,000-file pair was run **once**
(`GXW8_FIXTURE_FILES=6000 GXW8_FIXTURE_PACKAGES=120 GXW8_REPS=1`, everything else identical),
artifacts under `artifacts/W8m-W8.4/paired6000`, log `logs/w84-paired6000.log`. One repetition is a
single reading, not a median: it is reported to show where the 1,500-file conclusions do and do not
survive a 4× corpus, and nothing here is a budget.

**The baseline completed all nine phases. The candidate failed at `P5_main_advance` and stopped**, so
it has no `P6`–`P8` reading.

| phase | baseline (1 rep) | candidate (1 rep) | ratio | note |
|---|---|---|---|---|
| `P0_cold_index` | 5,809,183,612 | 6,075,728,036 | 1.05× | at 4× the corpus the cold-index gap **closes** (it was 1.89× at 1,500) |
| `P1_idle_cold` | 909,278,384 | **6,766,378,936** | **7.44×** | neither arm is really idle here; the candidate writes 6.3 GiB in a 62 s window and grows its store 245 MB → 512 MB |
| `P2_small_edits` | 30,306,232 | 28,139,968 | 0.93× | the 1,500-file `P2` regression (3.44×) **does not reproduce** at this scale |
| `P3_touch_stage_unstage` | 273,256 | 285,560 | 1.05× | a true no-op on both arms |
| `P4_amend_same_tree` | 222,432 | **149,917,592** | **674×** | the same-tree-amend defect reproduces and is **worse** at scale |
| `P5_main_advance` | 6,559,357,584 | 2,398,133,576 | 0.37× | directionally the 1,500-file result, but **the candidate's phase failed** (below) — the number describes work that was done, not a phase that passed |
| `P6`–`P8` | 12,198,808 / 6,013,953,660 / 2,892,888 | — | — | candidate stopped |

Retained bytes: baseline 879,012,063, candidate 794,227,643 (the candidate's run ended early, so this
is not a like-for-like teardown).

**Why the candidate's `P5` failed.** After its twentieth commit — all 20 commits and all 230
exactness waits on the primary and the ten dependents having succeeded — the phase's trailing
isolation probe asks dependent `wt01` for the main-only symbol and expects "not found". That single
probe came back carrying a non-exact / tool-error label, and `issue767Verdict`
(`issue767_fixture_shared_test.go:461-464`) turns any such answer into an error rather than a
verdict; `requireIsolation` is fatal on any arm that is not the baseline, so the phase failed. The
diagnostics the harness wrote at that moment
(`paired6000/candidate_rep1/diagnostics_P5_main_advance.json`) show all ten dependents
`checkout_ready`, `composed`, `route.ready` and `freshness.exact: true` — i.e. by the time the
diagnostics ran, the views were fine.

Two things follow, and both belong in the ledger:

- **We cannot say from this artifact whether the dependent's view leaked.** What is recorded is that
  the probe did not get an answer it could judge. The guard was **not** relaxed to make the arm
  finish; a fatal isolation outcome on the candidate is the gate-5 contract, and the run is reported
  as it ended.
- **The probe is single-shot where every other check in the phase is a bounded await**
  (`trySearchSymbolIn` asks once; `awaitProbe` retries to a deadline). At 1,500 files it never lost
  the race; at 6,000, under a coordinator that is still republishing, it did. Sharpening the probe —
  await exactness before judging, and record the answer payload in the phase notes — is a follow-up
  for the no-op/lifecycle matrix items, not something to change underneath a measurement that has
  already run.

**What the 6,000-file pair does support**: the `P5` direction (0.37×), the same-tree-amend defect
(worse, 674×), and that the `P0` and `P2` regressions seen at 1,500 files are scale-dependent rather
than constant. **What it flags**: the candidate's post-index settling spills far more work into the
first idle window at 4× corpus (7.44×), which is the single worst candidate number in this document.

## 8. Limitations and deviations

Each of these narrows what the numbers above may be used for. None of them is a reason to discard
the comparison; all of them are reasons not to extend it.

1. **Accelerated janitor, and no confirmatory default-interval arm.** Every measured daemon runs
   with `GORTEX_RECONCILE_INTERVAL=5s`. The execution plan asks for one confirmatory arm at the 1 h
   default; it was **not run**, and it cannot be run through this fixture without changing shared
   code: `issue767_fixture_shared_test.go:104` sets the interval unconditionally and the fixture
   drops every inherited `GORTEX_*` variable (`:94-99`), and that fixture is shared with the E2E
   matrix items. At the 1 h default the janitor would not fire once inside an ~11-minute phase set,
   so such an arm measures "no reconcile work at all", which is a different experiment rather than a
   check on this one. **Next action:** a separate item that parameterises the fixture's interval and
   runs one arm-pair at the default.
2. **Cold/warm is within a run, not across arms.** Every arm-repetition builds a new fixture, store
   and daemon, so all six runs are cold. "Warm" is `P8_idle_warm` against `P1_idle_cold` inside one
   run. The plan's `{cold, warm}` axis is therefore half-covered.
3. **Darwin only.** The primary series comes from `proc_pid_rusage(RUSAGE_INFO_V4)`. The Linux
   `/proc/<pid>/io` path exists in the sampler and is exercised by unit tests, but no Linux arm was
   run, so nothing here describes Linux behaviour.
4. **`wal_resets` is a lower bound.** A PASSIVE checkpoint that does not restart the log leaves the
   header's checkpoint sequence and salt-1 unchanged and is invisible from outside the process.
5. **The counter series is candidate-only, by construction.** `daemon status --format json` and the
   `views_*` counters do not exist on the baseline binary. The harness records that as a named
   unavailability per phase; it never substitutes zeros, and no counter is presented as a paired
   comparison.
6. **The generation census is not a paired series either.** The baseline's store carries no
   populated `view_generations` rows, so `generation_max_after`, per-generation `storage_bytes` and
   generation counts describe the candidate's model; against the baseline they are a structural
   difference, not a regression or an improvement.
7. **n = 3, medians only — and n = 1 at 6,000 files.** Min and max travel with every median, but
   three repetitions support no confidence interval. Differences of a few per cent between the arms
   are inside this measurement's noise; only the order-of-magnitude movements are load-bearing. The
   6,000-file pair (§7.5) is a **single** reading per arm, carries no budget, and the candidate arm
   did not finish it.
8. **Host load was not controlled.** Other agents compiled and ran test suites throughout. The
   `A/B/A/B/A/B` interleave shares that drift between the arms instead of concentrating it in one,
   and the per-repetition min/max show how much of it there was.
9. **Two knobs the plan's hazard list asks for are not applied** (§4): `GORTEX_SKIP_STORE_COMPACT`
   and `GORTEX_QUERY_LOG_DISABLE`. Both are unreachable through this fixture, and both were shown by
   source to be inert at this store size and through this surface. No boot VACUUM ran in either arm.
10. **The reduction step reports; it does not gate.** `TestW8PairedArmsVerdict` writes
    `budgets.json`, `verdict.json` and `verdict.md` and logs every over-budget phase and every
    regression; it does not fail the suite on them. The verdict is evidence for the ledger's gate-8
    row, and a regression is preserved in the artifact rather than turned into a red test that a
    later run might be tempted to loosen.
11. **The idle `sqlite_sequence.seq` invariant is measured here, asserted elsewhere.** This harness
    records the generation sequence at both boundaries of every phase (§7 reports it); the assertion
    that it does not move across an idle or no-op phase belongs to the E2E no-op matrix.
12. **The 6,000-file candidate arm ended at a failed isolation probe** (§7.5), so the branch has no
    end-to-end 6,000-file result for `P6`-`P8`, and the question the probe asked — did the dependent
    view leak a main-only symbol at that instant — is **unanswered**, not answered in the negative.
    The probe's single-shot shape is a harness sharpness gap; the guard was not relaxed.
13. **A small-fixture replay is not sustained daemon behaviour**, and process-accounted writes are
    not NAND writes. Both statements are repeated here deliberately: they are the two ways a reader
    is most likely to over-read this document.

## 9. Reproduction

Four steps. `$SP` is the scratch root the artifacts are kept under and `$R` a short private root
(`/private/tmp/gxh-w84` here — short, because a unix socket path has a length limit).

**1. Build the two arm binaries from exported trees.**

```bash
export GOWORK=off GOTOOLCHAIN=local GOFLAGS="-mod=mod -buildvcs=false" GOPROXY=off
export GOCACHE="$HOME/Library/Caches/go-build" GOMODCACHE="$HOME/go/pkg/mod"
for id in 56a1c29d514d8f7d3b5feec455c7b57de358b1d3 2fd5db821ab0acede84ec0cb8bda65944d088e20; do
  mkdir -p "$R/src-$id" && git archive "$id" | tar -x -C "$R/src-$id"
  (cd "$R/src-$id" && go build -o "$SP/harness/bin/gortex-$id" ./cmd/gortex/)
  shasum -a 256 "$SP/harness/bin/gortex-$id"
done
```

**2. Build the harness (one test binary, from this branch's `cmd/gortex`).**

```bash
go test -c -o "$R/w84.test" ./cmd/gortex/     # or: harness/validate.sh compile cmd normal
shasum -a 256 "$R/w84.test"
```

**3. Run the paired workload.** `env -i` plus an explicit allowlist; nothing is inherited.

```bash
mkdir -p "$R/paired1500"/{home,tmp,fixtures} "$SP/artifacts/W8m-W8.4/paired1500"
env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin \
  HOME="$R/paired1500/home" TMPDIR="$R/paired1500/tmp" \
  GORTEX_ISSUE767_ARTIFACT_DIR="$R/paired1500/fixtures" \
  GXW8_BASELINE_BINARY="$SP/harness/bin/gortex-baseline-56a1c29d" \
  GXW8_TEST_BINARY="$SP/harness/bin/gortex-2fd5db82" \
  GXW8_ARTIFACT_DIR="$SP/artifacts/W8m-W8.4/paired1500" \
  GXW8_REPS=3 GXW8_EDIT_INTERVAL=10s \
  "$R/w84.test" -test.run '^TestW8SustainedWriteAmplification$' -test.v -test.timeout 8h
```

Without `GXW8_TEST_BINARY` the test skips with a named reason, so it never runs in a default
`go test ./...`. `GXW8_BASELINE_BINARY` is what makes the run paired; without it only the candidate
arm runs. The defaults the run used (1,500 files / 60 packages / seed 767 / 10 worktrees /
20 commits / 10 edits / 60 s idles / 1 s sampling) come from `w8ConfigFromEnv` and every one of them
has a `GXW8_*` override with bounds that are refused by name.

**4. Freeze the budgets, then judge.** Two invocations, in this order — the first one is the freeze
and it never reads the candidate:

```bash
env ... GXW8_PAIRED_ARTIFACT_DIR="$SP/artifacts/W8m-W8.4/paired1500" GXW8_BUDGETS_ONLY=1 \
  "$R/w84.test" -test.run '^TestW8PairedArmsVerdict$' -test.v     # writes budgets.json
env ... GXW8_PAIRED_ARTIFACT_DIR="$SP/artifacts/W8m-W8.4/paired1500" \
  "$R/w84.test" -test.run '^TestW8PairedArmsVerdict$' -test.v     # writes verdict.json + verdict.md
```

`budgets.json` refuses to be rewritten by the second pass, and the verdict refuses to judge against
a budget set whose bytes no longer match the digest recorded inside it.

**Artifacts.** `$SP/artifacts/W8m-W8.4/paired1500/{baseline,candidate}_rep{1,2,3}/` each hold
`manifest.json`, `report.json`, one `phase_<name>.json` per phase and the raw `samples.ndjson`;
the run directory holds `budgets.json`, `verdict.json` and `verdict.md`. The console log is
`$SP/logs/w84-paired1500.log` and the host state is `$SP/logs/w84-paired1500-host.log`.
