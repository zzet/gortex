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

func TestEditLoopGateRetainsEveryRowWhenEvidenceIsMissing(t *testing.T) {
	var out, diagnostics bytes.Buffer
	code := run([]string{"edit-loop", "--budgets", "../budgets.yaml", "--scenario", "loadedit-4", "--inputs", filepath.Join(t.TempDir(), "absent-inputs.json")}, &out, &diagnostics)
	if code != 1 {
		t.Fatalf("missing gate evidence exit=%d, want1; diagnostics=%s", code, diagnostics.String())
	}
	text := out.String()
	for _, row := range []string{"edit_ack_p95", "edit_ack_max", "readback_p95", "readback_max", "readback_correct", "silent_stale_json", "silent_stale_toon", "silent_stale_compact", "silent_stale_facade", "silent_stale_cli", "wal_p95_mib", "wal_peak_mib", "reclaim_hold_p95", "read_gate_closures"} {
		if !strings.Contains(text, row) {
			t.Errorf("missing evidence hid mandatory row %s: %s", row, text)
		}
	}
	if !strings.Contains(text, "UNAVAILABLE") || !strings.Contains(strings.ToLower(text), "baseline") || !strings.Contains(strings.ToLower(text), "delta") {
		t.Errorf("gate lost unavailable/baseline/delta interpretation: %s", text)
	}
	if strings.Contains(text, "\tPASS\t") {
		t.Fatal("unobserved row became a numeric pass")
	}
}

func TestEditLoopCommandRetainsActualRowsAndSeededBreaches(t *testing.T) {
	for _, variant := range []string{"complete", "ack_wait", "closed_gate", "repeat_disagreement"} {
		t.Run(variant, func(t *testing.T) {
			dir := t.TempDir()
			config := inputs{Version: 1, Repeats: 3, CandidateRevision: strings.Repeat("a", 40), MainRevision: strings.Repeat("d", 40), TagRevision: shippedRevision, Hosts: map[string]string{"reference_linux": "synthetic-host"}}
			write := func(path string, value any) {
				t.Helper()
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, path), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for repeat := 1; repeat <= 3; repeat++ {
				r := editLoopEvidenceFixture(t)
				r.RunID = fmt.Sprintf("synthetic-edit-%d", repeat)
				var raw map[string]any
				if err := json.Unmarshal(r.Phases[0].EditProbe, &raw); err != nil {
					t.Fatal(err)
				}
				compact := raw["output_modes"].(map[string]any)["compact"].(map[string]any)
				compact["claims_fresh_observable"], compact["claims_fresh"] = false, 0
				if variant == "ack_wait" || variant == "repeat_disagreement" && repeat == 3 {
					for _, value := range raw["edits"].([]any) {
						value.(map[string]any)["t_ack_ms"] = map[string]any{"value": 3000, "unit": "ms", "reason": ""}
					}
				}
				if variant == "closed_gate" {
					r.Phases[0].Metrics["reclaim_gate_closures"] = metric{Value: pointer(1), Unit: "closures"}
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
			if exit := run([]string{"edit-loop", "--budgets", "../budgets.yaml", "--scenario", "loadedit-4", "--inputs", filepath.Join(dir, "inputs.json"), "--out", filepath.Join(dir, "result.json")}, &out, &diagnostics); exit != 1 {
				t.Fatalf("unobservable compact cell must stay explicit/nonpassing, exit%d: %s", exit, diagnostics.String())
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
			rows := map[string]editLoopRow{}
			for _, row := range result.Rows {
				rows[row.Metric] = row
			}
			if rows["silent_stale_compact"].State != "UNAVAILABLE" || rows["silent_stale_compact"].Value != nil {
				t.Error("unobservable compact freshness became numeric pass")
			}
			if row := rows["wal_p95_mib"]; row.State != "PASS" || row.Value == nil || *row.Value != 32 || row.Baseline == nil || *row.Baseline != 1059 || row.Delta == nil || *row.Delta != -1027 {
				t.Errorf("fixed shipped WAL baseline/delta missing: %+v", row)
			}
			wantAck, wantGate := "PASS", "PASS"
			if variant == "ack_wait" || variant == "repeat_disagreement" {
				wantAck = "FAIL"
			}
			if variant == "closed_gate" {
				wantGate = "FAIL"
			}
			if row := rows["edit_ack_p95"]; row.State != wantAck {
				t.Errorf("ACK row %+v, want%s; %s", row, wantAck, out.String())
			}
			if row := rows["read_gate_closures"]; row.State != wantGate {
				t.Errorf("read gate row %+v, want%s", row, wantGate)
			}
			if rows["silent_stale_json"].State != "PASS" || rows["readback_correct"].State != "PASS" {
				t.Errorf("actual complete witnesses lost: %s", out.String())
			}
		})
	}
}
