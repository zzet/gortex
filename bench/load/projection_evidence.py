"""Fail-closed postprocessing of opt-in C readback reports; never a launcher.

VmHWM is daemon-PID RSS since process start, not a workload-window delta,
process-tree peak, PSS, Go heap, or total reference-unit charge.
"""
import argparse
import datetime as dt
import hashlib
import json
import math
from pathlib import Path
import re

CATALOG_SHA256 = "e5d0770505cfe1c82a6a93d3cbbe3292cfdb72be19f8086c10bc06e06c1bdc41"
BASELINE_TAG_REVISION = "defa9f8e7a670792b5b693744d88488f741a8160"


class Unbound(ValueError):
    pass


def need(ok, reason):
    if not ok:
        raise Unbound(reason)


def integer(value, reason):
    need(type(value) is int and value >= 0, reason)
    return value


def stamp(value):
    need(isinstance(value, str), "timestamp missing")
    try:
        result = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as error:
        raise Unbound("timestamp invalid") from error
    need(result.tzinfo is not None, "timestamp has no epoch/zone")
    return result


def metric(value, unit):
    need(isinstance(value, dict) and value.get("unit") == unit and not value.get("reason"), "metric unavailable/wrong unit")
    number = value.get("value")
    need(type(number) in (int, float) and number >= 0 and number < 2**53 and number == int(number), "metric missing/nonfinite/inexact")
    return int(number)


def rpc(row):
    need(isinstance(row, dict) and not row.get("error"), "RPC missing/refused")
    need(integer(row.get("rpc_id"), "RPC ID unavailable") > 0, "RPC ID zero")
    names = ("started", "write_started", "write_finished", "reply_delivered", "finished")
    times = [stamp(row.get(name)) for name in names]
    need(times == sorted(times), "RPC wire timestamps incomplete/reordered")
    value = row.get("latency_ms", {})
    need(value.get("unit") == "ms" and type(value.get("value")) in (int, float) and math.isfinite(value["value"]) and value["value"] >= 0 and not value.get("reason"), "RPC censored latency")
    return times[0], times[-1]


