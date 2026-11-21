package main

import (
	"encoding/json"
	"testing"
)

func editLoopZeroHoldFixture(t *testing.T) phase {
	t.Helper()
	p := readGateFixture(t)
	raw := json.RawMessage(`{"pid":123,"storage":{"wal_reclaim":{"attempts":7,"pause_count":7,"writer_hold":{"count":2,"bucket_le_ms":[10,50,100],"bucket_counts":[2,0,0,0],"max_ms":8}}}}`)
	p.Status = []json.RawMessage{raw, raw}
	p.Metrics["reclaim_writer_holds"] = metric{Value: pointer(0), Unit: "holds"}
	p.Metrics["reclaim_writer_hold_p95_ms"] = metric{Unit: "ms", Reason: "no holds in the window"}
	p.Metrics["reclaim_writer_hold_max_ms"] = metric{Unit: "ms", Reason: "no holds in the window"}
	return p
}

func TestEditLoopZeroHoldApplicabilityRequiresCompleteWindow(t *testing.T) {
	for _, variant := range []string{"zero", "missing_capture", "window_mismatch", "foreign_pid", "missing_attempts", "attempts_changed", "missing_bucket", "count_changed", "bucket_changed", "metric_missing", "numeric_zero_p95", "nonnull_count_null_p95"} {
		t.Run(variant, func(t *testing.T) {
			p := editLoopZeroHoldFixture(t)
			var raw map[string]any
			if err := json.Unmarshal(p.Status[1], &raw); err != nil {
				t.Fatal(err)
			}
			reclaim := raw["storage"].(map[string]any)["wal_reclaim"].(map[string]any)
			hold := reclaim["writer_hold"].(map[string]any)
			switch variant {
			case "missing_capture":
				delete(p.Gate, "status_capture_v1")
			case "window_mismatch":
				p.Gate["process_counter_window"] = json.RawMessage(`{"started":"2026-11-03T00:00:02Z","ended":"2026-11-03T00:00:03Z"}`)
			case "foreign_pid":
				raw["pid"] = 456
			case "missing_attempts":
				delete(reclaim, "attempts")
			case "attempts_changed":
				reclaim["attempts"] = 8
			case "missing_bucket":
				hold["bucket_counts"] = []int{2, 0, 0}
			case "count_changed", "nonnull_count_null_p95":
				hold["count"] = 3
				hold["bucket_counts"] = []int{3, 0, 0, 0}
			case "bucket_changed":
				hold["bucket_le_ms"] = []int{10, 51, 100}
			case "metric_missing":
				delete(p.Metrics, "reclaim_writer_holds")
			case "numeric_zero_p95":
				p.Metrics["reclaim_writer_hold_p95_ms"] = metric{Value: pointer(0), Unit: "ms"}
			}
			data, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			p.Status[1] = data
			if err := qualifyEditLoopZeroHolds(p); (err == nil) != (variant == "zero") {
				t.Fatalf("qualification=%v for%s", err, variant)
			}
			row := assessEditLoopHoldSample(editLoopRow{Metric: "reclaim_hold_p95", State: "UNAVAILABLE", Budget: pointer(.05), Unit: "s"}, budget{Metric: "reclaim_hold_p95", Target: pointer(.05), Compare: "le"}, p)
			if variant == "zero" {
				if row.State != "NOT_EXERCISED" || row.Value != nil || editLoopRowBlocks(row) {
					t.Fatal("qualified zero became numeric/pass or blocking", row)
				}
			} else if row.State == "NOT_EXERCISED" || !editLoopRowBlocks(row) {
				t.Fatal("unknown/nonzero became inapplicable", row)
			}
		})
	}
}

func TestEditLoopZeroHoldApplicabilityRequiresConsistentRealRepeats(t *testing.T) {
	for _, variant := range []string{"three_zero", "one_zero", "missing_proof", "mixed_numeric_pass", "mixed_numeric_fail", "three_numeric"} {
		t.Run(variant, func(t *testing.T) {
			b := budget{Metric: "reclaim_hold_p95", Scenario: "loadedit-4", HostClass: "reference_linux", Target: pointer(.05), Unit: "s", Compare: "le"}
			d := dataset{config: inputs{Repeats: 3}}
			n := 3
			if variant == "one_zero" {
				n = 1
			}
			for i := 1; i <= n; i++ {
				p := editLoopZeroHoldFixture(t)
				p.EditProbe = editLoopEvidenceFixture(t).Phases[0].EditProbe
				if variant == "missing_proof" && i == 3 {
					delete(p.Gate, "status_capture_v1")
				}
				if variant == "three_numeric" || i == 3 && (variant == "mixed_numeric_pass" || variant == "mixed_numeric_fail") {
					p.Status[1] = holdStatus(t, []int64{3, 0, 0, 0}, 3, 10)
					p.Metrics["reclaim_writer_holds"] = metric{Value: pointer(1), Unit: "holds"}
					p.Metrics["reclaim_writer_hold_p95_ms"] = metric{Value: pointer(10), Unit: "ms"}
					if variant == "mixed_numeric_fail" {
						p.Status[1] = holdStatus(t, []int64{2, 0, 1, 0}, 3, 60)
						p.Metrics["reclaim_writer_hold_p95_ms"] = metric{Value: pointer(60), Unit: "ms"}
					}
				}
				d.runs = append(d.runs, observation{input: runInput{Role: "candidate", HostClass: b.HostClass, Scenario: b.Scenario, Repeat: i}, data: report{Phases: []phase{p}}})
			}
			row := assessEditLoopRow(b, d)
			want := "FAIL"
			switch variant {
			case "three_zero":
				want = "NOT_EXERCISED"
			case "one_zero", "missing_proof":
				want = "UNAVAILABLE"
			case "three_numeric":
				want = "PASS"
			}
			if row.State != want {
				t.Fatalf("got%+v want%s", row, want)
			}
			if variant == "three_zero" && (row.Value != nil || row.Delta != nil || editLoopRowBlocks(row)) {
				t.Fatal("collection applicability became numeric hold acceptance", row)
			}
			if variant == "three_numeric" && (row.Value == nil || *row.Value != .01) {
				t.Fatal("exercised hold measurement changed", row)
			}
		})
	}
}
