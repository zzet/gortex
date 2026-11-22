package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEditLoopRetainsProvenACKBreachWithIncompleteReads(t *testing.T) {
	for _, variant := range []string{
		"all_acks", "missing_ack_attempt", "wrong_ack_owner", "below_budget", "missing_repeat", "wrong_seed",
		"wire_below_budget", "different_corpus", "retry_after_terminal_attempt",
	} {
		t.Run(variant, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name string, value any) {
				t.Helper()
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			index := inputs{
				Version: 1, Repeats: 3, CandidateRevision: strings.Repeat("a", 40),
				MainRevision: strings.Repeat("d", 40), TagRevision: shippedRevision,
				Hosts: map[string]string{"reference_linux": "synthetic-host"},
			}
			for repeat := 1; repeat <= index.Repeats; repeat++ {
				if variant == "missing_repeat" && repeat == 2 {
					continue
				}
				report := seededACKReportWithIndependentReadFailures(t, repeat, variant)
				name := fmt.Sprintf("repeat-%d.json", repeat)
				write(name, report)
				index.Runs = append(index.Runs, runInput{
					Role: "candidate", HostClass: "reference_linux", Scenario: "loadedit-4",
					Repeat: repeat, Report: name,
				})
			}
			write("inputs.json", index)
			output := filepath.Join(dir, "rows.json")
			exit := run([]string{"edit-loop", "--budgets", "../budgets.yaml", "--scenario", "loadedit-4",
				"--inputs", filepath.Join(dir, "inputs.json"), "--out", output}, io.Discard, io.Discard)
			if exit != 1 {
				t.Fatalf("incomplete smoke exit=%d, want 1", exit)
			}
			data, err := os.ReadFile(output)
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
				if variant == "all_acks" && (row.Metric == "edit_ack_p95" || row.Metric == "edit_ack_max") {
					if row.State != "FAIL" || row.Value == nil || *row.Value != 3 ||
						!strings.Contains(row.Reason, "incomplete or unpinned edit-loop report") {
						t.Errorf("lost provable ACK breach or original incompleteness: %+v", row)
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

func seededACKReportWithIndependentReadFailures(t *testing.T, repeat int, variant string) report {
	t.Helper()
	r := editLoopEvidenceFixture(t)
	r.RunID = fmt.Sprintf("seeded-ack-%d", repeat)
	r.Seed = int64(repeat)
	var workload map[string]any
	if err := json.Unmarshal(r.Workload, &workload); err != nil {
		t.Fatal(err)
	}
	workload["edit_gate_seed"], workload["seed"] = "ack-wait", repeat
	if variant == "wrong_seed" && repeat == 2 {
		workload["edit_gate_seed"] = "closed-read-gate"
	}
	encode := func(value any) json.RawMessage {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	r.Workload = encode(workload)
	p := &r.Phases[0]
	var wire map[string]any
	if err := json.Unmarshal(p.EditProbe, &wire); err != nil {
		t.Fatal(err)
	}
	const file = "fixture.go"
	r.Corpus[0]["file"] = file
	if variant == "different_corpus" && repeat == 2 {
		r.Corpus[0]["commit"] = strings.Repeat("f", 40)
	}
	wire["physical_write_scope"] = map[string]any{"kind": "shared_worktree_file", "file": file, "editors": 4}
	plan := wire["plan"].(map[string]any)
	plan["kind"], plan["tool"], plan["file"], plan["seed"] = "add_intrafile_call", "edit.file", file, repeat
	steps, edits := plan["steps"].([]any), wire["edits"].([]any)
	p.Attempts = nil
	for ordinal := range steps {
		step, edit := steps[ordinal].(map[string]any), edits[ordinal].(map[string]any)
		hash := fmt.Sprintf("%064x", ordinal+1)
		mutation := fmt.Sprintf("load-edit-%016x-%03d", repeat, ordinal)
		receipt := fmt.Sprintf("owned-receipt-%d", ordinal)
		latency := float64(3000)
		if variant == "below_budget" {
			latency = 50
		}
		step["mutation_id"], step["request_sha256"] = mutation, hash
		edit["editor"], edit["mutation_id"], edit["mutation_receipt"] = ordinal%4, mutation, receipt
		edit["ack_readback_exact"], edit["ack_status"], edit["ack_disk_status"] = false, "applied", "committed"
		edit["ack_mutation_id"], edit["ack_commit_seq"], edit["ack_content_sha256"] = mutation, ordinal+1, hash
		edit["t_ack_ms"] = metric{Value: &latency, Unit: "ms"}
		if repeat < 3 && ordinal == 0 {
			r.Complete = false
			r.Errors, p.Errors = []string{"immediate summary readback incorrect"}, []string{"immediate summary readback incorrect"}
			p.Gate["edit_loop_wall"] = encode(map[string]any{"clock": "monotonic", "wall_ns": 600000000000, "complete": false})
			edit["outcome"], edit["errors"] = "diagnostic_failure", []string{"immediate summary readback incorrect"}
			if repeat == 2 {
				edit["outcome"], edit["errors"] = "harness_evidence_incomplete", []string{"per-edit raw evidence limit exceeded after ACK"}
			}
		}
		if repeat == 2 && ordinal == 0 {
			if variant == "missing_ack_attempt" {
				continue
			}
			if variant == "wrong_ack_owner" {
				edit["ack_mutation_id"] = "foreign-mutation"
			}
		}
		wireLatency := latency - 1
		if variant == "wire_below_budget" {
			wireLatency = 1
		}
		success := attempt{
			Tool: "edit.file", Role: "workload", Request: hash, Started: time.Unix(int64(ordinal+1), 0),
			Agent: new(0), Step: new(ordinal), Slot: new(0), Retry: new(0), Code: new(0),
			Outcome: "success", DurationMS: new(wireLatency), LogicalMS: new(wireLatency),
		}
		if repeat == 2 && ordinal == 0 && (variant == "all_acks" || variant == "retry_after_terminal_attempt") {
			previous := success
			previous.DurationMS, previous.LogicalMS = new(1.0), new(1.0)
			previous.Outcome, previous.Code, previous.Error = "busy", new(-32002), "store busy"
			if variant == "retry_after_terminal_attempt" {
				previous.Outcome, previous.Code, previous.Error = "product_deadline", new(0), "deadline exceeded"
			}
			p.Attempts = append(p.Attempts, previous)
			success.Retry = new(1)
			success.Started = previous.Started.Add(time.Millisecond)
		}
		p.Attempts = append(p.Attempts, success)
	}
	p.EditProbe = encode(wire)
	return r
}
