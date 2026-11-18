"""Synthetic raw-contract controls only; no daemon, native artifact or campaign."""
import copy
import datetime as dt
import hashlib
import json
from pathlib import Path
import tempfile
import unittest

import projection_evidence as gate


EPOCH = dt.datetime(2026, 11, 3, tzinfo=dt.timezone.utc)


def time_at(seconds):
    return (EPOCH + dt.timedelta(seconds=seconds)).isoformat()


def call(identity, seconds, raw=None):
    return dict(rpc_id=identity, started=time_at(seconds), write_started=time_at(seconds+.01),
                write_finished=time_at(seconds+.02), reply_delivered=time_at(seconds+.03),
                finished=time_at(seconds+.04), latency_ms=dict(value=40, unit="ms", reason=""),
                correct=True, raw=raw or {})


def evidence(pid, phase, peak_mib, counter=0):
    start = -2 if phase == "before" else 150
    section = dict(enabled=True, projection_divergence=counter,
                   published=1 if phase == "before" else 101,
                   retired=1 if phase == "before" else 101)
    raw = dict(contents=[dict(uri="gortex://index-health", text=json.dumps(dict(pending_projection=section)))])
    rpc = call(1 if phase == "before" else 1000, start, raw)
    rpc["request_params"] = {"uri": "gortex://index-health"}
    stat = str(pid) + " (daemon (worker)) S " + "0 " * 18 + "900 0\n"
    return dict(version=1, phase=phase, observed_at=time_at(start), connection_identity="fixed-editor-0",
                process=dict(pid=pid, birth_ticks="900", boot_id="boot-epoch", stat_before=stat,
                             stat_after=stat, status="VmHWM: %d kB\n" % (peak_mib*1024),
                             scope="owned_daemon_pid_only_since_process_start",
                             lifetime_rss_peak_bytes=dict(value=peak_mib*1024**2, unit="bytes", reason="")),
                projection_divergence=dict(value=counter, unit="count", reason=""), index_health_rpc=rpc)


def fixture():
    cell = dict(agents=32, ack_mode="", kind="add_crossfile_call", tool="edit.file", offers=100)
    selected = dict(index=4, schedule=dict(cell=cell, arrival=[dict(Editor=i % 32, DueNS=i*10**9) for i in range(100)]))
    policy = dict(version=1, seed=1, edit_mean_ns=15_000_000_000, product_deadline_seconds=120,
                  warm_seconds=1800, stop_seconds=30, tool_surface="all/defer")
    catalog = dict(policy, cells=[selected])
    host = dict(profile="reference", validated=True, os="linux", arch="amd64", machine="reference",
                limits=dict(cpu_quota=dict(value=8, unit="cores", reason=""),
                            memory_max=dict(value=16*1024**3, unit="bytes", reason="")))
    packet = dict(binding=dict(baseline_tag="v0.64.10", baseline_tag_receipt_sha256="f"*64, cell_index=4))
    for role, pid, digest in (("candidate", 42, "a"), ("tag", 52, "b")):
        revision = gate.BASELINE_TAG_REVISION if role == "tag" else digest*40
        plan = dict(policy, language="go", binary_revision=revision, binary_sha256=digest*64)
        plan.pop("cells", None)
        bound = dict(plan=plan, plan_sha256="d"*64, revision=revision, binary_sha256=digest*64,
                     manifest_sha256="e"*64, host=host, host_sha256="c"*64)
        records = []
        for i, arrival in enumerate(selected["schedule"]["arrival"]):
            calls = [call(10+i*5+j, i+1+j*.05) for j in range(5)]
            records.append(dict(ordinal=i, editor=arrival["Editor"], due_ns=arrival["DueNS"],
                                correct=True, physical_bytes_correct=True, errors=[],
                                connection_identity="fixed-editor-%d" % arrival["Editor"],
                                requested_connection_surface="all/defer", ack=calls[0],
                                probes=dict(zip(("source", "summary", "editing_context", "file"), calls[1:]))))
        h = dict(host=host, volume_identity="ssd-volume", volume_probe_path="/owned")
        report = dict(version=1, protocol_only=False, role="under_edit", cell=cell, errors=[],
                      plan=plan, plan_sha256="d"*64, manifest_sha256="e"*64, host=host,
                      host_before=h, host_after=copy.deepcopy(h), expected_offers=100, observed_offers=100,
                      acceptance_nominal_offers=100, records=records, correctness=dict(offered=100, correct=100, missing=0, failures=0, **{"pass": True}),
                      started=time_at(0), finished=time_at(200), daemon_pid=pid, tool_surface="all/defer",
                      after_image_population_sha256="e"*64, logical_read_plan_sha256="d"*64,
                      comparison_identity=dict(binary_sha256=digest*64, manifest_sha256="e"*64,
                                               host_sha256="c"*64, population_sha256="e"*64,
                                               read_plan_sha256="d"*64, arrival_sha256="f"*64,
                                               agents=32, kind=cell["kind"], tool_surface="all/defer"),
                      runtime_environment={x: "" for x in ("GOMAXPROCS", "GOMEMLIMIT", "GOGC", "GODEBUG")},
                      cleanup=dict(forced=False, stop_error="", native_handle_cleared=True, native_leader_joined=True,
                                   socket_absent=True, owned_store_removed=True),
                      readiness={"projection_evidence.before": evidence(pid, "before", 10),
                                 "projection_evidence.after": evidence(pid, "after", 50 if role == "candidate" else 30)})
        packet["binding"][role] = copy.deepcopy(bound)
        packet[role] = copy.deepcopy(report)
    return packet, catalog