def validate_report(report, bound, selected):
    """Consume the existing report ABI, keeping every offered call in census."""
    need(report.get("version") == 1 and report.get("protocol_only") is False, "not a full reference report")
    need(report.get("role") == "under_edit" and report.get("cell") == selected["schedule"]["cell"], "different cell/role")
    need(not report.get("errors") and not report.get("arrival_identity_reason"), "failed/censored phase")
    need(report.get("plan") == bound["plan"] and report.get("plan_sha256") == bound["plan_sha256"], "plan identity mismatch")
    need(re.fullmatch("[a-f0-9]{40}", bound.get("revision", "")) and re.fullmatch("[a-f0-9]{64}", bound.get("binary_sha256", "")) and re.fullmatch("[a-f0-9]{64}", bound.get("plan_sha256", "")), "artifact/source/plan pin UNBOUND")
    need(report["plan"].get("binary_revision") == bound["revision"] and report["plan"].get("binary_sha256") == bound["binary_sha256"], "binary revision/hash mismatch")
    need(report.get("manifest_sha256") == bound["manifest_sha256"], "manifest mismatch")
    for field in ("binary_sha256", "manifest_sha256", "host_sha256", "population_sha256", "read_plan_sha256", "arrival_sha256"):
        need(re.fullmatch("[a-f0-9]{64}", report.get("comparison_identity", {}).get(field, "")), "comparison identity absent: " + field)
    identity = report["comparison_identity"]
    need(identity["binary_sha256"] == bound["binary_sha256"] and identity["manifest_sha256"] == bound["manifest_sha256"] and identity["population_sha256"] == report.get("after_image_population_sha256") and identity["read_plan_sha256"] == report.get("logical_read_plan_sha256"), "comparison identity disagrees with raw report")
    need(identity.get("agents") == report["cell"]["agents"] and identity.get("kind") == report["cell"]["kind"] and identity.get("tool_surface") == report.get("tool_surface"), "actual cell identity mismatch")
    need(report.get("host") == bound["host"] and isinstance(report.get("host_before"), dict) and report.get("host_before") == report.get("host_after"), "host/resource/volume witness changed")
    need(report["host_before"].get("host") == report["host"] and report["host_before"].get("volume_identity") and report["host_before"].get("volume_probe_path"), "host/volume provenance missing")
    need(report["host"].get("profile") == "reference" and report["host"].get("validated") is True, "not a reference host")
    need(report["host"].get("os") == "linux" and report["host"].get("arch") in ("arm64", "amd64"), "bound native Linux architecture required")
    need(metric(report["host"].get("limits", {}).get("cpu_quota"), "cores") == 8 and metric(report["host"]["limits"].get("memory_max"), "bytes") == 16 * 1024**3, "reference resource limits changed")
    need(all(report.get("runtime_environment", {}).get(k) == "" for k in ("GOMAXPROCS", "GOMEMLIMIT", "GOGC", "GODEBUG")), "runtime tuning or missing environment evidence")
    need(identity["host_sha256"] == bound["host_sha256"], "bound host identity mismatch")
    count = selected["schedule"]["cell"]["offers"]
    need(count == 100 and all(report.get(k) == count for k in ("expected_offers", "observed_offers", "acceptance_nominal_offers")), "offer census changed")
    correctness = report.get("correctness", {})
    need(correctness.get("offered") == correctness.get("correct") == count and correctness.get("missing") == correctness.get("failures") == 0 and correctness.get("pass") is True, "incorrect/missing observations")
    rows, arrival = report.get("records", []), selected["schedule"]["arrival"]
    need(len(rows) == len(arrival) == count, "records missing or pooled")
    start, finish = stamp(report.get("started")), stamp(report.get("finished"))
    need(start < finish, "phase window invalid")
    identities, latest = set(), start
    for ordinal, (row, expected) in enumerate(zip(rows, arrival)):
        need(integer(row.get("ordinal"), "ordinal missing") == ordinal and integer(row.get("editor"), "editor missing") == expected["Editor"] and integer(row.get("due_ns"), "arrival missing") == expected["DueNS"], "actual arrival/call census differs")
        need(row.get("correct") is True and row.get("physical_bytes_correct") is True and not row.get("errors"), "censored/incorrect edit/readback")
        need(row.get("requested_connection_surface") == report.get("tool_surface") == "all/defer", "MCP surface changed")
        session = row.get("connection_identity")
        need(session == "fixed-editor-" + str(row["editor"]), "connection/editor identity mismatch")
        calls = [row.get("ack")] + [row.get("probes", {}).get(k) for k in ("source", "summary", "editing_context", "file")]
        for call in calls:
            a, b = rpc(call)
            key = (session, call["rpc_id"])
            need(key not in identities and start <= a <= b <= finish, "RPC reused/outside phase")
            identities.add(key)
            latest = max(latest, b)
        need(all(row["probes"][k].get("correct") is True for k in ("source", "summary", "editing_context", "file")), "readback not correct")
    cleanup = report.get("cleanup", {})
    need(cleanup.get("forced") is False and cleanup.get("stop_error") == "" and cleanup.get("native_handle_cleared") is True and cleanup.get("native_leader_joined") is True and cleanup.get("socket_absent") is True and cleanup.get("owned_store_removed") is True, "producer censored/forced")
    return latest


