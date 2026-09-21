# Incremental indexing — paired sustained-I/O measurements

This document records the paired measurement the incremental-indexing work is judged on: one
baseline arm on `main`, one candidate arm on this branch, the same generated corpus and the same
nine-phase workload driven against both, with the baseline's budgets frozen before the candidate's
numbers were read.

It is the evidence half of acceptance gate 10 (*reproducible release evidence*) and the only place
in this repository where a performance claim about the branch may be made. The execution ledger
cites it; it does not restate it.

**It now carries two verdicts.** §6-§7 are the first one, on candidate `2fd5db82`, and they are left
standing as the historical record including the attributions that later turned out to be wrong.
**§8 is the second verdict**, on candidate `271a9e9f` after the six fix items of
`scratchpad/reports/io-fix-plan.md`, judged against the **same frozen baseline and the same
`budgets.json`** — never re-frozen. Every correction §8 makes to §7 is listed in §8.6 and marked
again at the point in §7 where it applies. A reader who needs the latest recorded historical numbers
wants §8; a reader who needs to know what was believed when, and on what evidence, wants §7 with
those marks.

**Current-source boundary (September 15, 2026).** Both paired verdicts predate the merge of `main`
at `a4b5c4df` and the subsequent publication correction in
[`SparseGenerationBuilder.withholdContextPayload`](../internal/indexer/builder_generation.go#L975).
That correction conservatively retains contract-bearing context paths as explicit output so that
canonical contracts and surviving ownership edges remain coherent with generation masks. It can
retain extra unchanged payload. No phase of the paired protocol has run after this change; the
ratios, budgets and regressions below describe only the recorded historical candidates. Current
package tests and successful live publication do not measure that write cost. The later
[`OverlaidView.detachedBaseNodes`](../internal/graph/overlay.go#L1023) correction also postdates these
runs: aggregate totals now resolve base identities before applying file-coverage adjustments. Its
additional lookup work is bounded by overlay candidates and has not been benchmarked. The execution
ledger records current validation separately.

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

**§8 adds a third arm binary and no fourth**: the post-fix candidate
`fix/incremental-index-write-amplification` at `271a9e9f6bf2c3f3a9b491818aa70549c0b5ae6a`,
`harness/bin/gortex-271a9e9f`, sha256
`6378b76935d5c3dea0c106fa30d49364390cdb56164e2aea0fed7e1cf0aea271`, `version --short` `v0.64.3`.
It is judged against the **same** baseline arm and the **same** frozen budgets; the baseline was not
re-run and is not re-buildable differently. See §8 for what that re-use costs (§9(14)).

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

The harness is `cmd/gortex/sustained_io_integration_test.go` plus `sustained_io_sampler_test.go`,
`sustained_io_fixture_generator_test.go` and the shared private-daemon fixture in
`issue767_fixture_shared_test.go`. It is opt-in: without `GX_SUSTAINED_IO_TEST_BINARY` it skips with
a named reason and never runs in a default `go test ./...`.

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
uses (`pid`, `readIO`, `readSize`, `readWAL`) is wired explicitly when the run sampler is
constructed; a reader that is not wired is named in every sample (`unwired_readers`) and costs the
sample its health, because a zero from a missing wiring is indistinguishable from a real measurement
of zero.

On the Linux arm the process reader would use `/proc/<pid>/io write_bytes`; this measurement ran on
Darwin only.

## 4. Environment, isolation and accelerations

Every process runs under `env -i` with an explicit allowlist: no inherited credentials, private
`HOME`, `TMPDIR`, `XDG_*`, private gitconfig, `GIT_CONFIG_NOSYSTEM=1`. The daemon's own private root
lives under a short `/private/tmp/gxh-w84/...` path. **The user's live daemon, store and
configuration are never addressed.**

The fixture builds the daemon's environment itself. It **drops every inherited `GORTEX_*`, `XDG_*`
and `GIT_*` variable** and appends its own list
(the env list `newIssue767FixtureWithCorpus` builds in `cmd/gortex/issue767_fixture_shared_test.go`),
so the only product knobs in force are the
ones below; a variable exported by the operator around the run cannot reach either daemon. Each
run's `manifest.json` records the child's environment verbatim, which is where this table is
checkable.

Knobs in force, both arms, each of which changes the numbers:

| knob | value | where | why, and what it costs |
|---|---|---|---|
| `GORTEX_RECONCILE_INTERVAL` | `5s` (default `1h`) | fixture env (`issue767_fixture_shared_test.go`, `newIssue767FixtureWithCorpus`) | the janitor has to run inside a ~10-minute workload at all. **These are not default-configuration numbers**; the default interval would move almost all reconcile work outside every measured phase. |
| `--embeddings=false` | off | daemon start flag (`issue767_fixture_shared_test.go`, `(*issue767Fixture).start`) | embeddings are a separate, much larger I/O source and are not what this measures. Note this suppresses embedding *work*, not the model file: both arms retain an identical 33,514,412-byte `embedding_model` bucket in the destination census, which therefore cancels in every comparison. |
| `GORTEX_TELEMETRY=0` | off | fixture env (`newIssue767FixtureWithCorpus`) | telemetry is dormant without an endpoint, but its consent/rollup files under the private data dir are writes like any other. |
| `GORTEX_DAEMON_PPROF_ADDR` | `127.0.0.1:0` | fixture env (`newIssue767FixtureWithCorpus`) | an ephemeral local pprof listener; it writes nothing unless profiled, and it is identical on both arms. |
| `--backend sqlite --backend-path <root>/store.sqlite`, `--no-progress` | — | daemon start flags (`(*issue767Fixture).start`) | the measured store is one named file inside the private root, so the destination census can attribute it. |
| `GX_SUSTAINED_IO_EDIT_INTERVAL` | `10s` (harness default `30s`) | harness knob, not a product knob | six arm-repetitions at 30 s between edits add 20 minutes of pure waiting. The edits themselves are unchanged; only the quiet time between them is shorter, which makes `P2_small_edits` a denser workload than the default and is applied identically to both arms. |

Two knobs the measurement protocol's hazard list asks for are **not** applied, and this is deliberate:

- **`GORTEX_SKIP_STORE_COMPACT=1`** cannot reach the daemon (the fixture drops inherited `GORTEX_*`),
  and it would be a no-op here in any case: `maybeCompactStore` VACUUMs only when the freelist is
  **both** larger than 1 GiB and more than half the file's pages
  (`cmd/gortex/daemon_compact.go:47-65`, byte-identical at `56a1c29d`), while the largest store any
  arm reached is ~330 MB. The boot VACUUM never ran in either arm, so nothing was suppressed and
  nothing the hazard list wanted excluded was measured. Verified by source, not by absence of evidence.
- **`GORTEX_QUERY_LOG_DISABLE=1`** is likewise unreachable, and the query log belongs to the MCP
  surface (`internal/mcp/query_log.go:121`). The harness drives the CLI; no query-log bytes appear
  in either arm's destination census, whose 14 buckets account for every retained byte.

  > **Correction (§8.6(4)).** The last clause is **wrong**: query-log bytes were there all along,
  > folded into the `cache` bucket, which is why they were invisible. The census now gives the query
  > log a bucket of its own, and the difference is visible across the two arms: the baseline runs
  > carry 127,455 / 128,165 / 128,508 bytes in `cache` and 0 in `query_log`, while §8's candidate
  > runs carry 84 / 84 / 88 in `cache` and 136,818 / 120,862 / 126,673 in `query_log`. Same bytes,
  > previously mislabelled. The original sentence is kept for the record. Everything else in this
  > bullet stands.

Host load is **not** controlled. Other agents were compiling and running test suites on this
machine throughout; `uptime`'s load averages, free disk, CPU count and RAM are recorded at the start
and end of the run in `scratchpad/logs/w84-paired1500-host.log`. This is the reason the protocol
interleaves the arms `A/B/A/B/A/B` rather than running all of one arm and then all of the other:
drift in host load is shared by adjacent repetitions instead of being absorbed entirely by one arm.

## 5. Protocol

**Corpus.** Generated, not cloned, so a manifest can rebuild it byte for byte: the harness's fixture
generator at `{Files: 1500, Packages: 60, Seed: 767}` — one `go.mod`, a root package holding the
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
(`TestSustainedIOPairedArmsVerdict` in `cmd/gortex/sustained_io_paired_arms_test.go`) is run twice:
once with `GX_SUSTAINED_IO_BUDGETS_ONLY=1`, which reads only the `baseline_rep*` artifacts and
stops, and once without, which produces the verdict. Section 6 was written from the first run's
output before the second was started.

**Ceilings**, from the paired-I/O protocol:

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
code, not by memory: the budget-freeze reducer
(`cmd/gortex/sustained_io_paired_arms_test.go:307-320`) refuses any run whose manifest is not the
baseline arm, so no budget can absorb a candidate number even by accident; the frozen-budget writer
(`:653-671`) never overwrites; the ceiling formulas (`:569-597`) were written before any number
existed. The freeze pass is a separate invocation with `GX_SUSTAINED_IO_BUDGETS_ONLY=1` that stops
before loading the candidate directories, and this section was written from its output
(`logs/w84-freeze.log`) before the verdict pass ran. The one thing the operator is **not** blind to
is the run's own console log, which prints both arms as they interleave; that is inherent to an
A/B/A/B/A/B run in one process, and it is why the mechanism lives in the code rather than in a claim
about who looked at what.

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
  Note which way this one binds: the baseline itself **exceeds** the protocol's absolute idle budget,
  so the ceiling the candidate is judged against is the protocol's 8 MiB figure, not the baseline's
  behaviour.
- `P2_small_edits` — at most the baseline median, 16,879,664.
- `P5_main_advance` — the headline: 0.50 × 5,147,295,504 = 2,573,647,752.
- every other phase — no ceiling; the baseline median is recorded and a candidate above 1.10 × it is
  preserved as a regression.

**What the baseline arm looks like, beyond the ceilings.** All three repetitions completed all nine
phases with no failed phase, 250 exactness waits each, and one sample failure each (the first 1 Hz
sample, taken before the child daemon has a pid — it is named in the sample line, not counted as a
zero). Per-run WAL resets: 73 / 68 / 73. The `view_generations` sequence on `main` moves by +1 at
`P0`, **0** across `P1`, `P2`, `P3`, `P4` and `P8` in all three repetitions, +66/+62/+66 at `P5`, +3
at `P6` and +2 at `P7` — so on the baseline the twenty commits of `P5` are what produce generations,
and the dirty-edit, no-op and idle phases produce none. Dependent isolation held in every baseline
repetition; the harness's baseline-only isolation exemption
(`sustained_io_integration_test.go:1690-1699`) was never exercised.

**The one number to keep in view**: on the baseline, `P5_main_advance` — twenty commits on main with
ten dependent worktrees discovered — accounts for 4.79 GiB of process-accounted writes, 60 % of the
whole run. That is the write amplification this branch exists to attack, measured on `main` before
anything about the candidate was read.

## 7. The verdict

> **This is the FIRST verdict, on candidate `2fd5db82`.** It is kept unedited except for the marked
> corrections below. The branch's current numbers are in **§8**, which re-runs the candidate arm
> against this same frozen baseline after the round of fixes. The corrections are collected in §8.6.

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

   > **Correction (§8.6(2)).** The defect is real; the **stimulus named here is wrong**. Every byte
   > of the 108,454,232 lands 1-4 s after the `git add -A && git commit` this phase performs *inside
   > its own window*, and the amend proper is below the idle floor — zero DML in all 62 tables across
   > six repetitions. The commit's cost was a 209-file closure payload for a 10-file change (~20x)
   > plus ~12.7x SQLite amplification. So the amend met the no-op contract and the commit did not.
   > The phase is now split into `P4a_commit_tree_change` and `P4b_amend_same_tree` so this is
   > readable from the artifact; §8 reads 265,128 and 248,712 bytes respectively, and the whole phase
   > 513,840 against the baseline's 401,408. The original numbers above are kept for the record.
2. **`P2_small_edits`, 3.44 × and over the frozen ceiling** (16.9 MB → 58.0 MB) for ten dirty edits
   in the primary. The phase's counters show no coordinator build at all — no
   `views_coordinator_cycle_total{built_dirty}`, no dedicated-base activity — so these writes are the
   primary's own re-index path, not the view machinery. The candidate's store also grows during the
   phase (116 MB vs the baseline's flat 58 MB). The protocol's own budget for this phase was "at most the
   baseline"; it is missed by more than 3 ×.

   > **Correction (§8.6(1)).** "These writes are the primary's own re-index path" is **wrong**.
   > 103 % of the delta is one SQLite `wal_autocheckpoint(8000)` drain landing inside the window,
   > because the candidate entered `P2` about 1,120 WAL frames deeper. Re-reducing the **same frozen
   > artifacts** on the checkpoint-excluded series moves this row to 15,433,968 (0.91x) with
   > 42,607,016 booked as checkpoint bytes, and a controlled 40-edit re-run collapses the ratio
   > 3.44x → 1.042x with the candidate's edit path at 0.91x of the baseline's. The observation that
   > the counters show no coordinator build at all was true and was the clue; the conclusion drawn
   > from it was not. §8 reads 0.96x with 0 checkpoint bytes on either arm. The original numbers
   > above are kept for the record.
3. **`P0_cold_index`, 1.89 ×** (905 MiB → 1.67 GiB), and the store it leaves is twice the size
   (113–115 MiB against 58 MiB). This is the cost of the generation model at index time. It is worth
   reading next to the end state: after `P7`, the candidate's store is *smaller* than the baseline's
   (267–289 MiB against 307–315 MiB), and its retained bytes after teardown are 6 % below the
   baseline's. The candidate front-loads storage and recovers it as the workload runs.

And one ceiling miss that is not a regression against the baseline:

4. **`P8_idle_warm` over the absolute idle ceiling** — 15.2 MB median against 8,639,500. The ratio to
   the baseline is 1.02 ×, i.e. the two arms idle almost identically; what fails is the protocol's
   absolute 8 MiB/60 s figure, which **the baseline also exceeds** (14.8 MB). Both binaries write
   more than the protocol's idle budget on a worked store, so this is an absolute-budget miss shared by
   the arms rather than something the branch introduced. One candidate repetition is an outlier —
   **194,968,104 bytes** in a 62-second idle window — and its cause is visible in the artifact: that
   repetition's idle phase published two generations (`seq_delta=2`, one `built_commit` and one
   `built_dirty` coordinator cycle), i.e. a coordinator cycle landed inside the idle window instead
   of before it. With `GORTEX_RECONCILE_INTERVAL=5s` such a cycle is far more likely to land inside a
   60-second window than at the 1 h default; the median (15.2 MB) is the honest number, and the
   outlier is recorded rather than trimmed.

   > **Correction (§8.6(5)).** This row was judged on `ri_logical_writes`, and 11,481,168 of the
   > baseline's own comparable 14,820,872-byte figure is the store's 5-minute periodic PASSIVE
   > checkpoint — identical on `main`. On the checkpoint-excluded series the frozen baseline's `P8`
   > median is 3,299,312, which is the ceiling §8 judges against, and §8's candidate reads 2,799,664.
   > The verdict above was correct about the series it was computed on and wrong about what that
   > series meant. The original numbers are kept for the record.

### 7.3 What else moved

- **`P7_dependent_untrack_retrack` 0.54 ×** (1.36 GiB → 749 MiB): tracking one dependent as a
  worktree and untracking it back costs the candidate about half of what it costs `main`. Still the
  second most expensive phase on both arms — a re-index either way.
- **`P1_idle_cold` 0.93 ×, `P3_touch_stage_unstage` 0.89 ×, `P6_dependent_edits` 1.05 ×**: inside
  the noise of three repetitions. `P3` (touch with identical bytes, `git add -A`, `git reset`) is a
  true no-op on both arms — 0.33 MB and 0.36 MB, with zero disk-counter bytes and no generation
  movement anywhere.
- **Generation sequence across idle and no-op phases** (the protocol's `sqlite_sequence.seq` invariant,
  recorded here, asserted in the no-op matrix): `+0` for `P1`, `P2`, `P3` in all three candidate
  repetitions and `+0` for `P8` in two of three — the third is the outlier above. The baseline is
  `+0` for `P1`–`P4` and `P8`. Note `P4`: the candidate moves the sequence by **+1** on a same-tree
  amend where the baseline moves it by 0 — the same defect as §7.2 (1), visible in a second series.
  **Correction (§8.6(2)):** the `+1` is produced by the tree-changing **commit** the phase performs
  inside its own window, not by the amend; in §8 the sequence does not move across `P4` on either arm
  in any repetition.
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
nothing. Idle behaviour is unchanged within noise, and both binaries exceed the protocol's absolute idle
budget on a worked store. Nothing here says the write-amplification problem is fixed; it says one
named phase improved by a measured factor on one fixture, under the accelerations of §4, and that
three other phases got worse in ways the branch should answer for.

> **Correction (§8.6(1), §8.6(2), §8.6(5)).** Three of this paragraph's four claims about the
> candidate's costs do not survive the diagnoses: the 3.44x was one WAL checkpoint drain inside the
> window and not "the writes of ten dirty edits"; the 270x belongs to the ten-file **commit** the
> phase performs inside its own window and not to "a same-tree amend that `main` handles for
> nothing"; and the shared idle-budget miss was read off the series that carries the periodic
> checkpoint. The **1.89x cold index and the store twice as large were real** — and are the single
> largest thing the round of fixes closed. §8's one-paragraph verdict replaces this one for the
> branch's current state; this paragraph stands as what was believed on 2026-09-13.

### 7.5 The 6,000-file arm — one repetition, and the candidate did not finish it

The protocol's scale axis asks for `{1500, 6000}`. With 56 GiB free the 6,000-file pair was run **once**
(`GX_SUSTAINED_IO_FIXTURE_FILES=6000 GX_SUSTAINED_IO_FIXTURE_PACKAGES=120 GX_SUSTAINED_IO_REPS=1`,
everything else identical), artifacts under `artifacts/W8m-W8.4/paired6000`, log
`logs/w84-paired6000.log`. One repetition is a single reading, not a median: it is reported to show
where the 1,500-file conclusions do and do not survive a 4× corpus, and nothing here is a budget.

**The baseline completed all nine phases. The candidate failed at `P5_main_advance` and stopped**, so
it has no `P6`–`P8` reading.

| phase | baseline (1 rep) | candidate (1 rep) | ratio | note |
|---|---|---|---|---|
| `P0_cold_index` | 5,809,183,612 | 6,075,728,036 | 1.05× | at 4× the corpus the cold-index gap **closes** (it was 1.89× at 1,500) |
| `P1_idle_cold` | 909,278,384 | **6,766,378,936** | **7.44×** | neither arm is really idle here; the candidate writes 6.3 GiB in a 62 s window and grows its store 245 MB → 512 MB |
| `P2_small_edits` | 30,306,232 | 28,139,968 | 0.93× | the 1,500-file `P2` regression (3.44×) **does not reproduce** at this scale |
| `P3_touch_stage_unstage` | 273,256 | 285,560 | 1.05× | a true no-op on both arms |
| `P4_amend_same_tree` | 222,432 | **149,917,592** | **674×** | the defect reproduces and is **worse** at scale — but see §8.6(2): the stimulus is the ten-file **commit** the phase performs inside its own window, not the amend |
| `P5_main_advance` | 6,559,357,584 | 2,398,133,576 | 0.37× | directionally the 1,500-file result, but **the candidate's phase failed** (below) — the number describes work that was done, not a phase that passed |
| `P6`–`P8` | 12,198,808 / 6,013,953,660 / 2,892,888 | — | — | candidate stopped |

Retained bytes: baseline 879,012,063, candidate 794,227,643 (the candidate's run ended early, so this
is not a like-for-like teardown).

**Why the candidate's `P5` failed.** After its twentieth commit — all 20 commits and all 230
exactness waits on the primary and the ten dependents having succeeded — the phase's trailing
isolation probe asks dependent `wt01` for the main-only symbol and expects "not found". That single
probe came back carrying a non-exact / tool-error label, and `issue767Verdict`
(`cmd/gortex/issue767_fixture_shared_test.go`) turns any such answer into an error rather than a
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

  > **Correction (§8.6(3)).** It did not leak. The dedicated isolation diagnosis reproduced the same
  > label 3 times in 14 single shots at 6,000 files — a truthful, self-healing
  > `fallback_reason:"base_changed"`, clearing in 0.45 / 1.31 / 1.20 s — and found **0 named result
  > rows in 159 parsed samples, on both arms**. What failed was a single-shot probe racing a
  > coordinator that was still republishing; it is now a bounded retry, which is why §8's `P5`
  > completed in all three repetitions. The statement above is kept for the record.
- **The probe is single-shot where every other check in the phase is a bounded await**
  (`trySearchSymbolIn` asks once; `awaitProbe` retries to a deadline). At 1,500 files it never lost
  the race; at 6,000, under a coordinator that is still republishing, it did. Sharpening the probe —
  await exactness before judging, and record the answer payload in the phase notes — is a follow-up
  for the no-op/lifecycle matrix items, not something to change underneath a measurement that has
  already run.

**What the 6,000-file pair does support**: the `P5` direction (0.37×), the same-tree-amend defect
(worse, 674×), and that the `P0` and `P2` regressions seen at 1,500 files are scale-dependent rather
than constant. **What it flags**: the candidate's post-index settling spills far more work into the
first idle window at 4× corpus (7.44×), which was the single worst candidate number in this document
when §7 was written and remains the worst ratio in it: §8 re-ran only the 1,500-file axis, so neither
of §7.5's two headline numbers has a post-fix counterpart (§9(17)).

## 8. Second verdict (post-fix)

§6 and §7 are the historical record and are left standing. This section is a **second** candidate
arm, run after the six fix items of `scratchpad/reports/io-fix-plan.md` landed, judged against the
**same frozen budgets** — never re-frozen — with the checkpoint-excluded write series as the judged
series and the checkpoint bytes reported beside it in every row.

**What is new and what is not.**

| | first verdict (§7) | second verdict (§8) |
|---|---|---|
| baseline arm | `main 56a1c29d`, `gortex-baseline-56a1c29d`, sha256 `8785a8b4…` | **the same three runs, not re-run** |
| candidate arm | `2fd5db82`, `gortex-2fd5db82`, sha256 `68680abc…` | `271a9e9f6bf2c3f3a9b491818aa70549c0b5ae6a`, `gortex-271a9e9f`, sha256 `6378b76935d5c3dea0c106fa30d49364390cdb56164e2aea0fed7e1cf0aea271` |
| budgets | frozen `aca00104…` | the same file, byte-identical (md5 `6caf02683492a65009ffe1ed95bac0b7` before and after), augmented in-process to `cbc3794a…` |
| artifacts | `artifacts/W8m-W8.4/paired1500` | `artifacts/W8i-F6/paired1500-postfix` (the three frozen `baseline_rep*` directories copied in read-only, plus three new `candidate_rep*`) |
| repetitions | 3 per arm, interleaved | 3 candidate repetitions; **no fresh baseline arm was run** |
| fixture | `ff6ebe14…`, 1,500 files / 60 packages / seed 767 | the same digest — the verdict refuses an unequal one |

The candidate binary's source identity is checkable without trusting the build recipe: `git archive
271a9e9f | tar -x` into a scratch tree and `diff -r -x '*_test.go'` against the implementation
worktree reports **zero differing files**, and rebuilding `./cmd/gortex` from the worktree reproduces
sha256 `6378b769…` byte for byte. (A build of the exported tree is *not* byte-identical — 328,652,530
against 328,735,090 bytes — because without `-trimpath` the module root path is compiled in; the
source diff, not the hash, is what establishes identity here.)

**Why reusing the frozen baseline is legitimate, and where it stops being so.** The harness changed
between the two verdicts, but every change is on the *measurement* side — a derived series, a
sub-window bracket, a bounded probe retry, a reduction mapping. The daemon's work is driven by the
same nine phases, the same knobs (§4) and the same fixture. Two phases no longer map 1:1 onto the
frozen protocol, and both are handled by mapping rather than by re-freezing:

- `P1_idle_cold` and `P8_idle_warm` now measure idle **twice** — a polling arm that reproduces the
  frozen protocol exactly (60 s, one read-only search every 5 s) and a quiet arm that makes no
  client calls at all. The polling arm runs **first**, so it opens where the frozen window opened;
  the reduction judges the frozen phase name on it (the judged-window mapping,
  `cmd/gortex/sustained_io_paired_arms_test.go:443-459`) and leaves the quiet arm as an un-budgeted
  row. Judging the phase *total* would compare ~120 s of candidate against ~62 s of baseline.
- `P4_amend_same_tree` is split into `P4a_commit_tree_change` (the tree-changing commit the amend
  needs, which the phase performs inside its own window) and `P4b_amend_same_tree` (the amend). The
  **phase** row is still what the frozen budget names and what is judged; the two sub-windows are
  recorded beside it, which is what finally separates the commit's cost from the amend's.

No frozen phase was dropped, renamed or re-run. The baseline arm binary
`harness/bin/gortex-baseline-56a1c29d` and its artifacts stand exactly as §6 froze them.

### 8.1 The table

Judged against budget set `cbc3794a…` (the frozen `aca00104…` with the checkpoint-excluded series
filled in from the same baseline runs' `samples.ndjson`; the file on disk was read, never rewritten).
Artifacts `artifacts/W8i-F6/paired1500-postfix/{verdict.json,verdict.md}`, console log
`logs/W8i-F6/verdict.log`.

| phase | judged series | judged row (baseline / candidate) | baseline judged median (min/max) | candidate judged median (min/max) | ratio | first-verdict candidate (total series) | baseline `wal_checkpoint_bytes` | candidate `wal_checkpoint_bytes` | ceiling | verdict |
|---|---|---|---|---|---|---|---|---|---|---|
| `P0_cold_index` | `ri_logical_writes` | phase @26 s / phase @52 s | 949,066,148 (947,921,636/961,942,788) | 989,877,036 (989,119,308/994,855,020) | **1.04×** | 1,797,413,884 (1.89×) | 889,557,516 | 898,643,860 | none | recorded |
| `P1_idle_cold` | `ri_logical_writes` | phase @64 s / window `P1a_idle_cold_polling` @60 s | 1,490,944 (1,323,008/1,556,480) | 1,040,384 (765,952/1,044,480) | **0.70×** | 1,388,592 (0.93×) | 0 | 0 | 1,490,944 | within budget |
| `P2_small_edits` | `ri_logical_writes_excl_checkpoint` | phase @107 s / phase @107 s | 16,879,664 (16,781,360/17,084,464) | 16,162,864 (16,035,888/16,355,376) | **0.96×** | 58,040,984 (3.44×, **over budget**) | 0 | 0 | 16,879,664 | within budget |
| `P3_touch_stage_unstage` | `ri_logical_writes_excl_checkpoint` | phase @31 s / phase @31 s | 364,544 (286,720/458,752) | 335,792 (299,104/24,219,696) | **0.92×** | 325,568 (0.89×) | 0 | 32,178,256 (32,848/32,944,208) | none | recorded |
| `P4_amend_same_tree` | `ri_logical_writes_excl_checkpoint` | phase @33 s / phase @53 s | 401,408 (303,104/446,464) | 513,840 (438,016/595,232) | **1.28×** | 108,454,232 (270.18×) | 0 | 0 | none | **regression > 10 %** |
| `P5_main_advance` | `ri_logical_writes` | phase @172 s / phase @151 s | 5,147,295,504 (4,958,552,632/5,152,481,816) | 1,447,530,384 (1,420,576,656/1,496,648,000) | **0.28×** | 1,321,807,648 (0.26×) | 4,420,177,368 | 674,583,712 (354,344,176/873,265,792) | 2,573,647,752 | within budget |
| `P6_dependent_edits` | `ri_logical_writes` | phase @56 s / phase @47 s | 11,392,456 (11,113,944/13,308,200) | 8,707,752 (4,775,480/8,790,232) | **0.76×** | 11,959,912 (1.05×) | 0 | 0 | none | recorded |
| `P7_dependent_untrack_retrack` | `ri_logical_writes` | phase @54 s / phase @57 s | 1,464,290,236 (1,379,998,724/1,550,549,748) | 752,579,724 (698,810,348/803,904,484) | **0.51×** | 785,255,028 (0.54×) | 1,308,460,296 | 546,547,728 (523,672,456/568,116,416) | none | recorded |
| `P8_idle_warm` | `ri_logical_writes_excl_checkpoint` | phase @62 s / window `P8a_idle_warm_polling` @60 s | 3,299,312 (3,166,256/14,796,120) | 2,799,664 (2,147,408/4,228,088) | **0.85×** | 15,181,176 total (1.02×, **over budget**) | 24,752 | 0 (0/108,840,832) | 3,299,312 | within budget |

`ri_diskio_byteswritten`, reported beside the primary series and never instead of it — baseline
median then candidate median: `P0` 1,001,312,256 / 1,055,571,968; `P1` 0 / 0; `P2` 36,864 / 36,864;
`P3` 0 / 36,876,288; `P4` 0 / 0; `P5` 7,395,172,352 / 1,714,257,920; `P6` 327,680 / 0;
`P7` 1,888,002,048 / 735,490,048; `P8` 9,142,272 / 0 (0/150,515,712).

**over budget: none. incomparable: none. regressions preserved: `P4_amend_same_tree` 1.28×.** All
nine phases completed in all three repetitions; no failed phase; 256 exactness waits per run
(baseline 250 — the extra six are the bounded isolation-probe retries the harness corrections
added); one sample failure per run (the first 1 Hz sample, before the child has a pid, named in the
sample line).

**Sub-window rows** (candidate only — the frozen baseline predates the split and is judged on its
phase row, which is what the frozen protocol measured):

| phase | window | candidate `ri_logical_writes` median (min/max) | client calls | note |
|---|---|---|---|---|
| `P1_idle_cold` | `P1a_idle_cold_polling` | 1,040,384 (765,952/1,044,480) | 12 | the row the frozen `P1_idle_cold` ceiling is applied to |
| `P1_idle_cold` | `P1b_idle_cold_quiet` | 462,848 (401,408/552,960) | 0 | recorded, never judged — no frozen ceiling names it |
| `P4_amend_same_tree` | `P4a_commit_tree_change` | 265,128 (225,464/281,736) | 5 | recorded, never judged |
| `P4_amend_same_tree` | `P4b_amend_same_tree` | 248,712 (212,552/313,496) | 1 | recorded, never judged |
| `P8_idle_warm` | `P8a_idle_warm_polling` | 2,799,664 (2,147,408/113,068,920) | 12 | the row the frozen `P8_idle_warm` ceiling is applied to; checkpoint-excluded 2,799,664 (2,147,408/4,228,088) |
| `P8_idle_warm` | `P8b_idle_warm_quiet` | 2,078,296 (1,415,360/2,221,808) | 0 | recorded, never judged |

**Retained bytes after teardown**: candidate **375,751,391** (374,393,102 / 378,579,275) against the
frozen ceiling of 521,864,644 and the baseline's 434,887,204 — **within budget**, and 0.86 × the
baseline (the first verdict's candidate was 406,852,628, 0.94 ×).

**Store and generation state at the phase boundaries.** Medians of three repetitions:

| boundary | baseline store bytes | candidate store bytes | ratio | baseline `view_generations`.Count / Sequence | candidate |
|---|---|---|---|---|---|
| end of `P0_cold_index` | 61,267,968 | 61,648,896 | **1.006×** | 0 / 0 | **0 / 0** |
| end of `P4_amend_same_tree` | 61,272,064 | 62,402,560 | 1.018× | 0 / 0 | **0 / 0** |
| end of `P7`/`P8` (run end) | 324,927,488 | 270,340,096 | **0.83×** | 41 / 71 | 43 / 48 |

The first verdict's candidate left a store of 113–115 MiB at the end of `P0` against the baseline's
58 MiB and moved the generation sequence by +1 across `P4`. Neither happens now: at the end of `P0`
the candidate's store is 61.6 MB against the baseline's 61.3 MB, `view_generations` is empty, and the
`nodes` table holds one copy of the corpus (14,989 rows on both arms, as against 29,978 before).

**Destination census after teardown** (medians; the 14-bucket attribution of every retained byte):
`store` 324,927,488 → 270,340,096; `store_wal` 67,108,864 → 64,193,752; `sidecar_db` 4,695,192 →
**2,516,768** (0.54 ×, the coalesced savings ledger); `embedding_model` 33,514,412 on both arms
(identical, cancels); `query_log` 0 → **126,673** — see the correction in §8.6(4), this is a bucket
that did not exist when §4 was written, not new traffic.

### 8.2 What changed since the first verdict

Six items landed between the two arms. Each one's own single-phase measurement is cited; none of
those numbers is restated as if it were this arm's.

1. **The consumer-gated on-demand publication — a committed base is published only when something
   can read it.** `daemon_state.go` used to schedule an initial committed-base publication for
   **every** configured repository as soon as its generation-0 index completed, and
   `DedicatedBaseAdvanceTrigger.HeadChanged` enqueued on every HEAD move, in both cases with a
   reader set of size zero in the measured shape (`checkouts: 1, checkout_routes: 0`). A fourth skip
   — *"no dependent checkout"* — plus an on-demand schedule from the first reader's claim replaces
   it. Its own paired single-phase re-measurement of `P0` put `ri_logical_writes` at 995,418,813
   against a same-session baseline of 1,038,370,756 = **0.959×**, store 61,579,264 against
   61,603,840, `view_generations.Count` 0, node rows 14,989 (one copy). In this arm the same shape
   holds across the whole run: `P0` 1.04 × (989,877,036 against the frozen 949,066,148) instead of
   1.89 ×, and `P4`'s counters show the gate firing on the live path — 6 advances dispatched across
   three repetitions, **6 publications skipped, 0 published**.
2. **The generation bulk window, and a deterministic cold-load WAL drain.** A generation payload no
   longer pays per-row B-tree maintenance across 19 secondary indexes at the pooled 32 MiB
   `cache_size`; the replay that motivated it measured the identical payload at 841,300,936 bytes at
   store defaults against 258,812,948 with `cache_size=-262144` alone (−69 %) and 227,337,572 with
   the full fast-path shape. What this arm can say about the drain half is narrow: the candidate
   leaves `P0` with a WAL of 15,281,112 / 15,487,112 / 15,569,512 bytes (≈3,709 / 3,759 / 3,779
   frames, a band of ±35 frames) where the diagnosis measured 3,636 against 15,573 frames on the
   same workload — **but the frozen baseline's three runs are equally tight** (15,042,152 /
   14,980,352 / 15,128,672), so these three repetitions do not exhibit the lottery and cannot be
   used to say the bulk window and drain removed it. The difference that *is* paired is checkpoint
   frequency: `wal_resets` **31 / 33 / 29** per candidate run against the baseline's 73 / 68 / 73,
   i.e. the candidate restarts the log about half as often (§7.3 recorded 38/35/33 for the first
   verdict's candidate, so this is a continuation, not a new effect). `wal_resets` is a lower bound
   on checkpoints (§9(4)) and a count, not a byte figure.
3. **The change-bounded dedicated delta with the operator bound — a committed-base delta's write set
   is bounded by the change, and the operator knob is honoured.** Its single-phase re-measurement
   through the `diag-P4` S2 driver is the one item in the round of fixes that **missed its targets
   and says so**: store delta across the commit window +7,905,280 B against a ≤1 MB target (the
   frozen S2 baseline was +8,224,768 B, so 0.96 ×), and a one-sample burst of 82,983,960 B against a
   ≤20 MB target (frozen S2: 104,400,000 B, 0.79 ×). The mechanism it names is the delta's own
   withholding comparison, not the cap: the delta still writes 193 unchanged closure files for a
   ten-file commit (203 payload paths / 2,039 nodes). What the knob fix buys is that
   `index.affected_by_reresolve_max` now reaches the committed-base arm at all, and the closure cap
   is `change_sized` rather than a raised built-in. That item's report
   (`scratchpad/reports/W8g-F3b.md` §5–§6) is the record; this arm does not re-measure it.
4. **The coalesced savings ledger.** A read-only tool call used to write a durable ~37 KB sidecar
   transaction of its own; the ledger now buffers and commits once per window, with the one-shot
   flush wired from `runMCP` (N read-only calls → 0 transactions in the window, exactly 1 on flush).
   In this arm the retained `sidecar_db` bucket is **2,516,768 B against the baseline's 4,695,192
   B**, 0.54 ×.
5. **The store-side payload copy route, and the bulk bracket that closes on every exit.** When the
   claimed base's tree matches generation zero's, the payload is now **copied** (`INSERT … SELECT`
   inside the generation bulk window) instead of re-parsed, and the bracket closes through a
   deferred drain on every exit path including `runtime.Goexit`. The copy route's write-shape
   measurement on a 1,500-file-sized payload (6,000 nodes / 12,000 edges): copy 12,607,232 WAL bytes
   against re-parse 36,309,592 = **0.35 ×** of the log, 23.7 MB less, before counting the parse the
   copy does not do. In this arm `P5`'s counters show what route the 20 commits took: 60 advances
   dispatched, 63 claims built, **60 `publish{shape=delta}` and 3 `publish{shape=root}`** — one root
   per run, which is the first dependent's on-demand base that the consumer gate defers rather than
   drops. **And each of those three roots took the copy route in production**, not the re-parse one:
   each run's daemon log carries exactly one `claimed dedicated base source plan …
   "route":"copy_generation_zero","reason":"the working tree is clean at the reserved tree"`
   (`internal/indexer/builder_dedicated_claimed.go:220`) and no other route value, in all three
   repetitions.
6. **The harness corrections — the accounting the other five are read through.** The
   checkpoint-excluded series, the per-window envelope, the per-arm idle budget, the bounded
   isolation probe. These add no daemon work; what they add is the ability to say which bytes are a
   deferred drain of already-committed work and which are the phase's own. `P3` in this arm is the
   clearest case: the total series reads 89.09 × the baseline and the judged series reads **0.92
   ×**, because 32,178,256 of the candidate's 32,477,360 bytes are a WAL checkpoint landing inside a
   phase that touches a file with identical bytes, stages it and resets it. Both numbers are in the
   table; neither is presented alone.

### 8.3 The one regression, and the one phase whose total series moved

**`P4_amend_same_tree`, 1.28 × on the judged series** (401,408 → 513,840). This is the last survivor
of the 270 × row in §7, and it is 0.0047 × of that row's 108,454,232 bytes. The split says where the
remaining bytes are: the tree-changing commit costs 265,128 (median of three) and the amend itself
costs 248,712, so neither sub-window is the "cleanest possible no-op paying 103 MiB" of the first
verdict. The generation sequence does not move across `P4` in any repetition (0 → 0 on both arms,
against +1 for the first verdict's candidate) and the phase publishes nothing: 6 advances dispatched,
6 publications **skipped**. What is left is bookkeeping above the baseline's, preserved here as a
regression because it is above 1.10 × and because a 28 % gap on a no-op is worth an explanation the
counters do not yet give. It is the smallest absolute regression in this document — 112,432 bytes of
median difference.

**`P3_touch_stage_unstage`, 89.09 × on the total series and 0.92 × on the judged one.** The candidate
books 32,178,256 checkpoint bytes inside `P3` where the baseline books 0. This is the deferred drain
of the WAL that `P2`'s ten edits filled (both arms leave `P2` with a ~30 MB log), moving at a
different moment on the two arms because their janitors and auto-checkpoint crossings differ. It
creates no new logical content, and the `ri_diskio_byteswritten` row (0 against 36,876,288) says the
same thing in the second series. The phase's own work is 335,792 bytes against the baseline's
364,544. **Process writes are not NAND writes, and a deferred drain of already-committed pages is not
new content** — but it is also not free, and it is recorded rather than netted out.

**One outlier is preserved, not trimmed.** `P8a_idle_warm_polling` in one repetition read
113,068,920 total bytes against 2,147,408 and 4,228,088 in the other two; 108,840,832 of it is a
checkpoint, and the judged (excluded) reading for that repetition is 4,228,088. That repetition's
idle window also published two generations (`Sequence` 48 → 50, one `built_commit` and one
`built_dirty` coordinator cycle) — the same mechanism §7.2(4) recorded, and the same reason: with
`GORTEX_RECONCILE_INTERVAL=5s` a coordinator cycle is far more likely to land inside a 60 s window
than at the 1 h product default.

### 8.4 The confirmatory default-interval arm

§9(1) records that the confirmatory arm at the product's 1 h janitor default had
never been run. The harness corrections made the interval a harness knob
(`GX_SUSTAINED_IO_RECONCILE_INTERVAL=product`, which leaves `GORTEX_RECONCILE_INTERVAL` unset so the
daemon takes its own default), and it has now been run **once**, same binary, same fixture digest,
`GX_SUSTAINED_IO_REPS=1`: `artifacts/W8i-F6/confirm-default-interval/candidate_rep1`, log
`logs/W8i-F6/confirm-arm.log`.

This is **one reading, under a different configuration, against no ceiling** — the frozen budgets were
measured at 5 s and none of them applies here. It is reported to answer one question: how much of the
idle floor is the accelerated janitor.

| window | wall | `ri_logical_writes` | excl. checkpoint | `wal_checkpoint_bytes` | client calls |
|---|---|---|---|---|---|
| `P1a_idle_cold_polling` | 60.0 s | **360,448** | 360,448 | 0 | 12 |
| `P1b_idle_cold_quiet` | 60.0 s | **53,248** | 53,248 | 0 | 0 |
| `P8a_idle_warm_polling` | 60.0 s | 112,455,072 | **18,812,680** | 93,642,392 | 12 |
| `P8b_idle_warm_quiet` | 60.0 s | **335,872** | 335,872 | 0 | 0 |

The clean comparison is against §8's own candidate arm, because it is the **same binary** and the
only thing that differs is the janitor interval: `P1a_idle_cold_polling` reads 1,040,384 bytes at
`GORTEX_RECONCILE_INTERVAL=5s` and **360,448** at the product default, **0.35 ×**; the quiet arm
reads 462,848 against **53,248**, **0.12 ×**. (Against the frozen baseline's `P1` phase reading of
1,490,944 bytes over 64 s it is 0.24 ×, but that pair differs in binary as well as interval.) So on a
freshly indexed store at the shipped configuration, this daemon writes **53,248 bytes in 60 s when
nobody asks it anything**, and 360,448 when a reader polls a search every 5 s — and roughly two
thirds of the 5 s arm's cold idle floor is the acceleration §4 applies, not the product.

The warm-idle row goes the other way and is the more interesting one. At the 1 h default nothing
drained earlier in the run, so the store's own 5-minute periodic PASSIVE checkpoint had the whole
workload's log to move and it landed inside the warm-idle window: 93,642,392 of the window's
112,455,072 bytes are attributed to that checkpoint, and 18,812,680 are not. The quiet arm in the
**same run**, 60 s later, wrote 335,872. So at the product default the warm idle floor is not flat —
it is quiet with an occasional large deferred drain, where the 5 s janitor arm is continuously busy
with small ones (`wal_resets` 29–33 per run against the baseline's 68–73). Both shapes move
already-committed pages; neither is new logical content. What the 18,812,680 unattributed bytes are
**cannot be resolved from this artifact**: the attribution books a whole 1 Hz sample interval to a
checkpoint only when that interval's WAL header showed a reset, so a drain spanning several samples
leaves its neighbours' bytes outside the attribution — and genuine non-checkpoint work in the same
window is indistinguishable from that tail (§9(4), §9(15)). n = 1; no ceiling was applied to it and
none should be inferred from it.

### 8.5 Design costs, re-measured

The six design costs the round of fixes declared rather than fixed
(`scratchpad/reports/io-fix-plan.md` §2), each re-read against this arm.

1. **A committed base carries its own payload** — still true of the model, but it is no longer paid
   speculatively and it is no longer paid by re-parsing. The consumer gate makes the base **built on
   demand**: in `P0`–`P4` this arm publishes none at all (`view_generations` 0, store 61.6 MB
   against the baseline's 61.3 MB, one copy of the corpus at 14,989 node rows), and `P5` publishes 3
   roots across 3 runs — one per family, at the first dependent's claim — against 60 deltas. The
   copy route makes the root **bulk-copied when the tree matches generation zero's** rather than
   re-parsed: 12,607,232 WAL bytes against 36,309,592, 0.35 ×, on a payload the size of this
   fixture's — and §8.2(5) shows the route actually being taken here, once per run, from the
   daemon's own log. That 0.35 × is the copy route's isolated measurement of the two write routes,
   not a figure this arm re-derives: `P5` carries one copied root and sixty deltas, and nothing in
   the phase separates their bytes. The first verdict's "+62,402,560 B of store per full base, at
   index time, for a reader set of size zero" is gone from the measured workload; the cost that
   remains is one base per family when a dependent asks for one.
2. **The doubled store re-paid at every future checkpoint** — **the premise no longer holds and the
   number is withdrawn.** It was measured as `ri_diskio_byteswritten` 86,151,168 (baseline) against
   119,119,872 (candidate) = 1.38 × over the same two checkpoints, *because the flush scales with
   the store/mmap view* and the candidate's store was 123.0 MB against 61.7 MB. This arm's store is
   **61,648,896 bytes at the end of `P0` against the baseline's 61,267,968 (1.006 ×)** and
   **270,340,096 at the end of the run against 324,927,488 (0.83 ×)**; retained bytes after teardown
   are 375,751,391 against 434,887,204 (0.86 ×). The store does **not** double any more, at any
   boundary in this workload. `P0`'s disk counter is correspondingly 1,055,571,968 against
   1,001,312,256 — **1.05 ×**, not 1.38 ×. This cost should be re-derived from scratch if a future
   change reintroduces a speculative base; it is not a standing cost of the branch.
3. **The 5-minute periodic PASSIVE WAL checkpoint** (`internal/graph/store_sqlite/store.go`,
   identical on `main`) — **unchanged, and now visible as its own column rather than as an
   unexplained idle regression.** In the first verdict it was 11,481,168 of the 15,181,176-byte `P8`
   median (75.6 %). Here `P8`'s judged (excluded) median is 2,799,664 with a checkpoint median of 0
   and a max of 108,840,832; the confirmatory arm at the product janitor interval concentrates it
   —93,642,392 checkpoint bytes in one 60 s window (§8.4). The gate-2 wording ("idle must
   write nothing beyond bounded bookkeeping") is still unsatisfiable for any WAL store and still has
   to admit the deferred drain of already-committed work; what the checkpoint-excluded series adds
   is that the admission is now a number, not a concession.
4. **`base_changed` on a dependent read overlapping a committed-base advance** — **not re-measured
   here.** The mechanism is arm-independent (`view_request.go`, `lease.go` unchanged), the recorded
   figures stand (3 of 14 single shots at 6,000 files, self-healing in 0.45–1.31 s, 0 leaks in 159
   parsed samples), and the harness now retries a bounded number of times instead of failing a phase
   on one inexact answer. **The retry was not exercised in this arm**: all twelve isolation probes
   (four per run) returned a judgeable answer on their first poll, `leaked: false`, `judged: true`.
   That is a 1,500-file result and says nothing about 6,000, where the single-shot probe lost the
   race (§7.5). Whether the candidate *widens* that window relative to `main` remains open
   (`io-fix-plan` §3).
5. **Amplification per publish** — `P5_main_advance` is 1,447,530,384 against the baseline's
   5,147,295,504, **0.28 ×**, with 674,583,712 of the candidate's bytes attributed to checkpoints
   against 4,420,177,368 of the baseline's. The disk counter agrees in direction and magnitude
   (1,714,257,920 against 7,395,172,352). Whether a generation layer needs all eight `edges_by_*`
   indexes populated at publish time is still the open question.
6. **`view_generations.storage_bytes` records source bytes, not stored bytes** — unchanged and still
   deferred; harmless until a retirement or eviction policy keys on it.

### 8.6 Corrections to §7

§7 is left in place as the historical record, including the numbers that were misread. Each
correction below is also marked at the point it applies, so a reader arriving at §7 first is not
misled. **The original numbers are kept; what changes is what they are attributed to.**

1. **§7.2(2) — "these writes are the primary's own re-index path" is wrong.** The `P2_small_edits`
   3.44 × (16,879,664 → 58,040,984) was not the edit path. 103 % of the delta is one SQLite
   `wal_autocheckpoint(8000)` drain landing inside the window because the candidate entered `P2`
   about 1,120 WAL frames deeper. Re-reducing the **same frozen artifacts** on the
   checkpoint-excluded series moves the row to 15,433,968 (0.91 ×) with 42,607,016 booked as
   checkpoint bytes, and a controlled 40-edit re-run collapsed the ratio to 1.042 × with the
   candidate's edit path at 0.91 × of the baseline's. The phase's own claim in §7.2(2) — "the
   counters show no coordinator build at all" — was true and was the clue; the conclusion drawn from
   it was not. This arm reads 0.96 × with **0** checkpoint bytes on either side.
2. **§7.2(1), §7.3, §7.4 and §7.5 — the `P4` bytes are a ten-file commit, not the amend.** The
   diagnosis found that every byte of the 108,454,232 lands 1–4 s after the `git add -A && git
   commit` the phase itself performs inside its own window, and that the amend proper is below the
   idle floor: zero DML in all 62 tables across six repetitions. The commit's real cost was a
   209-file closure payload for a 10-file change (~20 × multiplier) plus ~12.7 × SQLite
   amplification. So "a 270 × regression on a same-tree amend that `main` handles for nothing"
   (§7.4) names the wrong stimulus: the amend met the no-op contract; the *commit inside the window*
   did not. The phase is now split (`P4a_commit_tree_change` / `P4b_amend_same_tree`) precisely so
   this is not re-derivable only from a diagnosis, and this arm reads 265,128 and 248,712
   respectively.
3. **§7.5 — "We cannot say from this artifact whether the dependent's view leaked" is superseded.**
   It did not leak. The isolation probe returned a truthful, self-healing `fallback_reason:
   "base_changed"` label — reproduced 3 of 14 single shots at 6,000 files, healing in 0.45 / 1.31 /
   1.20 s — and across 159 parsed samples there were **0 named result rows**, on both arms. What
   failed was a single-shot probe racing a coordinator that was still republishing, which is now a
   bounded retry.
4. **§4 — "no query-log bytes appear in either arm's destination census" is wrong.** They do. The
   query log is written under the cache directory, and the census folded it into the `cache` bucket,
   which is why it was invisible. It now has a bucket of its own
   (`cmd/gortex/sustained_io_sampler_test.go`, its own census bucket), and the two arms show the
   correction directly: the baseline runs — measured before the bucket existed — carry 127,455 /
   128,165 / 128,508 bytes in `cache` and 0 in `query_log`, while this arm's candidate runs carry 84
   / 84 / 88 in `cache` and **136,818 / 120,862 / 126,673 in `query_log`**. Same bytes, previously
   mislabelled. The rest of §4's paragraph stands: the query log belongs to the MCP surface, the
   harness drives the CLI, and `GORTEX_QUERY_LOG_DISABLE` remains unreachable through this fixture.
5. **§7's `P8_idle_warm` "OVER BUDGET" verdict was read off the wrong series.** The row failed the
   absolute idle ceiling on `ri_logical_writes` (15,181,176 against 8,639,500) at a moment when
   11,481,168 of the baseline's own comparable figure was a periodic checkpoint. On the
   checkpoint-excluded series the frozen baseline's `P8` median is 3,299,312, which is what the
   augmented ceiling is now derived from, and this arm reads 2,799,664 against it. The original
   verdict is not deleted: it was correct about the series it was computed on, and wrong about what
   that series meant.

### 8.7 What the second verdict does and does not say

It says: on this fixture, at this scale, under §4's accelerations, the branch at `271a9e9f` is
**within every frozen ceiling**, keeps the headline `P5_main_advance` result (0.28 × against a 0.50 ×
ceiling), and has closed the three regressions the first verdict preserved — `P0` 1.89 × → 1.04 ×,
`P2` 3.44 × → 0.96 ×, `P4` 270 × → 1.28 × — while leaving a **smaller** store at every boundary and
0.86 × the retained bytes. One regression survives (`P4`, 1.28 ×, 112,432 bytes of median difference)
and is preserved rather than rounded away.

It does not say the write-amplification problem is solved. It is one candidate arm against a
**re-used** baseline on **one** 1,500-file fixture at n = 3, with an accelerated janitor, on Darwin,
with the host's load uncontrolled. The 6,000-file scale axis has **not** been re-run post-fix, so
§7.5's two worst numbers — `P1@6000` 7.44 × and `P4@6000` 674 × — have no post-fix counterpart in
this document, even though both of their mechanisms (a speculative base publish, and a commit
misattributed to an amend) are the ones the consumer gate and the `P4` split address. The
change-bounded delta's own targets were missed and stay missed. And every caveat of §1 applies to §8
exactly as it applies to §7: process-accounted writes are not NAND writes, a WAL size is not
cumulative writes, and a small-fixture replay is not sustained daemon behaviour.

## 9. Limitations and deviations

Each of these narrows what the numbers above may be used for. None of them is a reason to discard
the comparison; all of them are reasons not to extend it.

1. **Accelerated janitor.** Every measured daemon in §6-§7 runs with `GORTEX_RECONCILE_INTERVAL=5s`,
   720x the product's own 1 h default, and so does §8's candidate arm — that is what keeps it
   comparable with the frozen baseline. When §7 was written the confirmatory arm at the
   product default had **not** been run and could not be: the fixture set the interval
   unconditionally and dropped every inherited `GORTEX_*` variable. The interval is now a harness
   knob (`GX_SUSTAINED_IO_RECONCILE_INTERVAL=product` leaves the variable unset so the daemon takes
   its own default) and **one confirmatory arm has been run — §8.4**. It remains a single reading
   under a different configuration against no ceiling, and it is not a second candidate arm: at the
   1 h default the janitor does not fire inside an ~11-minute phase set at all, so the two
   configurations answer different questions. What §8.4 establishes is the size of the
   acceleration's contribution to the idle floor (`P1` polling 360,448 B/60 s at the default against
   the frozen baseline's 1,490,944 B/64 s; quiet 53,248 B/60 s) and that the deferred checkpoint
   drain concentrates instead of spreading.
2. **Cold/warm is within a run, not across arms.** Every arm-repetition builds a new fixture, store
   and daemon, so all six runs are cold. "Warm" is `P8_idle_warm` against `P1_idle_cold` inside one
   run. The protocol's `{cold, warm}` axis is therefore half-covered.
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
9. **Two knobs the hazard list asks for are not applied** (§4): `GORTEX_SKIP_STORE_COMPACT`
   and `GORTEX_QUERY_LOG_DISABLE`. Both are unreachable through this fixture, and both were shown by
   source to be inert at this store size and through this surface. No boot VACUUM ran in either arm.
10. **The reduction step reports; it does not gate.** `TestSustainedIOPairedArmsVerdict` writes
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

The five below narrow **§8** specifically.

14. **§8 re-uses the frozen baseline; it does not re-run it.** The three `baseline_rep*` directories
    judged in §8 are byte-for-byte the ones frozen on 2026-09-13, copied read-only into the new
    artifact directory, and `budgets.json` is the same file (md5 `6caf0268…` before and after the
    reduction). That is deliberate — re-freezing against a fresh baseline would make the two
    verdicts incomparable — but it means §8's ratios carry **fourteen days of host drift** that the
    original `A/B/A/B/A/B` interleave was designed to share between the arms. Differences of a few
    per cent in §8 are worth less than the same difference in §7. The order-of-magnitude movements
    (`P0` 1.89x → 1.04x, `P2` 3.44x → 0.96x, `P4` 270x → 1.28x) are far outside that drift; `P6`'s
    1.05x → 0.76x and `P3`'s 0.89x → 0.92x are not.
15. **The checkpoint attribution is per sample interval, so the excluded series is a bound, not a
    partition.** `wal_checkpoint_bytes` books the whole `ri_logical_writes` delta of a 1 Hz sample
    interval in which the WAL header showed a reset. A drain spanning several samples leaves the
    neighbouring samples' bytes unattributed (the confirmatory arm's 18,812,680 unattributed bytes
    in §8.4 are the clearest instance), and a bracket shorter than one sample can be handed an
    interval carrying more bytes than the bracket's own delta — which is why the
    checkpoint-exclusion reducer clamps at zero rather than going negative. The excluded series is
    therefore an **upper** bound on non-checkpoint work, and both series are always printed
    together.
16. **§8's idle rows are judged on a sub-window, the baseline's on its phase.** The candidate's
    `P1`/`P8` ceilings are applied to a 60.0 s polling window; the frozen baseline's are its 64 s
    and 62 s phase rows, which also contain the phase's own bracket overhead. The polling window now
    runs **first** in the phase, so it opens where the frozen window opened and inherits the
    preceding phase's decaying tail the way the frozen one did — but the walls are still unequal,
    and the ratio is not wall-normalised. Per second: `P1` 23,296 B/s baseline against 17,340 B/s
    candidate (0.74x, where the raw ratio reads 0.70x); `P8` 53,215 B/s against 46,661 B/s (0.88x,
    raw 0.85x). Neither correction changes a verdict.
17. **The 6,000-file axis was not re-run after the round of fixes.** §7.5's two worst candidate
    numbers — `P1@6000` 7.44x and `P4@6000` 674x — have **no** post-fix counterpart anywhere in this
    document, and the consumer gate and the `P4` split address exactly the mechanisms behind both.
    Whether they are closed at 4x the corpus is **unmeasured**, not answered. §7.5 stands as the
    only 6,000-file evidence and it is n = 1 with an unfinished candidate arm.
18. **`P4_amend_same_tree`'s 1.28x is preserved without a mechanism.** §8 can say what it is not —
    no publication (6 dispatched, 6 skipped), no generation movement, not the amend alone (the two
    sub-windows are 265,128 and 248,712) — but the counters do not name what the extra 112,432
    median bytes above the baseline are. It is recorded as a regression rather than explained.

## 10. Reproduction

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
go test -c -o "$R/sustained_io.test" ./cmd/gortex/   # or: harness/validate.sh compile cmd normal
shasum -a 256 "$R/sustained_io.test"
```

**3. Run the paired workload.** `env -i` plus an explicit allowlist; nothing is inherited.

```bash
mkdir -p "$R/paired1500"/{home,tmp,fixtures} "$SP/artifacts/W8m-W8.4/paired1500"
env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin \
  HOME="$R/paired1500/home" TMPDIR="$R/paired1500/tmp" \
  GORTEX_ISSUE767_ARTIFACT_DIR="$R/paired1500/fixtures" \
  GX_SUSTAINED_IO_BASELINE_BINARY="$SP/harness/bin/gortex-baseline-56a1c29d" \
  GX_SUSTAINED_IO_TEST_BINARY="$SP/harness/bin/gortex-2fd5db82" \
  GX_SUSTAINED_IO_ARTIFACT_DIR="$SP/artifacts/W8m-W8.4/paired1500" \
  GX_SUSTAINED_IO_REPS=3 GX_SUSTAINED_IO_EDIT_INTERVAL=10s \
  "$R/sustained_io.test" -test.run '^TestSustainedIOSustainedWriteAmplification$' -test.v -test.timeout 8h
```

Without `GX_SUSTAINED_IO_TEST_BINARY` the test skips with a named reason, so it never runs in a
default `go test ./...`. `GX_SUSTAINED_IO_BASELINE_BINARY` is what makes the run paired; without it
only the candidate arm runs. The defaults the run used (1,500 files / 60 packages / seed 767 / 10
worktrees / 20 commits / 10 edits / 60 s idles / 1 s sampling) come from the harness's
environment-driven config and every one of them has a `GX_SUSTAINED_IO_*` override with bounds that
are refused by name.

**4. Freeze the budgets, then judge.** Two invocations, in this order — the first one is the freeze
and it never reads the candidate:

```bash
env ... GX_SUSTAINED_IO_PAIRED_ARTIFACT_DIR="$SP/artifacts/W8m-W8.4/paired1500" \
  GX_SUSTAINED_IO_BUDGETS_ONLY=1 \
  "$R/sustained_io.test" -test.run '^TestSustainedIOPairedArmsVerdict$' -test.v   # writes budgets.json
env ... GX_SUSTAINED_IO_PAIRED_ARTIFACT_DIR="$SP/artifacts/W8m-W8.4/paired1500" \
  "$R/sustained_io.test" -test.run '^TestSustainedIOPairedArmsVerdict$' -test.v   # writes verdict.json + verdict.md
```

`budgets.json` refuses to be rewritten by the second pass, and the verdict refuses to judge against
a budget set whose bytes no longer match the digest recorded inside it.

**Artifacts.** `$SP/artifacts/W8m-W8.4/paired1500/{baseline,candidate}_rep{1,2,3}/` each hold
`manifest.json`, `report.json`, one `phase_<name>.json` per phase and the raw `samples.ndjson`;
the run directory holds `budgets.json`, `verdict.json` and `verdict.md`. The console log is
`$SP/logs/w84-paired1500.log` and the host state is `$SP/logs/w84-paired1500-host.log`.

### 10.1 Reproducing the second verdict (§8)

Three steps, on top of step 1's baseline binary and its frozen artifacts, which are **inputs** here
and are never rewritten.

**1. Build the post-fix candidate.** `$W` is the implementation worktree at `271a9e9f`.

```bash
export GOWORK=off GOTOOLCHAIN=local GOFLAGS="-mod=mod -buildvcs=false" GOPROXY=off
export GOCACHE="$HOME/Library/Caches/go-build" GOMODCACHE="$HOME/go/pkg/mod"
(cd "$W" && go build -o "$SP/harness/bin/gortex-271a9e9f" ./cmd/gortex/)
shasum -a 256 "$SP/harness/bin/gortex-271a9e9f"   # 6378b769…
# source identity, independent of the build recipe:
mkdir -p "$R/src" && (cd "$W" && git archive 271a9e9f) | tar -x -C "$R/src"
diff -r -q -x '.git' -x '*_test.go' -x docs "$R/src" "$W" | grep '^Files .* differ' | wc -l   # 0
```

**2. Stage the frozen baseline into a NEW artifact directory, then run the candidate arm alone.**
`GX_SUSTAINED_IO_BASELINE_BINARY` is deliberately unset: without it only the candidate arm runs.

```bash
A="$SP/artifacts/W8i-F6/paired1500-postfix"; mkdir -p "$A"
cp -R "$SP/artifacts/W8m-W8.4/paired1500"/baseline_rep{1,2,3} "$A/"
cp "$SP/artifacts/W8m-W8.4/paired1500/budgets.json" "$A/"
md5 "$A/budgets.json"      # 6caf02683492a65009ffe1ed95bac0b7 — and again after step 3
env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin CI=1 NO_COLOR=1 GORTEX_TELEMETRY=0 \
  HOME="$R/home" TMPDIR="$R/tmp" XDG_CACHE_HOME="$R/cache" XDG_CONFIG_HOME="$R/config" \
  XDG_DATA_HOME="$R/data" GORTEX_ISSUE767_ARTIFACT_DIR="$R/fixtures" \
  GOWORK=off GOTOOLCHAIN=local GOFLAGS="-mod=mod -buildvcs=false" GOPROXY=off \
  GOCACHE="$HOME/Library/Caches/go-build" GOMODCACHE="$HOME/go/pkg/mod" \
  GX_SUSTAINED_IO_TEST_BINARY="$SP/harness/bin/gortex-271a9e9f" GX_SUSTAINED_IO_ARTIFACT_DIR="$A" \
  GX_SUSTAINED_IO_REPS=3 GX_SUSTAINED_IO_EDIT_INTERVAL=10s \
  go test -C "$W" -count=1 -timeout 240m -v -run '^TestSustainedIOSustainedWriteAmplification$' ./cmd/gortex/
```

**3. Judge.** One invocation — the freeze pass is unnecessary because `budgets.json` is already
there, and the frozen-budget writer reads an existing file rather than overwriting it.

```bash
env ... GX_SUSTAINED_IO_PAIRED_ARTIFACT_DIR="$A" \
  go test -C "$W" -count=1 -v -run '^TestSustainedIOPairedArmsVerdict$' ./cmd/gortex/
```

The reduction logs `augmented_from=aca00104…` and `digest=cbc3794a…`, writes `verdict.json` and
`verdict.md` next to the budgets, and leaves `budgets.json` at md5 `6caf0268…`. It refuses outright
if the candidate's fixture digest differs from the frozen one, if either arm's manifest names the
wrong arm, or if the budget set's contents no longer match the digest recorded inside it.

**The confirmatory default-interval arm (§8.4)** is the same step 2 with
`GX_SUSTAINED_IO_RECONCILE_INTERVAL=product GX_SUSTAINED_IO_REPS=1` and its own artifact directory
(`artifacts/W8i-F6/confirm-default-interval`); it is **not** reduced against the frozen budgets,
because those were frozen at a 5 s janitor and no ceiling in them applies to it.

**Artifacts and logs for §8.** `artifacts/W8i-F6/paired1500-postfix/` (three `baseline_rep*` copied
in, three `candidate_rep*` produced, `budgets.json`, `verdict.json`, `verdict.md`),
`artifacts/W8i-F6/confirm-default-interval/candidate_rep1/`, the console logs
`logs/W8i-F6/{candidate-arm.log,confirm-arm.log,verdict.log}`, and
`artifacts/W8i-F6/copy-route-evidence.log` — every `claimed dedicated base source plan` line the four
runs' daemons emitted, which is the evidence for §8.2(5) and §8.5(1). The private fixture roots
(`/private/tmp/gxh-w8if6*`) were removed after the run; everything cited here is in the artifact
directories.
