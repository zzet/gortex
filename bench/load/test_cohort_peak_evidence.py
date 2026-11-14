"""Owned-cohort contract fixtures; no Linux process, artifact or campaign launch."""
import copy
import unittest
import projection_evidence as gate
from test_projection_evidence import fixture, time_at


def proc(pid, parent, group):
    return dict(pid=pid, stat_before=f"{pid} (owned (process)) S {parent} " + "0 "*17 + "900 0\n",
                stat_after=f"{pid} (owned (process)) S {parent} " + "0 "*17 + "900 0\n",
                cgroup=f"0::{group}\n")


def cohort_fixture():
    packet, catalog = fixture()
    for role in ("candidate", "tag"):
        report, bound = packet[role], packet["binding"][role]
        unit = role + "-fresh.service"
        group = "/system.slice/" + unit
        identity = dict(device=9, inode=10 if role == "candidate" else 20, mount_id=30, fs_type=0x63677270)
        peak_id = dict(device=9, inode=identity["inode"]+1, mount_id=30, fs_type=0x63677270)
        owner = dict(version=1, unit=unit, group=group, run_id=role+"-fresh", invocation_id=("a" if role=="candidate" else "b")*32,
                     exec_main_start_timestamp_monotonic="100", main_pid=10, main_birth_ticks="900", boot_id="boot",
                     binary_sha256=bound["binary_sha256"], single_build=True, inherited_unit_only=True,
                     fresh_unit=True, delegate=False, cgroup=identity)
        outer = dict(run_id=owner["run_id"], unit=unit, client_exit=0, cleanup_closed=True, closure=dict(closed=True,forced=False), owned_cohort_peak=dict(owner, before=dict(owner), after=dict(owner),
                      terminal=dict(load_state="loaded",invocation_id=owner["invocation_id"], control_group=group, main_pid=0,
                                    active_state="inactive", result="success", empty=True)))
        bound["owned_cohort_outer"] = outer
        for phase in ("before", "after"):
            peak = (80 if role=="candidate" else 32)*1024**2
            book = dict(version=1, phase=phase, started_at=time_at(-2 if phase=="before" else 150),
                        observed_at=time_at(-1 if phase=="before" else 151), monotonic_ns=1000000000 if phase=="before" else 2000000000, scope="fresh_owned_reference_unit_lifetime_memory_peak",
                        unit=unit, group=group, boot_id="boot", driver_pid=11, daemon_pid=report["daemon_pid"],
                        self_cgroup=f"0::{group}\n", cgroup=identity, peak_file=peak_id,
                        raw_memory_peak=str(peak)+"\n", peak_bytes=peak, unavailable_reason="",
                        agent_connections=[f"fixed-editor-{i}" for i in range(32)],
                        members=[proc(10, 1, group), proc(11, 10, group), proc(report["daemon_pid"], 11, group)])
            report["readiness"]["owned_cohort_peak."+phase] = copy.deepcopy(book)
    return packet, catalog


