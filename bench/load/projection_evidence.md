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