class ProjectionEvidenceContract(unittest.TestCase):
    def test_actual_report_digest_outer_closure_and_opt_in_are_required(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            raw = b'{"version":1}\n'
            (root/"report.json").write_bytes(raw)
            bound = dict(report_path=str(root/"report.json"), report_sha256=hashlib.sha256(raw).hexdigest(),
                         outer_result_path=str(root/"outer.json"))
            outer = dict(report_sha256=bound["report_sha256"], client_exit=0, cleanup_closed=True,
                         closure=dict(closed=True, forced=False), argv=["client", "-readback-reference-projection-evidence"])
            (root/"outer.json").write_text(json.dumps(outer))
            self.assertEqual(gate.load_bound_report(bound), {"version": 1})
            for mode in ("digest", "missing_optin", "forced", "unknown_closure"):
                b, o = copy.deepcopy(bound), copy.deepcopy(outer)
                if mode == "digest": b["report_sha256"] = "0"*64
                if mode == "missing_optin": o["argv"] = ["legacy-client"]
                if mode == "forced": o["closure"]["forced"] = True
                if mode == "unknown_closure": o["closure"]["closed"] = None
                (root/"outer.json").write_text(json.dumps(o))
                with self.subTest(mode=mode), self.assertRaises(gate.Unbound):
                    gate.load_bound_report(b)

    def test_complete_pair_is_scoped_not_total_or_workload_attribution(self):
        p, c = fixture()
        result = gate.validate_pair(p, c)
        self.assertEqual(result["daemon_lifetime_rss_bound"], "PASS")
        self.assertEqual(result["difference_bytes"], 20*1024**2)
        self.assertTrue(result["total_owned_process_cohort_footprint"].startswith("UNBOUND"))
        self.assertTrue(result["projection_divergence"].startswith("OBSERVED"))

    def test_frozen_reports_without_opt_in_bookends_stay_unbound(self):
        p, c = fixture()
        p["candidate"]["readiness"] = {}
        with self.assertRaises(gate.Unbound):
            gate.validate_pair(p, c)

    def test_bound_linux_arm64_resources_are_supported(self):
        p, c = fixture()
        for role in ("candidate", "tag"):
            p["binding"][role]["host"]["arch"] = "arm64"
            p[role]["host"]["arch"] = "arm64"
            p[role]["host_before"]["host"]["arch"] = "arm64"
            p[role]["host_after"]["host"]["arch"] = "arm64"
        self.assertEqual(gate.validate_pair(p, c)["daemon_lifetime_rss_bound"], "PASS")

    def test_startup_dominated_lifetime_difference_is_not_zero_delta(self):
        p, c = fixture()
        for role, value in (("candidate", 100), ("tag", 1)):
            for phase in ("before", "after"):
                p[role]["readiness"]["projection_evidence."+phase] = evidence(p[role]["daemon_pid"], phase, value)
        result = gate.validate_pair(p, c)
        self.assertEqual(result["daemon_lifetime_rss_bound"], "FAIL")
        self.assertEqual(result["difference_bytes"], 99*1024**2)

    def test_main_cannot_be_relabelled_as_the_tag(self):
        p, c = fixture()
        p["binding"]["tag"]["revision"] = "a"*40
        p["binding"]["tag"]["plan"]["binary_revision"] = "a"*40
        p["tag"]["plan"]["binary_revision"] = "a"*40
        with self.assertRaises(gate.Unbound):
            gate.validate_pair(p, c)

    def test_independent_mutated_raw_contracts_refuse(self):
        mutations = {
            "missing_census": lambda p: p["candidate"]["records"].pop(),
            "censor": lambda p: p["candidate"]["records"][0]["errors"].append("censored"),
            "wrong_editor": lambda p: p["candidate"]["records"][0].update(editor=7),
            "wrong_session": lambda p: p["candidate"]["records"][0].update(connection_identity="fixed-editor-7"),
            "wrong_arrival": lambda p: p["candidate"]["records"][0].update(due_ns=99),
            "rpc_reuse": lambda p: p["candidate"]["records"][0]["ack"].update(rpc_id=1),
            "proxy_pid": lambda p: p["candidate"]["readiness"]["projection_evidence.after"]["process"].update(pid=99),
            "pid_reuse": lambda p: p["candidate"]["readiness"]["projection_evidence.after"]["process"].update(birth_ticks="901"),
            "boot_change": lambda p: p["candidate"]["readiness"]["projection_evidence.after"]["process"].update(boot_id="new"),
            "sampled_max": lambda p: p["candidate"]["readiness"]["projection_evidence.after"]["process"].update(scope="sampled_max"),
            "heap": lambda p: p["candidate"]["readiness"]["projection_evidence.after"]["process"].update(scope="Go_heap"),
            "driver_cgroup": lambda p: p["candidate"]["readiness"]["projection_evidence.after"]["process"].update(scope="whole_unit_memory.peak"),
            "null_memory": lambda p: p["candidate"]["readiness"]["projection_evidence.after"]["process"]["lifetime_rss_peak_bytes"].update(value=None),
            "fake_hwm": lambda p: p["candidate"]["readiness"]["projection_evidence.after"]["process"].update(status="VmHWM: 1 kB\n"),
            "late_before": lambda p: p["candidate"]["readiness"]["projection_evidence.before"]["index_health_rpc"].update(finished=time_at(2)),
            "early_after": lambda p: p["candidate"]["readiness"]["projection_evidence.after"].update(observed_at=time_at(1)),
            "unknown_surface": lambda p: p["candidate"].update(tool_surface="synthetic"),
            "wrong_tag": lambda p: p["binding"].update(baseline_tag="main"),
            "forced": lambda p: p["candidate"]["cleanup"].update(forced=True),
            "host_limit": lambda p: p["candidate"]["host"]["limits"]["memory_max"].update(value=8*1024**3),
            "runtime_tuning": lambda p: p["candidate"]["runtime_environment"].update(GOMEMLIMIT="4GiB"),
        }
        for name, mutate in mutations.items():
            with self.subTest(name=name):
                p, c = fixture(); mutate(p)
                with self.assertRaises((gate.Unbound, TypeError, KeyError)):
                    gate.validate_pair(p, c)

    def test_missing_nonzero_or_reset_counter_never_becomes_zero(self):
        for mode in ("null", "missing_section", "nonzero", "reset", "no_publication", "no_retirement"):
            with self.subTest(mode=mode):
                p, c = fixture()
                before = p["candidate"]["readiness"]["projection_evidence.before"]
                after = p["candidate"]["readiness"]["projection_evidence.after"]
                if mode == "null": after["projection_divergence"]["value"] = None
                if mode == "missing_section": after["index_health_rpc"]["raw"]["contents"][0]["text"] = "{}"
                if mode == "nonzero": after["projection_divergence"]["value"] = 1
                if mode == "reset": before["projection_divergence"]["value"] = 2
                if mode in ("nonzero", "reset"):
                    book = after if mode == "nonzero" else before
                    text = book["index_health_rpc"]["raw"]["contents"][0]
                    body = json.loads(text["text"])
                    body["pending_projection"]["projection_divergence"] = book["projection_divergence"]["value"]
                    text["text"] = json.dumps(body)
                if mode in ("no_publication", "no_retirement"):
                    text = after["index_health_rpc"]["raw"]["contents"][0]
                    body = json.loads(text["text"])
                    body["pending_projection"]["published" if mode == "no_publication" else "retired"] = 1
                    text["text"] = json.dumps(body)
                result = gate.validate_pair(p, c)
                self.assertFalse(result["projection_divergence"].startswith("OBSERVED"))

    def test_nightly_requires_complete_same_run_attempt_main_roster(self):
        p, c = fixture()
        nightly = dict(event="schedule", ref="refs/heads/main", run_id=10, attempt=1, revision="a"*40)
        case = dict(cell_index=4, nightly=copy.deepcopy(nightly), report=p["candidate"], bound=p["binding"]["candidate"])
        packet = dict(binding=dict(nightly=nightly), cases=[case])
        self.assertEqual(gate.validate_nightly(packet, c)["cases"], 1)
        for mode in ("missing", "duplicate", "pull_request", "attempt"):
            q = copy.deepcopy(packet)
            if mode == "missing": q["cases"] = []
            if mode == "duplicate": q["cases"] *= 2
            if mode == "pull_request": q["binding"]["nightly"]["event"] = "pull_request"
            if mode == "attempt": q["cases"][0]["nightly"]["attempt"] = 2
            with self.subTest(mode=mode), self.assertRaises(gate.Unbound):
                gate.validate_nightly(q, c)


if __name__ == "__main__":
    unittest.main()
