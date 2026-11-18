package main

import "testing"

func partialMetadataReportFixture() map[string]any {
	raw := readFreshnessReportFixture("projection_superseded")
	p := raw["phases"].([]any)[0].(map[string]any)
	last := p["attempts"].([]any)[1].(map[string]any)
	last["read_metadata_state"] = "partial_pending"
	p["tools"].(map[string]any)["read.summary"].(map[string]any)["partial_pending"] = 1
	return raw
}

func TestPartialMetadataCheckerKeepsRPCCompletionSeparate(t *testing.T) {
	// A nonzero explicit partial observation is not a new zero-partial gate.
	raw := partialMetadataReportFixture()
	if err := validateFreshnessFixture(t, raw); err != nil {
		t.Fatal(err)
	}
	p := raw["phases"].([]any)[0].(map[string]any)
	metric := p["tools"].(map[string]any)["read.summary"].(map[string]any)
	if metric["successes"] != 1 || metric["partial_pending"] != 1 {
		t.Fatal("RPC success and partial observation must remain independent")
	}
	old := readFreshnessReportFixture("projection_superseded")
	if err := validateFreshnessFixture(t, old); err != nil {
		t.Fatalf("historical missing fields are UNOBSERVED: %v", err)
	}
	zero := readFreshnessReportFixture("projection_superseded")
	zero["phases"].([]any)[0].(map[string]any)["tools"].(map[string]any)["read.summary"].(map[string]any)["partial_pending"] = 0
	if err := validateFreshnessFixture(t, zero); err != nil {
		t.Fatalf("zero means zero explicit observations, not completeness: %v", err)
	}
}

func TestPartialMetadataCheckerRejectsLostOrInventedObservations(t *testing.T) {
	for _, name := range []string{"unknown_enum", "counter_erased", "counter_missing", "counter_null", "row_missing", "negative_counter", "wrong_role", "wrong_tool", "on_refusal"} {
		t.Run(name, func(t *testing.T) {
			raw := partialMetadataReportFixture()
			p := raw["phases"].([]any)[0].(map[string]any)
			rows := p["attempts"].([]any)
			first, last := rows[0].(map[string]any), rows[1].(map[string]any)
			metric := p["tools"].(map[string]any)["read.summary"].(map[string]any)
			switch name {
			case "unknown_enum":
				last["read_metadata_state"] = "complete"
			case "counter_erased":
				metric["partial_pending"] = 0
			case "counter_missing":
				delete(metric, "partial_pending")
			case "counter_null":
				metric["partial_pending"] = nil
			case "row_missing":
				delete(last, "read_metadata_state")
			case "negative_counter":
				metric["partial_pending"] = -1
			case "wrong_role":
				last["role"] = "probe"
			case "wrong_tool":
				last["tool"] = "edit.file"
			case "on_refusal":
				delete(last, "read_metadata_state")
				first["read_metadata_state"] = "partial_pending"
			}
			if err := validateFreshnessFixture(t, raw); err == nil {
				t.Fatal("unreconciled or unsupported partial metadata observation accepted")
			}
		})
	}
}