def bookend(report, phase):
    value = report.get("readiness", {}).get("projection_evidence." + phase)
    need(isinstance(value, dict) and value.get("version") == 1 and value.get("phase") == phase, "opt-in producer bookend UNBOUND")
    proc = value.get("process", {})
    need(proc.get("scope") == "owned_daemon_pid_only_since_process_start", "heap/sampled/cgroup/proxy metric refused")
    pid = integer(proc.get("pid"), "PID missing")
    need(pid == report.get("daemon_pid") and pid > 0, "wrong daemon PID")
    birth, boot = proc.get("birth_ticks"), proc.get("boot_id")
    need(isinstance(birth, str) and birth.isdecimal() and int(birth) > 0 and isinstance(boot, str) and boot, "PID birth/boot epoch unavailable")
    for key in ("stat_before", "stat_after"):
        stat = proc.get(key, "")
        close = stat.rfind(")")
        fields = stat[close + 1:].split()
        need(stat.split("(", 1)[0].strip() == str(pid) and close > 0 and len(fields) > 19 and fields[19] == birth, "PID reuse/raw birth mismatch")
    lines = [line for line in proc.get("status", "").splitlines() if line.startswith("VmHWM:")]
    match = re.fullmatch(r"VmHWM:\s+(\d+)\s+kB\s*", lines[0]) if len(lines) == 1 else None
    need(match is not None, "raw kernel VmHWM unavailable")
    peak = metric(proc.get("lifetime_rss_peak_bytes"), "bytes")
    need(peak == int(match[1]) * 1024, "raw VmHWM/metric mismatch")
    rpc(value.get("index_health_rpc"))
    need(value.get("connection_identity") == "fixed-editor-0", "counter is not the bound producer connection")
    need(value["index_health_rpc"].get("request_params") == {"uri": "gortex://index-health"}, "undeclared counter query shape")
    return value, (pid, birth, boot), peak


def validate_bookends(report, latest):
    before, before_id, before_peak = bookend(report, "before")
    after, after_id, after_peak = bookend(report, "after")
    need(before_id == after_id and after_peak >= before_peak, "producer restart/reset or decreasing lifetime HWM")
    need(stamp(before["index_health_rpc"]["finished"]) <= stamp(report["started"]) and latest <= stamp(after["observed_at"]) <= stamp(report["finished"]), "bookends do not enclose offered probes")
    ids = {call["rpc_id"] for row in report["records"] if row["connection_identity"] == "fixed-editor-0" for call in [row["ack"], *row["probes"].values()]}
    need(before["index_health_rpc"]["rpc_id"] != after["index_health_rpc"]["rpc_id"] and not ids.intersection((before["index_health_rpc"]["rpc_id"], after["index_health_rpc"]["rpc_id"])), "counter call identity reused from offered burst")
    return before, after, after_peak


def counter_value(book):
    observed = metric(book.get("projection_divergence"), "count")
    raw = book["index_health_rpc"].get("raw", {})
    try:
        contents = raw["contents"]
        need(len(contents) == 1 and contents[0].get("uri") == "gortex://index-health", "counter resource identity mismatch")
        section = json.loads(contents[0]["text"])["pending_projection"]
        need(section.get("enabled") is True, "projection publishing disabled or unknown")
        actual = integer(section["projection_divergence"], "counter absent/null/noninteger")
    except (KeyError, TypeError, ValueError) as error:
        raise Unbound("authoritative pending_projection counter unavailable") from error
    need(actual == observed, "counter raw/metric mismatch")
    return actual, section


def validate_divergence(before, after):
    first, a = counter_value(before)
    last, b = counter_value(after)
    need(last >= first, "counter reset/censored epoch")
    need(first == last == 0, "nonzero projection_divergence")
    need(integer(b.get("published"), "publication census missing") > integer(a.get("published"), "publication census missing"), "no observed projection publication")
    need(integer(b.get("retired"), "retirement census missing") > integer(a.get("retired"), "retirement census missing"), "no observed checked retirement")


