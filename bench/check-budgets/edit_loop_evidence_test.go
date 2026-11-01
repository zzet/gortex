package main

import (
	"encoding/json"
	"fmt"
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
		tools := map[string]any{}
		for _, tool := range []string{"read.source", "read.summary", "read.editing_context", "search.symbols", "search.text", "relations.callers", "trace.call_chain"} {
			tools[tool] = map[string]any{"probe_answers": 20, "observed_edits": 20, "decode_failures": 0, "claims_fresh": 20}
		}
		coverage[mode] = map[string]any{"tools": tools, "probe_answers": 140, "observed_edits": 20, "decode_failures": 0, "claims_fresh": 140, "claims_fresh_observable": true, "stale_claimed_fresh": value(0, "answers")}
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

func TestEditLoopModeCoverageRequiresEveryApplicableTool(t *testing.T) {
	for _, variant := range []string{"missing_tool", "one_edit", "decode_failed", "missing_fresh_claim"} {
		t.Run(variant, func(t *testing.T) {
			r := editLoopEvidenceFixture(t)
			var raw map[string]any
			if err := json.Unmarshal(r.Phases[0].EditProbe, &raw); err != nil {
				t.Fatal(err)
			}
			tools := raw["output_modes"].(map[string]any)["json"].(map[string]any)["tools"].(map[string]any)
			tool := tools["read.summary"].(map[string]any)
			switch variant {
			case "missing_tool":
				delete(tools, "read.summary")
			case "one_edit":
				tool["observed_edits"] = 1
			case "decode_failed":
				tool["decode_failures"] = 1
			case "missing_fresh_claim":
				delete(tool, "claims_fresh")
			}
			data, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			r.Phases[0].EditProbe = data
			if _, err := measureEditLoopRow(r.Phases[0], "silent_stale_json"); err == nil {
				t.Fatal("another tool lent coverage or freshness credit to an unproved tool")
			}
		})
	}
}

func TestEditLoopNightlySingleEditorRetainsItsOfferedSchedule(t *testing.T) {
	r := editLoopEvidenceFixture(t)
	var workload, raw map[string]any
	if err := json.Unmarshal(r.Workload, &workload); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(r.Phases[0].EditProbe, &raw); err != nil {
		t.Fatal(err)
	}
	session := "one_editor_plus_one_separate_prober_per_edit"
	workload["edit_agents"], workload["session_model"] = 1, session
	plan := raw["plan"].(map[string]any)
	plan["editors"], plan["session_model"] = 1, session
	var err error
	r.Workload, err = json.Marshal(workload)
	if err != nil {
		t.Fatal(err)
	}
	r.Phases[0].EditProbe, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	r.Phases[0].SessionModel, r.Phases[0].Duration = session, 1500
	r.Phases[0].Gate["edit_loop_wall"] = json.RawMessage(`{"clock":"monotonic","wall_ns":1500000000000,"complete":true}`)
	config := inputs{CandidateRevision: strings.Repeat("a", 40), Hosts: map[string]string{"reference_linux": "synthetic-host"}}
	if err := validateEditLoopReport(config, runInput{Role: "candidate", HostClass: "reference_linux", Scenario: "loadedit-1"}, r); err != nil {
		t.Fatalf("nightly's 100 edits at the original 15-second single-editor cadence got an invented smoke deadline: %v", err)
	}
}

func TestEditLoopCompactQualificationNeverWaivesMissingProof(t *testing.T) {
	for _, variant := range []string{"complete", "missing_supported_tool", "one_correct_edit", "decode_failure", "missing_refusal", "missing_fallback", "untested_unsupported"} {
		t.Run(variant, func(t *testing.T) {
			r := editLoopEvidenceFixture(t)
			var raw map[string]any
			if err := json.Unmarshal(r.Phases[0].EditProbe, &raw); err != nil {
				t.Fatal(err)
			}
			mode := raw["output_modes"].(map[string]any)["compact"].(map[string]any)
			mode["claims_fresh_observable"], mode["claims_fresh"] = false, 0
			mode["stale_claimed_fresh"] = map[string]any{"value": nil, "unit": "answers", "reason": "compact freshness metadata unobservable"}
			tools := mode["tools"].(map[string]any)
			for name, value := range tools {
				tool := value.(map[string]any)
				if name == "read.source" || name == "read.editing_context" || name == "search.text" {
					tool["state"], tool["reason"], tool["refused_edits"], tool["fallback_answers"], tool["observed_edits"] = "UNSUPPORTED", "harness compact-format refusal; actual JSON fallback retained", 20, 20, 0
				} else {
					tool["correct_edits"] = 20
				}
			}
			switch variant {
			case "missing_supported_tool":
				delete(tools, "read.summary")
			case "one_correct_edit":
				tools["read.summary"].(map[string]any)["correct_edits"] = 1
			case "decode_failure":
				tools["read.summary"].(map[string]any)["decode_failures"] = 1
			case "missing_refusal":
				delete(tools["read.source"].(map[string]any), "refused_edits")
			case "missing_fallback":
				delete(tools["read.source"].(map[string]any), "fallback_answers")
			case "untested_unsupported":
				tools["read.source"].(map[string]any)["refused_edits"] = 0
			}
			data, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			r.Phases[0].EditProbe = data
			d := dataset{config: inputs{Repeats: 3}}
			for repeat := 1; repeat <= 3; repeat++ {
				d.runs = append(d.runs, observation{input: runInput{Role: "candidate", HostClass: "reference_linux", Scenario: "loadedit-4", Repeat: repeat}, data: r})
			}
			row := assessEditLoopRow(budget{Metric: "silent_stale_compact", Scenario: "loadedit-4", HostClass: "reference_linux", Target: pointer(0), Compare: "eq", Unit: "count"}, d)
			if variant == "complete" {
				if row.State != "UNOBSERVABLE" || row.Value != nil || row.Reason == "" {
					t.Errorf("qualified compact needs explicit nonnumeric reason: %+v", row)
				}
			} else if row.State == "PASS" || row.State == "UNOBSERVABLE" || row.Value != nil {
				t.Errorf("missing compact proof qualified: %+v", row)
			}
		})
	}
}

func TestEditLoopLargeFileRequiresActualTargetCensus(t *testing.T) {
	for _, variant := range []string{"complete", "small", "missing"} {
		t.Run(variant, func(t *testing.T) {
			r := editLoopEvidenceFixture(t)
			var raw map[string]any
			if err := json.Unmarshal(r.Phases[0].EditProbe, &raw); err != nil {
				t.Fatal(err)
			}
			for _, value := range raw["edits"].([]any) {
				value.(map[string]any)["admitted_target_lines"] = 10000
			}
			first := raw["edits"].([]any)[0].(map[string]any)
			if variant == "small" {
				first["admitted_target_lines"] = 9999
			}
			if variant == "missing" {
				delete(first, "admitted_target_lines")
			}
			data, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			r.Phases[0].EditProbe = data
			config := inputs{CandidateRevision: strings.Repeat("a", 40), Hosts: map[string]string{"reference_linux": "synthetic-host"}}
			err = validateEditLoopReport(config, runInput{Role: "candidate", HostClass: "reference_linux", Scenario: "loadedit-4-largefile"}, r)
			if (err == nil) != (variant == "complete") {
				t.Errorf("large-file alias accepted without actual target admission, variant%s error%v", variant, err)
			}
		})
	}
}

func TestEditLoopSixRepoCellRequiresPinnedRoutedEvidence(t *testing.T) {
	for _, variant := range []string{"complete", "missing_routes", "duplicate_route", "foreign_record", "foreign_pin", "one_corpus"} {
		t.Run(variant, func(t *testing.T) {
			r := editLoopEvidenceFixture(t)
			var raw, workload map[string]any
			if err := json.Unmarshal(r.Phases[0].EditProbe, &raw); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(r.Workload, &workload); err != nil {
				t.Fatal(err)
			}
			workload["edit_agents"] = 6
			plan := raw["plan"].(map[string]any)
			plan["editors"] = 6
			routes := make([]any, 6)
			r.Corpus = nil
			for i := range 6 {
				label := fmt.Sprintf("repo-%d", i)
				commit := fmt.Sprintf("%040x", i+1)
				routes[i] = map[string]any{"id": label, "repo": label, "commit": commit, "file": "fixture.go", "kind": "add_intrafile_call", "tool": "edit.file", "language": "go", "mode": "json"}
				r.Corpus = append(r.Corpus, map[string]any{"label": label, "commit": commit})
			}
			plan["routes"] = routes
			for i, value := range raw["edits"].([]any) {
				record := value.(map[string]any)
				step := plan["steps"].([]any)[i].(map[string]any)
				label := fmt.Sprintf("repo-%d", i%6)
				record["editor"], record["route"], step["editor"], step["route"] = i%6, label, i%6, label
			}
			switch variant {
			case "missing_routes":
				delete(plan, "routes")
			case "duplicate_route":
				routes[1].(map[string]any)["id"] = routes[0].(map[string]any)["id"]
			case "foreign_record":
				raw["edits"].([]any)[0].(map[string]any)["route"] = "repo-1"
			case "foreign_pin":
				routes[0].(map[string]any)["commit"] = strings.Repeat("f", 40)
			case "one_corpus":
				r.Corpus = r.Corpus[:1]
			}
			var err error
			r.Workload, err = json.Marshal(workload)
			if err != nil {
				t.Fatal(err)
			}
			r.Phases[0].EditProbe, err = json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			config := inputs{CandidateRevision: strings.Repeat("a", 40), Hosts: map[string]string{"reference_linux": "synthetic-host"}}
			err = validateEditLoopReport(config, runInput{Role: "candidate", HostClass: "reference_linux", Scenario: "multirepoedit-6"}, r)
			if (err == nil) != (variant == "complete") {
				t.Errorf("six-repo alias borrowed unproved routes, variant%s error%v", variant, err)
			}
		})
	}
}
