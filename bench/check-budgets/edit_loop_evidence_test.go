package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func editLoopEvidenceFixture(t *testing.T) report {
	t.Helper()
	raw := syntheticReport()
	workload := raw["workload"].(map[string]any)
	workload["scenario"], workload["agents"], workload["edit_agents"], workload["session_model"] = "edit-and-probe", 0, 4, "fixed_editor_per_ordinal_plus_one_separate_prober_per_edit"
	phase := raw["phases"].([]any)[0].(map[string]any)
	phase["name"], phase["session_model"], phase["duration_seconds"] = "edit-and-probe", workload["session_model"], 600
	phase["gate"].(map[string]any)["edit_loop_wall"] = map[string]any{"clock": "monotonic", "wall_ns": int64(600000000000), "complete": true}
	value := func(n float64, unit string) map[string]any {
		return map[string]any{"value": n, "unit": unit, "reason": ""}
	}
	metrics := phase["metrics"].(map[string]any)
	metrics["p95_wal_bytes"], metrics["peak_wal_bytes"] = value(32*1024*1024, "bytes"), value(64*1024*1024, "bytes")
	metrics["reclaim_gate_closures"], metrics["reclaim_writer_holds"], metrics["reclaim_writer_hold_p95_ms"] = value(0, "closures"), value(4, "holds"), value(10, "ms")
	modes := []string{"json", "toon", "compact", "facade", "cli"}
	steps, records := make([]any, 100), make([]any, 100)
	for i := range 100 {
		mode := modes[i%5]
		steps[i] = map[string]any{"ordinal": i, "probe_mode": mode, "editor": i % 4}
		reads := make([]any, 0, 5)
		for _, kind := range []string{"file", "source", "summary", "search_visible", "text"} {
			reads = append(reads, map[string]any{"kind": kind, "correct": true, "error": "", "observation_only": false})
		}
		records[i] = map[string]any{"ordinal": i, "probe_mode": mode, "outcome": "complete", "t_ack_ms": value(50, "ms"), "ack_readback_exact": true, "t_gate_readback_ms": value(100, "ms"), "readbacks": reads, "errors": []string{}}
	}
	coverage := map[string]any{}
	for _, mode := range modes {
		coverage[mode] = map[string]any{"probe_answers": 20, "observed_edits": 20, "decode_failures": 0, "claims_fresh": 20, "claims_fresh_observable": true, "stale_claimed_fresh": value(0, "answers")}
	}
	phase["edit_probe"] = map[string]any{"plan": map[string]any{"gate_modes": true, "count": 100, "editors": 4, "mode": "json", "arrival": "precomputed_open_loop_poisson", "mean_interarrival_ns": int64(15000000000), "session_model": workload["session_model"], "steps": steps}, "edits": records, "output_modes": coverage}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	result, err := decodeReport(data)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestEditLoopMeasurementsUseActualRecordsAndCounters(t *testing.T) {
	r := editLoopEvidenceFixture(t)
	config := inputs{CandidateRevision: strings.Repeat("a", 40), Hosts: map[string]string{"reference_linux": "synthetic-host"}}
	if err := validateEditLoopReport(config, runInput{Role: "candidate", HostClass: "reference_linux", Scenario: "loadedit-4"}, r); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]float64{"edit_ack_p95": .05, "edit_ack_max": .05, "readback_p95": .1, "readback_max": .1, "readback_correct": 1, "wal_p95_mib": 32, "wal_peak_mib": 64, "read_gate_closures": 0, "reclaim_hold_p95": .01, "silent_stale_json": 0} {
		got, err := measureEditLoopRow(r.Phases[0], name)
		if err != nil || got != want {
			t.Errorf("%s=%v error%v, want%v", name, got, err, want)
		}
	}
}

func TestEditLoopEvidenceRejectsWrongPhaseCadenceAndWall(t *testing.T) {
	config := inputs{CandidateRevision: strings.Repeat("a", 40), Hosts: map[string]string{"reference_linux": "synthetic-host"}}
	for _, variant := range []string{"phase_name", "coupled_phase", "cadence", "wall_missing", "wall_over15min"} {
		t.Run(variant, func(t *testing.T) {
			r := editLoopEvidenceFixture(t)
			switch variant {
			case "phase_name":
				r.Phases[0].Name = "read"
			case "coupled_phase":
				r.Phases[0].SessionModel = "coupled_native"
			case "wall_missing":
				delete(r.Phases[0].Gate, "edit_loop_wall")
			case "wall_over15min":
				r.Phases[0].Gate["edit_loop_wall"] = json.RawMessage(`{"clock":"monotonic","wall_ns":901000000000,"complete":true}`)
			case "cadence":
				var raw map[string]any
				if err := json.Unmarshal(r.Phases[0].EditProbe, &raw); err != nil {
					t.Fatal(err)
				}
				raw["plan"].(map[string]any)["mean_interarrival_ns"] = 1000000
				data, err := json.Marshal(raw)
				if err != nil {
					t.Fatal(err)
				}
				r.Phases[0].EditProbe = data
			}
			if err := validateEditLoopReport(config, runInput{Role: "candidate", HostClass: "reference_linux", Scenario: "loadedit-4"}, r); err == nil {
				t.Fatal("invalid timing/workload evidence accepted")
			}
		})
	}
}

func TestEditLoopModeCoverageCannotBorrowAnotherEditOrMissingFreshClaims(t *testing.T) {
	for _, variant := range []string{"one_edit", "no_claims", "missing_mode", "decode_failed", "unobservable"} {
		t.Run(variant, func(t *testing.T) {
			r := editLoopEvidenceFixture(t)
			var raw map[string]any
			if err := json.Unmarshal(r.Phases[0].EditProbe, &raw); err != nil {
				t.Fatal(err)
			}
			modes := raw["output_modes"].(map[string]any)
			coverage := modes["json"].(map[string]any)
			switch variant {
			case "one_edit":
				coverage["observed_edits"] = 1
			case "no_claims":
				coverage["claims_fresh"] = 0
			case "missing_mode":
				delete(modes, "json")
			case "decode_failed":
				coverage["decode_failures"] = 1
			case "unobservable":
				coverage["claims_fresh_observable"] = false
			}
			data, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			r.Phases[0].EditProbe = data
			if _, err := measureEditLoopRow(r.Phases[0], "silent_stale_json"); err == nil {
				t.Fatal("missing/refused per-mode evidence became zero-stale")
			}
		})
	}
}