def validate_pair(packet, catalog, *, cohort=False):
    binding = packet["binding"]
    need(binding.get("baseline_tag") == "v0.64.10", "wrong baseline tag")
    need(binding.get("tag", {}).get("revision") == BASELINE_TAG_REVISION, "baseline revision is not the pinned v0.64.10 tag")
    need(re.fullmatch("[a-f0-9]{64}", binding.get("baseline_tag_receipt_sha256", "")), "baseline tag provenance UNBOUND")
    selected = next((c for c in catalog["cells"] if c["index"] == binding["cell_index"]), None)
    need(selected is not None and selected["schedule"]["cell"]["agents"] == 32, "exact 32-agent catalog cell required")
    reports = [packet["candidate"], packet["tag"]]
    for key in ("cell", "host", "after_image_population_sha256", "logical_read_plan_sha256", "manifest_sha256", "tool_surface"):
        need(reports[0].get(key) == reports[1].get(key) and reports[0].get(key) is not None, "paired workload/host/population mismatch: " + key)
    common_plan = ("version", "seed", "edit_mean_ns", "product_deadline_seconds", "warm_seconds", "stop_seconds", "tool_surface", "language")
    need(all(reports[0]["plan"].get(k) == reports[1]["plan"].get(k) for k in common_plan), "paired resource/workload policy changed")
    need(all(reports[0]["comparison_identity"].get(k) == reports[1]["comparison_identity"].get(k) for k in ("host_sha256", "arrival_sha256", "agents", "kind", "tool_surface")), "paired actual host/arrival census mismatch")
    peaks, candidate_books = [], None
    for role, report in zip(("candidate", "tag"), reports):
        validate_catalog_policy(report, catalog)
        latest = validate_report(report, binding[role], selected)
        if cohort:
            before, after, peak = validate_cohort_bookends(report, binding[role], latest)
        else:
            before, after, peak = validate_bookends(report, latest)
        peaks.append(peak)
        if role == "candidate":
            candidate_books = before, after
    memory = "PASS" if peaks[0] <= peaks[1] + 64 * 1024**2 else "FAIL"
    if cohort:
        owners = [binding[role]["owned_cohort_outer"]["owned_cohort_peak"] for role in ("candidate", "tag")]
        same_boot = owners[0]["boot_id"] == owners[1]["boot_id"]
        need(not same_boot or owners[0]["cgroup"] != owners[1]["cgroup"], "candidate/tag reused the same unit cgroup")
        need(owners[0]["invocation_id"] != owners[1]["invocation_id"], "candidate/tag unit invocation reused")
        return {"version": 1, "metric_scope": "owned_cohort_peak_v1",
                "total_owned_process_cohort_footprint": memory,
                "candidate_cohort_peak_bytes": peaks[0], "tag_cohort_peak_bytes": peaks[1],
                "allowance_bytes": 67108864,
                "attribution": "Final fresh-unit lifetime memory.peak includes startup/warmup and whole-unit charge; not final minus initial peak",
                "projection_divergence": "NOT_ASSESSED_BY_COHORT_METRIC",
                "nightly_matrix_acceptance": "UNBOUND: one matched cell is not a nightly roster or other release criteria"}
    try:
        validate_divergence(*candidate_books)
        divergence = "OBSERVED_WINDOW_ZERO_WITH_PUBLICATION_AND_RETIREMENT"
    except Unbound as error:
        divergence = "UNBOUND_OR_FAILED: " + str(error)
    return {"daemon_lifetime_rss_bound": memory, "candidate_lifetime_rss_peak_bytes": peaks[0], "tag_lifetime_rss_peak_bytes": peaks[1], "difference_bytes": peaks[0] - peaks[1], "projection_divergence": divergence, "total_owned_process_cohort_footprint": "UNBOUND: daemon-PID VmHWM excludes children; no driver/proxy/cgroup or sampled substitution", "attribution": "Lifetime peak includes startup/warmup; paired difference is not workload-only attribution", "nightly_matrix_acceptance": "UNBOUND: one paired cell is not every required nightly run"}


def cohort_uint(value, reason):
    need(type(value) is int and 0 <= value <= 2**64-1, reason)
    return value


def cohort_identity(value):
    need(isinstance(value, dict) and set(value) == {"device", "inode", "mount_id", "fs_type"}, "held cgroup FD identity missing")
    for key in ("device", "inode", "mount_id"):
        need(cohort_uint(value.get(key), "invalid FD " + key) > 0, "zero FD " + key)
    need(value.get("fs_type") == 0x63677270, "held FD is not cgroup v2")
    return value


def cohort_proc(value, group):
    pid = integer(value.get("pid"), "member PID missing")
    need(pid > 0, "member PID zero")
    result = None
    for key in ("stat_before", "stat_after"):
        stat = value.get(key, "")
        need(isinstance(stat, str), "member proc stat missing")
        close = stat.rfind(")")
        fields = stat[close+1:].split()
        need(close > 0 and stat.split("(", 1)[0].strip() == str(pid) and len(fields) > 19,
             "member raw stat identity missing")
        need(fields[1].isdecimal() and fields[19].isdecimal() and int(fields[19]) > 0, "member PPID/birth missing")
        current = int(fields[1]), fields[19]
        need(result is None or result == current, "member PID reused/reparented during snapshot")
        result = current
    need(isinstance(value.get("cgroup"), str), "member cgroup missing")
    groups = [line[3:] for line in value["cgroup"].splitlines() if line.startswith("0::")]
    need(len(groups) == 1 and (groups[0] == group or groups[0].startswith(group+"/")), "member outside owned unit")
    need(all(part not in (".", "..", "") for part in groups[0].split("/")[1:]), "noncanonical member cgroup")
    return pid, result


