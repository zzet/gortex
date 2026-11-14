# Projection runtime evidence

This is an opt-in extension of the existing reference readback client. The
default is false. The existing client, catalog, default reports, and unrelated
cells keep their current meaning. Enabling
`-readback-reference-projection-evidence` requires a newly qualified client and
new bindings for the affected 32-agent or nightly cells; an older client report
without the bookends is unavailable evidence.

The only common hooks are before the offered-phase origin is recorded, and
after the offer runner has joined every edit/readback callback, before stop.
Each makes one existing `resources/read` request for `gortex://index-health`
through the first existing producer connection, with a bounded five-second
context and no retry. Neither hook runs between an ACK and its immediate reads.
The product deadline, arrival schedule, phase origin rule, immediate quartet,
stop policy, and resource API are unchanged. The additional bookend overhead
must fit the existing outer bound; it does not grant extra runtime.

`readiness.projection_evidence.before` and `.after` retain the raw request,
response, complete wire identity, and a `Metric` for the existing live
`pending_projection.projection_divergence` counter. The resource already adds
this live registry section after its cached base health payload. No product
resource or counter implementation is changed here. A missing or nil section,
missing count, unsupported resource, unavailable PID epoch, or failed call
stays structured unavailable; it never becomes zero. In particular, a freshly
started daemon may have no pending section before its first edit. This remains
unbound rather than adding a warm-up edit or inferring a measured zero.

The memory observation reads Linux `/proc/<owned-daemon-pid>/stat`, `status`,
and the boot ID. It verifies PID and start ticks around the read and again
after the resource request. Raw evidence is retained. `VmHWM` is RSS since
process start, including startup and warmup. Reading it does not reset it.
The postprocessor compares the candidate's **final lifetime peak** to the
matched v0.64.10 daemon's final lifetime peak plus 64 MiB. It never subtracts
before/after high-water marks to invent a workload-window peak. A difference
dominated by startup is not attributed to the edit workload.

This metric covers the owned daemon PID only. Children, driver/proxy processes,
sampled maxima, Go retained heap, and whole-unit cgroup charge are not
interchangeable. The pair postprocessor emits the scoped daemon lifetime bound
and leaves total owned-process-cohort footprint **UNBOUND**, with exit 2. It
cannot silently green the total-footprint criterion.

## Executable validation

`projection_evidence.py PAIR.json CATALOG.json` consumes raw report paths and
the existing C outer-result paths from the binding. It verifies report digests,
outer closure, explicit collector argv, actual 32-agent cell, the complete
100-offer edit-plus-quartet census, wire identities, host/resource/volume and
binary/plan/manifest/population/arrival bindings, epoch continuity, raw VmHWM,
and authoritative counter values. The two binary revisions differ as expected;
the baseline must have a pinned v0.64.10 provenance receipt. Reference limits
remain eight cores and 16 GiB, with no runtime tuning.

`--mode nightly` validates a complete roster of the supplied compiled catalog:
one main scheduled or manual nightly-equivalent run/attempt/revision, no missing
or duplicated cells, full actual edits/readbacks, and present zero counters
with observed publication and checked retirement in every case. This validates
only the bound projection-counter roster. It does not certify unrelated
nightly budgets, hosts, seeded RED proof, languages, or release acceptance.

Required pair bindings are `catalog_sha256`, `cell_index`, `baseline_tag`,
`baseline_tag_receipt_sha256`, and `candidate`/`tag` objects containing `plan`,
`plan_sha256`, `revision`, `binary_sha256`, `manifest_sha256`, `host`,
`host_sha256`, `report_path`, `report_sha256`, and `outer_result_path`. These
must come from actual qualified artifact/host/phase receipts, not planned
hashes. Nightly cases each carry the corresponding `bound`, `cell_index`, and
identical `nightly` run descriptor; the outer binding names the compiled
catalog. Missing runtime inputs return unavailable/failure, never numeric zero.

The small Go and Python contract tests use fake raw data only. Their passes
are source controls, not daemon, reference-host, memory, or nightly evidence.

### Opt-in whole-unit peak (separate from daemon VmHWM)

The acceptance test driver can opt into
`-readback-reference-cohort-peak-evidence=true`. This is not compiled into the
ordinary LOAD CLI. The default performs no added reads and emits no added fields.
The two `readiness.owned_cohort_peak.{before,after}` version-1 records surround
existing offered probes and callback joining. They read Linux cgroup-v2
`memory.peak` through the same held unit-directory FD, retaining exact integer
bytes, raw data, mount/device/inode identity, boot/monotonic epochs and the live
recursive unit process census. This collector does not reset the peak, create a
cgroup, alter resource limits, add a sampling loop, or issue tool RPCs.

Use `projection_evidence.py PACKET CATALOG --mode cohort-pair` only for a matched
32-agent **editing and read-back** candidate/v0.64.10 pair. This mode reuses the
strict full-report, catalog, source, arrival, correctness and normal-close checks.
It compares the final **lifetime** whole-unit peak directly:

