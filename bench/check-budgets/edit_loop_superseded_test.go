package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func supersededEditLoopFixture(t *testing.T) (report, map[string]any, map[string]any) {
	t.Helper()
	r := editLoopEvidenceFixture(t)
	var raw map[string]any
	if err := json.Unmarshal(r.Phases[0].EditProbe, &raw); err != nil {
		t.Fatal(err)
	}
	plan := raw["plan"].(map[string]any)
	plan["kind"], plan["tool"], plan["file"] = "add_intrafile_call", "edit.file", "sample.go"
	raw["physical_write_scope"] = map[string]any{"kind": "shared_worktree_file", "file": "sample.go", "editors": 4}
	records := raw["edits"].([]any)
	for i, item := range records {
		record := item.(map[string]any)
		record["editor"], record["ack_readback_exact"] = i%4, false
		record["ack_status"], record["ack_disk_status"] = "applied", "committed"
		record["mutation_id"], record["ack_mutation_id"] = "edit-a", "edit-a"
		record["ack_commit_seq"], record["ack_content_sha256"] = i+1, strings.Repeat("d", 64)
	}
	data, err := os.ReadFile("../receiptproof/testdata/superseded.json")
	if err != nil {
		t.Fatal(err)
	}
	var proof map[string]any
	if err := json.Unmarshal(data, &proof); err != nil {
		t.Fatal(err)
	}
	first := records[0].(map[string]any)
	first["outcome"], first["mutation_receipt"] = "proven_superseded", "commit-a"
	first["superseded_receipt"] = proof
	first["accounting"] = map[string]any{"state": "terminal_unavailable_background", "bound_rpc_id": "rpc-2", "requested_generation": 3, "applied_generation": 3, "acknowledgement": map[string]any{"wall_ns": 8, "sum_ns": 8, "residual_ns": 0, "within_five_percent": true}, "physical": nil, "requested_background": nil}
	return r, raw, first
}

func TestEditLoopProvenSupersededRequiresExactProofAndSharedScope(t *testing.T) {
	for _, mode := range []string{"proven", "missing_proof", "foreign_guard", "isolated", "accepted_timing", "capped", "wrong_ACK", "incorrect_read"} {
		t.Run(mode, func(t *testing.T) {
			r, raw, first := supersededEditLoopFixture(t)
			proof := first["superseded_receipt"].(map[string]any)
			switch mode {
			case "missing_proof":
				delete(first, "superseded_receipt")
			case "foreign_guard":
				proof["guard"].(map[string]any)["attempt_guard_publication_key"] = "foreign"
			case "isolated":
				delete(raw, "physical_write_scope")
			case "accepted_timing":
				first["accounting"].(map[string]any)["physical"] = map[string]any{"wall_ns": 1}
			case "capped":
				first["outcome"] = "harness_evidence_incomplete"
			case "wrong_ACK":
				first["ack_disk_status"] = "pending"
			case "incorrect_read":
				first["readbacks"].([]any)[0].(map[string]any)["correct"] = false
			}
			encoded, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			r.Phases[0].EditProbe = encoded
			_, err = decodeEditLoopWire(r.Phases[0])
			if (err == nil) != (mode == "proven" || mode == "incorrect_read") {
				t.Fatalf("classification error=%v for %s", err, mode)
			}
			if mode == "proven" || mode == "incorrect_read" {
				ack, ackErr := measureEditLoopRow(r.Phases[0], "edit_ack_p95")
				if ackErr != nil || ack != .05 {
					t.Fatalf("classified ACK=%v %v", ack, ackErr)
				}
				correct, readErr := measureEditLoopRow(r.Phases[0], "readback_correct")
				if readErr != nil || (correct == 1) != (mode == "proven") {
					t.Fatalf("classified read correctness=%v %v", correct, readErr)
				}
			}
		})
	}
}