def validate_cohort_bookends(report, bound, latest):
    need(type(report.get("cell", {}).get("agents")) is int and report["cell"]["agents"] == 32, "actual 32-agent edit cohort missing")
    outer = bound.get("owned_cohort_outer", {})
    need(type(outer.get("client_exit")) is int and outer["client_exit"] == 0 and outer.get("cleanup_closed") is True
         and outer.get("closure", {}).get("closed") is True and outer["closure"].get("forced") is False,
         "actual outer normal closure unavailable")
    owner = outer.get("owned_cohort_peak", {})
    need(owner.get("version") == 1 and owner.get("fresh_unit") is True and owner.get("single_build") is True
         and owner.get("inherited_unit_only") is True and owner.get("delegate") is False,
         "fresh single-build inherited owned unit UNBOUND (shared paired scope refused)")
    need(owner.get("binary_sha256") == bound["binary_sha256"], "unit build identity mismatch")
    unit, group = owner.get("unit", ""), owner.get("group", "")
    need(re.fullmatch(r"[A-Za-z0-9_.:-]+\.service", unit) and group == "/system.slice/" + unit,
         "exact reference unit missing")
    need(isinstance(owner.get("run_id"), str) and owner["run_id"] and outer.get("run_id") == owner["run_id"]
         and outer.get("unit") == unit, "owned outer run/unit missing or contradictory")
    invocation = owner.get("invocation_id", "")
    creation = owner.get("exec_main_start_timestamp_monotonic", "")
    need(re.fullmatch("[a-fA-F0-9]{32}", invocation) and int(invocation, 16) != 0,
         "unit InvocationID missing")
    need(isinstance(creation, str) and creation.isdecimal() and 0 < int(creation) <= 2**64-1,
         "unit creation timestamp missing")
    root = integer(owner.get("main_pid"), "owned unit main PID missing")
    need(root > 0 and isinstance(owner.get("main_birth_ticks"), str) and owner["main_birth_ticks"].isdecimal()
         and int(owner["main_birth_ticks"]) > 0 and isinstance(owner.get("boot_id"), str) and owner["boot_id"],
         "owned unit process epoch missing")
    identity = cohort_identity(owner.get("cgroup"))
    same = ("unit", "group", "run_id", "invocation_id", "exec_main_start_timestamp_monotonic", "main_pid",
            "main_birth_ticks", "boot_id", "binary_sha256", "single_build", "inherited_unit_only", "fresh_unit", "delegate", "cgroup")
    books = []
    driver_epoch = None
    peak_identity = None
    for phase in ("before", "after"):
        live = owner.get(phase, {})
        need(all(live.get(key) == owner.get(key) for key in same), "outer live unit epoch/ownership changed")
        book = report.get("readiness", {}).get("owned_cohort_peak."+phase, {})
        need(book.get("version") == 1 and book.get("phase") == phase and not book.get("unavailable_reason")
             and book.get("scope") == "fresh_owned_reference_unit_lifetime_memory_peak", "cohort bookend unavailable")
        need(book.get("unit") == unit and book.get("group") == group and book.get("boot_id") == owner["boot_id"]
             and cohort_identity(book.get("cgroup")) == identity, "unit/boot/held FD identity changed")
        raw = book.get("raw_memory_peak", "")
        need(isinstance(raw, str) and re.fullmatch(r"[0-9]+\n?", raw), "raw memory.peak missing/malformed")
        peak = cohort_uint(book.get("peak_bytes"), "memory.peak integer unavailable")
        need(int(raw) == peak, "raw peak differs from exact integer")
        current_peak_identity = cohort_identity(book.get("peak_file"))
        need(current_peak_identity["device"] == identity["device"] and current_peak_identity["mount_id"] == identity["mount_id"], "peak outside held mount")
        need(peak_identity is None or current_peak_identity == peak_identity, "peak file identity changed")
        peak_identity = current_peak_identity
        need(book.get("daemon_pid") == report["daemon_pid"], "cohort daemon mismatch")
        driver = integer(book.get("driver_pid"), "driver PID missing")
        need(driver > 0 and book.get("self_cgroup") == "0::"+group+"\n", "driver outside exact unit")
        need(book.get("agent_connections") == ["fixed-editor-"+str(i) for i in range(32)],
             "all32 initialized owned agent connections unavailable")
        members = book.get("members")
        need(isinstance(members, list) and 0 < len(members) <= 1024, "whole live unit census missing")
        parsed = [cohort_proc(value, group) for value in members]
        processes = dict(parsed)
        need(len(processes) == len(parsed) and root in processes and driver in processes and report["daemon_pid"] in processes,
             "duplicate/missing owned main/driver/daemon member")
        need(processes[root][1] == owner["main_birth_ticks"], "owned main PID reused")
        epoch = driver, processes[driver][1], report["daemon_pid"], processes[report["daemon_pid"]][1]
        need(driver_epoch is None or epoch == driver_epoch, "driver/daemon restarted")
        driver_epoch = epoch
        for pid in processes:
            seen = set()
            while pid != root:
                need(pid in processes and pid not in seen, "unowned/escaped process in cohort")
                seen.add(pid)
                pid = processes[pid][0]
        monotonic = cohort_uint(book.get("monotonic_ns"), "bookend monotonic epoch missing")
        need(monotonic >= int(creation)*1000, "unit creation is after the bookend")
        if books:
            need(monotonic >= books[0][0]["monotonic_ns"], "bookend monotonic clock reordered")
        a, b = stamp(book.get("started_at")), stamp(book.get("observed_at"))
        need(a <= b, "cohort bookend time reordered")
        if phase == "before":
            need(b <= stamp(report["started"]), "before bookend overlaps offers")
        else:
            need(latest <= a <= b <= stamp(report["finished"]), "after bookend precedes joined callbacks/outside report")
        books.append((book, peak))
    need(books[1][1] >= books[0][1], "decreasing/reset lifetime memory.peak")
    terminal = owner.get("terminal", {})
    need(type(terminal.get("main_pid")) is int and terminal["main_pid"] == 0
         and terminal.get("active_state") == "inactive" and terminal.get("empty") is True,
         "owned unit normal empty closure unavailable")
    if terminal.get("load_state") == "not-found":
        # Existing systemd-run --collect removes the stopped unit and its
        # InvocationID. The two live epochs are already checked; normal outer
        # exit plus this exact existing empty/collected shape closes custody.
        need(terminal.get("control_group") == "" and terminal.get("invocation_id") in (None, "")
             and terminal.get("result") in (None, "", "success"), "collected unit closure contradictory")
    else:
        need(terminal.get("load_state") == "loaded" and terminal.get("invocation_id") == invocation
             and terminal.get("control_group") == group and terminal.get("result") == "success",
             "loaded unit closure epoch/result unavailable")
    return books[0][0], books[1][0], books[1][1]


