package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditLoopRetainsProvenReadGateClosuresWithIncompleteReads(t *testing.T) {
	for _, variant := range []string{
		"all_closed", "one_zero", "unseeded", "wrong_revision", "different_corpus",
		"foreign_status", "counter_reset", "producer_mismatch", "missing_repeat", "reused_run",
		"forced_stop", "over_wall_cap", "unbound_commit",
	} {
		t.Run(variant, func(t *testing.T) {
			dir := t.TempDir()
			index := inputs{
				Version: 1, Repeats: 3, CandidateRevision: strings.Repeat("a", 40),
				MainRevision: strings.Repeat("d", 40), TagRevision: shippedRevision,
				Hosts: map[string]string{"reference_linux": "synthetic-host"},
			}
			for repeat, closures := range []int{7, 4, 6} {
				repeat++
				name := fmt.Sprintf("repeat-%d.json", repeat)
				index.Runs = append(index.Runs, runInput{
					Role: "candidate", HostClass: "reference_linux", Scenario: "loadedit-4",
					Repeat: repeat, Report: name,
				})
				if variant == "missing_repeat" && repeat == 3 {
					continue
				}
				caseForRepeat := ""
				if repeat == 3 {
					caseForRepeat = variant
				}
				r := seededReadGateReportWithIncompleteReads(t, repeat, closures, caseForRepeat)
				if err := writeNewJSON(filepath.Join(dir, name), r); err != nil {
					t.Fatal(err)
				}
			}
			inputsPath, outputPath := filepath.Join(dir, "inputs.json"), filepath.Join(dir, "rows.json")
			if err := writeNewJSON(inputsPath, index); err != nil {
				t.Fatal(err)
			}
			exit := run([]string{"edit-loop", "--budgets", "../budgets.yaml", "--scenario", "loadedit-4",
				"--inputs", inputsPath, "--out", outputPath}, io.Discard, io.Discard)
			if exit != 1 {
				t.Fatalf("incomplete smoke exit=%d, want 1", exit)
			}
			data, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Pass bool          `json:"pass"`
				Rows []editLoopRow `json:"rows"`
			}
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			if result.Pass || len(result.Rows) != 14 {
				t.Fatalf("full verdict or row census changed: %+v", result)
			}
			for _, row := range result.Rows {
				if variant == "all_closed" && row.Metric == "read_gate_closures" {
					if row.State != "FAIL" || row.Value == nil || *row.Value != 7 || row.Budget == nil || *row.Budget != 0 ||
						!strings.Contains(row.Reason, "incomplete or unpinned edit-loop report") {
						t.Errorf("lost confirmed closures or original validation error: %+v", row)
					}
					continue
				}
				if row.State != "UNAVAILABLE" || row.Value != nil {
					t.Errorf("incomplete proof was promoted: %+v", row)
				}
			}
		})
	}
}

func seededReadGateReportWithIncompleteReads(t *testing.T, repeat, closures int, variant string) report {
	t.Helper()
	r := seededACKReportWithIndependentReadFailures(t, repeat, "below_budget")
	r.RunID = fmt.Sprintf("seeded-read-gate-%d", repeat)
	if variant == "reused_run" {
		r.RunID = "seeded-read-gate-1"
	}
	if variant == "wrong_revision" {
		r.Revision = strings.Repeat("f", 40)
	}
	if variant == "different_corpus" {
		r.Corpus[0]["commit"] = strings.Repeat("f", 40)
	}
	var workload map[string]any
	if err := json.Unmarshal(r.Workload, &workload); err != nil {
		t.Fatal(err)
	}
	workload["edit_gate_seed"] = "closed-read-gate"
	if variant == "unseeded" {
		delete(workload, "edit_gate_seed")
	}
	r.Workload = encodeReadGateFixture(t, workload)
	r.Complete = false
	r.Errors = []string{"immediate summary readback incorrect"}
	p := &r.Phases[0]
	p.Errors = r.Errors
	wallNS := int64(600_000_000_000)
	if variant == "over_wall_cap" {
		wallNS = 901_000_000_000
	}
	p.Gate["edit_loop_wall"] = encodeReadGateFixture(t, map[string]any{"clock": "monotonic", "wall_ns": wallNS, "complete": false})
	if variant == "one_zero" {
		closures = 0
	}
	setEditLoopReadGateClosures(t, p, closures)
	switch variant {
	case "foreign_status":
		p.Status[1] = json.RawMessage(`{"pid":456,"storage":{"wal_reclaim":{"pause_count":13}}}`)
	case "counter_reset":
		p.Status[1] = json.RawMessage(`{"pid":123,"storage":{"wal_reclaim":{"pause_count":1}}}`)
	case "producer_mismatch":
		p.Metrics["reclaim_gate_closures"] = metric{Value: pointer(0), Unit: "closures"}
	case "forced_stop":
		p.Metrics["force_killed"] = metric{Value: pointer(1), Unit: "boolean"}
	case "unbound_commit":
		var wire map[string]any
		if err := json.Unmarshal(p.EditProbe, &wire); err != nil {
			t.Fatal(err)
		}
		wire["edits"].([]any)[0].(map[string]any)["ack_mutation_id"] = "foreign-mutation"
		p.EditProbe = encodeReadGateFixture(t, wire)
	}
	return r
}

func encodeReadGateFixture(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