func TestEditLoopSupersededWholeGateKeepsAllFourteenRowsStrict(t *testing.T) {
	for _, qualifiedKind := range []string{"superseded", "coalesced", "native_noop", "native_noop_same"} {
		for _, mode := range []string{"proven", "incorrect_read", "slow_ack", "read_gate_closed", "WAL_breach", "missing_output_mode", "unknown_proof"} {
			t.Run(qualifiedKind+"/"+mode, func(t *testing.T) {
				dir := t.TempDir()
				config := inputs{Version: 1, Repeats: 3, CandidateRevision: strings.Repeat("a", 40), MainRevision: strings.Repeat("d", 40), TagRevision: shippedRevision, Hosts: map[string]string{"reference_linux": "synthetic-host"}}
				write := func(name string, value any) {
					t.Helper()
					data, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				for repeat := 1; repeat <= 3; repeat++ {
					r, raw, first := supersededEditLoopFixture(t)
					if qualifiedKind == "coalesced" {
						r, raw, first = coalescedEditLoopFixture(t)
					}
					if qualifiedKind == "native_noop" || qualifiedKind == "native_noop_same" {
						r, raw, first = nativeNoOpEditLoopFixture(t, qualifiedKind == "native_noop_same")
					}
					r.RunID = fmt.Sprintf("superseded-gate-%d", repeat)
					// Retain the existing compact unobservable/refusal contract from the
					// ordinary whole-gate fixture. This change creates no numeric freshness.
					compact := raw["output_modes"].(map[string]any)["compact"].(map[string]any)
					compact["claims_fresh_observable"], compact["claims_fresh"] = false, 0
					compact["stale_claimed_fresh"] = map[string]any{"value": nil, "unit": "answers", "reason": "compact freshness metadata unobservable"}
					for name, value := range compact["tools"].(map[string]any) {
						tool := value.(map[string]any)
						if name == "read.source" || name == "read.editing_context" || name == "search.text" {
							tool["state"], tool["reason"], tool["refused_edits"], tool["fallback_answers"], tool["observed_edits"] = "UNSUPPORTED", "harness compact-format refusal; actual JSON fallback retained", 20, 20, 0
						} else {
							tool["correct_edits"] = 20
						}
					}

					switch mode {
					case "incorrect_read":
						first["readbacks"].([]any)[0].(map[string]any)["correct"] = false
					case "slow_ack":
						for _, value := range raw["edits"].([]any) {
							value.(map[string]any)["t_ack_ms"] = map[string]any{"value": 3000, "unit": "ms", "reason": ""}
						}
					case "read_gate_closed":
						r.Phases[0].Metrics["reclaim_gate_closures"] = metric{Value: pointer(1), Unit: "closures"}
					case "WAL_breach":
						r.Phases[0].Metrics["peak_wal_bytes"] = metric{Value: pointer(20 * 1024 * 1024 * 1024), Unit: "bytes"}
					case "missing_output_mode":
						delete(raw["output_modes"].(map[string]any), "json")
					case "unknown_proof":
						delete(first, "superseded_receipt")
						delete(first, "coalesced_functional_receipt")
					}
					data, err := json.Marshal(raw)
					if err != nil {
						t.Fatal(err)
					}
					r.Phases[0].EditProbe = data
					name := fmt.Sprintf("repeat-%d.json", repeat)
					write(name, r)
					config.Runs = append(config.Runs, runInput{Role: "candidate", HostClass: "reference_linux", Scenario: "loadedit-4", Repeat: repeat, Report: name})
				}
				write("inputs.json", config)
				var out, diagnostics bytes.Buffer
				code := run([]string{"edit-loop", "--budgets", "../budgets.yaml", "--scenario", "loadedit-4", "--inputs", filepath.Join(dir, "inputs.json"), "--out", filepath.Join(dir, "result.json")}, &out, &diagnostics)
				if (code == 0) != (mode == "proven") {
					t.Fatalf("whole gate exit=%d mode=%s: %s %s", code, mode, out.String(), diagnostics.String())
				}
				data, err := os.ReadFile(filepath.Join(dir, "result.json"))
				if err != nil {
					t.Fatal(err)
				}
				var result struct {
					Rows []editLoopRow `json:"rows"`
				}
				if err := json.Unmarshal(data, &result); err != nil {
					t.Fatal(err)
				}
				if len(result.Rows) != 14 {
					t.Fatalf("lost mandatory rows: %d", len(result.Rows))
				}
				if mode == "proven" {
					for _, row := range result.Rows {
						if row.Metric == "silent_stale_compact" {
							if row.State != "UNOBSERVABLE" || row.Value != nil {
								t.Fatalf("compact unavailable contract changed: %+v", row)
							}
							continue
						}
						if row.State != "PASS" {
							t.Fatalf("proven receipt hid mandatory row: %+v", row)
						}
					}
				}
			})
		}

	}
}
