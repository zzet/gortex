# Opt-in PR read offering and drain

`--duration 20s --pr-smoke-drain 2m --scenario read --agents 16 --edit-agents 0 --worktrees 0`
selects the experimental standalone native agent read window. The 20/30-second
window controls new offers; already admitted reads retain their original product
deadlines under the separate bounded drain context. Duration without the new
flag preserves the frozen behavior. This is a harness mode, not a measured
15-minute lane or an accepted sample-count profile.

Default/native baseline, nightly, release and coupled paired semantics remain
unchanged. The flag refuses reference/floor, paired, saturation, edits,
edit-and-probe, all and other scenarios. It also refuses CPU profiling and
retained state. Unsupported edit/probe work is not silently omitted.

Workload identity contains one optional `pr_smoke_drain` config only when enabled.
It binds the offered duration and maximum drain. Disabled mode omits the pointer,
preserving legacy JSON/request hashes. Only that new pointer is optional in the
schema; existing required lists and historical field behavior are unchanged.
Schema goldens must be generated from the integrated source rather than merged
manually across concurrent harness work.

The phase gate retains effective offer/drain deadlines, actual completion/wall
elapsed, time beyond the offer deadline, and admitted/not-offered call counts.
Every admitted attempt, busy retry, product expiry and parent/overdrain censor
remains in the normal census. Phase elapsed time includes drain; successful late
reads cannot disappear from its denominator. Complete reports require matching
observed timing/admission receipts. Stop/owned-daemon cleanup remains the existing
phase lifecycle, and forced stop is still a failure.

The initial implementation supports reads only. Exact edit/readback/publication
probes and a distinct idle-stop fixture remain separate required work. Actual
per-tool sample counts, p99 interpretation, 20 unchanged/seeded runs and the
full job's 15-minute wall-time acceptance remain unproven.