class CohortPeakEvidenceContract(unittest.TestCase):
    def test_lifetime_peak_inclusive_boundary_not_subtracted(self):
        packet, catalog = cohort_fixture()
        for phase in ("before", "after"):
            book=packet["candidate"]["readiness"]["owned_cohort_peak."+phase]
            book["peak_bytes"]=96*1024**2
            book["raw_memory_peak"]=str(book["peak_bytes"])+"\n"
        result=gate.validate_cohort_pair(packet,catalog)
        self.assertEqual(result["total_owned_process_cohort_footprint"],"PASS")
        self.assertEqual(result["candidate_cohort_peak_bytes"],96*1024**2)
        book=packet["candidate"]["readiness"]["owned_cohort_peak.after"]
        book["peak_bytes"]+=1;book["raw_memory_peak"]=str(book["peak_bytes"])+"\n"
        self.assertEqual(gate.validate_cohort_pair(packet,catalog)["total_owned_process_cohort_footprint"],"FAIL")

    def test_raw_integers_are_not_float64(self):
        packet,catalog=cohort_fixture()
        for role in ("candidate","tag"):
            for phase in ("before","after"):
                b=packet[role]["readiness"]["owned_cohort_peak."+phase]
                b["peak_bytes"]=9007199254740993+(67108864 if role=="candidate" else 0)
                b["raw_memory_peak"]=str(b["peak_bytes"])+"\n"
        self.assertEqual(gate.validate_cohort_pair(packet,catalog)["total_owned_process_cohort_footprint"],"PASS")

    def test_unknown_or_contradictory_scope_is_unbound(self):
        mutations={
            "shared_paired":lambda p:p["binding"]["candidate"]["owned_cohort_outer"]["owned_cohort_peak"].update(single_build=False),
            "unknown_creation":lambda p:p["binding"]["candidate"]["owned_cohort_outer"]["owned_cohort_peak"].pop("invocation_id"),
            "reused_unit":lambda p:p["binding"]["candidate"]["owned_cohort_outer"]["owned_cohort_peak"].update(fresh_unit=False),
            "delegate":lambda p:p["binding"]["candidate"]["owned_cohort_outer"]["owned_cohort_peak"].update(delegate=True),
            "restart":lambda p:p["binding"]["candidate"]["owned_cohort_outer"]["owned_cohort_peak"]["after"].update(invocation_id="c"*32),
            "outside_child":lambda p:p["candidate"]["readiness"]["owned_cohort_peak.after"]["members"].append(proc(99,1,"/elsewhere")),
            "missing_actor":lambda p:p["candidate"]["readiness"]["owned_cohort_peak.after"]["agent_connections"].pop(),
            "missing_daemon":lambda p:p["candidate"]["readiness"]["owned_cohort_peak.after"].update(members=[proc(10,1,"/system.slice/candidate-fresh.service"),proc(11,10,"/system.slice/candidate-fresh.service")]),
            "probe_unavailable":lambda p:p["candidate"]["readiness"]["owned_cohort_peak.after"].update(unavailable_reason="permission denied"),
            "missing_peak":lambda p:p["candidate"]["readiness"]["owned_cohort_peak.after"].update(peak_bytes=None),
            "malformed_peak":lambda p:p["candidate"]["readiness"]["owned_cohort_peak.after"].update(raw_memory_peak="max\n"),
            "non_cgroup_held_fd":lambda p:p["candidate"]["readiness"]["owned_cohort_peak.after"]["cgroup"].update(fs_type=0x01021994),
            "changed_fd":lambda p:p["candidate"]["readiness"]["owned_cohort_peak.after"]["cgroup"].update(inode=999),
            "unit_created_after_bookend":lambda p:p["binding"]["candidate"]["owned_cohort_outer"]["owned_cohort_peak"].update(exec_main_start_timestamp_monotonic="2000000"),
            "missing_monotonic":lambda p:p["candidate"]["readiness"]["owned_cohort_peak.after"].pop("monotonic_ns"),
            "late_before":lambda p:p["candidate"]["readiness"]["owned_cohort_peak.before"].update(observed_at=time_at(1)),
            "early_after":lambda p:p["candidate"]["readiness"]["owned_cohort_peak.after"].update(started_at=time_at(1)),
            "not32":lambda p:p["candidate"]["cell"].update(agents=16),
            "censor":lambda p:p["candidate"]["records"][0]["errors"].append("censored"),
            "readonly":lambda p:p["candidate"].update(role="no_edit"),
            "forced_closure":lambda p:p["candidate"]["cleanup"].update(forced=True),
            "forced_outer":lambda p:p["binding"]["candidate"]["owned_cohort_outer"]["closure"].update(forced=True),
            "unknown_terminal":lambda p:p["binding"]["candidate"]["owned_cohort_outer"]["owned_cohort_peak"]["terminal"].update(load_state="unknown"),
            "collected_but_group_present":lambda p:p["binding"]["candidate"]["owned_cohort_outer"]["owned_cohort_peak"]["terminal"].update(load_state="not-found"),
            "borrow_daemon_metric":lambda p:p["candidate"]["readiness"].pop("owned_cohort_peak.after"),
        }
        for name,mutate in mutations.items():
            with self.subTest(name=name):
                packet,catalog=cohort_fixture();mutate(packet)
                with self.assertRaises((gate.Unbound,KeyError,TypeError,ValueError)):
                    gate.validate_cohort_pair(packet,catalog)

    def test_existing_collect_lifecycle_normal_empty_unit_is_supported(self):
        packet,catalog=cohort_fixture()
        for role in ("candidate","tag"):
            packet["binding"][role]["owned_cohort_outer"]["owned_cohort_peak"]["terminal"] = dict(
                load_state="not-found",active_state="inactive",main_pid=0,control_group="",empty=True)
        self.assertEqual(gate.validate_cohort_pair(packet,catalog)["total_owned_process_cohort_footprint"],"PASS")
        packet["binding"]["candidate"]["owned_cohort_outer"]["client_exit"]=1
        with self.assertRaises(gate.Unbound):
            gate.validate_cohort_pair(packet,catalog)

    def test_candidate_tag_reused_invocation_is_unbound(self):
        packet,catalog=cohort_fixture()
        first=packet["binding"]["candidate"]["owned_cohort_outer"]["owned_cohort_peak"]
        second=packet["binding"]["tag"]["owned_cohort_outer"]["owned_cohort_peak"]
        for item in (second,second["before"],second["after"],second["terminal"]):
            item["invocation_id"]=first["invocation_id"]
        with self.assertRaises(gate.Unbound):
            gate.validate_cohort_pair(packet,catalog)

    def test_legacy_pair_remains_daemon_only(self):
        packet,catalog=cohort_fixture()
        self.assertTrue(gate.validate_pair(packet,catalog)["total_owned_process_cohort_footprint"].startswith("UNBOUND"))

if __name__=="__main__":
    unittest.main()
