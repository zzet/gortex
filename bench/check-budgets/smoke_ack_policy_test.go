package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSmokeChildSelectsExplicitLocalACK(t *testing.T) {
	c := fixtureLaneConfig()
	args := smokeSequenceArgs(c, "/new/owned/runs")
	if !strings.Contains(strings.Join(args, " "), "--edit-ack local") {
		t.Fatalf("future smoke leaves product ACK default implicit: %v", args)
	}
	if !strings.Contains(strings.Join(args, " "), "--main-binary "+c.Builds["main"].Binary) || !strings.Contains(strings.Join(args, " "), "--candidate-binary "+c.Builds["candidate"].Binary) {
		t.Fatal("explicit policy split the two-build child protocol")
	}
}

// Decode optional metadata through JSON so the pre-fix validator executes and
// exposes acceptance of mismatched/missing effective policy, rather than
// failing to compile because a new internal receipt field is absent.
func TestSmokeConfiguredACKMatchesEveryActualEdit(t *testing.T) {
	for _, mutation := range []string{"", "effective-full", "missing-effective", "missing-workload", "empty-edits", "unknown-policy"} {
		t.Run(map[string]string{"": "local"}[mutation]+mutation, func(t *testing.T) {
			receipt, d, _ := syntheticSmokeJob(t)
			var raw map[string]any
			data, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			policy := "local"
			if mutation == "unknown-policy" {
				policy = "unknown"
			}
			raw["smoke_provenance"].(map[string]any)["edit_ack_mode"] = policy
			data, err = json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &receipt); err != nil {
				t.Fatal(err)
			}
			for i := range d.runs {
				d.runs[i].input.Scenario = "loadedit-4"
				receipt.Tasks[i].Scenario = "loadedit-4"
				var workload map[string]any
				if err := json.Unmarshal(d.runs[i].data.Workload, &workload); err != nil {
					t.Fatal(err)
				}
				workload["scenario"], workload["agents"], workload["edit_agents"] = "edit", 0, 4
				workload["edit_ack_mode"] = "local"
				if i == 0 && mutation == "missing-workload" {
					delete(workload, "edit_ack_mode")
				}
				data, err = json.Marshal(workload)
				if err != nil {
					t.Fatal(err)
				}
				d.runs[i].data.Workload = data
				data, err = json.Marshal(d.runs[i].data.Phases[0])
				if err != nil {
					t.Fatal(err)
				}
				var phase map[string]any
				if err := json.Unmarshal(data, &phase); err != nil {
					t.Fatal(err)
				}
				mode := "local"
				if i == 0 && mutation == "effective-full" {
					mode = "full"
				}
				if i == 0 && mutation == "missing-effective" {
					mode = ""
				}
				phase["edits"] = []any{map[string]any{"acknowledgement": map[string]any{"structuredContent": map[string]any{"ack_mode": mode}}}}
				if i == 0 && mutation == "empty-edits" {
					phase["edits"] = []any{}
				}
				data, err = json.Marshal(phase)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(data, &d.runs[i].data.Phases[0]); err != nil {
					t.Fatal(err)
				}
			}
			err = validateSmokeJob(receipt, d)
			if mutation == "" && err != nil || mutation != "" && err == nil {
				t.Fatalf("configured/effective ACK identity not enforced: %v", err)
			}
		})
	}
}