def validate_cohort_pair(packet, catalog):
    return validate_pair(packet, catalog, cohort=True)


def validate_nightly(packet, catalog):
    nightly = packet["binding"]["nightly"]
    need(nightly.get("event") in ("schedule", "workflow_dispatch") and nightly.get("ref") == "refs/heads/main", "not a qualifying main nightly-equivalent run")
    need(integer(nightly.get("run_id"), "run ID missing") > 0 and integer(nightly.get("attempt"), "attempt missing") > 0 and re.fullmatch("[a-f0-9]{40}", nightly.get("revision", "")), "nightly producer identity missing")
    expected = {c["index"]: c for c in catalog["cells"]}
    cases = packet["cases"]
    need(len(cases) == len(expected) and {x["cell_index"] for x in cases} == set(expected), "incomplete/duplicate nightly catalog roster")
    for case in cases:
        need(case.get("nightly") == nightly and case["bound"]["revision"] == nightly["revision"], "mixed nightly run/attempt/revision")
        report = case["report"]
        validate_catalog_policy(report, catalog)
        latest = validate_report(report, case["bound"], expected[case["cell_index"]])
        before, after, _ = validate_bookends(report, latest)
        validate_divergence(before, after)
    return {"projection_divergence_zero": "PASS_FOR_COMPLETE_BOUND_CATALOG_ROSTER", "cases": len(cases), "nightly_release_gate": "NOT_ASSESSED: raw nightly budgets, hosts, seeded RED and other issue criteria remain independent"}