```
candidate_final_memory_peak_bytes <= tag_final_memory_peak_bytes + 67108864
```

The initial peak is an epoch/monotonicity witness, not a value to subtract.
Startup, warmup, driver, proxies, child processes and kernel/file-cache charges
in the fresh unit remain included. This is neither daemon RSS nor an isolated
projection allocation estimate. A shared paired unit remains UNBOUND: its peak
cannot be attributed to individual builds. A raw read alone gives no acceptance.

The existing selected one-build outer must additionally retain
`owned_cohort_peak` version 1 in its **actual outer-result file**. Its `unit` and
`run_id` must agree with that outer. This record binds `group`, `boot_id`,
`invocation_id` (actual systemd InvocationID),
`exec_main_start_timestamp_monotonic` (actual systemd microseconds as a decimal
string), `main_pid`/`main_birth_ticks`, `binary_sha256` and the held directory
`cgroup` identity (`device`, `inode`, `mount_id`, `fs_type`; the held directory and peak file must themselves report the cgroup-v2 filesystem type). It records actual
`fresh_unit=true`, `single_build=true`, `inherited_unit_only=true`, `delegate=false`.
The existing before/after unit/profile captures must carry these same fields;
normal `terminal` evidence must retain `main_pid=0`, `active_state=inactive`,
`empty=true`. A still-loaded inactive unit also retains the same `invocation_id`,
`control_group` and `result=success`. The existing `systemd-run --collect` lifecycle
may instead yield `load_state=not-found`, empty `control_group` and unavailable
InvocationID after collection. That exact collected shape is accepted only with
both matching live creation/FD epochs and the actual outer's zero client exit,
closed/unforced normal cleanup; an explicit failed/unknown result is refused.
These facts must come from the existing controller's actual systemd/profile/FD
bookends and normal closure, not a report binding assembled after the run.
The reducer obtains this record from the hash-linked outer result; an embedded
replacement in the packet cannot substitute for it.

No retained old controller proved all these new unit-creation/FD facts. Until a
selected one-build outer actually records them, `cohort-pair` remains UNBOUND.
Missing or changed ownership, unit restart/reuse, unowned members, missing peak,
failed/censored offers, read-only or non-32 workloads never become zero or PASS.
A cohort PASS is scoped to this one matched numeric criterion; it gives no
nightly-roster, latency, matrix, capacity or whole-issue acceptance. Legacy `pair`
and `nightly` modes retain their original daemon-only/divergence semantics.

### Original v0.64.10 acknowledgement adapter

For the **source-selected cell 72 only** (Go, 32 agents, N100,
`add_crossfile_call`/`edit.file`, omitted/default policy), the acceptance driver
can explicitly select `-readback-reference-v06410-original-ack=true`.
The selector requires the actual published Linux ARM64 executable SHA256
`51e84c3e133d530f21661623ec86633b741a3c75507ec3304b1f14a8a6d5c6be`
and native revision `defa9f8e7a670792b5b693744d88488f741a8160`.
This member came from release asset 669760344, archive SHA256
`d36835c90ccdfcb6546757602bd82b520ce8d98593eda460faebdbe660d301a2`;
its acquisition/physical-closure authority SHA256 is
`7db2df96bf5144ed940c9797136c16003b1dd12e152e8f1899ddcf0ab969b408`.
This selector supports that deliberately chosen artifact. It does not impose
byte-identity as a new condition on every possible v0.64.10 comparison.

The original tag emits `disk_status=committed`, `status=applied`, qualified
`path`, replacement/byte counts, and **Git-blob** `new_sha`. It has no
`ack_mode` or producer `content_sha256`. The adapter verifies these original
fields against the unchanged independently generated before/after images,
including a real change, exact target and original Git-blob after-image.
Explicit failure, unknown graph state, contradictory freshness flags,
unsupported operation, wrong artifact or legacy-local selection is refused.
The old policy stays `legacy_default_unavailable`; no full/local label,
producer SHA256 or P/B accounting is invented. Current candidate decoding
keeps its existing full/local and producer-SHA256 checks.

The same original ACK and immediate five read endpoints, all physical
post-images, all 32 initialized clients, N100 census, source generator,
15-second per-editor pacing, parent deadlines and catalog-derived bounds are
unchanged. No retry, additional RPC, wait, projection disable or operation
substitution is added. Optional `readiness.legacy_tag_ack_contract` explicitly
records the version/artifact/cell/policy; cohort-mode reduction validates it
and the actual opt-in argv. Existing raw reports are never rewritten.
Historical catalog bytes with digest `e5d07705...` were not locally held in
this source preparation. The selected helper must first emit/verify its actual
fresh cell-72 catalog and exact tail/deadlines before any product work.
No adapter or helper result alone supplies footprint, nightly or whole-issue
acceptance.
