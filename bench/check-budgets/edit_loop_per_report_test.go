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

func TestEditLoopPerReportDoesNotRelaxAggregate(t *testing.T) {
	for _, variant := range []string{"one_complete", "ack_breach", "missing_mode", "no_holds", "one_zero_verified", "invalid_second", "duplicate_report", "three_complete", "three_zero", "three_disagree"} {
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
			index := inputs{Version: 1, Repeats: 3, CandidateRevision: strings.Repeat("a", 40), MainRevision: strings.Repeat("d", 40), TagRevision: shippedRevision, Hosts: map[string]string{"reference_linux": "synthetic-host"}}
			n := 1
			if strings.HasPrefix(variant, "three_") {
				n = 3
			}
			if variant == "invalid_second" {
				n = 2
			}
			for repeat := 1; repeat <= n; repeat++ {
				r := editLoopEvidenceFixture(t)
				r.RunID = fmt.Sprintf("per-report-%d", repeat)
				var wire map[string]any
				if err := json.Unmarshal(r.Phases[0].EditProbe, &wire); err != nil {
					t.Fatal(err)
				}
				// Same already-approved compact proof shape as the aggregate
				// regression fixtures; never make its stale count numeric.
				compact := wire["output_modes"].(map[string]any)["compact"].(map[string]any)
				compact["claims_fresh_observable"], compact["claims_fresh"] = false, 0
				compact["stale_claimed_fresh"] = map[string]any{"value": nil, "unit": "answers", "reason": "compact freshness metadata unobservable"}
				for name, value := range compact["tools"].(map[string]any) {
					tool := value.(map[string]any)
					if name == "read.source" || name == "read.editing_context" || name == "search.text" {
						tool["state"], tool["reason"], tool["refused_edits"], tool["fallback_answers"], tool["observed_edits"] = "UNSUPPORTED", "harness format refusal; actual JSON fallback", 20, 20, 0
					} else {
						tool["correct_edits"] = 20
					}
				}
				if variant == "ack_breach" || variant == "three_disagree" && repeat == 3 {
					for _, row := range wire["edits"].([]any) {
						row.(map[string]any)["t_ack_ms"] = map[string]any{"value": 3000, "unit": "ms", "reason": ""}
					}
				}
				if variant == "missing_mode" {
					delete(wire["output_modes"].(map[string]any)["json"].(map[string]any)["tools"].(map[string]any), "read.summary")
				}
				if variant == "no_holds" {
					r.Phases[0].Metrics["reclaim_writer_holds"] = metric{Value: pointer(0), Unit: "holds"}
					r.Phases[0].Metrics["reclaim_writer_hold_p95_ms"] = metric{Unit: "ms", Reason: "no holds in the window"}
				}
				if variant == "one_zero_verified" || variant == "three_zero" {
					zero := editLoopZeroHoldFixture(t)
					r.Phases[0].Status = zero.Status
					for _, key := range []string{"status_capture_v1", "process_counter_window"} {
						r.Phases[0].Gate[key] = zero.Gate[key]
					}
					for _, key := range []string{"reclaim_writer_holds", "reclaim_writer_hold_p95_ms", "reclaim_writer_hold_max_ms"} {
						r.Phases[0].Metrics[key] = zero.Metrics[key]
					}
				}
				if variant == "invalid_second" && repeat == 2 {
					r.Complete = false
				}
				data, err := json.Marshal(wire)
				if err != nil {
					t.Fatal(err)
				}
				r.Phases[0].EditProbe = data
				name := fmt.Sprintf("report-%d.json", repeat)
				write(name, r)
				index.Runs = append(index.Runs, runInput{Role: "candidate", HostClass: "reference_linux", Scenario: "loadedit-4", Repeat: repeat, Report: name})
			}
			if variant == "duplicate_report" {
				duplicate := index.Runs[0]
				duplicate.Repeat = 2
				index.Runs = append(index.Runs, duplicate)
			}
			write("inputs.json", index)
			var stdout, stderr bytes.Buffer
			code := run([]string{"edit-loop", "--budgets", "../budgets.yaml", "--scenario", "loadedit-4", "--inputs", filepath.Join(dir, "inputs.json"), "--out", filepath.Join(dir, "aggregate.json"), "--per-report-out", filepath.Join(dir, "per-report.json")}, &stdout, &stderr)
			wantCode := 1
			if variant == "three_complete" || variant == "three_zero" {
				wantCode = 0
			}
			if code != wantCode {
				t.Fatalf("exit%d want%d; %s", code, wantCode, stderr.String())
			}
			var per struct {
				Scope           string `json:"scope"`
				ValidationError string `json:"validation_error"`
				Reports         []struct {
					RunID string        `json:"run_id"`
					Pass  bool          `json:"pass"`
					Rows  []editLoopRow `json:"rows"`
				} `json:"reports"`
			}
			data, err := os.ReadFile(filepath.Join(dir, "per-report.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(data, &per); err != nil {
				t.Fatal(err)
			}
			if per.Scope != "PER_VALIDATED_REPORT_ONLY_NOT_THREE_RUN_STABILITY" {
				t.Fatal("per-report output lost scope", per.Scope)
			}
			if variant == "invalid_second" || variant == "duplicate_report" {
				if per.ValidationError == "" || len(per.Reports) != 0 {
					t.Fatal("invalid dataset gave favorable prefix credit")
				}
				return
			}
			if per.ValidationError != "" || len(per.Reports) != n {
				t.Fatalf("per-report census=%d error=%s", len(per.Reports), per.ValidationError)
			}
			for i, report := range per.Reports {
				if report.RunID != fmt.Sprintf("per-report-%d", i+1) {
					t.Fatal("invented/reused report identity", report.RunID)
				}
				rows := map[string]editLoopRow{}
				for _, row := range report.Rows {
					rows[row.Metric] = row
				}
				if len(rows) != 14 {
					t.Fatalf("per-report dropped rows: %d", len(rows))
				}
				if rows["silent_stale_compact"].State != "UNOBSERVABLE" || rows["silent_stale_compact"].Value != nil {
					t.Fatal("compact proof changed")
				}
				if variant == "ack_breach" || variant == "three_disagree" && i == 2 {
					if rows["edit_ack_p95"].State != "FAIL" || report.Pass {
						t.Fatal("numeric breach became pass")
					}
				} else if rows["edit_ack_p95"].State != "PASS" || *rows["edit_ack_p95"].Value != .05 {
					t.Fatal("original ACK measure changed")
				}
				if variant == "missing_mode" && (rows["silent_stale_json"].State != "UNAVAILABLE" || report.Pass) {
					t.Fatal("missing coverage became pass")
				}
				if variant == "no_holds" && (rows["reclaim_hold_p95"].State != "UNAVAILABLE" || rows["reclaim_hold_p95"].Value != nil || report.Pass) {
					t.Fatal("zero events became numeric p95 or applicability waiver")
				}
				if (variant == "one_zero_verified" || variant == "three_zero") && (rows["reclaim_hold_p95"].State != "NOT_EXERCISED" || rows["reclaim_hold_p95"].Value != nil || !report.Pass) {
					t.Fatal("verified zero window lost null collection applicability")
				}
				if (variant == "one_complete" || variant == "three_complete") && !report.Pass {
					t.Fatal("complete single-report table unavailable")
				}
			}
			var aggregate struct {
				Pass bool          `json:"pass"`
				Rows []editLoopRow `json:"rows"`
			}
			data, err = os.ReadFile(filepath.Join(dir, "aggregate.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(data, &aggregate); err != nil {
				t.Fatal(err)
			}
			if aggregate.Pass != (variant == "three_complete" || variant == "three_zero") {
				t.Fatal("aggregate three-report gate relaxed")
			}
			if variant == "one_complete" {
				for _, row := range aggregate.Rows {
					if row.State != "UNAVAILABLE" || !strings.Contains(row.Reason, "repeat") {
						t.Fatal("one report supplied three-report verdict", row)
					}
				}
			}
			if variant == "three_disagree" && !strings.Contains(stdout.String(), "same-build repeat verdicts disagree") {
				t.Fatal("repeat disagreement was hidden")
			}
		})
	}
}