def validate_catalog_policy(report, catalog):
    need(catalog.get("version") == 1 and catalog.get("seed") == 1 and catalog.get("edit_mean_ns") == 15_000_000_000 and catalog.get("product_deadline_seconds") == 120 and catalog.get("warm_seconds") == 1800 and catalog.get("stop_seconds") == 30 and catalog.get("tool_surface") == "all/defer", "frozen C policy changed")
    need(all(report["plan"].get(k) == catalog[k] for k in ("seed", "edit_mean_ns", "product_deadline_seconds", "warm_seconds", "stop_seconds", "tool_surface")), "actual plan differs from frozen catalog policy")
    need(report["plan"].get("language", "") in ("", "go"), "Go catalog/language mismatch")


def load_bound_report(bound, *, cohort=False):
    raw = Path(bound["report_path"]).read_bytes()
    need(hashlib.sha256(raw).hexdigest() == bound["report_sha256"], "raw report digest mismatch")
    outer = json.loads(Path(bound["outer_result_path"]).read_text(encoding="utf-8"))
    need(outer.get("report_sha256") == bound["report_sha256"] and outer.get("client_exit") == 0 and outer.get("cleanup_closed") is True and outer.get("closure", {}).get("closed") is True and outer.get("closure", {}).get("forced") is False, "existing C outer raw/closure provenance unavailable")
    flag = "-readback-reference-cohort-peak-evidence" if cohort else "-readback-reference-projection-evidence"
    flags = [x for x in outer.get("argv", []) if x.startswith(flag)]
    need(len(flags) == 1 and flags[0] in (flag, flag+"=true"), "opt-in collector build/argv UNBOUND")
    if cohort:
        # Always take ownership from the actual existing outer result, never a
        # caller-supplied replacement embedded in the input binding.
        bound["owned_cohort_outer"] = outer
    return json.loads(raw)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("packet")
    parser.add_argument("catalog")
    parser.add_argument("--mode", choices=("pair", "nightly", "cohort-pair"), default="pair")
    args = parser.parse_args()
    with open(args.packet, encoding="utf-8") as stream:
        packet = json.load(stream)
    try:
        raw_catalog = Path(args.catalog).read_bytes()
        need(hashlib.sha256(raw_catalog).hexdigest() == packet["binding"]["catalog_sha256"] == CATALOG_SHA256, "compiled C catalog digest mismatch")
        catalog = json.loads(raw_catalog)
        if args.mode in ("pair", "cohort-pair"):
            cohort = args.mode == "cohort-pair"
            for role in ("candidate", "tag"):
                packet[role] = load_bound_report(packet["binding"][role], cohort=cohort)
            result = validate_pair(packet, catalog, cohort=cohort)
        else:
            for case in packet["cases"]:
                case["report"] = load_bound_report(case["bound"])
            result = validate_nightly(packet, catalog)
    except (Unbound, KeyError, TypeError, ValueError, OSError) as error:
        result = {"state": "UNBOUND_OR_FAILED", "reason": str(error)}
    print(json.dumps(result, indent=2, allow_nan=False))
    # A daemon-only metric cannot give the total-cohort criterion a green exit.
    # Its scoped numeric result remains available for the acceptance owner.
    return 0 if (result.get("projection_divergence_zero") == "PASS_FOR_COMPLETE_BOUND_CATALOG_ROSTER"
                 or result.get("total_owned_process_cohort_footprint") == "PASS") else 2


if __name__ == "__main__":
    raise SystemExit(main())
